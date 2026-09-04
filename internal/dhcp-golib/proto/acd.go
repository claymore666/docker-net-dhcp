package proto

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// ConflictMode is how this client checks that the address it is given is not
// already in use. D23, taken by the maintainer 2026-09-04.
//
// It is a typed enum whose ZERO VALUE IS THE SAFE ONE, so a caller that does
// not know this knob exists gets RFC 5227 section 2.1's "MUST test to see if
// the address is already in use" before the address is used, which is D22.
type ConflictMode uint8

// The three modes.
const (
	// ConflictWait probes before use: the lease is not announced and the
	// address is not usable until RFC 5227 section 2.1's probing has completed
	// with no conflict. D22.
	//
	// The cost is section 1.1's timing constants, paid at every acquisition:
	// best 4s, mean 5.5s, worst 7s from the DHCPACK to the announcement. It
	// is not this implementation being slow — see ACDParams.
	ConflictWait ConflictMode = iota

	// ConflictAsync announces the lease and lets the address be used at once,
	// and runs section 2.1's probing beside it. A conflict found in the probe
	// window is handled exactly as a conflict found later: DHCPDECLINE, the
	// lease reported lost with ReasonConflict, and the configuration process
	// restarted.
	//
	// It trades a guarantee for latency, and the trade is stated rather than
	// implied: the caller may be told to configure an address that this client
	// discovers, seconds later, belongs to someone else. RFC 5227 section 2.4
	// permits detecting a conflict after the fact — "Address Conflict
	// Detection is an ongoing process that is in effect for as long as a host
	// is using an address" — but section 2.1's pre-use check is a MUST for a
	// host implementing that specification, so a client in this mode is
	// conformant to RFC 2131 and not to RFC 5227 section 2.1.
	ConflictAsync

	// ConflictOff runs no probe and no listener. ReportConflict still works,
	// so a caller with its own detector keeps the DHCPDECLINE path.
	//
	// PERMITTED, and by which text. RFC 2131 section 3.1(5) makes the pre-use
	// check a SHOULD — "The client SHOULD perform a final check on the
	// parameters (e.g., ARP for allocated network address)" — and section
	// 4.4.1 repeats it as a SHOULD. The MUST in both sections is CONDITIONAL
	// on detection: "If the client detects that the address is already in use
	// ..., the client MUST send a DHCPDECLINE message". A client that does not
	// look detects nothing and owes nothing, and RFC 5227 section 1.3 says the
	// same for its own protocol: "For the protocol specified in this document
	// to be effective, it is not necessary that all hosts on the link
	// implement it."
	//
	// What it costs is stated where an operator can read it: two hosts on one
	// address, undetected, for the life of the lease.
	ConflictOff
)

func (c ConflictMode) String() string {
	switch c {
	case ConflictWait:
		return "wait"
	case ConflictAsync:
		return "async"
	case ConflictOff:
		return "off"
	default:
		return fmt.Sprintf("conflict-mode(%d)", uint8(c))
	}
}

// AllConflictModes is every ConflictMode. See AllStates for why this exists.
func AllConflictModes() []ConflictMode {
	return []ConflictMode{ConflictWait, ConflictAsync, ConflictOff}
}

