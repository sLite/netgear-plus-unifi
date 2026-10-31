package provision_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"netgear-plus-unifi/internal/mock"
	. "netgear-plus-unifi/internal/provision"
)

// UniFi: networks Default (1) and VLAN 3901; port 4 is an access port in
// 3901, ports without overrides use UniFi's "allow all" defaults.
const accessPortCfg = `users.1.name=admin
switch.managementvlan=1
switch.vlan.1.id=1
switch.vlan.1.mode=untagged
switch.vlan.2.id=3901
switch.vlan.2.mode=tagged
switch.port.4.pvid=3901
switch.vlan.1.port.4.mode=exclude
switch.vlan.2.port.4.mode=untagged
switch.port.5.status=disabled
`

const defaultCfg = "switch.vlan.1.id=1\nswitch.vlan.1.mode=untagged\n"

type clock struct{ now time.Time }

func (c *clock) Now() time.Time               { return c.now }
func (c *clock) advance(d time.Duration)      { c.now = c.now.Add(d) }
func newClock() *clock                        { return &clock{now: time.Unix(1_800_000_000, 0)} }
func submit(s *Sync, version, cfg string)     { s.Submit(version, SwitchSection(cfg)) }
func writerOf(w Writer) func() (Writer, bool) { return func() (Writer, bool) { return w, true } }

type harness struct {
	clock   *clock
	backend *mock.Backend
	writer  Writer
}

func newSyncHarness() *harness {
	b := &mock.Backend{Config: FactoryDefault(8)}
	return &harness{clock: newClock(), backend: b, writer: memWriter(b)}
}

func (h *harness) sync(enabled, dryRun bool) *Sync {
	p := DefaultPolicy()
	p.Enabled, p.DryRun, p.Debounce = enabled, dryRun, 5*time.Second
	return NewSync("sw", p, h.clock.Now)
}

func (h *harness) tick(s *Sync) { s.Tick(context.Background(), writerOf(h.writer)) }

func TestDisabledPolicyOnlyAcknowledges(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(false, true)
	submit(s, "v1", accessPortCfg)
	if st := s.Status(); st.ProcessedCfgVersion != "v1" || st.State != StateDisabled {
		t.Errorf("status = %+v", st)
	}
	h.tick(s)
	if h.backend.Config.PVIDs[4] != 1 {
		t.Error("disabled sync wrote to the switch")
	}
}

func TestDryRunPlansButDoesNotWrite(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(true, true)
	submit(s, "v1", accessPortCfg)
	h.tick(s)
	if s.Status().State != StatePending {
		t.Fatalf("state during debounce = %s", s.Status().State)
	}
	h.clock.advance(6 * time.Second)
	h.tick(s)
	st := s.Status()
	want := []string{
		"create VLAN 3901 with port 1=T, port 2=T, port 3=T, port 4=U, port 6=T, port 7=T, port 8=T", // port 5 disabled: native only
		"port 4: PVID 3901",
		"VLAN 1: set port 4=E",
		`port 5: speed disabled, name ""`,
	}
	if st.State != StateDryRun || st.ProcessedCfgVersion != "v1" || !reflect.DeepEqual(st.LastPlan, want) {
		t.Errorf("status = %+v", st)
	}
	if h.backend.Config.PVIDs[4] != 1 {
		t.Error("dry run wrote to the switch")
	}
}

func TestLiveApplyManagesTheWholeSwitch(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(true, false)
	submit(s, "v1", accessPortCfg)
	h.clock.advance(6 * time.Second)
	h.tick(s)
	if st := s.Status(); st.State != StateInSync || st.ProcessedCfgVersion != "v1" {
		t.Fatalf("status = %+v", st)
	}
	cfg := h.backend.Config
	want3901 := all(8, T)
	want3901[4] = U
	delete(want3901, 5) // disabled: native VLAN only
	if !maps.Equal(cfg.VLANs[3901], want3901) || cfg.Member(1, 4) != E || len(cfg.VLANs[1]) != 7 {
		t.Errorf("VLANs = %v", cfg.VLANs)
	}
	if cfg.PVIDs[4] != 3901 || cfg.Ports[5].Speed != SpeedDisabled {
		t.Errorf("PVID %d, port 5 %v", cfg.PVIDs[4], cfg.Ports[5])
	}

	// Restart: the device re-submits the stored configuration; the new sync
	// re-verifies and finds nothing to do.
	restarted := h.sync(true, false)
	submit(restarted, "v1", accessPortCfg)
	h.clock.advance(6 * time.Second)
	h.tick(restarted)
	if st := restarted.Status(); st.State != StateInSync || len(st.LastPlan) != 0 {
		t.Errorf("after restart: %+v", st)
	}
}

