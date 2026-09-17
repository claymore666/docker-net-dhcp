// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// fakeRouteTable stands in for the container's IPv6 routing table.
//
// It is a RECORDER AND A STORE, not a stub that returns a canned
// answer: the functions under test list, then add, replace and delete,
// and the questions this file has to answer are about WHICH routes came
// off ("only the default", "never the kernel's", "only what we put
// there"). A stub that ignored the writes could not be asked any of
// them.
type fakeRouteTable struct {
	routes  []netlink.Route
	added   []netlink.Route
	deleted []netlink.Route
	replace []netlink.Route
	delErr  error
	addErr  error
	listErr error
}

func (f *fakeRouteTable) install(t *testing.T, m *dhcpManager) {
	t.Helper()
	prevList, prevAdd, prevDel, prevRepl := nlHandleRouteListFiltered, nlHandleRouteAdd, nlHandleRouteDel, nlHandleRouteReplace
	nlHandleRouteListFiltered = func(*netlink.Handle, int, *netlink.Route, uint64) ([]netlink.Route, error) {
		return f.routes, f.listErr
	}
	nlHandleRouteAdd = func(_ *netlink.Handle, r *netlink.Route) error {
		if f.addErr != nil {
			return f.addErr
		}
		f.added = append(f.added, *r)
		return nil
	}
	nlHandleRouteDel = func(_ *netlink.Handle, r *netlink.Route) error {
		if f.delErr != nil {
			return f.delErr
		}
		f.deleted = append(f.deleted, *r)
		return nil
	}
	nlHandleRouteReplace = func(_ *netlink.Handle, r *netlink.Route) error {
		if f.addErr != nil {
			return f.addErr
		}
		f.replace = append(f.replace, *r)
		return nil
	}
	t.Cleanup(func() {
		nlHandleRouteListFiltered, nlHandleRouteAdd, nlHandleRouteDel, nlHandleRouteReplace = prevList, prevAdd, prevDel, prevRepl
	})
	m.netHandle = &netlink.Handle{}
	m.ctrLink = &fakeLink{}
}

func v6Manager(t *testing.T) (*dhcpManager, *Plugin, *fakeRouteTable) {
	t.Helper()
	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"}, DHCPNetworkOptions{IPv6: true}).withPlugin(p)
	f := &fakeRouteTable{}
	f.install(t, m)
	return m, p, f
}

func cidr(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %v: %v", s, err)
	}
	return n
}

func defaultV6Route(gw string) netlink.Route {
	return netlink.Route{Dst: nil, Gw: net.ParseIP(gw), Protocol: unixRTPROTBOOT}
}

// RTPROT_BOOT is what an unspecified RouteAdd lands as. Named here so
// the fixture says "a route somebody installed" rather than a number.
const unixRTPROTBOOT = 3

// A container with no default route gets the one the advertisement
// names, and it is the router's link-local address (RFC 4861 section
// 4.2 requires the Source Address of an advertisement to be one).
func TestReconcileV6DefaultRoute_InstallsTheAdvertisedRouter(t *testing.T) {
	m, _, f := v6Manager(t)
	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.added) != 1 || !f.added[0].Gw.Equal(net.ParseIP("fe80::1")) {
		t.Fatalf("added %v, want one default route via fe80::1", f.added)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("a container with no route had %v deleted", f.deleted)
	}
}

// A segment that renumbers its router moves the container with it. This
// is the case that has no other mechanism: with accept_ra=0 the
// kernel is not watching, and the DHCPv6 lease does not change.
func TestReconcileV6DefaultRoute_FollowsARenumberedRouter(t *testing.T) {
	m, _, f := v6Manager(t)
	f.routes = []netlink.Route{defaultV6Route("fe80::1")}
	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: "fe80::2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.replace) != 1 || !f.replace[0].Gw.Equal(net.ParseIP("fe80::2")) {
		t.Fatalf("replaced %v, want the default route repointed at fe80::2", f.replace)
	}
}

// The steady state writes nothing. An advertisement arrives every few
// seconds on a busy segment; a reconciler that replaced the route each
// time would churn the kernel's table for no reason and make every
// other assertion here unable to tell a change from a repeat.
func TestReconcileV6DefaultRoute_UnchangedRouterWritesNothing(t *testing.T) {
	m, _, f := v6Manager(t)
	f.routes = []netlink.Route{defaultV6Route("fe80::1")}
	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.added)+len(f.replace)+len(f.deleted) != 0 {
		t.Fatalf("an unchanged router produced writes: add=%v replace=%v del=%v", f.added, f.replace, f.deleted)
	}
}

// Router Lifetime 0: the route goes and the counter moves (#821).
func TestWithdraw_RemovesTheRouteAndCountsIt(t *testing.T) {
	m, p, f := v6Manager(t)
	f.routes = []netlink.Route{defaultV6Route("fe80::1")}
	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: ""}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.deleted) != 1 {
		t.Fatalf("deleted %v, want the one default route", f.deleted)
	}
	if got := p.ipv6RouterWithdrawn.Load(); got != 1 {
		t.Errorf("ipv6_router_withdrawn = %d, want 1", got)
	}
}

