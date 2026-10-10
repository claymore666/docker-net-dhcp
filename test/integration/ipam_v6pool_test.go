// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestIPAMv6Pool_TypedSubnetCreatesAndInspectShowsBothPools checks that a typed IPv6 --subnet becomes a pool of
// this driver and the daemon's record carries both pools (#1132); starting a container on it is the address side.
func TestIPAMv6Pool_TypedSubnetCreatesAndInspectShowsBothPools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName = "dh-itest-ipam6-pool"
	v4 := netip.MustParsePrefix(harness.V6SubnetCIDR)
	v6 := netip.MustParsePrefix(harness.V6SubnetV6CIDR)
	on := true
	res, err := cli.NetworkCreate(ctx, netName, docker.NetworkCreateOptions{
		Driver:     harness.DriverName,
		EnableIPv6: &on,
		IPAM: &network.IPAM{
			Driver: harness.DriverName,
			Config: []network.IPAMConfig{{Subnet: v4}, {Subnet: v6}},
		},
		Options: map[string]string{"mode": "bridge", "bridge": f.Bridge(), "ipv6_mode": "dhcp"},
	})
	if err != nil {
		t.Fatalf("NetworkCreate(%s, subnets %s and %s): %v", netName, v4, v6, err)
	}
	t.Cleanup(func() {
		_, _ = cli.NetworkRemove(context.Background(), res.ID, docker.NetworkRemoveOptions{})
	})

	insp := ipamNetworkInspect(t, ctx, cli, netName)
	if insp.IPAM.Driver != harness.DriverName {
		t.Errorf("the network's IPAM driver is %q, want %q", insp.IPAM.Driver, harness.DriverName)
	}
	got := map[netip.Prefix]bool{}
	for _, c := range insp.IPAM.Config {
		got[c.Subnet] = true
	}
	for _, want := range []netip.Prefix{v4, v6} {
		if !got[want] {
			t.Errorf("docker network inspect shows no pool %s; the IPAM config is %v", want, insp.IPAM.Config)
		}
	}
	if !insp.EnableIPv6 {
		t.Errorf("docker network inspect shows EnableIPv6 false on a network created with --ipv6")
	}
}

// ipam6PoolNetErr creates a bridge network on the v6 fixture with both pools from this driver, the IPv6 one v6, and
// returns the daemon's error; a created network is removed at cleanup (#1132).
func ipam6PoolNetErr(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, name, v6 string, opts map[string]string) error {
	t.Helper()
	all := map[string]string{"mode": "bridge", "bridge": f.Bridge(), "ipv6_mode": "dhcp"}
	for k, v := range opts {
		all[k] = v
	}
	on := true
	res, err := cli.NetworkCreate(ctx, name, docker.NetworkCreateOptions{
		Driver:     harness.DriverName,
		EnableIPv6: &on,
		IPAM: &network.IPAM{
			Driver: harness.DriverName,
			Config: []network.IPAMConfig{
				{Subnet: netip.MustParsePrefix(harness.V6SubnetCIDR)},
				{Subnet: netip.MustParsePrefix(v6)},
			},
		},
		Options: all,
	})
	if err != nil {
		return err
	}
	t.Cleanup(func() {
		_, _ = cli.NetworkRemove(context.Background(), res.ID, docker.NetworkRemoveOptions{})
	})
	return nil
}

func ipam6PoolNet(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, name, v6 string, opts map[string]string) {
	t.Helper()
	if err := ipam6PoolNetErr(t, ctx, cli, f, name, v6, opts); err != nil {
		t.Fatalf("NetworkCreate(%s, subnets %s and %s, %v): %v", name, harness.V6SubnetCIDR, v6, opts, err)
	}
}

// ipam6PoolRun starts a container pinned to mac, with --ip6 when ip6 is set, and returns the daemon's error.
func ipam6PoolRun(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName, ip6, mac string) error {
	t.Helper()
	ep := &network.EndpointSettings{MacAddress: harness.MustMAC(mac)}
	if ip6 != "" {
		ep.IPAMConfig = &network.EndpointIPAMConfig{IPv6Address: netip.MustParseAddr(ip6)}
	}
	return ipamRunContainerErr(t, ctx, cli, netName, ctrName, ep)
}

