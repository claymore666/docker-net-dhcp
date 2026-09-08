// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// DHCPv6 coverage, restored onto the 2.0 chassis (#911).
//
// These tests were written for 1.9.0 (#103, #213, #875) against a v6
// dhcpcd client, retired unrun when 2.0 landed without one, and are
// brought back here against the in-house library. The PROPERTIES they
// assert are unchanged -- that is the parity claim -- but three of the
// mechanisms underneath them are not, and each one is stated where it
// is used rather than assumed:
//
//   - THE IDENTITY IS STORED, NOT RE-DERIVED. 1.9.0 pinned dhcpcd's
//     DUID-LL by rendering `duid 00:03:00:01:<MAC>` into a generated
//     config on every start, so DUID stability followed from MAC
//     stability. 2.0 mints the identity once at CreateEndpoint and
//     writes it to the endpoint's record (D10), so it survives even
//     where the MAC cannot carry it -- which is what makes the ipvlan
//     case below possible at all. On bridge and macvlan the VALUE is
//     unchanged, deliberately: an endpoint upgraded from 1.x presents
//     the DUID the server already holds a binding for (P-8.6).
//   - DUPLICATE-ADDRESS DETECTION MOVED INTO THE CLIENT. RFC 9915
//     section 18.2.10.1 puts the check on the client, and the library
//     runs it before it reports the lease; the chassis then installs
//     the address with IFA_F_NODAD so the kernel does not run RFC 4862
//     section 5.4 a second time on an address that has just passed
//     (D30 Q1). TestDHCPv6_ADuplicateOnTheSegmentIsRefused is the
//     outside evidence that the first half of that actually happens.
//   - THE ROUTER-ADVERTISEMENT GUARD IS A WRITE, NOT A SHIELD. 1.9.0
//     had to fight dhcpcd, which cleared accept_ra and autoconf on
//     every carrier acquisition, so the guard wrote the knobs and then
//     remounted /proc/sys read-only to keep them. Nothing in 2.0
//     rewrites them, so the guard writes and reads back and that is
//     all; there is no shield and there are no `-writable` markers.
//     The obligation it discharges is the same one and it is not
//     optional: DHCPv6 carries no next hop (RFC 9915 section 21) and
//     RFC 5942 section 4 rule 1 forbids inferring an on-link prefix
//     from the assigned address, so an endpoint whose kernel is not
//     processing advertisements has an address and no route.
//
// What 1.9.0 could not observe and 2.0 can: the container's own kernel
// is free to solicit. dhcpcd set addr_gen_mode to NONE on the link,
// which left the container unable to ask for a fresh advertisement, so
// 1.9.0 could only ever witness the FIRST one. The 2.0 chassis touches
// addr_gen_mode nowhere.
package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
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
// given dnsmasq log -- the v6 sibling of the DHCPACK counting in the
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

// countLogToken counts lines of the dnsmasq log carrying every needle.
//
// Unlike countDHCPv6Replies it is not restricted to DHCPREPLY, because
// the tokens it is used for -- DHCPDECLINE among them -- are their own
// message types.
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

// TestIPv6_AcceptedAtCreate is the inversion of the refusal this
// milestone removed.
//
// 2.0 shipped without a DHCPv6 client and refused `ipv6=true` at
// CreateNetwork so that an operator asking for IPv6 was TOLD rather
// than handed a network that quietly did nothing with it. That refusal
// is gone, and its test is inverted rather than deleted: the create has
// to be ACCEPTED and the network has to exist afterwards.
//
// It is a create-only test on purpose. Everything about addresses is
// asserted by the golden paths below; this one is the cheapest possible
// statement that the option reaches the driver at all, and it is the one
// that fails first and most legibly if the refusal is ever reinstated by
// accident.
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

	// A create that "succeeded" and left nothing behind is the other
	// half of the same claim, and it is the half a refusal returning
	// nil would satisfy.
	if _, err := cli.NetworkInspect(ctx, netName, network.InspectOptions{}); err != nil {
		t.Errorf("the create was accepted and the network does not exist: %v", err)
	}
}

// TestIPv6_TheV4OnlyPathIsUnchanged is the preservation control for
// everything in this file.
//
// Wiring a second address family into the chassis is a change to the
// code path every IPv4 network takes as well: one family switch, one
// shared record store, one manager. A v4-only network created and used
// exactly as before is the cheapest statement that none of that moved.
func TestIPv6_TheV4OnlyPathIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dhcptest-ipv6-control"
	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, v4, _ := harness.RunContainer(t, ctx, netName, "dhcptest-ipv6-control-ctr")
	if !harness.IsInPool(net.ParseIP(v4)) {
		t.Errorf("IPv4 %s not in fixture pool", v4)
	}
	// And no IPv6 appeared on a network that did not ask for one. The
	// family switch defaulting the wrong way is silent otherwise: the
	// container works, and a second DHCP client is running against a
	// segment nobody asked it to touch.
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