// THE COUNTER COUNTS ROUTES REMOVED, NOT ADVERTISEMENTS SEEN. RFC 4861
// section 6.2.5 has a router send several advertisements as it shuts
// down, and a counter keyed on the advertisement would report how
// talkative the router was.
func TestWithdraw_RepeatedWithdrawalCountsOnce(t *testing.T) {
	m, p, f := v6Manager(t)
	f.routes = []netlink.Route{defaultV6Route("fe80::1")}
	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: ""}); err != nil {
		t.Fatalf("first withdrawal: %v", err)
	}
	// The route is gone now, which is what the next list returns.
	f.routes = nil
	for i := 0; i < 3; i++ {
		if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: ""}); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
	}
	if got := p.ipv6RouterWithdrawn.Load(); got != 1 {
		t.Errorf("ipv6_router_withdrawn = %d after four withdrawal advertisements, want 1", got)
	}
}

// A delete that fails is not a withdrawal. The counter is the operator's
// answer to "did a container lose its default route", and a route still
// in the table did not.
func TestWithdraw_FailedDeleteDoesNotCount(t *testing.T) {
	m, p, f := v6Manager(t)
	f.routes = []netlink.Route{defaultV6Route("fe80::1")}
	f.delErr = errors.New("no such process")
	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: ""}); err == nil {
		t.Fatal("a failed delete was reported as success")
	}
	if got := p.ipv6RouterWithdrawn.Load(); got != 0 {
		t.Errorf("ipv6_router_withdrawn = %d with the route still in the table, want 0", got)
	}
}

// The deletion is BOUNDED: only default routes, and never one the
// kernel installed. A container is on more than one network and this
// manager answers for one link's worth of routing.
func TestWithdraw_LeavesEverythingThatIsNotOurDefaultRoute(t *testing.T) {
	m, _, f := v6Manager(t)
	f.routes = []netlink.Route{
		defaultV6Route("fe80::1"),
		{Dst: cidr(t, "2001:db8:1::/64"), Protocol: unixRTPROTBOOT},
		{Dst: nil, Gw: net.ParseIP("fe80::9"), Protocol: 2 /* RTPROT_KERNEL */},
		{Dst: cidr(t, "fe80::/64"), Protocol: 2},
	}
	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: ""}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.deleted) != 1 || !f.deleted[0].Gw.Equal(net.ParseIP("fe80::1")) {
		t.Fatalf("deleted %v, want only the non-kernel default route via fe80::1", f.deleted)
	}
}

// An advertisement naming a gateway that is not an IPv6 address leaves
// the container's route alone. `Gw: nil` is not "no change" to netlink,
// it is an on-link default route.
func TestReconcileV6DefaultRoute_RefusesANonIPv6Gateway(t *testing.T) {
	for _, gw := range []string{"192.0.2.1", "not-an-address"} {
		m, _, f := v6Manager(t)
		f.routes = []netlink.Route{defaultV6Route("fe80::1")}
		if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: gw}); err != nil {
			t.Fatalf("%v: unexpected error: %v", gw, err)
		}
		if len(f.added)+len(f.replace)+len(f.deleted) != 0 {
			t.Errorf("%v: the existing route was touched", gw)
		}
	}
}

func destinations(routes []netlink.Route) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		if r.Dst == nil {
			out = append(out, "default")
			continue
		}
		out = append(out, r.Dst.String())
	}
	sort.Strings(out)
	return out
}

// Route Information options arriving for the first time become routes.
func TestReconcileAdvertisedRoutes_AppliesWhatIsAdvertised(t *testing.T) {
	m, _, f := v6Manager(t)
	err := m.reconcileAdvertisedRoutes(dhcp.Info{Routes: []dhcp.Route{
		{Destination: "2001:db8:1::/64", Gateway: "fe80::1"},
		{Destination: "2001:db8:2::/48", Gateway: "fe80::2"},
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := destinations(f.replace); len(got) != 2 {
		t.Fatalf("applied %v, want both prefixes", got)
	}
}

// AND THE OTHER DIRECTION, which is the one with no other mechanism: a
// prefix the advertisement stops mentioning loses its route. RFC 4861
// section 6.3.4 makes each advertisement the answer rather than an
// addition to a previous one.
func TestReconcileAdvertisedRoutes_WithdrawsWhatStoppedBeingAdvertised(t *testing.T) {
	m, _, f := v6Manager(t)
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Routes: []dhcp.Route{
		{Destination: "2001:db8:1::/64", Gateway: "fe80::1"},
		{Destination: "2001:db8:2::/48", Gateway: "fe80::1"},
	}}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	f.replace = nil

	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Routes: []dhcp.Route{
		{Destination: "2001:db8:1::/64", Gateway: "fe80::1"},
	}}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := destinations(f.deleted); len(got) != 1 || got[0] != "2001:db8:2::/48" {
		t.Fatalf("deleted %v, want only the prefix that stopped being advertised", got)
	}
	if len(f.replace) != 0 {
		t.Errorf("the unchanged prefix was rewritten: %v", destinations(f.replace))
	}
}

// The diff is against what THIS MANAGER installed, never against the
// table. A container also carries routes Join copied off the host
// bridge and routes an operator added; deleting those would be this
// plugin taking away something nobody asked it to touch.
func TestReconcileAdvertisedRoutes_NeverTouchesARouteItDidNotInstall(t *testing.T) {
	m, _, f := v6Manager(t)
	f.routes = []netlink.Route{
		{Dst: cidr(t, "2001:db8:ff::/64"), Protocol: unixRTPROTBOOT},
	}
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v, want nothing: the manager installed none of them", destinations(f.deleted))
	}
}

