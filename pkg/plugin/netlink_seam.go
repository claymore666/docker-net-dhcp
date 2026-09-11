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

// linkLister is the subset of *netlink.Handle that findLinkByMAC needs.
// Taking the interface (not the concrete handle) lets the MAC-walk logic
// be unit-tested without a live netns handle; *netlink.Handle satisfies
// it as-is.
type linkLister interface {
	LinkList() ([]netlink.Link, error)
}
