// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

func onWaitingEngine(p *Plugin) *Plugin {
	p.engine.Store(&engineIdentity{Version: "28.5.2", APIVersion: "1.51"})
	return p
}

func TestEngineWaitsForV6Link_OnEachSideOfThe28Line(t *testing.T) {
	for _, tc := range []struct {
		version string
		waits   bool
	}{
		{"20.10.24", false},
		{"23.0.6", false},
		{"24.0.9", false},
		{"26.1.5+dfsg1", false},
		{"27.5.1", false},
		{"27.99.0", false},
		{"28.0.0-rc.1", true},
		{"28.0.0", true},
		{"28.5.2", true},
		{"29.8.2", true},
		{"30.0.0", true},
		{unknownEngineField, false},
		{"", false},
		{"not-a-version", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			p := &Plugin{}
			p.engine.Store(&engineIdentity{Version: tc.version, APIVersion: "1.43"})
			if got := p.engineWaitsForV6Link(); got != tc.waits {
				t.Errorf("engineWaitsForV6Link() on %q = %v, want %v", tc.version, got, tc.waits)
			}
		})
	}
	if (&Plugin{}).engineWaitsForV6Link() {
		t.Error("a plugin that never probed the engine counts it as waiting")
	}
}

func joinOnEngine(t *testing.T, version string, opts DHCPNetworkOptions, hint joinHint) (JoinResponse, bool, string) {
	t.Helper()
	withStateDir(t, t.TempDir())
	opts.Bridge = "lo"
	if err := saveOptions("n1149", opts); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
		joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
	if version != "" {
		p.engine.Store(&engineIdentity{Version: version, APIVersion: "1.43"})
	}
	p.storeJoinHint("e1149", hint)
	var res JoinResponse
	var err error
	out := captureLog(t, func() {
		res, err = p.Join(context.Background(), JoinRequest{NetworkID: "n1149", EndpointID: "e1149"})
	})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	p.mu.Lock()
	m := p.persistentDHCP["e1149"]
	p.mu.Unlock()
	if m == nil {
		t.Fatal("Join registered no manager")
	}
	mark := m.engineGateway(true).Load()
	abortJoinAttach(t, p, "e1149", m)
	return res, mark, out
}

func TestJoin_TheV6GatewayAndLinkLocalNextHopsGoToTheEngineOnlyFrom28(t *testing.T) {
	hostV6 := hostRoute(t, "fd00:cc::/48", "fe80::2", 0)
	advert := []*StaticRoute{
		{Destination: "fd00:99::/64", RouteType: RouteTypeOnLink},
		{Destination: "fd00:aa::/48", RouteType: RouteTypeNextHop, NextHop: "fe80::1"},
		{Destination: "fd00:bb::/48", RouteType: RouteTypeNextHop, NextHop: "fd00:99::1"},
	}
	for _, tc := range []struct {
		engine     string
		gateway    string
		wantGW     string
		wantRoutes string
	}{
		{"27.5.1", "fe80::1", "", "fd00:cc::/48 via fe80::2, fd00:99::/64 onlink, fd00:bb::/48 via fd00:99::1"},
		{"24.0.9", "fe80::1", "", "fd00:cc::/48 via fe80::2, fd00:99::/64 onlink, fd00:bb::/48 via fd00:99::1"},
		{"", "fe80::1", "", "fd00:cc::/48 via fe80::2, fd00:99::/64 onlink, fd00:bb::/48 via fd00:99::1"},
		{"27.5.1", "", "", "fd00:cc::/48 via fe80::2, fd00:99::/64 onlink, fd00:bb::/48 via fd00:99::1"},
		{"28.0.0", "fe80::1", "fe80::1", "fd00:cc::/48 via fe80::2, fd00:99::/64 onlink, fd00:aa::/48 via fe80::1, fd00:bb::/48 via fd00:99::1"},
		{"29.8.2", "fe80::1", "fe80::1", "fd00:cc::/48 via fe80::2, fd00:99::/64 onlink, fd00:aa::/48 via fe80::1, fd00:bb::/48 via fd00:99::1"},
		{"29.8.2", "", "", "fd00:cc::/48 via fe80::2, fd00:99::/64 onlink, fd00:aa::/48 via fe80::1, fd00:bb::/48 via fd00:99::1"},
	} {
		t.Run("engine="+tc.engine+" gateway="+tc.gateway, func(t *testing.T) {
			stubHostRoutes(t, []netlink.Route{hostV6})
			res, mark, out := joinOnEngine(t, tc.engine, DHCPNetworkOptions{IPv6: true},
				joinHint{IPv4: v4Addr(t), IPv6: v6Addr(t), GatewayIPv6: tc.gateway, RoutesIPv6: advert})
			if res.GatewayIPv6 != tc.wantGW {
				t.Errorf("Join returned IPv6 gateway %q, want %q", res.GatewayIPv6, tc.wantGW)
			}
			if mark != (tc.wantGW != "") {
				t.Errorf("the manager's mark that the engine owes the v6 default route = %v with Join's gateway %q", mark, res.GatewayIPv6)
			}
			var v6 []string
			for _, r := range describeStaticRoutes(res.StaticRoutes) {
				if strings.Contains(r, ":") {
					v6 = append(v6, r)
				}
			}
			if got := strings.Join(v6, ", "); got != tc.wantRoutes {
				t.Errorf("Join returned IPv6 routes %q, want %q", got, tc.wantRoutes)
			}
			if handed := strings.Contains(out, "Setting IPv6 gateway"); handed != (tc.wantGW != "") {
				t.Errorf("the log says the gateway went to the engine = %v, Join returned %q", handed, res.GatewayIPv6)
			}
		})
	}
}
