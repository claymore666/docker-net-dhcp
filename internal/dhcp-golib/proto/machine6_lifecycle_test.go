package proto

import (
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// TestAResumeConfirmsRatherThanSolicits is §18.2.12: "When the client detects
// that it may have moved to a new link and it has obtained addresses and no
// delegated prefixes from a server, the client SHOULD initiate a Confirm/Reply
// message exchange" — and §18.2.3, which is the exchange itself.
//
// IT IS A SHOULD AND THIS CLIENT DOES IT ANYWAY, which is a choice and not a
// requirement: an address the chassis re-installed on restart has not been
// checked against this link, and D22's rule — an address is not used before
// conflict detection completes — is what decides the open half of a SHOULD.
//
// IT IS NOT v4's REBOOTING, and the three assertions below are the three ways
// it differs: a different message, no Server Identifier, and lifetimes zeroed
// because "the server will ignore these fields".
func TestAResumeConfirmsRatherThanSolicits(t *testing.T) {
	m, acts := confirming6(t, testParams6())
	conf := mustSendV6(t, acts, wire.MsgConfirm)
	if hasSendV6(acts, wire.MsgSolicit) {
		t.Error("a Solicit went out beside the Confirm")
	}
	if _, ok := conf.Options.First(wire.OptV6ServerID); ok {
		t.Error("the Confirm carries a Server Identifier; §18.2.3 lists none")
	}
	ias, err := conf.Options.IANAs()
	if err != nil || len(ias) != 1 {
		t.Fatalf("the Confirm's IA_NA: %v %v", ias, err)
	}
	if ias[0].T1 != 0 || ias[0].T2 != 0 {
		t.Errorf("the Confirm's IA_NA carries T1=%d T2=%d; §18.2.3: \"The client SHOULD set the T1 and T2 fields ... to 0, as the server will ignore these fields\"", ias[0].T1, ias[0].T2)
	}
	addrs, err := ias[0].Options.Addrs()
	if err != nil || len(addrs) != 1 {
		t.Fatalf("the Confirm's IA Address options: %v %v", addrs, err)
	}
	if addrs[0].Addr.String() != dnsmasqLeasedAddr {
		t.Errorf("the Confirm asks about %s, want the remembered %s", addrs[0].Addr, dnsmasqLeasedAddr)
	}
	if addrs[0].PreferredLifetime != 0 || addrs[0].ValidLifetime != 0 {
		t.Errorf("the Confirm's IA Address carries lifetimes %d/%d, want zeros (§18.2.3)",
			addrs[0].PreferredLifetime, addrs[0].ValidLifetime)
	}
	_ = m
}

// TestAConfirmingSuccessBindsThroughDuplicateAddressDetection is lead ruling 1:
// duplicate address detection runs after the Confirm too, because a remembered
// address on a link the client may have just joined is exactly the address most
// likely to belong to somebody else.
func TestAConfirmingSuccessBindsThroughDuplicateAddressDetection(t *testing.T) {
	m, acts := confirming6(t, testParams6())
	conf := mustSendV6(t, acts, wire.MsgConfirm)

	s, acts := m.Step(at(2), 3, receivedV6(t, wire.MsgReply, conf.XID,
		optClientID(capDUID), optServerID(testServerDUID), optStatus(wire.StatusSuccess)))
	if s != State6DAD {
		t.Fatalf("a confirming Reply left the machine in %s, want %s", s, State6DAD)
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatal("the confirming Reply acquired a lease before duplicate address detection")
	}
	dad, ok := find(acts, ActStartDAD)
	if !ok || dad.Target.String() != dnsmasqLeasedAddr {
		t.Fatalf("duplicate address detection ran on %v, want %s", dad.Target, dnsmasqLeasedAddr)
	}

	s, acts = m.Step(at(3), 3, DADResult(addr6(dnsmasqLeasedAddr), false))
	if s != State6Bound {
		t.Fatalf("the confirmed address did not bind: %s", s)
	}
	l, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatal("no lease was acquired")
	}
	if l.Lease6.Addrs[0].Valid != 300*Second {
		t.Errorf("the resumed lease's valid lifetime is %s, want the remembered 300s (§18.2.3's \"last known lifetimes\")", l.Lease6.Addrs[0].Valid)
	}
}

