// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestHostname_ReachesTheServersTableAfterTheClientStarts is #961's
// outside evidence.
//
// The plugin now starts the persistent client before it asks the daemon
// for the container's name, and gives the name to the running client
// afterwards. Every unit drive for that stops at the handover: the
// library's SetHostname returns before anything is on the wire and says
// so, so "the name was applied" is intent. THE SERVER'S OWN TABLE is the
// effect, and it is what this reads: dnsmasq writes the option-12 name
// into column four of its lease database, once per ACK. That assertion
// is unconditional and identical on both branches below.
//
// WHICH ROUTE NAMED THE CLIENT IS A PROPERTY OF THE HOST, and this cell
// is keyed on it the way sandbox_key_route_test.go's four cells are, for
// the same reason and off the same gauge. The name is handed to a
// RUNNING client only where the attach entered the namespace through the
// sandbox key. Where that key is refused the container PID route carries
// the attach, and that route has already inspected the container on the
// way in, so the name is in the client's opening parameters and handing
// it over again would put it on the wire later for nothing.
//
//   - sandbox_netns_propagation=1 (linked): the late path runs.
//     hostnames_applied_late is the counter the private branch cannot
//     move.
//   - sandbox_netns_propagation=0 (private): the name is still in the
//     server's table and hostnames_applied_late must NOT have moved,
//     because the PID route put the name in the opening parameters.
//     sandbox_pid_fallbacks is the counter the linked branch cannot
//     move.
//
// MEASURED: this suite's own pool answers 0. Integration run
// 35127912707, job 104901808558, main-3-suite: every attach in that job
// logged "Entering the sandbox through its netns key was refused; the
// container PID route carries this attach", and this cell's first
// execution failed there asserting the linked branch's counter on a
// private host. The hosted cross-check and the production host answer 1
// (sandbox_key_route_test.go records the runs).
//
// v4 ONLY, and that is the library's boundary rather than this cell's.
// dhcp-golib v1.0.0 sends no name option for DHCPv6 at all and refuses
// SetHostname on a v6 client, so there is no v6 half of this property to
// measure.
func TestHostname_ReachesTheServersTableAfterTheClientStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	netName := "dh-itest-hostname-live"
	ctrName := "dh-itest-hostname-live-ctr"

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
		"hostnames_applied_late", "hostname_lookup_failures", "hostname_apply_failures",
		"sandbox_key_entries", "sandbox_pid_fallbacks")

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ip, mac)

	// 1. OUTSIDE EVIDENCE, on both branches. The name lands at the bind
	// on the private branch and one exchange after it on the linked one:
	// there the client holds a lease by the time the daemon answers, so
	// it renews early to carry the name (RFC 2131 section 4.4.5) and
	// dnsmasq rewrites the lease line. The poll waits that out.
	if got, line := waitLeaseHostname(t, fixture.LeaseFile(), ip, ctrName, 30*time.Second); got != ctrName {
		t.Errorf("the DHCP server's table has %q as the name for %s, want %q. The lease line was:\n%s\n"+
			"An address with no name in it is what an endpoint gets when the name never reached the "+
			"running client (#961)", got, ip, ctrName, line)
	}

	// 2. The attach is asynchronous, so a running container is not yet
	// an attach the counters have seen. Wait for a route to be taken and
	// fail at the budget: a window closed early reads as "no route", and
	// every branch assertion below would then be about an empty set.
	if _, moved := w.Await(attachObservationBudget, func(now, before *harness.HealthResponse) bool {
		e, ok1 := delta(now.SandboxKeyEntries, before.SandboxKeyEntries)
		f, ok2 := delta(now.SandboxPIDFallbacks, before.SandboxPIDFallbacks)
		return ok1 && ok2 && e+f >= 1
	}); !moved {
		t.Errorf("neither sandbox_key_entries nor sandbox_pid_fallbacks moved within %s of the "+
			"container holding %s: no attach was observed, so nothing below is a measurement",
			attachObservationBudget, ip)
	}

	// 3. The late handover is one more exchange after the attach, so it
	// is waited for -- but only where it can happen. On a private host
	// this counter cannot move, and waiting the budget out for it would
	// charge the run 15s and then read the host as a plugin failure.
	// The branch is CHOSEN here from the live read and ASSERTED below
	// from both ends of the window, which is the read that can see the
	// gauge change under the cell.
	w.Await(15*time.Second, func(now, before *harness.HealthResponse) bool {
		if now.SandboxNetnsPropagation == nil || *now.SandboxNetnsPropagation != propagationLinked {
			return true
		}
		return now.HostnamesAppliedLate > before.HostnamesAppliedLate
	})

	before, after := w.End()
	branch, okBranch := propagationBranch(t, before.SandboxNetnsPropagation, after.SandboxNetnsPropagation)

	entries, ok1 := counterDelta(t, "sandbox_key_entries", before.SandboxKeyEntries, after.SandboxKeyEntries)
	fallbacks, ok2 := counterDelta(t, "sandbox_pid_fallbacks", before.SandboxPIDFallbacks, after.SandboxPIDFallbacks)
	if !ok1 || !ok2 {
		return
	}
	late := after.HostnamesAppliedLate - before.HostnamesAppliedLate
	lookupFailures := after.HostnameLookupFailures - before.HostnameLookupFailures
	applyFailures := after.HostnameApplyFailures - before.HostnameApplyFailures

	// Printed on a pass as well as a failure: the record of which branch
	// this run measured is read off this line.
	t.Logf("CELL-HOST branch=%s sandbox_netns_propagation=%s: sandbox_key_entries +%d, "+
		"sandbox_pid_fallbacks +%d, hostnames_applied_late +%d, hostname_lookup_failures +%d, "+
		"hostname_apply_failures +%d",
		branchName(branch, okBranch), gaugeString(after.SandboxNetnsPropagation),
		entries, fallbacks, late, lookupFailures, applyFailures)

	if entries+fallbacks != 1 {
		t.Errorf("sandbox_key_entries +%d and sandbox_pid_fallbacks +%d sum to %d across one container "+
			"attach, want exactly 1: one attach takes one route, and a sum of zero means the "+
			"assertions below are about an empty set", entries, fallbacks, entries+fallbacks)
	}

	// Neither branch tolerates these: on both, the daemon answered and
	// the name is in the server's table, so neither failure arm has
	// anything to report.
	if lookupFailures != 0 {
		t.Errorf("hostname_lookup_failures advanced by %d on an attach whose name did arrive", lookupFailures)
	}
	if applyFailures != 0 {
		t.Errorf("hostname_apply_failures advanced by %d on an attach whose name did arrive", applyFailures)
	}

	if !okBranch {
		return
	}
	if branch == propagationLinked {
		if late != 1 {
			t.Errorf("hostnames_applied_late advanced by %d, want 1. sandbox_netns_propagation=1 says "+
				"the key route carries this attach, so the client starts before the container is "+
				"inspected and the name has to be handed to it afterwards. The name IS in the "+
				"server's table, so a zero here means it got there in the client's opening "+
				"parameters: the attach waited for the daemon before it started the client, which "+
				"is the order #961 removed", late)
		}
		if fallbacks != 0 {
			t.Errorf("sandbox_pid_fallbacks advanced by %d on a host whose propagation gauge reads 1", fallbacks)
		}
		return
	}

	// The private branch: the name reached the server WITHOUT the late
	// path, which is the whole assertion. A rise here would mean the
	// plugin handed the name over a second time on a route that already
	// had it, putting it on the wire later than the opening parameters
	// did for no gain.
	if late != 0 {
		t.Errorf("hostnames_applied_late advanced by %d with sandbox_netns_propagation=0. The container "+
			"PID route carries this attach and it has already inspected the container, so the name "+
			"belongs in the client's opening parameters and a late handover on top of that is a "+
			"second exchange for a name the server already has (#961)", late)
	}
	if entries != 0 {
		t.Errorf("sandbox_key_entries advanced by %d on a host whose propagation gauge reads 0", entries)
	}
}

// waitLeaseHostname returns the name dnsmasq has recorded for addr, and
// the lease line it came from.
//
// dnsmasq's lease line is `<expiry> <mac> <ip> <hostname> <client-id>`,
// with `*` in the name column for a client that sent none. It polls for
// want rather than reading once: on the linked branch the first line for
// this address is written at the ACK that bound it, which is BEFORE the
// name arrives, so a single read there would measure the plugin's speed
// instead of its behaviour. The last line seen is returned either way,
// so a failure says what the server actually had.
func waitLeaseHostname(t *testing.T, leaseFile, addr, want string, budget time.Duration) (string, string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	got, line := "", ""
	for {
		data, err := os.ReadFile(leaseFile)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read lease file %s: %v", leaseFile, err)
		}
		for _, l := range strings.Split(string(data), "\n") {
			f := strings.Fields(l)
			if len(f) >= 4 && f[2] == addr {
				got, line = f[3], l
			}
		}
		if got == want || !time.Now().Before(deadline) {
			return got, line
		}
		time.Sleep(250 * time.Millisecond)
	}
}
