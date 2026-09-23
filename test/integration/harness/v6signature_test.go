// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The RA frames are verbatim from lane run 33996052773, main-1-suite; the logs are dnsmasq `--log-dhcp` output from
// the same argv, driven by a minimal DHCPv6 sender in a peer namespace (#911). An earlier set captured with another
// argv advertised a /120 prefix with infinite lifetimes where every fixture mode advertises /64 with 1800 s. The frames
// do not re-derive: if the runner's dnsmasq changes its RA, only a moved signature field turns the lane red.

// mode=managed: dnsmasq sets M and O and, serving addresses itself, clears the prefix's A bit.
const raManagedHex = "333300000001d66740dba1e886dd6c04517e00703afffe80000000000000d46740fffedba1e8" +
	"ff0200000000000000000000000000018600df8540c007080000000000000000030440800000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc0101d667" +
	"40dba1e81f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// mode=managed-exhausted advertises exactly as managed. Captured 2026-09-16, dnsmasq 2.91 in `unshare -Urn` (#989).
const raManagedExhaustedHex = "333300000001f25f24640dca86dd6c02e82100703afffe80000000000000f05f24fffe640dca" +
	"ff020000000000000000000000000001860008c140c007080000000000000000030440800000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc0101f25f" +
	"24640dca1f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// mode=stateless: O set, M clear, prefix advertised as autonomous.
const raStatelessHex = "33330000000146a1584be82e86dd6c0eca0100703afffe8000000000000044a158fffe4be82e" +
	"ff020000000000000000000000000001860043e6404007080000000000000000030440c00000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc010146a1" +
	"584be82e1f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// mode=slaac: neither flag, prefix autonomous.
const raSLAACHex = "3333000000018ad3401f959486dd6c04fa6700703afffe8000000000000088d340fffe1f9594" +
	"ff0200000000000000000000000000018600914e400007080000000000000000030440c00000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc01018ad3" +
	"401f95941f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// mode=auto-fallback: M and O set and the prefix autonomous, unlike any other mode (#818). Captured 2026-09-16, dnsmasq
// 2.91 in `unshare -Urn`, with tcpdump: flags byte 0xc0 at ICMPv6 offset 5, prefix option `03 04 40 c0`.
const raAutoFallbackHex = "33330000000176a66e95a4e486dd6c0ba59100703afffe8000000000000074a66efffe95a4e4" +
	"ff02000000000000000000000000000186003d5c40c007080000000000000000030440c00000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc010176a6" +
	"6e95a4e41f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// mode=managed-silent: byte for byte managed's signature; --dhcp-ignore changes answers, not advertisements.
const raManagedSilentHex = "33330000000162bb4b8d99c986dd6c066d8800703afffe8000000000000060bb4bfffe8d99c9" +
	"ff0200000000000000000000000000018600c1b840c007080000000000000000030440800000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc010162bb" +
	"4b8d99c91f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// A SLAAC prefix advertised deprecated (#819): A set, valid lifetime RFC 4861 section 4.6.2's infinity, preferred 0.
// Measured 2026-09-16, dnsmasq 2.91 in `unshare -Urn`, `--dhcp-range=fd00:6470:6865::,ra-only,deprecated` plus
// --enable-ra: the prefix option reads `03 04 40 c0 ffffffff 00000000`, and the library's client formed the address in
// one second with its preferred instant already past. Its five-field signature equals SLAAC's.
const raDeprecatedPrefixHex = "33330000000182a66511995286dd6c0da1e400703afffe8000000000000080a665fffe119952" +
	"ff02000000000000000000000000000186006c68400007080000000000000000030440c0ffff" +
	"ffff0000000000000000fd00647068650000000000000000000005010000000005dc010182a6" +
	"651199521f030000ffffffff0676366d6f6465076578616d706c650019030000fffffffffd00" +
	"6470686500000000000000000053"

func mustFrame(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("fixture frame is not hex: %v", err)
	}
	return b
}

