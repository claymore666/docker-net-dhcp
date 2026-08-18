// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PluginBuildDirEnv overrides where BuiltPluginDir looks. Set it when a
// caller knows better than the search below; leave it unset in the
// lanes, whose build directories are the two the search already knows.
const PluginBuildDirEnv = "PLUGIN_BUILD_DIR"

// pluginBuildDirs are the directories, relative to the repo root, that
// the two lanes build the plugin into: `make plugin` -> plugin/ on
// every PR, `make plugin-cover` -> plugin-cover/ on release PRs only.
//
// This is THE place they are named (#583). A test that spelled one of
// them itself was born broken in the lane that builds the other, and
// stayed green for a whole release cycle because that lane runs once
// per release (#541, fixed narrowly in #582): the contract restated in
// N places is the shape this repo keeps finding rot in. scripts/
// check-build-dir-refs.sh keeps every other test file from naming them.
var pluginBuildDirs = []string{"plugin", "plugin-cover"}

// BuiltPluginDir returns the directory holding the plugin rootfs the
// running lane built, or an error naming everywhere it looked.
//
// There is deliberately no "skip if absent" path: a missing rootfs
// means the calling test did not run, and a test that quietly does not
// run is the failure mode the STATE_DIR contract exists to prevent.
// TestBuiltPluginDir_MissingIsAnErrorNotASkip pins that.
func BuiltPluginDir() (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	candidates := pluginBuildDirs
	if override := os.Getenv(PluginBuildDirEnv); override != "" {
		candidates = []string{override}
	}
	tried := make([]string, 0, len(candidates))
	for _, name := range candidates {
		dir := name
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, name)
		}
		if _, err := os.Stat(filepath.Join(dir, "rootfs")); err == nil {
			return dir, nil
		}
		tried = append(tried, dir)
	}
	return "", fmt.Errorf("no built plugin rootfs in any of %s\n"+
		"Tests that package a throwaway plugin use the rootfs the runner already built. "+
		"Run `make plugin` (or the usual `make integration-local`, which does) first; "+
		"the coverage lane builds `make plugin-cover` instead. Set %s to point elsewhere.",
		strings.Join(tried, ", "), PluginBuildDirEnv)
}

// RepoRoot walks up from the working directory to the module root.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory")
		}
		dir = parent
	}
}
