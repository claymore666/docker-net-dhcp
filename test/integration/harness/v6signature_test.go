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

// The v6 mode signature's own observer, in the fast lane.
//
// Every frame and every log below is VERBATIM. The frames are the LANE's
// own: run 33996052773, main-1-suite, where the mode contract test
// decodes the advertisement each mode was accepted on and prints the
// bytes it decoded. The logs are `--log-dhcp --log-facility=-` output
// from the same argv off the lane, driven by a minimal DHCPv6 sender in
// a peer namespace, because no run on this branch can make a client
// speak v6.
//
// The frames come from the lane and not from a convenient namespace
// because the first set did not, and it showed: captured with an argv
// this fixture does not use, they advertised a /120 prefix with
// infinite lifetimes where every mode of this fixture advertises /64
// with 1800s. Nothing failed -- the bits the signature reads were the
// same -- and nothing would have, because nothing tied the bytes to the
// fixture. That is the raguard_parse.go lesson twice over: an observer
// validated in a world it does not run in was already shipped once,
// keyed on a field the real image never prints.
//
// THE BOUND ON THESE CONSTANTS, in the reviewer's words: the pinned
// frames do not re-derive. If the runner's dnsmasq changes its RA, the
// fast-lane decoder test keeps passing on 2026-09-05's bytes; the lane
// fixture goes red only if a field in the signature moves. The tie is
// the contract test printing `hex.EncodeToString(frames[0].Raw)` for a
// human to refresh from -- a pin to the past by construction. Building
// the re-derivation is deliberately not this round's work; the bound is
// written here so the next reader does not have to rediscover it.

// --- verbatim frames ----------------------------------------------------

// mode=managed. dnsmasq sets M and O
// and, because it is serving addresses for this prefix itself, does NOT
// set the prefix option's A bit.
const raManagedHex = "333300000001d66740dba1e886dd6c04517e00703afffe80000000000000d46740fffedba1e8" +
	"ff0200000000000000000000000000018600df8540c007080000000000000000030440800000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc0101d667" +
	"40dba1e81f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// mode=managed-exhausted: M and O set, prefix advertised WITHOUT the
// autonomous bit, exactly as managed -- the two differ in what the
// server does with an address and not in what it advertises. Captured
// 2026-09-16 on the session box, dnsmasq 2.91 in `unshare -Urn`, the
// same run as logManagedExhausted below.
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

// mode=auto-fallback: M and O set AND the prefix advertised as
// autonomous, which no other mode in this file does. Captured
// 2026-09-16 on the session box, dnsmasq 2.91 in `unshare -Urn`, one
// veth pair, the same run as logAutoFallback below; tcpdump rather
// than racapture.go, because the mode did not exist yet when it was
// measured. Flags byte 0xc0 at ICMPv6 offset 5, prefix option
// `03 04 40 c0` -- length 64, L and A.
const raAutoFallbackHex = "33330000000176a66e95a4e486dd6c0ba59100703afffe8000000000000074a66efffe95a4e4" +
	"ff02000000000000000000000000000186003d5c40c007080000000000000000030440c00000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc010176a6" +
	"6e95a4e41f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// mode=managed-silent: byte-for-byte the managed signature. --dhcp-ignore
// changes what the server ANSWERS, not what it advertises.
const raManagedSilentHex = "33330000000162bb4b8d99c986dd6c066d8800703afffe8000000000000060bb4bfffe8d99c9" +
	"ff0200000000000000000000000000018600c1b840c007080000000000000000030440800000" +
	"07080000070800000000fd00647068650000000000000000000005010000000005dc010162bb" +
	"4b8d99c91f030000000007080676366d6f6465076578616d706c65001903000000000708fd00" +
	"6470686500000000000000000053"

