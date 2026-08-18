// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// orphanReleaseBudget bounds the wait for the reclaim to land. The
// plugin has to bring up a temporary link, run a full DORA to reacquire
// the binding under the endpoint's identity, then release it — several
// seconds of real protocol traffic, none of it on the container
// teardown path. Generous because the failure this guards against is
// "never happens", not "happens slowly".
const orphanReleaseBudget = 45 * time.Second

// TestOrphanedLease_ReleasedWhenContainerExitsEarly covers #370.
//
// The address a container gets is acquired during endpoint setup by a
// one-shot client that deliberately keeps the binding, so the address
// reported to Docker is still held when the persistent client takes
// over at attach. Releasing it is the persistent client's job. A
// container that exits before that attach completes used to leave
// nobody holding that job, and the address stayed leased upstream until
// it expired on its own.
//
// This ran at 17 of 32 containers in one suite run — with
// lease_release_failures pinned at 0 throughout, because that counter
// only sees releases that were attempted and failed, not releases that
// were never attempted. It surfaced as an unrelated test asking for a
// specific address and getting a different one, since an earlier
// container was still holding what it wanted.
//
// # Why no container claims the endpoint under test
//
// There is a container running — it exists only to lend its netns, so
// the sandbox the attach is handed is a real one. What there is not is
// a container claiming the endpoint being tested.
//
// This test used to run a container executing `true`, and said in this
// comment that "the race is therefore not close, which is what makes
// the test stable rather than a coin flip". That claim was wrong, and
// the suite proved it: the sibling test built the same way lost exactly
// that race once #555 repartitioned the shards, and its wire log showed
// the persistent client binding and releasing normally — no orphan, no
// reclaim, nothing to assert on. Container exit versus dhcpcd's DORA is
// a coin flip with a heavy bias, and a biased coin still lands the
// other way eventually.
//
// So the orphan is constructed instead. Join is issued with no
// container behind the endpoint, so the attach's container lookup
// returns util.ErrNoContainer and the address is handed back — on every
// run, in every shard position, with no dependence on how fast anything
// else is. What used to be raced for is now stated.
//
// That path only reclaims because #566 made it reclaim. Before that fix
// it gave up, charged join_start_failures, and leaked the address, so
// this construction produced nothing at all. The hole was found by
// this very rewrite failing to build the state it wanted.
func TestOrphanedLease_ReleasedWhenContainerExitsEarly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName   = "dh-itest-orphanrel"
		holderCtr = "dh-itest-orphanrel-holder"
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

	// A real, running container purely to supply a real netns. The
	// endpoint under test is a separate one that no container ever
	// claims, so the sandbox question answers "present" and only the
	// container lookup can fail — which is the state being constructed.
	holderID, _, _ := harness.RunContainer(t, ctx, netName, holderCtr)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, holderID)

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli, "orphaned_leases_released")
	before := w.Before()
	logLinesBefore := harness.CountPluginLogLines(t, ctx, "Released orphaned lease")

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
	// below would be about an address the server never handed out.
	if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s; the endpoint never took a "+
			"lease, so this run proves nothing about releasing one", ip)
	}
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	// Orphan it. The sandbox is real and present — it belongs to the
	// holder container — and this endpoint is claimed by nobody, so the
	// container lookup is the only thing that can fail the attach and
	// nobody is left responsible for the address. Using a live netns
	// rather than a made-up path matters: a missing one would fail the
	// attach for a second, different reason and charge a different
	// counter, so the test would stop distinguishing them (#573).
	if err := drv.Join(ctx, netID, endpointID, sandboxKey); err != nil {
		t.Fatalf("Join(no container): %v", err)
	}

	// Poll the counter rather than sleeping a fixed span: the reclaim is
	// asynchronous by design and its duration is a DHCP round-trip, not
	// a constant.
	after, _ := w.Await(orphanReleaseBudget, func(now, before *harness.HealthResponse) bool {
		return now.OrphanedLeasesReleased > before.OrphanedLeasesReleased
	})
	// Close the window: the reclaim is confirmed, and this also proves
	// the plugin was the same process throughout.
	w.End()

	// Wire-side ground truth first, and it is the assertion that
	// actually matters: a counter can only prove the plugin believes it
	// released something. Only the server's log proves a DHCPRELEASE
	// reached it — and it is keyed on this endpoint's own address, so no
	// neighbouring release can satisfy it by accident.
	deadline := time.Now().Add(orphanReleaseBudget)
	for fixture.CountLogLines("DHCPRELEASE", ip)-releasesBefore < 1 {
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq never logged a DHCPRELEASE for %s within %v — the lease "+
				"is still held upstream with nobody responsible for it (#370)",
				ip, orphanReleaseBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if after == nil || after.OrphanedLeasesReleased <= before.OrphanedLeasesReleased {
		t.Fatalf("orphaned_leases_released did not advance within %v (before=%d, after=%d) — "+
			"the server saw the release but the plugin did not count it",
			orphanReleaseBudget, before.OrphanedLeasesReleased, orphanedAfter(after))
	}

	if got := after.OrphanedLeaseReleaseFailures - before.OrphanedLeaseReleaseFailures; got != 0 {
		t.Errorf("orphaned_lease_release_failures advanced by %d, want 0 — "+
			"the reclaim was attempted but could not complete", got)
	}

	// Plugin-side confirmation, scoped to this window rather than the
	// whole run.
	if got := harness.CountPluginLogLines(t, ctx, "Released orphaned lease") - logLinesBefore; got < 1 {
		t.Errorf("plugin logged %d orphan releases in this window, want at least 1", got)
	}

	awaitReleaseLinksGone(t)
}

