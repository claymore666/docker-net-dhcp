// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// systemd-networkd and NetworkManager install an RA Route Information option (RFC 4191 section 2.3) as a host route
// via the router's link-local address, and engines below 28.0 look up such a next hop before the link is up (#1149, #1344).
func TestJoin_AHostRouteViaALinkLocalNextHopIsWithheldOnAnEngineBelow28(t *testing.T) {
	stubHostRoutes(t, []netlink.Route{hostRoute(t, "fd00:97::/48", "fe80::1", 0)})
	withStateDir(t, t.TempDir())
	if err := saveOptions("n1149", DHCPNetworkOptions{Bridge: "lo", IPv6: true}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
		joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
	p.engine.Store(&engineIdentity{Version: "27.5.1", APIVersion: "1.47"})
	mac, _ := net.ParseMAC("02:42:c0:a8:63:3d")
	p.storeJoinHint("e1149", joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), MacAddress: mac})
	res, err := p.Join(context.Background(), JoinRequest{NetworkID: "n1149", EndpointID: "e1149"})
	p.mu.Lock()
	m := p.persistentDHCP["e1149"]
	p.mu.Unlock()
	if m != nil {
		abortJoinAttach(t, p, "e1149", m)
	}
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if _, linkLocal := splitLinkLocalNextHops(res.StaticRoutes); len(linkLocal) > 0 {
		t.Errorf("Join on engine 27.5.1 returned %v: a host route via a link-local next hop, a shape the plugin withholds from the advertisement on this engine",
			describeStaticRoutes(linkLocal))
	}
}

func TestJoin_HostV6RoutesKeepTheirShapeWhereTheEngineAllowsIt(t *testing.T) {
	for _, tc := range []struct {
		name, version, gw string
		wantKept          bool
	}{
		{"engine 28 keeps a link-local next hop", "28.5.2", "fe80::1", true},
		{"engine below 28 keeps a global next hop", "27.5.1", "fd00:97::1", true},
		{"engine below 28 withholds a link-local next hop", "27.5.1", "fe80::1", false},
		{"an unprobed engine withholds a link-local next hop", "", "fe80::1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubHostRoutes(t, []netlink.Route{hostRoute(t, "fd00:97::/48", tc.gw, 0)})
			withStateDir(t, t.TempDir())
			if err := saveOptions("n1344", DHCPNetworkOptions{Bridge: "lo", IPv6: true}); err != nil {
				t.Fatalf("saveOptions: %v", err)
			}
			p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
				joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
			if tc.version != "" {
				p.engine.Store(&engineIdentity{Version: tc.version, APIVersion: "1.47"})
			}
			mac, _ := net.ParseMAC("02:42:c0:a8:63:3d")
			p.storeJoinHint("e1344", joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), MacAddress: mac})
			res, err := p.Join(context.Background(), JoinRequest{NetworkID: "n1344", EndpointID: "e1344"})
			p.mu.Lock()
			m := p.persistentDHCP["e1344"]
			p.mu.Unlock()
			if m != nil {
				abortJoinAttach(t, p, "e1344", m)
			}
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			var kept bool
			for _, r := range res.StaticRoutes {
				kept = kept || r.NextHop == tc.gw
			}
			if kept != tc.wantKept {
				t.Errorf("route via %s kept = %v, want %v: %v", tc.gw, kept, tc.wantKept, describeStaticRoutes(res.StaticRoutes))
			}
		})
	}
}
