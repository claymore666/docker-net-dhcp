package proto

import (
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// The RFC 5227 tests. Ring 1, so no clock, no socket and no namespace: the
// whole of section 1.1's arithmetic and section 2.1.1's rules are decided here
// against a swept rnd, and the netns run measures them once on a real wire.

const testACDAddr = "192.168.99.50"

// theirMAC is the squatter's hardware address in these fixtures. It differs
// from testCHAddr in the LAST octet only, so a comparison that looked at a
// prefix rather than the whole address would pass every row.
var theirMAC = []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x63}

// acdMachine drives a client to the moment after its DHCPACK, in the given
// mode, and returns the machine and the action list the ACK produced.
//
// Reached through the wire in every mode — Start, OFFER, ACK — rather than by
// setting fields, because what the modes DIFFER in is exactly what that action
// list contains.
func acdMachine(t *testing.T, mode ConflictMode) (*Machine, []Action) {
	t.Helper()
	m := newMachine(t, acdParams(mode))
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testACDAddr, "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	_, acts = m.Step(at(2), 3, received(t, ackFor(req, testACDAddr, "192.168.99.1", 3600)))
	return m, acts
}

// armedACD reports the delay TimerACD was last armed for in this action list.
func armedACD(acts []Action) (Duration, bool) {
	for i := len(acts) - 1; i >= 0; i-- {
		if acts[i].Kind == ActSetTimer && acts[i].Timer == TimerACD {
			return acts[i].After, true
		}
	}
	return 0, false
}

func arpSends(acts []Action) []*wire.ARPPacket {
	var out []*wire.ARPPacket
	for _, a := range acts {
		if a.Kind == ActSendARP {
			out = append(out, a.ARP)
		}
	}
	return out
}

// probe, announcement, reply and request build the four ARP packet classes the
// tables below cross with the phases.
func probeFrom(hw []byte, target string) *wire.ARPPacket {
	return &wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: hw,
		SenderIP: netip.MustParseAddr("0.0.0.0"),
		TargetIP: netip.MustParseAddr(target),
	}
}

func announcementFrom(hw []byte, addr string) *wire.ARPPacket {
	a := netip.MustParseAddr(addr)
	return &wire.ARPPacket{Op: wire.ARPRequest, SenderHW: hw, SenderIP: a, TargetIP: a}
}

func replyFrom(hw []byte, sender, target string) *wire.ARPPacket {
	return &wire.ARPPacket{
		Op:       wire.ARPReply,
		SenderHW: hw,
		SenderIP: netip.MustParseAddr(sender),
		TargetIP: netip.MustParseAddr(target),
	}
}

func requestFrom(hw []byte, sender, target string) *wire.ARPPacket {
	return &wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: hw,
		SenderIP: netip.MustParseAddr(sender),
		TargetIP: netip.MustParseAddr(target),
	}
}

// ------------------------------------------------------- the constants --

// TestACDConstantsAreTheRFCValues pins all ten of RFC 5227 section 1.1.
//
// EVERY ONE OF THEM, including DefendInterval, which nothing in this library
// reads. Section 1.1: "Note that the values listed here are fixed constants;
// they are not intended to be modifiable by implementers, operators, or end
// users." A partial table invites the missing row to be added later with a
// value nobody checked, and the one this client does not act on is exactly the
// row that would be added carelessly.
func TestACDConstantsAreTheRFCValues(t *testing.T) {
	p := DefaultACDParams()
	cases := []struct {
		name string
		got  Duration
		want Duration
	}{
		{"PROBE_WAIT", p.ProbeWait, 1 * Second},
		{"PROBE_MIN", p.ProbeMin, 1 * Second},
		{"PROBE_MAX", p.ProbeMax, 2 * Second},
		{"ANNOUNCE_WAIT", p.AnnounceWait, 2 * Second},
		{"ANNOUNCE_INTERVAL", p.AnnounceInterval, 2 * Second},
		{"RATE_LIMIT_INTERVAL", p.RateLimitInterval, 60 * Second},
		{"DEFEND_INTERVAL", p.DefendInterval, 10 * Second},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %s, want RFC 5227 1.1's %s", c.name, c.got, c.want)
		}
	}
	counts := []struct {
		name string
		got  int
		want int
	}{
		{"PROBE_NUM", p.ProbeNum, 3},
		{"ANNOUNCE_NUM", p.AnnounceNum, 2},
		{"MAX_CONFLICTS", p.MaxConflicts, 10},
	}
	for _, c := range counts {
		if c.got != c.want {
			t.Errorf("%s = %d, want RFC 5227 1.1's %d", c.name, c.got, c.want)
		}
	}
	if got := DefaultParams(testCHAddr).ACD; got != p {
		t.Errorf("DefaultParams installs %+v, want the RFC table %+v", got, p)
	}
}

// TestTheDefaultConflictModeIsWait reads the fact from BOTH derivations.
//
// The zero value and DefaultParams are two ways to get a parameter set, and
// the looser one decides what a caller actually runs: a Params{} that meant
// ConflictOff would silently disable this whole milestone for every caller
// that filled its fields in by hand, with the suite still green (defeat row
// M6-17, M6-26).
func TestTheDefaultConflictModeIsWait(t *testing.T) {
	if got := (Params{}).Conflict; got != ConflictWait {
		t.Errorf("the zero Params runs in %s, want wait — the safe mode must be the one a caller gets by not knowing about the knob", got)
	}
	if got := DefaultParams(testCHAddr).Conflict; got != ConflictWait {
		t.Errorf("DefaultParams runs in %s, want wait", got)
	}
	m := newMachine(t, DefaultParams(testCHAddr))
	if m.acd == nil {
		t.Fatal("a default machine has no ACD sub-machine, so it would never probe")
	}
	// And the zero ACDParams really is the RFC table, not zeroes.
	if got := (Params{CHAddr: testCHAddr}).acd(); got != DefaultACDParams() {
		t.Errorf("the zero ACDParams resolves to %+v, want the RFC table", got)
	}
}

// TestPartialACDParamsAreRefused drives defeat row M6-25's neighbour: a table
// a caller filled in by halves.
//
// The tempting mistake is setting ProbeNum alone. Merging that with the
// defaults field by field would be the friendly behaviour and is the wrong
// one: it would make a table mean something different depending on which
// fields happen to be zero, so a caller asking for two probes and a caller
// asking for two probes and no ANNOUNCE_WAIT would be indistinguishable.
func TestPartialACDParamsAreRefused(t *testing.T) {
	full := DefaultACDParams()
	cases := []struct {
		name string
		acd  ACDParams
	}{
		{"only ProbeNum", ACDParams{ProbeNum: 2}},
		{"a table missing ANNOUNCE_WAIT", func() ACDParams { a := full; a.AnnounceWait = 0; return a }()},
		{"a table with no probes", func() ACDParams { a := full; a.ProbeNum = 0; return a }()},
		{"a table with no announcements", func() ACDParams { a := full; a.AnnounceNum = 0; return a }()},
		{"a negative interval", func() ACDParams { a := full; a.ProbeMin = -1; return a }()},
		{"PROBE_MIN above PROBE_MAX", func() ACDParams { a := full; a.ProbeMin = 5 * Second; return a }()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := DefaultParams(testCHAddr)
			p.ACD = c.acd
			if _, err := New(p); err == nil {
				t.Fatal("New accepted a partial RFC 5227 constant table")
			}
		})
	}
	// The preservation control: the complete table is accepted, so the
	// refusal above is not simply refusing everything.
	p := DefaultParams(testCHAddr)
	p.ACD = full
	if _, err := New(p); err != nil {
		t.Fatalf("New refused the complete RFC table: %v", err)
	}
}

// TestAnUnknownConflictModeIsRefused is defeat row M6-25.
//
// Refused rather than defaulted. The default is the SAFE mode, so silently
// defaulting an out-of-range value would turn a caller that meant ConflictOff
// into one that waits seconds per acquisition and never says why — and two
// switch statements with different default arms would then disagree about what
// the same client is doing.
func TestAnUnknownConflictModeIsRefused(t *testing.T) {
	p := DefaultParams(testCHAddr)
	p.Conflict = ConflictMode(7)
	if _, err := New(p); err == nil {
		t.Fatal("New accepted ConflictMode(7)")
	}
	for _, mode := range AllConflictModes() {
		p := DefaultParams(testCHAddr)
		p.Conflict = mode
		if _, err := New(p); err != nil {
			t.Errorf("New refused %s: %v", mode, err)
		}
	}
}

