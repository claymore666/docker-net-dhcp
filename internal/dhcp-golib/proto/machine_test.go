package proto

import (
	"bytes"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// -------------------------------------------------------------- totality --

// TestStepIsTotal is requirement R1 over the whole product, not a sample.
//
// It also drives EVERY state, including StateStopped, and every event kind
// including ones no state handles. The failure it exists to catch is a nil
// dereference or an index panic in ring 1, which in the plugin is not a
// returned error — it is the network driver going down with the daemon.
func TestStepIsTotal(t *testing.T) {
	states := AllStates()
	kinds := AllEventKinds()
	if len(states) == 0 || len(kinds) == 0 {
		t.Fatal("AllStates or AllEventKinds is empty; this test would measure nothing")
	}

	// The event payloads are deliberately hostile: a nil message, a message
	// with no type, a timer id outside the set. A totality test built only
	// from well-formed events measures the happy path twice.
	payloads := []struct {
		name string
		ev   func(*Machine) Event
	}{
		{"bare", func(*Machine) Event { return Event{} }},
		{"nil message", func(*Machine) Event { return Event{Msg: nil} }},
		{"typeless message", func(*Machine) Event {
			return Event{Msg: &wire.Message{Op: wire.BootReply, CHAddr: testCHAddr}}
		}},
		{"matching ack", func(m *Machine) Event {
			req := &wire.Message{XID: m.xid, CHAddr: testCHAddr}
			return Event{Msg: ackFor(req, "192.168.99.50", "192.168.99.1", 3600)}
		}},
		{"out-of-range timer", func(*Machine) Event { return Event{Timer: TimerID(200)} }},
	}

	for _, st := range states {
		for _, k := range kinds {
			for _, pl := range payloads {
				name := st.String() + "/" + k.String() + "/" + pl.name
				t.Run(name, func(t *testing.T) {
					m := machineIn(t, st)
					if m.State() != st {
						t.Fatalf("fixture is in %s, not %s", m.State(), st)
					}
					ev := pl.ev(m)
					ev.Kind = k
					next, acts := m.Step(at(1000), 0x1234_5678_9ABC_DEF0, ev)
					if !validState(next) {
						t.Fatalf("Step returned undefined state %d", next)
					}
					for i, a := range acts {
						if a.Kind == ActSend && a.Msg == nil {
							t.Fatalf("action %d is a Send with no message", i)
						}
					}
				})
			}
		}
	}
}

func validState(s State) bool {
	for _, x := range AllStates() {
		if x == s {
			return true
		}
	}
	return false
}

// machineIn returns a machine sitting in the named state, reached by real
// transitions. Constructing one by assignment would prove nothing: the states
// a test can build by hand include ones the machine cannot reach.
func TestAcquisition(t *testing.T) {
	m := newMachine(t, testParams())

	// Start: no desync configured, so the DISCOVER goes out at once.
	st, acts := m.Step(0, 0xAAAA, Simple(EvStart))
	if st != StateSelecting {
		t.Fatalf("after Start: %s, want SELECTING", st)
	}
	disc := mustSend(t, acts, wire.MsgDiscover)
	if disc.Op != wire.BootRequest {
		t.Fatalf("DISCOVER op = %s, want BOOTREQUEST", disc.Op)
	}
	if disc.Flags&wire.FlagBroadcast == 0 {
		t.Fatal("DISCOVER does not set the BROADCAST flag; a raw-socket client cannot receive the unicast reply")
	}
	if !disc.CIAddr.IsUnspecified() && disc.CIAddr.IsValid() {
		t.Fatalf("DISCOVER ciaddr = %s, want unset (RFC 2131 4.4.1)", disc.CIAddr)
	}
	if _, ok := disc.Options[wire.OptParameterList]; !ok {
		t.Fatal("DISCOVER carries no parameter request list")
	}
	if a, ok := find(acts, ActSetTimer); !ok || a.Timer != TimerRetransmit {
		t.Fatalf("no retransmit timer armed after DISCOVER: %v", RenderActions(acts))
	}

	// OFFER.
	st, acts = m.Step(at(1), 0xBBBB, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	if st != StateRequesting {
		t.Fatalf("after OFFER: %s, want REQUESTING", st)
	}
	req := mustSend(t, acts, wire.MsgRequest)

	// RFC 2131 section 4.4.1's table for a REQUEST sent from SELECTING.
	if got, ok := req.Addr4(wire.OptRequestedIP); !ok || got.String() != "192.168.99.50" {
		t.Fatalf("REQUEST requested-IP = %v/%v, want the OFFER's yiaddr (MUST)", got, ok)
	}
	if got, ok := req.Addr4(wire.OptServerID); !ok || got.String() != "192.168.99.1" {
		t.Fatalf("REQUEST server-identifier = %v/%v, want the OFFER's (MUST)", got, ok)
	}
	if req.CIAddr.IsValid() && !req.CIAddr.IsUnspecified() {
		t.Fatalf("REQUEST ciaddr = %s, want zero when sent from SELECTING (MUST)", req.CIAddr)
	}
	if req.XID != disc.XID {
		t.Fatalf("REQUEST xid %#x != DISCOVER xid %#x; they are one transaction", req.XID, disc.XID)
	}
	if !bytesEqual(req.Options[wire.OptParameterList], disc.Options[wire.OptParameterList]) {
		t.Fatal("REQUEST parameter list differs from the DISCOVER's (RFC 2131 4.4.1 MUST)")
	}

	// ACK.
	st, acts = m.Step(at(2), 0xCCCC, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	if st != StateBound {
		t.Fatalf("after ACK: %s, want BOUND", st)
	}
	got, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatalf("no LeaseAcquired: %v", RenderActions(acts))
	}
	l := got.Lease
	if l.Addr.String() != "192.168.99.50/24" {
		t.Fatalf("lease addr = %s, want 192.168.99.50/24", l.Addr)
	}
	if l.ServerID.String() != "192.168.99.1" {
		t.Fatalf("server id = %s", l.ServerID)
	}
	if l.Domain != "example.test" {
		t.Fatalf("domain = %q", l.Domain)
	}
	if l.MTU != 1500 {
		t.Fatalf("mtu = %d, want 1500", l.MTU)
	}

	// RFC 2131 section 4.4.5: the lease clock starts when the REQUEST was
	// SENT, not when the ACK arrived. The REQUEST left at t=1s, so the lease
	// runs out at 3601s, not 3602s. One second is invisible on a fixture and
	// is exactly the error that makes a client hold an address past expiry.
	if l.Start != at(1) {
		t.Fatalf("lease Start = %s, want the REQUEST send time 1s (RFC 2131 4.4.5)", Duration(l.Start))
	}
	exp, ok := l.Expire()
	if !ok || exp != at(3601) {
		t.Fatalf("expiry = %v/%v, want 3601s", exp, ok)
	}

	// And the expiry timer is armed for the REMAINING time, not the full lease.
	var armed Action
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerExpire {
			armed = a
		}
	}
	if armed.Kind != ActSetTimer {
		t.Fatalf("no expiry timer armed: %v", RenderActions(acts))
	}
	if armed.After != 3599*Second {
		t.Fatalf("expiry armed for %s at t=2s, want 3599s (expiry minus now), not the lease duration", armed.After)
	}

	// The retransmit timer is cancelled on entering BOUND: a live retransmit
	// timer in BOUND resends a REQUEST for a lease we already hold.
	var cancelled bool
	for _, a := range acts {
		if a.Kind == ActCancelTimer && a.Timer == TimerRetransmit {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("retransmit timer not cancelled on entering BOUND: %v", RenderActions(acts))
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExpiryDropsTheLeaseAndReacquires(t *testing.T) {
	// RFC 2131 section 4.4.5: on expiry the client "moves to INIT state, MUST
	// immediately stop any other network processing and requests network
	// initialization parameters as if the client were uninitialized".
	m := machineIn(t, StateBound)
	st, acts := m.Step(at(4000), 7, TimerFired(TimerExpire))

	lost, ok := find(acts, ActLeaseLost)
	if !ok {
		t.Fatalf("no LeaseLost on expiry: %v", RenderActions(acts))
	}
	if lost.Reason != ReasonExpired {
		t.Fatalf("LeaseLost reason = %s, want expired", lost.Reason)
	}
	// The order matters: the caller must be told to stop using the address
	// BEFORE the client starts asking for a new one.
	lostAt, sendAt := -1, -1
	for i, a := range acts {
		if a.Kind == ActLeaseLost && lostAt < 0 {
			lostAt = i
		}
		if a.Kind == ActSend && sendAt < 0 {
			sendAt = i
		}
	}
	if sendAt >= 0 && lostAt > sendAt {
		t.Fatalf("LeaseLost is emitted after the new DISCOVER: %v", RenderActions(acts))
	}
	if st != StateSelecting {
		t.Fatalf("after expiry: %s, want SELECTING (re-acquiring)", st)
	}
	if _, held := m.Lease(); held {
		t.Fatal("machine still holds the expired lease")
	}
	mustSend(t, acts, wire.MsgDiscover)
}

func TestNakRestarts(t *testing.T) {
	// RFC 2131 section 3.1(5): "If the client receives a DHCPNAK message, the
	// client restarts the configuration process."
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)

	st, acts := m.Step(at(2), 3, received(t, nakFor(req, "192.168.99.1", "lease not available")))
	if st != StateSelecting {
		t.Fatalf("after NAK: %s, want SELECTING", st)
	}
	f, ok := find(acts, ActFailed)
	if !ok {
		t.Fatalf("no Failed action on NAK: %v", RenderActions(acts))
	}
	if f.Reason != ReasonNak {
		t.Fatalf("Failed reason = %s, want nak", f.Reason)
	}
	if f.Note == "" {
		t.Fatal("NAK note is empty; the server's message is the only diagnosis a user gets")
	}
	newDisc := mustSend(t, acts, wire.MsgDiscover)
	if newDisc.XID == disc.XID {
		t.Fatal("restart reused the xid; a restart is a new transaction (RFC 2131 4.4.1)")
	}
}

func TestOfferWithoutServerIDIsDiscarded(t *testing.T) {
	// The REQUEST that follows MUST carry the server identifier. An OFFER
	// without one cannot produce a conformant REQUEST, so it is refused rather
	// than half-used.
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	bad := offerFor(disc, "192.168.99.50", "192.168.99.1")
	delete(bad.Options, wire.OptServerID)

	st, acts := m.Step(at(1), 2, received(t, bad))
	if st != StateSelecting {
		t.Fatalf("state = %s, want to stay in SELECTING", st)
	}
	if count(acts, ActSend) != 0 {
		t.Fatalf("a REQUEST was sent for an OFFER with no server identifier: %v", RenderActions(acts))
	}
}

func TestOfferWithoutYiaddrIsDiscarded(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	bad := offerFor(disc, "192.168.99.50", "192.168.99.1")
	bad.YIAddr = netip.Addr{}

	st, acts := m.Step(at(1), 2, received(t, bad))
	if st != StateSelecting || count(acts, ActSend) != 0 {
		t.Fatalf("OFFER with no yiaddr was acted on: state %s, %v", st, RenderActions(acts))
	}
}

func TestAckInSelectingIsDiscarded(t *testing.T) {
	// RFC 2131 section 4.4.1: "Any arriving DHCPACK messages must be silently
	// discarded." A client that took it would be BOUND to an address it never
	// requested.
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	st, acts := m.Step(at(1), 2, received(t, ackFor(disc, "192.168.99.50", "192.168.99.1", 3600)))
	if st != StateSelecting {
		t.Fatalf("state = %s after an unsolicited ACK, want SELECTING", st)
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatalf("an ACK in SELECTING produced a lease: %v", RenderActions(acts))
	}
}

func TestXidMismatchIsDiscarded(t *testing.T) {
	// RFC 2131 section 4.4.1: "If the 'xid' of an arriving DHCPOFFER message
	// does not match the 'xid' of the most recent DHCPDISCOVER message, the
	// DHCPOFFER message must be silently discarded."
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	other := offerFor(disc, "192.168.99.50", "192.168.99.1")
	other.XID = disc.XID ^ 0xFFFF

	st, acts := m.Step(at(1), 2, received(t, other))
	if st != StateSelecting || count(acts, ActSend) != 0 {
		t.Fatalf("a foreign xid was accepted: state %s, %v", st, RenderActions(acts))
	}
}

func TestChaddrMismatchIsDiscarded(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	other := offerFor(disc, "192.168.99.50", "192.168.99.1")
	other.CHAddr = []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01}

	st, acts := m.Step(at(1), 2, received(t, other))
	if st != StateSelecting || count(acts, ActSend) != 0 {
		t.Fatalf("another host's chaddr was accepted: state %s, %v", st, RenderActions(acts))
	}
}

func TestBootRequestReplyIsDiscarded(t *testing.T) {
	// Our own broadcast DISCOVER comes back on a raw socket bound to the same
	// link. A machine that accepted a BOOTREQUEST would answer itself.
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	echo := disc.Clone()
	st, acts := m.Step(at(1), 2, received(t, echo))
	if st != StateSelecting || count(acts, ActSend) != 0 {
		t.Fatalf("the client acted on its own DISCOVER: state %s, %v", st, RenderActions(acts))
	}
}

// ---------------------------------------------------------- retransmission --

func TestRetransmissionBudgetThenRestart(t *testing.T) {
	// RFC 2131 section 3.1(5) again: the retransmission algorithm is bounded,
	// and when it is exhausted the client "reverts to INIT state and restarts
	// the initialization process", notifying the user.
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	first := mustSend(t, acts, wire.MsgDiscover)

	budget := testParams().Discover.MaxRetransmissions
	now := Instant(0)
	for i := 0; i < budget; i++ {
		now = now.Add(10 * Second)
		st, acts := m.Step(now, uint64(100+i), TimerFired(TimerRetransmit))
		if st != StateSelecting {
			t.Fatalf("retransmission %d left SELECTING: %s", i+1, st)
		}
		again := mustSend(t, acts, wire.MsgDiscover)
		if again.XID != first.XID {
			t.Fatalf("retransmission %d used a new xid; a retransmission is the SAME transaction", i+1)
		}
		if again.Secs == 0 {
			t.Fatalf("retransmission %d has secs=0; the field counts from the start of acquisition", i+1)
		}
	}

	now = now.Add(10 * Second)
	st, acts := m.Step(now, 999, TimerFired(TimerRetransmit))
	f, ok := find(acts, ActFailed)
	if !ok {
		t.Fatalf("budget exhausted with no Failed notification: %v", RenderActions(acts))
	}
	if f.Reason != ReasonNoServer {
		t.Fatalf("Failed reason = %s, want no-server", f.Reason)
	}
	if st != StateSelecting {
		t.Fatalf("state = %s, want a restarted acquisition", st)
	}
	restart := mustSend(t, acts, wire.MsgDiscover)
	if restart.XID == first.XID {
		t.Fatal("the restart reused the xid; it is a new transaction")
	}
}

func TestSecsSaturates(t *testing.T) {
	// A wrap makes a client that has been trying for eighteen hours look like
	// one that just started, which is the opposite of what a relay reads the
	// field for.
	m := newMachine(t, testParams())
	m.Step(0, 1, Simple(EvStart))
	_, acts := m.Step(at(200000), 2, TimerFired(TimerRetransmit))
	msg := mustSend(t, acts, wire.MsgDiscover)
	if msg.Secs != 65535 {
		t.Fatalf("secs = %d after 200000s, want it saturated at 65535", msg.Secs)
	}
}

func TestDesyncDelay(t *testing.T) {
	// RFC 2131 section 4.4.1: "The client SHOULD wait a random time between
	// one and ten seconds to desynchronize the use of DHCP at startup."
	p := DefaultParams(testCHAddr)
	if p.DesyncMin != 1*Second || p.DesyncMax != 10*Second {
		t.Fatalf("default desync window is %s..%s, want 1s..10s", p.DesyncMin, p.DesyncMax)
	}
	for i := 0; i < 500; i++ {
		m := newMachine(t, p)
		st, acts := m.Step(0, uint64(i)*0x9E3779B97F4A7C15, Simple(EvStart))
		if st != StateInit {
			t.Fatalf("Start with desync configured went straight to %s; the delay was skipped", st)
		}
		if count(acts, ActSend) != 0 {
			t.Fatalf("a DISCOVER was sent before the desync delay elapsed: %v", RenderActions(acts))
		}
		var armed Action
		for _, a := range acts {
			if a.Kind == ActSetTimer && a.Timer == TimerDesync {
				armed = a
			}
		}
		if armed.Kind != ActSetTimer {
			t.Fatalf("no desync timer armed: %v", RenderActions(acts))
		}
		if armed.After < 1*Second || armed.After > 10*Second {
			t.Fatalf("desync delay %s is outside the RFC's 1..10s window", armed.After)
		}
	}

	// And the delay firing is what sends the DISCOVER.
	m := newMachine(t, p)
	m.Step(0, 42, Simple(EvStart))
	st, acts := m.Step(at(5), 43, TimerFired(TimerDesync))
	if st != StateSelecting {
		t.Fatalf("desync fire left the machine in %s", st)
	}
	mustSend(t, acts, wire.MsgDiscover)
}

// -------------------------------------------------------------------- R2 --

func TestFailedSendDoesNotAdvanceTheRetransmitCounter(t *testing.T) {
	// R2. The server never saw the message, so "one attempt used" would be a
	// lie the budget then pays for: with the counter advanced, a transport
	// that fails every send burns the whole retransmission budget without a
	// single packet reaching the wire.
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	send, ok := find(acts, ActSend)
	if !ok {
		t.Fatal("no send to fail")
	}

	before := m.retransmits
	_, acts = m.Step(at(1), 2, ActionFailed(send.ID, "network is down"))
	if m.retransmits != before {
		t.Fatalf("retransmit counter moved %d -> %d on a send that never left", before, m.retransmits)
	}
	// The retransmit timer is re-armed, so the machine tries again rather than
	// sitting idle.
	var rearmed bool
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerRetransmit {
			rearmed = true
		}
	}
	if !rearmed {
		t.Fatalf("no retransmit timer re-armed after a failed send: %v", RenderActions(acts))
	}
}

func TestMaxSendFailuresReportsTheTransport(t *testing.T) {
	// Without this bound, a machine whose every send fails sits in SELECTING
	// re-arming a timer forever and looks exactly like one waiting for a slow
	// server.
	p := testParams()
	p.MaxSendFailures = 3
	m := newMachine(t, p)
	_, acts := m.Step(0, 1, Simple(EvStart))
	send, _ := find(acts, ActSend)

	var last []Action
	for i := 0; i < p.MaxSendFailures; i++ {
		_, last = m.Step(at(int64(i+1)), uint64(i), ActionFailed(send.ID, "ENETDOWN"))
	}
	f, ok := find(last, ActFailed)
	if !ok {
		t.Fatalf("no Failed action after %d consecutive send failures: %v", p.MaxSendFailures, RenderActions(last))
	}
	if f.Reason != ReasonTransport {
		t.Fatalf("Failed reason = %s, want transport", f.Reason)
	}
	if m.State() != StateInit {
		t.Fatalf("state = %s, want INIT (parked, not spinning)", m.State())
	}
}

func TestBoundSendFailureDropsTheLease(t *testing.T) {
	// The preservation control's opposite direction: a transport that has
	// broken while we hold a lease must surface the LOSS, not just a Failed.
	// A caller left holding an address on a dead link is the v1.x failure this
	// library exists to stop repeating.
	p := testParams()
	p.MaxSendFailures = 1
	m := newMachine(t, p)
	// Reach BOUND with these params.
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	_, acts = m.Step(at(2), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	if m.State() != StateBound {
		t.Fatalf("fixture is in %s, not BOUND", m.State())
	}
	acq, _ := find(acts, ActLeaseAcquired)

	_, acts = m.Step(at(3), 4, ActionFailed(acq.ID, "ENETDOWN"))
	lost, ok := find(acts, ActLeaseLost)
	if !ok {
		t.Fatalf("a broken transport in BOUND did not report the lease lost: %v", RenderActions(acts))
	}
	if lost.Reason != ReasonTransport {
		t.Fatalf("LeaseLost reason = %s, want transport", lost.Reason)
	}
}

func TestLinkDownInBoundDropsTheLease(t *testing.T) {
	m := machineIn(t, StateBound)
	st, acts := m.Step(at(10), 1, Simple(EvLinkDown))
	lost, ok := find(acts, ActLeaseLost)
	if !ok || lost.Reason != ReasonLinkDown {
		t.Fatalf("link down did not drop the lease with a link-down reason: %v", RenderActions(acts))
	}
	if st != StateInit {
		t.Fatalf("state = %s, want INIT parked (nothing to send on a dead link)", st)
	}
	if count(acts, ActSend) != 0 {
		t.Fatalf("the machine sent on a link it was just told is down: %v", RenderActions(acts))
	}
}

func TestStopDropsTheLeaseAndCancelsEverything(t *testing.T) {
	m := machineIn(t, StateBound)
	st, acts := m.Step(at(10), 1, Simple(EvStop))
	if st != StateStopped {
		t.Fatalf("state = %s, want STOPPED", st)
	}
	lost, ok := find(acts, ActLeaseLost)
	if !ok || lost.Reason != ReasonStopped {
		t.Fatalf("Stop did not report the lease lost: %v", RenderActions(acts))
	}
	for _, id := range AllTimerIDs() {
		found := false
		for _, a := range acts {
			if a.Kind == ActCancelTimer && a.Timer == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("Stop left timer %s armed: %v", id, RenderActions(acts))
		}
	}
}

func TestStopWithNoLeaseReportsNoLoss(t *testing.T) {
	// The preservation control for the test above: LeaseLost must mean a lease
	// was actually lost. A Stop from SELECTING has nothing to lose, and a
	// spurious Lost event would make the plugin tear down an address it never
	// installed.
	m := machineIn(t, StateSelecting)
	_, acts := m.Step(at(10), 1, Simple(EvStop))
	if _, ok := find(acts, ActLeaseLost); ok {
		t.Fatalf("Stop with no lease emitted LeaseLost: %v", RenderActions(acts))
	}
}

func TestAckWithoutLeaseTimeIsDiscarded(t *testing.T) {
	m := machineIn(t, StateRequesting)
	// Rebuild the REQUEST the fixture sent so the ACK matches its xid.
	req := &wire.Message{XID: m.xid, CHAddr: testCHAddr}
	bad := ackFor(req, "192.168.99.50", "192.168.99.1", 3600)
	delete(bad.Options, wire.OptLeaseTime)

	st, acts := m.Step(at(5), 1, received(t, bad))
	if st != StateRequesting {
		t.Fatalf("state = %s, want to stay in REQUESTING", st)
	}
	if _, ok := find(acts, ActLeaseAcquired); ok {
		t.Fatalf("an ACK with no lease time produced a lease: %v", RenderActions(acts))
	}
}

func TestInfiniteLeaseArmsNoExpiry(t *testing.T) {
	m := machineIn(t, StateRequesting)
	req := &wire.Message{XID: m.xid, CHAddr: testCHAddr}
	ack := ackFor(req, "192.168.99.50", "192.168.99.1", InfiniteSeconds)

	st, acts := m.Step(at(5), 1, received(t, ack))
	if st != StateBound {
		t.Fatalf("state = %s, want BOUND", st)
	}
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerExpire {
			t.Fatalf("an expiry timer was armed for an infinite lease: %s", a)
		}
	}
	l, ok := m.Lease()
	if !ok {
		t.Fatal("no lease held")
	}
	if _, has := l.Expire(); has {
		t.Fatal("an infinite lease reports an expiry")
	}
}

func TestAckForAnAlreadyExpiredLeaseArmsZero(t *testing.T) {
	// The ACK grants a lease whose clock started at the REQUEST send and has
	// already run out. Arming a negative delay leaves the behaviour to
	// whatever the timer implementation does with one; arming zero makes ring
	// 3 fire it at once and the machine report the loss.
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)

	// The REQUEST went out at t=1s with a 2-second lease; the ACK arrives at
	// t=100s.
	_, acts = m.Step(at(100), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 2)))
	var armed *Action
	for i := range acts {
		if acts[i].Kind == ActSetTimer && acts[i].Timer == TimerExpire {
			armed = &acts[i]
		}
	}
	if armed == nil {
		t.Fatalf("no expiry timer armed: %v", RenderActions(acts))
	}
	if armed.After != 0 {
		t.Fatalf("expiry armed for %s, want 0 for a lease that has already run out", armed.After)
	}
}

