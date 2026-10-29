package device

import (
	"strings"
	"testing"
	"time"

	"netgear-plus-unifi/internal/config"
)

const netgearMAC = "28:94:01:8f:ff:71"

func (h *harness) physicalDevice() *Device {
	h.t.Helper()
	d, err := New(config.Switch{ID: "gs308ev4", UniFi: config.Virtual{Model: "USL8LP", MAC: "physical"}}, h.src, Options{
		InformURL: h.srv.URL + "/inform", Interval: 10 * time.Second, StaleAfter: 90 * time.Second,
		Store: h.store, Now: func() time.Time { return h.now },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func physicalHarness(t *testing.T) *harness {
	h := newHarness(t)
	snap := testSnapshot()
	snap.Device.HardwareMAC = "28:94:01:8F:FF:71"
	h.src.set(snap)
	return h
}

func TestPhysicalModeLearnsTheSwitchMAC(t *testing.T) {
	h := physicalHarness(t)
	d := h.physicalDevice()
	if d.MAC() != "" {
		t.Fatal("MAC must be unknown before telemetry")
	}
	adoptToConnected(t, h, d)
	if d.MAC() != netgearMAC || h.ctrl.last()["mac"] != netgearMAC {
		t.Fatalf("device must inform as the switch's own MAC, got %q / %v", d.MAC(), h.ctrl.last()["mac"])
	}
	st, _, _ := h.store.Load("gs308ev4")
	if st.MAC != netgearMAC || st.MACSource != "physical" || !st.Adopted {
		t.Errorf("learned MAC not persisted: %+v", st)
	}

	restarted := h.physicalDevice()
	if restarted.MAC() != netgearMAC || restarted.Status().Phase != PhaseAdopting {
		t.Errorf("restart must resume the saved identity: mac=%q phase=%s", restarted.MAC(), restarted.Status().Phase)
	}
}

func TestPhysicalModeWaitsForAUsableMAC(t *testing.T) {
	h := newHarness(t) // snapshot without hardware MAC
	d := h.physicalDevice()
	d.Step(t.Context())
	if h.ctrl.count() != 0 || d.MAC() != "" || !strings.Contains(d.Status().PauseReason, "no usable MAC") {
		t.Fatalf("must not inform without the switch's MAC: count=%d status=%+v", h.ctrl.count(), d.Status())
	}
}

func TestPhysicalModePausesWhenTheSwitchMACChanges(t *testing.T) {
	h := physicalHarness(t)
	d := h.physicalDevice()
	adoptToConnected(t, h, d)
	before := h.ctrl.count()

	other := testSnapshot()
	other.Device.HardwareMAC = "28:94:01:00:00:99"
	h.src.set(other)
	d.Step(t.Context())
	if h.ctrl.count() != before || !strings.Contains(d.Status().PauseReason, "now reports MAC") {
		t.Fatalf("a different switch MAC must pause the device: %+v", d.Status())
	}
}

func TestSwitchingFromAutoToPhysicalStartsANewDevice(t *testing.T) {
	h := physicalHarness(t)
	adoptToConnected(t, h, h.device()) // adopted with the derived 02:4e:47 MAC

	d := h.physicalDevice()
	if d.MAC() != "" {
		t.Fatal("state from auto mode must not provide the physical identity")
	}
	h.ctrl.adopt = false // UniFi has not adopted the new identity
	d.Step(t.Context())
	if d.MAC() != netgearMAC || d.Status().Phase != PhasePending {
		t.Errorf("want a fresh pending device as %s, got mac=%q phase=%s", netgearMAC, d.MAC(), d.Status().Phase)
	}
	st, _, _ := h.store.Load("gs308ev4")
	if st.PhysicalSerial == "" {
		t.Error("the binding to the physical switch must survive the identity change")
	}
}

func TestDownlinksResolvePeersAtInformTime(t *testing.T) {
	h := newHarness(t)
	peer := ""
	d, err := New(config.Switch{ID: "gs116ev2", UniFi: config.Virtual{Model: "USL16LP",
		Downlinks: []config.Downlink{{Port: 2, Switch: "gs308ev4"}}}}, h.src, Options{
		InformURL: h.srv.URL + "/inform", Interval: time.Second, StaleAfter: time.Minute,
		Store: h.store, Now: func() time.Time { return h.now },
		PeerMAC: func(string) string { return peer },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.resolveDownlinks(); len(got) != 0 {
		t.Errorf("unknown peer MAC must be skipped, got %v", got)
	}
	peer = netgearMAC
	if got := d.resolveDownlinks(); len(got) != 1 || got[0].MAC != netgearMAC || got[0].Port != 2 {
		t.Errorf("downlinks = %v", got)
	}
}
