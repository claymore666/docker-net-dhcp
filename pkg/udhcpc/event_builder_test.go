package udhcpc

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
		"ip":       "192.168.99.10",
		"mask":     "24",
		"router":   "192.168.99.1",
		"domain":   "corp.example",
		"dns":      "192.168.99.53 192.168.99.54",
		"ntpsrv":   "192.168.99.123 192.168.99.124",
		"search":   "corp.example internal.example",
		"tftp":     "tftp.example.test",
		"bootfile": "pxelinux.0",
		"mtu":      "1400",
	})

	got, emit := BuildEvent("bound", env)
	if !emit {
		t.Fatalf("emit=false on a well-formed bound event")
	}
	want := Event{
		Type: "bound",
		Data: Info{
			IP:         "192.168.99.10/24",
			Gateway:    "192.168.99.1",
			Domain:     "corp.example",
			DNSServers: []string{"192.168.99.53", "192.168.99.54"},
			MTU:        1400,
			NTPServers: []string{"192.168.99.123", "192.168.99.124"},
			SearchList: []string{"corp.example", "internal.example"},
			TFTPServer: "tftp.example.test",
			BootFile:   "pxelinux.0",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Event mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestBuildEvent_RenewBehavesAsBound(t *testing.T) {
	env := fakeEnv(map[string]string{
		"ip":     "10.0.0.5",
		"mask":   "16",
		"router": "10.0.0.1",
	})

	got, emit := BuildEvent("renew", env)
	if !emit {
		t.Fatalf("emit=false on renew")
	}
	if got.Type != "renew" || got.Data.IP != "10.0.0.5/16" || got.Data.Gateway != "10.0.0.1" {
		t.Errorf("renew should populate the same v4 fields as bound; got %+v", got)
	}
}

func TestBuildEvent_BoundV6_CanonicalisesIPAndCapturesDNS6(t *testing.T) {
	env := fakeEnv(map[string]string{
		"ipv6": "2001:db8::1",
		"dns6": "2001:db8::53 2001:db8::54",
	})

	got, emit := BuildEvent("bound", env)
	if !emit {
		t.Fatalf("emit=false on bound v6")
	}
	if got.Data.IP != "2001:db8::1/128" {
		t.Errorf("v6 IP not canonicalised to /128: got %q", got.Data.IP)
	}
	if !reflect.DeepEqual(got.Data.DNSServers, []string{"2001:db8::53", "2001:db8::54"}) {
		t.Errorf("v6 DNSServers wrong: %+v", got.Data.DNSServers)
	}
	// v6 path must not populate any of the v4-only fields.
	if got.Data.Gateway != "" || got.Data.Domain != "" || len(got.Data.NTPServers) > 0 ||
		got.Data.TFTPServer != "" || got.Data.BootFile != "" || len(got.Data.SearchList) > 0 {
		t.Errorf("v6 path leaked v4-only fields: %+v", got.Data)
	}
}

func TestBuildEvent_BoundV6_StripsExistingMaskBeforeCanonicalising(t *testing.T) {
	// Defensive against a future busybox that emits CIDR form.
	env := fakeEnv(map[string]string{
		"ipv6": "2001:db8::42/64",
	})

	got, emit := BuildEvent("bound", env)
	if !emit {
		t.Fatalf("emit=false on CIDR-form ipv6")
	}
	if got.Data.IP != "2001:db8::42/128" {
		t.Errorf("v6 with embedded mask should be canonicalised to /128: got %q", got.Data.IP)
	}
}

func TestBuildEvent_BoundV6_MalformedSkipsEvent(t *testing.T) {
	// udhcpc6 misbehaviour or hostile input shouldn't bring down the
	// whole renewal path — the handler skips the event and the
	// persistent client retries on the next one.
	env := fakeEnv(map[string]string{
		"ipv6": "not-an-ip",
	})

	if _, emit := BuildEvent("bound", env); emit {
		t.Errorf("emit=true on a malformed ipv6 — should have been skipped")
	}
}

func TestBuildEvent_MTUParseFailureIsBestEffort(t *testing.T) {
	// A garbage mtu env from a misbehaving udhcpc must not block
	// IP propagation; the rest of the event still flows through
	// with MTU == 0 (which the consumer treats as "no MTU info").
	env := fakeEnv(map[string]string{
		"ip":   "192.168.0.10",
		"mask": "24",
		"mtu":  "not-a-number",
	})

	got, emit := BuildEvent("bound", env)
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
	// Some servers send option 26 with value 0 (broken config). The
	// consumer's contract is "0 = do not change link MTU" so the
	// handler reflects that — never set MTU=0 from a present-but-zero
	// raw value.
	env := fakeEnv(map[string]string{
		"ip":   "192.168.0.10",
		"mask": "24",
		"mtu":  "0",
	})

	got, _ := BuildEvent("bound", env)
	if got.Data.MTU != 0 {
		t.Errorf("MTU=%d, want 0 for present-but-zero raw value", got.Data.MTU)
	}
}

func TestBuildEvent_LifecycleEvents_EmitTypeOnly(t *testing.T) {
	for _, evt := range []string{"deconfig", "leasefail", "nak"} {
		t.Run(evt, func(t *testing.T) {
			got, emit := BuildEvent(evt, fakeEnv(map[string]string{
				// These should be ignored — the lifecycle path
				// must not pull v4/v6 fields off env.
				"ip":   "192.168.0.10",
				"ipv6": "2001:db8::1",
			}))
			if !emit {
				t.Fatalf("emit=false on lifecycle event %q", evt)
			}
			if got.Type != evt {
				t.Errorf("Type = %q, want %q", got.Type, evt)
			}
			if !reflect.DeepEqual(got.Data, Info{}) {
				t.Errorf("lifecycle event leaked Data: %+v", got.Data)
			}
		})
	}
}

func TestBuildEvent_UnknownTypeIsSkipped(t *testing.T) {
	if _, emit := BuildEvent("definitely-not-a-real-udhcpc-event", fakeEnv(nil)); emit {
		t.Errorf("emit=true on unknown event type — should have been skipped")
	}
}

func TestBuildEvent_BoundV4_OmittedOptionsAreEmpty(t *testing.T) {
	// A minimal lease — only ip + mask. Every other field stays at
	// zero/empty, json:",omitempty" then keeps them out of the
	// downstream JSON. Pins that Getenv("missing") returning "" doesn't
	// accidentally insert an empty-string entry into a slice via
	// strings.Fields.
	env := fakeEnv(map[string]string{
		"ip":   "10.0.0.5",
		"mask": "8",
	})
	got, emit := BuildEvent("bound", env)
	if !emit {
		t.Fatalf("emit=false on minimal bound")
	}
	if len(got.Data.DNSServers) != 0 || len(got.Data.NTPServers) != 0 || len(got.Data.SearchList) != 0 {
		t.Errorf("missing env vars produced non-empty slices: %+v", got.Data)
	}
}

// TestBuildEvent_BoundV4_MalformedLeaseIsSkipped pins the #128
// hardening: a bound/renew whose `ip`/`mask` env doesn't form a valid
// CIDR is dropped at the handler instead of flowing downstream where
// netlink.ParseAddr would fail mid-renewal. Cases mirror the issue's
// test design: empty ip with valid mask, non-numeric mask, junk both.
func TestBuildEvent_BoundV4_MalformedLeaseIsSkipped(t *testing.T) {
	cases := map[string]map[string]string{
		"empty ip, valid mask": {"ip": "", "mask": "24"},
		"valid ip, empty mask": {"ip": "10.0.0.5", "mask": ""},
		"non-numeric mask":     {"ip": "10.0.0.5", "mask": "abc"},
		"negative mask":        {"ip": "10.0.0.5", "mask": "-1"},
		"mask out of range":    {"ip": "10.0.0.5", "mask": "33"},
		"garbage ip":           {"ip": "not-an-ip", "mask": "24"},
		"nothing set":          {},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, emit := BuildEvent("bound", fakeEnv(env)); emit {
				t.Errorf("emit=true for malformed v4 lease env %v; want skipped", env)
			}
		})
	}
}

