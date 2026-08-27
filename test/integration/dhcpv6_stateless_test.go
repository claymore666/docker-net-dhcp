// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestDHCPv6_Stateless_ConfigurationIsApplied is the acceptance test
// for #815: on a stateless DHCPv6 segment the plugin was receiving a
// perfectly good configuration reply and throwing it away.
//
// The two subtests are deliberately a matched pair and neither is
// sufficient alone. The stateless case says the configuration arrives
// and is applied; the SLAAC case says that outcome came from the
// DHCPv6 exchange and not from anything else on the segment, because
// SLAAC differs from stateless in exactly one thing — whether there is
// a DHCPv6 server to ask — and produces a container with an address
// and no configuration.
//
// Each subtest brings up its own segment and its own container and
// asserts against those. There is deliberately no assertion here of
// the form "the stateless segment behaves correctly" aggregated over
// anything: #816, #820 and #821 will each rely on their own
// observation, and a claim about a population is not a claim about its
// members.
func TestDHCPv6_Stateless_ConfigurationIsApplied(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	t.Run("stateless segment configures the container", func(t *testing.T) {
		f := harness.NewV6Fixture(t, harness.V6Stateless)
		t.Cleanup(func() {
			if t.Failed() {
				f.DumpLogs(func(s string) { t.Log(s) })
				harness.DumpPluginLog(t)
			}
		})

		w := harness.BeginCounterWindow(t, ctx, cli, "dhcpv6_config_only")

		netName := "dh-itest-v6sl"
		harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
			"bridge":        f.Bridge(),
			"ipv6":          "true",
			"propagate_dns": "true",
		})
		id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6sl-ctr")

		// Intent: the plugin says it took the configuration-only path.
		after, ok := w.Await(harness.IPAcquisitionBudget+30*time.Second,
			func(now, before *harness.HealthResponse) bool {
				return now.DHCPv6ConfigOnly > before.DHCPv6ConfigOnly
			})
		if !ok {
			t.Errorf("dhcpv6_config_only never rose above %d (last read %d) — "+
				"the plugin did not record handling a configuration-only DHCPv6 reply",
				w.Before().DHCPv6ConfigOnly, after.DHCPv6ConfigOnly)
		}
		// Close the window here: Await has already refused to hand back
		// a delta across a plugin restart, and everything below is
		// observed outside the plugin.
		w.End()

		// Effect, from outside the plugin: the server logged an
		// information request and answered it. DHCPINFORMATION-REQUEST
		// and DHCPREPLY are protocol tokens dnsmasq prints verbatim,
		// unlike its prose, which the runner's locale translates.
		if n := f.CountLogLines("DHCPINFORMATION-REQUEST"); n == 0 {
			t.Error("the DHCP server logged no DHCPv6 information request; " +
				"whatever configured the container, it was not this segment")
		}
		if n := f.CountLogLines("DHCPREPLY"); n == 0 {
			t.Error("the DHCP server logged no DHCPv6 reply")
		}

		// ...and it was stateless: the client never asked for an
		// address. A SOLICIT here would mean the segment drifted into
		// the managed mode and the test would be measuring that
		// instead.
		if n := f.CountLogLines("DHCPSOLICIT"); n != 0 {
			t.Errorf("the client sent %d DHCPv6 SOLICIT(s) on a stateless segment — "+
				"this is not the exchange the test means to observe", n)
		}

		// Effect, in the container: both halves of the reply. The
		// search list is the second defect in #815 and is dropped on
		// stateful replies too, so a test that only checked the
		// nameserver would have left it live.
		var out string
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
			if strings.Contains(out, harness.V6DNSServer) && strings.Contains(out, harness.V6SearchDomain) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !strings.Contains(out, harness.V6DNSServer) {
			t.Errorf("the advertised DHCPv6 nameserver %s never reached resolv.conf:\n%s",
				harness.V6DNSServer, out)
		}
		if !strings.Contains(out, harness.V6SearchDomain) {
			t.Errorf("the advertised DHCPv6 search domain %s never reached resolv.conf:\n%s",
				harness.V6SearchDomain, out)
		}
	})

	t.Run("slaac segment has nobody to ask", func(t *testing.T) {
		f := harness.NewV6Fixture(t, harness.V6SLAAC)
		t.Cleanup(func() {
			if t.Failed() {
				f.DumpLogs(func(s string) { t.Log(s) })
				harness.DumpPluginLog(t)
			}
		})

		w := harness.BeginCounterWindow(t, ctx, cli, "dhcpv6_config_only")

		netName := "dh-itest-v6slaac"
		harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
			"bridge":        f.Bridge(),
			"ipv6":          "true",
			"propagate_dns": "true",
		})
		id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6slaac-ctr")

		// The preservation half. Without this the subtest passes just
		// as well on a segment that is simply broken, and then it is
		// not a control — it is a second way of observing nothing.
		if v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget); v6 == "" {
			t.Fatal("no global IPv6 appeared on the container link; the SLAAC segment " +
				"is not working, so it cannot serve as the control for the stateless one")
		}

		if n := f.CountLogLines("DHCPINFORMATION-REQUEST"); n != 0 {
			t.Errorf("%d DHCPv6 information request(s) on a segment with no DHCPv6 server", n)
		}

		out := harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.V6DNSServer) {
			t.Errorf("nameserver %s reached resolv.conf on a SLAAC-only segment, "+
				"where nothing advertises it:\n%s", harness.V6DNSServer, out)
		}
		if strings.Contains(out, harness.V6SearchDomain) {
			t.Errorf("search domain %s reached resolv.conf on a SLAAC-only segment:\n%s",
				harness.V6SearchDomain, out)
		}

		before, after := w.End()
		if after.DHCPv6ConfigOnly != before.DHCPv6ConfigOnly {
			t.Errorf("dhcpv6_config_only moved %d -> %d with no DHCPv6 server on the segment",
				before.DHCPv6ConfigOnly, after.DHCPv6ConfigOnly)
		}
	})
}
