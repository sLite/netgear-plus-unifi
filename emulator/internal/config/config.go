// Package config loads the shared switches YAML file. The emulator reads the
// top-level `emulator` section and each switch's `id` and `unifi` (virtual
// identity) sections; the `source` section belongs to the collector and is
// ignored here, keeping physical and virtual identities separate.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied when the file leaves a value unset.
const (
	DefaultInformInterval = 10 * time.Second
	DefaultStaleAfter     = 90 * time.Second
	DefaultPollInterval   = 5 * time.Second
	DefaultStateDir       = "/data"
)

// Config is the emulator's view of the shared configuration file.
type Config struct {
	Emulator Emulator `yaml:"emulator"`
	Switches []Switch `yaml:"switches"`
}

// Emulator holds process-wide settings.
type Emulator struct {
	// InformURL is the controller inform endpoint, e.g.
	// http://192.168.1.1:8080/inform. The host must be an IP literal or
	// resolvable name (resolved once at startup).
	InformURL string `yaml:"inform_url"`
	// CollectorURL is the base URL of the NETGEAR telemetry collector.
	CollectorURL string `yaml:"collector_url"`
	// StateDir holds persisted adoption state (contains auth keys).
	StateDir string `yaml:"state_dir"`
	// InformIntervalSeconds is the initial inform interval; the controller
	// may retune it.
	InformIntervalSeconds int `yaml:"inform_interval_seconds"`
	// TelemetryPollSeconds is how often the collector is queried.
	TelemetryPollSeconds int `yaml:"telemetry_poll_seconds"`
	// StaleAfterSeconds is the telemetry age after which the emulator stops
	// informing for that switch, so UniFi shows it offline instead of
	// displaying outdated port state.
	StaleAfterSeconds int `yaml:"stale_after_seconds"`
}

// Switch is one physical switch mapped to one virtual UniFi device.
type Switch struct {
	ID string `yaml:"id"`
	// Disabled skips this switch in both the collector and the emulator.
	Disabled bool    `yaml:"disabled"`
	UniFi    Virtual `yaml:"unifi"`
}

// Virtual describes the emulated UniFi identity of a switch.
type Virtual struct {
	// Model is the UniFi model code reported to the controller (e.g. US8).
	Model string `yaml:"model"`
	// MAC is the virtual MAC: "auto" / empty derives a stable locally
	// administered address from the switch id, "physical" uses the
	// NETGEAR's own MAC (learned from the collector), anything else must be
	// an explicit locally administered MAC.
	MAC string `yaml:"mac"`
	// Name is the hostname reported before the device is renamed in UniFi.
	Name string `yaml:"name"`
	// IP is "physical" (report the NETGEAR management IP, default) or an
	// IPv4 literal.
	IP string `yaml:"ip"`
	// Downlinks optionally declare which other emulated switches hang off
	// which port, so UniFi can draw the link between two NETGEARs.
	Downlinks []Downlink `yaml:"downlinks"`
}

// Downlink is operator-declared topology: switch Switch (another configured
// switch id) is connected to Port. Only virtual switch MACs can be declared;
// client devices are never reported.
type Downlink struct {
	Port   int    `yaml:"port"`
	Switch string `yaml:"switch"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads path, applies environment overrides and defaults, and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, os.Getenv)
}

// Parse decodes raw YAML. getenv supplies the environment overrides
// UNIFI_INFORM_URL, COLLECTOR_URL and STATE_DIR so containers can be pointed
// elsewhere without editing the shared file.
func Parse(raw []byte, getenv func(string) string) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	overrideFromEnv(&cfg.Emulator, getenv)
	applyDefaults(&cfg.Emulator)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func overrideFromEnv(e *Emulator, getenv func(string) string) {
	if v := getenv("UNIFI_INFORM_URL"); v != "" {
		e.InformURL = v
	}
	if v := getenv("COLLECTOR_URL"); v != "" {
		e.CollectorURL = v
	}
	if v := getenv("STATE_DIR"); v != "" {
		e.StateDir = v
	}
}

func applyDefaults(e *Emulator) {
	if e.StateDir == "" {
		e.StateDir = DefaultStateDir
	}
	if e.InformIntervalSeconds == 0 {
		e.InformIntervalSeconds = int(DefaultInformInterval / time.Second)
	}
	if e.TelemetryPollSeconds == 0 {
		e.TelemetryPollSeconds = int(DefaultPollInterval / time.Second)
	}
	if e.StaleAfterSeconds == 0 {
		e.StaleAfterSeconds = int(DefaultStaleAfter / time.Second)
	}
}

func (c *Config) validate() error {
	var errs []error
	if err := validateHTTPURL("emulator.inform_url", c.Emulator.InformURL); err != nil {
		errs = append(errs, err)
	}
	if err := validateHTTPURL("emulator.collector_url", c.Emulator.CollectorURL); err != nil {
		errs = append(errs, err)
	}
	if c.Emulator.InformIntervalSeconds < 1 || c.Emulator.TelemetryPollSeconds < 1 || c.Emulator.StaleAfterSeconds < 1 {
		errs = append(errs, errors.New("emulator intervals must be positive"))
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
		if s.UniFi.Model == "" {
			errs = append(errs, fmt.Errorf("switch %q: unifi.model is required", s.ID))
		}
	}
	for _, s := range c.Switches {
		for _, dl := range s.UniFi.Downlinks {
			if dl.Port < 1 || dl.Switch == s.ID || !seen[dl.Switch] {
				errs = append(errs, fmt.Errorf("switch %q: downlink %+v needs port >= 1 and another configured switch id", s.ID, dl))
			}
		}
	}
	if len(c.Switches) == 0 {
		errs = append(errs, errors.New("no switches configured"))
	}
	return errors.Join(errs...)
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

// InformInterval returns the configured initial inform interval.
func (e Emulator) InformInterval() time.Duration {
	return time.Duration(e.InformIntervalSeconds) * time.Second
}

// TelemetryPoll returns the collector polling interval.
func (e Emulator) TelemetryPoll() time.Duration {
	return time.Duration(e.TelemetryPollSeconds) * time.Second
}

// StaleAfter returns the telemetry age limit.
func (e Emulator) StaleAfter() time.Duration {
	return time.Duration(e.StaleAfterSeconds) * time.Second
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