// TestLifecycleMacvlan_IPv6_GoldenPath: with ipv6=true, a container
// gets a v4 lease from the v4 pool AND a v6 lease from the ULA pool;
// docker inspect's GlobalIPv6Address agrees with the address actually
// on the link; teardown stops both families cleanly
// (client_stop_failures stays flat -- this exercises the v6 half of
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
	// health assertion. Neither client releases -- D-7, #800 -- so what
	// is being sequenced is the stop, not a release.
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
	// AddressIPv6 from the one-shot acquisition; the persistent client
	// re-binds with the SAME identity -- the DUID and IAID stored on
	// the endpoint's record, not re-derived -- so the server must hand
	// back the same address. A mismatch here is the v6 flavour of the
	// #104 divergence: if it fires, the audit found a real edge, so
	// document it and re-scope rather than loosening silently.
	insV6 := inspectV6(t, ctx, cli, id, netName)
	if insV6 == "" {
		t.Error("docker inspect has empty GlobalIPv6Address for an ipv6=true network")
	} else if !net.ParseIP(insV6).Equal(net.ParseIP(liveV6)) {
		t.Errorf("inspect IPv6 %s != live link IPv6 %s", insV6, liveV6)
	}

	assertLeasedV6IsInstalledWithNODAD(t, ctx, id, liveV6, fixture.DnsmasqLog())
	assertRouterAdvertsAreBeingProcessed(t, ctx, id, liveV6, fixture.DnsmasqLog())

	// Teardown: both families stop cleanly.
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	before, after := w.End()
	if after.ClientStopFailures != before.ClientStopFailures {
		t.Errorf("client_stop_failures moved %d -> %d over a dual-stack lifecycle; the v6 Stop path is failing",
			before.ClientStopFailures, after.ClientStopFailures)
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

	assertLeasedV6IsInstalledWithNODAD(t, ctx, id, liveV6, fixture.BridgeDnsmasqLogPath())
	assertRouterAdvertsAreBeingProcessed(t, ctx, id, liveV6, fixture.BridgeDnsmasqLogPath())
}

// TestTombstoneRestart_PreservesIPv6 is #213's acceptance test -- the
// v6 sibling of TestTombstoneRestart_PreservesMACAndIP. On Leave the
// plugin tombstones the endpoint's v6 address; on the restart's
// CreateEndpoint that address goes back out as the DHCPv6 hint (the
// IA_ADDR of the Solicit, proto.Params6.Hint), so a dual-stack
// container keeps its v6 lease across `docker restart` exactly as it
// keeps v4.
//
// The identity is the other half of why it sticks, and in 2.0 that half
// is stronger than 1.9.0's: the DUID and IAID come off the endpoint's
// record rather than being re-derived, so the Solicit is the same
// client's whatever the plumbing looks like on the second start.
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
	// A normal, promptly-stopping container -- see the note in
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
		t.Errorf("IPv6 changed across restart: before=%s after=%s (the tombstoned v6 address was not sent as the Solicit's hint, or the identity moved)", v6Before, v6After)
	}
	if v4After != v4Before {
		t.Errorf("IPv4 changed across restart: before=%s after=%s", v4Before, v4After)
	}
	if insV6 != "" && !net.ParseIP(insV6).Equal(net.ParseIP(v6After)) {
		t.Errorf("inspect IPv6 %s != live link IPv6 %s after restart", insV6, v6After)
	}
}

