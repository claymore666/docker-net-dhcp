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

// TestDNSPropagate_OptInWritesResolvConf checks that propagate_dns=true puts the server's option 6 DNS server in resolv.conf.
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

	// resolv.conf is written from the persistent client's bound event, after Join returns (harness.RetransmitBudget).
	budget := harness.RetransmitBudget(2)
	deadline := time.Now().Add(budget)
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.TestDNSServer) {
			t.Logf("resolv.conf inside container:\n%s", out)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("DHCP DNS server %s never appeared in container's resolv.conf within %s\nlast contents:\n%s",
		harness.TestDNSServer, budget, out)
}

// TestDNSPropagate_DefaultIsUnchanged checks that without propagate_dns resolv.conf never names the fixture's DNS server.
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

// resolv.conf is written by renew() in any mode, but bridge mode reaches it through a veth whose host side the manager
// runs on and a different CreateNetwork branch; the bridge fixture's option 6 differs from the macvlan fixture's, so
// an answer from the wrong fixture fails (#899).

// TestDNSPropagate_BridgeModeWritesResolvConfToo checks that propagate_dns=true writes the bridge fixture's DNS server in bridge mode.
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

	budget := harness.RetransmitBudget(2)
	deadline := time.Now().Add(budget)
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.BridgeTestDNSServer) {
			t.Logf("resolv.conf inside the bridge-mode container:\n%s", out)

			// The macvlan fixture's server must be absent, or a container on the wrong fixture would pass once both appeared.
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
		"within %s. propagate_dns is exercised on macvlan by the test above; this is the "+
		"mode that had no coverage at all after the v6 suite was retired.\nlast contents:\n%s",
		harness.BridgeTestDNSServer, budget, out)
}
