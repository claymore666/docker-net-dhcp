// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import "testing"

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
