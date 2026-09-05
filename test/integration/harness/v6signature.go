// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, deliberately, and for the reason
// raguard_parse.go gives: everything in this file is a pure function
// over bytes or over a captured log, so it is driven in the fast lane
// against real strings and real frames rather than validated only in a
// world that needs root, a bridge and a DHCP server to enter.
//
// The fixture itself (v6modes.go) is tagged, gathers the evidence, and
// asks the functions here for the verdict. The split is the point: an
// instrument whose verdict can only be exercised on the privileged lane
// is one nobody can drive in both directions.

package harness

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

// V6Mode selects what the segment offers over IPv6.
type V6Mode int

const (
	// V6Managed is stateful DHCPv6: an address pool plus RAs with the
	// M flag. The client gets its address over DHCPv6.
	V6Managed V6Mode = iota
	// V6Stateless is the #815 shape: RAs with the O flag and no
	// address pool. The client forms its own address from the prefix
	// and asks DHCPv6 only for configuration -- an
	// INFORMATION-REQUEST answered with a REPLY carrying DNS servers
	// and a search list, and no address at all.
	V6Stateless
	// V6SLAAC is RAs with neither flag and no DHCPv6 server. Address
	// and nothing else; there is nobody to ask for configuration.
	V6SLAAC
	// V6NoRA is a DHCPv6 server on a segment with no router
	// advertisements. Nothing tells the client to speak DHCPv6, so
	// the server sits there unused. This is the negative control: it
	// separates "the plugin handled the DHCPv6 reply" from "a DHCPv6
	// server was running nearby".
	V6NoRA
	// V6ManagedSilent is managed DHCPv6 whose server answers nothing:
	// RAs carry the M flag, so the segment says an address is
	// available over DHCPv6, and every SOLICIT is dropped.
	//
	// This is the mode that keeps #868's fix honest. Tolerating a
	// missing DHCPv6 address is correct exactly when the segment never
	// offered one; here it did. A fix that read "v6 acquisition failed,
	// carry on" rather than "the advertisement said no DHCPv6, carry
	// on" passes every other mode in this file and fails only this one.
	V6ManagedSilent
)

// V6Modes is every mode, in declaration order. It exists so a table
// test iterates the population rather than a list somebody maintains
// beside it: a mode added without a row here is a mode the drift
// matrix silently stops covering.
func V6Modes() []V6Mode {
	return []V6Mode{V6Managed, V6Stateless, V6SLAAC, V6NoRA, V6ManagedSilent}
}

func (m V6Mode) String() string {
	switch m {
	case V6Managed:
		return "managed"
	case V6Stateless:
		return "stateless"
	case V6SLAAC:
		return "slaac"
	case V6NoRA:
		return "nora"
	case V6ManagedSilent:
		return "managed-silent"
	}
	return fmt.Sprintf("V6Mode(%d)", int(m))
}

