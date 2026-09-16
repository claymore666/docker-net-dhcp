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

// #821's outside evidence: the plugin, and not the container's kernel,
// turns a Router Advertisement into the container's IPv6 configuration.
//
// Every assertion in this file reads the CONTAINER -- `ip -6 route`,
// `ip link`, `/etc/resolv.conf` -- and never the plugin's opinion of
// itself. The one health counter that is read is read only after the
// container has already shown the thing it counts, which is the rule
// that keeps a counter from being both the claim and its evidence.
//
// WHAT MAKES THESE OBSERVABLE AT ALL. The guard writes accept_ra=0 on
// the container's link, so the kernel there installs nothing from an
// advertisement and expires nothing either. Everything below is
// therefore attributable: a route, an MTU or a resolver inside the
// container arrived through Lease.Gateway / dhcp.Info and the plugin,
// or it did not arrive.

// raIntervalSeconds is dnsmasq's unsolicited advertisement interval for
// these fixtures.
//
// WHY IT IS SET AT ALL: dnsmasq's default is up to 600 s, which is
// outside any budget a test can hold, so the default fixtures can only
// ever show the steady state. Four seconds is inside RFC 4861 section
// 6.2.1's MinRtrAdvInterval floor of 3 s for MaxRtrAdvInterval values
// this small, and it is what makes a CHANGE observable: after a
// reconfiguration the plugin's client sees the new advertisement within
// one interval and the manager's own watch picks it up within a
// quarter of RFC 4861 section 10's 3 s MIN_DELAY_BETWEEN_RAS.
const raIntervalSeconds = 4

// raChangeBudget bounds the wait for a reconfigured router to reach the
// container. One advertisement interval, plus the plugin's watch
// interval, plus a wide allowance for a loaded runner. It is a
// DEADLINE: expiry fails the test rather than skipping it.
const raChangeBudget = 45 * time.Second

// advertisedMTU is deliberately NOT the bridge's own MTU.
//
// dnsmasq advertises the interface's MTU on every advertisement unless
// told otherwise (MEASURED, dnsmasq 2.91), and the fixture bridge is at
// the default 1500, so an assertion against 1500 would pass on a
// container whose link had simply never been touched. 1280 is IPv6's
// minimum link MTU (RFC 8200 section 5), so it is both a legal value
// and one nothing else on this path would produce.
const advertisedMTU = 1280

// raParams spells one dnsmasq --ra-param for the fixture bridge.
// Fields: interface, [mtu:N,] interval, router-lifetime.
func raParams(mtu, routerLifetime int) string {
	p := "--ra-param=" + harness.V6BridgeName + ","
	if mtu > 0 {
		p += "mtu:" + strconv.Itoa(mtu) + ","
	}
	return p + strconv.Itoa(raIntervalSeconds) + "," + strconv.Itoa(routerLifetime)
}

// managedArgsWith is the ordinary managed-DHCPv6 fixture plus one
// --ra-param. Built from RangeArgsFor rather than re-typed, so a change
// to what "managed" means reaches these tests too.
func managedArgsWith(param string) []string {
	return append(harness.RangeArgsFor(harness.V6Managed), param)
}

// containerV6Link returns the container-side interface carrying addr,
// and its MTU as the container's own kernel reports it.
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

// awaitContainerV6LinkMTU polls the container's own link MTU until it
// reads want, and returns the last value either way.
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

// defaultRoutes is `ip -6 route show default` inside the container.
func defaultRoutes(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	return harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show", "default")
}

// awaitDefaultRouteCount polls until the container has exactly n IPv6
// default routes, and returns the last output either way.
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

