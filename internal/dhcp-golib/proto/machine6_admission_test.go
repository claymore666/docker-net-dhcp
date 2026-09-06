package proto

import (
	"fmt"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// TestTheAdmissionGateNamesEveryDiscard is §16.3 and §16.10 as a table.
//
// EVERY ROW ASSERTS THE JOURNAL LINE AND NOT ONLY THE ABSENCE OF A
// TRANSITION, because a discard is invisible from outside: a machine that
// dropped a message for the wrong reason and one that dropped it for the right
// one both stay in SELECTING and both send nothing. The line is the only thing
// that separates them, which is sequencing §2.6's rule applied to this gate.
func TestTheAdmissionGateNamesEveryDiscard(t *testing.T) {
	good := func() []wire.OptionV6 {
		return []wire.OptionV6{
			optClientID(capDUID), optServerID(testServerDUID),
			optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}}),
		}
	}
	for _, tc := range []struct {
		name string
		xid  uint32
		opts []wire.OptionV6
		want string
	}{
		{
			"a transaction id from another exchange",
			0x111111,
			good(),
			"does not match the outstanding",
		},
		{
			"no Server Identifier",
			uint32(capXIDSolicit),
			[]wire.OptionV6{optClientID(capDUID),
				optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}})},
			"carries no Server Identifier option",
		},
		{
			"two Server Identifiers",
			uint32(capXIDSolicit),
			append(good(), optServerID(mustHexBytes("0001000100000000deadbeef00"))),
			"Server Identifier options: discarded, the sender is ambiguous",
		},
		{
			"no Client Identifier",
			uint32(capXIDSolicit),
			[]wire.OptionV6{optServerID(testServerDUID),
				optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}})},
			"carries no Client Identifier option",
		},
		{
			"another client's DUID",
			uint32(capXIDSolicit),
			[]wire.OptionV6{optClientID(mustHexBytes("00030001aabbccddeeff")), optServerID(testServerDUID),
				optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}})},
			"Client Identifier that is not ours",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := solicit6(t, testParams6())
			s, acts := m.Step(at(2), capXIDRequest,
				receivedV6(t, wire.MsgAdvertise, tc.xid, tc.opts...))
			if s != State6Selecting {
				t.Fatalf("the machine moved to %s on a message §16.3 makes a MUST-discard", s)
			}
			if hasSendV6(acts, wire.MsgRequest6) {
				t.Fatal("a Request went out on a discarded Advertise")
			}
			if !journalHas(acts, tc.want) {
				t.Errorf("the discard was not journalled as %q; the machine said:%s", tc.want, journalLines(acts))
			}
		})
	}

	// The preservation control: the same message with none of the defects is
	// admitted, so the gate is not simply refusing everything.
	t.Run("the control", func(t *testing.T) {
		m, _ := solicit6(t, testParams6())
		s, acts := m.Step(at(2), capXIDRequest,
			receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit), append(good(), optPreference(255))...))
		if s != State6Requesting {
			t.Fatalf("a valid Advertise left the machine in %s", s)
		}
		if !hasSendV6(acts, wire.MsgRequest6) {
			t.Fatal("a valid Advertise produced no Request")
		}
	})
}

// TestAMessageWithNoExchangeInFlightIsDiscarded is the arm §16 does not spell
// out and the machine needs anyway: a Reply that arrives while BOUND matches
// no transaction id because there is none.
func TestAMessageWithNoExchangeInFlightIsDiscarded(t *testing.T) {
	m := bind6(t, testParams6(), dnsmasqLeasedAddr)
	s, acts := m.Step(at(10), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))
	if s != State6Bound {
		t.Fatalf("an unsolicited Reply moved a bound machine to %s", s)
	}
	if !journalHas(acts, "no exchange in flight") {
		t.Errorf("the discard was not journalled: %v", acts)
	}
}

