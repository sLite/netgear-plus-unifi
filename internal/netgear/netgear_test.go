package netgear

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/telemetry"
)

func TestMain(m *testing.M) {
	if os.Getenv("TEST_LOGS") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	gs308PortSettle = 0
	os.Exit(m.Run())
}

func val[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestLoginHashesMatchTheFirmware(t *testing.T) {
	// Reference values computed with py-netgear-plus's netgear_crypt.
	for _, c := range []struct{ got, want string }{
		{mergeHash("test-password", "123456789"), "378f5b0f73f9f1005a51961504bfa897"},
		{mergeHash("a-much-longer-test-password", "12"), "55f7ed2d80333db1351bea894d1f6f4d"},
		{hexHMACMD5("test-password"), "f130571550b0b9854ecac3b120ab6943"},
		{hexHMACMD5("pässwörd"), "984fd7924dea677f432691c80e05e1b5"},
	} {
		if c.got != c.want {
			t.Errorf("hash = %s, want %s", c.got, c.want)
		}
	}
}

func TestGS308Ev4Reading(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs308ev4")
	r, err := sw.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := telemetry.Device{Vendor: "netgear", Model: "GS308Ev4", Firmware: "V1.0.1.9", Serial: "SERIAL308TEST",
		ManagementIP: r.Device.ManagementIP, PortCount: 8, HardwareMAC: "28:94:01:00:00:01",
		PortSpeeds: []string{"auto", "10f", "100f", "1000f"}}
	if !reflect.DeepEqual(r.Device, want) {
		t.Errorf("device = %+v", r.Device)
	}
	if len(r.Ports) != 8 {
		t.Fatalf("%d ports", len(r.Ports))
	}
	p1 := r.Ports[0]
	if !p1.LinkUp || val(p1.SpeedMbps) != 1000 || val(p1.FullDuplex) != true || val(p1.Autoneg) != true ||
		val(p1.Enabled) != true || val(p1.RXBytes) != uint64(1866737) || val(p1.TXBytes) != uint64(8704593) || val(p1.CRCErrors) != uint64(0) {
		t.Errorf("port 1 = %+v", p1)
	}
	for _, p := range r.Ports[1:] {
		if p.LinkUp || val(p.SpeedMbps) != 0 || p.FullDuplex != nil {
			t.Errorf("port %d should be down: %+v", p.Index, p)
		}
	}
	// The dashboard is both info and status page: one fetch plus statistics.
	reqs, _, logins, _ := f.snapshot()
	if logins != 1 || !reflect.DeepEqual(reqs[len(reqs)-2:], []string{"GET dashboard.cgi", "GET portStatistics.cgi"}) {
		t.Errorf("requests = %v (logins %d)", reqs, logins)
	}
}

func TestGS116Ev2Reading(t *testing.T) {
	_, sw := newFakeSwitch(t, "gs116ev2")
	r, err := sw.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if r.Device.Firmware != "2.6.0.50" || r.Device.Serial != "SERIAL116TEST" || r.Device.HardwareMAC != "38:94:ed:00:00:02" ||
		!reflect.DeepEqual(r.Device.PortSpeeds, []string{"auto", "10h", "10f", "100h", "100f", "1000f"}) {
		t.Errorf("device = %+v", r.Device)
	}
	up := map[int]any{}
	for _, p := range r.Ports {
		if p.LinkUp {
			up[p.Index] = val(p.SpeedMbps)
		}
	}
	if !reflect.DeepEqual(up, map[int]any{1: 10, 2: 1000, 7: 1000, 8: 1000, 12: 100, 15: 1000}) {
		t.Errorf("ports up = %v", up)
	}
	p8 := r.Ports[7]
	if val(p8.RXBytes) != uint64(24774038874) || p8.FullDuplex != nil { // > 2^32; no duplex on this model
		t.Errorf("port 8 = %+v", p8)
	}
}

func TestGS308Ev4CountersCombineHighAndLowWord(t *testing.T) {
	page := []byte(`<ul><li><span>1</span></li>
<input type="hidden" value="25"><input type="hidden" value="7">
<input type="hidden" value="1"><input type="hidden" value="0">
<input type="hidden" value="0"><input type="hidden" value="3"></ul>`)
	got := gs308Counters(page)[1]
	if got != (gs308Counter{rx: 25<<32 + 7, tx: 1 << 32, crc: 3}) {
		t.Errorf("counters = %+v", got)
	}
	if len(gs308Counters([]byte("<html>changed firmware</html>"))) != 0 {
		t.Error("missing markup must yield no counters")
	}
}

func TestMetadataIsCachedBetweenPolls(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	f, sw := newFakeSwitch(t, "gs116ev2")
	sw.now = clock.Now
	count := func() int {
		reqs, _, _, _ := f.snapshot()
		n := 0
		for _, r := range reqs {
			if r == "GET "+jgsSwitchInfo {
				n++
			}
		}
		return n
	}
	for range 2 {
		if _, err := sw.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 1 {
		t.Errorf("info page fetched %d times, want once", count())
	}
	clock.now = clock.now.Add(metadataRefresh + time.Second)
	if _, err := sw.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, logins, _ := f.snapshot(); count() != 2 || logins != 1 {
		t.Errorf("info fetched %d times, %d logins", count(), logins)
	}
}

func TestModelMismatchIsRefusedBeforeLogin(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs116ev2")
	sw.expected = "GS308Ev4"
	if _, err := sw.Read(t.Context()); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("got %v", err)
	}
	if _, _, logins, _ := f.snapshot(); logins != 0 {
		t.Error("logged in to the wrong model")
	}
}

func TestLoginErrors(t *testing.T) {
	for _, c := range []struct {
		setup func(*fakeSwitch)
		want  error
	}{
		{func(f *fakeSwitch) { f.rejectLogin = true }, ErrAuthentication},
		{func(f *fakeSwitch) { f.maxSessions = true }, ErrSessionLimit},
	} {
		for _, model := range []string{"gs308ev4", "gs116ev2"} {
			f, sw := newFakeSwitch(t, model)
			f.set(c.setup)
			if _, err := sw.Read(t.Context()); !errors.Is(err, c.want) {
				t.Errorf("%s: got %v, want %v", model, err, c.want)
			}
		}
	}
}

func TestExpiredGS308Ev4SessionIsRestoredTransparently(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs308ev4")
	if _, err := sw.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *fakeSwitch) { f.expired = true })
	if _, err := sw.Read(t.Context()); err != nil {
		t.Fatalf("expired session not restored: %v", err)
	}
	if _, _, logins, _ := f.snapshot(); logins != 2 {
		t.Errorf("logins = %d, want 2", logins)
	}
}

