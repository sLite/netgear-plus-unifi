package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func TestExampleConfigs(t *testing.T) {
	for _, path := range []string{"../../config/switches.example.yaml", "../../config/switches.mock.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(raw, noEnv); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	raw, _ := os.ReadFile("../../config/switches.example.yaml")
	cfg, _ := Parse(raw, noEnv)
	if len(cfg.Enabled()) != 2 || cfg.Switches[0].UniFi.Model != "USL8LP" || cfg.Switches[1].UniFi.Model != "USL16LP" {
		t.Errorf("unexpected switches: %+v", cfg.Switches)
	}
	if cfg.StaleAfter() != 90*time.Second || cfg.InformInterval() != 10*time.Second {
		t.Errorf("unexpected intervals: %+v", cfg)
	}
	for _, sw := range cfg.Switches {
		if p := sw.Source.Policy(); p.Enabled || !p.DryRun {
			t.Errorf("%s: example must ship with writes off and dry run on: %+v", sw.ID, p)
		}
	}
}

const minimal = "controller: {inform_url: 'http://a:8080/inform'}\n"

func TestEnvironmentOverridesAndDefaults(t *testing.T) {
	raw := minimal + `switches:
  - {id: s, source: {host: 192.0.2.1, password_env: PW, write: {enabled: true}}, unifi: {model: USL8LP}}
  - {id: t, disabled: true, source: {adapter: mock}, unifi: {model: USL8LP}}
`
	env := map[string]string{"UNIFI_INFORM_URL": "http://192.0.2.1:8080/inform", "STATE_DIR": "/tmp/x", "PW": "secret"}
	cfg, err := Parse([]byte(raw), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Controller.InformURL != env["UNIFI_INFORM_URL"] || cfg.StateDir != "/tmp/x" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	sw := cfg.Enabled()
	if len(sw) != 1 || sw[0].ID != "s" {
		t.Fatalf("disabled switch not skipped: %+v", sw)
	}
	src := sw[0].Source
	if src.Adapter != AdapterNetgear || src.PollInterval() != 15*time.Second || src.SessionYield() != 5*time.Minute {
		t.Errorf("defaults not applied: %+v", src)
	}
	if p := src.Policy(); !p.Enabled || !p.DryRun || p.Debounce != 10*time.Second {
		t.Errorf("enabled writes must default to dry run: %+v", p)
	}
	if pw, err := src.Password(func(k string) string { return env[k] }); err != nil || pw != "secret" {
		t.Errorf("password = %q, %v", pw, err)
	}
	if _, err := src.Password(noEnv); err == nil || !strings.Contains(err.Error(), "PW") {
		t.Errorf("missing password: %v", err)
	}
}

func TestValidation(t *testing.T) {
	sw := func(s string) string { return minimal + "switches: [" + s + "]" }
	cases := map[string]string{
		"switches: [{id: s, source: {adapter: mock}, unifi: {model: US8}}]":                                                "inform_url",
		"controller: {inform_url: 'ftp://x'}\nswitches: [{id: s, source: {adapter: mock}, unifi: {model: US8}}]":           "inform_url",
		sw("{id: S!, source: {adapter: mock}, unifi: {model: US8}}"):                                                       "lowercase",
		sw("{id: s, source: {adapter: mock}}"):                                                                             "unifi.model",
		sw("{id: s, source: {adapter: mock}, unifi: {model: US8}}, {id: s, source: {adapter: mock}, unifi: {model: US8}}"): "duplicated",
		sw("{id: s, source: {host: h}, unifi: {model: US8}}"):                                                              "password_env",
		sw("{id: s, source: {host: h, password_env: P, expected_model: GS105E}, unifi: {model: US8}}"):                     "not supported",
		sw("{id: s, source: {adapter: snmp}, unifi: {model: US8}}"):                                                        "adapter",
		sw("{id: s, source: {adapter: mock, mock: {model: GS105E}}, unifi: {model: US8}}"):                                 "mock.model",
		sw("{id: s, source: {adapter: mock}, unifi: {model: US8, ip: 'not-an-ip'}}"):                                       "unifi.ip",
		sw("{id: s, source: {adapter: mock, write: {ports: [4]}}, unifi: {model: US8}}"):                                   "ports",
		"emulator: {inform_url: 'http://a'}\nswitches: []":                                                                 "controller.inform_url",
		"collector: {listen: ':8099'}\nswitches: []":                                                                       "top-level",
	}
	for raw, want := range cases {
		if _, err := Parse([]byte(raw), noEnv); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %q: error %v, want mention of %q", raw, err, want)
		}
	}
}

func TestDownlinkValidation(t *testing.T) {
	b := "{id: b, source: {adapter: mock}, unifi: {model: US8}}"
	ok := minimal + "switches: [{id: a, source: {adapter: mock}, unifi: {model: US8, downlinks: [{port: 2, switch: b}]}}, " + b + "]"
	if _, err := Parse([]byte(ok), noEnv); err != nil {
		t.Errorf("valid downlink rejected: %v", err)
	}
	for _, bad := range []string{
		"[{id: a, source: {adapter: mock}, unifi: {model: US8, downlinks: [{port: 2, switch: zzz}]}}]",
		"[{id: a, source: {adapter: mock}, unifi: {model: US8, downlinks: [{port: 2, switch: a}]}}]",
		"[{id: a, source: {adapter: mock}, unifi: {model: US8, downlinks: [{port: 0, switch: b}]}}, " + b + "]",
	} {
		if _, err := Parse([]byte(minimal+"switches: "+bad), noEnv); err == nil || !strings.Contains(err.Error(), "downlink") {
			t.Errorf("invalid downlink accepted: %s (%v)", bad, err)
		}
	}
}
