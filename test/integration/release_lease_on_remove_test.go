// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// release_lease=on_remove (#984) read from the server: dnsmasq's lease database and log say whether the address went
// back, and the container's address and MAC whether it was claimed back. The window is the tombstone TTL, 60 seconds,
// plus the settle and one sweep tick, so the wall clock from the stop to the datagram is 65 to 80 seconds. No knob
// shortens it, since an override would reach every plugin copy a harness spawns; pkg/plugin/deferred_release_test.go
// drives the timing with an injected clock.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// onRemoveWindow is tombstoneTTL in pkg/plugin/state.go, transcribed so the installed plugin is not asked about its own import.
const onRemoveWindow = 60 * time.Second

// onRemoveVisibleBudget is the window, plus the settle (5s), plus one sweep tick (15s), plus room for a slow runner (#984).
const onRemoveVisibleBudget = onRemoveWindow + 75*time.Second

// onRemoveHeldProbe is well inside the window and well past the ~1s in which an on_stop release would have gone out.
const onRemoveHeldProbe = 20 * time.Second

// A release one second after the stop is on_stop under another name, which the lease file alone cannot tell (#984).

// waitLeaseFileWithin is waitLeaseFile with the caller's budget, returning how long the wait took.
func waitLeaseFileWithin(t *testing.T, leaseFile, addr string, want bool, budget time.Duration) (time.Duration, bool) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(budget)
	for {
		if leaseFileHolds(t, leaseFile, addr) == want {
			return time.Since(start), true
		}
		if !time.Now().Before(deadline) {
			return time.Since(start), false
		}
		time.Sleep(releaseVisiblePoll)
	}
}

// containerAddress waits for Docker to publish the container's address and MAC on the test's network.
func containerAddress(t *testing.T, ctx context.Context, cli *docker.Client, id string) (ip, mac string) {
	t.Helper()
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				return ep.IPAddress, ep.MacAddress
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the container came back with no address inside %s", harness.IPAcquisitionBudget)
	return "", ""
}

// An implementation releasing at DeleteEndpoint passes the hand-back and fails the first assertion, that the server
// still holds the address twenty seconds after the stop (#984).

// TestReleaseLease_OnRemoveHoldsTheAddressThenHandsItBack checks that the server keeps the address inside the window and gets it back after (#984).
func TestReleaseLease_OnRemoveHoldsTheAddressThenHandsItBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-onremove"
		ctrName = "dh-itest-onremove-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"release_lease": "on_remove"})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)

	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s, so nothing below can be read", ip)
	}

	w := harness.BeginCounterWindow(t, ctx, cli,
		"releases_sent_v4", "release_failures_v4", "releases_reclaimed_v4")
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	stopped := time.Now()
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	time.Sleep(onRemoveHeldProbe)
	if !leaseFileHolds(t, fixture.LeaseFile(), ip) {
		t.Errorf("dnsmasq gave %s up %s after the stop. release_lease=on_remove holds the "+
			"address for the restart window (%s) so a container that comes straight back "+
			"gets it again; a release this early is release_lease=on_stop under another name",
			ip, onRemoveHeldProbe, onRemoveWindow)
	}

	waited, gone := waitLeaseFileWithin(t, fixture.LeaseFile(), ip, false,
		onRemoveVisibleBudget-time.Since(stopped))
	elapsed := time.Since(stopped)
	if !gone {
		t.Errorf("dnsmasq still holds %s %s after the stop. The restart window is %s and "+
			"nothing claimed the address back, so release_lease=on_remove should have "+
			"handed it to the server", ip, elapsed, onRemoveWindow)
	} else {
		t.Logf("the address went back %s after the stop (the wait itself took %s)", elapsed, waited)
	}
	if gone && elapsed < onRemoveWindow {
		t.Errorf("the address went back %s after the stop, inside the %s restart window. A "+
			"container restarting in that window is promised this address, and handing it "+
			"to the server first is the duplicate assignment the window exists to prevent",
			elapsed, onRemoveWindow)
	}

	releaseLines := fixture.CountLogLines("DHCPRELEASE", ip) - releasesBefore
	t.Logf("dnsmasq logged %d DHCPRELEASE line(s) naming %s", releaseLines, ip)
	if releaseLines < 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s, want at least 1: the lease "+
			"can leave the DB for reasons other than a release", releaseLines, ip)
	}

	// The plugin's counters, read after the outside evidence and never instead of it.
	before, after := w.End()
	t.Logf("across the window the counters moved: releases_sent_v4 by %d, "+
		"release_failures_v4 by %d, releases_reclaimed_v4 by %d",
		after.ReleasesSentV4-before.ReleasesSentV4,
		after.ReleaseFailuresV4-before.ReleaseFailuresV4,
		after.ReleasesReclaimedV4-before.ReleasesReclaimedV4)
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got < 1 {
		t.Errorf("releases_sent_v4 moved by %d, want at least 1: the server gave the "+
			"address up and the plugin did not count it", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d on a release that reached the server", got)
	}
	if got := after.ReleasesReclaimedV4 - before.ReleasesReclaimedV4; got != 0 {
		t.Errorf("releases_reclaimed_v4 moved by %d with nothing restarting on this "+
			"network; that counter is for an address a container took back", got)
	}
}

