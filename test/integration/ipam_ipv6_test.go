// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Every assertion here reads the server's lease file, its log or docker inspect, never the plugin's counters (#960).

// ipam6Network creates a macvlan network on the shared fixture with this plugin as the IPAM driver and IPv6 on.
func ipam6Network(t *testing.T, ctx context.Context, name string, opts map[string]string) {
	t.Helper()
	all := map[string]string{"ipv6": "true"}
	for k, v := range opts {
		all[k] = v
	}
	harness.CreateNetworkIPAM(t, ctx, name, "macvlan", harness.SubnetCIDR, nil, all)
}

// ipam6Lease returns the IAID and client DUID of the lease file's v6 line holding addr, or two empty strings.
func ipam6Lease(t *testing.T, leaseFile, addr string) (iaid, duid string) {
	t.Helper()
	if !waitLeaseFile(t, leaseFile, addr, true) {
		return "", ""
	}
	data, err := os.ReadFile(leaseFile)
	if err != nil {
		t.Fatalf("read lease file %s: %v", leaseFile, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 5 && strings.EqualFold(f[2], addr) {
			return f[1], f[len(f)-1]
		}
	}
	return "", ""
}

// ipam6IdentityOf is the DUID-LL and decimal IAID resolveIdentity6 mints from mac, as dnsmasq prints them.
func ipam6IdentityOf(t *testing.T, mac string) (iaid, duid string) {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("parse MAC %q: %v", mac, err)
	}
	id, err := dhcp.IAIDFromMAC(hw)
	if err != nil {
		t.Fatalf("IAID from %s: %v", mac, err)
	}
	return strconv.FormatUint(uint64(id), 10), "00:03:00:01:" + strings.ToLower(hw.String())
}

// ipam6Addr waits for the container's global v6 address and returns it once docker inspect shows the same one.
func ipam6Addr(t *testing.T, ctx context.Context, cli *docker.Client, id, netName string) string {
	t.Helper()
	live := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	shown := inspectV6(t, ctx, cli, id, netName)
	if live == "" || !net.ParseIP(shown).Equal(net.ParseIP(live)) {
		t.Fatalf("docker inspect shows IPv6 %q and the container's link carries %q", shown, live)
	}
	return shown
}

// TestIPAMv6_DockerShowsTheAddressTheServersReplyCarried checks that GlobalIPv6Address is the address the server replied with and leased to the MAC-derived identity.
func TestIPAMv6_DockerShowsTheAddressTheServersReplyCarried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam6-reply"
	cli := ipamDockerClient(t)
	ipam6Network(t, ctx, netName, nil)
	id, _, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")

	addr := ipam6Addr(t, ctx, cli, id, netName)
	if !harness.IsInPoolV6(net.ParseIP(addr)) {
		t.Errorf("docker inspect shows %s, outside the fixture's DHCPv6 pool", addr)
	}
	if n := countDHCPv6Replies(t, fixture.DnsmasqLog(), addr); n < 1 {
		t.Errorf("the server logged no DHCPREPLY carrying %s, the address Docker shows", addr)
	}
	iaid, duid := ipam6Lease(t, fixture.LeaseFile(), addr)
	wantIAID, wantDUID := ipam6IdentityOf(t, mac)
	if !strings.EqualFold(duid, wantDUID) || iaid != wantIAID {
		t.Errorf("the server leased %s to DUID %q IAID %q; the endpoint's MAC %s gives DUID %q IAID %q",
			addr, duid, iaid, mac, wantDUID, wantIAID)
	}
}

// Docker mints a new MAC at every start of an IPAM-mode endpoint; the re-bind carries the v6 record with the v4 one,
// so the identity minted on the first MAC is sent again (#960).

