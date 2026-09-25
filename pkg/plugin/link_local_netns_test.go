// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// addLink replaces a link of the same name and brings the new one up.
func addLink(t *testing.T, h *netlink.Handle, link netlink.Link) {
	t.Helper()
	if old, err := h.LinkByName(link.Attrs().Name); err == nil {
		if err := h.LinkDel(old); err != nil {
			t.Fatalf("LinkDel %s: %v", link.Attrs().Name, err)
		}
	}
	if err := h.LinkAdd(link); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("no CAP_NET_ADMIN here: %v", err)
		}
		t.Fatalf("LinkAdd %s: %v", link.Attrs().Name, err)
	}
	if err := h.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp %s: %v", link.Attrs().Name, err)
	}
}

func addAddr(t *testing.T, h *netlink.Handle, name, addr string) {
	t.Helper()
	l, err := h.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", name, err)
	}
	a, _ := netlink.ParseAddr(addr)
	if err := h.AddrAdd(l, a); err != nil {
		t.Fatalf("AddrAdd %s on %s: %v", addr, name, err)
	}
}

// allV4Addrs lists every v4 address on the link, 169.254/16 included, which renumberLink.addrs leaves out.
func allV4Addrs(t *testing.T, l renumberLink) []string {
	t.Helper()
	list, err := util.DumpResult(l.h.AddrList(l.link, netlink.FAMILY_V4))
	if err != nil {
		t.Fatalf("AddrList: %v", err)
	}
	var out []string
	for _, a := range list {
		out = append(out, a.IPNet.String())
	}
	sort.Strings(out)
	return out
}

// hostBridge stands in for the bridge Join copies routes from: one next-hop and one on-link route besides the kernel's.
func hostBridge(t *testing.T, h *netlink.Handle) {
	t.Helper()
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "hb0"}})
	addAddr(t, h, "hb0", "192.168.99.2/24")
	hb, _ := h.LinkByName("hb0")
	_, via, _ := net.ParseCIDR("10.88.0.0/16")
	_, onlink, _ := net.ParseCIDR("10.99.0.0/16")
	for _, r := range []*netlink.Route{
		{LinkIndex: hb.Attrs().Index, Dst: via, Gw: net.ParseIP("192.168.99.253")},
		{LinkIndex: hb.Attrs().Index, Dst: onlink, Scope: netlink.SCOPE_LINK},
	} {
		if err := h.RouteAdd(r); err != nil {
			t.Fatalf("host route %v: %v", r.Dst, err)
		}
	}
}

func TestRenew_LeavingLinkLocalInstallsWhatJoinWithheld(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	lease := dhcp.Info{IP: "192.168.99.10/24", Gateway: "192.168.99.1",
		Routes: []dhcp.Route{{Destination: "172.30.0.0/16", Gateway: "192.168.99.254"}}}
	hostRoutes := []string{"10.88.0.0/16 via 192.168.99.253", "10.99.0.0/16"}
	for _, tc := range []struct {
		name, last, pinned string
		want               []string
	}{
		{name: "from link-local", last: "169.254.10.1/16",
			want: append([]string{"172.30.0.0/16 via 192.168.99.254", "192.168.99.0/24", "default via 192.168.99.1"}, hostRoutes...)},
		{name: "from link-local, pinned gateway", last: "169.254.10.1/16", pinned: "192.168.99.5",
			want: append([]string{"172.30.0.0/16 via 192.168.99.254", "192.168.99.0/24", "default via 192.168.99.5"}, hostRoutes...)},
		// The control: a lease-to-lease move leaves Join's routes as they were and copies nothing.
		{name: "from a lease", last: "192.168.99.61/24",
			want: []string{"192.168.99.0/24", "default via 192.168.99.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newRenumberLink(t, 0, "", "")
			hostBridge(t, l.h)
			addAddr(t, l.h, "rn0", tc.last)
			if !strings.HasPrefix(tc.last, "169.254.") {
				if err := l.h.RouteAdd(&netlink.Route{LinkIndex: l.link.Attrs().Index, Gw: net.ParseIP("192.168.99.1")}); err != nil {
					t.Fatalf("Join's default route: %v", err)
				}
			}
			m := l.manager(t, tc.last).withPlugin(&Plugin{})
			m.opts = DHCPNetworkOptions{Bridge: "hb0", LinkLocalFallback: true, Gateway: tc.pinned}
			var err error
			logged := captureLog(t, func() { err = m.renew(false, lease) })
			if err != nil {
				t.Fatalf("renew: %v", err)
			}
			if got := allV4Addrs(t, l); !equalStrings(got, []string{"192.168.99.10/24"}) {
				t.Errorf("link addresses = %v, want only the lease", got)
			}
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if got := l.routes(t); !equalStrings(got, want) {
				t.Errorf("link routes = %v, want %v", got, want)
			}
			if !strings.Contains(logged, "dhcp renew with changed IP") {
				t.Errorf("the move was not logged on the lease-changed path; log:\n%s", logged)
			}
			if left := strings.Contains(logged, "left its link-local address"); left != strings.HasPrefix(tc.last, "169.254.") {
				t.Errorf("link-local leave logged = %v for a move from %s", left, tc.last)
			}
		})
	}
}

