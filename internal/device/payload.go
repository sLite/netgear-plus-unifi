package device

import (
	"encoding/json"
	"fmt"
	"strings"

	"netgear-plus-unifi/internal/telemetry"
)

// fabricatedKeys are payload fields the upstream session fills with constant
// placeholder values (CPU 1.5 %, 50 % memory). NETGEAR Plus switches do not
// report CPU or memory, so the fields are dropped rather than shown as fact.
var fabricatedKeys = []string{"sys_stats", "system-stats"}

// livePortKeys are the port_table keys owned by telemetry. They always win
// over anything the controller provisioned for the same port.
var livePortKeys = []string{
	"port_idx", "ifname", "media", "poe_caps", "port_poe", "op_mode",
	"up", "enable", "speed", "full_duplex", "autoneg",
	"rx_bytes", "tx_bytes", "rx_bytes-r", "tx_bytes-r", "bytes-r", "rx_errors", "mac_table",
	"speed_caps", "is_uplink",
}

// speedCapBits are UniFi's port speed capability bits (SPEED_CAPS in the
// Network application UI). The port editor offers exactly the speeds whose
// bit is set, and shows the highest one as the port's media speed.
var speedCapBits = map[string]int{
	"10h": 1, "10f": 2, "100h": 4, "100f": 8, "1000f": 32, "auto": 1 << 20,
}

// speedCaps converts the switch's speed list to UniFi's bitmask; 0 means
// unknown, in which case the controller falls back to its model database.
func speedCaps(speeds []string) int {
	caps := 0
	for _, s := range speeds {
		caps |= speedCapBits[s]
	}
	return caps
}

// overlayInput is everything the overlay needs for one inform.
type overlayInput struct {
	// CfgVersion, when set, replaces the session's cfgversion: the version
	// reported as applied lags while the switch is being provisioned.
	CfgVersion  string
	Adopted     bool
	MAC         string
	IP          string
	Snapshot    *telemetry.Snapshot
	Downlinks   []neighbor
	Uplink      *uplink
	Provisioned map[string]json.RawMessage
}

// neighbor is a declared downstream emulated switch on a port.
type neighbor struct {
	Port int
	MAC  string
}

// uplink is the declared upstream device: Port of this switch is cabled to
// RemotePort of the device with MAC.
type uplink struct {
	Port       int
	MAC        string
	RemotePort int
}

// overlayPayload replaces the session's static switch tables with live
// telemetry. base is the JSON the inform session built; the result keeps
// every protocol field (keys, cfgversion, state) and swaps in real port data.
// Pending (unadopted) payloads only get their fabricated fields removed: the
// controller ignores tables until adoption.
func overlayPayload(base []byte, in overlayInput) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, fmt.Errorf("decode session payload: %w", err)
	}
	for _, k := range fabricatedKeys {
		delete(m, k)
	}
	if in.IP != "" {
		m["ip"] = in.IP
	}
	if in.CfgVersion != "" {
		m["cfgversion"] = in.CfgVersion
	}
	if in.Adopted && in.Snapshot != nil {
		ports := buildPortTable(in.Snapshot, provisionedPorts(in.Provisioned))
		addDeclaredDownlinks(ports, in.Downlinks)
		if in.Uplink != nil {
			addDeclaredUplink(m, ports, *in.Uplink, in.MAC)
		}
		m["port_table"] = ports
		m["ethernet_table"] = []map[string]any{{
			"mac": in.MAC, "name": "eth0", "num_port": len(ports),
		}}
	}
	return json.Marshal(m)
}

// addDeclaredUplink reports the declared upstream device the way a UniFi
// switch reports what it learned via LLDP: an lldp_table neighbour, the
// uplink object and is_uplink on the port, with live link data. The
// upstream MAC is deliberately not put in the port's mac_table: UniFi
// (Network 10.x on a DR7) reads a device MAC there as a downstream device
// and drew the gateway as a downlink of the switch.
func addDeclaredUplink(m map[string]any, ports []map[string]any, up uplink, ownMAC string) {
	var port map[string]any
	for _, p := range ports {
		isUplink := p["port_idx"] == up.Port
		p["is_uplink"] = isUplink
		if isUplink {
			port = p
		}
	}
	if port == nil {
		return // declared port does not exist on this switch
	}
	// Same shape as the entry a UniFi AP reports for a UniFi gateway
	// (captured from a DR7 site): the gateway's chassis and port ids are its
	// device MAC.
	m["lldp_table"] = []map[string]any{{
		"chassis_id": up.MAC, "chassis_id_subtype": "local", "port_id": up.MAC,
		"local_port_idx": up.Port, "local_port_name": port["ifname"], "is_wired": true,
	}}
	link := map[string]any{
		"type": "wire", "port_idx": up.Port, "name": port["ifname"], "mac": ownMAC,
		"media": "GE", "max_speed": 1000, "num_port": len(ports),
		"uplink_mac": up.MAC, "uplink_remote_port": up.RemotePort,
	}
	for _, k := range []string{"up", "speed", "full_duplex", "rx_bytes", "tx_bytes"} {
		if v, ok := port[k]; ok {
			link[k] = v
		}
	}
	m["uplink"] = link
}

