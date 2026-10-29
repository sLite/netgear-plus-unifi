package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client fetches switch status from the collector.
type Client struct {
	baseURL string
	hc      *http.Client
}

// NewClient returns a client for the collector at baseURL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 10 * time.Second},
	}
}

// FetchAll returns every switch the collector knows about.
func (c *Client) FetchAll(ctx context.Context) ([]SwitchStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/switches", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("collector request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read collector response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("collector answered %s", resp.Status)
	}
	var out struct {
		Switches []SwitchStatus `json:"switches"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode collector response: %w", err)
	}
	return out.Switches, nil
}

// SubmitConfig hands UniFi's switch configuration for one switch to the
// collector, which applies it to the switch if writes are enabled.
func (c *Client) SubmitConfig(ctx context.Context, switchID, cfgversion, switchConfig string) error {
	body, err := json.Marshal(map[string]string{"cfgversion": cfgversion, "system_cfg": switchConfig})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/switches/"+switchID+"/config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("submit config: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("submit config: collector answered %s", resp.Status)
	}
	return nil
}

// Fetcher is the part of Client the cache needs; tests substitute it.
type Fetcher interface {
	FetchAll(ctx context.Context) ([]SwitchStatus, error)
}

// Cache polls the collector and keeps the latest observation per switch id.
type Cache struct {
	fetcher  Fetcher
	interval time.Duration
	now      func() time.Time

	mu        sync.RWMutex
	latest    map[string]Observation
	reachable bool
	succeeded bool // at least one successful poll since start
}

// NewCache builds a cache that polls fetcher every interval.
func NewCache(fetcher Fetcher, interval time.Duration) *Cache {
	return &Cache{fetcher: fetcher, interval: interval, now: time.Now, latest: map[string]Observation{}, reachable: true}
}

// Run polls until ctx is done.
func (c *Cache) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		c.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh performs one poll. Collector reachability transitions are logged;
// individual failures while already unreachable are not.
func (c *Cache) Refresh(ctx context.Context) {
	statuses, err := c.fetcher.FetchAll(ctx)
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		switch {
		case ctx.Err() != nil || !c.reachable:
		case !c.succeeded:
			slog.Info("collector not reachable yet; retrying", "err", err)
		default:
			slog.Warn("collector unreachable; switches will go offline once telemetry is stale", "err", err)
		}
		c.reachable = false
		return
	}
	if !c.reachable {
		slog.Info("collector reachable")
	}
	c.reachable, c.succeeded = true, true
	for _, s := range statuses {
		c.latest[s.ID] = Observation{Status: s, ReceivedAt: now}
	}
}

// Get returns the latest observation for a switch id.
func (c *Cache) Get(id string) (Observation, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, ok := c.latest[id]
	return o, ok
}
