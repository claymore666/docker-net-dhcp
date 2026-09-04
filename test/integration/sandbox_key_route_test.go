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

// The PR-1 measurement (#725), and its ANSWER: **no.** Entering a
// container's network namespace through the sandbox key alone does NOT
// suffice on this engine, and these cells are the record of why.
//
// WHAT WAS MEASURED, 2026-09-05, Integration run 33927195482.
// libnetwork creates each entry under /var/run/docker/netns/ as an
// ordinary empty file and then bind-mounts the namespace over it. The
// plugin's /var/run/docker mount carries the DIRECTORY — #567 proved
// that, and sandbox_netns_visible still depends on it — but that mount
// is a bind taken when the PLUGIN PROCESS STARTS, and a bind mount is a
// SNAPSHOT, not a subscription. The daemon's later per-sandbox mounts
// never reach it, so the plugin opens the empty file underneath.
//
// A Join is always for a sandbox younger than the plugin, so that is
// every attach — these four cells. The one case where the sandbox is
// OLDER is recovery after a plugin restart, and there the key route
// works: see TestRecovery_PluginDisableEnable_PreservesEndpoint, which
// is the positive half of this measurement and the control that makes
// "snapshot" the explanation rather than "broken".
//
// The first version of this change did exactly that and counted it as
// a key-route entry, because a successful open looks like a successful
// entry. Every cell here went green, every container had its address,
// and twenty-six unrelated tests went red across the suite with
// "failed to set into network namespace N ... invalid argument": the
// address is applied by CreateEndpoint's one-shot client before Join,
// so the container looks right while the PERSISTENT client — renewals,
// resolv.conf, MTU — never starts. The counter has since been moved
// behind an NS_GET_NSTYPE check, so it counts namespaces entered
// rather than files opened.
//
// WHAT THESE CELLS ASSERT NOW. Two things, and the second is a pinned
// defect rather than a goal:
//
//  1. OUTSIDE EVIDENCE, unchanged and unconditional. `ip -4 addr show`
//     INSIDE the container carries the leased address — the kernel's
//     view of the namespace, not the plugin's report of it.
//  2. THE ROUTE, as it actually is. The key route is refused
//     (sandbox_key_entry_failures rises), the PID route carries the
//     attach (sandbox_pid_fallbacks rises), and nothing enters through
//     the key (sandbox_key_entries stays flat). Exactly one route per
//     attach, so neither counter can be satisfied by an empty domain.
//
// **If assertion 2 fails because sandbox_key_entries rose, that is GOOD
// NEWS and not a regression.** It means the daemon's sandbox mounts now
// reach the plugin — a newer engine, or the /var/run/docker mount given
// slave propagation in config.json, which is the named follow-up and
// which the recovery cell shows would be sufficient if the mounts
// arrived. The response is to update these cells, drop the fallback,
// and rewrite SECURITY.md's paragraph; not to make the assertion
// softer.
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
			if mode == "bridge" {
				// The bridge fixture runs its own dnsmasq on its own
				// subnet; without this a bridge-cell failure shows the
				// macvlan server's log, which never saw the request.
				fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			}
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
	// Printed whether or not the cell passes: the cell table in the
	// handover is read off these lines, and a table built only from
	// failures has no rows on a green run.
	t.Logf("CELL mode=%s user=%q: sandbox_key_entries +%d, sandbox_key_entry_failures +%d, sandbox_pid_fallbacks +%d",
		mode, user, entries, failures, fallbacks)

	// The domain: exactly one route carried this attach. Without it,
	// every assertion below is satisfied by a plugin that entered no
	// namespace at all.
	if entries+fallbacks != 1 {
		t.Errorf("sandbox_key_entries +%d and sandbox_pid_fallbacks +%d sum to %d across one "+
			"container attach on %s, want exactly 1: one attach takes one route, and a sum of zero "+
			"means the assertions below are about an empty set", entries, fallbacks, entries+fallbacks, mode)
	}

	if entries != 0 {
		t.Errorf("sandbox_key_entries rose by %d on %s. This is the LIMITATION LIFTING, not a "+
			"regression: the daemon's sandbox netns mounts now reach the plugin, so the key route "+
			"works. Update this cell to assert the key route, drop the PID fallback in "+
			"pkg/plugin/dhcp_manager.go, and rewrite SECURITY.md's \"What the sandbox-key route "+
			"changed\" paragraph — do not soften this assertion", entries, mode)
	}
	if failures != 1 {
		t.Errorf("sandbox_key_entry_failures rose by %d on %s, want exactly 1: the key route is "+
			"refused once per attach and must not be retried, because a permanent refusal polled to "+
			"the deadline is attach budget the PID route then does not have (#401)", failures, mode)
	}
	if fallbacks != 1 {
		t.Errorf("sandbox_pid_fallbacks rose by %d on %s, want exactly 1. The key route cannot carry "+
			"this attach on this engine, so the /proc/<pid>/ns/net route must — and it is why the "+
			"manifest still asks for the host PID namespace and CAP_SYS_PTRACE", fallbacks, mode)
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
