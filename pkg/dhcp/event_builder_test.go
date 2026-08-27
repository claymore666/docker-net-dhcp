// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"reflect"
	"testing"
)

// fakeEnv builds a Getenv closure over a fixed map. Returns the
// empty string for any key not present, matching os.Getenv's
// "missing variable" contract.
func fakeEnv(m map[string]string) Getenv {
	return func(k string) string {
		return m[k]
	}
}

func TestBuildEvent_BoundV4_AllOptions(t *testing.T) {
	env := fakeEnv(map[string]string{
		"new_ip_address":          "192.168.99.10",
		"new_subnet_cidr":         "24",
		"new_routers":             "192.168.99.1",
		"new_domain_name":         "corp.example",
		"new_domain_name_servers": "192.168.99.53 192.168.99.54",
		"new_ntp_servers":         "192.168.99.123 192.168.99.124",
		"new_domain_search":       "corp.example internal.example",
		"new_tftp_server_name":    "tftp.example.test",
		"new_bootfile_name":       "pxelinux.0",
		"new_interface_mtu":       "1400",
		"new_wpad":                "http://wpad.corp.example/wpad.dat",
		"new_posix_timezone":      "CET-1CEST,M3.5.0,M10.5.0/3",
		"new_tzdb_timezone":       "Europe/Berlin",
		"new_time_offset":         "3600",
	})

	got, emit := BuildEvent("BOUND", env)
	if !emit {
		t.Fatalf("emit=false on a well-formed BOUND event")
	}
	want := Event{
		Type: "bound",
		Data: Info{
			IP:            "192.168.99.10/24",
			Gateway:       "192.168.99.1",
			Domain:        "corp.example",
			DNSServers:    []string{"192.168.99.53", "192.168.99.54"},
			MTU:           1400,
			NTPServers:    []string{"192.168.99.123", "192.168.99.124"},
			SearchList:    []string{"corp.example", "internal.example"},
			TFTPServer:    "tftp.example.test",
			BootFile:      "pxelinux.0",
			WPAD:          "http://wpad.corp.example/wpad.dat",
			PosixTimezone: "CET-1CEST,M3.5.0,M10.5.0/3",
			TZDBTimezone:  "Europe/Berlin",
			TimeOffset:    "3600",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Event mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestBuildEvent_BoundV4_InformationalOptionsAbsent: the WPAD/timezone
// extras (#262) are observe-only and optional — absent env vars leave
// them empty, never blocking the event.
func TestBuildEvent_BoundV4_InformationalOptionsAbsent(t *testing.T) {
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "192.168.99.10",
		"new_subnet_cidr": "24",
		"new_routers":     "192.168.99.1",
	}))
	if !emit {
		t.Fatalf("emit=false on a minimal BOUND event")
	}
	if got.Data.WPAD != "" || got.Data.PosixTimezone != "" || got.Data.TZDBTimezone != "" || got.Data.TimeOffset != "" {
		t.Errorf("informational extras should be empty when unset, got %+v", got.Data)
	}
}

// TestBuildEvent_BoundV4_DottedMaskDerivesPrefix: when dhcpcd omits
// new_subnet_cidr, the prefix length is derived from the dotted-quad
// new_subnet_mask.
func TestBuildEvent_BoundV4_DottedMaskDerivesPrefix(t *testing.T) {
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "192.168.99.10",
		"new_subnet_mask": "255.255.255.0",
		"new_routers":     "192.168.99.1",
	}))
	if !emit {
		t.Fatalf("emit=false deriving prefix from dotted mask")
	}
	if got.Data.IP != "192.168.99.10/24" {
		t.Errorf("IP = %q, want 192.168.99.10/24 derived from dotted mask", got.Data.IP)
	}
}

// TestBuildEvent_BoundV4_MultipleRoutersTakesFirst: dhcpcd exports the
// routers option as a space-separated list; the plugin applies one
// default route, so the first entry wins.
func TestBuildEvent_BoundV4_MultipleRoutersTakesFirst(t *testing.T) {
	got, _ := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "10.0.0.5",
		"new_subnet_cidr": "24",
		"new_routers":     "10.0.0.1 10.0.0.2",
	}))
	if got.Data.Gateway != "10.0.0.1" {
		t.Errorf("Gateway = %q, want first of the routers list", got.Data.Gateway)
	}
}

