// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// A Join for an endpoint that already has a manager displaces it, registerDHCPManager returning the incumbent (#682).
// An endpoint untouched across a plugin disable/enable is adopted by the recovery walk with no Leave in between, and a
// disabled plugin has no socket to receive one; harness.DriverClient then issues the Join with no Leave by
// construction. Since 2.0 the client is an AF_PACKET socket inside the plugin, so one client on the interface is read
// from /proc/net/packet in the container's netns; displaced_stops only says the plugin asked.

// TestDisplacedClient_TheInterfaceNeverCarriesTwoClients checks that a Join with no preceding Leave leaves exactly one DHCPv4 client on the container's interface (#682).
func TestDisplacedClient_TheInterfaceNeverCarriesTwoClients(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-displaced"
		ctrName = "dh-itest-displaced-ctr"
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

	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ip, _ := harness.RunContainer(t, ctx, netName, ctrName)
	if ip == "" {
		t.Fatal("the container came up with no IPv4 address; there is no client to displace")
	}

	ifIndex := containerLinkIfIndex(t, ctx, id)
	t.Logf("container %s holds %s on ifindex %d", ctrName[:12], ip, ifIndex)

	// The persistent client starts after Join returns, once a goroutine resolves the container id through the Docker API;
	// the socket appeared 358ms after RunContainer returned on run 34597851110 (#682). The wait is reported whether or
	// not it was spent, so a regression inside the budget stays visible.
	censusStart := time.Now()
	n := len(dhcpv4Sockets(t, ctx, id, ifIndex))
	settle := censusStart.Add(harness.IPAcquisitionBudget)
	for n != 1 && time.Now().Before(settle) {
		time.Sleep(500 * time.Millisecond)
		n = len(dhcpv4Sockets(t, ctx, id, ifIndex))
	}
	t.Logf("SETTLE: the pre-check census read %d DHCPv4 socket(s) after %s of its %s budget",
		n, time.Since(censusStart).Round(10*time.Millisecond), harness.IPAcquisitionBudget)
	if n != 1 {
		t.Fatalf("the running container's namespace holds %d DHCPv4 client socket(s) %s after "+
			"the address was reported and before anything was displaced, want exactly 1. This "+
			"observer cannot judge the displacement until it can see the ordinary case:\n  %s",
			n, harness.IPAcquisitionBudget, harness.DescribePacketSockets(allSockets(t, ctx, id)))
	}

	w := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok", "displaced_stops").ExpectRecycle()

	// The recovery walk registers a manager for the live endpoint, the incumbent the Join below displaces.
	if err := cliReset(ctx, t); err != nil {
		t.Fatalf("plugin recycle: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	recovered := harness.WaitPluginHealthFor(t, ctx, cli, 30*time.Second,
		"recovery to adopt the endpoint whose container never stopped",
		func(h *harness.HealthResponse) bool { return h.RecoveredOK >= 1 })
	if recovered.RecoveryFailed != 0 {
		t.Fatalf("recovery_failed=%d after the recycle: the incumbent this test displaces "+
			"was never built, so the rest of this test would measure nothing",
			recovered.RecoveryFailed)
	}

	// A recycle that left the old process's socket behind would make the count two for a reason other than displacement.
	if got := dhcpv4Sockets(t, ctx, id, ifIndex); len(got) != 1 {
		t.Fatalf("after the recycle the container's namespace holds %d DHCPv4 client "+
			"socket(s), want exactly 1: %s", len(got), harness.DescribePacketSockets(got))
	}

	endpointID := endpointIDOf(t, ctx, cli, id, netName)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, id)

	drv := harness.NewDriverClient(t, ctx, cli)
	joinErr := drv.Join(ctx, netID, endpointID, sandboxKey)

	// A Join with no hint reacquires the endpoint and builds a new link with its MAC, which collides with the incumbent's
	// live link and is refused before registerDHCPManager, measured on the lane 2026-09-09 (#682); both outcomes keep one
	// client on the interface.
	var displacedStops int32
	if joinErr == nil {
		after := harness.WaitPluginHealthFor(t, ctx, cli, 30*time.Second,
			"displaced_stops to record the incumbent being stopped",
			func(h *harness.HealthResponse) bool { return h.DisplacedStops >= 1 })
		displacedStops = after.DisplacedStops
		t.Logf("the Join displaced the incumbent: displaced_stops=%d", displacedStops)
	} else {
		if !strings.Contains(joinErr.Error(), "reacquire endpoint after restart") {
			t.Fatalf("the Join failed for a reason this test did not build: %v\n"+
				"  The construction is a Join with no preceding Leave on a live endpoint; "+
				"anything else means the window was never opened.", joinErr)
		}
		t.Logf("the Join was refused before it could displace, by the reacquisition "+
			"guard: %v", joinErr)
	}

	// Displacement stops the incumbent on a goroutine, so one client is the settled state and the count is the failure.
	var got []harness.PacketSocket
	deadline := time.Now().Add(45 * time.Second)
	for {
		got = dhcpv4Sockets(t, ctx, id, ifIndex)
		if len(got) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Errorf("the container's interface carries %d DHCPv4 client socket(s) 45s after the "+
			"displacing Join, want exactly 1: %s\n"+
			"  displaced_stops=%d. Two "+
			"sockets on one interface is two clients renewing one lease, which is the "+
			"collision the displacement code exists to prevent (#682).",
			len(got), harness.DescribePacketSockets(got), displacedStops)
	}

	// A displacement that took the surviving client with it would read as one for a moment and then zero.
	if now := containerAddr(t, ctx, id); now != ip {
		t.Errorf("the container's address changed from %s to %s across the Join; the "+
			"surviving client was supposed to be the one holding this lease", ip, now)
	}
	if final := dhcpv4Sockets(t, ctx, id, ifIndex); len(final) != 1 {
		t.Errorf("the container ended the test with %d DHCPv4 client socket(s), want 1: %s",
			len(final), harness.DescribePacketSockets(final))
	}

	// Closed before any later test recycles the plugin under it.
	w.End()
}

