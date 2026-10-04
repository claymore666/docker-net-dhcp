// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// keaPDMovedPool is the pool case 3 reconfigures Kea to, apart from harness.Kea6PDPrefix's /48.
const keaPDMovedPool = "fd00:99::"

// keaPDSegment is TestKea6ServesAnAddress's managed segment with Kea started on opts.
func keaPDSegment(t *testing.T, opts ...harness.Kea6Option) (*harness.V6Fixture, *harness.Kea6Fixture) {
	t.Helper()
	spec := harness.RASpec{
		Managed:        true,
		RouterLifetime: harness.AdvertRouteLifetime,
		Prefixes:       []harness.RAPrefix{advertPrefix(t, harness.V6SubnetV6CIDR, false, 1800, 1800)},
	}
	f, sender := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), spec)
	dumpOnFailure(t, f)
	kea := harness.NewKea6Fixture(t, f, opts...)
	t.Cleanup(func() {
		if t.Failed() {
			kea.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
	answerRouterSolicits(t, f.Bridge(), sender, spec)
	return f, kea
}

// startPDContainer starts a container on an ipv6_pd=64 network and returns it with its address and DUID (#214).
func startPDContainer(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, kea *harness.Kea6Fixture,
	at v6Attach, netName string, extra map[string]string) (id, addr, duid string) {
	t.Helper()
	opts := map[string]string{"ipv6_mode": "dhcp", "ipv6_pd": "64", "propagate_dns": ""}
	for k, v := range extra {
		opts[k] = v
	}
	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, netName, opts)
	if err != nil {
		t.Fatalf("ContainerStart on the Kea segment with ipv6_pd=64: %v", err)
	}
	if addr = linkGlobalV6(t, ctx, id, 30*time.Second); addr == "" {
		t.Fatalf("the container holds no global IPv6 address 30s after it started; Kea's lease file:\n%s", kea.LeaseFileText())
	}
	iface := containerV6Iface(t, ctx, id, addr)
	macText := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/"+iface+"/address"))
	mac, err := net.ParseMAC(macText)
	if err != nil {
		t.Fatalf("the container's link %s has MAC %q: %v", iface, macText, err)
	}
	return id, addr, harness.DUIDLLFromMAC(mac)
}

// keaPDRows are the IA_PD rows Kea holds for duid, as prefixes.
func keaPDRows(kea *harness.Kea6Fixture, duid string) ([]harness.Kea6Row, []netip.Prefix) {
	rows := kea.HeldRows(duid, harness.Kea6LeasePD)
	var out []netip.Prefix
	for _, r := range rows {
		if a, err := netip.ParseAddr(r.Addr); err == nil {
			out = append(out, netip.PrefixFrom(a, r.PrefixLen))
		}
	}
	return rows, out
}

func pollPrefixes(budget time.Duration, read func() []netip.Prefix, ok func([]netip.Prefix) bool) ([]netip.Prefix, bool) {
	deadline := time.Now().Add(budget)
	for {
		got := read()
		if ok(got) {
			return got, true
		}
		if !time.Now().Before(deadline) {
			return got, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func aggregatesOf(t *testing.T, ctx context.Context, id string) func() []netip.Prefix {
	return func() []netip.Prefix { return harness.DelegatedAggregates(harness.ContainerV6Routes(t, ctx, id)) }
}

func heldPrefixesOf(kea *harness.Kea6Fixture, duid string) func() []netip.Prefix {
	return func() []netip.Prefix { _, p := keaPDRows(kea, duid); return p }
}

func nonEmpty(p []netip.Prefix) bool { return len(p) > 0 }

func samePrefixes(want []netip.Prefix) func([]netip.Prefix) bool {
	return func(got []netip.Prefix) bool {
		a, b := slices.Clone(got), slices.Clone(want)
		cmp := func(x, y netip.Prefix) int { return x.Addr().Compare(y.Addr()) }
		slices.SortFunc(a, cmp)
		slices.SortFunc(b, cmp)
		return slices.Equal(a, b)
	}
}

// awaitDelegation waits for Kea's IA_PD row for duid and for the container's aggregate of it.
func awaitDelegation(t *testing.T, ctx context.Context, kea *harness.Kea6Fixture, id, duid string) []netip.Prefix {
	t.Helper()
	held, ok := pollPrefixes(15*time.Second, heldPrefixesOf(kea, duid), nonEmpty)
	if !ok {
		t.Fatalf("Kea holds no IA_PD lease for DUID %s 15s after the address; Kea's lease file:\n%s", duid, kea.LeaseFileText())
	}
	if len(held) != 1 {
		t.Errorf("Kea holds %d IA_PD prefixes for DUID %s, want the one IA_PD the client asks for: %v", len(held), duid, held)
	}
	if got, ok := pollPrefixes(15*time.Second, aggregatesOf(t, ctx, id), samePrefixes(held)); !ok {
		t.Fatalf("the container's unreachable proto dhcp routes are %v, want Kea's delegation %v; its routes: %+v",
			got, held, harness.ContainerV6Routes(t, ctx, id))
	}
	return held
}

// TestKea6PD_TheDelegatedPrefixIsAnUnreachableAggregate checks the bridge path (#214).
func TestKea6PD_TheDelegatedPrefixIsAnUnreachableAggregate(t *testing.T) {
	testKea6PDDelegation(t, onV6Bridge, "dh-itest-pd")
}

// TestKea6PD_TheDelegatedPrefixIsAnUnreachableAggregate_Macvlan checks the macvlan path (#214).
func TestKea6PD_TheDelegatedPrefixIsAnUnreachableAggregate_Macvlan(t *testing.T) {
	testKea6PDDelegation(t, onV6Macvlan, "dh-itest-pdmv")
}

// The prefix is never put on the upstream link (RFC 3633 section 12.1), so the aggregate sits on lo and no address of
// the container is taken from it (#214).
func testKea6PDDelegation(t *testing.T, at v6Attach, netName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)
	f, kea := keaPDSegment(t, harness.WithKea6PD())
	id, _, duid := startPDContainer(t, ctx, cli, f, kea, at, netName, nil)

	held := awaitDelegation(t, ctx, kea, id, duid)
	pool := netip.PrefixFrom(netip.MustParseAddr(harness.Kea6PDPrefix), harness.Kea6PDPoolLen)
	for _, p := range held {
		if !pool.Contains(p.Addr()) || p.Bits() != harness.Kea6PDDelegatedLen {
			t.Errorf("Kea delegated %s, want a /%d out of %s", p, harness.Kea6PDDelegatedLen, pool)
		}
	}
	for _, r := range harness.ContainerV6Routes(t, ctx, id) {
		if r.Unreachable && r.DHCP && r.Dev != "lo" {
			t.Errorf("the aggregate %s is on %q, want the kernel's lo: a route with a link names that link", r.Dst, r.Dev)
		}
	}
	out := harness.ExecOutput(t, ctx, id, "ip", "-6", "-o", "addr", "show")
	for _, field := range strings.Fields(out) {
		a, err := netip.ParsePrefix(field)
		if err != nil {
			continue
		}
		for _, p := range held {
			if p.Contains(a.Addr()) {
				t.Errorf("the container holds %s out of the delegated %s: the prefix was put on a link\n%s", a, p, out)
			}
		}
	}

	h := harness.WaitPluginHealth(t, ctx, cli, 10*time.Second)
	if !healthReports(h, held[0]) {
		t.Errorf("no endpoint in Plugin.Health reports the delegated %s: %+v", held[0], h.Endpoints)
	}
}

func healthReports(h *harness.HealthResponse, p netip.Prefix) bool {
	for _, ep := range h.Endpoints {
		for _, d := range ep.DelegatedPrefixes {
			if d.Prefix == p.String() {
				return true
			}
		}
	}
	return false
}

// TestKea6PD_TheAggregateRefusesTheUnusedRestOfThePrefix checks that a destination in the delegated prefix fails at the
// sender, where the same send outside the prefix leaves on the default route (#214).
func TestKea6PD_TheAggregateRefusesTheUnusedRestOfThePrefix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)
	f, kea := keaPDSegment(t, harness.WithKea6PD())
	id, _, duid := startPDContainer(t, ctx, cli, f, kea, onV6Bridge, "dh-itest-pdagg", nil)
	held := awaitDelegation(t, ctx, kea, id, duid)

	// The control needs the default route the sender's Router Lifetime installs; without it both sends would fail alike.
	deadline := time.Now().Add(15 * time.Second)
	for strings.TrimSpace(harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show", "default")) == "" {
		if !time.Now().Before(deadline) {
			t.Fatalf("the container has no IPv6 default route 15s after its delegation; the control cannot be read")
		}
		time.Sleep(250 * time.Millisecond)
	}

	b := held[0].Addr().As16()
	b[14], b[15] = 0xff, 0x09
	inside := netip.AddrFrom16(b)
	// An unreachable route fails the send with EHOSTUNREACH, busybox ping's "Host is unreachable" (measured on
	// alpine:3.20, #214); a missing route would be ENETUNREACH, and a default route sends.
	if out := harness.ExecOutput(t, ctx, id, "ping", "-6", "-c1", "-W1", inside.String()); !strings.Contains(out, "sendto: Host is unreachable") {
		t.Errorf("a ping to %s inside the delegated %s was not refused at the sender:\n%s", inside, held[0], out)
	}
	outside := netip.MustParseAddr("fd00:55::9")
	if out := harness.ExecOutput(t, ctx, id, "ping", "-6", "-c1", "-W1", outside.String()); strings.Contains(out, "sendto:") {
		t.Errorf("the control: a ping to %s, outside the delegation, failed at the sender too, so the refusal above "+
			"is not the aggregate's:\n%s", outside, out)
	}
}

// TestKea6PD_ARenewOnAChangedPoolMovesTheAggregate checks that a renewal answered from a reconfigured pool replaces the
// aggregate and leaves the address alone (#214).
func TestKea6PD_ARenewOnAChangedPoolMovesTheAggregate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)
	f, kea := keaPDSegment(t, harness.WithKea6PD(), harness.WithKea6Timers(10, 16), harness.WithKea6Lifetimes(60, 40))
	id, addr, duid := startPDContainer(t, ctx, cli, f, kea, onV6Bridge, "dh-itest-pdmove", nil)
	old := awaitDelegation(t, ctx, kea, id, duid)
	subnet := netip.MustParsePrefix(harness.V6SubnetV6CIDR)
	hadSubnetRoute := hasRouteTo(harness.ContainerV6Routes(t, ctx, id), subnet)

	moved := netip.PrefixFrom(netip.MustParseAddr(keaPDMovedPool), harness.Kea6PDPoolLen)
	kea.Restart(harness.WithKea6PDPool(harness.Kea6PDPool{Prefix: keaPDMovedPool, PoolLen: harness.Kea6PDPoolLen,
		DelegatedLen: harness.Kea6PDDelegatedLen}))

	inMoved := func(p []netip.Prefix) bool { return len(p) == 1 && moved.Contains(p[0].Addr()) }
	got, ok := pollPrefixes(45*time.Second, aggregatesOf(t, ctx, id), inMoved)
	if !ok {
		t.Fatalf("45s after Kea moved its pool to %s the container's aggregates are %v (before: %v), want one out of %s",
			moved, got, old, moved)
	}
	if held, ok := pollPrefixes(10*time.Second, heldPrefixesOf(kea, duid), samePrefixes(got)); !ok {
		t.Errorf("Kea holds %v for DUID %s, want the routed %v", held, duid, got)
	}
	if now := linkGlobalV6(t, ctx, id, 5*time.Second); now != addr {
		t.Errorf("the container's address went from %s to %q across the prefix change", addr, now)
	}
	if hadSubnetRoute && !hasRouteTo(harness.ContainerV6Routes(t, ctx, id), subnet) {
		t.Errorf("the route to the link's own %s went with the old delegation", subnet)
	}
}

