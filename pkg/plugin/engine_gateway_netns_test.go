// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"testing"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

type gatewayRaceLink struct {
	renumberLink
	v6 bool
}

func newGatewayRaceLink(t *testing.T, v6 bool) gatewayRaceLink {
	t.Helper()
	l := newRenumberLink(t, 0, "", "")
	addr := "192.168.99.61/24"
	if v6 {
		addr = "fd00:99::61/64"
	}
	a, _ := netlink.ParseAddr(addr)
	if v6 {
		a.Flags |= unix.IFA_F_NODAD
	}
	if err := l.h.AddrAdd(l.link, a); err != nil {
		t.Fatalf("AddrAdd %s: %v", addr, err)
	}
	return gatewayRaceLink{renumberLink: l, v6: v6}
}

func (l gatewayRaceLink) addr() string {
	if l.v6 {
		return "fd00:99::61/64"
	}
	return "192.168.99.61/24"
}

func (l gatewayRaceLink) joinManager(t *testing.T, res JoinResponse, pinned string) *dhcpManager {
	t.Helper()
	a, _ := netlink.ParseAddr(l.addr())
	hint := joinHint{IPv4: a}
	if l.v6 {
		hint = joinHint{IPv6: a}
	}
	m := (&Plugin{}).newJoinManager(JoinRequest{}, DHCPNetworkOptions{Gateway: pinned}, hint, res)
	m.netHandle, m.ctrLink = l.h, l.link
	return m
}

func (l gatewayRaceLink) recoveryManager(t *testing.T) *dhcpManager {
	t.Helper()
	a, _ := netlink.ParseAddr(l.addr())
	m := newDHCPManager(nil, JoinRequest{}, DHCPNetworkOptions{})
	m.setLastIP(l.v6, a)
	m.netHandle, m.ctrLink = l.h, l.link
	return m
}

// engineInstall is moby's programGateway (v28.5.2): an exclusive RouteAdd, no metric, no protocol (#1084).
func (l gatewayRaceLink) engineInstall(gw string) error {
	return l.h.RouteAdd(&netlink.Route{Scope: netlink.SCOPE_UNIVERSE, LinkIndex: l.link.Attrs().Index, Gw: net.ParseIP(gw)})
}

