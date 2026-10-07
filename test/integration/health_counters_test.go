// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"
)

// TestHealthCounters_ObtainedAndReleased checks that a clean lifecycle raises leases_obtained and leaves client_stop_failures alone.
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

	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained", "client_stop_failures")
	before := w.Before()
	t.Logf("before: leases_obtained=%d leases_renewed=%d dhcp_timeouts=%d client_stop_failures=%d",
		before.LeasesObtained, before.LeasesRenewed, before.DHCPTimeouts, before.ClientStopFailures)

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	create, err := cli.ContainerCreate(ctx,
		docker.ContainerCreateOptions{Config: &container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		}, HostConfig: harness.HostConfig(), NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		}, Name: ctrName})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = cli.ContainerRemove(bg, id, docker.ContainerRemoveOptions{Force: true})
	})

	if _, err := cli.ContainerStart(ctx, id, docker.ContainerStartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	// Only the persistent client started in Join feeds leases_obtained; the one-shot client in CreateEndpoint does not.
	budget := harness.IPAcquisitionBudget + 5*time.Second
	afterStart, ok := w.Await(budget, func(now, before *harness.HealthResponse) bool {
		return now.LeasesObtained > before.LeasesObtained
	})
	if !ok {
		t.Fatalf("leases_obtained did not advance within %v (before=%d, last seen=%+v)",
			budget, before.LeasesObtained, afterStart)
	}
	t.Logf("after start: leases_obtained=%d (advanced by %d)",
		afterStart.LeasesObtained, afterStart.LeasesObtained-before.LeasesObtained)

	// With no release_lease the default `never` sends no release since #800, so a clean stop moves only the client's
	// counter; an `on_stop` release is counted in releases_sent (#962).
	if _, err := cli.ContainerStop(ctx, id, docker.ContainerStopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if _, err := cli.ContainerRemove(ctx, id, docker.ContainerRemoveOptions{Force: false}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}

	_, after := w.End()
	t.Logf("after teardown: client_stop_failures=%d", after.ClientStopFailures)

	if after.ClientStopFailures != before.ClientStopFailures {
		t.Errorf("client_stop_failures advanced on a clean teardown: before=%d after=%d",
			before.ClientStopFailures, after.ClientStopFailures)
	}
}