// ACDParams is RFC 5227 section 1.1's timing constant table, complete.
//
// THE VALUES ARE NOT A PREFERENCE. Section 1.1: "Note that the values listed
// here are fixed constants; they are not intended to be modifiable by
// implementers, operators, or end users. These constants are given symbolic
// names here to facilitate the writing of future standards that may want to
// reference this document with different values for these named constants;
// however, at the present time no such future standards exist."
//
// They are fields rather than package constants for one reason, and it is the
// same one Params.DesyncMin and Params.RestartDelay are fields for: a test
// whose subject is the ORDER of the probes, or the DHCPDECLINE that follows
// one, cannot spend five seconds per case measuring section 1.1 instead.
// DefaultACDParams is the RFC table and DefaultParams installs it;
// TestACDConstantsAreTheRFCValues pins every one of the nine.
//
// The complete table is transcribed even though this client never acts on
// DefendInterval, because a partial table invites the missing row to be added
// later with a value nobody checked.
type ACDParams struct {
	// ProbeWait is the maximum of the initial random delay before the first
	// probe: "the host should then wait for a random time interval selected
	// uniformly in the range zero to PROBE_WAIT seconds" (section 2.1.1).
	ProbeWait Duration
	// ProbeNum is how many ARP Probes are sent.
	ProbeNum int
	// ProbeMin and ProbeMax bound the gap between probes: "each of these probe
	// packets spaced randomly and uniformly, PROBE_MIN to PROBE_MAX seconds
	// apart" (section 2.1.1).
	ProbeMin Duration
	ProbeMax Duration
	// AnnounceWait is the quiet period after the last probe that ENDS the
	// probing phase: "If, by ANNOUNCE_WAIT seconds after the transmission of
	// the last ARP Probe no conflicting ARP Reply or ARP Probe has been
	// received, then the host has successfully determined that the desired
	// address may be used safely" (section 2.1.1).
	AnnounceWait Duration
	// AnnounceNum and AnnounceInterval are section 2.3's announcements:
	// "broadcasting ANNOUNCE_NUM ARP Announcements, spaced ANNOUNCE_INTERVAL
	// seconds apart".
	AnnounceNum      int
	AnnounceInterval Duration
	// MaxConflicts and RateLimitInterval are section 2.1.1's rate limit: "if
	// the host experiences MAX_CONFLICTS or more address conflicts on a given
	// interface, then the host MUST limit the rate at which it probes for new
	// addresses on this interface to no more than one attempted new address
	// per RATE_LIMIT_INTERVAL."
	//
	// D5, taken by the maintainer 2026-09-04: PER ENDPOINT. "A given
	// interface" is ambiguous on a macvlan parent shared by many endpoints,
	// and the RFC does not disambiguate for that topology. The count lives on
	// the Machine, and one Machine is one endpoint, so the per-parent reading
	// is not merely unimplemented — there is no state anywhere in this library
	// that two clients share.
	MaxConflicts      int
	RateLimitInterval Duration
	// DefendInterval is "the minimum interval between defensive ARPs"
	// (section 1.1), used by section 2.4's arms (b) and (c).
	//
	// THIS CLIENT NEVER DEFENDS, so nothing reads this field. Section 2.4 (a)
	// is the arm taken — "a host MAY elect to immediately cease using the
	// address, and signal an error to the configuring agent" — because section
	// 2.4 names this device class for it ("For most client machines that do
	// not need a fixed IP address, immediately requesting the configuring
	// agent ... to configure a new address as soon as the conflict is detected
	// is the best way to restore useful communication as quickly as
	// possible"), and because RFC 2131 section 3.1(5) makes the DHCPDECLINE a
	// MUST anyway: defending an address we are simultaneously telling the
	// server is unusable is a contradiction, not a policy.
	//
	// The behaviour is driven by its absence, in
	// TestArmAIsTakenSoTheAddressIsNeverDefended.
	DefendInterval Duration
}

// DefaultACDParams is RFC 5227 section 1.1's table, verbatim:
//
//	PROBE_WAIT           1 second   (initial random delay)
//	PROBE_NUM            3          (number of probe packets)
//	PROBE_MIN            1 second   (minimum delay until repeated probe)
//	PROBE_MAX            2 seconds  (maximum delay until repeated probe)
//	ANNOUNCE_WAIT        2 seconds  (delay before announcing)
//	ANNOUNCE_NUM         2          (number of Announcement packets)
//	ANNOUNCE_INTERVAL    2 seconds  (time between Announcement packets)
//	MAX_CONFLICTS       10          (max conflicts before rate-limiting)
//	RATE_LIMIT_INTERVAL 60 seconds  (delay between successive attempts)
//	DEFEND_INTERVAL     10 seconds  (minimum interval between defensive ARPs)
func DefaultACDParams() ACDParams {
	return ACDParams{
		ProbeWait:         1 * Second,
		ProbeNum:          3,
		ProbeMin:          1 * Second,
		ProbeMax:          2 * Second,
		AnnounceWait:      2 * Second,
		AnnounceNum:       2,
		AnnounceInterval:  2 * Second,
		MaxConflicts:      10,
		RateLimitInterval: 60 * Second,
		DefendInterval:    10 * Second,
	}
}

// ACDPhase is where the conflict-detection sub-machine is.
//
// It is REPORTED OUTWARD — on every lease event and into the durable record —
// because in ConflictAsync the caller is using an address whose probing has
// not finished, and "has this address been checked yet" is then a question
// with a real answer that only this machine holds.
type ACDPhase uint8