// TestBuildEvent_RenewValidatesLikeBound: the validation guards both
// event types that carry lease data.
func TestBuildEvent_RenewValidatesLikeBound(t *testing.T) {
	if _, emit := BuildEvent("renew", fakeEnv(map[string]string{"ip": "", "mask": "24"})); emit {
		t.Error("emit=true for malformed renew; want skipped")
	}
}

// TestBuildEvent_BoundV6_LinkLocalIsEmitted pins that a link-local
// udhcpc6 lease (fe80::...) is structurally valid and flows through —
// filtering link-local is a consumer policy decision, not the
// handler's.
func TestBuildEvent_BoundV6_LinkLocalIsEmitted(t *testing.T) {
	got, emit := BuildEvent("bound", fakeEnv(map[string]string{
		"ipv6": "fe80::42:acff:fe00:1",
	}))
	if !emit {
		t.Fatal("emit=false for link-local v6 lease")
	}
	if got.Data.IP != "fe80::42:acff:fe00:1/128" {
		t.Errorf("IP = %q, want canonicalised /128 link-local", got.Data.IP)
	}
}

// TestBuildEvent_BoundV6_UncompressedIsCanonicalised: udhcpc6 emits
// fully zero-padded addresses; the event must carry the compressed
// canonical form so downstream string comparisons are stable.
func TestBuildEvent_BoundV6_UncompressedIsCanonicalised(t *testing.T) {
	got, emit := BuildEvent("bound", fakeEnv(map[string]string{
		"ipv6": "fd00:6470:6863:0000:0000:0000:0000:0010",
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
	got, emit := BuildEvent("bound", fakeEnv(map[string]string{
		"ipv6": "fd00::10",
		"dns6": "fd00::53 fd00::54",
	}))
	if !emit {
		t.Fatal("emit=false")
	}
	want := []string{"fd00::53", "fd00::54"}
	if !reflect.DeepEqual(got.Data.DNSServers, want) {
		t.Errorf("DNSServers = %v, want %v", got.Data.DNSServers, want)
	}
}

// TestBuildEvent_BoundV6_EmptyIPv6FallsThroughToV4Validation: an
// empty `ipv6` env on a v6 bound routes into the v4 branch (the
// documented contract), where empty ip/mask now skips the event
// rather than emitting "/" as an address.
func TestBuildEvent_BoundV6_EmptyIPv6FallsThroughToV4Validation(t *testing.T) {
	if _, emit := BuildEvent("bound", fakeEnv(map[string]string{"ipv6": ""})); emit {
		t.Error("emit=true for empty ipv6 + no v4 env; want skipped")
	}
}

// TestBuildEvent_MTUNegativeIsIgnored extends the best-effort MTU
// contract to negative numbers ("-5" parses but is not a valid MTU).
func TestBuildEvent_MTUNegativeIsIgnored(t *testing.T) {
	got, emit := BuildEvent("bound", fakeEnv(map[string]string{
		"ip": "10.0.0.5", "mask": "24", "mtu": "-5",
	}))
	if !emit {
		t.Fatal("emit=false; MTU problems must not kill the event")
	}
	if got.Data.MTU != 0 {
		t.Errorf("MTU = %d, want 0 (negative input ignored)", got.Data.MTU)
	}
}
