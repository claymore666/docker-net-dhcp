package proto

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// TestAllStates6IsEveryDeclaredState is M7a's defeat row D-1 applied to the
// new enumeration: a state added to the constant block and not to AllStates6
// SHRINKS the totality test's domain rather than failing it.
//
// Its own domain is the State6 SPACE and not the slice, which is what lets it
// see a member go missing: it walks the integers upward until String() stops
// naming one.
func TestAllStates6IsEveryDeclaredState(t *testing.T) {
	seen := map[State6]int{}
	for _, s := range AllStates6() {
		seen[s]++
	}
	for i := 0; i < 256; i++ {
		s := State6(i)
		named := !strings.HasPrefix(s.String(), "state6(")
		if named && seen[s] != 1 {
			t.Errorf("%s is a declared State6 and appears %d time(s) in AllStates6()", s, seen[s])
		}
		if !named && seen[s] != 0 {
			t.Errorf("AllStates6() carries %d, which State6.String() does not name", i)
		}
		if !named {
			// Density from zero: the first unnamed value ends the enumeration,
			// so a gap in the constant block is a failure above rather than a
			// silently shorter walk.
			for j := i; j < 256; j++ {
				if !strings.HasPrefix(State6(j).String(), "state6(") {
					t.Fatalf("state6(%d) is unnamed but state6(%d) is named: the enumeration is not dense", i, j)
				}
			}
			return
		}
	}
}

// TestStep6IsTotal drives the whole product of AllStates6 and AllEventKinds.
// R1 for the second machine: every pair yields a defined result and no panic.
func TestStep6IsTotal(t *testing.T) {
	for _, s := range AllStates6() {
		for _, k := range AllEventKinds() {
			t.Run(s.String()+"/"+k.String(), func(t *testing.T) {
				m := machine6In(t, s)
				before := m.State()
				if before != s {
					t.Fatalf("the fixture put the machine in %s, want %s", before, s)
				}
				after, acts := m.Step(at(100), 12345, Simple(k))
				if after > State6Rebinding {
					t.Fatalf("%s in %s produced the undeclared state %s", k, s, after)
				}
				for _, a := range acts {
					_ = a.String()
				}
			})
		}
	}
}

// machine6In builds a machine sitting in s.
func machine6In(t *testing.T, s State6) *Machine6 {
	t.Helper()
	p := testParams6()
	switch s {
	case State6Stopped:
		return newMachine6(t, p)
	case State6Init:
		m := newMachine6(t, p)
		m.Step(at(0), 0, Simple(EvStart))
		return m
	case State6Selecting:
		m, _ := solicit6(t, p)
		return m
	case State6Requesting:
		m, _ := solicit6(t, p)
		m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
		return m
	case State6Confirming:
		p.Resume = &Resume6{
			Addrs:      []Addr6{{Addr: netip.MustParseAddr(dnsmasqLeasedAddr), Preferred: 300 * Second, Valid: 300 * Second}},
			ServerDUID: testServerDUID,
		}
		m := newMachine6(t, p)
		m.Step(at(0), 0, Simple(EvStart))
		m.Step(at(1), capXIDSolicit, TimerFired(Timer6Delay))
		return m
	case State6InfoRequesting:
		m, _ := solicit6(t, p)
		m.Step(at(2), 5, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
		return m
	case State6DAD:
		m, _ := solicit6(t, p)
		m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
		m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))
		return m
	case State6Bound:
		return bind6(t, p, dnsmasqLeasedAddr)
	case State6Renewing:
		m := bind6(t, p, dnsmasqLeasedAddr)
		m.Step(at(200), 7, TimerFired(Timer6Renew))
		return m
	case State6Rebinding:
		m := bind6(t, p, dnsmasqLeasedAddr)
		m.Step(at(200), 7, TimerFired(Timer6Rebind))
		return m
	}
	t.Fatalf("no fixture for %s", s)
	return nil
}

// raOtherOnly is a Router Advertisement with M clear and O set: RFC 4861
// §4.2's "other configuration information is available via DHCPv6" — and no
// addresses, because M is what would have offered those.
var raOtherOnly = []byte{
	134, 0, 0, 0,
	64, 0x40, 0x07, 0x08,
	0, 0, 0, 0,
	0, 0, 0, 0,
}

// raManaged has M set.
var raManaged = []byte{
	134, 0, 0, 0,
	64, 0x80, 0x07, 0x08,
	0, 0, 0, 0,
	0, 0, 0, 0,
}

