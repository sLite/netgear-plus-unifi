package provision

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Parsing the switch section of UniFi's setparam.system_cfg.
//
// Observed on UniFi Network 10.6.109 (see docs/captures/):
//
//	switch.vlan.<n>.id=<vid>                  VLAN table entry n
//	switch.vlan.<n>.mode=tagged|untagged      membership of ports without an override
//	switch.vlan.<n>.port.<p>.mode=untagged|tagged|exclude
//	switch.port.<p>.pvid=<vid>                native VLAN (absent: 1)
//	switch.port.<p>.status=disabled
//	switch.port.<p>.autoneg=disabled, .speed=<mbps>, .duplex=enabled|disabled
//	switch.port.<p>.name=<text>
//	resolv.host.1.name=<hostname>             device name, hostname-safe (spaces removed)
//
// Everything else in system_cfg (users, sshd, ntp, management VLAN, IP
// settings, MTU...) is ignored on purpose: it is not switch port
// configuration and some of it must never reach a switch.

// HostnameKey carries the UniFi device name.
const HostnameKey = "resolv.host.1.name"

var (
	vlanKey     = regexp.MustCompile(`^switch\.vlan\.(\d+)\.(id|mode)$`)
	vlanPortKey = regexp.MustCompile(`^switch\.vlan\.(\d+)\.port\.(\d+)\.mode$`)
	portKey     = regexp.MustCompile(`^switch\.port\.(\d+)\.(pvid|status|autoneg|speed|duplex|name)$`)
	// secretKey matches keys that carry credentials (e.g. switch.wevent.key).
	secretKey = regexp.MustCompile(`(?i)(authkey|password|passphrase|secret|psk|token|^x_|(^|[._])key$)`)
)

var memberModes = map[string]Member{"untagged": Untagged, "tagged": Tagged, "exclude": Excluded}

type forcedSpeed struct {
	mbps int
	full bool
}

var forcedSpeeds = map[forcedSpeed]Speed{
	{10, false}: Speed10Half, {10, true}: Speed10Full,
	{100, false}: Speed100Half, {100, true}: Speed100Full,
	{1000, true}: Speed1000Full,
}

// SwitchSection keeps the lines of a system_cfg document this integration
// uses (switch.* and the device hostname), without credentials.
func SwitchSection(systemCfg string) string {
	var out []string
	for line := range strings.SplitSeq(systemCfg, "\n") {
		line = strings.TrimSpace(line)
		key, _, ok := strings.Cut(line, "=")
		if ok && (strings.HasPrefix(key, "switch.") || key == HostnameKey) && !secretKey.MatchString(key) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// IsSecretKey reports whether a configuration key carries credentials.
func IsSecretKey(key string) bool { return secretKey.MatchString(key) }

// Parsed is UniFi's configuration resolved for one switch.
type Parsed struct {
	Config   *Config
	Warnings []string
}

type unifiPort map[string]string

// ParseUniFi resolves UniFi's VLAN defaults and per-port overrides into one
// explicit membership per (VLAN, port) for ports 1..ports.
func ParseUniFi(text string, ports int) Parsed {
	vlanIDs := map[int]int{}
	vlanModes := map[int]Member{}
	overrides := map[[2]int]Member{}
	portKeys := map[int]unifiPort{}
	cfg := NewConfig()

	for raw := range strings.SplitSeq(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(raw), "=")
		if !ok {
			continue
		}
		if m := vlanKey.FindStringSubmatch(key); m != nil {
			n := atoi(m[1])
			if m[2] == "id" {
				if vid, err := strconv.Atoi(value); err == nil {
					vlanIDs[n] = vid
				}
			} else if mode, ok := memberModes[value]; ok {
				vlanModes[n] = mode
			}
		} else if m := vlanPortKey.FindStringSubmatch(key); m != nil {
			if mode, ok := memberModes[value]; ok {
				overrides[[2]int{atoi(m[1]), atoi(m[2])}] = mode
			}
		} else if m := portKey.FindStringSubmatch(key); m != nil {
			p := atoi(m[1])
			if portKeys[p] == nil {
				portKeys[p] = unifiPort{}
			}
			portKeys[p][m[2]] = value
		} else if key == HostnameKey && strings.TrimSpace(value) != "" {
			cfg.SwitchName = SetName(strings.TrimSpace(value))
		}
	}

	for n, vid := range vlanIDs {
		def, ok := vlanModes[n]
		if !ok {
			def = Tagged
		}
		members := Members{}
		for port := 1; port <= ports; port++ {
			m, ok := overrides[[2]int{n, port}]
			if !ok {
				m = def
			}
			if m != Excluded {
				members[port] = m
			}
		}
		cfg.VLANs[vid] = members
	}

	var warnings []string
	for port := 1; port <= ports; port++ {
		keys := portKeys[port]
		cfg.PVIDs[port] = DefaultVLAN
		if v, err := strconv.Atoi(keys["pvid"]); err == nil {
			cfg.PVIDs[port] = v
		}
		speed, warning := keys.speed(port)
		if warning != "" {
			warnings = append(warnings, warning)
		}
		cfg.Ports[port] = PortSettings{Speed: speed, Name: SetName(explicitName(port, keys["name"]))}
	}
	return Parsed{Config: cfg, Warnings: warnings}
}

func (keys unifiPort) speed(port int) (Speed, string) {
	if keys["status"] == "disabled" {
		return SpeedDisabled, ""
	}
	if keys["autoneg"] != "disabled" {
		return SpeedAuto, ""
	}
	mbps, err := strconv.Atoi(keys["speed"])
	if err != nil {
		return SpeedAuto, fmt.Sprintf("port %d: autoneg off without a valid speed; using auto", port)
	}
	full := keys["duplex"] != "disabled"
	if s, ok := forcedSpeeds[forcedSpeed{mbps, full}]; ok {
		return s, ""
	}
	duplex := "half"
	if full {
		duplex = "full"
	}
	return SpeedAuto, fmt.Sprintf("port %d: %d Mbit/s %s unsupported", port, mbps, duplex)
}

// explicitName maps UniFi's port name to the switch's: UniFi always sends a
// name, and its default "Port N" means "no name".
func explicitName(port int, name string) string {
	name = strings.TrimSpace(name)
	if name == fmt.Sprintf("Port %d", port) {
		return ""
	}
	return name
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
