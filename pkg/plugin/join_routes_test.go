// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func stubHostRoutes(t *testing.T, routes []netlink.Route) {
	t.Helper()
	prev := nlRouteListFiltered
	nlRouteListFiltered = func(family int, _ *netlink.Route, _ uint64) ([]netlink.Route, error) {
		var out []netlink.Route
		for _, r := range routes {
			if (r.Dst.IP.To4() != nil) == (family == unix.AF_INET) {
				out = append(out, r)
			}
		}
		return out, nil
	}
	t.Cleanup(func() { nlRouteListFiltered = prev })
}

func hostRoute(t *testing.T, dst, gw string, metric int) netlink.Route {
	t.Helper()
	_, n, err := net.ParseCIDR(dst)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", dst, err)
	}
	if ones, bits := n.Mask.Size(); ones == 0 {
		n = netlinkDefaultDst(bits == 128)
	}
	r := netlink.Route{Dst: n, Protocol: unix.RTPROT_BOOT, Priority: metric}
	if gw != "" {
		r.Gw = net.ParseIP(gw)
	}
	return r
}

// netlinkDefaultDst is the destination netlink 1.3.1 gives a default route (route_linux.go deserializeRoute).
func netlinkDefaultDst(v6 bool) *net.IPNet {
	if v6 {
		return &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
	}
	return &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
}

func onLink(dst string) *StaticRoute {
	return &StaticRoute{Destination: dst, RouteType: RouteTypeOnLink}
}

func via(dst, gw string) *StaticRoute {
	return &StaticRoute{Destination: dst, RouteType: RouteTypeNextHop, NextHop: gw}
}

func runJoin(t *testing.T, opts DHCPNetworkOptions, hint joinHint) JoinResponse {
	t.Helper()
	withStateDir(t, t.TempDir())
	opts.Bridge = "lo"
	if err := saveOptions("n1125", opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := onWaitingEngine(&Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
		joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)})
	if hint.MacAddress == nil {
		hint.MacAddress, _ = net.ParseMAC("02:42:c0:a8:63:3d")
	}
	p.storeJoinHint("e1125", hint)
	res, err := p.Join(context.Background(), JoinRequest{NetworkID: "n1125", EndpointID: "e1125"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	p.mu.Lock()
	m := p.persistentDHCP["e1125"]
	p.mu.Unlock()
	if m != nil {
		abortJoinAttach(t, p, "e1125", m)
	}
	return res
}

func destKey(t *testing.T, dst string) string {
	t.Helper()
	p, err := netip.ParsePrefix(dst)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", dst, err)
	}
	return p.Masked().String()
}

func v4Addr(t *testing.T) *netlink.Addr {
	a, _ := netlink.ParseAddr("192.168.99.61/24")
	return a
}

func v6Addr(t *testing.T) *netlink.Addr {
	a, _ := netlink.ParseAddr("fd00:99::61/64")
	return a
}

func TestJoin_NoMixOfRouteSourcesReturnsOneDestinationTwice(t *testing.T) {
	hostSets := map[string][]netlink.Route{
		"none":     nil,
		"v4":       {hostRoute(t, "10.20.0.0/16", "192.168.99.254", 0)},
		"v6 B":     {hostRoute(t, "fd00:98::/64", "", 0)},
		"v6 B, C":  {hostRoute(t, "fd00:97::/48", "fd00:99::fe", 0), hostRoute(t, "fd00:98::/64", "", 0)},
		"v6 B x2":  {hostRoute(t, "fd00:98::/64", "", 100), hostRoute(t, "fd00:98::/64", "", 200)},
		"v4 + v6":  {hostRoute(t, "10.20.0.0/16", "", 0), hostRoute(t, "fd00:98::/64", "", 0)},
		"v4 twice": {hostRoute(t, "10.20.0.0/16", "", 100), hostRoute(t, "10.20.0.0/16", "192.168.99.254", 200)},
		"defaults": {hostRoute(t, "0.0.0.0/0", "192.168.99.1", 0), hostRoute(t, "::/0", "fe80::1", 0),
			hostRoute(t, "fd00:98::/64", "", 0)},
	}
	optionSets := map[string][]*StaticRoute{
		"none":      nil,
		"10.20/16":  {via("10.20.0.0/16", "192.168.99.253")},
		"host bits": {via("10.20.1.0/16", "192.168.99.253"), onLink("10.30.0.0/16")},
	}
	advertSets := map[string][]*StaticRoute{
		"none":        nil,
		"A, B":        {onLink("fd00:99::/64"), onLink("fd00:98::/64")},
		"B, A":        {onLink("fd00:98::/64"), onLink("fd00:99::/64")},
		"B spelled":   {onLink("fd00:98:0::/64"), via("fd00:97::/48", "fe80::1")},
		"C then B":    {via("fd00:97::/48", "fe80::1"), onLink("fd00:98::1/64")},
		"only A":      {onLink("fd00:99::/64")},
		"two spelled": {onLink("fd00:98::/64"), onLink("fd00:98:0:0::/64")},
	}
	spellings := map[string]DHCPNetworkOptions{
		"ipv6=true":      {IPv6: true},
		"ipv6_mode=dhcp": {IPv6Mode: "dhcp"},
	}
	for sn, opts := range spellings {
		for hn, host := range hostSets {
			for on, opt121 := range optionSets {
				for an, advert := range advertSets {
					t.Run(fmt.Sprintf("%s/host=%s/121=%s/advert=%s", sn, hn, on, an), func(t *testing.T) {
						stubHostRoutes(t, host)
						res := runJoin(t, opts, joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), Gateway: "192.168.99.1",
							GatewayIPv6: "fe80::1", Routes: opt121, RoutesIPv6: advert})

						var want []string
						offered := map[string]bool{}
						offer := func(dst string) {
							if k := destKey(t, dst); !offered[k] {
								offered[k] = true
								want = append(want, k)
							}
						}
						for _, v6 := range []bool{false, true} {
							for _, r := range host {
								if ones, _ := r.Dst.Mask.Size(); ones > 0 && (r.Dst.IP.To4() == nil) == v6 {
									offer(r.Dst.String())
								}
							}
						}
						for _, r := range opt121 {
							offer(r.Destination)
						}
						for _, r := range advert {
							offer(r.Destination)
						}
						var got []string
						for _, r := range res.StaticRoutes {
							got = append(got, destKey(t, r.Destination))
						}
						if strings.Join(got, " ") != strings.Join(want, " ") {
							t.Fatalf("Join returned destinations %v, want %v (each once, first-offered order)", got, want)
						}
						if res.GatewayIPv6 != "fe80::1" {
							t.Errorf("Join returned IPv6 gateway %q, want the advertisement's fe80::1", res.GatewayIPv6)
						}
					})
				}
			}
		}
	}
}