func TestBuildEvent_RenewBehavesAsBound(t *testing.T) {
	env := fakeEnv(map[string]string{
		"new_ip_address":  "10.0.0.5",
		"new_subnet_cidr": "16",
		"new_routers":     "10.0.0.1",
	})

	got, emit := BuildEvent("RENEW", env)
	if !emit {
		t.Fatalf("emit=false on RENEW")
	}
	if got.Type != "renew" || got.Data.IP != "10.0.0.5/16" || got.Data.Gateway != "10.0.0.1" {
		t.Errorf("RENEW should populate the same v4 fields as BOUND; got %+v", got)
	}
}

// TestBuildEvent_RebindMapsToRenew: a REBIND re-applies a possibly
// changed address, which is exactly the renew path's job.
func TestBuildEvent_RebindMapsToRenew(t *testing.T) {
	got, emit := BuildEvent("REBIND", fakeEnv(map[string]string{
		"new_ip_address":  "10.0.0.9",
		"new_subnet_cidr": "24",
	}))
	if !emit || got.Type != "renew" {
		t.Errorf("REBIND should map to renew; got type=%q emit=%v", got.Type, emit)
	}
}

func TestBuildEvent_BoundV6_CanonicalisesIPAndCapturesDNS6(t *testing.T) {
	env := fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "2001:db8::1",
		"new_dhcp6_name_servers":    "2001:db8::53 2001:db8::54",
	})

	got, emit := BuildEvent("BOUND6", env)
	if !emit {
		t.Fatalf("emit=false on BOUND6")
	}
	if got.Type != "bound" {
		t.Errorf("BOUND6 Type = %q, want bound", got.Type)
	}
	if got.Data.IP != "2001:db8::1/128" {
		t.Errorf("v6 IP not canonicalised to /128: got %q", got.Data.IP)
	}
	if !reflect.DeepEqual(got.Data.DNSServers, []string{"2001:db8::53", "2001:db8::54"}) {
		t.Errorf("v6 DNSServers wrong: %+v", got.Data.DNSServers)
	}
	// v6 path must not populate any of the v4-only fields. SearchList
	// left this list in #815: option 24 is a real DHCPv6 option and the
	// v6 branch now reads it, so its absence here is evidence that THIS
	// fixture sent none — not that the family cannot carry one. See
	// TestBuildEvent_BoundV6CapturesTheSearchList for the other half.
	if got.Data.Gateway != "" || got.Data.Domain != "" || len(got.Data.NTPServers) > 0 ||
		got.Data.TFTPServer != "" || got.Data.BootFile != "" || got.Data.MTU != 0 {
		t.Errorf("v6 path leaked v4-only fields: %+v", got.Data)
	}
	if len(got.Data.SearchList) > 0 {
		t.Errorf("SearchList set from an environment that carried none: %+v", got.Data.SearchList)
	}
}

func TestBuildEvent_RenewV6MapsToRenew(t *testing.T) {
	got, emit := BuildEvent("RENEW6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "fd00::10",
	}))
	if !emit || got.Type != "renew" || got.Data.IP != "fd00::10/128" {
		t.Errorf("RENEW6 should renew the v6 address; got %+v emit=%v", got, emit)
	}
}

func TestBuildEvent_BoundV6_StripsExistingMaskBeforeCanonicalising(t *testing.T) {
	// Defensive against a dhcpcd build that emits CIDR form.
	got, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "2001:db8::42/64",
	}))
	if !emit {
		t.Fatalf("emit=false on CIDR-form v6 address")
	}
	if got.Data.IP != "2001:db8::42/128" {
		t.Errorf("v6 with embedded mask should be canonicalised to /128: got %q", got.Data.IP)
	}
}

func TestBuildEvent_BoundV6_MalformedSkipsEvent(t *testing.T) {
	// A misbehaving client or hostile lease must not bring down the
	// renewal path — the handler skips the event and the persistent
	// client retries on the next one.
	if _, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "not-an-ip",
	})); emit {
		t.Errorf("emit=true on a malformed v6 address — should have been skipped")
	}
}

func TestBuildEvent_BoundV6_MissingAddressSkipsEvent(t *testing.T) {
	if _, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_name_servers": "2001:db8::53",
	})); emit {
		t.Errorf("emit=true on a v6 event with no IA_NA address — should have been skipped")
	}
}

func TestBuildEvent_MTUParseFailureIsBestEffort(t *testing.T) {
	// A garbage MTU must not block IP propagation; the rest of the
	// event still flows through with MTU == 0.
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":    "192.168.0.10",
		"new_subnet_cidr":   "24",
		"new_interface_mtu": "not-a-number",
	}))
	if !emit {
		t.Fatalf("emit=false on garbage-mtu — should still emit IP info")
	}
	if got.Data.MTU != 0 {
		t.Errorf("MTU should be 0 on parse failure; got %d", got.Data.MTU)
	}
	if got.Data.IP != "192.168.0.10/24" {
		t.Errorf("IP propagation broken by bad mtu: %q", got.Data.IP)
	}
}