// raQuiet has neither.
var raQuiet = []byte{
	134, 0, 0, 0,
	64, 0x00, 0x07, 0x08,
	0, 0, 0, 0,
	0, 0, 0, 0,
}

// TestEveryPathToIdleCancelsEveryTimer6 is the v4 test of the same name, for
// the second machine: "holding nothing" and "nothing armed" must be the same
// state, and cancelAll is what makes them so.
func TestEveryPathToIdleCancelsEveryTimer6(t *testing.T) {
	for _, s := range AllStates6() {
		if s == State6Stopped {
			continue
		}
		for _, ev := range []Event{Simple(EvStop), Simple(EvLinkDown)} {
			t.Run(s.String()+"/"+ev.Kind.String(), func(t *testing.T) {
				m := machine6In(t, s)
				after, acts := m.Step(at(500), 3, ev)
				if after != State6Stopped {
					t.Fatalf("%s in %s left the machine in %s", ev.Kind, s, after)
				}
				for _, id := range AllTimerIDs() {
					if !timerCancelled(acts, id) {
						t.Errorf("%s in %s did not cancel %s; a machine that holds nothing must have nothing armed", ev.Kind, s, id)
					}
				}
			})
		}
	}
}

// TestTheGoldenPathIsDrivenByDnsmasqsOwnBytes is design §A.4's Trap 2 in the
// form a pure machine can take.
//
// The machine is configured to BE the client that produced the capture — same
// DUID, same IAID — and fed the exact octets dnsmasq put on the wire. Every
// value asserted below is one dnsmasq printed in its own log or was given on
// its own command line, so nothing here is this package agreeing with itself.
func TestTheGoldenPathIsDrivenByDnsmasqsOwnBytes(t *testing.T) {
	m, sol := solicit6(t, testParams6())

	// The transaction id is the one the capture used, because the rnd fed to
	// the Step that opened the exchange was chosen to produce it. That is what
	// makes the captured Advertise admissible at all: §16.3 lists, among the
	// conditions on which a client discards an Advertise, "the
	// 'transaction-id' field value does not match the value the client used
	// in its Solicit message."
	if uint64(sol.XID) != capXIDSolicit {
		t.Fatalf("the Solicit's xid is %06x, want the capture's %06x", sol.XID, capXIDSolicit)
	}

	// The IA_NA the machine built carries the capture's IAID.
	capSol, err := wire.DecodeV6(capSolicit6)
	if err != nil {
		t.Fatalf("DecodeV6(capSolicit6): %v", err)
	}
	wantIA, err := capSol.Options.IANAs()
	if err != nil || len(wantIA) != 1 {
		t.Fatalf("the captured Solicit's IA_NA: %v %v", wantIA, err)
	}
	gotIA, err := sol.Options.IANAs()
	if err != nil || len(gotIA) != 1 {
		t.Fatalf("our Solicit's IA_NA: %v %v", gotIA, err)
	}
	if gotIA[0].IAID != wantIA[0].IAID {
		t.Errorf("IAID %d, the capture sent %d", gotIA[0].IAID, wantIA[0].IAID)
	}
	if gotIA[0].T1 != 0 || gotIA[0].T2 != 0 {
		t.Errorf("the Solicit's IA_NA carries T1=%d T2=%d; the capture sent zeros and a client has no times to propose", gotIA[0].T1, gotIA[0].T2)
	}

	// THE ORO IS WHERE THIS MACHINE AND THE CAPTURE'S CLIENT DIFFER, ON
	// PURPOSE. The capture's client asked for 23 and 24 only; §21.24 says "A
	// DHCP client MUST include the SOL_MAX_RT option code in any Option
	// Request option (see Section 21.7) it sends in a Solicit message", and
	// this machine does. Asserting byte equality with the capture would pin
	// the omission as correct.
	wantCodes, _ := capSol.Options.First(wire.OptV6ORO)
	gotCodes, ok := sol.Options.First(wire.OptV6ORO)
	if !ok {
		t.Fatal("the Solicit carries no Option Request option (§18.2.1 makes it a MUST)")
	}
	for _, c := range decodeORO(wantCodes) {
		if !containsCode(decodeORO(gotCodes), c) {
			t.Errorf("the Solicit's ORO does not ask for %s, which the capture's did", c)
		}
	}
	if !containsCode(decodeORO(gotCodes), wire.OptV6SolMaxRTCode) {
		t.Error("the Solicit's ORO does not ask for SOL_MAX_RT; §21.24 makes it a MUST and the capture's client predates it")
	}

	// The captured Advertise: preference 255, which dnsmasq's own log line
	// records as "sent size: 1 option: 7 preference 255".
	adv, err := wire.DecodeV6(capAdvertise6)
	if err != nil {
		t.Fatalf("DecodeV6(capAdvertise6): %v", err)
	}
	s, acts := m.Step(at(2), capXIDRequest, ReceivedV6(adv, capAdvertise6))
	if s != State6Requesting {
		t.Fatalf("the captured Advertise left the machine in %s, want %s", s, State6Requesting)
	}
	req := mustSendV6(t, acts, wire.MsgRequest6)
	if uint64(req.XID) != capXIDRequest {
		t.Errorf("the Request's xid is %06x, the capture's was %06x", req.XID, capXIDRequest)
	}
	if _, ok := req.Options.First(wire.OptV6ServerID); !ok {
		t.Error("the Request carries no Server Identifier; §18.2.2 makes it a MUST")
	}

	// The captured Reply.
	rep, err := wire.DecodeV6(capReply6)
	if err != nil {
		t.Fatalf("DecodeV6(capReply6): %v", err)
	}
	s, acts = m.Step(at(3), 0, ReceivedV6(rep, capReply6))
	if s != State6DAD {
		t.Fatalf("the captured Reply left the machine in %s, want %s — §18.2.10.1 requires duplicate address detection before the address is used", s, State6DAD)
	}
	dad, ok := find(acts, ActStartDAD)
	if !ok {
		t.Fatal("the Reply produced no ActStartDAD")
	}
	if dad.Target.String() != dnsmasqLeasedAddr {
		t.Errorf("duplicate address detection was asked for %s; dnsmasq logged DHCPREPLY %s", dad.Target, dnsmasqLeasedAddr)
	}

	s, acts = m.Step(at(4), 0, DADResult(netip.MustParseAddr(dnsmasqLeasedAddr), false))
	if s != State6Bound {
		t.Fatalf("a clean DAD result left the machine in %s", s)
	}
	acq, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatal("no lease was acquired")
	}
	l := acq.Lease6
	if len(l.Addrs) != 1 || l.Addrs[0].Addr.String() != dnsmasqLeasedAddr {
		t.Fatalf("acquired %v; dnsmasq logged DHCPREPLY %s", l.Addrs, dnsmasqLeasedAddr)
	}
	if want := Duration(dnsmasqLifetimeSeconds) * Second; l.Addrs[0].Preferred != want || l.Addrs[0].Valid != want {
		t.Errorf("lifetimes preferred=%s valid=%s; dnsmasq was run with --dhcp-range ...,%d",
			l.Addrs[0].Preferred, l.Addrs[0].Valid, dnsmasqLifetimeSeconds)
	}
	if len(l.DNS) != 1 || l.DNS[0].String() != dnsmasqDNS {
		t.Errorf("DNS %v; dnsmasq was run with --dhcp-option=option6:dns-server,[%s]", l.DNS, dnsmasqDNS)
	}
	if len(l.Search) != 1 || l.Search[0] != dnsmasqSearch {
		t.Errorf("search list %v; dnsmasq was run with --dhcp-option=option6:domain-search,%s", l.Search, dnsmasqSearch)
	}
	// T1 and T2 as the SERVER chose them, read back off its own octets.
	if l.T1 != 150*Second || l.T2 != 259*Second {
		t.Errorf("T1=%s T2=%s; the captured IA_NA carries 150 and 259 seconds", l.T1, l.T2)
	}
	// And the timers the machine armed follow the server's values rather than
	// §21.4's recommendation, which is only used when the server sends zero.
	//
	// The interval is T1 counted from the moment the REQUEST went out (t+2s)
	// and not from the Reply or from the bind. That is this client's
	// DELIBERATE DEVIATION from §4.2, which defines T1 as "interpreted as a
	// time interval since the message's reception" — Lease6.Start states the
	// deviation and its bound. The assertion below is what pins the deviation
	// in place: the machine sat two seconds in duplicate address detection,
	// and an implementation that had quietly switched to the reception origin
	// would arm 150s rather than 148s here.
	if d, ok := timerSet(acts, Timer6Renew); !ok || d != 150*Second-2*Second {
		t.Errorf("the renew timer was armed for %s, want %s: T1 is 150s from the Request at t+2s and the bind is at t+4s", d, 148*Second)
	}
	if d, ok := timerSet(acts, Timer6Rebind); !ok || d != 259*Second-2*Second {
		t.Errorf("the rebind timer was armed for %s, want %s", d, 257*Second)
	}
}