// RFC 4861 section 4.2 puts M and O in the octet after Cur Hop Limit. dnsmasq sends Cur Hop Limit 64 (0x40, which reads
// as "O set, M clear"), so an off-by-one decoder reports stateless for three modes (#911).
func TestParseRA_ReadsTheFlagsFromTheByteAfterCurHopLimit(t *testing.T) {
	cases := []struct {
		name           string
		mode           V6Mode
		hexFrame       string
		managed, other bool
		autonomous     bool
		// dnsmasq advertises 1800 s for both prefix lifetimes by default, whatever the lease time.
		wantValid, wantPreferred uint32
	}{
		{"managed", V6Managed, raManagedHex, true, true, false, 1800, 1800},
		{"stateless", V6Stateless, raStatelessHex, false, true, true, 1800, 1800},
		{"slaac", V6SLAAC, raSLAACHex, false, false, true, 1800, 1800},
		{"managed-silent", V6ManagedSilent, raManagedSilentHex, true, true, false, 1800, 1800},
		{"managed-exhausted", V6ManagedExhausted, raManagedExhaustedHex, true, true, false, 1800, 1800},
		{"auto-fallback", V6AutoFallback, raAutoFallbackHex, true, true, true, 1800, 1800},
		{"slaac with a deprecated prefix", V6SLAAC, raDeprecatedPrefixHex, false, false, true, RAInfiniteLifetime, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, ok := ParseRA(mustFrame(t, c.hexFrame))
			if !ok {
				t.Fatalf("ParseRA refused a captured router advertisement")
			}
			if f.Managed != c.managed || f.OtherConfig != c.other {
				t.Errorf("M=%v O=%v, want M=%v O=%v", f.Managed, f.OtherConfig, c.managed, c.other)
			}
			if f.CurHopLimit != 64 {
				t.Errorf("Cur Hop Limit = %d, want 64; the frames this test tells apart all "+
					"carry 64 here, and that is what makes the flag byte the only thing "+
					"separating them", f.CurHopLimit)
			}
			if len(f.Prefixes) != 1 {
				t.Fatalf("prefix options = %d, want 1", len(f.Prefixes))
			}
			p := f.Prefixes[0]
			if p.Autonomous != c.autonomous {
				t.Errorf("prefix A flag = %v, want %v", p.Autonomous, c.autonomous)
			}
			if !p.OnLink {
				t.Error("prefix L flag clear; dnsmasq sets on-link unless off-link was asked for")
			}
			if p.PrefixLen != 64 {
				t.Errorf("prefix length = %d, want 64", p.PrefixLen)
			}
			if got := p.Prefix.String(); got != "fd00:6470:6865::" {
				t.Errorf("prefix = %s, want fd00:6470:6865::", got)
			}
			if f.RouterLifetime != 1800*time.Second {
				t.Errorf("router lifetime = %s, want 30m", f.RouterLifetime)
			}
			if p.ValidLifetime != c.wantValid || p.PreferredLifetime != c.wantPreferred {
				t.Errorf("prefix valid=%d preferred=%d, want valid=%d preferred=%d",
					p.ValidLifetime, p.PreferredLifetime, c.wantValid, c.wantPreferred)
			}
			if f.SourceMAC == nil || len(f.SourceMAC) != 6 {
				t.Errorf("source MAC = %v, want six bytes", f.SourceMAC)
			}
		})
	}
}

func TestParseRA_RefusesWhatIsNotARouterAdvertisement(t *testing.T) {
	good := mustFrame(t, raManagedHex)

	mutate := func(name string, f func([]byte)) {
		t.Run(name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			f(b)
			if _, ok := ParseRA(b); ok {
				t.Errorf("accepted a frame that is not a router advertisement")
			}
		})
	}
	mutate("not IPv6", func(b []byte) { b[12], b[13] = 0x08, 0x00 })
	mutate("not ICMPv6", func(b []byte) { b[ethHeaderLen+6] = 17 })
	mutate("neighbour advertisement, not router", func(b []byte) { b[ethHeaderLen+ipv6HeaderLen] = 136 })
	mutate("IP version nibble is not 6", func(b []byte) { b[ethHeaderLen] = 0x45 })
	t.Run("truncated", func(t *testing.T) {
		if _, ok := ParseRA(good[:ethHeaderLen+ipv6HeaderLen+4]); ok {
			t.Error("accepted a frame too short to hold the advertisement header")
		}
	})
	if _, ok := ParseRA(good); !ok {
		t.Error("the unmutated captured frame no longer parses")
	}
}

