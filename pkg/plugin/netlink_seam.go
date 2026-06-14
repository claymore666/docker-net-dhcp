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
	nlRouteListFiltered = netlink.RouteListFiltered
)

// linkLister is the subset of *netlink.Handle that findLinkByMAC needs.
// Taking the interface (not the concrete handle) lets the MAC-walk logic
// be unit-tested without a live netns handle; *netlink.Handle satisfies
// it as-is.
type linkLister interface {
	LinkList() ([]netlink.Link, error)
}
