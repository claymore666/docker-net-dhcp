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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// libnetwork bind-mounts each namespace over an empty file under /var/run/docker/netns/, and the plugin's mount of
// that directory is a snapshot taken at plugin start, so a later sandbox reaches the plugin only where the daemon's
// mount is linked: sandbox_netns_propagation 1 linked, 0 private, -1 unreadable, which fails (#725, #417). Measured by
// these cells: 1 on hosted run 34617922956 (key route), 0 on the suite's pool run 34616833894 (PID route, hence the
// host PID namespace and CAP_SYS_PTRACE). An early key route that opened the placeholder file left the persistent
// client unstarted and twenty-six tests red with "failed to set into network namespace", so the counter sits behind
// an NS_GET_NSTYPE check (#725). The counters are deltas over a window because the suite shares one plugin (#405).

// sandboxKeyCell attaches one container and asserts its address, exactly one route, and the route the host's propagation predicts (#725).
func sandboxKeyCell(t *testing.T, mode, netName, ctrName, user string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			if mode == "bridge" {
				// The bridge fixture runs its own dnsmasq on its own subnet.
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
	// The pools differ per fixture.
	if mode == "bridge" {
		harness.AssertBridgeIP(t, ipv4)
	} else {
		harness.AssertIP(t, ipv4)
	}

	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show")
	if !strings.Contains(out, ipv4+"/") {
		t.Errorf("`ip -4 addr show` inside the container does not carry %s.\n%s\n"+
			"The address Docker reports is the plugin's word for it; this is the kernel's, "+
			"read from inside the namespace the plugin says it configured.", ipv4, out)
	}

	// The attach runs in a goroutine the Join response does not wait for, and a window closed early reads as no route taken.
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
	// Printed on every run, so a green run has rows too.
	t.Logf("CELL mode=%s user=%q: sandbox_key_entries +%d, sandbox_key_entry_failures +%d, sandbox_pid_fallbacks +%d",
		mode, user, entries, failures, fallbacks)
	t.Logf("CELL-ARM mode=%s user=%q: sandbox_key_absent +%d, sandbox_key_not_permitted +%d, "+
		"sandbox_key_not_a_namespace +%d, sandbox_key_wrong_ns_type +%d, sandbox_key_unavailable +%d",
		mode, user, absent, notPermitted, notANamespace, wrongType, unavailable)

	branch, okBranch := propagationBranch(t, before.SandboxNetnsPropagation, after.SandboxNetnsPropagation)

	// The route available is a property of the mount the daemon publishes sandbox keys on, not of the engine (#417).
	t.Logf("CELL-HOST mode=%s user=%q: branch=%s, sandbox_netns_propagation=%s, "+
		"sandbox_netns_init_mounts=%s, sandbox_netns_visible=%s",
		mode, user, branchName(branch, okBranch), gaugeString(after.SandboxNetnsPropagation),
		gaugeString(after.SandboxNetnsInitMounts), gaugeString(after.SandboxNetnsVisible))

	// Without exactly one route, the assertions below pass for a plugin that entered no namespace.
	if entries+fallbacks != 1 {
		t.Errorf("sandbox_key_entries +%d and sandbox_pid_fallbacks +%d sum to %d across one "+
			"container attach on %s, want exactly 1: one attach takes one route, and a sum of zero "+
			"means the assertions below are about an empty set", entries, fallbacks, entries+fallbacks, mode)
	}

	if !okBranch {
		return
	}

	if branch == propagationLinked {
		// entries is the counter the private branch cannot move.
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
		// fallbacks is the counter the linked branch cannot move.
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
		// SECURITY.md's causal sentence rests on this arm: a refused key route is either the bind snapshot
		// (sandbox_key_not_a_namespace) or a daemon with a non-default --exec-root (sandbox_key_not_permitted), with identical
		// aggregate counts and opposite remedies (#725).
		if notANamespace != 1 {
			t.Errorf("sandbox_key_not_a_namespace rose by %d on %s, want exactly 1. This is the arm "+
				"SECURITY.md names: the entry opened and was NOT a namespace, i.e. the daemon's later "+
				"bind mount never reached this plugin's mount namespace. Without this the refusal "+
				"counted above is equally consistent with a key shape this plugin simply refuses, "+
				"which is a different finding and a different fix", notANamespace, mode)
		}
	}

	// Each names a host misconfiguration, not a route.
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
	// libnetwork always puts the key in the Join request, so a rise means the refusal is about a missing input (#725).
	if absent != 0 {
		t.Errorf("sandbox_key_absent rose by %d on %s: this attach reached the key route with no key "+
			"at all, so the arm below is not measuring what the daemon published", absent, mode)
	}
	// The arms are exhaustive by construction (countSandboxKeyRefusal).
	if arms := absent + notPermitted + notANamespace + wrongType + unavailable; arms != failures {
		t.Errorf("the refusal arms sum to %d and sandbox_key_entry_failures rose by %d on %s: a "+
			"refusal was counted in the aggregate and attributed to no arm, so the arms are no "+
			"longer an account of it", arms, failures, mode)
	}
}

// The two measured answers of sandbox_netns_propagation; the third value, -1, is a broken instrument.
const (
	propagationPrivate int32 = 0
	propagationLinked  int32 = 1
)

// An absent gauge or -1 fails, never skips; the two reads must agree, since the mount predates the window.

// propagationBranch picks the branch a cell must assert and refuses any answer other than the two measured ones.
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

// branchName renders the branch for the CELL-HOST line, including the case where no branch could be chosen.
func branchName(branch int32, ok bool) string {
	if !ok {
		return "undecided"
	}
	if branch == propagationLinked {
		return "linked"
	}
	return "private"
}

// delta is the nil-safe subtraction Await's condition needs, reporting false for an absent counter.
func delta(now, before *int32) (int32, bool) {
	if now == nil || before == nil {
		return 0, false
	}
	return *now - *before, true
}

// counterDelta returns a counter's delta and refuses to compute one from an absent counter.
func counterDelta(t *testing.T, name string, before, after *int32) (int32, bool) {
	t.Helper()
	if before == nil || after == nil {
		t.Errorf("%s is not published by this plugin, so it cannot be judged — and reading its "+
			"absence as zero is how a missing route would pass this test", name)
		return 0, false
	}
	return *after - *before, true
}

// TestSandboxKeyRoute_Macvlan runs the sandbox-key cell for a macvlan network (#725).

func TestSandboxKeyRoute_Macvlan(t *testing.T) {
	sandboxKeyCell(t, "macvlan", "dh-itest-skey-mv", "dh-itest-skey-mv-ctr", "")
}

func TestSandboxKeyRoute_Bridge(t *testing.T) {
	sandboxKeyCell(t, "bridge", "dh-itest-skey-br", "dh-itest-skey-br-ctr", "")
}

func TestSandboxKeyRoute_Ipvlan(t *testing.T) {
	sandboxKeyCell(t, "ipvlan", "dh-itest-skey-iv", "dh-itest-skey-iv-ctr", "")
}

// The kernel gates /proc/<pid>/ns/net on PTRACE_MODE_READ, so a non-root init uid is the one cell where the PID route
// needs CAP_SYS_PTRACE (#317). This is a non-root init uid, not a `dockerd --userns-remap` daemon, which the lane does
// not run.

// TestSandboxKeyRoute_NonRootContainer runs the sandbox-key cell for a container whose init runs as uid 65534 (#317, #725).
func TestSandboxKeyRoute_NonRootContainer(t *testing.T) {
	sandboxKeyCell(t, "macvlan", "dh-itest-skey-nr", "dh-itest-skey-nr-ctr", "65534:65534")
}

// gaugeString renders a health gauge that may be absent, keeping "absent" and "-1" apart.
func gaugeString(v *int32) string {
	if v == nil {
		return "absent"
	}
	return strconv.Itoa(int(*v))
}
