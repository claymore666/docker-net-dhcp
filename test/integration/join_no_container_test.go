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

// TestJoinNoContainer_AddressIsHeldUntilItExpires covers #566 as #800
// settled it.
//
// CreateEndpoint leases an address with a one-shot client and
// deliberately keeps the binding, so the address is held when the
// persistent client takes over at attach. If the attach then fails
// because no container ever claimed the endpoint, there is no
// persistent client and no container using the address.
//
// #566 made the plugin hand that address back. #800 stopped it, along
// with every other release: the address is held until the lease
// expires, which is what happens to any host on the segment that goes
// away without releasing. What #566 leaves behind is the part that was
// always right — attributing the failure to the correct counter.
//
// # The counter, not the address, is what this test still guards
//
// The original defect had two halves. The address leaked, and — worse
// for an operator — it leaked while `join_start_failures` advanced.
// That counter is Healthy-affecting and means "a RUNNING container has
// no renewal client", so an endpoint nobody ever claimed both kept its
// address and paged somebody about a container that does not exist.
//
// The address half is now the accepted cost of the lease rule, and this
// test asserts the cost is exactly that and no more: no release, and no
// release machinery either. The counter half is unchanged and still
// asserted in both directions.
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
// A counter would say what the plugin believes it did. Only dnsmasq's
// log says what the server saw, and it is keyed on this endpoint's own
// address so no neighbouring endpoint's traffic can satisfy or fail it
// by accident.
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

	// A real, running container purely to supply a real netns. It holds
	// its OWN endpoint on this network; the endpoint under test below is
	// a separate one that no container ever claims, which is what makes
	// the lookup fail while the sandbox question still answers "present".
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

	// Wait on the counter that names this outcome, then let the tree
	// settle. There is no positive event to wait for after that — the
	// subject is an absence — so the settle time is spent in full.
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

	// The counter that used to move instead, and the reason the defect
	// was invisible: join_start_failures is Healthy-affecting and means
	// "a RUNNING container has no renewal client". Nothing is running
	// here, so charging this to it pages an operator about a container
	// that does not exist.
	if got := after.JoinStartFailures - before.JoinStartFailures; got != 0 {
		t.Errorf("join_start_failures advanced by %d, want 0 — an attach with no container "+
			"behind it is not a plugin fault, and counting it as one flips healthy for "+
			"something nobody can act on", got)
	}

	// Ground truth. The address stays leased: no client ever held it to
	// release, and the plugin no longer synthesises one to do it (#800).
	time.Sleep(leaseRetentionSettle)
	if got := fixture.CountLogLines("DHCPRELEASE", ip) - releasesBefore; got != 0 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s, want 0. Nothing this "+
			"plugin runs releases a lease — the address is held until it expires, the "+
			"same as for any host that leaves the segment without releasing (#800)",
			got, ip)
	}

	awaitNoReleaseLinks(t)
}

// joinNoContainerBudget bounds the wait for the attach to give up and
// record its outcome. The attach itself retries the container lookup for
// its whole budget before concluding nobody holds the endpoint, so this
// has to cover that plus the health scrape.
const joinNoContainerBudget = 45 * time.Second

// awaitNoReleaseLinks asserts the parent is clean, and is the structural
// half of the release assertion above.
//
// The removed reclaim did its work by attaching a temporary `dh-rel-*`
// child to the shared parent and holding it for a full DHCP round trip.
// No such link may exist any more — not transiently, not left behind.
//
// Two things are being caught. A reintroduced reclaim, which would show
// up here as a link even if its release never reached the server. And a
// leaked link on the shared parent, which is #486's mechanism: a parent
// carrying a macvlan child cannot accept an ipvlan one, so a link left
// behind hands the next test an EBUSY that looks like its own failure.
//
// Tests share one parent until #556 changes that; until then, leaving it
// as we found it is this test's job.
func awaitNoReleaseLinks(t *testing.T) {
	t.Helper()

	links, err := netlink.LinkList()
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
