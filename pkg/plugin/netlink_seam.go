// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

var (
	nlLinkByName        = netlink.LinkByName
	nlLinkDel           = netlink.LinkDel
	nlLinkSetUp         = netlink.LinkSetUp
	nlAddrList          = netlink.AddrList
	nlRouteListFiltered = netlink.RouteListFiltered
	nlLinkList          = netlink.LinkList

	nlLinkSetName    = netlink.LinkSetName
	nlLinkAddAltName = netlink.LinkAddAltName

	// nlRouteDel acts in the caller's current namespace; purgeRouterAdvertRoutes calls it inside the sandbox on a locked thread.
	nlRouteDel = netlink.RouteDel

	// nlNewHandleAt needs CAP_SYS_ADMIN even for the caller's own namespace, since it calls setns(2) (#417).
	nlNewHandleAt = netlink.NewHandleAt

	nlLinkByIndex = func(h *netlink.Handle, index int) (netlink.Link, error) {
		return h.LinkByIndex(index)
	}
)

var ipamAddReserveLink = (*Plugin).addIPAMReserveLink

// nlAddrDel is a seam because the release path must remove a DHCPv6 address before it sends
// Release (RFC 9915 section 18.2.7, #962).
var nlAddrDel = func(h *netlink.Handle, link netlink.Link, addr *netlink.Addr) error {
	return h.AddrDel(link, addr)
}

type linkLister interface {
	LinkList() ([]netlink.Link, error)
}

var (
	// The dump goes through util.DumpResult here so no seam can drop ErrDumpInterrupted (#802).
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

	nlHandleLinkSetMTU = func(h *netlink.Handle, l netlink.Link, mtu int) error {
		return h.LinkSetMTU(l, mtu)
	}

	// renew() writes the address before the routes, as the kernel needs, so this seam reaches all that follows (#821).
	nlHandleAddrReplace = func(h *netlink.Handle, l netlink.Link, a *netlink.Addr) error {
		return h.AddrReplace(l, a)
	}
)
