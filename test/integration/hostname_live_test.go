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

// SetHostname returns before anything is on the wire, so the evidence is dnsmasq's lease file, which records option 12
// in column four once per ACK. Only an attach through the sandbox key hands the name to a running client; the PID
// route already has it in the opening parameters. This suite's pool reads sandbox_netns_propagation=0 (run
// 35127912707, job 104901808558), the hosted cross-check and the production host read 1 (#961).
// v4 only: this path runs without register_dns, where a v6 client sends no name and SetHostname refuses it (#1029).

// TestHostname_ReachesTheServersTableAfterTheClientStarts checks that the container's name reaches the server's lease table on either attach route (#961).
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

	// On the linked branch the client already holds a lease when the name arrives, so it renews early to carry it
	// (RFC 2131 section 4.4.5) and dnsmasq rewrites the line (#961).
	if got, line := waitLeaseHostname(t, fixture.LeaseFile(), ip, ctrName, 30*time.Second); got != ctrName {
		t.Errorf("the DHCP server's table has %q as the name for %s, want %q. The lease line was:\n%s\n"+
			"An address with no name in it is what an endpoint gets when the name never reached the "+
			"running client (#961)", got, ip, ctrName, line)
	}

	// The attach is asynchronous; a window closed before a route was taken would make every branch assertion vacuous.
	if _, moved := w.Await(attachObservationBudget, func(now, before *harness.HealthResponse) bool {
		e, ok1 := delta(now.SandboxKeyEntries, before.SandboxKeyEntries)
		f, ok2 := delta(now.SandboxPIDFallbacks, before.SandboxPIDFallbacks)
		return ok1 && ok2 && e+f >= 1
	}); !moved {
		t.Errorf("neither sandbox_key_entries nor sandbox_pid_fallbacks moved within %s of the "+
			"container holding %s: no attach was observed, so nothing below is a measurement",
			attachObservationBudget, ip)
	}

	// The late handover cannot happen on a private host, so it is awaited only on the linked branch, chosen from the live gauge.
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

	// Printed on a pass too: the record of which branch a run measured is read off this line.
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

	// On the private branch a second handover would put the name on the wire later than the opening parameters did (#961).
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

// dnsmasq writes the line at the bind, before the name arrives on the linked branch, and puts `*` for no name.

// waitLeaseHostname polls until dnsmasq records want for addr and returns the recorded name and its lease line.
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
