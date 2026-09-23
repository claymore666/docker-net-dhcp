// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Untagged so the verdict functions run in the fast lane; the tagged fixture in v6modes.go gathers the evidence (#911).

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
	// V6Managed is stateful DHCPv6: an address pool and RAs with the M flag.
	V6Managed V6Mode = iota
	// V6Stateless is the #815 shape: RAs with the O flag, no pool, and an Information-request answered with configuration.
	V6Stateless
	// V6SLAAC is RAs with neither flag and no DHCPv6 server.
	V6SLAAC
	// V6NoRA is a segment with no RAs and a DHCPv6 server that answers nothing, which dhcpv6_no_router_advert needs.
	V6NoRA
	// V6ManagedSilent is managed DHCPv6 whose server drops every Solicit, which keeps #868's fix honest.
	V6ManagedSilent
	// V6ManagedExhausted is managed DHCPv6 answering every Solicit with Status Code NoAddrsAvail (RFC 9915 section 21.13).
	//
	// It is #816's other half beside managed-silent. Measured 2026-09-16, dnsmasq 2.91 under `unshare -Urn`, the
	// library's client on a veth peer: "DHCPADVERTISE(s0) ... no addresses available", "option: 13 status 2", and the
	// RA carrying [managed, other stateful] with a [onlink] prefix and no A bit (#989). dnsmasq spells it as a static-only
	// v6 --dhcp-range: address_allocate skips every CONTEXT_STATIC context (dhcp6.c:497).
	V6ManagedExhausted
	// V6AutoFallback advertises M, O and an autonomous prefix and ignores every Solicit, so Mode6Auto's fallback runs (#818).
	//
	// dnsmasq clears the A bit on a prefix it serves addresses for (radv.c:748), so managed-silent has nothing to fall
	// back to. dnsmasq spells it as a v6 --dhcp-range with third field `slaac`: option.c:3823-3830 adds CONTEXT_RA to
	// CONTEXT_DHCP, so radv.c:627-644 sets M and O and radv.c:748 the A bit, from one range.
	// Measured 2026-09-16, dnsmasq 2.91 under `unshare -Urn`, the library client in Mode6Auto: RA flags
	// [managed, other stateful], prefix [onlink, auto] /64, three ignored DHCPSOLICITs, acquired at 7.9 s with
	// slaac=true, SLAACFallbacks 1 and SLAACAddressesFormed 1.
	V6AutoFallback
)

// V6Modes is every mode, in declaration order.
func V6Modes() []V6Mode {
	return []V6Mode{V6Managed, V6Stateless, V6SLAAC, V6NoRA, V6ManagedSilent, V6ManagedExhausted, V6AutoFallback}
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
	case V6ManagedExhausted:
		return "managed-exhausted"
	case V6AutoFallback:
		return "auto-fallback"
	}
	return fmt.Sprintf("V6Mode(%d)", int(m))
}

// V6Signature is a segment's mode seen from outside: two facts from the server log and three from the RA (#911).
//
// Measured 2026-09-05, dnsmasq 2.91, one user namespace per mode (auto-fallback 2026-09-16 with tcpdump, #818):
//
//	mode pool/RA/M/O/PIO-A/PIO-L: managed y/y/1/1/n//120, stateless n/y/0/1/y//64, slaac n/y/0/0/y//64, nora y/n/-,
//	managed-silent y/y/1/1/n//120, managed-exhausted n/y/1/1/n//64, auto-fallback y/y/1/1/y//64; lifetime 1800s.
//
// dnsmasq sets M and O from CONTEXT_DHCP and CONTEXT_RA_STATELESS (radv.c:537-541, :627-644) and the A bit only when
// not serving addresses for the prefix (radv.c:748). The PIO length follows the pool extent and is not asserted.
type V6Signature struct {
	// Pool is whether the pool's start address is in the log; dnsmasq's "IP range" prose is translated ("IP-Bereich").
	Pool bool
	// RA is whether the server sends router advertisements at all.
	RA bool
	// Managed is the RA's M flag (RFC 4861 section 4.2).
	Managed bool
	// OtherConfig is the RA's O flag (RFC 4861 section 4.2).
	OtherConfig bool
	// AutoPrefix is whether a Prefix Information option carries the A bit (RFC 4861 section 4.6.2).
	AutoPrefix bool
}