// TestLeaseRenewIPv6_HonorsT1: the v6 sibling of
// TestLeaseRenew_HonorsT1 -- the direct test for "DHCPv6 renewal is
// less battle-tested" (#103).
//
// WHAT IT PROVES, and it is two things, both on evidence from outside
// the plugin: a renewal DHCPREPLY for this address reaches the SERVER's
// own log after T1, and the address the container holds is the same one
// on the far side of it. A counter would prove the plugin meant to
// renew; the server's log is what proves the renewal happened.
//
// THE WAIT IS THE SERVER'S T1 AND IT IS NOT SHORTENED (D41). dnsmasq
// derives DHCPv6 T1 as lease/2 = 60s from the fixture's 2m lease and
// offers no way to advertise it independently -- the v4 sibling's
// WithRenewTimes trick has no DHCPv6 counterpart in this server, and
// shortening the LEASE to move T1 is the one remedy this work is not
// allowed to take. So the 60s stands.
//
// WHAT THE FLAT SLEEP DID NOT PROVE, and this is a finding rather than
// a tidy-up. The old shape sampled the reply count immediately after
// the bind, slept a flat 75s, sampled again and required growth. First
// attempt at replacing that sleep with a poll returned in THREE
// seconds, green: MEASURED on run 34203647801, job 101988277652 --
// "DHCPREPLYs for fd00:...::92: start=1 end=2" with 1m12s of the
// ceiling unused. A second DHCPREPLY for the address lands within
// seconds of the bind, so the old assertion was satisfied by that reply
// and not by the renewal. The 75s wait was buying nothing; a 5s wait
// would have passed it just as reliably. The test claimed T1 and
// measured the bind.
//
// SO THE BASELINE MOVED TO THE BOUNDARY. The count is now sampled again
// at t1Floor, a slop below T1, and the growth that satisfies the test
// has to appear AFTER that sample -- in the window where T1 sits. The
// bind's own burst is inside the baseline by construction, and the test
// cannot pass without having waited t1Floor: there is no arrangement of
// bind-time replies that gets it to green early.
//
// That makes it strictly stronger than the version it replaces, and
// faster: MEASURED 90.03s before (median of runs 34059724566 /
// 34060966627 / 34064155841) against ~62s now, because what went is the
// 15s of idling AFTER the renewal was already in the log.
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
	// poisoning (#103) -- DHCPv6 failures in this environment tend to
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

	// The three constants, and each is derived rather than chosen.
	//
	//   t1 is dnsmasq's, not ours: it advertises lease/2 for DHCPv6 and
	//   the fixture's lease is 2m. There is no option to advertise it
	//   independently -- the v4 sibling's WithRenewTimes has no v6
	//   counterpart in this server -- and moving it would mean
	//   shortening the lease, which is the one thing this is not
	//   allowed to do.
	//
	//   t1Slop is how far BEFORE t1 the baseline is taken. It exists so
	//   a renewal that lands exactly on t1 is not swallowed by the
	//   sample meant to exclude the bind.
	//
	//   ceiling is unchanged from the flat sleep: t1 plus enough for a
	//   loaded runner's scheduling and dnsmasq's own write of the line.
	const (
		t1             = 60 * time.Second
		t1Slop         = 5 * time.Second
		renewalCeiling = 75 * time.Second
	)
	bound := time.Now()

	// The bind's own replies, and whatever else the exchange produces in
	// the seconds after it, all land in the baseline -- that is the
	// point of taking it here and not at `bound`.
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled before the renewal window opened: %v", ctx.Err())
	case <-time.After(t1 - t1Slop):
	}
	baseline := countDHCPv6Replies(t, fixture.DnsmasqLog(), v6)
	atBind := startReplies
	t.Logf("DHCPREPLYs for %s: %d at the bind, %d at %s — watching for one more until %s",
		v6, atBind, baseline, t1-t1Slop, renewalCeiling)

	deadline := bound.Add(renewalCeiling)
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
	t.Logf("DHCPREPLYs for %s: baseline=%d end=%d at %s after the bind",
		v6, baseline, endReplies, time.Since(bound).Round(time.Second))

	// Read the address AFTER the reply is in hand, so the comparison is
	// across the renewal rather than across an interval that happens to
	// contain one.
	after := linkGlobalV6(t, ctx, id, 5*time.Second)
	if after != v6 {
		t.Errorf("IPv6 changed across renewal window: %s -> %s", v6, after)
	}
	if endReplies <= baseline {
		t.Errorf("no DHCPREPLY for %s in the %s..%s window after the bind — T1 is %s, "+
			"so the v6 renewal timer never fired (the server logged %d reply/replies before "+
			"the window opened, and none inside it)",
			v6, t1-t1Slop, renewalCeiling, t1, baseline)
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

// TestDUID_PersistsAcrossPluginRestart is #103's "persistent DUID"
// item, and in 2.0 it is a test of the RECORD rather than of a
// derivation.
//
// 1.9.0 pinned dhcpcd's DUID-LL from the interface MAC on every start,
// so DUID stability followed from MAC stability. 2.0 mints the identity
// once at CreateEndpoint and stores it on the endpoint's record (D10),
// so what this now proves is that the record survives a plugin recycle
// and is read back rather than re-minted. The observable is unchanged
// and deliberately so: the dnsmasq lease DB must show the SAME client
// DUID for the container's address after the plugin restarts and the
// recovered client re-binds. That is what makes server-side v6
// reservations stick across plugin upgrades.
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
	assertDUIDStableAcrossAPluginRestart(t, ctx, cli, v6)
}

