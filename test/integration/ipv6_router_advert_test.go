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

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The RA guard writes accept_ra=0 on the container's link, so the kernel there installs and expires nothing from an
// advertisement; a route, MTU or resolver inside the container arrived through the plugin or not at all (#821). Every
// assertion reads the container, and a counter only after the container has shown what it counts.

// dnsmasq's default interval is up to 600 s; 4 s respects RFC 4861 section 6.2.1's 3 s MinRtrAdvInterval floor and
// makes a change visible within one interval (#821).

// raIntervalSeconds is dnsmasq's unsolicited advertisement interval for these fixtures.
const raIntervalSeconds = 4

// raChangeBudget bounds the wait for a reconfigured router to reach the container, and expiry fails the test.
const raChangeBudget = 45 * time.Second

// dnsmasq advertises the interface's MTU by default (measured, dnsmasq 2.91) and the bridge is at 1500, so the test
// uses 1280, IPv6's minimum link MTU (RFC 8200 section 5), which nothing else on this path produces (#821).

// advertisedMTU is the MTU the fixture advertises.
const advertisedMTU = 1280

// raParams spells one dnsmasq --ra-param for the fixture bridge: interface, optional mtu, interval, router lifetime.
func raParams(mtu, routerLifetime int) string {
	p := "--ra-param=" + harness.V6BridgeName + ","
	if mtu > 0 {
		p += "mtu:" + strconv.Itoa(mtu) + ","
	}
	return p + strconv.Itoa(raIntervalSeconds) + "," + strconv.Itoa(routerLifetime)
}

// managedArgsWith is the managed-DHCPv6 fixture from RangeArgsFor plus one --ra-param.
func managedArgsWith(param string) []string {
	return append(harness.RangeArgsFor(harness.V6Managed), param)
}

// containerV6Link returns the container interface carrying addr and its MTU as the container's kernel reports it.
func containerV6Link(t *testing.T, ctx context.Context, id, addr string) (string, int) {
	t.Helper()
	iface := containerV6Iface(t, ctx, id, addr)
	out := harness.ExecOutput(t, ctx, id, "ip", "link", "show", iface)
	mtu := 0
	f := strings.Fields(out)
	for i := 0; i < len(f)-1; i++ {
		if f[i] == "mtu" {
			mtu, _ = strconv.Atoi(f[i+1])
			break
		}
	}
	if mtu == 0 {
		t.Fatalf("could not read the MTU of %s inside the container; every assertion "+
			"keyed on it would measure nothing. `ip link show %s` said:\n%s", iface, iface, out)
	}
	return iface, mtu
}

// awaitContainerV6LinkMTU polls the container's link MTU until it reads want and returns the last value.
func awaitContainerV6LinkMTU(t *testing.T, ctx context.Context, id, addr string, want int, budget time.Duration) (string, int) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		iface, mtu := containerV6Link(t, ctx, id, addr)
		if mtu == want || !time.Now().Before(deadline) {
			return iface, mtu
		}
		time.Sleep(time.Second)
	}
}

// defaultRoutes returns `ip -6 route show default` from inside the container.
func defaultRoutes(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	return harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show", "default")
}

