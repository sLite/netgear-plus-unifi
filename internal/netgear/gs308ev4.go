package netgear

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/telemetry"
)

// gs308ev4 uses the GS30x page set: dashboard.cgi carries metadata and
// port status, portStatistics.cgi 64-bit counters, vlan.cgi the 802.1Q
// configuration. Every authenticated request after login carries the
// session "hash" from the dashboard.
type gs308ev4 struct{}

func (gs308ev4) name() string             { return "GS308Ev4" }
func (gs308ev4) ports() int               { return 8 }
func (gs308ev4) verifiedFirmware() string { return "V1.0.1.9" }

// Concurrent sessions are allowed, so an expired session can be restored.
func (gs308ev4) reloginOnExpiry() bool { return true }

func (gs308ev4) identify(p loginPage) bool { return p.rand != "" && p.title == "GS308Ev4" }

func (gs308ev4) caps() provision.Capabilities {
	return provision.Capabilities{
		Speeds:   []provision.Speed{provision.SpeedAuto, provision.SpeedDisabled, provision.Speed10Full, provision.Speed100Full},
		MaxVLANs: 64,
	}
}

func (gs308ev4) login(ctx context.Context, s *session, password string) error {
	page, err := s.do(ctx, "GET", "login.cgi", nil)
	if err != nil {
		return err
	}
	rand := parseHTML(page.body).first(tag("input"), attrIs("id", "rand")).val("value")
	if rand == "" {
		return errors.New("login page has no rand value")
	}
	s.cookie = ""
	resp, err := s.do(ctx, "POST", "login.cgi", []field{{"password", mergeHash(password, rand)}})
	if err != nil {
		return err
	}
	if !s.takeCookie(resp, "SID") {
		return loginFailure(resp.body)
	}
	return nil
}

func (gs308ev4) logout(ctx context.Context, s *session) {
	_, _ = s.do(ctx, "POST", "logout.cgi", nil)
}

func (gs308ev4) read(ctx context.Context, s *session, _ bool) (*metadata, []telemetry.PortReading, error) {
	dashboard, err := s.get(ctx, "dashboard.cgi")
	if err != nil {
		return nil, nil, err
	}
	doc := parseHTML(dashboard)
	hash := doc.first(tag("input"), attrIs("name", "hash")).val("value")
	if hash == "" {
		return nil, nil, errors.New("dashboard has no session hash")
	}
	stats, err := s.get(ctx, "portStatistics.cgi", field{"hash", hash})
	if err != nil {
		return nil, nil, err
	}
	ports, err := gs308Ports(doc, gs308Counters(stats))
	if err != nil {
		return nil, nil, err
	}
	return gs308Metadata(doc), ports, nil
}

// gs308Labels maps the dashboard's label ids (ml089 firmware, ml198 serial,
// ml678 MAC, ...) to their values: each label sits in a "hid_info_title"
// div followed by a div holding the value span.
func gs308Labels(doc node) map[string]string {
	out := map[string]string{}
	for _, title := range doc.all(tag("div"), attrIs("class", "hid_info_title")) {
		label := strings.TrimSpace(title.first(tag("span")).text())
		next := title.nextSiblings(tag("div"))
		if label == "" || len(next) == 0 {
			continue
		}
		if span := next[0].children(tag("span")); len(span) > 0 {
			out[label] = strings.TrimSpace(span[0].text())
		}
	}
	return out
}

func gs308Metadata(doc node) *metadata {
	labels := gs308Labels(doc)
	return &metadata{firmware: labels["ml089"], serial: labels["ml198"], mac: hardwareMAC(labels["ml678"])}
}

// gs308PortBlock is one port's block on the dashboard.
type gs308PortBlock struct {
	port   int
	status string
	fields map[string]string // hidden inputs by class
}

var gs308PortFields = []string{"port", "portName", "Speed", "LinkedSpeed", "ingressRate", "egressRate", "flowCtr"}

func gs308PortBlocks(doc node) map[int]gs308PortBlock {
	out := map[int]gs308PortBlock{}
	for _, li := range doc.all(tag("li"), classContains("list_item")) {
		b := gs308PortBlock{fields: map[string]string{}}
		for _, class := range gs308PortFields {
			if in := li.first(tag("input"), attrIs("class", class)); in.ok() {
				b.fields[class] = in.val("value")
			}
		}
		if pr := li.first(tag("input"), classContains("priority")); pr.ok() {
			b.fields["priority"] = pr.val("value")
		}
		port, err := strconv.Atoi(b.fields["port"])
		if err != nil {
			continue
		}
		b.port = port
		b.status = strings.TrimSpace(li.first(tag("span"), classContains("padding_r_18")).first(tag("span")).text())
		out[port] = b
	}
	return out
}

