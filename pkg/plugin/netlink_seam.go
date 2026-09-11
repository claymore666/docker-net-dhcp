// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "github.com/vishvananda/netlink"

// Indirection over the package-level netlink calls the plugin makes, so
// unit tests can inject failures or synthetic results for error paths
// that otherwise require CAP_NET_ADMIN and a live network namespace
// (and so are only reachable via the integration suite). Production code
// calls these vars; tests swap them out and restore in t.Cleanup. No
// behavioural change — each var is just the netlink function it names.
var (
	nlLinkByName        = netlink.LinkByName
	nlLinkDel           = netlink.LinkDel
	nlLinkSetUp         = netlink.LinkSetUp
	nlAddrList          = netlink.AddrList
	nlRouteListFiltered = netlink.RouteListFiltered
	nlLinkList          = netlink.LinkList
)

// nlAddrDel is the netns-handle-scoped AddrDel, as a seam.
//
// It is a function value and not a bare method reference because the
// handle is a per-endpoint object rather than a package-level one. The
// release path is the only caller (#962): a DHCPv6 address MUST be off
// the interface before the Release exchange begins (RFC 9915 section
// 18.2.7), and "the removal failed, so nothing was sent" is a decision
// no unit test could otherwise observe -- it needs CAP_NET_ADMIN and a
// live netns to reach.
//
// The renewal path's own AddrDel calls are deliberately left on the
// handle. They already have their observers, and routing them through
// here would be a change to running code for a test that exists.
var nlAddrDel = func(h *netlink.Handle, link netlink.Link, addr *netlink.Addr) error {
	return h.AddrDel(link, addr)
}

// linkLister is the subset of *netlink.Handle that findLinkByMAC needs.
// Taking the interface (not the concrete handle) lets the MAC-walk logic
// be unit-tested without a live netns handle; *netlink.Handle satisfies
// it as-is.
type linkLister interface {
	LinkList() ([]netlink.Link, error)
}