// The wait past the window is not padding: a claim check that missed would release then, with the container running
// on the address, the duplicate assignment of #524 (#984).

// TestReleaseLease_OnRemoveKeepsTheAddressForARestartInsideTheWindow checks that a restart inside the window keeps address and MAC with nothing sent (#984).
func TestReleaseLease_OnRemoveKeepsTheAddressForARestartInsideTheWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-onremove-restart"
		ctrName = "dh-itest-onremove-restart-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"release_lease": "on_remove"})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s; nothing below can be read", ip)
	}
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"releases_sent_v4", "release_failures_v4", "releases_reclaimed_v4")
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	stopped := time.Now()
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	ipAfter, macAfter := containerAddress(t, ctx, cli, id)
	t.Logf("restarted %s after the stop on ip=%s mac=%s", time.Since(stopped), ipAfter, macAfter)

	if !strings.EqualFold(macAfter, mac) {
		t.Errorf("the restarted container came back on MAC %s, not %s. release_lease="+
			"on_remove lays the tombstone exactly as `never` does, and without it the "+
			"container cannot ask for the address it had", macAfter, mac)
	}
	if ipAfter != ip {
		t.Errorf("the restarted container came back on %s, not %s", ipAfter, ip)
	}

	for time.Since(stopped) < onRemoveWindow+30*time.Second {
		time.Sleep(time.Second)
	}

	if !leaseFileHolds(t, fixture.LeaseFile(), ip) {
		t.Errorf("dnsmasq no longer holds %s, %s after the stop, while the container that "+
			"claimed it back is running on it. The address was handed to the server from "+
			"under a live container", ip, time.Since(stopped))
	}
	if got := fixture.CountLogLines("DHCPRELEASE", ip) - releasesBefore; got != 0 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) naming %s. Nothing may go on the "+
			"wire for an address a container claimed back", got, ip)
	}

	before, after := w.End()
	t.Logf("across the window the counters moved: releases_sent_v4 by %d, "+
		"releases_reclaimed_v4 by %d",
		after.ReleasesSentV4-before.ReleasesSentV4,
		after.ReleasesReclaimedV4-before.ReleasesReclaimedV4)
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got != 0 {
		t.Errorf("releases_sent_v4 moved by %d for an address that was claimed back", got)
	}
	if got := after.ReleasesReclaimedV4 - before.ReleasesReclaimedV4; got < 1 {
		t.Errorf("releases_reclaimed_v4 moved by %d, want at least 1. It is the only "+
			"outside sign that the window did what the option promises, and an operator "+
			"asking why an address was not handed back reads it", got)
	}
}

// RFC 9915 section 18.2.7 requires the address off the interface before a Release; at the deadline the link is gone.
// dnsmasq prints the same DHCPRELEASE token on both paths, so the observer is the per-family lease database (#984).

