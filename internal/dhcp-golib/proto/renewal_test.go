package proto

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

const (
	testLeaseAddr = "192.168.99.50"
	testServerID  = "192.168.99.1"
)

// bound drives a machine from STOPPED to BOUND and hands it back.
//
// The lease it lands on: sent at at(1), acked at at(2), 3600 seconds. So
// expiry is at(3601), T1 defaults to at(1801) and T2 to at(3151), and every
// instant in this file is derived from the machine's own Deadlines rather than
// from those numbers written out again.
func bound(t *testing.T, p Params, tweak func(*wire.Message)) *Machine {
	t.Helper()
	m := newMachine(t, p)
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testLeaseAddr, testServerID)))
	req := mustSend(t, acts, wire.MsgRequest)
	ack := ackFor(req, testLeaseAddr, testServerID, 3600)
	if tweak != nil {
		tweak(ack)
	}
	if _, acts = m.Step(at(2), 3, received(t, ack)); m.State() != StateBound {
		t.Fatalf("fixture did not reach BOUND: %s\n%v", m.State(), RenderActions(acts))
	}
	return m
}

// timerSet returns the delay the actions arm the given timer for.
func timerSet(acts []Action, id TimerID) (Duration, bool) {
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == id {
			return a.After, true
		}
	}
	return 0, false
}

func timerCancelled(acts []Action, id TimerID) bool {
	for _, a := range acts {
		if a.Kind == ActCancelTimer && a.Timer == id {
			return true
		}
	}
	return false
}

// sent returns the one message the actions send, with its destination.
func sent(t *testing.T, acts []Action, want wire.MessageType) (*wire.Message, Dest) {
	t.Helper()
	msg := mustSend(t, acts, want)
	for _, a := range acts {
		if a.Kind == ActSend {
			return msg, a.Dest
		}
	}
	t.Fatal("unreachable: mustSend found a send that is no longer there")
	return nil, Dest{}
}

func journalHas(acts []Action, sub string) bool {
	for _, a := range acts {
		if a.Kind == ActJournal && strings.Contains(a.Note, sub) {
			return true
		}
	}
	return false
}

// renew fires T1 and returns the actions.
func renew(t *testing.T, m *Machine) []Action {
	t.Helper()
	t1, ok := m.lease.RenewAt()
	if !ok {
		t.Fatal("the lease has no T1")
	}
	st, acts := m.Step(t1, 0x11, TimerFired(TimerRenew))
	_ = st
	return acts
}

// ------------------------------------------------------------- deadlines --

// TestDeadlinesTable is D17 on the arithmetic: the defaults, the server's own
// options 58 and 59, and the four values a server can send that the RFC's
// ordering rule forbids.
//
// The clamped rows are the point. RFC 2131 section 4.4.5's "T1 MUST be earlier
// than T2, which, in turn, MUST be earlier than the time at which the client's
// lease will expire" is addressed to whoever SETS the values; a client that
// assumed it would arm a rebind before a renew on a mistyped server config.
func TestDeadlinesTable(t *testing.T) {
	const start = 0
	cases := []struct {
		name                   string
		lease, t1, t2          Duration
		renew, rebind, expire  Duration
		hasR, hasB, hasE, note bool
	}{
		{
			name:  "defaults from the lease alone",
			lease: 3600 * Second,
			renew: 1800 * Second, rebind: 3150 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true,
		},
		{
			name:  "the server's own T1 and T2",
			lease: 3600 * Second, t1: 60 * Second, t2: 120 * Second,
			renew: 60 * Second, rebind: 120 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true,
		},
		{
			name:  "T1 later than T2 is clamped to half of T2",
			lease: 3600 * Second, t1: 3000 * Second, t2: 1200 * Second,
			renew: 600 * Second, rebind: 1200 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true, note: true,
		},
		{
			name:  "T1 equal to T2 is clamped too",
			lease: 3600 * Second, t1: 1200 * Second, t2: 1200 * Second,
			renew: 600 * Second, rebind: 1200 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true, note: true,
		},
		{
			name:  "T2 later than the lease falls back to 0.875",
			lease: 3600 * Second, t1: 60 * Second, t2: 7200 * Second,
			renew: 60 * Second, rebind: 3150 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true, note: true,
		},
		{
			// T1 is untouched here: 0.5 * lease is 1800, which is still
			// earlier than the clamped T2, so nothing is out of order and
			// nothing is second-guessed. The clamp is a repair of what the
			// server sent, not a recomputation of the whole schedule.
			name:  "T2 equal to the lease falls back, and the default T1 stands",
			lease: 3600 * Second, t2: 3600 * Second,
			renew: 1800 * Second, rebind: 3150 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true, note: true,
		},
		{
			name:   "a lease of zero has nothing to renew",
			lease:  0,
			expire: 0, hasE: true,
		},
		{
			name:  "an infinite lease has no expiry and no defaults",
			lease: Infinite,
		},
		{
			name:  "an infinite lease keeps the T1 and T2 the server sent",
			lease: Infinite, t1: 60 * Second, t2: 120 * Second,
			renew: 60 * Second, rebind: 120 * Second,
			hasR: true, hasB: true,
		},
		{
			name:  "T1 alone still defaults T2",
			lease: 3600 * Second, t1: 60 * Second,
			renew: 60 * Second, rebind: 3150 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true,
		},
		{
			name:  "T2 alone still defaults T1, and the default is kept when it is earlier",
			lease: 3600 * Second, t2: 3000 * Second,
			renew: 1800 * Second, rebind: 3000 * Second, expire: 3600 * Second,
			hasR: true, hasB: true, hasE: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := Lease{Start: start, LeaseTime: c.lease, T1: c.t1, T2: c.t2}
			d := l.Deadlines()
			if d.HasRenew != c.hasR || d.HasRebind != c.hasB || d.HasExpire != c.hasE {
				t.Fatalf("has renew/rebind/expire = %v/%v/%v, want %v/%v/%v (note %q)",
					d.HasRenew, d.HasRebind, d.HasExpire, c.hasR, c.hasB, c.hasE, d.Note)
			}
			if c.hasR && d.Renew != Instant(start).Add(c.renew) {
				t.Fatalf("T1 = %s, want %s", d.Renew, Instant(start).Add(c.renew))
			}
			if c.hasB && d.Rebind != Instant(start).Add(c.rebind) {
				t.Fatalf("T2 = %s, want %s", d.Rebind, Instant(start).Add(c.rebind))
			}
			if c.hasE && d.Expire != Instant(start).Add(c.expire) {
				t.Fatalf("expiry = %s, want %s", d.Expire, Instant(start).Add(c.expire))
			}
			if (d.Note != "") != c.note {
				t.Fatalf("note = %q, want a note: %v", d.Note, c.note)
			}
			// The ordering the RFC asks of the server, enforced here whatever
			// the server sent: this is the property the clamps exist for, and
			// it is asserted on EVERY row rather than on the clamped ones.
			if d.HasRenew && d.HasRebind && !d.Renew.Before(d.Rebind) {
				t.Fatalf("T1 %s is not earlier than T2 %s", d.Renew, d.Rebind)
			}
			if d.HasRebind && d.HasExpire && !d.Rebind.Before(d.Expire) {
				t.Fatalf("T2 %s is not earlier than the expiry %s", d.Rebind, d.Expire)
			}
		})
	}
}

