package device

import (
	"strings"
	"testing"
	"time"

	"netgear-plus-unifi/internal/config"
	"netgear-plus-unifi/internal/provision"
)

const systemCfg = "users.1.password=secret\nswitch.wevent.key=k5\nswitch.vlan.1.id=1\n" +
	"switch.vlan.2.id=3901\nresolv.host.1.name=SwitchKeller\nresolv.nameserver.1.status=disabled\n" +
	"switch.port.4.pvid=3901\nsshd.status=enabled"

func (h *harness) provisioningDevice() *Device {
	h.t.Helper()
	d, err := New(config.Switch{ID: "gs308ev4", UniFi: config.Virtual{Model: "US8"}}, h.src, Options{
		InformURL: h.srv.URL + "/inform", Interval: 10 * time.Second, StaleAfter: 90 * time.Second,
		Store: h.store, Now: func() time.Time { return h.now },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) sendSystemCfg(version string) {
	h.ctrl.queue = append(h.ctrl.queue, map[string]any{"_type": "setparam", "cfgversion": version,
		"mgmt_cfg": "cfgversion=" + version, "system_cfg": systemCfg})
}

func TestConfigIsDeliveredAndAcknowledgedOnlyWhenProcessed(t *testing.T) {
	h := newHarness(t)
	d := h.provisioningDevice()
	adoptToConnected(t, h, d)
	h.src.setStatus(provision.Status{Enabled: true, State: provision.StateWaiting})

	h.sendSystemCfg("c2")
	d.Step(t.Context()) // receives c2
	d.Step(t.Context()) // delivers c2, but the switch has not processed it
	subs := h.src.submissions()
	if len(subs) != 1 || !strings.HasPrefix(subs[0], "c2:switch.vlan.1.id=1") ||
		strings.Contains(subs[0], "k5") || strings.Contains(subs[0], "secret") {
		t.Fatalf("unexpected submissions: %q", subs)
	}
	if got := h.ctrl.last()["cfgversion"]; got != "abc123" {
		t.Errorf("cfgversion while applying = %v, want previous abc123", got)
	}

	h.src.setStatus(provision.Status{Enabled: true, State: provision.StateInSync, DesiredCfgVersion: "c2", ProcessedCfgVersion: "c2"})
	d.Step(t.Context())
	if got := h.ctrl.last()["cfgversion"]; got != "c2" {
		t.Errorf("cfgversion after processing = %v, want c2", got)
	}
	if n := len(h.src.submissions()); n != 1 {
		t.Errorf("configuration submitted %d times, want once", n)
	}
	st, _, _ := h.store.Load("gs308ev4")
	if st.SwitchConfigVersion != "c2" || st.AckedCfgVersion != "c2" || strings.Contains(st.SwitchConfig, "k5") {
		t.Errorf("state not persisted correctly: %+v", st)
	}

	// After a restart the stored configuration is submitted again, so the
	// sync re-verifies the switch.
	restarted := h.provisioningDevice()
	restarted.Step(t.Context())
	if subs := h.src.submissions(); len(subs) != 2 || !strings.HasPrefix(subs[1], "c2:") {
		t.Errorf("stored configuration not re-submitted after restart: %q", subs)
	}
}

func TestAcknowledgementHoldIsBounded(t *testing.T) {
	h := newHarness(t)
	d := h.provisioningDevice()
	adoptToConnected(t, h, d)
	h.src.setStatus(provision.Status{Enabled: true, State: provision.StatePending, DesiredCfgVersion: "c2"})
	h.sendSystemCfg("c2")
	d.Step(t.Context())
	d.Step(t.Context())
	if h.ctrl.last()["cfgversion"] == "c2" {
		t.Fatal("cfgversion acknowledged before the switch processed it")
	}
	h.now = h.now.Add(maxAckHold + time.Second)
	d.Step(t.Context())
	if got := h.ctrl.last()["cfgversion"]; got != "c2" {
		t.Errorf("hold must end after %s, got cfgversion %v", maxAckHold, got)
	}
}

func TestNoHoldWhenWritesAreDisabled(t *testing.T) {
	h := newHarness(t)
	d := h.provisioningDevice()
	adoptToConnected(t, h, d)
	h.src.setStatus(provision.Status{Enabled: false, State: provision.StateDisabled})
	h.sendSystemCfg("c2")
	d.Step(t.Context())
	d.Step(t.Context())
	if got := h.ctrl.last()["cfgversion"]; got != "c2" {
		t.Errorf("cfgversion = %v, want c2 immediately when writes are off", got)
	}
}