// TestReleaseLease_OnRemoveHandsTheV6AddressBackToo checks that the DHCPv6 address goes back at the window's end (#984).
func TestReleaseLease_OnRemoveHandsTheV6AddressBackToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-onremove6"
		ctrName = "dh-itest-onremove6-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"release_lease": "on_remove",
		"ipv6":          "true",
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, _ := harness.RunContainer(t, ctx, netName, ctrName)
	v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6 == "" {
		t.Fatalf("the endpoint took no DHCPv6 address, so there is no v6 release to observe")
	}
	t.Logf("container %s holds ip=%s v6=%s", ctrName, ip, v6)

	for _, addr := range []string{ip, v6} {
		if !waitLeaseFile(t, fixture.LeaseFile(), addr, true) {
			t.Fatalf("dnsmasq's lease DB has no entry for %s; a release for it could not be "+
				"told from a lease that was never recorded", addr)
		}
	}

	w := harness.BeginCounterWindow(t, ctx, cli, "releases_sent_v6", "release_failures")

	stopped := time.Now()
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	time.Sleep(onRemoveHeldProbe)
	if !leaseFileHolds(t, fixture.LeaseFile(), v6) {
		t.Errorf("dnsmasq gave the DHCPv6 address %s up %s after the stop, inside the %s "+
			"restart window", v6, onRemoveHeldProbe, onRemoveWindow)
	}

	for _, f := range []struct{ family, addr string }{{"v4", ip}, {"v6", v6}} {
		_, gone := waitLeaseFileWithin(t, fixture.LeaseFile(), f.addr, false,
			onRemoveVisibleBudget-time.Since(stopped))
		if !gone {
			t.Errorf("dnsmasq still holds the %s address %s, %s after the stop. Both "+
				"families are released on an on_remove network, and one arm working is "+
				"not the other arm working", f.family, f.addr, time.Since(stopped))
			continue
		}
		t.Logf("the %s address %s went back %s after the stop", f.family, f.addr, time.Since(stopped))
	}

	before, after := w.End()
	t.Logf("across the window releases_sent_v6 moved by %d and release_failures by %d",
		after.ReleasesSentV6-before.ReleasesSentV6,
		after.ReleaseFailures-before.ReleaseFailures)
	if got := after.ReleasesSentV6 - before.ReleasesSentV6; got < 1 {
		t.Errorf("releases_sent_v6 moved by %d, want at least 1", got)
	}
	if got := after.ReleaseFailures - before.ReleaseFailures; got != 0 {
		t.Errorf("release_failures moved by %d on a window both families completed", got)
	}
}

// In IPAM mode DeleteEndpoint retains the record and libnetwork's later ReleaseAddress finds it retained, so the sweep
// hands the address back; an address the driver still believes it owns would be handed out twice (#110, #984).

// TestReleaseLease_OnRemoveHandsAnIPAMAddressBackToo checks the same window through the bundled IPAM driver (#110, #984).
func TestReleaseLease_OnRemoveHandsAnIPAMAddressBackToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-onremove-ipam"
		ctrName = "dh-itest-onremove-ipam-ctr"
	)

	ipamDumpOnFailure(t)
	cli := ipamDockerClient(t)
	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", harness.SubnetCIDR, nil,
		map[string]string{"release_lease": "on_remove"})

	if err := ipamRunContainerErr(t, ctx, cli, netName, ctrName, &network.EndpointSettings{}); err != nil {
		t.Fatalf("the container did not start on an IPAM-mode on_remove network: %v", err)
	}
	ip, mac := ipamNetworkAddress(t, ctx, cli, ctrName, netName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)

	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s; nothing below can be read", ip)
	}

	w := harness.BeginCounterWindow(t, ctx, cli, "releases_sent_v4", "release_failures_v4")
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	stopped := time.Now()
	if err := cli.ContainerStop(ctx, ctrName, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	time.Sleep(onRemoveHeldProbe)
	if !leaseFileHolds(t, fixture.LeaseFile(), ip) {
		t.Errorf("dnsmasq gave %s up %s after the stop on an IPAM-mode network, inside the "+
			"%s restart window", ip, onRemoveHeldProbe, onRemoveWindow)
	}

	_, gone := waitLeaseFileWithin(t, fixture.LeaseFile(), ip, false,
		onRemoveVisibleBudget-time.Since(stopped))
	if !gone {
		t.Errorf("dnsmasq still holds %s %s after the stop on an IPAM-mode network. The "+
			"reference says release_lease reaches IPAM mode on this value", ip, time.Since(stopped))
	} else {
		t.Logf("the IPAM-mode address went back %s after the stop", time.Since(stopped))
	}
	if got := fixture.CountLogLines("DHCPRELEASE", ip) - releasesBefore; got < 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) naming %s, want at least 1", got, ip)
	}

	before, after := w.End()
	t.Logf("across the window releases_sent_v4 moved by %d and release_failures_v4 by %d",
		after.ReleasesSentV4-before.ReleasesSentV4,
		after.ReleaseFailuresV4-before.ReleaseFailuresV4)
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got < 1 {
		t.Errorf("releases_sent_v4 moved by %d, want at least 1", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d on a release that reached the server", got)
	}
}
