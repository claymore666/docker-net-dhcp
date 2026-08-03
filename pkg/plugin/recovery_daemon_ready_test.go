// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
)

// The tests here cover #383: Docker respawns the plugin during its own
// startup, so recovery's first Docker call routinely meets a daemon that
// is not serving yet. Before the fix that single timeout counted a
// recovery_failure and abandoned recovery entirely, leaving every
// attached container without a renewal client — silently, because the
// tombstone path still preserved the addresses.

// TestRecoverEndpointsDeferred_DaemonComesUp is the ordinary case: the
// daemon was not ready when the socket came up, but answers shortly
// after. Nothing should be counted as a failure.
func TestRecoverEndpointsDeferred_DaemonComesUp(t *testing.T) {
	fastRetries(t)
	f := &fakeDocker{
		listErr:      errors.New("daemon still starting"),
		listErrUntil: 4,
		listResult:   []dNetwork.Summary{{ID: "n1", Driver: "bridge"}},
	}
	p := &Plugin{docker: f}

	p.recoverEndpointsDeferred(context.Background(), testDaemonWait)

	if got := p.recoveryDeferred.Load(); got != 1 {
		t.Errorf("recovery_deferred: got %d want 1", got)
	}
	if got := p.recoveryFailed.Load(); got != 0 {
		t.Errorf("recovery_failed: got %d want 0 — the daemon did come up", got)
	}
}

// TestRecoverEndpointsDeferred_DaemonNeverComesUp is the arm that must
// still count a real failure. Nothing retries after this, so the
// endpoints genuinely are running without renewal.
func TestRecoverEndpointsDeferred_DaemonNeverComesUp(t *testing.T) {
	fastRetries(t)
	f := &fakeDocker{listErr: errors.New("daemon is gone")}
	p := &Plugin{docker: f}

	p.recoverEndpointsDeferred(context.Background(), testDaemonWait)

	if got := p.recoveryDeferred.Load(); got != 1 {
		t.Errorf("recovery_deferred: got %d want 1", got)
	}
	if got := p.recoveryFailed.Load(); got != 1 {
		t.Errorf("recovery_failed: got %d want 1 — an exhausted retry budget is a real failure", got)
	}
}

// TestRecoverEndpointsDeferred_CancelStopsTheWait proves Close can stop
// the retry. Without it a plugin shutting down mid-wait would sit for
// the full budget, and worse, could register a manager after the
// shutdown drain had already run.
func TestRecoverEndpointsDeferred_CancelStopsTheWait(t *testing.T) {
	f := &fakeDocker{listErr: errors.New("daemon still starting")}
	p := &Plugin{docker: f}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A wait far longer than the test could tolerate: only the
		// cancel can end this.
		p.recoverEndpointsDeferred(ctx, time.Hour)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred recovery ignored context cancellation")
	}
}

// TestApiHealth_RecoveryDeferredIsNotUnhealthy pins the classification.
// Meeting a still-starting daemon is the expected state at plugin
// respawn; if it flipped healthy false, every host reboot would page an
// operator over nothing — the #373/#376 mistake, one site further along.
func TestApiHealth_RecoveryDeferredIsNotUnhealthy(t *testing.T) {
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		startTime:      time.Now(),
	}
	p.recoveryDeferred.Add(3)

	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodPost, "/Plugin.Health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusOK)
	}
	var h HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if h.RecoveryDeferred != 3 {
		t.Errorf("recovery_deferred: got %d want 3", h.RecoveryDeferred)
	}
	if !h.Healthy {
		t.Error("recovery_deferred must not make the plugin unhealthy on its own")
	}
}
