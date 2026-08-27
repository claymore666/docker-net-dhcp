// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestApiHealth(t *testing.T) {
	p := &Plugin{
		startTime:      time.Now().Add(-3 * time.Second),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	// Seed some state so we can verify the counters reflect it.
	p.storeJoinHint("ep-pending-1", joinHint{Gateway: "192.168.0.1"})
	p.storeJoinHint("ep-pending-2", joinHint{Gateway: "192.168.0.1"})
	p.registerDHCPManager("ep-active-1", &dhcpManager{})

	req := httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil)
	rec := httptest.NewRecorder()
	p.apiHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}

	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if !got.Healthy {
		t.Error("expected healthy=true")
	}
	if got.ActiveEndpoints != 1 {
		t.Errorf("expected 1 active endpoint, got %d", got.ActiveEndpoints)
	}
	if got.PendingHints != 2 {
		t.Errorf("expected 2 pending hints, got %d", got.PendingHints)
	}
	if got.UptimeSeconds < 2.5 || got.UptimeSeconds > 60 {
		t.Errorf("uptime should be ~3s after seeding, got %v", got.UptimeSeconds)
	}
	if got.TombstoneWriteFailures != 0 {
		t.Errorf("expected 0 tombstone write failures, got %d", got.TombstoneWriteFailures)
	}
}

// TestApiHealth_TombstoneWriteFailureUnhealthy verifies that a non-zero
// tombstoneWriteFailures counter flips the response to unhealthy. The
// counter is bumped from addTombstone's saveTombstones error path; we
// just write to it directly here since the surface we care about is
// what /Plugin.Health reports, not the disk-write path that already
// has its own coverage.
func TestApiHealth_TombstoneWriteFailureUnhealthy(t *testing.T) {
	p := &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	p.tombstoneWriteFailures.Add(2)

	req := httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil)
	rec := httptest.NewRecorder()
	p.apiHealth(rec, req)

	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Healthy {
		t.Error("tombstone write failures must mark plugin unhealthy")
	}
	if got.TombstoneWriteFailures != 2 {
		t.Errorf("expected 2 tombstone failures reported, got %d", got.TombstoneWriteFailures)
	}
}

// TestApiHealth_JoinStartFailureUnhealthy verifies that a non-zero
// joinStartFailures counter flips the response to unhealthy and is
// reported under join_start_failures (#317). The counter is bumped from
// Join's Start-failure goroutine; as with the tombstone sibling above,
// the surface under test is what /Plugin.Health reports.
func TestApiHealth_JoinStartFailureUnhealthy(t *testing.T) {
	p := &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	p.joinStartFailures.Add(1)

	req := httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil)
	rec := httptest.NewRecorder()
	p.apiHealth(rec, req)

	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Healthy {
		t.Error("join-start failures must mark plugin unhealthy — a running container has no renewal client")
	}
	if got.JoinStartFailures != 1 {
		t.Errorf("expected 1 join-start failure reported, got %d", got.JoinStartFailures)
	}
}

// TestApiHealth_PerFamilyCounters pins the #212 contract on the wire as
// #730 restated it: BOTH halves are stored and rendered, and the
// un-suffixed counter is their sum rather than a counter of its own.
//
// The values are chosen so the aggregate cannot be confused with either
// half — no half equals another family's total — so a snapshot that
// rendered the wrong field fails a specific assertion rather than
// happening to match.
func TestApiHealth_PerFamilyCounters(t *testing.T) {
	p := &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	// Each family owns its counter; the un-suffixed field is the sum.
	p.naksReceivedV4.Add(5)
	p.naksReceivedV6.Add(2)
	p.dhcpTimeoutsV4.Add(3)
	p.dhcpTimeoutsV6.Add(1)
	p.leaseChangedV4.Add(4)
	p.leaseChangedV6.Add(6)
	p.clientStopFailuresV4.Add(2)
	p.clientStopFailuresV6.Add(1)

	req := httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil)
	rec := httptest.NewRecorder()
	p.apiHealth(rec, req)

	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range []struct {
		name           string
		agg, v4, v6    int32
		wantAgg, wantA int32
		wantB          int32
	}{
		{"naks_received", got.NAKsReceived, got.NAKsReceivedV4, got.NAKsReceivedV6, 7, 5, 2},
		{"dhcp_timeouts", got.DHCPTimeouts, got.DHCPTimeoutsV4, got.DHCPTimeoutsV6, 4, 3, 1},
		{"lease_changed", got.LeaseChanged, got.LeaseChangedV4, got.LeaseChangedV6, 10, 4, 6},
		{"client_stop_failures", got.ClientStopFailures, got.ClientStopFailuresV4, got.ClientStopFailuresV6, 3, 2, 1},
	} {
		if c.v4 != c.wantA || c.v6 != c.wantB {
			t.Errorf("%s: v4=%d v6=%d, want %d and %d", c.name, c.v4, c.v6, c.wantA, c.wantB)
		}
		if c.agg != c.wantAgg {
			t.Errorf("%s: aggregate=%d, want %d (v4+v6)", c.name, c.agg, c.wantAgg)
		}
	}

	// Pin the wire keys so the field names don't silently drift. Both
	// halves, because #730's whole point is that the v4 number is now
	// carried rather than reconstructed by a consumer.
	for _, key := range []string{
		"naks_received_v4", "dhcp_timeouts_v4", "leases_obtained_v4",
		"leases_renewed_v4", "lease_changed_v4", "client_stop_failures_v4",
		"naks_received_v6", "dhcp_timeouts_v6", "leases_obtained_v6",
		"leases_renewed_v6", "lease_changed_v6", "client_stop_failures_v6",
	} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Errorf("Health JSON missing %q field", key)
		}
	}
}