func (l gatewayRaceLink) defaultRoutes(t *testing.T) []string {
	t.Helper()
	family := netlink.FAMILY_V4
	if l.v6 {
		family = netlink.FAMILY_V6
	}
	list, err := util.DumpResult(l.h.RouteListFiltered(family,
		&netlink.Route{LinkIndex: l.link.Attrs().Index, Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE))
	if err != nil {
		t.Fatalf("RouteListFiltered: %v", err)
	}
	var out []string
	for _, r := range list {
		if isDefaultRoute(r) {
			out = append(out, fmt.Sprintf("default via %s metric %d", r.Gw, r.Priority))
		}
	}
	sort.Strings(out)
	return out
}

func (l gatewayRaceLink) deleteDefaultRoutes(t *testing.T) {
	t.Helper()
	family := netlink.FAMILY_V4
	if l.v6 {
		family = netlink.FAMILY_V6
	}
	list, err := util.DumpResult(l.h.RouteListFiltered(family,
		&netlink.Route{LinkIndex: l.link.Attrs().Index, Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE))
	if err != nil {
		t.Fatalf("RouteListFiltered: %v", err)
	}
	for i := range list {
		if isDefaultRoute(list[i]) {
			if err := l.h.RouteDel(&list[i]); err != nil {
				t.Fatalf("RouteDel %s: %v", list[i], err)
			}
		}
	}
}

type gatewayRaceStep struct {
	event, gw  string
	routerSeen bool
	want       []string
}

// An IPv6 event that names a router has seen one; the empty gateway without one is a lease before any advertisement.
func bound(gw string) gatewayRaceStep {
	return gatewayRaceStep{event: "bound", gw: gw, routerSeen: gw != ""}
}
func renewed(gw string) gatewayRaceStep {
	return gatewayRaceStep{event: "renew", gw: gw, routerSeen: gw != ""}
}
func advert(gw string) gatewayRaceStep {
	return gatewayRaceStep{event: "routeradvert", gw: gw, routerSeen: gw != ""}
}
func withdrawal() gatewayRaceStep      { return gatewayRaceStep{event: "routeradvert", routerSeen: true} }
func engine(gw string) gatewayRaceStep { return gatewayRaceStep{event: "engine", gw: gw} }
func routeDeleted() gatewayRaceStep    { return gatewayRaceStep{event: "delete"} }
func table(want ...string) gatewayRaceStep {
	return gatewayRaceStep{event: "table", want: want}
}

func (l gatewayRaceLink) run(t *testing.T, m *dhcpManager, steps []gatewayRaceStep) {
	t.Helper()
	for i, s := range steps {
		switch s.event {
		case "engine":
			if err := l.engineInstall(s.gw); err != nil {
				t.Fatalf("step %d: the engine's install of %s failed: %v (EEXIST is the container that never starts)",
					i, s.gw, err)
			}
		case "delete":
			l.deleteDefaultRoutes(t)
		case "table":
			want := append([]string{}, s.want...)
			sort.Strings(want)
			if got := l.defaultRoutes(t); !equalStrings(got, want) {
				t.Errorf("step %d: default routes = %v, want %v", i, got, want)
			}
		default:
			info := dhcp.Info{IP: l.addr(), Gateway: s.gw, RouterSeen: l.v6 && s.routerSeen}
			if s.event == "routeradvert" {
				info.IP = ""
			}
			m.handleEvent(dhcp.Event{Type: s.event, Data: info}, l.v6)
		}
	}
}

func TestFirstLease_LeavesTheJoinGatewayToTheEngineInEitherOrder_IPv4(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	const joinGw, otherGw = "192.168.99.1", "192.168.99.254"
	viaJoin, viaOther := "default via 192.168.99.1 metric 0", "default via 192.168.99.254 metric 0"
	joined := JoinResponse{Gateway: joinGw}
	for _, tc := range []struct {
		name     string
		res      JoinResponse
		pinned   string
		recovery bool
		steps    []gatewayRaceStep
	}{
		{name: "engine first", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(joinGw), table(viaJoin)}},
		{name: "plugin first", res: joined,
			steps: []gatewayRaceStep{bound(joinGw), table(), engine(joinGw), table(viaJoin)}},
		{name: "plugin first, a second bound before the engine", res: joined,
			steps: []gatewayRaceStep{bound(joinGw), bound(joinGw), engine(joinGw), table(viaJoin)}},
		{name: "plugin first, the first lease names another gateway, the next renew takes it", res: joined,
			steps: []gatewayRaceStep{bound(otherGw), engine(joinGw), table(viaJoin), renewed(otherGw), table(viaOther)}},
		{name: "engine first, the first lease names another gateway (#128 replace)", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(otherGw), table(viaOther)}},
		{name: "the engine never installs, the next renew adds the route", res: joined,
			steps: []gatewayRaceStep{bound(joinGw), table(), renewed(joinGw), table(viaJoin)}},
		{name: "a lease with no gateway or an unparsable one leaves the engine's route alone (#728)", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(""), renewed("not-an-address"), table(viaJoin)}},
		{name: "a route deleted after the bind saw it is added on the next renew", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(joinGw), routeDeleted(), renewed(joinGw), table(viaJoin)}},
		{name: "a route deleted after the bind saw it is added on the next bind", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(joinGw), routeDeleted(), bound(joinGw), table(viaJoin)}},
		{name: "a route deleted before any reconcile saw it is added on the next renew", res: joined,
			steps: []gatewayRaceStep{bound(joinGw), engine(joinGw), routeDeleted(), renewed(joinGw), table(viaJoin)}},
		{name: "a Join that returned only an IPv6 gateway carries no IPv4 mark, so the first bind adds the route",
			res:   JoinResponse{GatewayIPv6: "fe80::1"},
			steps: []gatewayRaceStep{bound(joinGw), table(viaJoin)}},
		{name: "a restart-recovery manager has no Join, so the first bind adds the route", recovery: true,
			steps: []gatewayRaceStep{bound(joinGw), table(viaJoin)}},
		{name: "a pinned gateway: the bind adds nothing and the engine's install succeeds", res: joined, pinned: joinGw,
			steps: []gatewayRaceStep{bound(otherGw), table(), engine(joinGw), renewed(otherGw), table(viaJoin)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newGatewayRaceLink(t, false)
			m := l.joinManager(t, tc.res, tc.pinned)
			if tc.recovery {
				m = l.recoveryManager(t)
			}
			l.run(t, m, tc.steps)
		})
	}
}

