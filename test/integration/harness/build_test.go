// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltPluginDir_MissingIsAnErrorNotASkip is #583's acceptance
// check: point the accessor at a lane that does not exist and the
// answer must be an error naming where it looked — never a nil that a
// caller could turn into t.Skip. A test that quietly does not run is
// how a rootfs-dependent test stayed green for a release cycle (#541).
func TestBuiltPluginDir_MissingIsAnErrorNotASkip(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-lane")
	t.Setenv(PluginBuildDirEnv, missing)

	dir, err := BuiltPluginDir()
	if err == nil {
		t.Fatalf("BuiltPluginDir() = %q, nil — want an error for a lane that does not exist", dir)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the directory it tried: %v", err)
	}
	if !strings.Contains(err.Error(), PluginBuildDirEnv) {
		t.Errorf("error does not tell the reader how to point it elsewhere: %v", err)
	}
}

// TestBuiltPluginDir_FindsARootfsWhereverTheLaneBuiltIt: the override
// is honoured when the directory it names holds a rootfs, absolute or
// relative to the repo root — the one contract both lanes and every
// test share.
func TestBuiltPluginDir_FindsARootfsWhereverTheLaneBuiltIt(t *testing.T) {
	lane := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lane, "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PluginBuildDirEnv, lane)

	got, err := BuiltPluginDir()
	if err != nil {
		t.Fatalf("BuiltPluginDir() error: %v", err)
	}
	if got != lane {
		t.Errorf("BuiltPluginDir() = %q, want %q", got, lane)
	}
}

// TestPluginBuildDirs_AreTheTwoLanes pins the search list to what the
// Makefile actually produces, so a renamed target cannot silently leave
// one lane unsearched.
func TestPluginBuildDirs_AreTheTwoLanes(t *testing.T) {
	want := []string{"plugin", "plugin-cover"}
	if strings.Join(pluginBuildDirs, ",") != strings.Join(want, ",") {
		t.Errorf("pluginBuildDirs = %v, want %v (make plugin / make plugin-cover)", pluginBuildDirs, want)
	}
}