// A SLAAC segment whose one prefix is advertised DEPRECATED: the
// autonomous bit is set, the valid lifetime is RFC 4861 section
// 4.6.2's infinity and the preferred lifetime is zero, so a node forms
// the address and the kernel marks it deprecated the moment it does.
//
// MEASURED 2026-09-16 on the session box, dnsmasq 2.91 under
// `unshare -Urn`, one veth pair, `--dhcp-range=fd00:6470:6865::,ra-only,deprecated`
// plus --enable-ra: the prefix option reads `03 04 40 c0 ffffffff
// 00000000`, and the library's own client formed
// fd00:6470:6865:0:b8be:26ff:fe2d:babd/64 from it in one second with
// its preferred instant already past and its valid instant the zero
// time, which is that seam's spelling of "never expires".
//
// It is the wire half of #819's deprecation arm. No mode of this
// fixture advertises it, because its five-field signature is SLAAC's
// exactly -- the difference is in the option's lifetimes, which the
// signature deliberately does not read -- so the segment is started
// through NewV6FixtureWithArgs under the slaac name and the test that
// wants it reads these two lifetimes itself.
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
		name           string
		mode           V6Mode
		hexFrame       string
		managed, other bool
		autonomous     bool
		// The prefix option's two lifetimes, in seconds. Every mode of
		// this fixture advertises 1800 for both, which is dnsmasq's
		// own default and not the fixture's 2m lease time; the
		// deprecated capture is the row that carries anything else,
		// and it is here so the two fields are read from a frame that
		// distinguishes them rather than from five that agree.
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
			// Every mode advertises the interface's own /64, whatever
			// its pool covers. This is asserted as a constant rather
			// than per row because it was per row, with /120 for the
			// two managed modes, which is what the frames captured
			// with the wrong argv said -- and no row disagreeing with
			// another is a row that checks nothing.
			if p.PrefixLen != 64 {
				t.Errorf("prefix length = %d, want 64", p.PrefixLen)
			}
			if got := p.Prefix.String(); got != "fd00:6470:6865::" {
				t.Errorf("prefix = %s, want fd00:6470:6865::", got)
			}
			if f.RouterLifetime != 1800*time.Second {
				t.Errorf("router lifetime = %s, want 30m", f.RouterLifetime)
			}
			// The prefix option's own lifetimes, which are a
			// different field from the router lifetime above and sit
			// twelve bytes further into a different option. Reading
			// one for the other is the same class of mistake as
			// reading Cur Hop Limit for the flags, and it is just as
			// plausible: five of these seven frames carry 1800 in all
			// three places.
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
Sep  5 23:33:24 dnsmasq-dhcp[747748]: 658188 DHCPSOLICIT(br0) 00:03:00:01:ce:41:ae:6d:50:36 ignored
`

	// Captured 2026-09-16 in the same shape: dnsmasq 2.91 on one end of
	// a veth pair in `unshare -Urn`, the library's own DHCPv6 client on
	// the other, LC_ALL=C. The static-only range is what makes the
	// server answer and refuse; the Advertise carries a message-level
	// Status Code 2 and the client reported Failed{nak, NoAddrsAvail}
	// on every Solicit.
	logManagedExhausted = `Sep 16 18:44:16 dnsmasq-dhcp[4182581]: DHCP, IP range 192.168.103.10 -- 192.168.103.99, lease time 2m
