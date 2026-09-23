// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"time"
)

// Since #440 the manifest bind-mounts STATE_DIR from the host and the daemon does not create a missing bind source;
// the documented install contract (#494, #499) is daemon behaviour: the install leaves a disabled plugin, a retry
// answers "already exists", and recovery is mkdir plus `docker plugin enable`. The throwaway plugin's name stays
// outside driverRegexp's namespaces, or it would run recovery over the suite plugin's networks. The rootfs comes from
// harness.BuiltPluginDir, the one place build directories are named (#541, #582, #583).

// TestStateDirBindSource_MissingSourceContract checks the daemon's contract for a plugin whose STATE_DIR bind source is missing (#494).
func TestStateDirBindSource_MissingSourceContract(t *testing.T) {
	// The mounts[] destination this test redirects; must match config.json.
	const stateDest = "/var/lib/net-dhcp"

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	root := repoRoot(t)
	srcPlugin, err := harness.BuiltPluginDir()
	if err != nil {
		t.Fatal(err)
	}

	// Under the repo, not /tmp: the rootfs copy uses hardlinks, which need one filesystem.
	scratch, err := os.MkdirTemp(root, ".itest-statedir-")
	if err != nil {
		t.Fatalf("scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })

	// A name enabled earlier in the same daemon lifetime keeps a registered driver handler after disable and rm, so a
	// reused name dials the dead socket, measured on 26.1.5, 4/4 (#500). Docker rejects a plugin reference that is not
	// lowercase, and os.MkdirTemp's suffix is mixed-case.
	suffix := strings.ToLower(strings.TrimPrefix(filepath.Base(scratch), ".itest-statedir-"))
	pluginRef := "local/dh-itest-statedir-" + suffix + ":500"

	// Not created yet: its absence is the scenario.
	bindSource := filepath.Join(scratch, "state")
	pkgDir := filepath.Join(scratch, "plugin")

	copyPluginPackage(ctx, t, srcPlugin, pkgDir)
	rewriteStateDirSource(t, filepath.Join(pkgDir, "config.json"), stateDest, bindSource)

	t.Cleanup(func() {
		// Cleanup must still run when the test ctx has expired.
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		_, _ = dockerRun(cctx, t, "plugin", "disable", "-f", pluginRef)
		if out, err := dockerRun(cctx, t, "plugin", "rm", "-f", pluginRef); err != nil {
			t.Logf("cleanup: plugin rm %s: %v: %s", pluginRef, err, out)
		}
	})

	// `create` stages the plugin without starting it, so it succeeds with the mount unsatisfied.
	if out, err := dockerRun(ctx, t, "plugin", "create", pluginRef, pkgDir); err != nil {
		if strings.Contains(out, "already exists") {
			t.Fatalf("plugin create %s hit a content-store collision: %v: %s\n"+
				"The rootfs marker written by copyPluginPackage is meant to make this "+
				"layer unique; if it is failing anyway, the digest is being computed over "+
				"something the marker does not change.", pluginRef, err, out)
		}
		t.Fatalf("plugin create %s: %v: %s", pluginRef, err, out)
	}

	// Enabling mounts, fails, and must name the path the operator has to create.
	out, err := dockerRun(ctx, t, "plugin", "enable", pluginRef)
	if err == nil {
		t.Fatalf("plugin enable succeeded with bind source %s absent; "+
			"the daemon now creates missing bind sources, and every doc that "+
			"tells operators to mkdir it first is wrong: %s", bindSource, out)
	}
	if !strings.Contains(out, bindSource) {
		t.Errorf("enable failed but the error does not name the missing bind source %s.\n"+
			"docs/ tells operators to create the path from this message. Got: %s",
			bindSource, out)
	}

	// Not rolled back: still installed, disabled.
	if enabled, ok := pluginEnabled(ctx, t, pluginRef); !ok {
		t.Fatalf("plugin %s is not listed after a failed enable; the daemon rolled the "+
			"install back, so the documented `docker plugin enable` recovery no longer "+
			"applies and the release notes are wrong", pluginRef)
	} else if enabled {
		t.Fatalf("plugin %s reports Enabled=true after enable failed", pluginRef)
	}

	// A retry answers "already exists" and never re-attempts the mount, so the docs say to fix the path and enable.
	out, err = dockerRun(ctx, t, "plugin", "create", pluginRef, pkgDir)
	if err == nil {
		t.Errorf("a second `plugin create` succeeded; the docs say a retry answers "+
			"'already exists': %s", out)
	} else if !strings.Contains(strings.ToLower(out), "already exist") {
		t.Errorf("a second `plugin create` failed with something other than "+
			"'already exists': %s", out)
	}
	if enabled, ok := pluginEnabled(ctx, t, pluginRef); !ok || enabled {
		t.Errorf("after the retry, plugin listed=%v enabled=%v; want listed and disabled", ok, enabled)
	}

	// The refusal says "disabled", not "not found", so the operator enables and does not reinstall.
	netName := "dh-itest-statedir-disabled"
	out, err = dockerRun(ctx, t, "network", "create", "-d", pluginRef, netName)
	if err == nil {
		_, _ = dockerRun(ctx, t, "network", "rm", netName)
		t.Fatalf("network create against a disabled plugin succeeded: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "disabled") {
		t.Errorf("network create against a disabled plugin should say it is disabled, "+
			"not merely missing. Got: %s", out)
	}

	// The marker stands in for the state a real STATE_DIR holds, which must survive (#440). It is seeded 0644, the state
	// #804 found on a production host, so this is the end-to-end observer for the startup mode sweep.
	if err := os.MkdirAll(bindSource, 0o755); err != nil {
		t.Fatalf("mkdir bind source: %v", err)
	}
	marker := filepath.Join(bindSource, "pre-existing-state.json")
	if err := os.WriteFile(marker, []byte(`{"kept":true}`), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if out, err := dockerRun(ctx, t, "plugin", "enable", pluginRef); err != nil {
		t.Fatalf("plugin enable after creating %s still failed — the documented recovery "+
			"does not work: %v: %s", bindSource, err, out)
	}
	if enabled, ok := pluginEnabled(ctx, t, pluginRef); !ok || !enabled {
		t.Fatalf("after recovery, plugin listed=%v enabled=%v; want listed and enabled", ok, enabled)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != `{"kept":true}` {
		t.Errorf("pre-existing state in the bind source did not survive enable (%q, %v); "+
			"the recovery is documented as lossless", string(b), err)
	}

	// The sweep runs inside NewPlugin before the socket appears; the poll keeps the assertion off that ordering (#804).
	mode := os.FileMode(0)
	for deadline := time.Now().Add(15 * time.Second); ; {
		fi, err := os.Stat(marker)
		if err != nil {
			t.Fatalf("stat %s after enable: %v", marker, err)
		}
		mode = fi.Mode().Perm()
		if mode == 0o600 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if mode != 0o600 {
		t.Errorf("%s was seeded 0644 and is %#o after a plugin started over it; the startup "+
			"sweep did not tighten a file an older plugin left behind (#804)", marker, mode)
	}
}

// repoRoot is harness.RepoRoot with the test's failure semantics.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := harness.RepoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return root
}

// copyPluginPackage hardlinks the built plugin package into dst, falling back to a real copy across filesystems.
func copyPluginPackage(ctx context.Context, t *testing.T, src, dst string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "cp", "-al", src, dst).CombinedOutput(); err != nil {
		t.Logf("hardlink copy of %s failed (%v: %s); falling back to a full copy",
			src, err, strings.TrimSpace(string(out)))
		if out, err := exec.CommandContext(ctx, "cp", "-a", src, dst).CombinedOutput(); err != nil {
			t.Fatalf("copy plugin package %s -> %s: %v: %s", src, dst, err, out)
		}
	}
	// `docker plugin create` stores the rootfs by digest, so a byte-identical copy fails with `content sha256:...:
	// already exists` (seen on 26.1.5); a new file, since the copy is hardlinked into the runner's real package (#500).
	marker := filepath.Join(dst, "rootfs", ".dh-itest-statedir-500")
	if err := os.WriteFile(marker, []byte(filepath.Base(filepath.Dir(dst))+"\n"), 0o644); err != nil {
		t.Fatalf("write rootfs marker: %v", err)
	}

	// Breaks the hardlink so the repo's own config.json cannot be modified through it.
	cfg := filepath.Join(dst, "config.json")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read %s: %v", cfg, err)
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatalf("unlink %s: %v", cfg, err)
	}
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", cfg, err)
	}
}

// rewriteStateDirSource points the mounts[] entry for dest at a new host source and fails when no such entry exists.
func rewriteStateDirSource(t *testing.T, cfgPath, dest, newSource string) {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read %s: %v", cfgPath, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", cfgPath, err)
	}
	mounts, _ := cfg["mounts"].([]any)
	found := false
	for _, m := range mounts {
		mm, ok := m.(map[string]any)
		if !ok || mm["destination"] != dest {
			continue
		}
		mm["source"] = newSource
		found = true
	}
	if !found {
		t.Fatalf("config.json has no bind mount with destination %s — the STATE_DIR bind "+
			"this test exercises is gone from the manifest", dest)
	}
	out, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", cfgPath, err)
	}
}

// pluginEnabled reports, through `docker plugin inspect` as the docs tell operators, whether ref is listed and enabled.
func pluginEnabled(ctx context.Context, t *testing.T, ref string) (enabled, listed bool) {
	t.Helper()
	out, err := dockerRun(ctx, t, "plugin", "inspect", "--format", "{{.Enabled}}", ref)
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(out) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		t.Fatalf("unexpected `plugin inspect --format {{.Enabled}}` output for %s: %q", ref, out)
		return false, false
	}
}

// dockerRun runs the docker CLI the docs quote and returns its combined output.
func dockerRun(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	t.Logf("docker %s -> err=%v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	return string(out), err
}