// Captured 2026-09-05, dnsmasq 2.91, one user namespace per mode, a minimal DHCPv6 sender in the peer namespace;
// trimmed to the lines the contract reads, none edited (#911).
const (
	logManaged = `Sep  5 23:32:56 dnsmasq-dhcp[747116]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:32:56 dnsmasq-dhcp[747116]: DHCPv6, IP range fd00:6470:6865::10 -- fd00:6470:6865::99, lease time 2m
Sep  5 23:32:57 dnsmasq-dhcp[747116]: RTR-ADVERT(br0) fd00:6470:6865::
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658188 DHCPSOLICIT(br0) 00:03:00:01:9e:14:a2:d9:ef:ef 
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658188 DHCPADVERTISE(br0) fd00:6470:6865::54 00:03:00:01:9e:14:a2:d9:ef:ef 
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658189 DHCPREQUEST(br0) 00:03:00:01:9e:14:a2:d9:ef:ef 
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658189 DHCPREPLY(br0) fd00:6470:6865::54 00:03:00:01:9e:14:a2:d9:ef:ef 
`

	// dnsmasq answers the Information-request (the sender received type 7) and logs no DHCPREPLY: log6_quiet is called
	// once on that path (rfc3315.c:1144, dnsmasq 2.91) (#911).
	logStateless = `Sep  5 23:33:11 dnsmasq-dhcp[747377]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:33:11 dnsmasq-dhcp[747377]: DHCPv6 stateless on fd00:6470:6865::
Sep  5 23:33:12 dnsmasq-dhcp[747377]: RTR-ADVERT(br0) fd00:6470:6865::
Sep  5 23:33:13 dnsmasq-dhcp[747377]: 658188 DHCPINFORMATION-REQUEST(br0) 00:03:00:01:8e:8a:3f:b8:e5:8a 
Sep  5 23:33:13 dnsmasq-dhcp[747377]: 658188 sent size: 16 option: 23 dns-server  fd00:6470:6865::53
`

	// A SLAAC-only range gives dnsmasq no DHCPv6 server for the prefix, so the Solicit is neither answered nor logged.
	logSLAAC = `Sep  5 23:33:15 dnsmasq-dhcp[747532]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:33:16 dnsmasq-dhcp[747532]: RTR-ADVERT(br0) fd00:6470:6865::
Sep  5 23:33:16 dnsmasq-dhcp[747532]: RTR-ADVERT(br0) fd00:6470:6865::
`

	logNoRA = `Sep  5 23:33:22 dnsmasq-dhcp[747748]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:33:22 dnsmasq-dhcp[747748]: DHCPv6, IP range fd00:6470:6865::10 -- fd00:6470:6865::99, lease time 2m
Sep  5 23:33:24 dnsmasq-dhcp[747748]: 658188 DHCPSOLICIT(br0) 00:03:00:01:ce:41:ae:6d:50:36 ignored
`

	// Captured 2026-09-16, dnsmasq 2.91 in `unshare -Urn` against the library's client, LC_ALL=C (#989). The static-only
	// range makes the server answer and refuse: the Advertise carries Status Code 2 and the client reported
	// Failed{nak, NoAddrsAvail} on every Solicit.
	logManagedExhausted = `Sep 16 18:44:16 dnsmasq-dhcp[4182581]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep 16 18:44:16 dnsmasq-dhcp[4182581]: DHCPv6, static leases only on fd00:6470:6865::ff, lease time 2m
Sep 16 18:44:17 dnsmasq-dhcp[4182581]: RTR-ADVERT(s0) fd00:6470:6865::
Sep 16 18:44:18 dnsmasq-dhcp[4182581]: 8008303 DHCPSOLICIT(s0) 00:03:00:01:02:42:ac:11:00:02 
Sep 16 18:44:18 dnsmasq-dhcp[4182581]: 8008303 DHCPADVERTISE(s0) 00:03:00:01:02:42:ac:11:00:02 no addresses available
Sep 16 18:44:18 dnsmasq-dhcp[4182581]: 8008303 sent size: 24 option: 13 status  2 no addresses available
`

	// Captured 2026-09-16, same run as raAutoFallbackHex (#818): the client in proto.Mode6Auto solicited three times, was
	// ignored, and formed fd00:6470:6865:0:bc1b:12ff:fe5b:c905/64 by SLAAC. --log-dhcp prints the `available DHCP range`
	// lines for a request it then ignores.
	logAutoFallback = `Sep 16 20:52:29 dnsmasq-dhcp[474848]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep 16 20:52:29 dnsmasq-dhcp[474848]: DHCPv6, IP range fd00:6470:6865::10 -- fd00:6470:6865::99, lease time 2m
Sep 16 20:52:29 dnsmasq-dhcp[474848]: router advertisement on fd00:6470:6865::
Sep 16 20:52:30 dnsmasq-dhcp[474848]: RTR-ADVERT(s0) fd00:6470:6865::
Sep 16 20:52:33 dnsmasq-dhcp[474848]: RTR-SOLICIT(s0) be:1b:12:5b:c9:05
Sep 16 20:52:33 dnsmasq-dhcp[474848]: RTR-ADVERT(s0) fd00:6470:6865::
Sep 16 20:52:33 dnsmasq-dhcp[474848]: 1100156 available DHCP range: fd00:6470:6865::10 -- fd00:6470:6865::99
Sep 16 20:52:33 dnsmasq-dhcp[474848]: 1100156 client MAC address: be:1b:12:5b:c9:05
Sep 16 20:52:33 dnsmasq-dhcp[474848]: 1100156 DHCPSOLICIT(s0) 00:03:00:01:be:1b:12:5b:c9:05 ignored
Sep 16 20:52:34 dnsmasq-dhcp[474848]: 1100156 DHCPSOLICIT(s0) 00:03:00:01:be:1b:12:5b:c9:05 ignored
Sep 16 20:52:36 dnsmasq-dhcp[474848]: 1100156 DHCPSOLICIT(s0) 00:03:00:01:be:1b:12:5b:c9:05 ignored
`

	logManagedSilent = `Sep  5 23:33:27 dnsmasq-dhcp[747851]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:33:27 dnsmasq-dhcp[747851]: DHCPv6, IP range fd00:6470:6865::10 -- fd00:6470:6865::99, lease time 2m
Sep  5 23:33:28 dnsmasq-dhcp[747851]: RTR-ADVERT(br0) fd00:6470:6865::
Sep  5 23:33:29 dnsmasq-dhcp[747851]: 658188 DHCPSOLICIT(br0) 00:03:00:01:0e:c7:07:f7:dd:66 ignored
`
)