// zonedNameserver: RFC 4007 section 11's scope zone on a link-local
// resolver, and NOTHING ELSE touched.
func TestZonedNameserver(t *testing.T) {
	for _, tc := range []struct{ in, iface, want string }{
		{"fe80::1", "eth0", "fe80::1%eth0"},
		{"fe80::1", "", "fe80::1"},
		{"fe80::1%eth9", "eth0", "fe80::1%eth9"},
		{"2001:db8::53", "eth0", "2001:db8::53"},
		{"192.0.2.53", "eth0", "192.0.2.53"},
		{"not-an-address", "eth0", "not-an-address"},
		{"fe80::1", "my-iface", "fe80::1%my-iface"},
	} {
		if got := zonedNameserver(tc.in, tc.iface); got != tc.want {
			t.Errorf("zonedNameserver(%q, %q) = %q, want %q", tc.in, tc.iface, got, tc.want)
		}
	}
}

// And it reaches the rendered file, which is what the container reads.
func TestBuildResolvConf_ZonesALinkLocalResolver(t *testing.T) {
	got := string(buildResolvConf([]string{"fe80::1", "2001:db8::53"}, nil, "", "eth0"))
	if !strings.Contains(got, "nameserver fe80::1%eth0") {
		t.Errorf("the link-local resolver lost its zone:\n%s", got)
	}
	if !strings.Contains(got, "nameserver 2001:db8::53\n") {
		t.Errorf("a global resolver was given a zone:\n%s", got)
	}
}

// v6AdvertisedRoutes: the two kinds of statement, and the precedence
// between them.
func TestV6AdvertisedRoutes(t *testing.T) {
	got := v6AdvertisedRoutes(dhcp.Info{
		OnLinkPrefixes: []string{"2001:db8::/64"},
		Routes: []dhcp.Route{
			{Destination: "2001:db8::/64", Gateway: "fe80::1"},
			{Destination: "2001:db8:1::/48", Gateway: "fe80::1"},
		},
	})
	if len(got) != 2 {
		t.Fatalf("got %v, want the on-link prefix and the one route that is not it", describeStaticRoutes(got))
	}
	if got[0].Destination != "2001:db8::/64" || got[0].RouteType != RouteTypeOnLink || got[0].NextHop != "" {
		t.Errorf("the on-link prefix did not stay on-link: %v", describeStaticRoutes(got[:1]))
	}
	if got[1].RouteType != RouteTypeNextHop || got[1].NextHop != "fe80::1" {
		t.Errorf("the Route Information option did not become a next-hop route: %v", describeStaticRoutes(got[1:]))
	}
}

// Nothing advertised is nil, so "no extra routes" and "no
// advertisement" are the same value at every reader.
func TestV6AdvertisedRoutes_EmptyIsNil(t *testing.T) {
	if got := v6AdvertisedRoutes(dhcp.Info{}); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// The v6 MTU is applied whether or not propagate_mtu is set, because
// until #821 the kernel applied it on every v6 network and an opt-in
// defaulting to false would have taken it away (#821).
//
// The v4 half is the preservation control: the option still governs
// that family, so this cannot pass against a propagateMTU that stopped
// reading the option at all.
func TestPropagateMTU_V6IsNotGatedOnTheOption(t *testing.T) {
	for _, tc := range []struct {
		name   string
		v6     bool
		opt    bool
		expect bool
	}{
		{"v6 without the option", true, false, true},
		{"v6 with the option", true, true, true},
		{"v4 without the option", false, false, false},
		{"v4 with the option", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			m := newDHCPManager(nil, JoinRequest{}, DHCPNetworkOptions{PropagateMTU: tc.opt}).withPlugin(p)
			// No link: the function returns before it could set an
			// MTU, and mtuRefused is the observable that says it got
			// past the gate. 68 is inside option 26's legal range and
			// outside the range this plugin will apply (#702).
			//
			// RouterSeen follows the family because it is not free to
			// choose: on v6 an MTU can only have come from an
			// advertisement, and a v4 client never sees one.
			m.propagateMTU(tc.v6, dhcp.Info{RouterSeen: tc.v6, MTU: 68})
			got := p.mtuRefused.Load() == 1
			if got != tc.expect {
				t.Errorf("reached the MTU decision = %v, want %v", got, tc.expect)
			}
		})
	}
}