// ---------------------------------------------------- the probe schedule --

// TestTheProbeScheduleIsRFC5227Section21 drives the whole of the schedule and
// reads the count, the order and the kinds off the actions.
//
// It uses the REAL constants, not the scaled fixtures, because the subject is
// the arithmetic. There is no clock here, so the seconds cost nothing.
func TestTheProbeScheduleIsRFC5227Section21(t *testing.T) {
	p := DefaultParams(testCHAddr)
	p.DesyncMin, p.DesyncMax = 0, 0
	m := newMachine(t, p)

	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testACDAddr, "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	_, acts = m.Step(at(2), 3, received(t, ackFor(req, testACDAddr, "192.168.99.1", 3600)))

	if m.State() != StateProbing {
		t.Fatalf("after the DHCPACK the machine is %s, want PROBING", m.State())
	}
	if n := count(acts, ActLeaseAcquired); n != 0 {
		t.Fatalf("the DHCPACK announced the lease %d time(s) before probing (D22)", n)
	}
	if n := len(arpSends(acts)); n != 0 {
		t.Fatalf("a probe went out in the same step as the DHCPACK; RFC 5227 2.1.1 requires the initial random delay first")
	}
	first, ok := armedACD(acts)
	if !ok {
		t.Fatal("no ACD timer was armed after the DHCPACK, so nothing would ever probe")
	}
	if first < 0 || first > 1*Second {
		t.Fatalf("the initial delay is %s, want RFC 5227 1.1's uniform draw from [0, PROBE_WAIT=1s]", first)
	}

	var probes, announcements int
	var lastGap Duration
	now := at(2)
	gaps := []Duration{}
	delay := first
	for step := 0; step < 10; step++ {
		now = now.Add(delay)
		_, acts = m.Step(now, uint64(step*7919+3), TimerFired(TimerACD))
		for _, a := range arpSends(acts) {
			if a.IsProbe() {
				probes++
			} else {
				announcements++
			}
		}
		next, armed := armedACD(acts)
		if !armed {
			break
		}
		gaps = append(gaps, next)
		lastGap = next
		delay = next
	}
	_ = lastGap

	if probes != 3 {
		t.Errorf("%d ARP Probes, want RFC 5227 1.1's PROBE_NUM = 3", probes)
	}
	if announcements != 2 {
		t.Errorf("%d ARP Announcements, want ANNOUNCE_NUM = 2", announcements)
	}
	if m.State() != StateBound {
		t.Errorf("the machine ended in %s, want BOUND", m.State())
	}
	// The gaps, in order: two probe spacings, then ANNOUNCE_WAIT, then one
	// ANNOUNCE_INTERVAL. Read positionally because the ORDER is the schedule.
	if len(gaps) != 4 {
		t.Fatalf("%d gaps armed %v, want 2 probe spacings + ANNOUNCE_WAIT + ANNOUNCE_INTERVAL", len(gaps), gaps)
	}
	for i := 0; i < 2; i++ {
		if gaps[i] < 1*Second || gaps[i] > 2*Second {
			t.Errorf("probe gap %d is %s, want RFC 5227 2.1.1's [PROBE_MIN=1s, PROBE_MAX=2s]", i+1, gaps[i])
		}
	}
	if gaps[2] != 2*Second {
		t.Errorf("the wait after the last probe is %s, want ANNOUNCE_WAIT = 2s", gaps[2])
	}
	if gaps[3] != 2*Second {
		t.Errorf("the gap between announcements is %s, want ANNOUNCE_INTERVAL = 2s", gaps[3])
	}
}

// TestTheProbeSpacingCoversTheWholeRFCWindow sweeps rnd and asserts the draws
// land across [PROBE_MIN, PROBE_MAX] rather than at one end of it.
//
// A uniform() that ignored rnd and returned PROBE_MIN would satisfy every
// range assertion above, and it would defeat the purpose of the spacing: RFC
// 5227 section 2.1.1 spaces the probes "randomly and uniformly" so that two
// hosts probing the same address at the same moment do not stay in lockstep.
func TestTheProbeSpacingCoversTheWholeRFCWindow(t *testing.T) {
	lo, hi := 1*Second, 2*Second
	span := uint64(hi - lo)

	// The spread. rnd comes from a real entropy source, so it is swept across
	// the whole uint64 range and not 0..N — a sweep of small integers on a
	// nanosecond-resolution window only ever draws the bottom microsecond of
	// it, which would make this test measure the fixture rather than uniform.
	const buckets = 10
	hit := map[int]int{}
	rnd := uint64(0x243F6A8885A308D3)
	for i := 0; i < 20000; i++ {
		// A cheap 64-bit mixer, so consecutive draws are not consecutive
		// integers. Any spreading function does; this one is deterministic.
		rnd ^= rnd << 13
		rnd ^= rnd >> 7
		rnd ^= rnd << 17
		d := uniform(lo, hi, rnd)
		if d < lo || d > hi {
			t.Fatalf("uniform drew %s, outside [PROBE_MIN, PROBE_MAX]", d)
		}
		b := int(uint64(d-lo) * buckets / (span + 1))
		hit[b]++
	}
	for b := 0; b < buckets; b++ {
		if hit[b] == 0 {
			t.Errorf("tenth %d of [PROBE_MIN, PROBE_MAX] was never drawn in 20000 draws; the spacing is not spread over the window", b)
		}
	}

	// Inclusive at both ends, asserted at the ends rather than hoped for out
	// of a sweep: RFC 5227 2.1.1's spacing is "PROBE_MIN to PROBE_MAX seconds
	// apart", and a half-open draw would never wait the full PROBE_MAX.
	if got := uniform(lo, hi, 0); got != lo {
		t.Errorf("uniform(lo, hi, 0) = %s, want PROBE_MIN %s", got, lo)
	}
	if got := uniform(lo, hi, span); got != hi {
		t.Errorf("uniform(lo, hi, span) = %s, want PROBE_MAX %s", got, hi)
	}
	if got := uniform(lo, hi, span+1); got != lo {
		t.Errorf("uniform wrapped to %s, want PROBE_MIN %s", got, lo)
	}
	// The zero-width case a fixture uses: lo == hi means exactly that, not a
	// panic and not a modulo by zero.
	if got := uniform(5*Second, 5*Second, 12345); got != 5*Second {
		t.Errorf("uniform(5s, 5s) = %s, want 5s", got)
	}
	if got := uniform(3*Second, 1*Second, 12345); got != 3*Second {
		t.Errorf("an inverted window drew %s, want the low end", got)
	}
}

// TestTheFirstAnnouncementPrecedesTheAcquisition holds the ORDER inside the
// step that ends probing.
//
// RFC 5227 section 2.3: "The host may begin legitimately using the IP address
// immediately after sending the first of the two ARP Announcements" — after
// SENDING. A ring-2 caller drains the action list in order and configures the
// address when it sees ActLeaseAcquired, so an announcement emitted after the
// acquisition would let the address be used before the link had been told who
// holds it. An index comparison, because that is where the order is decidable.
func TestTheFirstAnnouncementPrecedesTheAcquisition(t *testing.T) {
	m, acts := acdMachine(t, ConflictWait)
	delay, _ := armedACD(acts)
	now := at(2)
	for step := 0; step < 10; step++ {
		now = now.Add(delay)
		_, acts = m.Step(now, 5, TimerFired(TimerACD))
		if count(acts, ActLeaseAcquired) > 0 {
			break
		}
		var ok bool
		delay, ok = armedACD(acts)
		if !ok {
			t.Fatal("the schedule ended without acquiring")
		}
	}
	announce, acquire := -1, -1
	for i, a := range acts {
		if a.Kind == ActSendARP && announce < 0 {
			announce = i
		}
		if a.Kind == ActLeaseAcquired {
			acquire = i
		}
	}
	if announce < 0 || acquire < 0 {
		t.Fatalf("the acquiring step has announcement=%d acquired=%d; want both", announce, acquire)
	}
	if announce > acquire {
		t.Fatalf("ActLeaseAcquired is at index %d and the first ARP Announcement at %d; RFC 5227 2.3 permits use only after the announcement has been SENT", acquire, announce)
	}
}

