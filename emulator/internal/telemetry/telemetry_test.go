package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func age(v float64) *float64 { return &v }

func TestFreshSnapshot(t *testing.T) {
	now := time.Unix(1000, 0)
	snap := &Snapshot{}
	cases := []struct {
		name string
		obs  Observation
		want bool
	}{
		{"fresh", Observation{SwitchStatus{State: StateOK, AgeSeconds: age(5), Snapshot: snap}, now}, true},
		{"aged in emulator", Observation{SwitchStatus{State: StateOK, AgeSeconds: age(5), Snapshot: snap}, now.Add(-86 * time.Second)}, false},
		{"collector says unavailable", Observation{SwitchStatus{State: StateUnavailable, AgeSeconds: age(1), Snapshot: snap}, now}, false},
		{"no snapshot", Observation{SwitchStatus{State: StateStarting}, now}, false},
	}
	for _, c := range cases {
		if _, ok := c.obs.FreshSnapshot(now, 90*time.Second); ok != c.want {
			t.Errorf("%s: fresh=%v, want %v", c.name, ok, c.want)
		}
	}
}

func TestClientAndCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/switches" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"switches": []map[string]any{{
			"id": "sw", "state": "ok", "age_seconds": 1.5,
			"snapshot": map[string]any{"device": map[string]any{"model": "GS116Ev2", "port_count": 16},
				"ports": []map[string]any{{"index": 1, "link_up": true, "speed_mbps": 1000, "rx_bytes": 24774038874, "full_duplex": nil}}},
		}}})
	}))
	defer srv.Close()

	cache := NewCache(NewClient(srv.URL+"/"), time.Second)
	cache.Refresh(context.Background())
	obs, ok := cache.Get("sw")
	if !ok || obs.Status.Snapshot == nil {
		t.Fatal("snapshot not cached")
	}
	p := obs.Status.Snapshot.Ports[0]
	if !p.LinkUp || *p.SpeedMbps != 1000 || *p.RXBytes != 24774038874 || p.FullDuplex != nil {
		t.Errorf("port decoded wrong: %+v", p)
	}

	failing := NewCache(fetchFunc(func(context.Context) ([]SwitchStatus, error) { return nil, errors.New("down") }), time.Second)
	failing.Refresh(context.Background())
	if _, ok := failing.Get("sw"); ok {
		t.Error("failed refresh must not invent observations")
	}
}

type fetchFunc func(context.Context) ([]SwitchStatus, error)

func (f fetchFunc) FetchAll(ctx context.Context) ([]SwitchStatus, error) { return f(ctx) }