// ipam6NotRunning fails the test when the named container runs after a start the daemon refused.
func ipam6NotRunning(t *testing.T, ctx context.Context, cli *docker.Client, ctrName string) {
	t.Helper()
	ins, err := cli.ContainerInspect(ctx, ctrName, docker.ContainerInspectOptions{})
	if err != nil {
		return
	}
	if ins.Container.State != nil && ins.Container.State.Running {
		t.Errorf("container %s runs after the daemon refused its start", ctrName)
	}
}

// ipam6Answered is the address a refusal says the server answered, or the test fails.
func ipam6Answered(t *testing.T, err error, re *regexp.Regexp) string {
	t.Helper()
	m := re.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("the refusal %q does not name the address the server answered (%s)", err, re)
	}
	if _, perr := netip.ParseAddr(m[1]); perr != nil {
		t.Fatalf("the refusal %q names %q as the answered address, which is not an address", err, m[1])
	}
	return m[1]
}

// ipam6AssertReleased checks that a refused Reply's lease was handed back: one more DHCPRELEASE for the DUID and the
// lease file no longer holding the address (RFC 9915 section 18.2.7, #1132).
func ipam6AssertReleased(t *testing.T, f *harness.V6Fixture, duid string, releasesBefore int, addr string) {
	t.Helper()
	if n := f.AwaitLogLines(10*time.Second, "DHCPRELEASE", duid); n-releasesBefore < 1 {
		t.Errorf("the server logged no DHCPRELEASE for DUID %s after the refusal; the address %s it "+
			"answered stays leased to a container that never started", duid, addr)
	}
	if !waitLeaseFile(t, f.LeaseFile(), addr, false) {
		t.Errorf("the lease file still holds %s after the refusal", addr)
	}
}

// ipam6InRange reports whether addr lies in the fixture's DHCPv6 range.
func ipam6InRange(addr string) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	return a.Compare(netip.MustParseAddr(harness.V6PoolStartV6)) >= 0 &&
		a.Compare(netip.MustParseAddr(harness.V6PoolEndV6)) <= 0
}

// TestIPAMv6Pool_NoIP6HintGetsTheServersAddress checks that the address Docker shows and the link carries is the one
// the server's Reply gave the endpoint's DUID (#1132, T2).
func TestIPAMv6Pool_NoIP6HintGetsTheServersAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName, ctrName, mac = "dh-itest-ipam6p-nohint", "dh-itest-ipam6p-nohint-ctr", "02:00:00:11:32:02"
	ipam6PoolNet(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, nil)
	if err := ipam6PoolRun(t, ctx, cli, netName, ctrName, "", mac); err != nil {
		t.Fatalf("the container did not start on a network with an IPv6 pool: %v", err)
	}
	addr := ipam6Addr(t, ctx, cli, ctrName, netName)
	if !ipam6InRange(addr) {
		t.Errorf("docker inspect shows %s, outside the server's range %s-%s", addr, harness.V6PoolStartV6, harness.V6PoolEndV6)
	}
	wantIAID, wantDUID := ipam6IdentityOf(t, mac)
	if n := f.AwaitLogLines(10*time.Second, "DHCPREPLY", addr, wantDUID); n < 1 {
		t.Errorf("the server logged no DHCPREPLY of %s to DUID %s, so the address Docker shows is not one "+
			"the server gave this endpoint", addr, wantDUID)
	}
	iaid, duid := ipam6Lease(t, f.LeaseFile(), addr)
	if !strings.EqualFold(duid, wantDUID) || iaid != wantIAID {
		t.Errorf("the server leased %s to DUID %q IAID %q; MAC %s gives DUID %q IAID %q", addr, duid, iaid, mac, wantDUID, wantIAID)
	}
}