// TestAConfirmingNotOnLinkRestartsDiscovery is §18.2.10.3: "When the client
// only receives one or more Reply messages with the NotOnLink status in
// response to a Confirm message, the client performs DHCP server discovery as
// described in Section 18."
func TestAConfirmingNotOnLinkRestartsDiscovery(t *testing.T) {
	m, acts := confirming6(t, testParams6())
	conf := mustSendV6(t, acts, wire.MsgConfirm)

	s, acts := m.Step(at(2), 3, receivedV6(t, wire.MsgReply, conf.XID,
		optClientID(capDUID), optServerID(testServerDUID), optStatus(wire.StatusNotOnLink)))
	if s != State6Init {
		t.Fatalf("a NotOnLink Reply to a Confirm left the machine in %s, want discovery restarted", s)
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatal("a NotOnLink Reply acquired the remembered address anyway")
	}
	if _, ok := find(acts, ActStartDAD); ok {
		t.Fatal("a NotOnLink Reply started duplicate address detection on an address the server says is not on this link")
	}
	// And the next exchange is a Solicit, not another Confirm.
	_, acts = m.Step(at(3), capXIDSolicit, TimerFired(Timer6Delay))
	if !hasSendV6(acts, wire.MsgSolicit) {
		t.Error("the restart did not solicit; the remembered addresses have been dropped")
	}
}

// TestASilentConfirmKeepsTheAddresses is the arm where the v6 machine does the
// OPPOSITE of the v4 one on the same shape, which is why it is asserted rather
// than assumed.
//
// §18.2.3: "If the client receives no responses before the message transmission
// process terminates, as described in Section 15, the client SHOULD continue to
// use any leases, using the last known lifetimes for those leases, and SHOULD
// continue to use any other previously obtained configuration parameters."
// RFC 2131 §3.2(3) makes the same thing a MAY for an INIT-REBOOT that goes
// unanswered, and the v4 machine declines it.
func TestASilentConfirmKeepsTheAddresses(t *testing.T) {
	p := testParams6()
	m, _ := confirming6(t, p)

	// CNF_MAX_RD is a duration bound: §18.2.3 gives MRD as CNF_MAX_RD with no
	// MRC, so the exchange ends by the clock rather than by a count.
	now := int64(2)
	var acts []Action
	var s State6
	for i := 0; i < 40; i++ {
		s, acts = m.Step(at(now), 3, TimerFired(Timer6Retransmit))
		now += 2
		if s != State6Confirming {
			break
		}
	}
	if s == State6Confirming {
		t.Fatalf("the Confirm exchange was still running after %ds; CNF_MAX_RD is %s", now, p.Confirm().MRD)
	}
	if s != State6DAD {
		t.Fatalf("the exhausted Confirm left the machine in %s, want %s: §18.2.3 keeps the leases and lead ruling 1 checks them first", s, State6DAD)
	}
	if !journalHas(acts, "continuing with the last known lifetimes") {
		t.Errorf("the continue path was not journalled; the machine said:%s", journalLines(acts))
	}
	dad, ok := find(acts, ActStartDAD)
	if !ok || dad.Target.String() != dnsmasqLeasedAddr {
		t.Fatalf("the continue path started duplicate address detection on %v, want %s", dad.Target, dnsmasqLeasedAddr)
	}

	s, acts = m.Step(at(now), 3, DADResult(addr6(dnsmasqLeasedAddr), false))
	if s != State6Bound {
		t.Fatalf("the kept address did not bind: %s", s)
	}
	l, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatal("no lease was acquired from the kept addresses")
	}
	if l.Lease6.T1 != 150*Second || l.Lease6.T2 != 240*Second {
		t.Errorf("the kept lease carries T1=%s T2=%s, want the remembered 150s/240s (§18.2.3's \"last known lifetimes\")", l.Lease6.T1, l.Lease6.T2)
	}
}

