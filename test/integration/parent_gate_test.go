// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestParentGate_EndpointQueuesBehindAProbe proves the per-parent gate
// serialises two operations that would otherwise be attaching a child to
// the same parent NIC at the same time.
//
// The constraint it exists for is the kernel's: a parent registers one
// rx_handler, so it is a macvlan port or an ipvlan port and never both,
// and the second kind to ask gets EBUSY. Without the gate that reaches a
// user as a `docker run` refused because of something else entirely
// (#486, #549).
//
// # Why the holder is a probe and not a reclaim
//
// This test used to contend two orphaned-lease reclaims, because a
// reclaim held the parent for a full DHCP round trip from a goroutine
// ordered against no Docker request. That mechanism was removed in
// v1.9.0 (#800): it raced the tombstone that promises a restarting
// container the same address. The gate did not go with it — the
// validate_dhcp preflight probe still holds a parent across a DHCP round
// trip while an unrelated endpoint may ask for the other mode.
//
// So the holder here is that probe, pointed at a parent with no DHCP
// server on it. The probe then runs to its full 8-second budget instead
// of finishing in the ~2 seconds a reachable server takes, which is what
// turns the window from something to be raced into something to be
// walked into.
//
// # Why the contender is a raw driver call
//
// The old test recorded, in its own log output, why an endpoint issued
// through Docker cannot be the contender:
//
//	PHASE collision_setup 0.000s (window open -> rival issued), rival blocked 7.192s
//	parent_link_waits did not advance (before=3, after=3)
//
// The rival was issued the instant the window opened and still reached
// the gate after the holder had finished with the parent, because a
// `docker run` spends seconds getting from the client to the driver's
// CreateEndpoint. Measuring "window open -> rival ISSUED" is 0.000s by
// construction and says nothing about arrival — the wrong end of the
// margin, and it read as reassuring for several runs.
//
// The contender here is therefore a CreateEndpoint issued straight at
// the plugin socket, bypassing Docker entirely. Its path to lockParent
// is a socket write and an option lookup, so it arrives in milliseconds
// against a hold measured in seconds: three orders of magnitude, which
// is what makes this a construction rather than a race.
func TestParentGate_EndpointQueuesBehindAProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Both names stay under the 15-character kernel limit.
	const (
		parentName = "dh-itest-pgate"
		netName    = "dh-itest-pgnet"
		probeNet   = "dh-itest-pgprobe"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// A dummy parent, dedicated to this test. Dummies carry no L2
	// traffic anywhere, so the probe's DHCPDISCOVER vanishes and the
	// probe runs to its full budget — the long hold this test needs.
	//
	// Dedicated rather than shared: the holder occupies this NIC for
	// eight seconds and the contender is deliberately made to queue on
	// it, which is precisely what must not happen to a neighbouring
	// test's parent.
	la := netlink.NewLinkAttrs()
	la.Name = parentName
	dummy := &netlink.Dummy{LinkAttrs: la}
	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("LinkAdd dummy parent: %v", err)
	}
	t.Cleanup(func() {
		if err := netlink.LinkDel(dummy); err != nil {
			t.Logf("WARN: LinkDel dummy parent: %v", err)
		}
	})
	if err := netlink.LinkSetUp(dummy); err != nil {
		t.Fatalf("LinkSetUp dummy parent: %v", err)
	}

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// The contender's network, created BEFORE the window opens and
	// without validate_dhcp, so creating it neither probes nor waits.
	res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: harness.DriverName,
		IPAM:   &network.IPAM{Driver: "null"},
		Options: map[string]string{
			"mode":   "macvlan",
			"parent": parentName,
		},
	})
	if err != nil {
		t.Fatalf("NetworkCreate(%s): %v", netName, err)
	}
	netID := res.ID
	t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), netID) })

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"parent_link_waits", "parent_link_wait_timeouts")
	before := w.Before()

	// Open the window: a validate_dhcp network create on the same
	// parent. It takes the gate at the top of runDHCPProbe and holds it
	// until its child link is gone, then fails — the failure is
	// expected and is not what is under test.
	probeDone := make(chan struct{})
	probeStart := time.Now()
	go func() {
		defer close(probeDone)
		r, err := cli.NetworkCreate(context.Background(), probeNet, network.CreateOptions{
			Driver: harness.DriverName,
			IPAM:   &network.IPAM{Driver: "null"},
			Options: map[string]string{
				"mode":          "macvlan",
				"parent":        parentName,
				"validate_dhcp": "true",
			},
		})
		if err == nil {
			// Should not happen on an isolated dummy, but if it does the
			// network must not outlive this test.
			_ = cli.NetworkRemove(context.Background(), r.ID)
		}
	}()

	// Wait for the probe to actually be holding the parent, rather than
	// assuming it is. Its child link is the evidence, and it is created
	// after the gate is taken — so seeing it means the gate is held.
	if !awaitProbeLink(t, 30*time.Second) {
		<-probeDone
		t.Fatalf("no probe link appeared on %s within 30s; the collision window never "+
			"opened, so nothing below would be measuring the gate", parentName)
	}
	t.Logf("probe link present after %v; the parent is held", time.Since(probeStart))

	// The contender, straight at the plugin socket.
	epID := harness.NewEndpointID(t)
	contendStart := time.Now()
	_, epErr := drv.CreateEndpoint(ctx, netID, epID)
	contended := time.Since(contendStart)
	t.Cleanup(func() { drv.CleanupEndpoint(netID, epID) })
	// The endpoint fails: there is no DHCP server on a dummy parent. The
	// gate runs long before that, which is the whole point — this test
	// is about where it waited, not whether it got a lease.
	t.Logf("contending CreateEndpoint returned after %v (err=%v)", contended, epErr)

	<-probeDone

	after, _ := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.ParentLinkWaits+now.ParentLinkWaitTimeouts >
			before.ParentLinkWaits+before.ParentLinkWaitTimeouts
	})
	w.End()
	if after == nil {
		t.Fatalf("no health snapshot after the contention; nothing below can be judged")
	}

	waits := after.ParentLinkWaits - before.ParentLinkWaits
	timeouts := after.ParentLinkWaitTimeouts - before.ParentLinkWaitTimeouts
	t.Logf("parent_link_waits +%d, parent_link_wait_timeouts +%d", waits, timeouts)

	// Either counter proves the gate did its job: the contender found
	// the parent held and queued instead of going straight to the
	// kernel.
	//
	// Which of the two it lands on is decided by two constants, not by
	// timing. An isolated probe runs for its full 8s budget and
	// parentGateBudget is 4s, so the contender queues, exhausts the
	// budget and proceeds — measured here as parent_link_wait_timeouts
	// +1, with the probe's link visible 51ms after the window opened.
	// A margin of two orders of magnitude, and the direction that
	// matters is fixed: 8s > 4s.
	//
	// # What this does NOT prove
	//
	// The version of this test that contended two reclaims also asserted
	// parent_link_wait_timeouts stayed FLAT — that the budget was wide
	// enough to cover a normal holder's duration. That assertion has no
	// subject here and is deliberately not faked: this construction
	// produces a timeout by design, because the only way to make a probe
	// hold a parent for a usefully long time is to point it at a parent
	// with no DHCP server, which is also the slowest a probe can be.
	//
	// The wait-without-timeout path is covered where it can be driven
	// exactly — TestParentGate_SerialisesOneParent and
	// TestParentGate_BudgetExpiryCountsAndProceeds in pkg/plugin, which
	// hold the gate for a duration they choose. What is left for this
	// test is the half a unit test cannot reach: that a real endpoint
	// arriving at a real parent, through the real plugin, is serialised
	// against whatever is already holding it.
	//
	// Asserted as a sum for that reason. Pinning one counter would make
	// this fail when preflightProbeBudget or parentGateBudget moves,
	// which is a different subject from whether the gate engaged.
	if waits+timeouts < 1 {
		t.Errorf("neither parent_link_waits nor parent_link_wait_timeouts advanced while " +
			"an endpoint was created on a parent a probe was holding. The endpoint went " +
			"straight to the kernel: nothing serialised it, and a cross-mode caller in " +
			"its place would have been refused with EBUSY on an operation the user did " +
			"not cause (#486/#549)")
	}

	// Leave the parent as we found it, so the dummy can be deleted and
	// no child of ours outlives the test.
	if !awaitProbeLinksGone(t, 30*time.Second) {
		t.Errorf("probe link(s) still on %s after the probe returned; the deferred "+
			"LinkDel did not run, and a child left on a parent blocks the other mode",
			parentName)
	}
}

// awaitProbeLink waits for the preflight probe's temporary child to
// appear, which is the observable that says the gate is now held: the
// gate is taken at the top of runDHCPProbe and the link is created
// inside it.
//
// Reports rather than fails, so the caller can drain the probe goroutine
// before ending the test.
func awaitProbeLink(t *testing.T, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if len(probeLinks(t)) > 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// awaitProbeLinksGone is its counterpart for teardown.
func awaitProbeLinksGone(t *testing.T, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if len(probeLinks(t)) == 0 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func probeLinks(t *testing.T) []string {
	t.Helper()
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	var found []string
	for _, l := range links {
		if name := l.Attrs().Name; len(name) >= 9 && name[:9] == "dh-probe-" {
			found = append(found, name)
		}
	}
	return found
}