func TestDisplacedGS116Ev2SessionIsReportedNotRestored(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs116ev2")
	if _, err := sw.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *fakeSwitch) { f.displaced = true })
	if _, err := sw.Read(t.Context()); !errors.Is(err, ErrSessionDisplaced) {
		t.Fatalf("got %v", err)
	}
	// Logging in again at once would log the web UI user out.
	if _, _, logins, _ := f.snapshot(); logins != 1 {
		t.Errorf("logins = %d, want 1", logins)
	}
	if sw.sess != nil {
		t.Error("session not dropped")
	}
}

func TestFailedReadDropsSessionAndReconnects(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs116ev2")
	f.set(func(f *fakeSwitch) { f.overrides[jgsStatistics] = []byte("<html>garbage</html>") })
	if _, err := sw.Read(t.Context()); err == nil {
		t.Fatal("garbage statistics page accepted")
	}
	f.set(func(f *fakeSwitch) { delete(f.overrides, jgsStatistics) })
	if _, err := sw.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, logins, logouts := f.snapshot(); logins != 2 || logouts != 1 {
		t.Errorf("logins %d logouts %d, want 2 and 1", logins, logouts)
	}
}

func TestPortCountMismatchIsAnError(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs116ev2")
	f.set(func(f *fakeSwitch) {
		f.overrides[jgsPortStatus] = []byte("<script>portConfigEntry[0] = '1??Up?Auto?1000M?Disable';</script>")
	})
	if _, err := sw.Read(t.Context()); err == nil {
		t.Fatal("truncated status page accepted")
	}
}

