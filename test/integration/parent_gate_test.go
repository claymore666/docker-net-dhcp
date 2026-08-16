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

// TestParentGate_ReclaimDoesNotBlockOtherModeOnSameParent reproduces,
// deliberately, the collision that broke two unrelated tests in CI when
// the never-bound reclaim first landed.
//
// The mechanism is the kernel's: a parent NIC registers one rx_handler,
// so it is a macvlan port or an ipvlan port and never both. The
// orphaned-lease reclaim attaches a temporary child of the network's own
// kind and holds it for a full DHCP round trip — seconds — from a
// goroutine ordered against no Docker request. Any endpoint of the other
// kind arriving on that parent in the meantime used to be refused with
// "device or resource busy", surfacing to the user as a failed
// `docker run` that had nothing to do with the container that exited.
//
// # Why this drives the plugin directly instead of running containers
//
// The first version of this test ran a container that exited
// immediately, on the assumption that it would reliably exit before its
// persistent DHCP client could bind and thus reliably orphan its lease.
// That assumption is a race against dhcpcd's DORA, and #555's shard
// repartition moved the test to a position where it started losing:
// same commit, same code, green in shard 1 and red in shard 2. The wire
// log showed why — the persistent client bound and released normally,
// so no lease was ever orphaned, so no reclaim ever ran and there was
// nothing for the rival endpoint to collide with. A test whose verdict
// depends on its neighbours is not evidence.
//
// So both halves of the collision are now constructed rather than hoped
// for:
//
//   - The orphan is built by joining with no container behind the
//     endpoint at all. The attach's container lookup cannot resolve one,
//     returns util.ErrNoContainer, and the plugin hands the address
//     back. That is a real production state, not a fabricated one — it
//     is what the plugin sees whenever a container is removed inside
//     the attach window.
//   - The rival is not issued until the reclaim's temporary link is
//     observed on the host. The window is not merely likely to be open;
//     it is known to be open, near its start.
//
// What remains is a margin rather than a race: the reclaim holds the
// parent for a full DHCP round trip, and the rival's CreateEndpoint is a
// socket call. The test measures and logs that margin so a future
// regression in it is visible rather than silent.
//
// # This construction did not work when it was first written, and why
//
// Worth stating plainly, because the code now reads as though it always
// did. The first attempt aimed at the same state through the same call
// sequence and failed: ErrNoContainer was not a reclaiming error, so
// the attach simply gave up, charged itself to join_start_failures, and
// left the address leased upstream. No reclaim ran, no dh-rel-* link
// ever appeared, and the poll below timed out against something that
// was never going to happen.
//
// That was not a mis-implementation of a sound idea — it was the idea
// meeting a genuine hole in the product. #566 is that hole, found by
// this test failing to build the state it wanted, and it had to be
// fixed before this test could exist. Anyone reading this in a year
// should not conclude the approach was obvious; it only works because
// the plugin changed underneath it.
//
// # The self-guard stays
//
// parent_link_waits must advance. With the construction above it should
// never fail — but it is the only thing that can distinguish "the gate
// absorbed the collision" from "the collision never happened", and the
// second is exactly how the earlier version of this test managed to be
// green in one shard and red in another. It is kept for the same reason
// the fix exists: a check that goes red beats a comment that explains.
func TestParentGate_ReclaimDoesNotBlockOtherModeOnSameParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		ipvlanNet  = "dh-itest-pgate-ipvlan"
		macvlanNet = "dh-itest-pgate-macvlan"
	)

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
	t.Cleanup(func() { _ = cli.Close() })

	// Both networks up front. They share the fixture's parent NIC and
	// disagree about its kind, which is the whole point.
	ipvlanID := harness.CreateNetwork(t, ctx, ipvlanNet, "ipvlan", nil)
	macvlanID := harness.CreateNetwork(t, ctx, macvlanNet, "macvlan", nil)

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli, "parent_link_waits", "parent_link_wait_timeouts")
	before := w.Before()

	// Step 1: take a lease and attach an ipvlan child, exactly as a real
	// container's CreateEndpoint would.
	orphanEP := harness.NewEndpointID(t)
	addrs, err := drv.CreateEndpoint(ctx, ipvlanID, orphanEP)
	if err != nil {
		t.Fatalf("CreateEndpoint(orphan, ipvlan): %v", err)
	}
	t.Cleanup(func() { drv.CleanupEndpoint(ipvlanID, orphanEP) })
	t.Logf("orphan endpoint leased %s", addrs.Address)

	// Step 2: orphan it. Nothing claims this endpoint on the network, so
	// the attach's container lookup fails with util.ErrNoContainer and
	// the plugin gives the address back from a goroutine — attaching its
	// own temporary ipvlan child to the shared parent to do it (#566).
	if err := drv.Join(ctx, ipvlanID, orphanEP, harness.SyntheticSandboxKey(t)); err != nil {
		t.Fatalf("Join(orphan, no container): %v", err)
	}

	// Step 3: remove the orphan endpoint's OWN child link before the
	// rival arrives. Without this the rival would be refused by the
	// kernel because a live ipvlan endpoint still holds the parent —
	// a real limitation (#556) but NOT the one under test here, and it
	// would make this test pass or fail for the wrong reason.
	if err := drv.DeleteEndpoint(ctx, ipvlanID, orphanEP); err != nil {
		t.Fatalf("DeleteEndpoint(orphan): %v", err)
	}

	// Step 4: wait until the reclaim is provably holding the parent.
	relLink := awaitReleaseLinkPresent(t, 30*time.Second)
	windowOpen := time.Now()
	t.Logf("reclaim link %s observed; collision window is open", relLink)

	// Step 5: ask the same parent for the other kind. This is the
	// operation that used to be refused with EBUSY.
	//
	// # This asserts CreateEndpoint, where the earlier version asserted ContainerStart
	//
	// That looks like a downgrade and is worth being explicit about,
	// because nothing automated can tell the difference — a check that
	// moved down a layer reads exactly like a check that was quietly
	// weakened.
	//
	// It is not one. EBUSY was raised BY CreateEndpoint; the failed
	// `docker run` the user saw was that error surfacing. Asserting here
	// tests the fault at the layer it actually lives on, with Docker's
	// scheduling removed rather than a claim removed. And the choice was
	// never "keep the stronger assertion or the weaker one" — removing
	// the container is what makes the orphan deterministic, and a design
	// with no container has no ContainerStart to assert. The real
	// alternative was keeping a test that could not say whether the code
	// was wrong or the window had closed.
	//
	// What is genuinely not covered anywhere as a result: a container
	// start refused under parent contention, end to end. The golden-path
	// tests start containers on this parent in both modes, but none of
	// them does it while a reclaim holds it. If that coverage is ever
	// wanted back it needs a test that can construct the contention AND
	// keep a container, which is a harder thing than either half.
	rivalEP := harness.NewEndpointID(t)
	rivalStart := time.Now()
	rivalAddrs, rivalErr := drv.CreateEndpoint(ctx, macvlanID, rivalEP)
	rivalWait := time.Since(rivalStart)
	t.Cleanup(func() { drv.CleanupEndpoint(macvlanID, rivalEP) })

	// Log the rival's address as well as the orphan's. Both endpoints
	// this test creates land on the shared parent, and a run where a
	// neighbouring test later trips over a stale route needs to be able
	// to ask whether either of ours was involved. Logging one address
	// and not the other left exactly that question unanswerable once,
	// with the evidence window already outside the artifact.
	t.Logf("rival endpoint leased %s", rivalAddrs.Address)

	// The margin between the window opening and the rival reaching the
	// parent is what makes this a construction rather than a race.
	// Logged so a regression in it is visible; the reclaim holds the
	// parent for a DHCP round trip, so this should stay in single-digit
	// milliseconds against a window measured in seconds.
	t.Logf("PHASE collision_setup %.3fs (window open -> rival issued), rival blocked %.3fs",
		rivalStart.Sub(windowOpen).Seconds(), rivalWait.Seconds())

	after, _ := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.ParentLinkWaits > before.ParentLinkWaits
	})
	w.End()

	if rivalErr != nil {
		if strings.Contains(strings.ToLower(rivalErr.Error()), "device or resource busy") {
			t.Fatalf("the reclaim's link blocked an endpoint of the other mode on the same parent: %v\n"+
				"This is the collision the per-parent gate exists to remove — an endpoint "+
				"refused because an unrelated one had just been orphaned.", rivalErr)
		}
		t.Fatalf("CreateEndpoint(rival, macvlan): %v", rivalErr)
	}

	if after == nil || after.ParentLinkWaits <= before.ParentLinkWaits {
		got := int32(-1)
		if after != nil {
			got = after.ParentLinkWaits
		}
		t.Fatalf("parent_link_waits did not advance (before=%d, after=%d) — the rival endpoint "+
			"did not queue behind the reclaim even though the reclaim's link %s was on the host "+
			"when the rival was issued.\n"+
			"The construction is sound, so this is not a missed window: either the gate is not "+
			"being taken on one of the two paths, or the reclaim released the parent before its "+
			"link was removed. Do NOT relax this assertion — check lockParent's placement in "+
			"orphan_release.go and parent_attached.go first.",
			before.ParentLinkWaits, got, relLink)
	}

	if after.ParentLinkWaitTimeouts > before.ParentLinkWaitTimeouts {
		t.Errorf("parent_link_wait_timeouts advanced (before=%d, after=%d) — the endpoint gave up "+
			"waiting and only succeeded because the kernel happened to allow it. The gate's budget "+
			"is not covering a reclaim's normal duration.",
			before.ParentLinkWaitTimeouts, after.ParentLinkWaitTimeouts)
	}

	// Leave the parent as we found it: the reclaim's link must be gone
	// before the next test asks this NIC for a different kind.
	awaitReleaseLinksGone(t)
}