// Signature is the mode's expected signature.
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
	case V6ManagedExhausted:
		// A static-only range logs "static leases only on <addr>" and never the pool start, which separates this mode (#989).
		return V6Signature{Pool: false, RA: true, Managed: true, OtherConfig: true, AutoPrefix: false}
	case V6AutoFallback:
		return V6Signature{Pool: true, RA: true, Managed: true, OtherConfig: true, AutoPrefix: true}
	}
	return V6Signature{}
}

// V6Evidence is the server-log facts and every RA the fixture captured on its segment.
type V6Evidence struct {
	// PoolLogged is whether the pool's start address was in the server's log.
	PoolLogged bool
	// RALogged is whether the server logged an RTR-ADVERT( line.
	RALogged bool
	// Frames is every RA the capture saw.
	Frames []RAFrame
}

// Observed reduces the evidence to a signature; with no frames the RA fields are false.
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

// V6ModeNamesIn returns the modes named in s as whole names, since "managed" prefixes "managed-silent".
func V6ModeNamesIn(s string) []V6Mode {
	var out []V6Mode
	for _, m := range V6Modes() {
		name := m.String()
		for i := 0; ; {
			j := strings.Index(s[i:], name)
			if j < 0 {
				break
			}
			j += i
			if !isV6NameByte(byteAt(s, j-1)) && !isV6NameByte(byteAt(s, j+len(name))) {
				out = append(out, m)
				break
			}
			i = j + 1
		}
	}
	return out
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func isV6NameByte(b byte) bool { return (b >= 'a' && b <= 'z') || b == '-' }

// V6ModeNamed reports whether s names m as a whole mode name.
func V6ModeNamed(s string, m V6Mode) bool {
	for _, got := range V6ModeNamesIn(s) {
		if got == m {
			return true
		}
	}
	return false
}

// V6EvidenceSettled reports whether one RA has arrived, after which the verdict cannot change.
//
// Stopping on disagreement reported an ordinary managed segment as nora at 370 ms (run 33994533077, #911).
func V6EvidenceSettled(ev V6Evidence) bool { return len(ev.Frames) > 0 }

// ClassifyV6Segment names every mode whose signature the evidence matches.
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

// V6IndistinguishableModes returns every unordered pair of distinct modes with equal signatures.
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

// V6ModeFindings is the fixture's verdict, naming the requested and the matched mode first; empty means it agrees.
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

// RAFrame is one captured router advertisement.
type RAFrame struct {
	At time.Time
	// Raw is the frame as captured. Frames pinned on 2026-09-05 from another argv advertised a /120 prefix with infinite
	// lifetimes; the lane emits /64 and 1800s (#911).
	Raw []byte
	// SourceMAC is the ethernet source.
	SourceMAC net.HardwareAddr
	// CurHopLimit is 64 (0x40) in every fixture frame, which decodes as "O set, M clear" if read as the flags byte.
	CurHopLimit uint8
	// Managed is the M flag, OtherConfig the O flag (RFC 4861 section 4.2).
	Managed     bool
	OtherConfig bool
	// RouterLifetime is the router lifetime; zero means not a default router.
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
	// ValidLifetime and PreferredLifetime are wire seconds, since RFC 4861 section 4.6.2 spells infinity 0xFFFFFFFF (#819).
	ValidLifetime     uint32
	PreferredLifetime uint32
}

// RAInfiniteLifetime is RFC 4861 section 4.6.2's infinity; the kernel spells it as zero in IFA_CACHEINFO (#819).
const RAInfiniteLifetime uint32 = 0xFFFFFFFF

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
		parts = append(parts, fmt.Sprintf("%s/%d [%s] valid=%s preferred=%s",
			p.Prefix, p.PrefixLen, strings.Join(pf, ", "),
			raLifetimeString(p.ValidLifetime), raLifetimeString(p.PreferredLifetime)))
	}
	return fmt.Sprintf("%s RA src=%s flags=[%s] lifetime=%s hoplimit=%d prefixes=%s",
		f.At.Format("15:04:05.000"), f.SourceMAC, strings.Join(flags, ", "),
		f.RouterLifetime, f.CurHopLimit, strings.Join(parts, " "))
}

