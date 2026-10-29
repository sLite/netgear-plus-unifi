package device

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"

	"netgear-plus-unifi/internal/state"
)

// TestRestoreSessionRoundTrip pins the replay-based restore against the
// upstream inform.Session: if a future unifi-emu changes how Apply handles
// set-adopt, setparam or setstate, this fails instead of devices silently
// falling back to pending after a restart.
func TestRestoreSessionRoundTrip(t *testing.T) {
	boot := time.Unix(1_800_000_000, 0)
	desc := inform.Descriptor{MAC: "02:4e:47:00:00:01", Model: "US8", Type: "usw", Version: "7.4.1.16850"}
	saved := state.Device{
		Adopted: true, AuthKey: adoptKey, InformURL: "http://192.0.2.1:8080/inform",
		UseAESGCM: true, CfgVersion: "abc123", FirmwareVersion: desc.Version, BootTime: boot,
		Provisioned: map[string]json.RawMessage{"port_overrides": json.RawMessage(`[{"port_idx":1}]`)},
	}
	s := restoreSession(desc, "http://configured:8080/inform", saved)

	if !s.Adopted() || s.AuthKey() != adoptKey || !s.UseAESGCM() || s.InformURL() != saved.InformURL {
		t.Fatalf("restored session: adopted=%v key=%v gcm=%v url=%v", s.Adopted(), s.AuthKey() == adoptKey, s.UseAESGCM(), s.InformURL())
	}
	now := boot.Add(time.Hour)
	var p map[string]any
	if err := json.Unmarshal(s.BuildPayload(now), &p); err != nil {
		t.Fatal(err)
	}
	if p["cfgversion"] != "abc123" || p["uptime"] != float64(3600) || p["port_overrides"] == nil {
		t.Errorf("payload after restore: cfgversion=%v uptime=%v overrides=%v", p["cfgversion"], p["uptime"], p["port_overrides"])
	}

	captured := captureState(s, now, saved)
	if !sameState(captured, saved) {
		t.Errorf("capture after restore differs from saved state:\n got %+v\nwant %+v", captured, saved)
	}
}

func TestRestoreUnadoptedKeepsDefaults(t *testing.T) {
	s := restoreSession(inform.Descriptor{MAC: "02:4e:47:00:00:01", Type: "usw"}, "http://c/inform", state.Device{})
	if s.Adopted() || s.AuthKey() != inform.DefaultKey || s.InformURL() != "http://c/inform" {
		t.Error("an empty state must yield a fresh pending session")
	}
}
