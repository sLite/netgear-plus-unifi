// Command ngu-emulator presents NETGEAR Plus switches to a UniFi Network
// controller as adopted UniFi switches, using telemetry from the collector.
//
//	ngu-emulator [-config FILE] run          inform the controller (default)
//	ngu-emulator [-config FILE] ids          print virtual MACs per switch
//	ngu-emulator [-config FILE] reset ID...  forget saved adoption state ("all" for every switch)
//	ngu-emulator healthcheck                 exit 0 when the local status endpoint answers
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	emu "github.com/jamesbraid/unifi-emu"

	"netgear-plus-unifi/emulator/internal/config"
	"netgear-plus-unifi/emulator/internal/device"
	"netgear-plus-unifi/emulator/internal/identity"
	"netgear-plus-unifi/emulator/internal/state"
	"netgear-plus-unifi/emulator/internal/telemetry"
)

func main() {
	configPath := flag.String("config", envOr("CONFIG_FILE", "/config/switches.yaml"), "shared switches YAML file")
	statusAddr := flag.String("status-listen", envOr("STATUS_LISTEN", ":8098"), "address of the read-only status endpoint (empty disables)")
	flag.Parse()
	setupLogging(os.Getenv("LOG_LEVEL"))
	if flag.Arg(0) == "healthcheck" {
		os.Exit(healthcheck(*statusAddr))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config", err)
	}
	cmd, args := "run", flag.Args()
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		err = run(cfg, *statusAddr)
	case "ids":
		err = printIDs(cfg)
	case "reset":
		err = reset(cfg, args)
	default:
		err = fmt.Errorf("unknown command %q (want run, ids or reset)", cmd)
	}
	if err != nil {
		fatal(cmd, err)
	}
}

func run(cfg *config.Config, statusAddr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	informURL, err := emu.ResolveInformURL(ctx, cfg.Emulator.InformURL)
	if err != nil {
		return err
	}
	store, err := state.NewStore(cfg.Emulator.StateDir)
	if err != nil {
		return err
	}
	collector := telemetry.NewClient(cfg.Emulator.CollectorURL)
	cache := telemetry.NewCache(collector, cfg.Emulator.TelemetryPoll())
	byID := map[string]*device.Device{}
	opts := device.Options{
		// Devices are only read after the map is complete.
		PeerMAC: func(id string) string {
			if d := byID[id]; d != nil {
				return d.MAC()
			}
			return ""
		},
		Config:        collector,
		InformURL:     informURL,
		ControllerURL: cfg.Emulator.InformURL,
		Interval:      cfg.Emulator.InformInterval(),
		StaleAfter:    cfg.Emulator.StaleAfter(),
		Store:         store,
		Telemetry:     cache,
	}
	var devices []*device.Device
	for _, sw := range cfg.Enabled() {
		d, err := device.New(sw, opts)
		if err != nil {
			return err
		}
		devices = append(devices, d)
		byID[sw.ID] = d
	}
	slog.Info("emulator starting", "inform_url", informURL, "collector", cfg.Emulator.CollectorURL, "devices", len(devices))

	var wg sync.WaitGroup
	wg.Go(func() { cache.Run(ctx) })
	cache.Refresh(ctx) // first telemetry before the first inform
	for _, d := range devices {
		wg.Go(func() { d.Run(ctx) })
	}
	if statusAddr != "" {
		srv := statusServer(statusAddr, devices)
		wg.Go(func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("status endpoint", "err", err)
			}
		})
		wg.Go(func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		})
	}
	wg.Wait()
	slog.Info("emulator stopped")
	return nil
}

// statusServer exposes device phases for operators and health checks. It is
// read-only and reveals no keys.
func statusServer(addr string, devices []*device.Device) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		out := make([]device.Status, 0, len(devices))
		for _, d := range devices {
			out = append(out, d.Status())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": out})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// healthcheck probes the status endpoint; container images have no curl.
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

// printIDs shows each switch's virtual MAC. In physical mode the MAC is the
// switch's own and is shown once it was learned (saved state).
func printIDs(cfg *config.Config) error {
	store, err := state.NewStore(cfg.Emulator.StateDir)
	if err != nil {
		return err
	}
	for _, sw := range cfg.Switches {
		mac := "(switch's own MAC, not learned yet)"
		if identity.IsPhysical(sw.UniFi.MAC) {
			if st, ok, _ := store.Load(sw.ID); ok && st.MACSource == identity.PhysicalMode {
				mac = st.MAC + " (switch's own)"
			}
		} else {
			hw, err := identity.ResolveMAC(sw.ID, sw.UniFi.MAC)
			if err != nil {
				return err
			}
			mac = hw.String()
		}
		fmt.Printf("%-24s %s  %s\n", sw.ID, mac, sw.UniFi.Model)
	}
	return nil
}

// reset deletes saved adoption state. The device returns to pending on the
// next start; remove it in UniFi Network as well for a clean slate.
func reset(cfg *config.Config, ids []string) error {
	store, err := state.NewStore(cfg.Emulator.StateDir)
	if err != nil {
		return err
	}
	if len(ids) == 1 && ids[0] == "all" {
		if ids, err = store.IDs(); err != nil {
			return err
		}
	}
	if len(ids) == 0 {
		return errors.New("reset needs switch ids or \"all\"")
	}
	for _, id := range ids {
		if err := store.Delete(id); err != nil {
			return fmt.Errorf("reset %s: %w", id, err)
		}
		slog.Info("state removed", "switch", id)
	}
	return nil
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil || level == "" {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(what string, err error) {
	slog.Error(what+" failed", "err", err)
	os.Exit(1)
}
