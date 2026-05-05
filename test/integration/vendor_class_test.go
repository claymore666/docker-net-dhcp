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
	if !strings.Contains(out, harness.TestTaggedGateway) {
		t.Errorf("expected default route via %s (tagged gateway); got:\n%s",
			harness.TestTaggedGateway, out)
	}
	if strings.Contains(out, harness.DefaultGateway) {
		t.Errorf("default route still points at %s — vendor_class override didn't fire upstream:\n%s",
			harness.DefaultGateway, out)
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
	if !strings.Contains(out, harness.DefaultGateway) {
		t.Errorf("expected default route via %s (default gateway); got:\n%s",
			harness.DefaultGateway, out)
	}
	if strings.Contains(out, harness.TestTaggedGateway) {
		t.Errorf("default route points at %s — vendor_class tag fired without the override:\n%s",
			harness.TestTaggedGateway, out)
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
	if !strings.Contains(out, harness.DefaultGateway) {
		t.Errorf("non-matching vendor_class should still get the default gateway %s; got:\n%s",
			harness.DefaultGateway, out)
	}
}