// TestAckWithAnInfiniteLeaseTimeIsInfinite is D17's 0xFFFFFFFF row, driven
// through the decode rather than through a hand-set Duration: the sentinel has
// to survive SecondsToDuration, and a client that turned it into 136 years
// would arm an expiry no test on Lease alone would see.
func TestAckWithAnInfiniteLeaseTimeIsInfinite(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testLeaseAddr, testServerID)))
	req := mustSend(t, acts, wire.MsgRequest)
	ack := ackFor(req, testLeaseAddr, testServerID, InfiniteSeconds)
	_, acts = m.Step(at(2), 3, received(t, ack))
	if _, ok := timerSet(acts, TimerExpire); ok {
		t.Fatalf("an infinite lease armed an expiry:\n%v", RenderActions(acts))
	}
	for _, id := range []TimerID{TimerExpire, TimerRenew, TimerRebind} {
		if !timerCancelled(acts, id) {
			t.Fatalf("timer %s was neither set nor cancelled:\n%v", id, RenderActions(acts))
		}
	}
	if !m.lease.LeaseTime.IsInfinite() {
		t.Fatalf("lease time = %s, want the infinite sentinel", m.lease.LeaseTime)
	}
}

// TestBoundArmsAllThreeLeaseTimers.
func TestBoundArmsAllThreeLeaseTimers(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testLeaseAddr, testServerID)))
	req := mustSend(t, acts, wire.MsgRequest)
	_, acts = m.Step(at(2), 3, received(t, ackFor(req, testLeaseAddr, testServerID, 3600)))

	// Measured from the ACK's arrival, not from the lease's start: the lease
	// clock began when the REQUEST was sent at at(1), so by at(2) one second
	// of it is spent and every deadline is one second nearer.
	for _, c := range []struct {
		id   TimerID
		want Duration
	}{
		{TimerRenew, 1799 * Second},
		{TimerRebind, 3149 * Second},
		{TimerExpire, 3599 * Second},
	} {
		got, ok := timerSet(acts, c.id)
		if !ok {
			t.Fatalf("timer %s not armed:\n%v", c.id, RenderActions(acts))
		}
		if got != c.want {
			t.Fatalf("timer %s armed for %s, want %s", c.id, got, c.want)
		}
	}
}

// --------------------------------------------------------------- RENEWING --

// TestRenewalOmitsTheRequestedAddressAndServerIdentifier is RFC 2131 Table 5's
// RENEWING column: 'ciaddr' is the client's address, and options 50 and 54
// MUST NOT be filled in.
//
// Section 4.3.2 is why the server identifier is the one that bites: a server
// reads a DHCPREQUEST carrying one as generated during SELECTING, compares it
// with itself, and STAYS SILENT if it does not match. A renewal sent with a
// server identifier to a server whose identifier has changed is never answered
// at all, and the client only finds out at T2.
func TestRenewalOmitsTheRequestedAddressAndServerIdentifier(t *testing.T) {
	m := bound(t, testParams(), nil)
	acts := renew(t, m)
	if m.State() != StateRenewing {
		t.Fatalf("state = %s, want RENEWING", m.State())
	}
	msg, dest := sent(t, acts, wire.MsgRequest)

	if msg.CIAddr != netip.MustParseAddr(testLeaseAddr) {
		t.Fatalf("ciaddr = %s, want %s (RFC 2131 Table 5)", msg.CIAddr, testLeaseAddr)
	}
	if _, ok := msg.Options[wire.OptRequestedIP]; ok {
		t.Fatal("the renewal carries option 50; RFC 2131 Table 5 makes it MUST NOT in the RENEWING column")
	}
	if _, ok := msg.Options[wire.OptServerID]; ok {
		t.Fatal("the renewal carries option 54; RFC 2131 Table 5 makes it MUST NOT in the RENEWING column, and section 4.3.2 has the server ignore the message when it does not match")
	}
	if dest.Broadcast {
		t.Fatal("the renewal was broadcast; RFC 2131 4.4.5 unicasts it to the server")
	}
	if dest.Addr != netip.MustParseAddr(testServerID) {
		t.Fatalf("renewal sent to %s, want the lease's server %s", dest.Addr, testServerID)
	}
	if dest.Src != netip.MustParseAddr(testLeaseAddr) {
		t.Fatalf("renewal source = %s, want the leased address %s; the datagram carries it in ciaddr and must come from it", dest.Src, testLeaseAddr)
	}
	// The parameter request list still goes out: a renewal is where a client
	// learns that the server's DNS servers changed.
	if _, ok := msg.Options[wire.OptParameterList]; !ok {
		t.Fatal("the renewal carries no parameter request list")
	}
}

// TestRebindingBroadcastsAndKeepsCiaddr is Table 5's REBINDING column.
func TestRebindingBroadcastsAndKeepsCiaddr(t *testing.T) {
	m := bound(t, testParams(), nil)
	renew(t, m)
	t2, _ := m.lease.RebindAt()
	_, acts := m.Step(t2, 0x22, TimerFired(TimerRebind))
	if m.State() != StateRebinding {
		t.Fatalf("state = %s, want REBINDING", m.State())
	}
	msg, dest := sent(t, acts, wire.MsgRequest)
	if !dest.Broadcast {
		t.Fatalf("the rebinding request went to %s, not broadcast (RFC 2131 4.4.5)", dest.Addr)
	}
	if msg.CIAddr != netip.MustParseAddr(testLeaseAddr) {
		t.Fatalf("ciaddr = %s, want %s", msg.CIAddr, testLeaseAddr)
	}
	if _, ok := msg.Options[wire.OptServerID]; ok {
		t.Fatal("the rebinding request carries option 54; no server has been selected")
	}
}