// ----------------------------------------------------------- the modes --

// TestTheModesDifferInWhenAcquiredIsEmitted is Amendment 1 (D23) itself, and
// it is ONE assertion with the expected order flipped rather than three tests.
//
// The three modes are indistinguishable from broken implementations of one
// another on any assertion that only checks that a DHCPDECLINE eventually
// happens (defeat rows M6-20, M6-21, M6-22). What separates them is exactly
// this: WHEN the caller is told it has an address, relative to the probes.
func TestTheModesDifferInWhenAcquiredIsEmitted(t *testing.T) {
	cases := []struct {
		mode ConflictMode
		// acquiresOnTheAck: the DHCPACK's own action list announces the lease.
		acquiresOnTheAck bool
		// probes: the client ever puts an ARP Probe on the wire.
		probes bool
		state  State
	}{
		{ConflictWait, false, true, StateProbing},
		{ConflictAsync, true, true, StateBound},
		{ConflictOff, true, false, StateBound},
	}
	for _, c := range cases {
		t.Run(c.mode.String(), func(t *testing.T) {
			m, acts := acdMachine(t, c.mode)
			if m.State() != c.state {
				t.Errorf("after the DHCPACK the machine is %s, want %s", m.State(), c.state)
			}
			got := count(acts, ActLeaseAcquired) > 0
			if got != c.acquiresOnTheAck {
				t.Errorf("the DHCPACK announced the lease: %v, want %v", got, c.acquiresOnTheAck)
			}
			// And the ACD state is visible, which is what a restarting caller
			// reads to know whether to resume the probe.
			wantPhase := ACDProbing
			if c.mode == ConflictOff {
				wantPhase = ACDIdle
			}
			if m.ACDPhase() != wantPhase {
				t.Errorf("ACDPhase after the DHCPACK = %s, want %s", m.ACDPhase(), wantPhase)
			}

			all := append([]Action(nil), acts...)
			delay, armed := armedACD(acts)
			for step := 0; armed && step < 12; step++ {
				_, acts = m.Step(at(2).Add(delay), uint64(step+1), TimerFired(TimerACD))
				all = append(all, acts...)
				delay, armed = armedACD(acts)
			}
			var probes int
			for _, a := range arpSends(all) {
				if a.IsProbe() {
					probes++
				}
			}
			if (probes > 0) != c.probes {
				t.Errorf("%d ARP Probes went out, want any: %v", probes, c.probes)
			}
			if !c.probes {
				return
			}
			// The order across the whole run, which is what the two mutants
			// on the list move.
			firstProbe, acquire := -1, -1
			for i, a := range all {
				if a.Kind == ActSendARP && a.ARP.IsProbe() && firstProbe < 0 {
					firstProbe = i
				}
				if a.Kind == ActLeaseAcquired && acquire < 0 {
					acquire = i
				}
			}
			if firstProbe < 0 || acquire < 0 {
				t.Fatalf("firstProbe=%d acquire=%d; want both in the run", firstProbe, acquire)
			}
			if c.mode == ConflictWait && acquire < firstProbe {
				t.Errorf("wait announced the lease at index %d, before the first probe at %d: D22 says the address is not usable until the check completes", acquire, firstProbe)
			}
			if c.mode == ConflictAsync && acquire > firstProbe {
				t.Errorf("async announced the lease at index %d, after the first probe at %d: async announces at once and probes beside use", acquire, firstProbe)
			}
		})
	}
}

// TestConflictOffOpensNothing is defeat row M6-22: off is a SHAPE, not a flag.
//
// A mode implemented as `if !off { probe() }` scattered at call sites is one
// missed call site away from probing anyway, and the missed one is invisible.
// Here there is no sub-machine to ask, so there is nothing to forget.
func TestConflictOffOpensNothing(t *testing.T) {
	m := newMachine(t, acdParams(ConflictOff))
	if m.acd != nil {
		t.Fatal("a ConflictOff machine holds an ACD sub-machine; off must have nothing to run")
	}
	if m.ARPRelevant(replyFrom(theirMAC, testACDAddr, testACDAddr)) {
		t.Error("a ConflictOff machine subscribes to ARP; there is no listener to feed")
	}
	// And every ARP packet class, in every state, changes nothing.
	for _, s := range AllStates() {
		if s == StateProbing {
			continue // unreachable in this mode; TestEveryPhaseAndPacketClass covers it
		}
		for _, p := range adversarialARP() {
			mm := newMachine(t, acdParams(ConflictOff))
			mm.state = s
			before := mm.State()
			_, acts := mm.Step(at(10), 1, ARPReceived(p.pkt))
			if mm.State() != before {
				t.Errorf("%s: %s moved a ConflictOff machine out of %s", s, p.name, before)
			}
			if n := count(acts, ActSend); n != 0 {
				t.Errorf("%s: %s made a ConflictOff machine send %d message(s)", s, p.name, n)
			}
		}
	}
}

// TestConflictOffStillDeclinesOnAReportedConflict is defeat row M6-23.
//
// Off means no probing and no listener. It does NOT mean the caller's own
// evidence stops working: RFC 2131 section 3.1(5)'s DHCPDECLINE is a MUST once
// a conflict IS detected, and a caller with a detector of its own has detected
// one.
func TestConflictOffStillDeclinesOnAReportedConflict(t *testing.T) {
	m, _ := acdMachine(t, ConflictOff)
	if m.State() != StateBound {
		t.Fatalf("fixture is %s, want BOUND", m.State())
	}
	_, acts := m.Step(at(10), 1, Simple(EvConflictDetected))
	msg := mustSend(t, acts, wire.MsgDecline)
	got, ok := msg.Addr4(wire.OptRequestedIP)
	if !ok || got.String() != testACDAddr {
		t.Errorf("the DHCPDECLINE names %s (present %v), want %s", got, ok, testACDAddr)
	}
	if count(acts, ActLeaseLost) != 1 {
		t.Error("no ActLeaseLost after a reported conflict on a held lease")
	}
}

// ------------------------------------------------- the conflict rules --

// arpCase is one row of the (phase, packet class) product.
type arpCase struct {
	name string
	pkt  *wire.ARPPacket
}

// adversarialARP is the corpus: every packet class the tables cross with every
// phase, D17's rows included.
//
// It is ONE list used by three tests — the phase table, the relevance-filter
// check and the ConflictOff table — because the whole risk in a predicate like
// this is a packet class that one test knows about and another does not.
func adversarialARP() []arpCase {
	return []arpCase{
		{
			name: "an ARP Reply claiming our address, from another host",
			pkt:  replyFrom(theirMAC, testACDAddr, "192.168.99.9"),
		},
		{
			name: "an ARP Request whose sender IP is our address, from another host",
			pkt:  requestFrom(theirMAC, testACDAddr, "192.168.99.9"),
		},
		{
			name: "another host's Announcement for our address",
			pkt:  announcementFrom(theirMAC, testACDAddr),
		},
		{
			// D17. AF_PACKET delivers this host's own outgoing frames back to
			// it, so this is not a hypothetical row: it is what our own
			// Announcement looks like coming back.
			name: "an ARP Reply for our address from our OWN hardware address",
			pkt:  replyFrom(testCHAddr, testACDAddr, "192.168.99.9"),
		},
		{
			name: "our own Announcement echoed back by the link",
			pkt:  announcementFrom(testCHAddr, testACDAddr),
		},
		{
			// Our own Probe echoed back. Sender IP is zero, so section
			// 2.1.1's first rule cannot fire on it, and the second rule
			// exempts our own hardware address.
			name: "our own Probe echoed back by the link",
			pkt:  probeFrom(testCHAddr, testACDAddr),
		},
		{
			// ROUND 2, FINDING 1. The section 2.5 ARP Reply, as it comes back
			// on our own AF_PACKET socket: "whenever a host receives an ARP
			// Request ... where the 'target IP address' of the ARP Request is
			// (one of) the host's own IP address(es) configured on that
			// interface, the host MUST respond with an ARP Reply". Sender IP
			// is the leased address and the sender hardware address is ours,
			// because the kernel that sent it is ours. It exists the moment
			// the address is configured, which in ConflictAsync is while the
			// probe window is still open.
			name: "the kernel's own section 2.5 ARP Reply for our address",
			pkt:  replyFrom(testCHAddr, testACDAddr, "192.168.99.9"),
		},
		{
			// ROUND 2, FINDING 1, the other frame: our own stack resolving a
			// neighbour once the address is up. An ordinary ARP Request, whose
			// sender IP is the leased address because that is the source it
			// will send from.
			name: "our own ARP Request for a neighbour, from our address",
			pkt:  requestFrom(testCHAddr, testACDAddr, "192.168.99.222"),
		},
		{
			// Our own Probe for a DIFFERENT address. Neither rule can fire:
			// the sender IP is zero and the target is not the address under
			// test. It is here so that the exemption cannot be written as
			// "anything from our own MAC in the probe window is fine" and go
			// unnoticed on a corpus that never sends one.
			name: "our own Probe for a DIFFERENT address",
			pkt:  probeFrom(testCHAddr, "192.168.99.77"),
		},
		{
			name: "another host probing for OUR address at the same time",
			pkt:  probeFrom(theirMAC, testACDAddr),
		},
		{
			name: "another host probing for a DIFFERENT address",
			pkt:  probeFrom(theirMAC, "192.168.99.77"),
		},
		{
			name: "an ARP Reply about a different address entirely",
			pkt:  replyFrom(theirMAC, "192.168.99.77", "192.168.99.9"),
		},
		{
			// D17: a sender hardware address of zero. It matches nothing, so
			// it is not ours, so a claim on our address from it is a conflict
			// — which is the safe direction: the alternative is a host that
			// zeroes the field being able to take our address silently.
			name: "an ARP Reply for our address with a zero sender hardware address",
			pkt:  replyFrom([]byte{0, 0, 0, 0, 0, 0}, testACDAddr, "192.168.99.9"),
		},
		{
			name: "a nil packet",
			pkt:  nil,
		},
	}
}

