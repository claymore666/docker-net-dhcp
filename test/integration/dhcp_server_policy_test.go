// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Server selection for dhcp_servers (#111) and dhcp_deny_servers (#669) runs two DHCP servers on one bridge, the only
// place the suite does. Which server leased is read from the address, since the pools are disjoint
// (TestBridgeChallenger_AddressPlanIsUnambiguous), and each server's dnsmasq log says by MAC whom it served; the
// counters are a second statement. Bridge mode only, as the macvlan fixture is a veth pair with one server, and both
// acquisition paths go through acquireWithPolicy.

func policyClient(t *testing.T) *docker.Client {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// Both servers log every DHCPDISCOVER on a shared segment, so only the ACK names the winner.

// ackedIn reports whether a dnsmasq log contains a DHCPACK naming both this address and this MAC.
func ackedIn(logText, ip, mac string) bool {
	for _, line := range strings.Split(logText, "\n") {
		if !strings.Contains(line, "DHCPACK") {
			continue
		}
		if strings.Contains(line, ip) && strings.Contains(strings.ToLower(line), strings.ToLower(mac)) {
			return true
		}
	}
	return false
}

// policyACKBudget bounds the wait for a server to flush an ACK it already sent, a gap well under a second.
const policyACKBudget = 15 * time.Second

// bridgeServerIP returns the primary bridge dnsmasq's address, as a policy names it.
func bridgeServerIP() string { return strings.SplitN(harness.BridgeAddr, "/", 2)[0] }

// assertLeasedBy checks from both servers' logs and the address that want, and only want, served this container.
func assertLeasedBy(t *testing.T, want, ip, mac string) {
	t.Helper()

	primary := bridgeServerIP()
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Fatalf("leased address %q does not parse", ip)
	}
	inPrimary := harness.IsInBridgePool(parsed)
	inChallenger := harness.IsInBridgeChallengerPool(parsed)

	var got string
	switch {
	case inPrimary && !inChallenger:
		got = primary
	case inChallenger && !inPrimary:
		got = harness.BridgeChallengerIP
	default:
		t.Fatalf("leased address %s is in neither pool (or both): the pools no longer "+
			"identify the server that granted it", ip)
	}
	if got != want {
		t.Errorf("address %s came from server %s, want %s — the policy did not decide the race",
			ip, got, want)
	}

	// The address shows in `docker inspect` before the server has flushed its ACK, and a single read failed
	// intermittently on a correct address; waiting for the winner first also means the loser's log is current before its
	// absence is read (#669).
	wantPrimary := want == primary
	deadline := time.Now().Add(policyACKBudget)
	var ackedByPrimary, ackedByChallenger bool
	for {
		ackedByPrimary = ackedIn(fixture.BridgeLog(), ip, mac)
		ackedByChallenger = ackedIn(fixture.BridgeChallengerLog(), ip, mac)
		if (wantPrimary && ackedByPrimary) || (!wantPrimary && ackedByChallenger) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The server's log is the evidence; the pool reading has to agree with it.
	switch want {
	case primary:
		if !ackedByPrimary {
			t.Errorf("no DHCPACK for %s/%s in the PRIMARY server's log, though the address "+
				"says it leased it", ip, mac)
		}
		if ackedByChallenger {
			t.Errorf("the CHALLENGER also ACKed %s/%s: the excluded server served this "+
				"container", ip, mac)
		}
	case harness.BridgeChallengerIP:
		if !ackedByChallenger {
			t.Errorf("no DHCPACK for %s/%s in the CHALLENGER's log, though the address "+
				"says it leased it", ip, mac)
		}
		if ackedByPrimary {
			t.Errorf("the PRIMARY also ACKed %s/%s: the excluded server served this "+
				"container", ip, mac)
		}
	}
}

// Three containers, since an unpoliced race puts one on the named server about half the time, and a policy that did
// nothing survives three with a 1-in-8 chance (#111).

// runPolicyContainers starts n containers on netName and returns their ip and mac pairs.
func runPolicyContainers(t *testing.T, ctx context.Context, netName string, n int) [][2]string {
	t.Helper()
	out := make([][2]string, 0, n)
	for i := 0; i < n; i++ {
		_, ip, mac := harness.RunContainer(t, ctx, netName, fmt.Sprintf("%s-ctr%d", netName, i))
		out = append(out, [2]string{ip, mac})
	}
	return out
}

// TestServerPolicy_PrefersTheNamedServer checks in both directions that the server named in dhcp_servers leases (#111).
func TestServerPolicy_PrefersTheNamedServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})
	fixture.StartBridgeChallenger(t)

	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "primary", want: bridgeServerIP()},
		{name: "challenger", want: harness.BridgeChallengerIP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netName := "dh-itest-prefer-" + tc.name
			harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
				"dhcp_servers": tc.want,
			})
			for _, pair := range runPolicyContainers(t, ctx, netName, 3) {
				assertLeasedBy(t, tc.want, pair[0], pair[1])
			}
			t.Logf("✓ every container on %s leased from the preferred server %s", netName, tc.want)
		})
	}
}