// The phases.
const (
	// ACDIdle is not running: no address is being probed or defended.
	ACDIdle ACDPhase = iota
	// ACDProbing covers section 2.1.1's initial random delay AND the
	// PROBE_NUM probes. They are one phase because they are one schedule —
	// arm a timer, send a probe when it fires — and because section 2.1.1's
	// conflict window is defined over the whole of it: "from the beginning of
	// the probing process until ANNOUNCE_WAIT seconds after the last probe
	// packet is sent".
	ACDProbing
	// ACDSettling is the ANNOUNCE_WAIT after the last probe. The conflict
	// rules of ACDProbing still apply here; that is the difference between
	// this phase and ACDAnnouncing, and it is why the phase exists rather
	// than a probe counter alone.
	ACDSettling
	// ACDAnnouncing is section 2.3: the address has been determined safe, the
	// first ARP Announcement has gone out, and the remaining ones are still
	// to come. Section 2.4's ongoing rule is what applies from here on.
	ACDAnnouncing
	// ACDDefending is the rest of the lease's life: every announcement sent,
	// section 2.4's listener live. The name is section 2.4's ("Ongoing
	// Address Conflict Detection and Address Defense") and not a description
	// of what this client does on a conflict, which is arm (a): cease.
	ACDDefending
)

func (p ACDPhase) String() string {
	switch p {
	case ACDIdle:
		return "idle"
	case ACDProbing:
		return "probing"
	case ACDSettling:
		return "settling"
	case ACDAnnouncing:
		return "announcing"
	case ACDDefending:
		return "defending"
	default:
		return fmt.Sprintf("acd-phase(%d)", uint8(p))
	}
}

// AllACDPhases is every ACDPhase. See AllStates for why this exists.
func AllACDPhases() []ACDPhase {
	return []ACDPhase{ACDIdle, ACDProbing, ACDSettling, ACDAnnouncing, ACDDefending}
}

// acd is the conflict-detection sub-machine: pure, and separate from Machine
// on purpose.
//
// It owns the TIMING and the VERDICT and nothing else. It does not know what
// a DHCP lease is, it never builds a message, and it cannot decide to send a
// DHCPDECLINE — Machine does that, from the verdict this returns. The split is
// what lets the whole of RFC 5227's arithmetic be tabled against a swept rnd
// with no DHCP exchange anywhere near it.
//
// Its own step is acdStep, and it has the same shape as Machine.Step for the
// same reason: (now, rnd, event) in, (phase, actions) out, no clock, no
// entropy source, no I/O.
type acd struct {
	params ACDParams
	mode   ConflictMode

	phase ACDPhase

	// addr is the address being probed or defended: section 2.4's "(one of)
	// the host's own IP address(es) configured on that interface", narrowed
	// to the one this client owns. See the bound in the handover.
	addr netip.Addr

	// hw is this client's hardware address: section 2.1.1's "the hardware
	// address of any of the host's interfaces", narrowed the same way.
	hw []byte

	// sent counts probes in ACDProbing and announcements in ACDAnnouncing.
	sent int

	// attemptAt is when the current or most recent probing attempt began.
	// Section 2.1.1's rate limit is a RATE, so it needs the clock: "no more
	// than one attempted new address per RATE_LIMIT_INTERVAL".
	attemptAt Instant

	// conflicts is section 2.1.1's per-interface conflict count, which D5
	// makes per endpoint. It is NOT reset by a successful acquisition:
	// section 2.1.1 says "if the host experiences MAX_CONFLICTS or more
	// address conflicts", with no clause that forgets them, and the failure it
	// guards against — "a defective DHCP server that repeatedly assigns the
	// same address to every host that asks for one" — is precisely the one
	// where each attempt looks like a fresh start.
	conflicts int
}

// acdEventKind is what happened to the sub-machine.
type acdEventKind uint8

const (
	// acdEvStart begins section 2.1's probing for an address.
	acdEvStart acdEventKind = iota
	// acdEvTimer is the one ACD timer firing. There is one because the phases
	// are sequential: a second timer could only ever be armed by a phase that
	// had already armed the first.
	acdEvTimer
	// acdEvARP carries one received ARP packet.
	acdEvARP
	// acdEvStop returns the sub-machine to ACDIdle.
	acdEvStop
)