// TestEveryPhaseAndPacketClass is the table the brief asks for: every (ACD
// phase, ARP packet class) pair, in every mode that has phases.
//
// THE EXPECTED VERDICTS ARE WRITTEN PER PHASE and not per packet, because that
// is the thing that changes: section 2.1.1's rules apply during the probe
// window, section 2.4's afterwards, and the SAME packet is a conflict under one
// and not the other. A table keyed on the packet alone would be a table that
// could not see the bug.
func TestEveryPhaseAndPacketClass(t *testing.T) {
	// conflictIn[phase][packet name] is whether RFC 5227 calls it a conflict.
	// Named by rule in the comment beside each block.
	probeWindow := map[string]string{
		// Section 2.1.1, first rule: an ARP packet, Request or Reply, whose
		// sender IP is the address being probed — READ WITH SECTION 2.4'S
		// HARDWARE-ADDRESS CLAUSE, which is the correction round 2 makes.
		//
		// ROUND 1 PINNED THE OPPOSITE HERE, deliberately and wrongly: the two
		// own-MAC rows below asserted "RFC 5227 2.1.1", on the reading that
		// section 2.1.1's first rule is written without an exemption and that
		// adding one would weaken it. The premise that made that reading safe
		// — that a probing host cannot itself emit a frame carrying the
		// address — is false for a host that is already USING the address,
		// which is what ConflictAsync promises and what a renewal onto a moved
		// address forces even in ConflictWait. Section 2.4 governs a host that
		// is using an address and carries the clause; section 2.5 makes the
		// reply in the fourth row MANDATORY. conflictRule has the text.
		"an ARP Reply claiming our address, from another host":             "RFC 5227 2.1.1",
		"an ARP Request whose sender IP is our address, from another host": "RFC 5227 2.1.1",
		"another host's Announcement for our address":                      "RFC 5227 2.1.1",
		"an ARP Reply for our address with a zero sender hardware address": "RFC 5227 2.1.1",
		// Section 2.1.1, second rule: an ARP Probe whose target is the address
		// being probed and whose sender hardware address is not ours.
		"another host probing for OUR address at the same time": "RFC 5227 2.1.1",
		// Everything the host itself put on the wire is absent from this map,
		// which is the assertion: "an ARP Reply for our address from our OWN
		// hardware address", "our own Announcement echoed back by the link",
		// "the kernel's own section 2.5 ARP Reply for our address" and "our
		// own ARP Request for a neighbour, from our address" are all "".
	}
	ongoing := map[string]string{
		// Section 2.4: sender IP is one of our addresses AND the sender
		// hardware address is not one of ours. The second clause is what
		// exempts our own echoed announcements, and it is load-bearing.
		"an ARP Reply claiming our address, from another host":             "RFC 5227 2.4",
		"an ARP Request whose sender IP is our address, from another host": "RFC 5227 2.4",
		"another host's Announcement for our address":                      "RFC 5227 2.4",
		"an ARP Reply for our address with a zero sender hardware address": "RFC 5227 2.4",
		// The two frames the host's own stack emits are absent here too, and
		// were absent in round 1: this arm always carried the clause. That the
		// two maps now agree about them is the point — the SAME frame cannot
		// be our own traffic after the window and a squatter inside it.
	}
	expected := map[ACDPhase]map[string]string{
		ACDIdle:       {},
		ACDProbing:    probeWindow,
		ACDSettling:   probeWindow,
		ACDAnnouncing: ongoing,
		ACDDefending:  ongoing,
	}

	for _, phase := range AllACDPhases() {
		want := expected[phase]
		if want == nil {
			t.Fatalf("no expectation written for phase %s; a phase was added without extending this table", phase)
		}
		for _, c := range adversarialARP() {
			t.Run(phase.String()+"/"+c.name, func(t *testing.T) {
				a := newACD(DefaultACDParams(), ConflictWait, testCHAddr)
				a.phase = phase
				if phase != ACDIdle {
					a.addr = netip.MustParseAddr(testACDAddr)
				}
				rule, why := a.conflictRule(c.pkt)
				if c.pkt == nil {
					// conflictRule is not called with nil by arp(), which
					// guards; asked directly it must still be total.
					return
				}
				if got := want[c.name]; got != rule {
					t.Fatalf("rule = %q (%s), want %q", rule, why, got)
				}
			})
		}
	}
}

// TestTheRelevanceFilterCannotHideAConflict is defeat row M6-13.
//
// The manager pre-filters ARP frames so that ordinary link traffic does not
// wrap the bounded journal (R3). A filter NARROWER than the conflict predicate
// would make the whole mechanism silently smaller: the conflict simply never
// arrives, and every other test still passes because every other test feeds
// the machine directly.
//
// So this runs one against the other over the whole corpus and every phase.
func TestTheRelevanceFilterCannotHideAConflict(t *testing.T) {
	admitted := 0
	for _, phase := range AllACDPhases() {
		if phase == ACDIdle {
			continue // nothing is being watched, so nothing can conflict
		}
		for _, c := range adversarialARP() {
			if c.pkt == nil {
				continue
			}
			a := newACD(DefaultACDParams(), ConflictWait, testCHAddr)
			a.phase = phase
			a.addr = netip.MustParseAddr(testACDAddr)
			rule, why := a.conflictRule(c.pkt)
			if rule == "" {
				continue
			}
			if !a.relevant(c.pkt) {
				t.Errorf("%s in %s: the rules call it a conflict (%s: %s) and the relevance filter drops it, so it would never reach them",
					c.name, phase, rule, why)
			}
			admitted++
		}
	}
	if admitted == 0 {
		t.Fatal("no packet in the corpus was a conflict in any phase; the check compared two empty sets")
	}
	// The other direction is a PROPERTY, not a requirement: the filter is
	// allowed to be wider. What it must not do is admit everything, or it is
	// not a filter and the journal argument it exists for is void.
	a := newACD(DefaultACDParams(), ConflictWait, testCHAddr)
	a.phase = ACDDefending
	a.addr = netip.MustParseAddr(testACDAddr)
	if a.relevant(replyFrom(theirMAC, "192.168.99.77", "192.168.99.9")) {
		t.Error("the filter admits ARP about an address we neither hold nor probe; the bounded journal would wrap on ordinary link traffic")
	}
}

