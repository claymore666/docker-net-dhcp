// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
)

// The Leave tombstone keeps the MAC and IP for tombstoneTTL, 60 s since v0.6.1, and the restart reuses the endpoint ID (#55).

// TestTombstoneRestart_PreservesMACAndIP checks that `docker restart` keeps the container's MAC and IP.
func TestTombstoneRestart_PreservesMACAndIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-tombstone-net"
	ctrName := "dh-itest-tombstone-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	// A prompt stop is the negative control for #408: the slow-stop opt-out hid the replaced link holding the MAC and the
	// reclaim never running (#402).
	id, ipBefore, macBefore := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("before restart: ip=%s mac=%s", ipBefore, macBefore)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}

	// The endpoint is re-created, so the IP can be empty mid-restart.
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var ipAfter, macAfter string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				ipAfter = ep.IPAddress
				macAfter = ep.MacAddress
			}
		}
		if ipAfter != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ipAfter == "" {
		t.Fatalf("container did not re-acquire an IP within %v after restart", harness.IPAcquisitionBudget)
	}
	t.Logf("after restart:  ip=%s mac=%s", ipAfter, macAfter)

	if macAfter != macBefore {
		t.Errorf("MAC changed across restart: before=%s after=%s (tombstone not honored)", macBefore, macAfter)
	}
	if ipAfter != ipBefore {
		t.Errorf("IP changed across restart: before=%s after=%s (DHCP did not renew the cached lease)", ipBefore, ipAfter)
	}
}
