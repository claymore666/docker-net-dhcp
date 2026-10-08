// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestIgnoreConflicts_SecondNetworkOnOneBridge checks that a second bridge-mode network on a bridge another plugin
// network names is refused at create, and created with -o ignore_conflicts=true, where a container on it leases. The
// refusal is the control: without it, the created network would show nothing about the option (#903, docs/reference.md,
// "Bridge already in use").
func TestIgnoreConflicts_SecondNetworkOnOneBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		firstNet   = "dh-itest-ic-first"
		refusedNet = "dh-itest-ic-refused"
		secondNet  = "dh-itest-ic-second"
		ctrName    = "dh-itest-ic-second-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	harness.CreateNetwork(t, ctx, firstNet, "bridge", nil)

	res, createErr := cli.NetworkCreate(ctx, refusedNet, docker.NetworkCreateOptions{
		Driver:  harness.DriverName,
		IPAM:    &network.IPAM{Driver: "null"},
		Options: map[string]string{"mode": "bridge", "bridge": harness.BridgeName},
	})
	if createErr == nil {
		_, _ = cli.NetworkRemove(context.Background(), res.ID, docker.NetworkRemoveOptions{})
		t.Fatalf("a second network on bridge %s was created without ignore_conflicts, so the option below proves nothing", harness.BridgeName)
	}
	if !strings.Contains(strings.ToLower(createErr.Error()), "bridge already in use") {
		t.Fatalf("the second network was refused for the wrong reason, want %q:\n%v", "bridge already in use", createErr)
	}
	t.Logf("✓ refused as documented: %v", createErr)

	harness.CreateNetwork(t, ctx, secondNet, "bridge", map[string]string{"ignore_conflicts": "true"})
	_, ipv4, mac := harness.RunContainer(t, ctx, secondNet, ctrName)
	harness.AssertBridgeIP(t, ipv4)

	// The server's own log, not the plugin's report, says the lease came through the second network.
	if n := fixture.CountBridgeLogLines("DHCPACK", ipv4, mac); n < 1 {
		t.Errorf("the bridge fixture's dnsmasq logged no DHCPACK of %s to %s", ipv4, mac)
	}
}
