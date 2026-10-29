package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"netgear-plus-unifi/internal/device"
	"netgear-plus-unifi/internal/poller"
	"netgear-plus-unifi/internal/provision"
)

// switchStatus is one switch on the status endpoint: the UniFi device, the
// physical switch's telemetry and the configuration sync.
type switchStatus struct {
	ID         string           `json:"id"`
	Device     device.Status    `json:"device"`
	Source     poller.Status    `json:"source"`
	ConfigSync provision.Status `json:"config_sync"`
}

// statusServer exposes the state of every switch for operators and health
// checks. It is read-only and reveals no keys or passwords.
func statusServer(addr string, switches []*managedSwitch) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		out := make([]switchStatus, 0, len(switches))
		for _, s := range switches {
			out = append(out, switchStatus{
				ID: s.id, Device: s.device.Status(), Source: s.poller.Status(), ConfigSync: s.poller.ConfigStatus(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"switches": out})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// healthcheck probes the status endpoint; the container image has no curl.
func healthcheck(addr string) int {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
