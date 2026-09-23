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

// TestStaticRoutes_BridgeCopiesToContainer checks that a non-default, non-DHCP-subnet route on the bridge appears in the container.
func TestStaticRoutes_BridgeCopiesToContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		netName  = "dh-itest-routes-br"
		ctrName  = "dh-itest-routes-br-ctr"
		extraDst = "192.168.250.0/24"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})

	// addRoutes reads the host's routes for the bridge at Join, so a route added after Join would not reach the container.
	bridge, err := netlink.LinkByName(harness.BridgeName)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", harness.BridgeName, err)
	}
	_, dst, err := net.ParseCIDR(extraDst)
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	hostRoute := &netlink.Route{
		LinkIndex: bridge.Attrs().Index,
		Dst:       dst,
	}
	if err := netlink.RouteAdd(hostRoute); err != nil {
		t.Fatalf("RouteAdd %s dev %s: %v", extraDst, harness.BridgeName, err)
	}
	t.Cleanup(func() {
		if err := netlink.RouteDel(hostRoute); err != nil {
			t.Logf("WARN: RouteDel %s dev %s: %v", extraDst, harness.BridgeName, err)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "bridge", nil)
	id, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container ip=%s", ipv4)

	out := harness.ExecOutput(t, ctx, id, "ip", "route")
	if !strings.Contains(out, extraDst) {
		t.Errorf("static route %s not propagated into container netns\ncontainer routes:\n%s", extraDst, out)
	}
}
