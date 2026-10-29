// Command netgear-unifi presents NETGEAR Plus switches to a UniFi Network
// controller as adopted UniFi switches, with live port data from the
// switches and (opt-in) UniFi port configuration applied to them.
//
//	netgear-unifi [-config FILE] run          poll the switches and inform the controller (default)
//	netgear-unifi [-config FILE] ids          print the UniFi device MAC per switch
//	netgear-unifi [-config FILE] reset ID...  forget saved adoption state ("all" for every switch)
//	netgear-unifi healthcheck                 exit 0 when the local status endpoint answers
package main

import (
	"context"
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

	"netgear-plus-unifi/internal/config"
	"netgear-plus-unifi/internal/device"
	"netgear-plus-unifi/internal/identity"
	"netgear-plus-unifi/internal/mock"
	"netgear-plus-unifi/internal/netgear"
	"netgear-plus-unifi/internal/poller"
	"netgear-plus-unifi/internal/provision"
	"netgear-plus-unifi/internal/state"
)

func main() {
	configPath := flag.String("config", envOr("CONFIG_FILE", "/config/switches.yaml"), "switches YAML file")
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

// managedSwitch is one physical switch with its poller and UniFi device.
type managedSwitch struct {
	id     string
	poller *poller.Poller
	device *device.Device
}

func run(cfg *config.Config, statusAddr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	informURL, err := emu.ResolveInformURL(ctx, cfg.Controller.InformURL)
	if err != nil {
		return err
	}
	store, err := state.NewStore(cfg.StateDir)
	if err != nil {
		return err
	}
	switches, err := buildSwitches(cfg, store, informURL)
	if err != nil {
		return err
	}
	slog.Info("starting", "inform_url", informURL, "switches", len(switches))

	var wg sync.WaitGroup
	for _, s := range switches {
		wg.Go(func() { s.poller.Run(ctx) })
		wg.Go(func() { s.device.Run(ctx) })
	}
	if statusAddr != "" {
		srv := statusServer(statusAddr, switches)
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
	wg.Wait() // pollers log out of their switches before returning
	slog.Info("stopped")
	return nil
}

func buildSwitches(cfg *config.Config, store *state.Store, informURL string) ([]*managedSwitch, error) {
	byID := map[string]*managedSwitch{}
	opts := device.Options{
		// Devices are only asked after the map is complete.
		PeerMAC: func(id string) string {
			if s := byID[id]; s != nil {
				return s.device.MAC()
			}
			return ""
		},
		InformURL:     informURL,
		ControllerURL: cfg.Controller.InformURL,
		Interval:      cfg.InformInterval(),
		StaleAfter:    cfg.StaleAfter(),
		Store:         store,
	}
	var out []*managedSwitch
	for _, sw := range cfg.Enabled() {
		src, err := buildSource(sw)
		if err != nil {
			return nil, fmt.Errorf("switch %q: %w", sw.ID, err)
		}
		policy := sw.Source.Policy()
		if policy.Enabled {
			mode := "live"
			if policy.DryRun {
				mode = "dry run"
			}
			slog.Warn("configuration writes ENABLED, UniFi manages all ports", "switch", sw.ID, "mode", mode)
		}
		p := poller.New(sw.ID, src, provision.NewSync(sw.ID, policy, nil), poller.Options{
			Interval: sw.Source.PollInterval(), SessionYield: sw.Source.SessionYield(), StaleAfter: cfg.StaleAfter(),
		})
		d, err := device.New(sw, p, opts)
		if err != nil {
			return nil, err
		}
		s := &managedSwitch{id: sw.ID, poller: p, device: d}
		byID[sw.ID] = s
		out = append(out, s)
	}
	return out, nil
}

func buildSource(sw config.Switch) (poller.Source, error) {
	src := sw.Source
	if src.Adapter == config.AdapterMock {
		return mock.New(sw.ID, mock.Options{
			Model: src.Mock.Model, Seed: src.Mock.Seed, FailureRate: src.Mock.FailureRate,
			FlapPort: src.Mock.FlapPort, FlapPeriod: time.Duration(src.Mock.FlapPeriodSeconds * float64(time.Second)),
		}, nil)
	}
	password, err := src.Password(os.Getenv)
	if err != nil {
		return nil, err
	}
	return netgear.NewSwitch(sw.ID, src.Host, password, src.ExpectedModel), nil
}

// printIDs shows each switch's UniFi device MAC. In physical mode the MAC
// is the switch's own and is shown once it was learned (saved state).
func printIDs(cfg *config.Config) error {
	store, err := state.NewStore(cfg.StateDir)
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
	store, err := state.NewStore(cfg.StateDir)
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