func TestFirstLease_LeavesTheJoinGatewayToTheEngineInEitherOrder_IPv6(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	const joinGw, otherGw = "fe80::1", "fe80::2"
	viaJoin, viaOther := "default via fe80::1 metric 1024", "default via fe80::2 metric 1024"
	joined := JoinResponse{GatewayIPv6: joinGw}
	for _, tc := range []struct {
		name     string
		res      JoinResponse
		recovery bool
		steps    []gatewayRaceStep
	}{
		{name: "engine first", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(joinGw), table(viaJoin)}},
		{name: "plugin first", res: joined,
			steps: []gatewayRaceStep{bound(joinGw), table(), engine(joinGw), table(viaJoin)}},
		{name: "plugin first through a changed advertisement", res: joined,
			steps: []gatewayRaceStep{advert(joinGw), table(), engine(joinGw), table(viaJoin)}},
		{name: "plugin first, the first lease names another router, the next renew takes it", res: joined,
			steps: []gatewayRaceStep{bound(otherGw), engine(joinGw), table(viaJoin), renewed(otherGw), table(viaOther)}},
		{name: "engine first, the first lease names another router", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(otherGw), table(viaOther)}},
		{name: "the engine never installs, the next renew adds the route", res: joined,
			steps: []gatewayRaceStep{bound(joinGw), table(), renewed(joinGw), table(viaJoin)}},
		{name: "a route deleted after the bind saw it is added on the next bind", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(joinGw), routeDeleted(), bound(joinGw), table(viaJoin)}},
		{name: "a route deleted after an advertisement saw it is added by the next advertisement", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), advert(joinGw), routeDeleted(), advert(joinGw), table(viaJoin)}},
		{name: "a route deleted before any reconcile saw it is added on the next renew", res: joined,
			steps: []gatewayRaceStep{bound(joinGw), engine(joinGw), routeDeleted(), renewed(joinGw), table(viaJoin)}},
		{name: "a Join that returned only an IPv4 gateway carries no IPv6 mark, so the first bind adds the route",
			res:   JoinResponse{Gateway: "192.168.99.1"},
			steps: []gatewayRaceStep{bound(joinGw), table(viaJoin)}},
		{name: "a restart-recovery manager has no Join, so the first bind adds the route", recovery: true,
			steps: []gatewayRaceStep{bound(joinGw), table(viaJoin)}},
		{name: "engine first, a first lease that has seen no router leaves the engine's route alone", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(""), table(viaJoin)}},
		{name: "plugin first, a first lease that has seen no router adds nothing and the engine's install succeeds",
			res:   joined,
			steps: []gatewayRaceStep{bound(""), table(), engine(joinGw), table(viaJoin)}},
		{name: "a Join that returned no IPv6 gateway carries no mark, and a lease that has seen no router still keeps the route",
			res:   JoinResponse{Gateway: "192.168.99.1"},
			steps: []gatewayRaceStep{engine(joinGw), bound(""), table(viaJoin)}},
		{name: "a router's withdrawal still removes the route", res: joined,
			steps: []gatewayRaceStep{engine(joinGw), bound(joinGw), withdrawal(), table()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newGatewayRaceLink(t, true)
			m := l.joinManager(t, tc.res, "")
			if tc.recovery {
				m = l.recoveryManager(t)
			}
			l.run(t, m, tc.steps)
		})
	}
}

