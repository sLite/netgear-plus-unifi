// Package config loads config/switches.yaml. Each switch has a source
// section (the physical NETGEAR) and a unifi section (the virtual UniFi
// device). Passwords never go in the file: source.password_env names the
// environment variable that holds it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"netgear-plus-unifi/internal/mock"
	"netgear-plus-unifi/internal/netgear"
	"netgear-plus-unifi/internal/provision"
)

// Defaults applied when the file leaves a value unset.
const (
	DefaultInformInterval = 10 * time.Second
	DefaultPollInterval   = 15 * time.Second
	DefaultStaleAfter     = 90 * time.Second
	DefaultSessionYield   = 5 * time.Minute
	DefaultStateDir       = "/data"
)

// Source adapters.
const (
	AdapterNetgear = "netgear_plus"
	AdapterMock    = "mock"
)

// Config is the whole file.
type Config struct {
	Controller Controller `yaml:"controller"`
	// StateDir holds persisted adoption state (contains auth keys).
	StateDir string `yaml:"state_dir"`
	// PollIntervalSeconds is the default per-switch polling interval.
	PollIntervalSeconds float64 `yaml:"poll_interval_seconds"`
	// StaleAfterSeconds is the telemetry age after which a virtual switch
	// stops informing, so UniFi shows it offline instead of outdated ports.
	StaleAfterSeconds float64  `yaml:"stale_after_seconds"`
	Switches          []Switch `yaml:"switches"`
}

// Controller is the UniFi Network controller the virtual switches inform.
type Controller struct {
	// InformURL is the inform endpoint, e.g. http://192.168.1.1:8080/inform.
	InformURL string `yaml:"inform_url"`
	// InformIntervalSeconds is the initial inform interval; the controller
	// may retune it.
	InformIntervalSeconds float64 `yaml:"inform_interval_seconds"`
}

// Switch is one physical switch mapped to one virtual UniFi device.
type Switch struct {
	// ID is stable; renaming it creates a new virtual device.
	ID       string  `yaml:"id"`
	Disabled bool    `yaml:"disabled"`
	Source   Source  `yaml:"source"`
	UniFi    Virtual `yaml:"unifi"`
}

// Source is the physical switch.
type Source struct {
	Adapter     string `yaml:"adapter"` // netgear_plus (default) or mock
	Host        string `yaml:"host"`
	PasswordEnv string `yaml:"password_env"`
	// ExpectedModel refuses data if another model answers at Host.
	ExpectedModel       string  `yaml:"expected_model"`
	PollIntervalSeconds float64 `yaml:"poll_interval_seconds"`
	// SessionYieldSeconds is the pause after the switch dropped our session
	// because someone logged into its web UI.
	SessionYieldSeconds float64 `yaml:"session_yield_seconds"`
	Write               Write   `yaml:"write"`
	Mock                Mock    `yaml:"mock"`
}

// Write lets UniFi manage the switch's ports. Off unless enabled; dry run
// unless explicitly disabled.
type Write struct {
	Enabled         bool    `yaml:"enabled"`
	DryRun          *bool   `yaml:"dry_run"`
	DebounceSeconds float64 `yaml:"debounce_seconds"`
}

// Mock configures a simulated switch.
type Mock struct {
	Model             string  `yaml:"model"`
	Seed              uint64  `yaml:"seed"`
	FailureRate       float64 `yaml:"failure_rate"`
	FlapPort          int     `yaml:"flap_port"`
	FlapPeriodSeconds float64 `yaml:"flap_period_seconds"`
}

// Virtual describes the emulated UniFi identity of a switch.
type Virtual struct {
	// Model is the UniFi model code reported to the controller (e.g. USL8LP).
	Model string `yaml:"model"`
	// MAC is the virtual MAC: "physical" uses the NETGEAR's own MAC, "auto"
	// or empty derives a stable locally administered address from the
	// switch id, anything else must be a locally administered MAC.
	MAC string `yaml:"mac"`
	// Name is the hostname reported before the device is named in UniFi.
	Name string `yaml:"name"`
	// IP is "physical" (report the NETGEAR management IP, default) or an
	// IPv4 literal.
	IP string `yaml:"ip"`
	// Downlinks optionally declare which other configured switch hangs off
	// which port, so UniFi can draw the link between two NETGEARs.
	Downlinks []Downlink `yaml:"downlinks"`
}

