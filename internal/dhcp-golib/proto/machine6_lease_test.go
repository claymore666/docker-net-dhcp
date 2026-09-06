package proto

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// TestAdvertiseSelectionPrefersTheHighestPreference is §18.2.9's normative
// half — "Those Advertise messages with the highest server preference value
// SHOULD be preferred over all other Advertise messages" — and this client's
// stated tie-break for the rest of it.
//
// THE ARRIVAL ORDER AND THE PREFERENCE ORDER ARE DELIBERATELY OPPOSED in the
// rows below, because a selector that simply kept the first Advertise and one
// that ranked them agree on every input where they are not.
func TestAdvertiseSelectionPrefersTheHighestPreference(t *testing.T) {
	duidA := mustHexBytes("0001000100000001aaaaaaaaaaaa")
	duidB := mustHexBytes("0001000100000002bbbbbbbbbbbb")
	duidC := mustHexBytes("0001000100000003cccccccccccc")

	advFrom := func(sid []byte, pref uint8, addr string) Event {
		return receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit),
			optClientID(capDUID), optServerID(sid),
			optIANA(t, capIAID, 150, 240, []iaAddrSpec{{addr, 300, 300}}),
			optPreference(pref))
	}

	for _, tc := range []struct {
		name  string
		order []Event
		want  []byte
		why   string
	}{
		{
			"the highest preference wins even when it arrived last",
			[]Event{advFrom(duidA, 10, "fd00:99::1"), advFrom(duidB, 200, "fd00:99::2")},
			duidB,
			"preference 200 beat preference 10",
		},
		{
			"the highest preference wins even when it arrived first",
			[]Event{advFrom(duidA, 200, "fd00:99::1"), advFrom(duidB, 10, "fd00:99::2")},
			duidA,
			"preference 200 beat preference 10",
		},
		{
			"a tie goes to the one that arrived first",
			[]Event{advFrom(duidA, 7, "fd00:99::1"), advFrom(duidB, 7, "fd00:99::2"), advFrom(duidC, 7, "fd00:99::3")},
			duidA,
			"this client's stated tie-break is first-arrived",
		},
		{
			"an absent Preference option is preference 0",
			[]Event{
				receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit),
					optClientID(capDUID), optServerID(duidA),
					optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::1", 300, 300}})),
				advFrom(duidB, 1, "fd00:99::2"),
			},
			duidB,
			"§21.8: \"Any valid Advertise that does not include a Preference option is considered to have a preference value of 0\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := solicit6(t, testParams6())
			now := int64(2)
			for _, ev := range tc.order {
				s, acts := m.Step(at(now), 3, ev)
				now++
				if s != State6Selecting {
					t.Fatalf("an Advertise with preference below 255 left the collection window early: %s", s)
				}
				if hasSendV6(acts, wire.MsgRequest6) {
					t.Fatal("a Request went out inside the collection window; §18.2.1 makes the client collect for the first RT")
				}
			}
			// The first RT elapsing is what ends the window.
			s, acts := m.Step(at(now), 3, TimerFired(Timer6Retransmit))
			if s != State6Requesting {
				t.Fatalf("the collection window ended in %s, want %s", s, State6Requesting)
			}
			req := mustSendV6(t, acts, wire.MsgRequest6)
			sid, ok := req.Options.First(wire.OptV6ServerID)
			if !ok {
				t.Fatal("the Request names no server")
			}
			if !sameDUID(sid, tc.want) {
				t.Errorf("the Request went to %x, want %x: %s", sid, tc.want, tc.why)
			}
		})
	}
}

// TestPreference255SkipsTheCollectionWindow is §18.2.1's one short-circuit:
// "If the client receives a valid Advertise message that includes a Preference
// option with a preference value of 255, the client immediately begins a
// client-initiated message exchange".
func TestPreference255SkipsTheCollectionWindow(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	s, acts := m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	if s != State6Requesting {
		t.Fatalf("a preference-255 Advertise left the machine in %s", s)
	}
	if !hasSendV6(acts, wire.MsgRequest6) {
		t.Fatal("no Request went out")
	}
	if !journalHas(acts, "without waiting out the collection window") {
		t.Errorf("the short-circuit was not journalled; the machine said:%s", journalLines(acts))
	}

	// 254 is the boundary on the other side and it waits.
	m2, _ := solicit6(t, testParams6())
	s, acts = m2.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 254))
	if s != State6Selecting {
		t.Fatalf("a preference-254 Advertise left the machine in %s; only 255 short-circuits (§18.2.1)", s)
	}
	if hasSendV6(acts, wire.MsgRequest6) {
		t.Fatal("a preference-254 Advertise sent a Request without waiting out the window")
	}
}