// TestIPvlan_DHCPv6IdentityIsPerEndpointAndSurvivesARestart is the
// proof 1.9.0 could not write.
//
// An ipvlan L2 slave inherits the parent link's MAC by kernel design,
// so every endpoint on one ipvlan network has the same hardware
// address. 1.9.0 derived the DUID from that MAC, which means every
// container on an ipvlan network presented ONE DHCPv6 identity: they
// claimed one binding and the server handed the same address out
// repeatedly (#895, the v6 form of #219). 2.0 mints a per-endpoint
// DUID-UUID there instead (D30 Q4).
//
// Two claims, and neither implies the other:
//
//  1. Two containers on one ipvlan network get DIFFERENT addresses.
//     That is the defect, observed from outside.
//  2. An ipvlan endpoint's DUID survives a plugin restart. This is
//     where the RECORD is load-bearing and the 1.9.0 mechanism could
//     not have worked at all: there is nothing on the link to re-derive
//     a per-endpoint identity from, so if the record is not read back
//     the container becomes a new client and loses its address.
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

	// The premise, READ FROM THE LINK. Docker reports no MAC at all for
	// an ipvlan endpoint, so comparing what it reports would compare
	// two empty strings and pass whatever the kernel had done. What the
	// claim below needs is that the two links really do wear the same
	// address; if they do not, this test cannot distinguish a
	// per-endpoint identity from a MAC-derived one and would be
	// satisfied by the 1.9.0 mechanism.
	if linkA == "" || linkB == "" {
		t.Fatalf("could not read the ipvlan links' hardware addresses (a=%q b=%q)", linkA, linkB)
	}
	if linkA != linkB {
		t.Fatalf("the two ipvlan endpoints have different MACs (%s, %s), so this test "+
			"cannot distinguish a per-endpoint identity from a MAC-derived one. An "+
			"ipvlan L2 slave inherits the parent's MAC; if that has changed, this "+
			"test needs rewriting rather than relaxing (#895)", linkA, linkB)
	}
	// The second premise, and the one plugin-restart recovery rests on:
	// Docker reports NO MAC for these endpoints, which is why recovery
	// has to inherit the parent's rather than parse what it is given.
	if macA != "" || macB != "" {
		t.Logf("Docker now reports MACs for ipvlan endpoints (%q, %q); recoveredMAC's "+
			"ipvlan arm is no longer the path recovery takes here", macA, macB)
	}
	if v6A == v6B {
		t.Fatalf("two ipvlan containers sharing MAC %s were both handed %s. They present "+
			"ONE DHCPv6 identity, claim one binding, and the server hands the same "+
			"address to each of them in turn (#895)", macA, v6A)
	}

	// Claim 2: the identity is in the record, not on the link.
	assertDUIDStableAcrossAPluginRestart(t, ctx, cli, v6A)
}

