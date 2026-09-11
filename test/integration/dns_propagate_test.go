// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestDNSPropagate_OptInWritesResolvConf is the v0.9.0 / T1-1
// guard: when `propagate_dns=true` is set on the network, the
// container's /etc/resolv.conf must contain the DHCP-supplied DNS
// server (option 6, advertised by the fixture's dnsmasq as
// harness.TestDNSServer).
//
// Without this opt-in, Docker's embedded resolver handles DNS and
// the fixture's address never appears in resolv.conf. Together with
// the negative side of TestDNSPropagate_DefaultIsUnchanged below,
// this pins both the opt-in behaviour and the historical default.
func TestDNSPropagate_OptInWritesResolvConf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-dns"
	ctrName := "dh-itest-dns-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"propagate_dns": "true",
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	// resolv.conf is written from the persistent client's `bound`
	// event, which fires after libnetwork's Join — i.e. after
	// RunContainer's "got an IP" return. Poll briefly: the write
	// is fast but not synchronous with the inspect IP.
	deadline := time.Now().Add(5 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.TestDNSServer) {
			t.Logf("resolv.conf inside container:\n%s", out)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("DHCP DNS server %s never appeared in container's resolv.conf within 5s\nlast contents:\n%s",
		harness.TestDNSServer, out)
}

// TestDNSPropagate_DefaultIsUnchanged confirms the v0.7.0 baseline
// behaviour: without the propagate_dns opt-in, the container's
// resolv.conf is whatever Docker's resolver wrote (typically a
// 127.0.0.11 stub, or the host's nameservers — never our fixture's
// 192.168.99.53). Guards against an accidental flip of the default
// during refactors.
func TestDNSPropagate_DefaultIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-dns-default"
	ctrName := "dh-itest-dns-default-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
	if strings.Contains(out, harness.TestDNSServer) {
		t.Errorf("propagate_dns is off but DHCP DNS server %s still ended up in resolv.conf — default flipped?\ncontents:\n%s",
			harness.TestDNSServer, out)
	}
}

// TestDNSPropagate_BridgeModeWritesResolvConfToo holds the property
// that the ten retired DHCPv6 tests took with them (r2, finding 5a).
//
// WHAT WAS LOST AND WHY IT MATTERS. Base
// `dhcpv6_noaddress_modes_test.go:30-34` created BRIDGE networks with
// `propagate_dns: "true"`. It was retired with the rest of the v6 suite
// (brief §6), and every surviving user of the option —
// `TestDNSPropagate_OptInWritesResolvConf` above and
// `extra_options_test.go` — is macvlan. So after the retirement the
// opt-in was exercised on exactly one of the three modes the plugin
// ships.
//
// WHAT MAKES IT A REAL GAP RATHER THAN A TIDY ONE. resolv.conf is
// written from `renew()` in the container's netns and is not
// mode-keyed, so the write itself is shared. What is NOT shared is
// everything in front of it: bridge mode reaches the container over a
// veth pair whose host side the manager runs on, not over a macvlan
// child, and the endpoint the DHCP client is bound to is created by a
// different branch of CreateNetwork. A defect in that branch that left
// `Info.DNSServers` empty would pass every test in this file.
//
// The fixture's bridge dnsmasq advertises harness.BridgeTestDNSServer
// on option 6 — a DIFFERENT address from the macvlan fixture's, so a
// container that somehow answered from the wrong fixture fails here
// rather than passing by coincidence.
func TestDNSPropagate_BridgeModeWritesResolvConfToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-dns-br"
	ctrName := "dh-itest-dns-br-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		"propagate_dns": "true",
	})
	id, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
	harness.AssertBridgeIP(t, ipv4)

	// Same wait as the macvlan arm, and for the same reason:
	// resolv.conf is written from the Join manager's bind event, which
	// lands after libnetwork's Join returns.
	deadline := time.Now().Add(5 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.BridgeTestDNSServer) {
			t.Logf("resolv.conf inside the bridge-mode container:\n%s", out)

			// The negative half, in the same run: the macvlan
			// fixture's address must NOT be there. Without it a
			// container that had been attached to the wrong fixture
			// would satisfy the assertion above as soon as both
			// addresses appeared.
			if strings.Contains(out, harness.TestDNSServer) {
				t.Errorf("the bridge-mode container's resolv.conf also names the MACVLAN "+
					"fixture's DNS server %s; the endpoint is not on the network the test "+
					"created:\n%s", harness.TestDNSServer, out)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("bridge-mode DHCP DNS server %s never appeared in the container's resolv.conf "+
		"within 5s. propagate_dns is exercised on macvlan by the test above; this is the "+
		"mode that had no coverage at all after the v6 suite was retired.\nlast contents:\n%s",
		harness.BridgeTestDNSServer, out)
}