// Read from /sys, not derived from a name, since #125 and #218 change the name.

// containerLinkIfIndex reads the ifindex of the container's non-loopback link from inside the container.
func containerLinkIfIndex(t *testing.T, ctx context.Context, ctrID string) int {
	t.Helper()
	out := harness.ExecOutput(t, ctx, ctrID,
		"sh", "-c", "for d in /sys/class/net/*; do [ \"$d\" = /sys/class/net/lo ] || cat $d/ifindex; done")
	fields := strings.Fields(out)
	if len(fields) != 1 {
		t.Fatalf("the container has %d non-loopback link(s), want exactly 1 (got %q); the "+
			"socket census below would not know which one to judge", len(fields), out)
	}
	idx, err := strconv.Atoi(fields[0])
	if err != nil || idx <= 0 {
		t.Fatalf("could not read the container link's ifindex from %q: %v", out, err)
	}
	return idx
}

// allSockets returns every AF_PACKET socket in the container's namespace, for failure messages.
func allSockets(t *testing.T, ctx context.Context, ctrID string) []harness.PacketSocket {
	t.Helper()
	rows, err := harness.PacketSocketsFromProc(
		harness.ExecOutput(t, ctx, ctrID, "cat", "/proc/net/packet"))
	if err != nil {
		t.Fatalf("reading /proc/net/packet in the container: %v", err)
	}
	return rows
}

// dhcpv4Sockets returns the ETH_P_IP AF_PACKET sockets bound to the container's link, one per DHCPv4 client.
func dhcpv4Sockets(t *testing.T, ctx context.Context, ctrID string, ifIndex int) []harness.PacketSocket {
	t.Helper()
	return harness.PacketSocketsOn(allSockets(t, ctx, ctrID), harness.EthPIP, ifIndex)
}

// endpointIDOf returns the endpoint id Docker gave the container on netName, the key of the plugin's registry.
func endpointIDOf(t *testing.T, ctx context.Context, cli *docker.Client, ctrID, netName string) string {
	t.Helper()
	ins, err := cli.ContainerInspect(ctx, ctrID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	ep, ok := ins.NetworkSettings.Networks[netName]
	if !ok {
		t.Fatalf("container is not attached to %s", netName)
	}
	if ep.EndpointID == "" {
		t.Fatalf("docker reports no endpoint id for %s on %s", ctrID, netName)
	}
	return ep.EndpointID
}