// TestIPAMv6_ASingleRestartChangesTheMACAndKeepsDUIDIAIDAndAddress checks the server's lease after a docker restart.
func TestIPAMv6_ASingleRestartChangesTheMACAndKeepsDUIDIAIDAndAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam6-restart"
	cli := ipamDockerClient(t)
	ipam6Network(t, ctx, netName, nil)
	id, _, mac1 := harness.RunContainer(t, ctx, netName, netName+"-ctr")

	addr1 := ipam6Addr(t, ctx, cli, id, netName)
	iaid1, duid1 := ipam6Lease(t, fixture.LeaseFile(), addr1)
	wantIAID, wantDUID := ipam6IdentityOf(t, mac1)
	if !strings.EqualFold(duid1, wantDUID) || iaid1 != wantIAID {
		t.Fatalf("before the restart the server leased %s to DUID %q IAID %q, want %q %q from MAC %s",
			addr1, duid1, iaid1, wantDUID, wantIAID, mac1)
	}
	replies := countDHCPv6Replies(t, fixture.DnsmasqLog(), addr1)

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}
	_, mac2 := ipamNetworkAddress(t, ctx, cli, id, netName)
	addr2 := ipam6Addr(t, ctx, cli, id, netName)

	if strings.EqualFold(mac1, mac2) {
		t.Errorf("the endpoint kept MAC %s across the restart, so this run did not exercise the "+
			"identity carry-over it exists for", mac1)
	}
	if addr2 != addr1 {
		t.Errorf("the container came back on IPv6 %s; it held %s", addr2, addr1)
	}
	deadline := time.Now().Add(30 * time.Second)
	for countDHCPv6Replies(t, fixture.DnsmasqLog(), addr1) <= replies && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	if countDHCPv6Replies(t, fixture.DnsmasqLog(), addr1) <= replies {
		t.Fatalf("no DHCPREPLY for %s after the restart, so the lease file below predates it", addr1)
	}
	iaid2, duid2 := ipam6Lease(t, fixture.LeaseFile(), addr1)
	if !strings.EqualFold(duid2, duid1) || iaid2 != iaid1 {
		t.Errorf("after the restart the server leased %s to DUID %q IAID %q; before it was %q %q. "+
			"The identity minted on the first MAC (%s) must be sent again on the new one (%s)",
			addr1, duid2, iaid2, duid1, iaid1, mac1, mac2)
	}
}

// Two retained v4 records make the re-bind ambiguous, and neither family is re-bound (#960), so each
// container mints on its new MAC; docs/reference.md states it.

// TestIPAMv6_TwoRestartedTogetherIsTheDocumentedLimit checks that both come back with v6 and each under a new identity.
func TestIPAMv6_TwoRestartedTogetherIsTheDocumentedLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam6-together"
	cli := ipamDockerClient(t)
	ipam6Network(t, ctx, netName, nil)
	ids := map[string]string{}
	duids := map[string]string{}
	for _, n := range []string{"a", "b"} {
		id, _, _ := harness.RunContainer(t, ctx, netName, netName+"-"+n)
		ids[n] = id
		_, duids[n] = ipam6Lease(t, fixture.LeaseFile(), ipam6Addr(t, ctx, cli, id, netName))
	}
	for _, n := range []string{"a", "b"} {
		if err := cli.ContainerStop(ctx, ids[n], container.StopOptions{}); err != nil {
			t.Fatalf("ContainerStop %s: %v", n, err)
		}
	}
	for _, n := range []string{"a", "b"} {
		if err := cli.ContainerStart(ctx, ids[n], container.StartOptions{}); err != nil {
			t.Fatalf("ContainerStart %s: %v", n, err)
		}
	}
	for _, n := range []string{"a", "b"} {
		_, mac := ipamNetworkAddress(t, ctx, cli, ids[n], netName)
		addr := ipam6Addr(t, ctx, cli, ids[n], netName)
		_, duid := ipam6Lease(t, fixture.LeaseFile(), addr)
		_, fresh := ipam6IdentityOf(t, mac)
		if !strings.EqualFold(duid, fresh) || strings.EqualFold(duid, duids[n]) {
			t.Errorf("container %s came back on %s under DUID %q; the documented limit is a new "+
				"identity from its new MAC %s (%q), and it held %q before", n, addr, duid, mac, fresh, duids[n])
		}
	}
}

// TestIPAMv6_DashDashIPv6IsRefusedNamingTheOptionsThatSwitchIPv6On checks the RequestPool refusal as the daemon returns it.
func TestIPAMv6_DashDashIPv6IsRefusedNamingTheOptionsThatSwitchIPv6On(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const netName = "dh-itest-ipam6-flag"
	cli := ipamDockerClient(t)
	on := true
	res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver:     harness.DriverName,
		EnableIPv6: &on,
		IPAM:       &network.IPAM{Driver: harness.DriverName},
		Options:    map[string]string{"mode": "macvlan", "parent": harness.HostVeth},
	})
	if err == nil {
		_ = cli.NetworkRemove(context.Background(), res.ID)
		t.Fatal("docker network create --ipv6 was accepted on a network with this plugin as its IPAM driver")
	}
	for _, want := range []string{"the plugin allocates no IPv6 pool", "drop --ipv6", "`-o ipv6=true`", "`-o ipv6_mode=<mode>`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "#960") {
		t.Errorf("the refusal still points at the issue that lifted it:\n%v", err)
	}
}

// RFC 9915 section 18.2.7 and RFC 2131 section 4.4.6: one Release per family. dnsmasq logs a v4 DHCPRELEASE with the
// address and a v6 one with the client DUID only (rfc2131.c, rfc3315.c), so each family is counted by its own key (#962).