// TestRenewalKeepsOneTransactionAcrossT2 holds the decision that RENEWING and
// REBINDING are ONE transaction.
//
// RFC 2131 section 2 defines 'secs' as "seconds elapsed since client began
// address acquisition or renewal process", naming the renewal as one process
// with one start. The alternative — a fresh xid at T2 — would make every
// DHCPACK the server had already sent to the RENEWING transaction
// unacceptable, throwing away an answer that was on its way.
func TestRenewalKeepsOneTransactionAcrossT2(t *testing.T) {
	m := bound(t, testParams(), nil)
	acts := renew(t, m)
	first, _ := sent(t, acts, wire.MsgRequest)
	t1, _ := m.lease.RenewAt()
	t2, _ := m.lease.RebindAt()

	_, acts = m.Step(t2, 0x22, TimerFired(TimerRebind))
	second, _ := sent(t, acts, wire.MsgRequest)

	if second.XID != first.XID {
		t.Fatalf("REBINDING drew a new xid %#08x; RENEWING sent %#08x, and an ACK already in flight for it would be discarded",
			second.XID, first.XID)
	}
	if want := uint16(t2.Sub(t1).Seconds()); second.Secs != want {
		t.Fatalf("secs at T2 = %d, want %d — the renewal process began at T1, not at T2", second.Secs, want)
	}

	// And the ACK the RENEWING request earned is still acceptable in
	// REBINDING, which is the whole point.
	_, acts = m.Step(t2.Add(Second), 0x33, received(t, ackFor(first, testLeaseAddr, testServerID, 3600)))
	if m.State() != StateBound {
		t.Fatalf("an ACK for the renewal transaction was refused in REBINDING: %s\n%v", m.State(), RenderActions(acts))
	}
}

// TestRenewalRetransmitFollowsTheRFCSchedule is RFC 2131 section 4.4.5's
// "one-half of the remaining time until T2" and "one-half of the remaining
// lease time", "down to a minimum of 60 seconds".
//
// It is a table on the machine's own helper and not on the timers alone,
// because the two states measure against DIFFERENT deadlines and a single
// implementation using one for both passes any test that only checks RENEWING.
func TestRenewalRetransmitFollowsTheRFCSchedule(t *testing.T) {
	// A lease long enough that halving is above the floor for a while:
	// start 0, 40000 seconds, T1 20000, T2 35000.
	l := Lease{Start: 0, LeaseTime: 40000 * Second}
	cases := []struct {
		name  string
		state State
		now   Instant
		want  Duration
	}{
		{"renewing halves the time to T2", StateRenewing, at(20000), 7500 * Second},
		{"renewing halves again later", StateRenewing, at(27500), 3750 * Second},
		{"renewing floors at 60s", StateRenewing, at(34901), RenewRetransmitFloor},
		{"renewing floors past T2", StateRenewing, at(35001), RenewRetransmitFloor},
		{"rebinding halves the remaining lease", StateRebinding, at(35000), 2500 * Second},
		{"rebinding floors at 60s", StateRebinding, at(39901), RenewRetransmitFloor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Machine{state: c.state, lease: l, haveLse: true}
			if got := m.renewalDelay(c.now); got != c.want {
				t.Fatalf("delay = %s, want %s", got, c.want)
			}
		})
	}

	// The floor holds when there is no deadline to converge on at all.
	inf := &Machine{state: StateRebinding, lease: Lease{LeaseTime: Infinite}, haveLse: true}
	if got := inf.renewalDelay(at(1)); got != RenewRetransmitFloor {
		t.Fatalf("infinite lease delay = %s, want the floor %s", got, RenewRetransmitFloor)
	}
}

// TestRenewalRetransmitIsNotTheAcquisitionBackoff is the control on the row
// above: Params.Discover and Params.Request jitter and double, and section
// 4.4.5's schedule does neither. A machine that reused Backoff here would
// still halve nothing and would still stop at the budget.
func TestRenewalRetransmitIsNotTheAcquisitionBackoff(t *testing.T) {
	m := bound(t, testParams(), nil)
	acts := renew(t, m)
	first, ok := timerSet(acts, TimerRetransmit)
	if !ok {
		t.Fatalf("no retransmit timer armed for the renewal:\n%v", RenderActions(acts))
	}
	t1, _ := m.lease.RenewAt()
	t2, _ := m.lease.RebindAt()
	if want := t2.Sub(t1) / 2; first != want {
		t.Fatalf("first renewal retransmit at %s, want %s", first, want)
	}

	// Twenty retransmissions, well past any acquisition budget, and the
	// machine is still in RENEWING with the lease held.
	now := t1
	for i := 0; i < 20; i++ {
		now = now.Add(first)
		_, acts = m.Step(now, uint64(i), TimerFired(TimerRetransmit))
		if _, ok := find(acts, ActSend); !ok {
			t.Fatalf("retransmission %d sent nothing:\n%v", i, RenderActions(acts))
		}
		if _, ok := find(acts, ActFailed); ok {
			t.Fatalf("retransmission %d reported a failure; section 4.4.5's schedule has no budget:\n%v", i, RenderActions(acts))
		}
		first, _ = timerSet(acts, TimerRetransmit)
	}
	if m.State() != StateRenewing {
		t.Fatalf("state = %s after 20 retransmissions, want RENEWING", m.State())
	}
	if !m.haveLse {
		t.Fatal("the lease was dropped by retransmitting")
	}
}

