//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestClasslessStaticRoutes_AppliedFromDHCP is the integration proof for
// #260: a DHCP server that hands out a classless static route (option
// 121, RFC 3442) should have that route applied inside the container.
//
// The fixture's dnsmasq pushes TestClasslessRoute via TestClasslessRouteGW
// only to clients tagged with vendor_class = TestClasslessVendorClass, so
// a container on a network that opts into that vendor class must end up
// with the route in its table. This exercises the full path the unit
// tests can't reach: the one-shot lease's Routes ride the joinHint and
// are emitted as StaticRoutes in the Join response, which libnetwork then
// programs into the container netns.
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

// TestClasslessStaticRoutes_AbsentWithoutOptIn is the negative side: a
// default-config container (no matching vendor class) is not tagged, so
// dnsmasq never sends option 121 and the route must be absent. Guards
// against the fixture leaking the route to every client and against the
// plugin inventing routes from nowhere.
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

// routeGateway returns the `via` gateway of the route to dest in
// `ip route` output, failing the test if the route is absent. Matches on
// the exact destination field (fields[0]) to avoid the substring
// false-matches that bit the default-route helper (#130).
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

// hasRoute reports whether `ip route` output contains any route whose
// destination field is exactly dest.
func hasRoute(out, dest string) bool {
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 1 && fields[0] == dest {
			return true
		}
	}
	return false
}