// TestAddressLostWhileBoundDeclinesAndRestarts is lead ruling 8.
//
// The withdrawal is evidence somebody else has the address — sequencing §8.2
// and RFC 4429 §3.3 — so the server is told, because a binding this client
// cannot use is one the server should not hand back to it. That is why the
// action list carries BOTH an ActLeaseLost with ReasonConflict and a Decline.
func TestAddressLostWhileBoundDeclinesAndRestarts(t *testing.T) {
	for _, o := range leaseOrigins6() {
		t.Run(o.name, func(t *testing.T) {
			m := o.bound(t, testParams6())
			s, acts := m.Step(at(100), 3, Simple(EvAddressLost))

			lost, ok := find(acts, ActLeaseLost)
			if !ok {
				t.Fatal("the withdrawal did not report the lease lost")
			}
			if lost.Reason != ReasonConflict {
				t.Errorf("the lease was lost for %s, want %s: the address was withdrawn under us", lost.Reason, ReasonConflict)
			}
			if lost.Lease6.Addrs[0].Addr.String() != dnsmasqLeasedAddr {
				t.Errorf("the lost lease names %v", lost.Lease6.Addrs)
			}
			dec := mustSendV6(t, acts, wire.MsgDecline6)
			assertDeclineNamesTheServer(t, dec)
			addrs := declinedAddrs(t, dec)
			if len(addrs) != 1 || addrs[0].Addr.String() != dnsmasqLeasedAddr {
				t.Errorf("the Decline names %v, want %s", addrs, dnsmasqLeasedAddr)
			}
			if s != State6DAD {
				t.Fatalf("the withdrawal left the machine in %s; the Decline exchange runs from %s", s, State6DAD)
			}
			if _, held := m.Lease(); held {
				t.Error("the machine still reports a held lease after the address was withdrawn")
			}

			// The Reply to the Decline restarts discovery.
			s, _ = m.Step(at(101), 3, receivedV6(t, wire.MsgReply, dec.XID,
				optClientID(capDUID), optServerID(testServerDUID)))
			if s != State6Init {
				t.Errorf("the Reply to the Decline left the machine in %s, want discovery restarted", s)
			}
		})
	}
}

// TestAddressLostWhileRebindingDeclines is the reviewer's scenario (c) and the
// one the RFC nearly forbids.
//
// §18.2.5 builds the Rebind "as described in Section 18.2.4, with the following
// differences: ... The client does not include the Server Identifier option
// (see Section 21.3) in the Rebind message." So while REBINDING the EXCHANGE
// has no server — enterRebinding clears the machine's — and a Decline built
// from the exchange could not be sent at all.
//
// §18.2.8 says where the Decline's own Server Identifier comes from, and it is
// not the exchange: "The client MUST include a Server Identifier option (see
// Section 21.3) in the Decline message, identifying the server that allocated
// the lease(s)." The LEASE names that server, and the lease is what is being
// declined. So the Decline is possible in REBINDING and this is the row that
// says so.
func TestAddressLostWhileRebindingDeclines(t *testing.T) {
	for _, o := range leaseOrigins6() {
		t.Run(o.name, func(t *testing.T) {
			m := o.bound(t, testParams6())
			if s, _ := m.Step(at(90), 7, TimerFired(Timer6Rebind)); s != State6Rebinding {
				t.Fatalf("T2 left the machine in %s, want %s", s, State6Rebinding)
			}
			s, acts := m.Step(at(100), 3, Simple(EvAddressLost))
			if s != State6DAD {
				t.Fatalf("the withdrawal left the machine in %s; the Decline exchange runs from %s.%s", s, State6DAD, journalLines(acts))
			}
			dec := mustSendV6(t, acts, wire.MsgDecline6)
			assertDeclineNamesTheServer(t, dec)
			addrs := declinedAddrs(t, dec)
			if len(addrs) != 1 || addrs[0].Addr.String() != dnsmasqLeasedAddr {
				t.Errorf("the Decline names %v, want %s", addrs, dnsmasqLeasedAddr)
			}
			if _, ok := find(acts, ActLeaseLost); !ok {
				t.Error("the withdrawal did not report the lease lost")
			}
		})
	}
}