// TestRenewalSurvivesATransportThatRefusesUnicast is the relay answer, and it
// is the reason noteActionFailed has a RENEWING arm.
//
// runtime.PacketTransport cannot unicast to a server it has not learned a
// hardware address for, which is exactly the case when the server is behind a
// relay agent. That refusal is DETERMINISTIC — every attempt fails — so a
// machine that escalated on MaxSendFailures would drop a perfectly good lease
// within seconds of T1. Section 4.4.5's answer is T2: the same request
// broadcast, which reaches the relay.
func TestRenewalSurvivesATransportThatRefusesUnicast(t *testing.T) {
	p := testParams()
	p.MaxSendFailures = 2
	m := bound(t, p, nil)
	t1, _ := m.lease.RenewAt()
	t2, _ := m.lease.RebindAt()

	acts := renew(t, m)
	now := t1
	for i := 0; i < 10; i++ {
		send, ok := find(acts, ActSend)
		if !ok {
			t.Fatalf("attempt %d produced no send:\n%v", i, RenderActions(acts))
		}
		if send.Dest.Broadcast {
			t.Fatalf("attempt %d was broadcast while still in RENEWING", i)
		}
		_, acts = m.Step(now, uint64(i), ActionFailed(send.ID, "unicast unresolved"))
		if !m.haveLse {
			t.Fatalf("the lease was dropped after %d refused unicasts; MaxSendFailures is %d and a held lease is not the acquisition path", i+1, p.MaxSendFailures)
		}
		if a, ok := find(acts, ActFailed); ok {
			t.Fatalf("attempt %d reported %s to the caller; a refused renewal is not a broken transport while the lease still has an expiry armed", i, a.Reason)
		}
		d, ok := timerSet(acts, TimerRetransmit)
		if !ok {
			t.Fatalf("attempt %d re-armed nothing; the renewal would stall until T2:\n%v", i, RenderActions(acts))
		}
		now = now.Add(d)
		_, acts = m.Step(now, uint64(i), TimerFired(TimerRetransmit))
	}

	// T2 arrives with the lease still held, and the request goes out
	// BROADCAST — the shape a relay agent can carry.
	_, acts = m.Step(t2, 0x99, TimerFired(TimerRebind))
	_, dest := sent(t, acts, wire.MsgRequest)
	if !dest.Broadcast {
		t.Fatalf("at T2 the request went to %s, not broadcast; the relay case is exactly what REBINDING is for", dest.Addr)
	}
	if !m.haveLse {
		t.Fatal("the lease was lost on the way to REBINDING")
	}
}

// TestT1WithNoServerIdentifierWaitsForT2 drives the lease D17 names: a
// DHCPACK with no option 54. There is no unicast to address, so RENEWING is
// unreachable and BOUND waits for T2, where the request is broadcast and needs
// no server identifier at all.
func TestT1WithNoServerIdentifierWaitsForT2(t *testing.T) {
	m := bound(t, testParams(), func(ack *wire.Message) {
		delete(ack.Options, wire.OptServerID)
	})
	if m.lease.ServerID.IsValid() && !m.lease.ServerID.IsUnspecified() {
		t.Fatalf("fixture lease still carries a server identifier %s", m.lease.ServerID)
	}
	acts := renew(t, m)
	if m.State() != StateBound {
		t.Fatalf("state = %s, want BOUND: there is no server to unicast to", m.State())
	}
	if _, ok := find(acts, ActSend); ok {
		t.Fatalf("something was sent with no server to send it to:\n%v", RenderActions(acts))
	}
	if !journalHas(acts, "no server identifier") {
		t.Fatalf("nothing in the journal says why T1 did nothing:\n%v", RenderActions(acts))
	}

	// The xid the acquisition ran under. It is the one that went out on the
	// wire: the fixture's ACK copies the REQUEST's xid, and a machine that
	// took an ACK under a different one would have discarded it
	// (TestXidMismatchIsDiscarded).
	acqXid := m.xid

	t2, _ := m.lease.RebindAt()
	_, acts = m.Step(t2, 0x22, TimerFired(TimerRebind))
	if m.State() != StateRebinding {
		t.Fatalf("state = %s at T2, want REBINDING", m.State())
	}
	msg, dest := sent(t, acts, wire.MsgRequest)
	if !dest.Broadcast {
		t.Fatal("T2 did not broadcast")
	}
	if !m.haveLse {
		t.Fatal("the lease was dropped rather than rebound")
	}

	// THIS DOOR OPENS A NEW TRANSACTION, and the two fields that say so are
	// on the wire. RENEWING to REBINDING does not (one renewal spans both
	// states, TestRenewalKeepsOneTransactionAcrossT2); BOUND straight to
	// REBINDING is a renewal that never began, so it begins here.
	//
	// Both assertions are needed. The xid alone would pass a machine that
	// reset the transaction but kept counting 'secs' from the acquisition,
	// and 'secs' alone would pass one that reset the clock and reused the
	// xid — either of which offers a server a request it can match to
	// something the client is not doing.
	if msg.XID == acqXid {
		t.Fatalf("the rebind reused the acquisition xid %#08x; T2 out of BOUND is a new transaction", acqXid)
	}
	if msg.Secs != 0 {
		t.Fatalf("secs = %d in the first message of a new transaction, want 0: RFC 2131 section 2 counts it from the moment the client began this acquisition or renewal, and this one began now", msg.Secs)
	}
}

// ------------------------------------------------------- the renewal ACK --

// TestRenewedIsAnnouncedEvenWhenNothingChanged is seam gap G-3.
//
// A caller watching only Changed sees nothing at all through a year of
// successful renewals, and cannot tell a client that is renewing from one that
// is stuck. The identical-contents case is the one that matters, so it is the
// one driven first; the differing case below is its preservation control.
func TestRenewedIsAnnouncedEvenWhenNothingChanged(t *testing.T) {
	m := bound(t, testParams(), nil)
	acts := renew(t, m)
	req, _ := sent(t, acts, wire.MsgRequest)
	t1, _ := m.lease.RenewAt()

	_, acts = m.Step(t1.Add(Second), 0x44, received(t, ackFor(req, testLeaseAddr, testServerID, 3600)))
	if m.State() != StateBound {
		t.Fatalf("state = %s, want BOUND", m.State())
	}
	if _, ok := find(acts, ActLeaseRenewed); !ok {
		t.Fatalf("no ActLeaseRenewed on an ACK that changed nothing:\n%v", RenderActions(acts))
	}
	if _, ok := find(acts, ActLeaseChanged); ok {
		t.Fatalf("ActLeaseChanged on an ACK whose contents are identical:\n%v", RenderActions(acts))
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatalf("ActLeaseAcquired on a renewal; the caller already has this lease:\n%v", RenderActions(acts))
	}
	if _, ok := find(acts, ActLeaseLost); ok {
		t.Fatalf("ActLeaseLost on a successful renewal:\n%v", RenderActions(acts))
	}

	// The deadlines moved: the new lease runs from the renewal REQUEST.
	if m.lease.Start != t1 {
		t.Fatalf("lease start = %s, want the renewal request's %s (RFC 2131 4.4.5)", m.lease.Start, t1)
	}
	for _, id := range []TimerID{TimerExpire, TimerRenew, TimerRebind} {
		if _, ok := timerSet(acts, id); !ok {
			t.Fatalf("timer %s was not re-armed on the renewal:\n%v", id, RenderActions(acts))
		}
	}
	if !timerCancelled(acts, TimerRetransmit) {
		t.Fatalf("the renewal's retransmit timer was left armed:\n%v", RenderActions(acts))
	}
}