func raLifetimeString(secs uint32) string {
	if secs == RAInfiniteLifetime {
		return "infinite"
	}
	return fmt.Sprintf("%ds", secs)
}

// RFC 4861 section 4.2: the flags byte is ICMPv6 offset 5, after Cur Hop Limit at 4; M is 0x80, O is 0x40. dnsmasq
// sets Cur Hop Limit to 64, so a decoder one byte off reads "O set, M clear" for every mode (#911).
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
	// RFC 4861 section 4.6.2: valid lifetime at option offset 4, preferred at 8, prefix at 16.
	raPrefixValidOffset     = 4
	raPrefixPreferredOffset = 8
)

// ParseRA decodes an ethernet frame carrying an ICMPv6 RA and drops anything else, including extension headers.
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
		Raw:            append([]byte(nil), b...),
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
				Prefix:            net.IP(append([]byte(nil), o[16:32]...)),
				PrefixLen:         o[2],
				OnLink:            o[3]&raPrefixFlagOnLink != 0,
				Autonomous:        o[3]&raPrefixFlagAutonom != 0,
				ValidLifetime:     binary.BigEndian.Uint32(o[raPrefixValidOffset : raPrefixValidOffset+4]),
				PreferredLifetime: binary.BigEndian.Uint32(o[raPrefixPreferredOffset : raPrefixPreferredOffset+4]),
			})
		}
		o = o[optLen:]
	}
	return f, true
}

// dnsmasq 2.91's RA schedule (#911): ra_start_unsolicited(now, NULL) draws 0..5 s (radv.c:135), (now, context)
// fires at now+1 (radv.c:129), new_timeout within 60 s is 5..19 s (radv.c:977), then 3/4..1 x 600 s (radv.c:981).
// Every range here reaches dhcp6.c:715 "First time found, do fast RA", so the now+1 branch overwrites the draw.
// Measured 99 first RAs: 60 off the lane (0.931..0.971 s) and 39 on lane runs 33994533077..34001581367; 97 landed
// at 0.931..1.041 s and 2 at 13 and 18 ms, where send_alarm took its alarm(0) exit (dnsmasq.c:1371-1379). Later gaps
// were 5..19 s as new_timeout says. Lane runs (n): 33994533077 (1), 33995361430 (5), 33996052773 (5), 33996650903 (3),
// 33997007028 (0), 33997353467 (5, the 18 ms frame), 33997868882 (5, the 13 ms frame), 34000578906, 34000948877, 34001581367 (5 each).
const (
	dnsmasqRand16Max       = 65535
	dnsmasqFirstRADivisor  = 13000
	dnsmasqShortPeriodBase = 5
	dnsmasqShortPeriodDiv  = 4400
)

// DnsmasqFirstRAUpperBound is dnsmasq's worst first-RA delay over both branches, the 0..5 s draw (radv.c:135).
func DnsmasqFirstRAUpperBound() time.Duration {
	return time.Duration(dnsmasqRand16Max/dnsmasqFirstRADivisor) * time.Second
}

// Covers fork, exec, config parse and SIGALRM delivery: the largest overshoot of 99 was 41 ms (run 33996052773, #911).
const firstRASlop = 1 * time.Second

// V6NoRAWindow is twice RABudget, how long the fixture watches before concluding no RA is coming.
func V6NoRAWindow() time.Duration {
	return 2 * RABudget()
}

// RABudget is how long the fixture waits for an expected RA; an early expiry reads as the nora mode.
func RABudget() time.Duration { return DnsmasqFirstRAUpperBound() + firstRASlop }

