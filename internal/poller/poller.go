// Package poller runs the per-switch polling loop: it reads the physical
// switch, derives rates, applies pending UniFi configuration between reads
// on the same session, and tells the UniFi device whether its telemetry is
// fresh enough to report.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"netgear-plus-unifi/internal/netgear"
	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/telemetry"
)

const (
	maxBackoff = 5 * time.Minute
	// failureLogEvery: repeated failures are logged at WARN this often,
	// DEBUG otherwise.
	failureLogEvery = 10
)

// Source is a physical (or simulated) switch.
type Source interface {
	Read(ctx context.Context) (telemetry.Reading, error)
	// Writer returns a configuration writer on the current session.
	Writer() (provision.Writer, bool)
	// Close releases the switch's session slot.
	Close()
}

// State is the health of a switch's telemetry.
type State string

const (
	StateStarting    State = "starting"
	StateOK          State = "ok"
	StateUnavailable State = "unavailable"
)

// Options configure a poller.
type Options struct {
	Interval time.Duration
	// SessionYield is the pause after the switch dropped our session
	// because someone logged into its web UI.
	SessionYield time.Duration
	// StaleAfter is the snapshot age after which telemetry is unusable and
	// the UniFi device stops informing (UniFi then shows it offline).
	StaleAfter time.Duration
	Now        func() time.Time
}

// Poller polls one switch until its context ends.
type Poller struct {
	id    string
	src   Source
	sync  *provision.Sync
	opts  Options
	rates *telemetry.RateCalculator
	log   *slog.Logger

	mu          sync.Mutex
	snapshot    *telemetry.Snapshot
	lastError   string
	consecutive int
}

// New returns a poller for src; sync receives UniFi configuration.
func New(id string, src Source, sync *provision.Sync, opts Options) *Poller {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Poller{id: id, src: src, sync: sync, opts: opts, rates: telemetry.NewRateCalculator(id), log: slog.With("switch", id)}
}

// Run polls until ctx is cancelled, then logs out of the switch.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("polling", "interval", p.opts.Interval)
	defer func() {
		p.src.Close()
		p.log.Info("stopped polling")
	}()
	for {
		t := time.NewTimer(p.PollOnce(ctx))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// PollOnce polls once and returns the delay before the next poll.
func (p *Poller) PollOnce(ctx context.Context) (delay time.Duration) {
	defer func() {
		if r := recover(); r != nil { // a parser bug must not kill the poller
			p.log.Error("internal error while polling", "panic", r)
			p.src.Close()
			delay = p.onFailure(fmt.Errorf("internal error: %v", r))
		}
	}()
	started := time.Now()
	reading, err := p.src.Read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return p.opts.Interval
		}
		return p.onFailure(err)
	}
	snap := p.rates.Update(reading)
	p.mu.Lock()
	p.snapshot, p.lastError = &snap, ""
	recovered := p.consecutive
	p.consecutive = 0
	p.mu.Unlock()
	if recovered > 0 {
		p.log.Info("switch reachable again", "failed_polls", recovered)
	}
	p.sync.Tick(ctx, p.src.Writer)
	up := 0
	for _, port := range snap.Ports {
		if port.LinkUp {
			up++
		}
	}
	p.log.Debug("polled", "ports_up", up, "ports", len(snap.Ports), "took", time.Since(started).Round(time.Millisecond))
	return p.opts.Interval
}

func (p *Poller) onFailure(err error) time.Duration {
	p.mu.Lock()
	p.lastError = err.Error()
	p.consecutive++
	n := p.consecutive
	p.mu.Unlock()
	if errors.Is(err, netgear.ErrSessionDisplaced) {
		p.log.Info("session displaced, probably by a web UI login; yielding", "for", p.opts.SessionYield)
		return p.opts.SessionYield
	}
	// Failures that will not fix themselves quickly: retrying fast could
	// lock the switch's login or fight with an administrator's browser.
	delay := maxBackoff
	if !errors.Is(err, netgear.ErrAuthentication) && !errors.Is(err, netgear.ErrSessionLimit) &&
		!errors.Is(err, netgear.ErrIdentityMismatch) {
		delay = min(p.opts.Interval<<min(n-1, 16), maxBackoff)
	}
	level := slog.LevelDebug
	if n%failureLogEvery == 1 {
		level = slog.LevelWarn
	}
	p.log.Log(context.Background(), level, "poll failed", "attempt", n, "retry_in", delay, "err", err)
	return delay
}

// Snapshot returns the latest snapshot when it is fresh enough to report,
// or the reason why not.
func (p *Poller) Snapshot(now time.Time) (*telemetry.Snapshot, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.snapshot == nil && p.consecutive == 0:
		return nil, "switch not polled yet"
	case p.snapshot == nil || now.Sub(p.snapshot.CollectedAt) > p.opts.StaleAfter:
		reason := "telemetry stale"
		if p.lastError != "" {
			reason += ": " + p.lastError
		}
		return nil, reason
	}
	return p.snapshot, ""
}

// SubmitConfig hands UniFi's switch configuration to the sync.
func (p *Poller) SubmitConfig(cfgversion, switchConfig string) {
	p.sync.Submit(cfgversion, switchConfig)
}

// ConfigStatus returns the sync progress.
func (p *Poller) ConfigStatus() provision.Status { return p.sync.Status() }

// Status is the poller's state for the status endpoint.
type Status struct {
	State               State               `json:"state"`
	AgeSeconds          *float64            `json:"age_seconds"`
	LastError           string              `json:"last_error,omitempty"`
	ConsecutiveFailures int                 `json:"consecutive_failures"`
	Snapshot            *telemetry.Snapshot `json:"snapshot"`
}

// Status returns the current state.
func (p *Poller) Status() Status {
	now := p.opts.Now()
	snap, _ := p.Snapshot(now)
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{LastError: p.lastError, ConsecutiveFailures: p.consecutive, Snapshot: p.snapshot}
	switch {
	case snap != nil:
		s.State = StateOK
	case p.snapshot == nil && p.consecutive == 0:
		s.State = StateStarting
	default:
		s.State = StateUnavailable
	}
	if p.snapshot != nil {
		age := now.Sub(p.snapshot.CollectedAt).Seconds()
		s.AgeSeconds = &age
	}
	return s
}
