package mock

import (
	"context"
	"reflect"
	"testing"
	"time"

	"netgear-plus-unifi/internal/provision"
)

func TestDeterministicAndCountingUp(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	a, err := New("a", Options{Model: "GS116Ev2", Seed: 7}, clock)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := New("b", Options{Model: "GS116Ev2", Seed: 7}, clock)
	ra, _ := a.Read(context.Background())
	rb, _ := b.Read(context.Background())
	if len(ra.Ports) != 16 || !reflect.DeepEqual(ra.Ports, rb.Ports) {
		t.Fatal("same seed must give the same switch")
	}
	now = now.Add(10 * time.Second)
	later, _ := a.Read(context.Background())
	for i, p := range later.Ports {
		if p.LinkUp && *p.RXBytes <= *ra.Ports[i].RXBytes {
			t.Errorf("port %d counter did not grow", p.Index)
		}
	}
}

func TestFlappingPortAndFailures(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, _ := New("m", Options{Model: "GS308Ev4", FlapPort: 3, FlapPeriod: time.Minute}, func() time.Time { return now })
	first, _ := s.Read(context.Background())
	now = now.Add(30 * time.Second)
	second, _ := s.Read(context.Background())
	if first.Ports[2].LinkUp == second.Ports[2].LinkUp {
		t.Error("flap port did not change state after half a period")
	}
	failing, _ := New("f", Options{Model: "GS308Ev4", FailureRate: 1}, nil)
	if _, err := failing.Read(context.Background()); err == nil {
		t.Error("failure rate 1 must fail")
	}
}

func TestWritesReachTheSimulatedSwitch(t *testing.T) {
	s, _ := New("m", Options{Model: "GS308Ev4", Seed: 1}, nil)
	w, ok := s.Writer()
	if !ok {
		t.Fatal("no writer")
	}
	if err := w.Apply(context.Background(), provision.SetPort{Port: 1, Speed: provision.SpeedDisabled, Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Read(context.Background())
	if r.Ports[0].LinkUp || r.Ports[0].Name != "lab" {
		t.Errorf("disabled port 1 = %+v", r.Ports[0])
	}
}

func TestUnknownModelIsRejected(t *testing.T) {
	if _, err := New("x", Options{Model: "GS105E"}, nil); err == nil {
		t.Error("unsupported mock model accepted")
	}
}