func TestBuildEvent_MTUZeroIsTreatedAsAbsent(t *testing.T) {
	got, _ := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":    "192.168.0.10",
		"new_subnet_cidr":   "24",
		"new_interface_mtu": "0",
	}))
	if got.Data.MTU != 0 {
		t.Errorf("MTU=%d, want 0 for present-but-zero raw value", got.Data.MTU)
	}
}

func TestBuildEvent_MTUNegativeIsIgnored(t *testing.T) {
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address": "10.0.0.5", "new_subnet_cidr": "24", "new_interface_mtu": "-5",
	}))
	if !emit {
		t.Fatal("emit=false; MTU problems must not kill the event")
	}
	if got.Data.MTU != 0 {
		t.Errorf("MTU = %d, want 0 (negative input ignored)", got.Data.MTU)
	}
}

func TestBuildEvent_LeaseLossEvents_EmitTypeOnly(t *testing.T) {
	cases := map[string]string{
		"NAK":      "nak",
		"EXPIRE":   "leasefail",
		"TIMEOUT":  "leasefail",
		"EXPIRE6":  "leasefail",
		"TIMEOUT6": "leasefail",
	}
	for reason, wantType := range cases {
		t.Run(reason, func(t *testing.T) {
			got, emit := BuildEvent(reason, fakeEnv(map[string]string{
				// These should be ignored — the lease-loss path must
				// not pull v4/v6 fields off env.
				"new_ip_address":            "192.168.0.10",
				"new_subnet_cidr":           "24",
				"new_dhcp6_ia_na1_ia_addr1": "2001:db8::1",
			}))
			if !emit {
				t.Fatalf("emit=false on lease-loss event %q", reason)
			}
			if got.Type != wantType {
				t.Errorf("Type = %q, want %q", got.Type, wantType)
			}
			if !reflect.DeepEqual(got.Data, Info{}) {
				t.Errorf("lease-loss event leaked Data: %+v", got.Data)
			}
		})
	}
}

// TestBuildEvent_UnactionedReasonsSkipped: dhcpcd fires the hook for
// many transitions we don't act on; all must be suppressed.
//
// INFORM6 was in this list until #815 and that was the defect, not the
// contract: the list was written from "what does the plugin currently
// act on" rather than from "what should it act on", so the one reason a
// stateless network ever fires was pinned as correctly ignored. It is
// now covered by TestBuildEvent_Inform6ProducesConfigWithoutAddress.
//
// INFORM (v4) stays. dhcpcd only fires it under -I, which renderArgs
// never passes, so it is a reason no plugin input can produce.
//
// ROUTERADVERT stays too, and its presence here now says something
// narrower than it used to (#868). The fakeEnv below does not set
// NETDHCP_EMIT_RA, so this list is the claim about the DEFAULT scope --
// the persistent renewal client, whose stream is what #815 protected.
// The one-shot acquisition client opts in, and that path has its own
// coverage in TestBuildEvent_RouterAdvertEmitsOnlyWhenOptedIn and
// TestBuildEvent_RouterAdvertCarriesTheFlags. Reading this list as
// "ROUTERADVERT is never an event" would be reading it wider than it
// is measured.
func TestBuildEvent_UnactionedReasonsSkipped(t *testing.T) {
	for _, reason := range []string{
		"PREINIT", "CARRIER", "NOCARRIER", "ROUTERADVERT", "STOP", "STOP6",
		"STOPPED", "DEPARTED", "FAIL", "TEST", "IPV4LL", "STATIC", "3RDPARTY",
		"DELEGATED6", "RECONFIGURE", "INFORM", "",
		"definitely-not-a-real-reason",
	} {
		t.Run(reason, func(t *testing.T) {
			if _, emit := BuildEvent(reason, fakeEnv(map[string]string{
				"new_ip_address":  "10.0.0.5",
				"new_subnet_cidr": "24",
			})); emit {
				t.Errorf("emit=true on un-actioned reason %q — should have been skipped", reason)
			}
		})
	}
}