func TestEveryActionCarriesAUniqueID(t *testing.T) {
	// R2 needs a failure to name exactly which action did not happen. Two
	// actions sharing an id, or an id of zero on everything, makes
	// EvActionFailed ambiguous — and the machine's response to it wrong for
	// every action but one.
	m := newMachine(t, testParams())
	seen := map[ActionID]string{}
	steps := []Event{
		Simple(EvStart),
		TimerFired(TimerRetransmit),
		Simple(EvLinkDown),
		Simple(EvLinkUp),
		Simple(EvStop),
	}
	for i, ev := range steps {
		_, acts := m.Step(at(int64(i+1)), uint64(i), ev)
		for _, a := range acts {
			if prev, dup := seen[a.ID]; dup {
				t.Fatalf("action id %s reused: %q then %q", a.ID, prev, a.String())
			}
			seen[a.ID] = a.String()
		}
	}
	if len(seen) < 5 {
		t.Fatalf("only %d stamped actions were produced; this test measured almost nothing", len(seen))
	}
}

// TestBoundArmsExpiryBeforeAnnouncing pins the ORDER of two actions, which is
// the whole of the assertion: the expiry timer must be armed before the lease
// is announced.
//
// A ring-3 caller executes the action list in order and may act on
// ActLeaseAcquired the moment it sees it, so anything stamped after the
// announcement is something the caller can run ahead of. Announcing first left
// a window in which the caller had been told it holds an address and nothing
// yet bounded its use of it — RFC 2131 section 4.4.5 requires the client to
// stop at expiry, and measures the lease from the DHCPREQUEST rather than from
// the ACK, so that bound is already running when the ACK lands.
//
// MEASURED 2026-08-30, against the machine with the announcement first: twelve
// concurrent `go test -race -count=100 -run TestManagerReportsExpiry ./lease/` gave 2
// failing processes, all `no expiry timer armed after acquisition`. -race did
// not flag it and the ring-2 test could only catch it by losing a race, which
// is why the assertion belongs here, where the order is deterministic.
func TestBoundArmsExpiryBeforeAnnouncing(t *testing.T) {
	// Two ACKs whose expiry paths differ, because the arm is on one side of a
	// branch and the announcement on the other: a lease with time left, and
	// one that had already run out when the ACK arrived (armed for zero).
	for _, tc := range []struct {
		name     string
		ackAt    int64
		leaseFor uint32
	}{
		{"lease with time left", 2, 3600},
		{"lease already expired when the ACK landed", 900, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, testParams())
			_, acts := m.Step(0, 0xAAAA, Simple(EvStart))
			disc := mustSend(t, acts, wire.MsgDiscover)
			_, acts = m.Step(at(1), 0xBBBB, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
			req := mustSend(t, acts, wire.MsgRequest)

			st, acts := m.Step(at(tc.ackAt), 0xCCCC,
				received(t, ackFor(req, "192.168.99.50", "192.168.99.1", tc.leaseFor)))
			if st != StateBound {
				t.Fatalf("after ACK: %s, want BOUND", st)
			}

			arm, announce := -1, -1
			for i, a := range acts {
				if a.Kind == ActSetTimer && a.Timer == TimerExpire && arm < 0 {
					arm = i
				}
				if a.Kind == ActLeaseAcquired && announce < 0 {
					announce = i
				}
			}
			if arm < 0 {
				t.Fatalf("no expiry timer armed at all: %v", RenderActions(acts))
			}
			if announce < 0 {
				t.Fatalf("the lease was never announced: %v", RenderActions(acts))
			}
			if arm > announce {
				t.Fatalf("expiry armed at action %d, announced at %d: a caller acting on the "+
					"announcement can outrun the arm. %v", arm, announce, RenderActions(acts))
			}
		})
	}
}

