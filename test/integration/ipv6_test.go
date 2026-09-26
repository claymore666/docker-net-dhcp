// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// DHCPv6 coverage restored onto the 2.0 chassis (#911), with the properties 1.9.0 asserted (#103, #213, #875). The
// identity is minted once at CreateEndpoint and stored on the record, with the 1.x DUID-LL value on bridge and macvlan.
// The client runs duplicate-address detection before reporting (RFC 9915 section 18.2.10.1) and the chassis installs
// with IFA_F_NODAD. DHCPv6 carries no next hop (RFC 9915 section 21, RFC 5942 section 4), so since v2.2.0 the plugin's
// client processes advertisements and the guard writes accept_ra=0 and autoconf=0 and purges kernel routes (#821).
package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// inspectV6 returns the endpoint's GlobalIPv6Address from docker inspect, or "".
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

// linkGlobalV6 returns the first global-scope IPv6 address on the container's interface, polled until present or the budget is spent.
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

// dnsmasq logs one DHCPREPLY per accepted REQUEST or RENEW, so bind=1 and renewal=2.

// countDHCPv6Replies counts DHCPREPLY lines mentioning addr in the given dnsmasq log.
func countDHCPv6Replies(t *testing.T, logPath, addr string, alsoMatch ...string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read dnsmasq log: %v", err)
	}
	return harness.CountDHCPv6Binds(string(data), append([]string{addr}, alsoMatch...)...)
}

// lastDHCPv6ReplyAt returns the server's stamp on the last DHCPREPLY for addr, and whether one was readable.
func lastDHCPv6ReplyAt(t *testing.T, logPath, addr string) (time.Time, bool) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read dnsmasq log: %v", err)
	}
	return harness.LastDHCPv6BindAt(string(data), time.Now(), addr)
}

// countLogToken counts lines of the dnsmasq log carrying every needle.
func countLogToken(t *testing.T, logPath string, needles ...string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read dnsmasq log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		lowered := strings.ToLower(line)
		all := true
		for _, needle := range needles {
			if !strings.Contains(lowered, strings.ToLower(needle)) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// v6 lease lines are "<expiry> <iaid> <addr> <hostname> <client-duid>"; the server's own "duid <hex>" line has fewer
// fields (#103).

// leaseDUIDForV6 extracts the client DUID from the dnsmasq lease DB line holding addr.
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

// TestIPv6_AcceptedAtCreate checks that `ipv6=true` is accepted at create and the network exists afterwards (#911).
func TestIPv6_AcceptedAtCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	const netName = "dhcptest-ipv6-accepted"
	_, err = cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver:  harness.DriverName,
		IPAM:    &network.IPAM{Driver: "null"},
		Options: map[string]string{"mode": "macvlan", "parent": harness.HostVeth, "ipv6": "true"},
	})
	t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), netName) })
	if err != nil {
		t.Fatalf("an ipv6=true network was refused: %v", err)
	}

	if _, err := cli.NetworkInspect(ctx, netName, network.InspectOptions{}); err != nil {
		t.Errorf("the create was accepted and the network does not exist: %v", err)
	}
}

// TestIPv6_TheV4OnlyPathIsUnchanged checks that a v4-only network works as before and gets no IPv6 address.
func TestIPv6_TheV4OnlyPathIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dhcptest-ipv6-control"
	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, v4, _ := harness.RunContainer(t, ctx, netName, "dhcptest-ipv6-control-ctr")
	if !harness.IsInPool(net.ParseIP(v4)) {
		t.Errorf("IPv4 %s not in fixture pool", v4)
	}
	out := harness.ExecOutput(t, ctx, id, "ip", "-6", "addr", "show", "scope", "global")
	for _, f := range strings.Fields(out) {
		if strings.Contains(f, ":") && strings.Contains(f, "/") {
			if ip := net.ParseIP(strings.SplitN(f, "/", 2)[0]); ip != nil && ip.To4() == nil {
				t.Errorf("a global IPv6 address %v appeared on a network created without "+
					"ipv6=true:\n%s", ip, out)
			}
		}
	}
}

