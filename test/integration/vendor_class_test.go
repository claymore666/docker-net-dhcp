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

// TestVendorClass_OverrideRoutesViaTaggedGateway checks that vendor_class makes dnsmasq's tag rule hand out the tagged gateway (#106).
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

// TestVendorClass_DefaultUsesUntaggedGateway checks that the default vendor class "docker-net-dhcp" gets the untagged gateway (#106).
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

// TestVendorClass_NonMatchingValueUsesDefaultGateway checks that only the exact configured vendor class selects the tagged gateway (#106).
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

// BusyBox `ip` ignores `show default` and prints the whole table, and the gateway 192.168.99.1 is a prefix of every
// lease in .10-.19, so a substring match flaked in about 11 % of runs (#130).

// defaultRouteGateway returns the gateway of the default route in `ip route` output.
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