// TestServerPolicy_DenyExcludesTheNamedServer checks in both directions that a denied server never serves the network (#669).
func TestServerPolicy_DenyExcludesTheNamedServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})
	fixture.StartBridgeChallenger(t)

	for _, tc := range []struct {
		name string
		deny string
		want string
	}{
		{name: "deny-primary", deny: bridgeServerIP(), want: harness.BridgeChallengerIP},
		{name: "deny-challenger", deny: harness.BridgeChallengerIP, want: bridgeServerIP()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netName := "dh-itest-" + tc.name
			harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
				"dhcp_deny_servers": tc.deny,
			})
			for _, pair := range runPolicyContainers(t, ctx, netName, 3) {
				assertLeasedBy(t, tc.want, pair[0], pair[1])
			}
			t.Logf("✓ %s never served a container on %s", tc.deny, netName)
		})
	}
}

// dhcpcd consults its blacklist only when no whitelist is configured (10.3.2 src/dhcp.c:3181-3196), so the plugin
// subtracts the deny-list from the preference list at parse time; with the denied server first, a dropped denial
// leases from it (#669).

// TestServerPolicy_DenyBeatsPreferForTheSameServer checks that a server both preferred and denied never leases (#669).
func TestServerPolicy_DenyBeatsPreferForTheSameServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})
	fixture.StartBridgeChallenger(t)

	primary := bridgeServerIP()
	netName := "dh-itest-deny-over-prefer"
	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		// The denied server is the first preference, the one that would win if the denial were dropped.
		"dhcp_servers":      primary + "," + harness.BridgeChallengerIP,
		"dhcp_deny_servers": primary,
	})
	for _, pair := range runPolicyContainers(t, ctx, netName, 3) {
		assertLeasedBy(t, harness.BridgeChallengerIP, pair[0], pair[1])
	}
	t.Logf("✓ %s stayed denied despite being first in dhcp_servers", primary)
}

// The ladder divides the acquisition budget, so a preference list does not slow `docker run`; lease_timeout is raised
// so each of two tiers gets more than 5s of the 10s default (#111).

// TestServerPolicy_FallsBackToTheNextServer checks that acquisition moves past a silent first preference to the second (#111).
func TestServerPolicy_FallsBackToTheNextServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})
	fixture.StartBridgeChallenger(t)
	cli := policyClient(t)

	netName := "dh-itest-server-fallback"
	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		"dhcp_servers":  harness.BridgeAbsentServerIP + "," + harness.BridgeChallengerIP,
		"lease_timeout": "24s",
	})

	w := harness.BeginCounterWindow(t, ctx, cli,
		"dhcp_server_tier_fallbacks", "dhcp_server_policy_exhausted")
	_, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	before, after := w.End()

	assertLeasedBy(t, harness.BridgeChallengerIP, ip, mac)

	if d := after.DHCPServerTierFallbacks - before.DHCPServerTierFallbacks; d < 1 {
		t.Errorf("dhcp_server_tier_fallbacks moved by %d, want >= 1: the lease came from the "+
			"second preference, so the first one must have been tried and given up on", d)
	}
	if d := after.DHCPServerPolicyExhausted - before.DHCPServerPolicyExhausted; d != 0 {
		t.Errorf("dhcp_server_policy_exhausted moved by %d, want 0: a fallback that "+
			"succeeded is not an exhausted policy", d)
	}
	t.Logf("✓ fell back from the silent %s to %s within the network's own budget",
		harness.BridgeAbsentServerIP, harness.BridgeChallengerIP)
}

// TestServerPolicy_ExhaustedFailsClosed checks that no container starts with an address when every allowed server is silent (#111).
func TestServerPolicy_ExhaustedFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})
	fixture.StartBridgeChallenger(t)
	cli := policyClient(t)

	netName := "dh-itest-server-exhausted"
	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		// Both live servers would answer an unrestricted DISCOVER, so only an address nothing answers at is allowed.
		"dhcp_servers": harness.BridgeAbsentServerIP,
	})

	ctrName := netName + "-ctr"
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", ctrName, err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	})

	w := harness.BeginCounterWindow(t, ctx, cli, "dhcp_server_policy_exhausted")
	startErr := cli.ContainerStart(ctx, create.ID, container.StartOptions{})
	before, after := w.End()

	if startErr == nil {
		ins, _ := cli.ContainerInspect(ctx, create.ID)
		var got string
		for _, ep := range ins.NetworkSettings.Networks {
			got = ep.IPAddress
		}
		t.Fatalf("container started with address %q though the only permitted DHCP server "+
			"(%s) is silent — the policy widened instead of failing closed",
			got, harness.BridgeAbsentServerIP)
	}
	t.Logf("container start refused, as it must: %v", startErr)

	if d := after.DHCPServerPolicyExhausted - before.DHCPServerPolicyExhausted; d < 1 {
		t.Errorf("dhcp_server_policy_exhausted moved by %d, want >= 1: without it this "+
			"failure is indistinguishable from a broken DHCP segment", d)
	}
	// A single-entry list has no next tier, so the fallback counter stays zero.
	if d := after.DHCPServerTierFallbacks - before.DHCPServerTierFallbacks; d != 0 {
		t.Errorf("dhcp_server_tier_fallbacks moved by %d, want 0: there was only one "+
			"preference, so there was nothing to fall back to", d)
	}
}
