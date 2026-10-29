package provision_test

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"netgear-plus-unifi/internal/mock"
	. "netgear-plus-unifi/internal/provision"
)

func memWriter(b Backend) *VerifiedWriter {
	return NewVerifiedWriter(b, 8, caps, slog.Default())
}

func TestGuardRejectsUnsafeOperations(t *testing.T) {
	b := &mock.Backend{Config: FactoryDefault(8)}
	w := memWriter(b)
	ctx := t.Context()
	for _, c := range []struct {
		op   Operation
		want string
	}{
		{SetPort{Port: 9, Speed: SpeedDisabled}, "does not exist"},
		{DeleteVLAN{1}, "VLAN 1"},
		{CreateVLAN{4095, Members{4: T}}, "out of range"},
		{SetSwitchName{strings.Repeat("x", 21)}, "not valid"},
		{SetMembers{1, Members{0: E}}, "does not exist"},
		{SetPVID{Port: 0, VID: 1}, "does not exist"},
	} {
		if err := w.Apply(ctx, c.op); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: error %v, want forbidden mentioning %q", c.op, err, c.want)
		}
	}
	if err := w.Apply(ctx, CreateVLAN{10, Members{8: T}}); err != nil {
		t.Fatal(err)
	}
	b.Config.ManagementVLAN = 10
	if err := w.Apply(ctx, DeleteVLAN{10}); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "management") {
		t.Errorf("management VLAN deletion: %v", err)
	}
	if !maps.Equal(b.Config.VLANs[1], all(8, U)) {
		t.Errorf("VLAN 1 changed: %v", b.Config.VLANs[1])
	}
}

func TestMembershipChangeIsMergedIntoFreshState(t *testing.T) {
	b := &mock.Backend{Config: FactoryDefault(8)}
	if err := memWriter(b).Apply(t.Context(), SetMembers{1, Members{4: E}}); err != nil {
		t.Fatal(err)
	}
	want := all(8, U)
	delete(want, 4)
	if !maps.Equal(b.Config.VLANs[1], want) {
		t.Errorf("VLAN 1 = %v", b.Config.VLANs[1])
	}
}

func TestEmptyingAVLANIsRefused(t *testing.T) {
	w := memWriter(&mock.Backend{Config: FactoryDefault(8)})
	if err := w.Apply(t.Context(), CreateVLAN{3901, Members{4: T}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(t.Context(), SetMembers{3901, Members{4: E}}); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "no members") {
		t.Errorf("got %v", err)
	}
}

// clumsyBackend simulates a firmware or encoding bug: its first membership
// write also drops port 1.
type clumsyBackend struct {
	*mock.Backend
	broken bool
}

func (c *clumsyBackend) WriteMembers(ctx context.Context, vid int, members Members) error {
	if c.broken {
		c.broken = false
		members = maps.Clone(members)
		delete(members, 1)
	}
	return c.Backend.WriteMembers(ctx, vid, members)
}

func TestSideEffectsOnOtherPortsAreRestored(t *testing.T) {
	b := &clumsyBackend{Backend: &mock.Backend{Config: FactoryDefault(8)}, broken: true}
	err := memWriter(b).Apply(t.Context(), SetMembers{1, Members{4: E}})
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("got %v, want restored side effect", err)
	}
	// Port 1 is back; the intended change (port 4 removed) stays.
	want := all(8, U)
	delete(want, 4)
	if !maps.Equal(b.Config.VLANs[1], want) {
		t.Errorf("VLAN 1 = %v", b.Config.VLANs[1])
	}
}

// pvidLeak changes another port's PVID along with the requested one.
type pvidLeak struct{ *mock.Backend }

func (p pvidLeak) SetPVID(ctx context.Context, port, vid int) error {
	p.Config.PVIDs[1] = vid
	return p.Backend.SetPVID(ctx, port, vid)
}

func TestChangedPVIDOfAnotherPortFails(t *testing.T) {
	b := pvidLeak{&mock.Backend{Config: FactoryDefault(8)}}
	b.Config.VLANs[3901] = all(8, T)
	if err := memWriter(b).Apply(t.Context(), SetPVID{4, 3901}); err == nil || !strings.Contains(err.Error(), "port 1") {
		t.Errorf("got %v", err)
	}
}

// ignoring accepts every write and changes nothing.
type ignoring struct{ *mock.Backend }

func (ignoring) SetPort(context.Context, SetPort) error { return nil }

func TestUnappliedWriteIsReported(t *testing.T) {
	w := memWriter(ignoring{&mock.Backend{Config: FactoryDefault(8)}})
	if err := w.Apply(t.Context(), SetPort{4, Speed100Full, "lab"}); err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Errorf("got %v", err)
	}
}

func TestSwitchNameIsVerified(t *testing.T) {
	b := &mock.Backend{Config: FactoryDefault(8)}
	if err := memWriter(b).Apply(t.Context(), SetSwitchName{"SwitchKeller"}); err != nil {
		t.Fatal(err)
	}
	if b.Config.SwitchName.Value != "SwitchKeller" {
		t.Errorf("name = %q", b.Config.SwitchName.Value)
	}
}