func decodeORO(v []byte) []wire.OptionCodeV6 {
	var out []wire.OptionCodeV6
	for i := 0; i+1 < len(v); i += 2 {
		out = append(out, wire.OptionCodeV6(uint16(v[i])<<8|uint16(v[i+1])))
	}
	return out
}

// TestNoAcquiredBeforeDuplicateAddressDetection is D22's shape at the v6
// layer, and the first defeat-list row: an Acquired emitted on the Reply is a
// client using an address nothing has checked.
//
// §18.2.10.1: "The client MUST perform duplicate address detection as per
// Section 5.4 of [RFC4862] ... The client performs the duplicate address
// detection before using the received addresses for any traffic."
func TestNoAcquiredBeforeDuplicateAddressDetection(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	s, acts := m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))
	if s != State6DAD {
		t.Fatalf("the Reply left the machine in %s", s)
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatal("the Reply produced ActLeaseAcquired before any duplicate address detection result")
	}
	if _, held := m.Lease(); held {
		t.Fatal("Lease() reports a held lease while duplicate address detection is still running")
	}
	if _, ok := timerSet(acts, Timer6DAD); !ok {
		t.Error("no deadline was armed for the duplicate address detection result; a result that never arrives would leave the machine here forever")
	}
}

// TestADuplicateDeclinesAndRestarts is §18.2.10.1's "If any of the addresses
// are found to be in use on the link, the client sends a Decline message to
// the server for those addresses as described in Section 18.2.8."
//
// OVER BOTH ORIGINS. The resumed row is the reviewer's scenario (a) — a
// Resume6 with a ServerDUID, a Confirm answered Success, then a duplicate —
// and it is the row that fails against a machine whose server identity is set
// only on the Solicit path.
func TestADuplicateDeclinesAndRestarts(t *testing.T) {
	for _, o := range leaseOrigins6() {
		t.Run(o.name, func(t *testing.T) {
			m := o.dad(t, testParams6())

			s, acts := m.Step(at(4), 9, DADResult(netip.MustParseAddr(dnsmasqLeasedAddr), true))
			if s != State6DAD {
				t.Fatalf("a duplicate left the machine in %s; the Decline exchange runs from %s", s, State6DAD)
			}
			dec := mustSendV6(t, acts, wire.MsgDecline6)
			sid, ok := dec.Options.First(wire.OptV6ServerID)
			if !ok {
				t.Fatal("the Decline carries no Server Identifier; §18.2.8 makes it a MUST")
			}
			if string(sid) != string(testServerDUID) {
				t.Errorf("the Decline names the server %x, want %x: §18.2.8 wants \"the server that allocated the lease(s)\"", sid, testServerDUID)
			}
			ias, err := dec.Options.IANAs()
			if err != nil || len(ias) != 1 {
				t.Fatalf("the Decline's IA_NA: %v %v", ias, err)
			}
			addrs, err := ias[0].Options.Addrs()
			if err != nil || len(addrs) != 1 || addrs[0].Addr.String() != dnsmasqLeasedAddr {
				t.Fatalf("the Decline names %v, want the duplicate address %s", addrs, dnsmasqLeasedAddr)
			}
			if _, ok := find(acts, ActLeaseAcquired); ok {
				t.Fatal("a duplicate produced a lease")
			}

			// The Reply to the Decline completes it and discovery restarts.
			s, acts = m.Step(at(5), 0, receivedV6(t, wire.MsgReply, dec.XID,
				optClientID(capDUID), optServerID(testServerDUID)))
			if s != State6Init {
				t.Fatalf("the Reply to the Decline left the machine in %s, want %s (discovery restarts)", s, State6Init)
			}
			if _, ok := timerSet(acts, Timer6Delay); !ok {
				t.Error("discovery restarted without the §18.2.1 pre-transmission delay")
			}
		})
	}
}