// assertDeclineNamesTheServer is §18.2.8's MUST, checked by VALUE: a Decline
// carrying somebody else's Server Identifier satisfies "an option is present"
// and identifies the wrong binding.
func assertDeclineNamesTheServer(t *testing.T, dec *wire.MessageV6) {
	t.Helper()
	sid, ok := dec.Options.First(wire.OptV6ServerID)
	if !ok {
		t.Fatalf("the Decline carries no Server Identifier; §18.2.8: \"The client MUST include a Server Identifier option (see Section 21.3) in the Decline message, identifying the server that allocated the lease(s).\"")
	}
	if string(sid) != string(testServerDUID) {
		t.Errorf("the Decline names the server %x, want the one that allocated the lease, %x", sid, testServerDUID)
	}
}

func declinedAddrs(t *testing.T, msg *wire.MessageV6) []*wire.IAAddr {
	t.Helper()
	ias, err := msg.Options.IANAs()
	if err != nil || len(ias) != 1 {
		t.Fatalf("the %s's IA_NA: %v %v", msg.Type, ias, err)
	}
	addrs, err := ias[0].Options.Addrs()
	if err != nil {
		t.Fatalf("the %s's IA Address options: %v", msg.Type, err)
	}
	return addrs
}

// TestReleaseStopsUsingTheAddressBeforeItSendsAnything is §18.2.7's ordering
// MUST: "The client MUST stop using all of the leases being released before
// the client begins the Release message exchange process. For an address, this
// means the address MUST have been removed from the interface."
//
// ActLeaseLost is how ring 2 learns to remove the address, so its position in
// the action list relative to the ActSendV6 is the ordering the RFC requires.
func TestReleaseStopsUsingTheAddressBeforeItSendsAnything(t *testing.T) {
	m := bind6(t, testParams6(), dnsmasqLeasedAddr)
	s, acts := m.Step(at(100), 3, Simple(EvRelease))
	if s != State6Stopped {
		t.Fatalf("EvRelease left the machine in %s, want %s", s, State6Stopped)
	}

	lostAt, sendAt := -1, -1
	for i, a := range acts {
		if a.Kind == ActLeaseLost && lostAt < 0 {
			lostAt = i
		}
		if a.Kind == ActSendV6 && a.MsgV6.Type == wire.MsgRelease6 && sendAt < 0 {
			sendAt = i
		}
	}
	if lostAt < 0 {
		t.Fatal("EvRelease did not report the lease lost")
	}
	if sendAt < 0 {
		t.Fatal("EvRelease sent no Release")
	}
	if lostAt > sendAt {
		t.Errorf("the Release (action %d) precedes the ActLeaseLost (action %d); §18.2.7 requires the address to be removed from the interface BEFORE the exchange begins", sendAt, lostAt)
	}
	if acts[lostAt].Reason != ReasonReleased {
		t.Errorf("the lease was lost for %s, want %s", acts[lostAt].Reason, ReasonReleased)
	}

	rel := acts[sendAt].MsgV6
	if sid, ok := rel.Options.First(wire.OptV6ServerID); !ok || !sameDUID(sid, testServerDUID) {
		t.Errorf("the Release names the server %x, want %x (§18.2.7 makes it a MUST)", sid, testServerDUID)
	}
	addrs := declinedAddrs(t, rel)
	if len(addrs) != 1 || addrs[0].Addr.String() != dnsmasqLeasedAddr {
		t.Errorf("the Release names %v, want %s", addrs, dnsmasqLeasedAddr)
	}
	// The renewal timers are gone: nothing is held any more.
	for _, id := range []TimerID{Timer6Renew, Timer6Rebind, Timer6Expire} {
		if !timerCancelled(acts, id) {
			t.Errorf("%s survived the release", id)
		}
	}

	// §18.2.10.2: "When the client receives a valid Reply message in response
	// to a Release message, the client considers the Release event completed,
	// regardless of the Status Code option (see Section 21.13) returned by the
	// server."
	s, acts = m.Step(at(101), 3, receivedV6(t, wire.MsgReply, rel.XID,
		optClientID(capDUID), optServerID(testServerDUID), optStatus(wire.StatusUnspecFail)))
	if s != State6Stopped {
		t.Fatalf("the Reply to the Release left the machine in %s", s)
	}
	if !journalHas(acts, "the release is complete") {
		t.Errorf("the completion was not journalled even though §18.2.10.2 says the status is irrelevant; the machine said:%s", journalLines(acts))
	}
}

