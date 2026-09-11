// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// The route an attach takes into a container's network namespace is a
// property of the HOST, not of this engine and not of this plugin.
// These four cells are keyed on it.
//
// WHAT DECIDES IT. libnetwork creates each entry under
// /var/run/docker/netns/ as an ordinary empty file and then bind-mounts
// the namespace over it. The plugin's /var/run/docker mount is a bind
// taken when the PLUGIN PROCESS starts, and a bind is a snapshot and
// not a subscription, so whether the daemon's later per-sandbox mount
// reaches the plugin depends on the propagation of the mount the daemon
// publishes on. The plugin publishes that answer as
// sandbox_netns_propagation: 1 the mount is linked and a later mount
// arrives, 0 it is private and the plugin opens the placeholder file
// underneath.
//
// MEASURED, both branches:
//
//   - 1: production 2026-09-11 (systemd host, v2.0.0, read-only probe),
//     and the hosted runner in Integration run 34598318503
//     (ubuntu-latest, Engine 28.0.4), all four cells. The key route
//     carries the attach.
//   - 0: this suite's own pool, Integration run 34601503020, all four
//     cells. The container PID route carries the attach, which is why
//     the manifest still asks for the host PID namespace and
//     CAP_SYS_PTRACE.
//
// WHAT THESE CELLS ASSERT. Three things:
//
//  1. OUTSIDE EVIDENCE, unconditional and identical on both branches.
//     `ip -4 addr show` INSIDE the container carries the leased
//     address. That is the kernel's view of the namespace, not the
//     plugin's report of it.
//  2. EXACTLY ONE ROUTE per attach, on both branches, so no assertion
//     below can be satisfied by an attach that entered no namespace at
//     all.
//  3. THE ROUTE THIS HOST'S PROPAGATION PREDICTS. On 1: entries rise,
//     fallbacks and refusals stay flat. On 0: the refusal is counted,
//     its arm is sandbox_key_not_a_namespace, and the PID route carries
//     the attach. Each branch asserts a counter the other cannot move,
//     so a cell cannot pass on the wrong host for the wrong reason.
//
// -1 FAILS HERE, and is never a skip. It is the gauge reporting that
// the mount table could not be read, so the cell cannot know which
// branch to assert. That is a broken instrument and not an absent host;
// a skip would empty the domain of every assertion below while leaving
// the run green.
//
// A NOTE ON WHAT THE KEY ROUTE DOES NOT BUY, even on branch 1. The
// address a container has is applied by CreateEndpoint's one-shot
// client before Join, so a Join that opens the placeholder file and
// calls it a namespace leaves a container that LOOKS right while the
// PERSISTENT client — renewals, resolv.conf, MTU — never starts. An
// early version of this change did exactly that: every cell went green
// and twenty-six unrelated tests went red with "failed to set into
// network namespace N ... invalid argument". The counter has since sat
// behind an NS_GET_NSTYPE check, so it counts namespaces entered rather
// than files opened.
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
		"sandbox_key_entries", "sandbox_key_entry_failures", "sandbox_pid_fallbacks",
		"sandbox_key_absent", "sandbox_key_not_permitted", "sandbox_key_not_a_namespace",
		"sandbox_key_wrong_ns_type", "sandbox_key_unavailable")

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

	// The attach runs in a goroutine the Join response does not wait
	// for, so a running container is not yet an attach the counters have
	// seen. Wait for the route to be taken, and FAIL at the budget: a
	// window closed early reads as "no route was taken", which is the
	// one answer this cell must never produce quietly.
	if _, moved := w.Await(attachObservationBudget, func(now, before *harness.HealthResponse) bool {
		e, ok1 := delta(now.SandboxKeyEntries, before.SandboxKeyEntries)
		f, ok2 := delta(now.SandboxPIDFallbacks, before.SandboxPIDFallbacks)
		return ok1 && ok2 && e+f >= 1
	}); !moved {
		t.Errorf("neither sandbox_key_entries nor sandbox_pid_fallbacks moved within %s of the "+
			"container on %s holding %s. The attach is asynchronous, so this is either an attach "+
			"that never completed or a plugin that entered no namespace; it is not a host this "+
			"cell has nothing to say about", attachObservationBudget, mode, ipv4)
	}

	before, after := w.End()

	entries, ok1 := counterDelta(t, "sandbox_key_entries", before.SandboxKeyEntries, after.SandboxKeyEntries)
	fallbacks, ok2 := counterDelta(t, "sandbox_pid_fallbacks", before.SandboxPIDFallbacks, after.SandboxPIDFallbacks)
	failures, ok3 := counterDelta(t, "sandbox_key_entry_failures", before.SandboxKeyEntryFailures, after.SandboxKeyEntryFailures)
	notPermitted, ok4 := counterDelta(t, "sandbox_key_not_permitted", before.SandboxKeyNotPermitted, after.SandboxKeyNotPermitted)
	notANamespace, ok5 := counterDelta(t, "sandbox_key_not_a_namespace", before.SandboxKeyNotANamespace, after.SandboxKeyNotANamespace)
	wrongType, ok6 := counterDelta(t, "sandbox_key_wrong_ns_type", before.SandboxKeyWrongNSType, after.SandboxKeyWrongNSType)
	unavailable, ok7 := counterDelta(t, "sandbox_key_unavailable", before.SandboxKeyUnavailable, after.SandboxKeyUnavailable)
	absent, ok8 := counterDelta(t, "sandbox_key_absent", before.SandboxKeyAbsent, after.SandboxKeyAbsent)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 {
		return
	}
	// Printed whether or not the cell passes: the cell table in the
	// handover is read off these lines, and a table built only from
	// failures has no rows on a green run.
	t.Logf("CELL mode=%s user=%q: sandbox_key_entries +%d, sandbox_key_entry_failures +%d, sandbox_pid_fallbacks +%d",
		mode, user, entries, failures, fallbacks)
	t.Logf("CELL-ARM mode=%s user=%q: sandbox_key_absent +%d, sandbox_key_not_permitted +%d, "+
		"sandbox_key_not_a_namespace +%d, sandbox_key_wrong_ns_type +%d, sandbox_key_unavailable +%d",
		mode, user, absent, notPermitted, notANamespace, wrongType, unavailable)

	branch, okBranch := propagationBranch(t, before.SandboxNetnsPropagation, after.SandboxNetnsPropagation)

	// THE HOST, printed beside the route it took, and the branch named
	// rather than left to be inferred from the numbers. Which route is
	// available is a property of the mount the daemon publishes sandbox
	// keys on, not of this plugin. Without these numbers the assertions
	// below read as a claim about the engine when they are a claim about
	// one mount (#417).
	t.Logf("CELL-HOST mode=%s user=%q: branch=%s, sandbox_netns_propagation=%s, "+
		"sandbox_netns_init_mounts=%s, sandbox_netns_visible=%s",
		mode, user, branchName(branch, okBranch), gaugeString(after.SandboxNetnsPropagation),
		gaugeString(after.SandboxNetnsInitMounts), gaugeString(after.SandboxNetnsVisible))

	// The domain, on both branches: exactly one route carried this
	// attach. Without it, every assertion below is satisfied by a plugin
	// that entered no namespace at all.
	if entries+fallbacks != 1 {
		t.Errorf("sandbox_key_entries +%d and sandbox_pid_fallbacks +%d sum to %d across one "+
			"container attach on %s, want exactly 1: one attach takes one route, and a sum of zero "+
			"means the assertions below are about an empty set", entries, fallbacks, entries+fallbacks, mode)
	}

	if !okBranch {
		return
	}

	if branch == propagationLinked {
		// The linked branch. entries is the counter the private branch
		// cannot move, so this is not an assertion the other host also
		// satisfies.
		if entries != 1 {
			t.Errorf("sandbox_key_entries rose by %d on %s, want exactly 1. "+
				"sandbox_netns_propagation=1 says the daemon's later sandbox mount reaches this "+
				"plugin, so the key the Join request carries must resolve to the namespace and "+
				"carry the attach. A host that answers 1 and still falls back is the gauge and the "+
				"route disagreeing, which is the finding", entries, mode)
		}
		if fallbacks != 0 {
			t.Errorf("sandbox_pid_fallbacks rose by %d on %s with sandbox_netns_propagation=1: the "+
				"attach took /proc/<pid>/ns/net on a host whose mount propagation says the key "+
				"route was available, so the netns half of pidhost and CAP_SYS_PTRACE is being "+
				"used where SECURITY.md says it is not needed", fallbacks, mode)
		}
		if failures != 0 {
			t.Errorf("sandbox_key_entry_failures rose by %d on %s with sandbox_netns_propagation=1, "+
				"want 0: a refused key on a linked mount is a refusal SECURITY.md does not "+
				"describe, and the arm printed above says which", failures, mode)
		}
		if notANamespace != 0 {
			t.Errorf("sandbox_key_not_a_namespace rose by %d on %s with "+
				"sandbox_netns_propagation=1: the plugin opened the placeholder file libnetwork "+
				"leaves under the mount, which is the private-propagation symptom on a host the "+
				"gauge calls linked", notANamespace, mode)
		}
	} else {
		// The private branch: the pool's own host, and the one the
		// manifest's PID-namespace grant exists for. fallbacks is the
		// counter the linked branch cannot move.
		if entries != 0 {
			t.Errorf("sandbox_key_entries rose by %d on %s with sandbox_netns_propagation=0: the "+
				"plugin entered a namespace through a key whose mount cannot have reached it, so "+
				"either the gauge is wrong about this host or the entry was not the sandbox's "+
				"namespace", entries, mode)
		}
		if failures != 1 {
			t.Errorf("sandbox_key_entry_failures rose by %d on %s, want exactly 1: the key route is "+
				"refused once per attach and must not be retried, because a permanent refusal polled to "+
				"the deadline is attach budget the PID route then does not have (#401)", failures, mode)
		}
		if fallbacks != 1 {
			t.Errorf("sandbox_pid_fallbacks rose by %d on %s, want exactly 1. The key route cannot carry "+
				"an attach on a private mount, so the /proc/<pid>/ns/net route must — and it is why the "+
				"manifest still asks for the host PID namespace and CAP_SYS_PTRACE", fallbacks, mode)
		}
		// WHICH REFUSAL, and this is the assertion SECURITY.md's causal
		// sentence rests on rather than the aggregate above.
		//
		// "The key route was refused" is compatible with two causes that
		// produce identical counts and want opposite remedies: the bind
		// snapshot (this plugin opens the placeholder file libnetwork
		// left under the mount — sandbox_key_not_a_namespace) and a
		// daemon started with a non-default --exec-root, which
		// publishes keys in a directory this plugin declines outright
		// (sandbox_key_not_permitted). Until the arms were published,
		// every cell in this file was equally consistent with the
		// second, and SECURITY.md asserted the first.
		if notANamespace != 1 {
			t.Errorf("sandbox_key_not_a_namespace rose by %d on %s, want exactly 1. This is the arm "+
				"SECURITY.md names: the entry opened and was NOT a namespace, i.e. the daemon's later "+
				"bind mount never reached this plugin's mount namespace. Without this the refusal "+
				"counted above is equally consistent with a key shape this plugin simply refuses, "+
				"which is a different finding and a different fix", notANamespace, mode)
		}
	}

	// Neither branch expects these, and each names a different host
	// misconfiguration rather than a route.
	if notPermitted != 0 {
		t.Errorf("sandbox_key_not_permitted rose by %d on %s. The daemon is publishing sandbox keys "+
			"outside /var/run/docker/netns and /run/docker/netns — a non-default --exec-root does "+
			"this. The key route is then untested on this host rather than refused for the "+
			"documented reason, and SECURITY.md's paragraph does not describe what happened here",
			notPermitted, mode)
	}
	if wrongType != 0 {
		t.Errorf("sandbox_key_wrong_ns_type rose by %d on %s: the entry was a namespace of another "+
			"type, which no measured host has produced", wrongType, mode)
	}
	if unavailable != 0 {
		t.Errorf("sandbox_key_unavailable rose by %d on %s: the refusal was none of the four named "+
			"arms, so the cause is one nothing in the tree has named", unavailable, mode)
	}
	// An attach always carries a key: libnetwork puts it in the Join
	// request. A rise here means it did not, and then the refusal
	// measured above is about the absence of the input rather than
	// about the mount propagation SECURITY.md argues from.
	if absent != 0 {
		t.Errorf("sandbox_key_absent rose by %d on %s: this attach reached the key route with no key "+
			"at all, so the arm below is not measuring what the daemon published", absent, mode)
	}
	// The arms are exhaustive by construction (countSandboxKeyRefusal).
	// Asserting it here is what makes the five deltas above an account
	// of the aggregate rather than four numbers beside it.
	if arms := absent + notPermitted + notANamespace + wrongType + unavailable; arms != failures {
		t.Errorf("the refusal arms sum to %d and sandbox_key_entry_failures rose by %d on %s: a "+
			"refusal was counted in the aggregate and attributed to no arm, so the arms are no "+
			"longer an account of it", arms, failures, mode)
	}
}