// TestADuplicateOnAFreshAcquisitionIsReportedAndNotOnlyJournalled is the
// counterpart of TestTheDADDeadlineIsAFaultAndNotAnAcquisition, and it exists
// because the two arms disagreed.
//
// A deadline with no result already produced ActFailed{ReasonDADIncomplete}.
// A result that said "another node has this address" produced a Decline, a
// restart, and NOTHING the caller could see — so the more informative outcome
// was the silent one, and ring 2's ConflictsDetected and DADConflicts stayed
// at zero through a conflict that actually happened. Measured against real
// dnsmasq with a second holder on the link (runtime's
// TestADuplicateAddressOnTheLinkIsDeclined) before it was fixed here.
//
// machine_acd.go's acdConflict arm is the v4 statement of the same rule, and
// its comment is the argument: a conflict "visible only in the journal ... is
// not something a counter can be derived from".
func TestADuplicateOnAFreshAcquisitionIsReportedAndNotOnlyJournalled(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))

	_, acts := m.Step(at(4), 9, DADResult(netip.MustParseAddr(dnsmasqLeasedAddr), true))

	f, ok := find(acts, ActFailed)
	if !ok {
		t.Fatal("a duplicate on an address that was never bound produced no ActFailed: the conflict reached the caller only as a journal line")
	}
	if f.Reason != ReasonConflict {
		t.Errorf("reason %s, want %s: somebody else answered for the address, which is not the same as nobody answering at all", f.Reason, ReasonConflict)
	}
	if f.Note == "" {
		t.Error("the ActFailed carries no note; the journal line it is derived from names how many addresses of the IA were in use")
	}
	// AND NOT BOTH. Ring 2 adds ActLeaseLost{ReasonConflict} and
	// ActFailed{ReasonConflict} into one ConflictsDetected, so a machine that
	// emitted both here would count this conflict twice.
	if _, ok := find(acts, ActLeaseLost); ok {
		t.Error("the same conflict produced ActLeaseLost as well as ActFailed; ring 2 counts both, so this conflict would be counted twice")
	}
}