// TestSolMaxRTIsAppliedFromADiscardedAdvertise is §18.2.9's MUST, and the
// reason it is a separate test from the selection rows: the option is
// processed "even if the message contains a Status Code option ... indicating
// a failure, and the Advertise message will be discarded by the client."
//
// A client that applied the back-off only where it kept the message would take
// instruction from the servers that were working and ignore it from the one
// telling it to slow down.
func TestSolMaxRTIsAppliedFromADiscardedAdvertise(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	before := m.Params().SolMaxRT

	s, acts := m.Step(at(2), 3, receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit),
		optClientID(capDUID), optServerID(testServerDUID),
		optStatus(wire.StatusNoAddrsAvail),
		optU32(wire.OptV6SolMaxRTCode, 900)))
	if s != State6Selecting {
		t.Fatalf("an Advertise saying NoAddrsAvail moved the machine to %s", s)
	}
	if hasSendV6(acts, wire.MsgRequest6) {
		t.Fatal("a Request went out to a server that said NoAddrsAvail")
	}
	if got := m.Params().SolMaxRT; got != 900*Second {
		t.Errorf("SOL_MAX_RT is %s, want 900s: it was %s and the discarded Advertise carried 900 (§18.2.9)", got, before)
	}
	if !journalHas(acts, "ignored for selection") {
		t.Errorf("the discard was not journalled: %v", acts)
	}
}

// TestSolMaxRTOutsideItsRangeIsIgnored is §21.24: "A DHCP client MUST ignore
// any SOL_MAX_RT option values that are less than 60 or more than 86400."
func TestSolMaxRTOutsideItsRangeIsIgnored(t *testing.T) {
	for _, secs := range []uint32{0, 59, 86401, 0xffffffff} {
		t.Run(fmt.Sprint(secs), func(t *testing.T) {
			m, _ := solicit6(t, testParams6())
			before := m.Params().SolMaxRT
			_, acts := m.Step(at(2), 3, receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit),
				optClientID(capDUID), optServerID(testServerDUID),
				optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}}),
				optU32(wire.OptV6SolMaxRTCode, secs)))
			if got := m.Params().SolMaxRT; got != before {
				t.Errorf("SOL_MAX_RT moved to %s on the out-of-range value %d", got, secs)
			}
			if !journalHas(acts, "outside 60..86400") {
				t.Errorf("the ignored value was not journalled: %v", acts)
			}
		})
	}
}

// TestEveryMessageCarriesTheClientIdentifierAndElapsedTime walks the eight
// message types this machine sends.
//
// §18.2.1 through §18.2.8 each repeat the same two MUSTs, and eight
// per-message assertions in eight tests is how one of them comes to be
// missing. The domain here is the set of messages the machine actually
// produced on a driven path, so a message type that stops being sent fails the
// count rather than quietly leaving the table.
func TestEveryMessageCarriesTheClientIdentifierAndElapsedTime(t *testing.T) {
	sent := map[wire.MessageTypeV6]*wire.MessageV6{}
	collect := func(acts []Action) {
		for _, a := range acts {
			if a.Kind == ActSendV6 && a.MsgV6 != nil {
				if _, ok := sent[a.MsgV6.Type]; !ok {
					sent[a.MsgV6.Type] = a.MsgV6
				}
			}
		}
	}

	// Solicit, Request, Reply-driven DAD, Decline.
	p := testParams6()
	m, sol := solicit6(t, p)
	sent[wire.MsgSolicit] = sol
	_, acts := m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	collect(acts)
	_, acts = m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr))
	collect(acts)
	_, acts = m.Step(at(4), 9, DADResult(addr6(dnsmasqLeasedAddr), true))
	collect(acts)

	// Renew, Rebind, Release from a bound machine.
	m2 := bind6(t, p, dnsmasqLeasedAddr)
	_, acts = m2.Step(at(200), 7, TimerFired(Timer6Renew))
	collect(acts)
	_, acts = m2.Step(at(300), 7, TimerFired(Timer6Rebind))
	collect(acts)
	m3 := bind6(t, p, dnsmasqLeasedAddr)
	_, acts = m3.Step(at(200), 7, Simple(EvRelease))
	collect(acts)

	// Confirm and Information-request.
	m4, _ := solicit6(t, p)
	_, acts = m4.Step(at(2), 3, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
	collect(acts)
	_, confActs := confirming6(t, p)
	collect(confActs)

	want := []wire.MessageTypeV6{
		wire.MsgSolicit, wire.MsgRequest6, wire.MsgConfirm, wire.MsgRenew,
		wire.MsgRebind, wire.MsgRelease6, wire.MsgDecline6, wire.MsgInformationRequest,
	}
	for _, typ := range want {
		msg, ok := sent[typ]
		if !ok {
			t.Errorf("no %s was produced on any driven path: the table below never checked it", typ)
			continue
		}
		cid, ok := msg.Options.First(wire.OptV6ClientID)
		if !ok {
			t.Errorf("the %s carries no Client Identifier option (§21.2, a MUST in every §18.2 subsection)", typ)
		} else if !sameDUID(cid, capDUID) {
			t.Errorf("the %s carries the Client Identifier %x, want %x", typ, cid, capDUID)
		}
		if _, ok := msg.Options.First(wire.OptV6ElapsedTime); !ok {
			t.Errorf("the %s carries no Elapsed Time option (§21.9, a MUST in every §18.2 subsection)", typ)
		}
	}
	if len(want) != 8 {
		t.Fatalf("the table lists %d message types, want the eight this client sends", len(want))
	}
}

