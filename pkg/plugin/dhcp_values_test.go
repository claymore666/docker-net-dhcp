// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

func TestClampLeaseDeadline(t *testing.T) {
	tests := []struct {
		name    string
		in      time.Duration
		want    time.Duration
		clamped bool
	}{
		{"typical lease", 12 * time.Hour, 12 * time.Hour, false},
		{"exactly the ceiling", maxLeaseDeadline, maxLeaseDeadline, false},
		{"a day and a second", maxLeaseDeadline + time.Second, maxLeaseDeadline, true},
		{"a week", 7 * 24 * time.Hour, maxLeaseDeadline, true},
		{"zero", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, clamped := clampLeaseDeadline(tt.in)
			if got != tt.want || clamped != tt.clamped {
				t.Errorf("clampLeaseDeadline(%v) = (%v, %v), want (%v, %v)", tt.in, got, clamped, tt.want, tt.clamped)
			}
		})
	}
}

// TestOutageTracker_InfiniteLeaseStillLapses is the point of the clamp.
// A server granting 0xFFFFFFFF -- which dhcpcd exports verbatim -- used
// to give the watchdog a 136-year deadline, so `due` never fired and
// dhcp_timeouts stayed at zero through a total outage for that endpoint.
// Removing the clamp in leaseDeadline turns this red.
func TestOutageTracker_InfiniteLeaseStillLapses(t *testing.T) {
	start := time.Unix(0, 0)
	o := newOutageTracker(start)

	clamped := o.observe("bound", dhcp.Info{IP: "192.168.0.10/24", LeaseSeconds: 4294967295}, start)
	if !clamped {
		t.Fatal("observe did not report the 0xFFFFFFFF lease as clamped")
	}

	grace := 25 * time.Second

	// Still inside the clamped deadline: nothing is due.
	if count, _ := o.due(start.Add(maxLeaseDeadline), grace); count {
		t.Error("outage counted before the clamped deadline had passed")
	}

	// Past it: the silent lapse is caught.
	count, silent := o.due(start.Add(maxLeaseDeadline+grace+time.Second), grace)
	if !count {
		t.Error("outage NOT counted past the clamped deadline; an infinite lease disabled the watchdog")
	}
	if !silent {
		t.Error("lapse not reported as silent")
	}
}

// TestOutageTracker_ReasonableLeaseIsNotClamped guards the other
// direction: the clamp must not fire on, or shorten, a normal lease.
func TestOutageTracker_ReasonableLeaseIsNotClamped(t *testing.T) {
	start := time.Unix(0, 0)
	o := newOutageTracker(start)

	if clamped := o.observe("bound", dhcp.Info{IP: "192.168.0.10/24", LeaseSeconds: 3600}, start); clamped {
		t.Error("a one-hour lease was reported as clamped")
	}
	if o.lapseAfter != time.Hour {
		t.Errorf("lapseAfter = %v, want 1h", o.lapseAfter)
	}
}

func TestMTUAcceptable(t *testing.T) {
	tests := []struct {
		mtu  int
		want bool
	}{
		// Hostile / broken values the wire actually carries.
		{68, false}, // measured: dhcpcd exported this and the kernel took it
		{1, false},
		{575, false},
		{maxPropagatedMTU + 1, false},
		{1 << 20, false},
		// Legitimate ones.
		{576, true},
		{1280, true},
		{1500, true},
		{9000, true},
		{maxPropagatedMTU, true},
	}
	for _, tt := range tests {
		if got := mtuAcceptable(tt.mtu); got != tt.want {
			t.Errorf("mtuAcceptable(%d) = %v, want %v", tt.mtu, got, tt.want)
		}
	}
}

