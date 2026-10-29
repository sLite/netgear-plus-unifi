// Package state persists per-device adoption state so a restarted emulator
// resumes as the same adopted device instead of falling back to pending.
// Files contain controller auth keys: they are written 0600 and must never be
// logged or committed.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Device is everything needed to resume one virtual device.
type Device struct {
	SwitchID string `json:"switch_id"`
	MAC      string `json:"mac"`
	// MACSource is "physical" when MAC is the NETGEAR's own MAC.
	MACSource string    `json:"mac_source,omitempty"`
	Model     string    `json:"model"`
	BootTime  time.Time `json:"boot_time"`

	Adopted   bool   `json:"adopted"`
	AuthKey   string `json:"authkey,omitempty"`
	InformURL string `json:"inform_url,omitempty"`
	// BootstrapInformURL is the configured inform URL when the device was
	// adopted. If the configuration later points at a different controller
	// the stored key is meaningless there and the adoption is discarded.
	BootstrapInformURL string `json:"bootstrap_inform_url,omitempty"`
	UseAESGCM          bool   `json:"use_aes_gcm"`
	CfgVersion         string `json:"cfgversion,omitempty"`
	FirmwareVersion    string `json:"firmware_version,omitempty"`
	// Provisioned holds the tables the controller pushed via setstate. They
	// are echoed back to the controller but never applied to hardware.
	Provisioned map[string]json.RawMessage `json:"provisioned,omitempty"`

	// SwitchConfig is the switch.* section of the last system_cfg UniFi sent
	// (secrets removed) and SwitchConfigVersion its cfgversion. AckedCfgVersion
	// is the last cfgversion reported to UniFi as applied.
	SwitchConfig        string `json:"switch_config,omitempty"`
	SwitchConfigVersion string `json:"switch_config_version,omitempty"`
	AckedCfgVersion     string `json:"acked_cfgversion,omitempty"`

	// PhysicalModel and PhysicalSerial bind the virtual device to the
	// physical switch first seen behind it.
	PhysicalModel  string `json:"physical_model,omitempty"`
	PhysicalSerial string `json:"physical_serial,omitempty"`
}

// Store reads and writes device state files under a directory.
type Store struct {
	dir string
}

// NewStore returns a store rooted at dir, creating it if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "devices"), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) devicePath(id string) string {
	return filepath.Join(s.dir, "devices", id+".json")
}

// Load returns the stored state for a switch id; ok is false when none exists.
func (s *Store) Load(id string) (Device, bool, error) {
	raw, err := os.ReadFile(s.devicePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return Device{}, false, nil
	}
	if err != nil {
		return Device{}, false, fmt.Errorf("read state for %s: %w", id, err)
	}
	var d Device
	if err := json.Unmarshal(raw, &d); err != nil {
		return Device{}, false, fmt.Errorf("decode state for %s: %w", id, err)
	}
	return d, true, nil
}

// Save atomically writes the state for d.SwitchID.
func (s *Store) Save(d Device) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.devicePath(d.SwitchID), raw)
}

// Delete removes all state for a switch id, including captured provisioning.
func (s *Store) Delete(id string) error {
	if err := os.Remove(s.devicePath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.RemoveAll(filepath.Join(s.dir, "provisioning", id))
}

// IDs lists the switch ids with stored state.
func (s *Store) IDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "devices"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			ids = append(ids, name)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
