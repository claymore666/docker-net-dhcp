// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// failFirst makes the first call through a route seam fail and passes the rest to the fake table, counting calls.
func failFirst(t *testing.T, seam *func(*netlink.Handle, *netlink.Route) error, failWith error) *int {
	t.Helper()
	calls := 0
	prev := *seam
	*seam = func(h *netlink.Handle, r *netlink.Route) error {
		calls++
		if calls == 1 {
			return failWith
		}
		return prev(h, r)
	}
	t.Cleanup(func() { *seam = prev })
	return &calls
}

func TestReconcileAdvertisedRoutes_AFailedAddIsRetried(t *testing.T) {
	m, _, f := v6Manager(t)
	calls := failFirst(t, &nlHandleRouteReplace, errors.New("transient"))
	info := dhcp.Info{Gateway: "fe80::1", Routes: []dhcp.Route{{Destination: "2001:db8:1::/48", Gateway: "fe80::1"}}}
	if err := m.reconcileAdvertisedRoutes(info); err == nil {
		t.Fatalf("the first reconcile did not report the failed write")
	}
	if err := m.reconcileAdvertisedRoutes(info); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if *calls != 2 || len(f.replace) != 1 || destinations(f.replace)[0] != "2001:db8:1::/48" {
		t.Errorf("RouteReplace called %d time(s), the table got %v: the failed route was never retried (record %v)",
			*calls, destinations(f.replace), m.lastAdvertRoutes)
	}
}

func TestReconcileAdvertisedRoutes_AFailedWithdrawalIsRetried(t *testing.T) {
	m, _, f := v6Manager(t)
	m.lastAdvertRoutes = map[string]string{"2001:db8:1::/48": "fe80::1"}
	calls := failFirst(t, &nlHandleRouteDel, errors.New("transient"))
	info := dhcp.Info{Gateway: "fe80::1"}
	if err := m.reconcileAdvertisedRoutes(info); err == nil {
		t.Fatalf("the first reconcile did not report the failed delete")
	}
	if err := m.reconcileAdvertisedRoutes(info); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if *calls != 2 || len(f.deleted) != 1 || destinations(f.deleted)[0] != "2001:db8:1::/48" {
		t.Errorf("RouteDel called %d time(s), the table lost %v: the withdrawn route stays in the container",
			*calls, destinations(f.deleted))
	}
	if len(m.lastAdvertRoutes) != 0 {
		t.Errorf("the deleted route is still recorded: %v", m.lastAdvertRoutes)
	}
}

func TestReconcileAdvertisedRoutes_AWithdrawnRouteAlreadyGoneIsDroppedOnce(t *testing.T) {
	m, _, _ := v6Manager(t)
	m.lastAdvertRoutes = map[string]string{"2001:db8:1::/48": "fe80::1"}
	calls := failFirst(t, &nlHandleRouteDel, unix.ESRCH)
	info := dhcp.Info{Gateway: "fe80::1"}
	if err := m.reconcileAdvertisedRoutes(info); err != nil {
		t.Errorf("a route already gone from the kernel was reported as a failure: %v", err)
	}
	if err := m.reconcileAdvertisedRoutes(info); err != nil {
		t.Errorf("second reconcile: %v", err)
	}
	if *calls != 1 || len(m.lastAdvertRoutes) != 0 {
		t.Errorf("RouteDel called %d time(s), record %v: a route the kernel no longer has is retried",
			*calls, m.lastAdvertRoutes)
	}
}

// joinManagerWithRoutes builds the manager Join registers for an answer holding an advertised route, a host route
// (#102), and an advertised route that lost its destination to a host route in uniqueStaticRoutes.
func joinManagerWithRoutes(t *testing.T, opts DHCPNetworkOptions) (*dhcpManager, *fakeRouteTable) {
	t.Helper()
	adv := &StaticRoute{Destination: "2001:db8:1::/48", RouteType: RouteTypeNextHop, NextHop: "fe80::1"}
	shadowed := &StaticRoute{Destination: "2001:db8:5::/48", RouteType: RouteTypeNextHop, NextHop: "fe80::1"}
	host := &StaticRoute{Destination: "2001:db8:77::/48", RouteType: RouteTypeNextHop, NextHop: "fe80::1"}
	hostSameDest := &StaticRoute{Destination: "2001:db8:5::/48", RouteType: RouteTypeNextHop, NextHop: "fe80::99"}
	hint := joinHint{RoutesIPv6: []*StaticRoute{adv, shadowed}}
	res := JoinResponse{StaticRoutes: []*StaticRoute{host, hostSameDest, adv}}
	m := (&Plugin{}).newJoinManager(JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"}, opts, hint, res)
	f := &fakeRouteTable{}
	f.install(t, m)
	return m, f
}

func TestNewJoinManager_AnAdvertisedRouteJoinInstalledIsWithdrawn(t *testing.T) {
	m, f := joinManagerWithRoutes(t, DHCPNetworkOptions{IPv6: true})
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := destinations(f.deleted); len(got) != 1 || got[0] != "2001:db8:1::/48" {
		t.Errorf("deleted %v, want only the advertised route Join installed; host routes (#102) stay", got)
	}
}

func TestNewJoinManager_SkipRoutesRecordsNothing(t *testing.T) {
	m, f := joinManagerWithRoutes(t, DHCPNetworkOptions{IPv6: true, SkipRoutes: true})
	m.opts.SkipRoutes = false
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v: a skip_routes Join recorded routes", destinations(f.deleted))
	}
}