// TestIPAMv6_OnStopSendsOneReleasePerFamily checks the server's log and lease file across docker stop on release_lease=on_stop.
func TestIPAMv6_OnStopSendsOneReleasePerFamily(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam6-onstop"
	cli := ipamDockerClient(t)
	ipam6Network(t, ctx, netName, map[string]string{"release_lease": "on_stop"})
	id, v4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	v6 := ipam6Addr(t, ctx, cli, id, netName)
	for _, addr := range []string{v4, v6} {
		if !waitLeaseFile(t, fixture.LeaseFile(), addr, true) {
			t.Fatalf("the lease file holds no entry for %s before the stop", addr)
		}
	}
	_, duid := ipam6Lease(t, fixture.LeaseFile(), v6)
	if duid == "" {
		t.Fatalf("no DUID in the lease line for %s", v6)
	}
	keys := map[string]string{v4: v4, v6: strings.ToLower(duid)}
	before := map[string]int{
		v4: fixture.CountLogLines("DHCPRELEASE", keys[v4]),
		v6: fixture.CountLogLines("DHCPRELEASE", keys[v6]),
	}

	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	for _, addr := range []string{v4, v6} {
		if !waitLeaseFile(t, fixture.LeaseFile(), addr, false) {
			t.Errorf("after docker stop the lease file still holds %s", addr)
		}
		if got := fixture.CountLogLines("DHCPRELEASE", keys[addr]) - before[addr]; got != 1 {
			t.Errorf("the server logged %d DHCPRELEASE line(s) for %s (%s) across the stop, want 1", got, addr, keys[addr])
		}
	}
}

// TestIPAMv6_OnRemoveHandsBothFamiliesBack checks the server's log and lease file after the restart window on release_lease=on_remove (#960).
func TestIPAMv6_OnRemoveHandsBothFamiliesBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ipamDumpOnFailure(t)

	const netName = "dh-itest-ipam6-onremove"
	cli := ipamDockerClient(t)
	ipam6Network(t, ctx, netName, map[string]string{"release_lease": "on_remove"})
	id, v4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	v6 := ipam6Addr(t, ctx, cli, id, netName)
	for _, addr := range []string{v4, v6} {
		if !waitLeaseFile(t, fixture.LeaseFile(), addr, true) {
			t.Fatalf("the lease file holds no entry for %s before the stop", addr)
		}
	}
	_, duid := ipam6Lease(t, fixture.LeaseFile(), v6)
	if duid == "" {
		t.Fatalf("no DUID in the lease line for %s", v6)
	}
	keys := map[string]string{v4: v4, v6: strings.ToLower(duid)}
	before := map[string]int{
		v4: fixture.CountLogLines("DHCPRELEASE", keys[v4]),
		v6: fixture.CountLogLines("DHCPRELEASE", keys[v6]),
	}

	stopped := time.Now()
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	time.Sleep(onRemoveHeldProbe)
	for _, addr := range []string{v4, v6} {
		if !leaseFileHolds(t, fixture.LeaseFile(), addr) {
			t.Errorf("the server gave %s up %s after the stop, inside the %s restart window", addr, onRemoveHeldProbe, onRemoveWindow)
		}
	}
	for _, addr := range []string{v4, v6} {
		if _, gone := waitLeaseFileWithin(t, fixture.LeaseFile(), addr, false, onRemoveVisibleBudget-time.Since(stopped)); !gone {
			t.Errorf("the lease file still holds %s %s after the stop", addr, time.Since(stopped))
		}
	}
	t.Logf("both leases went back %s after the stop", time.Since(stopped))
	for _, addr := range []string{v4, v6} {
		if got := fixture.CountLogLines("DHCPRELEASE", keys[addr]) - before[addr]; got != 1 {
			t.Errorf("the server logged %d DHCPRELEASE line(s) for %s (%s) after the stop, want 1", got, addr, keys[addr])
		}
	}
}

// eui64 is RFC 4291 appendix A's modified EUI-64 address of mac in prefix.
func eui64(t *testing.T, prefix netip.Prefix, mac string) string {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		t.Fatalf("parse MAC %q: %v", mac, err)
	}
	b := prefix.Masked().Addr().As16()
	copy(b[8:], []byte{hw[0] ^ 0x02, hw[1], hw[2], 0xff, 0xfe, hw[3], hw[4], hw[5]})
	return netip.AddrFrom16(b).String()
}

