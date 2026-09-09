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

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// TestDisplacedClient_TheDisplacedClientLeavesTheInterface drives the
// displacement path for real and asserts on the kernel rather than on
// the plugin's opinion of itself (#682).
//
// # The path
//
// A Join for an endpoint the plugin already has a manager for displaces
// that manager: registerDHCPManager returns the incumbent and Join
// stops it on a goroutine. Reaching it needs a Join with NO PRECEDING
// LEAVE to the running plugin instance, which is described in
// pkg/plugin/network.go as "plugin restart racing a container restart".
//
// # The ordering, and why this test builds the window instead of racing
//
// OBSERVED, by this test, on every run: an endpoint whose container is
// untouched across a plugin disable/enable survives into the next
// plugin process and is adopted by the recovery walk — recovered_ok
// moves and no Leave is delivered in between, because there is nothing
// to deliver one for. That is the first half of the state the issue
// describes, and it is deterministic.
//
// The other half — a container restart WHILE the plugin is down — is
// not needed to reach the path and is not driven here. libnetwork
// delivers Leave as an HTTP call to the plugin's own socket, and a
// disabled plugin has no socket, so such a restart cannot deliver a
// Leave to any plugin instance; the endpoint survives to the next start
// exactly as it does here, and the Join that follows is the displacing
// one. The issue's second bullet allows precisely this: "If only as a
// race, the test must create the window rather than wait for it."
// harness.DriverClient is what creates it — a Join issued straight to
// the plugin socket, with no Leave before it, by construction.
//
// # The evidence
//
// Since 2.0 the DHCP client is a goroutine inside the plugin, not a
// dhcpcd process, so "one client on the interface" cannot be answered
// from the process table any more. It is answered from
// /proc/net/packet in the CONTAINER's network namespace: the client
// speaks DHCP over an AF_PACKET socket bound to the container link, and
// the kernel lists every such socket. See harness.PacketSocket for why
// the DHCP server's log cannot settle this — a stopped client and a
// client between renewals send the same nothing.
//
// displaced_stops is asserted too, as the secondary it is: it says the
// plugin ASKED the old client to stop.
func TestDisplacedClient_TheDisplacedClientLeavesTheInterface(t *testing.T) {
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

	// THE OBSERVER IS PROVEN BEFORE IT IS TRUSTED. A census that reads
	// zero here is blind — a wrong interface index, a client that never
	// opened its socket, a /proc/net/packet this kernel does not fill —
	// and every "exactly one" below would then pass over a plugin that
	// left two clients running.
	if n := len(dhcpv4Sockets(t, ctx, id, ifIndex)); n != 1 {
		t.Fatalf("the running container's namespace holds %d DHCPv4 client socket(s) before "+
			"anything was displaced, want exactly 1. This observer cannot judge the "+
			"displacement until it can see the ordinary case:\n  %s",
			n, harness.DescribePacketSockets(allSockets(t, ctx, id)))
	}

	w := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok", "displaced_stops").ExpectRecycle()

	// The recycle. Its product is a manager registered by the recovery
	// walk for a live endpoint — the incumbent the Join below displaces.
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

	// The recovered client is on the interface, and it is the only one.
	// A recycle that left the old process's socket behind would make
	// the count below two for a reason that has nothing to do with
	// displacement.
	if got := dhcpv4Sockets(t, ctx, id, ifIndex); len(got) != 1 {
		t.Fatalf("after the recycle the container's namespace holds %d DHCPv4 client "+
			"socket(s), want exactly 1: %s", len(got), harness.DescribePacketSockets(got))
	}

	endpointID := endpointIDOf(t, ctx, cli, id, netName)
	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, id)

	// The displacing Join. No Leave precedes it, which is the whole
	// construction: through Docker this state is only reachable as a
	// race, and the driver client makes it a sequence.
	drv := harness.NewDriverClient(t, ctx, cli)
	if err := drv.Join(ctx, netID, endpointID, sandboxKey); err != nil {
		t.Fatalf("the displacing Join was refused: %v", err)
	}

	// The secondary, first, because it is what says the path was
	// reached at all: without it a green run below could mean the Join
	// never displaced anything.
	after := harness.WaitPluginHealthFor(t, ctx, cli, 30*time.Second,
		"displaced_stops to record the incumbent being stopped",
		func(h *harness.HealthResponse) bool { return h.DisplacedStops >= 1 })
	t.Logf("displaced_stops=%d", after.DisplacedStops)

	// THE PRIMARY. displaced_stops says the plugin asked. This says the
	// client went.
	//
	// Polled rather than read once: Join stops the incumbent on a
	// goroutine, so "one client" is the settled state and not an
	// instant. The budget is generous and the failure is the count, so
	// a slow stop and a stop that never happened are distinguished by
	// the message rather than by the clock.
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
			"  displaced_stops=%d, so the plugin believes it stopped the incumbent. Two "+
			"sockets on one interface is two clients renewing one lease, which is the "+
			"collision the displacement code exists to prevent (#682).",
			len(got), harness.DescribePacketSockets(got), after.DisplacedStops)
	}

	// And the endpoint still works: a displacement that took the
	// surviving client with it would also read as "exactly one" for a
	// moment and then as zero.
	if now := containerAddr(t, ctx, id); now != ip {
		t.Errorf("the container's address changed from %s to %s across the displacement; "+
			"the surviving client was supposed to be the one holding this lease", ip, now)
	}
	if final := dhcpv4Sockets(t, ctx, id, ifIndex); len(final) != 1 {
		t.Errorf("the container ended the test with %d DHCPv4 client socket(s), want 1: %s",
			len(final), harness.DescribePacketSockets(final))
	}

	// Closed here rather than deferred, so it runs before any later
	// test recycles the plugin under it.
	w.End()
}

// containerLinkIfIndex reads the ifindex of the container's own
// non-loopback link, from inside the container.
//
// Read from /sys rather than derived from a name: the interface is
// eth0 today and the endpoint-name work (#125, #218) is about changing
// exactly that, so a test keyed on the name would start failing for a
// reason that is not its subject.
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

// allSockets is every AF_PACKET socket in the container's namespace,
// for a failure message that has to say what WAS there.
func allSockets(t *testing.T, ctx context.Context, ctrID string) []harness.PacketSocket {
	t.Helper()
	rows, err := harness.PacketSocketsFromProc(
		harness.ExecOutput(t, ctx, ctrID, "cat", "/proc/net/packet"))
	if err != nil {
		t.Fatalf("reading /proc/net/packet in the container: %v", err)
	}
	return rows
}

// dhcpv4Sockets is the ETH_P_IP AF_PACKET sockets bound to the
// container's link: one per DHCPv4 client on that interface.
func dhcpv4Sockets(t *testing.T, ctx context.Context, ctrID string, ifIndex int) []harness.PacketSocket {
	t.Helper()
	return harness.PacketSocketsOn(allSockets(t, ctx, ctrID), harness.EthPIP, ifIndex)
}

// endpointIDOf reads the endpoint id Docker gave this container on the
// named network. It is the id the plugin's own registry is keyed on, so
// a Join carrying it lands on the incumbent manager rather than
// creating a second endpoint.
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