// TestDHCPv6_ADuplicateOnTheSegmentIsRefused is the outside evidence
// for the first half of D30 Q1: the CLIENT runs duplicate-address
// detection, and it runs it before it reports the lease.
//
// RFC 9915 section 18.2.10.1: "The client performs duplicate address
// detection on each of the received addresses in any IAs it accepts
// before using that address for traffic". Section 18.2.10 then says
// what to do when it finds one -- the client sends a Decline and asks
// again. The chassis installs the address with IFA_F_NODAD precisely
// BECAUSE the library has already done this, so if the library ever
// stopped, the plugin would be installing an unchecked address with
// the kernel's own check switched off. Nothing else in the suite would
// notice: the container comes up, the address works, and it works until
// the other holder sends something.
//
// THE SHAPE. A container takes an address; that exact address is then
// put on the segment by another node; the container is restarted, so
// the tombstone asks for it back and the server -- which still holds
// the lease -- offers it. The library must now find the duplicate and
// refuse it, and the container must come up with a DIFFERENT address.
//
// A container that comes back on the SAME address is the failure this
// test is for, and it is not a flake: it means the check did not run.
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

	// The duplicate. It goes on the SEGMENT bridge rather than on the
	// container's parent veth: the parent is what the macvlan children
	// hang off, and a macvlan child does not see its own parent's
	// traffic, so an address there would be invisible to exactly the
	// node under test.
	//
	// `nodad` IS LOAD-BEARING AND IS NOT A SHORTCUT. The address being
	// added is, by construction, one the segment already has on it, so
	// the kernel's own duplicate-address detection on THIS side finds
	// the container and marks the address dadfailed -- an address in
	// that state answers nothing (RFC 4862 section 5.4.3), and the
	// squatter this test needs would sit there silent. MEASURED on the
	// lane 2026-09-06: without it the address never left the tentative
	// state and the test could not begin. RFC 4429 section 3.3 is the
	// same permission spelled for optimistic addresses: a node MAY use
	// an address it has reason to believe is unique, and here the test
	// has the opposite reason and wants the address anyway, because
	// being the duplicate is its whole job.
	dup := v6 + "/64"
	if out, err := exec.Command("ip", "-6", "addr", "add", dup, "dev", harness.DHCPSegment, "nodad").CombinedOutput(); err != nil {
		t.Fatalf("could not put a duplicate of %s on %s: %v\n%s",
			v6, harness.DHCPSegment, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "-6", "addr", "del", dup, "dev", harness.DHCPSegment).Run()
	})
	// The duplicate has to be answering before the restart, or the
	// probe finds nothing and this test measures the ordinary path.
	// A tentative address does not answer a neighbor solicitation
	// (RFC 4862 section 5.4.3), so wait for it to leave that state --
	// which `nodad` above should make immediate. This stays as the
	// OBSERVER of that: if the flag is ever dropped, or a kernel stops
	// honouring it, the failure below is the reason rather than a
	// mysterious pass on the ordinary path.
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
	// THE UPPER BOUND IS THE OTHER HALF OF THE SAME CLAIM, and it is
	// what made this test pass by accident before. Declining an address
	// and then asking for it again is a closed loop: MEASURED on the
	// lane 2026-09-06 (run 34058213252) the exchange ran Solicit ->
	// Advertise -> Request -> Reply -> DAD -> Decline about once a
	// second for sixteen seconds, because the preferred address is
	// hinted from the tombstone (#213) and a Decline does not clear the
	// hint. Two declines are legitimate -- the server may hand the same
	// address back to the hintless second attempt by chance, and the
	// library's own recovery covers that -- and a dozen are the loop.
	if n > 4 {
		t.Errorf("the duplicated address was declined %d times. A Decline whose retry "+
			"asks for the same address again cannot terminate; the endpoint is spending "+
			"the daemon's whole deadline on it (RFC 9915 sections 18.2.1 and 18.2.10.1)", n)
	}
}

// containerLinkMAC reads the hardware address the container's own
// non-loopback link wears, from inside the container.
//
// FROM THE LINK AND NOT FROM DOCKER, because for an ipvlan endpoint
// Docker reports no MAC at all: the plugin never sets one (the driver
// rejects it) and the inherited address is not in the engine's record.
// A premise checked against what Docker reports would be comparing two
// empty strings.
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

// awaitAddrSettled waits for addr on iface to leave the tentative
// state, which is when it starts answering neighbor solicitations.
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

