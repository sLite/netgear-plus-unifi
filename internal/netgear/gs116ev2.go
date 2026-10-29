package netgear

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/telemetry"
)

// gs116ev2 uses the JGS page set: JavaScript records in config/*.htm pages
// and form posts guarded by a per-page secureRand. It allows exactly one
// session; a web UI login displaces ours.
type gs116ev2 struct{}

const (
	jgsSwitchInfo = "config/status_switch_info.htm"
	jgsPortStatus = "config/status_status.htm"
	jgsStatistics = "config/monitoring_port_statistics.htm"
	jgsVLANConf   = "config/dot1based_advanced_vlan_conf.htm"
	jgsVLANMember = "config/dot1based_advanced_vlan_membership.htm"
	jgsPVID       = "config/dot1based_advanced_pvid.htm"
)

func (gs116ev2) name() string             { return "GS116Ev2" }
func (gs116ev2) ports() int               { return 16 }
func (gs116ev2) verifiedFirmware() string { return "2.6.0.50" }

// Single session: logging in again would log out a web UI user.
func (gs116ev2) reloginOnExpiry() bool { return false }

func (gs116ev2) identify(p loginPage) bool { return p.rand == "" && p.sysModel == "GS116Ev2" }

func (gs116ev2) caps() provision.Capabilities {
	return provision.Capabilities{
		Speeds: []provision.Speed{provision.SpeedAuto, provision.SpeedDisabled, provision.Speed10Half,
			provision.Speed10Full, provision.Speed100Half, provision.Speed100Full},
		MaxVLANs: 100,
	}
}

func (gs116ev2) login(ctx context.Context, s *session, password string) error {
	s.cookie = ""
	resp, err := s.do(ctx, "POST", "login.htm", []field{
		{"submitId", "pwdLogin"}, {"password", hexHMACMD5(password)}, {"submitEnd", ""},
	})
	if err != nil {
		return err
	}
	if !s.takeCookie(resp, "SID") {
		return loginFailure(resp.body)
	}
	return nil
}

// logout submits the web UI's logout form (the index page's "logoutBtn").
func (gs116ev2) logout(ctx context.Context, s *session) {
	page, err := s.get(ctx, "index.htm")
	if err != nil {
		return
	}
	rand, err := jgsVar(string(page), "secureRand")
	if err != nil {
		return
	}
	_, _ = s.post(ctx, "index.htm", field{"submitId", "logoutBtn"}, field{"secureRand", rand}, field{"submitEnd", ""})
}

func (gs116ev2) read(ctx context.Context, s *session, withMetadata bool) (*metadata, []telemetry.PortReading, error) {
	var meta *metadata
	if withMetadata {
		page, err := s.get(ctx, jgsSwitchInfo)
		if err != nil {
			return nil, nil, err
		}
		info, err := parseJGSSystemInfo(string(page))
		if err != nil {
			return nil, nil, err
		}
		meta = &metadata{firmware: info.firmware, serial: info.serial, mac: hardwareMAC(info.mac)}
	}
	status, err := s.get(ctx, jgsPortStatus)
	if err != nil {
		return nil, nil, err
	}
	stats, err := s.get(ctx, jgsStatistics)
	if err != nil {
		return nil, nil, err
	}
	ports, err := jgsPorts(string(status), string(stats))
	return meta, ports, err
}

var (
	jgsPortEntry  = regexp.MustCompile(`portConfigEntry\[(\d+)\] = '([^']*)';`)
	jgsStatsEntry = regexp.MustCompile(`StatisticsEntry\[(\d+)\] = '([^']*)';`)
)

// jgsEntries returns the '?'-separated records of a JavaScript array,
// keyed by array index + 1 (the port number).
func jgsEntries(re *regexp.Regexp, page string) map[int][]string {
	out := map[int][]string{}
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		idx, _ := strconv.Atoi(m[1])
		out[idx+1] = strings.Split(m[2], "?")
	}
	return out
}

// jgsPorts combines portConfigEntry records (port?description?state?
// mode?link speed?flow control) with StatisticsEntry records
// (port?rx bytes?tx bytes?CRC errors). The GS116Ev2 does not report duplex.
func jgsPorts(status, stats string) ([]telemetry.PortReading, error) {
	entries := jgsEntries(jgsPortEntry, status)
	counters := jgsEntries(jgsStatsEntry, stats)
	ports := make([]telemetry.PortReading, 0, len(entries))
	for i := 1; i <= len(entries); i++ {
		f, ok := entries[i]
		if !ok || len(f) < 5 {
			return nil, fmt.Errorf("port %d missing from the status page", i)
		}
		up := connectedStates[f[2]]
		mode := strings.ToLower(strings.TrimSpace(f[3]))
		r := telemetry.PortReading{Index: i, LinkUp: up, Name: strings.TrimSpace(f[1]), SpeedMbps: ptr(0)}
		if up {
			r.SpeedMbps = parseLinkSpeed(f[4])
		}
		if mode != "" {
			r.Autoneg, r.Enabled = ptr(mode == "auto"), ptr(mode != "disable")
		}
		c, ok := counters[i]
		if !ok || len(c) < 4 {
			return nil, fmt.Errorf("port %d missing from the statistics page", i)
		}
		var err error
		if r.RXBytes, err = parseUint(c[1]); err == nil {
			if r.TXBytes, err = parseUint(c[2]); err == nil {
				r.CRCErrors, err = parseUint(c[3])
			}
		}
		if err != nil {
			return nil, fmt.Errorf("port %d statistics: %w", i, err)
		}
		ports = append(ports, r)
	}
	if len(counters) != len(entries) {
		return nil, fmt.Errorf("statistics page has %d ports, status page %d", len(counters), len(entries))
	}
	return ports, nil
}