// TestASilentReleaseGivesUp is §18.2.7's other ending: the exchange has
// REL_MAX_RC and "the client considers the Release event completed" is not
// available, so the binding is left to expire.
func TestASilentReleaseGivesUp(t *testing.T) {
	p := testParams6()
	m := bind6(t, p, dnsmasqLeasedAddr)
	m.Step(at(100), 3, Simple(EvRelease))

	now := int64(101)
	for i := 1; i < p.RelMaxRC; i++ {
		_, acts := m.Step(at(now), 3, TimerFired(Timer6Retransmit))
		now++
		if !hasSendV6(acts, wire.MsgRelease6) {
			t.Fatalf("retransmission %d sent no Release", i)
		}
	}
	s, acts := m.Step(at(now), 3, TimerFired(Timer6Retransmit))
	if hasSendV6(acts, wire.MsgRelease6) {
		t.Errorf("a %d'th Release went out; §18.2.7 gives REL_MAX_RC as %d", p.RelMaxRC+1, p.RelMaxRC)
	}
	if s != State6Stopped {
		t.Errorf("the exhausted Release left the machine in %s", s)
	}
	if !journalHas(acts, "reclaimed when its valid lifetime expires") {
		t.Errorf("giving up was not journalled; the machine said:%s", journalLines(acts))
	}
}

