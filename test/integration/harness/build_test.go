// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestPluginBuildDirs_AreTheTwoLanes(t *testing.T) {
	want := []string{"plugin", "plugin-cover"}
	if strings.Join(pluginBuildDirs, ",") != strings.Join(want, ",") {
		t.Errorf("pluginBuildDirs = %v, want %v (make plugin / make plugin-cover)", pluginBuildDirs, want)
	}
}
