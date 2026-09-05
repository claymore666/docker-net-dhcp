// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// The v6 mode signature's own observer, in the fast lane.
//
// Every frame and every log below is VERBATIM: the frames were captured
// off the fixture's bridge with the AF_PACKET reader in racapture.go,
// one user namespace per mode, dnsmasq 2.91, 2026-09-05, and printed as
// hex; the logs are the same runs' `--log-dhcp --log-facility=-` output
// with a minimal DHCPv6 sender in the peer namespace. That is the
// raguard_parse.go pattern and it is here for the same reason: an
// observer validated in a world it does not run in was already shipped
// once, keyed on a field the real image never prints.

// --- verbatim frames ----------------------------------------------------

// Captured on the fixture bridge, mode=managed. dnsmasq sets M and O
// and, because it is serving addresses for this prefix itself, does NOT
// set the prefix option's A bit.
const raManagedHex = "333300000001dae712f7a02986dd6c0b662b00703afffe80000000000000d8e712fffef7a029" +
	"ff020000000000000000000000000001860019ec40c00708000000000000000003047880ffffffff" +
	"ffffffff00000000fd00647068650000000000000000000005010000000005dc0101dae712f7a029" +
	"1f030000ffffffff0676366d6f6465076578616d706c650019030000fffffffffd00647068650000" +
	"0000000000000053"

// mode=stateless: O set, M clear, prefix advertised as autonomous.
const raStatelessHex = "33330000000132ab352e26a586dd6c02f8a700703afffe8000000000000030ab35fffe2e26a5" +
	"ff02000000000000000000000000000186003520404007080000000000000000030440c000000708" +
	"0000070800000000fd00647068650000000000000000000005010000000005dc010132ab352e26a5" +
	"1f030000000007080676366d6f6465076578616d706c65001903000000000708fd00647068650000" +
	"0000000000000053"

// mode=slaac: neither flag, prefix autonomous.
const raSLAACHex = "3333000000010271220de30986dd6c08501400703afffe80000000000000007122fffe0de309" +
	"ff0200000000000000000000000000018600434d400007080000000000000000030440c000000708" +
	"0000070800000000fd00647068650000000000000000000005010000000005dc01010271220de309" +
	"1f030000000007080676366d6f6465076578616d706c65001903000000000708fd00647068650000" +
	"0000000000000053"

// mode=managed-silent: byte-for-byte the managed signature. --dhcp-ignore
// changes what the server ANSWERS, not what it advertises.
const raManagedSilentHex = "3333000000015a759226972886dd6c05a40c00703afffe80000000000000587592fffe269728" +
	"ff02000000000000000000000000000186002e7440c00708000000000000000003047880ffff" +
	"ffffffffffff00000000fd00647068650000000000000000000005010000000005dc01015a7592269728" +
	"1f030000ffffffff0676366d6f6465076578616d706c650019030000fffffffffd00647068650000" +
	"0000000000000053"

func mustFrame(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("fixture frame is not hex: %v", err)
	}
	return b
}