// TestDHCPv6_AdvertisedMTUReachesTheContainer is the MTU half of #821.
//
// Until #821 the container's kernel copied the advertised MTU itself
// (RFC 4861 section 6.3.4). accept_ra=0 stops it doing that, so the
// plugin applies it instead -- and it does so WITHOUT propagate_mtu,
// because an option that defaults to false would have silently taken
// the advertised MTU away from every existing IPv6 network. The network
// created here deliberately does not set propagate_mtu, so a plugin
// that gated the v6 MTU on it fails this test.
//
// The assertion is the container's own link MTU, read inside it.
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

	// A DEADLINE, NOT A SINGLE READ. The MTU does not arrive with the
	// Join answer: the plugin writes it from the manager goroutine Join
	// spawns, on the first bound event, which lands after the address
	// the poll above waited for. Reading once raced that and measured
	// the link Docker had just created (MEASURED, lane run 35131643324,
	// where the read landed 2 s after the DHCPv6 REPLY and saw 1500).
	iface, mtu := awaitContainerV6LinkMTU(t, ctx, id, addr, advertisedMTU, raChangeBudget)
	if mtu != advertisedMTU {
		t.Errorf("%s inside the container has MTU %d, want the advertised %d. The "+
			"container's kernel is at accept_ra=0 and no longer copies the advertised "+
			"MTU itself, so the plugin has to (#821). Note propagate_mtu is NOT set on "+
			"this network, deliberately: the v6 MTU is not gated on it",
			iface, mtu, advertisedMTU)
	}
	// NON-VACUITY: 1280 must not be what the link would have had
	// anyway. dnsmasq advertises the bridge's own MTU by default and
	// the fixture bridge is at 1500, so an assertion that could pass
	// against an untouched link would prove nothing.
	if advertisedMTU >= 1500 {
		t.Fatalf("the advertised MTU %d is not below the fixture bridge's; this test "+
			"cannot distinguish an applied MTU from an untouched link", advertisedMTU)
	}
}

// TestDHCPv6_RouterWithdrawalAndReturn is Q4's live-rewrite property,
// in both directions, on a container that keeps running throughout.
//
// DIRECTION ONE, the withdrawal. RFC 4861 section 4.2 defines Router
// Lifetime as "the lifetime associated with the default router" and
// section 6.3.4 reads 0 as "no longer to be used as a default router".
// Until #821 the container's kernel expired the route on that lifetime.
// It cannot now -- it is at accept_ra=0 and never installed the route
// in the first place -- so the plugin has to take it away, and if it
// does not the container keeps a default route to a black hole with
// every counter reading healthy and the DHCPv6 lease renewing happily.
//
// DIRECTION TWO, the return. A router that comes back must get the
// container back, without restarting it. Without this half, "the route
// went away" would also be satisfied by a plugin that deleted the route
// and never looked again.
//
// NO LEASE EVENT HAPPENS IN EITHER DIRECTION, which is the whole reason
// this needs machinery at all: the address does not change, so the
// library's state machine emits nothing, and what drives both halves is
// the chassis watching the router table.
//
// The resolvers are asserted across the withdrawal too, and they must
// NOT go away with the route: RFC 8106 section 6.1 says the DNS options
// "need not be dropped if the expiry of the RA router lifetime
// happens". A router saying "do not route through me" is not saying
// "stop resolving names".
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

	// THE PRECONDITION, and it is also the steady-state claim: exactly
	// one default route, via the advertisement's link-local source.
	// Without it every assertion below is about a container that never
	// had a route, and "the route went away" would be free.
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

	// The router withdraws itself. Same bridge, same addresses, same
	// DHCPv6 pool: only the Router Lifetime changes.
	f.Reannounce(managedArgsWith(raParams(0, 0)))

	routes, ok = awaitDefaultRouteCount(t, ctx, id, 0, raChangeBudget)
	if !ok {
		t.Fatalf("the container still has an IPv6 default route %v after its router "+
			"advertised Router Lifetime 0. Its kernel is at accept_ra=0 and never "+
			"installed that route, so nothing but the plugin can take it away (#821). "+
			"`ip -6 route show default` says:\n%s", raChangeBudget, routes)
	}

	// The counter is read AFTER the container has already shown the
	// route gone, so it is the plugin's account of something already
	// observed and not the evidence itself.
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

	// DIRECTION TWO. The router comes back, and so does the container,
	// with no restart.
	f.Reannounce(managedArgsWith(raParams(0, 1800)))

	routes, ok = awaitDefaultRouteCount(t, ctx, id, 1, raChangeBudget)
	if !ok || !harness.HasLinkLocalDefaultRoute(routes) {
		t.Fatalf("the container did not get its IPv6 default route back within %v of its "+
			"router advertising itself again. A plugin that removes the route and then "+
			"stops watching passes the withdrawal half of this test and fails here. "+
			"`ip -6 route show default` says:\n%s", raChangeBudget, routes)
	}
}