func logFor(m V6Mode) string {
	switch m {
	case V6Managed:
		return logManaged
	case V6Stateless:
		return logStateless
	case V6SLAAC:
		return logSLAAC
	case V6NoRA:
		return logNoRA
	case V6ManagedSilent:
		return logManagedSilent
	case V6ManagedExhausted:
		return logManagedExhausted
	case V6AutoFallback:
		return logAutoFallback
	}
	return ""
}

func TestV6ExchangeFindings_EachModesOwnLogPassesAndTheOthersDoNot(t *testing.T) {
	// SLAAC has no DHCPv6 server, so its contract is a must-NOT set only and passes any log without a v6 token (#911).
	passesForeignLogs := map[V6Mode]map[V6Mode]bool{
		V6SLAAC: {V6SLAAC: true},
		// no-RA and managed-silent produce the same DHCP log, an ignored SOLICIT; they differ only on the wire (#911).
		V6NoRA:          {V6NoRA: true, V6ManagedSilent: true, V6AutoFallback: true},
		V6ManagedSilent: {V6ManagedSilent: true, V6NoRA: true, V6AutoFallback: true},
		// auto-fallback's log is a third copy of that ignored SOLICIT; V6Signature separates the three on the wire (#818).
		V6AutoFallback: {V6AutoFallback: true, V6NoRA: true, V6ManagedSilent: true},
	}

	for _, mode := range V6Modes() {
		for _, other := range V6Modes() {
			name := mode.String() + "/log-of-" + other.String()
			t.Run(name, func(t *testing.T) {
				findings := V6ExchangeFindings(mode, logFor(other))
				wantPass := mode == other
				if allowed, ok := passesForeignLogs[mode]; ok && allowed[other] {
					wantPass = true
				}
				if wantPass && len(findings) != 0 {
					t.Errorf("mode %s rejected a log it must accept: %v", mode, findings)
				}
				if !wantPass && len(findings) == 0 {
					t.Errorf("mode %s accepted %s's log; the contract cannot tell them apart, "+
						"so every scenario in %s would pass on a segment in the wrong mode",
						mode, other, mode)
				}
			})
		}
	}
}

func TestV6ExchangeFindings_AnEmptyLogFailsEveryModeThatRequiresOne(t *testing.T) {
	for _, mode := range V6Modes() {
		findings := V6ExchangeFindings(mode, "")
		if mode == V6SLAAC {
			if len(findings) != 0 {
				t.Errorf("mode %s: %v; a must-NOT-only contract has nothing to require", mode, findings)
			}
			continue
		}
		if len(findings) == 0 {
			t.Errorf("mode %s accepted an EMPTY log; a scenario that never ran a client "+
				"would pass this assertion", mode)
		}
	}
}

// dnsmasq is translated and the runner speaks German, so a gettext-rewritten needle would match nothing (#942).
func TestV6ExchangeFindings_NoNeedleIsProseDnsmasqTranslates(t *testing.T) {
	// `_("ignored")` at rfc3315.c:652 renders as "ignoriert" under dnsmasq's po/de.po; withCLocale pins LC_ALL=C (#942).
	const knownTranslated = "ignored"

	for mode, c := range v6ExchangeContract {
		needles := append(append([]string{}, c.must...), c.mustNot...)
		needles = append(needles, c.mustLine...)
		for _, n := range needles {
			if n == knownTranslated {
				continue
			}
			if n != strings.ToUpper(n) {
				t.Errorf("mode %s: needle %q is not an upper-case protocol token; if dnsmasq "+
					"translates it, this assertion matches nothing and passes vacuously", mode, n)
			}
			if !strings.HasPrefix(n, "DHCP") {
				t.Errorf("mode %s: needle %q is not a DHCP message name", mode, n)
			}
		}
	}
}