// TestParseRA_ReadsTheFlagsFromTheByteAfterCurHopLimit is the decoder's
// load-bearing case.
//
// RFC 4861 section 4.2 puts the M and O flags in the octet AFTER Cur Hop
// Limit. Reading Cur Hop Limit instead is the mistake worth a test of
// its own rather than a comment, because it does not crash and it does
// not look wrong: dnsmasq sets Cur Hop Limit to 64 on every one of these
// frames, 64 is 0x40, and 0x40 is exactly "O set, M clear" -- so an
// off-by-one decoder reports STATELESS for the managed segment, for the
// stateless segment and for the managed-silent segment alike, and only
// the slaac row goes red. Three of five modes would pass while the
// instrument measured a constant.
//
// The three rows below therefore differ from each other in the flags
// byte and agree with each other in Cur Hop Limit, which is what makes
// them able to tell those two readings apart at all.
func TestParseRA_ReadsTheFlagsFromTheByteAfterCurHopLimit(t *testing.T) {
	cases := []struct {
		mode           V6Mode
		hexFrame       string
		managed, other bool
		autonomous     bool
		prefixLen      uint8
	}{
		{V6Managed, raManagedHex, true, true, false, 120},
		{V6Stateless, raStatelessHex, false, true, true, 64},
		{V6SLAAC, raSLAACHex, false, false, true, 64},
		{V6ManagedSilent, raManagedSilentHex, true, true, false, 120},
	}
	for _, c := range cases {
		t.Run(c.mode.String(), func(t *testing.T) {
			f, ok := ParseRA(mustFrame(t, c.hexFrame))
			if !ok {
				t.Fatalf("ParseRA refused a captured router advertisement")
			}
			if f.Managed != c.managed || f.OtherConfig != c.other {
				t.Errorf("M=%v O=%v, want M=%v O=%v", f.Managed, f.OtherConfig, c.managed, c.other)
			}
			// The control for the off-by-one: every one of these
			// frames carries Cur Hop Limit 64, so a decoder that read
			// the flags from here would report the same answer for all
			// four rows above -- and the rows above disagree.
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
			if p.PrefixLen != c.prefixLen {
				t.Errorf("prefix length = %d, want %d", p.PrefixLen, c.prefixLen)
			}
			if got := p.Prefix.String(); got != "fd00:6470:6865::" {
				t.Errorf("prefix = %s, want fd00:6470:6865::", got)
			}
			if f.RouterLifetime != 1800*time.Second {
				t.Errorf("router lifetime = %s, want 30m", f.RouterLifetime)
			}
			if f.SourceMAC == nil || len(f.SourceMAC) != 6 {
				t.Errorf("source MAC = %v, want six bytes", f.SourceMAC)
			}
		})
	}
}

// TestParseRA_RefusesWhatIsNotARouterAdvertisement drives the other
// direction. A decoder that accepts anything makes every "an RA
// arrived" assertion true for any traffic at all, and the socket this
// runs behind is bound to ETH_P_ALL, so it really does see everything
// on the link.
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
	// Preservation control: the unmutated frame still parses, so the
	// rejections above are not a decoder that refuses everything.
	if _, ok := ParseRA(good); !ok {
		t.Error("the unmutated captured frame no longer parses")
	}
}

// --- verbatim server logs -----------------------------------------------

