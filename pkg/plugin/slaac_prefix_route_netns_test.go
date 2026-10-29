// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	onLinkPrefix  = "fd00:1412:1::/64"
	onLinkAddr    = "fd00:1412:1::61/64"
	offLinkPrefix = "fd00:1412:2::/64"
	offLinkAddr   = "fd00:1412:2::61/64"
)

// engineInstallV6 is what the engine leaves after Join: the reported address with forever lifetimes and NODAD, and
// each on-link StaticRoute of the Join answer as a gatewayless route (#1412).
func (l renumberLink) engineInstallV6(t *testing.T, addr string, onLink ...string) {
	t.Helper()
	a, err := netlink.ParseAddr(addr)
	if err != nil {
		t.Fatalf("ParseAddr %s: %v", addr, err)
	}
	a.Flags |= unix.IFA_F_NODAD
	if err := l.h.AddrAdd(l.link, a); err != nil {
		t.Fatalf("AddrAdd %s: %v", addr, err)
	}
	for _, p := range onLink {
		_, dst, _ := net.ParseCIDR(p)
		if err := l.h.RouteAdd(&netlink.Route{LinkIndex: l.link.Attrs().Index, Dst: dst}); err != nil {
			t.Fatalf("RouteAdd %s: %v", p, err)
		}
	}
}

// routesTo lists the link's main-table IPv6 routes to dst, read from the kernel.
func (l renumberLink) routesTo(t *testing.T, dst string) []netlink.Route {
	t.Helper()
	list, err := util.DumpResult(l.h.RouteListFiltered(netlink.FAMILY_V6,
		&netlink.Route{LinkIndex: l.link.Attrs().Index, Table: unix.RT_TABLE_MAIN},
		netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE))
	if err != nil {
		t.Fatalf("RouteListFiltered: %v", err)
	}
	var out []netlink.Route
	for _, r := range list {
		if r.Dst != nil && r.Dst.String() == dst {
			out = append(out, r)
		}
	}
	return out
}

func (l renumberLink) addrFlags(t *testing.T, addr string) int {
	t.Helper()
	list, err := util.DumpResult(l.h.AddrList(l.link, netlink.FAMILY_V6))
	if err != nil {
		t.Fatalf("AddrList: %v", err)
	}
	for _, a := range list {
		if a.IPNet.String() == addr {
			return a.Flags
		}
	}
	t.Fatalf("%s is not on the link", addr)
	return 0
}

func slaacJoinManager(l renumberLink, main string, opts DHCPNetworkOptions) *dhcpManager {
	a, _ := netlink.ParseAddr(main)
	m := (&Plugin{}).newJoinManager(JoinRequest{}, opts, joinHint{IPv6: a}, JoinResponse{})
	m.netHandle, m.ctrLink = l.h, l.link
	return m
}

func TestSLAACPrefix_IsOnLinkOnlyByTheLFlagAndGoesWithValidLifetimeZero(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	l := newRenumberLink(t, 0, "", "")
	l.engineInstallV6(t, onLinkAddr, onLinkPrefix)
	if got := l.routesTo(t, onLinkPrefix); len(got) != 2 {
		t.Fatalf("the engine's install left %d route(s) to %s, want the kernel's and the Join answer's: %v",
			len(got), onLinkPrefix, got)
	}
	m := slaacJoinManager(l, onLinkAddr, DHCPNetworkOptions{})

	m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{
		IP: onLinkAddr, SLAAC: true, LeaseSeconds: 3600, PreferredSeconds: 1800,
		Gateway: "fe80::1", RouterSeen: true,
		Addrs: []dhcp.V6Addr{
			{IP: onLinkAddr, ValidSeconds: 3600, PreferredSeconds: 1800},
			{IP: offLinkAddr, ValidSeconds: 3600, PreferredSeconds: 1800},
		},
		OnLinkPrefixes: []string{onLinkPrefix},
	}}, true)

	for _, addr := range []string{onLinkAddr, offLinkAddr} {
		if l.addrFlags(t, addr)&unix.IFA_F_NOPREFIXROUTE == 0 {
			t.Errorf("%s is on the link without noprefixroute", addr)
		}
	}
	got := l.routesTo(t, onLinkPrefix)
	if len(got) != 1 || got[0].Protocol == unix.RTPROT_KERNEL || got[0].Gw != nil {
		t.Errorf("routes to the L=1 prefix %s = %v, want exactly the gatewayless Join route, no proto kernel one",
			onLinkPrefix, got)
	}
	if got := l.routesTo(t, offLinkPrefix); len(got) != 0 {
		t.Errorf("the L=0 prefix %s has route(s) %v, want none (RFC 5942 section 4)", offLinkPrefix, got)
	}

	m.handleEvent(dhcp.Event{Type: "routeradvert", Data: dhcp.Info{
		Gateway: "fe80::1", RouterSeen: true, WithdrawnOnLinkPrefixes: []string{onLinkPrefix},
	}}, true)

	if got := l.routesTo(t, onLinkPrefix); len(got) != 0 {
		t.Errorf("routes to %s after a Valid Lifetime 0 = %v, want none (RFC 4861 section 6.3.4)", onLinkPrefix, got)
	}
}

