package proto

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/claymore666/dhcp-golib/wire"
)

// Addr6 is one address of an IA_NA with the two lifetimes §21.6 gives it.
//
// The lifetimes are Durations relative to Lease6.Start and not Instants,
// because that is what the wire carries and what a Renew must send back:
// §18.2.4 has the client include "an IA Address option for each address
// assigned to the IA", and §21.4 says the T1/T2 values "are the number of
// seconds until T1 and T2 and are calculated since reception of the message".
type Addr6 struct {
	Addr      netip.Addr
	Preferred Duration
	Valid     Duration
}

func (a Addr6) String() string {
	return fmt.Sprintf("%s pref=%s valid=%s", a.Addr, a.Preferred, a.Valid)
}

// Lease6 is what Machine6 derived from a Reply.
//
// It is Lease's counterpart and it holds a SET of addresses where Lease holds
// one prefix, because an IA_NA is a set. Every deadline is derived from Start,
// which is an Instant on the monotonic clock, for the reason Lease says: ring
// 1 has no other clock, and turning these into a persistable wall-clock expiry
// is ring 2's job.
type Lease6 struct {
	// IAID is the identity association this lease belongs to: the value the
	// client sent, echoed by the server.
	IAID uint32

	// Addrs is the addresses the server assigned, in the order they appeared
	// in the IA_NA. Addresses with a valid lifetime of 0 are NOT here:
	// §18.2.10.1 says the client "Discard[s] any leases from the IA, as
	// recorded by the client, that have a valid lifetime of 0 in the IA
	// Address or IA Prefix option."
	Addrs []Addr6

	// ServerDUID is the Server Identifier option's contents, as sent. It is
	// the v6 counterpart of Lease.ServerID and the reason lease.Lease carries
	// both: a v6 server is named by opaque bytes, not by an address.
	ServerDUID []byte

	// T1 and T2 as the server supplied them, zero when it supplied none.
	// They are NOT defaulted here — see Deadlines, which applies §21.4's
	// recommendation where it belongs, at the point of use.
	T1, T2 Duration

	// Start is the Instant the message that produced this lease was SENT —
	// the FIRST transmission of the exchange, the same instant §21.9's
	// Elapsed Time counts from, not the last retransmission.
	//
	// THIS IS A DELIBERATE DEVIATION, and it is stated here rather than
	// discovered later. RFC 9915 measures from the other end, twice and
	// unambiguously: §4.2 defines T1 as "interpreted as a time interval since
	// the message's reception", and §18.2.10.1 tells the client to "Calculate
	// T1 and T2 times (based on T1 and T2 values sent in the message and the
	// message reception time)".
	//
	// WHAT IT COSTS, stated as a bound rather than as a reassurance: the
	// client's timers fire EARLY by the whole elapsed time of the exchange —
	// from the first Request transmission to the Reply's arrival — which is
	// one round trip when nothing is lost and as much as the §15
	// retransmission ran when something was. It is never late, so the client
	// never renews after the server considers T1 passed; it renews sooner
	// than it had to, and a §15 exchange that ran long makes it renew sooner
	// still. Both directions of that trade are visible in
	// TestTheGoldenPathIsDrivenByDnsmasqsOwnBytes, which pins the send origin
	// with an exchange that took two seconds.
	//
	// WHY DEVIATE AT ALL: D30 — v6 takes v4's shape. RFC 2131 §4.4.5 makes
	// the send instant normative for v4 ("the client computes the lease
	// expiration time as the sum of the time at which the client sent the
	// DHCPREQUEST message and the duration of the lease in the DHCPACK
	// message"), Lease.Start records it, and Deadlines is one helper shared
	// by both families. Two origins behind one field would make that helper
	// mean two things depending on who filled it in.
	Start Instant

	// DNS and Search are RFC 3646's two lists.
	DNS    []netip.Addr
	Search []string

	// Options is every top-level option of the Reply, unparsed, for the
	// reason Lease.Options exists: a forgotten option is recoverable rather
	// than gone.
	Options wire.OptionsV6
}

// Addr is the first address of the IA, which is the one a single-address
// caller wants, and whether there is one.
func (l Lease6) Addr() (netip.Addr, bool) {
	if len(l.Addrs) == 0 {
		return netip.Addr{}, false
	}
	return l.Addrs[0].Addr, true
}

// Prefix is the first address as a /128.
//
// §18.2.10.1 forbids any other length: "Addresses obtained from an IA Address
// option MUST NOT be used to form an implicit prefix with a length other than
// 128." The constant is written here once so no caller has to remember it,
// and a caller that wants the on-link prefix must get it from a Router
// Advertisement, which is the whole point of that sentence.
func (l Lease6) Prefix() (netip.Prefix, bool) {
	a, ok := l.Addr()
	if !ok {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, 128), true
}