// jgsSystemInfo is the sysGeneInfor record:
// model?name?MAC?firmware?DHCP?IP?netmask?gateway?serial.
type jgsSystemInfo struct {
	name, mac, firmware, serial string
	dhcp, ip, netmask, gateway  string // DHCP is "Enable" or "Disable"
}

func parseJGSSystemInfo(page string) (jgsSystemInfo, error) {
	raw, err := jgsVar(page, "sysGeneInfor")
	if err != nil {
		return jgsSystemInfo{}, err
	}
	f := strings.Split(raw, "?")
	if len(f) < 9 || (f[4] != "Enable" && f[4] != "Disable") {
		return jgsSystemInfo{}, fmt.Errorf("switch info record has %d fields, not understood", len(f))
	}
	return jgsSystemInfo{
		name: strings.ReplaceAll(f[1], "@#q", "?"), mac: f[2], firmware: f[3], serial: f[8],
		dhcp: f[4], ip: f[5], netmask: f[6], gateway: f[7],
	}, nil
}

func jgsVar(page, name string) (string, error) {
	m := regexp.MustCompile(`var ` + regexp.QuoteMeta(name) + ` = '([^']*)';`).FindStringSubmatch(page)
	if m == nil {
		return "", fmt.Errorf("page has no %s", name)
	}
	return m[1], nil
}

// Writes (JGS form protocol, from the switch's web-UI code).

var jgsSpeedCode = map[provision.Speed]string{
	provision.SpeedDisabled: "0", provision.SpeedAuto: "1", provision.Speed10Half: "2",
	provision.Speed10Full: "3", provision.Speed100Half: "4", provision.Speed100Full: "5",
}

// jgsUnchanged leaves a setting (flow control) as it is.
const jgsUnchanged = "255"

func (gs116ev2) backend(s *session) provision.Backend { return &jgsWriter{s: s} }

type jgsWriter struct{ s *session }

func (w *jgsWriter) getText(ctx context.Context, path string) (string, error) {
	page, err := w.s.get(ctx, path)
	return string(page), err
}

func (w *jgsWriter) ReadConfig(ctx context.Context) (*provision.Config, error) {
	page, err := w.getText(ctx, jgsVLANMember)
	if err != nil {
		return nil, err
	}
	cfg := provision.NewConfig()
	if err := parseJGSVLANs(page, cfg); err != nil {
		return nil, err
	}
	info, err := w.getText(ctx, jgsSwitchInfo)
	if err != nil {
		return nil, err
	}
	system, err := parseJGSSystemInfo(info)
	if err != nil {
		return nil, err
	}
	cfg.SwitchName = provision.SetName(system.name)
	status, err := w.getText(ctx, jgsPortStatus)
	if err != nil {
		return nil, err
	}
	for port, f := range jgsEntries(jgsPortEntry, status) {
		if len(f) >= 4 {
			cfg.Ports[port] = provision.PortSettings{Speed: jgsSpeed(f[3]), Name: provision.SetName(f[1])}
		}
	}
	return cfg, nil
}

// parseJGSVLANs reads vlanMem ("vid?U?T?..." per VLAN, comma-separated) and
// pvid ("1?1?..." per port) from the membership page.
func parseJGSVLANs(page string, cfg *provision.Config) error {
	vlans, err := jgsVar(page, "vlanMem")
	if err != nil {
		return err
	}
	for entry := range strings.SplitSeq(vlans, ",") {
		f := strings.Split(entry, "?")
		vid, err := strconv.Atoi(f[0])
		if err != nil {
			return errors.New("VLAN membership page not understood (advanced 802.1Q enabled?)")
		}
		members := provision.Members{}
		for i, code := range f[1:] {
			if m := provision.Member(code); m == provision.Untagged || m == provision.Tagged {
				members[i+1] = m
			}
		}
		cfg.VLANs[vid] = members
	}
	pvids, err := jgsVar(page, "pvid")
	if err != nil {
		return err
	}
	for i, v := range strings.Split(pvids, "?") {
		if vid, err := strconv.Atoi(v); err == nil {
			cfg.PVIDs[i+1] = vid
		}
	}
	return nil
}