// awaitLinkV6 polls the container's global v6 address until it is want or the budget is spent, and returns the last read.
func awaitLinkV6(t *testing.T, ctx context.Context, id, want string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	var got string
	for {
		got = linkGlobalV6(t, ctx, id, 2*time.Second)
		if got == want || !time.Now().Before(deadline) {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// A SLAAC address is formed from the link's MAC (RFC 4291 appendix A, RFC 4862 section 5.5.3) and Docker mints a new
// MAC at every start in IPAM mode, so the address moves unless --mac-address fixes the MAC (#960).

// TestIPAMv6_ASLAACAddressFollowsTheFreshMACAndMacAddressPinsIt checks the address after docker restart with and without --mac-address.
func TestIPAMv6_ASLAACAddressFollowsTheFreshMACAndMacAddressPinsIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6SLAAC)
	dumpOnFailure(t, f)
	prefix := v6SegmentPrefix(t)

	const netName = "dh-itest-ipam6-slaacmac"
	onV6IPAMBridge.createNet(t, ctx, netName, map[string]string{"bridge": f.Bridge(), "ipv6_mode": "slaac"})

	const pinned = "02:96:00:00:00:61"
	for _, c := range []struct {
		name string
		mac  string
	}{{netName + "-fresh", ""}, {netName + "-pinned", pinned}} {
		if err := ipamRunContainerErr(t, ctx, cli, netName, c.name, &network.EndpointSettings{MacAddress: c.mac}); err != nil {
			t.Fatalf("start %s: %v", c.name, err)
		}
		_, mac1 := ipamNetworkAddress(t, ctx, cli, c.name, netName)
		want1 := eui64(t, prefix, mac1)
		if got := awaitLinkV6(t, ctx, c.name, want1, slaacAddrBudget()); got != want1 {
			t.Fatalf("%s holds %q, want %s from its MAC %s", c.name, got, want1, mac1)
		}

		if err := cli.ContainerRestart(ctx, c.name, container.StopOptions{}); err != nil {
			t.Fatalf("ContainerRestart %s: %v", c.name, err)
		}
		_, mac2 := ipamNetworkAddress(t, ctx, cli, c.name, netName)
		want2 := eui64(t, prefix, mac2)
		got := awaitLinkV6(t, ctx, c.name, want2, slaacAddrBudget())
		if got != want2 {
			t.Errorf("%s holds %q after the restart, want %s from its MAC %s", c.name, got, want2, mac2)
		}
		if shown := inspectV6(t, ctx, cli, c.name, netName); shown != got {
			t.Errorf("%s: docker inspect shows %q and the link carries %q", c.name, shown, got)
		}
		switch {
		case c.mac != "" && (!strings.EqualFold(mac2, c.mac) || got != want1):
			t.Errorf("%s was started with --mac-address %s and came back on MAC %s and %q, "+
				"it held %s", c.name, c.mac, mac2, got, want1)
		case c.mac == "" && got == want1:
			t.Errorf("%s kept %s across the restart without --mac-address, on MAC %s then %s; "+
				"the documented limit is an address that follows the new MAC", c.name, got, mac1, mac2)
		}
		t.Logf("%s: %s on %s, then %s on %s", c.name, want1, mac1, got, mac2)
	}
}

// The v4 exchange runs in RequestAddress and the v6 one in CreateEndpoint, each an RPC with its own 30 s
// client timeout, so a silent DHCPv6 server fails the start after both; this measures that wall clock (#960).

// TestIPAMv6_ASilentManagedServerFailsTheStartInsideAMinute checks and logs the time docker start takes to fail.
func TestIPAMv6_ASilentManagedServerFailsTheStartInsideAMinute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6ManagedSilent)
	dumpOnFailure(t, f)

	const netName = "dh-itest-ipam6-silent"
	onV6IPAMBridge.createNet(t, ctx, netName, map[string]string{"bridge": f.Bridge(), "ipv6": "true"})
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, netName+"-ctr")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	})

	start := time.Now()
	err = cli.ContainerStart(ctx, create.ID, container.StartOptions{})
	elapsed := time.Since(start)
	t.Logf("wall clock: docker start failed after %.1fs: %v", elapsed.Seconds(), err)
	if err == nil {
		t.Fatal("the container started on a segment that advertised managed DHCPv6 and answered nothing")
	}
	if !strings.Contains(err.Error(), "via DHCPv6") || strings.Contains(err.Error(), "Client.Timeout") {
		t.Errorf("the start failed, but not with the plugin's DHCPv6 verdict:\n%v", err)
	}
	if elapsed >= time.Minute {
		t.Errorf("docker start took %s to fail, past the minute the two RPC budgets allow", elapsed)
	}
	f.AwaitIgnoredSolicit(30 * time.Second)
}
