// Package mock simulates NETGEAR switches for development without hardware.
//
// Port layout, speeds and traffic are derived from a seed so runs are
// reproducible; counters grow with elapsed time so rates look realistic. An
// optional flapping port and failure rate exercise link-state propagation
// and the poller's error handling end to end. Configuration writes go to an
// in-memory switch through the same verified writer as real hardware.
package mock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/telemetry"
)

// Models are the simulated models: port count and firmware.
var Models = map[string]struct {
	Ports    int
	Firmware string
}{
	"GS308Ev4": {8, "V1.0.1.9"},
	"GS116Ev2": {16, "2.6.0.50"},
}

// Options configure a simulated switch.
type Options struct {
	Model       string
	Seed        uint64
	FailureRate float64
	FlapPort    int // 0 = none
	FlapPeriod  time.Duration
}

// Caps are the simulated write capabilities (every speed but forced 1000).
var Caps = provision.Capabilities{
	Speeds: []provision.Speed{provision.SpeedAuto, provision.SpeedDisabled, provision.Speed10Half,
		provision.Speed10Full, provision.Speed100Half, provision.Speed100Full},
	MaxVLANs: 64,
}

var speeds = []int{1000, 1000, 100, 1000, 10}

// Switch is a simulated switch.
type Switch struct {
	opts     Options
	now      func() time.Time
	start    time.Time
	last     time.Time
	failures *rand.Rand
	links    []bool
	speeds   []int
	rates    []float64
	rx, tx   []float64
	device   telemetry.Device
	config   *provision.Config
	log      *slog.Logger
}

// New returns a simulated switch; now defaults to time.Now.
func New(id string, opts Options, now func() time.Time) (*Switch, error) {
	m, ok := Models[opts.Model]
	if !ok {
		return nil, fmt.Errorf("mock model %q is not one of GS308Ev4, GS116Ev2", opts.Model)
	}
	if now == nil {
		now = time.Now
	}
	layout := rand.New(rand.NewPCG(opts.Seed, 1))
	s := &Switch{
		opts: opts, now: now, start: now(), failures: rand.New(rand.NewPCG(opts.Seed, 2)),
		config: provision.FactoryDefault(m.Ports), log: slog.With("switch", id),
	}
	s.last = s.start
	for i := range m.Ports {
		s.links = append(s.links, layout.Float64() < 0.6)
		s.speeds = append(s.speeds, speeds[layout.IntN(len(speeds))])
		s.rates = append(s.rates, 1e3+layout.Float64()*2e6)
		// Counters integrate traffic over time so a port going down keeps
		// its totals, like real hardware.
		s.rx = append(s.rx, 1e6*float64(i+1))
		s.tx = append(s.tx, 5e5*float64(i+1))
	}
	var advertised []string
	for _, sp := range Caps.AdvertisedSpeeds() {
		advertised = append(advertised, string(sp))
	}
	s.device = telemetry.Device{
		Vendor: "netgear", Model: opts.Model, Firmware: m.Firmware, Serial: fmt.Sprintf("MOCK%09d", opts.Seed),
		ManagementIP: "198.51.100.10", PortCount: m.Ports, PortSpeeds: advertised,
	}
	return s, nil
}

// Read returns simulated port state.
func (s *Switch) Read(context.Context) (telemetry.Reading, error) {
	if s.opts.FailureRate > 0 && s.failures.Float64() < s.opts.FailureRate {
		return telemetry.Reading{}, errors.New("simulated switch timeout")
	}
	now := s.now()
	elapsed, dt := now.Sub(s.start), now.Sub(s.last).Seconds()
	s.last = now
	ports := make([]telemetry.PortReading, len(s.links))
	for i := range s.links {
		ports[i] = s.port(i, elapsed, dt)
	}
	return telemetry.Reading{Device: s.device, Ports: ports, At: now}, nil
}

func (s *Switch) port(i int, elapsed time.Duration, dt float64) telemetry.PortReading {
	index := i + 1
	up := s.links[i] && s.config.Ports[index].Speed != provision.SpeedDisabled
	if index == s.opts.FlapPort && s.opts.FlapPeriod > 0 {
		up = (elapsed/(s.opts.FlapPeriod/2))%2 == 0
	}
	speed := 0
	if up {
		s.rx[i] += s.rates[i] * dt
		s.tx[i] += s.rates[i] * 0.4 * dt
		speed = s.speeds[i]
	}
	r := telemetry.PortReading{
		Index: index, LinkUp: up, SpeedMbps: &speed, Autoneg: ptr(true), Enabled: ptr(true),
		Name:    s.config.Ports[index].Name.Value,
		RXBytes: ptr(uint64(s.rx[i])), TXBytes: ptr(uint64(s.tx[i])), CRCErrors: ptr(uint64(0)),
	}
	if up {
		r.FullDuplex = ptr(true)
	}
	return r
}

// Writer returns a writer on the in-memory configuration.
func (s *Switch) Writer() (provision.Writer, bool) {
	return provision.NewVerifiedWriter(&Backend{Config: s.config}, len(s.links), Caps, s.log), true
}

// Close does nothing.
func (s *Switch) Close() {}

// Backend is an in-memory switch configuration (also used by tests).
type Backend struct{ Config *provision.Config }

func (b *Backend) ReadConfig(context.Context) (*provision.Config, error) {
	return b.Config.Clone(), nil
}

func (b *Backend) CreateVLAN(_ context.Context, vid int, _ provision.Members) error {
	b.Config.VLANs[vid] = provision.Members{}
	return nil
}

func (b *Backend) WriteMembers(_ context.Context, vid int, members provision.Members) error {
	b.Config.VLANs[vid] = members
	return nil
}

func (b *Backend) SetPVID(_ context.Context, port, vid int) error {
	b.Config.PVIDs[port] = vid
	return nil
}

func (b *Backend) DeleteVLAN(_ context.Context, vid int) error {
	delete(b.Config.VLANs, vid)
	return nil
}

func (b *Backend) SetPort(_ context.Context, op provision.SetPort) error {
	b.Config.Ports[op.Port] = provision.PortSettings{Speed: op.Speed, Name: provision.SetName(op.Name)}
	return nil
}

func (b *Backend) SetSwitchName(_ context.Context, name string) error {
	b.Config.SwitchName = provision.SetName(name)
	return nil
}

func ptr[T any](v T) *T { return &v }
