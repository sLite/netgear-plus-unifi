package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
)

// MaxVID is the highest VLAN id both NETGEAR UIs accept for user VLANs.
const MaxVID = 4093

// ErrForbidden marks an operation that would touch something outside the
// write policy. It is never retried into success.
var ErrForbidden = errors.New("forbidden")

// Backend is the model-specific part of a writer. Implementations may keep
// form state (session hashes, page tokens) from the last ReadConfig; the
// verified writer always reads before it writes.
type Backend interface {
	ReadConfig(ctx context.Context) (*Config, error)
	CreateVLAN(ctx context.Context, vid int, members Members) error
	// WriteMembers writes the complete membership of vid (all ports).
	WriteMembers(ctx context.Context, vid int, members Members) error
	SetPVID(ctx context.Context, port, vid int) error
	DeleteVLAN(ctx context.Context, vid int) error
	SetPort(ctx context.Context, op SetPort) error
	SetSwitchName(ctx context.Context, name string) error
}

// VerifiedWriter wraps a Backend with the safety logic shared by every
// model: every operation re-reads the switch, is checked against the write
// guards, merges membership changes into that fresh state, and is verified
// by reading back. Anything the operation did not name that changed is
// restored immediately.
type VerifiedWriter struct {
	backend Backend
	ports   int
	caps    Capabilities
	log     *slog.Logger
}

// NewVerifiedWriter returns a writer for a switch with ports ports.
func NewVerifiedWriter(b Backend, ports int, caps Capabilities, log *slog.Logger) *VerifiedWriter {
	return &VerifiedWriter{backend: b, ports: ports, caps: caps, log: log}
}

func (w *VerifiedWriter) Capabilities() Capabilities { return w.caps }
func (w *VerifiedWriter) PortCount() int             { return w.ports }

// ReadConfig reads the switch's current configuration.
func (w *VerifiedWriter) ReadConfig(ctx context.Context) (*Config, error) {
	return w.backend.ReadConfig(ctx)
}

// Apply performs one operation and verifies it.
func (w *VerifiedWriter) Apply(ctx context.Context, op Operation) error {
	before, err := w.backend.ReadConfig(ctx)
	if err != nil {
		return err
	}
	if err := w.guard(op, before); err != nil {
		return err
	}
	w.log.Info("write", "op", op.String())
	if err := w.dispatch(ctx, op, before); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return w.verify(ctx, op, before)
}

func (w *VerifiedWriter) dispatch(ctx context.Context, op Operation, before *Config) error {
	switch op := op.(type) {
	case CreateVLAN:
		if err := w.backend.CreateVLAN(ctx, op.VID, op.Members); err != nil {
			return err
		}
		fresh, err := w.backend.ReadConfig(ctx)
		if err != nil {
			return err
		}
		return w.setMembers(ctx, op.VID, op.Members, fresh)
	case SetMembers:
		return w.setMembers(ctx, op.VID, op.Members, before)
	case SetPVID:
		return w.backend.SetPVID(ctx, op.Port, op.VID)
	case DeleteVLAN:
		return w.backend.DeleteVLAN(ctx, op.VID)
	case SetPort:
		return w.backend.SetPort(ctx, op)
	case SetSwitchName:
		return w.backend.SetSwitchName(ctx, op.Name)
	}
	return fmt.Errorf("%w: unknown operation %T", ErrForbidden, op)
}

func (w *VerifiedWriter) guard(op Operation, before *Config) error {
	var ports []int
	switch op := op.(type) {
	case CreateVLAN, SetMembers:
		vid, members := vlanOp(op)
		if vid < 1 || vid > MaxVID {
			return fmt.Errorf("%w: VLAN id %d out of range", ErrForbidden, vid)
		}
		for p := range members {
			ports = append(ports, p)
		}
	case SetPVID:
		ports = []int{op.Port}
	case SetPort:
		ports = []int{op.Port}
	case DeleteVLAN:
		if ProtectedVLANs(before)[op.VID] {
			return fmt.Errorf("%w: VLAN %d (default/management) is never deleted", ErrForbidden, op.VID)
		}
	case SetSwitchName:
		if op.Name != SwitchName(op.Name) {
			return fmt.Errorf("%w: switch name %q is not valid for the switch", ErrForbidden, op.Name)
		}
	}
	for _, p := range ports {
		if p < 1 || p > w.ports {
			return fmt.Errorf("%w: port %d does not exist on this switch", ErrForbidden, p)
		}
	}
	return nil
}