func TestV6ExchangeContract_ForbidsOnlyTokensTheV4PathNeverPrints(t *testing.T) {
	if findings := V6ContractFindings(v6ExchangeContract); len(findings) != 0 {
		t.Fatalf("the shipped exchange contract is not clean:\n  %s", strings.Join(findings, "\n  "))
	}

	ambiguous := DnsmasqAmbiguousDHCPTokens()
	if len(ambiguous) < 3 {
		t.Fatalf("the v4/v6 name tables intersect in %v; this test's premise is that the "+
			"intersection is bigger than the one token round 1 named", ambiguous)
	}
	for _, tok := range ambiguous {
		for _, mode := range V6Modes() {
			t.Run(mode.String()+"/forbids/"+tok, func(t *testing.T) {
				bad := make(map[V6Mode]v6ExchangeRule, len(v6ExchangeContract))
				for k, v := range v6ExchangeContract {
					bad[k] = v
				}
				r := bad[mode]
				r.mustNot = append(append([]string{}, r.mustNot...), tok)
				bad[mode] = r

				findings := V6ContractFindings(bad)
				if len(findings) == 0 {
					t.Fatalf("mode %s may forbid %q, which dnsmasq prints on BOTH paths; the "+
						"guard is not keyed on the property it names", mode, tok)
				}
				if !strings.Contains(findings[0], tok) || !strings.Contains(findings[0], mode.String()) {
					t.Errorf("the finding names neither the mode nor the token: %s", findings[0])
				}
			})
		}
	}

	control := V6OnlyDHCPTokens()[0]
	for _, mode := range V6Modes() {
		ok := make(map[V6Mode]v6ExchangeRule, len(v6ExchangeContract))
		for k, v := range v6ExchangeContract {
			ok[k] = v
		}
		r := ok[mode]
		r.mustNot = append(append([]string{}, r.mustNot...), control)
		ok[mode] = r
		if findings := V6ContractFindings(ok); len(findings) != 0 {
			t.Errorf("mode %s refused %q, which no v4 path prints: %s", mode, control, findings[0])
		}
	}

	// dnsmasq's v4 path writes `DHCPRELEASE(br0) ... ignored` (rfc2131.c:1096, message at :1105), the shape of the v6
	// `DHCPSOLICIT ... ignored` line the managed-silent row requires (#915).
	for _, tok := range ambiguous {
		t.Run(V6ManagedSilent.String()+"/requires-line/"+tok, func(t *testing.T) {
			bad := make(map[V6Mode]v6ExchangeRule, len(v6ExchangeContract))
			for k, v := range v6ExchangeContract {
				bad[k] = v
			}
			r := bad[V6ManagedSilent]
			r.mustLine = []string{tok, "ignored"}
			bad[V6ManagedSilent] = r

			findings := V6ContractFindings(bad)
			if len(findings) == 0 {
				t.Fatalf("mode %s may require the line %v, every token of which dnsmasq's v4 "+
					"path prints; the guard reads the columns that fail red and not the one "+
					"that fails green", V6ManagedSilent, r.mustLine)
			}
			if !strings.Contains(findings[0], tok) || !strings.Contains(findings[0], V6ManagedSilent.String()) {
				t.Errorf("the finding names neither the mode nor the token: %s", findings[0])
			}
		})
	}

	for _, tok := range ambiguous {
		ok := make(map[V6Mode]v6ExchangeRule, len(v6ExchangeContract))
		for k, v := range v6ExchangeContract {
			ok[k] = v
		}
		r := ok[V6ManagedSilent]
		r.mustLine = []string{tok, V6OnlyDHCPTokens()[0], "ignored"}
		ok[V6ManagedSilent] = r
		if findings := V6ContractFindings(ok); len(findings) != 0 {
			t.Errorf("a required line carrying the v6-only %q beside the ambiguous %q was "+
				"refused: %s", V6OnlyDHCPTokens()[0], tok, findings[0])
		}
	}

	missing := map[V6Mode]v6ExchangeRule{}
	for k, v := range v6ExchangeContract {
		if k != V6SLAAC {
			missing[k] = v
		}
	}
	if findings := V6ContractFindings(missing); len(findings) != 1 ||
		!strings.Contains(findings[0], V6SLAAC.String()) {
		t.Errorf("dropping %s's row produced %v, want one finding naming it", V6SLAAC, findings)
	}
}

