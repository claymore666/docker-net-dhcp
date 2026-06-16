package plugin

import (
	"errors"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// fakeLink is a minimal netlink.Link for driving the type/flags-dependent
// branches of validateParentForChild and the route helpers without a real
// interface.
type fakeLink struct {
	attrs netlink.LinkAttrs
	typ   string
}

func (f *fakeLink) Attrs() *netlink.LinkAttrs { return &f.attrs }
func (f *fakeLink) Type() string              { return f.typ }

// stubLinkByName swaps nlLinkByName for the test's duration.
func stubLinkByName(t *testing.T, fn func(string) (netlink.Link, error)) {
	t.Helper()
	prev := nlLinkByName
	nlLinkByName = fn
	t.Cleanup(func() { nlLinkByName = prev })
}

func TestValidateParentForChild(t *testing.T) {
	cases := []struct {
		name    string
		link    netlink.Link
		lookErr error
		wantErr error
		wantOK  bool
	}{
		{
			name:    "lookup_failure",
			lookErr: errors.New("no such device"),
			wantErr: nil, // wrapped generic error, just expect non-nil
		},
		{
			name:    "reject_bridge",
			link:    &fakeLink{typ: "bridge"},
			wantErr: util.ErrParentInvalid,
		},
		{
			name:    "reject_macvlan",
			link:    &fakeLink{typ: "macvlan"},
			wantErr: util.ErrParentInvalid,
		},
		{
			name:    "reject_ipvlan",
			link:    &fakeLink{typ: "ipvlan"},
			wantErr: util.ErrParentInvalid,
		},
		{
			name:    "reject_macvtap",
			link:    &fakeLink{typ: "macvtap"},
			wantErr: util.ErrParentInvalid,
		},
		{
			name:    "parent_down",
			link:    &fakeLink{typ: "device", attrs: netlink.LinkAttrs{Flags: 0}},
			wantErr: util.ErrParentDown,
		},
		{
			name:   "ok_device_up",
			link:   &fakeLink{typ: "device", attrs: netlink.LinkAttrs{Flags: net.FlagUp}},
			wantOK: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubLinkByName(t, func(string) (netlink.Link, error) {
				return c.link, c.lookErr
			})
			link, err := validateParentForChild("parent0")
			if c.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if link == nil {
					t.Fatal("expected a link, got nil")
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("expected errors.Is(%v), got %v", c.wantErr, err)
			}
		})
	}
}

