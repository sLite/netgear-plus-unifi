package provision

import (
	"fmt"
	"maps"
	"slices"
)

// Diffing desired against actual configuration into an ordered plan.
//
// UniFi is authoritative for every port of a switch with writes enabled, as
// for a native UniFi switch. Safety properties (unit-tested):
//
//   - VLAN 1 and the switch's management VLAN are never deleted.
//   - VLANs that UniFi does not define, or that would be left without ports,
//     are deleted (the switches cannot hold an empty 802.1Q VLAN).
//   - Membership operations carry only the ports whose membership changes;
//     the writer merges them into a fresh read of the switch.
//   - Ordering keeps ports connected throughout: add memberships, then move
//     PVIDs, then remove memberships and delete VLANs, then change
//     speed/admin state/names, then the switch name.

// DefaultVLAN is the VLAN every port belongs to on a factory-default switch.
const DefaultVLAN = 1

// SwitchNameMax is the longest switch name both NETGEAR UIs accept.
const SwitchNameMax = 20

// Operation is one planned change.
type Operation interface {
	fmt.Stringer
	isOperation()
}

// CreateVLAN creates VLAN VID with the given port memberships.
type CreateVLAN struct {
	VID     int
	Members Members
}

// SetMembers changes the membership of the listed ports in VLAN VID.
// Excluded removes a port from the VLAN.
type SetMembers struct {
	VID     int
	Members Members
}

// SetPVID sets the native VLAN of a port.
type SetPVID struct {
	Port int
	VID  int
}

// DeleteVLAN deletes VLAN VID.
type DeleteVLAN struct{ VID int }

// SetPort sets the speed (or disabled state) and name of a port.
type SetPort struct {
	Port  int
	Speed Speed
	Name  string
}

// SetSwitchName sets the switch (system) name.
type SetSwitchName struct{ Name string }

func (CreateVLAN) isOperation()    {}
func (SetMembers) isOperation()    {}
func (SetPVID) isOperation()       {}
func (DeleteVLAN) isOperation()    {}
func (SetPort) isOperation()       {}
func (SetSwitchName) isOperation() {}

func (o CreateVLAN) String() string { return fmt.Sprintf("create VLAN %d with %s", o.VID, o.Members) }
func (o SetMembers) String() string { return fmt.Sprintf("VLAN %d: set %s", o.VID, o.Members) }
func (o SetPVID) String() string    { return fmt.Sprintf("port %d: PVID %d", o.Port, o.VID) }
func (o DeleteVLAN) String() string { return fmt.Sprintf("delete VLAN %d", o.VID) }
func (o SetPort) String() string {
	return fmt.Sprintf("port %d: speed %s, name %q", o.Port, o.Speed, o.Name)
}
func (o SetSwitchName) String() string { return fmt.Sprintf("switch name %q", o.Name) }

// Plan is the ordered list of operations plus anything that cannot be done.
type Plan struct {
	Operations []Operation
	Warnings   []string
}

// Describe returns one line per operation.
func (p Plan) Describe() []string {
	out := make([]string, 0, len(p.Operations))
	for _, op := range p.Operations {
		out = append(out, op.String())
	}
	return out
}

// ProtectedVLANs are never deleted: the default and the management VLAN.
func ProtectedVLANs(actual *Config) map[int]bool {
	return map[int]bool{DefaultVLAN: true, actual.ManagementVLAN: true}
}

// SwitchName returns name as the switches accept it: printable ASCII, at
// most SwitchNameMax characters.
func SwitchName(name string) string {
	out := make([]byte, 0, len(name))
	for _, r := range name {
		if r >= ' ' && r <= '~' && len(out) < SwitchNameMax {
			out = append(out, byte(r))
		}
	}
	return string(out)
}

// MakePlan plans the changes that make every port 1..ports of the switch
// match desired.
func MakePlan(desired, actual *Config, ports int, caps Capabilities) Plan {
	var p Plan
	p.planGains(desired, actual, ports, caps)
	p.planPVIDs(desired, actual, ports)
	p.planLosses(desired, actual, ports)
	for port := 1; port <= ports; port++ {
		if op, ok := p.portOp(port, desired, actual, caps); ok {
			p.Operations = append(p.Operations, op)
		}
	}
	if desired.SwitchName.Set {
		name := SwitchName(desired.SwitchName.Value)
		if name != actual.SwitchName.Value {
			p.Operations = append(p.Operations, SetSwitchName{Name: name})
		}
	}
	return p
}

