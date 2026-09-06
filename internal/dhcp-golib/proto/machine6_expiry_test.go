package proto

import (
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// TestEveryStateEndsALeaseAtItsValidLifetime is the totality row for the
// expiry, over AllStates6 rather than over the states somebody remembered.
//
// §18.2.5: "The message exchange is terminated when the valid lifetimes of all
// leases across all IAs have expired, at which time the client uses the Solicit
// message to locate a new DHCP server and sends a Request for the expired IAs
// to the new server." A lease the caller has been TOLD about therefore ends by
// its valid lifetime wherever the machine happens to be, and the caller hears
// about it: ring 3 installed an address on that word and nothing else will
// take it away.
//
// MEASURED 2026-09-06: BOUND6 and the two renewal states handled the expiry
// and the other seven journalled "timer expire6 fired in INIT6: ignored". The
// states are reachable holding a lease — a Renew answered with an IA_NA that
// carries no address restarts discovery and §18.2.10.1 says to keep the lease
// while it runs — so the caller kept an expired IPv6 address, with no event,
// forever.
func TestEveryStateEndsALeaseAtItsValidLifetime(t *testing.T) {
	covered := 0
	for _, s := range AllStates6() {
		t.Run(s.String(), func(t *testing.T) {
			m, ok := leaseHeld6In(t, s)
			if !ok {
				t.Skipf("%s cannot hold an announced lease; see leaseHeld6In for why", s)
			}
			covered++
			if got := m.State(); got != s {
				t.Fatalf("the fixture put the machine in %s, want %s", got, s)
			}
			if _, held := m.Lease(); !held {
				t.Fatalf("the fixture reached %s without the lease it is meant to be holding", s)
			}
			_, acts := m.Step(at(100000), 31, TimerFired(Timer6Expire))
			lost, ok := find(acts, ActLeaseLost)
			if !ok {
				t.Fatalf("the valid lifetime ran out in %s and the caller was never told: no ActLeaseLost.%s",
					s, journalLines(acts))
			}
			if lost.Reason != ReasonExpired {
				t.Errorf("the lease was ended with reason %s, want %s", lost.Reason, ReasonExpired)
			}
			if _, held := m.Lease(); held {
				t.Error("the machine still holds the lease it just reported lost")
			}
			if !timerCancelled(acts, Timer6Expire) {
				t.Error("the expiry timer was left armed after it fired")
			}
		})
	}
	// D-1: a state added to the constant block and not to the fixture would
	// shrink this domain silently, and a skip reads like a pass. Two states
	// cannot hold an announced lease and every other one must.
	if want := len(AllStates6()) - 2; covered != want {
		t.Errorf("%d states were driven, want %d: a state that can hold a lease is being skipped", covered, want)
	}
}

// TestARenewAnsweredWithNoAddressKeepsTheLeaseUntilItExpires is the reviewer's
// own drive, kept as a row of its own because the totality test above builds
// its fixtures through this path and could not fail independently of it.
//
// §18.2.10.1, of a Reply that leaves an IA out: "Leave unchanged any
// information about leases the client has recorded in the IA but that were not
// included in the IA from the server." So the lease survives the restart, the
// expiry stays armed, and the renewal timers — which belong to the exchange
// that just ended, not to the lease — do not.
func TestARenewAnsweredWithNoAddressKeepsTheLeaseUntilItExpires(t *testing.T) {
	m, acts := renewedIntoNothing(t)

	if _, ok := find(acts, ActLeaseLost); ok {
		t.Fatal("the empty IA_NA ended the lease at the transition; §18.2.10.1 says to leave it unchanged")
	}
	if _, held := m.Lease(); !held {
		t.Fatal("the machine dropped the lease it was told to leave unchanged")
	}
	if !timerCancelled(acts, Timer6Renew) || !timerCancelled(acts, Timer6Rebind) {
		t.Errorf("the renewal schedule of the exchange that just ended was left armed:%s", journalLines(acts))
	}
	if _, set := timerSet(acts, Timer6Expire); set {
		t.Error("the expiry was re-armed on a restart; it belongs to the lease and was already running")
	}
	if timerCancelled(acts, Timer6Expire) {
		t.Error("the expiry was cancelled while the lease it belongs to is still held")
	}

	// And then the lifetime runs out where the search has got to.
	if got := m.State(); got != State6Init {
		t.Fatalf("the restart left the machine in %s, want %s", got, State6Init)
	}
	_, acts = m.Step(at(303*int64(Second)), 13, TimerFired(Timer6Expire))
	if _, ok := find(acts, ActLeaseLost); !ok {
		t.Fatalf("the valid lifetime ran out in INIT6 with no ActLeaseLost:%s", journalLines(acts))
	}
}

// TestARenewAnsweredNotOnLinkEndsTheLease is the other half of the same rule
// and the one place a restart DOES end the lease.
//
// §18.2.10.1 and §18.2.10.3 both answer NotOnLink with server discovery, and
// both are written for a client that holds no lease — the RFC says nothing
// about a NotOnLink answering a Renew. THE BOUND THIS CLIENT STATES: NotOnLink
// is the server saying the address is not on the link this client is on, and
// carrying on using it is the failure the plugin exists to avoid. So the lease
// ends at the transition rather than at its valid lifetime, which is RFC 2131
// 3.2(3)'s DHCPNAK answered the same way (D30).
func TestARenewAnsweredNotOnLinkEndsTheLease(t *testing.T) {
	m := bind6(t, testParams6(), dnsmasqLeasedAddr)
	_, acts := m.Step(at(200), 7, TimerFired(Timer6Renew))
	rn := mustSendV6(t, acts, wire.MsgRenew)

	s, acts := m.Step(at(201), uint64(capXIDSolicit), receivedV6(t, wire.MsgReply, rn.XID,
		optClientID(capDUID), optServerID(testServerDUID), optStatus(wire.StatusNotOnLink)))

	lost, ok := find(acts, ActLeaseLost)
	if !ok {
		t.Fatalf("NotOnLink left the caller holding an address the server says is not on this link:%s", journalLines(acts))
	}
	if lost.Reason != ReasonNak {
		t.Errorf("the lease was ended for %s, want %s: the server refused the binding", lost.Reason, ReasonNak)
	}
	if _, held := m.Lease(); held {
		t.Error("the machine still holds the lease it reported lost")
	}
	if !timerCancelled(acts, Timer6Expire) {
		t.Error("the expiry of a lease that has just ended was left armed")
	}
	if s != State6Init {
		t.Errorf("NotOnLink left the machine in %s, want discovery restarted (§18.2.10.1)", s)
	}
}

// renewedIntoNothing drives BOUND -> Renew -> a Reply whose IA_NA carries no
// address, which is §18.2.10.1's "the client finds no usable addresses" and the
// one path that reaches discovery still holding a lease.
func renewedIntoNothing(t *testing.T) (*Machine6, []Action) {
	t.Helper()
	m := bind6(t, testParams6(), dnsmasqLeasedAddr)
	_, acts := m.Step(at(200), 7, TimerFired(Timer6Renew))
	rn := mustSendV6(t, acts, wire.MsgRenew)
	_, acts = m.Step(at(201), uint64(capXIDSolicit), receivedV6(t, wire.MsgReply, rn.XID,
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, nil)))
	return m, acts
}