// shortestPreferred and longestValid are the two aggregates §21.4 and §18.2.5
// name. Infinite absorbs: an IA with one infinite lifetime does not expire.
func (l Lease6) shortestPreferred() Duration {
	out := Duration(0)
	first := true
	for _, a := range l.Addrs {
		if a.Preferred.IsInfinite() {
			continue
		}
		if first || a.Preferred < out {
			out, first = a.Preferred, false
		}
	}
	if first {
		if len(l.Addrs) == 0 {
			return 0
		}
		return Infinite
	}
	return out
}

func (l Lease6) longestValid() Duration {
	out := Duration(0)
	for _, a := range l.Addrs {
		if a.Valid.IsInfinite() {
			return Infinite
		}
		if a.Valid > out {
			out = a.Valid
		}
	}
	return out
}

// PreferredUntil is when the shortest preferred lifetime in the IA runs out,
// and reports false for an infinite one or an empty IA.
//
// It is a FOURTH moment beside Deadlines' three, and it is separate because
// nothing in ring 1 arms a timer for it: RFC 9915 §7.1's preferred lifetime is
// "the length of time that a valid address is preferred", after which RFC 4862
// §5.5.4 makes the address deprecated — "A deprecated address SHOULD continue
// to be used as a source address in existing communications, but SHOULD NOT be
// used to initiate new communications if an alternate (non-deprecated) address
// of sufficient scope can easily be used instead" — which is a rule for
// whoever opens sockets
// and not for the DHCP client. Ring 2 reports it; ring 3 acts on it.
func (l Lease6) PreferredUntil() (Instant, bool) {
	if len(l.Addrs) == 0 {
		return 0, false
	}
	p := l.shortestPreferred()
	if p.IsInfinite() || p <= 0 {
		return 0, false
	}
	return l.Start.Add(p), true
}

// Deadlines are the three moments Machine6 arms timers for, in the same shape
// the v4 machine uses (D30) and through the same type.
//
// §21.4's recommendation is applied HERE and not at decode, for the reason
// Lease.Deadlines applies RFC 2131's 0.5/0.875 here: one derivation, so a
// caller reading the three cannot get an answer the machine's timers disagree
// with.
//
// THE 0.5/0.8 FIGURES ARE A RECOMMENDATION TO THE SERVER, NOT A CLIENT
// DEFAULT, and the difference is stated because a brief citation is a claim.
// §21.4: "The server selects the T1 and T2 values to allow the client to
// extend the lifetimes of any addresses in the IA_NA before the lifetimes
// expire, even if the server is unavailable for some short period of time.
// Recommended values for T1 and T2 are 0.5 and 0.8 times the shortest
// preferred lifetime of the addresses in the IA that the server is willing to
// extend, respectively." What the CLIENT is obliged to do when the server
// sends zero is §14.2: "When T1 and/or T2 values are set to 0, the client MUST
// choose a time to avoid message storms. In particular, it MUST NOT transmit
// immediately." So this client chooses the server's own recommended fractions
// as its choice — a value the server would have considered reasonable, and one
// that is not immediate — and says here that it is a choice.
func (l Lease6) Deadlines() Deadlines {
	var d Deadlines
	valid := l.longestValid()
	if !valid.IsInfinite() {
		d.Expire, d.HasExpire = l.Start.Add(valid), true
	}
	if len(l.Addrs) == 0 || (!valid.IsInfinite() && valid <= 0) {
		return d
	}

	pref := l.shortestPreferred()
	renew, rebind := l.T1, l.T2
	if renew <= 0 || rebind <= 0 {
		// §21.4: "If the 'shortest' preferred lifetime is 0xffffffff
		// ('infinity'), the recommended T1 and T2 values are also
		// 0xffffffff." An infinite preferred lifetime therefore produces no
		// renewal timer at all rather than a huge one.
		if pref.IsInfinite() {
			if renew <= 0 {
				renew = Infinite
			}
			if rebind <= 0 {
				rebind = Infinite
			}
		} else {
			if renew <= 0 {
				renew = pref / 2
			}
			if rebind <= 0 {
				rebind = (pref / 5) * 4
			}
		}
	}

	// §18.2.5 puts the Rebind exchange's end at the expiry: "The message
	// exchange is terminated when the valid lifetimes of all leases across all
	// IAs have expired". A T2 at or after that is a T2 the machine could never
	// act on, so it is clamped to the recommendation and the clamp is
	// journalled — the same trade Lease.Deadlines makes, for the same reason:
	// refusing the Reply hands back a working lease over a server's typo.
	if !rebind.IsInfinite() && !valid.IsInfinite() && rebind >= valid {
		was := rebind
		if pref.IsInfinite() {
			rebind = valid
		} else {
			rebind = (pref / 5) * 4
		}
		d.Note = "T2 (" + was.String() + ") is not earlier than the longest valid lifetime (" +
			valid.String() + "): using §21.4's 0.8 of the shortest preferred lifetime, " + rebind.String()
	}
	if !renew.IsInfinite() && !rebind.IsInfinite() && renew >= rebind {
		// §21.4 makes this the SERVER's error and names the remedy for a
		// well-formed one at option level: "If a client receives an IA_NA with
		// T1 greater than T2 and both T1 and T2 are greater than 0, the client
		// discards the IA_NA option". That discard happens in
		// leaseFromReply, where the option is; by the time a lease exists the
		// two values came from different places (one from the server, one from
		// the fallback above) and half of T2 is the answer that keeps the
		// ordering without inventing a renewal after the rebind.
		was := renew
		renew = rebind / 2
		note := "T1 (" + was.String() + ") is not earlier than T2 (" + rebind.String() +
			"): using half of T2, " + renew.String()
		if d.Note == "" {
			d.Note = note
		} else {
			d.Note += "; " + note
		}
	}

	if renew > 0 {
		d.Renew, d.HasRenew = l.Start.Add(renew), true
	}
	if rebind > 0 {
		d.Rebind, d.HasRebind = l.Start.Add(rebind), true
	}
	return d
}

