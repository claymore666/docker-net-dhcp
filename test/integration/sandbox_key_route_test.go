// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// The PR-1 measurement (#725): does the sandbox key Docker publishes
// suffice to enter a container's network namespace, in every mode and
// for a container whose init is not root?
//
// WHY THIS CANNOT BE ASKED OF THE UNIT SUITE. The key names a bind
// mount libnetwork makes on the host, reached through the read-only
// /var/run/docker mount in config.json. Whether that mount carries the
// daemon's per-sandbox netns mounts — as opposed to merely showing
// their directory entries, which #567 already proved it does — is a
// property of mount propagation between two namespaces on a live host.
// Nothing below a real daemon can answer it, and it is the property the
// whole route rests on.
//
// WHAT EACH CELL ASSERTS, AND WHY IT IS TWO THINGS
//
//  1. OUTSIDE EVIDENCE. `ip -4 addr show` INSIDE the container carries
//     the leased address. Not the plugin's report of it, not Docker's
//     inspect: the kernel's own view from inside the namespace the
//     plugin claims to have configured. If the plugin entered the wrong
//     namespace, or none, this is the assertion that says so.
//  2. WHICH ROUTE CARRIED IT. sandbox_key_entries must rise and
//     sandbox_pid_fallbacks must not. Assertion 1 alone is satisfied by
//     the PID fallback doing all the work, which is precisely the shape
//     that would make a green suite mean nothing here.
//
// WHY THE PAIR IS AIRTIGHT AND EITHER HALF ALONE IS NOT. "entries >= 1"
// alone is satisfied by some other attach in the window while this
// cell's attach fell back. "fallbacks == 0" alone is satisfied by a
// plugin that opened no namespace at all. Together, over one window:
// nothing in the window fell back, and assertion 1 says this cell's
// container was in fact configured — so this cell's attach went through
// the key. Nothing in this suite calls t.Parallel, so the window holds
// this cell's attach and, at worst, some cleanup; the argument does not
// depend on that, which is why it is written to survive parallelism.
//
// The counters are deltas over a window, because the suite shares one
// plugin instance and an absolute read would be arithmetic over every
// test that ran before this one (#405).
func sandboxKeyCell(t *testing.T, mode, netName, ctrName, user string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli,
		"sandbox_key_entries", "sandbox_key_entry_failures", "sandbox_pid_fallbacks")

	harness.CreateNetwork(t, ctx, netName, mode, nil)

	var id, ipv4 string
	if user == "" {
		id, ipv4, _ = harness.RunContainer(t, ctx, netName, ctrName)
	} else {
		id, ipv4, _ = harness.RunContainerUser(t, ctx, netName, ctrName, user)
	}
	// The pools differ per fixture, and asserting the wrong one would
	// fail a cell for a reason that has nothing to do with the route.
	if mode == "bridge" {
		harness.AssertBridgeIP(t, ipv4)
	} else {
		harness.AssertIP(t, ipv4)
	}

	// 1. Outside evidence, from inside the namespace.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show")
	if !strings.Contains(out, ipv4+"/") {
		t.Errorf("`ip -4 addr show` inside the container does not carry %s.\n%s\n"+
			"The address Docker reports is the plugin's word for it; this is the kernel's, "+
			"read from inside the namespace the plugin says it configured.", ipv4, out)
	}

	before, after := w.End()

	entries, ok1 := counterDelta(t, "sandbox_key_entries", before.SandboxKeyEntries, after.SandboxKeyEntries)
	fallbacks, ok2 := counterDelta(t, "sandbox_pid_fallbacks", before.SandboxPIDFallbacks, after.SandboxPIDFallbacks)
	failures, ok3 := counterDelta(t, "sandbox_key_entry_failures", before.SandboxKeyEntryFailures, after.SandboxKeyEntryFailures)
	if !ok1 || !ok2 || !ok3 {
		return
	}
	// Printed in the run whether or not the cell passes: the cell table
	// in the handover is read off these lines, and a table built only
	// from failures has no rows on a green run.
	t.Logf("CELL mode=%s user=%q: sandbox_key_entries +%d, sandbox_key_entry_failures +%d, sandbox_pid_fallbacks +%d",
		mode, user, entries, failures, fallbacks)

	if entries < 1 {
		t.Errorf("sandbox_key_entries rose by %d while a container was attached on %s: the key route "+
			"carried nothing, so \"no fallbacks\" below would be a statement about an empty set", entries, mode)
	}
	if fallbacks != 0 {
		t.Errorf("sandbox_pid_fallbacks rose by %d on %s: the sandbox key did NOT suffice here and the "+
			"/proc/<pid>/ns/net route carried the attach. This cell is the reason the host PID namespace "+
			"and CAP_SYS_PTRACE stay in config.json — record it in SECURITY.md, do not silence it", fallbacks, mode)
	}
	if failures != 0 {
		t.Errorf("sandbox_key_entry_failures rose by %d on %s: the key route was refused or timed out at "+
			"least once, even though something later succeeded", failures, mode)
	}
}

// counterDelta reads a delta and refuses to compute one from an absent
// counter. A plugin that does not publish the field is not a plugin
// reporting zero, and reading the absence as zero is the exact shape
// that would let a build without the key route pass every cell above.
func counterDelta(t *testing.T, name string, before, after *int32) (int32, bool) {
	t.Helper()
	if before == nil || after == nil {
		t.Errorf("%s is not published by this plugin, so it cannot be judged — and reading its "+
			"absence as zero is how a missing route would pass this test", name)
		return 0, false
	}
	return *after - *before, true
}

// The cells. Each is its own test so the run names which one failed
// rather than which combination did.

func TestSandboxKeyRoute_Macvlan(t *testing.T) {
	sandboxKeyCell(t, "macvlan", "dh-itest-skey-mv", "dh-itest-skey-mv-ctr", "")
}

func TestSandboxKeyRoute_Bridge(t *testing.T) {
	sandboxKeyCell(t, "bridge", "dh-itest-skey-br", "dh-itest-skey-br-ctr", "")
}

func TestSandboxKeyRoute_Ipvlan(t *testing.T) {
	sandboxKeyCell(t, "ipvlan", "dh-itest-skey-iv", "dh-itest-skey-iv-ctr", "")
}

// The non-root cell is #317's case: the kernel gates /proc/<pid>/ns/net
// on PTRACE_MODE_READ, so a container whose init runs as uid 65534 is
// the only cell in which the PID route needs CAP_SYS_PTRACE at all. If
// the key route carries this one, the netns half of that capability's
// justification is gone — the mount-namespace half is not, and
// SECURITY.md says so.
//
// BOUND, stated rather than implied: this is a non-root INIT UID, not a
// userns-remapped daemon. `dockerd --userns-remap` is a daemon-level
// setting this lane does not run, so nothing here measures it; what is
// measured is the uid mismatch that makes the ptrace check bite, which
// is the mechanism #317 was about.
func TestSandboxKeyRoute_NonRootContainer(t *testing.T) {
	sandboxKeyCell(t, "macvlan", "dh-itest-skey-nr", "dh-itest-skey-nr-ctr", "65534:65534")
}