func TestCloseLogsOut(t *testing.T) {
	for _, model := range []string{"gs308ev4", "gs116ev2"} {
		f, sw := newFakeSwitch(t, model)
		if _, err := sw.Read(t.Context()); err != nil {
			t.Fatal(err)
		}
		sw.Close()
		sw.Close()
		if _, _, _, logouts := f.snapshot(); logouts != 1 {
			t.Errorf("%s: logouts = %d", model, logouts)
		}
	}
}

func TestPollingNeverWrites(t *testing.T) {
	// Safety regression test: reading only ever GETs status pages; the only
	// POSTs are login and logout.
	for _, model := range []string{"gs308ev4", "gs116ev2"} {
		f, sw := newFakeSwitch(t, model)
		for range 3 {
			if _, err := sw.Read(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		sw.Close()
		if posts := f.writePosts(); len(posts) != 0 {
			t.Errorf("%s: polling posted %v", model, posts)
		}
	}
}

func readConfig(t *testing.T, sw *Switch) (provision.Backend, *provision.Config) {
	t.Helper()
	if _, err := sw.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	b := sw.model.backend(sw.sess)
	cfg, err := b.ReadConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return b, cfg
}

func TestGS308Ev4ReadsAndEncodesLikeTheWebUI(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs308ev4")
	b, cfg := readConfig(t, sw)
	if !reflect.DeepEqual(cfg.VLANs, map[int]provision.Members{1: {1: "U", 2: "U", 3: "U", 4: "U", 5: "U", 6: "U", 7: "U", 8: "U"}}) ||
		cfg.PVIDs[4] != 1 || cfg.ManagementVLAN != 1 || cfg.Ports[4] != (provision.PortSettings{Speed: provision.SpeedAuto, Name: provision.SetName("")}) ||
		cfg.SwitchName != provision.SetName("") {
		t.Fatalf("config = %+v", cfg)
	}
	ctx := t.Context()
	members := provision.Members{1: "U", 2: "U", 3: "U", 5: "U", 6: "U", 7: "U", 8: "U"}
	must(t, b.WriteMembers(ctx, 1, members))
	must(t, b.CreateVLAN(ctx, 3901, provision.Members{4: "T"}))
	must(t, b.SetPort(ctx, provision.SetPort{Port: 4, Speed: provision.Speed100Full, Name: "lab"}))
	must(t, b.SetSwitchName(ctx, "SwitchKeller"))
	posts := f.writePosts()
	hash := strings.Repeat("1", 32)
	want := []post{
		{"8021qAdvanced.cgi", []field{{"hash", hash}, {"ACTION", "Apply"}, {"VLAN_ID", "1"}, {"vlanNum", "1"},
			{"VLAN_NAME", "Default"}, {"hiddenMem", "22232222"}, {"voiceVLANID", "0"}, {"voiceVlanCos", "6"}}},
		{"8021qAdvanced.cgi", []field{{"hash", hash}, {"ACTION", "Add"}, {"VLAN_ID", "3901"}, {"vlanNum", "1"},
			{"VLAN_NAME", "unifi-3901"}, {"hiddenMem", "33313333"}, {"voiceVLANID", "0"}, {"voiceVlanCos", "6"}}},
		{"port_status.cgi", []field{{"hash", hash}, {"port4", "checked"}, {"SPEED", "6"}, {"FLOW_CONTROL", "2"},
			{"DESCRIPTION", "lab"}, {"IngressRate", "1"}, {"EgressRate", "1"}, {"priority", "0"}}},
		{"switch_name.cgi", []field{{"hash", hash}, {"switch_name", "SwitchKeller"}, {"SET_LANG", "en"}}},
	}
	if !reflect.DeepEqual(posts, want) {
		t.Errorf("posts:\n got %v\nwant %v", posts, want)
	}
}

func TestGS116Ev2ReadsAndEncodesLikeTheWebUI(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs116ev2")
	b, cfg := readConfig(t, sw)
	if len(cfg.VLANs) != 1 || len(cfg.VLANs[1]) != 16 || cfg.PVIDs[16] != 1 ||
		cfg.Ports[4] != (provision.PortSettings{Speed: provision.SpeedAuto, Name: provision.SetName("")}) {
		t.Fatalf("config = %+v", cfg)
	}
	ctx := t.Context()
	must(t, b.WriteMembers(ctx, 3901, provision.Members{4: "T", 2: "U"}))
	must(t, b.CreateVLAN(ctx, 3901, nil))
	must(t, b.SetPVID(ctx, 4, 3901))
	must(t, b.SetPort(ctx, provision.SetPort{Port: 4, Speed: provision.SpeedDisabled}))
	posts := f.writePosts()
	want := []post{
		{jgsVLANMember, []field{{"submitId", "vlanDot1TagCfg"}, {"secureRand", "ABCDEF01"}, {"vid", "3901"},
			{"member", "20"}, {"tag", "16"}, {"submitEnd", ""}}},
		{jgsVLANConf, []field{{"submitId", "vlanDot1VidCfg"}, {"secureRand", "ABCDEF01"}, {"changeType", "0"},
			{"addVid", "3901"}, {"confirmOK", "0"}, {"submitEnd", ""}}},
		{jgsPVID, []field{{"submitId", "vlanDot1PvidCfg"}, {"secureRand", "ABCDEF01"}, {"port", "4"}, {"pvid", "3901"}, {"submitEnd", ""}}},
		{jgsPortStatus, []field{{"submitId", "sysStatus"}, {"secureRand", "ABCDEF02"}, {"speed", "0"}, {"flow", "255"},
			{"description", ""}, {"port", "4"}, {"submitEnd", ""}}},
	}
	if !reflect.DeepEqual(posts, want) {
		t.Errorf("posts:\n got %v\nwant %v", posts, want)
	}
	for _, p := range posts {
		if v, ok := formMap(p.form)["changeType"]; ok && v != "0" {
			t.Error("changeType other than 0 resets the VLAN configuration")
		}
	}
}

func TestGS116Ev2SwitchNameEchoesIPSettingsUnchanged(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs116ev2")
	b, _ := readConfig(t, sw)
	must(t, b.SetSwitchName(t.Context(), "SwitchBuero"))
	posts := f.writePosts()
	if len(posts) != 1 || posts[0].path != jgsSwitchInfo {
		t.Fatalf("posts = %v", posts)
	}
	want := []field{{"submitId", "sysIp"}, {"secureRand", strings.Repeat("0", 32)}, {"swName", "SwitchBuero"}, {"dhcp", "2"}, {"submitEnd", ""}}
	if !reflect.DeepEqual(posts[0].form, want) { // dhcp=2: DHCP stays on
		t.Errorf("form = %v", posts[0].form)
	}
}

func TestGS116Ev2SwitchNameDetectsChangedIPSettings(t *testing.T) {
	f, sw := newFakeSwitch(t, "gs116ev2")
	b, _ := readConfig(t, sw)
	changed := strings.Replace(string(f.page("info")), "?Enable?192.168.1.230?", "?Enable?192.168.1.99?", 1)
	f.set(func(f *fakeSwitch) { f.afterPost[jgsSwitchInfo] = []byte(changed) })
	if err := b.SetSwitchName(t.Context(), "SwitchBuero"); err == nil || !strings.Contains(err.Error(), "IP settings changed") {
		t.Errorf("got %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedModelIsNamed(t *testing.T) {
	f, _ := newFakeSwitch(t, "gs308ev4")
	page := strings.Replace(string(f.page("login")), "<title>NETGEAR GS308Ev4</title>", "<title>NETGEAR GS108Ev4</title>", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login.cgi" {
			_, _ = w.Write([]byte(page))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	sw := NewSwitch("sw", strings.TrimPrefix(srv.URL, "http://"), testPassword, "")
	_, err := sw.Read(t.Context())
	if !errors.Is(err, ErrIdentityMismatch) || !strings.Contains(err.Error(), "GS108Ev4 is not supported") {
		t.Errorf("got %v", err)
	}
}

// readOnly is a model without write support.
type readOnly struct{ gs116ev2 }

func (readOnly) backend(*session) provision.Backend { return nil }

func TestReadOnlyModelHasNoWriter(t *testing.T) {
	sw := &Switch{sess: &session{}, model: readOnly{}}
	if _, ok := sw.Writer(); ok {
		t.Error("a model without backend must not offer a writer")
	}
}