// TestOrphanedLease_ReleasedWhenClientNeverBound covers the third state
// #549 found, the one between the two the plugin already knew.
//
// The reclaim above triggers on "the persistent client never started".
// dhcpcd's own `release` covers "it started and held the lease". Neither
// covers "it started and was signalled before it ever bound" — dhcpcd
// releases only a binding it holds, so it sends nothing, and because
// Start succeeded the reclaim was skipped. The address the one-shot
// acquired stayed held upstream with nobody responsible for it, while
// the audit ledger recorded a release the server never saw.
//
// # Why connect/disconnect rather than a short-lived container
//
// The window is between the attach starting the persistent client and
// that client completing its DORA — seconds wide, and not reachable by
// timing a container's exit, which is what the two tests above do from
// the other side. Connecting a network to an already-running container
// and disconnecting it immediately drives Join and Leave directly, with
// no container lifecycle in between.
//
// # Why this is a sound test even though the race is a race
//
// It does not assert which side of the race it landed on. Both sides
// owe exactly one thing — the server must see a DHCPRELEASE for this
// address — and the assertion is that, keyed on the address so no
// neighbouring container can satisfy it by accident. On unfixed code
// the never-bound side produces no release at all and this goes red; on
// the bound side it passes, as it should, because nothing is wrong
// there. So it can be red only when the bug is real, never on timing.
// The state machine itself is pinned deterministically in the unit test
// (TestStop_NeverBoundClientReclaimsInsteadOfClaimingRelease); this one
// exists to prove the effect on the wire, which no counter can.
//
// Deliberately macvlan: the bug lives in the manager's shutdown path and
// is mode-independent. It was found through an ipvlan test only because
// that test races container exit against attach.
func TestOrphanedLease_ReleasedWhenClientNeverBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-neverbound"
		ctrName = "dh-itest-neverbound-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// A container that stays up, started on no plugin network at all, so
	// the connect below is the only thing that touches DHCP.
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		harness.HostConfig(),
		nil, nil, ctrName,
	)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg := context.Background()
		if err := cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true}); err != nil {
			t.Logf("WARN: ContainerRemove(%s): %v", id, err)
		}
	})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	if err := cli.NetworkConnect(ctx, netName, id, &network.EndpointSettings{}); err != nil {
		t.Fatalf("NetworkConnect: %v", err)
	}

	// Read the address before disconnecting — it is the key every
	// assertion below hangs on, and it is gone from Docker's view the
	// moment the endpoint is removed. One inspect costs milliseconds
	// against a DORA measured in hundreds; the window stays open.
	ip := endpointIPv4(t, ctx, cli, id, netName)

	if err := cli.NetworkDisconnect(ctx, netName, id, false); err != nil {
		t.Fatalf("NetworkDisconnect: %v", err)
	}

	// dnsmasq must have handed this exact address out, or the test is
	// asserting about an address that was never leased and would pass
	// for the wrong reason.
	if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s; the endpoint never took a "+
			"lease, so this run proves nothing about releasing one", ip)
	}

	deadline := time.Now().Add(orphanReleaseBudget)
	for fixture.CountLogLines("DHCPRELEASE", ip) < 1 {
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq never logged a DHCPRELEASE for %s within %v — the "+
				"lease is still held upstream with nobody responsible for it. "+
				"The persistent client was signalled before it bound, so it "+
				"released nothing, and the reclaim that covers a client which "+
				"never started did not run (#549)", ip, orphanReleaseBudget)
		}
		time.Sleep(500 * time.Millisecond)
	}

	awaitReleaseLinksGone(t)
}