// V6Signature is what a segment in one mode looks like from OUTSIDE the
// plugin: two facts from the DHCP server's own log and three from the
// router advertisement on the wire.
//
// MEASURED 2026-09-05 against dnsmasq 2.91 on the session box, one user
// namespace per mode, the frames captured with the AF_PACKET reader in
// racapture.go and decoded by ParseRA below. Lane run ids for the same
// table are in the PR body.
//
//	mode            pool  RA   M  O  PIO A   PIO L  router lifetime
//	managed         yes   yes  1  1  no      /120   1800s
//	stateless       no    yes  0  1  yes     /64    1800s
//	slaac           no    yes  0  0  yes     /64    1800s
//	nora            yes   no   -  -  -       -      -
//	managed-silent  yes   yes  1  1  no      /120   1800s
//
// The wire half is derived from dnsmasq's own source and then measured,
// which is why the two "to be measured" cells of the M7 design table
// are now filled: dnsmasq sets the M and O flags at radv.c:537-541 from
// whether the matching v6 context carries CONTEXT_DHCP and
// CONTEXT_RA_STATELESS (radv.c:627-644), and it sets the prefix
// option's A (autonomous) bit only when it is NOT serving addresses for
// that prefix (`if (do_slaac) opt->flags |= 0x40`, radv.c:748). So
// "managed" and "stateless" differ on the wire in TWO independent
// places, not one.
//
// WHY THE PREFIX-OPTION A BIT IS IN THE SIGNATURE AT ALL. Without it,
// stateless and slaac are separated only by the O flag -- one bit, in
// the one byte a decoder is most likely to read from the wrong offset
// (see ParseRA). With it, a decoder that reads Cur Hop Limit as the
// flags byte still gets slaac and stateless wrong in a way this table
// catches, because Cur Hop Limit is 64 in every one of these frames and
// carries no information at all.
//
// The PIO prefix LENGTH is measured and recorded above but deliberately
// NOT asserted: dnsmasq derives it from the range's extent (::10--::99
// fits in a /120), so it is a fact about the fixture's pool bounds and
// would go red on a pool resize that changes nothing this file is about.
type V6Signature struct {
	// Pool is whether the DHCPv6 pool's start address appears in the
	// server's log. LOCALE-PROOF: an address is an address in every
	// language, and dnsmasq's surrounding prose ("IP range") is
	// translated -- "IP-Bereich" under the German locale the
	// integration runner speaks. Matching the prose would go quietly
	// green-to-red on a locale nobody changed on purpose.
	Pool bool
	// RA is whether the server sends router advertisements at all.
	RA bool
	// Managed is the RA's M flag (RFC 4861 section 4.2): addresses are
	// available over DHCPv6.
	Managed bool
	// OtherConfig is the RA's O flag: other configuration is available
	// over DHCPv6.
	OtherConfig bool
	// AutoPrefix is whether at least one Prefix Information option
	// carries the A (autonomous address-configuration) bit, RFC 4861
	// section 4.6.2 -- "this prefix can be used for stateless address
	// autoconfiguration".
	AutoPrefix bool
}

// Signature is the mode's expected signature. It replaces the
// wantPool/wantRA pair, which could not separate stateless from slaac
// and said so.
func (m V6Mode) Signature() V6Signature {
	switch m {
	case V6Managed, V6ManagedSilent:
		return V6Signature{Pool: true, RA: true, Managed: true, OtherConfig: true, AutoPrefix: false}
	case V6Stateless:
		return V6Signature{Pool: false, RA: true, Managed: false, OtherConfig: true, AutoPrefix: true}
	case V6SLAAC:
		return V6Signature{Pool: false, RA: true, Managed: false, OtherConfig: false, AutoPrefix: true}
	case V6NoRA:
		return V6Signature{Pool: true, RA: false}
	}
	return V6Signature{}
}

// V6Evidence is everything the fixture gathered about its own segment:
// two facts read from the DHCP server's log and every router
// advertisement seen on the wire.
type V6Evidence struct {
	// PoolLogged is whether the DHCPv6 pool's start address was in the
	// server's log.
	PoolLogged bool
	// RALogged is whether the server logged at least one RTR-ADVERT(
	// line -- the server's own claim that it advertised.
	RALogged bool
	// Frames is every router advertisement the capture saw, which is
	// the wire's answer to the same question. Both columns are
	// required: the log says what the server believes it did, the wire
	// says what left the interface.
	Frames []RAFrame
}

// Observed reduces the evidence to a signature. With no frames the
// three RA fields are false, which is the no-RA row.
func (ev V6Evidence) Observed() V6Signature {
	s := V6Signature{Pool: ev.PoolLogged, RA: ev.RALogged && len(ev.Frames) > 0}
	for _, f := range ev.Frames {
		if f.Managed {
			s.Managed = true
		}
		if f.OtherConfig {
			s.OtherConfig = true
		}
		for _, p := range f.Prefixes {
			if p.Autonomous {
				s.AutoPrefix = true
			}
		}
	}
	return s
}

// ClassifyV6Segment names every mode whose signature the evidence
// matches. It returns more than one when two modes are genuinely
// indistinguishable from outside (managed and managed-silent are, until
// a client speaks), and none when the evidence matches no mode at all.
//
// It exists so a failure message can say WHICH mode the segment is in
// rather than only that it is not the one asked for. "started as
// managed, the segment answers as slaac" is a diagnosis; "mode check
// failed" is a bug report somebody else has to reproduce.
func ClassifyV6Segment(ev V6Evidence) []V6Mode {
	got := ev.Observed()
	var out []V6Mode
	for _, m := range V6Modes() {
		if m.Signature() == got {
			out = append(out, m)
		}
	}
	return out
}

