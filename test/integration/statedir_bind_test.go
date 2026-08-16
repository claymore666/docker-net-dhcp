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
	"time"
)

// TestStateDirBindSource_MissingSourceContract pins the install-time
// contract documented in #494 / #499 and repeated at the top of the
// v1.5.0 release notes.
//
// Since #440 the manifest bind-mounts STATE_DIR from the host, and the
// Docker daemon does NOT create a missing bind source. Everything the
// docs then tell an operator — that the install leaves a *disabled*
// plugin rather than rolling back, that retrying the install answers
// "already exists" and never re-attempts the mount, that the recovery
// is mkdir + `docker plugin enable` — is a statement about daemon
// behaviour, not about our code. Until now it was verified only by a
// manual run pasted into a PR body, so a future engine release could
// change any of it and the docs would go quietly wrong. That is the
// exact failure mode this project keeps being burned by: prose that
// decays silently while nothing goes red.
//
// Point 3 below is the one that matters most. "The install rolled back"
// and "you now have a disabled plugin" are different worlds for an
// operator, and it is the second that makes the retry advice necessary.
//
// The suite's own plugin is installed once per run and the rest of the
// suite is using it, so this test builds a throwaway plugin of its own:
// its own name, its own bind source under a temporary path, never
// /var/lib/net-dhcp. It reuses the rootfs the runner already built, so
// it costs a directory copy rather than a second image build.
//
// The throwaway name is deliberately OUTSIDE the maintained namespaces
// that driverRegexp matches. A plugin named `.../claymore666/
// docker-net-dhcp:<tag>` treats every network on such a driver as its
// own on startup and runs recovery over them — which, with the suite's
// plugin live on the same daemon, would mean two instances
// re-DISCOVERing the same endpoints. The daemon contract under test
// does not care what the plugin is called; the blast radius does.
func TestStateDirBindSource_MissingSourceContract(t *testing.T) {
	// The in-plugin mount point, i.e. the mounts[] entry whose source
	// this test redirects. Must match config.json.
	const stateDest = "/var/lib/net-dhcp"

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	root := repoRoot(t)
	srcPlugin := filepath.Join(root, "plugin")
	if _, err := os.Stat(filepath.Join(srcPlugin, "rootfs")); err != nil {
		t.Fatalf("no built plugin rootfs at %s: %v\n"+
			"This test packages a throwaway plugin from the rootfs the runner already built. "+
			"Run `make plugin` (or the usual `make integration-local`, which does) first.",
			srcPlugin, err)
	}

	// Scratch lives under the repo, not /tmp: the rootfs copy is done
	// with hardlinks, which need the same filesystem.
	scratch, err := os.MkdirTemp(root, ".itest-statedir-")
	if err != nil {
		t.Fatalf("scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })

	// The plugin name is unique per run, and that is load-bearing
	// rather than tidiness.
	//
	// A name that was successfully enabled earlier in the SAME daemon
	// lifetime keeps a registered network-driver handler even after
	// `plugin disable` + `plugin rm`. Re-create that name and step 5
	// below stops answering "found but disabled" and instead dials the
	// dead socket of the previous incarnation — measured on 26.1.5, 4/4
	// deterministic with a fresh name and reproducible with a reused
	// one. CI would never have seen it (its daemon is new each run);
	// a second `make integration-local` on a developer's box would.
	//
	// The name also stays OUTSIDE the namespaces driverRegexp matches
	// — see the doc comment above.
	// Lowercased: os.MkdirTemp's random suffix is mixed-case and Docker
	// rejects a plugin reference that is not lowercase ("repository
	// name ... must be lowercase"), which would fail nearly every run.
	suffix := strings.ToLower(strings.TrimPrefix(filepath.Base(scratch), ".itest-statedir-"))
	pluginRef := "local/dh-itest-statedir-" + suffix + ":500"

	// The bind source the daemon will be asked for. Deliberately NOT
	// created yet — its absence is the whole scenario.
	bindSource := filepath.Join(scratch, "state")
	pkgDir := filepath.Join(scratch, "plugin")

	copyPluginPackage(ctx, t, srcPlugin, pkgDir)
	rewriteStateDirSource(t, filepath.Join(pkgDir, "config.json"), stateDest, bindSource)

	t.Cleanup(func() {
		// Fresh ctx: cleanup must still run when the test ctx expired.
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		_, _ = dockerRun(cctx, t, "plugin", "disable", "-f", pluginRef)
		if out, err := dockerRun(cctx, t, "plugin", "rm", "-f", pluginRef); err != nil {
			t.Logf("cleanup: plugin rm %s: %v: %s", pluginRef, err, out)
		}
	})

	// 1. Package it with the bind source absent. `create` is the local
	//    equivalent of the pull half of `docker plugin install`: it
	//    stages the plugin without starting it, so it must succeed even
	//    though the mount cannot be satisfied.
	if out, err := dockerRun(ctx, t, "plugin", "create", pluginRef, pkgDir); err != nil {
		if strings.Contains(out, "already exists") {
			t.Fatalf("plugin create %s hit a content-store collision: %v: %s\n"+
				"The rootfs marker written by copyPluginPackage is meant to make this "+
				"layer unique; if it is failing anyway, the digest is being computed over "+
				"something the marker does not change.", pluginRef, err, out)
		}
		t.Fatalf("plugin create %s: %v: %s", pluginRef, err, out)
	}

	// 2. Enabling is where the mount happens, and it must fail — naming
	//    the path the operator has to create. An error that does not
	//    name it is a usability regression: the whole recovery
	//    instruction is "mkdir the path from the error".
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

	// 3. THE claim: not rolled back, still installed, disabled.
	if enabled, ok := pluginEnabled(ctx, t, pluginRef); !ok {
		t.Fatalf("plugin %s is not listed after a failed enable; the daemon rolled the "+
			"install back, so the documented `docker plugin enable` recovery no longer "+
			"applies and the release notes are wrong", pluginRef)
	} else if enabled {
		t.Fatalf("plugin %s reports Enabled=true after enable failed", pluginRef)
	}

	// 4. A second attempt does not silently fix it. `create` answers
	//    "already exists" (as `install` does), and the plugin stays
	//    disabled — the mount is never re-attempted, which is why the
	//    docs say to fix the path and enable rather than to reinstall.
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

	// 5. Using it in this state is refused, and the refusal says
	//    "disabled" rather than "not found" — the difference between an
	//    operator re-installing (which will not help) and enabling.
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

	// 6. Recovery: create the directory, enable, done — and nothing
	//    already in that directory is touched. The marker stands in for
	//    the tombstones, per-network options and audit ledger a real
	//    operator's state dir holds; #440's entire point is that they
	//    survive, so an enable that wiped them would be silent data
	//    loss.
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
}

// repoRoot walks up from the test's working directory to the module
// root. `go test` runs the binary in its package directory, but the
// suite is also run through several make targets and from shard jobs,
// so deriving the root rather than assuming "../.." keeps it honest.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above the test working directory")
		}
		dir = parent
	}
}

