// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
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
// The sequence is arranged so the collision is near-certain rather than
// hoped for: both networks and the macvlan container exist before the
// reclaim is triggered, so the only work between the trigger and the
// competing CreateEndpoint is a ContainerStart. The reclaim's own DORA
// against the fixture server takes ~2s, comfortably wider than that gap.
//
// Two assertions, and both are needed:
//
//   - the macvlan container starts. This is the user-visible property
//     and the one that fails without the gate.
//   - parent_link_waits advanced. Without it the test could pass by
//     simply missing the window — a silent no-op that would look
//     identical to a pass. This is the check that the scenario was
//     actually reproduced.
func TestParentGate_ReclaimDoesNotBlockOtherModeOnSameParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		ipvlanNet  = "dh-itest-pgate-ipvlan"
		macvlanNet = "dh-itest-pgate-macvlan"
		orphanCtr  = "dh-itest-pgate-orphan-ctr"
		rivalCtr   = "dh-itest-pgate-rival-ctr"
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
	harness.CreateNetwork(t, ctx, ipvlanNet, "ipvlan", nil)
	harness.CreateNetwork(t, ctx, macvlanNet, "macvlan", nil)

	// The rival container is created but NOT started: its CreateEndpoint
	// is the operation that has to survive, and it must not run yet.
	rival, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "30"},
			Hostname: rivalCtr,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{macvlanNet: {}},
		},
		nil,
		rivalCtr,
	)
	if err != nil {
		t.Fatalf("ContainerCreate(rival): %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		if err := cli.ContainerRemove(bg, rival.ID, container.RemoveOptions{Force: true}); err != nil {
			t.Logf("WARN: ContainerRemove(%s): %v", rival.ID, err)
		}
	})

	w := harness.BeginCounterWindow(t, ctx, cli, "parent_link_waits", "parent_link_wait_timeouts")
	before := w.Before()

	// Trigger the reclaim: a container that exits before its persistent
	// client can bind leaves a lease nobody owns, and the plugin hands
	// it back from a goroutine — attaching an ipvlan child to the shared
	// parent to do it.
	orphan, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"true"},
			Hostname: orphanCtr,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{ipvlanNet: {}},
		},
		nil,
		orphanCtr,
	)
	if err != nil {
		t.Fatalf("ContainerCreate(orphan): %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		if err := cli.ContainerRemove(bg, orphan.ID, container.RemoveOptions{Force: true}); err != nil {
			t.Logf("WARN: ContainerRemove(%s): %v", orphan.ID, err)
		}
	})

	if err := cli.ContainerStart(ctx, orphan.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(orphan): %v", err)
	}

	waitCh, errCh := cli.ContainerWait(ctx, orphan.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		t.Fatalf("ContainerWait(orphan): %v", err)
	case <-waitCh:
	case <-ctx.Done():
		t.Fatalf("orphan container did not exit: %v", ctx.Err())
	}

	// The reclaim is in flight now. Ask the same parent for the other
	// kind, which is exactly what CI did by accident.
	startErr := cli.ContainerStart(ctx, rival.ID, container.StartOptions{})

	after, _ := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.ParentLinkWaits > before.ParentLinkWaits
	})
	w.End()

	if startErr != nil {
		if strings.Contains(strings.ToLower(startErr.Error()), "device or resource busy") {
			t.Fatalf("the reclaim's link blocked an endpoint of the other mode on the same parent: %v\n"+
				"This is the collision the per-parent gate exists to remove — a container start "+
				"refused because an unrelated container had just exited.", startErr)
		}
		t.Fatalf("ContainerStart(rival): %v", startErr)
	}

	if after == nil || after.ParentLinkWaits <= before.ParentLinkWaits {
		got := int32(-1)
		if after != nil {
			got = after.ParentLinkWaits
		}
		t.Fatalf("parent_link_waits did not advance (before=%d, after=%d) — the rival endpoint "+
			"never queued behind the reclaim, so this run did not reproduce the collision and "+
			"proves nothing. The reclaim most likely finished before the rival's CreateEndpoint "+
			"reached the parent; the test needs a wider reclaim window, not a relaxed assertion.",
			before.ParentLinkWaits, got)
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
