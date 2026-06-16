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

// TestApiHealth_PerFamilyCounters pins the #212 contract on the wire:
// the un-suffixed counters stay v4+v6 aggregates while the *_v6 siblings
// carry the v6 subset, and both surface under their snake_case keys.
func TestApiHealth_PerFamilyCounters(t *testing.T) {
	p := &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	// Aggregates are totals; the v6 siblings are a subset of them.
	p.naksReceived.Add(5)
	p.naksReceivedV6.Add(2)
	p.dhcpTimeouts.Add(3)
	p.dhcpTimeoutsV6.Add(1)
	p.leaseChanged.Add(4)
	p.leaseChangedV6.Add(4)

	req := httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil)
	rec := httptest.NewRecorder()
	p.apiHealth(rec, req)

	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NAKsReceived != 5 || got.NAKsReceivedV6 != 2 {
		t.Errorf("naks: aggregate=%d v6=%d, want 5 and 2", got.NAKsReceived, got.NAKsReceivedV6)
	}
	if got.DHCPTimeouts != 3 || got.DHCPTimeoutsV6 != 1 {
		t.Errorf("timeouts: aggregate=%d v6=%d, want 3 and 1", got.DHCPTimeouts, got.DHCPTimeoutsV6)
	}
	if got.LeaseChanged != 4 || got.LeaseChangedV6 != 4 {
		t.Errorf("lease_changed: aggregate=%d v6=%d, want 4 and 4", got.LeaseChanged, got.LeaseChangedV6)
	}

	// Pin the wire keys so the field names don't silently drift.
	for _, key := range []string{"naks_received_v6", "dhcp_timeouts_v6", "leases_obtained_v6", "leases_renewed_v6", "lease_changed_v6"} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Errorf("Health JSON missing %q field", key)
		}
	}
}