// V6IndistinguishableModes returns every unordered pair of distinct
// modes whose signatures are equal -- the pairs no fixture-time check
// can separate, whatever it reads.
//
// It is DERIVED from the signature table rather than written down
// beside it. The drift matrix exempts exactly the pairs this returns,
// so a mode added later whose signature collides with an existing one
// is exempted by this function and named by the test that pins its
// size, instead of quietly passing a matrix that hand-lists the one
// collision we knew about.
func V6IndistinguishableModes() [][2]V6Mode {
	var out [][2]V6Mode
	modes := V6Modes()
	for i := 0; i < len(modes); i++ {
		for j := i + 1; j < len(modes); j++ {
			if modes[i].Signature() == modes[j].Signature() {
				out = append(out, [2]V6Mode{modes[i], modes[j]})
			}
		}
	}
	return out
}

// V6ModeFindings is the fixture's verdict: empty when the evidence says
// the segment is in the mode it was started as, one line per
// disagreement otherwise.
//
// The first finding always names BOTH the mode asked for and the mode
// the evidence matches, because that pair is the whole diagnosis and
// the drift matrix asserts on it.
func V6ModeFindings(mode V6Mode, ev V6Evidence) []string {
	want, got := mode.Signature(), ev.Observed()
	if want == got {
		return nil
	}

	var out []string
	switch actual := ClassifyV6Segment(ev); len(actual) {
	case 0:
		out = append(out, fmt.Sprintf(
			"started as %s, but the segment matches no known mode", mode))
	default:
		names := make([]string, 0, len(actual))
		for _, m := range actual {
			names = append(names, m.String())
		}
		out = append(out, fmt.Sprintf(
			"started as %s, but the segment answers as %s",
			mode, strings.Join(names, " or ")))
	}

	for _, d := range []struct {
		what      string
		want, got bool
		yes, no   string
	}{
		{"DHCPv6 address pool", want.Pool, got.Pool,
			"the pool's start address is in the server log", "it is not"},
		{"router advertisement", want.RA, got.RA,
			"an RA was logged AND captured on the wire", "none was"},
		{"RA M flag", want.Managed, got.Managed,
			"addresses are offered over DHCPv6", "they are not"},
		{"RA O flag", want.OtherConfig, got.OtherConfig,
			"other configuration is offered over DHCPv6", "it is not"},
		{"RA prefix A flag", want.AutoPrefix, got.AutoPrefix,
			"the prefix is advertised as autonomous", "it is not"},
	} {
		if d.want != d.got {
			said := d.no
			if d.got {
				said = d.yes
			}
			out = append(out, fmt.Sprintf("%s: want %v, the wire and the log say %v (%s)",
				d.what, d.want, d.got, said))
		}
	}
	return out
}

// --- the wire -----------------------------------------------------------

// RAFrame is one captured router advertisement, reduced to what the
// mode signature turns on.
type RAFrame struct {
	At time.Time
	// SourceMAC is the ethernet source. A segment with two routers on
	// it is a segment whose mode is whichever advertisement arrived
	// last, so a test that finds two distinct sources here has learned
	// something no flag comparison would have told it.
	SourceMAC net.HardwareAddr
	// CurHopLimit is carried so a test can prove the flags were NOT
	// read from this byte: it is 64 in every frame this fixture emits,
	// and 64 is 0x40, which decodes as "O set, M clear" -- the
	// stateless answer, for every mode.
	CurHopLimit uint8
	// Managed is the M flag, OtherConfig the O flag (RFC 4861 4.2).
	Managed     bool
	OtherConfig bool
	// RouterLifetime is the advertisement's router lifetime. Zero means
	// "not a default router", which is a different segment from an
	// unadvertised one and worth being able to tell apart.
	RouterLifetime time.Duration
	Prefixes       []RAPrefix
}

// RAPrefix is one Prefix Information option (RFC 4861 section 4.6.2).
type RAPrefix struct {
	Prefix    net.IP
	PrefixLen uint8
	// OnLink is the L bit, Autonomous the A bit.
	OnLink     bool
	Autonomous bool
}