// releaseLinks returns the reclaim's temporary links currently on the
// host. The reclaim creates exactly one at a time and removes it in a
// deferred call, so a non-empty result means a reclaim is in flight.
func releaseLinks(t *testing.T) []string {
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
	return found
}

// awaitReleaseLinkPresent blocks until a reclaim's temporary link is on
// the host, and returns its name.
//
// This is the other half of constructing a parent-interface collision,
// and it is what turns "fire the second operation and hope it lands
// inside the window" into "do not fire it until the window is provably
// open". The reclaim holds its link for a full DHCP round trip; this
// returns as soon as the link exists, so the caller enters that window
// near its start rather than at an unknown point in it.
//
// Polled tightly on purpose. The interval is the caller's margin: every
// millisecond spent not noticing the link is a millisecond of the
// reclaim's window spent, and the point of the exercise is to make that
// margin large and known rather than incidental.
func awaitReleaseLinkPresent(t *testing.T, budget time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(budget)
	for {
		if found := releaseLinks(t); len(found) > 0 {
			return found[0]
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no orphan-release link appeared within %v — the reclaim never "+
				"started, so there is no window to collide with. This is a failure to "+
				"CONSTRUCT the scenario, not evidence about the code under test: check "+
				"that the endpoint was actually orphaned (Join against a live sandbox "+
				"that no container claims) before looking anywhere else", budget)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// endpointIPv4 reads the address Docker recorded for this endpoint,
// which is what CreateEndpoint's one-shot leased and therefore the
// address that has to come back.
func endpointIPv4(t *testing.T, ctx context.Context, cli *docker.Client, id, netName string) string {
	t.Helper()

	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	settings, ok := info.NetworkSettings.Networks[netName]
	if !ok {
		t.Fatalf("container is not attached to %q after NetworkConnect", netName)
	}
	if settings.IPAddress == "" {
		t.Fatalf("endpoint on %q reported no IPv4 address", netName)
	}
	return settings.IPAddress
}

// orphanedAfter renders the after-counter for a failure message,
// tolerating the nil that a first-iteration health error would leave.
func orphanedAfter(h *harness.HealthResponse) int32 {
	if h == nil {
		return -1
	}
	return h.OrphanedLeasesReleased
}

// TestOrphanedLease_ReleasedInIpvlanMode covers the half of #402 that is
// not a race.
//
// ipvlan children share the parent NIC's hardware address by kernel
// design, so the MAC recorded for an ipvlan endpoint IS the parent's.
// The release path built its temporary link carrying that address, and
// the kernel's duplicate check tests the parent's own address
// explicitly — so bringing the link up returned EADDRINUSE every single
// time. Not sometimes: an ipvlan orphaned lease had never been released.
//
// That determinism is why this test is here rather than a second
// macvlan one: it fails on every run against the unfixed code and
// passes on every run against the fixed one, which is what a negative
// control has to do to be worth anything.
//
// The FAULT was always deterministic. Reaching it was not — this test
// used to orphan its lease by racing a container's exit against the
// persistent client's DORA, so a deterministic negative control sat
// behind a probabilistic setup. It is now constructed the same way as
// the macvlan test above: Join against a live sandbox that no container
// claims, so the container lookup is the only thing that can fail.
//
// Same shape as that test, and the assertion that matters is the same:
// the DHCP server's own log, not a counter. A counter can only say the
// plugin believes it released something.
func TestOrphanedLease_ReleasedInIpvlanMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName   = "dh-itest-orphanrel-ipvlan"
		holderCtr = "dh-itest-orphanrel-ipvlan-holder"
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

	// A real, running container purely to supply a real netns. The
	// endpoint under test is a separate one that no container ever
	// claims, so the sandbox question answers "present" and only the
	// container lookup can fail — which is the state being constructed.
	holderID, _, _ := harness.RunContainer(t, ctx, netName, holderCtr)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, holderID)

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli, "orphaned_leases_released")
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
		t.Fatalf("dnsmasq logged no DHCPACK for %s; the endpoint never took a "+
			"lease, so this run proves nothing about releasing one", ip)
	}
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	if err := drv.Join(ctx, netID, endpointID, sandboxKey); err != nil {
		t.Fatalf("Join(no container): %v", err)
	}

	after, _ := w.Await(orphanReleaseBudget, func(now, before *harness.HealthResponse) bool {
		return now.OrphanedLeasesReleased > before.OrphanedLeasesReleased
	})
	// Close the window: the reclaim is confirmed, and this also proves
	// the plugin was the same process throughout.
	w.End()

	// The server's log first, deliberately. This assertion used to sit
	// below the counter check and was unreachable whenever the counter
	// failed — so a run where the lease genuinely leaked reported
	// "orphaned_leases_released did not advance", a statement about the
	// plugin's bookkeeping, and never got as far as saying what actually
	// happened on the wire (#549). Evidence before opinion.
	deadline := time.Now().Add(orphanReleaseBudget)
	for fixture.CountLogLines("DHCPRELEASE", ip)-releasesBefore < 1 {
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq never logged a DHCPRELEASE for %s within %v — an ipvlan "+
				"orphaned lease is still held upstream (#402)", ip, orphanReleaseBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if after == nil || after.OrphanedLeasesReleased <= before.OrphanedLeasesReleased {
		t.Fatalf("orphaned_leases_released did not advance within %v (before=%d, after=%d) — "+
			"the server saw the release but the plugin did not count it",
			orphanReleaseBudget, before.OrphanedLeasesReleased, orphanedAfter(after))
	}

	// The specific failure #402 is about. Before the fix this is where
	// the test lands: the reclaim is attempted, the temporary link
	// cannot come up because the parent's own MAC is in use, and the
	// failure counter advances instead.
	if got := after.OrphanedLeaseReleaseFailures - before.OrphanedLeaseReleaseFailures; got != 0 {
		t.Errorf("orphaned_lease_release_failures advanced by %d, want 0 — "+
			"the reclaim was attempted but could not complete (#402)", got)
	}

	awaitReleaseLinksGone(t)
}

