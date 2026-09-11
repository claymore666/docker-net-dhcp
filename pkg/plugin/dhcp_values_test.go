// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"testing"
)

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
			// The escape from the exact-cover test, and the reason
			// routableUnicastV4 exists. This reaches 239.255.255.255 --
			// every routable unicast address plus all of multicast --
			// and stops one prefix short of 0.0.0.0/0. The container
			// cannot tell the difference: what is left uncovered is
			// 240.0.0.0/4, which is reserved and unroutable.
			//
			// Under the old predicate this returned false. Adding a
			// prefix defeated the detector for the exact traffic it
			// existed to notice.
			name: "one prefix short of everything, leaving only reserved space",
			routes: []*StaticRoute{
				nexthop("0.0.0.0/1", "10.99.0.9"), nexthop("128.0.0.0/2", "10.99.0.9"),
				nexthop("192.0.0.0/3", "10.99.0.9"), nexthop("224.0.0.0/4", "10.99.0.9"),
			},
			want: true,
		},
		{
			// A sender that does not care about multicast needs only
			// three. This is the cheapest form of the same evasion.
			name: "three prefixes taking all unicast",
			routes: []*StaticRoute{
				nexthop("0.0.0.0/1", "10.99.0.9"), nexthop("128.0.0.0/2", "10.99.0.9"),
				nexthop("192.0.0.0/3", "10.99.0.9"),
			},
			want: true,
		},
		{
			// The other direction, which is what stops the looser
			// predicate becoming an alarm on ordinary configurations: a
			// genuine hole in ROUTABLE space is a split tunnel, however
			// much of the rest is claimed. Here 64.0.0.0 through
			// 127.255.255.255 keeps the container's default route.
			name:   "a hole in routable space is still a split tunnel",
			routes: []*StaticRoute{nexthop("0.0.0.0/2", "10.99.0.9"), nexthop("128.0.0.0/1", "10.99.0.9")},
			want:   false,
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

// TestRoutableUnicastV4Boundaries pins the arithmetic in
// routableUnicastV4 to the addresses it claims to describe.
//
// The constants are written as hex because the walk is arithmetic, and
// hex is unreadable in exactly the way that lets an off-by-one sit
// there: a wrong bound here does not crash or fail to compile, it just
// silently moves what counts as a full takeover. So each edge is
// restated as a dotted quad, and each excluded block is checked to fall
// in a gap rather than being asserted about in prose.
func TestRoutableUnicastV4Boundaries(t *testing.T) {
	as32 := func(t *testing.T, s string) uint32 {
		t.Helper()
		ip := net.ParseIP(s).To4()
		if ip == nil {
			t.Fatalf("ParseIP(%q) is not an IPv4 address", s)
		}
		return binary.BigEndian.Uint32(ip)
	}

	for _, tc := range []struct {
		i      int
		lo, hi string
	}{
		{0, "1.0.0.0", "126.255.255.255"},
		{1, "128.0.0.0", "169.253.255.255"},
		{2, "169.255.0.0", "223.255.255.255"},
	} {
		got := routableUnicastV4[tc.i]
		if got.lo != as32(t, tc.lo) {
			t.Errorf("routableUnicastV4[%d].lo = %#08x, want %s", tc.i, got.lo, tc.lo)
		}
		if got.hi != as32(t, tc.hi) {
			t.Errorf("routableUnicastV4[%d].hi = %#08x, want %s", tc.i, got.hi, tc.hi)
		}
	}

	// The blocks deliberately left out, each named with why a container
	// does not route through it. A route set that covers everything
	// except these is a full takeover, so none of them may be required.
	for _, tc := range []struct{ addr, why string }{
		{"0.0.0.0", "this network"},
		{"0.255.255.255", "this network"},
		{"127.0.0.1", "loopback"},
		{"127.255.255.255", "loopback"},
		{"169.254.0.0", "link-local"},
		{"169.254.255.255", "link-local"},
		{"224.0.0.1", "multicast"},
		{"239.255.255.255", "multicast"},
		{"240.0.0.0", "reserved"},
		{"255.255.255.255", "reserved/broadcast"},
	} {
		v := as32(t, tc.addr)
		for i, req := range routableUnicastV4 {
			if v >= req.lo && v <= req.hi {
				t.Errorf("%s (%s) falls inside routableUnicastV4[%d]; requiring it "+
					"means a full takeover that skips %s goes unreported",
					tc.addr, tc.why, i, tc.why)
			}
		}
	}

	// And RFC 1918 space is NOT excluded: omitting the container's own
	// private ranges is the definition of a split tunnel, and that must
	// keep reading as one.
	for _, addr := range []string{"10.0.0.0", "172.16.0.0", "192.168.0.0"} {
		v := as32(t, addr)
		inside := false
		for _, req := range routableUnicastV4 {
			if v >= req.lo && v <= req.hi {
				inside = true
			}
		}
		if !inside {
			t.Errorf("%s is not required; a route set omitting private space "+
				"would then count as taking every destination", addr)
		}
	}
}

// TestSpansCover covers the walk itself, including the two cases a real
// route set produces constantly and a boundary that would wrap.
func TestSpansCover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spans   []v4Span
		lo, hi  uint32
		covered bool
	}{
		{"exact", []v4Span{{10, 20}}, 10, 20, true},
		{"contained", []v4Span{{0, 100}}, 10, 20, true},
		{"adjacent spans join", []v4Span{{10, 14}, {15, 20}}, 10, 20, true},
		{"overlapping spans join", []v4Span{{10, 16}, {12, 20}}, 10, 20, true},
		{"nested span does not shorten the run", []v4Span{{10, 20}, {12, 14}}, 10, 20, true},
		{"one address missing in the middle", []v4Span{{10, 14}, {16, 20}}, 10, 20, false},
		{"starts late", []v4Span{{11, 20}}, 10, 20, false},
		{"ends early", []v4Span{{10, 19}}, 10, 20, false},
		{"nothing", nil, 10, 20, false},
		{
			// A span ending at the top of the space must not compute
			// hi+1 and wrap to 0, which would restart the walk and
			// report a gap as covered.
			name:    "a span to the top of the space does not wrap",
			spans:   []v4Span{{0x80000000, math.MaxUint32}},
			lo:      0x80000000,
			hi:      0xDFFFFFFF,
			covered: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spansCover(tc.spans, tc.lo, tc.hi); got != tc.covered {
				t.Errorf("spansCover(%v, %d, %d) = %v, want %v",
					tc.spans, tc.lo, tc.hi, got, tc.covered)
			}
		})
	}
}