func TestBuildEvent_BoundV4_OmittedOptionsAreEmpty(t *testing.T) {
	// A minimal lease — only address + mask. Every other field stays at
	// zero/empty, json:",omitempty" then keeps them out of the
	// downstream JSON. Pins that Getenv("missing") returning "" doesn't
	// accidentally insert an empty-string entry into a slice via
	// strings.Fields.
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "10.0.0.5",
		"new_subnet_cidr": "8",
	}))
	if !emit {
		t.Fatalf("emit=false on minimal BOUND")
	}
	if len(got.Data.DNSServers) != 0 || len(got.Data.NTPServers) != 0 || len(got.Data.SearchList) != 0 {
		t.Errorf("missing env vars produced non-empty slices: %+v", got.Data)
	}
	if got.Data.Gateway != "" {
		t.Errorf("absent new_routers should leave Gateway empty; got %q", got.Data.Gateway)
	}
}

// TestBuildEvent_BoundV4_MalformedLeaseIsSkipped pins the #128
// hardening: a BOUND/RENEW whose address/mask doesn't form a valid
// CIDR is dropped at the handler instead of flowing downstream where
// netlink.ParseAddr would fail mid-renewal.
func TestBuildEvent_BoundV4_MalformedLeaseIsSkipped(t *testing.T) {
	cases := map[string]map[string]string{
		"empty ip, valid cidr": {"new_ip_address": "", "new_subnet_cidr": "24"},
		"valid ip, no mask":    {"new_ip_address": "10.0.0.5"},
		"non-numeric cidr":     {"new_ip_address": "10.0.0.5", "new_subnet_cidr": "abc"},
		"cidr out of range":    {"new_ip_address": "10.0.0.5", "new_subnet_cidr": "33"},
		"garbage ip":           {"new_ip_address": "not-an-ip", "new_subnet_cidr": "24"},
		"garbage dotted mask":  {"new_ip_address": "10.0.0.5", "new_subnet_mask": "not-a-mask"},
		"non-contiguous mask":  {"new_ip_address": "10.0.0.5", "new_subnet_mask": "255.0.255.0"},
		"nothing set":          {},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, emit := BuildEvent("BOUND", fakeEnv(env)); emit {
				t.Errorf("emit=true for malformed v4 lease env %v; want skipped", env)
			}
		})
	}
}

// TestBuildEvent_RenewValidatesLikeBound: the validation guards both
// event types that carry lease data.
func TestBuildEvent_RenewValidatesLikeBound(t *testing.T) {
	if _, emit := BuildEvent("RENEW", fakeEnv(map[string]string{"new_ip_address": "", "new_subnet_cidr": "24"})); emit {
		t.Error("emit=true for malformed RENEW; want skipped")
	}
}

// TestBuildEvent_BoundV6_LinkLocalIsEmitted pins that a link-local v6
// lease flows through — filtering link-local is a consumer policy
// decision, not the handler's.
func TestBuildEvent_BoundV6_LinkLocalIsEmitted(t *testing.T) {
	got, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "fe80::42:acff:fe00:1",
	}))
	if !emit {
		t.Fatal("emit=false for link-local v6 lease")
	}
	if got.Data.IP != "fe80::42:acff:fe00:1/128" {
		t.Errorf("IP = %q, want canonicalised /128 link-local", got.Data.IP)
	}
}

// TestBuildEvent_BoundV6_UncompressedIsCanonicalised: an uncompressed
// address must canonicalise to the compressed form so downstream string
// comparisons are stable.
func TestBuildEvent_BoundV6_UncompressedIsCanonicalised(t *testing.T) {
	got, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "fd00:6470:6863:0000:0000:0000:0000:0010",
	}))
	if !emit {
		t.Fatal("emit=false for uncompressed v6 lease")
	}
	if got.Data.IP != "fd00:6470:6863::10/128" {
		t.Errorf("IP = %q, want compressed canonical form", got.Data.IP)
	}
}

// TestBuildEvent_BoundV6_MultipleDNS6Servers: option 23 with several
// servers arrives space-separated; each becomes one entry.
func TestBuildEvent_BoundV6_MultipleDNS6Servers(t *testing.T) {
	got, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "fd00::10",
		"new_dhcp6_name_servers":    "fd00::53 fd00::54",
	}))
	if !emit {
		t.Fatal("emit=false")
	}
	want := []string{"fd00::53", "fd00::54"}
	if !reflect.DeepEqual(got.Data.DNSServers, want) {
		t.Errorf("DNSServers = %v, want %v", got.Data.DNSServers, want)
	}
}

// Option 121 (classless static routes, RFC 3442).