// TestLifecycleMacvlan_IPv6_GoldenPath checks that with ipv6=true a macvlan container gets both families, inspect matches the link, and teardown stops both cleanly.
func TestLifecycleMacvlan_IPv6_GoldenPath(t *testing.T) {
	testLifecycleMacvlanIPv6GoldenPath(t, onV6Macvlan, "dh-itest-v6mv")
}

// TestLifecycleMacvlan_IPv6_GoldenPath_IPAM is the macvlan golden path with this plugin as the IPAM driver (#960).
func TestLifecycleMacvlan_IPv6_GoldenPath_IPAM(t *testing.T) {
	testLifecycleMacvlanIPv6GoldenPath(t, onV6IPAMMacvlan, "dh-itest-v6mvi")
}

func testLifecycleMacvlanIPv6GoldenPath(t *testing.T, at v6Attach, netName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ctrName := netName + "-ctr"

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

	at.createNet(t, ctx, netName, map[string]string{"ipv6": "true"})

	// Neither client releases (#800), so the test sequences the stop.
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

	liveV6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if liveV6 == "" {
		t.Fatalf("no global IPv6 appeared on the container link")
	}
	if !harness.IsInPoolV6(net.ParseIP(liveV6)) {
		t.Errorf("live IPv6 %s not in fixture v6 pool [%s, %s]", liveV6, harness.DHCPv6PoolStart, harness.DHCPv6PoolEnd)
	}

	// The persistent client re-binds with the DUID and IAID stored on the record, so the server hands back the address
	// CreateEndpoint reported; a mismatch is the v6 form of #104.
	insV6 := inspectV6(t, ctx, cli, id, netName)
	if insV6 == "" {
		t.Error("docker inspect has empty GlobalIPv6Address for an ipv6=true network")
	} else if !net.ParseIP(insV6).Equal(net.ParseIP(liveV6)) {
		t.Errorf("inspect IPv6 %s != live link IPv6 %s", insV6, liveV6)
	}

	assertLeasedV6IsInstalledWithNODAD(t, ctx, id, liveV6, fixture.DnsmasqLog())
	assertRouterAdvertsAreBeingProcessed(t, ctx, id, liveV6, fixture.DnsmasqLog())

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	before, after := w.End()
	if after.ClientStopFailures != before.ClientStopFailures {
		t.Errorf("client_stop_failures moved %d -> %d over a dual-stack lifecycle; the v6 Stop path is failing",
			before.ClientStopFailures, after.ClientStopFailures)
	}
}

// TestLifecycleBridge_IPv6_GoldenPath checks the same dual-stack contract through the bridge wiring path.
func TestLifecycleBridge_IPv6_GoldenPath(t *testing.T) {
	testLifecycleBridgeIPv6GoldenPath(t, onV6Bridge, "dh-itest-v6br")
}

// TestLifecycleBridge_IPv6_GoldenPath_IPAM is the bridge golden path with this plugin as the IPAM driver (#960).
func TestLifecycleBridge_IPv6_GoldenPath_IPAM(t *testing.T) {
	testLifecycleBridgeIPv6GoldenPath(t, onV6IPAMBridge, "dh-itest-v6bri")
}

func testLifecycleBridgeIPv6GoldenPath(t *testing.T, at v6Attach, netName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	at.createNet(t, ctx, netName, map[string]string{"ipv6": "true"})
	id, v4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

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

	assertLeasedV6IsInstalledWithNODAD(t, ctx, id, liveV6, fixture.BridgeDnsmasqLogPath())
	assertRouterAdvertsAreBeingProcessed(t, ctx, id, liveV6, fixture.BridgeDnsmasqLogPath())
}

// On Leave the v6 address is tombstoned and goes back out as the Solicit's IA_ADDR hint (proto.Params6.Hint), and the
// DUID and IAID come off the record (#213).

