// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The IsRouter verdict is driven here rather than only through a
// container, for the reason the route verdict beside it already gives:
// logic reachable only from the heavy lane is found broken ten minutes
// later and one layer away from the line at fault.
//
// Every case below states what it PROVES, and the pass cases are as
// load-bearing as the fail cases -- an assertion that fires on
// everything is not an assertion, it is an outage.
func TestAssertGatewayIsRouter(t *testing.T) {
	const gw = "fe80::d86b:d4ff:fe29:204a"

	// The table this fixture actually produces, both ways round. The
	// R=0 line is what run 33208729673 recorded a millisecond before
	// each default-route deletion.
	const withRouter = "fe80::d86b:d4ff:fe29:204a dev eth0 lladdr da:6b:d4:29:20:4a router STALE\n" +
		"fe80::1 dev eth0 lladdr 02:42:ac:11:00:01 STALE\n"
	const withoutRouter = "fe80::d86b:d4ff:fe29:204a dev eth0 lladdr da:6b:d4:29:20:4a STALE\n" +
		"fe80::1 dev eth0 lladdr 02:42:ac:11:00:01 router STALE\n"

	cases := []struct {
		name    string
		neigh   string
		gateway string
		wantErr bool
		proves  string
	}{
		{
			name: "gateway marked router", neigh: withRouter, gateway: gw, wantErr: false,
			proves: "the PRESERVATION control: the corrected fixture must pass. Without " +
				"this case a verdict that always failed would look like a working guard.",
		},
		{
			name: "gateway not marked router", neigh: withoutRouter, gateway: gw, wantErr: true,
			proves: "the defect itself: the observed R=0 table is rejected.",
		},
		{
			name: "router marked on a DIFFERENT neighbour", neigh: withoutRouter, gateway: gw, wantErr: true,
			proves: "the word `router` is not matched anywhere in the table -- it must " +
				"be on the GATEWAY's line. A substring search over the whole table " +
				"would pass this and is the obvious wrong implementation.",
		},
		{
			name: "gateway absent from the table", neigh: "fe80::1 dev eth0 router STALE\n", gateway: gw, wantErr: true,
			proves: "absent is not a pass. A missing entry says nothing about IsRouter " +
				"either way, and silence must not read as success.",
		},
		{
			name: "empty table", neigh: "", gateway: gw, wantErr: true,
			proves: "the same, at the boundary where the instrument collected nothing.",
		},
		{
			name: "no gateway resolved", neigh: withRouter, gateway: "", wantErr: true,
			proves: "an unresolved gateway is refused rather than skipped. Otherwise a " +
				"broken route parse would silently disarm this verdict.",
		},
		{
			name: "gateway is a PREFIX of another entry", gateway: "fe80::1",
			neigh: "fe80::11 dev eth0 lladdr 02:42:ac:11:00:02 router STALE\n",
			proves: "the address is matched as a whole field, not by prefix. `fe80::1` " +
				"must not be satisfied by `fe80::11` being a router.",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r recordingReporter
			AssertGatewayIsRouter(&r, tc.neigh, tc.gateway)
			got := r.failed
			if got != tc.wantErr {
				t.Errorf("wantErr=%v got=%v — this case proves %s\nerrors: %v",
					tc.wantErr, got, tc.proves, r.msgs)
			}
		})
	}
}

func TestV6DefaultGateway(t *testing.T) {
	cases := []struct {
		name, in, want, proves string
	}{
		{
			name: "default via", want: "fe80::d86b:d4ff:fe29:204a",
			in: "fd00:6470:6865:99::/64 dev eth0 proto ra metric 256 expires 1799sec\n" +
				"default via fe80::d86b:d4ff:fe29:204a dev eth0 proto ra metric 1024 expires 1799sec\n",
			proves: "the gateway is read off the default line, not the first line.",
		},
		{
			name: "no default route", in: "fd00::/64 dev eth0 proto ra metric 256\n", want: "",
			proves: "absence returns empty, which the caller must treat as unknown.",
		},
		{
			name: "empty input", in: "", want: "",
			proves: "the boundary where nothing was collected.",
		},
		{
			name: "default without via", in: "default dev eth0 metric 1024\n", want: "",
			proves: "a device-scope default has no gateway address to check, and must " +
				"not yield the device name as one.",
		},
		{
			name: "a route to a prefix merely NAMED default-ish", want: "",
			in:     "defaultish via fe80::1 dev eth0\n",
			proves: "the first field is matched whole, not by prefix.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := V6DefaultGateway(tc.in); got != tc.want {
				t.Errorf("got %q want %q — this case proves %s", got, tc.want, tc.proves)
			}
		})
	}
}

// The restore's verification is driven here because the failure that
// matters most cannot be produced in CI on demand: a restore that
// reports success and reinstates NOTHING. Reaching that state for real
// needs a router advertisement, a purge and a netlink call that lies.
// Reaching it here needs a map.
func TestRoutesMissingFrom(t *testing.T) {
	mk := func(gw string, idx int) netlink.Route {
		return netlink.Route{
			LinkIndex: idx,
			Dst:       &net.IPNet{IP: net.ParseIP("::"), Mask: net.CIDRMask(0, 128)},
			Gw:        net.ParseIP(gw),
			Protocol:  unix.RTPROT_RA,
		}
	}
	a := mk("fe80::a893:a7ff:fe55:6c2d", 14)
	b := mk("fe80::a893:a7ff:fe55:6c2d", 16)
	set := func(rs ...netlink.Route) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r.String()] = true
		}
		return m
	}

	cases := []struct {
		name   string
		before []netlink.Route
		now    map[string]bool
		want   int
		proves string
	}{
		{
			name: "the restore silently no-opped", before: []netlink.Route{a, b},
			now: set(), want: 2,
			proves: "THE CASE THIS EXISTS FOR. netlink reported success, the table has " +
				"nothing, and the verification must report both routes missing. A " +
				"check that trusted the return code would pass here.",
		},
		{
			name: "the restore worked", before: []netlink.Route{a, b},
			now: set(a, b), want: 0,
			proves: "the PRESERVATION control: a real restore must not be reported as a " +
				"failure, or the fixture refuses every run and the guard is an outage.",
		},
		{
			name: "the restore was partial", before: []netlink.Route{a, b},
			now: set(a), want: 1,
			proves: "a count, not a boolean. One of two restored must not read as " +
				"success — this is what a `len(now) > 0` check would get wrong.",
		},
		{
			name: "nothing was purged", before: nil, now: set(a), want: 0,
			proves: "an empty snapshot has nothing to verify and must not fabricate a " +
				"failure from routes it never took.",
		},
		{
			name:   "a DIFFERENT route appeared in place of the purged ones",
			before: []netlink.Route{a, b}, now: set(mk("fe80::dead", 14)), want: 2,
			proves: "identity is per-route. A namespace that has *a* default route is " +
				"not a namespace that has the ones that were taken, and a count-only " +
				"check would call this restored.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(routesMissingFrom(tc.before, tc.now)); got != tc.want {
				t.Errorf("got %d missing, want %d — this case proves %s", got, tc.want, tc.proves)
			}
		})
	}
}
