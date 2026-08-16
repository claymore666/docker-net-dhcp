// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestParentGate_SecondReclaimQueuesBehindTheFirst proves the per-parent
// gate serialises two operations that would otherwise be on the parent
// NIC at the same time.
//
// The constraint is the kernel's: a parent registers one rx_handler, so
// it is a macvlan port or an ipvlan port and never both. The
// orphaned-lease reclaim attaches a temporary child of the network's own
// kind and holds it for a full DHCP round trip — seconds — from a
// goroutine ordered against no Docker request. Without the gate, an
// operation of the other kind arriving meanwhile is refused with "device
// or resource busy", which reaches a user as a `docker run` that failed
// because of an unrelated container (#486, #549).
//
// # Two reclaims, not a reclaim and an endpoint
//
// The obvious construction is the one this test used to have: orphan an
// ipvlan lease, wait for the reclaim's link to appear, then create a
// macvlan endpoint on the same parent and assert it was not refused.
// That is the user-visible collision exactly, and it does not work. It
// is not flaky, it is structurally unable to make the window:
//
//	parent_gate_test.go:150: reclaim link dh-rel-53d707 observed; collision window is open
//	parent_gate_test.go:197: PHASE collision_setup 0.000s (window open -> rival issued), rival blocked 7.192s
//	parent_gate_test.go:219: parent_link_waits did not advance (before=3, after=3)
//
// The rival was issued 0.000s after the window opened and still
// registered no wait at all. That is the whole diagnosis: it reached the
// gate after the reclaim had already finished with the parent. A
// reclaim's hold is one DHCP round trip, about two seconds; a
// CreateEndpoint spends longer than that getting from the socket to its
// LinkAdd. The earlier log line measured "window open -> rival ISSUED",
// which is 0.000s by construction and says nothing about when the rival
// arrived — the wrong end of the margin, and it read as reassuring for
// several runs.
//
// So the contender here is a second RECLAIM. Its path from the Join
// socket call to lockParent is name generation and a MAC plan —
// arithmetic, no IO, no Docker API — so it reaches the gate in
// milliseconds against a hold measured in seconds. That is a margin of
// roughly two orders of magnitude, which is what makes this a
// construction rather than a race.
//
// # What this therefore does NOT cover, and why nothing can
//
// It does not reproduce a cross-mode EBUSY. Both reclaims here are the
// same kind, so the kernel would accept both even with no gate at all;
// what is proved is that the gate serialised them, which is the
// mechanism that prevents the cross-mode refusal rather than the
// refusal itself.
//
// That gap is not an oversight and is not closeable by trying harder.
// Two operations of DIFFERENT kinds can only overlap on one parent if
// one of them is a CreateEndpoint — a parent cannot hold live children
// of both kinds, so the two endpoints cannot exist at once — and the
// measurement above is that a CreateEndpoint cannot reach the gate
// inside a reclaim's hold. Anyone who wants that coverage back needs to
// lengthen the hold or shorten the arrival, and both of those are
// changes to the product made to suit a test.
//
// # The wire is the assertion of record
//
// parent_link_waits proves the plugin believes it queued. Both leases
// coming back to the server proves the two reclaims actually completed
// while sharing the parent — a gate that serialised them into a
// deadlock would satisfy the counter and fail here.
func TestParentGate_SecondReclaimQueuesBehindTheFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName   = "dh-itest-pgate"
		holderCtr = "dh-itest-pgate-holder"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	netID := harness.CreateNetwork(t, ctx, netName, "ipvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// A real, running container purely to supply a real netns. Both
	// endpoints below are joined against ITS sandbox key: the netns
	// genuinely exists, so the only thing that can fail their attach is
	// that no container claims them — which is what makes each one an
	// orphan deterministically rather than by out-running dhcpcd.
	holderID, _, _ := harness.RunContainer(t, ctx, netName, holderCtr)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, holderID)

	drv := harness.NewDriverClient(t, ctx, cli)

	// Both endpoints are created BEFORE the window opens. A
	// CreateEndpoint issued inside the window would be the construction
	// this test replaced — it cannot reach the gate in time, and it
	// would also be the slow half of the race rather than the fast one.
	// Same network, so both children are the same kind and coexist.
	epA := harness.NewEndpointID(t)
	addrsA, err := drv.CreateEndpoint(ctx, netID, epA)
	if err != nil {
		t.Fatalf("CreateEndpoint(A): %v", err)
	}
	t.Cleanup(func() { drv.CleanupEndpoint(netID, epA) })
	ipA := addressOnly(addrsA.Address)

	epB := harness.NewEndpointID(t)
	addrsB, err := drv.CreateEndpoint(ctx, netID, epB)
	if err != nil {
		t.Fatalf("CreateEndpoint(B): %v", err)
	}
	t.Cleanup(func() { drv.CleanupEndpoint(netID, epB) })
	ipB := addressOnly(addrsB.Address)

	if ipA == "" || ipB == "" || ipA == ipB {
		t.Fatalf("the two endpoints did not take two distinct addresses (A=%q B=%q); "+
			"there is no pair of reclaims to serialise", addrsA.Address, addrsB.Address)
	}
	t.Logf("endpoint A leased %s, endpoint B leased %s", ipA, ipB)

	// Both must actually hold a lease, or the reclaims below have
	// nothing to hand back and the test would pass over an empty window.
	for _, ip := range []string{ipA, ipB} {
		if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
			t.Fatalf("dnsmasq logged no DHCPACK for %s; that endpoint never took a lease, "+
				"so its reclaim has nothing to do and would not hold the parent", ip)
		}
	}
	releasesBeforeA := fixture.CountLogLines("DHCPRELEASE", ipA)
	releasesBeforeB := fixture.CountLogLines("DHCPRELEASE", ipB)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"parent_link_waits", "parent_link_wait_timeouts", "orphaned_leases_released")
	before := w.Before()

	// Orphan A. Nothing claims this endpoint, so the attach's container
	// lookup fails, the plugin hands the address back from a goroutine,
	// and that goroutine attaches its own temporary child to the shared
	// parent to do it (#566).
	if err := drv.Join(ctx, netID, epA, sandboxKey); err != nil {
		t.Fatalf("Join(A): %v", err)
	}
	// A's own child link goes now, so the only thing of A's on the
	// parent is the reclaim's temporary one.
	if err := drv.DeleteEndpoint(ctx, netID, epA); err != nil {
		t.Fatalf("DeleteEndpoint(A): %v", err)
	}

	// The window is open once A's reclaim is provably on the parent —
	// known to be open, near its start, rather than likely to be.
	relLink := awaitReleaseLinkPresent(t, 30*time.Second)
	windowOpen := time.Now()
	t.Logf("reclaim link %s observed; A holds the parent", relLink)

	// Orphan B into that window. Everything B's reclaim does before it
	// reaches lockParent is computation, which is the point.
	if err := drv.Join(ctx, netID, epB, sandboxKey); err != nil {
		t.Fatalf("Join(B): %v", err)
	}
	arrived := time.Since(windowOpen)
	if err := drv.DeleteEndpoint(ctx, netID, epB); err != nil {
		t.Fatalf("DeleteEndpoint(B): %v", err)
	}

	// LOGGED, NOT ASSERTED, on purpose. A threshold here would turn a
	// slow runner into a red build that says nothing about the plugin.
	// What makes the test sound is the two orders of magnitude between
	// this and a reclaim's multi-second hold; if this line ever starts
	// reading in seconds, the construction has stopped working and the
	// assertion below is the thing that will say so.
	t.Logf("PHASE collision_setup %.3fs (window open -> B's join returned)", arrived.Seconds())

	after, _ := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.ParentLinkWaits > before.ParentLinkWaits
	})
	w.End()

	if after == nil || after.ParentLinkWaits <= before.ParentLinkWaits {
		got := int32(-1)
		if after != nil {
			got = after.ParentLinkWaits
		}
		t.Fatalf("parent_link_waits did not advance (before=%d, after=%d) — B's reclaim did not "+
			"queue behind A's, even though A's link %s was on the parent %.3fs before B was "+
			"orphaned.\n"+
			"B's path from the join socket call to lockParent is name generation and a MAC plan, "+
			"no IO at all, against a hold that is a whole DHCP round trip — so this is not a "+
			"missed window and relaxing the assertion or widening the budget would only hide it. "+
			"Check that lockParent is still taken in synthesiseRelease, and that it is still taken "+
			"BEFORE upReleaseLink rather than somewhere inside it.",
			before.ParentLinkWaits, got, relLink, arrived.Seconds())
	}

	if after.ParentLinkWaitTimeouts > before.ParentLinkWaitTimeouts {
		t.Errorf("parent_link_wait_timeouts advanced (before=%d, after=%d) — B gave up waiting and "+
			"proceeded anyway. It may still have succeeded, because same-kind children coexist, but "+
			"the gate's budget is no longer covering a reclaim's normal duration and a cross-mode "+
			"caller in B's place would have been refused.",
			before.ParentLinkWaitTimeouts, after.ParentLinkWaitTimeouts)
	}

	// Ground truth. The counter says the plugin queued; only the server
	// says both addresses actually came back while the two reclaims
	// shared the parent.
	deadline := time.Now().Add(orphanReleaseBudget)
	for {
		gotA := fixture.CountLogLines("DHCPRELEASE", ipA) - releasesBeforeA
		gotB := fixture.CountLogLines("DHCPRELEASE", ipB) - releasesBeforeB
		if gotA >= 1 && gotB >= 1 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq saw DHCPRELEASE for %s=%d and %s=%d within %v, want both — "+
				"the gate serialised the two reclaims and at least one of them then failed to "+
				"hand its lease back, which is a leaked address rather than a queueing problem",
				ipA, gotA, ipB, gotB, orphanReleaseBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if after.OrphanedLeasesReleased-before.OrphanedLeasesReleased < 2 {
		t.Errorf("orphaned_leases_released advanced by %d, want at least 2 — the server saw both "+
			"releases but the plugin did not count them, so the counter is not a usable signal "+
			"for this path",
			after.OrphanedLeasesReleased-before.OrphanedLeasesReleased)
	}

	// Leave the parent as we found it: an ipvlan child of ours must be
	// gone before the next test asks this NIC for a macvlan one.
	awaitReleaseLinksGone(t)
}
