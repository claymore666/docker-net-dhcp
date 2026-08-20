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

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// Server-selection tests for dhcp_servers (#111) and
// dhcp_deny_servers (#669).
//
// These are the only tests in the suite that deliberately run TWO DHCP
// servers on one broadcast domain. Everywhere else the harness works
// hard to keep exactly one, because a race between servers makes every
// address assertion ambiguous. Here the race IS the subject: the
// feature exists to decide it, and against a single server a passing
// test would prove nothing at all.
//
// Every assertion below is read from outside the plugin:
//
//   - WHICH server leased is read from the leased address. The two
//     pools are disjoint (harness.BridgeChallengerPool*), and
//     TestBridgeChallenger_AddressPlanIsUnambiguous keeps them that
//     way, so the address names its issuer with no inference.
//   - THAT a server did or did not serve a given container is read
//     from that server's own dnsmasq log, by MAC.
//
// The health counters are checked too, but only as a second statement
// about the same event — never as the primary evidence. A counter
// proves the plugin's intent; the server's log proves the effect.
//
// The whole file is bridge-mode: it needs a real Linux bridge to hang
// a second server off. The macvlan/ipvlan fixture is a point-to-point
// veth pair with one server at the far end and cannot host a second
// one. The selection logic itself is mode-independent — both
// acquisition paths go through acquireWithPolicy — so mode is a
// property of the fixture here, not of the feature.

// policyClient returns a docker client for the counter windows below.
func policyClient(t *testing.T) *docker.Client {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// ackedIn reports whether a dnsmasq log contains a DHCPACK naming both
// this address and this MAC — i.e. whether THIS server is the one that
// completed the exchange.
//
// Matching on the ACK specifically, rather than on the MAC appearing
// anywhere, matters: both servers see every DHCPDISCOVER on a shared
// segment and both log it. A server that saw the discover and was
// refused looks identical to the winner if you only grep for the MAC.
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

// bridgeServerIP is the primary bridge dnsmasq's address, which is what
// a test passes to name it in a policy.
func bridgeServerIP() string { return strings.SplitN(harness.BridgeAddr, "/", 2)[0] }

// assertLeasedBy checks, from both servers' logs and from the address
// itself, that want (and only want) served this container.
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

	primaryLog := fixture.BridgeLog()
	chalLog := fixture.BridgeChallengerLog()
	ackedByPrimary := ackedIn(primaryLog, ip, mac)
	ackedByChallenger := ackedIn(chalLog, ip, mac)

	// The server's own log is the outside evidence; the pool reading
	// above is a shortcut that has to agree with it.
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

// runPolicyContainers starts n containers on netName and returns their
// (ip, mac) pairs.
//
// n is 3 rather than 1 on purpose. With two servers answering, one
// container landing on the named server is what an unpoliced race
// produces roughly half the time. Three consecutive wins is not a
// proof, but it is the difference between an assertion that can fail
// and one that cannot: a policy that did nothing has a 1-in-8 chance
// of surviving this, and the deterministic evidence (the losing
// server's log carrying no ACK) has to hold for every one of them.
func runPolicyContainers(t *testing.T, ctx context.Context, netName string, n int) [][2]string {
	t.Helper()
	out := make([][2]string, 0, n)
	for i := 0; i < n; i++ {
		_, ip, mac := harness.RunContainer(t, ctx, netName, fmt.Sprintf("%s-ctr%d", netName, i))
		out = append(out, [2]string{ip, mac})
	}
	return out
}

// TestServerPolicy_PrefersTheNamedServer is the core #111 assertion:
// with two servers answering, the one named in dhcp_servers is the one
// that leases — in either direction.
//
// Both directions are tested because one alone cannot distinguish the
// feature from luck. If only "prefer the challenger" were checked, a
// bug that always picked the challenger regardless of configuration
// would pass.
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

// TestServerPolicy_DenyExcludesTheNamedServer is the #669 assertion:
// the denied server never serves this network, and the other one does.
//
// Again in both directions — a deny-list that denied the wrong server,
// or denied everything, would pass a single-direction test.
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

// TestServerPolicy_DenyBeatsPreferForTheSameServer is the regression
// guard for dhcpcd's precedence rule, and it is the reason the plugin
// subtracts the deny-list from the preference list at parse time
// instead of emitting both directives.
//
// dhcpcd only consults its blacklist when no whitelist is configured
// (10.3.2 src/dhcp.c:3181-3196). A network that set both and had both
// passed through would therefore see the blacklist silently ignored —
// and because the denied server is FIRST in the preference list here,
// that bug has exactly one visible symptom: the container leases from
// the server the operator denied. Nothing else in the suite would
// notice.
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
		// The denied server is the FIRST preference: if the denial were
		// dropped, this is the server that would win.
		"dhcp_servers":      primary + "," + harness.BridgeChallengerIP,
		"dhcp_deny_servers": primary,
	})
	for _, pair := range runPolicyContainers(t, ctx, netName, 3) {
		assertLeasedBy(t, harness.BridgeChallengerIP, pair[0], pair[1])
	}
	t.Logf("✓ %s stayed denied despite being first in dhcp_servers", primary)
}

// TestServerPolicy_FallsBackToTheNextServer covers the ladder: the
// first preference is an address nothing answers at, so acquisition
// must move on to the second and succeed.
//
// lease_timeout is raised because the ladder DIVIDES the acquisition
// budget rather than extending it — that is a deliberate property of
// the feature (a preference list must not make `docker run` slower),
// and it means two tiers at the 10s default would give each tier 5s.
// This is not the test being loosened to pass: the default budget is
// still what an unconfigured network gets, and a shorter per-tier
// slice would test dhcpcd's retry timing rather than the fallback.
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

// TestServerPolicy_ExhaustedFailsClosed is the property that makes the
// feature safe to use: when every server the network is allowed to use
// is silent, the container does NOT start with an address from
// somewhere else. A policy that widened under pressure would hand an
// operator the one outcome they configured it to prevent.
//
// It also pins the counter that separates this from an ordinary DHCP
// timeout. Both failures look identical in a log — "no lease in 10s" —
// and they call for different operator action.
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
		// Both live servers are on this segment and both would answer
		// an unrestricted DISCOVER. Only an address nothing answers at
		// is allowed.
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
	// A single-entry preference list has no next tier, so nothing was
	// fallen back FROM. Pinning it at zero keeps the two counters
	// meaning different things.
	if d := after.DHCPServerTierFallbacks - before.DHCPServerTierFallbacks; d != 0 {
		t.Errorf("dhcp_server_tier_fallbacks moved by %d, want 0: there was only one "+
			"preference, so there was nothing to fall back to", d)
	}
}
