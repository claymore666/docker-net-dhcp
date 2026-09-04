package lease

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// ------------------------------------------------------------ acquisition --

func TestManagerAcquires(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event is %s, want acquired", ev)
	}
	if ev.Lease.Addr.String() != testYIAddr+"/24" {
		t.Fatalf("addr = %s", ev.Lease.Addr)
	}
	if ev.Lease.Gateway.String() != testServerID {
		t.Fatalf("gateway = %s", ev.Lease.Gateway)
	}
	if ev.Lease.Domain != "example.test" || ev.Lease.MTU != 1500 {
		t.Fatalf("domain/mtu = %q/%d", ev.Lease.Domain, ev.Lease.MTU)
	}

	// The outward lease reports WALL-CLOCK deadlines: a monotonic reading
	// means nothing to a caller and nothing to a file.
	if ev.Lease.Expire.IsZero() {
		t.Fatal("expiry is zero for a finite lease")
	}
	if !ev.Lease.Expire.After(ev.Lease.Acquired) {
		t.Fatalf("expiry %s is not after acquisition %s", ev.Lease.Expire, ev.Lease.Acquired)
	}
	if got := ev.Lease.Expire.Sub(ev.Lease.Acquired); got.Seconds() != 3600 {
		t.Fatalf("lease runs for %s, want 3600s", got)
	}
	// T1 and T2 defaults, converted for the caller (RFC 2131 section 4.4.5).
	if got := ev.Lease.Renew.Sub(ev.Lease.Acquired); got.Seconds() != 1800 {
		t.Fatalf("renew at +%s, want +1800s", got)
	}
	if got := ev.Lease.Rebind.Sub(ev.Lease.Acquired); got.Seconds() != 3150 {
		t.Fatalf("rebind at +%s, want +3150s", got)
	}

	// Two messages out, in order.
	sent := r.server.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want DISCOVER then REQUEST", len(sent))
	}
	if got, _ := sent[0].Type(); got != wire.MsgDiscover {
		t.Fatalf("first message is %s", got)
	}
	if got, _ := sent[1].Type(); got != wire.MsgRequest {
		t.Fatalf("second message is %s", got)
	}

	// Four packets captured: two out, two in (G1).
	if n := len(r.packets.Packets()); n != 4 {
		t.Fatalf("captured %d packets, want 4", n)
	}
	dirs := ""
	for _, p := range r.packets.Packets() {
		dirs += p.Dir.String()[:1]
		if len(p.Raw) == 0 {
			t.Fatalf("a captured packet has no raw bytes: %+v", p)
		}
		if p.At.IsZero() {
			t.Fatal("a captured packet has no timestamp")
		}
	}
	if dirs != "oioi" {
		t.Fatalf("packet directions %q, want out,in,out,in", dirs)
	}

	if got, ok := r.mgr.Lease(); !ok || got.Addr != ev.Lease.Addr {
		t.Fatalf("Lease() = %v/%v, want the acquired lease", got, ok)
	}
	s := r.mgr.Stats()
	if s.Sent != 2 || s.Received != 2 || s.LeasesAcquired != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if s.DecodeFailures != 0 || s.SendFailures != 0 {
		t.Fatalf("clean run reports failures: %+v", s)
	}
}

// TestManagerJournalReplays is done-condition (b) through the REAL manager:
// the journal it produced, replayed offline through ring 1, yields the same
// lease. The ring-3 test does this again with a real server's packets.
func TestManagerJournalReplays(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	// Stop the manager so the journal is complete and not being appended to
	// while it is read.
	_ = r.stop()

	entries := r.mgr.Journal()
	if len(entries) == 0 {
		t.Fatal("the journal is empty")
	}
	res, err := proto.Replay(testParams(), entries)
	if err != nil {
		t.Fatalf("the manager's own journal does not replay: %v", err)
	}
	if res.Steps != len(entries) {
		t.Fatalf("replayed %d of %d entries", res.Steps, len(entries))
	}
	// The run ended with a Stop, so the replayed machine ends stopped and
	// holding nothing — the lease is checked at the entry that acquired it.
	if res.State != proto.StateStopped {
		t.Fatalf("replay ended in %s, want STOPPED", res.State)
	}

	// Replay up to the ACK instead, and the lease must match what the caller
	// was told. This is the assertion that matters: a replay that only
	// reproduced the final state would agree with a machine that never leased.
	var upto []proto.JournalEntry
	for _, e := range entries {
		upto = append(upto, e)
		if e.To == proto.StateBound {
			break
		}
	}
	res, err = proto.Replay(testParams(), upto)
	if err != nil {
		t.Fatalf("replay to BOUND: %v", err)
	}
	if !res.Held {
		t.Fatal("replay to BOUND produced no lease")
	}
	if res.Lease.Addr.String() != testYIAddr+"/24" {
		t.Fatalf("replayed lease %s, want %s/24", res.Lease.Addr, testYIAddr)
	}
}