func TestJoin_AHostRouteToAnAdvertisedPrefixIsReturnedOnce(t *testing.T) {
	stubHostRoutes(t, []netlink.Route{hostRoute(t, "fd00:98::/64", "", 0)})
	res := runJoin(t, DHCPNetworkOptions{IPv6: true}, joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), GatewayIPv6: "fe80::1",
		RoutesIPv6: []*StaticRoute{onLink("fd00:99::/64"), onLink("fd00:98::/64")}})
	got := describeStaticRoutes(res.StaticRoutes)
	if strings.Join(got, ", ") != "fd00:98::/64 onlink, fd00:99::/64 onlink" {
		t.Fatalf("Join returned %v, want B from the host table, then A", got)
	}
}

func TestJoin_AHostTableHoldingOnePrefixTwiceReturnsItOnce(t *testing.T) {
	stubHostRoutes(t, []netlink.Route{hostRoute(t, "fd00:98::/64", "", 100), hostRoute(t, "fd00:98::/64", "fd00:99::fe", 200)})
	res := runJoin(t, DHCPNetworkOptions{IPv6: true}, joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t)})
	got := describeStaticRoutes(res.StaticRoutes)
	if strings.Join(got, ", ") != "fd00:98::/64 onlink" {
		t.Fatalf("Join returned %v, want the first of the two host routes only", got)
	}
}

func TestJoin_IPv6ModeAloneAnswersLikeIPv6True(t *testing.T) {
	stubHostRoutes(t, nil)
	hint := joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), GatewayIPv6: "fe80::1",
		RoutesIPv6: []*StaticRoute{onLink("fd00:99::/64")}}
	for _, mode := range []string{"dhcp", "slaac", "auto"} {
		t.Run(mode, func(t *testing.T) {
			res := runJoin(t, DHCPNetworkOptions{IPv6Mode: mode}, hint)
			if res.GatewayIPv6 != "fe80::1" || strings.Join(describeStaticRoutes(res.StaticRoutes), ", ") != "fd00:99::/64 onlink" {
				t.Fatalf("ipv6_mode=%s: Join returned IPv6 gateway %q and routes %v, want fe80::1 and fd00:99::/64 onlink",
					mode, res.GatewayIPv6, describeStaticRoutes(res.StaticRoutes))
			}
		})
	}
	t.Run("off", func(t *testing.T) {
		res := runJoin(t, DHCPNetworkOptions{IPv6Mode: "off"}, hint)
		if res.GatewayIPv6 != "" || len(res.StaticRoutes) != 0 {
			t.Fatalf("ipv6_mode=off: Join returned IPv6 gateway %q and routes %v, want neither",
				res.GatewayIPv6, describeStaticRoutes(res.StaticRoutes))
		}
	})
}

