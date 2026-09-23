// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PluginBuildDirEnv overrides where BuiltPluginDir looks.
const PluginBuildDirEnv = "PLUGIN_BUILD_DIR"

// pluginBuildDirs are the lanes' build directories under the repo root: `make plugin` on every PR, `make plugin-cover`
// on release PRs. They are named only here (#583, #541); scripts/check-build-dir-refs.sh keeps other tests from naming them.
var pluginBuildDirs = []string{"plugin", "plugin-cover"}

// BuiltPluginDir returns the plugin rootfs directory the running lane built, or an error naming where it looked, never a skip (#583).
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
	return "", fmt.Errorf("no built plugin rootfs in any of %s — "+
		"tests that package a throwaway plugin use the rootfs the runner already built; "+
		"run `make plugin` (or the usual `make integration-local`, which does) first, "+
		"the coverage lane builds `make plugin-cover` instead, or set %s to point elsewhere",
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