func gs308Ports(doc node, counters map[int]gs308Counter) ([]telemetry.PortReading, error) {
	blocks := gs308PortBlocks(doc)
	ports := make([]telemetry.PortReading, 0, len(blocks))
	for i := 1; i <= len(blocks); i++ {
		b, ok := blocks[i]
		if !ok {
			return nil, fmt.Errorf("port %d missing from the dashboard", i)
		}
		up := connectedStates[b.status]
		r := telemetry.PortReading{Index: i, LinkUp: up, Name: strings.TrimSpace(b.fields["portName"]), SpeedMbps: ptr(0)}
		if up {
			r.SpeedMbps = parseLinkSpeed(b.fields["LinkedSpeed"])
			r.FullDuplex = parseDuplex(b.fields["LinkedSpeed"])
		}
		if code, ok := b.fields["Speed"]; ok {
			r.Autoneg = ptr(code == gs308SpeedCode[provision.SpeedAuto])
			r.Enabled = ptr(code != gs308SpeedCode[provision.SpeedDisabled])
		}
		if c, ok := counters[i]; ok {
			r.RXBytes, r.TXBytes, r.CRCErrors = ptr(c.rx), ptr(c.tx), ptr(c.crc)
		}
		ports = append(ports, r)
	}
	return ports, nil
}

type gs308Counter struct{ rx, tx, crc uint64 }

// gs308Counters parses portStatistics.cgi: each port row
// <li><span>N</span>...</li> is followed by six hidden inputs holding rx,
// tx and CRC errors as (high, low) pairs of 32-bit words. py-netgear-plus
// reads only the low word, so its counters wrap every 4 GiB.
func gs308Counters(page []byte) map[int]gs308Counter {
	out := map[int]gs308Counter{}
	for _, li := range parseHTML(page).all(tag("li")) {
		spans := li.children(tag("span"))
		if len(spans) == 0 {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(spans[0].text()))
		if err != nil {
			continue
		}
		var words []uint64
		for _, in := range li.nextSiblings(tag("input"), attrIs("type", "hidden")) {
			if len(words) == 6 {
				break
			}
			v, err := strconv.ParseUint(in.val("value"), 10, 64)
			if err != nil {
				break
			}
			words = append(words, v)
		}
		if len(words) == 6 {
			out[port] = gs308Counter{rx: words[0]<<32 + words[1], tx: words[2]<<32 + words[3], crc: words[4]<<32 + words[5]}
		}
	}
	return out
}

// Writes (GS30x protocol, from the switch's web-UI code).

var gs308MemberCode = map[provision.Member]string{provision.Tagged: "1", provision.Untagged: "2", provision.Excluded: "3"}

var gs308SpeedCode = map[provision.Speed]string{
	provision.SpeedAuto: "1", provision.SpeedDisabled: "2", provision.Speed10Full: "4", provision.Speed100Full: "6",
}

var gs308SpeedFromCode = map[string]provision.Speed{
	"1": provision.SpeedAuto, "2": provision.SpeedDisabled, "3": provision.Speed10Half,
	"4": provision.Speed10Full, "5": provision.Speed100Half, "6": provision.Speed100Full,
}

// gs308PortSettle is how long a port settings change takes to apply; the
// switch restarts the port and its web UI waits as well.
var gs308PortSettle = 3 * time.Second

func (gs308ev4) backend(s *session) provision.Backend { return &gs308Writer{s: s} }

// gs308Writer keeps the form state of its last ReadConfig.
type gs308Writer struct {
	s         *session
	vlanForm  gs308VLANForm
	vlanNames map[int]string
	ports     map[int]gs308PortBlock
	lang      string
}

type gs308VLANForm struct {
	hash, vlanNum, voiceVID, cos string
}