// SLAAC's must-NOT column is the v6-only set, so its v4 half, including an RFC 5227 DHCPDECLINE, never reaches the v6
// verdict (#915).
func TestV6ExchangeFindings_AV4OnlyExchangeSatisfiesNoModeAndAccusesNone(t *testing.T) {
	var b strings.Builder
	for _, n := range dnsmasqV4MessageNames {
		fmt.Fprintf(&b, "Sep  6 00:00:00 dnsmasq-dhcp[1]: %s(br0) 192.168.103.10 aa:bb:cc:dd:ee:ff ignored\n", n)
	}
	v4Only := b.String()
	for _, tok := range []string{"DHCPDECLINE", "DHCPRELEASE"} {
		if !strings.Contains(v4Only, tok) {
			t.Fatalf("the v4 log this test drives does not contain %q, so it cannot show the "+
				"conflict path is harmless", tok)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(v4Only), "\n") {
		if !strings.Contains(line, "ignored") {
			t.Fatalf("the v4 log this test drives has a line with no `message` on it (%q); "+
				"the v4 path writes one, and without it this log cannot exercise a mustLine",
				line)
		}
	}

	for _, mode := range V6Modes() {
		for _, f := range V6ExchangeFindings(mode, v4Only) {
			if strings.Contains(f, "forbids") {
				t.Errorf("mode %s is FAILED by a v4-only exchange: %s", mode, f)
			}
		}
	}

	for _, mode := range V6Modes() {
		if mode == V6SLAAC {
			continue
		}
		if len(V6ExchangeFindings(mode, v4Only)) == 0 {
			t.Errorf("mode %s is satisfied by a v4-only exchange", mode)
		}
	}
}

func TestV6ExchangeFindings_TheMustLineIsPerLineNotWholeLog(t *testing.T) {
	rule, ok := v6ExchangeContract[V6ManagedSilent]
	if !ok || len(rule.mustLine) == 0 {
		t.Fatalf("%s has no mustLine, so this test observes nothing; the per-line scope "+
			"of countLinesWithAll is unobserved as of now", V6ManagedSilent)
	}
	if len(rule.mustLine) < 2 {
		t.Fatalf("%s's mustLine is %v; a one-token line rule cannot tell per-line from "+
			"whole-log, so this observer would be vacuous", V6ManagedSilent, rule.mustLine)
	}

	var b strings.Builder
	for i, tok := range rule.mustLine {
		fmt.Fprintf(&b, "Sep  6 00:00:0%d dnsmasq-dhcp[1]: %s(br0) 2001:db8::10 00:01:00:01\n", i, tok)
	}
	split := b.String()

	for _, tok := range rule.mustLine {
		if !strings.Contains(split, tok) {
			t.Fatalf("the log this test drives does not contain %q, so a whole-log "+
				"conjunction would reject it for the wrong reason and this test "+
				"would pass without observing anything", tok)
		}
	}
	if countLinesWithAll(split, rule.mustLine) != 0 {
		t.Fatalf("a line of the log this test drives carries every one of %v; the log is "+
			"supposed to have them SEPARATED:\n%s", rule.mustLine, split)
	}

	findings := V6ExchangeFindings(V6ManagedSilent, split)
	var named bool
	for _, f := range findings {
		if strings.Contains(f, "no single log line carries all of") {
			named = true
		}
	}
	if !named {
		t.Errorf("mode %s is satisfied by a log whose mustLine tokens %v sit on separate "+
			"lines (findings: %v). The rule is that they appear TOGETHER; read across the "+
			"whole log it is satisfied by any fixture whose v4 half logged a refusal, "+
			"which every mode of this fixture can do",
			V6ManagedSilent, rule.mustLine, findings)
	}

	joined := "Sep  6 00:00:00 dnsmasq-dhcp[1]: " + strings.Join(rule.mustLine, " ") + "\n"
	for _, f := range V6ExchangeFindings(V6ManagedSilent, joined) {
		if strings.Contains(f, "no single log line carries all of") {
			t.Errorf("mode %s reports a missing line against a log whose one line carries "+
				"all of %v: %s", V6ManagedSilent, rule.mustLine, f)
		}
	}
}

func evidenceFor(t *testing.T, mode V6Mode) V6Evidence {
	t.Helper()
	hexes := map[V6Mode]string{
		V6Managed:          raManagedHex,
		V6Stateless:        raStatelessHex,
		V6SLAAC:            raSLAACHex,
		V6ManagedSilent:    raManagedSilentHex,
		V6ManagedExhausted: raManagedExhaustedHex,
		V6AutoFallback:     raAutoFallbackHex,
	}
	ev := V6Evidence{PoolLogged: mode.Signature().Pool}
	if h, ok := hexes[mode]; ok {
		f, parsed := ParseRA(mustFrame(t, h))
		if !parsed {
			t.Fatalf("captured frame for %s does not parse", mode)
		}
		f.At = time.Now()
		ev.RALogged = true
		ev.Frames = []RAFrame{f}
	}
	return ev
}

func TestV6ModeFindings_TheCapturedEvidenceMatchesTheModeItCameFrom(t *testing.T) {
	for _, mode := range V6Modes() {
		t.Run(mode.String(), func(t *testing.T) {
			if f := V6ModeFindings(mode, evidenceFor(t, mode)); len(f) != 0 {
				t.Errorf("the segment's own captured evidence was rejected: %v", f)
			}
		})
	}
}

func TestV6ModeFindings_EvidenceFromAnotherModeIsRejectedAndNamesThePair(t *testing.T) {
	exempt := map[[2]V6Mode]bool{}
	for _, p := range V6IndistinguishableModes() {
		exempt[p] = true
		exempt[[2]V6Mode{p[1], p[0]}] = true
	}

	for _, name := range V6Modes() {
		for _, actual := range V6Modes() {
			if name == actual || exempt[[2]V6Mode{name, actual}] {
				continue
			}
			t.Run(name.String()+"/evidence-of-"+actual.String(), func(t *testing.T) {
				findings := V6ModeFindings(name, evidenceFor(t, actual))
				if len(findings) == 0 {
					t.Fatalf("evidence from a %s segment passed as %s", actual, name)
				}
				if !strings.Contains(findings[0], name.String()) {
					t.Errorf("the finding does not name the mode asked for (%s): %q",
						name, findings[0])
				}
				if !strings.Contains(findings[0], actual.String()) {
					t.Errorf("the finding does not name the mode observed (%s): %q",
						actual, findings[0])
				}
			})
		}
	}
}

func TestV6IndistinguishableModes_IsExactlyManagedAndManagedSilent(t *testing.T) {
	got := V6IndistinguishableModes()
	if len(got) != 1 {
		t.Fatalf("modes with equal signatures: %v, want exactly one pair — a new collision "+
			"silently shrinks the drift matrix", got)
	}
	if got[0] != [2]V6Mode{V6Managed, V6ManagedSilent} {
		t.Errorf("the indistinguishable pair is %v, want managed/managed-silent", got[0])
	}
}

func TestClassifyV6Segment_NamesTheModeOrNothing(t *testing.T) {
	for _, mode := range V6Modes() {
		got := ClassifyV6Segment(evidenceFor(t, mode))
		found := false
		for _, m := range got {
			if m == mode {
				found = true
			}
		}
		if !found {
			t.Errorf("%s's own evidence classified as %v", mode, got)
		}
	}
	impossible := V6Evidence{PoolLogged: false, RALogged: false}
	if got := ClassifyV6Segment(impossible); len(got) != 0 {
		t.Errorf("classified an empty segment as %v; no mode has that signature", got)
	}
}

// The budget must cover both of dnsmasq's first-RA branches: the 1 s branch this fixture takes
// (radv.c:129 `ra_time = now + 1`, reached from dhcp6.c:715) and the 0 to 5 s draw (radv.c:135).
// A late RA leaves no frame and classifies as nora (#915).
func TestRABudget_CoversBothScheduleBranchesWithAMargin(t *testing.T) {
	// dnsmasq's clock is integer seconds, so the 1 s branch lands within 2 s.
	const fixedBranch = 2 * time.Second
	if RABudget() < fixedBranch {
		t.Errorf("the budget (%s) does not cover the `now + 1` branch (radv.c:129) with its "+
			"integer-second rounding (%s), which is the branch every measured bring-up of "+
			"this fixture is on", RABudget(), fixedBranch)
	}
	if RABudget() <= DnsmasqFirstRAUpperBound() {
		t.Errorf("the budget (%s) does not exceed dnsmasq's own worst case for a first "+
			"advertisement (%s); a wait that expires exactly when the thing it waits for is "+
			"still allowed to arrive reports the wrong MODE, not a slow segment",
			RABudget(), DnsmasqFirstRAUpperBound())
	}
	if firstRASlop <= 0 {
		t.Errorf("the slop is %s; the bound above is dnsmasq's own schedule and accounts for "+
			"no fork, exec, config parse or SIGALRM delivery on a loaded runner", firstRASlop)
	}
	if RABudget() >= V6NoRAWindow() {
		t.Errorf("the budget (%s) reaches the absence window (%s)", RABudget(), V6NoRAWindow())
	}
}

func TestV6NoRAWindow_IsLongerThanDnsmasqsOwnWorstCase(t *testing.T) {
	first := DnsmasqFirstRAUpperBound()
	if first != 5*time.Second {
		t.Errorf("dnsmasq's first-advertisement bound = %s; radv.c:135 schedules "+
			"now + rand16()/13000, which is 0..5s", first)
	}
	if V6NoRAWindow() <= first {
		t.Errorf("the no-RA window (%s) is not longer than the delay in which an advertisement "+
			"would have arrived (%s); the negative would pass by not waiting",
			V6NoRAWindow(), first)
	}
	if V6NoRAWindow() <= RABudget() {
		t.Errorf("the no-RA window (%s) is not longer than the budget the POSITIVE case spends "+
			"(%s); a segment declared silent on less evidence than one declared noisy",
			V6NoRAWindow(), RABudget())
	}
}

// Run 33994533077: a stateless fixture started with managed's flags disagreed on the pool bit within milliseconds,
// before its RA arrived, and stopping there reported it as nora (#915).
func TestV6EvidenceSettled_ADisagreementAloneDoesNotFinishAnObservation(t *testing.T) {
	// 370 ms in: dnsmasq has logged its range lines and no RA has been captured.
	early := V6Evidence{PoolLogged: true}
	if V6EvidenceSettled(early) {
		t.Fatalf("an observation with no advertisement in it reports as settled; the fixture " +
			"will stop the clock on whatever it happens to have seen and name that the mode")
	}
	if got := ClassifyV6Segment(early); len(got) != 1 || got[0] != V6NoRA {
		t.Fatalf("ClassifyV6Segment(pool logged, nothing on the wire) = %v, want [%s]; the "+
			"premise of this test is that the unsettled observation names a plausible WRONG "+
			"mode rather than nothing at all", got, V6NoRA)
	}

	f, ok := ParseRA(mustFrame(t, raManagedHex))
	if !ok {
		t.Fatalf("the managed capture no longer parses")
	}
	settled := V6Evidence{PoolLogged: true, RALogged: true, Frames: []RAFrame{f}}
	if !V6EvidenceSettled(settled) {
		t.Fatalf("an observation holding an advertisement reports as unsettled; every fixture " +
			"in an advertising mode would then spend its whole budget")
	}
	if got := ClassifyV6Segment(settled); len(got) != 2 || got[0] != V6Managed {
		t.Fatalf("ClassifyV6Segment(settled managed evidence) = %v, want managed first", got)
	}

	findings := V6ModeFindings(V6Stateless, settled)
	if len(findings) == 0 {
		t.Fatalf("managed evidence under the stateless name produced no finding")
	}
	if !strings.Contains(findings[0], V6Managed.String()) {
		t.Fatalf("the refusal does not name the mode observed (%s): %s", V6Managed, findings[0])
	}
	if strings.Contains(findings[0], V6NoRA.String()) {
		t.Fatalf("the refusal names %s, the answer the unsettled observation gave: %s",
			V6NoRA, findings[0])
	}
}

func TestV6ModeNamesIn_APrefixOfALongerModeNameIsNotThatMode(t *testing.T) {
	cases := []struct {
		s    string
		want []V6Mode
	}{
		{"started as stateless, but the segment answers as managed-silent",
			[]V6Mode{V6Stateless, V6ManagedSilent}},
		{"started as stateless, but the segment answers as managed",
			[]V6Mode{V6Managed, V6Stateless}},
		{"the segment answers as managed or managed-silent",
			[]V6Mode{V6Managed, V6ManagedSilent}},
		{"mode=nora", []V6Mode{V6NoRA}},
		{"slaac; managed-silent.", []V6Mode{V6SLAAC, V6ManagedSilent}},
		{"no mode here", nil},
		{"managed-silent", []V6Mode{V6ManagedSilent}},
		{"managed", []V6Mode{V6Managed}},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			got := V6ModeNamesIn(c.s)
			if len(got) != len(c.want) {
				t.Fatalf("V6ModeNamesIn(%q) = %v, want %v", c.s, got, c.want)
			}
			for _, w := range c.want {
				if !V6ModeNamed(c.s, w) {
					t.Errorf("V6ModeNamesIn(%q) = %v, missing %s", c.s, got, w)
				}
			}
		})
	}

	for _, a := range V6Modes() {
		for _, b := range V6Modes() {
			if a == b || !strings.HasPrefix(b.String(), a.String()) {
				continue
			}
			if V6ModeNamed(b.String(), a) {
				t.Errorf("a message naming only %s also reads as naming %s", b, a)
			}
		}
	}
}

