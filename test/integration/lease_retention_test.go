// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// No early exit: there is no positive event, and an absence declared too early is a pass not earned (#800).

// leaseRetentionSettle is how long a release, if one were sent, has to reach the server's log.
const leaseRetentionSettle = 5 * time.Second

// leaseRetentionRebootDeadline and leaseRetentionPoll bound the wait for a resumed endpoint's first packet; the test asserts which message arrived, never how long it took.
const (
	leaseRetentionRebootDeadline = 45 * time.Second
	leaseRetentionPoll           = 250 * time.Millisecond
)

// Without release_lease no path sends a DHCPRELEASE (#800, #962). The server's log is the evidence: the removed
// reclaim counted a success while releasing an address a live container was using. CountLogLines returns 0 for an
// unreadable log, so a DHCPACK is asserted first; releasing had two sources, the client's `release` directive on a
// graceful stop and a background reclaim, so the whole lifecycle is walked.

// TestLeaseRetention_NothingEverReleases checks that no DHCPRELEASE reaches the server across stop, restart and removal (#800).
func TestLeaseRetention_NothingEverReleases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-retention"
		ctrName = "dh-itest-retention-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)

	// An address never ACKed cannot be released, so this is the precondition and the positive control.
	if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s. Either the endpoint never took a "+
			"lease or this test is not reading the server's log — and in both cases "+
			"the release assertions below are vacuous", ip)
	}

	// Every phase compares against the start of the test, so a release in one phase cannot hide in the next baseline.
	baseline := fixture.CountLogLines("DHCPRELEASE", ip)
	if baseline != 0 {
		t.Logf("NOTE: %d DHCPRELEASE line(s) for %s predate this test; asserting on the delta",
			baseline, ip)
	}

	assertNoRelease := func(phase, why string) {
		t.Helper()
		time.Sleep(leaseRetentionSettle)
		if got := fixture.CountLogLines("DHCPRELEASE", ip) - baseline; got != 0 {
			t.Errorf("after %s: dnsmasq logged %d DHCPRELEASE line(s) for %s, want 0.\n%s\n"+
				"A lease is a lease (#800): the address stays leased until it expires, "+
				"exactly as it would for a physical host that rebooted or lost power.",
				phase, got, ip, why)
		} else {
			t.Logf("after %s: no DHCPRELEASE for %s", phase, ip)
		}
	}

	// A release_lease=on_stop network does release here (#962).
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	assertNoRelease("a graceful `docker stop`",
		"A stopped container is a host that is switched off. Releasing here is what "+
			"raced the tombstone: the tombstone promises the SAME address to the "+
			"restart that may be seconds away, and the release told the server it "+
			"was free.")

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var ipAfter string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				ipAfter = ep.IPAddress
			}
		}
		if ipAfter != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ipAfter != ip {
		t.Errorf("address changed across a stop/start: before=%s after=%q. Holding the "+
			"lease is only the right call if the container gets it back — that is what "+
			"makes this ordinary DHCP rather than a leak", ip, ipAfter)
	}
	assertNoRelease("a stop/start cycle", "Neither half of a restart releases.")

	// The case the removed reclaim existed for: a missed reclaim leaves a lease to expire, a wrong one takes an address in use (#800).
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	assertNoRelease("`docker rm -f`",
		"Even a container that is gone for good does not release. The plugin cannot "+
			"tell that apart from a restart at the moment it would have to decide, "+
			"and guessing wrong hands a live container's address to somebody else.")
}

// The durable lease record is handed to the Join manager as proto.Params.Resume, making the first packet after a
// plugin restart an RFC 2131 section 4.4.2 INIT-REBOOT DHCPREQUEST (PR #899). The address is not the oracle: without
// Resume, a DISCOVER is usually re-offered the same free binding. No new DISCOVER is the property, and a new REQUEST is
// the control against a Join manager that sent nothing.

// TestLeaseRetention_ARestartRebootsRatherThanDiscovers checks that after a plugin recycle the endpoint sends a DHCPREQUEST and no DHCPDISCOVER (PR #899).
func TestLeaseRetention_ARestartRebootsRatherThanDiscovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-reboot"
		ctrName = "dh-itest-reboot-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	_, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)

	// CountLogLines AND-matches substrings, so an empty MAC would match every line.
	if mac == "" {
		t.Fatal("the container reports no MAC, so the per-endpoint counts below would be " +
			"counts of the whole fixture log")
	}
	// An endpoint that never took a lease has nothing to reboot into, and an unreadable log returns 0.
	if got := fixture.CountLogLines("DHCPACK", mac); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s: either the endpoint never leased or "+
			"this test is not reading the server's log, and every count below is vacuous", mac)
	}

	discoverBefore := fixture.CountLogLines("DHCPDISCOVER", mac)
	requestBefore := fixture.CountLogLines("DHCPREQUEST", mac)
	t.Logf("before the recycle: %d DHCPDISCOVER, %d DHCPREQUEST for %s",
		discoverBefore, requestBefore, mac)

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)
	t.Log("plugin recycled")

	// RFC 2131 puts the DHCPREQUEST first on the reboot path (4.4.2) and the DHCPDISCOVER first on the init path (4.4.1),
	// so the first packet is the verdict; the deadline only has to outlast a slow plugin start.
	var request, discover int
	deadline := time.Now().Add(leaseRetentionRebootDeadline)
	for {
		request = fixture.CountLogLines("DHCPREQUEST", mac) - requestBefore
		discover = fixture.CountLogLines("DHCPDISCOVER", mac) - discoverBefore
		if request > 0 || discover > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(leaseRetentionPoll)
	}
	t.Logf("after the recycle: +%d DHCPREQUEST, +%d DHCPDISCOVER for %s", request, discover, mac)

	if request < 1 {
		t.Errorf("no DHCPREQUEST reached the server for %s after the plugin recycle. The "+
			"resumed client never spoke at all, so the absence of a DISCOVER below says "+
			"nothing about INIT-REBOOT.", mac)
	}
	if discover != 0 {
		t.Errorf("the plugin sent %d DHCPDISCOVER for %s after the recycle, want 0. The "+
			"endpoint's remembered lease was not resumed as an INIT-REBOOT "+
			"(RFC 2131 4.4.2). The address usually comes back anyway because the binding "+
			"is still free in the fixture pool — which is exactly why this asserts on the "+
			"server's log and not on the address.", discover, mac)
	}
}
