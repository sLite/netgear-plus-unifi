package provision

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Writer reads and changes the configuration of one physical switch.
type Writer interface {
	Capabilities() Capabilities
	PortCount() int
	ReadConfig(ctx context.Context) (*Config, error)
	// Apply performs one operation and verifies it by reading back.
	Apply(ctx context.Context, op Operation) error
}

// Policy controls whether and how UniFi configuration reaches a switch.
type Policy struct {
	Enabled bool
	DryRun  bool
	// Debounce waits for UniFi to finish a burst of changes.
	Debounce time.Duration
	// MaxAttempts failed applies before a cfgversion is given up on.
	MaxAttempts int
	// Retry is the delay before attempt n+1 (multiplied by n).
	Retry time.Duration
	// Recheck is how often a switch in sync is checked for drift (changes
	// made in the NETGEAR web UI).
	Recheck time.Duration
}

// DefaultPolicy returns the timing defaults with writes disabled.
func DefaultPolicy() Policy {
	return Policy{DryRun: true, Debounce: 10 * time.Second, MaxAttempts: 3, Retry: 30 * time.Second, Recheck: 5 * time.Minute}
}

// State is the sync progress of one switch.
type State string

const (
	StateDisabled    State = "disabled" // writes off: configuration is only acknowledged
	StateWaiting     State = "waiting"  // no configuration from UniFi yet
	StatePending     State = "pending"  // new configuration, debounce or retry pending
	StateInSync      State = "in_sync"
	StateDryRun      State = "dry_run" // plan computed and logged, nothing written
	StateFailed      State = "failed"  // gave up on this cfgversion
	StateUnsupported State = "unsupported"
)

// Status is the externally visible sync state.
type Status struct {
	Enabled             bool     `json:"enabled"`
	DryRun              bool     `json:"dry_run"`
	State               State    `json:"state"`
	DesiredCfgVersion   string   `json:"desired_cfgversion,omitempty"`
	ProcessedCfgVersion string   `json:"processed_cfgversion,omitempty"`
	LastPlan            []string `json:"last_plan"`
	Warnings            []string `json:"warnings"`
	LastError           string   `json:"last_error,omitempty"`
}

// Processed reports whether cfgversion was applied and verified, planned in
// dry-run mode, acknowledged without writes, or given up on.
func (s Status) Processed(cfgversion string) bool { return s.ProcessedCfgVersion == cfgversion }

// Sync keeps one switch in sync with UniFi's port configuration. Submit is
// called by the UniFi device; Tick by the poller after every successful
// poll, so writes share the polling session and never overlap with reads.
//
// UniFi only sees a cfgversion as provisioned once it is processed, i.e. the
// hardware matches (or the failure is visible in logs and status).
type Sync struct {
	switchID string
	policy   Policy
	now      func() time.Time
	log      *slog.Logger

	mu          sync.Mutex
	desired     string // cfgversion
	desiredText string
	processed   string
	state       State
	submittedAt time.Time
	attempts    int
	nextAttempt time.Time
	lastCheck   time.Time
	lastPlan    []string
	warnings    []string
	lastError   string
}

// NewSync returns the sync for one switch; now defaults to time.Now.
func NewSync(switchID string, policy Policy, now func() time.Time) *Sync {
	if now == nil {
		now = time.Now
	}
	s := &Sync{switchID: switchID, policy: policy, now: now, log: slog.With("switch", switchID), state: StateDisabled}
	if policy.Enabled {
		s.state = StateWaiting
	}
	return s
}

// Policy returns the configured policy.
func (s *Sync) Policy() Policy { return s.policy }

// Submit records UniFi's switch configuration (the output of
// SwitchSection) for cfgversion. Identical resubmissions are ignored.
func (s *Sync) Submit(cfgversion, switchConfig string) {
	s.mu.Lock()
	if cfgversion == s.desired && switchConfig == s.desiredText {
		s.mu.Unlock()
		return
	}
	s.desired, s.desiredText = cfgversion, switchConfig
	s.submittedAt = s.now()
	s.attempts, s.nextAttempt = 0, time.Time{}
	if s.policy.Enabled {
		s.state = StatePending
	} else {
		s.processed = cfgversion
	}
	s.mu.Unlock()
	s.log.Info("received UniFi configuration", "cfgversion", cfgversion)
}

