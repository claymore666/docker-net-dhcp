// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// RFC 2131 section 4.4.6 defines no answer to a DHCPRELEASE, so the wait is for dnsmasq to rewrite its lease file.

// releaseVisibleBudget bounds the wait for a release to reach the server's lease DB.
const releaseVisibleBudget = 30 * time.Second

const releaseVisiblePoll = 250 * time.Millisecond

// dnsmasq prints `DHCPRELEASE` on both families, and on v4 also for a release it ignored, with `ignored` on the same
// line (rfc2131.c), so only the lease DB says the address was given up (#962). The address is the third field in a
// v4 line (`<expiry> <mac> <addr> <hostname> <client-id>`) and a v6 line (`<expiry> <iaid> <addr> <hostname> <duid>`).

// leaseFileHolds reports whether dnsmasq's lease DB still has an entry for addr.
func leaseFileHolds(t *testing.T, leaseFile, addr string) bool {
	t.Helper()
	data, err := os.ReadFile(leaseFile)
	if err != nil {
		t.Fatalf("read lease file %s: %v", leaseFile, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && strings.EqualFold(fields[2], addr) {
			return true
		}
	}
	return false
}

func waitLeaseFile(t *testing.T, leaseFile, addr string, want bool) bool {
	t.Helper()
	deadline := time.Now().Add(releaseVisibleBudget)
	for {
		if leaseFileHolds(t, leaseFile, addr) == want {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(releaseVisiblePoll)
	}
}

// on_remove is a timed release since #984: libnetwork deletes an endpoint when its container stops, not when it is
// removed, so there is no remove-time call to hang a release on.

// TestReleaseLease_TheOptionIsRefusedAtCreateOrItIsNot checks which release_lease values network create accepts and refuses (#962, #984).
func TestReleaseLease_TheOptionIsRefusedAtCreateOrItIsNot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	for _, tc := range []struct {
		name     string
		value    string
		wantErr  bool
		mentions string
	}{
		{name: "on_stop is implemented", value: "on_stop"},
		{name: "never is the default and is spellable", value: "never"},
		{name: "on_remove is implemented", value: "on_remove"},
		{name: "a typo is refused", value: "on_stpo", wantErr: true, mentions: "release_lease"},
		// The refusal names the value it did not take (#984).
		{name: "a near miss of the new value is refused", value: "on_delete", wantErr: true, mentions: "on_remove"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netName := "dhcptest-rl-" + strings.ReplaceAll(tc.value, "_", "-")
			_, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
				Driver: harness.DriverName,
				IPAM:   &network.IPAM{Driver: "null"},
				Options: map[string]string{
					"mode": "macvlan", "parent": harness.HostVeth, "release_lease": tc.value,
				},
			})
			t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), netName) })

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("release_lease=%q was refused: %v", tc.value, err)
				}
				if _, err := cli.NetworkInspect(ctx, netName, network.InspectOptions{}); err != nil {
					t.Errorf("the create was accepted and the network does not exist: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("release_lease=%q was accepted; an operator gets a network that "+
					"looks configured and behaves as `never`", tc.value)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("the refusal does not say %q, so the operator cannot tell which "+
					"option was wrong: %v", tc.mentions, err)
			}
		})
	}
}

// releases_sent is the plugin's belief, and dnsmasq may ignore a counted release, so the server's lease DB is the
// evidence. A released endpoint lays no tombstone, so its successor gets a new MAC (#962).

// TestReleaseLease_OnStopHandsTheAddressBack checks that release_lease=on_stop returns the address in the server's lease DB (#962).
func TestReleaseLease_OnStopHandsTheAddressBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-release"
		ctrName = "dh-itest-release-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"release_lease": "on_stop"})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)

	// Waits for the one-shot's lease, not for the persistent client: the stop may arrive before that client exists, the
	// `docker run --rm` case, and the release is built from the lease record (#962).
	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s, so the assertion below cannot "+
			"tell a release from a lease that was never recorded", ip)
	}

	// A delta across a plugin recycle would subtract two processes' numbers (#405).
	w := harness.BeginCounterWindow(t, ctx, cli,
		"releases_sent_v4", "release_failures_v4", "releases_sent_v6", "release_failures_v6")
	releasesBefore := fixture.CountLogLines("DHCPRELEASE", ip)

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	if !waitLeaseFile(t, fixture.LeaseFile(), ip, false) {
		t.Errorf("after `docker stop`, dnsmasq's lease DB still holds %s. "+
			"release_lease=on_stop asks for the address back at Leave, and the server "+
			"has not given it up.", ip)
	}
	releaseLines := fixture.CountLogLines("DHCPRELEASE", ip) - releasesBefore
	t.Logf("across the stop dnsmasq logged %d DHCPRELEASE line(s) naming %s", releaseLines, ip)
	if releaseLines < 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s across the stop, want at "+
			"least 1", releaseLines, ip)
	}

	// The operator's view, read after the outside evidence.
	before, after := w.End()
	t.Logf("across the stop the counters moved: releases_sent_v4 by %d, "+
		"release_failures_v4 by %d, releases_sent_v6 by %d, release_failures_v6 by %d",
		after.ReleasesSentV4-before.ReleasesSentV4,
		after.ReleaseFailuresV4-before.ReleaseFailuresV4,
		after.ReleasesSentV6-before.ReleasesSentV6,
		after.ReleaseFailuresV6-before.ReleaseFailuresV6)
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got < 1 {
		t.Errorf("releases_sent_v4 moved by %d across the stop, want at least 1: the "+
			"server gave the address up and the plugin did not count it", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d on a release that reached the server", got)
	}
	if got := after.ReleasesSentV6 + after.ReleaseFailuresV6 - before.ReleasesSentV6 - before.ReleaseFailuresV6; got != 0 {
		t.Errorf("the v6 pair moved by %d on a v4-only network", got)
	}

	// No tombstone was laid, so the restart is a new endpoint with a new MAC.
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var macAfter, ipAfter string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				ipAfter, macAfter = ep.IPAddress, ep.MacAddress
			}
		}
		if ipAfter != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if ipAfter == "" {
		t.Fatalf("the container did not come back with an address after a release")
	}
	if strings.EqualFold(macAfter, mac) {
		t.Errorf("the restarted container came back on the same MAC %s. A released "+
			"endpoint must lay no tombstone: inheriting the MAC would have this "+
			"container ask for addresses the server has already put back in its pool",
			macAfter)
	}
}

// Docker tears the endpoint down when the container stops, and the tombstone written there is what the next start
// consumes; `docker rm` is never called here, which is why on_remove is a timed release (#984).

// TestReleaseLease_DefaultNetworksStillKeepTheirAddresses checks that a network without release_lease sends no release and keeps its MAC across a stop (#962).
func TestReleaseLease_DefaultNetworksStillKeepTheirAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-norelease"
		ctrName = "dh-itest-norelease-ctr"
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
	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s; nothing below can be read", ip)
	}
	w := harness.BeginCounterWindow(t, ctx, cli,
		"tombstones_consumed", "releases_sent", "release_failures")

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	time.Sleep(leaseRetentionSettle)
	if !leaseFileHolds(t, fixture.LeaseFile(), ip) {
		t.Errorf("dnsmasq gave %s up across a stop on a network that did not ask for it. "+
			"The default is `never`: the address stays leased until it expires, exactly "+
			"as it would for a physical host that was switched off", ip)
	}

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var macAfter, ipAfter string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				ipAfter, macAfter = ep.IPAddress, ep.MacAddress
			}
		}
		if ipAfter != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if !strings.EqualFold(macAfter, mac) {
		t.Errorf("MAC changed across a stop/start without a `docker rm`: before=%s "+
			"after=%q. The tombstone written at DeleteEndpoint is what keeps it, and "+
			"this is the evidence that DeleteEndpoint runs on a STOP", mac, macAfter)
	}
	if ipAfter != ip {
		t.Errorf("address changed across a stop/start: before=%s after=%q", ip, ipAfter)
	}

	before, after := w.End()
	if got := after.TombstonesConsumed - before.TombstonesConsumed; got < 1 {
		t.Errorf("tombstones_consumed moved by %d across a stop/start with no `docker rm`, "+
			"want at least 1. Docker deletes an endpoint when its container STOPS, which "+
			"is why release_lease=on_remove is a timed release and not a DeleteEndpoint "+
			"handler", got)
	}
	for _, c := range []struct {
		name        string
		before, now int32
	}{
		{"releases_sent", before.ReleasesSent, after.ReleasesSent},
		{"release_failures", before.ReleaseFailures, after.ReleaseFailures},
	} {
		if got := c.now - c.before; got != 0 {
			t.Errorf("%s moved by %d on a network that does not set release_lease. "+
				"A counter that moves here is folded from something other than a "+
				"release this network never asked for", c.name, got)
		}
	}
}

// RFC 9915 section 18.2.7: the client MUST stop using a released address, so the v6 arm removes it from the link
// first and sends nothing if that fails; the v4 arm keeps its address, since RFC 2131 section 3.1(6) identifies the
// binding by ciaddr. dnsmasq prints `DHCPRELEASE` on both paths, so the lease DB is read per family (#962).

// TestReleaseLease_OnStopHandsTheV6AddressBackToo checks that release_lease=on_stop returns both addresses of a dual-stack endpoint (#962).
func TestReleaseLease_OnStopHandsTheV6AddressBackToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-release6"
		ctrName = "dh-itest-release6-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"release_lease": "on_stop",
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
		// Waits for the lease, not the v6 client, as in the v4 test (#962).
		if !waitLeaseFile(t, fixture.LeaseFile(), addr, true) {
			t.Fatalf("dnsmasq's lease DB has no entry for %s; a release for it could not "+
				"be told from a lease that was never recorded", addr)
		}
	}

	w := harness.BeginCounterWindow(t, ctx, cli,
		"releases_sent_v4", "releases_sent_v6", "release_failures")

	// Per family and per address, since a bare `DHCPRELEASE` count cannot say which arm printed it.
	releasesBefore := map[string]int{
		ip: fixture.CountLogLines("DHCPRELEASE", ip),
		v6: fixture.CountLogLines("DHCPRELEASE", v6),
	}

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}

	for _, f := range []struct{ family, addr string }{{"v4", ip}, {"v6", v6}} {
		if !waitLeaseFile(t, fixture.LeaseFile(), f.addr, false) {
			t.Errorf("after `docker stop`, dnsmasq's lease DB still holds the %s address %s. "+
				"Both families release on a release_lease=on_stop network, and one arm "+
				"working is not the other arm working", f.family, f.addr)
		}
	}

	t.Logf("across the stop dnsmasq logged %d DHCPRELEASE line(s) naming %s and %d naming %s",
		fixture.CountLogLines("DHCPRELEASE", ip)-releasesBefore[ip], ip,
		fixture.CountLogLines("DHCPRELEASE", v6)-releasesBefore[v6], v6)

	before, after := w.End()
	t.Logf("across the stop the counters moved: releases_sent_v4 by %d, "+
		"releases_sent_v6 by %d, release_failures by %d",
		after.ReleasesSentV4-before.ReleasesSentV4,
		after.ReleasesSentV6-before.ReleasesSentV6,
		after.ReleaseFailures-before.ReleaseFailures)
	for _, c := range []struct {
		name        string
		before, now int32
	}{
		{"releases_sent_v4", before.ReleasesSentV4, after.ReleasesSentV4},
		{"releases_sent_v6", before.ReleasesSentV6, after.ReleasesSentV6},
	} {
		if got := c.now - c.before; got < 1 {
			t.Errorf("%s moved by %d across the stop, want at least 1", c.name, got)
		}
	}
	if got := after.ReleaseFailures - before.ReleaseFailures; got != 0 {
		t.Errorf("release_failures moved by %d on a teardown both families completed. "+
			"A v6 address that could not be taken off the link before the exchange "+
			"(RFC 9915 section 18.2.7) counts here and sends nothing", got)
	}
}
