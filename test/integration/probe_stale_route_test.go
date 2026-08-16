// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"testing"
	"time"

	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestConflictProbe_ReclaimsLeftoverRoute covers #572.
//
// The conflict probe installs a temporary /32 towards the address it is
// checking, so the ARP leaves by the parent instead of by whatever the
// host routing table would otherwise pick — the misrouting that made
// #524 invisible. It removes that route in a deferred call.
//
// The probe runs in a detached goroutine with its own background
// context, so nothing waits for it. If the process goes away inside the
// probe's window — a daemon restart, `docker plugin disable`, an
// upgrade — the deferred removal never runs and the /32 stays on the
// parent. Every later probe for that address then fails at RouteAdd
// with EEXIST and reports "probe could not run", so the address is
// never checked. The detector stops answering and only a counter nobody
// reads says so.
//
// Seen twice in CI on unrelated branches, both times on the shard that
// restarts the daemon, both times as `route <addr> via <parent>: file
// exists` with a cleanup warning on the same parent shortly before.
//
// # Why this does not restart anything
//
// Reproducing through a daemon restart means racing the probe's
// two-second window, which is exactly why the failure was intermittent
// and why two runs disagreed. What a cut-short probe leaves behind is
// only a route, so this test leaves that route itself and then makes
// the plugin probe the same address again.
//
// The second lease is pinned rather than hoped for: the DHCP server
// keys its offers on the client MAC, so asking for the same MAC gets
// the same address back. Without that this would be a test that usually
// constructs its own premise.
func TestConflictProbe_ReclaimsLeftoverRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-staleroute"
		// Locally administered, and fixed so both leases below are the
		// same DHCP client as far as the server is concerned.
		pinnedMAC = "02:42:ac:11:00:99"
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

	drv := harness.NewDriverClient(t, ctx, cli)

	parent := harness.HostVeth
	link, err := netlink.LinkByName(parent)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", parent, err)
	}
	src, err := parentIPv4(link)
	if err != nil {
		t.Fatalf("parent %s has no IPv4 address (%v) — the probe borrows one only when the "+
			"parent has none, so without it this test plants a route of a different shape "+
			"from the one the plugin builds and proves nothing about the real path",
			parent, err)
	}

	// First lease: this is only here to find out which address the
	// pinned MAC gets, so the leftover can be planted for it.
	firstEP := harness.NewEndpointID(t)
	first, err := drv.CreateEndpointWithMAC(ctx, netID, firstEP, pinnedMAC)
	if err != nil {
		t.Fatalf("CreateEndpoint(first): %v", err)
	}
	target := net.ParseIP(addressOnly(first.Address))
	if target == nil {
		t.Fatalf("first endpoint returned no usable IPv4 address (got %q)", first.Address)
	}
	if err := drv.DeleteEndpoint(ctx, netID, firstEP); err != nil {
		t.Fatalf("DeleteEndpoint(first): %v", err)
	}

	// Let the first endpoint's own probe finish and clean up, so the
	// route this test plants is unambiguously the one under test.
	awaitNoRouteFor(t, link, target, 30*time.Second)

	stale := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: target, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
		Src:       src,
	}

	// THIS is the residue a cut-short probe leaves behind. Planting it
	// directly is the whole construction.
	if err := netlink.RouteAdd(stale); err != nil {
		t.Fatalf("could not plant the leftover route towards %s: %v", target, err)
	}
	t.Cleanup(func() {
		// The probe removes it on the way out; absence here is the
		// expected end state, not a failure.
		_ = netlink.RouteDel(stale)
	})

	// It must actually block a fresh add, or the condition CI reported
	// is not what this test has built.
	if err := netlink.RouteAdd(stale); err == nil {
		t.Fatal("a duplicate /32 was accepted, so RouteAdd is not the operation that fails " +
			"on residue and this test no longer reproduces #572")
	}

	w := harness.BeginCounterWindow(t, ctx, cli,
		"conflict_probe_stale_routes", "conflict_probe_failures")
	before := w.Before()

	// Second lease, same MAC, therefore the same address — and its probe
	// walks straight into the leftover.
	secondEP := harness.NewEndpointID(t)
	second, err := drv.CreateEndpointWithMAC(ctx, netID, secondEP, pinnedMAC)
	if err != nil {
		t.Fatalf("CreateEndpoint(second): %v", err)
	}
	t.Cleanup(func() { drv.CleanupEndpoint(netID, secondEP) })

	if got := addressOnly(second.Address); got != target.String() {
		t.Fatalf("the second lease took %s, not %s — the DHCP server did not honour the "+
			"pinned MAC, so the probe under test never met the planted route and this run "+
			"proves nothing. Do not relax this: without the address matching there is no "+
			"construction at all.", got, target)
	}

	after, _ := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.ConflictProbeStaleRoutes > before.ConflictProbeStaleRoutes
	})
	w.End()

	if after == nil || after.ConflictProbeStaleRoutes <= before.ConflictProbeStaleRoutes {
		got := int32(-1)
		if after != nil {
			got = after.ConflictProbeStaleRoutes
		}
		t.Fatalf("conflict_probe_stale_routes did not advance (before=%d, after=%d) — the "+
			"probe met a leftover route and did not report reclaiming it. Either it failed "+
			"outright, which is #572 unfixed, or it recovered silently, which is how a "+
			"detector stops being trustworthy.",
			before.ConflictProbeStaleRoutes, got)
	}

	// The point of the fix: the probe RAN. A reclaim that still ends in
	// a failed probe would leave the address unchecked, which is the
	// whole harm.
	if got := after.ConflictProbeFailures - before.ConflictProbeFailures; got != 0 {
		t.Errorf("conflict_probe_failures advanced by %d, want 0 — the leftover was "+
			"reclaimed but the probe still could not run, so %s was never checked",
			got, target)
	}

	// And it cleaned up after itself, so the next probe for this address
	// does not inherit the same problem.
	awaitNoRouteFor(t, link, target, 30*time.Second)
}

// awaitNoRouteFor waits until no /32 towards target remains on link.
func awaitNoRouteFor(t *testing.T, link netlink.Link, target net.IP, budget time.Duration) {
	t.Helper()

	deadline := time.Now().Add(budget)
	for {
		routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("RouteList(%s): %v", link.Attrs().Name, err)
		}
		found := false
		for _, r := range routes {
			if r.Dst == nil || !r.Dst.IP.Equal(target) {
				continue
			}
			if ones, _ := r.Dst.Mask.Size(); ones == 32 {
				found = true
				break
			}
		}
		if !found {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("a /32 towards %s is still on %s after %v — the probe did not remove "+
				"its temporary route, which is the leak that makes every later probe for "+
				"this address fail (#572)", target, link.Attrs().Name, budget)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// parentIPv4 returns the parent's first IPv4 address, which is the
// source the probe uses when it does not have to borrow one.
func parentIPv4(link netlink.Link) (net.IP, error) {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			return a.IP, nil
		}
	}
	return nil, net.InvalidAddrError("no IPv4 address on " + link.Attrs().Name)
}
