package proto

import (
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// TestTheSolicitDoesNotWaitForARouterAdvertisement is design §A.3.3
// interlock 1, and the reason it is the first router test rather than a
// footnote to them: a client that waited for M=1 would be silent on exactly
// the links where a DHCPv6 server exists and the router has not been told.
func TestTheSolicitDoesNotWaitForARouterAdvertisement(t *testing.T) {
	m := newMachine6(t, testParams6())
	s, acts := m.Step(at(0), 500, Simple(EvStart))
	if s != State6Init {
		t.Fatalf("EvStart left the machine in %s", s)
	}
	if _, ok := find(acts, ActSendRouterSolicit); !ok {
		t.Error("EvStart sent no Router Solicitation (RFC 4861 §6.3.7)")
	}
	if _, ok := timerSet(acts, Timer6Delay); !ok {
		t.Fatal("EvStart armed no delay for the first Solicit")
	}

	// And the delay expiring sends the Solicit with no router seen at all.
	s, acts = m.Step(at(1), capXIDSolicit, TimerFired(Timer6Delay))
	if s != State6Selecting {
		t.Fatalf("the delay expiring left the machine in %s", s)
	}
	if !hasSendV6(acts, wire.MsgSolicit) {
		t.Fatal("no Solicit went out on a link with no Router Advertisement")
	}
	if m.Router().Seen {
		t.Error("the machine reports a router observation with no Router Advertisement received")
	}
}

// TestTheRouterSolicitationScheduleStopsAtTheFirstAdvertisement is RFC 4861
// §6.3.7: "To obtain Router Advertisements quickly, a host SHOULD transmit up
// to MAX_RTR_SOLICITATIONS Router Solicitation messages, each separated by at
// least RTR_SOLICITATION_INTERVAL seconds."
//
// "To obtain Router Advertisements quickly" is the whole reason the remaining
// solicitations are cancelled once one arrives: they would be asking a
// question that has been answered.
func TestTheRouterSolicitationScheduleStopsAtTheFirstAdvertisement(t *testing.T) {
	p := testParams6()
	m := newMachine6(t, p)
	_, acts := m.Step(at(0), 500, Simple(EvStart))
	if _, ok := find(acts, ActSendRouterSolicit); !ok {
		t.Fatal("no first Router Solicitation")
	}
	d, ok := timerSet(acts, Timer6RouterSolicit)
	if !ok {
		t.Fatal("no next Router Solicitation was armed")
	}
	if d != RtrSolicitationInterval {
		t.Errorf("the interval is %s, want RTR_SOLICITATION_INTERVAL %s (RFC 4861 §10)", d, RtrSolicitationInterval)
	}

	sent := 1
	for i := 1; i < MaxRtrSolicitations; i++ {
		_, acts = m.Step(at(int64(4*i)), 3, TimerFired(Timer6RouterSolicit))
		if _, ok := find(acts, ActSendRouterSolicit); !ok {
			t.Fatalf("solicitation %d was not sent", i+1)
		}
		sent++
	}
	if sent != MaxRtrSolicitations {
		t.Fatalf("sent %d solicitations, want MAX_RTR_SOLICITATIONS %d", sent, MaxRtrSolicitations)
	}
	// The last one arms nothing further.
	if !timerCancelled(acts, Timer6RouterSolicit) {
		t.Error("the last solicitation left the timer armed; §6.3.7 bounds the count at MAX_RTR_SOLICITATIONS")
	}
	_, acts = m.Step(at(20), 3, TimerFired(Timer6RouterSolicit))
	if _, ok := find(acts, ActSendRouterSolicit); ok {
		t.Errorf("a %d'th Router Solicitation went out; MAX_RTR_SOLICITATIONS is %d", MaxRtrSolicitations+1, MaxRtrSolicitations)
	}

	// A machine that hears a router stops early.
	m2 := newMachine6(t, p)
	m2.Step(at(0), 500, Simple(EvStart))
	_, acts = m2.Step(at(1), 3, RouterAdvertRaw(mustRA(t, raManaged), raManaged))
	if !timerCancelled(acts, Timer6RouterSolicit) {
		t.Error("a Router Advertisement did not cancel the remaining solicitations")
	}
	_, acts = m2.Step(at(4), 3, TimerFired(Timer6RouterSolicit))
	if _, ok := find(acts, ActSendRouterSolicit); ok {
		t.Error("a solicitation went out after a Router Advertisement had already answered")
	}
}

// TestEveryRouterAdvertisementIsReportedInEveryState is lead ruling 7 and
// design Q2: ActRouterObserved is a diagnostic, so a caller that waited out its
// own deadline on a link whose router says there is no DHCPv6 here can tell
// which of the two happened.
//
// THE DOMAIN IS AllStates6, not a hand-picked list, for M7a's D-1 reason: a
// state added later must not be able to drop the diagnostic silently.
func TestEveryRouterAdvertisementIsReportedInEveryState(t *testing.T) {
	for _, st := range AllStates6() {
		for _, ra := range []struct {
			name string
			raw  []byte
		}{
			{"M=1", raManaged}, {"M=0 O=1", raOtherOnly}, {"neither", raQuiet},
		} {
			t.Run(st.String()+"/"+ra.name, func(t *testing.T) {
				m := machine6In(t, st)
				_, acts := m.Step(at(600), 3, RouterAdvertRaw(mustRA(t, ra.raw), ra.raw))
				obs, ok := find(acts, ActRouterObserved)
				if !ok {
					t.Fatalf("a Router Advertisement in %s produced no ActRouterObserved", st)
				}
				want := mustRA(t, ra.raw)
				if obs.Router.Managed != want.Managed || obs.Router.Other != want.Other {
					t.Errorf("reported M=%t O=%t, the advertisement carried M=%t O=%t",
						obs.Router.Managed, obs.Router.Other, want.Managed, want.Other)
				}
				if !m.Router().Seen {
					t.Error("the machine does not remember having seen a router")
				}
			})
		}
	}
}

// TestMZeroOOneWhileSolicitingSwitchesToInformationRequest is design §A.3.3
// interlock 1's one transition.
//
// RFC 4861 §4.2 defines the flags; a link whose router says M=0 O=1 is one
// where "no addresses are available via DHCPv6" and the Solicit in flight will
// never be answered, so continuing to retransmit it is the client spending
// §14.1's budget on a message the network has already answered.
func TestMZeroOOneWhileSolicitingSwitchesToInformationRequest(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	s, acts := m.Step(at(2), 3, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
	if s != State6InfoRequesting {
		t.Fatalf("M=0 O=1 while soliciting left the machine in %s, want %s", s, State6InfoRequesting)
	}
	inf := mustSendV6(t, acts, wire.MsgInformationRequest)
	if _, ok := inf.Options.First(wire.OptV6IANA); ok {
		t.Error("the Information-request carries an IA_NA; §16: \"an IA option is not allowed to appear in an Information-request message\"")
	}
	if !timerCancelled(acts, Timer6Retransmit) || !hasSendV6(acts, wire.MsgInformationRequest) {
		// The retransmit timer is re-armed for the new exchange, so the
		// assertion that matters is that the Solicit stopped.
		if hasSendV6(acts, wire.MsgSolicit) {
			t.Error("a Solicit went out after the router said no addresses are available via DHCPv6")
		}
	}
	if !journalHas(acts, "switching to Information-request") {
		t.Errorf("the switch was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestMOneDoesNotDisturbTheExchangeInFlight is the other half of interlock 1.
func TestMOneDoesNotDisturbTheExchangeInFlight(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	s, acts := m.Step(at(2), 3, RouterAdvertRaw(mustRA(t, raManaged), raManaged))
	if s != State6Selecting {
		t.Fatalf("M=1 while soliciting moved the machine to %s", s)
	}
	for _, a := range acts {
		if a.Kind == ActSendV6 {
			t.Errorf("M=1 produced a %s; the Solicit already in flight is what M asks for", a.MsgV6.Type)
		}
	}
}

// TestNeitherFlagDoesNotStopTheClient is RFC 4861 §4.2's "If neither M nor O
// flags are set, this indicates that no information is available via DHCPv6."
//
// The client keeps going anyway, and the reason is written into the journal
// line: a router that has not been told about the DHCPv6 server is a
// configuration this client cannot verify from the outside, and the caller's
// own deadline is what ends the attempt.
func TestNeitherFlagDoesNotStopTheClient(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	s, acts := m.Step(at(2), 3, RouterAdvertRaw(mustRA(t, raQuiet), raQuiet))
	if s != State6Selecting {
		t.Fatalf("M=0 O=0 moved the machine to %s; nothing in this library ends the attempt but the caller", s)
	}
	if _, ok := find(acts, ActLeaseLost); ok {
		t.Error("M=0 O=0 reported a lease lost")
	}
	if _, ok := find(acts, ActFailed); ok {
		t.Error("M=0 O=0 reported a failure; it is a diagnostic and the caller decides")
	}
	if !journalHas(acts, "no information is available via DHCPv6") {
		t.Errorf("the observation was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestANilRouterAdvertisementIsIgnored is the shape ring 3 can hand up when a
// decode failed and its error was dropped.
func TestANilRouterAdvertisementIsIgnored(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	s, acts := m.Step(at(2), 3, Event{Kind: EvRouterAdvert})
	if s != State6Selecting {
		t.Fatalf("a nil Router Advertisement moved the machine to %s", s)
	}
	if _, ok := find(acts, ActRouterObserved); ok {
		t.Error("a nil Router Advertisement produced an observation")
	}
	if m.Router().Seen {
		t.Error("a nil Router Advertisement counted as having seen a router")
	}
	if !journalHas(acts, "nil Router Advertisement") {
		t.Errorf("it was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestOnlySolicitingSwitchesOnMZeroOOne is the half of design §A.3.3 interlock 1
// that TestEveryRouterAdvertisementIsReportedInEveryState cannot see.
//
// That test asserts the DIAGNOSTIC is produced in every state, and that stays
// true of a machine which also abandons whatever exchange was in flight.
// MEASURED: dropping `&& m.state == State6Selecting` from the switch — so that
// every state switches to §18.2.6's stateless exchange — SURVIVED the suite
// until this test existed. A BOUND client that took a Router Advertisement as a
// reason to restart as stateless would drop a lease it holds; a REBINDING one
// would abandon the exchange that was trying to keep it.
//
// THE DOMAIN IS AllStates6, not a hand-picked list, for M7a's D-1 reason: a
// state added later must not be able to acquire the transition silently.
func TestOnlySolicitingSwitchesOnMZeroOOne(t *testing.T) {
	for _, st := range AllStates6() {
		t.Run(st.String(), func(t *testing.T) {
			m := machine6In(t, st)
			before := m.State()
			s, acts := m.Step(at(600), 3, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
			if before == State6Selecting {
				if s != State6InfoRequesting {
					t.Fatalf("M=0 O=1 while soliciting left the machine in %s, want %s", s, State6InfoRequesting)
				}
				return
			}
			if s != before {
				t.Errorf("M=0 O=1 in %s moved the machine to %s: only SELECTING6 switches (design §A.3.3 interlock 1)", before, s)
			}
			if hasSendV6(acts, wire.MsgInformationRequest) {
				t.Errorf("M=0 O=1 in %s started a stateless exchange, abandoning the one in flight", before)
			}
		})
	}
}