func jgsSpeed(text string) provision.Speed {
	t := strings.ToLower(strings.ReplaceAll(text, " ", ""))
	full := strings.Contains(t, "full")
	switch {
	case strings.Contains(t, "disable"):
		return provision.SpeedDisabled
	case strings.Contains(t, "auto"):
		return provision.SpeedAuto
	case strings.HasPrefix(t, "100") && full:
		return provision.Speed100Full
	case strings.HasPrefix(t, "100"):
		return provision.Speed100Half
	case strings.HasPrefix(t, "10") && full:
		return provision.Speed10Full
	case strings.HasPrefix(t, "10"):
		return provision.Speed10Half
	}
	return provision.SpeedAuto
}

// formPost submits a JGS form: submitId first, the page's current
// secureRand, the fields, submitEnd last (the firmware relies on the order).
func (w *jgsWriter) formPost(ctx context.Context, page, submitID string, fields ...field) error {
	text, err := w.getText(ctx, page)
	if err != nil {
		return err
	}
	rand, err := jgsVar(text, "secureRand")
	if err != nil {
		return err
	}
	form := append([]field{{"submitId", submitID}, {"secureRand", rand}}, fields...)
	_, err = w.s.post(ctx, page, append(form, field{"submitEnd", ""})...)
	return err
}

func (w *jgsWriter) CreateVLAN(ctx context.Context, vid int, _ provision.Members) error {
	// changeType must stay 0: changeType=1 switches the VLAN mode and
	// resets the whole VLAN configuration. Members are set afterwards.
	return w.formPost(ctx, jgsVLANConf, "vlanDot1VidCfg",
		field{"changeType", "0"}, field{"addVid", strconv.Itoa(vid)}, field{"confirmOK", "0"})
}

func (w *jgsWriter) WriteMembers(ctx context.Context, vid int, members provision.Members) error {
	var member, tag uint64
	for port, m := range members {
		member |= 1 << port
		if m == provision.Tagged {
			tag |= 1 << port
		}
	}
	return w.formPost(ctx, jgsVLANMember, "vlanDot1TagCfg",
		field{"vid", strconv.Itoa(vid)},
		field{"member", strconv.FormatUint(member, 10)},
		field{"tag", strconv.FormatUint(tag, 10)})
}

func (w *jgsWriter) DeleteVLAN(ctx context.Context, vid int) error {
	return w.formPost(ctx, jgsVLANConf, "vlanDot1VidCfg", field{"changeType", "0"}, field{"delVid", strconv.Itoa(vid)})
}

func (w *jgsWriter) SetPVID(ctx context.Context, port, vid int) error {
	return w.formPost(ctx, jgsPVID, "vlanDot1PvidCfg", field{"port", strconv.Itoa(port)}, field{"pvid", strconv.Itoa(vid)})
}

func (w *jgsWriter) SetPort(ctx context.Context, op provision.SetPort) error {
	code, ok := jgsSpeedCode[op.Speed]
	if !ok {
		return fmt.Errorf("%w: speed %s not supported", provision.ErrForbidden, op.Speed)
	}
	return w.formPost(ctx, jgsPortStatus, "sysStatus",
		field{"speed", code}, field{"flow", jgsUnchanged}, field{"description", op.Name}, field{"port", strconv.Itoa(op.Port)})
}

// SetSwitchName posts the name. The GS116Ev2 has no separate name form: the
// name shares a form with the management IP settings, which are echoed
// exactly as the switch reports them (what its own UI does for a name-only
// change) and verified afterwards.
func (w *jgsWriter) SetSwitchName(ctx context.Context, name string) error {
	page, err := w.getText(ctx, jgsSwitchInfo)
	if err != nil {
		return err
	}
	before, err := parseJGSSystemInfo(page)
	if err != nil {
		return err
	}
	rand, err := jgsVar(page, "secureRand")
	if err != nil {
		return err
	}
	ip := []field{{"dhcp", "2"}} // 2 = keep DHCP on
	if before.dhcp != "Enable" {
		ip = []field{{"dhcp", "0"}, {"ip", before.ip}, {"subnet", before.netmask}, {"gateway", before.gateway}}
	}
	form := append([]field{{"submitId", "sysIp"}, {"secureRand", rand}, {"swName", name}}, ip...)
	if _, err := w.s.post(ctx, jgsSwitchInfo, append(form, field{"submitEnd", ""})...); err != nil {
		return err
	}
	afterPage, err := w.getText(ctx, jgsSwitchInfo)
	if err != nil {
		return err
	}
	after, err := parseJGSSystemInfo(afterPage)
	if err != nil {
		return err
	}
	if after.dhcp != before.dhcp || after.ip != before.ip || after.netmask != before.netmask || after.gateway != before.gateway {
		return fmt.Errorf("management IP settings changed by the name update (%s %s/%s gw %s -> %s %s/%s gw %s)",
			before.dhcp, before.ip, before.netmask, before.gateway, after.dhcp, after.ip, after.netmask, after.gateway)
	}
	return nil
}
