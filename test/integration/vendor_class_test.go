//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestVendorClass_OverrideRoutesViaTaggedGateway is the v0.9.0 / T2-3
// integration counterpart for #106 — closes the test-plan gap noted
// in the post-merge audit.
//
// The fixture's dnsmasq is configured with --dhcp-vendorclass=set:...
// + --dhcp-option=tag:...,3,TestTaggedGateway, so a container whose
// network sets vendor_class=harness.TestVendorClass should receive
// the tagged gateway (.250) instead of dnsmasq's default
// listen-address gateway (.1). End-to-end proof that the operator's
// vendor_class override actually reaches the wire and class-based
// policy fires upstream — the unit test
// TestNewDHCPClient_VendorClassOverride only verified the udhcpc
// argv shape.
func TestVendorClass_OverrideRoutesViaTaggedGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-vc-tagged"
	ctrName := "dh-itest-vc-tagged-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"vendor_class": harness.TestVendorClass,
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	if gw := defaultRouteGateway(t, out); gw != harness.TestTaggedGateway {
		t.Errorf("default route via %s, want tagged gateway %s — vendor_class override didn't fire upstream:\n%s",
			gw, harness.TestTaggedGateway, out)
	}
	t.Logf("default route: %s", strings.TrimSpace(out))
}

// TestVendorClass_DefaultUsesUntaggedGateway is the negative side:
// without the vendor_class opt-in, the container's vendor identifier
// stays at the plugin's default ("docker-net-dhcp"), which doesn't
// match dnsmasq's tag rule, so the gateway stays on the default .1.
//
// Together with the override test above, this pins both branches of
// the class-based-policy split. Guards against:
//   - a future refactor that accidentally sets a vendor class even
//     when the operator didn't ask for one
//   - a fixture mistake that flips the tag rule's polarity
func TestVendorClass_DefaultUsesUntaggedGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-vc-default"
	ctrName := "dh-itest-vc-default-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	if gw := defaultRouteGateway(t, out); gw != harness.DefaultGateway {
		t.Errorf("default route via %s, want default gateway %s — vendor_class tag fired without the override:\n%s",
			gw, harness.DefaultGateway, out)
	}
	t.Logf("default route: %s", strings.TrimSpace(out))
}

// TestVendorClass_NonMatchingValueUsesDefaultGateway verifies that
// only the *exact* configured vendor class triggers the dnsmasq tag.
// A network that sets vendor_class to some other string still gets
// the untagged gateway — proves the override goes on the wire (not
// silently dropped), and that dnsmasq's matching is actually doing
// the work (not falling open).
func TestVendorClass_NonMatchingValueUsesDefaultGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-vc-nonmatch"
	ctrName := "dh-itest-vc-nonmatch-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"vendor_class": "totally-different-vendor-class",
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	if gw := defaultRouteGateway(t, out); gw != harness.DefaultGateway {
		t.Errorf("non-matching vendor_class should still get the default gateway %s, got %s:\n%s",
			harness.DefaultGateway, gw, out)
	}
}

// defaultRouteGateway extracts the gateway of the default route from
// `ip route` output. BusyBox `ip` (alpine test containers) ignores the
// `show default` filter and prints the whole routing table, so
// substring assertions against the raw output false-match the leased
// address: the default gateway "192.168.99.1" is a prefix of every
// lease in .10–.19, which appears in the subnet route's `src` field
// (~11% flake, #130). Parse the actual default line and compare
// gateways exactly instead.
func defaultRouteGateway(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "default" && fields[1] == "via" {
			return fields[2]
		}
	}
	t.Fatalf("no default route in ip route output:\n%s", out)
	return ""
}
