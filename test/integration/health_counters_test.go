// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestHealthCounters_ObtainedAndReleased pins the v0.9.0 / T2-4
// wiring: a clean container lifecycle (create → bound → release →
// remove) advances /Plugin.Health.leases_obtained by at least one
// and leaves client_stop_failures unchanged.
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

	w := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained", "client_stop_failures")
	before := w.Before()
	t.Logf("before: leases_obtained=%d leases_renewed=%d dhcp_timeouts=%d client_stop_failures=%d",
		before.LeasesObtained, before.LeasesRenewed, before.DHCPTimeouts, before.ClientStopFailures)

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
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
		_ = cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	// Wait for the persistent client's `bound` event — that's what
	// bumps leases_obtained. CreateEndpoint's initial DISCOVER runs
	// a one-shot dhcpcd whose events don't feed the plugin's counters;
	// the persistent client started in Join is what we're testing.
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

	// Drive the explicit teardown: ContainerStop -> Leave ->
	// dhcpManager.Stop -> SIGTERM -> the client exits. A clean shutdown
	// must NOT bump client_stop_failures. No release is involved — since
	// #800 the address stays leased — which is why the counter is named
	// for the client and not for the lease.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: false}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}

	_, after := w.End()
	t.Logf("after teardown: client_stop_failures=%d", after.ClientStopFailures)

	if after.ClientStopFailures != before.ClientStopFailures {
		t.Errorf("client_stop_failures advanced on a clean teardown: before=%d after=%d",
			before.ClientStopFailures, after.ClientStopFailures)
	}
}