// TestRenewalThatChangesTheLeaseAnnouncesBoth is the preservation control on
// the test above, and the assertion that Changed still works.
func TestRenewalThatChangesTheLeaseAnnouncesBoth(t *testing.T) {
	m := bound(t, testParams(), nil)
	acts := renew(t, m)
	req, _ := sent(t, acts, wire.MsgRequest)
	t1, _ := m.lease.RenewAt()

	ack := ackFor(req, testLeaseAddr, testServerID, 3600)
	ack.Options[wire.OptDNSServer] = append(addr4("192.168.99.53"), addr4("192.168.99.54")...)
	_, acts = m.Step(t1.Add(Second), 0x44, received(t, ack))

	iRenewed, iChanged := -1, -1
	for i, a := range acts {
		switch a.Kind {
		case ActLeaseRenewed:
			iRenewed = i
		case ActLeaseChanged:
			iChanged = i
		}
	}
	if iRenewed < 0 || iChanged < 0 {
		t.Fatalf("want both a renewal and a change:\n%v", RenderActions(acts))
	}
	// Renewed carries the new lease and is the caller's record of it; Changed
	// is the instruction to act on the difference. A caller reconfiguring on
	// Changed must already hold what it is reconfiguring to.
	if iChanged < iRenewed {
		t.Fatalf("ActLeaseChanged is announced before ActLeaseRenewed:\n%v", RenderActions(acts))
	}
}

// TestRenewalOntoADifferentAddressIsJournalled: RFC 2131 does not forbid a
// server extending a lease onto a different yiaddr, and it invalidates
// everything the caller configured. It is the one lease change worth naming.
func TestRenewalOntoADifferentAddressIsJournalled(t *testing.T) {
	m := bound(t, testParams(), nil)
	acts := renew(t, m)
	req, _ := sent(t, acts, wire.MsgRequest)
	t1, _ := m.lease.RenewAt()

	_, acts = m.Step(t1.Add(Second), 0x44, received(t, ackFor(req, "192.168.99.77", testServerID, 3600)))
	if !journalHas(acts, "moved the address") {
		t.Fatalf("a renewal onto a different address said nothing:\n%v", RenderActions(acts))
	}
	if _, ok := find(acts, ActLeaseChanged); !ok {
		t.Fatalf("a renewal onto a different address did not announce a change:\n%v", RenderActions(acts))
	}
	if m.lease.Addr.Addr() != netip.MustParseAddr("192.168.99.77") {
		t.Fatalf("held address = %s, want the one the server just granted", m.lease.Addr)
	}
}

// TestNakDuringRenewalDropsTheAddressAndRestarts is RFC 2131 Figure 5's
// "DHCPNAK / Halt network" edge out of RENEWING and REBINDING.
//
// The loss BEFORE the restart is the load-bearing part: a NAK during renewal
// means the server no longer holds this binding, so continuing to use the
// address puts a host on the network with an address somebody else may have.
func TestNakDuringRenewalDropsTheAddressAndRestarts(t *testing.T) {
	for _, st := range []State{StateRenewing, StateRebinding} {
		t.Run(st.String(), func(t *testing.T) {
			m := machineIn(t, st)
			var req *wire.Message
			// The transaction in flight is the renewal's; build the NAK for it.
			req = &wire.Message{XID: m.xid, CHAddr: testCHAddr}
			now := at(4000)
			_, acts := m.Step(now, 0x55, received(t, nakFor(req, testServerID, "lease not found")))

			if m.State() != StateSelecting {
				t.Fatalf("state = %s, want SELECTING (Figure 5 lands in INIT and INIT sends at once)", m.State())
			}
			if m.haveLse {
				t.Fatal("the lease is still held after a DHCPNAK")
			}
			iLost, iFailed, iSend := -1, -1, -1
			for i, a := range acts {
				switch {
				case a.Kind == ActLeaseLost:
					iLost = i
				case a.Kind == ActFailed:
					iFailed = i
				case a.Kind == ActSend && iSend < 0:
					iSend = i
				}
			}
			if iLost < 0 {
				t.Fatalf("no ActLeaseLost:\n%v", RenderActions(acts))
			}
			if a, _ := find(acts, ActLeaseLost); a.Reason != ReasonNak {
				t.Fatalf("lost for %s, want %s", a.Reason, ReasonNak)
			}
			if iFailed < 0 || iSend < 0 {
				t.Fatalf("want both a failure report and a new DISCOVER:\n%v", RenderActions(acts))
			}
			if iLost > iSend {
				t.Fatalf("the DISCOVER is announced before the loss; a caller draining this list would still be using the address:\n%v", RenderActions(acts))
			}
			if msg, _ := sent(t, acts, wire.MsgDiscover); msg.CIAddr.IsValid() && !msg.CIAddr.IsUnspecified() {
				t.Fatalf("the new DISCOVER carries ciaddr %s", msg.CIAddr)
			}
		})
	}
}

// TestExpiryDuringRenewalDropsTheAddress is section 4.4.5's "If the lease
// expires before the client receives a DHCPACK, the client moves to INIT
// state".
func TestExpiryDuringRenewalDropsTheAddress(t *testing.T) {
	for _, st := range []State{StateRenewing, StateRebinding} {
		t.Run(st.String(), func(t *testing.T) {
			m := machineIn(t, st)
			exp, ok := m.lease.Expire()
			if !ok {
				t.Fatal("the fixture lease has no expiry")
			}
			_, acts := m.Step(exp, 0x66, TimerFired(TimerExpire))
			if m.haveLse {
				t.Fatal("the lease is still held past its expiry")
			}
			a, ok := find(acts, ActLeaseLost)
			if !ok || a.Reason != ReasonExpired {
				t.Fatalf("want a loss for %s:\n%v", ReasonExpired, RenderActions(acts))
			}
			if m.State() != StateSelecting {
				t.Fatalf("state = %s, want SELECTING", m.State())
			}
		})
	}
}

// TestReleaseDuringRenewalSendsTheRelease: a lease IS held in RENEWING, so
// this is RFC 2131 section 4.4.6's real DHCPRELEASE and not the silent halt
// that releaseBeforeBound does.
func TestReleaseDuringRenewalSendsTheRelease(t *testing.T) {
	m := machineIn(t, StateRenewing)
	_, acts := m.Step(at(2000), 0x77, Simple(EvRelease))
	msg, dest := sent(t, acts, wire.MsgRelease)
	if msg.CIAddr != netip.MustParseAddr(testLeaseAddr) {
		t.Fatalf("DHCPRELEASE ciaddr = %s, want %s", msg.CIAddr, testLeaseAddr)
	}
	if dest.Broadcast {
		t.Fatal("DHCPRELEASE was broadcast; RFC 2131 4.4.4 unicasts it")
	}
	if m.State() != StateStopped {
		t.Fatalf("state = %s, want STOPPED", m.State())
	}
}