func TestRevertingInUniFiRestoresDefaults(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(true, false)
	submit(s, "v1", accessPortCfg)
	h.clock.advance(6 * time.Second)
	h.tick(s)
	submit(s, "v2", defaultCfg)
	h.clock.advance(6 * time.Second)
	h.tick(s)
	cfg := h.backend.Config
	if len(cfg.VLANs) != 1 || !maps.Equal(cfg.VLANs[1], all(8, U)) || cfg.PVIDs[4] != 1 || cfg.Ports[5].Speed != SpeedAuto {
		t.Errorf("not back to defaults: %v %v %v", cfg.VLANs, cfg.PVIDs, cfg.Ports[5])
	}
}

func TestDriftIsCorrected(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(true, false)
	submit(s, "v1", accessPortCfg)
	h.clock.advance(6 * time.Second)
	h.tick(s)
	h.backend.Config.PVIDs[4] = 1 // someone changed it in the NETGEAR web UI
	h.clock.advance(100 * time.Second)
	h.tick(s)
	if h.backend.Config.PVIDs[4] != 1 {
		t.Fatal("rechecked before the recheck interval")
	}
	h.clock.advance(300 * time.Second)
	h.tick(s)
	if h.backend.Config.PVIDs[4] != 3901 {
		t.Error("drift not corrected")
	}
}

type brokenWriter struct{ Writer }

func (brokenWriter) ReadConfig(context.Context) (*Config, error) {
	return nil, errors.New("switch unreachable")
}

func TestFailuresRetryThenGiveUp(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(true, false)
	submit(s, "v1", accessPortCfg)
	h.clock.advance(6 * time.Second)
	for range 3 {
		s.Tick(context.Background(), writerOf(brokenWriter{h.writer}))
		if s.Status().State == StateFailed {
			break
		}
		if st := s.Status(); st.State != StatePending || st.ProcessedCfgVersion == "v1" {
			t.Fatalf("before giving up: %+v", st)
		}
		h.clock.advance(200 * time.Second)
	}
	st := s.Status()
	// Given up and acknowledged, so UniFi stops re-sending; the failure is
	// visible in status and logs.
	if st.State != StateFailed || st.ProcessedCfgVersion != "v1" || !strings.Contains(st.LastError, "switch unreachable") {
		t.Errorf("status = %+v", st)
	}
}

func TestUnsupportedModel(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(true, false)
	submit(s, "v1", accessPortCfg)
	h.clock.advance(6 * time.Second)
	s.Tick(context.Background(), func() (Writer, bool) { return nil, false })
	if st := s.Status(); st.State != StateUnsupported || st.ProcessedCfgVersion != "v1" {
		t.Errorf("status = %+v", st)
	}
}

func TestIdenticalResubmissionDoesNotRestartDebounce(t *testing.T) {
	h := newSyncHarness()
	s := h.sync(true, false)
	submit(s, "v1", accessPortCfg)
	h.clock.advance(4 * time.Second)
	submit(s, "v1", accessPortCfg)
	h.clock.advance(2 * time.Second)
	h.tick(s)
	if s.Status().State != StateInSync {
		t.Errorf("state = %s", s.Status().State)
	}
}

func TestNewVersionWithIdenticalSwitchSectionIsAcknowledgedAfterCheck(t *testing.T) {
	// UniFi bumps cfgversion for changes outside the switch section (e.g.
	// IP settings); the switch is verified and the version processed.
	h := newSyncHarness()
	s := h.sync(true, false)
	submit(s, "v1", defaultCfg)
	h.clock.advance(6 * time.Second)
	h.tick(s)
	submit(s, "v2", defaultCfg+"netconf.1.ip=192.0.2.9\n")
	if s.Status().Processed("v2") {
		t.Fatal("processed before the check")
	}
	h.clock.advance(6 * time.Second)
	h.tick(s)
	if st := s.Status(); !st.Processed("v2") || st.State != StateInSync {
		t.Errorf("status = %+v", st)
	}
}