func TestDHCPv6Address_KeepsTheKernelRouteOfItsOwn(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	const granted, prefix = "fd00:1412:3::61/128", "fd00:1412:3::/64"
	l := newRenumberLink(t, 0, "", "")
	l.engineInstallV6(t, granted, prefix)
	m := slaacJoinManager(l, granted, DHCPNetworkOptions{})

	m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{
		IP: granted, LeaseSeconds: 3600, PreferredSeconds: 1800, Gateway: "fe80::1", RouterSeen: true,
		Addrs:          []dhcp.V6Addr{{IP: granted, ValidSeconds: 3600, PreferredSeconds: 1800}},
		OnLinkPrefixes: []string{prefix},
	}}, true)

	if l.addrFlags(t, granted)&unix.IFA_F_NOPREFIXROUTE != 0 {
		t.Errorf("the granted address %s carries noprefixroute", granted)
	}
	if got := l.routesTo(t, granted); len(got) != 1 || got[0].Protocol != unix.RTPROT_KERNEL {
		t.Errorf("routes to %s = %v, want the kernel's own", granted, got)
	}
	if got := l.routesTo(t, prefix); len(got) != 1 {
		t.Errorf("routes to the on-link prefix %s = %v, want the Join route", prefix, got)
	}
}

func TestSLAACPrefix_SkipRoutesLeavesItOffLink(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	l := newRenumberLink(t, 0, "", "")
	l.engineInstallV6(t, onLinkAddr)
	m := slaacJoinManager(l, onLinkAddr, DHCPNetworkOptions{SkipRoutes: true})

	m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{
		IP: onLinkAddr, SLAAC: true, LeaseSeconds: 3600, Gateway: "fe80::1", RouterSeen: true,
		Addrs:          []dhcp.V6Addr{{IP: onLinkAddr, ValidSeconds: 3600}},
		OnLinkPrefixes: []string{onLinkPrefix},
	}}, true)

	if got := l.routesTo(t, onLinkPrefix); len(got) != 0 {
		t.Errorf("skip_routes=true and the formed prefix %s has route(s) %v, want none", onLinkPrefix, got)
	}
}

func TestSLAACPrefix_TheDeprecatedRenewalOfAValidLifetimeZeroLeavesNoRoute(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	l := newRenumberLink(t, 0, "", "")
	l.engineInstallV6(t, onLinkAddr, onLinkPrefix)
	m := slaacJoinManager(l, onLinkAddr, DHCPNetworkOptions{})
	m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{
		IP: onLinkAddr, SLAAC: true, LeaseSeconds: 3600, PreferredSeconds: 1800, Gateway: "fe80::1", RouterSeen: true,
		Addrs:          []dhcp.V6Addr{{IP: onLinkAddr, ValidSeconds: 3600, PreferredSeconds: 1800}},
		OnLinkPrefixes: []string{onLinkPrefix},
	}}, true)

	m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{
		IP: onLinkAddr, SLAAC: true, LeaseSeconds: 7200, IPDeprecated: true, Gateway: "fe80::1", RouterSeen: true,
		Addrs:                   []dhcp.V6Addr{{IP: onLinkAddr, ValidSeconds: 7200, Deprecated: true}},
		WithdrawnOnLinkPrefixes: []string{onLinkPrefix},
	}}, true)

	if l.addrFlags(t, onLinkAddr)&unix.IFA_F_NOPREFIXROUTE == 0 {
		t.Errorf("the deprecated address %s lost noprefixroute", onLinkAddr)
	}
	if got := l.routesTo(t, onLinkPrefix); len(got) != 0 {
		t.Errorf("routes to %s with its address kept deprecated under the two-hour rule = %v, want none "+
			"(RFC 4862 section 5.5.3 e, RFC 4861 section 6.3.4)", onLinkPrefix, got)
	}
}

func TestSLAACPrefix_AnInfiniteValidLifetimeAlsoLeavesNoKernelRoute(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	l := newRenumberLink(t, 0, "", "")
	l.engineInstallV6(t, onLinkAddr, onLinkPrefix)
	m := slaacJoinManager(l, onLinkAddr, DHCPNetworkOptions{})

	m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{
		IP: onLinkAddr, SLAAC: true, Gateway: "fe80::1", RouterSeen: true,
		Addrs:          []dhcp.V6Addr{{IP: onLinkAddr}},
		OnLinkPrefixes: []string{onLinkPrefix},
	}}, true)

	if l.addrFlags(t, onLinkAddr)&unix.IFA_F_NOPREFIXROUTE == 0 {
		t.Errorf("%s with an infinite valid lifetime is on the link without noprefixroute", onLinkAddr)
	}
	got := l.routesTo(t, onLinkPrefix)
	if len(got) != 1 || got[0].Protocol == unix.RTPROT_KERNEL {
		t.Errorf("routes to %s = %v, want exactly the gatewayless Join route, no proto kernel one", onLinkPrefix, got)
	}
}