// TestOurOwnProbesDoNotTripTheProbeWindow drives the echo AF_PACKET actually
// delivers, defeat row M6-3.
//
// An AF_PACKET socket receives this host's own outgoing frames. During the
// probe window our own Probes come back, and section 2.1.1's FIRST rule has no
// hardware-address exemption — so the only thing that saves us is that a Probe
// carries an all-zero sender IP. If a future edit ever puts a real sender
// address in a Probe (the 1.x defect, design section 8.4), this test is what
// fails, and it fails as "the client can never acquire".
func TestOurOwnProbesDoNotTripTheProbeWindow(t *testing.T) {
	m, acts := acdMachine(t, ConflictWait)
	delay, _ := armedACD(acts)
	now := at(2)
	var probesSeen int
	for step := 0; step < 12; step++ {
		now = now.Add(delay)
		_, acts = m.Step(now, uint64(step+1), TimerFired(TimerACD))
		for _, p := range arpSends(acts) {
			// Feed every frame we sent straight back, which is what the
			// kernel does.
			probesSeen++
			_, echo := m.Step(now, 1, ARPReceived(p))
			if n := count(echo, ActSend); n != 0 {
				t.Fatalf("our own %s echoed back produced %d DHCP message(s): the client is declining its own address", p, n)
			}
		}
		var ok bool
		delay, ok = armedACD(acts)
		if !ok {
			break
		}
	}
	if probesSeen == 0 {
		t.Fatal("nothing was sent, so nothing was echoed; the check was vacuous")
	}
	if m.State() != StateBound {
		t.Fatalf("the machine ended in %s, want BOUND: it did not survive the echo of its own frames", m.State())
	}
}

// ------------------------------------------------------- the consequence --

// TestAConflictInTheProbeWindowDeclinesAndReportsNoLoss is defeat row M6-7 and
// the brief's "say what IS emitted so the chassis can count it".
func TestAConflictInTheProbeWindowDeclinesAndReportsNoLoss(t *testing.T) {
	m, acts := acdMachine(t, ConflictWait)
	delay, _ := armedACD(acts)
	// One probe out, then the squatter answers.
	_, acts = m.Step(at(2).Add(delay), 1, TimerFired(TimerACD))
	if len(arpSends(acts)) != 1 {
		t.Fatalf("the first timer produced %d ARP packets, want 1", len(arpSends(acts)))
	}
	_, acts = m.Step(at(3), 2, ARPReceived(replyFrom(theirMAC, testACDAddr, "192.168.99.9")))

	msg := mustSend(t, acts, wire.MsgDecline)
	got, ok := msg.Addr4(wire.OptRequestedIP)
	if !ok || got.String() != testACDAddr {
		t.Errorf("the DHCPDECLINE names %s (present %v), want the probed address %s", got, ok, testACDAddr)
	}
	if n := count(acts, ActLeaseLost); n != 0 {
		t.Errorf("%d ActLeaseLost after a conflict in the probe window; nothing was ever acquired", n)
	}
	found := false
	for _, a := range acts {
		if a.Kind == ActFailed && a.Reason == ReasonConflict {
			found = true
		}
	}
	if !found {
		t.Error("no ActFailed{ReasonConflict}: a conflict before use would be visible only in the journal, and a counter cannot be derived from that")
	}
	if m.State() != StateInit {
		t.Errorf("after the DHCPDECLINE the machine is %s, want INIT (RFC 2131 3.2(3))", m.State())
	}
	if !hasTimer(acts, TimerRestart) {
		t.Error("no restart timer armed: RFC 2131 3.1(5) requires a wait before restarting")
	}
	if m.ACDPhase() != ACDIdle {
		t.Errorf("the ACD phase after a conflict is %s, want idle", m.ACDPhase())
	}
}

// TestAConflictAfterBoundIsSection24sPath is the other half: a lease that WAS
// announced, so the caller has configured the address and must be told.
func TestAConflictAfterBoundIsSection24sPath(t *testing.T) {
	for _, mode := range []ConflictMode{ConflictWait, ConflictAsync} {
		t.Run(mode.String(), func(t *testing.T) {
			m, acts := acdMachine(t, mode)
			// Run the schedule out so the lease is held and the sub-machine
			// is in section 2.4's ongoing phase.
			delay, armed := armedACD(acts)
			for step := 0; armed && step < 12; step++ {
				_, acts = m.Step(at(2).Add(delay), uint64(step+1), TimerFired(TimerACD))
				delay, armed = armedACD(acts)
			}
			if m.State() != StateBound {
				t.Fatalf("fixture is %s, want BOUND", m.State())
			}
			if got := m.ACDPhase(); got != ACDDefending {
				t.Fatalf("ACD phase is %s, want defending — section 2.4's listener must still be live", got)
			}

			_, acts = m.Step(at(100), 9, ARPReceived(replyFrom(theirMAC, testACDAddr, "192.168.99.9")))
			mustSend(t, acts, wire.MsgDecline)
			if n := count(acts, ActLeaseLost); n != 1 {
				t.Errorf("%d ActLeaseLost, want exactly 1: the caller configured this address and must be told", n)
			}
			for _, a := range acts {
				if a.Kind == ActLeaseLost && a.Reason != ReasonConflict {
					t.Errorf("ActLeaseLost carries %s, want conflict (seam row G-5)", a.Reason)
				}
				if a.Kind == ActFailed && a.Reason == ReasonConflict {
					t.Error("both ActLeaseLost and ActFailed carry the conflict; ring 2 would count it twice")
				}
			}
		})
	}
}

// TestTheOngoingListenerOutlivesTheAcquisition is defeat row M6-5.
//
// The failure it drives is the one that only appears when a conflict happens
// LATER, which on a healthy link is never — so it is invisible for the life of
// every lease that does not conflict.
func TestTheOngoingListenerOutlivesTheAcquisition(t *testing.T) {
	m, acts := acdMachine(t, ConflictWait)
	delay, armed := armedACD(acts)
	for step := 0; armed && step < 12; step++ {
		_, acts = m.Step(at(2).Add(delay), uint64(step+1), TimerFired(TimerACD))
		delay, armed = armedACD(acts)
	}
	if !m.ARPRelevant(replyFrom(theirMAC, testACDAddr, "192.168.99.9")) {
		t.Fatal("after the acquisition the client no longer subscribes to ARP for its own address: RFC 5227 2.4 is 'an ongoing process that is in effect for as long as a host is using an address'")
	}
	// Through a renewal too: a renewal does not pause section 2.4.
	t1, ok := m.lease.RenewAt()
	if !ok {
		t.Fatal("the fixture's lease has no T1")
	}
	m.Step(t1, 4, TimerFired(TimerRenew))
	if m.State() != StateRenewing {
		t.Fatalf("fixture is %s, want RENEWING", m.State())
	}
	_, acts = m.Step(t1.Add(1*Second), 5, ARPReceived(replyFrom(theirMAC, testACDAddr, "192.168.99.9")))
	mustSend(t, acts, wire.MsgDecline)
	if count(acts, ActLeaseLost) != 1 {
		t.Error("a conflict during RENEWING did not cost the lease")
	}
}

// ------------------------------------------------------- the rate limit --

// TestTheRateLimitEngagesAtTheTenth is defeat row M6-10, and it drives the
// boundary in both directions rather than asserting one side.
//
// RFC 5227 section 2.1.1: "if the host experiences MAX_CONFLICTS or MORE
// address conflicts on a given interface, then the host MUST limit the rate at
// which it probes for new addresses on this interface to no more than one
// attempted new address per RATE_LIMIT_INTERVAL." Or more — so the tenth is
// the first one that engages it, not the eleventh.
func TestTheRateLimitEngagesAtTheTenth(t *testing.T) {
	p := DefaultACDParams()
	for n := 1; n <= 12; n++ {
		a := newACD(p, ConflictWait, testCHAddr)
		a.conflicts = n
		a.attemptAt = at(100)
		got := a.restartDelay(at(100), DefaultRestartDelay)
		want := DefaultRestartDelay
		if n >= p.MaxConflicts {
			want = p.RateLimitInterval
		}
		if got != want {
			t.Errorf("after %d conflicts the next attempt waits %s, want %s (MAX_CONFLICTS = %d)",
				n, got, want, p.MaxConflicts)
		}
	}
	// It is measured from the START OF THE LAST ATTEMPT, not from the conflict
	// that ended it, which is what "one attempted new address per
	// RATE_LIMIT_INTERVAL" says. A probe that ran for 20 seconds before
	// failing leaves 40, not 60.
	a := newACD(p, ConflictWait, testCHAddr)
	a.conflicts = p.MaxConflicts
	a.attemptAt = at(100)
	if got := a.restartDelay(at(120), DefaultRestartDelay); got != 40*Second {
		t.Errorf("20s into the interval the wait is %s, want 40s: the rate is measured from the attempt, not from the conflict", got)
	}
	// And it never undercuts RFC 2131 section 3.1(5)'s floor.
	if got := a.restartDelay(at(200), DefaultRestartDelay); got != DefaultRestartDelay {
		t.Errorf("past the interval the wait is %s, want RFC 2131 3.1(5)'s ten-second floor", got)
	}
}

