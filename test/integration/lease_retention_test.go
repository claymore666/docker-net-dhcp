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

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// leaseRetentionSettle is how long a release, if one were sent, has to
// reach the server's log after the event that would have sent it.
//
// Sized well above the observed plugin-to-dnsmasq latency rather than
// tuned to it: this budget bounds how long the test waits before
// declaring an absence, and an absence declared too early is a pass the
// tree has not earned. Every wait here is spent in full — there is no
// early exit, because there is no positive event to wait for.
const leaseRetentionSettle = 5 * time.Second

// TestLeaseRetention_NothingEverReleases is #800, asserted where it can
// actually be seen.
//
// The rule: a container is a host on this segment, and a host does not
// hand its address back when it stops. The lease expires on the server's
// clock, or the container comes back before then and re-claims it.
// Nothing this plugin runs sends a DHCPRELEASE, on any path.
//
// # Why the assertion is the server's log
//
// There is no counter for this and there deliberately is not one. A
// counter would say what the plugin believes it did; only dnsmasq's log
// says what the server actually saw, and the two came apart before —
// the reclaim this change removes counted a success while releasing an
// address a live container was still using. So the whole test is a
// statement about one file that the plugin does not write.
//
// # Why a DHCPACK is asserted first
//
// CountLogLines returns 0 for a log it cannot read, which is the same
// answer it gives for "no releases happened". Every phase below is
// therefore preceded by proof that this endpoint's traffic IS in that
// file: if the DHCPACK for this address is visible, the reader works,
// the path is right, and a zero release count is a fact about the wire
// rather than about the test.
//
// # The lifecycle, not one event
//
// Releasing had two sources and they fired at different moments — the
// client's own `release` directive on a graceful stop, and a background
// reclaim on an endpoint that no persistent client took over. Stopping
// only one of them still leaves the defect, so this walks the whole
// lifecycle and re-checks after each step.
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

	// The positive control, and the precondition. Both are the same
	// check: an address that was never ACKed cannot be released either,
	// so a run that skipped this would report "no releases" for an
	// endpoint that never had a lease.
	if got := fixture.CountLogLines("DHCPACK", ip); got < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %s. Either the endpoint never took a "+
			"lease or this test is not reading the server's log — and in both cases "+
			"the release assertions below are vacuous", ip)
	}

	// Every phase measures against the count at the START of the test,
	// not against the previous phase, so a release in phase one cannot
	// be absorbed into phase two's baseline.
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

	// Phase 1: a graceful stop. This is what the client's `release`
	// directive fired on, and the one an operator sees most.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	assertNoRelease("a graceful `docker stop`",
		"A stopped container is a host that is switched off. Releasing here is what "+
			"raced the tombstone: the tombstone promises the SAME address to the "+
			"restart that may be seconds away, and the release told the server it "+
			"was free.")

	// Phase 2: the restart. The address must come back, which is the
	// other half of the rule — not releasing is only correct if
	// re-claiming works.
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

	// Phase 3: the endpoint really going away. This is the case the
	// removed reclaim existed for, and the one where holding the lease
	// costs something — an address unavailable until it expires. That
	// cost is the accepted trade: a missed reclaim leaves a lease to
	// expire, a wrong one takes an address from something using it.
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	assertNoRelease("`docker rm -f`",
		"Even a container that is gone for good does not release. The plugin cannot "+
			"tell that apart from a restart at the moment it would have to decide, "+
			"and guessing wrong hands a live container's address to somebody else.")
}

// TestLeaseRetention_ARestartRebootsRatherThanDiscovers is the other
// half of "a lease is a lease", and it is asserted on the wire for the
// same reason the release test is.
//
// # What it is for
//
// The 2.0 plugin remembers each endpoint's lease in a durable record
// and hands it to the Join manager as proto.Params.Resume. That turns
// the first packet after a plugin restart into RFC 2131 section
// 4.4.2's INIT-REBOOT DHCPREQUEST instead of a DHCPDISCOVER, which is
// the whole of what makes an address survive the restart rather than
// be re-offered by luck.
//
// # Why the address is NOT the oracle
//
// This is the trap the test exists to avoid. Drop Resume entirely and
// the container almost always keeps its address anyway: the binding is
// still free in the server's pool, so the DISCOVER comes back with the
// same lease. An assertion that compares the address before and after
// stays green over a plugin that lost INIT-REBOOT completely, and only
// goes red on a busy segment, in production, months later, when the
// address has been handed to someone else in the meantime.
//
// So the oracle is what dnsmasq logged. After the recycle there must be
// a DHCPREQUEST for this MAC and NO new DHCPDISCOVER. Those are two
// assertions on purpose:
//
//   - no new DISCOVER is the property. A DISCOVER means the record was
//     not read, or was read and not resumed.
//   - a new REQUEST is the control. Without it, a plugin whose Join
//     manager never started at all — no packets whatsoever — would
//     satisfy the first assertion perfectly.
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

	// The MAC is what makes both counts belong to THIS endpoint.
	// CountLogLines AND-matches substrings, so an empty MAC would match
	// every line of the fixture's log and both deltas below would be
	// about the whole server.
	if mac == "" {
		t.Fatal("the container reports no MAC, so the per-endpoint counts below would be " +
			"counts of the whole fixture log")
	}
	// The precondition and the positive control in one: an endpoint
	// that never took a lease has nothing to reboot into, and a log
	// this test cannot read returns 0 for everything.
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

	// Spent in full. The INIT-REBOOT goes out as the Join manager
	// starts, but "no DISCOVER" is an ABSENCE and an absence declared
	// early is a pass the tree has not earned.
	time.Sleep(leaseRetentionSettle)

	request := fixture.CountLogLines("DHCPREQUEST", mac) - requestBefore
	discover := fixture.CountLogLines("DHCPDISCOVER", mac) - discoverBefore
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