// TestADuplicateUnderAHeldLeaseReportsTheLossAndNotAFailure is the other half
// of the exclusivity above, driven from the state where a lease IS held.
//
// §18.2.6-era renewals can move a client onto an address it did not have, and
// freshAddrs runs the check on exactly those. The caller must be told the
// lease is gone — which is an ActLeaseLost — and must NOT also be told an
// acquisition failed, because ring 2 adds the two.
func TestADuplicateUnderAHeldLeaseReportsTheLossAndNotAFailure(t *testing.T) {
	m := bind6(t, testParams6(), dnsmasqLeasedAddr)
	_, acts := m.Step(at(100), 9, DADResult(netip.MustParseAddr(dnsmasqLeasedAddr), true))

	l, ok := find(acts, ActLeaseLost)
	if !ok {
		t.Fatal("a duplicate under a bound lease produced no ActLeaseLost")
	}
	if l.Reason != ReasonConflict {
		t.Errorf("ActLeaseLost reason %s, want %s", l.Reason, ReasonConflict)
	}
	if _, ok := find(acts, ActFailed); ok {
		t.Error("the same conflict produced ActFailed as well as ActLeaseLost; ring 2 counts both, so this conflict would be counted twice")
	}
}

// TestTheDeclineGivesUpAtDecMaxRC is §18.2.8's "MRC: DEC_MAX_RC".
func TestTheDeclineGivesUpAtDecMaxRC(t *testing.T) {
	p := testParams6()
	m, _ := solicit6(t, p)
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))
	m.Step(at(4), 9, DADResult(netip.MustParseAddr(dnsmasqLeasedAddr), true))

	now := int64(5)
	for i := 1; i < p.DecMaxRC; i++ {
		now++
		s, acts := m.Step(at(now), 3, TimerFired(Timer6Retransmit))
		if s != State6DAD {
			t.Fatalf("retransmission %d left the machine in %s", i, s)
		}
		if !hasSendV6(acts, wire.MsgDecline6) {
			t.Fatalf("retransmission %d sent no Decline", i)
		}
	}
	now++
	s, acts := m.Step(at(now), 3, TimerFired(Timer6Retransmit))
	if hasSendV6(acts, wire.MsgDecline6) {
		t.Errorf("a %d'th Decline went out; §18.2.8 gives DEC_MAX_RC as %d", p.DecMaxRC+1, p.DecMaxRC)
	}
	if s != State6Init {
		t.Errorf("DEC_MAX_RC left the machine in %s, want discovery restarted at %s", s, State6Init)
	}
}