// assertDUIDStableAcrossAPluginRestart recycles the plugin and requires
// the server's lease DB to name the same client DUID for addr
// afterwards.
//
// A restart is used rather than a fresh endpoint because that is where
// the identity can quietly move: nothing in the container changes, the
// address is still on the link, and the only thing that decides whether
// the recovered client is the SAME DHCPv6 client is whether the record
// was read back.
func assertDUIDStableAcrossAPluginRestart(t *testing.T, ctx context.Context, cli *docker.Client, v6 string) {
	t.Helper()

	// dnsmasq records the lease (with the client DUID) once the
	// persistent client's request is replied to; poll for the entry.
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

	// Plugin restart: the same belt-and-braces shape as the recovery
	// tests -- re-enable is registered as cleanup BEFORE the disable so
	// a failed assertion cannot leave the runner's plugin off.
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

	// A fresh DHCPREPLY for the address proves the post-restart
	// exchange happened, so the lease DB entry read below is
	// post-restart truth rather than a stale leftover.
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

// raGuardKnobs is the sysctl contract the Router-Advertisement guard
// asserts inside the container, and the value each knob must hold.
//
// DERIVED from the guard itself, never a second copy of its table.
//
// The rejected design is a hand-written map here, and its failure mode
// is asymmetric in the direction that matters: value drift between the
// two enumerations goes red, so the copy LOOKS safe, while a knob ADDED
// in pkg/dhcp is silently unobserved -- the assertion iterates the copy,
// the new knob is simply not among the things checked, and the suite
// stays green over a guard it no longer covers. A count check is the
// same defect one step along, because the count comes from the copy too.
//
// A FUNCTION rather than a package-level var, and that is not style.
// RouterAdvertGuardContract returns a fresh map per call precisely so an
// observer's expectations cannot be rewritten by anything it observes; a
// package-level cache would hand that property straight back, since any
// test in this package could mutate it and every later caller would read
// the edit.
func raGuardKnobs() map[string]string { return dhcp.RouterAdvertGuardContract() }

// assertRAGuardReportedNoFailure checks that the guard raised no
// failure on a host where it demonstrably held.
//
// verified/wantVerified are the knobs the CALLER read back from inside
// the container. They are parameters rather than an adjacent guard on
// purpose: this assertion is only meaningful AFTER that read-back, and a
// dependency expressed by adjacency is one a reorder carries with it.
// The health assertion below is sound ONLY because it runs after that
// loop. Making the dependency executable means a reorder leaves
// verified at 0 and fails here instead of silently asserting nothing.
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

	// The counter's HEALTHY path, observed on the CI engine.
	//
	// WHAT THIS IS, stated because it is not what it looks like: an
	// assertion about the OBSERVER, not about the effect. The loop above
	// is the outside evidence -- it reads the container's real sysctls --
	// and this project's standing rule is to assert on that rather than on
	// the plugin's own counters. It is NOT taken here as a substitute for
	// that loop.
	//
	// It earns its place only because it runs AFTER the loop. The loop has
	// already proven the guard ran and the knobs hold, so a zero here
	// reads as "ran, and reported no failure" rather than "never ran" --
	// which is exactly what a zero would mean on its own.
	//
	// WHY IT IS WORTH ADDING: the false-alarm direction is otherwise
	// unobservable in CI. A spurious failure on a healthy host would go
	// unseen, because the plugin log is only dumped on a failure path --
	// so the ABSENCE of a complaint from a passing run's logs is not
	// evidence and must not be read as one.
	//
	// WHAT IT CANNOT SEE. Which step failed. Anything endpoint-scoped,
	// the counter being plugin-wide -- a failure raised by any other
	// client in this shard lands here too. And the read-back being
	// deleted from ApplyRouterAdvertGuard: the knobs would still hold,
	// the loop above would still pass, the counter would still be zero,
	// and this assertion would still pass having observed nothing about
	// it. That case is closed in the other lane, by
	// TestApplyRouterAdvertGuard_ReadsBackWhatItWrote.
	//
	// What the loop above DOES rule out is the guard as a whole never
	// executing: accept_ra=2 and keep_addr_on_down=1 are non-default and
	// nothing but the guard writes them. That is a narrower claim than
	// "every step ran", and the difference is the point.
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

// persistentV6BindBudget bounds the wait for the persistent v6
// client's own DHCPv6 bind. MEASURED on 1.9.0 in the CI run that
// exposed the ordering bug below: the gap between the one-shot's bind
// and the persistent client's was 2 s (bridge) and 5 s (macvlan), so
// this is roughly an order of magnitude of headroom for a loaded
// runner. It is a deadline, not a settling time -- expiry fails the
// test.
const persistentV6BindBudget = 45 * time.Second

// awaitPersistentV6Bind blocks until the fixture's DHCP server has
// recorded a SECOND DHCPv6 bind for addr, which is the precondition
// every RA-guard assertion below depends on.
//
// Why a precondition is needed at all. There are TWO v6 clients per
// endpoint. The one-shot runs at CreateEndpoint, in the HOST namespace,
// and it is the one whose lease Docker is told about -- so a container
// has its global v6 address, and `docker inspect` agrees, well before
// the PERSISTENT client has started inside the container namespace. The
// RA guard runs on the way to that persistent client. An assertion
// gated only on "the address is there" is therefore free to run before
// the guard has written anything.
//
// It did, on 1.9.0. MEASURED, macvlan shard, one-second log resolution:
//
//	13:57:31  one-shot binds the address (host ns, link pre-rename)
//	13:57:34.180  test reads eth0/accept_ra          -> 1
//	13:57:34.456  test reads eth0/keep_addr_on_down  -> 0
//	13:57:35  the guard runs on eth0, then the client solicits
//
// Every value read was a kernel default. The test read the right file,
// in the right namespace, one second before anything wrote to it.
// `autoconf` could never have caught this, its default and its target
// both being 1.
//
// Why THIS anchor. It is outside evidence -- the DHCP server's own
// record, not the plugin's opinion of itself. It is strictly downstream
// of the guard: the guard runs while the link is being prepared, before
// the persistent client exists, so a bind logged by the server proves
// the guard ran. And it is FIX-INDEPENDENT -- the persistent client
// binds whether or not the guard held, so the precondition cannot
// quietly become a restatement of the thing under test.
//
// Why the second bind and not the first: MEASURED on 1.9.0 -- the
// one-shot contributes exactly one DHCPREPLY per address. dnsmasq logs
// one DHCPREPLY per blessed request or renewal, so the persistent
// client's own bind is the second.
//
// mac scopes the count to THIS endpoint. The fixture log is shared
// across every test in a shard, so counting replies by address alone
// would also count a reply left by an EARLIER container that happened
// to be handed the same pooled address, firing the anchor early and
// restoring the race this function exists to close. dnsmasq puts the
// client's DUID on the reply line, and on bridge and macvlan the plugin
// mints that DUID as a DUID-LL over the container's MAC, so the MAC is
// an exact per-endpoint discriminator:
//
//	DHCPREPLY(dh-itest-br2) fd00:6470:6864::32 00:03:00:01:ea:eb:ed:a4:b0:f5
//
// i.e. 00:03 (link-layer) + 00:01 (Ethernet) + the six MAC bytes.
//
// THE BOUND THAT CAME WITH 2.0: this scoping holds for bridge and
// macvlan and NOT for ipvlan, whose DUID is a per-endpoint DUID-UUID
// with no MAC in it (D30 Q4, #895). No caller here is an ipvlan
// endpoint; one added later would silently match nothing and time out.
func awaitPersistentV6Bind(t *testing.T, logPath, addr, mac string) {
	t.Helper()

	if logPath == "" {
		t.Fatal("awaitPersistentV6Bind: empty dnsmasq log path — the fixture was " +
			"never started, so this assertion would have measured nothing")
	}
	// An unreadable MAC must not silently degrade to an address-only
	// match. That is the same "assertion that cannot read its subject
	// quietly passes" pattern this whole block is about.
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

// awaitPersistentV6BindFor is the anchor above, taken for the endpoint
// carrying addr, and it returns the container interface that carries
// it.
//
// It exists so the two observers that depend on the PERSISTENT client
// having bound -- the Router-Advertisement guard's knobs and the
// installed address's flags -- take the precondition by calling for it
// rather than by sitting after something else that took it. Adjacency
// is not a dependency; a reorder carries a neighbouring guard along to
// where it is vacuous.
func awaitPersistentV6BindFor(t *testing.T, ctx context.Context, id, addr, logPath string) string {
	t.Helper()

	iface := containerV6Iface(t, ctx, id, addr)
	mac := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/"+iface+"/address"))
	awaitPersistentV6Bind(t, logPath, addr, mac)
	return iface
}

// assertLeasedV6IsInstalledWithNODAD is the OUTSIDE evidence for D30
// Q1: the leased address, as the CONTAINER'S OWN KERNEL holds it,
// carries IFA_F_NODAD and is neither tentative nor dadfailed.
//
// # WHY THE UNIT PROOFS ARE NOT ENOUGH
//
// v6AddrAttrs is a pure function and its unit tests say only that the
// chassis ASKED for the flag. What is between the ask and the kernel is
// installV6Address's AddrReplace over an address libnetwork already put
// on the link, from the value CreateEndpoint returned, with no flags
// and no lifetimes. If that re-apply does not take -- a failed replace,
// the wrong link, a mode the call never reaches -- the container is
// left holding the RIGHT ADDRESS with kernel duplicate-address
// detection armed and no lifetimes, and every other proof in this file
// still passes: linkGlobalV6 returns the first global v6 address it
// finds and reads no flags at all. That is a silent defect with no
// observer, which is what this closes (#911 review round 1, finding 1).
//
// It is the flag that is asserted and not the timing. A proof that
// reads the address right after the bind and requires it to be usable
// sees a settled address on a fast box and a tentative one on a loaded
// runner; IFA_F_NODAD is a property of how it was installed and holds
// whatever the runner is doing.
//
// The precondition is taken by calling for it: the re-apply is what the
// PERSISTENT client's first Acquired does, and libnetwork's flagless
// address is on the link well before that client exists. Read before
// the anchor, this would assert on the engine's install and fail for a
// correct plugin.
//
// The poll after the anchor is a deadline, not a settling time. The
// server's Reply comes before the library's own duplicate-address check
// and therefore before Acquired, so the anchor returns a moment early;
// expiry here fails the test.
//
// The renderings this reads are harness.V6AddrFlagsFromAddrShow's
// problem, and the reason it is a pure function driven in the fast lane
// against captured output from the shipped image: alpine's busybox has
// no name for IFA_F_NODAD and prints `flags 02`.
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

// assertRouterAdvertsAreBeingProcessed is the OUTSIDE observer for the
// Router-Advertisement guard. Everything else about it is visible only
// to the plugin: it runs inside the container's namespace, its failures
// land in a health counter, and a counter reading zero is equally
// consistent with "the guard held" and "the guard never ran".
//
// So this asserts on the container's own kernel state, in the image
// that actually ships, through the managed plugin -- not on anything
// the plugin says about itself.
//
// Two independent claims, because each one alone can pass while the
// wiring is broken:
//
//  1. The knobs read the values the guard writes. accept_ra=2 and
//     keep_addr_on_down=1 are not kernel defaults and nothing else
//     writes them, so reading them back is evidence the guard ran.
//  2. A default route via a LINK-LOCAL address is present. DHCPv6
//     carries no router -- the option catalogue is RFC 9915 section 21
//     and nothing in it has a next hop -- and this plugin sets no IPv6
//     gateway of its own, so a default route via fe80::/10 can only
//     have been learned from a Router Advertisement. The knobs being
//     right is the plugin's doing; this is the KERNEL's, and it can
//     only happen if an advertisement was actually accepted off the
//     wire.
//
// Claim 2 was first written as a match on `proto ra`, which is a string
// the container's `ip` PROVABLY NEVER PRINTS: the test image's busybox
// route output carries no `proto` field at all. Keying on the
// via-address instead makes the assertion a property of the protocol
// rather than of one tool's formatting.
//
// The bound: this does not observe REFRESH. The fixture's dnsmasq runs
// with --enable-ra and no --ra-param, so its unsolicited interval is
// dnsmasq's default (up to 600s), far outside any budget here.
func assertRouterAdvertsAreBeingProcessed(t *testing.T, ctx context.Context, id, addr, logPath string) {
	t.Helper()

	// Establish the precondition BEFORE reading any knob -- see the
	// measured ordering above. Everything below is a statement about
	// the persistent client, and until this returns there is no
	// persistent client to make a statement about.
	iface := awaitPersistentV6BindFor(t, ctx, id, addr, logPath)

	t.Logf("RA guard: asserting on derived container interface %q", iface)

	// NON-VACUITY, kept BESIDE the obligation rather than only in the
	// other lane. raGuardKnobs is derived from
	// dhcp.RouterAdvertGuardContract(), so an empty table would make
	// this loop -- and therefore this entire assertion -- pass having
	// measured nothing.
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
		// A read that FAILED is a different verdict from a value that is
		// wrong, and it must never be reported as either a pass or a
		// mere mismatch: it means this assertion measured nothing.
		if harness.SysctlReadFailed(got) {
			t.Errorf("COULD NOT MEASURE %s: %q. The assertion did not run — this is "+
				"not evidence the guard failed, it is evidence the observer is "+
				"pointed at the wrong place", p, got)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q — the Router-Advertisement guard did not hold "+
				"this knob inside the shipped image. An endpoint whose kernel is not "+
				"processing advertisements has an address and no route: DHCPv6 carries "+
				"no next hop (RFC 9915 section 21) and RFC 5942 section 4 forbids "+
				"inferring one from the address (#911)", p, got, want)
			continue
		}
		verified++
	}

	// The counter check is a CALL that takes what the loop proved, not a
	// statement sitting next to it. Adjacency is not a dependency: a
	// reorder moves a neighbouring guard along with the thing it guards,
	// and the guard then travels to where it is vacuous. Passing
	// `verified` makes the ordering a DATA dependency instead -- moved
	// above the loop this call passes 0 and fails; moved above the
	// declaration it does not compile.
	assertRAGuardReportedNoFailure(t, ctx, verified, len(knobs))

	// The RA itself is asynchronous: the container solicits at link-up
	// and dnsmasq answers. Poll rather than sample once.
	//
	// This poll returns on the first success, which is only sound
	// because awaitPersistentV6Bind has already run. Do not hoist it
	// above the anchor to "save time" -- the budget is a deadline for an
	// RA that may be slow, not a window in which the defect might show
	// up.
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
		"router (RFC 9915 section 21) and the plugin sets no IPv6 gateway, so the absence "+
		"of one means the kernel never accepted a Router Advertisement. "+
		"`ip -6 route show default` says:\n%s", iface, harness.IPAcquisitionBudget, routes)
}