func (p *Plan) planGains(desired, actual *Config, ports int, caps Capabilities) {
	vids := map[int]bool{}
	for vid := range desired.VLANs {
		vids[vid] = true
	}
	for vid := range actual.VLANs {
		vids[vid] = true
	}
	count := len(actual.VLANs)
	for _, vid := range slices.Sorted(maps.Keys(vids)) {
		gains := Members{}
		for port := 1; port <= ports; port++ {
			want := desired.Member(vid, port)
			if want != Excluded && want != actual.Member(vid, port) {
				gains[port] = want
			}
		}
		switch {
		case len(gains) == 0:
		case actual.VLANs[vid] != nil:
			p.Operations = append(p.Operations, SetMembers{VID: vid, Members: gains})
		case count >= caps.MaxVLANs:
			p.Warnings = append(p.Warnings, fmt.Sprintf("VLAN %d not created: switch limit of %d", vid, caps.MaxVLANs))
		default:
			count++
			p.Operations = append(p.Operations, CreateVLAN{VID: vid, Members: gains})
		}
	}
}

func (p *Plan) planPVIDs(desired, actual *Config, ports int) {
	for port := 1; port <= ports; port++ {
		want := pvid(desired, port)
		if want == pvid(actual, port) {
			continue
		}
		if desired.Member(want, port) == Excluded {
			p.Warnings = append(p.Warnings, fmt.Sprintf("port %d: PVID %d skipped, port is not a member", port, want))
			continue
		}
		p.Operations = append(p.Operations, SetPVID{Port: port, VID: want})
	}
}

func (p *Plan) planLosses(desired, actual *Config, ports int) {
	protected := ProtectedVLANs(actual)
	for _, vid := range slices.Sorted(maps.Keys(actual.VLANs)) {
		current := actual.VLANs[vid]
		losses := Members{}
		for port := 1; port <= ports; port++ {
			if _, member := current[port]; member && desired.Member(vid, port) == Excluded {
				losses[port] = Excluded
			}
		}
		remaining := len(current) - len(losses)
		_, wanted := desired.VLANs[vid]
		switch {
		case !protected[vid] && (!wanted || remaining == 0):
			p.Operations = append(p.Operations, DeleteVLAN{VID: vid})
		case len(losses) > 0 && remaining > 0:
			p.Operations = append(p.Operations, SetMembers{VID: vid, Members: losses})
		case len(losses) > 0:
			p.Warnings = append(p.Warnings, fmt.Sprintf("VLAN %d: kept (default/management VLAN) although UniFi wants it without ports", vid))
		}
	}
}

func (p *Plan) portOp(port int, desired, actual *Config, caps Capabilities) (Operation, bool) {
	want, have := desired.Ports[port], actual.Ports[port]
	if want.Speed == "" {
		want.Speed = SpeedAuto
	}
	if have.Speed == "" {
		have.Speed = SpeedAuto
	}
	speed := want.Speed
	switch {
	case speed == Speed1000Full && !caps.Supports(speed) && caps.Supports(SpeedAuto):
		// Neither switch can force 1000 Mbit/s; 1000BASE-T needs
		// autonegotiation anyway, so auto is the closest equivalent.
		speed = SpeedAuto
	case !caps.Supports(speed):
		p.Warnings = append(p.Warnings, fmt.Sprintf("port %d: speed %s not supported by this switch; unchanged", port, speed))
		speed = have.Speed
	}
	name := have.Name.Value
	if want.Name.Set {
		name = want.Name.Value
	}
	if speed == have.Speed && name == have.Name.Value {
		return nil, false
	}
	return SetPort{Port: port, Speed: speed, Name: name}, true
}

func pvid(c *Config, port int) int {
	if v, ok := c.PVIDs[port]; ok {
		return v
	}
	return DefaultVLAN
}