// TestDHCPv6_NoAddressSegmentGetsNoIPv6RouteYet PINS THE WRONG ANSWER
// ON PURPOSE (#821 -> #818), on the two segments where it is wrong.
//
// A segment that advertises a router and hands out no DHCPv6 address
// gives the container nothing on IPv6 but a link-local. Before #821 its
// own kernel read the advertisement and gave it a default route and a
// SLAAC address; the #821 guard writes accept_ra=0 and autoconf=0, so
// neither happens now, and the plugin does not supply them either.
//
// IT IS NOT A CHOICE. An endpoint with no global IPv6 address has IPv6
// disabled on its link by the engine, and the kernel then refuses every
// IPv6 route on such a link. A Join answer carrying the advertised
// gateway or on-link prefix fails the WHOLE sandbox:
//
//	error setting interface "<host-if>" routes to ["fd00:...::/64"]: permission denied
//
// and the container does not start at all -- losing its IPv4 with it
// (MEASURED, lane run 35131643324, four shards). Nor can the plugin
// clear disable_ipv6 first: that clear runs in the manager goroutine
// Join spawns, after the engine has moved the link and applied the
// answer. So the route is not installable until there is a global
// address to install it beside, which is #818 on this milestone.
//
// THIS TEST GOES RED WHEN #818 LANDS, which is the point: the container
// gets a global address, both pinned assertions stop being true at
// once, and whoever lands #818 inverts them here instead of discovering
// that the gap closed silently. Asserting nothing would have let the
// suite pass identically on the intended end state and on this one.
//
// What it asserts POSITIVELY is the guarantee #868 bought and #821 must
// not spend: the container STARTS on these segments, and has its IPv4.
//
// BOTH MODES, not one. Stateless (O=1) and SLAAC (O=0) reach the
// absence path through different classifications, and a fix that
// reached only one of them is exactly the shape this repository has
// shipped before.
func TestDHCPv6_NoAddressSegmentGetsNoIPv6RouteYet(t *testing.T) {
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
	}{
		{"stateless", harness.V6Stateless, "dh-itest-v6slra"},
		{"slaac", harness.V6SLAAC, "dh-itest-v6slaacra"},
	}
	// NON-VACUITY. Both arms are segments on which the advertisement is
	// the ONLY source of configuration; an emptied table leaves this
	// test green while claiming the thing it no longer checks.
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

			// THE POSITIVE HALF FIRST: the endpoint exists and
			// carries its IPv4. This is what went red when the
			// advertised route was put into the Join answer -- not
			// an IPv6 assertion but every container on the segment.
			v4 := harness.ExecOutput(t, ctx, id, "ip", "-4", "-o", "addr", "show", "scope", "global")
			if !strings.Contains(v4, "inet ") {
				t.Errorf("the container on a %s segment has no IPv4 address, so the "+
					"endpoint's IPv6 half took its IPv4 with it:\n%s", tc.mode, v4)
			}

			// THE PINNED HALF. Exactly zero IPv6 default routes, and
			// still zero after the plugin's advertisement watch has
			// had ticks to run, so a route that arrives late is not
			// missed by reading too early.
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

			// A link-local address cannot leave the link (RFC 4291
			// section 2.5.6), so the absent route is not the only
			// thing missing: there is no global address either, and
			// the two only become useful together.
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