func (f RAFrame) String() string {
	flags := []string{}
	if f.Managed {
		flags = append(flags, "managed")
	}
	if f.OtherConfig {
		flags = append(flags, "other stateful")
	}
	if len(flags) == 0 {
		flags = append(flags, "none")
	}
	parts := make([]string, 0, len(f.Prefixes))
	for _, p := range f.Prefixes {
		pf := []string{}
		if p.OnLink {
			pf = append(pf, "onlink")
		}
		if p.Autonomous {
			pf = append(pf, "auto")
		}
		parts = append(parts, fmt.Sprintf("%s/%d [%s]", p.Prefix, p.PrefixLen, strings.Join(pf, ", ")))
	}
	return fmt.Sprintf("%s RA src=%s flags=[%s] lifetime=%s hoplimit=%d prefixes=%s",
		f.At.Format("15:04:05.000"), f.SourceMAC, strings.Join(flags, ", "),
		f.RouterLifetime, f.CurHopLimit, strings.Join(parts, " "))
}

// The offsets ParseRA reads, spelled out because getting one of them
// wrong is the failure this decoder is most exposed to and the wrong
// answer is a plausible one rather than a crash.
//
// RFC 4861 section 4.2 lays the advertisement out as:
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|     Type      |     Code      |          Checksum             |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	| Cur Hop Limit |M|O|  Reserved |       Router Lifetime         |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// So the flags are the byte AFTER Cur Hop Limit -- ICMPv6 payload
// offset 5, not 4 -- and M is 0x80, O is 0x40. Offset 4 is Cur Hop
// Limit, which dnsmasq sets to 64 = 0x40 on every frame, so a decoder
// off by one byte reports "O set, M clear" for MANAGED, STATELESS and
// SLAAC alike and only the slaac row goes red. That is why RAFrame
// carries CurHopLimit and why the fast-lane test asserts on it.
const (
	ethHeaderLen  = 14
	ipv6HeaderLen = 40
	ethertypeIPv6 = 0x86DD
	protoICMPv6   = 58
	icmpTypeRA    = 134

	raFlagsOffset       = 5
	raFlagManaged       = 0x80
	raFlagOtherConfig   = 0x40
	raOptionOffset      = 16
	raOptPrefixInfo     = 3
	raPrefixFlagOnLink  = 0x80
	raPrefixFlagAutonom = 0x40
)

// ParseRA decodes an ethernet frame carrying an ICMPv6 Router
// Advertisement. Anything else is dropped: this instrument answers one
// question, and a frame it half-understands is worse than one it
// refuses.
//
// It does NOT walk IPv6 extension headers. A router advertisement is
// sent with hop limit 255 and, in every implementation this fixture
// runs against, ICMPv6 directly after the fixed header; a frame with an
// extension header is dropped rather than mis-parsed, which is the safe
// direction (an advertisement missed reads as absence, and absence is
// only ever asserted after a window that a present advertisement would
// have filled). Named here rather than claimed away.
func ParseRA(b []byte) (RAFrame, bool) {
	if len(b) < ethHeaderLen+ipv6HeaderLen+raOptionOffset {
		return RAFrame{}, false
	}
	if binary.BigEndian.Uint16(b[12:14]) != ethertypeIPv6 {
		return RAFrame{}, false
	}
	ip := b[ethHeaderLen:]
	if ip[0]>>4 != 6 {
		return RAFrame{}, false
	}
	if ip[6] != protoICMPv6 {
		return RAFrame{}, false
	}
	icmp := ip[ipv6HeaderLen:]
	if icmp[0] != icmpTypeRA {
		return RAFrame{}, false
	}

	f := RAFrame{
		SourceMAC:      net.HardwareAddr(append([]byte(nil), b[6:12]...)),
		CurHopLimit:    icmp[4],
		Managed:        icmp[raFlagsOffset]&raFlagManaged != 0,
		OtherConfig:    icmp[raFlagsOffset]&raFlagOtherConfig != 0,
		RouterLifetime: time.Duration(binary.BigEndian.Uint16(icmp[6:8])) * time.Second,
	}

	for o := icmp[raOptionOffset:]; len(o) >= 2; {
		optLen := int(o[1]) * 8
		if optLen == 0 || optLen > len(o) {
			break
		}
		if o[0] == raOptPrefixInfo && optLen >= 32 {
			f.Prefixes = append(f.Prefixes, RAPrefix{
				Prefix:     net.IP(append([]byte(nil), o[16:32]...)),
				PrefixLen:  o[2],
				OnLink:     o[3]&raPrefixFlagOnLink != 0,
				Autonomous: o[3]&raPrefixFlagAutonom != 0,
			})
		}
		o = o[optLen:]
	}
	return f, true
}

// --- dnsmasq's own advertisement schedule --------------------------------

// The constants dnsmasq schedules router advertisements by, read from
// its source at 2.91 (`src/radv.c`), so the fixture's absence window is
// DERIVED from the server's behaviour rather than set to a number that
// happened to work.
//
//	ra_start_unsolicited(now, NULL):     ra_time = now + rand16()/13000
//	                                     -> 0 .. 5 s   (radv.c:135)
//	ra_start_unsolicited(now, context):  ra_time = now + 1  (radv.c:129)
//	new_timeout, within 60 s of start:   now + 5 + rand16()/4400
//	                                     -> 5 .. 19 s  (radv.c:977)
//	new_timeout, after that:             3/4..1 x MaxRtrAdvInterval,
//	                                     default 600 s (radv.c:981, :999)
//
// MEASURED on the session box, dnsmasq 2.91, 12 consecutive bring-ups:
// the first advertisement arrived 0.950 s .. 0.983 s after dnsmasq
// started, every time -- the `context` branch, whose comment says
// "start after 1 second to get logging right at startup". The 0..5 s
// draw is the branch this configuration does not end up on, but it is
// the branch a different dnsmasq build or a later address arrival CAN
// end up on, so the bound below uses it and not the measurement.
const (
	dnsmasqRand16Max       = 65535
	dnsmasqFirstRADivisor  = 13000
	dnsmasqShortPeriodBase = 5
	dnsmasqShortPeriodDiv  = 4400
)

// DnsmasqFirstRAUpperBound is dnsmasq's own worst case for the delay
// from process start to the first unsolicited router advertisement.
func DnsmasqFirstRAUpperBound() time.Duration {
	return time.Duration(dnsmasqRand16Max/dnsmasqFirstRADivisor) * time.Second
}

// V6NoRAWindow is how long the fixture must watch a segment before it
// may conclude that no router advertisement is coming.
//
// It is twice DnsmasqFirstRAUpperBound, and the factor is the margin
// for everything between dnsmasq deciding to send and the capture
// recording the frame -- process scheduling on a loaded runner, and the
// fixture's own readiness poll, which runs before the window starts.
// Written as a derivation and not as a literal because the failure this
// window guards is EXACTLY a window shorter than the interval: the
// no-RA mode would then pass because it did not wait, which is a gate
// with one possible verdict dressed as evidence.
func V6NoRAWindow() time.Duration {
	return 2 * DnsmasqFirstRAUpperBound()
}

// raBudget is how long the fixture waits for an advertisement it
// EXPECTS.
//
// It stays at five seconds and is deliberately not widened. dnsmasq's
// own worst case for the first unsolicited advertisement is five
// seconds (DnsmasqFirstRAUpperBound), and the measured value on this
// segment is a shade under one, because the address is already present
// when dnsmasq starts and it takes the "start after 1 second to get
// logging right at startup" branch. The margin is therefore five-fold
// on the measurement and nil on the theoretical bound, and that is
// recorded rather than repaired: widening this is the repair that buys
// a fixture which no longer checks the thing it exists to check. If it
// ever fires spuriously the answer is to make the advertisement
// deterministic -- a router solicitation gets one back inside
// MAX_RA_DELAY_TIME -- not to wait longer.
const raBudget = 5 * time.Second

// --- the exchange --------------------------------------------------------

// The DHCPv6 message tokens dnsmasq prints, verbatim. MEASURED
// 2026-09-05 with `strings /usr/sbin/dnsmasq` on the session box's
// 2.91: each of these appears exactly once in the binary.
//
// DHCPREQUEST is deliberately ABSENT from every "must contain" set
// below even though the M7 design table lists it for the managed mode,
// and that is a correction rather than an omission. It is the one
// DHCPv6 message name dnsmasq also prints for DHCPv4
// (`rfc2131.c`), and this fixture is dual-stack in every mode -- the v4
// half leases on the same server and writes the same word. Requiring it
// is therefore satisfied by the v4 exchange alone, on a segment whose
// v6 half never answered: an assertion that cannot fail for the reason
// it names. DHCPADVERTISE is the token that actually separates a
// stateful v6 exchange from anything else, and it is v6-only.
var v6OnlyDHCPTokens = []string{
	"DHCPSOLICIT",
	"DHCPADVERTISE",
	"DHCPREPLY",
	"DHCPRENEW",
	"DHCPREBIND",
	"DHCPCONFIRM",
	"DHCPRELEASE",
	"DHCPDECLINE",
	"DHCPINFORMATION-REQUEST",
}