// Equal reports whether two leases would configure the interface identically.
// Start is NOT compared: a renewal that changes nothing but the expiry is
// ActLeaseRenewed and not ActLeaseChanged, which is the distinction this
// predicate exists to draw.
func (l Lease6) Equal(o Lease6) bool {
	if l.IAID != o.IAID || len(l.Addrs) != len(o.Addrs) {
		return false
	}
	for i := range l.Addrs {
		if l.Addrs[i] != o.Addrs[i] {
			return false
		}
	}
	if !sameDUID(l.ServerDUID, o.ServerDUID) {
		return false
	}
	return l.T1 == o.T1 && l.T2 == o.T2 &&
		addrsEqual(l.DNS, o.DNS) && stringsEqual(l.Search, o.Search)
}

// String is the diagnostic rendering. It builds through fmt.Sprintf and
// strings.Builder rather than fmt.Fprintf, because gate T1 refuses the
// Fprint family in a pure ring: Fprintf takes an io.Writer, and a ring-1
// file that can name it can hold a stream. MEASURED 2026-09-06: the first
// draft of this function used fmt.Fprintf(&b, ...) and ./verify.sh's t1 row
// named both lines. strings.Builder is not a stream the gate can see, but
// the gate keys on the identifier, not on what it was handed, and that is
// the right direction for a gate to fail in.
func (l Lease6) String() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("iaid=%d", l.IAID))
	for _, a := range l.Addrs {
		b.WriteString(" ")
		b.WriteString(a.String())
	}
	b.WriteString(fmt.Sprintf(" t1=%s t2=%s", l.T1, l.T2))
	if len(l.DNS) > 0 {
		b.WriteString(" dns=" + addrsText(l.DNS))
	}
	if len(l.Search) > 0 {
		b.WriteString(" search=" + strings.Join(l.Search, ","))
	}
	return b.String()
}

// iaResult is what one IA_NA in a Reply produced, and why, so every discard
// arm has a journal line rather than a silence.
type iaResult struct {
	addrs []Addr6
	t1    Duration
	t2    Duration
	// status is the Status Code scoped to this IA, or Success when there was
	// none. §21.4: "The status of any operations involving this IA_NA is
	// indicated in a Status Code option".
	status wire.StatusCode
	found  bool
}

// leaseFromReply builds a Lease6 from a Reply's IA_NA for iaid.
//
// It returns the notes for the journal and whether a usable lease came out.
// EVERY REFUSAL IS A NOTE: sequencing §2.6's rule is that a discard is
// invisible in a passing test, so a message that yields nothing must say which
// of the six reasons applied.
func leaseFromReply(m *wire.MessageV6, iaid uint32, sentAt Instant) (Lease6, []string, wire.StatusCode, bool) {
	var notes []string
	l := Lease6{IAID: iaid, Start: sentAt, Options: append(wire.OptionsV6(nil), m.Options...)}
	if sid, ok := m.Options.First(wire.OptV6ServerID); ok {
		l.ServerDUID = append([]byte(nil), sid...)
	}

	// §21.6's option is "only specified to be encapsulated within an IA_NA",
	// so one at the top level is a misplaced option. §16 forbids discarding
	// the message over it — "Clients and servers MAY choose to either (1)
	// extract information from such a message if the information is of use to
	// the recipient or (2) ignore such a message completely and just discard
	// it" — and an address with no IAID is one nothing can renew, rebind or
	// release, so the OPTION is dropped and the drop is named.
	if n := m.Options.Count(wire.OptV6IAAddr); n > 0 {
		notes = append(notes, fmt.Sprintf("%d IA Address option(s) at the top level, outside any IA_NA: ignored, §21.6 encapsulates them within IA_NA", n))
	}

	res, ns := readIA(m.Options, iaid)
	notes = append(notes, ns...)
	if !res.found {
		return Lease6{}, notes, wire.StatusSuccess, false
	}
	l.T1, l.T2, l.Addrs = res.t1, res.t2, res.addrs
	if dns, err := m.Options.DNSServers(); err != nil {
		notes = append(notes, "DNS Recursive Name Server option: "+err.Error())
	} else {
		l.DNS = dns
	}
	if s, err := m.Options.DomainSearch(); err != nil {
		notes = append(notes, "Domain Search List option: "+err.Error())
	} else {
		l.Search = s
	}
	return l, notes, res.status, len(l.Addrs) > 0
}

