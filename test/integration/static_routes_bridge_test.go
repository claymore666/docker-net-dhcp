//go:build integration

package integration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/vishvananda/netlink"
)

// TestStaticRoutes_BridgeCopiesToContainer verifies the plugin's
// bridge-mode static-route copy logic in pkg/plugin/network.go
// addRoutes: every non-default, non-DHCP-subnet, non-kernel-protocol
// route present on the host bridge at Join time is replicated into
// the container's netns via res.StaticRoutes.
//
// We pre-add a route `192.168.250.0/24 dev dh-itest-br2` on the host
// bridge before connecting the container, then assert the container's
// `ip route` lists 192.168.250.0/24 inside its netns.
//
// Why this matters operationally: bridge-mode networks bridged to a
// VLAN trunk often need extra-subnet on-link routes (e.g. management
// VLANs reachable through the same bridge but not handed out by DHCP).
// Without this copy, those routes work on the host but vanish inside
// every container — a silent footgun. The skip_routes=true option is
// the consumer-facing escape hatch; this test guards the default
// behaviour.
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

	// Add the extra route to the bridge before any container attaches.
	// Plugin's addRoutes runs at Join time and snapshots the host's
	// route table for the bridge — a route added after Join wouldn't
	// land in the container.
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