func hasRouteTo(routes []harness.ContainerRoute, dst netip.Prefix) bool {
	for _, r := range routes {
		if r.Dst == dst && !r.Unreachable {
			return true
		}
	}
	return false
}

// TestKea6PD_AServerWithNoPrefixesStillLeasesTheAddress checks that NoPrefixAvail costs the endpoint only the
// delegation (RFC 8415 section 18.2.10.1, #214).
func TestKea6PD_AServerWithNoPrefixesStillLeasesTheAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)
	f, kea := keaPDSegment(t)
	id, addr, duid := startPDContainer(t, ctx, cli, f, kea, onV6Bridge, "dh-itest-pdnone", nil)

	if !keaHoldsAddr(kea.HeldRows(duid, harness.Kea6LeaseNA), netip.MustParseAddr(addr)) {
		t.Errorf("Kea holds no IA_NA lease of %s for DUID %s\nKea's lease file:\n%s", addr, duid, kea.LeaseFileText())
	}
	// The address and the IA_PD's status arrive in one Reply, so the delegation is settled once the address is up.
	if got, appeared := pollPrefixes(5*time.Second, aggregatesOf(t, ctx, id), nonEmpty); appeared {
		t.Errorf("the container routes %v, which no server delegated", got)
	}
	if rows, _ := keaPDRows(kea, duid); len(rows) != 0 {
		t.Errorf("Kea without a pd-pool holds IA_PD rows for DUID %s: %+v", duid, rows)
	}
	h := harness.WaitPluginHealth(t, ctx, cli, 10*time.Second)
	for _, ep := range h.Endpoints {
		if len(ep.DelegatedPrefixes) > 0 {
			t.Errorf("endpoint %s reports delegated prefixes no server gave: %+v", ep.Endpoint, ep.DelegatedPrefixes)
		}
	}
}

