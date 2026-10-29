// Package telemetry is the vendor-independent switch model: raw readings as
// a switch reports them, and snapshots with traffic rates derived from
// successive readings. A nil pointer always means "the switch does not
// report this", never zero.
package telemetry

import (
	"log/slog"
	"time"
)

// Device identifies the physical switch. These values never become part of
// the virtual UniFi identity, except HardwareMAC with unifi.mac: physical.
type Device struct {
	Vendor       string `json:"vendor"`
	Model        string `json:"model"`
	Firmware     string `json:"firmware"`
	Serial       string `json:"serial"`
	ManagementIP string `json:"management_ip"`
	PortCount    int    `json:"port_count"`
	// HardwareMAC is the switch's own (burned-in) MAC, as the rest of the
	// network sees it.
	HardwareMAC string `json:"hardware_mac"`
	// PortSpeeds lists the speed settings every port accepts ("auto",
	// "10h", "10f", "100h", "100f", "1000f"); empty if unknown.
	PortSpeeds []string `json:"port_speeds"`
}

// PortReading is one port as read from the switch, with raw cumulative
// counters (bytes since the switch booted or its statistics were cleared,
// the same semantics a UniFi switch reports).
type PortReading struct {
	Index      int     `json:"index"`
	Name       string  `json:"name"`
	LinkUp     bool    `json:"link_up"`
	SpeedMbps  *int    `json:"speed_mbps"`
	FullDuplex *bool   `json:"full_duplex"`
	Autoneg    *bool   `json:"autoneg"`
	Enabled    *bool   `json:"enabled"`
	RXBytes    *uint64 `json:"rx_bytes"`
	TXBytes    *uint64 `json:"tx_bytes"`
	CRCErrors  *uint64 `json:"crc_errors"`
}

// Reading is one complete poll of a switch.
type Reading struct {
	Device Device
	Ports  []PortReading
	At     time.Time // must carry a monotonic clock reading (time.Now)
}

// Port is a port with rates derived from successive readings.
type Port struct {
	PortReading
	RXBytesPerS *float64 `json:"rx_bytes_per_s"`
	TXBytesPerS *float64 `json:"tx_bytes_per_s"`
}

// Snapshot is the latest successful reading of a switch, with rates.
type Snapshot struct {
	Device      Device    `json:"device"`
	Ports       []Port    `json:"ports"`
	CollectedAt time.Time `json:"collected_at"`
}

const (
	// lineRateTolerance is the headroom over line rate: the switch updates
	// counters asynchronously to our HTTP requests, so a short interval can
	// legitimately overshoot a little.
	lineRateTolerance = 1.25
	// fallbackMaxMbps is used when the link speed is unknown: nothing on
	// these switches exceeds 10G.
	fallbackMaxMbps = 10_000
)

// RateCalculator derives rates from successive readings of one switch. Not
// safe for concurrent use.
//
//   - The first reading of a port has no rate.
//   - A counter that goes backwards is a reset (reboot or "clear counters"):
//     the rate for that interval is unknown.
//   - A rate above what the negotiated link allows is a sampling glitch and
//     is dropped rather than reported.
type RateCalculator struct {
	log      *slog.Logger
	previous *Reading
}

// NewRateCalculator returns a calculator for switchID.
func NewRateCalculator(switchID string) *RateCalculator {
	return &RateCalculator{log: slog.With("switch", switchID)}
}

// Update turns a reading into a snapshot.
func (c *RateCalculator) Update(r Reading) Snapshot {
	prev := map[int]PortReading{}
	var dt float64
	if c.previous != nil {
		for _, p := range c.previous.Ports {
			prev[p.Index] = p
		}
		dt = r.At.Sub(c.previous.At).Seconds()
	}
	ports := make([]Port, len(r.Ports))
	for i, cur := range r.Ports {
		maxMbps := fallbackMaxMbps
		if cur.LinkUp && cur.SpeedMbps != nil && *cur.SpeedMbps > 0 {
			maxMbps = *cur.SpeedMbps
		}
		p, seen := prev[cur.Index]
		ports[i] = Port{PortReading: cur}
		if seen {
			ports[i].RXBytesPerS = c.rate(cur.Index, "rx", p.RXBytes, cur.RXBytes, dt, maxMbps)
			ports[i].TXBytesPerS = c.rate(cur.Index, "tx", p.TXBytes, cur.TXBytes, dt, maxMbps)
		}
	}
	c.previous = &r
	return Snapshot{Device: r.Device, Ports: ports, CollectedAt: r.At}
}

func (c *RateCalculator) rate(port int, direction string, prev, cur *uint64, dt float64, maxMbps int) *float64 {
	if prev == nil || cur == nil || dt <= 0 {
		return nil
	}
	if *cur < *prev {
		c.log.Info("counter reset detected", "port", port, "direction", direction, "from", *prev, "to", *cur)
		return nil
	}
	rate := float64(*cur-*prev) / dt
	if rate > float64(maxMbps)*1e6/8*lineRateTolerance {
		c.log.Debug("implausible rate dropped", "port", port, "direction", direction, "bytes_per_s", rate)
		return nil
	}
	return &rate
}