// TestTombstoneRestart_PreservesIPv6 checks that a dual-stack container keeps its v6 address across `docker restart` (#213).
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
	// A promptly stopping container; a slow stop hid #402 and #408, and this is the IPv6 half of #408's negative control.
	id, v4Before, macBefore := harness.RunContainer(t, ctx, netName, ctrName)
	v6Before := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6Before == "" {
		t.Fatal("no global IPv6 appeared before restart")
	}
	t.Logf("before restart: v4=%s v6=%s mac=%s", v4Before, v6Before, macBefore)

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}

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
		t.Errorf("IPv6 changed across restart: before=%s after=%s (the tombstoned v6 address was not sent as the Solicit's hint, or the identity moved)", v6Before, v6After)
	}
	if v4After != v4Before {
		t.Errorf("IPv4 changed across restart: before=%s after=%s", v4Before, v4After)
	}
	if insV6 != "" && !net.ParseIP(insV6).Equal(net.ParseIP(v6After)) {
		t.Errorf("inspect IPv6 %s != live link IPv6 %s after restart", insV6, v6After)
	}
}

// dnsmasq derives DHCPv6 T1 as lease/2 = 60s from the 2m lease and cannot advertise it separately, so the wait stands.
// A second DHCPREPLY lands within seconds of the bind (run 34203647801), so the baseline is taken a slop before T1 and
// the window is anchored on the stamp of the bind's DHCPREPLY (#103).

// TestLeaseRenewIPv6_HonorsT1 checks from the server's log that a DHCPv6 renewal happens at T1 and keeps the address (#103).
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

	// These diagnostics root-caused the udev MACAddressPolicy neighbour-cache poisoning (#103).
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

	// t1 is dnsmasq's lease/2; t1Slop keeps a renewal exactly at t1 out of the baseline; ceiling allows a loaded runner.
	const (
		t1             = 60 * time.Second
		t1Slop         = 5 * time.Second
		renewalCeiling = 75 * time.Second
	)

	// T1 starts when dnsmasq sends the reply, and the address shows in the container after DAD, a netlink hop and a poll,
	// which left 4 to 5 seconds before the renewal; the anchor is the reply's own stamp, with the client-side time as a
	// logged fallback (#103). No reply in [t1-t1Slop, ceiling] means the timer did not fire in the window; a different
	// address means the lease was replaced.
	clientAnchor := time.Now()
	anchor, anchorName := clientAnchor, "the address surfacing (server stamp unreadable)"
	if serverBind, ok := lastDHCPv6ReplyAt(t, fixture.DnsmasqLog(), v6); ok {
		anchor, anchorName = serverBind, "the server's own DHCPREPLY stamp"
		t.Logf("anchor: %s, %s before the address surfaced",
			anchorName, clientAnchor.Sub(serverBind).Round(time.Second))
	} else {
		t.Logf("anchor: %s", anchorName)
	}

	if wait := time.Until(anchor.Add(t1 - t1Slop)); wait > 0 {
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled before the renewal window opened: %v", ctx.Err())
		case <-time.After(wait):
		}
	}
	baseline := countDHCPv6Replies(t, fixture.DnsmasqLog(), v6)
	atBind := startReplies
	t.Logf("DHCPREPLYs for %s: %d at the bind, %d at %s after %s — watching for one more until %s",
		v6, atBind, baseline, t1-t1Slop, anchorName, renewalCeiling)

	deadline := anchor.Add(renewalCeiling)
	endReplies := baseline
	for time.Now().Before(deadline) {
		if endReplies = countDHCPv6Replies(t, fixture.DnsmasqLog(), v6); endReplies > baseline {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled inside the renewal window: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	t.Logf("DHCPREPLYs for %s: baseline=%d end=%d at %s after %s",
		v6, baseline, endReplies, time.Since(anchor).Round(time.Second), anchorName)

	// Read after the reply, so the comparison spans the renewal.
	after := linkGlobalV6(t, ctx, id, 5*time.Second)
	if after != v6 {
		t.Errorf("IPv6 changed across renewal window: %s -> %s", v6, after)
	}
	if endReplies <= baseline {
		t.Errorf("no DHCPREPLY for %s in the %s..%s window after %s — T1 is %s, "+
			"so the v6 renewal timer never fired (the server logged %d reply/replies before "+
			"the window opened, and none inside it)",
			v6, t1-t1Slop, renewalCeiling, anchorName, t1, baseline)
	}
}

// resolv.conf is last-writer-wins between the families, so the v6 nameserver's appearance is polled.

// TestIPv6_DNS6Propagation checks that propagate_dns=true writes the DHCPv6 option-23 server into resolv.conf.
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

		if v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget); v6 == "" {
			t.Fatal("no global IPv6 appeared on the container link")
		}
		out := harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
		if strings.Contains(out, harness.TestDNS6Server) {
			t.Errorf("propagate_dns off but %s ended up in resolv.conf:\n%s", harness.TestDNS6Server, out)
		}
	})
}