// TestIPAMv6Pool_AnIP6InTheRangeIsTheAddress checks that a --ip6 the server can give is the address of the Reply,
// docker inspect and the link (#1132, T3).
func TestIPAMv6Pool_AnIP6InTheRangeIsTheAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName, ctrName, mac = "dh-itest-ipam6p-hint", "dh-itest-ipam6p-hint-ctr", "02:00:00:11:32:03"
	const hint = "fd00:6470:6865::20"
	ipam6PoolNet(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, nil)
	if err := ipam6PoolRun(t, ctx, cli, netName, ctrName, hint, mac); err != nil {
		t.Fatalf("the container with --ip6 %s did not start: %v", hint, err)
	}
	if addr := ipam6Addr(t, ctx, cli, ctrName, netName); addr != hint {
		t.Errorf("docker inspect and the link show %s; --ip6 asked for %s", addr, hint)
	}
	_, duid := ipam6IdentityOf(t, mac)
	if n := f.AwaitLogLines(10*time.Second, "DHCPREPLY", hint, duid); n < 1 {
		t.Errorf("the server logged no DHCPREPLY of %s to DUID %s", hint, duid)
	}
}

// TestIPAMv6Pool_AnIgnoredHintIsRefusedNamingBothAndReleased checks D3: a Reply naming another address than --ip6
// fails the start naming both, and the lease is released (#1132, T4). The fixture has no range holding the hint.
func TestIPAMv6Pool_AnIgnoredHintIsRefusedNamingBothAndReleased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName, ctrName, mac = "dh-itest-ipam6p-ignored", "dh-itest-ipam6p-ignored-ctr", "02:00:00:11:32:04"
	hint := harness.V6StaticOnlyAddrV6
	ipam6PoolNet(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, nil)
	_, duid := ipam6IdentityOf(t, mac)
	releases := f.CountLogLines("DHCPRELEASE", duid)

	err := ipam6PoolRun(t, ctx, cli, netName, ctrName, hint, mac)
	if err == nil {
		t.Fatalf("the container started with --ip6 %s, an address the server serves to nobody; it holds %s",
			hint, ipam6Addr(t, ctx, cli, ctrName, netName))
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), hint) {
		t.Errorf("the refusal %q does not name the --ip6 that was asked (%s)", err, hint)
	}
	answered := ipam6Answered(t, err, regexp.MustCompile(`answered (\S+) instead`))
	if !ipam6InRange(answered) {
		t.Errorf("the refusal names %s as the server's answer, outside the server's range", answered)
	}
	ipam6AssertReleased(t, f, duid, releases, answered)
	ipam6NotRunning(t, ctx, cli, ctrName)
}

// TestIPAMv6Pool_AnIP6OutsideThePrefixIsRefusedBeforeAnySolicit checks that the engine refuses a --ip6 outside the
// IPv6 subnet before the plugin puts a Solicit on the segment (#1132, T5).
func TestIPAMv6Pool_AnIP6OutsideThePrefixIsRefusedBeforeAnySolicit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName, ctrName, mac = "dh-itest-ipam6p-foreign", "dh-itest-ipam6p-foreign-ctr", "02:00:00:11:32:05"
	const foreign = "fd00:9999::20"
	ipam6PoolNet(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, nil)
	solicits := f.CountLogLines("DHCPSOLICIT")

	err := ipam6PoolRun(t, ctx, cli, netName, ctrName, foreign, mac)
	if err == nil {
		t.Fatalf("a container started with --ip6 %s, outside the network's IPv6 subnet %s", foreign, harness.V6SubnetV6CIDR)
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), foreign) {
		t.Errorf("the refusal %q does not name the --ip6 that was typed (%s)", err, foreign)
	}
	time.Sleep(2 * time.Second)
	if n := f.CountLogLines("DHCPSOLICIT") - solicits; n != 0 {
		t.Errorf("the server logged %d DHCPSOLICIT line(s) for a --ip6 the engine refuses; nothing should reach the wire", n)
	}
	ipam6NotRunning(t, ctx, cli, ctrName)
}