// joinGuardErrors keeps both reasons, because they share one counter.
func TestJoinGuardErrors(t *testing.T) {
	a, b := errors.New("sysctl"), errors.New("purge")
	if got := joinGuardErrors(nil, nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if got := joinGuardErrors(a, nil); got != a {
		t.Errorf("got %v, want %v", got, a)
	}
	if got := joinGuardErrors(nil, b); got != b {
		t.Errorf("got %v, want %v", got, b)
	}
	got := joinGuardErrors(a, b)
	if !strings.Contains(got.Error(), "sysctl") || !strings.Contains(got.Error(), "purge") {
		t.Errorf("one of the two reasons was lost: %v", got)
	}
	if !errors.Is(got, b) {
		t.Errorf("the second error is not unwrappable from %v", got)
	}
}

// stubKernelRouteTable swaps the two package-level netlink calls the
// purge makes. Both need CAP_NET_ADMIN and a live namespace, so without
// the seam neither direction of this is reachable in the unit lane.
func stubKernelRouteTable(t *testing.T, routes []netlink.Route, listErr, delErr error) *[]netlink.Route {
	t.Helper()
	deleted := &[]netlink.Route{}
	prevList, prevDel := nlRouteListFiltered, nlRouteDel
	nlRouteListFiltered = func(int, *netlink.Route, uint64) ([]netlink.Route, error) {
		return routes, listErr
	}
	nlRouteDel = func(r *netlink.Route) error {
		if delErr != nil {
			return delErr
		}
		*deleted = append(*deleted, *r)
		return nil
	}
	t.Cleanup(func() { nlRouteListFiltered, nlRouteDel = prevList, prevDel })
	return deleted
}

// WRITING accept_ra=0 PURGES NOTHING: the routes an advertisement
// already installed stay until their own lifetime runs out, and RFC
// 4861 section 4.2 allows that to be 65535 seconds. The engine brings
// the link up at the kernel default before this guard runs, so the
// window is real. This is what closes it.
func TestPurgeRouterAdvertRoutes_RemovesWhatTheKernelInstalled(t *testing.T) {
	deleted := stubKernelRouteTable(t, []netlink.Route{
		{Dst: nil, Gw: net.ParseIP("fe80::1"), Protocol: 9 /* RTPROT_RA */},
		{Dst: cidr(t, "2001:db8::/64"), Protocol: 9},
	}, nil, nil)

	failed, err := purgeRouterAdvertRoutes(7)
	if err != nil || failed != 0 {
		t.Fatalf("failed=%d err=%v, want a clean purge", failed, err)
	}
	if len(*deleted) != 2 {
		t.Fatalf("deleted %v, want both kernel routes", destinations(*deleted))
	}
}

// One route that will not come off does not abandon the rest, and the
// default route is not reliably first in the list.
func TestPurgeRouterAdvertRoutes_CountsFailuresAndKeepsGoing(t *testing.T) {
	stubKernelRouteTable(t, []netlink.Route{
		{Dst: nil, Gw: net.ParseIP("fe80::1"), Protocol: 9},
		{Dst: cidr(t, "2001:db8::/64"), Protocol: 9},
	}, nil, errors.New("no such process"))

	failed, err := purgeRouterAdvertRoutes(7)
	if failed != 2 {
		t.Errorf("failed = %d, want one per route", failed)
	}
	if err == nil {
		t.Error("failures were counted with no reason to log")
	}
}

// A list that cannot be read is ONE failure and not zero. Zero would
// report a link whose kernel routes were never looked at as a link that
// had none.
func TestPurgeRouterAdvertRoutes_UnreadableListIsAFailure(t *testing.T) {
	stubKernelRouteTable(t, nil, errors.New("operation not permitted"), nil)
	failed, err := purgeRouterAdvertRoutes(7)
	if failed != 1 || err == nil {
		t.Fatalf("failed=%d err=%v, want one counted failure with a reason", failed, err)
	}
}

// A link the kernel never acted on costs nothing and reports nothing.
func TestPurgeRouterAdvertRoutes_NothingToPurge(t *testing.T) {
	deleted := stubKernelRouteTable(t, nil, nil, nil)
	failed, err := purgeRouterAdvertRoutes(7)
	if failed != 0 || err != nil || len(*deleted) != 0 {
		t.Fatalf("failed=%d err=%v deleted=%v, want a silent no-op", failed, err, *deleted)
	}
}

// THE ORDER IS THE CLAIM, and this is the only place it can be
// observed: the purge must run AFTER accept_ra=0 has taken, or the very
// next advertisement puts back what it just removed.
//
// The observer is the stub reading the sysctl tree at the moment it is
// called. A stub that only recorded "I was called" could not tell the
// two orders apart.
func TestPrepareV6LinkUnder_PurgesOnlyAfterTheGuardHasTaken(t *testing.T) {
	const iface = "eth0"
	dir := v6LinkSysctlDir(t, iface)

	acceptRAWhenPurged := "(never purged)"
	prevList, prevDel := nlRouteListFiltered, nlRouteDel
	nlRouteListFiltered = func(int, *netlink.Route, uint64) ([]netlink.Route, error) {
		acceptRAWhenPurged = v6LinkKnob(t, dir, iface, "accept_ra")
		return []netlink.Route{{Dst: nil, Gw: net.ParseIP("fe80::1"), Protocol: 9}}, nil
	}
	purged := 0
	nlRouteDel = func(*netlink.Route) error { purged++; return nil }
	t.Cleanup(func() { nlRouteListFiltered, nlRouteDel = prevList, prevDel })

	_, res, err := prepareV6LinkUnder(dir, iface, 7)
	if err != nil {
		t.Fatalf("prepareV6LinkUnder: %v", err)
	}
	if res.Failures != 0 {
		t.Errorf("guard reported %d failure(s) on a writable tree with a clean purge: %v",
			res.Failures, res.Err)
	}
	if purged != 1 {
		t.Fatalf("purged %d route(s), want the one the kernel had installed", purged)
	}
	want := dhcp.RouterAdvertGuardContract()["accept_ra"]
	if want == "" {
		t.Fatal("the contract has no accept_ra, so this test observes nothing")
	}
	if acceptRAWhenPurged != want {
		t.Errorf("accept_ra read %q at the moment of the purge, want %q. The purge ran "+
			"before the guard took, so the next advertisement reinstalls what it "+
			"removed (#821)", acceptRAWhenPurged, want)
	}
}

// A purge that fails is counted with the guard's own failures, because
// they are one obligation: the kernel must not be what decides this
// container's IPv6 route.
func TestPrepareV6LinkUnder_PurgeFailuresJoinTheGuardsCount(t *testing.T) {
	const iface = "eth0"
	dir := v6LinkSysctlDir(t, iface)
	stubKernelRouteTable(t, []netlink.Route{
		{Dst: nil, Gw: net.ParseIP("fe80::1"), Protocol: 9},
	}, nil, errors.New("no such process"))

	_, res, err := prepareV6LinkUnder(dir, iface, 7)
	if err != nil {
		t.Fatalf("prepareV6LinkUnder: %v", err)
	}
	if res.Failures != 1 {
		t.Errorf("Failures = %d, want the one failed deletion", res.Failures)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "router-advertisement route") {
		t.Errorf("the failure has no reason naming the purge: %v", res.Err)
	}
}

// A link that has not been located yet skips the purge rather than
// deleting routes on link index 0, which is not this container's link
// and may not be a link at all.
func TestPrepareV6LinkUnder_NoLinkIndexSkipsThePurge(t *testing.T) {
	const iface = "eth0"
	dir := v6LinkSysctlDir(t, iface)
	deleted := stubKernelRouteTable(t, []netlink.Route{
		{Dst: nil, Gw: net.ParseIP("fe80::1"), Protocol: 9},
	}, nil, nil)

	if _, _, err := prepareV6LinkUnder(dir, iface, 0); err != nil {
		t.Fatalf("prepareV6LinkUnder: %v", err)
	}
	if len(*deleted) != 0 {
		t.Errorf("deleted %v with no link located", destinations(*deleted))
	}
}

// fillV6Hint is the one place the IPv6 half of the Join hint is
// written, and both attach paths call it. These drive it directly.
func TestFillV6Hint(t *testing.T) {
	var h joinHint
	fillV6Hint(&h, dhcp.Info{
		Gateway:        "fe80::1",
		OnLinkPrefixes: []string{"2001:db8::/64"},
		Routes:         []dhcp.Route{{Destination: "2001:db8:1::/48", Gateway: "fe80::1"}},
	})
	if h.GatewayIPv6 != "fe80::1" {
		t.Errorf("GatewayIPv6 = %q, want the advertisement's link-local source", h.GatewayIPv6)
	}
	if len(h.RoutesIPv6) != 2 {
		t.Errorf("RoutesIPv6 = %v, want the on-link prefix and the advertised route",
			describeStaticRoutes(h.RoutesIPv6))
	}
	// The v4 half is untouched: the two families share a hint and a
	// fill that reached across would make one network's v4 gateway
	// decide its v6 route or the other way round.
	if h.Gateway != "" || h.Routes != nil {
		t.Errorf("the v6 fill wrote the v4 fields: gateway=%q routes=%v", h.Gateway, h.Routes)
	}
}

// A segment with no router leaves the hint empty, so Join emits no
// gateway rather than an empty-string one.
func TestFillV6Hint_NoRouterLeavesItEmpty(t *testing.T) {
	var h joinHint
	fillV6Hint(&h, dhcp.Info{})
	if h.GatewayIPv6 != "" || h.RoutesIPv6 != nil {
		t.Errorf("a segment with no advertisement produced gateway=%q routes=%v",
			h.GatewayIPv6, describeStaticRoutes(h.RoutesIPv6))
	}
}

// THE DISPATCH IS A CALL SITE, and a call site is a place the fix can
// be undone without touching the function it calls. Every other test
// here drives reconcileV6DefaultRoute directly, so a reconcileDefaultRoute
// whose v6 arm returned nil -- which is exactly what this branch's base
// does -- left all of them green. The event handler calls the v4-named
// function for both families, so this is the only observer of which body
// a v6 lease reaches.
//
// The v4 arm is the preservation control: a dispatch that sent both
// families into the v6 body would install an IPv6 route for an IPv4
// lease, and the v4 half of this test is what refuses it.
func TestReconcileDefaultRoute_DispatchesByFamily(t *testing.T) {
	t.Run("a v6 lease reaches the v6 reconciler", func(t *testing.T) {
		m, _, f := v6Manager(t)
		if err := m.reconcileDefaultRoute(true, dhcp.Info{Gateway: "fe80::1"}); err != nil {
			t.Fatalf("reconcileDefaultRoute: %v", err)
		}
		if len(f.added) != 1 {
			t.Fatalf("added %v, want the advertised IPv6 default route", destinations(f.added))
		}
		if got := f.added[0].Gw.String(); got != "fe80::1" {
			t.Errorf("gateway %q, want the advertisement's router", got)
		}
		if f.added[0].Dst != nil && f.added[0].Dst.IP.To4() != nil {
			t.Errorf("the v6 arm installed an IPv4 destination: %v", f.added[0].Dst)
		}
	})

	t.Run("a v4 lease does not", func(t *testing.T) {
		m, _, f := v6Manager(t)
		// The v4 body is skipped on an operator override, which is the
		// cheapest way to reach it with no kernel write and still prove
		// the v6 body was not the one entered: the v6 body has no such
		// gate and would have installed fe80::1 regardless.
		m.opts.Gateway = "192.168.100.1"
		if err := m.reconcileDefaultRoute(false, dhcp.Info{Gateway: "fe80::1"}); err != nil {
			t.Fatalf("reconcileDefaultRoute: %v", err)
		}
		if len(f.added)+len(f.replace)+len(f.deleted) != 0 {
			t.Errorf("a v4 lease with a gateway override wrote routes: added=%v replaced=%v deleted=%v",
				destinations(f.added), destinations(f.replace), destinations(f.deleted))
		}
	})
}

// THE FILTER IS THE DEFINITION OF THE POPULATION, so it is the thing to
// assert on. Every other purge test stubs the list call and hands back
// whatever it likes, which means none of them can tell a filter that
// names RTPROT_RA from one that names nothing: the kernel applies the
// filter, and a stub that ignores it reproduces the kernel's silence.
//
// What a widened filter would delete: the connected prefix route the
// kernel installs for the container's own address (RTPROT_KERNEL), and
// the routes this plugin installs from the Join answer (RTPROT_BOOT).
// Both are on the same link and both are IPv6, so the link index and
// the family are not enough on their own.
func TestPurgeRouterAdvertRoutes_AsksTheKernelOnlyForTheKernelsOwnRoutes(t *testing.T) {
	var gotFamily int
	var gotFilter netlink.Route
	var gotMask uint64
	prevList, prevDel := nlRouteListFiltered, nlRouteDel
	nlRouteListFiltered = func(family int, f *netlink.Route, mask uint64) ([]netlink.Route, error) {
		gotFamily, gotFilter, gotMask = family, *f, mask
		return nil, nil
	}
	nlRouteDel = func(*netlink.Route) error { return nil }
	t.Cleanup(func() { nlRouteListFiltered, nlRouteDel = prevList, prevDel })

	if _, err := purgeRouterAdvertRoutes(7); err != nil {
		t.Fatalf("purgeRouterAdvertRoutes: %v", err)
	}

	if gotFamily != unix.AF_INET6 {
		t.Errorf("family = %d, want AF_INET6 (%d): the v4 table is not this purge's business",
			gotFamily, unix.AF_INET6)
	}
	if gotFilter.LinkIndex != 7 {
		t.Errorf("LinkIndex = %d, want 7: an unscoped purge reaches every container "+
			"sharing this namespace", gotFilter.LinkIndex)
	}
	if gotFilter.Protocol != unix.RTPROT_RA {
		t.Errorf("Protocol = %d, want RTPROT_RA (%d). Anything else takes the "+
			"container's connected prefix route (RTPROT_KERNEL %d) and the routes "+
			"this plugin installed from the Join answer (RTPROT_BOOT %d) with it",
			gotFilter.Protocol, unix.RTPROT_RA, unix.RTPROT_KERNEL, unixRTPROTBOOT)
	}
	// A filter field the mask does not name is ignored by the kernel, so
	// the two halves have to be asserted together: a Protocol of
	// RTPROT_RA with RT_FILTER_PROTOCOL missing from the mask deletes
	// exactly as much as no protocol filter at all.
	if want := uint64(netlink.RT_FILTER_OIF | netlink.RT_FILTER_PROTOCOL); gotMask != want {
		t.Errorf("mask = %#x, want %#x: a filter field the mask does not name is "+
			"not applied", gotMask, want)
	}
}

// A route this plugin installs must not claim the kernel installed it.
//
// The replacement path reuses the route it found, and what it found may
// be one the kernel stamped RTPROT_RA before the guard ran. Carrying
// that stamp forward makes `ip -6 route` inside the container report
// "proto ra" for a route the kernel had nothing to do with, which is
// exactly the reading docs/reference.md asks an operator to make when
// there are two default routes.
func TestReconcileV6DefaultRoute_TheReplacementIsNotStampedAsTheKernels(t *testing.T) {
	m, _, f := v6Manager(t)
	f.routes = []netlink.Route{
		{Dst: nil, Gw: net.ParseIP("fe80::1"), Protocol: unix.RTPROT_RA},
	}

	if err := m.reconcileV6DefaultRoute(dhcp.Info{Gateway: "fe80::2"}); err != nil {
		t.Fatalf("reconcileV6DefaultRoute: %v", err)
	}
	if len(f.replace) != 1 {
		t.Fatalf("replaced %v, want the one default route", destinations(f.replace))
	}
	if f.replace[0].Protocol == unix.RTPROT_RA {
		t.Errorf("the replacement still carries RTPROT_RA (%d), so the container reports "+
			"a plugin route as the kernel's", unix.RTPROT_RA)
	}
	if got := f.replace[0].Gw.String(); got != "fe80::2" {
		t.Errorf("gateway %q, want the router the advertisement now names", got)
	}
}

// CASE, NOT RULE (#821 -> #818). A segment that offers no DHCPv6
// address leaves the Join answer's IPv6 half EMPTY, and this test pins
// that wrong-but-required answer.
//
// The right answer is the advertisement's gateway and routes: the
// #821 guard writes accept_ra=0, so the container's own kernel no
// longer installs the default route it used to install here. The
// answer is not reachable yet. An endpoint with no global IPv6 address
// has IPv6 disabled on its link by the engine, and the kernel then
// refuses every IPv6 route on it, so a Join answer carrying one fails
// the whole sandbox:
//
//	error setting interface "<host-if>" routes to ["fd00:...::/64"]: permission denied
//
// and no container starts on the segment at all, losing its IPv4 too
// (MEASURED, lane run 35131643324, four shards). The plugin cannot
// clear disable_ipv6 first: that clear runs in the manager goroutine
// Join spawns, after the engine has already applied the answer.
//
// #818 gives the container a global address; it is the change that
// makes this test change.
func TestNoteV6Absence_LeavesTheJoinAnswersIPv6HalfEmpty(t *testing.T) {
	p := &Plugin{joinHints: map[string]joinHint{}}
	// Seen, not Managed: the segment said there are no DHCPv6
	// addresses here, which is v6NotOffered and the normal case.
	// Mode6DHCP, and that is what keeps this case a case. #818 gives a
	// container a global address where the ipv6_mode FORMS one, which
	// is slaac and auto; an `ipv6=true` endpoint on a segment that
	// hands out no DHCPv6 address still has no global address, so the
	// engine still disables IPv6 on its link and the Join answer's
	// IPv6 half still has nowhere to go. The answer this pins changes
	// when the route becomes installable for THIS mode.
	ok := p.noteV6Absence(dhcp.RAObservation{Seen: true}, "eth0", "ep-1", errors.New("no lease"), proto.Mode6DHCP)
	if !ok {
		t.Fatal("the absence was treated as fatal; the endpoint would not have been created")
	}
	h := p.joinHints["ep-1"]
	if h.GatewayIPv6 != "" || h.RoutesIPv6 != nil {
		t.Errorf("the absence path put an IPv6 half into the Join answer "+
			"(gateway=%q routes=%v). The engine has disabled IPv6 on that link and "+
			"refuses it, and NO container starts on the segment",
			h.GatewayIPv6, describeStaticRoutes(h.RoutesIPv6))
	}
}

// ONE LINK, TWO FAMILIES, ONE NUMBER.
//
// Both families write the same link MTU. Without a rule for deciding
// between them the link flips on every renewal of either family and
// neither value is ever stable, and the "nothing to do" test cannot
// catch it because netlink caches the value it set.
func TestPropagateMTU_TheTwoFamiliesDoNotFightOverTheLink(t *testing.T) {
	m, _, _ := v6Manager(t)
	m.opts.PropagateMTU = true
	link := m.ctrLink.(*fakeLink)
	sets := 0
	prev := nlHandleLinkSetMTU
	nlHandleLinkSetMTU = func(_ *netlink.Handle, l netlink.Link, mtu int) error {
		sets++
		l.Attrs().MTU = mtu
		return nil
	}
	t.Cleanup(func() { nlHandleLinkSetMTU = prev })
	link.Attrs().MTU = 1500

	m.propagateMTU(false, dhcp.Info{MTU: 9000})
	if got := link.Attrs().MTU; got != 9000 {
		t.Fatalf("link MTU = %d after the v4 option alone, want 9000", got)
	}
	m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1400})
	if got := link.Attrs().MTU; got != 1400 {
		t.Fatalf("link MTU = %d, want the smaller of the two: 9000 is a promise the link "+
			"cannot keep for the family that asked for 1400", got)
	}

	// The renewal is where last-writer-wins shows itself: the v4 half
	// renews, supplies 9000 again, and must not take the link back.
	before := sets
	m.propagateMTU(false, dhcp.Info{MTU: 9000})
	if got := link.Attrs().MTU; got != 1400 {
		t.Errorf("a v4 renewal moved the link back to %d; the link flips once per renewal "+
			"of either family and neither value is ever stable", got)
	}
	if sets != before {
		t.Errorf("the v4 renewal wrote the link MTU %d extra time(s) with nothing to change",
			sets-before)
	}

	// A family that stops supplying one does not keep voting: the v6
	// half going to zero leaves the v4 number.
	m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1500})
	if got := link.Attrs().MTU; got != 1500 {
		t.Errorf("link MTU = %d after the advertisement raised its own value, want 1500", got)
	}
}