// Status returns a copy of the current state.
func (s *Sync) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Enabled: s.policy.Enabled, DryRun: s.policy.DryRun, State: s.state,
		DesiredCfgVersion: s.desired, ProcessedCfgVersion: s.processed,
		LastPlan: append([]string{}, s.lastPlan...), Warnings: append([]string{}, s.warnings...),
		LastError: s.lastError,
	}
}

// Tick applies pending configuration or checks for drift when due. writer
// returns the switch's writer, or false when the model cannot be written.
func (s *Sync) Tick(ctx context.Context, writer func() (Writer, bool)) {
	if !s.policy.Enabled || !s.due() {
		return
	}
	s.mu.Lock()
	version, text := s.desired, s.desiredText
	s.mu.Unlock()
	w, ok := writer()
	if !ok {
		s.finish(version, StateUnsupported, nil, nil, "no write support for this model")
		return
	}
	if err := s.sync(ctx, w, version, text); err != nil {
		s.onError(version, err)
	}
}

func (s *Sync) due() bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.desired == "":
		return false
	case s.processed != s.desired:
		return now.Sub(s.submittedAt) >= s.policy.Debounce && !now.Before(s.nextAttempt)
	default:
		// In sync: re-check periodically to catch drift.
		return s.state == StateInSync && now.Sub(s.lastCheck) >= s.policy.Recheck
	}
}

func (s *Sync) sync(ctx context.Context, w Writer, version, text string) error {
	parsed := ParseUniFi(text, w.PortCount())
	actual, err := w.ReadConfig(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lastCheck = s.now()
	drift := s.processed == version
	s.mu.Unlock()
	plan := MakePlan(parsed.Config, actual, w.PortCount(), w.Capabilities())
	warnings := append(parsed.Warnings, plan.Warnings...)
	steps := plan.Describe()
	switch {
	case len(plan.Operations) == 0:
		s.finish(version, StateInSync, steps, warnings, "")
		return nil
	case s.policy.DryRun:
		s.log.Info("dry run, would apply", "cfgversion", version, "plan", strings.Join(steps, "; "))
		s.finish(version, StateDryRun, steps, warnings, "")
		return nil
	}
	if drift {
		s.log.Warn("switch drifted from UniFi configuration, re-applying")
	}
	for _, op := range plan.Operations {
		if err := w.Apply(ctx, op); err != nil {
			return err
		}
	}
	after, err := w.ReadConfig(ctx)
	if err != nil {
		return err
	}
	if rest := MakePlan(parsed.Config, after, w.PortCount(), w.Capabilities()); len(rest.Operations) > 0 {
		return errors.New("not converged: " + strings.Join(rest.Describe(), "; "))
	}
	s.log.Info("applied UniFi configuration", "cfgversion", version, "plan", strings.Join(steps, "; "))
	s.finish(version, StateInSync, steps, warnings, "")
	return nil
}

func (s *Sync) finish(version string, state State, steps, warnings []string, lastError string) {
	s.mu.Lock()
	if version != s.desired {
		s.mu.Unlock()
		return // superseded while we worked; the next tick handles it
	}
	changed := s.processed != version || s.state != state
	s.processed, s.state = version, state
	s.lastPlan, s.warnings, s.lastError = steps, warnings, lastError
	s.attempts = 0
	s.mu.Unlock()
	if changed {
		for _, w := range warnings {
			s.log.Warn(w)
		}
	}
}

func (s *Sync) onError(version string, err error) {
	s.mu.Lock()
	s.attempts++
	attempts := s.attempts
	s.lastError = err.Error()
	s.nextAttempt = s.now().Add(s.policy.Retry * time.Duration(attempts))
	steps, warnings := s.lastPlan, s.warnings
	s.mu.Unlock()
	if attempts < s.policy.MaxAttempts {
		s.log.Warn("apply failed", "attempt", attempts, "err", err)
		return
	}
	s.log.Error("giving up on UniFi configuration", "cfgversion", version, "attempts", attempts, "err", err)
	s.finish(version, StateFailed, steps, warnings, err.Error())
}
