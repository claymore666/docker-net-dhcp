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
	// v6 path must not populate any of the v4-only fields.
	if got.Data.Gateway != "" || got.Data.Domain != "" || len(got.Data.NTPServers) > 0 ||
		got.Data.TFTPServer != "" || got.Data.BootFile != "" || len(got.Data.SearchList) > 0 || got.Data.MTU != 0 {
		t.Errorf("v6 path leaked v4-only fields: %+v", got.Data)
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
func TestBuildEvent_UnactionedReasonsSkipped(t *testing.T) {
	for _, reason := range []string{
		"PREINIT", "CARRIER", "NOCARRIER", "ROUTERADVERT", "STOP", "STOP6",
		"STOPPED", "DEPARTED", "FAIL", "TEST", "IPV4LL", "STATIC", "3RDPARTY",
		"DELEGATED6", "RECONFIGURE", "INFORM", "INFORM6", "",
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
