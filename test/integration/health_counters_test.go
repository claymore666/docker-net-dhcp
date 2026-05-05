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

// TestHealthCounters_ObtainedAndReleased pins the v0.9.0 / T2-4
// wiring: a clean container lifecycle (create → bound → release →
// remove) advances /Plugin.Health.leases_obtained by at least one
// and leaves lease_release_failures unchanged.
//
// Inlining ContainerCreate/Start/Stop/Remove instead of using
// harness.RunContainer because Run defers cleanup via t.Cleanup,
// which fires after the test body returns — we need the release
// to happen WITHIN the test so we can take the post-release health
// snapshot before the assertion.
func TestHealthCounters_ObtainedAndReleased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-health-counters"
	ctrName := "dh-itest-health-counters-ctr"

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
	defer cli.Close()

	before, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (before): %v", err)
	}
	t.Logf("before: leases_obtained=%d leases_renewed=%d dhcp_timeouts=%d lease_release_failures=%d",
		before.LeasesObtained, before.LeasesRenewed, before.DHCPTimeouts, before.LeaseReleaseFailures)

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		&container.HostConfig{},
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
		_ = cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	// Wait for the persistent client's `bound` event — that's what
	// bumps leases_obtained. CreateEndpoint's initial DISCOVER runs
	// a one-shot udhcpc that doesn't go through the event handler;
	// the persistent client started in Join is what we're testing.
	deadline := time.Now().Add(harness.IPAcquisitionBudget + 5*time.Second)
	var afterStart *harness.HealthResponse
	for time.Now().Before(deadline) {
		h, err := harness.PluginHealth(ctx, cli)
		if err == nil && h.LeasesObtained > before.LeasesObtained {
			afterStart = h
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if afterStart == nil {
		last, _ := harness.PluginHealth(ctx, cli)
		t.Fatalf("leases_obtained did not advance within %v (before=%d, last seen=%+v)",
			harness.IPAcquisitionBudget+5*time.Second, before.LeasesObtained, last)
	}
	t.Logf("after start: leases_obtained=%d (advanced by %d)",
		afterStart.LeasesObtained, afterStart.LeasesObtained-before.LeasesObtained)

	// Drive the explicit teardown: ContainerStop -> Leave ->
	// dhcpManager.Stop -> SIGTERM -> DHCPRELEASE. A clean release
	// must NOT bump lease_release_failures.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: false}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}

	after, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (after): %v", err)
	}
	t.Logf("after teardown: lease_release_failures=%d", after.LeaseReleaseFailures)

	if after.LeaseReleaseFailures != before.LeaseReleaseFailures {
		t.Errorf("lease_release_failures advanced on a clean teardown: before=%d after=%d",
			before.LeaseReleaseFailures, after.LeaseReleaseFailures)
	}
}
