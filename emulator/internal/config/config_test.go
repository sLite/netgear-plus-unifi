package config

import (
	"os"
	"strings"
	"testing"
)

func TestExampleConfig(t *testing.T) {
	raw, err := os.ReadFile("../../../config/switches.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(raw, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Enabled()) != 2 || cfg.Switches[0].UniFi.Model != "USL8LP" || cfg.Switches[1].UniFi.Model != "USL16LP" {
		t.Errorf("unexpected switches: %+v", cfg.Switches)
	}
	if cfg.Emulator.StaleAfter().Seconds() != 90 || cfg.Emulator.InformInterval().Seconds() != 10 {
		t.Errorf("unexpected intervals: %+v", cfg.Emulator)
	}
}

func TestEnvironmentOverridesAndDefaults(t *testing.T) {
	raw := []byte("emulator: {inform_url: 'http://a:8080/inform', collector_url: 'http://c:1'}\nswitches: [{id: s, unifi: {model: US8}}, {id: t, disabled: true, unifi: {model: US8}}]\n")
	env := map[string]string{"UNIFI_INFORM_URL": "http://192.0.2.1:8080/inform", "STATE_DIR": "/tmp/x"}
	cfg, err := Parse(raw, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Emulator.InformURL != env["UNIFI_INFORM_URL"] || cfg.Emulator.StateDir != "/tmp/x" || cfg.Emulator.TelemetryPollSeconds != 5 {
		t.Errorf("overrides/defaults not applied: %+v", cfg.Emulator)
	}
	if got := cfg.Enabled(); len(got) != 1 || got[0].ID != "s" {
		t.Errorf("disabled switch not skipped: %+v", got)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"emulator: {collector_url: 'http://c'}\nswitches: [{id: s, unifi: {model: US8}}]":                                                       "inform_url",
		"emulator: {inform_url: 'ftp://x', collector_url: 'http://c'}\nswitches: [{id: s, unifi: {model: US8}}]":                                "inform_url",
		"emulator: {inform_url: 'http://a', collector_url: 'http://c'}\nswitches: [{id: S!, unifi: {model: US8}}]":                              "lowercase",
		"emulator: {inform_url: 'http://a', collector_url: 'http://c'}\nswitches: [{id: s}]":                                                    "unifi.model",
		"emulator: {inform_url: 'http://a', collector_url: 'http://c'}\nswitches: [{id: s, unifi: {model: US8}}, {id: s, unifi: {model: US8}}]": "duplicated",
	}
	for raw, want := range cases {
		if _, err := Parse([]byte(raw), func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %q: error %v, want mention of %q", raw, err, want)
		}
	}
}

func TestDownlinkValidation(t *testing.T) {
	head := "emulator: {inform_url: 'http://a', collector_url: 'http://c'}\n"
	ok := head + "switches: [{id: a, unifi: {model: US8, downlinks: [{port: 2, switch: b}]}}, {id: b, unifi: {model: US8}}]"
	if _, err := Parse([]byte(ok), func(string) string { return "" }); err != nil {
		t.Errorf("valid downlink rejected: %v", err)
	}
	for _, bad := range []string{
		"switches: [{id: a, unifi: {model: US8, downlinks: [{port: 2, switch: zzz}]}}]",
		"switches: [{id: a, unifi: {model: US8, downlinks: [{port: 2, switch: a}]}}]",
		"switches: [{id: a, unifi: {model: US8, downlinks: [{port: 0, switch: b}]}}, {id: b, unifi: {model: US8}}]",
	} {
		if _, err := Parse([]byte(head+bad), func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "downlink") {
			t.Errorf("invalid downlink accepted: %s (%v)", bad, err)
		}
	}
}