func TestDeleteParentAttachedEndpoint(t *testing.T) {
	t.Run("link_already_gone", func(t *testing.T) {
		stubLinkByName(t, func(string) (netlink.Link, error) {
			return nil, errors.New("not found")
		})
		p := &Plugin{}
		if err := p.deleteParentAttachedEndpoint(DeleteEndpointRequest{NetworkID: "n1", EndpointID: "ep1"}); err != nil {
			t.Fatalf("expected nil (link gone is expected), got %v", err)
		}
	})

	t.Run("link_del_error", func(t *testing.T) {
		stubLinkByName(t, func(string) (netlink.Link, error) {
			return &fakeLink{typ: "macvlan"}, nil
		})
		prev := nlLinkDel
		nlLinkDel = func(netlink.Link) error { return errors.New("del boom") }
		t.Cleanup(func() { nlLinkDel = prev })

		p := &Plugin{}
		if err := p.deleteParentAttachedEndpoint(DeleteEndpointRequest{NetworkID: "n1", EndpointID: "ep1"}); err == nil {
			t.Fatal("expected error when LinkDel fails")
		}
	})

	t.Run("success", func(t *testing.T) {
		stubLinkByName(t, func(string) (netlink.Link, error) {
			return &fakeLink{typ: "macvlan"}, nil
		})
		prev := nlLinkDel
		nlLinkDel = func(netlink.Link) error { return nil }
		t.Cleanup(func() { nlLinkDel = prev })

		p := &Plugin{}
		if err := p.deleteParentAttachedEndpoint(DeleteEndpointRequest{NetworkID: "n1", EndpointID: "ep1"}); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
}

func stubRouteList(t *testing.T, routes []netlink.Route, err error) {
	t.Helper()
	prev := nlRouteListFiltered
	nlRouteListFiltered = func(int, *netlink.Route, uint64) ([]netlink.Route, error) {
		return routes, err
	}
	t.Cleanup(func() { nlRouteListFiltered = prev })
}

func TestAddRoutes_ListError(t *testing.T) {
	stubRouteList(t, nil, errors.New("list boom"))
	p := &Plugin{}
	res := &JoinResponse{}
	err := p.addRoutes(&DHCPNetworkOptions{}, false, &fakeLink{}, JoinRequest{}, joinHint{}, res)
	if err == nil {
		t.Fatal("expected error when RouteListFiltered fails")
	}
}

func TestAddRoutes_DefaultGatewayV4(t *testing.T) {
	stubRouteList(t, []netlink.Route{
		{Dst: nil, Gw: net.IPv4(192, 168, 0, 1)}, // default route
	}, nil)
	p := &Plugin{}
	res := &JoinResponse{}
	if err := p.addRoutes(&DHCPNetworkOptions{}, false, &fakeLink{}, JoinRequest{}, joinHint{}, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Gateway != "192.168.0.1" {
		t.Fatalf("gateway: got %q want 192.168.0.1", res.Gateway)
	}
}

func TestAddRoutes_DefaultGatewayV6(t *testing.T) {
	stubRouteList(t, []netlink.Route{
		{Dst: nil, Gw: net.ParseIP("fe80::1")},
	}, nil)
	p := &Plugin{}
	res := &JoinResponse{}
	if err := p.addRoutes(&DHCPNetworkOptions{}, true, &fakeLink{}, JoinRequest{}, joinHint{}, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.GatewayIPv6 != "fe80::1" {
		t.Fatalf("v6 gateway: got %q want fe80::1", res.GatewayIPv6)
	}
}

func TestAddRoutes_SkipRoutesDropsStatic(t *testing.T) {
	_, dst, _ := net.ParseCIDR("10.0.0.0/8")
	stubRouteList(t, []netlink.Route{
		{Dst: dst, Gw: net.IPv4(192, 168, 0, 254)},
	}, nil)
	p := &Plugin{}
	res := &JoinResponse{}
	if err := p.addRoutes(&DHCPNetworkOptions{SkipRoutes: true}, false, &fakeLink{}, JoinRequest{}, joinHint{}, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.StaticRoutes) != 0 {
		t.Fatalf("skip_routes should drop static routes, got %d", len(res.StaticRoutes))
	}
}

func TestAddRoutes_KernelRouteSkipped(t *testing.T) {
	_, dst, _ := net.ParseCIDR("10.0.0.0/8")
	stubRouteList(t, []netlink.Route{
		{Dst: dst, Protocol: unix.RTPROT_KERNEL},
	}, nil)
	p := &Plugin{}
	res := &JoinResponse{}
	// hint left empty: the kernel-protocol check short-circuits before
	// the hint deref.
	if err := p.addRoutes(&DHCPNetworkOptions{}, false, &fakeLink{}, JoinRequest{}, joinHint{}, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.StaticRoutes) != 0 {
		t.Fatalf("kernel route should be skipped, got %d static routes", len(res.StaticRoutes))
	}
}

func TestAddRoutes_OnLinkRoute(t *testing.T) {
	_, dst, _ := net.ParseCIDR("10.0.0.0/8")
	stubRouteList(t, []netlink.Route{
		{Dst: dst}, // no Gw -> on-link
	}, nil)
	p := &Plugin{}
	res := &JoinResponse{}
	hint := joinHint{IPv4: &netlink.Addr{IPNet: &net.IPNet{IP: net.IPv4(192, 168, 0, 50), Mask: net.CIDRMask(24, 32)}}}
	if err := p.addRoutes(&DHCPNetworkOptions{}, false, &fakeLink{}, JoinRequest{}, hint, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.StaticRoutes) != 1 || res.StaticRoutes[0].RouteType != RouteTypeOnLink {
		t.Fatalf("expected one on-link route, got %+v", res.StaticRoutes)
	}
}

type fakeLinkLister struct {
	links []netlink.Link
	err   error
}

func (f fakeLinkLister) LinkList() ([]netlink.Link, error) { return f.links, f.err }

func TestFindLinkByMAC(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	other, _ := net.ParseMAC("11:22:33:44:55:66")
	match := &fakeLink{typ: "macvlan", attrs: netlink.LinkAttrs{HardwareAddr: mac}}

	t.Run("list_error", func(t *testing.T) {
		_, err := findLinkByMAC(fakeLinkLister{err: errors.New("boom")}, mac)
		if err == nil {
			t.Fatal("expected error when LinkList fails")
		}
	})
	t.Run("no_match", func(t *testing.T) {
		_, err := findLinkByMAC(fakeLinkLister{links: []netlink.Link{
			&fakeLink{typ: "device", attrs: netlink.LinkAttrs{HardwareAddr: other}},
		}}, mac)
		if err == nil {
			t.Fatal("expected error when no link matches the MAC")
		}
	})
	t.Run("found", func(t *testing.T) {
		got, err := findLinkByMAC(fakeLinkLister{links: []netlink.Link{
			&fakeLink{typ: "device", attrs: netlink.LinkAttrs{HardwareAddr: other}},
			match,
		}}, mac)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != match {
			t.Fatalf("got wrong link: %+v", got)
		}
	})
}

func TestAddRoutes_StaticNextHopV4(t *testing.T) {
	_, dst, _ := net.ParseCIDR("10.0.0.0/8")
	stubRouteList(t, []netlink.Route{
		{Dst: dst, Gw: net.IPv4(192, 168, 0, 254)},
	}, nil)
	p := &Plugin{}
	res := &JoinResponse{}
	// hint.IPv4 must be set: the route-skip check dereferences it for v4.
	hint := joinHint{IPv4: &netlink.Addr{IPNet: &net.IPNet{IP: net.IPv4(192, 168, 0, 50), Mask: net.CIDRMask(24, 32)}}}
	if err := p.addRoutes(&DHCPNetworkOptions{}, false, &fakeLink{}, JoinRequest{}, hint, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.StaticRoutes) != 1 {
		t.Fatalf("static routes: got %d want 1", len(res.StaticRoutes))
	}
	if res.StaticRoutes[0].RouteType != RouteTypeNextHop || res.StaticRoutes[0].NextHop != "192.168.0.254" {
		t.Fatalf("static route: got %+v", res.StaticRoutes[0])
	}
}