// dnsmasq 2.91's message-name tables, measured 2026-09-06 as every "DHCP..." literal in src/rfc2131.c (v4) and
// src/rfc3315.c (v6), the only two files holding one (#911).
var dnsmasqV4MessageNames = []string{
	"DHCPACK",
	"DHCPDECLINE",
	"DHCPDISCOVER",
	"DHCPINFORM",
	"DHCPNAK",
	"DHCPOFFER",
	"DHCPRELEASE",
	"DHCPREQUEST",
}

var dnsmasqV6MessageNames = []string{
	"DHCPADVERTISE",
	"DHCPCONFIRM",
	"DHCPDECLINE",
	"DHCPINFORMATION-REQUEST",
	"DHCPREBIND",
	"DHCPRELEASE",
	"DHCPRENEW",
	"DHCPREPLY",
	"DHCPREQUEST",
	"DHCPSOLICIT",
}

// DnsmasqTablesTranscribedFrom is the dnsmasq version the tables were read from; the runner had 2.92rel2 on 2026-08-27
// and 2.91 on 2026-09-05, and another version is red in the fixture (#911).
const DnsmasqTablesTranscribedFrom = "2.91"

// DnsmasqVersionFindings reports how the `dnsmasq --version` probe disagrees with DnsmasqTablesTranscribedFrom.
func DnsmasqVersionFindings(probe string) []string {
	got, ok := parseDnsmasqVersion(probe)
	if !ok {
		return []string{fmt.Sprintf(
			"the dnsmasq version probe returned %q, which carries no version; the DHCP "+
				"message-name tables in v6signature.go were transcribed from %s's source and "+
				"nothing here can say whether that still holds",
			probe, DnsmasqTablesTranscribedFrom)}
	}
	if got != DnsmasqTablesTranscribedFrom {
		return []string{fmt.Sprintf(
			"this dnsmasq is %s; the DHCP message-name tables in v6signature.go were "+
				"transcribed from %s's src/rfc2131.c and src/rfc3315.c, and V6OnlyDHCPTokens() "+
				"is v6-minus-v4 over those tables. A version that adds or renames a v6 message "+
				"name makes that set silently short, so re-run the two greps named beside the "+
				"tables against %s's source and update them and this constant together",
			got, DnsmasqTablesTranscribedFrom, got)}
	}
	return nil
}

// The first line reads "Dnsmasq version 2.91  Copyright ...".
func parseDnsmasqVersion(probe string) (string, bool) {
	fields := strings.Fields(probe)
	for i, f := range fields {
		if strings.EqualFold(f, "version") && i+1 < len(fields) {
			v := fields[i+1]
			if v == "" || !(v[0] >= '0' && v[0] <= '9') {
				return "", false
			}
			return v, true
		}
	}
	return "", false
}

// V6OnlyDHCPTokens is derived as the v6 table minus the v4 table, since every mode runs a v4 range too.
func V6OnlyDHCPTokens() []string {
	v4 := make(map[string]bool, len(dnsmasqV4MessageNames))
	for _, n := range dnsmasqV4MessageNames {
		v4[n] = true
	}
	out := make([]string, 0, len(dnsmasqV6MessageNames))
	for _, n := range dnsmasqV6MessageNames {
		if !v4[n] {
			out = append(out, n)
		}
	}
	return out
}

// DnsmasqAmbiguousDHCPTokens is the tokens both dnsmasq paths print.
func DnsmasqAmbiguousDHCPTokens() []string {
	v4 := make(map[string]bool, len(dnsmasqV4MessageNames))
	for _, n := range dnsmasqV4MessageNames {
		v4[n] = true
	}
	out := make([]string, 0, len(dnsmasqV6MessageNames))
	for _, n := range dnsmasqV6MessageNames {
		if v4[n] {
			out = append(out, n)
		}
	}
	return out
}