func TestClaimLinkLocal_AnAddressTheKernelHoldsOnThePeerIsSkipped(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("netlink handle: %v", err)
	}
	defer h.Close()
	addLink(t, h, &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "ll0"}, PeerName: "ll1"})
	peer, err := h.LinkByName("ll1")
	if err != nil {
		t.Fatalf("LinkByName ll1: %v", err)
	}
	if err := h.LinkSetUp(peer); err != nil {
		t.Fatalf("LinkSetUp ll1: %v", err)
	}
	addAddr(t, h, "ll1", "169.254.10.1/16")

	link, err := openARPLink("ll0")
	if err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("no packet socket here: %v", err)
		}
		t.Fatalf("open ARP socket: %v", err)
	}
	defer link.Close()
	acd := fastACD()
	acd.ProbeMin, acd.ProbeMax = 20*proto.Millisecond, 30*proto.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	next, _ := sequence("169.254.10.1", "169.254.10.2")
	addr, tried, err := claimLinkLocal(ctx, link, acd, next)
	if err != nil || addr != netip.MustParseAddr("169.254.10.2") || tried != 2 {
		t.Fatalf("claim = %v after %d tried, %v; want the peer's 169.254.10.1 skipped for 169.254.10.2", addr, tried, err)
	}
}

// createWithNoServer runs CreateEndpoint against a DHCP one-shot that finds no server, returning the response, the
// join hint, the one-shot's deadline measured from before the call, and how often the claim opened the link.
func createWithNoServer(t *testing.T, ep string, opts DHCPNetworkOptions) (CreateEndpointResponse, joinHint, time.Duration, int, error) {
	t.Helper()
	withStateDir(t, t.TempDir())
	if err := saveOptions("n1", opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	var deadline time.Time
	restore := dhcpGetIP
	dhcpGetIP = func(ctx context.Context, _ string, _ *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		deadline, _ = ctx.Deadline()
		return dhcp.Info{}, dhcp.RAObservation{}, dhcp.ErrNoLease
	}
	t.Cleanup(func() { dhcpGetIP = restore })
	opened := withFakeARP(t, newFakeARPLink("02:42:0a:00:00:09"))

	p := newPluginForTest()
	p.docker = &fakeDocker{}
	before := time.Now()
	res, err := p.CreateEndpoint(t.Context(), CreateEndpointRequest{NetworkID: "n1", EndpointID: ep, Interface: &EndpointInterface{}})
	var hint joinHint
	p.updateJoinHint(ep, func(h *joinHint) { hint = *h })
	return res, hint, deadline.Sub(before), *opened, err
}

func TestCreateEndpoint_ANoServerStartClaimsLinkLocalInsideTheBudgetWithNoGateway(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	addLink(t, h, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "llbr0"}})
	addLink(t, h, &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "llpa0"}})
	for i, tc := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"bridge", DHCPNetworkOptions{Bridge: "llbr0"}},
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "llpa0"}},
	} {
		on, off := fmt.Sprintf("a%d1%061x", i, 0), fmt.Sprintf("a%d2%061x", i, 0)
		t.Run(tc.name, func(t *testing.T) {
			withLL := tc.opts
			withLL.LinkLocalFallback, withLL.Gateway = true, "192.168.99.5"
			res, hint, dhcpBudget, opened, err := createWithNoServer(t, on, withLL)
			if err != nil {
				t.Fatalf("CreateEndpoint: %v", err)
			}
			if !isLinkLocalV4String(res.Interface.Address) || !strings.HasSuffix(res.Interface.Address, "/16") {
				t.Errorf("address = %q, want a 169.254/16 address", res.Interface.Address)
			}
			if hint.Gateway != "" || hint.IPv4 == nil || opened != 1 {
				t.Errorf("gateway = %q, hint address %v, link opened %d times; want no gateway despite the pinned one, the claimed address, one claim", hint.Gateway, hint.IPv4, opened)
			}
			// The one-shot must end at callStart+17s, not 17s after the hostname lookup's two seconds.
			if dhcpBudget <= 0 || dhcpBudget > linkLocalLeaseTimeout+time.Second {
				t.Errorf("the DHCP one-shot's deadline is %v after the call, want at most %v", dhcpBudget, linkLocalLeaseTimeout)
			}

			// The control: without the option the same failure fails the endpoint and claims nothing.
			_, _, _, opened, err = createWithNoServer(t, off, tc.opts)
			if !errors.Is(err, dhcp.ErrNoLease) || opened != 0 {
				t.Errorf("option off: err = %v, link opened %d times; want ErrNoLease and no claim", err, opened)
			}
		})
	}
}
