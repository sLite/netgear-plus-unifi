package device

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"netgear-plus-unifi/emulator/internal/config"
	"netgear-plus-unifi/emulator/internal/telemetry"
)

const systemCfg = "users.1.password=secret\nswitch.wevent.key=k5\nswitch.vlan.1.id=1\n" +
	"switch.vlan.2.id=3901\nresolv.host.1.name=SwitchKeller\nresolv.nameserver.1.status=disabled\n" +
	"switch.port.4.pvid=3901\nsshd.status=enabled"

type fakeSink struct {
	mu   sync.Mutex
	subs []string
}

func (f *fakeSink) SubmitConfig(_ context.Context, id, version, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, id+"@"+version+":"+text)
	return nil
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func str(s string) *string { return &s }

func (h *harness) setSync(cs *telemetry.ConfigSync) {
	h.tel.mu.Lock()
	defer h.tel.mu.Unlock()
	h.tel.obs.Status.ConfigSync = cs
}

func (h *harness) provisioningDevice(sink ConfigSink) *Device {
	h.t.Helper()
	d, err := New(config.Switch{ID: "gs308ev4", UniFi: config.Virtual{Model: "US8"}}, Options{
		InformURL: h.srv.URL + "/inform", Interval: 10 * time.Second, StaleAfter: 90 * time.Second,
		Store: h.store, Telemetry: h.tel, Config: sink, Now: func() time.Time { return h.now },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func TestSwitchSectionDropsSecretsAndOtherSections(t *testing.T) {
	got := switchSection(systemCfg)
	want := "switch.vlan.1.id=1\nswitch.vlan.2.id=3901\nresolv.host.1.name=SwitchKeller\nswitch.port.4.pvid=3901"
	if got != want {
		t.Fatalf("switchSection = %q, want %q", got, want)
	}
}

func TestConfigIsDeliveredAndAcknowledgedOnlyWhenProcessed(t *testing.T) {
	h := newHarness(t)
	sink := &fakeSink{}
	d := h.provisioningDevice(sink)
	adoptToConnected(t, h, d)
	h.setSync(&telemetry.ConfigSync{Enabled: true, State: "waiting"})

	h.ctrl.queue = append(h.ctrl.queue, map[string]any{"_type": "setparam", "cfgversion": "c2",
		"mgmt_cfg": "cfgversion=c2", "system_cfg": systemCfg})
	d.Step(t.Context()) // receives c2
	d.Step(t.Context()) // delivers c2, but collector has not processed it
	if sink.count() != 1 || !strings.HasPrefix(sink.subs[0], "gs308ev4@c2:switch.vlan.1.id=1") ||
		strings.Contains(sink.subs[0], "k5") || strings.Contains(sink.subs[0], "secret") {
		t.Fatalf("unexpected submissions: %q", sink.subs)
	}
	if got := h.ctrl.last()["cfgversion"]; got != "abc123" {
		t.Errorf("cfgversion while applying = %v, want previous abc123", got)
	}

	h.setSync(&telemetry.ConfigSync{Enabled: true, State: "in_sync", DesiredCfgVersion: str("c2"), ProcessedCfgVersion: str("c2")})
	d.Step(t.Context())
	if got := h.ctrl.last()["cfgversion"]; got != "c2" {
		t.Errorf("cfgversion after processing = %v, want c2", got)
	}
	if sink.count() != 1 {
		t.Errorf("re-delivered although the collector has it: %d", sink.count())
	}
	st, _, _ := h.store.Load("gs308ev4")
	if st.SwitchConfigVersion != "c2" || st.AckedCfgVersion != "c2" || strings.Contains(st.SwitchConfig, "k5") {
		t.Errorf("state not persisted correctly: %+v", st)
	}

	// A collector restart that lost the configuration gets it again.
	h.setSync(&telemetry.ConfigSync{Enabled: true, State: "waiting"})
	d.Step(t.Context())
	if sink.count() != 2 {
		t.Errorf("configuration not re-delivered after collector restart")
	}
}

func TestAcknowledgementHoldIsBounded(t *testing.T) {
	h := newHarness(t)
	d := h.provisioningDevice(&fakeSink{})
	adoptToConnected(t, h, d)
	h.setSync(&telemetry.ConfigSync{Enabled: true, State: "pending", DesiredCfgVersion: str("c2")})
	h.ctrl.queue = append(h.ctrl.queue, map[string]any{"_type": "setparam", "cfgversion": "c2",
		"mgmt_cfg": "cfgversion=c2", "system_cfg": systemCfg})
	d.Step(t.Context())
	d.Step(t.Context())
	if h.ctrl.last()["cfgversion"] == "c2" {
		t.Fatal("cfgversion acknowledged before the collector processed it")
	}
	h.now = h.now.Add(maxAckHold + time.Second)
	h.tel.set(testSnapshot(), 1, h.now)
	h.setSync(&telemetry.ConfigSync{Enabled: true, State: "pending", DesiredCfgVersion: str("c2")})
	d.Step(t.Context())
	if got := h.ctrl.last()["cfgversion"]; got != "c2" {
		t.Errorf("hold must end after %s, got cfgversion %v", maxAckHold, got)
	}
}

func TestNoHoldWhenWritesAreDisabled(t *testing.T) {
	h := newHarness(t)
	d := h.provisioningDevice(&fakeSink{})
	adoptToConnected(t, h, d)
	h.setSync(&telemetry.ConfigSync{Enabled: false, State: "disabled"})
	h.ctrl.queue = append(h.ctrl.queue, map[string]any{"_type": "setparam", "cfgversion": "c2",
		"mgmt_cfg": "cfgversion=c2", "system_cfg": systemCfg})
	d.Step(t.Context())
	d.Step(t.Context())
	if got := h.ctrl.last()["cfgversion"]; got != "c2" {
		t.Errorf("cfgversion = %v, want c2 immediately when writes are off", got)
	}
}