// The two measured answers of sandbox_netns_propagation. Named because
// a cell keyed on a bare 1 reads as a boolean, and the gauge has three
// values, the third of which is a broken instrument.
const (
	propagationPrivate int32 = 0
	propagationLinked  int32 = 1
)

// propagationBranch picks the branch a cell must assert, and refuses
// every answer that is not one of the two measured ones.
//
// An absent gauge is a plugin that does not publish it; -1 is a plugin
// that could not read its own mount table. Neither is a host on which
// these cells have nothing to say, and treating either as a skip is how
// four cells go green having asserted no route at all. Both fail.
//
// The two reads must agree. The gauge is a property of a mount taken
// before this window opened, so a change across it means the branch was
// chosen from something that did not hold for the whole attach.
func propagationBranch(t *testing.T, before, after *int32) (int32, bool) {
	t.Helper()
	if before == nil || after == nil {
		t.Errorf("sandbox_netns_propagation is not published by this plugin, so the route these " +
			"cells assert cannot be chosen. Reading its absence as either branch is how one host's " +
			"numbers get asserted on both")
		return 0, false
	}
	if *before != *after {
		t.Errorf("sandbox_netns_propagation read %d at the start of this attach and %d at the end: "+
			"the branch below would be chosen from a host property that did not hold for the whole "+
			"window", *before, *after)
		return 0, false
	}
	switch *after {
	case propagationPrivate, propagationLinked:
		return *after, true
	}
	t.Errorf("sandbox_netns_propagation=%d: the plugin could not read the mount that decides which "+
		"route an attach takes, so this cell cannot know which branch to assert. That is a broken "+
		"instrument and not an absent host, and a skip here would leave the run green with no "+
		"route asserted anywhere", *after)
	return 0, false
}

// branchName renders the branch for the CELL-HOST line, including the
// case where no branch could be chosen: a line that printed "private"
// for an unreadable gauge would put a measurement in the record that
// the run refused to make.
func branchName(branch int32, ok bool) string {
	if !ok {
		return "undecided"
	}
	if branch == propagationLinked {
		return "linked"
	}
	return "private"
}

// delta is the nil-safe subtraction Await's condition needs. It reports
// false for an absent counter instead of zero, so a plugin that does
// not publish the field never satisfies the wait and the caller's
// timeout says so.
func delta(now, before *int32) (int32, bool) {
	if now == nil || before == nil {
		return 0, false
	}
	return *now - *before, true
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

// gaugeString renders a health gauge that may be absent. "absent" and
// "-1" are different findings: the first is a plugin that does not
// publish the field, the second is one that published "I could not
// tell". Folding them would make an old plugin look like a measured
// unknown.
func gaugeString(v *int32) string {
	if v == nil {
		return "absent"
	}
	return strconv.Itoa(int(*v))
}