// TestAnAdvertiseAfterTheWindowIsActedOnAtOnce is the other half of §18.2.1:
// "The client terminates the retransmission process as soon as it receives any
// valid Advertise message, and the client acts on the received Advertise
// message without waiting for any additional Advertise messages."
func TestAnAdvertiseAfterTheWindowIsActedOnAtOnce(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	// The window ends with nothing collected, so the Solicit is retransmitted.
	s, acts := m.Step(at(3), 3, TimerFired(Timer6Retransmit))
	if s != State6Selecting {
		t.Fatalf("an empty collection window left the machine in %s", s)
	}
	if !hasSendV6(acts, wire.MsgSolicit) {
		t.Fatal("an empty collection window sent no retransmission")
	}
	if !journalHas(acts, "the first RT elapsed with no Advertise message") {
		t.Errorf("the empty window was not journalled; the machine said:%s", journalLines(acts))
	}

	s, acts = m.Step(at(4), capXIDRequest, advertise(t, uint32(capXIDSolicit), 1))
	if s != State6Requesting {
		t.Fatalf("an Advertise arriving after the window left the machine in %s", s)
	}
	if !hasSendV6(acts, wire.MsgRequest6) {
		t.Fatal("an Advertise arriving after the window produced no Request")
	}
}

// TestAnAdvertiseWithNoAddressIsIgnoredForSelection is §18.2.9: "The client
// MUST ignore any Advertise message that contains no addresses ... with the
// exception that the client: MUST process an included SOL_MAX_RT option".
func TestAnAdvertiseWithNoAddressIsIgnoredForSelection(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	s, acts := m.Step(at(2), 3, receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit),
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, nil),
		optPreference(255)))
	if s != State6Selecting {
		t.Fatalf("an addressless Advertise with preference 255 moved the machine to %s", s)
	}
	if hasSendV6(acts, wire.MsgRequest6) {
		t.Fatal("a Request went out for an Advertise that offered no address")
	}
	if !journalHas(acts, "offers no address") {
		t.Errorf("the ignored Advertise was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestTheIARulesDiscardWhatTheRFCSaysToDiscard is §21.4's T1/T2 rule,
// §21.6's preferred/valid rule and §18.2.10.1's zero-valid rule, driven
// through the machine rather than against readIA directly.
//
// Each row is a Reply the machine must find UNUSABLE, so the assertion is that
// no lease appears — the shape a client that read the fields without checking
// them would fail.
func TestTheIARulesDiscardWhatTheRFCSaysToDiscard(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  wire.OptionV6
		note string
		// line is the journal text this discard must produce. A discard is
		// invisible in a passing test — the lease is absent either way — so
		// the arm that did the discarding has to be named. MEASURED
		// 2026-09-06: the foreign-IAID arm's behaviour was driven and its
		// note was not, so deleting the note left the suite green.
		line string
	}{
		{
			"T1 greater than T2, both non-zero",
			optIANA(t, capIAID, 300, 200, []iaAddrSpec{{"fd00:99::183", 400, 400}}),
			"§21.4: \"If a client receives an IA_NA with T1 greater than T2 and both T1 and T2 are greater than 0, the client discards the IA_NA option and processes the remainder of the message as though the server had not included the invalid IA_NA option.\"",
			"is greater than T2 200s",
		},
		{
			"a preferred lifetime greater than the valid lifetime",
			optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 500, 300}}),
			"§21.6: \"The client MUST discard any addresses for which the preferred lifetime is greater than the valid lifetime.\"",
			"preferred 500 greater than valid 300",
		},
		{
			"a valid lifetime of zero",
			optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 0, 0}}),
			"§18.2.10.1: an address whose valid lifetime is 0 has expired and is not a binding",
			"valid lifetime",
		},
		{
			"another client's IAID",
			optIANA(t, capIAID+1, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}}),
			"the IA_NA is keyed on the IAID this client sent",
			"is not ours",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := solicit6(t, testParams6())
			m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
			s, acts := m.Step(at(3), 5, receivedV6(t, wire.MsgReply, uint32(capXIDRequest),
				optClientID(capDUID), optServerID(testServerDUID), tc.opt))
			if _, ok := find(acts, ActLeaseAcquired); ok {
				t.Fatalf("a lease was acquired from an IA the RFC says to discard.\n%s", tc.note)
			}
			if _, held := m.Lease(); held {
				t.Fatalf("the machine holds a lease built from a discarded IA.\n%s", tc.note)
			}
			if s == State6DAD {
				t.Fatalf("the machine started duplicate address detection on a discarded IA.\n%s", tc.note)
			}
			if s != State6Init && s != State6Selecting {
				t.Errorf("an unusable Reply left the machine in %s, want discovery restarted", s)
			}
			if !journalHas(acts, tc.line) {
				t.Errorf("the discard was not journalled as %q; the machine said:%s", tc.line, journalLines(acts))
			}
		})
	}

	// The preservation control: the same Reply with values inside the rules
	// binds, so the checks are not refusing every IA.
	t.Run("the control", func(t *testing.T) {
		m := bind6(t, testParams6(), dnsmasqLeasedAddr)
		l, held := m.Lease()
		if !held || len(l.Addrs) != 1 {
			t.Fatalf("the control did not bind: %v %t", l, held)
		}
		if l.T1 != 150*Second || l.T2 != 240*Second {
			t.Errorf("T1=%s T2=%s, want the values the fixture's IA_NA carries", l.T1, l.T2)
		}
	})
}