// awaitDefaultRouteCount polls until the container has exactly n IPv6 default routes and returns the last output.
func awaitDefaultRouteCount(t *testing.T, ctx context.Context, id string, n int, budget time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var out string
	for {
		out = defaultRoutes(t, ctx, id)
		if harness.CountDefaultRoutes(out) == n {
			return out, true
		}
		if !time.Now().Before(deadline) {
			return out, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// With accept_ra=0 the kernel no longer copies the advertised MTU (RFC 4861 section 6.3.4), so the plugin applies it,
// without propagate_mtu, since a false default would have taken it from every existing IPv6 network (#821).

// TestDHCPv6_AdvertisedMTUReachesTheContainer checks that the advertised MTU reaches the container's link with propagate_mtu unset (#821).
func TestDHCPv6_AdvertisedMTUReachesTheContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	f := harness.NewV6FixtureWithArgs(t, harness.V6Managed, managedArgsWith(raParams(advertisedMTU, 1800)))
	dumpOnFailure(t, f)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	netName := "dh-itest-v6mtu"
	id, startErr := startOnV6Segment(t, ctx, cli, f, netName)
	if startErr != nil {
		t.Fatalf("ContainerStart on a managed v6 segment: %v", startErr)
	}

	addr := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if addr == "" {
		t.Fatal("no global IPv6 appeared on the container link")
	}

	// The plugin writes the MTU on the first bound event, after the address; a single read on lane run 35131643324 landed
	// 2 s after the DHCPv6 REPLY and saw 1500 (#821).
	iface, mtu := awaitContainerV6LinkMTU(t, ctx, id, addr, advertisedMTU, raChangeBudget)
	if mtu != advertisedMTU {
		t.Errorf("%s inside the container has MTU %d, want the advertised %d. The "+
			"container's kernel is at accept_ra=0 and no longer copies the advertised "+
			"MTU itself, so the plugin has to (#821). Note propagate_mtu is NOT set on "+
			"this network, deliberately: the v6 MTU is not gated on it",
			iface, mtu, advertisedMTU)
	}
	// 1280 must differ from the bridge's own 1500, or an untouched link would pass.
	if advertisedMTU >= 1500 {
		t.Fatalf("the advertised MTU %d is not below the fixture bridge's; this test "+
			"cannot distinguish an applied MTU from an untouched link", advertisedMTU)
	}
}

// RFC 4861 sections 4.2 and 6.3.4 read Router Lifetime 0 as no longer a default router; at accept_ra=0 the kernel
// never installed the route, so the plugin removes it and restores it on return, with no lease event, driven by the
// chassis watching the router table (#821). Resolvers stay: RFC 8106 section 6.1 says the DNS options "need not be
// dropped if the expiry of the RA router lifetime happens".

// TestDHCPv6_RouterWithdrawalAndReturn checks that a running container loses its IPv6 default route when the router withdraws and regains it on return (#821).
func TestDHCPv6_RouterWithdrawalAndReturn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	f := harness.NewV6FixtureWithArgs(t, harness.V6Managed, managedArgsWith(raParams(0, 1800)))
	dumpOnFailure(t, f)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	netName := "dh-itest-v6raw"
	id, startErr := startOnV6Segment(t, ctx, cli, f, netName)
	if startErr != nil {
		t.Fatalf("ContainerStart on a managed v6 segment: %v", startErr)
	}

	addr := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if addr == "" {
		t.Fatal("no global IPv6 appeared on the container link")
	}

	// Exactly one default route via the advertisement's link-local source, or "the route went away" is free.
	routes, ok := awaitDefaultRouteCount(t, ctx, id, 1, harness.IPAcquisitionBudget)
	if !ok || !harness.HasLinkLocalDefaultRoute(routes) {
		t.Fatalf("the container did not reach exactly one IPv6 default route via a "+
			"link-local address within %v, so the withdrawal below would measure "+
			"nothing. `ip -6 route show default` says:\n%s", harness.IPAcquisitionBudget, routes)
	}

	resolvBefore := harness.ResolvNameservers(harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf"))
	if len(resolvBefore) == 0 {
		t.Error("the container has no nameserver at all before the withdrawal, so the " +
			"RFC 8106 section 6.1 assertion below cannot tell 'kept' from 'never had'")
	}

	before := harness.PluginHealthOrNil(ctx)
	if before == nil {
		t.Fatal("could not read the plugin health surface before the withdrawal. Absent " +
			"data is not a zero and the counter claim below would be unmeasured")
	}

	// Only the Router Lifetime changes.
	f.Reannounce(managedArgsWith(raParams(0, 0)))

	routes, ok = awaitDefaultRouteCount(t, ctx, id, 0, raChangeBudget)
	if !ok {
		t.Fatalf("the container still has an IPv6 default route %v after its router "+
			"advertised Router Lifetime 0. Its kernel is at accept_ra=0 and never "+
			"installed that route, so nothing but the plugin can take it away (#821). "+
			"`ip -6 route show default` says:\n%s", raChangeBudget, routes)
	}

	// Read after the container has shown the route gone.
	if after := harness.PluginHealthOrNil(ctx); after == nil {
		t.Error("could not read the plugin health surface after the withdrawal")
	} else if after.IPv6RouterWithdrawn <= before.IPv6RouterWithdrawn {
		t.Errorf("ipv6_router_withdrawn stayed at %d while a container demonstrably "+
			"lost its IPv6 default route to a Router Lifetime of 0. The route is off "+
			"the link and the operator has no record of why (#821)", after.IPv6RouterWithdrawn)
	}

	// RFC 8106 section 6.1, verbatim: the DNS options "need not be
	// dropped if the expiry of the RA router lifetime happens".
	resolvAfter := harness.ResolvNameservers(harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf"))
	if len(resolvAfter) < len(resolvBefore) {
		t.Errorf("the container lost resolvers when its router withdrew itself: %v -> %v. "+
			"RFC 8106 section 6.1 says the DNS options need not be dropped when the "+
			"router lifetime expires; a router saying 'do not route through me' is not "+
			"saying 'stop resolving names'", resolvBefore, resolvAfter)
	}

	// The router comes back, and so does the route, with no restart.
	f.Reannounce(managedArgsWith(raParams(0, 1800)))

	routes, ok = awaitDefaultRouteCount(t, ctx, id, 1, raChangeBudget)
	if !ok || !harness.HasLinkLocalDefaultRoute(routes) {
		t.Fatalf("the container did not get its IPv6 default route back within %v of its "+
			"router advertising itself again. A plugin that removes the route and then "+
			"stops watching passes the withdrawal half of this test and fails here. "+
			"`ip -6 route show default` says:\n%s", raChangeBudget, routes)
	}
}

// Known wrong, pinned (#821, #818): the engine disables IPv6 on a link whose endpoint has no global address and the
// kernel then refuses every IPv6 route there, so an advertised gateway in the Join answer failed the whole sandbox
// ("routes to [...]: permission denied") and the container lost its IPv4 too, lane run 35131643324. The plugin's
// disable_ipv6 clear runs after the engine applies the answer. When #818 lands both pinned assertions go red and are
// inverted; the container starting with its IPv4 is #868's guarantee. Stateless (O=1) and SLAAC (O=0) take different
// classifications, so both run.

// TestDHCPv6_NoAddressSegmentGetsNoIPv6RouteYet checks that a container on an RA-only segment starts with its IPv4 and, until #818, no IPv6 default route.
func TestDHCPv6_NoAddressSegmentGetsNoIPv6RouteYet(t *testing.T) {
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
	}{
		{"stateless", harness.V6Stateless, "dh-itest-v6slra"},
		{"slaac", harness.V6SLAAC, "dh-itest-v6slaacra"},
	}
	// Both arms are segments where the advertisement is the only source of configuration.
	if len(cases) != 2 {
		t.Fatalf("this test needs both no-address modes and has %d", len(cases))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := harness.NewV6Fixture(t, tc.mode)
			dumpOnFailure(t, f)

			id, err := startOnV6Segment(t, ctx, cli, f, tc.net)
			if err != nil {
				t.Fatalf("the container did not start on a %s segment: %v", tc.mode, err)
			}

			// The positive half first: this went red when the advertised route was in the Join answer (#821).
			v4 := harness.ExecOutput(t, ctx, id, "ip", "-4", "-o", "addr", "show", "scope", "global")
			if !strings.Contains(v4, "inet ") {
				t.Errorf("the container on a %s segment has no IPv4 address, so the "+
					"endpoint's IPv6 half took its IPv4 with it:\n%s", tc.mode, v4)
			}

			// Zero, and still zero after the advertisement watch has ticked, so a late route is not missed.
			for _, when := range []string{"as soon as the container is running", "after the advertisement watch had run"} {
				out := defaultRoutes(t, ctx, id)
				if n := harness.CountDefaultRoutes(out); n != 0 {
					t.Fatalf("the container on a %s segment has %d IPv6 default routes %s, "+
						"want 0. If #818 has landed, invert this: the route is now both "+
						"installable and useful, and it is required here. "+
						"`ip -6 route show default` said:\n%s", tc.mode, n, when, out)
				}
				time.Sleep(2 * raIntervalSeconds * time.Second)
			}

			// A link-local address cannot leave the link (RFC 4291 section 2.5.6), and there is no global address either.
			global := harness.ExecOutput(t, ctx, id, "ip", "-6", "addr", "show", "scope", "global")
			if strings.TrimSpace(global) != "" {
				t.Errorf("the container on a %s segment HAS a global IPv6 address:\n%s\n"+
					"That is the state #818 is meant to produce and this tree is not "+
					"supposed to be in it yet. If #818 has landed, invert this "+
					"assertion and the route assertion above together.", tc.mode, global)
			}
		})
	}
}