// TestTheRateLimitIsPerClientNotPerParent is D5 and defeat row M6-9.
//
// "A given interface" is ambiguous on a macvlan parent shared by many
// endpoints, and the RFC does not disambiguate for that topology. The
// maintainer's decision is per endpoint. This drives two machines, because on
// a one-client fixture the two readings are indistinguishable — and every
// existing test in this package has one client.
func TestTheRateLimitIsPerClientNotPerParent(t *testing.T) {
	loser := newMachine(t, acdParams(ConflictWait))
	neighbour := newMachine(t, acdParams(ConflictWait))

	p := loser.params.acd()
	for i := 0; i < p.MaxConflicts; i++ {
		loser.acd.conflicts++
	}
	if !loser.acd.rateLimited() {
		t.Fatal("the fixture did not reach the rate limit")
	}
	if neighbour.acd.rateLimited() {
		t.Fatal("a second client on the same parent is rate limited by the first one's conflicts; D5 says the state is per endpoint")
	}
	if neighbour.acd.conflicts != 0 {
		t.Fatalf("the second client has %d conflicts, want 0: the count is shared", neighbour.acd.conflicts)
	}
}

// TestTheConflictCountSurvivesAnAcquisition holds the property the rate limit
// is FOR.
//
// Section 2.1.1's limit exists for "a defective DHCP server that repeatedly
// assigns the same address to every host that asks for one" — a loop in which
// each attempt looks like a fresh start. A count reset by a successful probe,
// or by returning to idle, would make the limit unreachable in exactly that
// case.
func TestTheConflictCountSurvivesAnAcquisition(t *testing.T) {
	a := newACD(DefaultACDParams(), ConflictWait, testCHAddr)
	a.start(at(0), 1, netip.MustParseAddr(testACDAddr), nil)
	a.arp(replyFrom(theirMAC, testACDAddr, "192.168.99.9"), nil)
	if a.conflicts != 1 {
		t.Fatalf("conflicts = %d after one, want 1", a.conflicts)
	}
	if a.phase != ACDIdle {
		t.Fatalf("phase after a conflict is %s, want idle", a.phase)
	}
	a.start(at(100), 1, netip.MustParseAddr("192.168.99.51"), nil)
	if a.conflicts != 1 {
		t.Errorf("conflicts = %d after starting a new attempt, want 1: a loop is a run of attempts, and forgetting between them makes the limit unreachable", a.conflicts)
	}
	a.stop()
	if a.conflicts != 1 {
		t.Errorf("conflicts = %d after stop, want 1", a.conflicts)
	}
}

// --------------------------------------------------------- arm (a) --

// TestArmAIsTakenSoTheAddressIsNeverDefended drives the ABSENCE.
//
// RFC 5227 section 2.4 offers three responses to a conflict on an address in
// use. This client takes (a): "a host MAY elect to immediately cease using the
// address, and signal an error to the configuring agent". Arms (b) and (c)
// defend the address with a broadcast ARP Announcement, rate-limited by
// DEFEND_INTERVAL.
//
// Nothing in this library reads DefendInterval, and the way to prove a
// behaviour is absent is to look for what it would produce: a conflict must
// produce a DHCPDECLINE and NO ARP packet at all. A defending implementation
// would emit one here and every other assertion in this file would still pass.
func TestArmAIsTakenSoTheAddressIsNeverDefended(t *testing.T) {
	m, acts := acdMachine(t, ConflictWait)
	delay, armed := armedACD(acts)
	for step := 0; armed && step < 12; step++ {
		_, acts = m.Step(at(2).Add(delay), uint64(step+1), TimerFired(TimerACD))
		delay, armed = armedACD(acts)
	}
	if m.ACDPhase() != ACDDefending {
		t.Fatalf("fixture is in %s, want the ongoing phase", m.ACDPhase())
	}
	_, acts = m.Step(at(100), 9, ARPReceived(replyFrom(theirMAC, testACDAddr, "192.168.99.9")))
	if n := len(arpSends(acts)); n != 0 {
		t.Errorf("the conflict produced %d ARP packet(s); arm (a) ceases using the address and never defends it", n)
	}
	mustSend(t, acts, wire.MsgDecline)
	// Nothing re-arms the ACD timer either: a defending client would keep
	// DEFEND_INTERVAL running.
	if _, still := armedACD(acts); still {
		t.Error("the ACD timer was re-armed after a conflict; nothing is left to schedule once the address is given up")
	}
}

// ------------------------------------------------------- the triggers --

// TestARenewalOnTheSameAddressDoesNotReProbe is defeat row M6-15, and RFC 5227
// section 2.1's "A host MUST NOT perform this check periodically as a matter
// of course."
//
// Its opposite, M6-16, is the row below: the two are one test in two arms
// because writing them apart is what produces `if renewal { skip }`.
func TestARenewalOnTheSameAddressDoesNotReProbe(t *testing.T) {
	cases := []struct {
		name       string
		renewAddr  string
		wantProbes bool
	}{
		{"the same address", testACDAddr, false},
		{"a different address", "192.168.99.51", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, acts := acdMachine(t, ConflictWait)
			delay, armed := armedACD(acts)
			for step := 0; armed && step < 12; step++ {
				_, acts = m.Step(at(2).Add(delay), uint64(step+1), TimerFired(TimerACD))
				delay, armed = armedACD(acts)
			}
			if m.State() != StateBound {
				t.Fatalf("fixture is %s, want BOUND", m.State())
			}
			t1, ok := m.lease.RenewAt()
			if !ok {
				t.Fatal("the fixture's lease has no T1")
			}
			_, acts = m.Step(t1, 4, TimerFired(TimerRenew))
			req := mustSend(t, acts, wire.MsgRequest)
			_, acts = m.Step(t1.Add(1*Second), 5,
				received(t, ackFor(req, c.renewAddr, "192.168.99.1", 3600)))

			_, armedNow := armedACD(acts)
			if armedNow != c.wantProbes {
				t.Fatalf("a renewal onto %s armed the ACD timer: %v, want %v", c.renewAddr, armedNow, c.wantProbes)
			}
			if !c.wantProbes {
				if m.ACDPhase() != ACDDefending {
					t.Errorf("the renewal moved the ACD phase to %s; section 2.4's listener should be untouched", m.ACDPhase())
				}
				return
			}
			// A moved address IS section 2.1's "configured with a new
			// address" trigger, and it probes beside use because the lease was
			// announced long ago and cannot be un-announced.
			if m.State() != StateBound {
				t.Errorf("the client left BOUND to probe a renewed address; there is no 'before use' left to wait for")
			}
			if m.ACDPhase() != ACDProbing {
				t.Errorf("ACD phase = %s after a renewal onto a new address, want probing", m.ACDPhase())
			}
		})
	}
}

// ------------------------------------------------------ the plumbing --

// TestAtMostOneARPSendIsOutstanding holds the claim Machine.arpAction is built
// on: one remembered ActionID is enough because the schedule never has two ARP
// sends in flight.
//
// Asserted over the whole schedule rather than reasoned about, because if it
// ever became false the symptom would be an ARP send failure counted against
// a DHCP budget — which is silent.
func TestAtMostOneARPSendIsOutstanding(t *testing.T) {
	m, acts := acdMachine(t, ConflictWait)
	delay, armed := armedACD(acts)
	total := 0
	for step := 0; armed && step < 12; step++ {
		_, acts = m.Step(at(2).Add(delay), uint64(step+1), TimerFired(TimerACD))
		if n := len(arpSends(acts)); n > 1 {
			t.Fatalf("one step emitted %d ARP sends; a single remembered ActionID cannot name them all", n)
		} else {
			total += n
		}
		delay, armed = armedACD(acts)
	}
	if total != 5 {
		t.Fatalf("%d ARP packets over the schedule, want PROBE_NUM + ANNOUNCE_NUM = 5", total)
	}
}