// A route installed at Join can be withdrawn.
//
// The diff base starts empty and Docker installs the Join answer's
// routes before this manager exists, so without a seed the first
// advertisement that drops one of them diffs against nothing, finds
// nothing to withdraw, and leaves the container routing over a prefix
// the segment stopped offering. That is the ordinary case: the first
// change to any route is a change to a route installed at Join.
func TestRenew_SeedsTheAdvertisedRouteDiffBase(t *testing.T) {
	m, _, f := v6Manager(t)
	prevMTU, prevAddr := nlHandleLinkSetMTU, nlHandleAddrReplace
	nlHandleLinkSetMTU = func(*netlink.Handle, netlink.Link, int) error { return nil }
	nlHandleAddrReplace = func(*netlink.Handle, netlink.Link, *netlink.Addr) error { return nil }
	t.Cleanup(func() { nlHandleLinkSetMTU, nlHandleAddrReplace = prevMTU, prevAddr })

	bound := dhcp.Info{
		IP:      "2001:db8::5/64",
		Gateway: "fe80::1",
		Routes:  []dhcp.Route{{Destination: "2001:db8:1::/48", Gateway: "fe80::1"}},
	}
	if err := m.renew(true, bound); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if got := m.lastAdvertRoutes["2001:db8:1::/48"]; got != "fe80::1" {
		t.Fatalf("the diff base did not record the Join-installed route: %v", m.lastAdvertRoutes)
	}

	// The segment stops offering it.
	f.deleted = nil
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("reconcileAdvertisedRoutes: %v", err)
	}
	if got := destinations(f.deleted); len(got) != 1 || got[0] != "2001:db8:1::/48" {
		t.Errorf("deleted %v, want the withdrawn route. A route installed at Join can "+
			"never be withdrawn if the diff base is not seeded", got)
	}
}