// TestNoteDNSPropagationPIDMismatch_CountsTheEffect asserts the counter
// that container_netns_test.go:37 and :95 say they are protecting.
//
// Those two tests assert the error still carries errPIDNotContainer,
// with comments reading "so the counter can fire" and "or the mismatch
// is never counted". Both were true about the precondition and neither
// touched the effect: deleting the `.Add(1)` line left `go test ./...`
// green across the whole tree, with those comments still standing guard
// in writing over nothing.
//
// The cases that carry the weight are the ones that must NOT count. A
// mismatch is the signal that a PID was reused between resolution and
// use; if any other failure counted too, the counter stops separating
// that from an ordinary error and an operator cannot act on it.
//
// netnsPIDMismatches is asserted here only in the negative. It is
// counted inside openSandboxNetNS, which owns it, and this method must
// not touch it — an earlier version of this method took a `kind` and
// covered both, which double-counted once #731's opener landed.
func TestNoteDNSPropagationPIDMismatch_CountsTheEffect(t *testing.T) {
	other := errors.New("some other failure")
	wrapped := fmt.Errorf("failed to open: %w", errPIDNotContainer)

	for _, tc := range []struct {
		name    string
		calls   []error
		wantDNS int32
	}{
		{
			name:    "a mismatch counts",
			calls:   []error{errPIDNotContainer},
			wantDNS: 1,
		},
		{
			// The call site hands this method a wrapped error, never
			// the sentinel itself -- writeContainerResolvConf wraps. A
			// predicate written with == instead of errors.Is passes
			// every other case here.
			name:    "a wrapped sentinel still counts",
			calls:   []error{wrapped},
			wantDNS: 1,
		},
		{
			name:    "an unrelated failure counts nothing",
			calls:   []error{other},
			wantDNS: 0,
		},
		{
			// A PID reused twice is twice the fault, not one event
			// with a flag. A counter set to 1 rather than incremented
			// passes every single-call case above.
			name:    "each mismatch counts, not just the first",
			calls:   []error{errPIDNotContainer, errPIDNotContainer, errPIDNotContainer},
			wantDNS: 3,
		},
		{
			name:    "a nil error is not a mismatch",
			calls:   []error{nil},
			wantDNS: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			m := &dhcpManager{plugin: p}
			for _, err := range tc.calls {
				m.noteDNSPropagationPIDMismatch(err)
			}
			if got := p.dnsPropagationPIDMismatches.Load(); got != tc.wantDNS {
				t.Errorf("dns_propagation_pid_mismatches = %d, want %d", got, tc.wantDNS)
			}
			if got := p.netnsPIDMismatches.Load(); got != 0 {
				t.Errorf("netns_pid_mismatches = %d, want 0: this method must not touch "+
					"the netns counter, which openSandboxNetNS owns. It did once, and the "+
					"result was one refusal counted twice", got)
			}
		})
	}
}

// TestNoteDNSPropagationPIDMismatch_SurvivesANilPlugin pins the guard the
// call site relied on before the extraction. Unit tests that do not stand
// up a Plugin leave dhcpManager.plugin nil, and a refusal is still a
// refusal when there is no counter to bump.
func TestNoteDNSPropagationPIDMismatch_SurvivesANilPlugin(t *testing.T) {
	m := &dhcpManager{}
	m.noteDNSPropagationPIDMismatch(errPIDNotContainer)
}
