// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newHealthPlugin() *Plugin {
	return &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
}

// macsEqual decides whether the device that answered is us. Getting it
// wrong in either direction is a shipped bug: a false "equal" hides a
// real conflict, a false "different" reports every bridge-mode endpoint
// as one.
func TestMACsEqual(t *testing.T) {
	mustMAC := func(s string) net.HardwareAddr {
		t.Helper()
		m, err := net.ParseMAC(s)
		if err != nil {
			t.Fatalf("ParseMAC(%q): %v", s, err)
		}
		return m
	}

	cases := []struct {
		name string
		a, b net.HardwareAddr
		want bool
	}{
		{"identical", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:00:53:01"), true},
		{"different", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:00:53:02"), false},
		{"last octet only", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:00:53:03"), false},
		// Both nil must NOT compare equal. A bytes.Equal would say yes,
		// and "we don't know either MAC" would then read as "the device
		// that answered is us" — silently discarding the conflict.
		{"both nil", nil, nil, false},
		{"ours unknown", mustMAC("00:00:5e:00:53:01"), nil, false},
		{"theirs unknown", nil, mustMAC("00:00:5e:00:53:01"), false},
		// A EUI-64 answer against a EUI-48 ours is not a match, and
		// must not panic on the length difference.
		{"different lengths", mustMAC("00:00:5e:00:53:01"), mustMAC("00:00:5e:ff:fe:00:53:01"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := macsEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("macsEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// A probe that cannot run must say so — as a probe failure, never as a
// clean address. #524 is a check that silently did not happen; a
// detector that silently declines to run is the same bug again.
func TestCheckAddressConflict_UnrunnableCountsAsFailure(t *testing.T) {
	cases := []struct {
		name              string
		parent, cidr, mac string
		wantFailures      int32
	}{
		{"unparseable address", "eth0", "not-an-address", "00:00:5e:00:53:01", 1},
		{"unparseable MAC", "eth0", "192.0.2.10/24", "not-a-mac", 1},
		{"empty MAC", "eth0", "192.0.2.10/24", "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newHealthPlugin()
			p.checkAddressConflict(tc.parent, tc.cidr, tc.mac, "endpoint-id", "network-id")
			if got := p.conflictProbeFailures.Load(); got != tc.wantFailures {
				t.Errorf("conflict_probe_failures = %d, want %d", got, tc.wantFailures)
			}
			if got := p.addressConflicts.Load(); got != 0 {
				t.Errorf("address_conflicts = %d, want 0 — an unrunnable probe is not a conflict", got)
			}
		})
	}
}

// No parent or no address means there is nothing to probe and nothing
// went wrong. Distinct from the cases above: those are a probe that
// should have run and could not.
func TestCheckAddressConflict_NothingToProbeIsNotAFailure(t *testing.T) {
	for _, tc := range []struct{ name, parent, cidr string }{
		{"no parent", "", "192.0.2.10/24"},
		{"no address", "eth0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newHealthPlugin()
			p.checkAddressConflict(tc.parent, tc.cidr, "00:00:5e:00:53:01", "e", "n")
			if got := p.conflictProbeFailures.Load(); got != 0 {
				t.Errorf("conflict_probe_failures = %d, want 0", got)
			}
			if got := p.addressConflicts.Load(); got != 0 {
				t.Errorf("address_conflicts = %d, want 0", got)
			}
		})
	}
}

// The counter has to reach the wire, and it has to move Healthy. A
// conflict that is only logged is what production already had.
func TestApiHealth_AddressConflictIsUnhealthy(t *testing.T) {
	p := newHealthPlugin()

	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var clean HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &clean); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !clean.Healthy {
		t.Fatalf("baseline is already unhealthy; the assertion below would pass for the wrong reason")
	}
	if clean.AddressConflicts != 0 {
		t.Errorf("address_conflicts = %d on a fresh plugin, want 0", clean.AddressConflicts)
	}

	p.addressConflicts.Add(1)
	rec = httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AddressConflicts != 1 {
		t.Errorf("address_conflicts = %d, want 1", got.AddressConflicts)
	}
	if got.Healthy {
		t.Error("healthy = true with an address conflict recorded; the endpoint is up on an address that belongs to someone else")
	}

	// Pin the wire keys — an operator's alert is written against these.
	for _, key := range []string{"address_conflicts", "conflict_probe_failures"} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Errorf("Health JSON missing %q field", key)
		}
	}
}

// A probe that could not run is not a broken address, so it must not
// latch the plugin unhealthy. Operators still need to see it, which is
// why it is a counter and not just a log line.
func TestApiHealth_ProbeFailureIsNotUnhealthy(t *testing.T) {
	p := newHealthPlugin()
	p.conflictProbeFailures.Add(3)

	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ConflictProbeFailures != 3 {
		t.Errorf("conflict_probe_failures = %d, want 3", got.ConflictProbeFailures)
	}
	if !got.Healthy {
		t.Error("healthy = false on probe failures alone; an unasked question is not a known-broken address")
	}
}