// TestTheServerIdentifierIsPresentWhereItIsRequiredAndAbsentWhereItIsForbidden
// pairs §18.2.4's MUST with §18.2.5's MUST NOT, which is the one place in this
// protocol where the same message built two ways differs by exactly one option.
func TestTheServerIdentifierIsPresentWhereItIsRequiredAndAbsentWhereItIsForbidden(t *testing.T) {
	p := testParams6()

	m := bind6(t, p, dnsmasqLeasedAddr)
	_, acts := m.Step(at(200), 7, TimerFired(Timer6Renew))
	renew := mustSendV6(t, acts, wire.MsgRenew)
	sid, ok := renew.Options.First(wire.OptV6ServerID)
	if !ok {
		t.Fatal("the Renew carries no Server Identifier; §18.2.4: \"The client MUST include a Server Identifier option ... identifying the server with which the client most recently communicated\"")
	}
	if !sameDUID(sid, testServerDUID) {
		t.Errorf("the Renew names the server %x, want %x", sid, testServerDUID)
	}

	_, acts = m.Step(at(300), 7, TimerFired(Timer6Rebind))
	rebind := mustSendV6(t, acts, wire.MsgRebind)
	if _, ok := rebind.Options.First(wire.OptV6ServerID); ok {
		t.Error("the Rebind carries a Server Identifier; §18.2.5: \"The client does not include the Server Identifier option ... in the Rebind message\"")
	}
	// The rest of the message is the Renew's, which is what §18.2.5 says it is.
	if _, ok := rebind.Options.First(wire.OptV6IANA); !ok {
		t.Error("the Rebind carries no IA_NA")
	}
	if _, ok := rebind.Options.First(wire.OptV6ClientID); !ok {
		t.Error("the Rebind carries no Client Identifier")
	}
}

