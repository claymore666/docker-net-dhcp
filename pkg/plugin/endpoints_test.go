package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
