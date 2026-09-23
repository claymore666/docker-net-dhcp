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

func TestNewInstanceID_NeverEmpty(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := newInstanceID(); got == "" {
			t.Fatalf("newInstanceID returned empty on call %d — an empty id "+
				"silently disables every reset check that compares it", i)
		}
	}
}

func TestNewInstanceID_DiffersBetweenCalls(t *testing.T) {
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
