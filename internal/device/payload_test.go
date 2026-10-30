package device

import (
	"encoding/json"
	"testing"

	"netgear-plus-unifi/internal/telemetry"
)

func ptr[T any](v T) *T { return &v }

// testSnapshot mirrors a real GS308Ev4 reading: port 1 up at 1 Gbit/s.
func testSnapshot() *telemetry.Snapshot {
	snap := &telemetry.Snapshot{Device: telemetry.Device{
		Model: "GS308Ev4", Firmware: "V1.0.1.9", Serial: "SERIAL308TEST",
		ManagementIP: "192.168.1.247", PortCount: 8,
	}}
	for i := 1; i <= 8; i++ {
		p := telemetry.Port{PortReading: telemetry.PortReading{Index: i, SpeedMbps: ptr(0), Enabled: ptr(true),
			Autoneg: ptr(true), RXBytes: ptr(uint64(0)), TXBytes: ptr(uint64(0)), CRCErrors: ptr(uint64(0))}}
		if i == 1 {
			p.LinkUp, p.SpeedMbps, p.FullDuplex = true, ptr(1000), ptr(true)
			p.RXBytes, p.TXBytes = ptr(uint64(1866737)), ptr(uint64(8704593))
			p.RXBytesPerS, p.TXBytesPerS = ptr(100.0), ptr(250.0)
		}
		snap.Ports = append(snap.Ports, p)
	}
	return snap
}

