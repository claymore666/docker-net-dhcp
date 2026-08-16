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
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestJoinNoContainer_AddressIsReleased covers #566.
//
// CreateEndpoint leases an address with a one-shot client and
// deliberately keeps the binding, so the address is held when the
// persistent client takes over at attach. If the attach then fails
// because no container ever claimed the endpoint, there is no
// persistent client to release it and no container using it — the
// address used to sit leased upstream until it expired on its own,
// while the plugin logged "lease will not be renewed" and flipped
// unhealthy on join_start_failures.
//
// That combination is the worst shape a failure can have here: a
// counter moves, so it looks observed, but the counter names the wrong
// thing and the address never comes back. It is the same family as
// #370, #549 and #561 — the plugin's bookkeeping and the DHCP server's
// state disagreeing, with only the server being right.
//
// # How this reaches the state, and why the sandbox must be REAL
//
// The plugin asks two independent questions when an attach fails: is
// the sandbox netns still there, and does any container hold this
// endpoint on the network. Only the second one is this test's subject.
//
// So the Join is issued with the netns path of a container that is
// genuinely running — a live sandbox, provably present on the host —
// against an endpoint Docker never attached to anything. The netns
// question answers "still there", the lookup retries for the whole
// attach budget and then answers "nobody holds it", and
// util.ErrNoContainer is the only path out.
//
// The earlier version pointed Join at a netns path that did not exist,
// which reached the same branch for the wrong reason: the plugin could
// not read the netns directory at all, so it could never conclude
// "gone". The moment that directory became visible (#567), the same
// construction started answering "the container vanished" and landed on
// a different counter. A live key is stable under both, and needing it
// to be is the tell that the old one was a stand-in.
//
// The production shape this stands for is a container disconnected from
// the network while its attach is in flight — netns present, endpoint
// no longer on the network.
//
// # The assertion is the server's log
//
// orphaned_leases_released only proves the plugin believes it released
// something. Only a DHCPRELEASE reaching dnsmasq proves the address is
// actually back in the pool, and it is keyed on this endpoint's own
// address so no neighbouring release can satisfy it by accident. The
// counters are checked too, but after the wire, and never instead of
// it.
func TestJoinNoContainer_AddressIsReleased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName   = "dh-itest-nocontainer"
		holderCtr = "dh-itest-nocontainer-holder"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// A real, running container purely to supply a real netns. It holds
	// its OWN endpoint on this network; the endpoint under test below is
	// a separate one that no container ever claims, which is what makes
	// the lookup fail while the sandbox question still answers "present".
	holderID, _, _ := harness.RunContainer(t, ctx, netName, holderCtr)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, holderID)

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"orphaned_leases_released", "join_aborted_no_container", "join_start_failures")
	before := w.Before()

	endpointID := harness.NewEndpointID(t)
	addrs, err := drv.CreateEndpoint(ctx, netID, endpointID)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	t.Cleanup(func() { drv.CleanupEndpoint(netID, endpointID) })

	ip := addressOnly(addrs.Address)
	if ip == "" {
		t.Fatalf("CreateEndpoint returned no IPv4 address (got %q)", addrs.Address)
	}

	// The address must actually have been leased, or every assertion
	// below is about an address the server never handed out and the test
	// would pass for the wrong reason.
	if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s; the endpoint never took a lease, "+
			"so this run proves nothing about releasing one", ip)
	}
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	// The sandbox is real and present; the endpoint is claimed by
	// nobody. Only the second fact can fail the attach.
	if err := drv.Join(ctx, netID, endpointID, sandboxKey); err != nil {
		t.Fatalf("Join: %v", err)
	}

	after, _ := w.Await(orphanReleaseBudget, func(now, before *harness.HealthResponse) bool {
		return now.OrphanedLeasesReleased > before.OrphanedLeasesReleased
	})
	w.End()

	// Ground truth first. This is the assertion the issue is about; the
	// counters below only explain it.
	deadline := time.Now().Add(orphanReleaseBudget)
	for fixture.CountLogLines("DHCPRELEASE", ip)-releasesBefore < 1 {
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq never logged a DHCPRELEASE for %s within %v — the address is "+
				"still held upstream with nobody responsible for it. No container ever "+
				"claimed this endpoint, so nothing is using the address and nothing will "+
				"ever release it; it stays leased until expiry (#566)",
				ip, orphanReleaseBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if after == nil || after.OrphanedLeasesReleased <= before.OrphanedLeasesReleased {
		got := int32(-1)
		if after != nil {
			got = after.OrphanedLeasesReleased
		}
		t.Fatalf("orphaned_leases_released did not advance within %v (before=%d, after=%d) — "+
			"the server saw the release but the plugin did not count it",
			orphanReleaseBudget, before.OrphanedLeasesReleased, got)
	}

	if got := after.JoinAbortedNoContainer - before.JoinAbortedNoContainer; got != 1 {
		t.Errorf("join_aborted_no_container advanced by %d, want 1 — the release happened "+
			"but is not attributed to this cause, so an operator cannot tell this apart "+
			"from an ordinary orphaned lease", got)
	}

	// The counter that used to move instead, and the reason the leak was
	// invisible: join_start_failures is healthy-affecting and means "a
	// RUNNING container has no renewal client". Nothing is running here,
	// so charging this to it both leaked the address and paged an
	// operator about a container that does not exist.
	if got := after.JoinStartFailures - before.JoinStartFailures; got != 0 {
		t.Errorf("join_start_failures advanced by %d, want 0 — an attach with no container "+
			"behind it is not a plugin fault, and counting it as one flips healthy for "+
			"something nobody can act on", got)
	}

	if got := after.OrphanedLeaseReleaseFailures - before.OrphanedLeaseReleaseFailures; got != 0 {
		t.Errorf("orphaned_lease_release_failures advanced by %d, want 0 — "+
			"the reclaim was attempted but could not complete", got)
	}

	awaitReleaseLinksGone(t)
}