// TestBoundAnnouncesAnInfiniteLeaseWithNoTimer is the preservation control for
// the ordering above. Restructuring enterBound so the arm precedes the
// announcement puts the announcement after a branch that returns for an
// infinite lease; if it were left inside that branch, an infinite lease would
// be installed and never reported, and the ordering test above — which requires
// an expiry timer — could not see it.
func TestBoundAnnouncesAnInfiniteLeaseWithNoTimer(t *testing.T) {
	m := newMachine(t, testParams())
	_, acts := m.Step(0, 0xAAAA, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 0xBBBB, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)

	st, acts := m.Step(at(2), 0xCCCC,
		received(t, ackFor(req, "192.168.99.50", "192.168.99.1", InfiniteSeconds)))
	if st != StateBound {
		t.Fatalf("after ACK: %s, want BOUND", st)
	}
	if _, ok := find(acts, ActLeaseAcquired); !ok {
		t.Fatalf("an infinite lease was installed and never announced: %v", RenderActions(acts))
	}
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerExpire {
			t.Fatalf("an infinite lease armed an expiry timer for %s (RFC 2132 section 3.3)", a.After)
		}
	}
}

// ------------------------------------------------- decline and release --

// terminalParams turns on every option base() adds, so the tests below cannot
// pass vacuously. A DHCPDECLINE built from a Params with no host name and no
// vendor class carries neither for the wrong reason.
func terminalParams() Params {
	p := testParams()
	p.Hostname = "container-a"
	p.VendorClass = "docker-net-dhcp"
	p.ClientID = []byte{0xFF, 0x01, 0x02, 0x03}
	p.RequestedLease = 3600 * Second
	p.Broadcast = true
	return p
}

