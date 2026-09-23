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

// A nil Init means the daemon default, init off on every engine this suite runs, which cost ~10 s per teardown (#367).
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

func TestHostConfig_FreshPerCall(t *testing.T) {
	a, b := HostConfig(), HostConfig()
	if a == b {
		t.Fatal("HostConfig() returned the same pointer twice; callers mutate the result")
	}
	if a.Init == b.Init {
		t.Error("HostConfig() shares its Init *bool between calls; a caller flipping it would affect every container")
	}
}

// A slow-stop opt-out hid the lease reclaim never running (#402) and `docker restart` failing with `address already in use` (#408).
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