// TestUnsolicitedMessagesInBoundAreDiscarded: BOUND has no transaction open,
// so a retransmitted DHCPACK for the transaction that produced the lease must
// not restart the timers.
func TestUnsolicitedMessagesInBoundAreDiscarded(t *testing.T) {
	m := bound(t, testParams(), nil)
	req := &wire.Message{XID: m.xid, CHAddr: testCHAddr}
	_, acts := m.Step(at(100), 0x88, received(t, ackFor(req, testLeaseAddr, testServerID, 7200)))
	if _, ok := find(acts, ActSetTimer); ok {
		t.Fatalf("a DHCPACK in BOUND re-armed a timer:\n%v", RenderActions(acts))
	}
	if _, ok := find(acts, ActLeaseRenewed); ok {
		t.Fatalf("a DHCPACK in BOUND was taken as a renewal:\n%v", RenderActions(acts))
	}
	if m.lease.LeaseTime != 3600*Second {
		t.Fatalf("lease time = %s; an unsolicited ACK extended the lease", m.lease.LeaseTime)
	}
}

// ----------------------------------------------------------- server policy --

// TestServerPolicyFiltersEveryInboundMessage is P-4.
//
// The DHCPNAK rows are the ones that decide where the filter goes: a policy
// attached to the DHCPOFFER alone lets a server the operator excluded NAK this
// client out of a lease it never gave, and one attached to OFFER and ACK still
// lets it do so during a renewal. So the filter sits in acceptable, which
// every inbound message passes through.
func TestServerPolicyFiltersEveryInboundMessage(t *testing.T) {
	allow := netip.MustParseAddr(testServerID)
	other := netip.MustParseAddr("192.168.99.9")

	t.Run("an allowed server still works", func(t *testing.T) {
		p := testParams()
		p.Servers.Allow = []netip.Addr{allow}
		m := bound(t, p, nil)
		if !m.haveLse {
			t.Fatal("a lease from an allowed server was refused")
		}
	})

	t.Run("an offer from a server outside the allow list is discarded", func(t *testing.T) {
		p := testParams()
		p.Servers.Allow = []netip.Addr{other}
		m := newMachine(t, p)
		_, acts := m.Step(at(0), 1, Simple(EvStart))
		disc := mustSend(t, acts, wire.MsgDiscover)
		_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testLeaseAddr, testServerID)))
		if m.State() != StateSelecting {
			t.Fatalf("state = %s, want SELECTING", m.State())
		}
		if !journalHas(acts, "not on the allow list") {
			t.Fatalf("nothing says why:\n%v", RenderActions(acts))
		}
	})

	t.Run("a denied server cannot NAK a held lease away", func(t *testing.T) {
		p := testParams()
		p.Servers.Deny = []netip.Addr{other}
		m := bound(t, p, nil)
		acts := renew(t, m)
		req, _ := sent(t, acts, wire.MsgRequest)
		_, acts = m.Step(at(2000), 0x99, received(t, nakFor(req, other.String(), "not yours")))
		if !m.haveLse {
			t.Fatal("a DHCPNAK from a denied server ended the lease")
		}
		if m.State() != StateRenewing {
			t.Fatalf("state = %s, want RENEWING", m.State())
		}
		if !journalHas(acts, "deny list") {
			t.Fatalf("nothing says why:\n%v", RenderActions(acts))
		}
	})

	t.Run("deny wins over allow", func(t *testing.T) {
		s := ServerPolicy{Allow: []netip.Addr{allow}, Deny: []netip.Addr{allow}}
		if ok, why := s.permits(allow, true); ok {
			t.Fatalf("a server on both lists was permitted (%q)", why)
		}
	})

	t.Run("an allow list fails closed on an absent identifier", func(t *testing.T) {
		s := ServerPolicy{Allow: []netip.Addr{allow}}
		if ok, _ := s.permits(netip.Addr{}, false); ok {
			t.Fatal("a message with no server identifier satisfied an allow list")
		}
	})

	t.Run("a deny list alone fails open on an absent identifier", func(t *testing.T) {
		s := ServerPolicy{Deny: []netip.Addr{other}}
		if ok, why := s.permits(netip.Addr{}, false); !ok {
			t.Fatalf("a deny list refused a message that names no server: %q", why)
		}
	})

	t.Run("an empty policy permits everything", func(t *testing.T) {
		var s ServerPolicy
		if ok, why := s.permits(other, true); !ok {
			t.Fatalf("the zero policy refused %s: %q", other, why)
		}
		if ok, why := s.permits(netip.Addr{}, false); !ok {
			t.Fatalf("the zero policy refused a message with no identifier: %q", why)
		}
	})
}

// ------------------------------------------------------------- option set --

// TestParameterListRequestsClasslessRoutesFirst is RFC 3442's ordering MUST:
// "The Classless Static Routes option code MUST appear in the parameter
// request list prior to both the Router option code and the Static Routes
// option code, if present."
//
// Asserted on the ENCODED option and not on the Params slice: the list is
// copied, normalised and serialised between the two, and it is the bytes on
// the wire the MUST is about.
func TestParameterListRequestsClasslessRoutesFirst(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	pl, ok := disc.Options[wire.OptParameterList]
	if !ok {
		t.Fatal("no parameter request list")
	}
	idx := func(c wire.OptionCode) int {
		for i, b := range pl {
			if wire.OptionCode(b) == c {
				return i
			}
		}
		return -1
	}
	classless := idx(wire.OptClasslessStaticRte)
	if classless < 0 {
		t.Fatal("option 121 is not requested at all; RFC 3442 requires a client that supports it to ask for it")
	}
	if r := idx(wire.OptRouter); r < 0 {
		t.Fatal("option 3 is not requested; RFC 3442 requires BOTH")
	} else if classless > r {
		t.Fatalf("option 121 is at %d and option 3 at %d: RFC 3442 makes the order a MUST\nlist: %v", classless, r, pl)
	}
	if s := idx(wire.OptStaticRoute); s >= 0 && classless > s {
		t.Fatalf("option 121 is at %d and option 33 at %d: RFC 3442 makes the order a MUST\nlist: %v", classless, s, pl)
	}
	// Every code appears once: a duplicated request is a longer message for
	// no benefit, and the pin catches a merge that appended rather than moved.
	seen := map[byte]bool{}
	for _, b := range pl {
		if seen[b] {
			t.Fatalf("option %d appears twice in the parameter request list %v", b, pl)
		}
		seen[b] = true
	}
}