func TestRoutesSupersedeDefault(t *testing.T) {
	nexthop := func(dest, gw string) *StaticRoute {
		return &StaticRoute{Destination: dest, RouteType: RouteTypeNextHop, NextHop: gw}
	}
	onlink := func(dest string) *StaticRoute {
		return &StaticRoute{Destination: dest, RouteType: RouteTypeOnLink}
	}

	tests := []struct {
		name   string
		routes []*StaticRoute
		want   bool
	}{
		{
			// The attack this counter exists for: neither half is a
			// default route, together they take everything.
			name:   "two halves cover everything",
			routes: []*StaticRoute{nexthop("0.0.0.0/1", "10.99.0.9"), nexthop("128.0.0.0/1", "10.99.0.9")},
			want:   true,
		},
		{
			name:   "halves in the other order",
			routes: []*StaticRoute{nexthop("128.0.0.0/1", "10.99.0.9"), nexthop("0.0.0.0/1", "10.99.0.9")},
			want:   true,
		},
		{
			name: "four quarters",
			routes: []*StaticRoute{
				nexthop("0.0.0.0/2", "10.99.0.9"), nexthop("64.0.0.0/2", "10.99.0.9"),
				nexthop("128.0.0.0/2", "10.99.0.9"), nexthop("192.0.0.0/2", "10.99.0.9"),
			},
			want: true,
		},
		{
			name:   "a literal default route",
			routes: []*StaticRoute{nexthop("0.0.0.0/0", "10.99.0.9")},
			want:   true,
		},
		{
			name:   "overlapping but with a gap in the middle",
			routes: []*StaticRoute{nexthop("0.0.0.0/1", "10.99.0.9"), nexthop("192.0.0.0/2", "10.99.0.9")},
			want:   false,
		},
		{
			name:   "the top half only",
			routes: []*StaticRoute{nexthop("128.0.0.0/1", "10.99.0.9")},
			want:   false,
		},
		{
			name:   "ordinary split-tunnel routes",
			routes: []*StaticRoute{nexthop("10.0.0.0/8", "10.99.0.1"), onlink("192.168.99.0/24")},
			want:   false,
		},
		{
			name:   "no routes",
			routes: nil,
			want:   false,
		},
		{
			name:   "an IPv6 default cannot cover the v4 default",
			routes: []*StaticRoute{nexthop("::/0", "fe80::1")},
			want:   false,
		},
		{
			name:   "a nil entry is skipped, not panicked on",
			routes: []*StaticRoute{nil, nexthop("0.0.0.0/1", "g"), nexthop("128.0.0.0/1", "g")},
			want:   true,
		},
		{
			name:   "an unparseable destination is skipped",
			routes: []*StaticRoute{nexthop("not-a-cidr", "g"), nexthop("128.0.0.0/1", "g")},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routesSupersedeDefault(tt.routes); got != tt.want {
				t.Errorf("routesSupersedeDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDescribeStaticRoutes(t *testing.T) {
	got := describeStaticRoutes([]*StaticRoute{
		{Destination: "10.0.0.0/8", RouteType: RouteTypeNextHop, NextHop: "10.99.0.1"},
		{Destination: "192.168.99.0/24", RouteType: RouteTypeOnLink},
		nil,
	})
	want := []string{"10.0.0.0/8 via 10.99.0.1", "192.168.99.0/24 onlink"}
	if len(got) != len(want) {
		t.Fatalf("describeStaticRoutes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("describeStaticRoutes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAppendDHCPStaticRoutes_SupersedingSetIsCounted is the end of the
// #700 path: a server that sends `0.0.0.0/1 g 128.0.0.0/1 g` instead of
// a default route takes every destination from the container while the
// gateway reported to Docker -- and therefore `docker inspect` -- still
// names the legitimate router. Nothing in the response changes; only the
// counter says so.
//
// Removing the routesSupersedeDefault call turns this red.
func TestAppendDHCPStaticRoutes_SupersedingSetIsCounted(t *testing.T) {
	p := &Plugin{}
	hint := joinHint{
		Gateway: "10.99.0.1",
		Routes: []*StaticRoute{
			{Destination: "0.0.0.0/1", RouteType: RouteTypeNextHop, NextHop: "10.99.0.9"},
			{Destination: "128.0.0.0/1", RouteType: RouteTypeNextHop, NextHop: "10.99.0.9"},
		},
	}
	res := JoinResponse{Gateway: hint.Gateway}

	p.appendDHCPStaticRoutes(DHCPNetworkOptions{}, JoinRequest{}, hint, &res)

	if got := p.dhcpDefaultRouteSuperseded.Load(); got != 1 {
		t.Errorf("dhcp_default_route_superseded = %d, want 1", got)
	}
	if got := p.dhcpRoutesApplied.Load(); got != 2 {
		t.Errorf("dhcp_routes_applied = %d, want 2", got)
	}
	// The routes are still applied -- refusing them would break
	// legitimate split-tunnel configurations.
	if len(res.StaticRoutes) != 2 {
		t.Errorf("StaticRoutes = %d entries, want 2 (the routes must still be applied)", len(res.StaticRoutes))
	}
	// And this is the point: the reported gateway is untouched, which
	// is why the counter had to exist.
	if res.Gateway != "10.99.0.1" {
		t.Errorf("Gateway = %q, want the legitimate router 10.99.0.1", res.Gateway)
	}
}

func TestAppendDHCPStaticRoutes_OrdinaryRoutesDoNotCount(t *testing.T) {
	p := &Plugin{}
	hint := joinHint{
		Gateway: "10.99.0.1",
		Routes: []*StaticRoute{
			{Destination: "10.0.0.0/8", RouteType: RouteTypeNextHop, NextHop: "10.99.0.5"},
		},
	}
	res := JoinResponse{Gateway: hint.Gateway}

	p.appendDHCPStaticRoutes(DHCPNetworkOptions{}, JoinRequest{}, hint, &res)

	if got := p.dhcpDefaultRouteSuperseded.Load(); got != 0 {
		t.Errorf("dhcp_default_route_superseded = %d, want 0 for an ordinary split-tunnel route", got)
	}
	if got := p.dhcpRoutesApplied.Load(); got != 1 {
		t.Errorf("dhcp_routes_applied = %d, want 1", got)
	}
}

func TestAppendDHCPStaticRoutes_SkipRoutesCountsNothing(t *testing.T) {
	p := &Plugin{}
	hint := joinHint{Routes: []*StaticRoute{
		{Destination: "0.0.0.0/1", RouteType: RouteTypeNextHop, NextHop: "10.99.0.9"},
		{Destination: "128.0.0.0/1", RouteType: RouteTypeNextHop, NextHop: "10.99.0.9"},
	}}
	res := JoinResponse{}

	p.appendDHCPStaticRoutes(DHCPNetworkOptions{SkipRoutes: true}, JoinRequest{}, hint, &res)

	if len(res.StaticRoutes) != 0 {
		t.Errorf("skip_routes=true still applied %d routes", len(res.StaticRoutes))
	}
	if p.dhcpRoutesApplied.Load() != 0 || p.dhcpDefaultRouteSuperseded.Load() != 0 {
		t.Error("skip_routes=true moved a counter for routes it did not apply")
	}
}
