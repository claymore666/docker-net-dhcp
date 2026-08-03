// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The instance id exists so a consumer holding two /Plugin.Health reads
// can tell "these counters did not move" apart from "these counters were
// reset under me" (#405). Everything below defends one of the three
// properties that makes it usable for that: it is never empty, it
// differs between processes, and it is stable within one.

func TestNewInstanceID_NeverEmpty(t *testing.T) {
	// An empty id is the dangerous value, not merely an ugly one: two
	// empty ids compare equal, so a consumer would read "same process"
	// across a genuine restart and trust a delta spanning a reset.
	for i := 0; i < 100; i++ {
		if got := newInstanceID(); got == "" {
			t.Fatalf("newInstanceID returned empty on call %d — an empty id "+
				"silently disables every reset check that compares it", i)
		}
	}
}

func TestNewInstanceID_DiffersBetweenCalls(t *testing.T) {
	// Each call stands in for a plugin process. A generator that
	// repeated itself would make a restart look like continuity, which
	// is the failure #405 is about.
	const n = 100
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		id := newInstanceID()
		if prev, dup := seen[id]; dup {
			t.Fatalf("newInstanceID returned %q on both call %d and call %d — "+
				"a repeated id makes a plugin restart indistinguishable from "+
				"continuous uptime", id, prev, i)
		}
		seen[id] = i
	}
}

func TestNewPlugin_InstanceIDIsStableAcrossReads(t *testing.T) {
	// Within one process the id must not move, or every delta would
	// report a spurious reset and the check would be worthless in the
	// opposite direction.
	p := &Plugin{
		startTime:      time.Now(),
		instanceID:     newInstanceID(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}

	first := healthOf(t, p)
	if first.InstanceID == "" {
		t.Fatal("/Plugin.Health reported an empty instance_id")
	}
	second := healthOf(t, p)
	if first.InstanceID != second.InstanceID {
		t.Errorf("instance_id changed between two reads of the same plugin: %q then %q",
			first.InstanceID, second.InstanceID)
	}
}

func TestNewPlugin_DistinctPluginsReportDistinctInstanceIDs(t *testing.T) {
	// The end-to-end property, read off the wire rather than the field:
	// two plugins are two processes and must be distinguishable through
	// /Plugin.Health, which is the only surface the integration harness
	// has.
	mk := func() *Plugin {
		return &Plugin{
			startTime:      time.Now(),
			instanceID:     newInstanceID(),
			joinHints:      make(map[string]joinHint),
			persistentDHCP: make(map[string]*dhcpManager),
		}
	}
	a, b := healthOf(t, mk()), healthOf(t, mk())
	if a.InstanceID == b.InstanceID {
		t.Errorf("two separate plugins both reported instance_id %q — a recycle "+
			"between two health reads would be invisible", a.InstanceID)
	}
}

func healthOf(t *testing.T, p *Plugin) HealthResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Plugin.Health returned %d (body=%s)", rec.Code, rec.Body.String())
	}
	var out HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode health: %v (body=%s)", err, rec.Body.String())
	}
	return out
}