// TestLeaseDecodesTheOptionSet is P-5 end to end: what a DHCPACK carries
// becomes what Lease exposes.
func TestLeaseDecodesTheOptionSet(t *testing.T) {
	m := bound(t, testParams(), func(ack *wire.Message) {
		ack.Options[wire.OptRouter] = append(addr4("192.168.99.1"), addr4("192.168.99.2")...)
		ack.Options[wire.OptDNSServer] = append(addr4("192.168.99.53"), addr4("192.168.99.54")...)
		ack.Options[wire.OptInterfaceMTU] = []byte{0x05, 0x78}
		ack.Options[wire.OptDomainName] = []byte("lan.example")
		ack.Options[wire.OptDomainSearch] = []byte{
			3, 'e', 'n', 'g', 3, 'l', 'a', 'n', 0, 0xC0, 0x04,
		}
	})
	l := m.lease
	if l.MTU != 1400 {
		t.Fatalf("MTU = %d, want 1400", l.MTU)
	}
	if l.Domain != "lan.example" {
		t.Fatalf("domain = %q", l.Domain)
	}
	if len(l.DNS) != 2 || l.DNS[1] != netip.MustParseAddr("192.168.99.54") {
		t.Fatalf("dns = %v", l.DNS)
	}
	if len(l.Router) != 2 {
		t.Fatalf("router = %v, want both addresses", l.Router)
	}
	if g, ok := l.Gateway(); !ok || g != netip.MustParseAddr("192.168.99.1") {
		t.Fatalf("gateway = %v, %v", g, ok)
	}
	want := []string{"eng.lan", "lan"}
	if len(l.DomainSearch) != 2 || l.DomainSearch[0] != want[0] || l.DomainSearch[1] != want[1] {
		t.Fatalf("domain search = %q, want %q", l.DomainSearch, want)
	}
	if l.Addr.Bits() != 24 {
		t.Fatalf("prefix = %s, want /24 from the subnet mask", l.Addr)
	}
}

// TestClasslessRoutesSupersedeTheRouterOption is RFC 3442's "If the DHCP
// server returns both a Classless Static Routes option and a Router option,
// the DHCP client MUST ignore the Router option."
func TestClasslessRoutesSupersedeTheRouterOption(t *testing.T) {
	m := bound(t, testParams(), func(ack *wire.Message) {
		ack.Options[wire.OptRouter] = addr4("192.168.99.254")
		ack.Options[wire.OptStaticRoute] = append(addr4("10.0.0.0"), addr4("192.168.99.253")...)
		ack.Options[wire.OptClasslessStaticRte] = append(
			append([]byte{0}, addr4("192.168.99.1")...),
			append([]byte{24, 10, 0, 0}, addr4("192.168.99.2")...)...)
	})
	l := m.lease
	if len(l.Router) != 0 {
		t.Fatalf("Router = %v; RFC 3442 makes ignoring option 3 a MUST when 121 is present", l.Router)
	}
	if len(l.Routes) != 2 {
		t.Fatalf("Routes = %v, want option 121's two", l.Routes)
	}
	g, ok := l.Gateway()
	if !ok || g != netip.MustParseAddr("192.168.99.1") {
		t.Fatalf("gateway = %v, %v; want option 121's default route and NOT option 3's 192.168.99.254", g, ok)
	}
	for _, r := range l.Routes {
		if r.Router == netip.MustParseAddr("192.168.99.253") {
			t.Fatal("a route from option 33 survived; RFC 3442 makes ignoring it a MUST too")
		}
	}
}

// TestClasslessRoutesWithNoDefaultRouteLeaveNoGateway is the other half of
// RFC 3442's supersession, and the half that is easy to get wrong in the
// forgiving direction: option 121 decoded, but carrying no 0.0.0.0/0 entry.
// The Router option is still ignored — the RFC conditions the MUST on the two
// options being returned together, not on the classless list being useful —
// so the answer is a lease with routes and no gateway, not a lease with
// option 3's gateway.
func TestClasslessRoutesWithNoDefaultRouteLeaveNoGateway(t *testing.T) {
	m := bound(t, testParams(), func(ack *wire.Message) {
		ack.Options[wire.OptRouter] = addr4("192.168.99.254")
		ack.Options[wire.OptClasslessStaticRte] = append([]byte{24, 10, 0, 0}, addr4("192.168.99.2")...)
	})
	l := m.lease
	if len(l.Routes) != 1 {
		t.Fatalf("Routes = %v, want option 121's one", l.Routes)
	}
	if g, ok := l.Gateway(); ok {
		t.Fatalf("gateway = %v; option 121 carried no default route, so option 3's must not stand in for it", g)
	}
	if len(l.Router) != 0 {
		t.Fatalf("Router = %v; RFC 3442 makes ignoring option 3 a MUST whenever option 121 is present", l.Router)
	}
}

// TestMalformedClasslessRoutesFallsBackToTheOlderOptions holds the decision
// that a value which does not decode is not a supersession. A host with no
// default route at all is a worse answer than one with the route option 3
// gave, and the fallback is journalled because the two outcomes are otherwise
// indistinguishable from outside.
func TestMalformedClasslessRoutesFallsBackToTheOlderOptions(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testLeaseAddr, testServerID)))
	req := mustSend(t, acts, wire.MsgRequest)
	ack := ackFor(req, testLeaseAddr, testServerID, 3600)
	ack.Options[wire.OptRouter] = addr4("192.168.99.254")
	ack.Options[wire.OptStaticRoute] = append(addr4("10.9.9.9"), addr4("192.168.99.253")...)
	ack.Options[wire.OptClasslessStaticRte] = []byte{24, 10, 0}
	_, acts = m.Step(at(2), 3, received(t, ack))

	if g, ok := m.lease.Gateway(); !ok || g != netip.MustParseAddr("192.168.99.254") {
		t.Fatalf("gateway = %v, %v; want option 3's, because option 121 did not decode", g, ok)
	}
	// Both older options, not just the one the gateway comes from: the
	// supersession that did not happen covers 3 AND 33.
	want := wire.Route{Dest: netip.MustParsePrefix("10.9.9.9/32"), Router: netip.MustParseAddr("192.168.99.253")}
	found := false
	for _, r := range m.lease.Routes {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("routes = %v; want the static route %v the fallback should have taken from option 33", m.lease.Routes, want)
	}
	if !journalHas(acts, "falling back") {
		t.Fatalf("the fallback was silent:\n%v", RenderActions(acts))
	}
}