type blockingInspectDocker struct{ fakeDocker }

func (b *blockingInspectDocker) NetworkInspect(ctx context.Context, _ string, _ dNetwork.InspectOptions) (dNetwork.Inspect, error) {
	<-ctx.Done()
	return dNetwork.Inspect{}, ctx.Err()
}

func TestJoin_MarksExactlyTheFamiliesWhoseGatewayItReturned(t *testing.T) {
	withStateDir(t, t.TempDir())
	stubKernelRouteTable(t, nil, nil, nil)
	if err := saveOptions("net-1084", DHCPNetworkOptions{Bridge: "lo", IPv6: true}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	a4, _ := netlink.ParseAddr("192.168.99.61/24")
	a6, _ := netlink.ParseAddr("fd00:99::61/64")
	mac, _ := net.ParseMAC("02:42:c0:a8:63:3d")
	for _, tc := range []struct{ gw4, gw6 string }{
		{}, {gw4: "192.168.99.1"}, {gw6: "fe80::1"}, {gw4: "192.168.99.1", gw6: "fe80::1"},
	} {
		t.Run(fmt.Sprintf("gateway=%q gateway6=%q", tc.gw4, tc.gw6), func(t *testing.T) {
			p := &Plugin{docker: &blockingInspectDocker{}, awaitTimeout: time.Minute,
				joinHints: make(map[string]joinHint), persistentDHCP: make(map[string]*dhcpManager)}
			p.storeJoinHint("ep-1084", joinHint{IPv4: a4, IPv6: a6, MacAddress: mac, Gateway: tc.gw4, GatewayIPv6: tc.gw6})
			res, err := p.Join(context.Background(), JoinRequest{NetworkID: "net-1084", EndpointID: "ep-1084"})
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			p.mu.Lock()
			m := p.persistentDHCP["ep-1084"]
			p.mu.Unlock()
			if m == nil {
				t.Fatal("Join registered no manager")
			}
			defer func() { m.attachCancel(); <-m.startedCh }()
			if res.Gateway != tc.gw4 || res.GatewayIPv6 != tc.gw6 {
				t.Fatalf("Join returned gateways %q, %q, want %q, %q", res.Gateway, res.GatewayIPv6, tc.gw4, tc.gw6)
			}
			if got := m.engineGateway(false).Load(); got != (tc.gw4 != "") {
				t.Errorf("IPv4 mark = %v with Join's IPv4 gateway %q", got, tc.gw4)
			}
			if got := m.engineGateway(true).Load(); got != (tc.gw6 != "") {
				t.Errorf("IPv6 mark = %v with Join's IPv6 gateway %q", got, tc.gw6)
			}
			last4, last6 := m.lastIPs()
			if last4 == nil || !last4.Equal(*a4) || last6 == nil || !last6.Equal(*a6) {
				t.Errorf("last addresses = %v, %v, want the hint's", last4, last6)
			}
			if m.MacAddress.String() != mac.String() || m.plugin != p || m.joinReq.EndpointID != "ep-1084" {
				t.Errorf("the manager lost the hint's MAC, the plugin or the request")
			}
		})
	}
}

// The recorded EEXIST is the defect's own signature; this pins that the kernel still refuses the engine's install
// over a plugin route, so the netns tests above measure the race they claim to (#1084).
func TestFirstLease_TheKernelRefusesTheEngineInstallOverAPluginRoute(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for _, v6 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v6=%v", v6), func(t *testing.T) {
			l := newGatewayRaceLink(t, v6)
			gw := "192.168.99.1"
			if v6 {
				gw = "fe80::1"
			}
			m := l.recoveryManager(t)
			m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{IP: l.addr(), Gateway: gw}}, v6)
			if err := l.engineInstall(gw); !errors.Is(err, unix.EEXIST) {
				t.Errorf("the engine's install over the plugin's route = %v, want EEXIST", err)
			}
		})
	}
}