// acdEvent is one input to acdStep.
type acdEvent struct {
	kind acdEventKind
	// addr is set on acdEvStart: the address to probe.
	addr netip.Addr
	// pkt is set on acdEvARP.
	pkt *wire.ARPPacket
}

// acdActionKind is what the sub-machine asks Machine to do.
type acdActionKind uint8

const (
	// acdSendProbe broadcasts one RFC 5227 section 2.1.1 ARP Probe.
	acdSendProbe acdActionKind = iota
	// acdSendAnnounce broadcasts one section 2.3 ARP Announcement.
	acdSendAnnounce
	// acdSetTimer arms the one ACD timer.
	acdSetTimer
	// acdCancelTimer disarms it, at the end of the schedule.
	//
	// EMITTED RATHER THAN LEFT TO THE CALLER. Section 2.3's last
	// Announcement is the end of everything this sub-machine schedules;
	// section 2.4's ongoing detection is a listener and has no timer at all
	// (this client never defends, so DEFEND_INTERVAL schedules nothing). A
	// timer left armed there would fire minutes later in a phase with no arm
	// for it, and the only trace would be a journal line saying it was
	// ignored.
	acdCancelTimer
	// acdReady says section 2.1's probing completed with no conflict, so the
	// address may be used safely. In ConflictWait it is what releases the
	// lease to the caller.
	acdReady
	// acdConflict says a conflicting ARP packet was seen. Note carries the
	// rule and the evidence.
	acdConflict
	// acdNote is a journal line and nothing else.
	acdNote
)

// acdAction is one thing Machine must do with the sub-machine's output.
type acdAction struct {
	kind  acdActionKind
	after Duration
	note  string
}

// newACD builds the sub-machine for a client.
func newACD(p ACDParams, mode ConflictMode, hw []byte) *acd {
	return &acd{params: p, mode: mode, hw: append([]byte(nil), hw...)}
}

// running reports whether section 2.4's listener should be consulted at all.
func (a *acd) running() bool { return a.phase != ACDIdle }

// stop returns the sub-machine to ACDIdle, keeping the conflict count.
//
// The count survives on purpose: section 2.1.1's rate limit exists for the
// looping case, and a loop is precisely a sequence of acquisitions each of
// which stopped the previous probe.
func (a *acd) stop() {
	a.phase = ACDIdle
	a.addr = netip.Addr{}
	a.sent = 0
}

// acdStep is the sub-machine's whole surface.
//
// Total over (phase, event) by the same rule Machine.Step is total: every pair
// yields a defined result, because an ARP packet arriving in a phase that has
// no use for it is the ordinary case on a shared link, not an error.
func (a *acd) acdStep(now Instant, rnd uint64, ev acdEvent) (ACDPhase, []acdAction) {
	var out []acdAction
	switch ev.kind {
	case acdEvStop:
		a.stop()

	case acdEvStart:
		out = a.start(now, rnd, ev.addr, out)

	case acdEvTimer:
		out = a.timer(rnd, out)

	case acdEvARP:
		out = a.arp(ev.pkt, out)
	}
	return a.phase, out
}

// start begins section 2.1's check for addr.
//
// ConflictOff never gets here: Machine does not call it. ConflictAsync does,
// and enters the same probing phase — the difference between the two modes is
// what Machine does with acdReady, not what the sub-machine measures.
func (a *acd) start(now Instant, rnd uint64, addr netip.Addr, out []acdAction) []acdAction {
	a.addr = addr
	a.sent = 0
	a.attemptAt = now
	a.phase = ACDProbing
	if a.params.ProbeNum <= 0 {
		// A configuration with no probes still has to pass through the
		// ANNOUNCE_WAIT window, because section 2.1.1's conflict rules run
		// "from the beginning of the probing process" and a zero-probe
		// configuration still listens. It is not reachable from
		// DefaultACDParams and is defined rather than special-cased.
		a.phase = ACDSettling
		return append(out, acdAction{kind: acdSetTimer, after: a.params.AnnounceWait})
	}
	// Section 2.1.1: "the host should then wait for a random time interval
	// selected uniformly in the range zero to PROBE_WAIT seconds ... This
	// initial random delay helps ensure that a large number of hosts powered
	// on at the same time do not all send their initial probe packets
	// simultaneously."
	return append(out, acdAction{kind: acdSetTimer, after: uniform(0, a.params.ProbeWait, rnd)})
}