// TestFqdnReplacesTheHostNameOption is RFC 4702 section 3.1: "clients that
// send the Client FQDN option in their messages MUST NOT also send the Host
// Name option".
func TestFqdnReplacesTheHostNameOption(t *testing.T) {
	p := testParams()
	p.Hostname = "host"
	p.FQDN = FQDN{Name: "host.example.test."}
	m := newMachine(t, p)
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	v, ok := disc.Options[wire.OptFQDN]
	if !ok {
		t.Fatal("option 81 was not sent")
	}
	if v[0] != DefaultFQDNFlags {
		t.Fatalf("flags = %#02x, want the default %#02x", v[0], DefaultFQDNFlags)
	}
	if _, ok := disc.Options[wire.OptHostName]; ok {
		t.Fatal("option 12 was sent beside option 81; RFC 4702 section 3.1 makes that a MUST NOT")
	}

	// And the renewal carries it too: section 3.1's obligation is on "their
	// messages", and a server that got the FQDN only at acquisition would
	// drop the DNS record at the first renewal from a client that stopped.
	m2 := bound(t, p, nil)
	acts = renew(t, m2)
	req, _ := sent(t, acts, wire.MsgRequest)
	if _, ok := req.Options[wire.OptFQDN]; !ok {
		t.Fatal("the renewal does not carry option 81")
	}
	if _, ok := req.Options[wire.OptHostName]; ok {
		t.Fatal("the renewal carries option 12 beside option 81")
	}
}

// TestHostNameSurvivesWithoutAnFqdn is the preservation control on the test
// above: the suppression must be caused by option 81 and by nothing else.
func TestHostNameSurvivesWithoutAnFqdn(t *testing.T) {
	p := testParams()
	p.Hostname = "host"
	m := newMachine(t, p)
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	if v, ok := disc.Options[wire.OptHostName]; !ok || string(v) != "host" {
		t.Fatalf("option 12 = %q, %v; want it sent when there is no option 81", v, ok)
	}
	if _, ok := disc.Options[wire.OptFQDN]; ok {
		t.Fatal("option 81 was sent for a zero Params.FQDN")
	}
}

// TestNewRefusesAnUnencodableFqdn: refused at construction, not at the first
// DHCPDISCOVER, so a caller finds out before the machine is running.
func TestNewRefusesAnUnencodableFqdn(t *testing.T) {
	p := testParams()
	p.FQDN = FQDN{Name: "host..example."}
	if _, err := New(p); err == nil {
		t.Fatal("New accepted a name that cannot be encoded")
	}
	p.FQDN = FQDN{Name: "host.", Flags: wire.FQDNFlagO}
	if _, err := New(p); err == nil {
		t.Fatal("New accepted the O bit, which RFC 4702 2.1 says a client MUST set to 0")
	}
	// The ASCII form, E clear: the case review round 1 found reaching the
	// transport. ErrBadFQDN's doc says "refused at construction rather than
	// at the first DHCPDISCOVER", and this is the row that holds it — a name
	// this long used to build a machine that failed every send and reported
	// a broken transport.
	p.FQDN = FQDN{Name: strings.Repeat("abcdefghij.", 30), Flags: wire.FQDNFlagS}
	if _, err := New(p); err == nil {
		t.Fatal("New accepted a 330-octet name with the E bit clear; every send it makes would be refused by Encode instead")
	}
}

// ----------------------------------------------------------------- STOPPED --

// TestStoppedDiscardsALateAck is carried from M2's review, where the mutant
// "STOPPED consumes a matching ACK" survived because nothing asserted it.
//
// The window is real: releaseBeforeBound halts from REQUESTING with a
// DHCPREQUEST still on the wire, and a server may answer it. RFC 2131 section
// 4.4.1's "Any arriving DHCPACK messages must be silently discarded" is what
// STOPPED must do — and silently to the WIRE, not to the journal.
func TestStoppedDiscardsALateAck(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(at(0), 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, testLeaseAddr, testServerID)))
	req := mustSend(t, acts, wire.MsgRequest)

	if _, acts = m.Step(at(2), 3, Simple(EvRelease)); m.State() != StateStopped {
		t.Fatalf("state = %s, want STOPPED", m.State())
	}

	// The ACK the server sends anyway: same xid, same chaddr, well formed.
	// Nothing about it is refusable except the state.
	st, acts := m.Step(at(3), 4, received(t, ackFor(req, testLeaseAddr, testServerID, 3600)))
	if st != StateStopped {
		t.Fatalf("a late DHCPACK moved a stopped machine to %s", st)
	}
	if _, ok := m.Lease(); ok {
		t.Fatal("a stopped machine took a lease from a late DHCPACK")
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatalf("a stopped machine announced a lease:\n%v", RenderActions(acts))
	}
	if _, ok := find(acts, ActSetTimer); ok {
		t.Fatalf("a stopped machine armed a timer:\n%v", RenderActions(acts))
	}
	if _, ok := find(acts, ActSend); ok {
		t.Fatalf("a stopped machine sent something:\n%v", RenderActions(acts))
	}
	if !journalHas(acts, "STOPPED") {
		t.Fatalf("the discard is not in the journal; silent to the wire is not silent to the operator:\n%v", RenderActions(acts))
	}

	// The same for a machine stopped from BOUND: a DHCPACK that arrives after
	// the DHCPRELEASE must not resurrect the lease.
	m2 := bound(t, testParams(), nil)
	renewActs := renew(t, m2)
	renewReq, _ := sent(t, renewActs, wire.MsgRequest)
	m2.Step(at(2000), 5, Simple(EvRelease))
	if m2.State() != StateStopped {
		t.Fatalf("state = %s, want STOPPED", m2.State())
	}
	_, acts = m2.Step(at(2001), 6, received(t, ackFor(renewReq, testLeaseAddr, testServerID, 3600)))
	if _, ok := m2.Lease(); ok {
		t.Fatal("a released machine took the lease back from a late DHCPACK")
	}
	if _, ok := find(acts, ActLeaseRenewed); ok {
		t.Fatalf("a released machine renewed:\n%v", RenderActions(acts))
	}
}
