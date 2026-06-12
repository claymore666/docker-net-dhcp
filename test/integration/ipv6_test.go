//go:build integration

// DHCPv6 coverage (#103) — the suite's first v6 tests. The fixture
// dnsmasq instances are dual-stack (stateful DHCPv6 on ULA prefixes,
// --enable-ra); `ipv6=true` networks run udhcpc6 alongside udhcpc.
//
// Audit findings these tests encode (from the busybox source,
// networking/udhcp/d6_dhcpc.c):
//   - udhcpc6's CLIENTID is a DUID-LL (type 3) derived from the
//     interface MAC — NOT a per-process timestamped DUID-LLT. DUID
//     stability across plugin restarts therefore follows from MAC
//     stability, which the plugin already guarantees (the container
//     link keeps its MAC across a plugin disable/enable; tombstones
//     pin it across container restarts). TestDUID_PersistsAcross-
//     PluginRestart asserts this end-to-end.
//   - option 23 (DNS servers) is requested by default — the `dns6`
//     env arrives without any -O flag.
//   - there is no "request this address" flag for v6 (`-r` is v4
//     only), so v6 leases always come unhinted; continuity relies on
//     the server recognising the stable DUID.
package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
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
// given dnsmasq log — the v6 sibling of countDHCPACKs. dnsmasq logs
// one DHCPREPLY per blessed REQUEST/RENEW, so bind=1, renewal=2.
func countDHCPv6Replies(t *testing.T, logPath, addr string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read dnsmasq log: %v", err)
	}
	needle := strings.ToLower(addr)
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "dhcpreply") && strings.Contains(l, needle) {
			count++
		}
	}
	return count
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
// (lease_release_failures stays flat — this exercises the v6 half of
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

	before, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (before): %v", err)
	}

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6": "true"})

	// Lifecycle inlined so ContainerStop (and with it the v4+v6
	// DHCPRELEASE pair) happens inside the test body, before the
	// final health assertion.
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		&container.HostConfig{},
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
	// AddressIPv6 from the initial one-shot udhcpc6 exchange; the
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

	// Teardown: both families release cleanly.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	after, err := harness.PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("Plugin.Health (after): %v", err)
	}
	if after.LeaseReleaseFailures != before.LeaseReleaseFailures {
		t.Errorf("lease_release_failures moved %d -> %d over a dual-stack lifecycle; the v6 Stop path is failing",
			before.LeaseReleaseFailures, after.LeaseReleaseFailures)
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

	// TEMPORARY CI diagnostics (#103): the persistent udhcpc6 SOLICITs
	// forever in the container netns while dnsmasq logs matching
	// ADVERTISEs that never arrive — local replication of the exact
	// topology works, so capture the wire + neighbor state in the real
	// environment. Dumped only on failure; remove once root-caused.
	var dumps []*os.File
	for _, iface := range []string{harness.HostVeth, harness.DhcpVeth} {
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
		t.Errorf("no renewal DHCPREPLY for %s after crossing T1 — udhcpc6 renewal appears stuck", v6)
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
// #103's "persistent DUID" item, resolved by the audit: busybox
// udhcpc6 derives a DUID-LL from the interface MAC (d6_dhcpc.c), so
// the DUID is stable as long as the MAC is — and the container link's
// MAC survives a plugin disable/enable. The dnsmasq lease DB must
// show the SAME client DUID for the container's address after the
// plugin restarts and its recovered udhcpc6 re-binds. This is what
// makes server-side v6 reservations stick across plugin upgrades.
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
		time.Sleep(time.Second)
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
	t.Log("plugin restarted; awaiting the recovered udhcpc6's re-bind...")

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
		time.Sleep(time.Second)
	}
	if !rebound {
		t.Fatalf("no post-restart DHCPREPLY for %s within 90s — recovered udhcpc6 never re-bound", v6)
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
