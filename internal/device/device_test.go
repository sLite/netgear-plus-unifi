package device

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"

	"netgear-plus-unifi/internal/config"
	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/state"
	"netgear-plus-unifi/internal/telemetry"
)

const adoptKey = "00112233445566778899aabbccddeeff"

// fakeController speaks the controller side of the inform protocol: 404
// while pending, the adoption key via mgmt_cfg once adopted, then whatever
// the test queues (noop by default).
type fakeController struct {
	t       *testing.T
	mu      sync.Mutex
	adopt   bool
	queue   []map[string]any
	informs []map[string]any
	gcm     []bool
}

func (c *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	useGCM := len(body) >= 16 && binary.BigEndian.Uint16(body[14:16])&0x08 != 0
	key := inform.DefaultKey
	pkt, err := inform.Decode(body, key)
	if err != nil {
		key = adoptKey
		if pkt, err = inform.Decode(body, key); err != nil {
			c.t.Errorf("controller could not decode inform: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(pkt.Payload, &payload); err != nil {
		c.t.Errorf("inform payload is not JSON: %v", err)
	}
	c.informs = append(c.informs, payload)
	c.gcm = append(c.gcm, useGCM)

	var reply map[string]any
	switch {
	case key == inform.DefaultKey && !c.adopt:
		w.WriteHeader(http.StatusNotFound)
		return
	case key == inform.DefaultKey:
		reply = map[string]any{"_type": "setparam", "mgmt_cfg": "cfgversion=abc123\nauthkey=" + adoptKey + "\nuse_aes_gcm=true"}
	case len(c.queue) > 0:
		reply, c.queue = c.queue[0], c.queue[1:]
	default:
		reply = map[string]any{"_type": "noop", "interval": 10}
	}
	raw, _ := json.Marshal(reply)
	out := &inform.Packet{MAC: pkt.MAC, Payload: raw}
	var enc []byte
	if useGCM {
		enc, err = out.EncodeGCM(key)
	} else {
		enc, err = out.Encode(key)
	}
	if err != nil {
		c.t.Fatalf("encode reply: %v", err)
	}
	_, _ = w.Write(enc)
}

func (c *fakeController) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.informs[len(c.informs)-1]
}

func (c *fakeController) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.informs)
}

// fakeSource stands in for the poller and sync of a physical switch.
type fakeSource struct {
	mu     sync.Mutex
	snap   *telemetry.Snapshot
	reason string
	status provision.Status
	subs   []string
}

func (f *fakeSource) Snapshot(time.Time) (*telemetry.Snapshot, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.reason
}

// set makes snap the fresh telemetry.
func (f *fakeSource) set(snap *telemetry.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap, f.reason = snap, ""
}

// stale makes telemetry unusable for reason.
func (f *fakeSource) stale(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap, f.reason = nil, reason
}

func (f *fakeSource) SubmitConfig(version, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, version+":"+text)
}

func (f *fakeSource) ConfigStatus() provision.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeSource) setStatus(s provision.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

func (f *fakeSource) submissions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.subs...)
}

type harness struct {
	t     *testing.T
	ctrl  *fakeController
	srv   *httptest.Server
	src   *fakeSource
	store *state.Store
	dir   string
	now   time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctrl := &fakeController{t: t}
	srv := httptest.NewServer(ctrl)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctrl: ctrl, srv: srv, src: &fakeSource{}, store: store, dir: dir, now: time.Unix(1_800_000_000, 0)}
	h.src.set(testSnapshot())
	return h
}