// TestATopLevelIAAddressOptionIsIgnored drives the third of ruling 5's discard
// arms, which had behaviour and no test: an IA Address option sitting at the
// top level of a Reply, outside any IA_NA.
//
// §21.6's option is "only specified to be encapsulated within an IA_NA", and
// §16 forbids throwing the message away over a misplaced option — "Clients and
// servers MAY choose to either (1) extract information from such a message if
// the information is of use to the recipient or (2) ignore such a message
// completely and just discard it". So the OPTION is dropped and the message is
// still read. An address with no IA_NA around it has no IAID, and this client
// could never renew, rebind or release it.
//
// TWO ROWS, because either one alone can pass with the arm deleted: the bare
// one would still yield no lease (there is no IA_NA to read), and the row
// beside a real IA_NA would still bind. What the arm is answerable for is that
// the top-level address is NOT TAKEN and that the drop is named.
func TestATopLevelIAAddressOptionIsIgnored(t *testing.T) {
	const stray = "fd00:99::dead"
	t.Run("alone in the Reply", func(t *testing.T) {
		m, _ := solicit6(t, testParams6())
		m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
		s, acts := m.Step(at(3), 5, receivedV6(t, wire.MsgReply, uint32(capXIDRequest),
			optClientID(capDUID), optServerID(testServerDUID),
			optIAAddrTop(t, iaAddrSpec{stray, 300, 300})))
		if _, ok := find(acts, ActLeaseAcquired); ok {
			t.Error("a lease was acquired from an IA Address option with no IA_NA around it")
		}
		if dad, ok := find(acts, ActStartDAD); ok {
			t.Errorf("duplicate address detection started on %s, which arrived outside any IA_NA", dad.Target)
		}
		if s == State6Bound || s == State6DAD {
			t.Errorf("a Reply whose only address is misplaced left the machine in %s", s)
		}
		if !journalHas(acts, "at the top level, outside any IA_NA") {
			t.Errorf("the misplaced option was not journalled; the machine said:%s", journalLines(acts))
		}
	})

	t.Run("beside our IA_NA", func(t *testing.T) {
		m, _ := solicit6(t, testParams6())
		m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
		s, acts := m.Step(at(3), 5, receivedV6(t, wire.MsgReply, uint32(capXIDRequest),
			optClientID(capDUID), optServerID(testServerDUID),
			optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}}),
			optIAAddrTop(t, iaAddrSpec{stray, 300, 300})))
		if s != State6DAD {
			t.Fatalf("the Reply left the machine in %s, want duplicate address detection on the IA_NA's address", s)
		}
		if !journalHas(acts, "at the top level, outside any IA_NA") {
			t.Errorf("the misplaced option was not journalled; the machine said:%s", journalLines(acts))
		}
		for _, a := range acts {
			if a.Kind == ActStartDAD && a.Target.String() == stray {
				t.Errorf("duplicate address detection started on %s, which arrived outside any IA_NA", stray)
			}
		}
		dad, ok := find(acts, ActStartDAD)
		if !ok {
			t.Fatal("the IA_NA's own address never reached duplicate address detection")
		}
		if dad.Target.String() != "fd00:99::183" {
			t.Errorf("duplicate address detection started on %s, want the IA_NA's address", dad.Target)
		}
	})
}