func (w *VerifiedWriter) setMembers(ctx context.Context, vid int, changes Members, before *Config) error {
	current := before.VLANs[vid]
	merged := merge(current, changes)
	if maps.Equal(merged, current) {
		return nil
	}
	if len(merged) == 0 {
		return fmt.Errorf("%w: VLAN %d would have no members", ErrForbidden, vid)
	}
	return w.backend.WriteMembers(ctx, vid, merged)
}

func (w *VerifiedWriter) verify(ctx context.Context, op Operation, before *Config) error {
	after, err := w.backend.ReadConfig(ctx)
	if err != nil {
		return err
	}
	if err := w.checkSideEffects(ctx, op, before, after); err != nil {
		return err
	}
	if problem := verifyOp(op, after); problem != "" {
		return fmt.Errorf("%s: not applied (%s)", op, problem)
	}
	return nil
}

// checkSideEffects is the last line of defence: nothing the operation did
// not name may change. A VLAN whose membership differs from what the
// operation should have produced is written back immediately.
func (w *VerifiedWriter) checkSideEffects(ctx context.Context, op Operation, before, after *Config) error {
	for vid, expected := range expectedVLANs(op, before) {
		actual, ok := after.VLANs[vid]
		if !ok || maps.Equal(actual, expected) {
			continue
		}
		w.log.Error("unexpected VLAN membership after write; restoring",
			"vlan", vid, "actual", actual.String(), "expected", expected.String())
		if len(expected) > 0 {
			if err := w.backend.WriteMembers(ctx, vid, expected); err != nil {
				return fmt.Errorf("VLAN %d: unexpected change on other ports, restore failed: %w", vid, err)
			}
		}
		return fmt.Errorf("VLAN %d: unexpected change on other ports, restored", vid)
	}
	changed := 0
	if op, ok := op.(SetPVID); ok {
		changed = op.Port
	}
	for port, vid := range before.PVIDs {
		if port != changed && after.PVIDs[port] != vid {
			return fmt.Errorf("PVID of port %d changed although the operation did not name it", port)
		}
	}
	return nil
}

// expectedVLANs is every VLAN's membership as op should leave it.
func expectedVLANs(op Operation, before *Config) map[int]Members {
	expected := make(map[int]Members, len(before.VLANs))
	for vid, m := range before.VLANs {
		expected[vid] = maps.Clone(m)
	}
	switch op := op.(type) {
	case CreateVLAN, SetMembers:
		vid, members := vlanOp(op)
		expected[vid] = merge(expected[vid], members)
	case DeleteVLAN:
		delete(expected, op.VID)
	}
	return expected
}

func verifyOp(op Operation, after *Config) string {
	switch op := op.(type) {
	case CreateVLAN, SetMembers:
		vid, members := vlanOp(op)
		for port, m := range members {
			if got := after.Member(vid, port); got != m {
				return fmt.Sprintf("port %d is %s", port, got)
			}
		}
	case SetPVID:
		if got := after.PVIDs[op.Port]; got != op.VID {
			return fmt.Sprintf("PVID is %d", got)
		}
	case DeleteVLAN:
		if _, ok := after.VLANs[op.VID]; ok {
			return "VLAN still exists"
		}
	case SetSwitchName:
		if after.SwitchName.Value != op.Name {
			return fmt.Sprintf("name is %q", after.SwitchName.Value)
		}
	case SetPort:
		have := after.Ports[op.Port]
		if have.Speed != op.Speed {
			return fmt.Sprintf("speed is %s", have.Speed)
		}
		if have.Name.Value != op.Name {
			return fmt.Sprintf("name is %q", have.Name.Value)
		}
	}
	return ""
}

func vlanOp(op Operation) (int, Members) {
	switch op := op.(type) {
	case CreateVLAN:
		return op.VID, op.Members
	case SetMembers:
		return op.VID, op.Members
	}
	return 0, nil
}

func merge(current, changes Members) Members {
	merged := maps.Clone(current)
	if merged == nil {
		merged = Members{}
	}
	for port, m := range changes {
		if m == Excluded {
			delete(merged, port)
		} else {
			merged[port] = m
		}
	}
	return merged
}