// TestIPAMv6Pool_AReplyOutsideTheTypedPrefixIsRefusedAndReleased checks D4: a typed /120 beside the server's range
// gets a Reply outside it, which fails the start naming the subnet, and the lease is released (#1132, T6).
func TestIPAMv6Pool_AReplyOutsideTheTypedPrefixIsRefusedAndReleased(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName, ctrName, mac = "dh-itest-ipam6p-beside", "dh-itest-ipam6p-beside-ctr", "02:00:00:11:32:06"
	const typed = "fd00:6470:6865::100/120"
	ipam6PoolNet(t, ctx, cli, f, netName, typed, nil)
	_, duid := ipam6IdentityOf(t, mac)
	releases := f.CountLogLines("DHCPRELEASE", duid)

	err := ipam6PoolRun(t, ctx, cli, netName, ctrName, "", mac)
	if err == nil {
		t.Fatalf("the container started on IPv6 subnet %s, which the server's range %s-%s does not touch; it holds %s",
			typed, harness.V6PoolStartV6, harness.V6PoolEndV6, ipam6Addr(t, ctx, cli, ctrName, netName))
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), typed) {
		t.Errorf("the refusal %q does not name the subnet that was typed (%s)", err, typed)
	}
	answered := ipam6Answered(t, err, regexp.MustCompile(`answered (\S+), which is outside`))
	if !ipam6InRange(answered) {
		t.Errorf("the refusal names %s as the server's answer, outside the server's range", answered)
	}
	ipam6AssertReleased(t, f, duid, releases, answered)
	ipam6NotRunning(t, ctx, cli, ctrName)
}

// TestIPAMv6Pool_AnExhaustedServerFailsInsideTheBudget checks that a server with no address to give fails the start
// with the plugin's DHCPv6 message, not the daemon's call timeout, and that a second start fails the same way rather
// than on a reservation the first left behind (#1132, T7).
func TestIPAMv6Pool_AnExhaustedServerFailsInsideTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6ManagedExhausted)
	dumpOnFailure(t, f)

	const netName, mac = "dh-itest-ipam6p-exhausted", "02:00:00:11:32:07"
	ipam6PoolNet(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, nil)
	for _, ctrName := range []string{netName + "-a", netName + "-b"} {
		start := time.Now()
		err := ipam6PoolRun(t, ctx, cli, netName, ctrName, "", mac)
		took := time.Since(start)
		if err == nil {
			t.Fatalf("container %s started on a server with no IPv6 address to give; it shows %s",
				ctrName, inspectV6(t, ctx, cli, ctrName, netName))
		}
		t.Logf("%s refused after %v: %v", ctrName, took, err)
		if !strings.Contains(err.Error(), "via DHCPv6") {
			t.Errorf("container %s was refused with %q, not the plugin's DHCPv6 failure; after a first refusal "+
				"this is what a reservation left behind looks like", ctrName, err)
		}
		if took > time.Minute {
			t.Errorf("the refusal of %s took %v; the plugin's budget per call is 30s", ctrName, took)
		}
		ipam6NotRunning(t, ctx, cli, ctrName)
	}
	_, duid := ipam6IdentityOf(t, mac)
	if n := f.CountLogLines("DHCPREPLY", duid); n != 0 {
		t.Errorf("the exhausted server logged %d DHCPREPLY line(s) for DUID %s", n, duid)
	}
}

