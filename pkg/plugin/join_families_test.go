// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// One table over IPv4 on and off, each ipv6 spelling, the gateways a Join is handed and skip_routes (#1318).
func TestJoin_EveryFamilyGatewayAndSkipRoutesCombinationAnswersWhatTheReferenceSays(t *testing.T) {
	families := []struct {
		name   string
		opts   DHCPNetworkOptions
		v4, v6 bool
	}{
		{"no ipv6 option", DHCPNetworkOptions{}, true, false},
		{"ipv6=true", DHCPNetworkOptions{IPv6: true}, true, true},
		{"ipv6_mode=off", DHCPNetworkOptions{IPv6Mode: "off"}, true, false},
		{"ipv6_mode=dhcp", DHCPNetworkOptions{IPv6Mode: "dhcp"}, true, true},
		{"ipv6_mode=slaac", DHCPNetworkOptions{IPv6Mode: "slaac"}, true, true},
		{"ipv6_mode=auto", DHCPNetworkOptions{IPv6Mode: "auto"}, true, true},
		{"ipv6=true with ipv6_mode=dhcp", DHCPNetworkOptions{IPv6: true, IPv6Mode: "dhcp"}, true, true},
		{"ipv6=true with ipv6_mode=slaac", DHCPNetworkOptions{IPv6: true, IPv6Mode: "slaac"}, true, true},
		{"ipv6=true with ipv6_mode=auto", DHCPNetworkOptions{IPv6: true, IPv6Mode: "auto"}, true, true},
		{"ipv4 off, ipv6=true", DHCPNetworkOptions{IPv4Off: true, IPv6: true}, false, true},
		{"ipv4 off, ipv6_mode=dhcp", DHCPNetworkOptions{IPv4Off: true, IPv6Mode: "dhcp"}, false, true},
		{"ipv4 off, ipv6_mode=slaac", DHCPNetworkOptions{IPv4Off: true, IPv6Mode: "slaac"}, false, true},
		{"ipv4 off, ipv6_mode=auto", DHCPNetworkOptions{IPv4Off: true, IPv6Mode: "auto"}, false, true},
	}
	gateways := []struct {
		name     string
		hint     joinHint
		defaults bool
		gw4, gw6 string
	}{
		{"lease and advertisement name gateways", joinHint{Gateway: "192.168.99.1", GatewayIPv6: "fe80::1"}, true,
			"192.168.99.1", "fe80::1"},
		{"no lease gateway: the host IPv4 default fills in, the host IPv6 one never does", joinHint{}, true,
			"192.168.99.254", ""},
		{"no gateway anywhere", joinHint{}, false, "", ""},
	}
	for _, f := range families {
		for _, g := range gateways {
			for _, skip := range []bool{false, true} {
				name := f.name + "/" + g.name
				if skip {
					name += "/skip_routes"
				}
				t.Run(name, func(t *testing.T) {
					host := []netlink.Route{
						hostRoute(t, "10.20.0.0/16", "", 0), hostRoute(t, "10.30.0.0/16", "192.168.99.253", 0),
						hostRoute(t, "fd00:98::/64", "", 0),
					}
					if g.defaults {
						host = append(host, hostRoute(t, "0.0.0.0/0", "192.168.99.254", 0), hostRoute(t, "::/0", "fe80::fe", 0))
					}
					stubHostRoutes(t, host)
					hint := g.hint
					hint.IPv4, hint.IPv6 = v4Addr(t), v6Addr(t)
					hint.Routes = []*StaticRoute{via("10.40.0.0/16", "192.168.99.252")}
					hint.RoutesIPv6 = []*StaticRoute{onLink("fd00:99::/64")}
					opts := f.opts
					opts.SkipRoutes = skip
					res := runJoin(t, opts, hint)

					var want []string
					if f.v4 && !skip {
						want = append(want, "10.20.0.0/16 onlink", "10.30.0.0/16 via 192.168.99.253", "10.40.0.0/16 via 192.168.99.252")
					}
					if f.v6 && !skip {
						want = append(want, "fd00:98::/64 onlink", "fd00:99::/64 onlink")
					}
					got := describeStaticRoutes(res.StaticRoutes)
					sort.Strings(got)
					sort.Strings(want)
					if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
						t.Errorf("routes in the Join answer = %v, want %v", got, want)
					}
					wantGW4, wantGW6 := "", ""
					if f.v4 {
						wantGW4 = g.gw4
					}
					if f.v6 {
						wantGW6 = g.gw6
					}
					if res.Gateway != wantGW4 || res.GatewayIPv6 != wantGW6 {
						t.Errorf("gateways in the Join answer = %q and %q, want %q and %q", res.Gateway, res.GatewayIPv6, wantGW4, wantGW6)
					}
					if wantOff := !f.v4 && wantGW6 == ""; res.DisableGatewayService != wantOff {
						t.Errorf("DisableGatewayService = %v, want %v", res.DisableGatewayService, wantOff)
					}
				})
			}
		}
	}
}

// A stored ipv6=true beside ipv6_mode=off is refused at Join, answered as neither family (#1318, #817).
func TestJoin_AStoredIPv6TrueBesideModeOffIsRefused(t *testing.T) {
	stubHostRoutes(t, nil)
	withStateDir(t, t.TempDir())
	if err := saveOptions("n1318", DHCPNetworkOptions{Bridge: "lo", IPv6: true, IPv6Mode: "off"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := onWaitingEngine(&Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
		joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)})
	p.storeJoinHint("e1318", joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), GatewayIPv6: "fe80::1"})
	res, err := p.Join(context.Background(), JoinRequest{NetworkID: "n1318", EndpointID: "e1318"})
	if err == nil || !strings.Contains(err.Error(), "ipv6") {
		t.Fatalf("Join = %+v, %v; want a refusal naming ipv6", res, err)
	}
}