func TestManagerReportsExpiry(t *testing.T) {
	// RFC 2131 section 4.4.5: on expiry the client must stop using the
	// address. The caller learns that from a Lost event, and it must arrive
	// before the client's next DISCOVER goes out.
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	d, ok := r.timers.armedAt(proto.TimerExpire)
	if !ok {
		t.Fatal("no expiry timer armed after acquisition")
	}
	if d.Seconds() != 3600 {
		t.Fatalf("expiry armed for %s, want ~3600s (the clock does not move in this rig)", d)
	}

	r.clock.advance(3600 * proto.Second)
	r.timers.fire(proto.TimerExpire)

	ev := r.nextEvent(t)
	if ev.Kind != Lost {
		t.Fatalf("event after expiry is %s, want lost", ev)
	}
	if ev.Reason != proto.ReasonExpired {
		t.Fatalf("reason = %s, want expired", ev.Reason)
	}

	// And it re-acquires: the second acquisition is the proof that expiry is
	// a restart and not a dead end.
	ev = r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("event after the loss is %s, want a fresh acquisition", ev)
	}
	if r.mgr.Stats().LeasesLost != 1 {
		t.Fatalf("stats = %+v", r.mgr.Stats())
	}
}

func TestManagerReportsNak(t *testing.T) {
	nakOnce := func(req *wire.Message, n int) []*wire.Message {
		t, _ := req.Type()
		switch {
		case t == wire.MsgDiscover:
			return []*wire.Message{offerFor(req)}
		case t == wire.MsgRequest && n == 2:
			return []*wire.Message{nakFor(req)}
		case t == wire.MsgRequest:
			return []*wire.Message{ackFor(req, 3600)}
		}
		return nil
	}
	r := newRig(t, testParams(), nakOnce, Fault{})

	ev := r.nextEvent(t)
	if ev.Kind != Failed || ev.Reason != proto.ReasonNak {
		t.Fatalf("first event is %s, want a nak failure", ev)
	}
	if ev.Note == "" {
		t.Fatal("the NAK note is empty; the server's text is the only diagnosis the user gets")
	}
	// RFC 2131 section 3.1(5): the client restarts. It must reach a lease.
	if ev = r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("after the NAK restart: %s, want acquired", ev)
	}
	if r.mgr.Stats().AcquireFailures != 1 {
		t.Fatalf("stats = %+v", r.mgr.Stats())
	}
}

// ---------------------------------------------------------------- R2 tests --

func TestFailedSendIsRetriedNotCounted(t *testing.T) {
	// R2 end to end: the first send fails at the transport, the machine is
	// told, and the retransmit timer is re-armed WITHOUT spending a
	// retransmission. Firing that timer then gets the DISCOVER out.
	r := newRig(t, testParams(), answerNormally, Fault{FailSends: []int{1}})

	if !r.timers.waitArmed(proto.TimerRetransmit) {
		t.Fatal("the retransmit timer was never re-armed")
	}
	r.timers.fire(proto.TimerRetransmit)

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("event = %s, want the retry to acquire", ev)
	}

	sends, _ := r.fault.Counts()
	if sends != 3 {
		t.Fatalf("the transport saw %d sends, want 3 (one failed DISCOVER, one retry, one REQUEST)", sends)
	}
	s := r.mgr.Stats()
	if s.SendFailures != 1 {
		t.Fatalf("SendFailures = %d, want exactly the one injected", s.SendFailures)
	}
	if s.ActionsFailedFed != 1 {
		t.Fatalf("ActionsFailedFed = %d, want the failure to have re-entered the machine", s.ActionsFailedFed)
	}
}