// addMACEntry adds a learned-looking forwarding entry for mac to a port.
func addMACEntry(port map[string]any, mac string) {
	table, _ := port["mac_table"].([]map[string]any)
	port["mac_table"] = append(table, map[string]any{
		"mac": mac, "age": 4, "uptime": 1200, "vlan": 1, "static": false,
	})
}

// buildPortTable renders one port_table row per physical port. Values the
// switch does not report are omitted rather than guessed, with one
// exception: a gigabit link is reported full duplex, because 1000BASE-T
// half duplex does not occur in practice and UniFi needs the flag.
func buildPortTable(snap *telemetry.Snapshot, provisioned map[int]map[string]any) []map[string]any {
	rows := make([]map[string]any, 0, len(snap.Ports))
	caps := speedCaps(snap.Device.PortSpeeds)
	for _, p := range snap.Ports {
		row := map[string]any{}
		for k, v := range provisioned[p.Index] {
			row[k] = v
		}
		for _, k := range livePortKeys {
			delete(row, k)
		}
		row["port_idx"] = p.Index
		row["ifname"] = fmt.Sprintf("eth%d", p.Index-1)
		row["media"] = "GE"
		row["poe_caps"] = 0
		row["port_poe"] = false
		row["op_mode"] = "switch"
		if caps != 0 {
			row["speed_caps"] = caps
		}
		if _, named := row["name"]; !named {
			row["name"] = portName(p)
		}
		applyLinkFields(row, p)
		applyCounterFields(row, p)
		rows = append(rows, row)
	}
	return rows
}

func portName(p telemetry.Port) string {
	if n := strings.TrimSpace(p.Name); n != "" {
		return n
	}
	return fmt.Sprintf("Port %d", p.Index)
}

func applyLinkFields(row map[string]any, p telemetry.Port) {
	row["up"] = p.LinkUp
	row["enable"] = p.Enabled == nil || *p.Enabled
	speed := 0
	if p.LinkUp && p.SpeedMbps != nil {
		speed = *p.SpeedMbps
	}
	row["speed"] = speed
	switch {
	case p.FullDuplex != nil:
		row["full_duplex"] = *p.FullDuplex && p.LinkUp
	case p.LinkUp && speed >= 1000:
		row["full_duplex"] = true
	}
	if p.Autoneg != nil {
		row["autoneg"] = *p.Autoneg
	}
}

func applyCounterFields(row map[string]any, p telemetry.Port) {
	if p.RXBytes != nil {
		row["rx_bytes"] = *p.RXBytes
	}
	if p.TXBytes != nil {
		row["tx_bytes"] = *p.TXBytes
	}
	if p.CRCErrors != nil {
		row["rx_errors"] = *p.CRCErrors
	}
	if p.RXBytesPerS != nil && p.TXBytesPerS != nil {
		row["rx_bytes-r"] = *p.RXBytesPerS
		row["tx_bytes-r"] = *p.TXBytesPerS
		row["bytes-r"] = *p.RXBytesPerS + *p.TXBytesPerS
	}
}

// addDeclaredDownlinks reports declared downstream switches in the port's
// mac_table, the forwarding-table evidence the controller uses to build its
// downlink topology. The age/uptime values mirror what the controller expects
// from a learned entry; only declared neighbours are ever added to mac_table.
func addDeclaredDownlinks(ports []map[string]any, downlinks []neighbor) {
	for _, dl := range downlinks {
		for _, p := range ports {
			if p["port_idx"] == dl.Port {
				addMACEntry(p, dl.MAC)
			}
		}
	}
}

// provisionedPorts indexes a controller-provisioned port_table by port_idx.
// Malformed or absent tables yield an empty index.
func provisionedPorts(provisioned map[string]json.RawMessage) map[int]map[string]any {
	out := map[int]map[string]any{}
	raw, ok := provisioned["port_table"]
	if !ok {
		return out
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return out
	}
	for _, r := range rows {
		if idx, ok := r["port_idx"].(float64); ok {
			out[int(idx)] = r
		}
	}
	return out
}
