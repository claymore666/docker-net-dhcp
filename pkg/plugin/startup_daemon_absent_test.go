// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A restarting daemon disables a plugin whose socket is late (#1176); startRecovery must not wait on it.

// startupFakeDaemon counts requests: a hanging one never answers, an answering one lists no networks (#1176).
func startupFakeDaemon(t *testing.T, answer bool) (*Plugin, *atomic.Int64) {
	t.Helper()
	var seen atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if !answer {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("API-Version", "1.47")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/_ping" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	p := &Plugin{}
	client, err := newDockerClient("tcp://"+srv.Listener.Addr().String(), p)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	p.docker = client
	return p, &seen
}

func TestStartRecovery_AnAbsentDaemonDefersRecoveryWithoutWaiting(t *testing.T) {
	fastRetries(t)
	p, seen := startupFakeDaemon(t, false)
	p.engine.Store(&engineIdentity{Version: unknownEngineField, APIVersion: unknownEngineField})
	p.daemonAnswered.Store(false)

	start := time.Now()
	p.startRecovery()
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("startRecovery took %v with the daemon absent; nothing before Listen may wait on it, "+
			"or the daemon disables the plugin before its socket exists", elapsed)
	}
	if !p.recoveryPending {
		t.Error("recoveryPending is false: the post-Listen path would never run recovery or the engine check")
	}
	if got := seen.Load(); got != 0 {
		t.Errorf("the absent daemon saw %d requests before Listen, want 0", got)
	}
}

func TestStartRecovery_AnAnsweringDaemonStillRecoversBeforeListen(t *testing.T) {
	fastRetries(t)
	p, seen := startupFakeDaemon(t, true)
	p.engine.Store(&engineIdentity{Version: "27.0.0", APIVersion: "1.47"})
	p.daemonAnswered.Store(true)

	p.startRecovery()

	if p.recoveryPending {
		t.Error("recoveryPending is true although the daemon answered; recovery must finish before Listen (#383)")
	}
	if got := seen.Load(); got < 1 {
		t.Errorf("the answering daemon saw %d requests, want at least 1: recovery did not run", got)
	}
}

// An error status is an answer (#383); only a probe that never reached the daemon defers recovery (#1176).
func TestProbeEngine_OnlyAnUnreachableDaemonIsAbsent(t *testing.T) {
	serve := func(h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return "tcp://" + srv.Listener.Addr().String()
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closedHost := "tcp://" + closed.Listener.Addr().String()
	closed.Close()

	version := func(w http.ResponseWriter, r *http.Request, status int) {
		w.Header().Set("API-Version", "1.47")
		if r.URL.Path == "/_ping" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"daemon is starting"}`))
	}

	for _, tc := range []struct {
		name         string
		host         string
		ctxTimeout   time.Duration
		wantAnswered bool
	}{
		{"refused connection", closedHost, 0, false},
		{"hangs past the probe", serve(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), 200 * time.Millisecond, false},
		{"ping ok, version query fails", serve(func(w http.ResponseWriter, r *http.Request) { version(w, r, http.StatusOK) }), 0, true},
		{"ping answers 503", serve(func(w http.ResponseWriter, r *http.Request) { version(w, r, http.StatusServiceUnavailable) }), 0, true},
		{"ping answers 500", serve(func(w http.ResponseWriter, r *http.Request) { version(w, r, http.StatusInternalServerError) }), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			client, err := newDockerClient(tc.host, p)
			if err != nil {
				t.Fatalf("docker client: %v", err)
			}
			p.docker = client
			ctx := context.Background()
			if tc.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTimeout)
				defer cancel()
			}

			if err := p.probeEngine(ctx); err != nil {
				t.Fatalf("probeEngine refused: %v", err)
			}
			if got := p.daemonAnsweredAtStart(); got != tc.wantAnswered {
				t.Errorf("daemonAnsweredAtStart: got %v want %v", got, tc.wantAnswered)
			}
		})
	}
}

// The daemon answers /_ping, fails the first version query and then recovers: recovery finishes before Listen, so
// the engine must still get its second look once the socket serves (#1176).
func TestStartup_ASlowFirstVersionQueryIsIdentifiedAfterListen(t *testing.T) {
	var versionQueries atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.47")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/_ping":
		case strings.HasSuffix(r.URL.Path, "/version"):
			if versionQueries.Add(1) == 1 {
				http.Error(w, `{"message":"starting"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"Version":"29.7.2","ApiVersion":"1.47"}`))
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	defer srv.Close()

	p := &Plugin{}
	client, err := newDockerClient("tcp://"+srv.Listener.Addr().String(), p)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	p.docker = client

	if err := p.probeEngine(context.Background()); err != nil {
		t.Fatalf("probeEngine refused: %v", err)
	}
	p.startRecovery()
	if p.recoveryPending {
		t.Fatal("recovery was deferred; this drive needs the synchronous path")
	}
	if p.engineAnswered() {
		t.Fatal("the first version query was meant to fail")
	}

	go func() { _ = p.Listen(filepath.Join(t.TempDir(), "p.sock")) }()
	t.Cleanup(func() { _ = p.server.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for !p.engineAnswered() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.engineSnapshot().Version; got != "29.7.2" {
		t.Errorf("engine after Listen: got %q want 29.7.2 after %d version queries", got, versionQueries.Load())
	}
}

func TestStartup_ThePreListenBudgetFitsTheDaemonsDialWindow(t *testing.T) {
	// Moby pluginPostStart (daemon/pkg/plugin/manager_linux.go, c13266d2c0ab) dials at +0.5, +3.5, +6.5, +9.5 s (#1176).
	const (
		daemonPluginLastDial = 500*time.Millisecond + 3*3*time.Second
		boardAllowance       = 5 * time.Second
		measuredBoardWork    = 3 * time.Second
	)

	// The answered path also spends recoverySyncDaemonWait; its second engine look runs after Listen (#1176).
	if got := engineProbeTimeout + recoverySyncDaemonWait + measuredBoardWork; got >= daemonPluginLastDial {
		t.Errorf("the answered path's pre-Listen budget is %v (probe %v, sync wait %v, %v of board work) and the "+
			"daemon's last dial is at %v", got, engineProbeTimeout, recoverySyncDaemonWait, measuredBoardWork,
			daemonPluginLastDial)
	}

	if got := engineProbeTimeout + boardAllowance; got >= daemonPluginLastDial {
		t.Errorf("the pre-Listen budget is %v (engineProbeTimeout %v plus %v for the state dir, record and IPAM "+
			"index) and the daemon's last dial of the socket is at %v. A longer probe lets the daemon disable "+
			"the plugin on a slow host (#1176)",
			got, engineProbeTimeout, boardAllowance, daemonPluginLastDial)
	}
}