// readIA finds the IA_NA whose IAID is ours and reads it.
func readIA(o wire.OptionsV6, iaid uint32) (iaResult, []string) {
	var notes []string
	var out iaResult
	ias, err := o.IANAs()
	if err != nil {
		return out, append(notes, "IA_NA option: "+err.Error())
	}
	for _, ia := range ias {
		if ia.IAID != iaid {
			// §18.2.10 has the client update "the information it has recorded
			// about IAs from the IA options contained in the Reply message" —
			// its own IAs. An IA the client never asked for belongs to some
			// other identity association, so it is skipped rather than
			// treated as an error: the rest of the message is still ours.
			notes = append(notes, fmt.Sprintf("IA_NA with IAID %d is not ours (%d): ignored", ia.IAID, iaid))
			continue
		}
		if out.found {
			notes = append(notes, fmt.Sprintf("a second IA_NA with IAID %d: ignored, this client sends one", iaid))
			continue
		}
		t1, t2 := SecondsToDuration(ia.T1), SecondsToDuration(ia.T2)
		// §21.4, verbatim: "If a client receives an IA_NA with T1 greater than
		// T2 and both T1 and T2 are greater than 0, the client discards the
		// IA_NA option and processes the remainder of the message as though
		// the server had not included the invalid IA_NA option."
		// The test is on the WIRE values and not on the Durations, because
		// SecondsToDuration maps 0xffffffff to the Infinite sentinel, which is
		// negative: an infinite T1 beside a finite T2 would compare as
		// less-than and slip past the rule the RFC states over the encoded
		// numbers.
		if ia.T1 > 0 && ia.T2 > 0 && ia.T1 > ia.T2 {
			notes = append(notes, fmt.Sprintf("IA_NA T1 %s is greater than T2 %s, both non-zero: IA_NA discarded (§21.4)", t1, t2))
			continue
		}
		out.found, out.t1, out.t2 = true, t1, t2
		st, ok, err := ia.Options.Status()
		switch {
		case err != nil:
			// The malformed Status Code value is checked BEFORE the value is
			// read, which is what wire.StatusMalformed exists to make
			// unnecessary to remember. Both are done: the error decides, the
			// sentinel makes forgetting it visible instead of silent.
			notes = append(notes, "IA_NA Status Code option: "+err.Error())
			out.status = wire.StatusMalformed
		case ok:
			out.status = st.Code
		default:
			out.status = wire.StatusSuccess
		}
		addrs, err := ia.Options.Addrs()
		if err != nil {
			notes = append(notes, "IA Address option: "+err.Error())
			continue
		}
		for _, a := range addrs {
			// §21.6: "The client MUST discard any addresses for which the
			// preferred lifetime is greater than the valid lifetime."
			// wire.IAAddr.Valid is the predicate; the discard is here,
			// because ring 0 holds no policy and an address dropped by the
			// decoder could not be counted or journalled.
			if !a.Valid() {
				notes = append(notes, fmt.Sprintf("IA Address %s has preferred %d greater than valid %d: discarded (§21.6)",
					a.Addr, a.PreferredLifetime, a.ValidLifetime))
				continue
			}
			if a.ValidLifetime == 0 {
				notes = append(notes, fmt.Sprintf("IA Address %s has a valid lifetime of 0: discarded (§18.2.10.1)", a.Addr))
				continue
			}
			if !a.Addr.Is6() || a.Addr.Is4In6() || a.Addr.IsUnspecified() {
				notes = append(notes, fmt.Sprintf("IA Address %s is not a usable IPv6 address: discarded", a.Addr))
				continue
			}
			out.addrs = append(out.addrs, Addr6{
				Addr:      a.Addr,
				Preferred: SecondsToDuration(a.PreferredLifetime),
				Valid:     SecondsToDuration(a.ValidLifetime),
			})
		}
	}
	if !out.found && len(ias) > 0 {
		notes = append(notes, fmt.Sprintf("no IA_NA with our IAID %d in the message", iaid))
	}
	return out, notes
}
