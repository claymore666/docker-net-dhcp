package udhcpc

import (
	"encoding/json"
	"net"
	"testing"
)

// FuzzBuildEvent exercises the env-var parsing path that turns a
// busybox udhcpc(6) handler invocation into a downstream Event.
//
// This is the project's primary untrusted-input surface: every field
// here is data the *DHCP server* put on the wire (busybox just relays
// it into env vars), so a malformed-input crash or a bad-but-emitted
// lease is an attacker-influenced fault. The fuzzer drives the highest
// -value fields — the v4 ip/mask, the v6 address, MTU, and the
// space-separated list options — across the bound/renew/lifecycle
// event types.
//
// Invariant under test (stronger than "doesn't panic"): when
// BuildEvent says "emit this", any IP it put on the Event MUST be
// parseable as CIDR. Emitting an unparseable IP string is the exact
// #128 bug class — it blows up later inside netlink.ParseAddr on the
// renewal path, far from the input that caused it.
func FuzzBuildEvent(f *testing.F) {
	// Seeds mirror the shapes the integration fixtures produce
	// (192.168.99.0/24 macvlan, 192.168.100.0/24 bridge) plus the
	// degenerate inputs the unit tests already pin: missing fields,
	// the v6 branch, the lifecycle types, and known-malformed values.
	// args: eventType, ip, mask, ipv6, mtu, dns, router, search
	f.Add("bound", "192.168.99.16", "24", "", "1500", "192.168.99.1", "192.168.99.1", "corp.example")
	f.Add("renew", "192.168.100.5", "24", "", "", "", "192.168.100.1", "")
	f.Add("bound", "", "", "fe80::1", "1280", "2001:db8::53", "", "")
	f.Add("deconfig", "", "", "", "", "", "", "")
	f.Add("nak", "", "", "", "", "", "", "")
	f.Add("leasefail", "", "", "", "", "", "", "")
	f.Add("bogus-type", "", "", "", "", "", "", "")
	f.Add("bound", "not-an-ip", "abc", "", "not-an-int", "", "", "")
	f.Add("bound", "192.168.0.1", "255.255.255.0", "", "0", "", "", "") // dotted mask: ParseCIDR rejects → skip

	f.Fuzz(func(t *testing.T, eventType, ip, mask, ipv6, mtu, dns, router, search string) {
		env := fakeEnv(map[string]string{
			"ip":     ip,
			"mask":   mask,
			"ipv6":   ipv6,
			"mtu":    mtu,
			"dns":    dns,
			"dns6":   dns,
			"router": router,
			"domain": search,
			"search": search,
			"ntpsrv": dns,
		})

		event, emit := BuildEvent(eventType, env)
		if !emit {
			// A suppressed event carries no contract beyond "don't crash",
			// which we already got here by not panicking.
			return
		}

		// Emitted bound/renew events must never carry an IP string that
		// downstream netlink parsing would reject (#128).
		if event.Data.IP != "" {
			if _, _, err := net.ParseCIDR(event.Data.IP); err != nil {
				t.Fatalf("BuildEvent emitted unparseable IP %q (eventType=%q ip=%q mask=%q ipv6=%q)",
					event.Data.IP, eventType, ip, mask, ipv6)
			}
		}

		// A positive MTU is the only value BuildEvent should ever store;
		// 0/negative/garbage must be dropped, not propagated.
		if event.Data.MTU < 0 {
			t.Fatalf("BuildEvent emitted negative MTU %d from mtu=%q", event.Data.MTU, mtu)
		}
	})
}

// FuzzEventUnmarshal exercises the other untrusted boundary: the
// newline-delimited JSON the udhcpc-handler binary writes into the
// event pipe, which DHCPClient.Start decodes one line at a time
// (client.go: json.Unmarshal(scanner.Bytes(), &event)). The bytes
// cross a process boundary, so the decoder must tolerate any input
// without panicking — a malformed line should fail the Unmarshal, not
// take down the persistent-client goroutine.
func FuzzEventUnmarshal(f *testing.F) {
	f.Add([]byte(`{"Type":"bound","Data":{"IP":"192.168.99.16/24","Gateway":"192.168.99.1","MTU":1500}}`))
	f.Add([]byte(`{"Type":"deconfig"}`))
	f.Add([]byte(`{"Type":"bound","Data":{"DNSServers":["192.168.99.53"],"SearchList":["corp.example"]}}`))
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"Type":123}`))
	f.Add([]byte(`not json at all`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var event Event
		// Contract is "never panic"; an error return is the expected,
		// safe outcome for garbage input.
		_ = json.Unmarshal(data, &event)
	})
}