// TestAFailedARPSendDoesNotCostTheLease drives the arm noteActionFailed keys on
// the ACTION rather than on the state.
//
// MaxSendFailures exists to break a machine that retransmits forever into a
// dead transport. The ACD schedule retransmits nothing, so counting its
// failures toward that budget would let five unsendable announcements drop a
// lease the server is perfectly happy with.
func TestAFailedARPSendDoesNotCostTheLease(t *testing.T) {
	m, acts := acdMachine(t, ConflictAsync)
	if count(acts, ActLeaseAcquired) != 1 {
		t.Fatal("the async fixture did not acquire on the DHCPACK")
	}
	delay, armed := armedACD(acts)
	fails := 0
	for step := 0; armed && step < 12; step++ {
		_, acts = m.Step(at(2).Add(delay), uint64(step+1), TimerFired(TimerACD))
		for _, a := range acts {
			if a.Kind != ActSendARP {
				continue
			}
			fails++
			_, f := m.Step(at(2).Add(delay), 1, ActionFailed(a.ID, "no such device"))
			if n := count(f, ActLeaseLost); n != 0 {
				t.Fatalf("a failed ARP send cost the lease after %d failures", fails)
			}
			if n := count(f, ActFailed); n != 0 {
				t.Fatalf("a failed ARP send was reported as a transport failure after %d failures", fails)
			}
		}
		delay, armed = armedACD(acts)
	}
	if fails < 5 {
		t.Fatalf("only %d ARP sends were failed; MaxSendFailures is %d and the check needs to pass it", fails, m.params.MaxSendFailures)
	}
	if _, held := m.Lease(); !held {
		t.Error("the lease was given up after five failed ARP sends")
	}
}

// TestReleaseDuringProbingGivesTheAllocationBack.
//
// The server wrote a binding when it sent the DHCPACK, so a caller that gives
// up during the probe has an address to relinquish even though it never used
// one. Staying silent would leak the binding for the whole lease time on every
// such caller — and would look exactly like the correct behaviour, because
// RFC 2131 section 4.4.6's DHCPRELEASE is never answered.
func TestReleaseDuringProbingGivesTheAllocationBack(t *testing.T) {
	m := machineIn(t, StateProbing)
	_, acts := m.Step(at(5), 1, Simple(EvRelease))
	msg := mustSend(t, acts, wire.MsgRelease)
	if got := msg.CIAddr.String(); got != testACDAddr {
		t.Errorf("the DHCPRELEASE names ciaddr %s, want the probed address %s", got, testACDAddr)
	}
	if n := count(acts, ActLeaseLost); n != 0 {
		t.Errorf("%d ActLeaseLost on a release during probing; no lease was ever announced", n)
	}
	if m.State() != StateStopped {
		t.Errorf("after the release the machine is %s, want STOPPED", m.State())
	}
}

// TestLinkDownDuringProbingDeclinesNothing.
//
// A link that went down is evidence about the link, not about the address, and
// RFC 2131 section 4.4.1 makes the DHCPDECLINE the answer to an address in
// use. Declining here would tell the server an address is unusable on the
// strength of no evidence at all.
func TestLinkDownDuringProbingDeclinesNothing(t *testing.T) {
	m := machineIn(t, StateProbing)
	_, acts := m.Step(at(5), 1, Simple(EvLinkDown))
	for _, a := range acts {
		if a.Kind == ActSend {
			t.Fatalf("a link-down during probing sent %s", a.Msg.Summary())
		}
	}
	if m.State() != StateInit {
		t.Errorf("after a link-down the machine is %s, want INIT", m.State())
	}
	if m.ACDPhase() != ACDIdle {
		t.Errorf("the ACD phase is %s after a link-down, want idle", m.ACDPhase())
	}
}

// hasTimer reports whether the list arms t.
func hasTimer(acts []Action, t TimerID) bool {
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == t {
			return true
		}
	}
	return false
}

// ---------------------------------- round 2, finding 1: our own traffic --

// advanceToPhase steps the machine's ACD timer until it reaches want, and
// returns the instant it got there.
//
// It drives the SCHEDULE rather than assigning a.phase, because the phase a
// frame arrives in is the whole subject here and a hand-set phase is a claim
// that the schedule can reach it.
func advanceToPhase(t *testing.T, m *Machine, acts []Action, want ACDPhase) Instant {
	t.Helper()
	now := at(2)
	delay, armed := armedACD(acts)
	for step := 0; step < 12; step++ {
		if m.ACDPhase() == want {
			return now
		}
		if !armed {
			break
		}
		now = now.Add(delay)
		_, acts = m.Step(now, uint64(step+7), TimerFired(TimerACD))
		delay, armed = armedACD(acts)
	}
	if m.ACDPhase() != want {
		t.Fatalf("the schedule did not reach %s; it stopped in %s", want, m.ACDPhase())
	}
	return now
}

// TestOurOwnTrafficInTheProbeWindowIsNeverAConflict is round 2's finding 1,
// and defeat rows M6-28, M6-29 and M6-30.
//
// THE PRODUCT IS (probing, settling) x (three packet classes) x (three modes),
// driven through Machine.Step rather than through conflictRule, because the
// defect the reviewer found was not in the predicate's table — that table
// asserted the wrong answer and passed — but in what the machine DOES with it:
// a DHCPDECLINE for the client's own lease.
//
// The three classes are the three the finding names:
//
//   - OUR sender hardware address carrying the LEASED sender IP. Two frames,
//     because the host's stack emits two: section 2.5's mandatory ARP Reply,
//     and an ordinary ARP Request for a neighbour. Never a conflict.
//   - a FOREIGN sender hardware address carrying the leased sender IP. Still a
//     conflict, in every mode that looks — that is what the exemption must not
//     cost, and it is the assertion that fails if the exemption is widened to
//     the sender IP alone.
//   - OUR sender hardware address probing for a DIFFERENT address. Never a
//     conflict, and it is here so that "our own MAC" cannot become the whole
//     rule.
//
// ConflictOff is in the product and its expectation is the same for all three
// classes — nothing — because an off client has no sub-machine to consult. It
// is driven anyway: an off client that declined on a frame would be a
// milestone-wide failure and no other test in this file feeds it one in a
// state where it holds a lease.
func TestOurOwnTrafficInTheProbeWindowIsNeverAConflict(t *testing.T) {
	classes := []struct {
		name       string
		pkt        *wire.ARPPacket
		isConflict bool
	}{
		{
			name: "the kernel's section 2.5 ARP Reply for the leased address",
			pkt:  replyFrom(testCHAddr, testACDAddr, "192.168.99.9"),
		},
		{
			name: "our own ARP Request for a neighbour, sent from the leased address",
			pkt:  requestFrom(testCHAddr, testACDAddr, "192.168.99.222"),
		},
		{
			name:       "another host's ARP Reply claiming the leased address",
			pkt:        replyFrom(theirMAC, testACDAddr, "192.168.99.9"),
			isConflict: true,
		},
		{
			name: "our own Probe for a different address",
			pkt:  probeFrom(testCHAddr, "192.168.99.77"),
		},
	}

	// The phase axis is PER MODE and not a constant list, because an off
	// client has no probe window: it is in ACDIdle from the DHCPACK onwards.
	// Writing {probing, settling} for it and skipping the rows would report a
	// product this test did not drive.
	phasesOf := func(mode ConflictMode) []ACDPhase {
		if mode == ConflictOff {
			return []ACDPhase{ACDIdle}
		}
		return []ACDPhase{ACDProbing, ACDSettling}
	}

	for _, mode := range []ConflictMode{ConflictWait, ConflictAsync, ConflictOff} {
		for _, phase := range phasesOf(mode) {
			for _, c := range classes {
				t.Run(mode.String()+"/"+phase.String()+"/"+c.name, func(t *testing.T) {
					m, acts := acdMachine(t, mode)
					now := at(2)
					if mode != ConflictOff {
						now = advanceToPhase(t, m, acts, phase)
					} else if got := m.ACDPhase(); got != ACDIdle {
						t.Fatalf("an off client is in ACD phase %s after its DHCPACK, want idle", got)
					}
					state := m.State()
					_, got := m.Step(now.Add(Nanosecond), 99, ARPReceived(c.pkt))

					conflicted := count(got, ActLeaseLost)+count(got, ActFailed) > 0
					want := c.isConflict && mode != ConflictOff
					if conflicted != want {
						t.Fatalf("conflict = %v, want %v; the actions were %v", conflicted, want, got)
					}
					if !want {
						// The DECLINE is the damage. Nothing at all may be
						// sent: a client that declined its own lease here
						// would look, to the server, exactly like one that
						// found a squatter.
						if n := count(got, ActSend); n != 0 {
							t.Fatalf("%d DHCP message(s) sent on our own traffic; the first is %v", n, got[0])
						}
						if m.State() != state {
							t.Fatalf("the machine moved from %s to %s on its own traffic", state, m.State())
						}
						if m.ACDPhase() != phase {
							t.Fatalf("the ACD phase moved from %s to %s on our own traffic", phase, m.ACDPhase())
						}
					} else {
						msg := mustSend(t, got, wire.MsgDecline)
						if addr, ok := msg.Addr4(wire.OptRequestedIP); !ok || addr.String() != testACDAddr {
							t.Fatalf("the DHCPDECLINE names %s (present %v), want %s", addr, ok, testACDAddr)
						}
					}
				})
			}
		}
	}
}

