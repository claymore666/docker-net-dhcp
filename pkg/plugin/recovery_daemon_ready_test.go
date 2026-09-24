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

// Docker respawns the plugin during its own startup, so recovery's first Docker call
// can meet a daemon that is not serving yet (#383).

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

func TestRecoverEndpointsDeferred_CancelStopsTheWait(t *testing.T) {
	f := &fakeDocker{listErr: errors.New("daemon still starting")}
	p := &Plugin{docker: f}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.recoverEndpointsDeferred(ctx, time.Hour)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deferred recovery ignored context cancellation")
	}
}

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
