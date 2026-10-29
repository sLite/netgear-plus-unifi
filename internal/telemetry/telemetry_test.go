package telemetry

import (
	"io"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("TEST_LOGS") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}

var t0 = time.Unix(1_800_000_000, 0)

func u64(v uint64) *uint64 { return &v }

func reading(sec float64, rx, tx *uint64, speed int) Reading {
	return Reading{
		Device: Device{Model: "GS308Ev4", PortCount: 1},
		Ports:  []PortReading{{Index: 1, LinkUp: true, SpeedMbps: &speed, RXBytes: rx, TXBytes: tx}},
		At:     t0.Add(time.Duration(sec * float64(time.Second))),
	}
}

func near(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", what, deref(got), want)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestFirstReadingHasNoRate(t *testing.T) {
	p := NewRateCalculator("sw").Update(reading(0, u64(100), u64(200), 1000)).Ports[0]
	if *p.RXBytes != 100 || *p.TXBytes != 200 || p.RXBytesPerS != nil || p.TXBytesPerS != nil {
		t.Errorf("port = %+v", p)
	}
}

func TestRateIsBytesPerSecond(t *testing.T) {
	c := NewRateCalculator("sw")
	c.Update(reading(0, u64(1_000), u64(2_000), 1000))
	p := c.Update(reading(10, u64(11_000), u64(52_000), 1000)).Ports[0]
	near(t, "rx", p.RXBytesPerS, 1_000)
	near(t, "tx", p.TXBytesPerS, 5_000)
}

func TestCounterResetHasNoRateForThatInterval(t *testing.T) {
	c := NewRateCalculator("sw")
	c.Update(reading(0, u64(5_000_000), u64(5_000_000), 1000))
	p := c.Update(reading(10, u64(1_000), u64(6_000_000), 1000)).Ports[0]
	if *p.RXBytes != 1_000 || p.RXBytesPerS != nil { // raw counter passed through
		t.Errorf("port after reset = %+v", p)
	}
	near(t, "tx", p.TXBytesPerS, 100_000)
	near(t, "rx after reset", c.Update(reading(20, u64(11_000), u64(6_000_000), 1000)).Ports[0].RXBytesPerS, 1_000)
}

func TestRateAboveLineRateIsDropped(t *testing.T) {
	c := NewRateCalculator("sw")
	c.Update(reading(0, u64(0), u64(0), 100))
	// 100 Mbit/s = 12.5 MB/s; 50 MB/s in one second is a sampling glitch.
	p := c.Update(reading(1, u64(50_000_000), u64(1_000_000), 100)).Ports[0]
	if p.RXBytesPerS != nil {
		t.Errorf("implausible rate kept: %v", *p.RXBytesPerS)
	}
	near(t, "tx", p.TXBytesPerS, 1_000_000)
}

func TestUnknownCountersStayUnknown(t *testing.T) {
	c := NewRateCalculator("sw")
	c.Update(reading(0, nil, nil, 1000))
	if p := c.Update(reading(10, nil, nil, 1000)).Ports[0]; p.RXBytes != nil || p.RXBytesPerS != nil {
		t.Errorf("port = %+v", p)
	}
}

func TestNonIncreasingTimeYieldsNoRate(t *testing.T) {
	c := NewRateCalculator("sw")
	c.Update(reading(5, u64(0), u64(0), 1000))
	if p := c.Update(reading(5, u64(100), u64(100), 1000)).Ports[0]; p.RXBytesPerS != nil {
		t.Errorf("rate without elapsed time: %v", *p.RXBytesPerS)
	}
}