func TestBuildEvent_ClasslessRoutes_NextHopAndOnLink(t *testing.T) {
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "192.168.99.10",
		"new_subnet_cidr": "24",
		"new_routers":     "192.168.99.1",
		// next-hop route, then an on-link route (gateway 0.0.0.0).
		"new_classless_static_routes": "10.0.0.0/8 192.168.99.2 172.16.0.0/12 0.0.0.0",
	}))
	if !emit {
		t.Fatal("emit=false on a well-formed BOUND event")
	}
	want := []Route{
		{Destination: "10.0.0.0/8", Gateway: "192.168.99.2"},
		{Destination: "172.16.0.0/12"}, // on-link: empty gateway
	}
	if !reflect.DeepEqual(got.Data.Routes, want) {
		t.Errorf("Routes = %+v, want %+v", got.Data.Routes, want)
	}
	// No opt-121 default route present, so option 3 still sets the gateway.
	if got.Data.Gateway != "192.168.99.1" {
		t.Errorf("Gateway = %q, want the option-3 router", got.Data.Gateway)
	}
}

func TestBuildEvent_ClasslessDefaultRoute_SupersedesRouters(t *testing.T) {
	got, _ := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "192.168.99.10",
		"new_subnet_cidr": "24",
		"new_routers":     "192.168.99.1",
		// A 0.0.0.0/0 entry must win over new_routers per RFC 3442, and
		// must NOT appear among the static routes.
		"new_classless_static_routes": "0.0.0.0/0 192.168.99.254 10.0.0.0/8 192.168.99.2",
	}))
	if got.Data.Gateway != "192.168.99.254" {
		t.Errorf("Gateway = %q, want the opt-121 default route to supersede routers", got.Data.Gateway)
	}
	want := []Route{{Destination: "10.0.0.0/8", Gateway: "192.168.99.2"}}
	if !reflect.DeepEqual(got.Data.Routes, want) {
		t.Errorf("Routes = %+v, want only the non-default route %+v", got.Data.Routes, want)
	}
}

func TestBuildEvent_ClasslessRoutes_MalformedEntriesSkippedBestEffort(t *testing.T) {
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "192.168.99.10",
		"new_subnet_cidr": "24",
		// bad destination, bad gateway, then a valid route, then an odd
		// trailing token — none may drop the event or the valid route.
		"new_classless_static_routes": "not-a-cidr 192.168.99.2 10.0.0.0/8 not-an-ip 192.168.50.0/24 192.168.99.3 192.168.60.0/24",
	}))
	if !emit {
		t.Fatal("a malformed route must not drop the whole lease event")
	}
	want := []Route{{Destination: "192.168.50.0/24", Gateway: "192.168.99.3"}}
	if !reflect.DeepEqual(got.Data.Routes, want) {
		t.Errorf("Routes = %+v, want only the one valid route %+v", got.Data.Routes, want)
	}
}

func TestBuildEvent_NoClasslessRoutes_LeavesRoutesNilAndKeepsRouters(t *testing.T) {
	got, _ := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":  "192.168.99.10",
		"new_subnet_cidr": "24",
		"new_routers":     "192.168.99.1",
	}))
	if got.Data.Routes != nil {
		t.Errorf("Routes = %+v, want nil when option 121 is absent", got.Data.Routes)
	}
	if got.Data.Gateway != "192.168.99.1" {
		t.Errorf("Gateway = %q, want the option-3 router unchanged", got.Data.Gateway)
	}
}

// Lease lifetime capture (#353). The plugin's outage detection derives
// its deadline from these, because dhcpcd under --noconfigure never
// tells us a lease lapsed.

func TestBuildEvent_BoundV4_CapturesLeaseSeconds(t *testing.T) {
	got, emit := BuildEvent("BOUND", fakeEnv(map[string]string{
		"new_ip_address":        "192.168.99.10",
		"new_subnet_cidr":       "24",
		"new_dhcp_lease_time":   "86400",
		"new_dhcp_renewal_time": "43200",
	}))
	if !emit {
		t.Fatal("emit=false on a well-formed BOUND event")
	}
	if got.Data.LeaseSeconds != 86400 {
		t.Errorf("LeaseSeconds = %d, want 86400", got.Data.LeaseSeconds)
	}
}

func TestBuildEvent_BoundV6_CapturesValidLifetime(t *testing.T) {
	got, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1":        "fd00:9::1f",
		"new_dhcp6_ia_na1_ia_addr1_vltime": "120",
	}))
	if !emit {
		t.Fatal("emit=false on a well-formed BOUND6 event")
	}
	if got.Data.LeaseSeconds != 120 {
		t.Errorf("LeaseSeconds = %d, want the IA_NA valid lifetime 120", got.Data.LeaseSeconds)
	}
}