func (h *harness) device() *Device {
	h.t.Helper()
	d, err := New(config.Switch{ID: "gs308ev4", UniFi: config.Virtual{Model: "US8", Name: "office"}}, h.src, Options{
		InformURL:  h.srv.URL + "/inform",
		Interval:   10 * time.Second,
		StaleAfter: 90 * time.Second,
		Store:      h.store,
		Now:        func() time.Time { return h.now },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func adoptToConnected(t *testing.T, h *harness, d *Device) {
	t.Helper()
	ctx := t.Context()
	d.Step(ctx)
	if got := d.Status().Phase; got != PhasePending {
		t.Fatalf("phase after first inform = %s, want pending", got)
	}
	h.ctrl.adopt = true
	d.Step(ctx)
	if got := d.Status().Phase; got != PhaseAdopting {
		t.Fatalf("phase after key delivery = %s, want adopting", got)
	}
	d.Step(ctx)
	if got := d.Status().Phase; got != PhaseConnected {
		t.Fatalf("phase after adopted inform = %s, want connected", got)
	}
}

func TestAdoptionLifecycleWithLiveTelemetry(t *testing.T) {
	h := newHarness(t)
	d := h.device()
	adoptToConnected(t, h, d)

	pending := h.ctrl.informs[0]
	if pending["default"] != true || pending["port_table"] != nil {
		t.Errorf("pending inform should be default and carry no port_table: %v", pending)
	}
	if h.ctrl.gcm[1] || !h.ctrl.gcm[2] {
		t.Errorf("want CBC before adoption and AES-GCM after, got %v", h.ctrl.gcm)
	}
	adopted := h.ctrl.last()
	if adopted["default"] != false || adopted["mac"] != d.MAC() || adopted["ip"] != "192.168.1.247" {
		t.Errorf("unexpected adopted identity: default=%v mac=%v ip=%v", adopted["default"], adopted["mac"], adopted["ip"])
	}
	if _, ok := adopted["sys_stats"]; ok {
		t.Error("fabricated sys_stats must not be reported")
	}
	ports := adopted["port_table"].([]any)
	if len(ports) != 8 {
		t.Fatalf("port_table has %d rows, want 8", len(ports))
	}
	p1 := ports[0].(map[string]any)
	if p1["up"] != true || p1["speed"] != float64(1000) || p1["rx_bytes"] != float64(1866737) || p1["poe_caps"] != float64(0) {
		t.Errorf("port 1 does not reflect telemetry: %v", p1)
	}

	st, ok, err := h.store.Load("gs308ev4")
	if err != nil || !ok || !st.Adopted || st.AuthKey != adoptKey || !st.UseAESGCM || st.CfgVersion != "abc123" {
		t.Fatalf("persisted state incomplete: ok=%v err=%v %+v", ok, err, st)
	}
	info, err := os.Stat(filepath.Join(h.dir, "devices", "gs308ev4.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file must be 0600: %v %v", info.Mode(), err)
	}
}

func TestRestartResumesAdoptionWithoutReadoption(t *testing.T) {
	h := newHarness(t)
	adoptToConnected(t, h, h.device())

	h.ctrl.adopt = false // a pending-style inform would now get 404
	restarted := h.device()
	if got := restarted.Status().Phase; got != PhaseAdopting {
		t.Fatalf("restored phase = %s, want adopting until the controller answers", got)
	}
	restarted.Step(t.Context())
	if got := restarted.Status().Phase; got != PhaseConnected {
		t.Fatalf("phase after restart = %s, want connected", got)
	}
	last := h.ctrl.last()
	if last["default"] != false || last["cfgversion"] != "abc123" || !h.ctrl.gcm[len(h.ctrl.gcm)-1] {
		t.Errorf("restarted device must inform adopted, with cfgversion and AES-GCM: %v", last["cfgversion"])
	}
}

func TestStaleTelemetryStopsInforming(t *testing.T) {
	h := newHarness(t)
	d := h.device()
	adoptToConnected(t, h, d)
	before := h.ctrl.count()

	h.src.stale("telemetry stale: switch unreachable")
	d.Step(t.Context())
	if h.ctrl.count() != before {
		t.Fatal("device informed with stale telemetry")
	}
	if s := d.Status(); !s.Paused || !strings.Contains(s.PauseReason, "stale") {
		t.Errorf("status should report the pause: %+v", s)
	}

	h.src.set(testSnapshot())
	d.Step(t.Context())
	if h.ctrl.count() != before+1 || d.Status().Paused {
		t.Error("device did not resume after telemetry recovered")
	}
}

func TestDifferentPhysicalSwitchIsRefused(t *testing.T) {
	h := newHarness(t)
	d := h.device()
	adoptToConnected(t, h, d)
	before := h.ctrl.count()

	other := testSnapshot()
	other.Device.Serial = "SOMEONEELSE"
	h.src.set(other)
	d.Step(t.Context())
	if h.ctrl.count() != before || !strings.Contains(d.Status().PauseReason, "identity changed") {
		t.Fatalf("telemetry from a different switch must not be reported: %+v", d.Status())
	}
}

func TestProvisioningIsRecordedEchoedButNotApplied(t *testing.T) {
	h := newHarness(t)
	d := h.device()
	adoptToConnected(t, h, d)

	overrides := []map[string]any{{"port_idx": 2, "portconf_id": "abc", "name": "printer"}}
	h.ctrl.queue = append(h.ctrl.queue, map[string]any{"_type": "setstate", "cfgversion": "def456", "port_overrides": overrides})
	d.Step(t.Context()) // receives setstate
	d.Step(t.Context()) // next inform echoes it
	echo, _ := json.Marshal(h.ctrl.last()["port_overrides"])
	if !strings.Contains(string(echo), "printer") || h.ctrl.last()["cfgversion"] != "def456" {
		t.Errorf("provisioned config must be echoed: %s cfgversion=%v", echo, h.ctrl.last()["cfgversion"])
	}
	st, _, _ := h.store.Load("gs308ev4")
	if _, ok := st.Provisioned["port_overrides"]; !ok {
		t.Error("provisioned tables must be persisted for restarts")
	}

	restarted := h.device()
	restarted.Step(t.Context())
	echo, _ = json.Marshal(h.ctrl.last()["port_overrides"])
	if !strings.Contains(string(echo), "printer") {
		t.Error("provisioned config lost across restart")
	}
}

func TestSetDefaultReturnsToPendingAndClearsKey(t *testing.T) {
	h := newHarness(t)
	d := h.device()
	adoptToConnected(t, h, d)

	h.ctrl.queue = append(h.ctrl.queue, map[string]any{"_type": "cmd", "cmd": "setdefault"})
	h.ctrl.adopt = false
	d.Step(t.Context())
	if got := d.Status().Phase; got != PhasePending {
		t.Fatalf("phase after setdefault = %s, want pending", got)
	}
	st, _, _ := h.store.Load("gs308ev4")
	if st.Adopted || st.AuthKey != "" || st.Provisioned != nil {
		t.Errorf("forgotten device must drop its key and provisioning: %+v", st)
	}
	if st.PhysicalSerial == "" {
		t.Error("physical binding should survive a controller-side forget")
	}
}

func TestChangedInformURLDiscardsAdoption(t *testing.T) {
	h := newHarness(t)
	adoptToConnected(t, h, h.device())

	d, err := New(config.Switch{ID: "gs308ev4", UniFi: config.Virtual{Model: "US8"}}, h.src, Options{
		InformURL: "http://192.0.2.1:8080/inform", Interval: time.Second, StaleAfter: time.Minute,
		Store: h.store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status().Phase != PhasePending {
		t.Error("an adoption for another controller must not be reused")
	}
}

func TestNonSwitchModelIsRejected(t *testing.T) {
	_, err := New(config.Switch{ID: "x", UniFi: config.Virtual{Model: "U7PRO"}}, &fakeSource{}, Options{Store: newHarness(t).store})
	if err == nil || !strings.Contains(err.Error(), "not a switch") {
		t.Fatalf("want rejection of AP model, got %v", err)
	}
}

func TestProvisioningChangeWithoutCfgversionChangeIsPersisted(t *testing.T) {
	h := newHarness(t)
	d := h.device()
	adoptToConnected(t, h, d)
	push := func(name string) {
		h.ctrl.queue = append(h.ctrl.queue, map[string]any{"_type": "setstate", "cfgversion": "same",
			"port_overrides": []map[string]any{{"port_idx": 2, "name": name}}})
		d.Step(t.Context())
	}
	push("first")
	push("second")
	st, _, _ := h.store.Load("gs308ev4")
	if !strings.Contains(string(st.Provisioned["port_overrides"]), "second") {
		t.Fatalf("latest provisioning not persisted: %s", st.Provisioned["port_overrides"])
	}
}