// copyPluginPackage clones the built plugin package into dst. Hardlinks
// first (`cp -al`): the rootfs is tens of thousands of files and this
// test only ever rewrites config.json, which is written fresh rather
// than edited in place, so nothing is shared back into the original.
// A real copy is the fallback for the case where the two paths turn out
// to be on different filesystems.
func copyPluginPackage(ctx context.Context, t *testing.T, src, dst string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "cp", "-al", src, dst).CombinedOutput(); err != nil {
		t.Logf("hardlink copy of %s failed (%v: %s); falling back to a full copy",
			src, err, strings.TrimSpace(string(out)))
		if out, err := exec.CommandContext(ctx, "cp", "-a", src, dst).CombinedOutput(); err != nil {
			t.Fatalf("copy plugin package %s -> %s: %v: %s", src, dst, err, out)
		}
	}
	// Make the rootfs layer unique to this run.
	//
	// `docker plugin create` digests the rootfs and stores it as
	// content. The runner built this same tree minutes earlier and
	// created the suite's own plugin from it, so a byte-identical copy
	// hashes to a blob the daemon already holds and `create` fails with
	// `content sha256:...: already exists` (seen on 26.1.5). The marker
	// is one file with the scratch directory's random name in it: it
	// changes the digest, is inert inside the plugin, and is unique per
	// run so a repeat on the same daemon does not collide either.
	//
	// It has to be a NEW file rather than an edit — the copy above is
	// hardlinked, so writing to any existing path would write into the
	// runner's real plugin package.
	marker := filepath.Join(dst, "rootfs", ".dh-itest-statedir-500")
	if err := os.WriteFile(marker, []byte(filepath.Base(filepath.Dir(dst))+"\n"), 0o644); err != nil {
		t.Fatalf("write rootfs marker: %v", err)
	}

	// config.json is rewritten below; break the hardlink so the
	// repo's own config.json can never be modified through it.
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

// rewriteStateDirSource points the mounts[] entry whose destination is
// dest at a new host source, leaving the rest of the manifest exactly
// as shipped. It fails loudly when no such entry exists: that would
// mean the bind mount this whole test is about has been removed from
// config.json, and silently testing nothing is the outcome to avoid.
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

// pluginEnabled reports whether ref is currently listed by the daemon
// and, if so, whether it is enabled. Read through `docker plugin
// inspect` rather than the Go client so the test observes exactly what
// the docs tell an operator to run.
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

// dockerRun shells out to the docker CLI and returns its combined
// output. The CLI, not the API client, because every claim under test
// is quoted from documentation that tells operators to run these exact
// commands — including the error text they are told to read.
func dockerRun(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	t.Logf("docker %s -> err=%v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	return string(out), err
}