// TestASecondIANAWithOurIAIDIsIgnored is the shape a server bug produces and
// §21.4 does not describe: two IA_NA options with the same IAID.
//
// A client that merged them would hold an address the server did not intend to
// grant twice over; a client that took the last would depend on option order.
// INFERRED, and marked as such because the RFC has no sentence to quote here:
// §8 says only that "Options are stored serially in the 'options' field, with
// no padding between the options", and no section of RFC 9915 gives the order
// of two options of the same code a meaning. Depending on it would be
// depending on something the specification does not define, so this client
// takes the first and journals the second.
func TestASecondIANAWithOurIAIDIsIgnored(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	s, acts := m.Step(at(3), 5, receivedV6(t, wire.MsgReply, uint32(capXIDRequest),
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}}),
		optIANA(t, capIAID, 10, 20, []iaAddrSpec{{"fd00:99::184", 300, 300}})))
	if s != State6DAD {
		t.Fatalf("the Reply left the machine in %s", s)
	}
	if !journalHas(acts, "a second IA_NA") {
		t.Errorf("the duplicate IA_NA was not journalled; the machine said:%s", journalLines(acts))
	}
	dad, ok := find(acts, ActStartDAD)
	if !ok {
		t.Fatal("no duplicate address detection started")
	}
	if dad.Target.String() != "fd00:99::183" {
		t.Errorf("the machine took the address %s from the second IA_NA; the first is the one it keeps", dad.Target)
	}
}

