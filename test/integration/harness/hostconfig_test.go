// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostConfig_EnablesInit pins the helper itself. Init must be a
// non-nil true: a nil *bool means "daemon default", which on every
// engine this suite runs against is init OFF — the exact state that
// cost the suite ~10s per container teardown (#367).
func TestHostConfig_EnablesInit(t *testing.T) {
	hc := HostConfig()
	if hc.Init == nil {
		t.Fatal("HostConfig().Init is nil — the daemon default is init off, which restores the 10s stop grace")
	}
	if !*hc.Init {
		t.Error("HostConfig().Init is false; test containers need an init PID 1 to exit on SIGTERM")
	}
	if hc.AutoRemove {
		t.Error("HostConfig().AutoRemove is true; the suite removes containers explicitly in t.Cleanup, and AutoRemove races that")
	}
}

// TestHostConfig_FreshPerCall guards the "mutate the returned struct"
// contract the doc comment offers. recovery_daemon_test.go adds a
// RestartPolicy to its copy; if the helper ever returned a shared
// value, that would leak an always-restart policy into every other
// container in the suite and the damage would show up somewhere else
// entirely.
func TestHostConfig_FreshPerCall(t *testing.T) {
	a, b := HostConfig(), HostConfig()
	if a == b {
		t.Fatal("HostConfig() returned the same pointer twice; callers mutate the result")
	}
	if a.Init == b.Init {
		t.Error("HostConfig() shares its Init *bool between calls; a caller flipping it would affect every container")
	}
}

// TestHostConfig_NoSlowStopOptOut holds the line the opt-out used to
// hold, from the other side.
//
// There used to be a HostConfigNoInit / RunContainerNoInit escape hatch
// that restored `docker stop`'s full 10-second grace, with a comment
// admitting the restart tests "only pass with a container that is slow
// to stop". That was true, and it was the problem: the slow stop was
// concealing two real defects — the lease reclaim never running (#402)
// and `docker restart` failing outright with `address already in use`
// (#408). Both were fixed; the opt-out is gone.
//
// It must not come back. A test that "needs" a slow stop is a test
// standing on top of a product race, and the next person to reach for
// one will be re-hiding whatever this suite would otherwise have found.
// Add the slow stop and this fails, which is the point.
func TestHostConfig_NoSlowStopOptOut(t *testing.T) {
	patterns := []string{
		filepath.Join("..", "*_test.go"),
		filepath.Join(".", "*.go"),
	}
	var files []string
	for _, p := range patterns {
		matched, err := filepath.Glob(p)
		if err != nil {
			t.Fatalf("glob %s: %v", p, err)
		}
		files = append(files, matched...)
	}
	if len(files) == 0 {
		t.Fatal("globbed no sources; this guard would pass vacuously")
	}

	// This file necessarily names the identifiers it forbids.
	const self = "hostconfig_test.go"

	for _, f := range files {
		if filepath.Base(f) == self {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, banned := range []string{"HostConfigNoInit", "RunContainerNoInit"} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s reintroduces %s. A slow container stop hides product races — "+
					"it hid #402 and #408 for months. If a test appears to need one, it is "+
					"standing on a race that should be fixed instead.",
					filepath.Base(f), banned)
			}
		}
	}
}

// TestHostConfig_NoBareLiteralsInSuite is the part that actually holds
// the line. The helper is only worth having if every creation site
// uses it, and the natural thing to write in a new test is
// `&container.HostConfig{}` — which compiles, passes, and silently
// hands back the 10-second teardown.
//
// Static rather than behavioural on purpose: catching this by timing a
// container stop would cost more wall clock than the bug does. It also
// lives here rather than in the suite package because the suite's
// TestMain requires root and a running plugin, and a check this cheap
// should not need either.
func TestHostConfig_NoBareLiteralsInSuite(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "*_test.go"))
	if err != nil {
		t.Fatalf("glob suite sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no ../*_test.go found; the guard would pass vacuously")
	}

	const bare = "&container.HostConfig{"
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, bare) {
				continue
			}
			t.Errorf("%s:%d constructs a HostConfig directly:\n\t%s\nUse harness.HostConfig() — a bare literal leaves Init unset, "+
				"so `sleep infinity` as PID 1 ignores SIGTERM and every teardown waits out docker stop's full 10s grace (#367). "+
				"Need extra fields? Mutate the returned struct, as recovery_daemon_test.go does for its RestartPolicy.",
				filepath.Base(f), i+1, strings.TrimSpace(line))
		}
	}
}