// The DUID is stored on the endpoint's record at CreateEndpoint, so a recycle must read it back; that keeps server-side
// v6 reservations across plugin upgrades (#103).

// TestDUID_PersistsAcrossPluginRestart checks that the lease DB shows the same client DUID after a plugin restart.
func TestDUID_PersistsAcrossPluginRestart(t *testing.T) {
	testDUIDPersistsAcrossPluginRestart(t, onV6Macvlan, "dh-itest-v6duid")
}

// TestDUID_PersistsAcrossPluginRestart_IPAM is the same check on the record ipam_endpoint.go stores (#960).
func TestDUID_PersistsAcrossPluginRestart_IPAM(t *testing.T) {
	testDUIDPersistsAcrossPluginRestart(t, onV6IPAMMacvlan, "dh-itest-v6duidi")
}

func testDUIDPersistsAcrossPluginRestart(t *testing.T, at v6Attach, netName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

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

	at.createNet(t, ctx, netName, map[string]string{"ipv6": "true"})
	id, _, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

	v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6 == "" {
		t.Fatal("no global IPv6 appeared on the container link")
	}
	assertDUIDStableAcrossAPluginRestart(t, ctx, cli, v6)
}

// An ipvlan L2 slave inherits the parent's MAC, so a MAC-derived DUID gave every container one DHCPv6 identity and one
// address (#895, the v6 form of #219); the plugin mints a per-endpoint DUID-UUID there, and only the record carries it.

// TestIPvlan_DHCPv6IdentityIsPerEndpointAndSurvivesARestart checks that ipvlan containers get different v6 addresses and keep their DUID across a restart (#895).
func TestIPvlan_DHCPv6IdentityIsPerEndpointAndSurvivesARestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	netName := "dh-itest-v6ipvl"

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

	harness.CreateNetwork(t, ctx, netName, "ipvlan", map[string]string{"ipv6": "true"})
	idA, _, macA := harness.RunContainer(t, ctx, netName, "dh-itest-v6ipvl-a")
	idB, _, macB := harness.RunContainer(t, ctx, netName, "dh-itest-v6ipvl-b")

	v6A := linkGlobalV6(t, ctx, idA, harness.IPAcquisitionBudget)
	v6B := linkGlobalV6(t, ctx, idB, harness.IPAcquisitionBudget)
	if v6A == "" || v6B == "" {
		t.Fatalf("an ipvlan container has no global IPv6: a=%q b=%q", v6A, v6B)
	}
	linkA := containerLinkMAC(t, ctx, idA)
	linkB := containerLinkMAC(t, ctx, idB)
	t.Logf("ipvlan endpoints: a=%s (docker mac %q, link mac %s) b=%s (docker mac %q, link mac %s)",
		v6A, macA, linkA, v6B, macB, linkB)

	// Docker reports no MAC for an ipvlan endpoint, so the shared MAC is read from the links.
	if linkA == "" || linkB == "" {
		t.Fatalf("could not read the ipvlan links' hardware addresses (a=%q b=%q)", linkA, linkB)
	}
	if linkA != linkB {
		t.Fatalf("the two ipvlan endpoints have different MACs (%s, %s), so this test "+
			"cannot distinguish a per-endpoint identity from a MAC-derived one. An "+
			"ipvlan L2 slave inherits the parent's MAC; if that has changed, this "+
			"test needs rewriting rather than relaxing (#895)", linkA, linkB)
	}
	// Recovery inherits the parent's MAC because Docker reports none for these endpoints.
	if macA != "" || macB != "" {
		t.Logf("Docker now reports MACs for ipvlan endpoints (%q, %q); recoveredMAC's "+
			"ipvlan arm is no longer the path recovery takes here", macA, macB)
	}
	if v6A == v6B {
		t.Fatalf("two ipvlan containers sharing MAC %s were both handed %s. They present "+
			"ONE DHCPv6 identity, claim one binding, and the server hands the same "+
			"address to each of them in turn (#895)", macA, v6A)
	}

	assertDUIDStableAcrossAPluginRestart(t, ctx, cli, v6A)
}