// timer advances the schedule.
func (a *acd) timer(rnd uint64, out []acdAction) []acdAction {
	switch a.phase {
	case ACDProbing:
		out = append(out, acdAction{kind: acdSendProbe})
		a.sent++
		if a.sent < a.params.ProbeNum {
			// Section 2.1.1: "each of these probe packets spaced randomly and
			// uniformly, PROBE_MIN to PROBE_MAX seconds apart."
			return append(out, acdAction{
				kind:  acdSetTimer,
				after: uniform(a.params.ProbeMin, a.params.ProbeMax, rnd),
			})
		}
		// The last probe is out. Section 2.1.1's window runs ANNOUNCE_WAIT
		// past it, and the phase changes because the phase is what says the
		// window is still open.
		a.phase = ACDSettling
		return append(out, acdAction{kind: acdSetTimer, after: a.params.AnnounceWait})

	case ACDSettling:
		// "If, by ANNOUNCE_WAIT seconds after the transmission of the last ARP
		// Probe no conflicting ARP Reply or ARP Probe has been received, then
		// the host has successfully determined that the desired address may be
		// used safely."
		//
		// THE ANNOUNCEMENT IS EMITTED BEFORE acdReady, and the order is the
		// subject of TestTheFirstAnnouncementPrecedesTheAcquisition. Section
		// 2.3: "The host may begin legitimately using the IP address
		// immediately after sending the first of the two ARP Announcements" —
		// AFTER SENDING. A caller drains this list in order and configures the
		// address on acdReady, so putting the announcement second would let it
		// use the address before the link had been told who holds it.
		a.phase = ACDAnnouncing
		a.sent = 0
		out = a.announce(out)
		return append(out, acdAction{kind: acdReady})

	case ACDAnnouncing:
		return a.announce(out)

	default:
		return append(out, acdAction{kind: acdNote,
			note: "ACD timer fired in phase " + a.phase.String() + ": ignored"})
	}
}

// announce sends one section 2.3 ARP Announcement and arms the next, or ends
// the announcement run.
//
// THE FIRST ANNOUNCEMENT IS WHAT RELEASES THE ADDRESS, not the last. Section
// 2.3: "The host may begin legitimately using the IP address immediately after
// sending the first of the two ARP Announcements; the sending of the second
// ARP Announcement may be completed asynchronously, concurrent with other
// networking operations the host may wish to perform." acdReady is therefore
// emitted beside the first one, and Machine binds on it.
func (a *acd) announce(out []acdAction) []acdAction {
	if a.params.AnnounceNum <= 0 {
		a.phase = ACDDefending
		return append(out, acdAction{kind: acdCancelTimer})
	}
	out = append(out, acdAction{kind: acdSendAnnounce})
	a.sent++
	if a.sent < a.params.AnnounceNum {
		return append(out, acdAction{kind: acdSetTimer, after: a.params.AnnounceInterval})
	}
	a.phase = ACDDefending
	return append(out, acdAction{kind: acdCancelTimer})
}

// arp applies RFC 5227's conflict rules to one received packet.
//
// THREE RULES, and which ones apply depends on the phase, which is the whole
// reason the phase is a value rather than a counter.
func (a *acd) arp(p *wire.ARPPacket, out []acdAction) []acdAction {
	if p == nil || !a.addr.IsValid() {
		return out
	}
	rule, why := a.conflictRule(p)
	if rule == "" {
		return out
	}
	a.conflicts++
	a.stop()
	return append(out, acdAction{kind: acdConflict, note: rule + ": " + why})
}

