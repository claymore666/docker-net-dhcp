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

// Each case reads the default route from the DHCP offer: both fixtures' dnsmasq hand the tagged gateway only to a client
// that sent option 77 = harness.TestUserClass, one plain word because dnsmasq splits its rule on commas (#1120).

func userClassDump(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

// TestUserClass_MacvlanRoutesViaTaggedGateway checks that user_class reaches the macvlan endpoint's DISCOVER (#1120).
func TestUserClass_MacvlanRoutesViaTaggedGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	userClassDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-uc-tagged", "macvlan", map[string]string{"user_class": harness.TestUserClass})
	id, _, _ := harness.RunContainer(t, ctx, "dh-itest-uc-tagged", "dh-itest-uc-tagged-ctr")

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	if gw := defaultRouteGateway(t, out); gw != harness.TestUserClassGateway {
		t.Errorf("default route via %s, want tagged gateway %s: the user_class option 77 did not reach the server:\n%s",
			gw, harness.TestUserClassGateway, out)
	}
}

// TestUserClass_MacvlanWithoutTheOptionUsesDefaultGateway checks that a network without user_class sends no option 77 (#1120).
func TestUserClass_MacvlanWithoutTheOptionUsesDefaultGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	userClassDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-uc-default", "macvlan", nil)
	id, _, _ := harness.RunContainer(t, ctx, "dh-itest-uc-default", "dh-itest-uc-default-ctr")

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	if gw := defaultRouteGateway(t, out); gw != harness.DefaultGateway {
		t.Errorf("default route via %s, want default gateway %s: the user-class rule fired without the option:\n%s",
			gw, harness.DefaultGateway, out)
	}
}

// TestUserClass_BridgeRoutesViaTaggedGateway checks that user_class reaches the bridge-mode one-shot in CreateEndpoint (#1120).
func TestUserClass_BridgeRoutesViaTaggedGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	userClassDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-uc-br-tagged", "bridge", map[string]string{"user_class": harness.TestUserClass})
	id, _, _ := harness.RunContainer(t, ctx, "dh-itest-uc-br-tagged", "dh-itest-uc-br-tagged-ctr")

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	if gw := defaultRouteGateway(t, out); gw != harness.BridgeUserClassGateway {
		t.Errorf("default route via %s, want tagged gateway %s: the user_class option 77 did not reach the server:\n%s",
			gw, harness.BridgeUserClassGateway, out)
	}
}

// TestUserClass_BridgeWithoutTheOptionUsesDefaultGateway checks that a bridge network without user_class sends no option 77 (#1120).
func TestUserClass_BridgeWithoutTheOptionUsesDefaultGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	userClassDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-uc-br-default", "bridge", nil)
	id, _, _ := harness.RunContainer(t, ctx, "dh-itest-uc-br-default", "dh-itest-uc-br-default-ctr")

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	want := strings.SplitN(harness.BridgeAddr, "/", 2)[0]
	if gw := defaultRouteGateway(t, out); gw != want {
		t.Errorf("default route via %s, want the bridge fixture's own address %s:\n%s", gw, want, out)
	}
}

// TestUserClass_IPAMMacvlanRoutesViaTaggedGateway checks that user_class reaches the IPAM-mode lease at RequestAddress (#1120).
func TestUserClass_IPAMMacvlanRoutesViaTaggedGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	userClassDump(t)

	harness.CreateNetworkIPAM(t, ctx, "dh-itest-uc-ipam", "macvlan", harness.SubnetCIDR, nil,
		map[string]string{"user_class": harness.TestUserClass})
	id, _, _ := harness.RunContainer(t, ctx, "dh-itest-uc-ipam", "dh-itest-uc-ipam-ctr")

	out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
	if gw := defaultRouteGateway(t, out); gw != harness.TestUserClassGateway {
		t.Errorf("default route via %s, want tagged gateway %s: the user_class option 77 did not reach the IPAM-mode lease:\n%s",
			gw, harness.TestUserClassGateway, out)
	}
}