// RFC 9915 section 18.2.10.1 puts duplicate-address detection on the client before use and section 18.2.10 answers a
// duplicate with a Decline; the chassis installs with IFA_F_NODAD because of it, so the kernel would not catch a
// library that stopped (#911).

// TestDHCPv6_ADuplicateOnTheSegmentIsRefused checks that a restarted container whose previous v6 address is taken by another node comes back on a different address.
func TestDHCPv6_ADuplicateOnTheSegmentIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	netName := "dh-itest-v6dup"

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
	id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6dup-ctr")

	v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6 == "" {
		t.Fatal("no global IPv6 appeared on the container link")
	}

	// A macvlan child does not see its parent's traffic, so the duplicate goes on the segment bridge. Without `nodad` the
	// kernel marks it dadfailed and it answers nothing (RFC 4862 section 5.4.3); measured on the lane 2026-09-06 (#911).
	dup := v6 + "/64"
	if out, err := exec.Command("ip", "-6", "addr", "add", dup, "dev", harness.DHCPSegment, "nodad").CombinedOutput(); err != nil {
		t.Fatalf("could not put a duplicate of %s on %s: %v\n%s",
			v6, harness.DHCPSegment, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "-6", "addr", "del", dup, "dev", harness.DHCPSegment).Run()
	})
	// A tentative address does not answer a neighbour solicitation (RFC 4862 section 5.4.3).
	if !awaitAddrSettled(t, harness.DHCPSegment, v6, 15*time.Second) {
		t.Fatalf("the duplicate %s on %s never left the tentative state, so it would "+
			"not have answered the client's probe and this test would measure nothing",
			v6, harness.DHCPSegment)
	}

	declinesBefore := countLogToken(t, fixture.DnsmasqLog(), "DHCPDECLINE")

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}
	after := linkGlobalV6(t, ctx, id, 2*harness.IPAcquisitionBudget)
	if after == "" {
		t.Fatal("the container did not come up with any global IPv6 after the restart. " +
			"Finding a duplicate must cost the endpoint that ADDRESS, not its network")
	}
	if after == v6 {
		t.Errorf("the container took %s again while another node on the segment holds it. "+
			"The client's duplicate-address check (RFC 9915 section 18.2.10.1) did not "+
			"run or did not see the answer -- and the chassis installs this address with "+
			"IFA_F_NODAD, so the kernel will not catch it either", v6)
	}
	n := countLogToken(t, fixture.DnsmasqLog(), "DHCPDECLINE") - declinesBefore
	if n < 1 {
		t.Errorf("the server logged no DHCPDECLINE for the duplicated address (%d new "+
			"lines). The container may have avoided the address by luck rather than by "+
			"declining it, and the server still believes the binding is good "+
			"(RFC 9915 section 18.2.10)", n)
	}
	// Run 34058213252 (2026-09-06) looped Solicit to Decline about once a second for sixteen seconds because the tombstone
	// hint survived the Decline (#213, #911); two declines are legitimate, a dozen are the loop.
	if n > 4 {
		t.Errorf("the duplicated address was declined %d times. A Decline whose retry "+
			"asks for the same address again cannot terminate; the endpoint is spending "+
			"the daemon's whole deadline on it (RFC 9915 sections 18.2.1 and 18.2.10.1)", n)
	}
}

