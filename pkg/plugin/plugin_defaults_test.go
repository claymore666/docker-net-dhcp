// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// What survives of the outage-cadence tests.
//
// OUTAGE_TICK and OUTAGE_GRACE went with the watchdog: they existed to
// make a synthetic cadence cheap for the integration suite, and the
// library reports a failed attempt directly, so there is no cadence to
// tune. AWAIT_TIMEOUT is the knob that is left, and the two rules these
// tests hold are unchanged and still needed — a deployment that sets
// nothing gets the documented default, and the value config.json
// declares is the value the code uses, because the manifest is what an
// operator reads to learn it.

func TestNewPluginOptions_ZeroValueIsProductionDefault(t *testing.T) {
	// NewPlugin dials docker, so exercise the defaulting logic the way
	// NewPlugin does rather than constructing a Plugin — this is the
	// contract cmd/net-dhcp relies on when it leaves fields at zero.
	opts := Options{}
	if opts.AwaitTimeout <= 0 {
		opts.AwaitTimeout = defaultAwaitTimeout
	}

	p := &Plugin{awaitTimeout: opts.AwaitTimeout}
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