// boundWith reaches BOUND with the given Params, the way machineIn does for
// the default ones.
func boundWith(t *testing.T, p Params) *Machine {
	t.Helper()
	m := newMachine(t, p)
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	m.Step(at(2), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	if m.State() != StateBound {
		t.Fatalf("fixture reached %s, want BOUND", m.State())
	}
	return m
}

// TestDeclineAndReleaseCarryOnlyThePermittedOptions is RFC 2131 Table 5's
// DHCPDECLINE/DHCPRELEASE column, checked as a WHITELIST rather than as a list
// of forbidden codes.
//
// A test that enumerated the six options base() adds would be satisfied the day
// a seventh is added to base() — the shape this project keeps paying for. The
// permitted set is closed by the table's "All others: MUST NOT", so the
// whitelist is the honest form and it catches an option nobody thought of.
func TestDeclineAndReleaseCarryOnlyThePermittedOptions(t *testing.T) {
	permitted := map[wire.MessageType]map[wire.OptionCode]bool{
		wire.MsgDecline: {
			wire.OptMessageType: true, // MUST, and RFC 2131 section 2 for every message
			wire.OptRequestedIP: true, // MUST (DHCPDECLINE)
			wire.OptServerID:    true, // MUST
			wire.OptMessage:     true, // SHOULD
			wire.OptClientID:    true, // MAY, and section 3.1(6)
		},
		wire.MsgRelease: {
			wire.OptMessageType: true,
			wire.OptServerID:    true,
			wire.OptMessage:     true,
			wire.OptClientID:    true,
		},
	}

	for _, tc := range []struct {
		want wire.MessageType
		ev   EventKind
	}{
		{wire.MsgDecline, EvConflictDetected},
		{wire.MsgRelease, EvRelease},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			p := terminalParams()
			m := boundWith(t, p)
			_, acts := m.Step(at(10), 0xFEED, Simple(tc.ev))
			msg := mustSend(t, acts, tc.want)

			for _, c := range msg.Options.Codes() {
				if !permitted[tc.want][c] {
					t.Errorf("%s carries option %s, which RFC 2131 Table 5 forbids", tc.want, c)
				}
			}
			for c := range permitted[tc.want] {
				if c == wire.OptClientID {
					// Option 61 is a MAY in Table 5 and a MUST in section
					// 3.1(6) for a client that used one; presence alone is
					// therefore not the property, and it is asserted by VALUE
					// below instead of being counted here.
					continue
				}
				if _, ok := msg.Options[c]; !ok {
					t.Errorf("%s does not carry option %s", tc.want, c)
				}
			}

			// The two IDENTITY fields, and neither is held by the whitelist
			// above: 'chaddr' is not an option at all, and option 61 is
			// skipped by the presence loop by construction. RFC 2131 section
			// 3.1(6): "The client identifies the lease to be released with its
			// 'client identifier', or 'chaddr' and network address in the
			// DHCPRELEASE message. If the client used a 'client identifier'
			// when it obtained the lease, it MUST use the same 'client
			// identifier'." A message carrying neither, or carrying somebody
			// else's, is one the server cannot match to a binding — and
			// nothing answers either message, so it fails in silence.
			if !bytes.Equal(msg.CHAddr, p.CHAddr) {
				t.Errorf("%s carries chaddr %x, want the client's %x", tc.want, msg.CHAddr, p.CHAddr)
			}
			if got, ok := msg.Options[wire.OptClientID]; !ok || !bytes.Equal(got, p.ClientID) {
				t.Errorf("%s carries client-id %x/%v, want the one the lease was obtained with, %x", tc.want, got, ok, p.ClientID)
			}
			if msg.Secs != 0 {
				t.Errorf("secs = %d, Table 5 says 0 for this column", msg.Secs)
			}
			if msg.Flags != 0 {
				t.Errorf("flags = %#04x, Table 5 says 0 — the BROADCAST bit is not set here even for a client that sets it everywhere else", msg.Flags)
			}
			if msg.Op != wire.BootRequest || msg.Hops != 0 {
				t.Errorf("op/hops = %s/%d, want BOOTREQUEST/0", msg.Op, msg.Hops)
			}
			for _, f := range []struct {
				name string
				a    netip.Addr
			}{{"yiaddr", msg.YIAddr}, {"siaddr", msg.SIAddr}, {"giaddr", msg.GIAddr}} {
				if f.a.IsValid() && !f.a.IsUnspecified() {
					t.Errorf("%s = %s, Table 5 says 0", f.name, f.a)
				}
			}
		})
	}
}