// A FAMILY THAT STOPS SUPPLYING AN MTU STOPS VOTING.
//
// propagateMTU used to return on a zero before it recorded anything, so
// a router that dropped its MTU option left its old number in place for
// the life of the endpoint and the link stayed clamped to a value
// nothing on the segment was asking for. The change does reach the
// manager -- advertisedDiffers compares MTU, so the routeradvert event
// fires -- and it was discarded at the door.
func TestPropagateMTU_AWithdrawnMTUStopsVoting(t *testing.T) {
	newManager := func(t *testing.T) (*dhcpManager, *fakeLink) {
		t.Helper()
		m, _, _ := v6Manager(t)
		m.opts.PropagateMTU = true
		link := m.ctrLink.(*fakeLink)
		link.Attrs().MTU = 1500
		prev := nlHandleLinkSetMTU
		nlHandleLinkSetMTU = func(_ *netlink.Handle, l netlink.Link, mtu int) error {
			l.Attrs().MTU = mtu
			return nil
		}
		t.Cleanup(func() { nlHandleLinkSetMTU = prev })
		return m, link
	}

	t.Run("the other family's value takes over", func(t *testing.T) {
		m, link := newManager(t)
		m.propagateMTU(false, dhcp.Info{MTU: 9000})
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1400})
		if got := link.Attrs().MTU; got != 1400 {
			t.Fatalf("link MTU = %d before the withdrawal, want the smaller of the two", got)
		}
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 0})
		if got := link.Attrs().MTU; got != 9000 {
			t.Errorf("link MTU = %d after the router dropped its MTU option, want the v4 "+
				"value 9000. A withdrawn vote that is never cleared keeps the link "+
				"clamped to a number nothing on the segment asks for", got)
		}
	})

	t.Run("both silent: the link goes back to what Docker gave it", func(t *testing.T) {
		m, link := newManager(t)
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1400})
		if got := link.Attrs().MTU; got != 1400 {
			t.Fatalf("link MTU = %d, want 1400", got)
		}
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 0})
		if got := link.Attrs().MTU; got != 1500 {
			t.Errorf("link MTU = %d with nothing supplying one, want the 1500 the link "+
				"had before this manager touched it", got)
		}
	})

	// PRESERVATION CONTROL, and the distinction the fix rests on: a
	// REFUSED value is not a withdrawal. "The server said something
	// impossible" leaves the previous vote standing; only "the server
	// stopped asking" clears it.
	t.Run("a refused value is not a withdrawal", func(t *testing.T) {
		m, link := newManager(t)
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1400})
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 68})
		if got := link.Attrs().MTU; got != 1400 {
			t.Errorf("link MTU = %d after a refused 68, want the last accepted 1400", got)
		}
		if m.plugin.mtuRefused.Load() != 1 {
			t.Errorf("mtu_refused = %d, want 1", m.plugin.mtuRefused.Load())
		}
	})

	// THE CONTROL THAT BOUNDS THE WITHDRAWAL, and the reason every v6
	// fixture above carries RouterSeen: a zero from an event stamped
	// BEFORE the first advertisement is silence, not a withdrawal.
	//
	// RFC 9915 section 18.2.1's Solicit goes out without waiting for
	// router discovery, so the first bound event -- the one that
	// carries the MTU -- can be stamped on a link whose router has not
	// spoken yet, and the library documents its observation that way:
	// "the zero value means it had seen none WHEN THIS EVENT WAS
	// STAMPED". Treating that as a withdrawal drops the v6 vote, lets
	// the v4 number take the link, and hands it back when the next
	// event carries the advertisement -- the same flip the
	// smaller-of-two rule exists to prevent, through the other door.
	//
	// The assertion is across BOTH transitions and not only the final
	// value, because a test that reads the link once at the end passes
	// on a link that flapped.
	t.Run("an event stamped before the first advertisement does not withdraw", func(t *testing.T) {
		m, link := newManager(t)
		m.opts.PropagateMTU = true
		m.propagateMTU(false, dhcp.Info{MTU: 9000})
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1400})
		if got := link.Attrs().MTU; got != 1400 {
			t.Fatalf("link MTU = %d, want the smaller 1400", got)
		}

		// The early-stamped event: no router seen yet, so no MTU.
		m.propagateMTU(true, dhcp.Info{MTU: 0})
		if got := link.Attrs().MTU; got != 1400 {
			t.Errorf("link MTU = %d after an event stamped before the first "+
				"advertisement, want the advertised 1400 still. The router had not "+
				"spoken yet; it had not stopped speaking", got)
		}

		// And the genuine withdrawal still is one.
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 0})
		if got := link.Attrs().MTU; got != 9000 {
			t.Errorf("link MTU = %d after the router advertised without an MTU option, "+
				"want the v4 value 9000: that one IS a withdrawal", got)
		}
	})

	// A family the operator switched off never votes, so it cannot
	// withdraw the other family's value either.
	t.Run("propagate_mtu off: the v4 half cannot withdraw the v6 value", func(t *testing.T) {
		m, link := newManager(t)
		m.opts.PropagateMTU = false
		m.propagateMTU(true, dhcp.Info{RouterSeen: true, MTU: 1400})
		m.propagateMTU(false, dhcp.Info{MTU: 0})
		if got := link.Attrs().MTU; got != 1400 {
			t.Errorf("link MTU = %d, want 1400: a family gated off by the operator has "+
				"no vote to cast and none to withdraw", got)
		}
	})
}