func TestBuildEvent_UnusableLeaseTimeIsZeroNotFatal(t *testing.T) {
	// A server that omits or mangles the lifetime must cost us the
	// deadline, never the lease event itself — the consumer treats 0 as
	// "no deadline known" and falls back to event-driven detection.
	for _, raw := range []string{"", "not-a-number", "0", "-5"} {
		t.Run("lease="+raw, func(t *testing.T) {
			env := map[string]string{
				"new_ip_address":  "192.168.99.10",
				"new_subnet_cidr": "24",
			}
			if raw != "" {
				env["new_dhcp_lease_time"] = raw
			}
			got, emit := BuildEvent("BOUND", fakeEnv(env))
			if !emit {
				t.Fatalf("emit=false — an unusable lease time dropped the whole event")
			}
			if got.Data.LeaseSeconds != 0 {
				t.Errorf("LeaseSeconds = %d, want 0 for %q", got.Data.LeaseSeconds, raw)
			}
		})
	}
}

func TestBuildEvent_ReleaseIsNotALeaseLoss(t *testing.T) {
	// This was load-bearing until #800 and is now a guard against
	// regression rather than a live constraint.
	//
	// Up to v1.8.x a lapse and a graceful stop BOTH fired RELEASE, so
	// counting it would have turned every clean teardown into a DHCP
	// failure. Both of those needed the `release` directive, which #800
	// removed — measured four ways in #855 — so no client this build
	// starts can produce RELEASE at all, and a lapse now fires EXPIRE.
	//
	// The contract is pinned anyway: if a `release` directive ever comes
	// back, RELEASE becomes ambiguous again the same day, and this test
	// is what makes that a red build rather than a silent miscount.
	for _, reason := range []string{"RELEASE", "RELEASE6"} {
		t.Run(reason, func(t *testing.T) {
			if _, emit := BuildEvent(reason, fakeEnv(map[string]string{
				"new_ip_address":  "10.0.0.5",
				"new_subnet_cidr": "24",
			})); emit {
				t.Errorf("emit=true on %q — a release is ambiguous and must not count as a lease loss", reason)
			}
		})
	}
}

// --- #815: address-less DHCPv6 configuration ----------------------------
//
// The environments below are not invented. They are the variables dhcpcd
// 10.3.2 actually exported to a hook script on a dnsmasq `ra-stateless`
// network, driven with the config renderConfig itself produces — the
// probe was derived from the subject rather than written to resemble it,
// after a hand-written config omitted the plugin's `option ...` request
// list and made the DNS servers look absent from the protocol when they
// were absent from the probe.

// The headline: an information reply carries options and no address, and
// before #815 it was dropped twice over — mapReason had no INFORM6 case,
// and the v6 branch skips any event without an IA_NA address.
func TestBuildEvent_Inform6ProducesConfigWithoutAddress(t *testing.T) {
	got, emit := BuildEvent("INFORM6", fakeEnv(map[string]string{
		"new_dhcp6_name_servers":      "fd00:6470:6863::53 fd00:6470:6863::54",
		"new_dhcp6_domain_search":     "probe.example two.example",
		"new_dhcp6_info_refresh_time": "600",
		"new_dhcp6_server_id":         "00010001322319893a2b31dc0868",
	}))
	if !emit {
		t.Fatalf("emit=false on INFORM6 — a stateless network's configuration was discarded")
	}
	if got.Type != "config" {
		t.Errorf("INFORM6 Type = %q, want config", got.Type)
	}
	if got.Data.IP != "" {
		t.Errorf("a config event must carry no address; got IP=%q", got.Data.IP)
	}
	if !reflect.DeepEqual(got.Data.DNSServers, []string{"fd00:6470:6863::53", "fd00:6470:6863::54"}) {
		t.Errorf("DNSServers = %+v", got.Data.DNSServers)
	}
	if !reflect.DeepEqual(got.Data.SearchList, []string{"probe.example", "two.example"}) {
		t.Errorf("SearchList = %+v", got.Data.SearchList)
	}
	// Nothing from the lease path may appear: a config event that
	// acquired a gateway or a lease clock would be fed to machinery that
	// has no address to go with it.
	if got.Data.Gateway != "" || got.Data.LeaseSeconds != 0 || got.Data.MTU != 0 || len(got.Data.Routes) > 0 {
		t.Errorf("config event leaked lease fields: %+v", got.Data)
	}
}