// v6ExchangeContract is the client-dependent half of the mode
// signature: what the server's log must and must not say once a client
// has spoken. The client-INdependent half (the pool line, RTR-ADVERT)
// is checked at fixture construction and is not repeated here.
//
// MEASURED 2026-09-05, dnsmasq 2.91, one user namespace per mode, a
// minimal DHCPv6 sender in the peer namespace; the captured logs are
// pasted verbatim into v6signature_test.go and are what the fast lane
// drives this table against.
//
// Two cells differ from the M7 design table, both measured:
//
//   - stateless does NOT log DHCPREPLY. dnsmasq answers an
//     Information-request (the client receives message type 7) but
//     logs only the DHCPINFORMATION-REQUEST line -- `log6_quiet` is
//     called once, at rfc3315.c:1144, and there is no reply-side call
//     on that path. Requiring DHCPREPLY here would fail every stateless
//     scenario M7d writes, against a server that behaved correctly.
//   - managed does not require DHCPREQUEST, for the v4 collision above.
//
// The `ignored` needle for managed-silent is a TRANSLATED string, not a
// protocol token: dnsmasq writes it as `_("ignored")` (rfc3315.c:652)
// and po/de.po:2083 renders it "ignoriert". It is safe here only
// because withCLocale pins every fixture server to LC_ALL=C, so the
// dependency is on that helper and is named rather than assumed.
var v6ExchangeContract = map[V6Mode]struct {
	must    []string
	mustNot []string
	// mustLine is a set of substrings that have to appear on ONE line
	// together, which is a different claim from each appearing
	// somewhere.
	mustLine []string
}{
	V6Managed: {
		must: []string{"DHCPSOLICIT", "DHCPADVERTISE", "DHCPREPLY"},
	},
	V6Stateless: {
		must:    []string{"DHCPINFORMATION-REQUEST"},
		mustNot: []string{"DHCPADVERTISE"},
	},
	V6SLAAC: {
		mustNot: v6OnlyDHCPTokens,
	},
	V6NoRA: {
		must: []string{"DHCPSOLICIT"},
	},
	V6ManagedSilent: {
		mustLine: []string{"DHCPSOLICIT", "ignored"},
		mustNot:  []string{"DHCPADVERTISE", "DHCPREPLY"},
	},
}

// V6ExchangeFindings reports how the server's log disagrees with what
// the mode says a completed client exchange looks like. Empty means it
// agrees.
//
// THE BOUND, named rather than claimed away: for V6SLAAC the contract
// is a must-NOT set and nothing else, because a SLAAC-only segment has
// no DHCPv6 server to talk to and therefore nothing client-dependent to
// require. So this function CANNOT distinguish "the client ran and
// correctly said nothing" from "no client ran" in that mode, and it
// returns no findings for an empty log. Every other mode's must-set
// makes an empty log a finding, which is what makes the live negative
// control in the fixture's contract test meaningful.
func V6ExchangeFindings(mode V6Mode, log string) []string {
	c, ok := v6ExchangeContract[mode]
	if !ok {
		return []string{fmt.Sprintf("no exchange contract for mode %s", mode)}
	}
	var out []string
	for _, tok := range c.must {
		if !strings.Contains(log, tok) {
			out = append(out, fmt.Sprintf(
				"mode %s: the server never logged %q, so the exchange this mode is defined by did not happen",
				mode, tok))
		}
	}
	if len(c.mustLine) > 0 && countLinesWithAll(log, c.mustLine) == 0 {
		out = append(out, fmt.Sprintf(
			"mode %s: no single log line carries all of %v; the parts appearing separately is a different segment",
			mode, c.mustLine))
	}
	for _, tok := range c.mustNot {
		if strings.Contains(log, tok) {
			out = append(out, fmt.Sprintf(
				"mode %s: the server logged %q, which this mode forbids", mode, tok))
		}
	}
	return out
}

// countLinesWithAll counts log lines containing every one of subs.
func countLinesWithAll(log string, subs []string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if line == "" {
			continue
		}
		all := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}