// TestKea6PD_ADisconnectOnStopReleasesThePrefixAndWithdrawsTheAggregate checks Leave on release_lease=on_stop from the
// server's lease file and the still-running container's routes (RFC 8415 section 18.2.7, #214).
func TestKea6PD_ADisconnectOnStopReleasesThePrefixAndWithdrawsTheAggregate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)
	const netName = "dh-itest-pdrel"
	f, kea := keaPDSegment(t, harness.WithKea6PD())
	id, _, duid := startPDContainer(t, ctx, cli, f, kea, onV6Bridge, netName, map[string]string{"release_lease": "on_stop"})
	held := awaitDelegation(t, ctx, kea, id, duid)

	before := kea.CountLogLines("DHCP6_RELEASE_PD_EXPIRED")
	if err := cli.NetworkDisconnect(ctx, netName, id, false); err != nil {
		t.Fatalf("NetworkDisconnect: %v", err)
	}
	// Kea writes the row before it logs the release, and a Release has no Reply to wait for (dhcp-golib #60).
	deadline := time.Now().Add(15 * time.Second)
	for kea.CountLogLines("DHCP6_RELEASE_PD_EXPIRED") <= before && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	if kea.CountLogLines("DHCP6_RELEASE_PD_EXPIRED") <= before {
		t.Errorf("Kea logged no DHCP6_RELEASE_PD_EXPIRED within 15s of the disconnect: the Release named no IA_PD")
	}
	for _, r := range harness.Kea6Latest(kea.Rows()) {
		if r.LeaseType == harness.Kea6LeasePD && r.Addr == held[0].Addr().String() && r.Valid != 0 {
			t.Errorf("Kea's last row for %s is %+v after the disconnect, want a valid lifetime of 0", held[0], r)
		}
	}
	for _, r := range harness.ContainerV6Routes(t, ctx, id) {
		if r.DHCP {
			t.Errorf("the container kept the proto dhcp route %+v after it left the network", r)
		}
	}
}