// `dnsmasq --version` first lines as seen: 2.91 on the runner 2026-09-05, 2.92rel2 on the same role 2026-08-27 (#915).
func TestDnsmasqVersionFindings_TheTranscriptionPremiseIsCheckedNotAssumed(t *testing.T) {
	const transcribed = "Dnsmasq version 2.91  Copyright (c) 2000-2024 Simon Kelley"
	if findings := DnsmasqVersionFindings(transcribed); len(findings) != 0 {
		t.Fatalf("the version the tables were transcribed from is refused: %s", findings[0])
	}

	refused := []struct {
		name  string
		probe string
	}{
		{"a later minor", "Dnsmasq version 2.92rel2  Copyright (c) 2000-2024 Simon Kelley"},
		{"an earlier minor", "Dnsmasq version 2.90  Copyright (c) 2000-2024 Simon Kelley"},
		{"a later major", "Dnsmasq version 3.0  Copyright (c) 2000-2030 Simon Kelley"},
		{"the probe failed", "(could not read dnsmasq --version: exec: \"/usr/sbin/dnsmasq\": file does not exist)"},
		{"empty output", ""},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			findings := DnsmasqVersionFindings(c.probe)
			if len(findings) == 0 {
				t.Fatalf("%q is accepted; the tables are v6-minus-v4 over ONE dnsmasq's names, "+
					"and a version that renames or adds one makes the derived set silently "+
					"short", c.probe)
			}
			if !strings.Contains(findings[0], DnsmasqTablesTranscribedFrom) {
				t.Errorf("the finding does not name the version the tables came from: %s", findings[0])
			}
		})
	}

	if v, ok := parseDnsmasqVersion("Dnsmasq version " + DnsmasqTablesTranscribedFrom); !ok ||
		v != DnsmasqTablesTranscribedFrom {
		t.Errorf("DnsmasqTablesTranscribedFrom = %q does not parse as a version",
			DnsmasqTablesTranscribedFrom)
	}
}