// Downlink is operator-declared topology: Switch (another configured
// switch id) is connected to Port.
type Downlink struct {
	Port   int    `yaml:"port"`
	Switch string `yaml:"switch"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads path and applies environment overrides and defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, os.Getenv)
}

// Parse decodes raw YAML. getenv supplies the overrides UNIFI_INFORM_URL
// and STATE_DIR, so containers can be pointed elsewhere without editing the
// file. Unknown keys are errors.
func Parse(raw []byte, getenv func(string) string) (*Config, error) {
	if err := rejectLegacyLayout(raw); err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if v := getenv("UNIFI_INFORM_URL"); v != "" {
		cfg.Controller.InformURL = v
	}
	if v := getenv("STATE_DIR"); v != "" {
		cfg.StateDir = v
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// rejectLegacyLayout explains the move from the two-service layout.
func rejectLegacyLayout(raw []byte) error {
	var top map[string]any
	if yaml.Unmarshal(raw, &top) != nil {
		return nil // reported by the strict decoder
	}
	if _, ok := top["emulator"]; ok {
		return errors.New("config: the emulator and collector sections were merged; move emulator.inform_url " +
			"to controller.inform_url and remove collector/emulator (see config/switches.example.yaml)")
	}
	if _, ok := top["collector"]; ok {
		return errors.New("config: the collector section was removed; poll_interval_seconds, stale_after_seconds " +
			"and state_dir are top-level now (see config/switches.example.yaml)")
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	setDefault(&c.Controller.InformIntervalSeconds, DefaultInformInterval)
	setDefault(&c.PollIntervalSeconds, DefaultPollInterval)
	setDefault(&c.StaleAfterSeconds, DefaultStaleAfter)
	for i := range c.Switches {
		s := &c.Switches[i].Source
		if s.Adapter == "" {
			s.Adapter = AdapterNetgear
		}
		if s.PollIntervalSeconds == 0 {
			s.PollIntervalSeconds = c.PollIntervalSeconds
		}
		setDefault(&s.SessionYieldSeconds, DefaultSessionYield)
		setDefault(&s.Write.DebounceSeconds, 10*time.Second)
		setDefault(&s.Mock.FlapPeriodSeconds, time.Minute)
		if s.Mock.Model == "" {
			s.Mock.Model = "GS308Ev4"
		}
	}
}

func setDefault(v *float64, d time.Duration) {
	if *v == 0 {
		*v = d.Seconds()
	}
}

func (c *Config) validate() error {
	var errs []error
	if err := validateHTTPURL("controller.inform_url", c.Controller.InformURL); err != nil {
		errs = append(errs, err)
	}
	if c.Controller.InformIntervalSeconds <= 0 || c.PollIntervalSeconds <= 0 || c.StaleAfterSeconds <= 0 {
		errs = append(errs, errors.New("intervals must be positive"))
	}
	seen := map[string]bool{}
	for i, s := range c.Switches {
		if !idPattern.MatchString(s.ID) {
			errs = append(errs, fmt.Errorf("switches[%d].id %q: want lowercase letters, digits and dashes", i, s.ID))
		}
		if seen[s.ID] {
			errs = append(errs, fmt.Errorf("switches[%d].id %q is duplicated", i, s.ID))
		}
		seen[s.ID] = true
		errs = append(errs, s.validate()...)
	}
	for _, s := range c.Switches {
		for _, dl := range s.UniFi.Downlinks {
			if dl.Port < 1 || dl.Switch == s.ID || !seen[dl.Switch] {
				errs = append(errs, fmt.Errorf("switch %q: downlink %+v needs port >= 1 and another configured switch id", s.ID, dl))
			}
		}
	}
	if len(c.Enabled()) == 0 {
		errs = append(errs, errors.New("no enabled switches configured"))
	}
	return errors.Join(errs...)
}

func (s Switch) validate() []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("switch %q: "+format, append([]any{s.ID}, args...)...))
	}
	src := s.Source
	switch src.Adapter {
	case AdapterNetgear:
		if src.Host == "" || src.PasswordEnv == "" {
			fail("source.host and source.password_env are required")
		}
		if src.ExpectedModel != "" && !slices.Contains(netgear.SupportedModels(), src.ExpectedModel) {
			fail("source.expected_model %q is not supported (one of %v)", src.ExpectedModel, netgear.SupportedModels())
		}
	case AdapterMock:
		if _, ok := mock.Models[src.Mock.Model]; !ok {
			fail("source.mock.model %q must be one of %v", src.Mock.Model, slices.Sorted(maps.Keys(mock.Models)))
		}
	default:
		fail("source.adapter %q must be %s or %s", src.Adapter, AdapterNetgear, AdapterMock)
	}
	if src.PollIntervalSeconds <= 0 || src.SessionYieldSeconds <= 0 || src.Write.DebounceSeconds <= 0 {
		fail("source intervals must be positive")
	}
	if s.UniFi.Model == "" {
		fail("unifi.model is required")
	}
	if ip := s.UniFi.IP; ip != "" && ip != "physical" && net.ParseIP(ip).To4() == nil {
		fail("unifi.ip %q must be \"physical\" or an IPv4 address", ip)
	}
	return errs
}

func validateHTTPURL(field, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q is not an http(s) URL", field, raw)
	}
	return nil
}

// Enabled returns the switches that are not disabled.
func (c *Config) Enabled() []Switch {
	out := make([]Switch, 0, len(c.Switches))
	for _, s := range c.Switches {
		if !s.Disabled {
			out = append(out, s)
		}
	}
	return out
}

// InformInterval returns the configured initial inform interval.
func (c *Config) InformInterval() time.Duration { return seconds(c.Controller.InformIntervalSeconds) }

// StaleAfter returns the telemetry age limit.
func (c *Config) StaleAfter() time.Duration { return seconds(c.StaleAfterSeconds) }

// PollInterval returns the switch's polling interval.
func (s Source) PollInterval() time.Duration { return seconds(s.PollIntervalSeconds) }

// SessionYield returns the pause after a displaced session.
func (s Source) SessionYield() time.Duration { return seconds(s.SessionYieldSeconds) }

// Password reads the switch password from the environment.
func (s Source) Password(getenv func(string) string) (string, error) {
	if v := getenv(s.PasswordEnv); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %s is not set", s.PasswordEnv)
}

// Policy returns the write policy for the switch.
func (s Source) Policy() provision.Policy {
	p := provision.DefaultPolicy()
	p.Enabled = s.Write.Enabled
	p.DryRun = s.Write.DryRun == nil || *s.Write.DryRun
	p.Debounce = seconds(s.Write.DebounceSeconds)
	return p
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }
