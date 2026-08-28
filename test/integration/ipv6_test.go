// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// DHCPv6 coverage (#103) — the suite's first v6 tests. The fixture
// dnsmasq instances are dual-stack (stateful DHCPv6 on ULA prefixes,
// --enable-ra); `ipv6=true` networks run a v6 dhcpcd client alongside
// the v4 one.
//
// The two findings these tests encode were originally derived from the
// busybox source (networking/udhcp/d6_dhcpc.c), because the v6 client
// used to be udhcpc6. Since #152 it is dhcpcd, so that derivation no
// longer supports anything here. Both were re-derived against dhcpcd
// (#875) rather than restated, and the mechanism changed underneath
// each of them even though the conclusion did not:
//
//   - The client identifier is a DUID-LL (type 3, RFC 8415 §11.4)
//     over the interface MAC — not a per-process timestamped
//     DUID-LLT. Under udhcpc6 that was the client's own derivation.
//     Under dhcpcd the plugin PINS it: dhcpcd.go renders duidLL(MAC)
//     as a literal `duid` value, deliberately not the `duid ll`
//     keyword, so a pre-existing /var/lib/dhcpcd/duid cannot override
//     it. MEASURED as outside evidence — the fixture dnsmasq records
//     `DUID 00:03:00:01:<mac>` for these containers, i.e. type 0x0003
//     (link-layer) + hardware type 0x0001 (Ethernet) + the six MAC
//     bytes, and the IAID is that MAC's low four bytes. DUID
//     stability across plugin restarts therefore still follows from
//     MAC stability, which the plugin guarantees (the container link
//     keeps its MAC across a plugin disable/enable; tombstones pin it
//     across container restarts). TestDUID_PersistsAcrossPluginRestart
//     asserts it end-to-end and does not depend on which client is in
//     use.
//   - DNS servers still arrive as `dns6`, but NOT "by default with no
//     -O flag" as they did under udhcpc6. dhcpcd asks because the
//     plugin's generated config asks: dhcpcd.go emits an explicit
//     request list containing domain_name_servers, which dhcpcd maps
//     to the right per-protocol code — option 6 on v4, option 23 on
//     v6. TestIPv6_DNS6Propagation asserts the arrival, not the
//     mechanism.
//
// #213 wires a preferred-address request on top: a requested v6
// (`--ip6` or tombstone-inherited) is sent as the IA_NA preferred
// address (`ia_na <iaid> / ADDR`), so v6 stickiness no longer relies
// on the server's DUID memory alone.
package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// inspectV6 returns the endpoint's GlobalIPv6Address from docker
// inspect, or "".
func inspectV6(t *testing.T, ctx context.Context, cli *docker.Client, ctrID, netName string) string {
	t.Helper()
	ins, err := cli.ContainerInspect(ctx, ctrID)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if ep := ins.NetworkSettings.Networks[netName]; ep != nil {
		return ep.GlobalIPv6Address
	}
	return ""
}

// linkGlobalV6 returns the first global-scope IPv6 address on the
// container's interface, polled until present or the budget is spent.
func linkGlobalV6(t *testing.T, ctx context.Context, ctrID string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		out := harness.ExecOutput(t, ctx, ctrID, "ip", "-6", "addr", "show", "scope", "global")
		for _, f := range strings.Fields(out) {
			if strings.Contains(f, ":") && strings.Contains(f, "/") {
				bare := strings.SplitN(f, "/", 2)[0]
				if ip := net.ParseIP(bare); ip != nil && ip.To4() == nil {
					return bare
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}

// countDHCPv6Replies counts DHCPREPLY lines mentioning addr in the
// given dnsmasq log — the v6 sibling of the DHCPACK counting in the
// lease-renew test. dnsmasq logs one DHCPREPLY per blessed
// REQUEST/RENEW, so bind=1, renewal=2.
func countDHCPv6Replies(t *testing.T, logPath, addr string, alsoMatch ...string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read dnsmasq log: %v", err)
	}
	return harness.CountDHCPv6Binds(string(data), append([]string{addr}, alsoMatch...)...)
}

// leaseDUIDForV6 extracts the client DUID from the dnsmasq lease DB
// line holding addr. v6 lease lines are "<expiry> <iaid> <addr>
// <hostname> <client-duid>"; the server's own DUID line ("duid <hex>")
// has fewer fields and never matches an address.
func leaseDUIDForV6(t *testing.T, leaseFile, addr string) string {
	t.Helper()
	data, err := os.ReadFile(leaseFile)
	if err != nil {
		t.Fatalf("read lease file: %v", err)
	}
	needle := strings.ToLower(addr)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && strings.EqualFold(fields[2], needle) {
			return fields[len(fields)-1]
		}
	}
	return ""
}

// TestLifecycleMacvlan_IPv6_GoldenPath: with ipv6=true, a container
// gets a v4 lease from the v4 pool AND a v6 lease from the ULA pool;
// docker inspect's GlobalIPv6Address agrees with the address actually
// on the link; teardown releases both families cleanly
// (client_stop_failures stays flat — this exercises the v6 half of
// dhcpManager.Stop).
func TestLifecycleMacvlan_IPv6_GoldenPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	netName := "dh-itest-v6mv"
	ctrName := "dh-itest-v6mv-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "client_stop_failures")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})

	// Lifecycle inlined so ContainerStop (and with it the v4+v6 client
	// shutdown pair) happens inside the test body, before the final
	// health assertion. Neither client releases — #800 — so what is
	// being sequenced is the stop, not a release.
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	// v4 side: same contract as the existing golden paths.
	var v4 string
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if ep := ins.NetworkSettings.Networks[netName]; ep != nil && ep.IPAddress != "" {
			v4 = ep.IPAddress
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if v4 == "" {
		t.Fatalf("no IPv4 within %v", harness.IPAcquisitionBudget)
	}
	if !harness.IsInPool(net.ParseIP(v4)) {
		t.Errorf("IPv4 %s not in fixture pool", v4)
	}

	// v6 side: the live link must carry a ULA-pool address...
	liveV6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if liveV6 == "" {
		t.Fatalf("no global IPv6 appeared on the container link")
	}
	if !harness.IsInPoolV6(net.ParseIP(liveV6)) {
		t.Errorf("live IPv6 %s not in fixture v6 pool [%s, %s]", liveV6, harness.DHCPv6PoolStart, harness.DHCPv6PoolEnd)
	}

	// ...and inspect must agree with reality. CreateEndpoint returns
	// AddressIPv6 from the initial one-shot dhcpcd exchange; the
	// persistent client re-binds with the same DUID, so the server
	// must hand back the same address. A mismatch here is the v6
	// flavour of the #104 divergence — if this fires, the audit found
	// a real edge: document it and re-scope rather than loosening
	// silently.
	insV6 := inspectV6(t, ctx, cli, id, netName)
	if insV6 == "" {
		t.Error("docker inspect has empty GlobalIPv6Address for an ipv6=true network")
	} else if !net.ParseIP(insV6).Equal(net.ParseIP(liveV6)) {
		t.Errorf("inspect IPv6 %s != live link IPv6 %s", insV6, liveV6)
	}

	assertRouterAdvertsAreBeingProcessed(t, ctx, id, liveV6, fixture.DnsmasqLog())

	// Teardown: both families release cleanly.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	before, after := w.End()
	if after.ClientStopFailures != before.ClientStopFailures {
		t.Errorf("client_stop_failures moved %d -> %d over a dual-stack lifecycle; the v6 Stop path is failing",
			before.ClientStopFailures, after.ClientStopFailures)
	}
}

// TestTombstoneRestart_PreservesIPv6 is the #213 acceptance test — the
// v6 sibling of TestTombstoneRestart_PreservesMACAndIP. On Leave the
// plugin tombstones the endpoint's v6 address; on the restart's
// CreateEndpoint that address is requested back as the DHCPv6 preferred
// address (dhcpcd `ia_na <iaid> / ADDR`), so a dual-stack container
// keeps its v6 lease across `docker restart` exactly as it keeps v4.
func TestTombstoneRestart_PreservesIPv6(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	netName := "dh-itest-v6tomb"
	ctrName := "dh-itest-v6tomb-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})
	// A normal, promptly-stopping container — see the note in
	// tombstone_restart_test.go. The v6 half never needed the slow stop;
	// the v4 half only appeared to, because a slow stop hid #402 and
	// #408. This is the IPv6 half of #408's negative control.
	id, v4Before, macBefore := harness.RunContainer(t, ctx, netName, ctrName)
	v6Before := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6Before == "" {
		t.Fatal("no global IPv6 appeared before restart")
	}
	t.Logf("before restart: v4=%s v6=%s mac=%s", v4Before, v6Before, macBefore)

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}

	// The endpoint is torn down and recreated; wait for the v6 to
	// reappear on the link before reading the settled values.
	v6After := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6After == "" {
		t.Fatalf("container did not re-acquire a global IPv6 within %v after restart", harness.IPAcquisitionBudget)
	}
	insV6 := inspectV6(t, ctx, cli, id, netName)
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	var v4After, macAfter string
	if ep := ins.NetworkSettings.Networks[netName]; ep != nil {
		v4After, macAfter = ep.IPAddress, ep.MacAddress
	}
	t.Logf("after restart:  v4=%s v6=%s (inspect v6=%s) mac=%s", v4After, v6After, insV6, macAfter)

	if macAfter != macBefore {
		t.Errorf("MAC changed across restart: before=%s after=%s (tombstone not honored)", macBefore, macAfter)
	}
	if v6After != v6Before {
		t.Errorf("IPv6 changed across restart: before=%s after=%s (tombstone v6 not requested as preferred address)", v6Before, v6After)
	}
	if v4After != v4Before {
		t.Errorf("IPv4 changed across restart: before=%s after=%s", v4Before, v4After)
	}
	if insV6 != "" && !net.ParseIP(insV6).Equal(net.ParseIP(v6After)) {
		t.Errorf("inspect IPv6 %s != live link IPv6 %s after restart", insV6, v6After)
	}
}

// TestLifecycleBridge_IPv6_GoldenPath: the same dual-stack contract
// through the bridge wiring path (veth into a Linux bridge instead of
// a macvlan child).
func TestLifecycleBridge_IPv6_GoldenPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	netName := "dh-itest-v6br"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"ipv6": "true"})
	id, v4, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6br-ctr")

	if !harness.IsInBridgePool(net.ParseIP(v4)) {
		t.Errorf("IPv4 %s not in bridge fixture pool", v4)
	}
	liveV6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if liveV6 == "" {
		t.Fatal("no global IPv6 appeared on the container link (bridge mode)")
	}
	if !harness.IsInBridgePoolV6(net.ParseIP(liveV6)) {
		t.Errorf("live IPv6 %s not in bridge fixture v6 pool [%s, %s]", liveV6, harness.BridgeDHCPv6PoolStart, harness.BridgeDHCPv6PoolEnd)
	}

	assertRouterAdvertsAreBeingProcessed(t, ctx, id, liveV6, fixture.BridgeDnsmasqLogPath())
}

// TestLeaseRenewIPv6_HonorsT1: the v6 sibling of
// TestLeaseRenew_HonorsT1 — the direct test for "DHCPv6 renewal is
// less battle-tested" (#103). dnsmasq derives T1 = lease/2 = 60s; we
// idle 75s and assert the address survived and a renewal DHCPREPLY
// landed on top of the bind's.
func TestLeaseRenewIPv6_HonorsT1(t *testing.T) {

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	netName := "dh-itest-v6renew"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// Wire + neighbor diagnostics, dumped only on failure. These are
	// what root-caused the udev MACAddressPolicy neighbor-cache
	// poisoning (#103) — DHCPv6 failures in this environment tend to
	// be L2-delivery problems that no application log can show, so
	// the capture stays.
	var dumps []*os.File
	for _, iface := range []string{harness.HostVeth, harness.IpvlanParent, harness.DHCPSegment} {
		f, err := os.CreateTemp("", "v6dbg-"+iface+"-*.txt")
		if err != nil {
			t.Fatalf("tcpdump capture file: %v", err)
		}
		td := exec.Command("tcpdump", "-i", iface, "-l", "-n", "-e",
			"udp port 546 or udp port 547 or icmp6")
		td.Stdout, td.Stderr = f, f
		if err := td.Start(); err != nil {
			t.Logf("tcpdump unavailable (%v); continuing without capture", err)
			break
		}
		dumps = append(dumps, f)
		t.Cleanup(func() {
			_ = td.Process.Kill()
			_, _ = td.Process.Wait()
		})
	}
	t.Cleanup(func() {
		for _, f := range dumps {
			if t.Failed() {
				data, _ := os.ReadFile(f.Name())
				t.Logf("--- tcpdump %s ---\n%s", f.Name(), data)
			}
			_ = os.Remove(f.Name())
		}
		if t.Failed() {
			neigh, _ := exec.Command("ip", "-6", "neigh", "show").CombinedOutput()
			t.Logf("--- host ip -6 neigh ---\n%s", neigh)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})
	id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6renew-ctr")

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- container ip -6 addr ---\n%s", harness.ExecOutput(t, context.Background(), id, "ip", "-6", "addr"))
			t.Logf("--- container ip -6 neigh ---\n%s", harness.ExecOutput(t, context.Background(), id, "ip", "-6", "neigh"))
		}
	})

	v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6 == "" {
		t.Fatal("no global IPv6 appeared on the container link")
	}
	startReplies := countDHCPv6Replies(t, fixture.DnsmasqLog(), v6)

	t.Log("waiting 75s for the DHCPv6 renewal cycle...")
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled before renewal window: %v", ctx.Err())
	case <-time.After(75 * time.Second):
	}

	after := linkGlobalV6(t, ctx, id, 5*time.Second)
	if after != v6 {
		t.Errorf("IPv6 changed across renewal window: %s -> %s", v6, after)
	}
	endReplies := countDHCPv6Replies(t, fixture.DnsmasqLog(), v6)
	t.Logf("DHCPREPLYs for %s: start=%d end=%d", v6, startReplies, endReplies)
	if endReplies-startReplies < 1 {
		t.Errorf("no renewal DHCPREPLY for %s after crossing T1 — dhcpcd v6 renewal appears stuck", v6)
	}
}

// TestIPv6_DNS6Propagation: propagate_dns=true writes the DHCPv6
// option-23 server into resolv.conf (the v6 mirror of the existing
// v4 pair). resolv.conf is last-writer-wins between the families, so
// the assertion is "the v6 nameserver appears", polled across the
// bind window.
func TestIPv6_DNS6Propagation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	t.Run("opt-in writes dns6", func(t *testing.T) {

		netName := "dh-itest-v6dns"
		harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
			"ipv6": "true", "propagate_dns": "true",
		})
		id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6dns-ctr")

		deadline := time.Now().Add(20 * time.Second)
		var out string
		for time.Now().Before(deadline) {
			out = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
			if strings.Contains(out, harness.TestDNS6Server) {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Errorf("DHCPv6 DNS server %s never appeared in resolv.conf\nlast contents:\n%s", harness.TestDNS6Server, out)
	})

	t.Run("default leaves resolv.conf alone", func(t *testing.T) {
		netName := "dh-itest-v6dnsoff"
		harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})
		id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6dnsoff-ctr")

		// Wait for the v6 bind (the moment a propagating network
		// would have written), then assert absence.
		if v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget); v6 == "" {
			t.Fatal("no global IPv6 appeared on the container link")
		}
		out := harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.TestDNS6Server) {
			t.Errorf("propagate_dns off but %s ended up in resolv.conf:\n%s", harness.TestDNS6Server, out)
		}
	})
}

// TestDUID_PersistsAcrossPluginRestart: the acceptance test for
// #103's "persistent DUID" item. The plugin pins dhcpcd's DUID-LL from
// the interface MAC (#152: a literal `duid 00:03:00:01:<MAC>` in the
// generated config), so the DUID is stable as long as the MAC is — and
// the container link's MAC survives a plugin disable/enable. The
// dnsmasq lease DB must show the SAME client DUID for the container's
// address after the plugin restarts and its recovered dhcpcd re-binds.
// This is what makes server-side v6 reservations stick across plugin
// upgrades.
func TestDUID_PersistsAcrossPluginRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	netName := "dh-itest-v6duid"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})
	id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6duid-ctr")

	v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6 == "" {
		t.Fatal("no global IPv6 appeared on the container link")
	}

	// dnsmasq records the lease (with the client DUID) once the
	// persistent client's REQUEST is REPLYed; poll for the entry.
	var duidBefore string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if duidBefore = leaseDUIDForV6(t, fixture.LeaseFile(), v6); duidBefore != "" {
			break
		}
		// Cheap lease-file read; 250ms poll trims overshoot, 30s
		// deadline unchanged (#254).
		time.Sleep(250 * time.Millisecond)
	}
	if duidBefore == "" {
		t.Fatalf("no v6 lease entry for %s in the dnsmasq lease DB", v6)
	}
	repliesBefore := countDHCPv6Replies(t, fixture.DnsmasqLog(), v6)

	// Plugin restart: same belt-and-braces shape as the recovery
	// tests — re-enable is registered as cleanup before the disable
	// so a failed assertion can't leave the runner's plugin off.
	t.Cleanup(func() {
		bg := context.Background()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})
	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 30*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	t.Log("plugin restarted; awaiting the recovered dhcpcd v6 client's re-bind...")

	// The recovered persistent client SOLICITs immediately; a fresh
	// DHCPREPLY for our address proves the post-restart exchange
	// happened (so the lease DB entry is post-restart truth, not a
	// stale leftover).
	deadline = time.Now().Add(90 * time.Second)
	rebound := false
	for time.Now().Before(deadline) {
		if countDHCPv6Replies(t, fixture.DnsmasqLog(), v6) > repliesBefore {
			rebound = true
			break
		}
		// Cheap log read; 250ms poll trims overshoot past the
		// post-restart REPLY, 90s deadline unchanged (#254).
		time.Sleep(250 * time.Millisecond)
	}
	if !rebound {
		t.Fatalf("no post-restart DHCPREPLY for %s within 90s — recovered dhcpcd v6 client never re-bound", v6)
	}

	duidAfter := leaseDUIDForV6(t, fixture.LeaseFile(), v6)
	if duidAfter == "" {
		t.Fatalf("v6 lease entry for %s vanished after plugin restart", v6)
	}
	if !strings.EqualFold(duidBefore, duidAfter) {
		t.Errorf("client DUID changed across plugin restart: %s -> %s — v6 reservations keyed on DUID will not stick",
			duidBefore, duidAfter)
	}
}

// raGuardKnobs is the sysctl contract the Router-Advertisement guard
// asserts inside the container, and the value each knob must hold
// (#875). Kept as data so the assertion below cannot quietly check
// fewer knobs than the guard claims to hold.
var raGuardKnobs = map[string]string{
	"accept_ra":         "2",
	"autoconf":          "1",
	"keep_addr_on_down": "1",
}

// containerV6Iface returns the name of the interface inside the
// container that carries addr.
//
// It is DERIVED, never assumed. The first version of this helper's
// caller hardcoded "eth0" and every read returned "No such file or
// directory": this plugin names the container link after the network
// (`dh-itest-br20`), not `eth0`, so the assertions below produced no
// measurement at all while looking like a normal failure. The
// interface that holds the leased address is by definition the one the
// guard was supposed to configure, so derive it from the address.
func containerV6Iface(t *testing.T, ctx context.Context, id, addr string) string {
	t.Helper()
	out := harness.ExecOutput(t, ctx, id, "ip", "-6", "-o", "addr", "show", "scope", "global")
	if iface := harness.V6IfaceFromAddrShow(out, addr); iface != "" {
		return iface
	}
	t.Fatalf("could not derive the container interface carrying %s; "+
		"every assertion keyed on it would measure nothing.\n"+
		"`ip -6 -o addr show scope global` said:\n%s", addr, out)
	return ""
}

// assertRouterAdvertsAreBeingProcessed is the OUTSIDE observer for
// #875. Everything else about that fix is visible only to the plugin:
// the guard's steps run inside dhcpcd's private mount namespace, its
// failures land in a health counter, and a counter reading zero is
// equally consistent with "the guard held" and "the guard never ran".
//
// So this asserts on the container's own kernel state, in the image
// that actually ships, through the managed plugin — not on anything
// the plugin says about itself.
//
// Two independent claims, because each one alone can pass while the
// fix is broken:
//
//  1. The knobs read the values the guard writes. dhcpcd's
//     if_setup_inet6() sets accept_ra=0 and autoconf=0 on every
//     carrier acquisition; without the guard's write AND its
//     read-only shield, accept_ra reads 0 here. This is the defect
//     #875 reported, observed directly.
//
//  2. A default route via a LINK-LOCAL address is present. DHCPv6
//     carries no router — the option catalogue is RFC 8415 §21 and
//     nothing in it has a next hop — and this plugin sets no IPv6
//     gateway of its own, so a default route via fe80::/10 can only
//     have been learned from a Router Advertisement. The knobs being
//     right is the plugin's doing; this is the KERNEL's, and it can
//     only happen if an advertisement was actually accepted off the
//     wire.
//
// Claim 2 was first written as a match on `proto ra`, which is a
// string the container's `ip` PROVABLY NEVER PRINTS: the test image is
// busybox, whose route output carries no `proto` field at all. It was
// absence-driven against a probe image with full iproute2 and could
// never have passed in the image CI runs. Keying on the via-address
// instead makes the assertion a property of the protocol rather than
// of one tool's formatting.
//
// Note the bound: this does not observe REFRESH. The fixture's dnsmasq
// runs with --enable-ra and no --ra-param, so its unsolicited interval
// is dnsmasq's default (up to 600s) — far outside any test budget here
// — and a container whose addr_gen_mode dhcpcd has set to NONE cannot
// solicit a fresh one (measured; see the residual in
// pkg/dhcp/ra_guard.go). Refresh over time is evidenced in the PR by
// direct measurement, not by this test.
// persistentV6BindBudget bounds the wait for the persistent v6
// client's own DHCPv6 bind. MEASURED in the CI run that exposed the
// ordering bug below: the gap between the one-shot's bind and the
// persistent client's was 2 s (bridge) and 5 s (macvlan), so this is
// roughly an order of magnitude of headroom for a loaded runner. It
// is a deadline, not a settling time — expiry fails the test.
const persistentV6BindBudget = 45 * time.Second

// awaitPersistentV6Bind blocks until the fixture's DHCP server has
// recorded a SECOND DHCPv6 bind for addr, which is the precondition
// every RA-guard assertion below depends on and which none of them
// used to establish.
//
// Why a precondition is needed at all. There are TWO dhcpcd v6 clients
// per endpoint. The one-shot runs at CreateEndpoint, in the HOST
// namespace, and it is the one whose lease Docker is told about — so
// a container has its global v6 address, and `docker inspect` agrees,
// well before the PERSISTENT client has started inside the container
// namespace. The RA guard is a property of the persistent client's
// prologue. An assertion gated only on "the address is there" is
// therefore free to run before the guard has written anything.
//
// It did. MEASURED, macvlan shard, one-second log resolution:
//
//	13:57:31  one-shot binds the address (host ns, link pre-rename)
//	13:57:34.180  test reads eth0/accept_ra          -> 1
//	13:57:34.456  test reads eth0/keep_addr_on_down  -> 0
//	13:57:35  the guard's prologue runs on eth0, then dhcpcd solicits
//
// Every value read was a kernel default: neither the guard's 2/1/1 nor
// dhcpcd's 0/0/0. The test read the right file, in the right
// namespace, one second before anything wrote to it. `autoconf` could
// never have caught this, its default and its target both being 1.
//
// The route half was blind in the same way for a different reason: it
// polls for 15 s but RETURNS ON THE FIRST SUCCESS, and at 13:57:34 the
// RA-derived default route is still present because the code that
// deletes it has not run. A fifteen-second timeout does not make an
// assertion patient if it is satisfied immediately.
//
// So the whole block was vacuous, and vacuous in the worst direction:
// it could not have witnessed #875's symptom on the UNFIXED tree
// either, which is the only thing that makes a green here mean
// anything.
//
// Why THIS anchor. It is outside evidence — the DHCP server's own
// record, not the plugin's opinion of itself (the standing rule).
// It is strictly downstream of the guard: the prologue runs before
// dhcpcd is exec'd, so a bind logged by the server proves the
// prologue completed. And it is FIX-INDEPENDENT — the persistent
// client binds on the unfixed tree too, so the precondition cannot
// quietly become a restatement of the thing under test.
//
// Why the second bind and not the first: MEASURED — the one-shot
// contributes exactly one DHCPREPLY per address (the whole fixture log
// of each failing shard held exactly one, which is also independent
// corroboration that the persistent client had not bound before the
// test gave up). dnsmasq logs one DHCPREPLY per blessed REQUEST/RENEW,
// so the persistent client's own bind is the second.
//
// This is a wait for an EVENT, not a retry: nothing here re-reads a
// failed assertion hoping for a better answer, and expiry is fatal
// rather than skipped.
// mac scopes the count to THIS endpoint. The fixture log is shared
// across every test in a shard, so counting replies by address alone
// would also count a reply left by an EARLIER container that happened
// to be handed the same pooled address, firing the anchor early and
// restoring the race this function exists to close.
//
// dnsmasq puts the client's DUID on the reply line, and the plugin
// pins that DUID as a DUID-LL over the container's MAC, so the MAC is
// an exact per-endpoint discriminator. MEASURED, one line from the
// failing run's fixture log next to the plugin's own record of the
// same endpoint:
//
//	DHCPREPLY(dh-itest-br2) fd00:6470:6864::32 00:03:00:01:ea:eb:ed:a4:b0:f5
//	"dh-itest-br20: IAID ed:a4:b0:f5"
//
// i.e. 00:03 (link-layer) + 00:01 (Ethernet) + the six MAC bytes, with
// the IAID as that MAC's low four.
//
// Worth being explicit about the direction of the bug this closes: an
// early anchor makes the knobs read kernel defaults, so it FAILS the
// test. It was a flake source, not a hole in the gate. It is fixed
// anyway because it was exactly eliminable.
func awaitPersistentV6Bind(t *testing.T, logPath, addr, mac string) {
	t.Helper()

	if logPath == "" {
		t.Fatal("awaitPersistentV6Bind: empty dnsmasq log path — the fixture was " +
			"never started, so this assertion would have measured nothing (#875)")
	}
	// An unreadable MAC must not silently degrade to an address-only
	// match. That is the same "assertion that cannot read its subject
	// quietly passes" pattern this whole change is about.
	if mac == "" {
		t.Fatal("awaitPersistentV6Bind: could not read the container link's MAC, so " +
			"the bind count cannot be scoped to this endpoint (#875)")
	}

	deadline := time.Now().Add(persistentV6BindBudget)
	replies := 0
	for time.Now().Before(deadline) {
		replies = countDHCPv6Replies(t, logPath, addr, mac)
		if replies >= 2 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("only %d DHCPv6 bind(s) for %s (mac %s) after %s — the PERSISTENT v6 "+
		"client never bound, so the Router-Advertisement guard never ran and there is "+
		"nothing here to assert on. This is a failure, not a reason to skip: an "+
		"unfixed tree would reach exactly this point too (#875)",
		replies, addr, mac, persistentV6BindBudget)
}

func assertRouterAdvertsAreBeingProcessed(t *testing.T, ctx context.Context, id, addr, logPath string) {
	t.Helper()

	iface := containerV6Iface(t, ctx, id, addr)
	mac := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/"+iface+"/address"))

	// Establish the precondition BEFORE reading any knob -- see the
	// measured ordering above. Everything below is a statement about
	// the persistent client, and until this returns there is no
	// persistent client to make a statement about.
	awaitPersistentV6Bind(t, logPath, addr, mac)

	t.Logf("RA guard: asserting on derived container interface %q (mac %s)", iface, mac)

	for knob, want := range raGuardKnobs {
		p := "/proc/sys/net/ipv6/conf/" + iface + "/" + knob
		got := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", p))
		// A read that FAILED is a different verdict from a value that is
		// wrong, and it must never be reported as either a pass or a
		// mere mismatch: it means this assertion measured nothing.
		if harness.SysctlReadFailed(got) {
			t.Errorf("COULD NOT MEASURE %s: %q. The assertion did not run — this is "+
				"not evidence the guard failed, it is evidence the observer is "+
				"pointed at the wrong place (#875)", p, got)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q — the Router-Advertisement guard did not hold "+
				"this knob inside the shipped image. accept_ra=0 is what dhcpcd's "+
				"if_setup_inet6() writes, so this reading means the guard's write or "+
				"its read-only shield did not take effect (#875)", p, got, want)
		}
	}

	// The RA itself is asynchronous: the container solicits at link-up
	// and dnsmasq answers. Poll rather than sample once.
	//
	// This poll returns on the first success, which is only sound
	// because awaitPersistentV6Bind has already run: before that, the
	// route is trivially present on ANY tree because the code that
	// would delete it has not executed yet, and this loop exits on
	// poll #1 having witnessed nothing. Do not hoist this above the
	// anchor to "save time" -- the fifteen seconds are a deadline for
	// an RA that may be slow, not a window in which the defect might
	// show up.
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var routes string
	for time.Now().Before(deadline) {
		routes = harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show", "default")
		if harness.HasLinkLocalDefaultRoute(routes) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("no default route via a link-local address on %s after %s. DHCPv6 carries no "+
		"router (RFC 8415 §21) and the plugin sets no IPv6 gateway, so the absence of one "+
		"means the kernel never accepted a Router Advertisement — the #875 symptom. "+
		"`ip -6 route show default` says:\n%s", iface, harness.IPAcquisitionBudget, routes)
}