// TestARenewWithNoServerDUIDRebindsInstead drives the arm §18.2.4's MUST
// leaves the machine no way through.
//
// The §18.2.3 Confirm carries no Server Identifier — the message is multicast
// to All_DHCP_Relay_Agents_and_Servers and §18.2.3 lists no such option — so a
// client that resumed from stored addresses alone reaches BOUND holding a
// lease that names no server. §18.2.4 then makes the Renew impossible: "The
// client MUST include a Server Identifier option (see Section 21.3) in the
// Renew message". §18.2.5's Rebind is the message that does not need one.
func TestARenewWithNoServerDUIDRebindsInstead(t *testing.T) {
	p := testParams6()
	p.Resume = &Resume6{
		Addrs: []Addr6{{Addr: addr6(dnsmasqLeasedAddr), Preferred: 300 * Second, Valid: 300 * Second}},
		T1:    150 * Second,
		T2:    240 * Second,
	}
	m := newMachine6(t, p)
	m.Step(at(0), 0, Simple(EvStart))
	s, acts := m.Step(at(1), capXIDSolicit, TimerFired(Timer6Delay))
	if s != State6Confirming {
		t.Fatalf("a live resume left the machine in %s, want %s", s, State6Confirming)
	}
	conf := mustSendV6(t, acts, wire.MsgConfirm)
	if _, ok := conf.Options.First(wire.OptV6ServerID); ok {
		t.Error("the Confirm carries a Server Identifier; §18.2.3 lists none and the message is multicast to every server on the link")
	}

	s, _ = m.Step(at(2), 0, receivedV6(t, wire.MsgReply, conf.XID,
		optClientID(capDUID), optServerID(testServerDUID), optStatus(wire.StatusSuccess)))
	if s != State6DAD {
		t.Fatalf("a confirming Reply left the machine in %s, want %s", s, State6DAD)
	}
	s, _ = m.Step(at(3), 0, DADResult(addr6(dnsmasqLeasedAddr), false))
	if s != State6Bound {
		t.Fatalf("the confirmed address did not bind: %s", s)
	}
	l, _ := m.Lease()
	if len(l.ServerDUID) != 0 {
		t.Fatalf("the resumed lease names the server %x; the Confirm exchange never learned one", l.ServerDUID)
	}

	s, acts = m.Step(at(200), 7, TimerFired(Timer6Renew))
	if s != State6Rebinding {
		t.Fatalf("T1 on a lease that names no server left the machine in %s, want %s", s, State6Rebinding)
	}
	if hasSendV6(acts, wire.MsgRenew) {
		t.Error("a Renew went out with no Server Identifier to put in it (§18.2.4)")
	}
	if !hasSendV6(acts, wire.MsgRebind) {
		t.Error("no Rebind went out")
	}
	if !journalHas(acts, "names no server") {
		t.Errorf("the substitution was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestTheORORequestsWhatEachExchangeNeeds is §21.24's and §21.25's MUSTs and
// §18.2.6's list, checked per message type rather than once.
func TestTheORORequestsWhatEachExchangeNeeds(t *testing.T) {
	p := testParams6()

	m, sol := solicit6(t, p)
	assertORO(t, sol, "Solicit", []wire.OptionCodeV6{
		wire.OptV6SolMaxRTCode, wire.OptV6DNSServers, wire.OptV6DomainList,
	})

	_, acts := m.Step(at(2), 3, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
	inf := mustSendV6(t, acts, wire.MsgInformationRequest)
	assertORO(t, inf, "Information-request", []wire.OptionCodeV6{
		wire.OptV6InfMaxRTCode, wire.OptV6InfoRefresh, wire.OptV6DNSServers, wire.OptV6DomainList,
	})

	b := bind6(t, p, dnsmasqLeasedAddr)
	_, acts = b.Step(at(200), 7, TimerFired(Timer6Renew))
	assertORO(t, mustSendV6(t, acts, wire.MsgRenew), "Renew", []wire.OptionCodeV6{
		wire.OptV6SolMaxRTCode, wire.OptV6DNSServers, wire.OptV6DomainList,
	})
}

func assertORO(t *testing.T, msg *wire.MessageV6, what string, want []wire.OptionCodeV6) {
	t.Helper()
	v, ok := msg.Options.First(wire.OptV6ORO)
	if !ok {
		t.Fatalf("the %s carries no Option Request option", what)
	}
	got := decodeORO(v)
	for _, c := range want {
		if !containsCode(got, c) {
			t.Errorf("the %s's ORO does not ask for %s; it asks for %v", what, c, got)
		}
	}
	seen := map[wire.OptionCodeV6]int{}
	for _, c := range got {
		seen[c]++
		if seen[c] > 1 {
			t.Errorf("the %s's ORO asks for %s twice", what, c)
		}
	}
}

// TestTheElapsedTimeRestartsPerExchangeAndSaturates is §21.9.
//
// "The elapsed time is measured from the time at which the client sent the
// first message in the message exchange, and the elapsed-time field is set to
// 0 in the first message in the message exchange", and the field carries "The
// amount of time since the client began its current DHCP transaction ...
// expressed in hundredths of a second (10^-2 seconds)" in two octets, so it
// runs out after 655.36 seconds and MUST NOT wrap: a client
// that wrapped would tell a server it had just started trying after eleven
// minutes of failure, which is the exact input §14's rate limiting is
// listening for.
func TestTheElapsedTimeRestartsPerExchangeAndSaturates(t *testing.T) {
	p := testParams6()
	m, sol := solicit6(t, p)
	if got := elapsed(t, sol); got != 0 {
		t.Errorf("the first Solicit's elapsed time is %d hundredths, want 0 (§21.9)", got)
	}

	// A retransmission of the SAME exchange counts from the exchange's start.
	_, acts := m.Step(at(11), 3, TimerFired(Timer6Retransmit))
	again := mustSendV6(t, acts, wire.MsgSolicit)
	if got := elapsed(t, again); got != 1000 {
		t.Errorf("a Solicit retransmitted at t+11s (the exchange began at t+1s) reports %d hundredths, want 1000", got)
	}

	// A NEW exchange starts over.
	_, acts = m.Step(at(12), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	req := mustSendV6(t, acts, wire.MsgRequest6)
	if got := elapsed(t, req); got != 0 {
		t.Errorf("the first Request of a new exchange reports %d hundredths, want 0 (§21.9)", got)
	}

	// And it saturates rather than wrapping.
	m2, _ := solicit6(t, p)
	_, acts = m2.Step(at(1+700), 3, TimerFired(Timer6Retransmit))
	late := mustSendV6(t, acts, wire.MsgSolicit)
	if got := elapsed(t, late); got != 0xffff {
		t.Errorf("a Solicit retransmitted 700s into the exchange reports %d hundredths; the field is 16 bits and 70000 does not fit, so it must saturate at 65535 rather than wrap to %d", got, 70000%65536)
	}
}

func elapsed(t *testing.T, msg *wire.MessageV6) uint16 {
	t.Helper()
	v, ok := msg.Options.First(wire.OptV6ElapsedTime)
	if !ok {
		t.Fatalf("the %s carries no Elapsed Time option", msg.Type)
	}
	if len(v) != 2 {
		t.Fatalf("the Elapsed Time option is %d octets, want 2 (§21.9)", len(v))
	}
	return uint16(v[0])<<8 | uint16(v[1])
}

// TestTheSolicitsFirstRTIsStrictlyGreaterThanIRT is §18.2.1's exception to
// §15's schedule: "Also, the first RT MUST be selected to be strictly greater
// than IRT by choosing RAND to be strictly greater than 0."
//
// The rnd values below span the range that produces a NEGATIVE randomisation
// factor, which is exactly where §15's own formula would put the first
// retransmission at or below IRT.
func TestTheSolicitsFirstRTIsStrictlyGreaterThanIRT(t *testing.T) {
	p := testParams6()
	irt := p.Solicit().IRT
	for _, rnd := range []uint64{0, 1, 7, 1 << 20, 1<<63 - 1, ^uint64(0)} {
		t.Run(fmt.Sprint(rnd), func(t *testing.T) {
			m := newMachine6(t, p)
			m.Step(at(0), 0, Simple(EvStart))
			_, acts := m.Step(at(1), rnd, TimerFired(Timer6Delay))
			d, ok := timerSet(acts, Timer6Retransmit)
			if !ok {
				t.Fatal("no retransmission timer was armed for the Solicit")
			}
			if d <= irt {
				t.Errorf("the first RT is %s and IRT is %s; §18.2.1 makes it strictly greater", d, irt)
			}
		})
	}
}

// TestTheFirstSolicitIsDelayed is §18.2.1's "The first Solicit message from the
// client on the interface SHOULD be delayed by a random amount of time between
// 0 and SOL_MAX_DELAY."
//
// The delay is drawn from the rnd Step was handed, which is what makes a replay
// reproduce it; the assertion is on the RANGE and on the fact that the Solicit
// has not gone out yet.
func TestTheFirstSolicitIsDelayed(t *testing.T) {
	p := testParams6()
	for _, rnd := range []uint64{0, 1, 500, 999999, ^uint64(0)} {
		t.Run(fmt.Sprint(rnd), func(t *testing.T) {
			m := newMachine6(t, p)
			s, acts := m.Step(at(0), rnd, Simple(EvStart))
			if s != State6Init {
				t.Fatalf("EvStart left the machine in %s", s)
			}
			if hasSendV6(acts, wire.MsgSolicit) {
				t.Fatal("the Solicit went out at EvStart with no delay (§18.2.1)")
			}
			d, ok := timerSet(acts, Timer6Delay)
			if !ok {
				t.Fatal("no delay was armed")
			}
			if d < 0 || d > p.SolMaxDelay {
				t.Errorf("the delay is %s, outside [0, SOL_MAX_DELAY=%s]", d, p.SolMaxDelay)
			}
		})
	}
}

// journalLines is every journal line in acts, for a failure message that shows
// what the machine actually said.
func journalLines(acts []Action) string {
	var b strings.Builder
	for _, a := range acts {
		if a.Kind == ActJournal {
			b.WriteString("\n\t" + a.Note)
		}
	}
	return b.String()
}

// TestAnAdvertiseSayingNoAddrsAvailIsNotSelected isolates §18.2.9's Status Code
// check from the addressless check standing next to it.
//
// THE FIXTURE IS THE WHOLE TEST, and it is why this is not another row in
// TestSolMaxRTIsAppliedFromADiscardedAdvertise. That test's Advertise carries no
// IA_NA at all, so it is discarded TWICE over — once for the status and once for
// offering nothing — and deleting the status check leaves it discarded by the
// second guard with the suite still green. MEASURED: replacing
// `st.Code != wire.StatusSuccess` with `false` SURVIVED the suite until this
// test existed. The Advertise below carries a usable IA_NA and preference 255,
// so the status is the only thing between it and an immediate Request.
//
// §18.2.9 states the discard inside the exception it grants to it: the client
// MUST process a SOL_MAX_RT option "even if the message contains a Status Code
// option (see Section 21.13) indicating a failure, and the Advertise message
// will be discarded by the client."
func TestAnAdvertiseSayingNoAddrsAvailIsNotSelected(t *testing.T) {
	usable := []wire.OptionV6{
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}}),
		optPreference(255),
		optU32(wire.OptV6SolMaxRTCode, 7200),
	}

	m, _ := solicit6(t, testParams6())
	s, acts := m.Step(at(2), capXIDRequest, receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit),
		append(append([]wire.OptionV6(nil), usable...), optStatus(wire.StatusNoAddrsAvail))...))
	if s != State6Selecting {
		t.Fatalf("an Advertise carrying an address, preference 255 and NoAddrsAvail moved the machine to %s: §18.2.9 says such a message \"will be discarded by the client\"", s)
	}
	if hasSendV6(acts, wire.MsgRequest6) {
		t.Fatal("a Request went out to a server that said NoAddrsAvail: the address it offered beside the failure was taken as an offer")
	}
	if got := m.Params().SolMaxRT; got != 7200*Second {
		t.Errorf("SOL_MAX_RT is %s, want 7200s: the discarded Advertise carried 7200 (§18.2.9, §21.24)", got)
	}

	// The preservation control. The same message WITHOUT the Status Code is
	// selected immediately, so what refused it above is the status and not some
	// other property of the fixture.
	m2, _ := solicit6(t, testParams6())
	if s, _ := m2.Step(at(2), capXIDRequest, receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit), usable...)); s != State6Requesting {
		t.Fatalf("the same Advertise with no Status Code left the machine in %s, want %s: the discard above was the fixture, not the status", s, State6Requesting)
	}
}
