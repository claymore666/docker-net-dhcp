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

// AwaitRecoveryRebuildWindow waits on the window spanning the recycle, so every read is checked against the window's instance (#405).
func AwaitRecoveryRebuildWindow(w *CounterWindow, what string, cond func(*HealthResponse) bool) (*HealthResponse, bool) {
	w.t.Helper()
	return awaitRecoveryRebuild(w.t.Logf, what, func() {
		checkInstalledAwaitTimeout(w.t, w.ctx, w.cli)
	}, func(budget time.Duration) (*HealthResponse, bool) {
		return w.Await(budget, func(now, _ *HealthResponse) bool { return cond(now) })
	})
}

// AwaitRecoveryRebuildOn is the identity-blind wait for daemon-restart tests, whose window cannot span the restart (#376).
func AwaitRecoveryRebuildOn(t *testing.T, ctx context.Context, cli *docker.Client, what string,
	cond func(*HealthResponse) bool) (*HealthResponse, bool) {
	t.Helper()
	return awaitRecoveryRebuild(t.Logf, what, func() {
		checkInstalledAwaitTimeout(t, ctx, cli)
	}, func(budget time.Duration) (*HealthResponse, bool) {
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

// DumpPluginLogOnFailure prints the plugin log from mark when the test failed; teardown removes the plugin and its log (#933).
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

// checkInstalledAwaitTimeout reports, with Errorf so the recycle test keeps reporting, a wait bound the installed plugin contradicts (#376).
func checkInstalledAwaitTimeout(t *testing.T, ctx context.Context, cli *docker.Client) {
	t.Helper()
	p, _, err := cli.PluginInspectWithRaw(ctx, PluginRef)
	if err != nil {
		t.Errorf("PluginInspect, to read the AWAIT_TIMEOUT the recovery budget is derived from: %v", err)
		return
	}
	if msg := InstalledAwaitTimeoutDrift(p.Settings.Env); msg != "" {
		t.Errorf("%s", msg)
	}
}