// TestTerminalMessagesOmitAnUnusedClientIdentifier is the other direction of
// the section 3.1(6) MUST, and it is what keeps the assertion above from being
// satisfied by a builder that always emits option 61.
//
// Table 5 makes the client identifier a MAY. A client that obtained its lease
// WITHOUT one must not invent one here: the server matches the binding by
// 'chaddr' in that case, and an option 61 the DHCPREQUEST never carried names
// a binding that does not exist.
func TestTerminalMessagesOmitAnUnusedClientIdentifier(t *testing.T) {
	p := terminalParams()
	p.ClientID = nil

	for _, tc := range []struct {
		want wire.MessageType
		ev   EventKind
	}{
		{wire.MsgDecline, EvConflictDetected},
		{wire.MsgRelease, EvRelease},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			m := boundWith(t, p)
			_, acts := m.Step(at(10), 0xFEED, Simple(tc.ev))
			msg := mustSend(t, acts, tc.want)
			if got, ok := msg.Options[wire.OptClientID]; ok {
				t.Errorf("%s carries client-id %x for a client that used none to obtain the lease", tc.want, got)
			}
			if !bytes.Equal(msg.CHAddr, p.CHAddr) {
				t.Errorf("%s carries chaddr %x, want %x — the only identity left when there is no option 61", tc.want, msg.CHAddr, p.CHAddr)
			}
		})
	}
}

// TestBaseMessagesStillCarryEverythingTheyShould is the preservation control
// for the test above: the terminal builder must not have been achieved by
// stripping base(). A DISCOVER and a REQUEST built from the same Params still
// carry the options Table 5 permits them.
func TestBaseMessagesStillCarryEverythingTheyShould(t *testing.T) {
	p := terminalParams()
	m := newMachine(t, p)
	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	for _, c := range []wire.OptionCode{
		wire.OptHostName, wire.OptVendorClassID, wire.OptParameterList,
		wire.OptLeaseTime, wire.OptClientID,
	} {
		if _, ok := disc.Options[c]; !ok {
			t.Errorf("DHCPDISCOVER lost option %s", c)
		}
	}
	if disc.Flags&wire.FlagBroadcast == 0 {
		t.Error("DHCPDISCOVER lost the BROADCAST flag, which Params.Broadcast asks for")
	}
}

// TestDeclineAndReleaseCarryTheAddressInTheRightPlace is the cell of RFC 2131
// Table 5 that differs between the two messages, driven in both directions.
//
// A DHCPDECLINE puts the address in the 'requested IP address' option with
// 'ciaddr' 0; a DHCPRELEASE puts it in 'ciaddr' and MUST NOT carry the option.
// Neither message is answered, so getting this the wrong way round is
// invisible on the wire — the server simply cannot match the binding.
func TestDeclineAndReleaseCarryTheAddressInTheRightPlace(t *testing.T) {
	want := netip.MustParseAddr("192.168.99.50")

	t.Run("decline", func(t *testing.T) {
		m := boundWith(t, terminalParams())
		_, acts := m.Step(at(10), 0xFEED, Simple(EvConflictDetected))
		msg := mustSend(t, acts, wire.MsgDecline)
		got, ok := msg.Addr4(wire.OptRequestedIP)
		if !ok || got != want {
			t.Errorf("requested-ip = %v/%v, want %s", got, ok, want)
		}
		if msg.CIAddr.IsValid() && !msg.CIAddr.IsUnspecified() {
			t.Errorf("ciaddr = %s, Table 5 says 0 for a DHCPDECLINE", msg.CIAddr)
		}
	})

	t.Run("release", func(t *testing.T) {
		m := boundWith(t, terminalParams())
		_, acts := m.Step(at(10), 0xFEED, Simple(EvRelease))
		msg := mustSend(t, acts, wire.MsgRelease)
		if msg.CIAddr != want {
			t.Errorf("ciaddr = %s, want %s", msg.CIAddr, want)
		}
		if _, ok := msg.Options[wire.OptRequestedIP]; ok {
			t.Error("DHCPRELEASE carries a requested-ip option, which Table 5 makes a MUST NOT")
		}
	})
}

// TestDeclineIsBroadcastAndReleaseIsUnicast is RFC 2131 section 4.4.4, whose
// one sentence answers the question DIFFERENTLY for the two messages. The
// symmetric guess is wrong on exactly one of them.
func TestDeclineIsBroadcastAndReleaseIsUnicast(t *testing.T) {
	m := boundWith(t, terminalParams())
	_, acts := m.Step(at(10), 0xFEED, Simple(EvConflictDetected))
	a, _ := find(acts, ActSend)
	if !a.Dest.Broadcast {
		t.Errorf("DHCPDECLINE sent to %s, RFC 2131 section 4.4.4 broadcasts it", a.Dest)
	}

	m = boundWith(t, terminalParams())
	_, acts = m.Step(at(10), 0xFEED, Simple(EvRelease))
	a, _ = find(acts, ActSend)
	if a.Dest.Broadcast || a.Dest.Addr != netip.MustParseAddr("192.168.99.1") {
		t.Errorf("DHCPRELEASE sent to %s, RFC 2131 section 4.4.4 unicasts it to the server", a.Dest)
	}
}