// containerLinkMAC reads the hardware address of the container's own non-loopback link, which Docker does not report for ipvlan.
func containerLinkMAC(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	for _, line := range strings.Split(harness.ExecOutput(t, ctx, id, "ip", "-o", "link", "show"), "\n") {
		if strings.Contains(line, ": lo:") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "link/ether" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	return ""
}

// awaitAddrSettled waits for addr on iface to leave the tentative state, which is when it starts answering neighbor solicitations.
func awaitAddrSettled(t *testing.T, iface, addr string, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ip", "-6", "-o", "addr", "show", "dev", iface).CombinedOutput()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, addr+"/") {
					continue
				}
				if strings.Contains(line, "tentative") || strings.Contains(line, "dadfailed") {
					break
				}
				return true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// assertDUIDStableAcrossAPluginRestart recycles the plugin and requires the server's lease DB to name the same client DUID for addr afterwards.
func assertDUIDStableAcrossAPluginRestart(t *testing.T, ctx context.Context, cli *docker.Client, v6 string) {
	t.Helper()

	var duidBefore string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if duidBefore = leaseDUIDForV6(t, fixture.LeaseFile(), v6); duidBefore != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if duidBefore == "" {
		t.Fatalf("no v6 lease entry for %s in the dnsmasq lease DB", v6)
	}
	repliesBefore := countDHCPv6Replies(t, fixture.DnsmasqLog(), v6)

	// Re-enable is registered before the disable, so a failed assertion cannot leave the plugin off.
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
	t.Log("plugin restarted; awaiting the recovered v6 client's re-bind...")

	// A fresh DHCPREPLY proves the post-restart exchange happened before the lease DB is read.
	deadline = time.Now().Add(90 * time.Second)
	rebound := false
	for time.Now().Before(deadline) {
		if countDHCPv6Replies(t, fixture.DnsmasqLog(), v6) > repliesBefore {
			rebound = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !rebound {
		t.Fatalf("no post-restart DHCPREPLY for %s within 90s — the recovered v6 client never re-bound", v6)
	}

	duidAfter := leaseDUIDForV6(t, fixture.LeaseFile(), v6)
	if duidAfter == "" {
		t.Fatalf("v6 lease entry for %s vanished after plugin restart", v6)
	}
	if !strings.EqualFold(duidBefore, duidAfter) {
		t.Errorf("client DUID changed across plugin restart: %s -> %s — the identity was "+
			"re-minted rather than read back from the endpoint's record, and v6 "+
			"reservations keyed on DUID will not stick", duidBefore, duidAfter)
	}
}

// A hand-written copy would miss a knob added in pkg/dhcp, and RouterAdvertGuardContract returns a fresh map per call
// so no caller can edit another's expectations (#875).

// raGuardKnobs returns the sysctl contract the Router-Advertisement guard must leave in the container.
func raGuardKnobs() map[string]string { return dhcp.RouterAdvertGuardContract() }

// verified is a parameter so the check fails when it runs before the caller's read-back (#875).

// assertRAGuardReportedNoFailure checks that the guard raised no failure on a host where it demonstrably held.
func assertRAGuardReportedNoFailure(t *testing.T, ctx context.Context, verified, wantVerified int) {
	t.Helper()
	if verified != wantVerified {
		t.Fatalf("%d of %d RA-guard knobs were verified from inside the container "+
			"before the health check below. That check reads the plugin's OWN "+
			"counter, which proves intent rather than effect; a zero there means "+
			"\"ran and reported no failure\" only once every knob has been read "+
			"back here. Either a knob assertion above failed, or this block has "+
			"been moved ahead of the loop (#875, #911)", verified, wantVerified)
	}

	// A spurious failure on a healthy host is otherwise unseen in CI, and a zero reads as "ran, no failure" only after the
	// sysctl loop proved the guard ran. The counter is plugin-wide, and a deleted read-back is caught by
	// TestApplyRouterAdvertGuard_ReadsBackWhatItWrote (#875).
	if h := harness.PluginHealthOrNil(ctx); h == nil {
		t.Error("could not read the plugin health surface, so the RA guard's " +
			"false-alarm direction was not measured here. Absent data is not a " +
			"zero and must not be recorded as one (#875)")
	} else if h.RouterAdvertGuardFailures != 0 {
		t.Errorf("router_advert_guard_failures = %d after a golden path whose knobs "+
			"all read correctly. The guard reported a failed step on a host where it "+
			"demonstrably held: either a write was refused, or a knob read back a "+
			"value other than the one written. Note the counter is plugin-wide, so "+
			"another endpoint in this shard is also a candidate (#911)",
			h.RouterAdvertGuardFailures)
	}
}

// The plugin names the container link after the network, not eth0 (#875).

// containerV6Iface returns the name of the interface inside the container that carries addr.
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

// Measured on 1.9.0: the persistent client bound 2 s (bridge) and 5 s (macvlan) after the one-shot (#875).

// persistentV6BindBudget bounds the wait for the persistent v6 client's own DHCPv6 bind.
const persistentV6BindBudget = 45 * time.Second

// The one-shot binds in the host namespace at CreateEndpoint, and the guard runs before the persistent client; on
// 1.9.0 the test read kernel defaults one second before the guard wrote (#875). The server's second DHCPREPLY for the
// address is downstream of the guard and independent of it. The count is scoped by the DUID-LL on the reply line
// (00:03:00:01 + MAC), which holds for bridge and macvlan but not ipvlan's DUID-UUID (#895).

// awaitPersistentV6Bind blocks until the fixture's DHCP server logs this endpoint's second DHCPv6 bind for addr.
func awaitPersistentV6Bind(t *testing.T, logPath, addr, mac string) {
	t.Helper()

	if logPath == "" {
		t.Fatal("awaitPersistentV6Bind: empty dnsmasq log path — the fixture was " +
			"never started, so this assertion would have measured nothing")
	}
	if mac == "" {
		t.Fatal("awaitPersistentV6Bind: could not read the container link's MAC, so " +
			"the bind count cannot be scoped to this endpoint")
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
		"nothing here to assert on. This is a failure, not a reason to skip",
		replies, addr, mac, persistentV6BindBudget)
}

// awaitPersistentV6BindFor waits for the persistent client's bind for addr and returns the container interface that carries it.
func awaitPersistentV6BindFor(t *testing.T, ctx context.Context, id, addr, logPath string) string {
	t.Helper()

	iface := containerV6Iface(t, ctx, id, addr)
	mac := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/"+iface+"/address"))
	awaitPersistentV6Bind(t, logPath, addr, mac)
	return iface
}

// The unit tests prove only that the chassis asked for the flag; installV6Address replaces the flagless address
// libnetwork installed, and linkGlobalV6 reads no flags (#911). Busybox prints IFA_F_NODAD as `flags 02`, which
// harness.V6AddrFlagsFromAddrShow decodes. The Reply precedes the library's DAD, so the poll after the anchor is a
// deadline.

// assertLeasedV6IsInstalledWithNODAD checks that the container's kernel holds the leased address with IFA_F_NODAD, neither tentative nor dadfailed.
func assertLeasedV6IsInstalledWithNODAD(t *testing.T, ctx context.Context, id, addr, logPath string) {
	t.Helper()

	iface := awaitPersistentV6BindFor(t, ctx, id, addr, logPath)

	var last harness.V6AddrFlags
	var out string
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "ip", "-6", "-o", "addr", "show", "dev", iface)
		last = harness.V6AddrFlagsFromAddrShow(out, addr)
		if last.Found && last.NoDAD && !last.Tentative && !last.DADFailed {
			t.Logf("leased v6 %s on %s: nodad set, not tentative, not dadfailed — %q", addr, iface, last.Line)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}

	switch {
	case !last.Found:
		t.Errorf("the leased address %s is not on %s inside the container after %s. "+
			"`ip -6 -o addr show dev %s` said:\n%s",
			addr, iface, harness.IPAcquisitionBudget, iface, out)
	case !last.NoDAD:
		t.Errorf("the leased address %s is installed WITHOUT IFA_F_NODAD after %s. "+
			"The library already ran duplicate-address detection (RFC 9915 section "+
			"18.2.10.1) and the chassis re-applies the engine's address to say so; "+
			"without the flag the kernel repeats RFC 4862 section 5.4 on an address "+
			"that has just passed it, which costs a tentative window and can withdraw "+
			"the address outright (RFC 7527 section 4.1). Line: %q",
			addr, harness.IPAcquisitionBudget, last.Line)
	case last.DADFailed:
		t.Errorf("the leased address %s is DADFAILED: the kernel took an address the "+
			"library had already cleared out of service. Line: %q", addr, last.Line)
	default:
		t.Errorf("the leased address %s is still tentative after %s, so the kernel is "+
			"running duplicate-address detection on it. Line: %q",
			addr, harness.IPAcquisitionBudget, last.Line)
	}
}

// accept_ra=0 and keep_addr_on_down=1 are not kernel defaults. DHCPv6 has no next hop (RFC 9915 section 21) and the
// kernel may not install one, so exactly one default route via fe80::/10 proves Lease.Gateway reached the engine and
// the kernel's early route was purged (#821). Busybox prints no `proto`, so the route is keyed on its via-address. The
// fixture's unsolicited interval is dnsmasq's default, up to 600s, so refresh is not observed.

// assertRouterAdvertsAreBeingProcessed checks the guard's knobs and the single link-local default route in the container.
func assertRouterAdvertsAreBeingProcessed(t *testing.T, ctx context.Context, id, addr, logPath string) {
	t.Helper()

	iface := awaitPersistentV6BindFor(t, ctx, id, addr, logPath)

	t.Logf("RA guard: asserting on derived container interface %q", iface)

	// An empty contract would make this loop pass having measured nothing (#875).
	knobs := raGuardKnobs()
	if len(knobs) == 0 {
		t.Fatal("the RA-guard knob contract is empty, so the loop below would assert " +
			"nothing. It is derived from dhcp.RouterAdvertGuardContract(); an empty " +
			"table means the guard exports no knobs, NOT that the guard is healthy")
	}

	verified := 0
	for knob, want := range knobs {
		p := "/proc/sys/net/ipv6/conf/" + iface + "/" + knob
		got := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", p))
		if harness.SysctlReadFailed(got) {
			t.Errorf("COULD NOT MEASURE %s: %q. The assertion did not run — this is "+
				"not evidence the guard failed, it is evidence the observer is "+
				"pointed at the wrong place", p, got)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q — the Router-Advertisement guard did not hold "+
				"this knob inside the shipped image. Since #821 the plugin reads the "+
				"advertisement itself and puts the gateway into the Join answer, so a "+
				"kernel still acting on the same frames adds a SECOND default route "+
				"beside it and the container's next hop is decided by a metric "+
				"comparison nobody chose (#911, #821)", p, got, want)
			continue
		}
		verified++
	}

	assertRAGuardReportedNoFailure(t, ctx, verified, len(knobs))

	// This poll is sound only after awaitPersistentV6Bind has run.
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var routes string
	for time.Now().Before(deadline) {
		routes = harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show", "default")
		if harness.HasLinkLocalDefaultRoute(routes) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !harness.HasLinkLocalDefaultRoute(routes) {
		t.Errorf("no default route via a link-local address on %s after %s. DHCPv6 carries no "+
			"router (RFC 9915 section 21), and since #821 the container's kernel is at "+
			"accept_ra=0, so the only way one arrives is the plugin's own client reading "+
			"the advertisement and the Join answer carrying it. Its absence means that "+
			"chain is broken. `ip -6 route show default` says:\n%s",
			iface, harness.IPAcquisitionBudget, routes)
		return
	}

	if n := harness.CountDefaultRoutes(routes); n != 1 {
		t.Errorf("%d IPv6 default routes on %s, want exactly 1. Two means the container's "+
			"kernel installed one of its own beside the plugin's -- either accept_ra=0 did "+
			"not take, or the route it had already installed before the guard ran was not "+
			"purged (#821). Which of the two the container uses is a metric comparison "+
			"nobody chose. `ip -6 route show default` says:\n%s", n, iface, routes)
	}
}