func TestBrokenTransportIsReported(t *testing.T) {
	// Every send fails. Without a bound the machine would sit re-arming a
	// timer forever and look exactly like one waiting for a slow server.
	p := testParams()
	p.MaxSendFailures = 3
	r := newRig(t, p, answerNormally, Fault{FailEvery: 1})

	// The first send failed during Start. Drive the re-arm/fire cycle until
	// the machine gives up; the event channel is the barrier.
	go func() {
		for i := 0; i < p.MaxSendFailures+2; i++ {
			if !r.timers.waitArmed(proto.TimerRetransmit) {
				return
			}
			r.timers.fire(proto.TimerRetransmit)
		}
	}()

	ev := r.nextEvent(t)
	if ev.Kind != Failed {
		t.Fatalf("event = %s, want a failure", ev)
	}
	if ev.Reason != proto.ReasonTransport {
		t.Fatalf("reason = %s, want transport", ev.Reason)
	}
	if _, held := r.mgr.Lease(); held {
		t.Fatal("a client that never sent anything reports holding a lease")
	}
}

func TestDuplicateReplyProducesOneLease(t *testing.T) {
	// A duplicated ACK — a retransmitting server, or a link that mirrors
	// frames — must not produce two Acquired events. The second one would make
	// the plugin reconfigure an interface that did not change.
	r := newRig(t, testParams(), answerNormally, Fault{DuplicateInbound: []int{2}})

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	// Barrier: the machine has STEPPED the duplicate. The duplicate is the
	// only event of the run whose Step starts in BOUND, so the predicate names
	// it exactly. Waiting on the packet ring instead would prove only that the
	// duplicate arrived — see waitAppended.
	r.journal.waitAppended(t, "the duplicated ACK", func(e proto.JournalEntry) bool {
		return e.From == proto.StateBound
	})

	// The counter is read HERE, before the expiry below, and that ordering is
	// the fix for a flake this test had when it was written: an expiry is a
	// RESTART, so the fake server answers the DISCOVER that follows it and
	// LeasesAcquired legitimately reaches 2. Reading the counter afterwards
	// was measuring a race between the test and a correct re-acquisition —
	// four runs in ten. The assertion was wrong about WHEN, not about what.
	_, inbounds := r.fault.Counts()
	if inbounds < 2 {
		t.Fatalf("the fault plan named inbound 2 but only %d arrived; nothing was injected", inbounds)
	}
	if got := r.mgr.Stats().LeasesAcquired; got != 1 {
		t.Fatalf("LeasesAcquired = %d after a duplicated ACK, want 1", got)
	}

	// And a second, independent barrier on the EVENT stream: expire the lease.
	// If the duplicate had produced an event, it would arrive before this one.
	r.clock.advance(3600 * proto.Second)
	r.timers.fire(proto.TimerExpire)
	if ev = r.nextEvent(t); ev.Kind != Lost {
		t.Fatalf("second event is %s, want the expiry — a duplicate ACK produced an extra event", ev)
	}
}

func TestCorruptReplyIsCountedAndDiscarded(t *testing.T) {
	// CorruptInbound flips the first payload byte, which is 'op'. The message
	// still DECODES and then fails ring 1's BOOTREPLY check — a
	// hostile-but-well-formed input rather than a codec error.
	r := newRig(t, testParams(), answerNormally, Fault{CorruptInbound: []int{1}})

	// The corrupted OFFER is discarded, so nothing moves until the retransmit
	// timer fires and a second DISCOVER goes out.
	if !r.timers.waitArmed(proto.TimerRetransmit) {
		t.Fatal("the retransmit timer was never re-armed")
	}
	r.timers.fire(proto.TimerRetransmit)

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("event = %s, want the retry to acquire", ev)
	}
	if got := r.mgr.Stats().Received; got < 3 {
		t.Fatalf("Received = %d, want the corrupted packet counted too", got)
	}
	// It reached the packet ring, which is the point of capturing separately
	// from the journal: a packet ring 1 refused leaves no journal entry.
	if len(r.packets.Packets()) < 5 {
		t.Fatalf("captured %d packets, want the discarded one among them", len(r.packets.Packets()))
	}
}

