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
