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

// Since v0.9.0 parent-attached modes copy host routes like bridge mode; skip_routes=true keeps the earlier no-copy behaviour.

// TestStaticRoutes_MacvlanCopiesToContainer checks that a non-default route on the parent NIC appears in the container.
func TestStaticRoutes_MacvlanCopiesToContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		netName  = "dh-itest-routes-mv"
		ctrName  = "dh-itest-routes-mv-ctr"
		extraDst = "192.168.251.0/24"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	parent, err := netlink.LinkByName(harness.HostVeth)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", harness.HostVeth, err)
	}
	_, dst, err := net.ParseCIDR(extraDst)
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	hostRoute := &netlink.Route{
		LinkIndex: parent.Attrs().Index,
		Dst:       dst,
	}
	if err := netlink.RouteAdd(hostRoute); err != nil {
		t.Fatalf("RouteAdd %s dev %s: %v", extraDst, harness.HostVeth, err)
	}
	t.Cleanup(func() {
		if err := netlink.RouteDel(hostRoute); err != nil {
			t.Logf("WARN: RouteDel %s dev %s: %v", extraDst, harness.HostVeth, err)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container ip=%s", ipv4)

	out := harness.ExecOutput(t, ctx, id, "ip", "route")
	if !strings.Contains(out, extraDst) {
		t.Errorf("static route %s not propagated into macvlan container netns\ncontainer routes:\n%s", extraDst, out)
	}
}

// TestStaticRoutes_MacvlanSkipRoutesOpt checks that skip_routes=true keeps the parent-NIC route out of the container.
func TestStaticRoutes_MacvlanSkipRoutesOpt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		netName  = "dh-itest-routes-mv-skip"
		ctrName  = "dh-itest-routes-mv-skip-ctr"
		extraDst = "192.168.252.0/24"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	parent, err := netlink.LinkByName(harness.HostVeth)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", harness.HostVeth, err)
	}
	_, dst, err := net.ParseCIDR(extraDst)
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	hostRoute := &netlink.Route{
		LinkIndex: parent.Attrs().Index,
		Dst:       dst,
	}
	if err := netlink.RouteAdd(hostRoute); err != nil {
		t.Fatalf("RouteAdd %s dev %s: %v", extraDst, harness.HostVeth, err)
	}
	t.Cleanup(func() {
		if err := netlink.RouteDel(hostRoute); err != nil {
			t.Logf("WARN: RouteDel %s dev %s: %v", extraDst, harness.HostVeth, err)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"skip_routes": "true",
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "route")
	if strings.Contains(out, extraDst) {
		t.Errorf("skip_routes=true but static route %s still propagated into container\nroutes:\n%s", extraDst, out)
	}
}