func TestJoin_AnEndpointWithoutIPv6GetsNoIPv6HostRoutes(t *testing.T) {
	stubHostRoutes(t, []netlink.Route{
		hostRoute(t, "::/0", "fe80::1", 0),
		hostRoute(t, "fd00:98::/64", "", 0),
		hostRoute(t, "fd00:97::/48", "fe80::1", 0),
		hostRoute(t, "10.20.0.0/16", "", 0),
	})
	for _, opts := range []DHCPNetworkOptions{{IPv6: true}, {IPv6Mode: "dhcp"}} {
		res := runJoin(t, opts, joinHint{IPv4: v4Addr(t)})
		if got := strings.Join(describeStaticRoutes(res.StaticRoutes), ", "); got != "10.20.0.0/16 onlink" {
			t.Fatalf("%+v: Join returned %q, want the IPv4 host route only", opts, got)
		}
		if res.GatewayIPv6 != "" {
			t.Fatalf("%+v: Join returned IPv6 gateway %q with no IPv6 address", opts, res.GatewayIPv6)
		}
	}
}

func TestAddRoutes_TheHostDefaultIsTheFallbackGatewayInTheShapeNetlinkReturns(t *testing.T) {
	for _, tc := range []struct {
		name, gw, want string
	}{
		{"via a gateway", "192.168.99.1", "192.168.99.1"},
		{"no gateway", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubHostRoutes(t, []netlink.Route{hostRoute(t, "0.0.0.0/0", tc.gw, 0)})
			res := &JoinResponse{}
			if err := (&Plugin{}).addRoutes(&DHCPNetworkOptions{}, false, &fakeLink{}, JoinRequest{}, joinHint{IPv4: v4Addr(t)}, res); err != nil {
				t.Fatalf("addRoutes: %v", err)
			}
			if res.Gateway != tc.want || len(res.StaticRoutes) != 0 {
				t.Fatalf("gateway %q and routes %v, want gateway %q and no routes", res.Gateway,
					describeStaticRoutes(res.StaticRoutes), tc.want)
			}
		})
	}
}

func TestAddRoutes_ANonKernelRouteToTheEndpointsOwnPrefixIsNotCopied(t *testing.T) {
	for _, tc := range []struct {
		name  string
		v6    bool
		table []netlink.Route
		want  string
	}{
		{"v4", false, []netlink.Route{hostRoute(t, "192.168.99.0/24", "", 0), hostRoute(t, "10.20.0.0/16", "", 0)}, "10.20.0.0/16 onlink"},
		{"v6", true, []netlink.Route{hostRoute(t, "fd00:99::/64", "", 0), hostRoute(t, "fd00:98::/64", "", 0)}, "fd00:98::/64 onlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubHostRoutes(t, tc.table)
			res := &JoinResponse{}
			hint := joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t)}
			if err := (&Plugin{}).addRoutes(&DHCPNetworkOptions{}, tc.v6, &fakeLink{}, JoinRequest{}, hint, res); err != nil {
				t.Fatalf("addRoutes: %v", err)
			}
			if got := strings.Join(describeStaticRoutes(res.StaticRoutes), ", "); got != tc.want {
				t.Fatalf("routes %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJoin_TheLeaseGatewayWinsOverTheHostDefault(t *testing.T) {
	stubHostRoutes(t, []netlink.Route{hostRoute(t, "0.0.0.0/0", "192.168.99.2", 0)})
	res := runJoin(t, DHCPNetworkOptions{}, joinHint{IPv4: v4Addr(t), Gateway: "192.168.99.1"})
	if res.Gateway != "192.168.99.1" {
		t.Fatalf("Join returned gateway %q, want the lease's 192.168.99.1 over the host's 192.168.99.2", res.Gateway)
	}
	pre := &JoinResponse{Gateway: "192.168.99.1"}
	if err := (&Plugin{}).addRoutes(&DHCPNetworkOptions{}, false, &fakeLink{}, JoinRequest{}, joinHint{IPv4: v4Addr(t)}, pre); err != nil {
		t.Fatalf("addRoutes: %v", err)
	}
	if pre.Gateway != "192.168.99.1" {
		t.Fatalf("addRoutes replaced the gateway already set with %q", pre.Gateway)
	}
}

func TestJoin_TwoPrefixesSharingANetworkAddressAreBothReturned(t *testing.T) {
	stubHostRoutes(t, []netlink.Route{hostRoute(t, "10.0.0.0/8", "", 0), hostRoute(t, "fd00:98::/48", "", 0)})
	res := runJoin(t, DHCPNetworkOptions{IPv6: true}, joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), GatewayIPv6: "fe80::1",
		Routes: []*StaticRoute{via("10.0.0.0/16", "192.168.99.253")}, RoutesIPv6: []*StaticRoute{onLink("fd00:98::/64")}})
	got := strings.Join(describeStaticRoutes(res.StaticRoutes), ", ")
	want := "10.0.0.0/8 onlink, fd00:98::/48 onlink, 10.0.0.0/16 via 192.168.99.253, fd00:98::/64 onlink"
	if got != want {
		t.Fatalf("Join returned %q, want %q", got, want)
	}
}