func (w *gs308Writer) ReadConfig(ctx context.Context) (*provision.Config, error) {
	page, err := w.s.get(ctx, "vlan.cgi")
	if err != nil {
		return nil, err
	}
	doc := parseHTML(page)
	cfg := provision.NewConfig()
	if err := w.parseVLANPage(doc, cfg); err != nil {
		return nil, err
	}
	dashboard, err := w.s.get(ctx, "dashboard.cgi")
	if err != nil {
		return nil, err
	}
	ddoc := parseHTML(dashboard)
	w.lang = ddoc.first(tag("input"), attrIs("name", "SET_LANG")).val("value")
	if w.lang == "" {
		return nil, errors.New("dashboard lacks the language field of the name form")
	}
	cfg.SwitchName = provision.SetName(ddoc.first(tag("input"), attrIs("id", "switchName")).val("value"))
	w.ports = gs308PortBlocks(ddoc)
	for port, b := range w.ports {
		speed, ok := gs308SpeedFromCode[b.fields["Speed"]]
		if !ok {
			speed = provision.SpeedAuto
		}
		cfg.Ports[port] = provision.PortSettings{Speed: speed, Name: provision.SetName(b.fields["portName"])}
	}
	return cfg, nil
}

// parseVLANPage reads vlan.cgi in advanced 802.1Q mode: the selected mode,
// the VLAN table (id, name, hidden-mem membership codes), the PVID table
// and the form fields every VLAN write needs.
func (w *gs308Writer) parseVLANPage(doc node, cfg *provision.Config) error {
	w.vlanForm = gs308VLANForm{
		hash:     doc.first(tag("input"), attrIs("id", "hash")).val("value"),
		vlanNum:  doc.first(tag("input"), attrIs("id", "vlanNum")).val("value"),
		voiceVID: doc.first(tag("input"), attrIs("id", "voVid")).val("value"),
		cos:      strings.TrimSpace(doc.first(attrIs("id", "cos")).text()),
	}
	if w.vlanForm.hash == "" || w.vlanForm.vlanNum == "" {
		return errors.New("VLAN page lacks hash or vlanNum")
	}
	if w.vlanForm.voiceVID == "" {
		w.vlanForm.voiceVID = "0"
	}
	if w.vlanForm.cos == "" {
		w.vlanForm.cos = "6"
	}
	if mode := gs308VLANMode(doc); mode != "adv8021Q" {
		return fmt.Errorf("VLAN mode is %q, advanced 802.1Q required", mode)
	}
	if mgmt, err := strconv.Atoi(doc.first(tag("input"), attrIs("name", "mgmtVlan")).val("value")); err == nil {
		cfg.ManagementVLAN = mgmt
	}
	w.vlanNames = map[int]string{}
	table := doc.first(tag("div"), classContains("vlan-right-view")).first(tag("ul"), classContains("tab-init"))
	for _, li := range table.children(tag("li")) {
		spans := li.first(hasAttr("li-header")).children(tag("span"))
		if len(spans) < 2 {
			return errors.New("VLAN table row not understood")
		}
		vid, err := strconv.Atoi(strings.TrimSpace(spans[0].text()))
		if err != nil {
			return fmt.Errorf("VLAN table row: %w", err)
		}
		w.vlanNames[vid] = strings.TrimSpace(spans[1].text())
		members := provision.Members{}
		for i, code := range li.first(tag("input"), hasAttr("hidden-mem")).val("value") {
			switch string(code) {
			case gs308MemberCode[provision.Tagged]:
				members[i+1] = provision.Tagged
			case gs308MemberCode[provision.Untagged]:
				members[i+1] = provision.Untagged
			}
		}
		cfg.VLANs[vid] = members
	}
	for _, li := range doc.first(tag("ul"), classContains("pvid-tab")).children(tag("li")) {
		port, err := strconv.Atoi(strings.TrimSpace(li.first(tag("span"), classContains("pot-num")).text()))
		if err != nil {
			continue
		}
		// "1*, 10, " lists the port's VLANs; the starred one is the PVID.
		for entry := range strings.SplitSeq(li.first(tag("span"), hasAttr("pvid-str")).text(), ",") {
			if v, ok := strings.CutSuffix(strings.TrimSpace(entry), "*"); ok {
				if vid, err := strconv.Atoi(v); err == nil {
					cfg.PVIDs[port] = vid
				}
			}
		}
	}
	return nil
}

func gs308VLANMode(doc node) string {
	selected := doc.first(tag("input"), attrIs("id", "vlanMod")).val("value")
	for _, b := range doc.first(tag("div"), classContains("vlan-left-view")).all(tag("button")) {
		if b.val("mod-num") == selected && selected != "" {
			return b.val("name")
		}
	}
	return ""
}