// At accept_ra=0 with DHCPv6 carrying no next hop, a default route via a link-local address proves an advertisement
// reached the plugin, so it is the precondition. router_adverts_seen alone cannot tell silent routers from a client
// that never asked, so the solicitation count is asserted beside it; an absent field decodes as 0 and fails (#814).

// TestDHCPv6_RouterDiscoveryCountersRise checks that the router discovery counters rise on a segment with advertisements (#814).
func TestDHCPv6_RouterDiscoveryCountersRise(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	f := harness.NewV6FixtureWithArgs(t, harness.V6Managed, managedArgsWith(raParams(0, 1800)))
	dumpOnFailure(t, f)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// Taken before the network exists, so every advertisement falls inside the window.
	before := harness.PluginHealthOrNil(ctx)
	if before == nil {
		t.Fatal("could not read the plugin health surface before the container started. " +
			"Absent data is not a zero and a rise measured from an assumed baseline is " +
			"not a measurement")
	}

	netName := "dh-itest-v6rdc"
	id, startErr := startOnV6Segment(t, ctx, cli, f, netName)
	if startErr != nil {
		t.Fatalf("ContainerStart on a managed v6 segment: %v", startErr)
	}

	addr := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if addr == "" {
		t.Fatal("no global IPv6 appeared on the container link")
	}

	// The container's own evidence that the segment advertised.
	routes, ok := awaitDefaultRouteCount(t, ctx, id, 1, harness.IPAcquisitionBudget)
	if !ok || !harness.HasLinkLocalDefaultRoute(routes) {
		t.Fatalf("the container did not reach exactly one IPv6 default route via a "+
			"link-local address within %v, so nothing here proves the segment "+
			"advertised and the counter claim below would be free. "+
			"`ip -6 route show default` says:\n%s", harness.IPAcquisitionBudget, routes)
	}

	after := harness.PluginHealthOrNil(ctx)
	if after == nil {
		t.Fatal("could not read the plugin health surface after the container had its route")
	}

	// The route may also come from a guard that did not take, so the guard-failure count is printed beside (#814).
	if after.RouterAdvertsSeen <= before.RouterAdvertsSeen {
		t.Errorf("router_adverts_seen stayed at %d while a container on this segment "+
			"demonstrably took its IPv6 default route from an advertisement. Two things "+
			"produce that: the frames reached the client and no number says so, which is "+
			"what this test is for; or the RA guard did not take and the route came from "+
			"the container's own kernel, in which case the plugin's client may have seen "+
			"nothing and the defect is the guard. router_advert_guard_failures went from "+
			"%d to %d, which separates them (#814)",
			after.RouterAdvertsSeen,
			before.RouterAdvertGuardFailures, after.RouterAdvertGuardFailures)
	}
	if after.RouterSolicitsSent <= before.RouterSolicitsSent {
		t.Errorf("router_solicits_sent stayed at %d, so the sighting count beside it "+
			"cannot be read: a zero there would be indistinguishable from a client that "+
			"never asked (#814)", after.RouterSolicitsSent)
	}

	// Printed on green runs too, so the numbers operators read have a record.
	t.Logf("router discovery on a segment with advertisements enabled: solicits +%d, "+
		"adverts seen +%d, refused +%d, options ignored +%d, table entries dropped +%d, evicted +%d",
		after.RouterSolicitsSent-before.RouterSolicitsSent,
		after.RouterAdvertsSeen-before.RouterAdvertsSeen,
		after.RouterAdvertsRefused-before.RouterAdvertsRefused,
		after.RouterAdvertOptionsIgnored-before.RouterAdvertOptionsIgnored,
		after.RouterTableEntriesDropped-before.RouterTableEntriesDropped,
		after.RouterTableEntriesEvicted-before.RouterTableEntriesEvicted)

	// The fixture's dnsmasq is well formed, so a refusal is a decoder finding; the counter is process-wide, and no test in
	// test/integration calls t.Parallel(), though an earlier test's container could still be soliciting (#814).
	if d := after.RouterAdvertsRefused - before.RouterAdvertsRefused; d != 0 {
		t.Errorf("router_adverts_refused rose by %d against a dnsmasq fixture whose "+
			"advertisements are well formed. Either the decoder refused something it "+
			"should read, or something on this path corrupted the frames", d)
	}
}