// awaitReleaseLinksGone waits for the reclaim's temporary link to be
// removed from the shared parent.
//
// Two reasons, and the second is why it is not optional. The plugin
// must not leak the links it creates: the reclaim deletes `dh-rel-*` in
// a deferred call after the release, so a link still present long
// afterwards is a real defect and nothing else asserts it. And this
// test hands the parent straight to the next one — a parent carrying a
// macvlan child cannot accept an ipvlan one, which is #486's mechanism
// and #556's residue, so returning early would hand a neighbour an
// EBUSY that looks like its own failure.
//
// Tests share one parent until #556 changes that; until then, leaving
// it as we found it is this test's job.
func awaitReleaseLinksGone(t *testing.T) {
	t.Helper()

	const budget = 15 * time.Second
	deadline := time.Now().Add(budget)
	for {
		links, err := netlink.LinkList()
		if err != nil {
			t.Fatalf("LinkList: %v", err)
		}
		var left []string
		for _, l := range links {
			if strings.HasPrefix(l.Attrs().Name, "dh-rel-") {
				left = append(left, l.Attrs().Name)
			}
		}
		if len(left) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("orphan-release link(s) %v still on the host after %v; the reclaim "+
				"did not remove them, and a macvlan child left on the shared parent "+
				"blocks the next ipvlan test (#486/#556)", left, budget)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// addressOnly strips the prefix length from a CIDR address, so a wire
// assertion can be keyed on what the DHCP server logs. Returns "" if
// there is nothing to key on, which the caller must treat as a failure
// to construct rather than a failure of the code under test.
func addressOnly(cidr string) string {
	if cidr == "" {
		return ""
	}
	if i := strings.IndexByte(cidr, '/'); i >= 0 {
		return cidr[:i]
	}
	return cidr
}
