package poller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"netgear-plus-unifi/internal/netgear"
	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/telemetry"
)

func TestMain(m *testing.M) {
	if os.Getenv("TEST_LOGS") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}

type fakeSource struct {
	now     *time.Time
	errs    []error // returned in order, then success
	panicky bool
	closed  int
	writes  int
}

func (f *fakeSource) Read(context.Context) (telemetry.Reading, error) {
	if f.panicky {
		panic("parser bug")
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return telemetry.Reading{}, err
	}
	return telemetry.Reading{Device: telemetry.Device{Model: "GS308Ev4"},
		Ports: []telemetry.PortReading{{Index: 1, LinkUp: true}}, At: *f.now}, nil
}

func (f *fakeSource) Writer() (provision.Writer, bool) { f.writes++; return nil, false }
func (f *fakeSource) Close()                           { f.closed++ }

type harness struct {
	now time.Time
	src *fakeSource
	p   *Poller
}

func newHarness(errs ...error) *harness {
	h := &harness{now: time.Unix(1_800_000_000, 0)}
	h.src = &fakeSource{now: &h.now, errs: errs}
	policy := provision.DefaultPolicy()
	policy.Enabled = true
	h.p = New("sw", h.src, provision.NewSync("sw", policy, func() time.Time { return h.now }), Options{
		Interval: 15 * time.Second, SessionYield: 5 * time.Minute, StaleAfter: 90 * time.Second,
		Now: func() time.Time { return h.now },
	})
	return h
}

func TestStateFollowsSnapshotAge(t *testing.T) {
	h := newHarness()
	if st := h.p.Status(); st.State != StateStarting {
		t.Errorf("before the first poll: %s", st.State)
	}
	if _, reason := h.p.Snapshot(h.now); !strings.Contains(reason, "not polled") {
		t.Errorf("reason = %q", reason)
	}
	h.p.PollOnce(t.Context())
	if snap, _ := h.p.Snapshot(h.now); snap == nil || h.p.Status().State != StateOK {
		t.Fatal("fresh snapshot not usable")
	}
	h.now = h.now.Add(91 * time.Second)
	if snap, reason := h.p.Snapshot(h.now); snap != nil || !strings.Contains(reason, "stale") {
		t.Errorf("stale snapshot used (reason %q)", reason)
	}
	if st := h.p.Status(); st.State != StateUnavailable || st.AgeSeconds == nil || *st.AgeSeconds < 90 {
		t.Errorf("status = %+v", st)
	}
}

func TestFailureBeforeFirstSuccessIsUnavailable(t *testing.T) {
	h := newHarness(errors.New("timeout"))
	h.p.PollOnce(t.Context())
	if st := h.p.Status(); st.State != StateUnavailable || st.LastError != "timeout" || st.ConsecutiveFailures != 1 {
		t.Errorf("status = %+v", st)
	}
	if _, reason := h.p.Snapshot(h.now); !strings.Contains(reason, "timeout") {
		t.Errorf("reason = %q", reason)
	}
}

func TestBackoffDoublesAndRecoveryResets(t *testing.T) {
	h := newHarness(errors.New("a"), errors.New("b"), errors.New("c"))
	var delays []time.Duration
	for range 4 {
		delays = append(delays, h.p.PollOnce(t.Context()))
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 15 * time.Second}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Errorf("delays = %v, want %v", delays, want)
	}
	if st := h.p.Status(); st.ConsecutiveFailures != 0 || st.LastError != "" {
		t.Errorf("not reset after recovery: %+v", st)
	}
}

func TestSlowRetryForAuthSessionAndIdentityErrors(t *testing.T) {
	for _, err := range []error{netgear.ErrAuthentication, netgear.ErrSessionLimit, netgear.ErrIdentityMismatch} {
		h := newHarness(fmt.Errorf("login: %w", err))
		if d := h.p.PollOnce(t.Context()); d != maxBackoff {
			t.Errorf("%v: delay %s, want %s", err, d, maxBackoff)
		}
	}
}

func TestDisplacedSessionYields(t *testing.T) {
	h := newHarness(fmt.Errorf("GET x: %w", netgear.ErrSessionDisplaced))
	if d := h.p.PollOnce(t.Context()); d != 5*time.Minute {
		t.Errorf("delay = %s, want the session yield", d)
	}
}

func TestPanicDoesNotKillThePoller(t *testing.T) {
	h := newHarness()
	h.src.panicky = true
	if d := h.p.PollOnce(t.Context()); d != 15*time.Second {
		t.Errorf("delay = %s", d)
	}
	if st := h.p.Status(); !strings.Contains(st.LastError, "internal error") || h.src.closed != 1 {
		t.Errorf("status = %+v, closed %d", st, h.src.closed)
	}
}

func TestSyncTicksOnlyAfterSuccessfulPolls(t *testing.T) {
	h := newHarness(errors.New("down"))
	h.p.SubmitConfig("v1", "switch.vlan.1.id=1")
	h.now = h.now.Add(time.Minute) // past the debounce
	h.p.PollOnce(t.Context())
	if h.src.writes != 0 {
		t.Fatal("sync ran after a failed poll")
	}
	h.p.PollOnce(t.Context())
	if h.src.writes != 1 || h.p.ConfigStatus().State != provision.StateUnsupported {
		t.Errorf("writes %d, sync %+v", h.src.writes, h.p.ConfigStatus())
	}
}

func TestRunClosesTheSourceOnStop(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { h.p.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if h.src.closed != 1 {
		t.Errorf("closed %d times", h.src.closed)
	}
}