// TestTheDADDeadlineIsAFaultAndNotAnAcquisition is lead ruling 1: "A deadline
// without a result is a fault (ActFailed + journal + restart discovery), never
// an Acquired".
func TestTheDADDeadlineIsAFaultAndNotAnAcquisition(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))

	s, acts := m.Step(at(30), 4, TimerFired(Timer6DAD))
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatal("a duplicate address detection deadline with no result produced a lease")
	}
	f, ok := find(acts, ActFailed)
	if !ok {
		t.Fatal("the deadline produced no ActFailed")
	}
	if f.Reason != ReasonDADIncomplete {
		t.Errorf("reason %s, want %s: nobody answered, which is not the same as somebody else holding the address", f.Reason, ReasonDADIncomplete)
	}
	if hasSendV6(acts, wire.MsgDecline6) {
		t.Error("a Decline went out on evidence nobody produced")
	}
	if s != State6Init {
		t.Errorf("the deadline left the machine in %s, want discovery restarted", s)
	}
}

// TestADADResultForAnAddressWeNeverAskedAbout is a D17 row: ring 3 runs
// duplicate address detection for the chassis too.
func TestADADResultForAnAddressWeNeverAskedAbout(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))

	s, acts := m.Step(at(4), 0, DADResult(netip.MustParseAddr("fd00:99::999"), true))
	if s != State6DAD {
		t.Fatalf("a result for another address moved the machine to %s", s)
	}
	if hasSendV6(acts, wire.MsgDecline6) {
		t.Error("a result for an address this machine never asked about produced a Decline")
	}
	if !journalHas(acts, "did not ask about") {
		t.Errorf("the ignored result was not journalled: %v", acts)
	}
	// And the real result still binds.
	if s, _ := m.Step(at(5), 0, DADResult(netip.MustParseAddr(dnsmasqLeasedAddr), false)); s != State6Bound {
		t.Errorf("the awaited result left the machine in %s", s)
	}
}

// TestDADPhaseTracksTheCheck is the field ring 2 persists, driven through the
// whole check rather than read off a fresh machine.
//
// It is DERIVED from the machine's state rather than stored, which is what
// this test holds: a second field tracking the same fact is a second thing to
// forget to update, and the way that defect shows is a phase that stops moving
// on one path.
func TestDADPhaseTracksTheCheck(t *testing.T) {
	p := testParams6()
	m, _ := solicit6(t, p)
	if got := m.DADPhase(); got != DADIdle {
		t.Errorf("a soliciting machine reports %s, want %s", got, DADIdle)
	}
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	if got := m.DADPhase(); got != DADIdle {
		t.Errorf("a requesting machine reports %s, want %s", got, DADIdle)
	}
	m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))
	if got := m.DADPhase(); got != DADTentative {
		t.Errorf("a machine waiting for a result reports %s, want %s (RFC 4862 §5.4.1)", got, DADTentative)
	}
	m.Step(at(4), 0, DADResult(addr6(dnsmasqLeasedAddr), false))
	if got := m.DADPhase(); got != DADPassed {
		t.Errorf("a bound machine reports %s, want %s (RFC 4862 §5.4.5)", got, DADPassed)
	}

	// The other ending.
	m2, _ := solicit6(t, p)
	m2.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	m2.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))
	m2.Step(at(4), 9, DADResult(addr6(dnsmasqLeasedAddr), true))
	if got := m2.DADPhase(); got != DADFailed {
		t.Errorf("a machine declining a duplicate reports %s, want %s", got, DADFailed)
	}
}

// TestAllDADPhasesIsEveryDeclaredPhase is D-1 applied to the third
// enumeration this round adds.
func TestAllDADPhasesIsEveryDeclaredPhase(t *testing.T) {
	seen := map[DADPhase]int{}
	for _, p := range AllDADPhases() {
		seen[p]++
	}
	for i := 0; i < 256; i++ {
		p := DADPhase(i)
		named := !strings.HasPrefix(p.String(), "dad-phase(")
		if named && seen[p] != 1 {
			t.Errorf("%s is a declared DADPhase and appears %d time(s) in AllDADPhases()", p, seen[p])
		}
		if !named {
			if seen[p] != 0 {
				t.Errorf("AllDADPhases() carries %d, which DADPhase.String() does not name", i)
			}
			return
		}
	}
}
