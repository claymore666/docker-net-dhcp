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

// AwaitRecoveryRebuildWindow waits, on the window that spans the
// recycle, for a property that only holds once recovery has rebuilt an
// endpoint's renewal client.
//
// Through the window rather than a bare health poll because every read
// is then checked against the instance the window opened on
// (CounterWindow.Await): a second, unnoticed restart inside the wait
// would otherwise satisfy or defeat the condition with counters from a
// process the test never meant to measure — the #405 shape, in its
// slowest form.
//
// The caller asserts. This returns whether the property held so the
// failure text, and the decision that it IS a failure, stay at the call
// site.
func AwaitRecoveryRebuildWindow(w *CounterWindow, what string, cond func(*HealthResponse) bool) (*HealthResponse, bool) {
	w.t.Helper()
	return awaitRecoveryRebuild(w.t.Logf, what, func(budget time.Duration) (*HealthResponse, bool) {
		return w.Await(budget, func(now, _ *HealthResponse) bool { return cond(now) })
	})
}

// AwaitRecoveryRebuildOn is the same wait for a site that cannot hold a
// window across the event.
//
// The daemon-restart tests are that site: the restart ends the plugin
// process AND the client the window was opened with, so their windows
// are closed before the daemon goes down and the read afterwards is
// taken on a fresh client. That read is identity-blind — it always has
// been, this only makes it a bounded wait rather than a single sample —
// and the blindness is the reason this is a separate function instead
// of a default.
func AwaitRecoveryRebuildOn(t *testing.T, ctx context.Context, cli *docker.Client, what string,
	cond func(*HealthResponse) bool) (*HealthResponse, bool) {
	t.Helper()
	return awaitRecoveryRebuild(t.Logf, what, func(budget time.Duration) (*HealthResponse, bool) {
		deadline := time.Now().Add(budget)
		var last *HealthResponse
		for {
			if h, err := PluginHealth(ctx, cli); err == nil {
				last = h
				if cond(h) {
					return h, true
				}
			}
			if !time.Now().Before(deadline) {
				return last, false
			}
			time.Sleep(awaitPollInterval)
		}
	})
}

// DumpPluginLogOnFailure prints the plugin's own log for the window
// beginning at mark, but only when the test failed.
//
// The recycle tests had no plugin log in the job at all: the fixture
// dumper reads dnsmasq and nothing else, and the suite's teardown
// removes the plugin, which destroys the file. So the lines that say
// what recovery did — "Plugin recovery complete", the deferred retry,
// the per-endpoint Start failure — were never captured for the one run
// that needed them.
//
// Windowed, not whole: on the one-process lane the log spans the entire
// suite, and a failure here would otherwise arrive under ten minutes of
// another test's output (#933). Only on failure, because a passing run
// does not need it and the log is large.
func DumpPluginLogOnFailure(t *testing.T, ctx context.Context, mark int64, what string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		window := ReadPluginLogSince(t, ctx, mark)
		if window == "" {
			t.Logf("--- net-dhcp plugin log since %s --- (empty: nothing was written, or the log "+
				"could not be read; the note above says which)", what)
			return
		}
		t.Logf("--- net-dhcp plugin log since %s ---\n%s", what, window)
	})
}