// TestOrphanedLease_DualStackReleasesBothFamilies is the IPv6 half of
// TestOrphanedLease_ReleasedWhenContainerExitsEarly (#608).
//
// A dual-stack endpoint's one-shot takes two addresses. Until #608 an
// orphan handed back only the IPv4 one: the reclaim worked from the v4
// address alone, and the ledger wrote the v6 address up as released on
// the strength of the v6 client's clean exit — a claim about a DHCPv6
// RELEASE the server never received. Both halves are asserted here on
// the server's own log, per address, because a counter is exactly what
// let the v6 leak stay invisible: the plugin's counters and ledger read
// clean the whole time.
//
// Same construction as the v4 test — a real sandbox, an endpoint no
// container claims — so the only thing that differs is `ipv6=true`.
func TestOrphanedLease_DualStackReleasesBothFamilies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName   = "dh-itest-orphan6"
		holderCtr = "dh-itest-orphan6-holder"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	holderID, _, _ := harness.RunContainer(t, ctx, netName, holderCtr)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, holderID)

	drv := harness.NewDriverClient(t, ctx, cli)

	w := harness.BeginCounterWindow(t, ctx, cli, "orphaned_leases_released")
	before := w.Before()

	endpointID := harness.NewEndpointID(t)
	addrs, err := drv.CreateEndpoint(ctx, netID, endpointID)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	t.Cleanup(func() { drv.CleanupEndpoint(netID, endpointID) })

	ip4 := addressOnly(addrs.Address)
	ip6 := addressOnly(addrs.AddressIPv6)
	if ip4 == "" || ip6 == "" {
		t.Fatalf("CreateEndpoint returned v4=%q v6=%q; want both families leased", addrs.Address, addrs.AddressIPv6)
	}

	// Both addresses must actually have been leased by the server, or
	// the release assertions below would be about addresses it never
	// handed out. dnsmasq logs the v6 grant as DHCPREPLY.
	if got := fixture.CountLogLines("DHCPACK", ip4); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s; the v4 lease was never taken", ip4)
	}
	if got := fixture.CountLogLines("DHCPREPLY", ip6); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPREPLY for %s; the v6 lease was never taken", ip6)
	}
	rel4Before := fixture.CountLogLines("DHCPRELEASE", ip4)
	rel6Before := fixture.CountLogLines("DHCPRELEASE", ip6)

	// Orphan it — see the v4 test for why a live sandbox and no
	// container is the state being constructed.
	if err := drv.Join(ctx, netID, endpointID, sandboxKey); err != nil {
		t.Fatalf("Join(no container): %v", err)
	}

	// One reclaim, two releases: the counter is per address, so it must
	// advance by two before the window closes.
	after, _ := w.Await(2*orphanReleaseBudget, func(now, before *harness.HealthResponse) bool {
		return now.OrphanedLeasesReleased-before.OrphanedLeasesReleased >= 2
	})
	w.End()

	// Wire-side ground truth, per family and per address. The v6 line
	// is the one #608 exists for: on unfixed code the plugin counts one
	// release, the ledger says two, and dnsmasq has seen only the v4
	// one.
	deadline := time.Now().Add(orphanReleaseBudget)
	for fixture.CountLogLines("DHCPRELEASE", ip4)-rel4Before < 1 ||
		fixture.CountLogLines("DHCPRELEASE", ip6)-rel6Before < 1 {
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq DHCPRELEASE lines within %v: v4 %s +%d, v6 %s +%d — want both ≥1; "+
				"a family at 0 is a lease still held upstream with nobody responsible for it (#608)",
				orphanReleaseBudget,
				ip4, fixture.CountLogLines("DHCPRELEASE", ip4)-rel4Before,
				ip6, fixture.CountLogLines("DHCPRELEASE", ip6)-rel6Before)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if after == nil || after.OrphanedLeasesReleased-before.OrphanedLeasesReleased < 2 {
		t.Fatalf("orphaned_leases_released advanced by %d, want 2 (one per family) — "+
			"the server saw both releases but the plugin did not count them",
			orphanedAfter(after)-before.OrphanedLeasesReleased)
	}
	if got := after.OrphanedLeaseReleaseFailures - before.OrphanedLeaseReleaseFailures; got != 0 {
		t.Errorf("orphaned_lease_release_failures advanced by %d, want 0", got)
	}

	awaitReleaseLinksGone(t)
}