// The ordering, driven directly. The config branch must run BEFORE the
// IA_NA guard, not after it — mapping INFORM6 onto "bound" would have
// been dropped one step later by that guard. This fixture carries an
// address the config branch must ignore: if the branches were swapped,
// or the config case fell through to the v6 lease code, IP would be set.
func TestBuildEvent_Inform6IgnoresAnyAddressAndNeverReachesTheLeasePath(t *testing.T) {
	got, emit := BuildEvent("INFORM6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1":        "2001:db8::1",
		"new_dhcp6_ia_na1_ia_addr1_vltime": "120",
		"new_dhcp6_name_servers":           "2001:db8::53",
	}))
	if !emit || got.Type != "config" {
		t.Fatalf("INFORM6 should still be a config event; got type=%q emit=%v", got.Type, emit)
	}
	if got.Data.IP != "" || got.Data.LeaseSeconds != 0 {
		t.Errorf("config event took the lease path: IP=%q LeaseSeconds=%d", got.Data.IP, got.Data.LeaseSeconds)
	}
}

// A server that advertises "other configuration available" and then
// supplies none still produces an event. The counter is then the only
// evidence the exchange happened at all, and that misconfiguration is
// precisely what an operator needs to be able to see. A guard that
// emitted only when DNS was present would make it invisible again.
func TestBuildEvent_Inform6WithNoOptionsStillEmits(t *testing.T) {
	got, emit := BuildEvent("INFORM6", fakeEnv(map[string]string{
		"new_dhcp6_server_id": "00010001322319893a2b31dc0868",
	}))
	if !emit {
		t.Fatalf("emit=false on an option-less information reply — the exchange left no trace")
	}
	if got.Type != "config" || len(got.Data.DNSServers) != 0 || len(got.Data.SearchList) != 0 {
		t.Errorf("unexpected config event: %+v", got)
	}
}

// The second defect, found while measuring the first and fixed with it:
// the v6 LEASE branch read the DNS servers and not the search list, so a
// DHCPv6 network got a resolv.conf with nameservers and never a `search`
// line — on every lease, not only on an information reply. This test
// fails against the tree before #815.
func TestBuildEvent_BoundV6CapturesTheSearchList(t *testing.T) {
	got, emit := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "2001:db8::1",
		"new_dhcp6_name_servers":    "2001:db8::53",
		"new_dhcp6_domain_search":   "corp.example lab.example",
	}))
	if !emit {
		t.Fatalf("emit=false on BOUND6")
	}
	if !reflect.DeepEqual(got.Data.SearchList, []string{"corp.example", "lab.example"}) {
		t.Errorf("v6 lease dropped option 24 (domain search): %+v", got.Data.SearchList)
	}
	// DHCPv6 has no option-15 equivalent, so Domain stays empty and the
	// resolv.conf writer falls back from SearchList to Domain, not back.
	if got.Data.Domain != "" {
		t.Errorf("v6 must not set Domain: %q", got.Data.Domain)
	}
}

// v6 NTP is deliberately unread; see the comment at the site. Measured:
// with two NTP addresses configured on the server, exactly one reached
// the hook environment as new_dhcp6_ntp_server_addr, and not the first.
// Reporting one member of a longer list through a field documented as
// the server list is a false claim in an operator-facing log. This test
// pins the decision so it cannot be undone by accident; the measurement
// and the options for resolving it are #859.
func TestBuildEvent_BoundV6DoesNotReportPartialNTP(t *testing.T) {
	got, _ := BuildEvent("BOUND6", fakeEnv(map[string]string{
		"new_dhcp6_ia_na1_ia_addr1": "2001:db8::1",
		"new_dhcp6_ntp_server_addr": "2001:db8::124",
		"new_ntp_servers":           "10.0.0.1",
	}))
	if len(got.Data.NTPServers) != 0 {
		t.Errorf("v6 NTPServers should stay empty until the partial-list "+
			"semantics are decided; got %+v", got.Data.NTPServers)
	}
}

// dhcpcd fires the hook far more often than we act on it. ROUTERADVERT
// in particular arrives repeatedly on every v6 network, including the
// stateless one, and must stay outside the persistent client's event
// stream — otherwise #815's new case would have widened the domain
// rather than the map.
//
// #868 added an opt-in for ROUTERADVERT, so this test no longer proves
// what its name says on its own: without the env variable every reason
// here is dropped, which is the default-scope claim and nothing more.
// TestBuildEvent_OptingIntoRAWidensTheSetByExactlyOne is the other half
// — it runs the same list WITH the opt-in and requires the other six to
// stay dropped.
func TestBuildEvent_Inform6DidNotWidenTheReasonSet(t *testing.T) {
	for _, reason := range inform6UnwidenedReasons {
		if _, emit := BuildEvent(reason, fakeEnv(map[string]string{
			"new_dhcp6_name_servers": "2001:db8::53",
		})); emit {
			t.Errorf("reason %q became an event; only INFORM6 was added", reason)
		}
	}
}