func TestReconcileAdvertisedRoutes_NoRouterHeardYetWithdrawsNothing(t *testing.T) {
	m, f := joinManagerWithRoutes(t, DHCPNetworkOptions{IPv6: true})
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v before any advertisement was heard; a Solicit does not wait for router discovery "+
			"(RFC 9915 section 18.2.1)", destinations(f.deleted))
	}
}

// v6LinkRoute is a route on the container link as the kernel lists it.
func v6LinkRoute(t *testing.T, dst, gw string, proto netlink.RouteProtocol) netlink.Route {
	return netlink.Route{Dst: cidr(t, dst), Gw: net.ParseIP(gw), Protocol: proto}
}

// recoveredWithHostRoutes builds a recovered manager on a macvlan network whose parent holds hostRoutes.
func recoveredWithHostRoutes(t *testing.T, opts DHCPNetworkOptions, hostRoutes []netlink.Route, hostErr error) (*dhcpManager, *fakeRouteTable) {
	t.Helper()
	prevLink, prevList := nlLinkByName, nlRouteListFiltered
	nlLinkByName = func(name string) (netlink.Link, error) {
		if name != "eth9" {
			return nil, errors.New("no such link")
		}
		return &fakeLink{attrs: netlink.LinkAttrs{Name: name, Index: 9}}, nil
	}
	nlRouteListFiltered = func(_ int, filter *netlink.Route, _ uint64) ([]netlink.Route, error) {
		if filter == nil || filter.LinkIndex != 9 {
			t.Errorf("host routes read from link %+v, want the parent", filter)
		}
		return hostRoutes, hostErr
	}
	t.Cleanup(func() { nlLinkByName, nlRouteListFiltered = prevLink, prevList })
	opts.Mode, opts.Parent = ModeMacvlan, "eth9"
	ipv6, _ := netlink.ParseAddr("2001:db8::5/64")
	m := (&Plugin{}).recoveredManager("net-1", "ep-1", net.HardwareAddr{2, 0, 0, 0, 0, 1}, nil, ipv6, opts)
	f := &fakeRouteTable{}
	f.install(t, m)
	f.routes = []netlink.Route{
		defaultV6Route("fe80::1"),
		v6LinkRoute(t, "2001:db8:1::/48", "fe80::1", unixRTPROTBOOT),
		v6LinkRoute(t, "2001:db8:77::/48", "fe80::1", unixRTPROTBOOT),
		v6LinkRoute(t, "2001:db8:88::/48", "2001:db8::1", unixRTPROTBOOT),
		v6LinkRoute(t, "2001:db8:99::/48", "fe80::1", unix.RTPROT_RA),
		v6LinkRoute(t, "2001:db8::/64", "fe80::1", unix.RTPROT_KERNEL),
	}
	return m, f
}

var parentHostRoutes = []netlink.Route{
	{Dst: nil, Gw: net.ParseIP("fe80::1")},
	{Dst: &net.IPNet{IP: net.ParseIP("2001:db8:77::"), Mask: net.CIDRMask(48, 128)}, Gw: net.ParseIP("fe80::1")},
}

func TestRecoveredManager_WithdrawsOnlyTheAdvertisedRouteOnTheLink(t *testing.T) {
	m, f := recoveredWithHostRoutes(t, DHCPNetworkOptions{IPv6: true}, parentHostRoutes, nil)
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := destinations(f.deleted); len(got) != 1 || got[0] != "2001:db8:1::/48" {
		t.Errorf("deleted %v, want only the route through the router: a host route (#102), a global next hop, "+
			"the kernel's and a kernel RA's stay", got)
	}
}

func TestRecoveredManager_AdoptsNothingBeforeARouterIsHeard(t *testing.T) {
	m, f := recoveredWithHostRoutes(t, DHCPNetworkOptions{IPv6: true}, parentHostRoutes, nil)
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 || len(m.lastAdvertRoutes) != 0 {
		t.Errorf("deleted %v, record %v before any advertisement was heard", destinations(f.deleted), m.lastAdvertRoutes)
	}
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Gateway: "fe80::1", Routes: []dhcp.Route{
		{Destination: "2001:db8:1::/48", Gateway: "fe80::1"},
	}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 || len(f.replace) != 0 || m.lastAdvertRoutes["2001:db8:1::/48"] != "fe80::1" {
		t.Errorf("deleted %v, rewrote %v, record %v: the advertised route on the link was not adopted as is",
			destinations(f.deleted), destinations(f.replace), m.lastAdvertRoutes)
	}
}

func TestRecoveredManager_AnUnreadableHostTableAdoptsNothing(t *testing.T) {
	m, f := recoveredWithHostRoutes(t, DHCPNetworkOptions{IPv6: true}, nil, errors.New("dump interrupted"))
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v without knowing which routes Join copied from the host", destinations(f.deleted))
	}
}

func TestRecoveredManager_SkipRoutesAdoptsNothing(t *testing.T) {
	m, f := recoveredWithHostRoutes(t, DHCPNetworkOptions{IPv6: true, SkipRoutes: true}, parentHostRoutes, nil)
	m.opts.SkipRoutes = false
	if err := m.reconcileAdvertisedRoutes(dhcp.Info{Gateway: "fe80::1"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted %v on a skip_routes network", destinations(f.deleted))
	}
}
