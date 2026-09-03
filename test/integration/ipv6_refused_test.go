// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// What is left of the DHCPv6 suite in the IPv4-only beta.
//
// The 2.0 beta leases through the in-house DHCP library, which
// implements DHCPv4; DHCPv6 is written next and lands at M7. Ten tests
// across ipv6_test.go and dhcpv6_noaddress_modes_test.go drove a v6
// dhcpcd client alongside the v4 one, and every one of them was a test
// of a client this build does not contain. None is skipped and none is
// left to pass vacuously: they are retired, named in the commit that
// retired them, and this file holds the property that replaces all ten
// — that an operator asking for IPv6 is TOLD, at the create, rather
// than getting a network that quietly does nothing with it.
//
// What the retired tests were, and where each one goes:
//
//   - TestLifecycleMacvlan_IPv6_GoldenPath, TestLifecycleBridge_IPv6_GoldenPath
//     — a container gets a v6 address in each mode. Returns at M7 as
//     the golden path of the v6 client.
//   - TestTombstoneRestart_PreservesIPv6 — v6 stickiness across a
//     container restart. The v4 half of the same property is still
//     covered by TestTombstoneRestart_PreservesMACAndIP.
//   - TestLeaseRenewIPv6_HonorsT1 — v6 renewal timing. The v4 half is
//     TestLeaseRenew_HonorsT1.
//   - TestIPv6_DNS6Propagation — DHCPv6 options 23/24 into resolv.conf.
//     The v4 half is TestDNSPropagate_*.
//   - TestDUID_PersistsAcrossPluginRestart — DUID-LL stability across
//     a plugin recycle. Its v4 analogue is the client-id suite, and
//     the identity it pinned is now stored rather than re-derived
//     (D10, the record's Identity field, written once).
//   - TestDHCPv6_NoAddressModes_StartTheEndpoint,
//     TestDHCPv6_Stateless_ConfigurationReachesTheContainer,
//     TestDHCPv6_Managed_StillRequiresALease,
//     TestDHCPv6_Managed_ServerSilent_IsStillFatal — the four #868/#815
//     segment-shape cases. They depend on router advertisements and on
//     the information-reply path, which are v6 by definition.
//
// TestV6Fixture_ModesComeUpAsRequested is NOT retired: it asserts that
// the fixture's own dnsmasq instances advertise the segment shapes the
// suite claims they do, which is a statement about the fixture and not
// about the plugin. It keeps the v6 fixture exercised so that the M7
// work does not start by re-debugging it.
package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestIPv6_RefusedAtCreateWithTheMilestoneNamed is the whole of IPv6 in
// the beta.
//
// It asserts three things that are separate failures:
//
//  1. The create FAILS. A network created with ipv6=true and no v6
//     client is the silent regression this milestone is most likely to
//     ship: every container comes up, `docker inspect` shows no
//     GlobalIPv6Address, and nothing anywhere says why.
//  2. The error names the BETA and the MILESTONE. An operator reading
//     "invalid option" has no way to tell a typo from a feature that
//     is coming back, and the answer decides whether they stay on 1.x.
//  3. No network is left behind. A refusal that persisted the network
//     would leave a create that failed and a network that exists.
//
// It runs against the daemon, not against the plugin's own view: the
// evidence is what `docker network create` returns and what
// `docker network ls` shows afterwards.
func TestIPv6_RefusedAtCreateWithTheMilestoneNamed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	const netName = "dhcptest-ipv6-refused"
	_, err = cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver:  harness.DriverName,
		IPAM:    &network.IPAM{Driver: "null"},
		Options: map[string]string{"mode": "macvlan", "parent": harness.HostVeth, "ipv6": "true"},
	})
	if err == nil {
		_ = cli.NetworkRemove(ctx, netName)
		t.Fatal("an ipv6=true network was created by an IPv4-only build. " +
			"Every container on it would come up with no IPv6 address and nothing would say so.")
	}

	got := err.Error()
	// Two independent halves of the message, asserted separately so a
	// message that kept one and lost the other cannot pass. The
	// milestone is the half an operator acts on.
	for _, want := range []string{"2.0 beta", "M7"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not name %q, so an operator cannot tell "+
				"a rejected option from a feature that is coming back: %s", want, got)
		}
	}

	// The control for assertion 3. A refusal is only complete if it
	// left nothing behind; a create that failed AND persisted the
	// network is the worst of both.
	if _, err := cli.NetworkInspect(ctx, netName, network.InspectOptions{}); err == nil {
		_ = cli.NetworkRemove(ctx, netName)
		t.Error("the create was refused and the network exists anyway")
	}
}

// TestIPv6_TheV4PathIsUnaffectedByTheRefusal is the preservation
// control, and without it the test above measures nothing useful.
//
// A refusal that rejected every create would satisfy every assertion
// in this file. The cheapest way to be sure the refusal is keyed on
// IPv6 and not on something broader is to create the same network
// WITHOUT the option and require it to work.
func TestIPv6_TheV4PathIsUnaffectedByTheRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dhcptest-ipv6-control"
	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	// CreateNetwork fails the test itself if the create is refused, so
	// reaching here is the assertion. Cleanup is the harness's.
}