func (w *gs308Writer) hiddenMem(members provision.Members) string {
	var b strings.Builder
	for p := 1; p <= 8; p++ {
		m, ok := members[p]
		if !ok {
			m = provision.Excluded
		}
		b.WriteString(gs308MemberCode[m])
	}
	return b.String()
}

func (w *gs308Writer) vlanPost(ctx context.Context, fields ...field) error {
	body, err := w.s.post(ctx, "8021qAdvanced.cgi", append([]field{{"hash", w.vlanForm.hash}}, fields...)...)
	if err != nil {
		return err
	}
	return gs308Check(body, "<div")
}

func (w *gs308Writer) CreateVLAN(ctx context.Context, vid int, members provision.Members) error {
	return w.vlanPost(ctx,
		field{"ACTION", "Add"},
		field{"VLAN_ID", strconv.Itoa(vid)},
		field{"vlanNum", w.vlanForm.vlanNum},
		field{"VLAN_NAME", fmt.Sprintf("unifi-%d", vid)},
		field{"hiddenMem", w.hiddenMem(members)},
		field{"voiceVLANID", "0"},
		field{"voiceVlanCos", w.vlanForm.cos},
	)
}

func (w *gs308Writer) WriteMembers(ctx context.Context, vid int, members provision.Members) error {
	voice := "0"
	if w.vlanForm.voiceVID == strconv.Itoa(vid) {
		voice = "1"
	}
	return w.vlanPost(ctx,
		field{"ACTION", "Apply"},
		field{"VLAN_ID", strconv.Itoa(vid)},
		field{"vlanNum", w.vlanForm.vlanNum},
		field{"VLAN_NAME", w.vlanNames[vid]},
		field{"hiddenMem", w.hiddenMem(members)},
		field{"voiceVLANID", voice},
		field{"voiceVlanCos", w.vlanForm.cos},
	)
}

func (w *gs308Writer) DeleteVLAN(ctx context.Context, vid int) error {
	return w.vlanPost(ctx, field{"ACTION", "Delete"}, field{"VLAN_ID", strconv.Itoa(vid)})
}

func (w *gs308Writer) SetPVID(ctx context.Context, port, vid int) error {
	body, err := w.s.post(ctx, "portPVID.cgi",
		field{"hash", w.vlanForm.hash}, field{"PORT", strconv.Itoa(port)}, field{"PVID", strconv.Itoa(vid)})
	if err != nil {
		return err
	}
	return gs308Check(body, "<div")
}

func (w *gs308Writer) SetPort(ctx context.Context, op provision.SetPort) error {
	code, ok := gs308SpeedCode[op.Speed]
	if !ok {
		return fmt.Errorf("%w: speed %s not supported", provision.ErrForbidden, op.Speed)
	}
	form := w.ports[op.Port].fields
	body, err := w.s.post(ctx, "port_status.cgi",
		field{"hash", w.vlanForm.hash},
		field{fmt.Sprintf("port%d", op.Port), "checked"},
		field{"SPEED", code},
		field{"FLOW_CONTROL", orDefault(form["flowCtr"], "2")},
		field{"DESCRIPTION", op.Name},
		field{"IngressRate", orDefault(form["ingressRate"], "1")},
		field{"EgressRate", orDefault(form["egressRate"], "1")},
		field{"priority", orDefault(form["priority"], "0")},
	)
	if err != nil {
		return err
	}
	if err := gs308Check(body, "SUCCESS"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(gs308PortSettle):
		return nil
	}
}

func (w *gs308Writer) SetSwitchName(ctx context.Context, name string) error {
	body, err := w.s.post(ctx, "switch_name.cgi",
		field{"hash", w.vlanForm.hash}, field{"switch_name", name}, field{"SET_LANG", w.lang})
	if err != nil {
		return err
	}
	return gs308Check(body, "SWITCH NAME SUCCESS")
}

func gs308Check(body []byte, okPrefix string) error {
	text := bytes.TrimSpace(body)
	switch {
	case bytes.HasPrefix(text, []byte(okPrefix)):
		return nil
	case bytes.HasPrefix(text, []byte("CHECK HASH FAILED")):
		return errors.New("switch rejected the session hash")
	}
	if len(text) > 80 {
		text = text[:80]
	}
	return fmt.Errorf("switch answered %q", text)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
