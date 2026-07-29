package plugin

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The outage watchdog's tick and grace are the only part of outage
// detection the plugin controls — the rest is the DHCP lease. They are
// overridable so the integration suite can stop paying the production
// cadence on top of a fixture lease it cannot shorten (#278/#356).
//
// What these tests protect: a deployment that sets nothing must behave
// exactly as before, and a misconfigured value must never reach
// time.NewTicker, which panics on a non-positive duration.

func TestOutageCadence_DefaultsWhenUnconfigured(t *testing.T) {
	// The nil-plugin case is real: unit tests drive a manager directly.
	m := &dhcpManager{}
	tick, grace := m.outageCadence()
	if tick != defaultOutageTick {
		t.Errorf("tick = %v, want the production default %v", tick, defaultOutageTick)
	}
	if grace != defaultOutageGrace {
		t.Errorf("grace = %v, want the production default %v", grace, defaultOutageGrace)
	}
}

func TestOutageCadence_ZeroFieldsFallBack(t *testing.T) {
	// A Plugin built without the knobs set must not produce a zero
	// tick — that is the value time.NewTicker panics on.
	m := &dhcpManager{plugin: &Plugin{}}
	tick, grace := m.outageCadence()
	if tick != defaultOutageTick || grace != defaultOutageGrace {
		t.Errorf("got %v/%v, want defaults %v/%v", tick, grace, defaultOutageTick, defaultOutageGrace)
	}
}

func TestOutageCadence_OverrideIsHonoured(t *testing.T) {
	m := &dhcpManager{plugin: &Plugin{
		outageTick:  2 * time.Second,
		outageGrace: 10 * time.Second,
	}}
	tick, grace := m.outageCadence()
	if tick != 2*time.Second {
		t.Errorf("tick = %v, want the configured 2s", tick)
	}
	if grace != 10*time.Second {
		t.Errorf("grace = %v, want the configured 10s", grace)
	}
}

func TestOutageCadence_TickIsFloored(t *testing.T) {
	// A hostile-but-parseable value must not spin the watchdog
	// goroutine, and must never be <= 0 by the time NewTicker sees it.
	for _, tc := range []time.Duration{1 * time.Nanosecond, time.Millisecond} {
		m := &dhcpManager{plugin: &Plugin{outageTick: tc}}
		tick, _ := m.outageCadence()
		if tick < minOutageTick {
			t.Errorf("tick %v produced %v, below the %v floor", tc, tick, minOutageTick)
		}
	}
}

func TestNewPluginOptions_ZeroValueIsProductionDefault(t *testing.T) {
	// NewPlugin dials docker, so exercise the defaulting logic the way
	// NewPlugin does rather than constructing a Plugin — this is the
	// contract cmd/net-dhcp relies on when it leaves fields at zero.
	opts := Options{}
	if opts.AwaitTimeout <= 0 {
		opts.AwaitTimeout = defaultAwaitTimeout
	}
	if opts.OutageTick <= 0 {
		opts.OutageTick = defaultOutageTick
	}
	if opts.OutageGrace <= 0 {
		opts.OutageGrace = defaultOutageGrace
	}

	p := &Plugin{
		awaitTimeout: opts.AwaitTimeout,
		outageTick:   opts.OutageTick,
		outageGrace:  opts.OutageGrace,
	}
	m := &dhcpManager{plugin: p}
	tick, grace := m.outageCadence()
	if tick != defaultOutageTick || grace != defaultOutageGrace {
		t.Errorf("zero Options gave %v/%v, want %v/%v", tick, grace, defaultOutageTick, defaultOutageGrace)
	}
	if p.awaitTimeout != defaultAwaitTimeout {
		t.Errorf("awaitTimeout = %v, want %v", p.awaitTimeout, defaultAwaitTimeout)
	}
}

// TestConfigJSONMatchesCodeDefaults keeps the shipped manifest honest.
// config.json's declared values are what an operator reads to learn the
// defaults, and nothing else would catch them drifting from the code.
func TestConfigJSONMatchesCodeDefaults(t *testing.T) {
	want := map[string]time.Duration{
		"AWAIT_TIMEOUT": defaultAwaitTimeout,
		"OUTAGE_TICK":   defaultOutageTick,
		"OUTAGE_GRACE":  defaultOutageGrace,
	}

	for _, path := range []string{"../../config.json", "../../config-cover.json"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var cfg struct {
			Env []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		seen := map[string]bool{}
		for _, e := range cfg.Env {
			expected, tracked := want[e.Name]
			if !tracked {
				continue
			}
			seen[e.Name] = true
			got, err := time.ParseDuration(e.Value)
			if err != nil {
				t.Errorf("%s: %s value %q does not parse as a duration", path, e.Name, e.Value)
				continue
			}
			if got != expected {
				t.Errorf("%s: %s declares %s, code default is %s", path, e.Name, e.Value, expected)
			}
		}
		for name := range want {
			if !seen[name] {
				t.Errorf("%s: %s is not declared, so `docker plugin set %s=...` would be rejected", path, name, name)
			}
		}
	}
}