// TestDeclineAndReleaseAreSentBeforeTheLossIsAnnounced is B18's ordering
// invariant on the two new paths.
//
// A ring-3 caller drains this list in order and may act on ActLeaseLost the
// moment it sees it — removing the address, tearing the interface down. Any
// send after the announcement is one the caller can outrun, and for the
// DHCPRELEASE it is worse than a race: section 4.4.4 unicasts it FROM the
// released address, so a torn-down interface leaves it with no source.
func TestDeclineAndReleaseAreSentBeforeTheLossIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   EventKind
		want wire.MessageType
	}{
		{"decline", EvConflictDetected, wire.MsgDecline},
		{"release", EvRelease, wire.MsgRelease},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := boundWith(t, terminalParams())
			_, acts := m.Step(at(10), 0xFEED, Simple(tc.ev))
			sent, lost := -1, -1
			for i, a := range acts {
				if a.Kind == ActSend && sent < 0 {
					if got, ok := a.Msg.Type(); ok && got == tc.want {
						sent = i
					}
				}
				if a.Kind == ActLeaseLost && lost < 0 {
					lost = i
				}
			}
			if sent < 0 || lost < 0 {
				t.Fatalf("send=%d lost=%d in %v", sent, lost, RenderActions(acts))
			}
			if sent > lost {
				t.Fatalf("%s emitted at action %d, loss announced at %d: a caller acting on the announcement can outrun the send", tc.want, sent, lost)
			}
		})
	}
}

// TestDeclineWaitsBeforeRestarting is RFC 2131 section 3.1(5): "The client
// SHOULD wait a minimum of ten seconds before restarting the configuration
// process to avoid excessive network traffic in case of looping."
//
// The wait is asserted as a FLOOR, not as an equality, because the RFC states
// a minimum and a client that waits longer is conformant.
func TestDeclineWaitsBeforeRestarting(t *testing.T) {
	m := boundWith(t, terminalParams())
	st, acts := m.Step(at(10), 0xFEED, Simple(EvConflictDetected))
	if st != StateInit {
		t.Fatalf("after DHCPDECLINE: %s, want INIT (RFC 2131 section 3.2(3))", st)
	}
	if count(acts, ActSend) != 1 {
		t.Fatalf("the decline step sent %d message(s); a DHCPDISCOVER before the wait is the loop the wait exists to stop: %v",
			count(acts, ActSend), RenderActions(acts))
	}
	var armed *Action
	for i := range acts {
		if acts[i].Kind == ActSetTimer && acts[i].Timer == TimerRestart {
			armed = &acts[i]
		}
	}
	if armed == nil {
		t.Fatalf("no restart timer armed; the machine is parked in INIT with nothing to wake it: %v", RenderActions(acts))
	}
	if armed.After < 10*Second {
		t.Fatalf("restart timer armed for %s, RFC 2131 section 3.1(5) says a minimum of ten seconds", armed.After)
	}
}

// TestRestartFiresAFreshTransaction drives the other half of the wait: a timer
// nothing handles parks the machine in INIT forever, which looks exactly like
// an idle client.
//
// It also drives WHICH restart, because "a DHCPDISCOVER follows" is satisfied
// by continuing the declined transaction. RFC 2131 section 3.1(5) restarts the
// CONFIGURATION PROCESS, which section 4.4.1 begins by generating a new
// transaction identifier, and section 2 has 'secs' count from the moment the
// client began the acquisition. So both are observable on the wire and both
// are asserted: reusing the acquisition's xid, or its start time, is the
// cheap wrong version and neither is visible in the state alone.
func TestRestartFiresAFreshTransaction(t *testing.T) {
	m := newMachine(t, terminalParams())
	_, acts := m.Step(0, 1, Simple(EvStart))
	first := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(1), 2, received(t, offerFor(first, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	m.Step(at(2), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	if m.State() != StateBound {
		t.Fatalf("fixture reached %s, want BOUND", m.State())
	}

	_, acts = m.Step(at(10), 0xFEED, Simple(EvConflictDetected))
	declined := mustSend(t, acts, wire.MsgDecline)

	st, acts := m.Step(at(30), 0xBEEF, TimerFired(TimerRestart))
	if st != StateSelecting {
		t.Fatalf("after the restart wait: %s, want SELECTING", st)
	}
	disc := mustSend(t, acts, wire.MsgDiscover)
	if disc.XID == declined.XID {
		t.Errorf("the restarted DHCPDISCOVER reuses the DHCPDECLINE's xid %#08x; RFC 2131 Table 5 selects a new one per transaction", disc.XID)
	}
	if disc.XID == first.XID {
		t.Errorf("the restarted DHCPDISCOVER reuses the declined acquisition's xid %#08x; RFC 2131 section 4.4.1 generates one per configuration process", disc.XID)
	}
	if disc.Secs != 0 {
		t.Errorf("the restarted DHCPDISCOVER carries secs=%d; RFC 2131 section 2 counts from when the client began THIS acquisition, and a restarted process began now", disc.Secs)
	}
	// The wait is over, so nothing may still be armed to fire it again.
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerRestart {
			t.Errorf("the restart re-armed the restart timer: %v", RenderActions(acts))
		}
	}
}

// TestReleaseIsSentFromTheReleasedAddress is RFC 2131 section 4.4.4's
// DHCPRELEASE as ring 3 has to execute it.
//
// Table 5 puts the released address in 'ciaddr', which the byte tests already
// hold. This holds the OTHER half, which is not in the message at all: the
// datagram is unicast to the server FROM that address, and ring 3 cannot
// derive it — the transport is a packet socket on a link the kernel has no
// address on. If the machine does not say, the release goes out from 0.0.0.0,
// which no server can answer and none can match.
//
// The DHCPDECLINE is the control: section 4.4.4 broadcasts it and section 4.1
// requires the source to be 0 for a client that is giving the address up.
func TestReleaseIsSentFromTheReleasedAddress(t *testing.T) {
	want := netip.MustParseAddr("192.168.99.50")
	server := netip.MustParseAddr("192.168.99.1")

	m := boundWith(t, terminalParams())
	_, acts := m.Step(at(10), 0xFEED, Simple(EvRelease))
	a, _ := find(acts, ActSend)
	if a.Dest.Broadcast {
		t.Fatalf("the DHCPRELEASE is a broadcast: %s", a.Dest)
	}
	if a.Dest.Addr != server {
		t.Errorf("DHCPRELEASE destination = %s, want the server %s", a.Dest.Addr, server)
	}
	if a.Dest.Src != want {
		t.Errorf("DHCPRELEASE source = %s, want the released address %s", a.Dest.Src, want)
	}

	m = boundWith(t, terminalParams())
	_, acts = m.Step(at(10), 0xFEED, Simple(EvConflictDetected))
	a, _ = find(acts, ActSend)
	if !a.Dest.Broadcast {
		t.Fatalf("the DHCPDECLINE is not a broadcast: %s", a.Dest)
	}
	if a.Dest.Src.IsValid() && !a.Dest.Src.IsUnspecified() {
		t.Errorf("the broadcast DHCPDECLINE names source %s; RFC 2131 section 4.1 sends it from 0", a.Dest.Src)
	}
}

// TestAFailedTerminalSendIsAnnouncedAndAccountedFor covers what happens when
// the DHCPRELEASE or the DHCPDECLINE does not leave the host.
//
// STANDARD, and it decides the first half: RFC 2131 section 4.4.6 says "the
// correct operation of DHCP does not depend on the transmission of DHCPRELEASE
// messages", and section 3.1(5) makes giving up a conflicting address a MUST
// on DETECTION, not on transmission. So the loss is announced either way — a
// client that kept using an address it has decided to stop using because a
// packet did not leave would be the worse failure.
//
// What must NOT happen is the failure passing unremarked. Neither message is
// answered and neither is retransmitted, so this journal line is the only
// place the divergence — this client has released, the server still holds the
// binding — is readable at all.
func TestAFailedTerminalSendIsAnnouncedAndAccountedFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ev    EventKind
		want  Reason
		state State
	}{
		{"release", EvRelease, ReasonReleased, StateStopped},
		{"decline", EvConflictDetected, ReasonConflict, StateInit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := boundWith(t, terminalParams())
			_, acts := m.Step(at(10), 0xFEED, Simple(tc.ev))
			send, ok := find(acts, ActSend)
			if !ok {
				t.Fatalf("nothing was sent: %v", RenderActions(acts))
			}
			lost, ok := find(acts, ActLeaseLost)
			if !ok || lost.Reason != tc.want {
				t.Fatalf("the lease was not given up: %v", RenderActions(acts))
			}
			if m.State() != tc.state {
				t.Fatalf("state = %s, want %s", m.State(), tc.state)
			}

			st, acts := m.Step(at(11), 1, ActionFailed(send.ID, "ENETDOWN"))
			if st != tc.state {
				t.Fatalf("a failed terminal send moved the machine to %s, want %s: the lease is given up whether or not the message left", st, tc.state)
			}
			said := false
			for _, a := range acts {
				if a.Kind == ActJournal && strings.Contains(a.Note, "failed") && strings.Contains(a.Note, "ENETDOWN") {
					said = true
				}
			}
			if !said {
				t.Fatalf("the failed send was not journalled by name: %v", RenderActions(acts))
			}
			for _, a := range acts {
				if a.Kind == ActJournal && strings.Contains(a.Note, "ignored") {
					t.Fatalf("a failed terminal send was journalled as ignored: %v", RenderActions(acts))
				}
			}
		})
	}
}

