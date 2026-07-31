//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
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

	before, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (before): %v", err)
	}
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
	deadline := time.Now().Add(orphanReleaseBudget)
	var after *harness.HealthResponse
	for time.Now().Before(deadline) {
		after, err = harness.PluginHealth(ctx, cli)
		if err != nil {
			t.Fatalf("Plugin.Health (after): %v", err)
		}
		if after.OrphanedLeasesReleased > before.OrphanedLeasesReleased {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

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

	before, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (before): %v", err)
	}
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

	deadline := time.Now().Add(orphanReleaseBudget)
	var after *harness.HealthResponse
	for time.Now().Before(deadline) {
		after, err = harness.PluginHealth(ctx, cli)
		if err != nil {
			t.Fatalf("Plugin.Health (after): %v", err)
		}
		if after.OrphanedLeasesReleased > before.OrphanedLeasesReleased {
			break
		}
		time.Sleep(250 * time.Millisecond)
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

	if got := fixture.CountLogLines("DHCPRELEASE") - releasesBefore; got < 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE lines in this window, want at least 1 — "+
			"the plugin counted a release the server never saw", got)
	}
}