// Measured 2026-09-05, dnsmasq 2.91; the logs are pinned in v6signature_test.go (#911). Stateless logs no DHCPREPLY:
// log6_quiet is called once at rfc3315.c:1144. "ignored" is gettext (rfc3315.c:652, po/de.po:2083 "ignoriert") and
// needs withCLocale's LC_ALL=C.
type v6ExchangeRule struct {
	must    []string
	mustNot []string
	// mustLine is substrings that must appear on one line together.
	mustLine []string
}

var v6ExchangeContract = map[V6Mode]v6ExchangeRule{
	V6Managed: {
		must: []string{"DHCPSOLICIT", "DHCPADVERTISE", "DHCPREPLY"},
	},
	V6Stateless: {
		must:    []string{"DHCPINFORMATION-REQUEST"},
		mustNot: []string{"DHCPADVERTISE"},
	},
	V6SLAAC: {
		mustNot: V6OnlyDHCPTokens(),
	},
	V6NoRA: {
		must:    []string{"DHCPSOLICIT"},
		mustNot: []string{"DHCPADVERTISE", "DHCPREPLY"},
	},
	V6ManagedSilent: {
		mustLine: []string{"DHCPSOLICIT", "ignored"},
		mustNot:  []string{"DHCPADVERTISE", "DHCPREPLY"},
	},
	V6ManagedExhausted: {
		// "no addresses available" is gettext (rfc3315.c:809), "keine Adressen verfügbar" on the runner's locale (measured
		// 2026-09-16, #816); the message types are locale-proof.
		must:    []string{"DHCPSOLICIT", "DHCPADVERTISE"},
		mustNot: []string{"DHCPREPLY"},
	},
	V6AutoFallback: {
		// Managed-silent's pair; the two modes differ on the wire, not in the log (#818).
		mustLine: []string{"DHCPSOLICIT", "ignored"},
		mustNot:  []string{"DHCPADVERTISE", "DHCPREPLY"},
	},
}

// V6ContractFindings reports how a contract breaks the dual-stack rules; empty means it agrees.
//
// mustNot needs every DHCP token v6-only. mustLine needs at least one, since dnsmasq's v4 path logs
// `log_packet("DHCPRELEASE", ..., message)` (rfc2131.c:1096) with message = _("ignored") (rfc2131.c:1105) (#911).
func V6ContractFindings(contract map[V6Mode]v6ExchangeRule) []string {
	v6only := make(map[string]bool)
	for _, n := range V6OnlyDHCPTokens() {
		v6only[n] = true
	}
	var out []string
	for _, mode := range V6Modes() {
		c, ok := contract[mode]
		if !ok {
			out = append(out, fmt.Sprintf(
				"mode %s has no exchange contract, so AssertExchange cannot say anything about it", mode))
			continue
		}
		for _, tok := range c.mustNot {
			if !strings.HasPrefix(tok, "DHCP") {
				continue
			}
			if !v6only[tok] {
				out = append(out, fmt.Sprintf(
					"mode %s forbids %q, which dnsmasq's v4 path prints too (rfc2131.c); every mode "+
						"of this fixture runs a v4 range, so the v4 half alone can fail this mode "+
						"for something its v6 half never did", mode, tok))
			}
		}
		if len(c.mustLine) > 0 && !anyV6Only(c.mustLine, v6only) {
			out = append(out, fmt.Sprintf(
				"mode %s requires the line %v, in which no token is v6-only; dnsmasq's v4 path "+
					"writes a line of this shape (rfc2131.c:1096 with the message at :1105), so "+
					"the v4 half alone satisfies this mode on a segment whose v6 half never spoke",
				mode, c.mustLine))
		}
	}
	return out
}

func anyV6Only(tokens []string, v6only map[string]bool) bool {
	for _, tok := range tokens {
		if v6only[tok] {
			return true
		}
	}
	return false
}

// V6ExchangeFindings reports how the server log disagrees with the mode's exchange; empty means it agrees.
//
// V6SLAAC has only a must-NOT set, so an empty log yields no findings in that mode (#911).
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