// TestAFailedDeclineDoesNotCancelTheRestart is the same failure at the one
// setting that used to strand the machine.
//
// With MaxSendFailures at 1, a single failed DHCPDECLINE send reaches the
// transport-broken escalation, which parked the machine in INIT with every
// timer cancelled — including the restart wait that declineAndRestart had just
// armed and that nothing else re-arms. No lease, no timer, no event coming,
// and INIT-with-nothing-armed is what an idle client looks like.
func TestAFailedDeclineDoesNotCancelTheRestart(t *testing.T) {
	p := terminalParams()
	p.MaxSendFailures = 1
	m := boundWith(t, p)

	_, acts := m.Step(at(10), 0xFEED, Simple(EvConflictDetected))
	send := mustSend(t, acts, wire.MsgDecline)
	_ = send
	sendAct, _ := find(acts, ActSend)
	armed := false
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerRestart {
			armed = true
		}
	}
	if !armed {
		t.Fatalf("the fixture armed no restart: %v", RenderActions(acts))
	}

	st, acts := m.Step(at(11), 1, ActionFailed(sendAct.ID, "ENETDOWN"))
	if st != StateInit {
		t.Fatalf("state = %s, want INIT", st)
	}
	for _, a := range acts {
		if a.Kind == ActCancelTimer && a.Timer == TimerRestart {
			t.Fatalf("the failed DHCPDECLINE cancelled the restart wait: %v", RenderActions(acts))
		}
	}
	// The wait still fires, which is the property the cancellation removed.
	st, acts = m.Step(at(30), 0xBEEF, TimerFired(TimerRestart))
	if st != StateSelecting {
		t.Fatalf("after the restart wait: %s, want SELECTING", st)
	}
	mustSend(t, acts, wire.MsgDiscover)
}

// TestRestartDoesNotWaitTwice drives the restart under a LIVE desync window.
//
// Every other test in this file turns the window off, and a zero window
// resolves to no delay at all — so under those fixtures a restart that also
// applied RFC 2131 section 4.4.1's startup draw is indistinguishable from one
// that does not. DefaultParams ships the window ON, which makes the fixture
// the only reason the difference is invisible.
//
// The two waits are different obligations: section 4.4.1's one-to-ten-second
// draw desynchronises hosts starting together, section 3.1(5)'s ten seconds
// keeps a client whose address is permanently in use from looping. The second
// has just elapsed, so serving the first as well delays the restart again for
// a reason that does not apply.
func TestRestartDoesNotWaitTwice(t *testing.T) {
	p := terminalParams()
	p.DesyncMin, p.DesyncMax = 1*Second, 10*Second

	m := newMachine(t, p)
	_, acts := m.Step(0, 1, Simple(EvStart))
	if _, ok := find(acts, ActSend); ok {
		t.Fatalf("the fixture's desync window is not in force: %v", RenderActions(acts))
	}
	_, acts = m.Step(at(5), 1, TimerFired(TimerDesync))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m.Step(at(6), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	m.Step(at(7), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	if m.State() != StateBound {
		t.Fatalf("fixture reached %s, want BOUND", m.State())
	}

	m.Step(at(10), 0xFEED, Simple(EvConflictDetected))
	st, acts := m.Step(at(30), 0xBEEF, TimerFired(TimerRestart))
	if st != StateSelecting {
		t.Fatalf("after the restart wait: %s, want SELECTING — the restart served a second wait", st)
	}
	mustSend(t, acts, wire.MsgDiscover)
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerDesync {
			t.Fatalf("the restart armed a desync wait on top of the section 3.1(5) one: %v", RenderActions(acts))
		}
	}
}

// TestRestartDelayMeetsTheRFCMinimum pins DefaultRestartDelay to the floor its
// own citation names. Cited from proto.DefaultRestartDelay.
func TestRestartDelayMeetsTheRFCMinimum(t *testing.T) {
	if DefaultRestartDelay < 10*Second {
		t.Fatalf("DefaultRestartDelay is %s, RFC 2131 section 3.1(5) says a minimum of ten seconds", DefaultRestartDelay)
	}
	if got := DefaultParams(testCHAddr).RestartDelay; got != DefaultRestartDelay {
		t.Fatalf("DefaultParams.RestartDelay = %s, want %s", got, DefaultRestartDelay)
	}
	var zero Params
	zero.CHAddr = testCHAddr
	if got := zero.restartDelay(); got != DefaultRestartDelay {
		t.Fatalf("an unset RestartDelay resolves to %s; zero means the default here, not 'no wait'", got)
	}
}

// TestReleaseLeavesTheMachineStopped pins the decision RFC 2131 does not make.
//
// Figure 5 has no DHCPRELEASE edge and section 4.4.6 names no state. INIT was
// the alternative: it is wrong here because a released machine in INIT
// re-acquires on the next EvLinkUp, undoing what the caller asked for. The
// second half of this test is what makes that concrete.
func TestReleaseLeavesTheMachineStopped(t *testing.T) {
	m := boundWith(t, terminalParams())
	st, acts := m.Step(at(10), 0xFEED, Simple(EvRelease))
	if st != StateStopped {
		t.Fatalf("after DHCPRELEASE: %s, want STOPPED", st)
	}
	lost, ok := find(acts, ActLeaseLost)
	if !ok || lost.Reason != ReasonReleased {
		t.Fatalf("release did not report the lease lost as released: %v", RenderActions(acts))
	}

	st, acts = m.Step(at(20), 1, Simple(EvLinkUp))
	if st != StateStopped {
		t.Fatalf("a link event after a release moved the machine to %s; it must not re-acquire what the caller gave back", st)
	}
	if _, ok := find(acts, ActSend); ok {
		t.Fatalf("a link event after a release sent something: %v", RenderActions(acts))
	}

	// The preservation control: EvStart still resumes. STOPPED is a parked
	// state, not a dead one.
	st, acts = m.Step(at(30), 2, Simple(EvStart))
	if st != StateSelecting {
		t.Fatalf("Start after a release reached %s, want SELECTING", st)
	}
	mustSend(t, acts, wire.MsgDiscover)
}

// TestReleaseWithNoLeaseSendsNothing, with the BOUND case beside it as the
// control: a release must mean a lease was actually given back.
func TestReleaseWithNoLeaseSendsNothing(t *testing.T) {
	for _, st := range []State{StateStopped, StateInit, StateSelecting, StateRequesting} {
		t.Run(st.String(), func(t *testing.T) {
			m := machineIn(t, st)
			got, acts := m.Step(at(10), 1, Simple(EvRelease))
			if _, ok := find(acts, ActSend); ok {
				t.Fatalf("release in %s sent a message with no lease held: %v", st, RenderActions(acts))
			}
			if _, ok := find(acts, ActLeaseLost); ok {
				t.Fatalf("release in %s announced a loss with no lease held: %v", st, RenderActions(acts))
			}

			// SENDING NOTHING IS HALF THE OBLIGATION. Until round 4 this test
			// asserted only the half above, and EvRelease fell through the
			// default in the three pre-BOUND states: the caller had given the
			// address up and the machine carried on and took one anyway. What
			// makes that a defect rather than a delay is the next assertion,
			// not this one.
			if got != StateStopped {
				t.Fatalf("release in %s left the machine in %s, want STOPPED: the caller asked for no address and this one is still acquiring", st, got)
			}
			// Nothing re-armed. cancelAll's cancels are expected here; a SET
			// would be an acquisition still in flight.
			if a, ok := find(acts, ActSetTimer); ok {
				t.Fatalf("release in %s armed %s, so the acquisition is still running: %v", st, a.Timer, RenderActions(acts))
			}
			// The preservation control, per state: STOPPED is parked, not
			// dead, so the caller can still change its mind.
			if resumed, _ := m.Step(at(11), 2, Simple(EvStart)); resumed != StateSelecting {
				t.Fatalf("Start after a release in %s reached %s, want SELECTING", st, resumed)
			}
		})
	}
	t.Run("BOUND", func(t *testing.T) {
		m := machineIn(t, StateBound)
		_, acts := m.Step(at(10), 1, Simple(EvRelease))
		mustSend(t, acts, wire.MsgRelease)
	})
	t.Run("twice", func(t *testing.T) {
		m := machineIn(t, StateBound)
		m.Step(at(10), 1, Simple(EvRelease))
		_, acts := m.Step(at(11), 2, Simple(EvRelease))
		if _, ok := find(acts, ActSend); ok {
			t.Fatalf("a second release sent another DHCPRELEASE for a lease already given back: %v", RenderActions(acts))
		}
	})
}

// TestNoServerIdentifierMeansNoTerminalMessage is the case where the MUST
// cannot be met.
//
// RFC 2131 Table 5 makes the server identifier a MUST in both messages, and an
// ACK is not obliged to carry one, so this lease is reachable. Sending a
// message without it would be indistinguishable from a correct one — nothing
// answers either — so it is not sent, and the lease is still given up.
func TestNoServerIdentifierMeansNoTerminalMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   EventKind
		want Reason
	}{
		{"decline", EvConflictDetected, ReasonConflict},
		{"release", EvRelease, ReasonReleased},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, terminalParams())
			_, acts := m.Step(0, 1, Simple(EvStart))
			disc := mustSend(t, acts, wire.MsgDiscover)
			_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
			req := mustSend(t, acts, wire.MsgRequest)
			ack := ackFor(req, "192.168.99.50", "192.168.99.1", 3600)
			delete(ack.Options, wire.OptServerID)
			if _, acts = m.Step(at(2), 3, received(t, ack)); m.State() != StateBound {
				t.Fatalf("fixture is in %s, want BOUND: %v", m.State(), RenderActions(acts))
			}

			_, acts = m.Step(at(10), 4, Simple(tc.ev))
			if _, ok := find(acts, ActSend); ok {
				t.Fatalf("a message was sent for a lease with no server identifier: %v", RenderActions(acts))
			}
			lost, ok := find(acts, ActLeaseLost)
			if !ok || lost.Reason != tc.want {
				t.Fatalf("the lease was not given up: %v", RenderActions(acts))
			}
			// The silence has to be accounted for. Not sending is correct
			// here and it is also what a broken builder looks like; the
			// journal note is the only thing that tells an operator which of
			// the two happened.
			said := false
			for _, a := range acts {
				if a.Kind == ActJournal && strings.Contains(a.Note, "server identifier") {
					said = true
				}
			}
			if !said {
				t.Fatalf("nothing was sent and nothing said why: %v", RenderActions(acts))
			}
		})
	}
}

