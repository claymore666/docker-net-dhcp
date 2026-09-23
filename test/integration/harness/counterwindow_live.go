// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"testing"
	"time"

	docker "github.com/docker/docker/client"
)

// CounterWindow brackets two /Plugin.Health reads and refuses a delta across a plugin restart: counters are in-memory
// per process, and three tests end the process on purpose (#405).
type CounterWindow struct {
	t             *testing.T
	ctx           context.Context
	cli           *docker.Client
	before        *HealthResponse
	after         *HealthResponse
	counters      []string
	expectRecycle bool
	ended         bool
}

// BeginCounterWindow opens a window on a health read, failing the test if it cannot read.
func BeginCounterWindow(t *testing.T, ctx context.Context, cli *docker.Client, counters ...string) *CounterWindow {
	t.Helper()
	before, err := PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("opening a counter window: %v\n"+
			"Without a baseline read there is nothing to compare against; failing "+
			"rather than measuring from an assumed zero.", err)
	}
	w := &CounterWindow{t: t, ctx: ctx, cli: cli, before: before, counters: counters}
	t.Cleanup(func() {
		// Only reported when the test was otherwise fine, so it never buries the real failure.
		if !w.ended && !t.Failed() {
			t.Errorf("counter window opened at BeginCounterWindow was never closed with End() — "+
				"no reset check ran and no delta was verified (counters: %v)", w.counters)
		}
	})
	return w
}

// ExpectRecycle declares the window spans a plugin restart and makes End fail if none happened; it is an assertion,
// never an opt-out (#405, #413).
func (w *CounterWindow) ExpectRecycle() *CounterWindow {
	w.expectRecycle = true
	return w
}

// Before returns the opening read without closing the window.
func (w *CounterWindow) Before() *HealthResponse {
	return w.before
}

// Await polls health until cond(now, before) holds or budget is spent, and returns the last read. It fails when
// identity breaks mid-poll (#405), and when no read ever succeeded; a single read error is tolerated.
func (w *CounterWindow) Await(budget time.Duration, cond func(now, before *HealthResponse) bool) (*HealthResponse, bool) {
	w.t.Helper()
	deadline := time.Now().Add(budget)
	var last *HealthResponse
	for time.Now().Before(deadline) {
		h, err := PluginHealth(w.ctx, w.cli)
		if err == nil {
			last = h
			if msg := CounterWindowError(CompareInstances(w.before, h), w.expectRecycle, w.before, h, w.counters...); msg != "" {
				w.t.Fatalf("while waiting for a condition on the plugin's counters: %s", msg)
			}
			if cond(h, w.before) {
				return h, true
			}
		}
		time.Sleep(awaitPollInterval)
	}
	if last == nil {
		w.t.Fatalf("waited %s for a condition on the plugin's counters and never got a single "+
			"successful health read; that is an unreachable plugin, not an unmet condition (counters: %v)",
			budget, w.counters)
	}
	return last, false
}

// End takes the closing read, fails on an unreadable read, an unexpected recycle or unknown identity, and returns both
// reads; later calls return the same closing read (#405).
func (w *CounterWindow) End() (before, after *HealthResponse) {
	w.t.Helper()
	if w.ended {
		return w.before, w.after
	}
	w.ended = true

	after, err := PluginHealth(w.ctx, w.cli)
	if err != nil {
		w.t.Fatalf("closing a counter window: %v\n"+
			"The delta for %v cannot be computed, and an unreadable plugin is not "+
			"a passing measurement.", err, w.counters)
	}
	if msg := CounterWindowError(CompareInstances(w.before, after), w.expectRecycle, w.before, after, w.counters...); msg != "" {
		w.t.Fatal(msg)
	}
	w.after = after
	return w.before, after
}
