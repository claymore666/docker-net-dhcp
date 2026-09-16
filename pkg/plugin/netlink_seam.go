// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

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

	// The two halves of the #978 rename, and they are seams for the
	// same reason nlLinkByIndex is: renaming a link needs CAP_NET_ADMIN
	// and a live namespace, so nothing root-free reaches the arm where
	// the kernel refuses one.
	nlLinkSetName    = netlink.LinkSetName
	nlLinkAddAltName = netlink.LinkAddAltName

	// nlRouteDel deletes one route in the CALLER'S CURRENT network
	// namespace, which is why it is the package-level call and not a
	// handle method: its one caller (purgeRouterAdvertRoutes) has
	// already entered the sandbox namespace on a locked thread, and a
	// handle built elsewhere would name the wrong link. Deleting a
	// route needs CAP_NET_ADMIN, so without the seam neither the
	// success nor the failure path is reachable root-free.
	nlRouteDel = netlink.RouteDel

	// nlNewHandleAt is the one that needs CAP_SYS_ADMIN even for the
	// caller's OWN namespace: it setns()es to build the socket. Without
	// the seam, nothing root-free can reach a single line of Start past
	// the namespace open, which is where #417 moved the work that
	// matters.
	nlNewHandleAt = netlink.NewHandleAt

	// nlLinkByIndex re-reads a link the manager already holds. It is a
	// closure and not a method value because the handle is per-attach,
	// and a var because the rename it exists to survive cannot be
	// produced root-free: renaming a link needs CAP_NET_ADMIN.
	nlLinkByIndex = func(h *netlink.Handle, index int) (netlink.Link, error) {
		return h.LinkByIndex(index)
	}
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

// The handle-scoped route calls the IPv6 advertisement path makes, as
// seams.
//
// WHY THESE AND NOT THE v4 SIBLINGS. Adding, replacing and deleting a
// route all need CAP_NET_ADMIN and a live namespace, so root-free
// nothing past the first call is reachable. The v4 default-route
// reconciler has lived with that since before this file existed and its
// behaviour is pinned by the integration suite; routing it through here
// would be a change to running code for a test that already exists. The
// IPv6 path arrived with #821 and has three decisions the integration
// suite cannot separate -- install, replace, and the WITHDRAWAL that
// moves ipv6_router_withdrawn -- so it gets the seam.
//
// Each var is the handle method it names and nothing else.
var (
	// The dump goes through util.DumpResult HERE and not at the call
	// site, so the seam cannot be swapped for one that drops
	// ErrDumpInterrupted on the floor (#802).
	nlHandleRouteListFiltered = func(h *netlink.Handle, family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
		return util.DumpResult(h.RouteListFiltered(family, filter, mask))
	}
	nlHandleRouteAdd = func(h *netlink.Handle, r *netlink.Route) error {
		return h.RouteAdd(r)
	}
	nlHandleRouteDel = func(h *netlink.Handle, r *netlink.Route) error {
		return h.RouteDel(r)
	}
	nlHandleRouteReplace = func(h *netlink.Handle, r *netlink.Route) error {
		return h.RouteReplace(r)
	}

	// The link MTU, for the same reason as the routes above: #821 made
	// the two families share this one write, and "which number wins
	// when both supplied one" is a decision no test could reach while
	// it needed CAP_NET_ADMIN and a live link.
	nlHandleLinkSetMTU = func(h *netlink.Handle, l netlink.Link, mtu int) error {
		return h.LinkSetMTU(l, mtu)
	}

	// The address write, for one reason only: renew() applies the
	// address BEFORE it touches routes (the order the kernel needs),
	// so without this no unit test can reach anything renew does after
	// it. The decisions past this line are #821's, and one of them --
	// seeding the route diff base from the bound lease -- is the whole
	// of whether a route installed at Join can ever be withdrawn.
	nlHandleAddrReplace = func(h *netlink.Handle, l netlink.Link, a *netlink.Addr) error {
		return h.AddrReplace(l, a)
	}
)