// TestIPAMv6Pool_RestartKeepsTheV6AddressAndDUID checks that docker restart, which mints a new MAC, comes back on the
// same IPv6 address under the same DUID and IAID (#1132, T8).
func TestIPAMv6Pool_RestartKeepsTheV6AddressAndDUID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName = "dh-itest-ipam6p-restart"
	ipam6PoolNet(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, nil)
	id, _, mac1 := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	addr1 := ipam6Addr(t, ctx, cli, id, netName)
	iaid1, duid1 := ipam6Lease(t, f.LeaseFile(), addr1)
	wantIAID, wantDUID := ipam6IdentityOf(t, mac1)
	if !strings.EqualFold(duid1, wantDUID) || iaid1 != wantIAID {
		t.Fatalf("before the restart the server leased %s to DUID %q IAID %q, want %q %q from MAC %s",
			addr1, duid1, iaid1, wantDUID, wantIAID, mac1)
	}
	replies := f.CountLogLines("DHCPREPLY", addr1)

	if _, err := cli.ContainerRestart(ctx, id, docker.ContainerRestartOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}
	_, mac2 := ipamNetworkAddress(t, ctx, cli, id, netName)
	if addr2 := ipam6Addr(t, ctx, cli, id, netName); addr2 != addr1 {
		t.Errorf("the container came back on IPv6 %s; it held %s", addr2, addr1)
	}
	if strings.EqualFold(mac1, mac2) {
		t.Errorf("the endpoint kept MAC %s across the restart, so this run did not exercise the identity carry-over", mac1)
	}
	if n := f.AwaitLogLines(30*time.Second, "DHCPREPLY", addr1); n <= replies {
		t.Fatalf("no DHCPREPLY for %s after the restart, so the lease file below predates it", addr1)
	}
	iaid2, duid2 := ipam6Lease(t, f.LeaseFile(), addr1)
	if !strings.EqualFold(duid2, duid1) || iaid2 != iaid1 {
		t.Errorf("after the restart the server leased %s to DUID %q IAID %q; before it was %q %q (MACs %s, %s)",
			addr1, duid2, iaid2, duid1, iaid1, mac1, mac2)
	}
}

// TestIPAMv6Pool_SLAACWithAPoolIsRefusedAtCreate checks D5: an IPv6 pool needs ipv6_mode=dhcp (#1132, T9).
func TestIPAMv6Pool_SLAACWithAPoolIsRefusedAtCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName = "dh-itest-ipam6p-slaac"
	err := ipam6PoolNetErr(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, map[string]string{"ipv6_mode": "slaac"})
	if err == nil {
		t.Fatalf("a network with an IPv6 pool and ipv6_mode=slaac was created; its addresses would come from no one")
	}
	for _, want := range []string{"ipv6_mode=slaac", "-o ipv6_mode=dhcp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not contain %q", err, want)
		}
	}
	if _, ierr := cli.NetworkInspect(ctx, netName, docker.NetworkInspectOptions{}); ierr == nil {
		t.Errorf("network %s exists after its create was refused", netName)
	}
}

// TestIPAMv6Pool_OnStopSendsOneReleasePerFamily checks that release_lease=on_stop hands both leases back once each;
// dnsmasq logs a v4 DHCPRELEASE with the address and a v6 one with the DUID (#1132, T10).
func TestIPAMv6Pool_OnStopSendsOneReleasePerFamily(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := ipamDockerClient(t)
	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName = "dh-itest-ipam6p-onstop"
	ipam6PoolNet(t, ctx, cli, f, netName, harness.V6SubnetV6CIDR, map[string]string{"release_lease": "on_stop"})
	id, v4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	v6 := ipam6Addr(t, ctx, cli, id, netName)
	for _, addr := range []string{v4, v6} {
		if !waitLeaseFile(t, f.LeaseFile(), addr, true) {
			t.Fatalf("the lease file holds no entry for %s before the stop", addr)
		}
	}
	_, duid := ipam6Lease(t, f.LeaseFile(), v6)
	if duid == "" {
		t.Fatalf("no DUID in the lease line for %s", v6)
	}
	keys := map[string]string{v4: v4, v6: strings.ToLower(duid)}
	before := map[string]int{v4: f.CountLogLines("DHCPRELEASE", keys[v4]), v6: f.CountLogLines("DHCPRELEASE", keys[v6])}

	if _, err := cli.ContainerStop(ctx, id, docker.ContainerStopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if _, err := cli.ContainerRemove(ctx, id, docker.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	for _, addr := range []string{v4, v6} {
		if !waitLeaseFile(t, f.LeaseFile(), addr, false) {
			t.Errorf("after docker stop and rm the lease file still holds %s", addr)
		}
		if got := f.CountLogLines("DHCPRELEASE", keys[addr]) - before[addr]; got != 1 {
			t.Errorf("the server logged %d DHCPRELEASE line(s) for %s (%s) across stop and rm, want 1", got, addr, keys[addr])
		}
	}
}
