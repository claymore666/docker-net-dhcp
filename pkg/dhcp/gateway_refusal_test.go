// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import "testing"

// TestBuildEvent_RefusesAnUnparseableGateway pins #728 at the ingress.
//
// Option 3 was the one path into Gateway that was not ParseIP-validated
// on the way in: the option-121 default gateway comes out of
// parseClasslessRoutes, which parses every address it returns, while
// this one is `strings.Fields(new_routers)[0]`, taken verbatim.
//
// What makes it dangerous downstream is that net.ParseIP's nil is not a
// refusal — it is a valid netlink argument meaning "no gateway", and
// reconcileDefaultRoute hands it to RouteAdd/RouteReplace. The result is
// `default dev ethX scope link`: an on-link default route, which makes
// the container ARP for every off-net destination.
//
// So the assertion is not "the value was rejected" but "Gateway is
// EMPTY", because empty is the state the sink already handles by
// leaving the container's existing route alone. A refusal that produced
// any other value would be a refusal the sink cannot act on.
func TestBuildEvent_RefusesAnUnparseableGateway(t *testing.T) {
	cases := []struct {
		name        string
		routers     string
		wantGateway string
		wantDropped int
	}{
		{
			name:        "an_ordinary_gateway_is_kept",
			routers:     "192.168.0.1",
			wantGateway: "192.168.0.1",
			wantDropped: 0,
		},
		{
			name: "the_first_of_several_is_kept",
			// dhcpcd exports routers as a space-separated list and the
			// plugin applies a single default route.
			routers:     "192.168.0.1 192.168.0.2",
			wantGateway: "192.168.0.1",
			wantDropped: 0,
		},
		{
			name:        "a_hostname_is_refused",
			routers:     "gateway.local",
			wantGateway: "",
			wantDropped: 1,
		},
		{
			name:        "a_truncated_address_is_refused",
			routers:     "192.168.0.",
			wantGateway: "",
			wantDropped: 1,
		},
		{
			name: "a_cidr_is_refused",
			// ParseIP rejects a prefix, and a prefix reaching netlink as
			// a gateway is nonsense either way.
			routers:     "192.168.0.1/24",
			wantGateway: "",
			wantDropped: 1,
		},
		{
			name: "a_shell_looking_value_is_refused",
			// Not because it is shell — nothing here is a shell — but
			// because it is the shape an operator will actually see in
			// a log when something upstream is wrong.
			routers:     "$(id)",
			wantGateway: "",
			wantDropped: 1,
		},
		{
			name: "an_ipv6_router_is_kept",
			// ParseIP accepts it, and this is the v4 path only because
			// reconcileDefaultRoute skips v6; refusing it here would be
			// this function inventing a rule its caller does not have.
			routers:     "fe80::1",
			wantGateway: "fe80::1",
			wantDropped: 0,
		},
		{
			name:        "no_routers_at_all_is_not_a_drop",
			routers:     "",
			wantGateway: "",
			wantDropped: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"new_ip_address":      "192.168.0.50",
				"new_subnet_mask":     "255.255.255.0",
				"new_routers":         tc.routers,
				"new_dhcp_lease_time": "3600",
			}
			ev, ok := BuildEvent("BOUND", func(k string) string { return env[k] })
			if !ok {
				t.Fatalf("BuildEvent(BOUND) did not emit")
			}

			if ev.Data.Gateway != tc.wantGateway {
				t.Errorf("Gateway = %q, want %q — an unparseable gateway must "+
					"leave Gateway EMPTY, which is the state reconcileDefaultRoute "+
					"already handles by leaving the existing route alone; any "+
					"other value reaches netlink as `Gw: nil`, i.e. an on-link "+
					"default route", ev.Data.Gateway, tc.wantGateway)
			}
			if ev.UnsafeValuesDropped != tc.wantDropped {
				t.Errorf("UnsafeValuesDropped = %d, want %d — a drop that leaves "+
					"no trace is indistinguishable from a value that was never sent",
					ev.UnsafeValuesDropped, tc.wantDropped)
			}
		})
	}
}

// TestBuildEvent_Option121GatewayStillWins guards the direction the fix
// could break.
//
// RFC 3442: an option-121 default route supersedes option 3. That
// gateway is already parsed by parseClasslessRoutes, so the new refusal
// must not touch it — a guard that refused both would silently drop
// every classless default route, which is the failure mode that is
// invisible until somebody's traffic stops.
func TestBuildEvent_Option121GatewayStillWins(t *testing.T) {
	env := map[string]string{
		"new_ip_address":  "192.168.0.50",
		"new_subnet_mask": "255.255.255.0",
		// Option 3 says one thing, option 121's default route says
		// another. 121 wins.
		"new_routers":                 "192.168.0.1",
		"new_classless_static_routes": "0.0.0.0/0 192.168.0.254",
		"new_dhcp_lease_time":         "3600",
	}
	ev, ok := BuildEvent("BOUND", func(k string) string { return env[k] })
	if !ok {
		t.Fatal("BuildEvent(BOUND) did not emit")
	}
	if ev.Data.Gateway != "192.168.0.254" {
		t.Errorf("Gateway = %q, want 192.168.0.254 — the option-121 default "+
			"route supersedes option 3 (RFC 3442) and the new refusal must "+
			"not disturb it", ev.Data.Gateway)
	}
	if ev.UnsafeValuesDropped != 0 {
		t.Errorf("UnsafeValuesDropped = %d, want 0", ev.UnsafeValuesDropped)
	}
}