// conflictRule reports which of RFC 5227's rules this packet trips in the
// current phase, and the evidence, or "" for a packet that is not a conflict.
//
// It is a separate function from arp because it is a PREDICATE and is tabled
// as one, and because ARPRelevant has to be checkable against it: a relevance
// filter that could hide a packet this returns non-empty for would make the
// whole mechanism silently narrower. TestTheRelevanceFilterCannotHideAConflict
// runs one against the other.
func (a *acd) conflictRule(p *wire.ARPPacket) (rule, why string) {
	if p == nil {
		// arp() already guards, so this is unreachable through acdStep. It is
		// handled anyway and not left to a nil dereference, for the reason R1
		// gives about Step being total: a panic in ring 1 takes the whole
		// plugin down, and this predicate is reached from whatever anyone can
		// put on a wire.
		return "", ""
	}
	switch a.phase {
	case ACDProbing, ACDSettling:
		// Section 2.1.1, first rule: "If during this period, from the
		// beginning of the probing process until ANNOUNCE_WAIT seconds after
		// the last probe packet is sent, the host receives any ARP packet
		// (Request *or* Reply) on the interface where the probe is being
		// performed, where the packet's 'sender IP address' is the address
		// being probed for, then the host MUST treat this address as being in
		// use by some other host".
		//
		// THE SENDER-HARDWARE-ADDRESS EXEMPTION IS THIS LIBRARY'S, AND
		// SECTION 2.4 IS THE TEXT IT COMES FROM. Section 2.1.1's rule as
		// quoted has no such clause, and it does not need one IN THE MODEL
		// THAT DOCUMENT ASSUMES: there, a host is not using the address while
		// it probes, its Probes carry an all-zero sender IP by the MUST two
		// paragraphs up, and section 2.5's duty to answer ARP Requests for the
		// address starts only "from the time a host sends its first ARP
		// Announcement". Under those three facts no frame the host emits in
		// this window can carry the address as its sender IP, so the clause
		// would have nothing to exempt.
		//
		// D23 BREAKS THAT PREMISE ON PURPOSE, and so does the RFC 2131 path
		// that has nothing to do with D23. In ConflictAsync the caller is told
		// Acquired at the DHCPACK and configures the address while this window
		// is open; on a renewal that MOVES the address the caller has held the
		// old one for hours and cannot un-hold it. In both, the address is in
		// use, and section 2.4 is the text that governs a host that is using
		// an address: "Address Conflict Detection is an ongoing process that
		// is in effect for as long as a host is using an address. At any time,
		// if a host receives an ARP packet (Request *or* Reply) where the
		// 'sender IP address' is (one of) the host's own IP address(es)
		// configured on that interface, but the 'sender hardware address' does
		// not match any of the host's own interface addresses, then this is a
		// conflicting ARP packet". Section 2.5 then requires the very frames
		// the unexempted rule was declining on: "whenever a host receives an
		// ARP Request, that's not a conflicting ARP packet as described above
		// in Section 2.4, where the 'target IP address' of the ARP Request is
		// (one of) the host's own IP address(es) configured on that interface,
		// the host MUST respond with an ARP Reply". A rule that declines the
		// lease on a reply section 2.5 makes mandatory is not a stricter
		// reading of section 2.1.1; it is two of the document's sections
		// contradicting each other.
		//
		// So section 2.4's predicate is used in every phase, and section
		// 2.1.1's own NOTE says the same thing in the same words: "the
		// precaution described above is necessary to ensure that a host is not
		// confused when it sees its own ARP packets echoed back."
		//
		// WHAT THIS MUST NOT COST: a FOREIGN host claiming the address in this
		// window is still a conflict, in every mode. The exemption is keyed on
		// the sender hardware address being ours and on nothing else — not on
		// the mode, not on whether the caller has been told Acquired — so it
		// cannot widen to cover a squatter. TestEveryPhaseAndPacketClass
		// carries both directions and the netns runs drive both on a wire.
		if p.SenderIP == a.addr && !a.isOurs(p.SenderHW) {
			return "RFC 5227 2.1.1", "an ARP packet from " + hw(p.SenderHW) +
				" claims " + a.addr.String() + " while we are probing for it"
		}
		// Section 2.1.1, second rule: "if during this period the host receives
		// any ARP Probe where the packet's 'target IP address' is the address
		// being probed for, and the packet's 'sender hardware address' is not
		// the hardware address of any of the host's interfaces, then the host
		// SHOULD similarly treat this as an address conflict ... This can
		// occur if two (or more) hosts have, for whatever reason, been
		// inadvertently configured with the same address, and both are
		// simultaneously in the process of probing that address."
		if p.IsProbe() && p.TargetIP == a.addr && !a.isOurs(p.SenderHW) {
			return "RFC 5227 2.1.1", "another host at " + hw(p.SenderHW) +
				" is probing for " + a.addr.String() + " at the same time"
		}
		return "", ""

	case ACDAnnouncing, ACDDefending:
		// Section 2.4: "At any time, if a host receives an ARP packet (Request
		// *or* Reply) where the 'sender IP address' is (one of) the host's own
		// IP address(es) configured on that interface, but the 'sender
		// hardware address' does not match any of the host's own interface
		// addresses, then this is a conflicting ARP packet, indicating some
		// other host also thinks it is validly using this address."
		//
		// THE HARDWARE-ADDRESS CHECK IS LOAD-BEARING AND IS NOT AN
		// OPTIMISATION. Section 2.1.1's NOTE: "Some kinds of Ethernet hub
		// (often called a 'buffered repeater') and many wireless access points
		// may 'rebroadcast' any received broadcast packets to all recipients,
		// including the original sender itself. For this reason, the
		// precaution described above is necessary to ensure that a host is not
		// confused when it sees its own ARP packets echoed back." An
		// AF_PACKET socket sees this host's own outgoing frames unconditionally,
		// so without this check every announcement this client sends is a
		// conflict with itself and no lease ever survives.
		if p.SenderIP == a.addr && !a.isOurs(p.SenderHW) {
			return "RFC 5227 2.4", hw(p.SenderHW) + " is using " + a.addr.String() +
				", which this client holds"
		}
		return "", ""

	default:
		return "", ""
	}
}