Sep 16 18:44:16 dnsmasq-dhcp[4182581]: DHCPv6, static leases only on fd00:6470:6865::ff, lease time 2m
Sep 16 18:44:17 dnsmasq-dhcp[4182581]: RTR-ADVERT(s0) fd00:6470:6865::
Sep 16 18:44:18 dnsmasq-dhcp[4182581]: 8008303 DHCPSOLICIT(s0) 00:03:00:01:02:42:ac:11:00:02 
Sep 16 18:44:18 dnsmasq-dhcp[4182581]: 8008303 DHCPADVERTISE(s0) 00:03:00:01:02:42:ac:11:00:02 no addresses available
Sep 16 18:44:18 dnsmasq-dhcp[4182581]: 8008303 sent size: 24 option: 13 status  2 no addresses available
`

	// Captured 2026-09-16, dnsmasq 2.91 in `unshare -Urn`, LC_ALL=C, the
	// library's own client in proto.Mode6Auto on the peer end of the
	// veth pair; the same run as raAutoFallbackHex. The client solicited
	// three times, was ignored three times, gave up on DHCPv6 and formed
	// fd00:6470:6865:0:bc1b:12ff:fe5b:c905/64 from the advertised
	// prefix, reporting SLAACFallbacks 1 and SLAACAddressesFormed 1.
	//
	// THE `available DHCP range` LINES ARE PART OF THE CAPTURE AND ARE
	// KEPT. --log-dhcp prints them for a request the server then
	// ignores, so a reader who sees them in a lane log is looking at a
	// segment that refused, not at one that served.
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
		// A no-RA segment and a managed-silent one produce THE SAME
		// DHCP log: a SOLICIT that is ignored and nothing else. They
		// differ in whether the segment ADVERTISES, which is the
		// fixture-time half and is asserted there (AssertNoRAWithin,
		// AwaitRAAfter) rather than here. The pair is declared in both
		// directions because the property is symmetric, and a row that
		// named only one direction would be claiming a discrimination
		// the log cannot carry.
		V6NoRA:          {V6NoRA: true, V6ManagedSilent: true, V6AutoFallback: true},
		V6ManagedSilent: {V6ManagedSilent: true, V6NoRA: true, V6AutoFallback: true},
		// auto-fallback's DHCP log is a third copy of that same ignored
		// SOLICIT, so it joins the pair above in all three directions.
		// The three modes are separated on the WIRE and nowhere else:
		// no-RA advertises nothing, managed-silent advertises no
		// autonomous prefix, auto-fallback advertises one. All three
		// differences are in V6Signature and are asserted at fixture
		// construction, which is why the drift matrix can tell the three
		// apart while this table cannot.
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

// TestV6ExchangeContract_ForbidsOnlyTokensTheV4PathNeverPrints is
// finding 1's guard, and it replaces one that named the property in its
// title and tested a spelling in its body.
//
// The claim: no mode's must-NOT column may name a token dnsmasq's v4
// path also prints. This fixture is dual-stack in every mode, so such a
// column fails a segment for something its v4 half did. The previous
// version of this test asserted the single literal "DHCPREQUEST" and
// therefore could not see DHCPDECLINE and DHCPRELEASE sitting in
// SLAAC's column -- and its own comment said DHCPREQUEST was "the one"
// such name, when the intersection of dnsmasq's two print tables is
// three.
//
// It is driven, not merely asserted: the mutated contracts below are
// wrong in exactly the way the shipped one was wrong, and every mode is
// driven with every ambiguous token rather than with a representative
// one. A mode with no row at all is a finding too, which is the second
// claim the old test made and the only one it kept.
func TestV6ExchangeContract_ForbidsOnlyTokensTheV4PathNeverPrints(t *testing.T) {
	// The shipped table agrees with the property.
	if findings := V6ContractFindings(v6ExchangeContract); len(findings) != 0 {
		t.Fatalf("the shipped exchange contract is not clean:\n  %s", strings.Join(findings, "\n  "))
	}

	// Drive the failure, with the real offenders rather than with a
	// token somebody thought was one. Round 1's guard tested the
	// literal "DHCPREQUEST" and therefore could not see DHCPDECLINE or
	// DHCPRELEASE sitting in SLAAC's must-NOT column.
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

	// The other direction: a token that IS v6-only may be forbidden by
	// any mode, so the guard is not simply refusing everything.
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

	// The mustLine column, which is the one that fails GREEN. A line
	// every one of whose tokens the v4 path also prints is satisfied by
	// the v4 half alone: dnsmasq writes `DHCPRELEASE(br0) ... ignored`
	// on the v4 path (rfc2131.c:1096 with the message at :1105), which
	// is the same shape as the v6 `DHCPSOLICIT ... ignored` that the
	// managed-silent row -- the one row in this table that has a
	// mustLine -- requires.
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

	// The preservation control for that column: a line whose tokens are
	// ambiguous EXCEPT for one v6-only name is fine, because that one
	// name is what the v4 path cannot write. Without this the rule
	// above could be "reject every mustLine" and still pass.
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

	// A mode with no row at all is a finding, not a silent pass.
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

// TestV6ExchangeFindings_AV4OnlyExchangeSatisfiesNoModeAndAccusesNone is
// the same property from the log side, and the SLAAC row is the reason
// it exists.
//
// SLAAC's must-NOT column is the derived v6-only set and nothing else,
// which is the decision this round made: a SLAAC segment's v4 half is
// free to do anything DHCPv4 does, including the RFC 5227 conflict path
// where the plugin sends a DHCPDECLINE, and none of it reaches the v6
// verdict. The alternative -- keeping the ambiguous tokens and
// exempting SLAAC -- would have left the same trap for the next mode
// that acquires a must-NOT column.
func TestV6ExchangeFindings_AV4OnlyExchangeSatisfiesNoModeAndAccusesNone(t *testing.T) {
	// Every v4 message name dnsmasq can print, in one log, including
	// the two that used to be in SLAAC's must-NOT column. No v6.
	//
	// These are LINES, not bare names, and the difference is the point.
	// dnsmasq's v4 log_packet appends a `message` to the name on the
	// same line (rfc2131.c:1096, with message = _("ignored") at :1105),
	// so a v4 line reads `DHCPRELEASE(br0) 192.168.103.10 aa:.. ignored`
	// -- the same shape as the v6 `DHCPSOLICIT ... ignored` the
	// stateless row requires. A log built from bare names could not
	// have caught a mustLine satisfied by the v4 half, so this test
	// would have been the second observer of a rule it could not see.
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

	// It accuses nobody: no mode's must-NOT column is tripped by it.
	for _, mode := range V6Modes() {
		for _, f := range V6ExchangeFindings(mode, v4Only) {
			if strings.Contains(f, "forbids") {
				t.Errorf("mode %s is FAILED by a v4-only exchange: %s", mode, f)
			}
		}
	}

	// And it satisfies nobody that requires anything.
	for _, mode := range V6Modes() {
		if mode == V6SLAAC {
			continue // requires nothing; see the bound on V6ExchangeFindings
		}
		if len(V6ExchangeFindings(mode, v4Only)) == 0 {
			t.Errorf("mode %s is satisfied by a v4-only exchange", mode)
		}
	}
}

// TestV6ExchangeFindings_TheMustLineIsPerLineNotWholeLog is the
// observer for countLinesWithAll's per-line scope, owed to this round
// by #915.
//
// The mustLine column exists because it fails GREEN, and the whole
// reason it is a LINE rule is that dnsmasq's v4 path writes the same
// shape: `DHCPRELEASE(br0) <addr> <mac> ignored` (rfc2131.c:1096 with
// the message at :1105). If countLinesWithAll were a whole-log
// conjunction — `strings.Contains(log, a) && strings.Contains(log, b)`
// — the two halves could come from two different lines, and no other
// test in either lane would notice: the v4-only log in
// TestV6ExchangeFindings_AV4OnlyExchangeSatisfiesNoModeAndAccusesNone
// carries `ignored` but no DHCPSOLICIT at all, so its finding fires
// under either scope.
//
// The log below is the case that separates them, and it is a segment
// that can really happen: a managed-silent fixture whose SOLICIT was
// in fact ANSWERED — the mistyped ignore directive AwaitIgnoredSolicit
// guards against — on a fixture whose v4 half refused one release.
// Both needles are in the log; neither line carries both.
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

	// Every needle on its own line, none of them together. The tokens
	// come from the contract rather than being retyped, so a contract
	// edit cannot leave this test driving a rule that no longer exists.
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

	// The other direction, so the assertion above is not satisfied by a
	// mustLine that nothing can ever meet: the same tokens on ONE line
	// produce no line finding.
	joined := "Sep  6 00:00:00 dnsmasq-dhcp[1]: " + strings.Join(rule.mustLine, " ") + "\n"
	for _, f := range V6ExchangeFindings(V6ManagedSilent, joined) {
		if strings.Contains(f, "no single log line carries all of") {
			t.Errorf("mode %s reports a missing line against a log whose one line carries "+
				"all of %v: %s", V6ManagedSilent, rule.mustLine, f)
		}
	}
}

// --- the mode signature -------------------------------------------------

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
// TestRABudget_CoversBothScheduleBranchesWithAMargin is finding 2's
// observer.
//
// The number this replaces was the literal 5s, which is
// DnsmasqFirstRAUpperBound EXACTLY: a wait for an advertisement that
// expires at the same instant as dnsmasq's own worst case for sending
// one. The reason given for the zero margin was a measured range
// ("0.950 s .. 0.983 s every time") that the lane falsified twice in
// the very run the record cited. So the margin is asserted here rather
// than argued in a comment, and it is asserted against BOTH branches:
// the 1s branch this fixture is measured to be on (radv.c:129, reached
// from dhcp6.c:715) and the 0..5s draw it is not (radv.c:135).
//
// What a too-small budget buys is not a slow test; it is the wrong
// MODE. A bring-up whose advertisement lands after the budget is a
// bring-up with no frame in hand, and a segment with no frame in hand
// classifies as nora.
func TestRABudget_CoversBothScheduleBranchesWithAMargin(t *testing.T) {
	// The 1s branch: radv.c:129 is `ra_time = now + 1`, and dnsmasq's
	// clock is integer seconds, so the frame lands in the second
	// after the one it started in.
	const fixedBranch = 2 * time.Second
	if RABudget() < fixedBranch {
		t.Errorf("the budget (%s) does not cover the `now + 1` branch (radv.c:129) with its "+
			"integer-second rounding (%s), which is the branch every measured bring-up of "+
			"this fixture is on", RABudget(), fixedBranch)
	}
	// The draw: radv.c:135. Covering it is what makes the budget a
	// bound rather than a description of the fast branch.
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
	// And it is a margin, not a rewrite: the budget stays inside the
	// no-RA window, which is the invariant the window test guards from
	// the other side.
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

// TestV6EvidenceSettled_ADisagreementAloneDoesNotFinishAnObservation is
// the regression that run 33994533077 bought.
//
// The evidence loop used to stop the moment what it had seen disagreed
// with the mode under test. That is sound for a VERDICT -- the
// disagreement is real and only grows -- and wrong for the MESSAGE,
// which is built from the same evidence and is what the drift matrix
// asserts on. A stateless fixture started with managed's flags
// disagrees on the pool bit within milliseconds, long before the
// advertisement that would have said "managed" arrives; stopping there
// reported the segment as nora, which it never was.
//
// Both outcomes are driven here, on the exact shape the lane produced.
func TestV6EvidenceSettled_ADisagreementAloneDoesNotFinishAnObservation(t *testing.T) {
	// 370 ms in: dnsmasq has written its range lines, no advertisement
	// has been captured yet.
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

	// The same segment once its first advertisement lands.
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

	// What the drift matrix actually asks for: the refusal names the
	// mode the segment IS, and it can only do that from the settled
	// observation.
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

// TestV6ModeNamesIn_APrefixOfALongerModeNameIsNotThatMode drives the
// substring trap the drift matrix's pair assertion sat in.
//
// "managed" is a prefix of "managed-silent", and the refusal a drifted
// cell produces frequently names BOTH ("the segment answers as managed
// or managed-silent") because they are indistinguishable at fixture
// time. So a Contains test for "managed" is satisfied by a message that
// names only managed-silent, and the cell that was supposed to prove
// the diagnosis proves nothing about which half of the pair was found.
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
		// The trap in isolation: naming only the longer name must not
		// name the shorter one.
		{"managed-silent", []V6Mode{V6ManagedSilent}},
		// ...and the reverse control, so this is not a function that
		// simply never reports managed.
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

	// The property, stated once rather than per row: naming the longer
	// mode never names the shorter, for every pair of modes whose names
	// overlap that way.
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

// TestDnsmasqVersionFindings_TheTranscriptionPremiseIsCheckedNotAssumed
// is finding 3's observer, and it drives the direction the lane cannot:
// a runner whose dnsmasq is not the one the tables were read from.
//
// The verbatim strings below are `dnsmasq --version`'s first line as
// this project has actually seen it -- 2.91 on the runner 2026-09-05
// and 2.92rel2 on the same machine role 2026-08-27 -- plus the sentence
// dnsmasqVersion() returns when the probe itself fails, which is the
// case that would otherwise pass by carrying no version at all.
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

	// The premise is a constant a reader can check against the tables,
	// so it must not drift into something that is not a version.
	if v, ok := parseDnsmasqVersion("Dnsmasq version " + DnsmasqTablesTranscribedFrom); !ok ||
		v != DnsmasqTablesTranscribedFrom {
		t.Errorf("DnsmasqTablesTranscribedFrom = %q does not parse as a version",
			DnsmasqTablesTranscribedFrom)
	}
}