func overlay(t *testing.T, in overlayInput) map[string]any {
	t.Helper()
	base := `{"mac":"02:4e:47:00:00:01","default":false,"state":4,"ip":"","sys_stats":{"cpu":1.5},` +
		`"system-stats":{},"port_table":[{"port_idx":1,"up":true,"speed":1000}],"ethernet_table":[]}`
	raw, err := overlayPayload([]byte(base), in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func row(m map[string]any, i int) map[string]any {
	return m["port_table"].([]any)[i].(map[string]any)
}

func TestOverlayReportsLiveStateAndOmitsUnknowns(t *testing.T) {
	snap := testSnapshot()
	snap.Ports[2].LinkUp, snap.Ports[2].SpeedMbps = true, ptr(100) // duplex unknown at 100M
	snap.Ports[3].RXBytes, snap.Ports[3].RXBytesPerS = nil, nil
	m := overlay(t, overlayInput{Adopted: true, MAC: "02:4e:47:00:00:01", IP: "192.168.1.247", Snapshot: snap})

	if _, ok := m["sys_stats"]; ok {
		t.Error("sys_stats placeholder must be removed")
	}
	if m["ip"] != "192.168.1.247" {
		t.Errorf("ip = %v", m["ip"])
	}
	eth := m["ethernet_table"].([]any)[0].(map[string]any)
	if eth["num_port"] != float64(8) {
		t.Errorf("ethernet_table num_port = %v", eth["num_port"])
	}
	p1 := row(m, 0)
	want := map[string]any{"up": true, "speed": 1000.0, "full_duplex": true, "rx_bytes": 1866737.0,
		"tx_bytes": 8704593.0, "rx_bytes-r": 100.0, "tx_bytes-r": 250.0, "bytes-r": 350.0,
		"poe_caps": 0.0, "port_poe": false, "name": "Port 1", "ifname": "eth0"}
	for k, v := range want {
		if p1[k] != v {
			t.Errorf("port 1 %s = %v, want %v", k, p1[k], v)
		}
	}
	p2 := row(m, 1)
	if p2["up"] != false || p2["speed"] != 0.0 {
		t.Errorf("port 2 should be down: %v", p2)
	}
	if _, ok := row(m, 2)["full_duplex"]; ok {
		t.Error("unknown duplex below 1 Gbit/s must be omitted")
	}
	if _, ok := row(m, 3)["rx_bytes"]; ok {
		t.Error("unknown counters must be omitted, not reported as 0")
	}
	if _, ok := p2["mac_table"]; ok {
		t.Error("client tables must never be fabricated")
	}
}

func TestOverlayLeavesPendingPayloadWithoutTables(t *testing.T) {
	m := overlay(t, overlayInput{Adopted: false, Snapshot: testSnapshot()})
	if rows := m["port_table"].([]any); len(rows) != 1 {
		t.Errorf("pending payload must not be rebuilt, got %d rows", len(rows))
	}
	if _, ok := m["sys_stats"]; ok {
		t.Error("sys_stats placeholder must be removed in every state")
	}
}

func TestOverlayKeepsProvisionedFieldsButLiveStateWins(t *testing.T) {
	prov := map[string]json.RawMessage{"port_table": json.RawMessage(
		`[{"port_idx":1,"name":"uplink","portconf_id":"abc","up":false,"speed":10}]`)}
	m := overlay(t, overlayInput{Adopted: true, Snapshot: testSnapshot(), Provisioned: prov})
	p1 := row(m, 0)
	if p1["name"] != "uplink" || p1["portconf_id"] != "abc" {
		t.Errorf("provisioned fields lost: %v", p1)
	}
	if p1["up"] != true || p1["speed"] != 1000.0 {
		t.Errorf("live link state must override provisioned values: %v", p1)
	}
}

func TestPortNameFromSwitch(t *testing.T) {
	snap := testSnapshot()
	snap.Ports[0].Name = "  NAS "
	if got := row(overlay(t, overlayInput{Adopted: true, Snapshot: snap}), 0)["name"]; got != "NAS" {
		t.Errorf("name = %v", got)
	}
}

func TestOverlayDeclaredDownlinks(t *testing.T) {
	m := overlay(t, overlayInput{Adopted: true, Snapshot: testSnapshot(),
		Downlinks: []neighbor{{Port: 3, MAC: "02:4e:47:00:00:05"}}})
	mt := row(m, 2)["mac_table"].([]any)
	if len(mt) != 1 || mt[0].(map[string]any)["mac"] != "02:4e:47:00:00:05" {
		t.Errorf("declared downlink missing from mac_table: %v", mt)
	}

	plain := overlay(t, overlayInput{Adopted: true, Snapshot: testSnapshot(),
		Provisioned: map[string]json.RawMessage{"port_table": json.RawMessage(`[{"port_idx":3,"mac_table":[{"mac":"aa:aa:aa:aa:aa:aa"}]}]`)}})
	if _, ok := row(plain, 2)["mac_table"]; ok {
		t.Error("mac_table must only ever contain declared switches")
	}
}

func TestOverlayDeclaredUplink(t *testing.T) {
	const gw = "8c:30:66:00:00:01"
	m := overlay(t, overlayInput{Adopted: true, MAC: "28:94:01:00:00:01", Snapshot: testSnapshot(),
		Uplink: &uplink{Port: 1, MAC: gw, RemotePort: 2}})
	if row(m, 0)["is_uplink"] != true || row(m, 1)["is_uplink"] != false {
		t.Errorf("is_uplink: port 1 %v, port 2 %v", row(m, 0)["is_uplink"], row(m, 1)["is_uplink"])
	}
	if _, ok := row(m, 0)["mac_table"]; ok {
		t.Error("the upstream MAC must not be in mac_table: UniFi reads it as a downlink")
	}
	lldp, _ := m["lldp_table"].([]any)
	if len(lldp) != 1 {
		t.Fatalf("lldp_table = %v", m["lldp_table"])
	}
	if e := lldp[0].(map[string]any); e["chassis_id"] != gw || e["port_id"] != gw || e["local_port_idx"] != 1.0 {
		t.Errorf("lldp entry = %v", e)
	}
	up, _ := m["uplink"].(map[string]any)
	if up["uplink_mac"] != gw || up["uplink_remote_port"] != 2.0 || up["port_idx"] != 1.0 ||
		up["up"] != true || up["speed"] != 1000.0 || up["mac"] != "28:94:01:00:00:01" {
		t.Errorf("uplink = %v", up)
	}

	plain := overlay(t, overlayInput{Adopted: true, Snapshot: testSnapshot()})
	if _, ok := plain["uplink"]; ok || plain["lldp_table"] != nil || row(plain, 0)["is_uplink"] != nil {
		t.Error("nothing about an uplink may be reported unless declared")
	}
}

func TestOverlaySpeedCaps(t *testing.T) {
	snap := testSnapshot()
	snap.Device.PortSpeeds = []string{"auto", "10f", "100f", "1000f"}
	if got := row(overlay(t, overlayInput{Adopted: true, Snapshot: snap}), 0)["speed_caps"]; got != float64(1048618) {
		t.Errorf("GS308Ev4 speed_caps = %v, want 1048618 (auto, 10/100/1000 full)", got)
	}
	snap.Device.PortSpeeds = []string{"auto", "10h", "10f", "100h", "100f", "1000f"}
	if got := row(overlay(t, overlayInput{Adopted: true, Snapshot: snap}), 0)["speed_caps"]; got != float64(1048623) {
		t.Errorf("GS116Ev2 speed_caps = %v, want 1048623", got)
	}
	if _, ok := row(overlay(t, overlayInput{Adopted: true, Snapshot: testSnapshot()}), 0)["speed_caps"]; ok {
		t.Error("speed_caps must be omitted when the switch's speeds are unknown")
	}
}
