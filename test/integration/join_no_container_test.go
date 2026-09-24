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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Since #800 an endpoint whose attach finds no container keeps its CreateEndpoint lease until it expires, and the
// failure must move join_no_container, never join_start_failures, which pages about a running container (#566). The
// Join uses a live container's netns against an endpoint Docker never attached, so only the container lookup can fail;
// a missing netns path answered "vanished" once #567 made the directory visible. The evidence is dnsmasq's log, keyed
// on this endpoint's address.

// TestJoinNoContainer_AddressIsHeldUntilItExpires checks that an attach nobody claims moves the right counter and sends no release (#566, #800).
func TestJoinNoContainer_AddressIsHeldUntilItExpires(t *testing.T) {
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

	// The holder's own endpoint supplies a real netns; the endpoint under test is one no container claims (#566).
	holderID, _, _ := harness.RunContainer(t, ctx, netName, holderCtr)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, holderID)

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"join_aborted_no_container", "join_start_failures")
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

	if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s; the endpoint never took a lease, "+
			"so this run proves nothing about releasing one", ip)
	}
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	if err := drv.Join(ctx, netID, endpointID, sandboxKey); err != nil {
		t.Fatalf("Join: %v", err)
	}

	// The subject is an absence, so the settle is spent in full.
	after, _ := w.Await(joinNoContainerBudget, func(now, before *harness.HealthResponse) bool {
		return now.JoinAbortedNoContainer > before.JoinAbortedNoContainer
	})
	w.End()

	if after == nil {
		t.Fatalf("no health snapshot after the attach; nothing below can be judged")
	}

	if got := after.JoinAbortedNoContainer - before.JoinAbortedNoContainer; got != 1 {
		t.Errorf("join_aborted_no_container advanced by %d, want 1 — the attach ended for "+
			"this reason but is not attributed to it, so an operator cannot tell an "+
			"endpoint nobody claimed from an endpoint whose start failed", got)
	}

	if got := after.JoinStartFailures - before.JoinStartFailures; got != 0 {
		t.Errorf("join_start_failures advanced by %d, want 0 — an attach with no container "+
			"behind it is not a plugin fault, and counting it as one flips healthy for "+
			"something nobody can act on", got)
	}

	// No client ever held the address, and there is no Leave for an endpoint no container joined, so release_lease cannot apply (#800, #962).
	time.Sleep(leaseRetentionSettle)
	if got := fixture.CountLogLines("DHCPRELEASE", ip) - releasesBefore; got != 0 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s, want 0. Nothing releases "+
			"a lease here — the address is held until it expires, the same as for any "+
			"host that leaves the segment without releasing (#800), and no Leave ever "+
			"runs for this endpoint for release_lease to act on (#962)",
			got, ip)
	}

	awaitNoReleaseLinks(t)
}

// joinNoContainerBudget covers the attach's container-lookup retries for its whole budget plus the health scrape.
const joinNoContainerBudget = 45 * time.Second

// A link would be a reintroduced reclaim, or a leak on the shared parent, where a macvlan child makes the next ipvlan
// child fail with EBUSY (#486, #556).

// awaitNoReleaseLinks asserts that no dh-rel-* reclaim link exists on the parent, transiently or left behind (#800).
func awaitNoReleaseLinks(t *testing.T) {
	t.Helper()

	links, err := util.DumpResult(netlink.LinkList())
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	var found []string
	for _, l := range links {
		if strings.HasPrefix(l.Attrs().Name, "dh-rel-") {
			found = append(found, l.Attrs().Name)
		}
	}
	if len(found) > 0 {
		t.Errorf("release link(s) %v are on the host. The orphaned-lease reclaim that "+
			"created them was removed in v1.9.0 (#800); if something is creating them "+
			"again it is releasing addresses a container may be about to re-claim. "+
			"A macvlan child left on the shared parent also blocks the next ipvlan "+
			"test (#486/#556)", found)
	}
}

// addressOnly strips the prefix length from a CIDR, returning "" when there is nothing to key on.
func addressOnly(cidr string) string {
	if cidr == "" {
		return ""
	}
	if i := strings.IndexByte(cidr, '/'); i >= 0 {
		return cidr[:i]
	}
	return cidr
}
