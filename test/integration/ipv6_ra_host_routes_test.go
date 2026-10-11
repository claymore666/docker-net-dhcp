// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// raHostRoutes lists the host link's IPv6 routes with proto ra, which the kernel gives routes learned from RAs (#1317).
func raHostRoutes(t *testing.T, link netlink.Link) []netlink.Route {
	t.Helper()
	routes, err := util.DumpResult(netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Protocol:  unix.RTPROT_RA,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL))
	if err != nil {
		t.Fatalf("RouteListFiltered(%s, proto ra): %v", link.Attrs().Name, err)
	}
	return routes
}

// TestDHCPv6_AnEndpointWithNoV6LeaseStartsOnAHostWithRARoutes starts a container whose DHCPv6 lease never comes, on a
// host whose bridge holds routes learned from advertisements, and requires it to run on its IPv4 lease (#1317, #1125).
// With no IPv6 address the engine turns IPv6 off on the link, so a route copied from the host fails the whole start.
func TestDHCPv6_AnEndpointWithNoV6LeaseStartsOnAHostWithRARoutes(t *testing.T) {
	const netName = "dh-itest-rahost"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	f := harness.NewV6Fixture(t, harness.V6NoRA)
	dumpOnFailure(t, f)

	// The fixture sends no advertisement, so the learned routes go on the bridge as the kernel would add them (#1317).
	bridge, err := netlink.LinkByName(f.Bridge())
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", f.Bridge(), err)
	}
	_, onLink, _ := net.ParseCIDR(harness.AdvertPrefixA)
	_, behindRouter, _ := net.ParseCIDR(harness.AdvertPrefixB)
	for _, r := range []*netlink.Route{
		{LinkIndex: bridge.Attrs().Index, Dst: onLink, Protocol: unix.RTPROT_RA},
		{LinkIndex: bridge.Attrs().Index, Dst: behindRouter, Gw: net.ParseIP("fe80::1"), Protocol: unix.RTPROT_RA},
	} {
		if err := netlink.RouteAdd(r); err != nil {
			t.Fatalf("RouteAdd %s dev %s proto ra: %v", r.Dst, f.Bridge(), err)
		}
		t.Cleanup(func() { _ = netlink.RouteDel(r) })
	}
	if got := raHostRoutes(t, bridge); len(got) < 2 {
		t.Fatalf("the host's %s holds %d proto ra routes, want the 2 just added; the case would not exercise the host's "+
			"learned routes (#1317)", f.Bridge(), len(got))
	}

	id, err := startOnV6SegmentAs(t, ctx, cli, f, onV6Bridge, netName, nil)
	if err != nil {
		t.Fatalf("the container did not start with no DHCPv6 lease on a host holding proto ra routes: %v (#1317, #1125)", err)
	}
	assertAttachedAs(t, ctx, f, id, onV6Bridge)
	f.AwaitIgnoredSolicit(harness.IPAcquisitionBudget)

	pool := netip.MustParsePrefix(harness.V6SubnetCIDR)
	v4 := harness.ExecOutput(t, ctx, id, "ip", "-4", "-o", "addr", "show", "scope", "global")
	if m := containerV4.FindStringSubmatch(v4); m == nil || !pool.Contains(netip.MustParsePrefix(m[1]).Addr()) {
		t.Errorf("the container holds no IPv4 address from the segment's pool %s:\n%s", pool, v4)
	}
	if global := harness.ExecOutput(t, ctx, id, "ip", "-6", "addr", "show", "scope", "global"); strings.TrimSpace(global) != "" {
		t.Errorf("the container holds a global IPv6 address on a segment whose DHCPv6 server was silent:\n%s", global)
	}
	routes := harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show")
	for _, p := range []string{harness.AdvertPrefixA, harness.AdvertPrefixB} {
		if strings.Contains(routes, p) {
			t.Errorf("the host's proto ra route to %s reached a container with no IPv6 address:\n%s", p, routes)
		}
	}
	if got := raHostRoutes(t, bridge); len(got) < 2 {
		t.Errorf("the host's %s holds %d proto ra routes after the start, want the 2 it had before", f.Bridge(), len(got))
	}
}