func TestUndecodablePacketIsCountedNotFatal(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	// Garbage on the wire. A raw socket sees everything on the segment.
	go r.server.injectRaw([]byte{1, 2, 3, 4})
	r.packets.waitRecorded(t, "the undecodable packet", func(c CapturedPacket) bool {
		return c.DecodeErr != nil && len(c.Raw) == 4
	})

	// And the loop is still alive afterwards: expire the lease and read the
	// Lost. Garbage on the wire must not stall or kill the manager.
	r.clock.advance(3600 * proto.Second)
	r.timers.fire(proto.TimerExpire)
	if ev := r.nextEvent(t); ev.Kind != Lost {
		t.Fatalf("event = %s, want the expiry after garbage was ignored", ev)
	}
	if got := r.mgr.Stats().DecodeFailures; got != 1 {
		t.Fatalf("DecodeFailures = %d, want 1", got)
	}
	// The undecodable bytes are in the packet ring WITH the decode error, which
	// is the only place the evidence exists.
	found := false
	for _, p := range r.packets.Packets() {
		if p.DecodeErr != nil && p.Msg == nil && len(p.Raw) == 4 {
			found = true
		}
	}
	if !found {
		t.Fatal("the undecodable packet was not captured with its decode error")
	}
}

func TestTransportErrorIsCounted(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	go r.server.injectErr(errors.New("ENETDOWN"))
	r.packets.waitRecorded(t, "the transport error", func(c CapturedPacket) bool {
		return c.DecodeErr != nil && c.Raw == nil
	})

	r.clock.advance(3600 * proto.Second)
	r.timers.fire(proto.TimerExpire)
	if ev := r.nextEvent(t); ev.Kind != Lost {
		t.Fatalf("event = %s, want the expiry", ev)
	}
	if got := r.mgr.Stats().TransportErrors; got != 1 {
		t.Fatalf("TransportErrors = %d, want 1", got)
	}
}

// ------------------------------------------------------------- lifecycle --

func TestCancelReportsTheLeaseLostAndClosesTheStream(t *testing.T) {
	// A caller ranging over Events must see the final Lost and then a clean
	// close. A channel that simply stops leaves the caller holding an address
	// nobody is maintaining.
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}

	ev, ok := <-r.mgr.Events()
	if !ok {
		t.Fatal("the event channel closed without reporting the lease lost")
	}
	if ev.Kind != Lost || ev.Reason != proto.ReasonStopped {
		t.Fatalf("final event is %s, want lost/stopped", ev)
	}
	if _, ok := <-r.mgr.Events(); ok {
		t.Fatal("the event channel did not close after the final event")
	}
}

func TestTransportClosingIsNotACleanStop(t *testing.T) {
	// Something took the socket away. That is an error, not an orderly exit:
	// reporting nil would let a supervisor treat a vanished interface as a
	// completed job.
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}
	_ = r.fault.Close()

	err := r.wait()
	if err == nil {
		t.Fatal("Run returned nil after the transport closed under it")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("Run reported %v, want a transport error", err)
	}
}

