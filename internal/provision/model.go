// Package provision holds the vendor-independent switch configuration model,
// the parser for UniFi's switch configuration, the planner that diffs desired
// against actual configuration, and the sync loop that applies the plan.
package provision

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Member is the membership of a port in an 802.1Q VLAN.
type Member string

const (
	Untagged Member = "U"
	Tagged   Member = "T"
	Excluded Member = "E"
)

// Speed is an administrative port speed setting. Disabled is how NETGEAR
// Plus switches (and this model) express "port administratively down".
type Speed string

const (
	SpeedAuto     Speed = "auto"
	SpeedDisabled Speed = "disabled"
	Speed10Half   Speed = "10h"
	Speed10Full   Speed = "10f"
	Speed100Half  Speed = "100h"
	Speed100Full  Speed = "100f"
	Speed1000Full Speed = "1000f"
)

// AllSpeeds lists every speed setting in UniFi's display order.
var AllSpeeds = []Speed{SpeedAuto, SpeedDisabled, Speed10Half, Speed10Full, Speed100Half, Speed100Full, Speed1000Full}

// Name is a port or switch name. In desired configuration the zero value
// leaves the switch's name alone and SetName("") clears it; in actual
// configuration an empty Value means unnamed.
type Name struct {
	Value string
	Set   bool
}

// SetName returns a Name that sets (or, for "", clears) the name.
func SetName(v string) Name { return Name{Value: v, Set: true} }

// PortSettings are the per-port settings other than VLANs.
type PortSettings struct {
	Speed Speed
	Name  Name
}

// Members maps port number to membership; ports missing are excluded.
type Members map[int]Member

func (m Members) String() string {
	parts := make([]string, 0, len(m))
	for _, p := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("port %d=%s", p, m[p]))
	}
	return strings.Join(parts, ", ")
}

// Config is the VLANs, PVIDs and port settings of one switch, desired or
// actual. VLANs maps VLAN id to the ports that are members.
type Config struct {
	VLANs map[int]Members
	PVIDs map[int]int
	Ports map[int]PortSettings
	// ManagementVLAN is the VLAN the switch's own management is reachable
	// in (actual configuration only).
	ManagementVLAN int
	// SwitchName is the switch (system) name. Desired: the zero value
	// leaves it alone.
	SwitchName Name
}

// NewConfig returns an empty configuration with management VLAN 1.
func NewConfig() *Config {
	return &Config{
		VLANs:          map[int]Members{},
		PVIDs:          map[int]int{},
		Ports:          map[int]PortSettings{},
		ManagementVLAN: DefaultVLAN,
	}
}

// FactoryDefault is a switch with every port untagged in VLAN 1.
func FactoryDefault(ports int) *Config {
	c := NewConfig()
	c.VLANs[DefaultVLAN] = Members{}
	for p := 1; p <= ports; p++ {
		c.VLANs[DefaultVLAN][p] = Untagged
		c.PVIDs[p] = DefaultVLAN
		c.Ports[p] = PortSettings{Speed: SpeedAuto}
	}
	return c
}

// Member returns the membership of port in VLAN vid.
func (c *Config) Member(vid, port int) Member {
	if m, ok := c.VLANs[vid][port]; ok {
		return m
	}
	return Excluded
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	out := *c
	out.VLANs = make(map[int]Members, len(c.VLANs))
	for vid, m := range c.VLANs {
		out.VLANs[vid] = maps.Clone(m)
	}
	out.PVIDs = maps.Clone(c.PVIDs)
	out.Ports = maps.Clone(c.Ports)
	return &out
}

// Capabilities are what a switch model accepts.
type Capabilities struct {
	Speeds   []Speed
	MaxVLANs int
}

// Supports reports whether s is an accepted speed setting.
func (c Capabilities) Supports(s Speed) bool { return slices.Contains(c.Speeds, s) }

// AdvertisedSpeeds are the speed settings to offer in UniFi: what the switch
// accepts plus 1000 full, which the planner maps to auto (the ports are
// gigabit; leaving it out would make UniFi show them as 100 Mbit/s ports).
func (c Capabilities) AdvertisedSpeeds() []Speed {
	var out []Speed
	for _, s := range AllSpeeds {
		if s != SpeedDisabled && (s == Speed1000Full || c.Supports(s)) {
			out = append(out, s)
		}
	}
	return out
}
