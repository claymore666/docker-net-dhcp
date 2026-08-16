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
// The container here runs `true`: it exits within milliseconds of
// start, while the persistent-client attach needs to find the container
// PID, enter its netns, locate the link and complete a DHCP exchange.
// The race is therefore not close, which is what makes the test stable
// rather than a coin flip.
func TestOrphanedLease_ReleasedWhenContainerExitsEarly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-orphanrel"
		ctrName = "dh-itest-orphanrel-ctr"
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

	w := harness.BeginCounterWindow(t, ctx, cli, "orphaned_leases_released")
	before := w.Before()
	releasesBefore := fixture.CountLogLines("DHCPRELEASE")
	logLinesBefore := harness.CountPluginLogLines(t, ctx, "Released orphaned lease")

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"true"},
			Hostname: ctrName,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {},
			},
		},
		nil,
		ctrName,
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

	// Wait for the exit so the reclaim window is genuinely open before
	// we start polling — otherwise a slow start could have us give up
	// while the container is still running and holding its address
	// legitimately.
	waitCh, errCh := cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		t.Fatalf("ContainerWait: %v", err)
	case <-waitCh:
	case <-ctx.Done():
		t.Fatalf("container did not exit: %v", ctx.Err())
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

	if after == nil || after.OrphanedLeasesReleased <= before.OrphanedLeasesReleased {
		t.Fatalf("orphaned_leases_released did not advance within %v (before=%d, after=%d) — "+
			"the lease this container acquired is still held upstream",
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

	// Wire-side ground truth, and the assertion that actually matters: a
	// counter can only prove the plugin believes it released something.
	// Only the server's log proves a DHCPRELEASE reached it. Tests run
	// sequentially against the shared fixture, so a delta across this
	// window is attributable to this container.
	if got := fixture.CountLogLines("DHCPRELEASE") - releasesBefore; got < 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE lines in this window, want at least 1 — "+
			"the plugin counted a release the server never saw", got)
	}
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

// awaitReleaseLinksGone waits for the reclaim's temporary link to be
// removed from the shared parent.
//
// Two reasons, and the second is why it is not optional here.
//
// The plugin must not leak the links it creates: the reclaim deletes
// `dh-rel-*` in a deferred call after the release, so a link still
// present long afterwards is a real defect, and nothing else asserts it.
//
// And this test hands the parent straight to the next one. It is macvlan
// and it runs immediately before TestOrphanedLease_ReleasedInIpvlanMode
// in the same shard; the reclaim's link outlives the DHCPRELEASE that
// this test waits for, because deleting it is the step after. A parent
// carrying a macvlan child cannot accept an ipvlan one — that is #486's
// mechanism and #556's residue — so returning early would hand the next
// test an EBUSY that looks like its own failure. Tests share one parent
// until #556 changes that; until then, leaving it as we found it is this
// test's job.
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
			t.Fatalf("orphan-release link(s) %v still on the host after %v; the "+
				"reclaim did not remove them, and a macvlan child left on the "+
				"shared parent blocks the next ipvlan test (#486/#556)", left, budget)
		}
		time.Sleep(250 * time.Millisecond)
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
// macvlan one. The macvlan failure depends on losing a race with
// Docker's teardown, so a test for it passes or fails on timing. This
// one fails on every run against the unfixed code and passes on every
// run against the fixed one, which is what a negative control has to do
// to be worth anything.
//
// Same shape as the macvlan test above, and the assertion that matters
// is the same: the DHCP server's own log, not a counter. A counter can
// only say the plugin believes it released something.
func TestOrphanedLease_ReleasedInIpvlanMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-orphanrel6"
		ctrName = "dh-itest-orphanrel6-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "ipvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	w := harness.BeginCounterWindow(t, ctx, cli, "orphaned_leases_released")
	before := w.Before()
	releasesBefore := fixture.CountLogLines("DHCPRELEASE")

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"true"},
			Hostname: ctrName,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		},
		nil,
		ctrName,
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

	waitCh, errCh := cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		t.Fatalf("ContainerWait: %v", err)
	case <-waitCh:
	case <-ctx.Done():
		t.Fatalf("container did not exit: %v", ctx.Err())
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
	if got := fixture.CountLogLines("DHCPRELEASE") - releasesBefore; got < 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE lines in this window, want at least 1 — "+
			"the lease is still held upstream", got)
	}

	if after == nil || after.OrphanedLeasesReleased <= before.OrphanedLeasesReleased {
		t.Fatalf("orphaned_leases_released did not advance within %v (before=%d, after=%d) — "+
			"an ipvlan orphaned lease is still held upstream",
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
}