// TestEveryPathToIdleCancelsEveryTimer is what replaced three hand-written
// cancel lists.
//
// The lists were identical, had to be edited together, and nothing checked
// that they were. Only one of the three sat under a test that enumerated
// AllTimerIDs, so adding a timer would have left two of them short, and a
// stale timer firing in a state that does not expect it is silent.
func TestEveryPathToIdleCancelsEveryTimer(t *testing.T) {
	paths := []struct {
		name string
		from State
		ev   Event
	}{
		{"stop from BOUND", StateBound, Simple(EvStop)},
		{"stop from SELECTING", StateSelecting, Simple(EvStop)},
		{"link down in BOUND", StateBound, Simple(EvLinkDown)},
		{"link down in SELECTING", StateSelecting, Simple(EvLinkDown)},
		{"address lost in BOUND", StateBound, Simple(EvAddressLost)},
		{"conflict in BOUND", StateBound, Simple(EvConflictDetected)},
		{"release in BOUND", StateBound, Simple(EvRelease)},
		{"restart from INIT", StateInit, Simple(EvStart)},
		{"lease expiry", StateBound, TimerFired(TimerExpire)},
	}
	ids := AllTimerIDs()
	if len(ids) == 0 {
		t.Fatal("AllTimerIDs is empty; this test would measure nothing")
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			m := machineIn(t, p.from)
			_, acts := m.Step(at(10), 0x99, p.ev)
			for _, id := range ids {
				found := false
				for _, a := range acts {
					if a.Kind == ActCancelTimer && a.Timer == id {
						found = true
					}
				}
				if !found {
					t.Errorf("timer %s was never cancelled: %v", id, RenderActions(acts))
				}
			}
		})
	}
}

// TestAllTimerIDsIsEveryDeclaredTimer closes the hand-list itself.
//
// AllTimerIDs is written out by hand and two things derive from it rather than
// from the constants: cancelAll, so an id missing from the list is never
// cancelled, and runtime's numTimers, so Timers.Set silently RETURNS for an id
// past the end of the table. Dropping TimerRestart from the list survived the
// whole of proto and lease; the only thing that caught it was the namespaced
// dnsmasq test, by HANGING to go test's timeout.
//
// Every other test that ranges over AllTimerIDs — TestEveryPathToIdleCancelsEveryTimer
// is the one that matters — takes the list as its domain, so shrinking the
// list satisfies them instead of failing them. This test's domain is the
// TimerID space, which is why it is the one that can see a member go missing.
func TestAllTimerIDsIsEveryDeclaredTimer(t *testing.T) {
	listed := map[TimerID]int{}
	for _, id := range AllTimerIDs() {
		listed[id]++
	}

	// A TimerID is DECLARED when String has a name for it; everything else
	// falls back to timer(%d). That fallback is the only enumeration of the
	// constants that exists at run time, so it is what the list is checked
	// against — checking the list against itself is what let the gap open.
	declared := map[TimerID]bool{}
	for i := 0; i <= 255; i++ {
		id := TimerID(i)
		if id.String() != fmt.Sprintf("timer(%d)", i) {
			declared[id] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no TimerID has a name, so String's fallback would make any list correct and this test would assert nothing")
	}

	for id := range declared {
		switch listed[id] {
		case 1:
		case 0:
			t.Errorf("%s (TimerID %d) is declared and missing from AllTimerIDs: cancelAll will never cancel it and runtime's Timers.Set will silently drop it", id, uint8(id))
		default:
			t.Errorf("%s (TimerID %d) appears %d times in AllTimerIDs", id, uint8(id), listed[id])
		}
	}
	for id := range listed {
		if !declared[id] {
			t.Errorf("AllTimerIDs contains TimerID %d, which String has no name for", uint8(id))
		}
	}

	// DENSE FROM ZERO, which is a separate property from membership: runtime
	// indexes a slice of len(AllTimerIDs()) BY the id, so a declared id at or
	// above the count is dropped by a bounds check rather than reported.
	for id := range declared {
		if int(id) >= len(declared) {
			t.Errorf("%s is TimerID %d with %d timers declared: runtime's timer table is indexed by id, so this one is out of range there", id, uint8(id), len(declared))
		}
	}
}