// TestInformationRequestCarriesNoIAAndRefreshesOnItsOwnClock is §18.2.6 and
// §21.23.
func TestInformationRequestCarriesNoIAAndRefreshesOnItsOwnClock(t *testing.T) {
	p := testParams6()
	m, _ := solicit6(t, p)
	_, acts := m.Step(at(2), 3, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
	inf := mustSendV6(t, acts, wire.MsgInformationRequest)

	s, acts := m.Step(at(3), 3, receivedV6(t, wire.MsgReply, inf.XID,
		optClientID(capDUID), optServerID(testServerDUID),
		wire.OptionV6{Code: wire.OptV6DNSServers, Data: addr16(dnsmasqDNS)},
		optU32(wire.OptV6InfoRefresh, 3600)))
	if s != State6InfoRequesting {
		t.Fatalf("the Reply to the Information-request left the machine in %s", s)
	}
	cfg, ok := find(acts, ActConfigured)
	if !ok {
		t.Fatal("the Reply produced no ActConfigured")
	}
	if len(cfg.Config.DNS) != 1 || cfg.Config.DNS[0].String() != dnsmasqDNS {
		t.Errorf("the configuration carries DNS %v, want %s", cfg.Config.DNS, dnsmasqDNS)
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatal("an Information-request exchange acquired a lease; §18.2.6 has no addresses in it")
	}
	d, ok := timerSet(acts, Timer6Refresh)
	if !ok || d != 3600*Second {
		t.Errorf("the refresh was armed for %s, want the 3600s the Reply carried (§21.23)", d)
	}

	// The refresh fires, delays, and asks again.
	s, acts = m.Step(at(3700), 250, TimerFired(Timer6Refresh))
	if s != State6InfoRequesting {
		t.Fatalf("the refresh left the machine in %s", s)
	}
	if hasSendV6(acts, wire.MsgInformationRequest) {
		t.Error("the refresh sent an Information-request immediately; §21.23: \"the client MUST delay sending the first Information-request by a random amount of time between 0 and INF_MAX_DELAY\"")
	}
	delay, ok := timerSet(acts, Timer6Delay)
	if !ok {
		t.Fatal("the refresh armed no delay")
	}
	if delay < 0 || delay > p.InfMaxDelay {
		t.Errorf("the delay is %s, outside [0, INF_MAX_DELAY=%s]", delay, p.InfMaxDelay)
	}
	_, acts = m.Step(at(3701), 77, TimerFired(Timer6Delay))
	if !hasSendV6(acts, wire.MsgInformationRequest) {
		t.Error("the delay expiring sent no Information-request")
	}
}

// TestTheInformationRefreshTimeIsBounded is §21.23's two rules and §7.7's
// infinity, as a table.
func TestTheInformationRefreshTimeIsBounded(t *testing.T) {
	p := testParams6()
	for _, tc := range []struct {
		name    string
		opts    []wire.OptionV6
		want    Duration
		armed   bool
		journal string
		// report is what ActConfigured must carry. It is a separate column
		// from want because the two were separately DERIVED and disagreed —
		// the caller was told the raw option value while the bounded one was
		// armed — and a column that read `want` for both could not tell.
		report Duration
	}{
		{
			"absent",
			nil,
			p.IRTDefault, true,
			"using IRT_DEFAULT",
			p.IRTDefault,
		},
		{
			"below IRT_MINIMUM",
			[]wire.OptionV6{optU32(wire.OptV6InfoRefresh, 60)},
			p.IRTMinimum, true,
			"below IRT_MINIMUM",
			p.IRTMinimum,
		},
		{
			"exactly IRT_MINIMUM",
			[]wire.OptionV6{optU32(wire.OptV6InfoRefresh, 600)},
			600 * Second, true,
			"",
			600 * Second,
		},
		{
			"infinite",
			[]wire.OptionV6{optU32(wire.OptV6InfoRefresh, 0xffffffff)},
			0, false,
			"no refresh is armed",
			Infinite,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := solicit6(t, p)
			_, acts := m.Step(at(2), 3, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
			inf := mustSendV6(t, acts, wire.MsgInformationRequest)

			opts := append([]wire.OptionV6{optClientID(capDUID), optServerID(testServerDUID)}, tc.opts...)
			_, acts = m.Step(at(3), 3, receivedV6(t, wire.MsgReply, inf.XID, opts...))

			d, ok := timerSet(acts, Timer6Refresh)
			if ok != tc.armed {
				t.Fatalf("a refresh timer armed = %t, want %t", ok, tc.armed)
			}
			if tc.armed && d != tc.want {
				t.Errorf("the refresh was armed for %s, want %s (§21.23)", d, tc.want)
			}
			if !tc.armed && !timerCancelled(acts, Timer6Refresh) {
				t.Error("an infinite refresh time left the timer armed rather than cancelled (§21.23, §7.7)")
			}
			if tc.journal != "" && !journalHas(acts, tc.journal) {
				t.Errorf("the rule was not journalled as %q; the machine said:%s", tc.journal, journalLines(acts))
			}
			// The value the CALLER is told, which is the half §21.23's rules
			// were not reaching. Configuration.Refresh promises the bounds;
			// this is where that promise is either kept or not.
			cfg, ok := find(acts, ActConfigured)
			if !ok {
				t.Fatal("the Reply produced no ActConfigured")
			}
			if cfg.Config.RefreshTime != tc.report {
				t.Errorf("the caller was told the refresh time is %s, want %s: §21.23's rules decide what this client does and so decide what it reports",
					cfg.Config.RefreshTime, tc.report)
			}
		})
	}
}

// addr16 is one IPv6 address as the 16 octets an option carries.
func addr16(s string) []byte {
	a := addr6(s).As16()
	return a[:]
}