// -------------------------------- round 2, finding 2: the rate limit acts --

// armedRestart reports the delay TimerRestart was last armed for in this
// action list. It is the OBSERVABLE the rate limit changes, and round 1 had
// no assertion on it: TestTheRateLimitEngagesAtTheTenth reads the pure
// function's return value, which is the number before it is composed.
func armedRestart(acts []Action) (Duration, bool) {
	for i := len(acts) - 1; i >= 0; i-- {
		if acts[i].Kind == ActSetTimer && acts[i].Timer == TimerRestart {
			return acts[i].After, true
		}
	}
	return 0, false
}

// TestTheACDFixtureCanSeeTheRateLimit is defeat row M6-32: the fixture
// carries its own requirement as an assertion.
//
// The reviewer's mutant survived round 1 because acdParams scaled
// RATE_LIMIT_INTERVAL to 600ns while the restart delay it composes with stayed
// at ten seconds, so `max(base, rate)` was `base` for every reachable input
// and the composition had no observable. A comment saying "do not scale this"
// is not a check; this is. It fails if the two are ever made
// indistinguishable again, whichever of them moves.
func TestTheACDFixtureCanSeeTheRateLimit(t *testing.T) {
	p := acdParams(ConflictWait)
	if p.ACD.RateLimitInterval <= p.restartDelay() {
		t.Fatalf("the fixture's RATE_LIMIT_INTERVAL is %s and its restart delay is %s: "+
			"restartDelay composes them as a maximum, so no test built on this fixture can tell "+
			"a client that applies the limit from one that does not",
			p.ACD.RateLimitInterval, p.restartDelay())
	}
}

// TestTheRateLimitIsArmedOnTheRestartTimer is round 2's finding 2 and defeat
// row M6-31: RFC 5227 section 2.1.1's rate limit is a MUST, and this is the
// observer where it ACTS.
//
// "if the host experiences MAX_CONFLICTS or more address conflicts on a given
// interface, then the host MUST limit the rate at which it probes for new
// addresses on this interface to no more than one attempted new address per
// RATE_LIMIT_INTERVAL."
//
// TWELVE REAL CONFLICTS, DRIVEN THROUGH THE WIRE, not a count assigned to a
// field. The failure mode the RFC names is "a defective DHCP server that
// repeatedly assigns the same address to every host that asks for one", which
// is a LOOP: DHCPDECLINE, restart timer, DISCOVER, OFFER, ACK, probe,
// conflict, again. Each turn of that loop moves acd.attemptAt, and the limit
// is measured from the attempt rather than from the conflict, so a test that
// set the count directly would never exercise the arithmetic that makes the
// interval a rate.
//
// The assertion is on the duration TimerRestart is ARMED for — the action a
// ring-2 caller drains — because that is the only thing that changes what the
// client does. The journal line beside it says the same number and cannot be
// the evidence: a client that journals "next attempt in 59s" and arms ten
// seconds is exactly the defect.
func TestTheRateLimitIsArmedOnTheRestartTimer(t *testing.T) {
	p := acdParams(ConflictWait)
	max := p.ACD.MaxConflicts
	if max != 10 {
		t.Fatalf("the fixture's MAX_CONFLICTS is %d; this test names the tenth and the eleventh", max)
	}

	m := newMachine(t, p)
	_, acts := m.Step(0, 1, Simple(EvStart))
	now := at(0)

	for n := 1; n <= max+2; n++ {
		if n > 1 {
			// The restart timer the previous conflict armed. Firing it is
			// what makes the next attempt an attempt.
			_, acts = m.Step(now, uint64(n)*10+1, TimerFired(TimerRestart))
		}
		disc := mustSend(t, acts, wire.MsgDiscover)
		_, acts = m.Step(now.Add(Second), uint64(n)*10+2, received(t, offerFor(disc, testACDAddr, "192.168.99.1")))
		req := mustSend(t, acts, wire.MsgRequest)
		// The attempt begins here: acd.start stamps attemptAt at the DHCPACK
		// that puts the machine into PROBING.
		attempt := now.Add(2 * Second)
		_, acts = m.Step(attempt, uint64(n)*10+3, received(t, ackFor(req, testACDAddr, "192.168.99.1", 3600)))
		if m.ACDPhase() != ACDProbing {
			t.Fatalf("conflict %d: the machine is in ACD phase %s after its DHCPACK, want probing", n, m.ACDPhase())
		}

		// The squatter answers. One second into the attempt, so that the
		// remaining interval is a number this test computes rather than reads.
		conflictAt := attempt.Add(Second)
		_, acts = m.Step(conflictAt, uint64(n)*10+4, ARPReceived(replyFrom(theirMAC, testACDAddr, "192.168.99.9")))
		mustSend(t, acts, wire.MsgDecline)

		d, ok := armedRestart(acts)
		if !ok {
			t.Fatalf("conflict %d: no restart timer was armed after the DHCPDECLINE", n)
		}
		// RFC 2131 section 3.1(5)'s floor below MAX_CONFLICTS; section
		// 2.1.1's rate above it, measured from the start of the attempt.
		want := p.restartDelay()
		if n >= max {
			want = attempt.Add(p.ACD.RateLimitInterval).Sub(conflictAt)
		}
		if d != want {
			t.Fatalf("conflict %d: TimerRestart armed for %s, want %s (MAX_CONFLICTS = %d, RATE_LIMIT_INTERVAL = %s, floor %s)",
				n, d, want, max, p.ACD.RateLimitInterval, p.restartDelay())
		}
		if n == max-1 && d != p.restartDelay() {
			t.Fatalf("the limit engaged at conflict %d, one before MAX_CONFLICTS", n)
		}
		now = conflictAt.Add(d)
	}

	// D5, at the same observable. A second client on the same parent has its
	// own count, so its own restart is the floor and not the rate. Round 1
	// asserted this on acd.rateLimited(); the boundary that matters is the
	// timer, and a rate limit keyed on the parent would arm 59s here.
	neighbour := newMachine(t, acdParams(ConflictWait))
	_, nacts := neighbour.Step(at(1000), 1, Simple(EvStart))
	disc := mustSend(t, nacts, wire.MsgDiscover)
	_, nacts = neighbour.Step(at(1001), 2, received(t, offerFor(disc, testACDAddr, "192.168.99.1")))
	req := mustSend(t, nacts, wire.MsgRequest)
	_, nacts = neighbour.Step(at(1002), 3, received(t, ackFor(req, testACDAddr, "192.168.99.1", 3600)))
	_, nacts = neighbour.Step(at(1003), 4, ARPReceived(replyFrom(theirMAC, testACDAddr, "192.168.99.9")))
	mustSend(t, nacts, wire.MsgDecline)
	d, ok := armedRestart(nacts)
	if !ok {
		t.Fatal("the second client armed no restart timer after its DHCPDECLINE")
	}
	if d != p.restartDelay() {
		t.Fatalf("a second client on the same parent waits %s after its FIRST conflict, want the %s floor: "+
			"D5 makes the rate limit per endpoint", d, p.restartDelay())
	}
}