// TestKea6PD_APluginRestartRebindsAndKeepsTheAggregate checks that a recovered endpoint holding a prefix resumes with a
// Rebind, never a Confirm, and keeps its route (RFC 8415 section 18.2.12, #214).
func TestKea6PD_APluginRestartRebindsAndKeepsTheAggregate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)
	f, kea := keaPDSegment(t, harness.WithKea6PD())
	cap6 := f.StartDHCPv6Capture()
	id, _, duid := startPDContainer(t, ctx, cli, f, kea, onV6Bridge, "dh-itest-pdrst", nil)
	held := awaitDelegation(t, ctx, kea, id, duid)
	rows, _ := keaPDRows(kea, duid)
	expireBefore := rows[0].Expire

	// Kea's expiry has a one-second grain, so a Rebind in the same second as the Request would not move it.
	time.Sleep(1100 * time.Millisecond)
	// The resumed endpoint's v4 probe is the asynchronous one and races teardown; one container, one lease, as in
	// TestRecovery_PluginDisableEnable_PreservesEndpoint (#551).
	harness.AllowUnprobedLeases(1)
	mark := time.Now()
	recyclePlugin(t, ctx, cli, nil)

	var after map[uint8]int
	deadline := time.Now().Add(60 * time.Second)
	for {
		after = map[uint8]int{}
		for _, m := range cap6.ClientMessages() {
			if m.At.After(mark) {
				after[m.Type]++
			}
		}
		if after[harness.DHCPv6Rebind] > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if after[harness.DHCPv6Rebind] == 0 {
		cap6.Dump(func(s string) { t.Log(s) })
		t.Fatalf("no Rebind from the client within 60s of the plugin restart; client messages since: %v", after)
	}
	if after[harness.DHCPv6Confirm] > 0 || after[harness.DHCPv6Solicit] > 0 {
		t.Errorf("after the restart the client sent %d Confirm and %d Solicit; a binding with prefixes resumes "+
			"with a Rebind alone", after[harness.DHCPv6Confirm], after[harness.DHCPv6Solicit])
	}
	renewed := func(p []netip.Prefix) bool {
		r, _ := keaPDRows(kea, duid)
		return samePrefixes(held)(p) && len(r) == 1 && r[0].Expire > expireBefore
	}
	if got, ok := pollPrefixes(10*time.Second, heldPrefixesOf(kea, duid), renewed); !ok {
		r, _ := keaPDRows(kea, duid)
		t.Errorf("Kea's IA_PD rows for DUID %s are %v (%+v) after the Rebind, want %v with an expiry past %d",
			duid, got, r, held, expireBefore)
	}
	if got := aggregatesOf(t, ctx, id)(); !samePrefixes(held)(got) {
		t.Errorf("the container routes %v after the restart, want the unchanged %v", got, held)
	}
}
