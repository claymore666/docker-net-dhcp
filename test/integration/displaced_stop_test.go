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
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

// displacedReleaseBudget covers the displaced manager's Stop: a SIGTERM
// to its dhcpcd, that client's DHCPRELEASE, and dnsmasq writing the line.
// The same size as orphanReleaseBudget, and for the same reason — it is
// one release cycle, not a lease timer.
const displacedReleaseBudget = 45 * time.Second

// TestDisplaced_ReleasesItsLease drives the Join-displaces-recovery
// direction of #480 for real and asserts on the DHCP server's log.
//
// # The claim being gated
//
// docs/reference.md says of the two registration directions: "Both
// directions end with exactly one DHCP client on the interface, which is
// the property that matters (#480)." Only one direction was gated.
// Recovery-yields-to-Join has scripts/check-manager-registration.sh and
// a recovery_already_managed observed in CI. The other direction — a
// Join landing on an endpoint recovery already registered, which stops
// the manager it displaced (network.go, p.displacedStops) — was asserted
// nowhere that reaches the real path. close_test.go exercises that
// WaitGroup with hand-built entries, and displaced_stops appeared in no
// integration test at all (#682).
//
// The counter cannot stand in for the property, and its own help text
// says so: "Counts the intent to stop; it is not evidence the client
// went away." A manager whose Stop silently failed increments it exactly
// like one whose dhcpcd released and exited.
//
// # Why the Join is replayed through the driver socket
//
// The production window is a race, not a sequence. A daemon shutdown
// does not always deliver Leave — docs/reference.md records the address
// surviving "either by recovery (when the daemon's shutdown never called
// Leave) or by the tombstone (when it did)" — so recovery adopts live
// endpoints on the way back up. The deferred retry (#383) then runs
// while a host of restart-policy containers is rejoining, and
// registerDHCPManagerIfAbsent's comment prices that window at
// microseconds per endpoint. Whichever call arrives second decides the
// direction.
//
// A test cannot wait for a microsecond race and call the result a gate.
// It has to build the window, and Docker will not send a second Join for
// an endpoint whose container never left. So the Join is replayed
// through harness.DriverClient with the container's own live sandbox
// key — byte-for-byte the call Docker makes, in an order Docker cannot
// be asked for. That is the narrow case DriverClient's own doc comment
// licenses: the daemon here is only a sequencer.
//
// # What "exactly one client" can and cannot be observed as
//
// It cannot be counted directly. The suite is a sibling PID namespace to
// the plugin, measured on the #785 probe, so no test here can see the
// plugin's dhcpcd processes. Both clients also share one MAC and one
// address, so while both run they are indistinguishable to the server.
//
// The tempting substitute — tear the endpoint down and watch for
// renewals from a leaked client — is a check with one possible verdict.
// Removing the container destroys the macvlan interface, and a leaked
// dhcpcd bound to a destroyed interface goes quiet whether or not it was
// ever stopped. Silence would be reported as proof of the thing it
// cannot distinguish.
//
// So the property is gated as its two observable halves, both on the
// wire or inside the netns, neither from the plugin's own account:
//
//   - the displaced client STOPPED — it sent a DHCPRELEASE, which
//     dhcpcd emits only when told to shut down;
//   - the surviving client is still there and still holds the address —
//     the container's own view of its interface still carries it.
//
// A displaced client that never stopped fails the first. A displacement
// that took the live client down with it fails the second.
//
// **Do not parallelize.** It disables and enables the plugin, which is
// daemon-global state.
func TestDisplaced_ReleasesItsLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-displaced-net"
		ctrName = "dh-itest-displaced-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	ctrID, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container: ip=%s mac=%s", ip, mac)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// The address must actually have been leased, or every assertion
	// below is about an address the server never handed out.
	if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s; this container never took a lease, "+
			"so nothing below is about a real DHCP client", ip)
	}

	endpointID := endpointIDOf(t, ctx, cli, ctrID, netName)

	// Belt-and-braces re-enable, registered before the disable so a
	// t.Fatal anywhere in the recycle still leaves the plugin enabled
	// rather than stranding every later test on this host.
	t.Cleanup(func() {
		bg := context.Background()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})

	// --- phase 1: make recovery adopt this live endpoint ---------------
	//
	// ExpectRecycle is what makes this window honest: counter deltas
	// straddling a plugin restart are void, because the counters are
	// in-memory and die with the process. This window is good for
	// recovered_ok on the FRESH process and for nothing else, which is
	// why displaced_stops is measured in a second window below rather
	// than by subtracting across this one.
	w1 := harness.BeginCounterWindow(t, ctx, cli, "recovered_ok").ExpectRecycle()

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("waiting for plugin to report disabled: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("waiting for plugin to report enabled: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	_, afterRecycle := w1.End()
	if afterRecycle.RecoveredOK < 1 {
		t.Fatalf("recovered_ok=%d after the recycle: recovery did not adopt this endpoint, so "+
			"there is no recovery-built manager for a Join to displace and this test would "+
			"pass by exercising nothing", afterRecycle.RecoveredOK)
	}

	// --- phase 2: the Join that displaces ------------------------------

	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	w2 := harness.BeginCounterWindow(t, ctx, cli, "displaced_stops")
	before := w2.Before()

	sandboxKey := harness.LiveSandboxKey(t, ctx, cli, ctrID)
	drv := harness.NewDriverClient(t, ctx, cli)
	if err := drv.Join(ctx, netID, endpointID, sandboxKey); err != nil {
		t.Fatalf("replayed Join for the recovered endpoint: %v", err)
	}

	after, _ := w2.Await(displacedReleaseBudget, func(now, before *harness.HealthResponse) bool {
		return now.DisplacedStops > before.DisplacedStops
	})
	w2.End()

	// Ground truth first, counters after and never instead. Only the
	// server's log proves the displaced client shut down: dhcpcd sends
	// DHCPRELEASE on its way out, and nothing else in this sequence
	// releases — the surviving client is acquiring, not releasing.
	deadline := time.Now().Add(displacedReleaseBudget)
	for fixture.CountLogLines("DHCPRELEASE", ip)-releasesBefore < 1 {
		if !time.Now().Before(deadline) {
			t.Fatalf("dnsmasq never logged a DHCPRELEASE for %s within %v after the Join "+
				"displaced the recovery-built manager. The displaced client's dhcpcd is "+
				"still running on this interface, untracked and unstoppable, competing "+
				"with the new client for the same address (#682)",
				ip, displacedReleaseBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// The other half of "exactly one client": the survivor is still
	// there. A displacement that stopped BOTH clients would satisfy the
	// release assertion above and leave the container with an address
	// nothing renews.
	if out := harness.ExecOutput(t, ctx, ctrID, "ip", "-4", "addr"); !strings.Contains(out, ip+"/") {
		t.Errorf("after the displacement the container no longer carries %s:\n%s\n"+
			"The displaced manager's Stop took the live client's address with it, so the "+
			"interface ends with zero clients rather than one", ip, out)
	}

	// Secondary. It proves the plugin ATTRIBUTED the stop, which is what
	// an operator reads; the wire above proves it happened.
	if after == nil {
		t.Fatalf("displaced_stops never advanced within %v — the release reached dnsmasq but "+
			"the plugin did not count a displacement, so an operator cannot tell this apart "+
			"from an ordinary lease release", displacedReleaseBudget)
	}
	if got := after.DisplacedStops - before.DisplacedStops; got != 1 {
		t.Errorf("displaced_stops advanced by %d, want exactly 1 — one Join displaced one "+
			"recovery-built manager, so any other number means the registry held something "+
			"this test did not put there", got)
	}
}

// endpointIDOf returns the container's endpoint ID on the named network.
// Fatal rather than empty: an empty endpoint ID would make the replayed
// Join land on an endpoint the plugin has never heard of, which fails
// for a reason that has nothing to do with displacement.
func endpointIDOf(t *testing.T, ctx context.Context, cli *docker.Client, containerID, netName string) string {
	t.Helper()
	ins, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		t.Fatalf("ContainerInspect(%s): %v", containerID, err)
	}
	ep, ok := ins.NetworkSettings.Networks[netName]
	if !ok {
		t.Fatalf("container is not attached to %q (networks: %v)", netName, ins.NetworkSettings.Networks)
	}
	if ep.EndpointID == "" {
		t.Fatalf("container's endpoint on %q has no ID", netName)
	}
	return ep.EndpointID
}