// leaseHeld6In builds a machine sitting in s WHILE HOLDING an announced lease,
// and reports whether that state can hold one at all.
//
// The two that cannot are named rather than omitted. STOPPED6 cannot: halt
// reports the lease lost on the way in, so a stopped machine holding one is a
// state the machine has no path to. CONFIRMING6 cannot: the resume path builds
// its binding into m.pending and announces nothing until duplicate address
// detection answers, which is D22 — a resumed address is not in use until
// something has checked it on this link.
func leaseHeld6In(t *testing.T, s State6) (*Machine6, bool) {
	t.Helper()
	p := testParams6()
	switch s {
	case State6Stopped, State6Confirming:
		return nil, false
	case State6Bound:
		return bind6(t, p, dnsmasqLeasedAddr), true
	case State6Renewing:
		m := bind6(t, p, dnsmasqLeasedAddr)
		m.Step(at(200), 7, TimerFired(Timer6Renew))
		return m, true
	case State6Rebinding:
		m := bind6(t, p, dnsmasqLeasedAddr)
		m.Step(at(200), 7, TimerFired(Timer6Rebind))
		return m, true
	case State6DAD:
		// A renewal that comes back on a DIFFERENT address: §18.2.10.1 makes
		// the client run duplicate address detection on an address it has not
		// checked, and the lease it already holds is what it keeps meanwhile.
		m := bind6(t, p, dnsmasqLeasedAddr)
		_, acts := m.Step(at(200), 7, TimerFired(Timer6Renew))
		rn := mustSendV6(t, acts, wire.MsgRenew)
		m.Step(at(201), 9, receivedV6(t, wire.MsgReply, rn.XID,
			optClientID(capDUID), optServerID(testServerDUID),
			optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::184", 300, 300}})))
		return m, true
	case State6Init:
		m, _ := renewedIntoNothing(t)
		return m, true
	case State6Selecting:
		m, _ := renewedIntoNothing(t)
		m.Step(at(202), uint64(capXIDSolicit), TimerFired(Timer6Delay))
		return m, true
	case State6Requesting:
		m, _ := renewedIntoNothing(t)
		m.Step(at(202), uint64(capXIDSolicit), TimerFired(Timer6Delay))
		m.Step(at(203), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
		return m, true
	case State6InfoRequesting:
		m, _ := renewedIntoNothing(t)
		m.Step(at(202), uint64(capXIDSolicit), TimerFired(Timer6Delay))
		m.Step(at(203), 5, RouterAdvertRaw(mustRA(t, raOtherOnly), raOtherOnly))
		return m, true
	}
	t.Fatalf("no lease-holding fixture for %s", s)
	return nil, false
}