// Captured 2026-09-05, dnsmasq 2.91, one user namespace per mode, a
// minimal DHCPv6 sender in the peer namespace. Trimmed to the lines the
// contract reads; no line is edited.
const (
	logManaged = `Sep  5 23:32:56 dnsmasq-dhcp[747116]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:32:56 dnsmasq-dhcp[747116]: DHCPv6, IP range fd00:6470:6865::10 -- fd00:6470:6865::99, lease time 2m
Sep  5 23:32:57 dnsmasq-dhcp[747116]: RTR-ADVERT(br0) fd00:6470:6865::
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658188 DHCPSOLICIT(br0) 00:03:00:01:9e:14:a2:d9:ef:ef 
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658188 DHCPADVERTISE(br0) fd00:6470:6865::54 00:03:00:01:9e:14:a2:d9:ef:ef 
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658189 DHCPREQUEST(br0) 00:03:00:01:9e:14:a2:d9:ef:ef 
Sep  5 23:32:58 dnsmasq-dhcp[747116]: 658189 DHCPREPLY(br0) fd00:6470:6865::54 00:03:00:01:9e:14:a2:d9:ef:ef 
`

	// The stateless log is the reason one cell of the M7 design table
	// is wrong. dnsmasq ANSWERS the Information-request -- the sender
	// received message type 7 -- and logs no DHCPREPLY line for it:
	// log6_quiet is called once on that path, at rfc3315.c:1144.
	// Requiring DHCPREPLY here would have failed every stateless
	// scenario against a server that behaved correctly.
	logStateless = `Sep  5 23:33:11 dnsmasq-dhcp[747377]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:33:11 dnsmasq-dhcp[747377]: DHCPv6 stateless on fd00:6470:6865::
Sep  5 23:33:12 dnsmasq-dhcp[747377]: RTR-ADVERT(br0) fd00:6470:6865::
Sep  5 23:33:13 dnsmasq-dhcp[747377]: 658188 DHCPINFORMATION-REQUEST(br0) 00:03:00:01:8e:8a:3f:b8:e5:8a 
Sep  5 23:33:13 dnsmasq-dhcp[747377]: 658188 sent size: 16 option: 23 dns-server  fd00:6470:6865::53
`

	// A SLAAC-only range gives dnsmasq no DHCPv6 server for the
	// prefix, so the Solicit the sender emitted is not answered and
	// not logged at all.
	logSLAAC = `Sep  5 23:33:15 dnsmasq-dhcp[747532]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:33:16 dnsmasq-dhcp[747532]: RTR-ADVERT(br0) fd00:6470:6865::
Sep  5 23:33:16 dnsmasq-dhcp[747532]: RTR-ADVERT(br0) fd00:6470:6865::
`

	logNoRA = `Sep  5 23:33:22 dnsmasq-dhcp[747748]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep  5 23:33:22 dnsmasq-dhcp[747748]: DHCPv6, IP range fd00:6470:6865::10 -- fd00:6470:6865::99, lease time 2m
Sep  5 23:33:24 dnsmasq-dhcp[747748]: 658188 DHCPSOLICIT(br0) 00:03:00:01:ce:41:ae:6d:50:36 
Sep  5 23:33:24 dnsmasq-dhcp[747748]: 658188 DHCPADVERTISE(br0) fd00:6470:6865::90 00:03:00:01:ce:41:ae:6d:50:36 
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
	}
	return ""
}

// TestV6ExchangeFindings_EachModesOwnLogPassesAndTheOthersDoNot drives
// the exchange contract in both directions, per mode, against the logs
// a real exchange produced.
//
// The off-diagonal is the half that matters. A contract whose must-set
// is empty and whose must-NOT set is empty passes every log, which is
// what "AssertExchange" would silently become if a mode's row were
// dropped -- and the diagonal alone cannot see that.
func TestV6ExchangeFindings_EachModesOwnLogPassesAndTheOthersDoNot(t *testing.T) {
	// SLAAC's contract is a must-NOT set and nothing else, because a
	// SLAAC-only segment has no DHCPv6 server to complete an exchange
	// with. Its row therefore passes any log with no v6 DHCP token in
	// it, and that is a property of the mode rather than a gap in the
	// table -- the bound is stated on V6ExchangeFindings and pinned
	// here so it cannot widen unnoticed.
	passesForeignLogs := map[V6Mode]map[V6Mode]bool{
		V6SLAAC: {V6SLAAC: true},
		// A no-RA segment's client-dependent evidence is a SOLICIT, and
		// the managed log has one too: the two modes differ in whether
		// the segment ADVERTISES, which is the fixture-time half.
		V6NoRA: {V6NoRA: true, V6Managed: true, V6ManagedSilent: true},
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

// TestV6ExchangeFindings_AnEmptyLogFailsEveryModeThatRequiresOne is the
// live negative control's fast-lane twin: a fixture on which no client
// ever ran has a log with no exchange in it, and AssertExchange must
// refuse rather than pass.
//
// SLAAC is the named exception and the reason the exception is named:
// it requires nothing, so it cannot detect that nothing happened.
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

// TestV6ExchangeFindings_NoNeedleIsProseDnsmasqTranslates.
//
// dnsmasq is translated and the integration runner speaks German. A
// needle that gettext rewrites matches nothing under that locale, and a
// must-NOT set that matches nothing passes VACUOUSLY -- the failure
// that does not announce itself. Every needle in the contract is
// therefore either an upper-case protocol token dnsmasq prints verbatim
// or the one translated word this harness knowingly depends on, and
// that one is safe only because withCLocale pins the server to LC_ALL=C.
func TestV6ExchangeFindings_NoNeedleIsProseDnsmasqTranslates(t *testing.T) {
	// The single knowingly-translated needle, and the reason it is
	// allowed: `_("ignored")`, rfc3315.c:652, rendered "ignoriert" by
	// dnsmasq's own po/de.po. locale_test.go is what keeps withCLocale
	// on every server this harness starts.
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

// TestV6ExchangeContract_CoversEveryModeAndNamesNoV4AmbiguousToken.
//
// Two claims, both of which a reader would otherwise have to take on
// trust. Every mode has a row -- a mode without one returns "no
// exchange contract" rather than passing. And DHCPREQUEST appears
// nowhere, because it is the one DHCPv6 message name dnsmasq also
// prints for DHCPv4 and this fixture is dual-stack in every mode: a
// must-set containing it is satisfied by the v4 lease alone, on a
// segment whose v6 half never answered.
func TestV6ExchangeContract_CoversEveryModeAndNamesNoV4AmbiguousToken(t *testing.T) {
	for _, mode := range V6Modes() {
		if _, ok := v6ExchangeContract[mode]; !ok {
			t.Errorf("mode %s has no exchange contract row", mode)
		}
	}
	for mode, c := range v6ExchangeContract {
		for _, n := range append(append(append([]string{}, c.must...), c.mustNot...), c.mustLine...) {
			if n == "DHCPREQUEST" {
				t.Errorf("mode %s names DHCPREQUEST, which dnsmasq also prints for DHCPv4; "+
					"the v4 half of this dual-stack fixture satisfies it on its own", mode)
			}
		}
	}
	// The v4 log line this fixture always produces must not, on its
	// own, satisfy any mode's must-set.
	const v4Only = "Sep  5 23:32:58 dnsmasq-dhcp[1]: DHCPREQUEST(br0) 192.168.103.10 aa:bb:cc:dd:ee:ff\n" +
		"Sep  5 23:32:58 dnsmasq-dhcp[1]: DHCPACK(br0) 192.168.103.10 aa:bb:cc:dd:ee:ff\n"
	for _, mode := range V6Modes() {
		if mode == V6SLAAC {
			continue // requires nothing; see the bound on V6ExchangeFindings
		}
		if len(V6ExchangeFindings(mode, v4Only)) == 0 {
			t.Errorf("mode %s is satisfied by a v4-only exchange", mode)
		}
	}
}

// --- the mode signature -------------------------------------------------

func evidenceFor(t *testing.T, mode V6Mode) V6Evidence {
	t.Helper()
	hexes := map[V6Mode]string{
		V6Managed:       raManagedHex,
		V6Stateless:     raStatelessHex,
		V6SLAAC:         raSLAACHex,
		V6ManagedSilent: raManagedSilentHex,
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

// TestV6ModeFindings_TheCapturedEvidenceMatchesTheModeItCameFrom is the
// signature table's diagonal, driven against real frames rather than
// against the table restated.
func TestV6ModeFindings_TheCapturedEvidenceMatchesTheModeItCameFrom(t *testing.T) {
	for _, mode := range V6Modes() {
		t.Run(mode.String(), func(t *testing.T) {
			if f := V6ModeFindings(mode, evidenceFor(t, mode)); len(f) != 0 {
				t.Errorf("the segment's own captured evidence was rejected: %v", f)
			}
		})
	}
}

// TestV6ModeFindings_EvidenceFromAnotherModeIsRejectedAndNamesThePair
// is the drift matrix's fast-lane twin: the same statement, without a
// bridge, so it runs on every push instead of only on the privileged
// lane.
//
// The exempt pairs are DERIVED from the signature table by
// V6IndistinguishableModes rather than listed here. A mode added later
// whose signature collides with an existing one is exempted by that
// function and counted by the test below, instead of quietly making a
// hand-written exemption list wrong.
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

// TestV6IndistinguishableModes_IsExactlyManagedAndManagedSilent pins
// the size and the membership of the exemption the matrix above
// derives.
//
// Without this, a change that made two more modes look alike would
// silently shrink the matrix: the derivation would exempt the new pair
// and nothing would say so. The pair that IS exempt is closed by
// AwaitIgnoredSolicit and by AssertExchange, which read the one thing
// that separates managed from managed-silent -- what the server does
// when a client finally asks.
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

// TestClassifyV6Segment_NamesTheModeOrNothing.
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
	// Evidence that matches nothing must name nothing, rather than
	// falling back on the first row of the table.
	impossible := V6Evidence{PoolLogged: false, RALogged: false}
	if got := ClassifyV6Segment(impossible); len(got) != 0 {
		t.Errorf("classified an empty segment as %v; no mode has that signature", got)
	}
}

// TestV6NoRAWindow_IsLongerThanDnsmasqsOwnWorstCase.
//
// The negative half of trap 1 fails silently when the window is shorter
// than the interval at which the server would have advertised: the
// no-RA mode then passes because it did not wait. The window is derived
// from dnsmasq's own scheduling constants rather than written as a
// number, and this is the assertion that the derivation still points
// the right way after somebody edits one of them.
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
	if V6NoRAWindow() <= raBudget {
		t.Errorf("the no-RA window (%s) is not longer than the budget the POSITIVE case spends "+
			"(%s); a segment declared silent on less evidence than one declared noisy",
			V6NoRAWindow(), raBudget)
	}
}
