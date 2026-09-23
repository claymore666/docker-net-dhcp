// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The fixture sends option 121 only to vendor class TestClasslessVendorClass; the route rides the one-shot lease's
// joinHint into Join's StaticRoutes, which libnetwork programs (#260, RFC 3442).

// TestClasslessStaticRoutes_AppliedFromDHCP checks that an option 121 route from the server is applied in the container.
func TestClasslessStaticRoutes_AppliedFromDHCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-csr"
	ctrName := "dh-itest-csr-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"vendor_class": harness.TestClasslessVendorClass,
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show")
	if gw := routeGateway(t, out, harness.TestClasslessRoute); gw != harness.TestClasslessRouteGW {
		t.Errorf("route to %s via %q, want DHCP-pushed gateway %s — option 121 didn't reach the container:\n%s",
			harness.TestClasslessRoute, gw, harness.TestClasslessRouteGW, out)
	}
	t.Logf("classless route applied: %s", strings.TrimSpace(out))
}

// TestClasslessStaticRoutes_AbsentWithoutOptIn checks that a container outside the vendor class gets no option 121 route (#260).
func TestClasslessStaticRoutes_AbsentWithoutOptIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-csr-none"
	ctrName := "dh-itest-csr-none-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show")
	if hasRoute(out, harness.TestClasslessRoute) {
		t.Errorf("route to %s present without the vendor-class opt-in — option 121 leaked:\n%s",
			harness.TestClasslessRoute, out)
	}
}

// routeGateway returns the `via` gateway of the route to dest, matching the destination field exactly (#130).
func routeGateway(t *testing.T, out, dest string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == dest && fields[1] == "via" {
			return fields[2]
		}
	}
	t.Fatalf("no route to %s in ip route output:\n%s", dest, out)
	return ""
}

// hasRoute reports whether `ip route` output has a route whose destination is exactly dest.
func hasRoute(out, dest string) bool {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 1 && fields[0] == dest {
			return true
		}
	}
	return false
}