// isOurs reports whether hw is this client's hardware address.
//
// BOUND: section 2.4 says "any of the host's own interface addresses" and this
// knows ONE — the address this client leases with. A host whose second
// interface answered for our address would be read as a conflict. That is the
// narrowing direction: it costs a DHCPDECLINE and a re-acquisition, where the
// widening direction costs two hosts on one address.
func (a *acd) isOurs(b []byte) bool {
	if len(b) == 0 || len(b) != len(a.hw) {
		return false
	}
	for i := range b {
		if b[i] != a.hw[i] {
			return false
		}
	}
	return true
}

// relevant reports whether this ARP packet is worth a Step at all.
//
// IT IS A SUBSCRIPTION, NOT A VERDICT, and the difference decides where it may
// live. Ring 2 feeds every ARP frame on the link into this client, and every
// frame that reaches Step costs a journal entry — so on a link with ordinary
// ARP traffic the bounded journal would wrap between one acquisition and the
// next and the client would stop being replayable, which is R3's whole point.
//
// The predicate is HERE, in ring 1, rather than in the loop that calls it,
// because a filter written beside a channel is a protocol rule with no fast
// test. TestTheRelevanceFilterCannotHideAConflict asserts it against
// conflictRule over the adversarial corpus: every packet the rules call a
// conflict is one this admits.
func (a *acd) relevant(p *wire.ARPPacket) bool {
	if p == nil || !a.running() || !a.addr.IsValid() {
		return false
	}
	return p.SenderIP == a.addr || p.TargetIP == a.addr
}

// rateLimited reports whether section 2.1.1's rate limit has engaged, and is
// the boundary the table drives.
//
// "if the host experiences MAX_CONFLICTS or more address conflicts on a given
// interface, then the host MUST limit the rate at which it probes for new
// addresses on this interface to no more than one attempted new address per
// RATE_LIMIT_INTERVAL." OR MORE, so the MAX_CONFLICTSth conflict is the first
// one that engages it, not the one after: at ten conflicts the host has
// "experienced MAX_CONFLICTS or more", and TestTheRateLimitEngagesAtTheTenth
// drives both sides of that boundary.
func (a *acd) rateLimited() bool {
	return a.params.MaxConflicts > 0 && a.conflicts >= a.params.MaxConflicts
}

// restartDelay is how long to wait before the next attempt at a new address,
// given the delay the DHCP machine would otherwise use.
//
// The limit is expressed as a rate — "no more than one attempted new address
// per RATE_LIMIT_INTERVAL" — so it is measured from the START OF THE LAST
// ATTEMPT and not from the conflict that ended it. Measuring it from the
// conflict would make the interval longer than the RFC's whenever a probe ran
// for a while before failing, which is a different protocol.
//
// It never SHORTENS the caller's delay. RFC 2131 section 3.1(5)'s "The client
// SHOULD wait a minimum of ten seconds before restarting the configuration
// process" is a floor that this must not undercut, so the two bounds compose
// as a maximum rather than one replacing the other.
func (a *acd) restartDelay(now Instant, base Duration) Duration {
	if !a.rateLimited() {
		return base
	}
	next := a.attemptAt.Add(a.params.RateLimitInterval)
	d := next.Sub(now)
	if d < base {
		return base
	}
	return d
}

