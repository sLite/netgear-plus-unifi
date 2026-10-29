package device

import (
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"

	"netgear-plus-unifi/internal/state"
)

// provisionedTables are the setstate keys the session echoes back. They are
// recorded so the echo survives restarts; they are never applied to hardware.
var provisionedTables = []string{"radio_table", "vap_table", "port_table", "port_overrides"}

// restoreSession rebuilds an inform session from persisted state.
//
// inform.Session keeps its adoption state private, so the restore replays the
// controller messages that originally produced it through the public Apply
// API: a set-adopt (key and inform URL), a setparam (cfgversion, AES-GCM) and
// a setstate (provisioned tables). This keeps the upstream package unmodified;
// session_test.go pins the behaviour so an upstream change cannot silently
// break resume.
func restoreSession(desc inform.Descriptor, informURL string, st state.Device) *inform.Session {
	s := inform.NewSession(desc, informURL, st.BootTime)
	if !st.Adopted || st.AuthKey == "" {
		return s
	}
	replay(s, st.BootTime, map[string]any{
		"_type": "cmd", "cmd": "set-adopt", "key": st.AuthKey, "uri": st.InformURL,
	})
	mgmt := []string{"use_aes_gcm=" + strconv.FormatBool(st.UseAESGCM)}
	if st.CfgVersion != "" {
		mgmt = append(mgmt, "cfgversion="+st.CfgVersion)
	}
	replay(s, st.BootTime, map[string]any{"_type": "setparam", "mgmt_cfg": strings.Join(mgmt, "\n")})
	if len(st.Provisioned) > 0 {
		msg := map[string]any{"_type": "setstate", "cfgversion": st.CfgVersion}
		for k, v := range st.Provisioned {
			msg[k] = v
		}
		replay(s, st.BootTime, msg)
	}
	return s
}

func replay(s *inform.Session, now time.Time, msg map[string]any) {
	raw, _ := json.Marshal(msg) // only JSON-safe values
	s.Apply(now, raw)
}

// captureState returns prev updated with the session's current adoption
// state. cfgversion and firmware version have no getters and are read back
// from the payload the session would send.
func captureState(s *inform.Session, now time.Time, prev state.Device) state.Device {
	next := prev
	next.Adopted = s.Adopted()
	next.UseAESGCM = s.UseAESGCM()
	next.InformURL = s.InformURL()
	next.AuthKey = ""
	if next.Adopted {
		next.AuthKey = s.AuthKey()
	}
	var p struct {
		CfgVersion string `json:"cfgversion"`
		Version    string `json:"version"`
	}
	if err := json.Unmarshal(s.BuildPayload(now), &p); err == nil {
		next.CfgVersion = p.CfgVersion
		next.FirmwareVersion = p.Version
	}
	if !next.Adopted {
		next.Provisioned = nil
		next.BootstrapInformURL = ""
		next.SwitchConfig, next.SwitchConfigVersion, next.AckedCfgVersion = "", "", ""
	}
	return next
}

// recordProvisioned merges the tables of a setstate reply into st. The map
// is replaced, never mutated, because st is a copy that shares it with the
// previous state the caller compares against.
func recordProvisioned(st *state.Device, reply map[string]json.RawMessage) bool {
	merged := maps.Clone(st.Provisioned)
	if merged == nil {
		merged = map[string]json.RawMessage{}
	}
	changed := false
	for _, k := range provisionedTables {
		if v, ok := reply[k]; ok && string(merged[k]) != string(v) {
			merged[k] = v
			changed = true
		}
	}
	if changed {
		st.Provisioned = merged
	}
	return changed
}

// sameState reports whether two device states would persist identically.
// encoding/json sorts map keys, so equal states encode to equal bytes.
func sameState(a, b state.Device) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ra) == string(rb)
}