// TestNoAddrsAvailTriesTheNextServer is §18.2.10.1: "If the Reply message
// contains any IAs but the client finds no usable addresses ... in any of
// these IAs, the client may either try another server (perhaps restarting the
// DHCP server discovery process) or use the Information-request message".
func TestNoAddrsAvailTriesTheNextServer(t *testing.T) {
	duidA := mustHexBytes("0001000100000001aaaaaaaaaaaa")
	duidB := mustHexBytes("0001000100000002bbbbbbbbbbbb")

	m, _ := solicit6(t, testParams6())
	for i, sid := range [][]byte{duidA, duidB} {
		s, _ := m.Step(at(int64(2+i)), 3, receivedV6(t, wire.MsgAdvertise, uint32(capXIDSolicit),
			optClientID(capDUID), optServerID(sid),
			optIANA(t, capIAID, 150, 240, []iaAddrSpec{{fmt.Sprintf("fd00:99::%d", i+1), 300, 300}}),
			optPreference(uint8(10-i))))
		if s != State6Selecting {
			t.Fatalf("Advertise %d left the machine in %s", i, s)
		}
	}
	s, acts := m.Step(at(5), 3, TimerFired(Timer6Retransmit))
	if s != State6Requesting {
		t.Fatalf("the window ended in %s", s)
	}
	req := mustSendV6(t, acts, wire.MsgRequest6)
	if sid, _ := req.Options.First(wire.OptV6ServerID); !sameDUID(sid, duidA) {
		t.Fatalf("the first Request went to %x, want the higher-preference %x", sid, duidA)
	}

	s, acts = m.Step(at(6), 3, receivedV6(t, wire.MsgReply, req.XID,
		optClientID(capDUID), optServerID(duidA),
		optIANA(t, capIAID, 0, 0, nil, optStatus(wire.StatusNoAddrsAvail))))
	if s != State6Requesting {
		t.Fatalf("a NoAddrsAvail Reply left the machine in %s, want a Request to the other server", s)
	}
	req2 := mustSendV6(t, acts, wire.MsgRequest6)
	if sid, _ := req2.Options.First(wire.OptV6ServerID); !sameDUID(sid, duidB) {
		t.Errorf("the second Request went to %x, want the other server %x", sid, duidB)
	}
	if !journalHas(acts, "NoAddrsAvail") {
		t.Errorf("the status was not journalled; the machine said:%s", journalLines(acts))
	}

	// And when that server also has nothing, discovery restarts rather than
	// looping over the same two.
	s, acts = m.Step(at(7), 3, receivedV6(t, wire.MsgReply, req2.XID,
		optClientID(capDUID), optServerID(duidB),
		optIANA(t, capIAID, 0, 0, nil, optStatus(wire.StatusNoAddrsAvail))))
	if s != State6Init {
		t.Errorf("with both servers exhausted the machine is in %s, want discovery restarted", s)
	}
	if !journalHas(acts, "restarting discovery") {
		t.Errorf("the restart was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestNotOnLinkRestartsDiscovery is §18.2.10.1's other status arm.
func TestNotOnLinkRestartsDiscovery(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	s, acts := m.Step(at(3), 3, receivedV6(t, wire.MsgReply, uint32(capXIDRequest),
		optClientID(capDUID), optServerID(testServerDUID),
		optStatus(wire.StatusNotOnLink)))
	if s != State6Init {
		t.Errorf("a NotOnLink Reply left the machine in %s, want discovery restarted", s)
	}
	if !journalHas(acts, "NotOnLink") {
		t.Errorf("the status was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestUnspecFailChangesNothing is §18.2.10: the client "MUST limit the rate at
// which it retransmits the message", which the schedule already in flight IS.
//
// The assertion is that the machine does NOT restart, does not lose its
// exchange, and does not send anything extra — a client that treated
// UnspecFail as a failure of the exchange would abandon a server that asked
// for a moment.
func TestUnspecFailChangesNothing(t *testing.T) {
	m, _ := solicit6(t, testParams6())
	m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
	s, acts := m.Step(at(3), 3, receivedV6(t, wire.MsgReply, uint32(capXIDRequest),
		optClientID(capDUID), optServerID(testServerDUID),
		optStatus(wire.StatusUnspecFail)))
	if s != State6Requesting {
		t.Errorf("an UnspecFail Reply left the machine in %s, want the Request exchange unchanged", s)
	}
	for _, a := range acts {
		if a.Kind == ActSendV6 {
			t.Errorf("an UnspecFail Reply produced a %s", a.MsgV6.Type)
		}
	}
	if !journalHas(acts, "UnspecFail") {
		t.Errorf("the status was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestNoBindingOnARenewRequestsAgain is §18.2.10.1: the client "Sends a Request
// message to the server that responded if any of the IAs in the Reply message
// contain the NoBinding status code."
func TestNoBindingOnARenewRequestsAgain(t *testing.T) {
	m := bind6(t, testParams6(), dnsmasqLeasedAddr)
	_, acts := m.Step(at(200), 7, TimerFired(Timer6Renew))
	renew := mustSendV6(t, acts, wire.MsgRenew)

	s, acts := m.Step(at(201), 7, receivedV6(t, wire.MsgReply, renew.XID,
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 0, 0, nil, optStatus(wire.StatusNoBinding))))
	if s != State6Requesting {
		t.Fatalf("a NoBinding Reply to a Renew left the machine in %s, want %s", s, State6Requesting)
	}
	req := mustSendV6(t, acts, wire.MsgRequest6)
	if sid, _ := req.Options.First(wire.OptV6ServerID); !sameDUID(sid, testServerDUID) {
		t.Errorf("the Request went to %x, want the server that answered %x", sid, testServerDUID)
	}
	if _, held := m.Lease(); !held {
		t.Error("the lease was dropped on a NoBinding; §18.2.10.1 sends a Request and keeps using the lease until it expires")
	}
}

// TestARenewedAddressIsNotCheckedAgain is §18.2.10.1's "on which it has not
// performed duplicate address detection during processing of any of the
// previous Reply messages from the server."
//
// A client that re-ran duplicate address detection on every renewal would stop
// using its own address for four seconds at every T1, which is the defect this
// clause exists to prevent.
func TestARenewedAddressIsNotCheckedAgain(t *testing.T) {
	m := bind6(t, testParams6(), dnsmasqLeasedAddr)
	_, acts := m.Step(at(200), 7, TimerFired(Timer6Renew))
	renew := mustSendV6(t, acts, wire.MsgRenew)

	s, acts := m.Step(at(201), 7, receivedV6(t, wire.MsgReply, renew.XID,
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, []iaAddrSpec{{dnsmasqLeasedAddr, 600, 600}})))
	if s != State6Bound {
		t.Fatalf("a renewal of the address already held left the machine in %s, want %s with no duplicate address detection", s, State6Bound)
	}
	if _, ok := find(acts, ActStartDAD); ok {
		t.Error("the renewal re-ran duplicate address detection on an address this client already holds and already checked (§18.2.10.1)")
	}
	if _, ok := find(acts, ActLeaseRenewed); !ok {
		t.Error("no ActLeaseRenewed was emitted")
	}

	// A renewal that brings a NEW address does check it.
	_, acts = m.Step(at(400), 7, TimerFired(Timer6Renew))
	renew2 := mustSendV6(t, acts, wire.MsgRenew)
	s, acts = m.Step(at(401), 7, receivedV6(t, wire.MsgReply, renew2.XID,
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::200", 600, 600}})))
	if s != State6DAD {
		t.Fatalf("a renewal that granted a different address left the machine in %s, want %s", s, State6DAD)
	}
	dad, ok := find(acts, ActStartDAD)
	if !ok || dad.Target.String() != "fd00:99::200" {
		t.Errorf("duplicate address detection ran on %v, want the new address fd00:99::200", dad.Target)
	}
}

// TestTheV6DeadlinesApplyTwentyOneFoursRecommendation drives Lease6.Deadlines
// directly, which nothing else in this suite does.
//
// MEASURED, AND THE REASON THIS TEST EXISTS: swapping §21.4's two fractions —
// T1 taken as 0.8 and T2 as 0.5 of the shortest preferred lifetime — SURVIVED
// the whole suite. Every message fixture here carries a server-supplied T1 and
// T2 (150 and 240), so the fallback branch never ran with the input that would
// show it wrong. "Does this line ever run with that input?" is the question,
// and the answer was no.
//
// §21.4: "Recommended values for T1 and T2 are 0.5 and 0.8 times the shortest
// preferred lifetime of the addresses in the IA that the server is willing to
// extend, respectively." That is a recommendation to the SERVER; §14.2 is what
// obliges the client when the server sends zero: "When T1 and/or T2 values are
// set to 0, the client MUST choose a time to avoid message storms. In
// particular, it MUST NOT transmit immediately." Taking the server's own
// recommendation as the client's choice satisfies both, and the rows below are
// what that means arithmetically.
func TestTheV6DeadlinesApplyTwentyOneFoursRecommendation(t *testing.T) {
	start := at(1000)
	addr := netip.MustParseAddr(dnsmasqLeasedAddr)
	other := netip.MustParseAddr("fd00:99::184")

	for _, c := range []struct {
		name                 string
		addrs                []Addr6
		t1, t2               Duration
		renew, rebind, expir Duration
		hasR, hasB, hasE     bool
		note                 bool
	}{
		{
			name:  "the server's own T1 and T2 are used as sent",
			addrs: []Addr6{{addr, 600 * Second, 900 * Second}},
			t1:    300 * Second, t2: 480 * Second,
			renew: 300 * Second, rebind: 480 * Second, expir: 900 * Second,
			hasR: true, hasB: true, hasE: true,
		},
		{
			name:  "both zero falls back to 0.5 and 0.8 of the shortest preferred lifetime",
			addrs: []Addr6{{addr, 600 * Second, 900 * Second}},
			renew: 300 * Second, rebind: 480 * Second, expir: 900 * Second,
			hasR: true, hasB: true, hasE: true,
		},
		{
			name:  "T1 alone still falls back for T2",
			addrs: []Addr6{{addr, 600 * Second, 900 * Second}},
			t1:    60 * Second,
			renew: 60 * Second, rebind: 480 * Second, expir: 900 * Second,
			hasR: true, hasB: true, hasE: true,
		},
		{
			name: "the fractions are of the SHORTEST preferred lifetime and the expiry is the LONGEST valid one",
			addrs: []Addr6{
				{addr, 1200 * Second, 900 * Second},
				{other, 600 * Second, 1800 * Second},
			},
			renew: 300 * Second, rebind: 480 * Second, expir: 1800 * Second,
			hasR: true, hasB: true, hasE: true,
		},
		{
			name:  "an infinite preferred lifetime arms no renewal and no rebind",
			addrs: []Addr6{{addr, Infinite, Infinite}},
		},
		{
			name:  "a T2 at the expiry is clamped to 0.8 of the shortest preferred, and said so",
			addrs: []Addr6{{addr, 600 * Second, 900 * Second}},
			t1:    300 * Second, t2: 900 * Second,
			renew: 300 * Second, rebind: 480 * Second, expir: 900 * Second,
			hasR: true, hasB: true, hasE: true, note: true,
		},
		{
			name:  "a T1 not earlier than T2 becomes half of T2, and said so",
			addrs: []Addr6{{addr, 600 * Second, 900 * Second}},
			t1:    500 * Second, t2: 480 * Second,
			renew: 240 * Second, rebind: 480 * Second, expir: 900 * Second,
			hasR: true, hasB: true, hasE: true, note: true,
		},
		{
			// An IA holding nothing has already expired, and the expiry it
			// reports is its own start. THE DIRECTION IS THE POINT: reporting
			// no expiry at all would say "this lease never runs out", which is
			// the dangerous answer for the one shape that owns no address.
			// Nothing in ring 1 builds this — §18.2.10.1 makes an addressless
			// Reply a reason to try another server — so the row is here to
			// pin which way the empty case fails.
			name: "an IA with no address has already expired and arms no timers",
			t1:   300 * Second, t2: 480 * Second,
			expir: 0, hasE: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := Lease6{Start: start, Addrs: c.addrs, T1: c.t1, T2: c.t2}
			d := l.Deadlines()
			if d.HasRenew != c.hasR || d.HasRebind != c.hasB || d.HasExpire != c.hasE {
				t.Fatalf("has renew/rebind/expire = %v/%v/%v, want %v/%v/%v (note %q)",
					d.HasRenew, d.HasRebind, d.HasExpire, c.hasR, c.hasB, c.hasE, d.Note)
			}
			if c.hasR && d.Renew != start.Add(c.renew) {
				t.Errorf("T1 = %s, want %s (start + %s)", d.Renew, start.Add(c.renew), c.renew)
			}
			if c.hasB && d.Rebind != start.Add(c.rebind) {
				t.Errorf("T2 = %s, want %s (start + %s)", d.Rebind, start.Add(c.rebind), c.rebind)
			}
			if c.hasE && d.Expire != start.Add(c.expir) {
				t.Errorf("expiry = %s, want %s (start + %s)", d.Expire, start.Add(c.expir), c.expir)
			}
			if (d.Note != "") != c.note {
				t.Errorf("note = %q, want a note: %v", d.Note, c.note)
			}
			// The ordering §21.4 asks of the server, asserted on EVERY row
			// whatever it sent: it is the property the two clamps exist for.
			if d.HasRenew && d.HasRebind && !d.Renew.Before(d.Rebind) {
				t.Errorf("T1 %s is not earlier than T2 %s", d.Renew, d.Rebind)
			}
			if d.HasRebind && d.HasExpire && !d.Rebind.Before(d.Expire) {
				t.Errorf("T2 %s is not earlier than the expiry %s", d.Rebind, d.Expire)
			}
		})
	}
}