// probe builds the section 2.1.1 ARP Probe for the address under test.
//
// "The client MUST fill in the 'sender hardware address' field of the ARP
// Request with the hardware address of the interface through which it is
// sending the packet. The 'sender IP address' field MUST be set to all zeroes;
// this is to avoid polluting ARP caches in other hosts on the same link in the
// case where the address turns out to be already in use by another host. The
// 'target hardware address' field is ignored and SHOULD be set to all zeroes.
// The 'target IP address' field MUST be set to the address being probed."
//
// THE ALL-ZERO SENDER IS WHAT THIS WHOLE MILESTONE IS FOR. Design document
// section 8.4: v1.x forces L2 resolution by sending a UDP datagram from a REAL
// source address, which is not an ARP Probe and does pollute caches. The zero
// is not reachable from any field of this struct, so no future edit can fill
// it in from somewhere.
func (a *acd) probe() *wire.ARPPacket {
	return &wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: append([]byte(nil), a.hw...),
		SenderIP: netip.AddrFrom4([4]byte{}),
		TargetIP: a.addr,
	}
}

// announcement builds the section 2.3 ARP Announcement.
//
// "An ARP Announcement is identical to the ARP Probe described above, except
// that now the sender and target IP addresses are both set to the host's newly
// selected IPv4 address." Section 3 of the same document is why it is a
// Request and not a Reply: "BSD Unix, Microsoft Windows, Mac OS 9, Mac OS X,
// etc., all use ARP Request packets as described in Stevens."
func (a *acd) announcement() *wire.ARPPacket {
	return &wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: append([]byte(nil), a.hw...),
		SenderIP: a.addr,
		TargetIP: a.addr,
	}
}

// uniform returns a value drawn uniformly from [lo, hi] using rnd.
//
// Inclusive at both ends, and lo when the span is not positive, which is what
// makes a fixture with lo == hi mean "exactly this" rather than "panic".
func uniform(lo, hi Duration, rnd uint64) Duration {
	span := hi - lo
	if span <= 0 {
		return lo
	}
	return lo + Duration(rnd%uint64(span+1))
}

// hw renders a hardware address for a journal line.
//
// A conflict journal line names the OTHER host, because "somebody has your
// address" is not actionable and "3a:1f:… has your address" is: it is what an
// operator greps the switch's forwarding table for.
func hw(b []byte) string {
	if len(b) == 0 {
		return "an unknown host"
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 3*len(b)-1)
	for i, c := range b {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexd[c>>4], hexd[c&0x0f])
	}
	return string(out)
}

// validate refuses a partially-filled RFC 5227 constant table.
//
// The zero struct is the RFC table (Params.acd), so the only thing to check is
// that a table a caller DID fill in was filled in completely. Every field is
// required to be positive, including the two counts: a table naming zero
// probes or zero announcements is not a faster ACD, it is a different protocol,
// and a caller that wants no probing at all has ConflictOff to say so.
func (a ACDParams) validate() error {
	if a == (ACDParams{}) {
		return nil
	}
	for _, f := range []struct {
		name string
		d    Duration
	}{
		{"ProbeWait", a.ProbeWait},
		{"ProbeMin", a.ProbeMin},
		{"ProbeMax", a.ProbeMax},
		{"AnnounceWait", a.AnnounceWait},
		{"AnnounceInterval", a.AnnounceInterval},
		{"RateLimitInterval", a.RateLimitInterval},
		{"DefendInterval", a.DefendInterval},
	} {
		if f.d <= 0 {
			return fmt.Errorf("%w: %s is %s", ErrBadACDParams, f.name, f.d)
		}
	}
	for _, f := range []struct {
		name string
		n    int
	}{
		{"ProbeNum", a.ProbeNum},
		{"AnnounceNum", a.AnnounceNum},
		{"MaxConflicts", a.MaxConflicts},
	} {
		if f.n <= 0 {
			return fmt.Errorf("%w: %s is %d", ErrBadACDParams, f.name, f.n)
		}
	}
	if a.ProbeMin > a.ProbeMax {
		return fmt.Errorf("%w: ProbeMin %s is greater than ProbeMax %s",
			ErrBadACDParams, a.ProbeMin, a.ProbeMax)
	}
	return nil
}