// inform6UnwidenedReasons is shared by the two halves of the boundary
// on purpose. Two copies of this list would let the opt-in half go
// green over a smaller domain than the default half, which is exactly
// the failure the pair exists to catch.
var inform6UnwidenedReasons = []string{
	"ROUTERADVERT", "INFORM", "PREINIT", "CARRIER", "STOP6", "STOPPED", "DELEGATED6",
}

// TestBuildEvent_OptingIntoRAWidensTheSetByExactlyOne drives the same
// list as above with NETDHCP_EMIT_RA set — the one-shot acquisition
// client's configuration (#868).
//
// The opt-in must move ROUTERADVERT and nothing else. Widening
// mapReason's default arm, or keying the opt-in on anything coarser
// than the single reason, is caught here and not by either half alone.
func TestBuildEvent_OptingIntoRAWidensTheSetByExactlyOne(t *testing.T) {
	for _, reason := range inform6UnwidenedReasons {
		_, emit := BuildEvent(reason, fakeEnv(map[string]string{
			EmitRAEnv:                "1",
			"new_dhcp6_name_servers": "2001:db8::53",
		}))
		want := reason == "ROUTERADVERT"
		if emit != want {
			t.Errorf("reason %q with the opt-in set: emit=%v, want %v", reason, emit, want)
		}
	}
}

// TestBuildEvent_RouterAdvertEmitsOnlyWhenOptedIn pins the gate itself
// rather than the reason set: the SAME reason and the SAME flags,
// differing only in whether the one-shot client's env variable is
// present.
//
// Without this, mapReason returning `true` unconditionally for
// ROUTERADVERT would still pass TestBuildEvent_RouterAdvertCarriesTheFlags
// below, and the only thing that would go red is the default-scope list
// — which reads as a change to #815's contract rather than as a broken
// gate.
func TestBuildEvent_RouterAdvertEmitsOnlyWhenOptedIn(t *testing.T) {
	base := map[string]string{"nd1_flags": "MO"}

	if _, emit := BuildEvent("ROUTERADVERT", fakeEnv(base)); emit {
		t.Error("ROUTERADVERT emitted without the opt-in; #815's persistent stream would carry every RA on the segment")
	}

	optedIn := map[string]string{"nd1_flags": "MO", EmitRAEnv: "1"}
	if _, emit := BuildEvent("ROUTERADVERT", fakeEnv(optedIn)); !emit {
		t.Error("ROUTERADVERT dropped WITH the opt-in; the acquisition client would see no advertisement and #868 would be unfixed")
	}
}

// TestBuildEvent_RouterAdvertCarriesTheFlags covers the three flag
// spellings dhcpcd exports, measured against dhcpcd 10.3.2 and dnsmasq
// 2.92 with one mode per network namespace:
//
//	managed (--dhcp-range=<pool> --enable-ra)              nd1_flags=MO
//	stateless (--dhcp-range=<prefix>,ra-stateless)         nd1_flags=O
//	SLAAC (--dhcp-range=<prefix>,ra-only)                  nd1_flags= (empty)
//
// The fourth mode — no router at all — is deliberately absent from this
// table: it fires no ROUTERADVERT, so there is no builder input to
// assert on. Its coverage lives one layer up, where the acquisition
// client distinguishes "an advertisement said no DHCPv6" from "nothing
// advertised", because that is the layer that can observe an absence.
//
// The empty case is the one worth stating: an RA with neither flag is
// SLAAC, so an empty RouterFlags on an emitted routeradvert event means
// "no flags were set", never "no advertisement". The event's existence
// carries that.
func TestBuildEvent_RouterAdvertCarriesTheFlags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags string
	}{
		{"managed", "MO"},
		{"stateless", "O"},
		{"slaac", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{EmitRAEnv: "1"}
			if tc.flags != "" {
				env["nd1_flags"] = tc.flags
			}

			got, emit := BuildEvent("ROUTERADVERT", fakeEnv(env))
			if !emit {
				t.Fatal("ROUTERADVERT with the opt-in did not emit")
			}
			if got.Type != "routeradvert" {
				t.Errorf("Type = %q, want %q", got.Type, "routeradvert")
			}
			if got.RouterFlags != tc.flags {
				t.Errorf("RouterFlags = %q, want %q", got.RouterFlags, tc.flags)
			}
			// An advertisement carries no lease. A non-empty IP here
			// would mean the routeradvert arm fell through into the
			// address path and handed the consumer an address the
			// segment never offered.
			if got.Data.IP != "" {
				t.Errorf("routeradvert event carried an address %q; it must carry flags only", got.Data.IP)
			}
		})
	}
}
