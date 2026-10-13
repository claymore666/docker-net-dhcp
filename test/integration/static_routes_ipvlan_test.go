// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/vishvananda/netlink"
)

// addHostRoute adds dst on link for the test; the plugin reads the parent's routes at Join, so a later route misses the container (#1320).
func addHostRoute(t *testing.T, link, dst string) {
	t.Helper()
	l, err := netlink.LinkByName(link)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", link, err)
	}
	_, ipnet, err := net.ParseCIDR(dst)
	if err != nil {
		t.Fatalf("ParseCIDR(%s): %v", dst, err)
	}
	route := &netlink.Route{LinkIndex: l.Attrs().Index, Dst: ipnet}
	if err := netlink.RouteAdd(route); err != nil {
		t.Fatalf("RouteAdd %s dev %s: %v", dst, link, err)
	}
	t.Cleanup(func() {
		if err := netlink.RouteDel(route); err != nil {
			t.Logf("WARN: RouteDel %s dev %s: %v", dst, link, err)
		}
	})
}

// TestStaticRoutes_IpvlanCopiesToContainer checks that a non-default route on the ipvlan parent appears in the container (#1320).
func TestStaticRoutes_IpvlanCopiesToContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		netName  = "dh-itest-routes-ipv"
		ctrName  = "dh-itest-routes-ipv-ctr"
		extraDst = "192.168.253.0/24"
	)
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	addHostRoute(t, harness.IpvlanParent, extraDst)
	harness.CreateNetwork(t, ctx, netName, "ipvlan", nil)
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "route")
	if !strings.Contains(out, extraDst) {
		t.Errorf("static route %s not propagated into ipvlan container netns\ncontainer routes:\n%s", extraDst, out)
	}
}

// TestStaticRoutes_SkipRoutesKeepsParentRoutesOutOnBridgeAndIpvlan checks that skip_routes=true keeps the parent's route out of the container and leaves the default route (#1320).
func TestStaticRoutes_SkipRoutesKeepsParentRoutesOutOnBridgeAndIpvlan(t *testing.T) {
	for _, tc := range []struct {
		name, mode, parent, dst string
	}{
		{name: "bridge", mode: "bridge", parent: harness.BridgeName, dst: "192.168.254.0/24"},
		{name: "ipvlan", mode: "ipvlan", parent: harness.IpvlanParent, dst: "192.168.255.0/24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			netName := "dh-itest-routes-skip-" + tc.name
			t.Cleanup(func() {
				if t.Failed() {
					fixture.DumpLogs(func(s string) { t.Log(s) })
					fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
				}
			})

			addHostRoute(t, tc.parent, tc.dst)
			harness.CreateNetwork(t, ctx, netName, tc.mode, map[string]string{"skip_routes": "true"})
			id, _, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

			out := harness.ExecOutput(t, ctx, id, "ip", "route")
			if strings.Contains(out, tc.dst) {
				t.Errorf("skip_routes=true but static route %s still propagated into the %s container\nroutes:\n%s", tc.dst, tc.mode, out)
			}
			// The default gateway is not a copied route and skip_routes leaves it alone (docs/reference.md, skip_routes).
			defaultRouteGateway(t, out)
		})
	}
}
