// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func shrinkShutdownTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := pluginShutdownTimeout
	pluginShutdownTimeout = d
	t.Cleanup(func() { pluginShutdownTimeout = prev })
}

func instantManager() *dhcpManager {
	m := &dhcpManager{startedCh: make(chan struct{}), startErr: errors.New("start failed")}
	close(m.startedCh)
	return m
}

func servedPlugin(t *testing.T, p *Plugin, handler http.Handler) *http.Client {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "test.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.server = http.Server{Handler: handler}
	go func() { _ = p.server.Serve(l) }()

	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

func TestClose_WaitsForInFlightHandlerBeforeDraining(t *testing.T) {
	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	handlerEntered := make(chan struct{})
	release := make(chan struct{})
	client := servedPlugin(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-release
		p.registerDHCPManager("late-endpoint", instantManager())
		w.WriteHeader(http.StatusOK)
	}))

	go func() {
		resp, err := client.Post("http://unix/NetworkDriver.Join", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-handlerEntered

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	select {
	case err := <-closed:
		t.Fatalf("Close returned while a handler was still in flight (err=%v) — the registry it drained was not final", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the handler finished")
	}

	p.mu.Lock()
	left := len(p.persistentDHCP)
	p.mu.Unlock()
	if left != 0 {
		t.Errorf("%d manager(s) left in the registry after Close; the late registration was not swept", left)
	}
}

func TestClose_WedgedHandlerStillBoundsShutdown(t *testing.T) {
	shrinkShutdownTimeout(t, 200*time.Millisecond)

	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	handlerEntered := make(chan struct{})
	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	client := servedPlugin(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-wedged
	}))

	go func() {
		resp, err := client.Post("http://unix/NetworkDriver.Join", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-handlerEntered

	start := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close on the forced path returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close never returned with a wedged handler — the shutdown budget is not bounding it")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Close took %v against a 200ms budget", elapsed)
	}
}

func TestClose_PhasesShareOneBudget(t *testing.T) {
	const budget = 300 * time.Millisecond
	shrinkShutdownTimeout(t, budget)

	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	handlerEntered := make(chan struct{})
	wedged := make(chan struct{})
	t.Cleanup(func() { close(wedged) })
	client := servedPlugin(t, p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-wedged
	}))
	go func() {
		resp, err := client.Post("http://unix/NetworkDriver.Join", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-handlerEntered

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	p.displacedStops.Add(1)
	go func() {
		defer p.displacedStops.Done()
		<-stuck
	}()

	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 4*budget {
		t.Errorf("Close took %v with a %v budget — phases look like they are each getting their own timeout", elapsed, budget)
	}
}

func TestClose_DrainsDisplacedManagerStops(t *testing.T) {
	p := newTestPlugin(t)
	p.docker = &fakeDocker{}

	releasing := make(chan struct{})
	stopped := make(chan struct{})
	p.displacedStops.Add(1)
	go func() {
		defer p.displacedStops.Done()
		<-releasing
		close(stopped)
	}()

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	select {
	case <-closed:
		t.Fatal("Close returned while a displaced manager was still releasing its lease")
	case <-time.After(150 * time.Millisecond):
	}

	close(releasing)
	<-stopped

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the displaced stop finished")
	}
}

func TestWaitBounded(t *testing.T) {
	var done sync.WaitGroup
	if !waitBounded(&done, time.Second) {
		t.Error("waitBounded on an empty group reported a timeout")
	}

	var blocked sync.WaitGroup
	blocked.Add(1)
	t.Cleanup(blocked.Done)
	if waitBounded(&blocked, 50*time.Millisecond) {
		t.Error("waitBounded reported completion for a group that never finished")
	}
}
