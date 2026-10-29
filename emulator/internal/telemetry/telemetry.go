// Package telemetry is the emulator's view of the collector's JSON API: the
// vendor-independent switch model and a cache that polls it.
package telemetry

import "time"

// CollectorState mirrors the collector's per-switch state.
type CollectorState string

const (
	StateStarting    CollectorState = "starting"
	StateOK          CollectorState = "ok"
	StateUnavailable CollectorState = "unavailable"
)

// SwitchStatus is one entry of GET /v1/switches.
type SwitchStatus struct {
	ID                  string         `json:"id"`
	State               CollectorState `json:"state"`
	AgeSeconds          *float64       `json:"age_seconds"`
	LastError           string         `json:"last_error"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	Snapshot            *Snapshot      `json:"snapshot"`
	ConfigSync          *ConfigSync    `json:"config_sync"`
}

// ConfigSync is the collector's progress applying UniFi port configuration.
type ConfigSync struct {
	Enabled             bool    `json:"enabled"`
	State               string  `json:"state"`
	DesiredCfgVersion   *string `json:"desired_cfgversion"`
	ProcessedCfgVersion *string `json:"processed_cfgversion"`
}

// Snapshot is the latest successful reading of a physical switch.
type Snapshot struct {
	Device      Device  `json:"device"`
	Ports       []Port  `json:"ports"`
	CollectedAt float64 `json:"collected_at"`
}

// Device identifies the physical switch. These values never become part of
// the virtual UniFi identity; they are only used for display and for
// detecting that a different switch now answers at the configured address.
type Device struct {
	Model        string `json:"model"`
	Firmware     string `json:"firmware"`
	Serial       string `json:"serial"`
	ManagementIP string `json:"management_ip"`
	PortCount    int    `json:"port_count"`
	// HardwareMAC is the switch's own MAC (used with unifi.mac: physical).
	HardwareMAC string `json:"hardware_mac"`
	// PortSpeeds lists the speed settings every port accepts ("auto",
	// "10h", "10f", "100h", "100f", "1000f"); empty if unknown.
	PortSpeeds []string `json:"port_speeds"`
}

// Port is the normalized state of one physical port. Pointer fields are nil
// when the switch does not report the value.
type Port struct {
	Index       int      `json:"index"`
	Name        string   `json:"name"`
	LinkUp      bool     `json:"link_up"`
	SpeedMbps   *int     `json:"speed_mbps"`
	FullDuplex  *bool    `json:"full_duplex"`
	Autoneg     *bool    `json:"autoneg"`
	Enabled     *bool    `json:"enabled"`
	RXBytes     *uint64  `json:"rx_bytes"`
	TXBytes     *uint64  `json:"tx_bytes"`
	RXBytesPerS *float64 `json:"rx_bytes_per_s"`
	TXBytesPerS *float64 `json:"tx_bytes_per_s"`
	CRCErrors   *uint64  `json:"crc_errors"`
}

// Observation is a snapshot plus when the emulator learned it, so freshness
// is judged on the emulator's own clock rather than the collector's.
type Observation struct {
	Status     SwitchStatus
	ReceivedAt time.Time
}

// FreshSnapshot returns the snapshot when the collector reports it usable and
// it is younger than maxAge, judged as the collector-reported age plus the
// time since the emulator received it.
func (o Observation) FreshSnapshot(now time.Time, maxAge time.Duration) (*Snapshot, bool) {
	s := o.Status
	if s.Snapshot == nil || s.State != StateOK || s.AgeSeconds == nil {
		return nil, false
	}
	age := time.Duration(*s.AgeSeconds*float64(time.Second)) + now.Sub(o.ReceivedAt)
	if age > maxAge {
		return nil, false
	}
	return s.Snapshot, true
}