func TestNewManagerRefusesAMissingPort(t *testing.T) {
	// Every port is required, and the refusal names which one. A nil port
	// would otherwise surface as a nil dereference inside Run, on a goroutine,
	// with no indication of what was not wired.
	base := func() Config {
		return Config{
			Params:    testParams(),
			Transport: newFakeServer(answerNormally),
			Clock:     newFakeClock(),
			Timers:    newFakeTimers(),
			Entropy:   &fakeEntropy{},
		}
	}
	cases := []struct {
		name string
		mut  func(*Config)
		want error
	}{
		{"transport", func(c *Config) { c.Transport = nil }, ErrNoTransport},
		{"clock", func(c *Config) { c.Clock = nil }, ErrNoClock},
		{"timers", func(c *Config) { c.Timers = nil }, ErrNoTimers},
		{"entropy", func(c *Config) { c.Entropy = nil }, ErrNoEntropy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(&cfg)
			if _, err := NewManager(cfg); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// The preservation control: the same config with nothing removed must
	// build. Four refusals prove nothing if the constructor refuses always.
	if _, err := NewManager(base()); err != nil {
		t.Fatalf("a complete config was refused: %v", err)
	}
}

func TestNilJournalAndPacketsAreDiscarding(t *testing.T) {
	// Both are optional and must default to a discarding implementation, not
	// to a nil dereference on the first step.
	cfg := Config{
		Params:    testParams(),
		Transport: newFakeServer(answerNormally),
		Clock:     newFakeClock(),
		Timers:    newFakeTimers(),
		Entropy:   &fakeEntropy{},
	}
	mgr, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := mgr.Journal(); got != nil {
		t.Fatalf("Journal() = %v, want nil from the discarding default", got)
	}
	if got := mgr.Packets(); got != nil {
		t.Fatalf("Packets() = %v, want nil from the discarding default", got)
	}
}

// ------------------------------------------------- decline and release --

// TestManagerReleaseGivesTheLeaseBack drives RFC 2131 section 4.4.6 end to end
// through the manager: the caller asks, the server sees a DHCPRELEASE off the
// wire, and the caller is told the lease is gone with a reason it can branch on.
//
// The Lost event is also the BARRIER, and that is not a convenience: the
// machine emits the send before the announcement, so a Lost in hand proves the
// send already returned. A test that read the server's record without one
// would be racing the manager's goroutine.
func TestManagerReleaseGivesTheLeaseBack(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	r.mgr.Release()

	ev := r.nextEvent(t)
	if ev.Kind != Lost || ev.Reason != proto.ReasonReleased {
		t.Fatalf("event after Release is %s, want lost/released", ev)
	}
	if _, held := r.mgr.Lease(); held {
		t.Fatal("the manager still reports a held lease after releasing it")
	}

	msg := lastSent(t, r, wire.MsgRelease)
	if msg.CIAddr.String() != testYIAddr {
		t.Errorf("ciaddr = %s, want %s (RFC 2131 Table 5 carries the released address in the field, not in option 50)", msg.CIAddr, testYIAddr)
	}
	if _, ok := msg.Options[wire.OptRequestedIP]; ok {
		t.Error("the DHCPRELEASE off the wire carries a requested-ip option, a MUST NOT")
	}
	if sid, ok := msg.Addr4(wire.OptServerID); !ok || sid.String() != testServerID {
		t.Errorf("server-id = %v/%v, want %s (a MUST)", sid, ok, testServerID)
	}

	if s := r.mgr.Stats(); s.ReleasesSent != 1 || s.DeclinesSent != 0 {
		t.Fatalf("stats = %+v, want exactly one release sent and no decline", s)
	}
}

// TestManagerReportConflictDeclinesAndReacquires is RFC 2131 section 3.1(5)
// end to end: the MUST, the wait, and the restart.
//
// THE TRIGGER IS STUBBED. Nothing in this library detects an address
// conflict — no ARP, no duplicate address detection — so the conflict enters
// through ReportConflict, which is a caller's call. The DECLINE path below is
// real; what produces the conflict is not.
func TestManagerReportConflictDeclinesAndReacquires(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	r.mgr.ReportConflict()

	ev := r.nextEvent(t)
	if ev.Kind != Lost || ev.Reason != proto.ReasonConflict {
		t.Fatalf("event after ReportConflict is %s, want lost/conflict", ev)
	}

	msg := lastSent(t, r, wire.MsgDecline)
	if got, ok := msg.Addr4(wire.OptRequestedIP); !ok || got.String() != testYIAddr {
		t.Errorf("requested-ip = %v/%v, want %s (RFC 2131 Table 5 carries the declined address in option 50)", got, ok, testYIAddr)
	}
	if msg.CIAddr.IsValid() && !msg.CIAddr.IsUnspecified() {
		t.Errorf("ciaddr = %s, Table 5 says 0 for a DHCPDECLINE", msg.CIAddr)
	}

	// The wait, and then the restart. Waiting is the whole point of section
	// 3.1(5); a client that redistributed straight back into DISCOVER is the
	// loop it exists to stop.
	if !r.timers.waitArmed(proto.TimerRestart) {
		t.Fatal("no restart timer armed after the DHCPDECLINE")
	}
	d, ok := r.timers.armedAt(proto.TimerRestart)
	if !ok || d < 10*proto.Second {
		t.Fatalf("restart armed for %s/%v, RFC 2131 section 3.1(5) says a minimum of ten seconds", d, ok)
	}

	r.clock.advance(d)
	r.timers.fire(proto.TimerRestart)

	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("event after the restart wait is %s, want a fresh acquisition", ev)
	}
	if s := r.mgr.Stats(); s.DeclinesSent != 1 || s.ReleasesSent != 0 {
		t.Fatalf("stats = %+v, want exactly one decline sent and no release", s)
	}
}

// TestManagerReleaseWithNoLeaseSendsNothing is the preservation control's
// other half: a request the machine has nothing to act on must not put a
// message on the wire.
//
// The rig's server never answers the DISCOVER here, so the manager is in
// SELECTING when the release arrives. The barrier is the retransmit timer
// being re-armed, which only happens after the release has been stepped.
func TestManagerReleaseWithNoLeaseSendsNothing(t *testing.T) {
	silent := func(*wire.Message, int) []*wire.Message { return nil }
	r := newRig(t, testParams(), silent, Fault{})
	if !r.timers.waitArmed(proto.TimerRetransmit) {
		t.Fatal("no retransmit timer armed after the first DISCOVER")
	}

	r.mgr.Release()
	r.journal.waitAppended(t, "the release", func(e proto.JournalEntry) bool {
		return e.Kind == proto.EvRelease
	})

	for _, msg := range r.server.sentMessages() {
		if got, ok := msg.Type(); ok && (got == wire.MsgRelease || got == wire.MsgDecline) {
			t.Fatalf("a %s went out with no lease held", got)
		}
	}
	if s := r.mgr.Stats(); s.ReleasesSent != 0 {
		t.Fatalf("stats = %+v, want no release sent", s)
	}
}

// TestManagerConcurrentRequestsAreSerialisedByTheLoop is defeat-list row 10.
//
// Release and ReportConflict are called from the caller's goroutine while Run
// owns the Machine, so an implementation that stepped the machine directly
// would race every Step. The race detector only reports a race a test actually
// drives, and every other test here calls one request once, from one
// goroutine, at a moment the loop is parked between events. This one calls
// both, many times, from many goroutines.
//
// It also drives the idempotency the request channel's depth assumes: whichever
// request wins, the machine has no lease afterwards, so EXACTLY ONE terminal
// message may reach the wire no matter how many callers asked.
func TestManagerConcurrentRequestsAreSerialisedByTheLoop(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s", ev)
	}

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(2 * callers)
	for i := 0; i < callers; i++ {
		go func() { defer wg.Done(); r.mgr.Release() }()
		go func() { defer wg.Done(); r.mgr.ReportConflict() }()
	}
	wg.Wait()

	// Which of the two wins is a scheduling outcome and is not asserted; that
	// only one of them puts a message on the wire is the invariant.
	ev := r.nextEvent(t)
	if ev.Kind != Lost || (ev.Reason != proto.ReasonReleased && ev.Reason != proto.ReasonConflict) {
		t.Fatalf("event after the requests is %s, want lost/released or lost/conflict", ev)
	}
	if _, held := r.mgr.Lease(); held {
		t.Fatal("the manager still reports a held lease")
	}

	terminal := 0
	for _, msg := range r.server.sentMessages() {
		if got, ok := msg.Type(); ok && (got == wire.MsgRelease || got == wire.MsgDecline) {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("%d terminal message(s) on the wire from %d callers; the lease can only be given up once", terminal, 2*callers)
	}
	if s := r.mgr.Stats(); s.ReleasesSent+s.DeclinesSent != 1 {
		t.Fatalf("stats = %+v, want exactly one terminal message counted", s)
	}
}

// TestReleaseDuringAcquisitionStopsTheClient is the window Release fell
// through until round 4.
//
// EvRelease was handled only in BOUND. A caller that released while the client
// was still acquiring got a journal line and nothing else: the machine went on
// to take an address the caller had already given up, with nothing left that
// would ever release it. Manager.Release's documented remedy — a Release that
// produced no Lost was dropped, so call again — could not terminate either,
// because the second call fell through the same default.
//
// The fixture holds SELECTING open rather than racing it: the server is deaf
// to the FIRST DISCOVER only, so the machine sits on its retransmit timer, and
// that timer is fired AFTER the release. Firing it is the point. Without it
// this test could not tell a client that has stopped from one that is merely
// between retransmissions, and "no acquisition follows" would be a claim about
// timing rather than about state.
func TestReleaseDuringAcquisitionStopsTheClient(t *testing.T) {
	deafToTheFirstDiscover := func(req *wire.Message, n int) []*wire.Message {
		if tp, ok := req.Type(); ok && tp == wire.MsgDiscover && n == 1 {
			return nil
		}
		return answerNormally(req, n)
	}
	r := newRig(t, testParams(), deafToTheFirstDiscover, Fault{})

	// In SELECTING with the window open: the DISCOVER is out and unanswered,
	// and the retransmit is armed. A barrier, not a delay.
	if !r.timers.waitArmed(proto.TimerRetransmit) {
		t.Fatal("the retransmit timer was never armed, so the client never reached SELECTING and this test would measure nothing")
	}

	r.mgr.Release()
	r.journal.waitAppended(t, "the release", func(e proto.JournalEntry) bool {
		return e.Kind == proto.EvRelease
	})

	rel := findEntry(t, r, "the release", func(e proto.JournalEntry) bool {
		return e.Kind == proto.EvRelease
	})
	if rel.From != proto.StateSelecting {
		t.Fatalf("the release was seen in %s, not SELECTING: the fixture did not hold the window open", rel.From)
	}
	if rel.To != proto.StateStopped {
		t.Fatalf("release in SELECTING left the machine in %s, want STOPPED — the caller gave the address up and this client is still acquiring", rel.To)
	}

	// Fire the retransmit the machine was sitting on. In SELECTING it sends a
	// second DHCPDISCOVER; in STOPPED it must do nothing at all.
	r.timers.fire(proto.TimerRetransmit)
	r.journal.waitAppended(t, "the retransmit after the release", func(e proto.JournalEntry) bool {
		return e.Kind == proto.EvTimerFired && e.Timer == proto.TimerRetransmit && e.From == proto.StateStopped
	})
	fire := findEntry(t, r, "the retransmit after the release", func(e proto.JournalEntry) bool {
		return e.Kind == proto.EvTimerFired && e.Timer == proto.TimerRetransmit && e.From == proto.StateStopped
	})
	if fire.To != proto.StateStopped {
		t.Fatalf("the retransmit moved the stopped machine to %s: the acquisition the caller released is running again", fire.To)
	}

	// Outside evidence for the same fact: what actually went on the wire.
	// Exactly one DISCOVER, ever, and no DHCPRELEASE — there was no lease to
	// give back, so a release message would name a binding this client never
	// held.
	var discovers, releases int
	for _, c := range r.mgr.Packets() {
		if c.Dir != DirOut || c.Msg == nil {
			continue
		}
		switch tp, _ := c.Msg.Type(); tp {
		case wire.MsgDiscover:
			discovers++
		case wire.MsgRelease:
			releases++
		}
	}
	if discovers != 1 {
		t.Errorf("%d DHCPDISCOVERs went out, want 1: the client kept acquiring after it was released", discovers)
	}
	if releases != 0 {
		t.Errorf("%d DHCPRELEASEs went out, want 0: nothing was leased, so there is no binding to relinquish", releases)
	}

	// No Lost, and no Acquired. STOPPED is only left on EvStart, so after the
	// barrier above nothing more can be emitted and this read is not a race.
	for {
		select {
		case ev, ok := <-r.mgr.Events():
			if !ok {
				return
			}
			t.Fatalf("the client emitted %s after a release that held no lease: Lost is the confirmation a lease was given back, so it must not arrive when there was none", ev)
		default:
			return
		}
	}
}
