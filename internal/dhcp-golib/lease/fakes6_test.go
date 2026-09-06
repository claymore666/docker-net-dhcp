package lease

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// The v6 rig. It is fakes_test.go's rig with the v6 ports substituted and
// nothing else changed, which is D30 at the level of the test suite: a
// different harness for the second family would make every comparison between
// them an argument about the harnesses.

// server6Behaviour answers one client message with zero or more replies. It is
// serverBehaviour's counterpart.
type server6Behaviour func(req *wire.MessageV6, n int) []*wire.MessageV6

// fakeServer6 is a TransportV6 that answers DHCPv6 messages the way a server
// on the same link would, pushing on an UNBUFFERED channel so the push
// completes only once the manager has taken it.
type fakeServer6 struct {
	behaviour server6Behaviour

	mu   sync.Mutex
	sent []*wire.MessageV6
	dest []proto.Dest
	n    int

	inbound chan Inbound
	seen    chan wire.MessageTypeV6
	closed  chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func newFakeServer6(b server6Behaviour) *fakeServer6 {
	return &fakeServer6{
		behaviour: b,
		inbound:   make(chan Inbound),
		seen:      make(chan wire.MessageTypeV6, 64),
		closed:    make(chan struct{}),
	}
}

func (s *fakeServer6) Send(dst proto.Dest, payload []byte) error {
	msg, err := wire.DecodeV6(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.n++
	n := s.n
	s.sent = append(s.sent, msg)
	s.dest = append(s.dest, dst)
	s.mu.Unlock()
	select {
	case s.seen <- msg.Type:
	default:
	}

	for _, reply := range s.behaviour(msg, n) {
		raw, err := wire.EncodeV6(reply)
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func(raw []byte) {
			defer s.wg.Done()
			select {
			case s.inbound <- Inbound{Payload: raw, From: netip.MustParseAddr("fe80::1")}:
			case <-s.closed:
			}
		}(raw)
	}
	return nil
}

func (s *fakeServer6) Received() <-chan Inbound { return s.inbound }

func (s *fakeServer6) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.wg.Wait()
		close(s.inbound)
	})
	return nil
}

func (s *fakeServer6) sentMessages() []*wire.MessageV6 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*wire.MessageV6(nil), s.sent...)
}

func (s *fakeServer6) destinations() []proto.Dest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]proto.Dest(nil), s.dest...)
}

func (s *fakeServer6) injectRaw(raw []byte) {
	select {
	case s.inbound <- Inbound{Payload: raw, From: netip.MustParseAddr("fe80::1")}:
	case <-s.closed:
	}
}

// fakeND is an ND port that records what went out and lets a test push frames
// in. It is the ARP fake's counterpart.
type fakeND struct {
	mu   sync.Mutex
	sent []wire.ICMPv6Packet

	in     chan NDInbound
	closed chan struct{}
	once   sync.Once
}

func newFakeND() *fakeND {
	return &fakeND{in: make(chan NDInbound), closed: make(chan struct{})}
}

func (n *fakeND) Send(pkt wire.ICMPv6Packet) error {
	n.mu.Lock()
	n.sent = append(n.sent, pkt)
	n.mu.Unlock()
	return nil
}

func (n *fakeND) Received() <-chan NDInbound { return n.in }

func (n *fakeND) Close() error {
	n.once.Do(func() { close(n.closed); close(n.in) })
	return nil
}

func (n *fakeND) sentPackets() []wire.ICMPv6Packet {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]wire.ICMPv6Packet(nil), n.sent...)
}

// inject pushes one frame at the manager. The channel is unbuffered, so the
// push completes only once the manager has taken it.
func (n *fakeND) inject(frame []byte) {
	select {
	case n.in <- NDInbound{Frame: frame}:
	case <-n.closed:
	}
}

// journalRecorder6 is the v6 journal, recording every Step for Replay6.
type journalRecorder6 struct {
	mu       sync.Mutex
	entries  []proto.JournalEntry6
	appended chan proto.JournalEntry6
	// markers carries only rig6.settle's marker Steps, on a channel of its
	// own. appended drops on a full buffer, which is right for a stream nobody
	// has to consume and wrong for a barrier: a dropped marker is a settle
	// that never returns.
	markers chan struct{}
}

func newJournalRecorder6() *journalRecorder6 {
	return &journalRecorder6{
		appended: make(chan proto.JournalEntry6, 64),
		markers:  make(chan struct{}, 64),
	}
}

func (j *journalRecorder6) Append(e proto.JournalEntry6) {
	j.mu.Lock()
	j.entries = append(j.entries, e)
	j.mu.Unlock()
	if e.Kind == proto.EvTimerFired && e.Timer == proto.TimerACD {
		j.markers <- struct{}{}
		return
	}
	select {
	case j.appended <- e:
	default:
	}
}

// waitAppended blocks until a journalled Step satisfies want. It is
// journalRecorder.waitAppended for the second machine, and it is the barrier
// for the same reason: a journal entry is appended AFTER Step and after the
// previous event's actions have drained, so it is the only thing in this
// package that proves the machine has SEEN something.
func (j *journalRecorder6) waitAppended(t *testing.T, what string, want func(proto.JournalEntry6) bool) {
	t.Helper()
	for e := range j.appended {
		if want(e) {
			return
		}
	}
	t.Fatalf("the journal closed before %s was recorded", what)
}

func (j *journalRecorder6) Entries() []proto.JournalEntry6 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]proto.JournalEntry6(nil), j.entries...)
}

// packetRecorder6 is the v6 packet ring.
type packetRecorder6 struct {
	mu sync.Mutex
	p  []CapturedPacketV6
}

func newPacketRecorder6() *packetRecorder6 { return &packetRecorder6{} }

func (p *packetRecorder6) Record(c CapturedPacketV6) {
	p.mu.Lock()
	p.p = append(p.p, c)
	p.mu.Unlock()
}

func (p *packetRecorder6) Packets() []CapturedPacketV6 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]CapturedPacketV6(nil), p.p...)
}

// -------------------------------------------------------------- fixtures --

const (
	test6Addr   = "fd00:99::183"
	test6DNS    = "fd00:99::1"
	test6Search = "fixture.invalid"
)

var (
	test6DUID       = mustHex("00030001ea494ee531ed")
	test6ServerDUID = mustHex("00010001322ecbbfea494ee531ed")
)

const test6IAID uint32 = 0x0a0b0c0d

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func testParams6() proto.Params6 {
	p := proto.DefaultParams6()
	p.DUID = append([]byte(nil), test6DUID...)
	p.IAID = test6IAID
	return p
}

func optV6(code wire.OptionCodeV6, data []byte) wire.OptionV6 {
	return wire.OptionV6{Code: code, Data: data}
}

func ianaOption(t *testing.T, iaid, t1, t2 uint32, addr string, secs uint32, extra ...wire.OptionV6) wire.OptionV6 {
	t.Helper()
	ia := &wire.IANA{IAID: iaid, T1: t1, T2: t2}
	if addr != "" {
		v, err := wire.EncodeIAAddr(&wire.IAAddr{
			Addr:              netip.MustParseAddr(addr),
			PreferredLifetime: secs,
			ValidLifetime:     secs,
		})
		if err != nil {
			t.Fatalf("EncodeIAAddr: %v", err)
		}
		ia.Options = append(ia.Options, optV6(wire.OptV6IAAddr, v))
	}
	ia.Options = append(ia.Options, extra...)
	v, err := wire.EncodeIANA(ia)
	if err != nil {
		t.Fatalf("EncodeIANA: %v", err)
	}
	return optV6(wire.OptV6IANA, v)
}

// advertiseFor and replyFor are what a server on this link would answer.
func advertiseFor(t *testing.T, req *wire.MessageV6) *wire.MessageV6 {
	t.Helper()
	return &wire.MessageV6{
		Type: wire.MsgAdvertise, XID: req.XID,
		Options: wire.OptionsV6{
			optV6(wire.OptV6ClientID, test6DUID),
			optV6(wire.OptV6ServerID, test6ServerDUID),
			ianaOption(t, test6IAID, 150, 240, test6Addr, 300),
			optV6(wire.OptV6Preference, []byte{255}),
		},
	}
}

func replyFor(t *testing.T, req *wire.MessageV6) *wire.MessageV6 {
	t.Helper()
	search, err := wire.EncodeDomainSearch([]string{test6Search})
	if err != nil {
		t.Fatalf("EncodeDomainSearch: %v", err)
	}
	dns := netip.MustParseAddr(test6DNS).As16()
	return &wire.MessageV6{
		Type: wire.MsgReply, XID: req.XID,
		Options: wire.OptionsV6{
			optV6(wire.OptV6ClientID, test6DUID),
			optV6(wire.OptV6ServerID, test6ServerDUID),
			ianaOption(t, test6IAID, 150, 240, test6Addr, 300),
			optV6(wire.OptV6DNSServers, dns[:]),
			optV6(wire.OptV6DomainList, search),
		},
	}
}

// answerNormally6 is the ordinary exchange: Advertise for the Solicit, Reply
// for everything else that expects one.
func answerNormally6(t *testing.T) server6Behaviour {
	return func(req *wire.MessageV6, _ int) []*wire.MessageV6 {
		switch req.Type {
		case wire.MsgSolicit:
			return []*wire.MessageV6{advertiseFor(t, req)}
		case wire.MsgRequest6, wire.MsgRenew, wire.MsgRebind, wire.MsgDecline6, wire.MsgRelease6:
			return []*wire.MessageV6{replyFor(t, req)}
		}
		return nil
	}
}

// silent6 answers nothing, for the tests that drive the machine by timer.
func silent6(*wire.MessageV6, int) []*wire.MessageV6 { return nil }

// ------------------------------------------------------------------ rig --

type rig6 struct {
	mgr     *Manager
	server  *fakeServer6
	nd      *fakeND
	timers  *fakeTimers
	clock   *fakeClock
	journal *journalRecorder6
	packets *packetRecorder6
	cancel  context.CancelFunc
	done    chan error

	waitOnce sync.Once
	runErr   error
}

func (r *rig6) wait() error {
	r.waitOnce.Do(func() { r.runErr = <-r.done })
	return r.runErr
}

func (r *rig6) stop() error {
	r.cancel()
	return r.wait()
}

// rig6Option sets a Config field that is not Params6, for rigOption's reason.
type rig6Option func(*Config)

func withRateLimit(l RateLimit) rig6Option { return func(c *Config) { c.RateLimit = l } }

func withResume6(l Lease) rig6Option { return func(c *Config) { c.Resume6 = &l } }

func withDAD(d DADRunner) rig6Option { return func(c *Config) { c.DAD = d } }

// fakeDAD is a DADRunner, and it is the port's contract rather than a stand-in
// for runtime.DADProbe's exchange: it records the addresses it was asked
// about and answers each with a fixed verdict.
//
// IT ANSWERS FROM ANOTHER GOROUTINE BECAUSE THE PORT SAYS report IS CALLED
// FROM ONE. Manager calls Start while dispatching a Step, and
// Manager.ReportDADResult posts into the same request channel that dispatch is
// draining, so a runner that reported inline would deadlock the manager on its
// first acquisition. A fake that got that wrong would pass this suite and the
// real runner would hang; answering the way the doc comment requires is what
// makes this fake evidence about the seam.
type fakeDAD struct {
	verdict bool
	// silent models a runner that takes the question and never answers, which
	// is the case ring 1's own deadline exists for.
	silent bool

	mu       sync.Mutex
	done     *sync.Cond
	started  []netip.Addr
	inflight int
}

// idle returns the condition the answered barrier waits on, built on first use
// under the lock its caller already holds.
//
// A sync.WaitGroup stood here and it was a RACE, MEASURED 2026-09-06 by the
// race detector inside the arbiter's own unit-suite row: the manager goroutine
// calls Start — and so Add — while the test goroutine is already inside Wait,
// which is precisely the ordering WaitGroup forbids. It reproduced about one
// run in three and it failed the whole pure suite when it did, so it was worth
// more than the four lines it costs to state the wait over a counter this
// fake owns.
func (d *fakeDAD) idle() *sync.Cond {
	if d.done == nil {
		d.done = sync.NewCond(&d.mu)
	}
	return d.done
}

func (d *fakeDAD) Start(addr netip.Addr, report func(netip.Addr, bool)) {
	d.mu.Lock()
	d.started = append(d.started, addr)
	if d.silent {
		d.mu.Unlock()
		return
	}
	d.inflight++
	d.mu.Unlock()
	go func() {
		report(addr, d.verdict)
		d.mu.Lock()
		d.inflight--
		d.idle().Broadcast()
		d.mu.Unlock()
	}()
}

// answered blocks until every report goroutine this fake started has posted
// its verdict into the manager's request channel.
//
// IT IS WHAT KEEPS A REMOVED SEAM A FAILURE RATHER THAN A HANG. A test that
// waited for the Acquired event instead would hang on exactly the mutant it
// exists to catch — the ActStartDAD arm not calling Start at all — and
// mutate.sh scores a hang as its own verdict and explicitly not as a kill.
// MEASURED 2026-09-06: with `mg.cfg.DAD.Start(a.Target, mg.ReportDADResult)`
// removed, a rig6.nextEvent-based version of these tests hung for the full 60s
// timeout. With this barrier plus rig6.settle they FAIL, naming the missing
// call.
//
// It returns IMMEDIATELY when nothing was started, which is the whole point:
// "the runner answered" and "the runner was never asked" then differ by what
// the journal and the event channel hold, which a settled read can state.
func (d *fakeDAD) answered() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.inflight > 0 {
		d.idle().Wait()
	}
}

func (d *fakeDAD) asked() []netip.Addr {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]netip.Addr(nil), d.started...)
}

func newRig6(t *testing.T, p proto.Params6, behaviour server6Behaviour, opts ...rig6Option) *rig6 {
	t.Helper()
	return newRig6On(t, newFakeClock(), p, behaviour, opts...)
}

func newRig6On(t *testing.T, clk *fakeClock, p proto.Params6, behaviour server6Behaviour, opts ...rig6Option) *rig6 {
	t.Helper()

	srv := newFakeServer6(behaviour)
	r := &rig6{
		server:  srv,
		nd:      newFakeND(),
		timers:  newFakeTimers(),
		clock:   clk,
		journal: newJournalRecorder6(),
		packets: newPacketRecorder6(),
		done:    make(chan error, 1),
	}

	cfg := Config{
		Params6:     &p,
		TransportV6: srv,
		ND:          r.nd,
		Clock:       r.clock,
		Timers:      r.timers,
		Entropy:     &fakeEntropy{},
		Journal6:    r.journal,
		PacketsV6:   r.packets,
		EventBuffer: 16,
		LinkLocal:   netip.MustParseAddr("fe80::e849:4eff:fee5:31ed"),
		LinkHW:      mustHex("ea494ee531ed"),
	}
	for _, o := range opts {
		o(&cfg)
	}
	mgr, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	r.mgr = mgr

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- mgr.Run(ctx) }()

	// RFC 9915 §18.2.1 delays the first Solicit "by a random amount of time
	// between 0 and SOL_MAX_DELAY" (§18.2.3 says the same of the first
	// Confirm), so NOTHING leaves the host until that timer fires and these
	// timers never fire on their own. The rig fires it, so every test below
	// starts from a client that has begun talking; the delay itself is ring
	// 1's to assert, and proto's suite does.
	if r.timers.waitArmed(proto.Timer6Delay) {
		r.timers.fire(proto.Timer6Delay)
	}

	t.Cleanup(func() {
		_ = r.stop()
		_ = srv.Close()
		_ = r.timers.Close()
	})
	return r
}

// nextEvent reads one outward event. It blocks on the channel, which is the
// point: an event that never arrives is a failure of the test, not of the rig.
// Where the event is the thing a mutant can remove, use takeEvent instead.
func (r *rig6) nextEvent(t *testing.T) Event {
	t.Helper()
	e, ok := <-r.mgr.Events()
	if !ok {
		t.Fatal("the event channel closed before the expected event arrived")
	}
	return e
}

// takeEvent reads an event that must ALREADY be queued, settling first.
//
// WHY IT IS NOT A PLAIN CHANNEL READ. Every mutant that stops an event being
// emitted at all turns a plain read into a hang, and mutate.sh scores a hang
// as its own verdict and explicitly not as a kill. MEASURED 2026-09-06: the
// proto/event.go mutants "the DADResult constructor inverts its verdict" and
// "the DADResult constructor drops the address" both scored HUNG against
// acquire6's unbounded read, while proto's own suite failed under each of
// them.
//
// THE BOUND IS EVENT-DRIVEN, NOT A CLOCK. Gate T2 refuses time.After and
// context.WithTimeout in a _test.go file, and it is right to: a wall-clock
// bound makes the verdict depend on how loaded the box is. rig6.settle is the
// happens-after this package already has — the marker Step is journalled only
// once everything the manager had taken has finished dispatching, and
// Manager.emit sends into a buffered channel during that dispatch — so an
// event produced by a Step the manager has already taken is queued by the time
// settle returns.
//
// ITS PRECONDITION IS THAT NOTHING IS STILL IN FLIGHT FROM THE SERVER.
// fakeServer6.Send hands its replies to a goroutine, so a Reply can be queued
// for delivery while the manager is idle and settle would return before the
// machine ever saw it. That is why this is not nextEvent: it is for events
// that fall out of a Step the caller has already witnessed being journalled —
// acquire6's Acquired falls out of the EvDADResult Step settleDAD waits for —
// and not for events that still need a round trip.
func (r *rig6) takeEvent(t *testing.T) Event {
	t.Helper()
	r.settle(t)
	select {
	case e, ok := <-r.mgr.Events():
		if !ok {
			t.Fatal("the event channel closed before the expected event arrived")
		}
		return e
	default:
		t.Fatalf("no event was emitted; the machine has settled in %s and its last Steps were:\n%s",
			r.state6(), r.tail6(6))
		return Event{}
	}
}

// state6 is the state the last journalled Step left the machine in, for a
// failure message. It reads the recorder's snapshot, so it belongs after a
// barrier and not instead of one.
func (r *rig6) state6() proto.State6 {
	entries := r.mgr.Journal6()
	for i := len(entries) - 1; i >= 0; i-- {
		if !entries[i].Note {
			return entries[i].To
		}
	}
	return proto.State6Init
}

// tail6 renders the last n journalled Steps, which is what a bounded barrier
// owes its reader: the bound says "this did not happen", and these lines say
// what happened instead.
func (r *rig6) tail6(n int) string {
	entries := r.mgr.Journal6()
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("  #%d %s %s -> %s %v\n", e.Seq, e.Kind, e.From, e.To, e.Actions))
	}
	return b.String()
}

// acquire6 drives the rig to its Acquired event, answering the duplicate
// address detection request the machine makes on the way.
//
// THE DAD RESULT IS FED BY THE TEST BECAUSE RING 3 IS NOT BUILT YET (M7c).
// That is the port's contract exercised rather than stubbed away: the machine
// emits proto.ActStartDAD, this ring records it, and the answer arrives as the
// proto.EvDADResult ring 3 will one day produce.
func (r *rig6) acquire6(t *testing.T) Event {
	t.Helper()
	r.settleDAD(t, test6Addr, false)
	e := r.takeEvent(t)
	if e.Kind != Acquired {
		t.Fatalf("the first event is %s, want acquired", e.Kind)
	}
	return e
}

// settleDAD waits for the machine to ask about addr, answers, and waits for
// the answer to be STEPPED.
//
// THE SECOND BARRIER IS WHAT MAKES rig6.nextEvent's BOUND SOUND. ReportDADResult
// posts into Manager's requests channel and returns; Run selects between that
// channel and the timers, and settle's marker arrives on the timers. Without
// this wait, settle could dispatch the marker first and nextEvent would read
// an empty channel while the answer was still queued — a flake, not a finding.
// Waiting for the EvDADResult entry says the answer has been taken; settle
// then says its actions have drained.
//
// It waits for the KIND and not for the address, deliberately: an answer whose
// address the machine does not recognise is journalled and ignored (see
// Machine6.takeDADResult), and that is exactly the case a mutant that drops
// the address produces. Keying on the address here would put the hang back.
func (r *rig6) settleDAD(t *testing.T, addr string, duplicate bool) {
	t.Helper()
	r.waitDADRequested(t, addr)
	r.mgr.ReportDADResult(netip.MustParseAddr(addr), duplicate)
	r.waitDADStepped(t)
}

// waitDADStepped blocks until the machine has STEPPED a duplicate-address
// verdict — not until one was posted, which is a different claim.
//
// IT IS HERE BECAUSE settle CANNOT MAKE THIS CLAIM, and two settles cannot
// make it either. settle fires the marker timer and waits for its Step;
// Manager.Run selects between the timer channel and the request channel, so a
// verdict already queued has an even chance of being served after the marker.
// A second settle is a second coin toss, not a proof. MEASURED 2026-09-06:
// with two settles and no journal barrier, the two tests that use this
// helper's siblings — TestTheConfiguredRunnersDuplicateVerdictDeclines and
// TestTheConfiguredRunnerIsAskedAndItsAnswerIsTheMachines — each failed about
// two runs in thirty ("no event was emitted; the machine has settled in
// DAD6"), and they took three oracle scenarios down with them.
//
// The journal entry is appended BEFORE the Step's actions drain (see
// Manager.dispatch), so this barrier says the verdict was seen and NOT that
// what it produced has happened. A caller that reads an event, a sent message
// or a counter still needs a settle after it — takeEvent carries its own.
func (r *rig6) waitDADStepped(t *testing.T) {
	t.Helper()
	r.journal.waitAppended(t, "the machine's Step on the runner's verdict",
		func(e proto.JournalEntry6) bool { return e.Kind == proto.EvDADResult })
}

// waitDADRequested blocks until the journal shows the machine asked about addr.
//
// IT STOPS ON THREE THINGS, NOT ONE, AND TWO OF THEM ARE FAILURES. A barrier
// that only waited for the line it wants turns every way of not producing that
// line into a HANG, and a hang is a weaker verdict than a failure: it says the
// suite hit a bound of its own, not what the code did.
//
//   - the request naming addr — what it is waiting for;
//   - the machine reaching BOUND6 — a client that skipped RFC 9915
//     §18.2.10.1's check never asks at all;
//   - a StartDAD line that does NOT name addr — the request went out about
//     something else, or the action stopped carrying its target;
//   - a message received in SELECTING6 that leaves the machine in SELECTING6 —
//     this rig's server answers every Solicit with an Advertise carrying a
//     Preference option of 255, and RFC 9915 §18.2.1 says of that value that
//     "the client immediately begins a client-initiated message exchange (as
//     described in Section 18.2.2) by sending a Request message to the server
//     from which the Advertise message was received", so a client still
//     selecting after that Advertise will never reach the Reply that makes it
//     ask about an address at all.
//
// MEASURED 2026-09-06: the third arm did not exist and the proto/action.go
// mutant "the StartDAD action drops its target" scored HUNG rather than
// KILLED. proto's own TestV6ActionPayloadsRender fails under it; this
// package hung, and the hang is the verdict mutate.sh reports. The fourth arm
// did not exist and the proto/machine6.go mutant "18.2.1: preference 255 waits
// out the collection window like any other" scored HUNG for the same reason,
// against a proto suite that fails under it.
func (r *rig6) waitDADRequested(t *testing.T, addr string) {
	t.Helper()
	bound := false
	wrong := ""
	stalled := false
	r.journal.waitAppended(t, "the duplicate address detection request for "+addr,
		func(e proto.JournalEntry6) bool {
			if e.To == proto.State6Bound {
				bound = true
				return true
			}
			if e.Kind == proto.EvReceived && e.From == proto.State6Selecting && e.To == proto.State6Selecting {
				stalled = true
				return true
			}
			for _, a := range e.Actions {
				if !strings.Contains(a, "StartDAD") {
					continue
				}
				if strings.Contains(a, addr) {
					return true
				}
				wrong = a
				return true
			}
			return false
		})
	if bound {
		t.Fatalf("the machine reached BOUND6 without asking for duplicate address detection on %s: RFC 9915 §18.2.10.1 says \"The client performs the duplicate address detection before using the received addresses for any traffic\"", addr)
	}
	if wrong != "" {
		t.Fatalf("the duplicate address detection request reads %q and does not name %s; ring 3 is told which address to check by that action and nothing else", wrong, addr)
	}
	if stalled {
		t.Fatalf("an Advertise arrived while soliciting and left the machine in %s; its Preference option is 255 and RFC 9915 §18.2.1 says the client \"immediately begins a client-initiated message exchange\" on that value", proto.State6Selecting)
	}
}

// waitSent blocks until a message of this type has actually left the host.
//
// IT IS THE SEND BARRIER AND THE JOURNAL IS NOT. Manager.dispatch appends a
// journal entry BEFORE it drains that entry's own actions, so "the journal
// shows a SendV6 action" means the machine decided to send and not that
// anything reached a socket. This channel is written by the fake transport's
// Send, which is the far side of the port.
func (r *rig6) waitSent(t *testing.T, want wire.MessageTypeV6) {
	t.Helper()
	for {
		select {
		case got, ok := <-r.server.seen:
			if !ok {
				t.Fatalf("the transport closed before a %s was sent", want)
			}
			if got == want {
				return
			}
		case <-r.server.closed:
			t.Fatalf("the transport closed before a %s was sent", want)
		}
	}
}

// findEntry6 returns the first journalled Step satisfying want. It reads the
// recorder's snapshot, so it must follow a waitAppended for the same predicate
// rather than replace one: the barrier is what makes the entry present.
func findEntry6(t *testing.T, r *rig6, what string, want func(proto.JournalEntry6) bool) proto.JournalEntry6 {
	t.Helper()
	for _, e := range r.mgr.Journal6() {
		if want(e) {
			return e
		}
	}
	t.Fatalf("no journal entry for %s", what)
	return proto.JournalEntry6{}
}

// journalHas6 reports whether any recorded Step mentions sub.
func journalHas6(r *rig6, sub string) bool {
	for _, e := range r.mgr.Journal6() {
		if strings.Contains(e.Reason, sub) {
			return true
		}
		for _, a := range e.Actions {
			if strings.Contains(a, sub) {
				return true
			}
		}
	}
	return false
}

// lastSent6 returns the last message the fake SERVER decoded, which is the
// bytes that left rather than the machine's opinion of them.
func lastSent6(t *testing.T, r *rig6, want wire.MessageTypeV6) *wire.MessageV6 {
	t.Helper()
	sent := r.server.sentMessages()
	if len(sent) == 0 {
		t.Fatal("the server saw nothing")
	}
	msg := sent[len(sent)-1]
	if msg.Type != want {
		t.Fatalf("the last message the server saw is %s, want %s", msg.Type, want)
	}
	return msg
}

// settle returns once everything the manager had ALREADY TAKEN has finished
// dispatching. It is this package's happens-after, and it is not a wait.
//
// HOW IT WORKS. Manager.Run takes one event at a time, and Manager.dispatch
// drains that event's whole cascade — its actions, the events those actions
// produce, and those events' actions — before Run selects again. So a timer
// fired behind an event that has already been taken is journalled after that
// event's last action, and the marker entry appearing is the ordering this
// package otherwise has no way to state.
//
// THE MARKER IS TimerACD, which is one of the V4 machine's timer ids. Machine6
// is total over it and acts on it in no state — every state journals it as a
// timer that fired and was ignored — so the marker cannot change what is being
// measured, and the entry carries the id whatever the machine did with it.
//
// WHY IT EXISTS. Waiting for the thing under test turns every mutant that
// REMOVES that thing into a HANG, and mutate.sh scores a hang as its own
// verdict and explicitly not as a kill: a barrier that can only succeed
// measures nothing. MEASURED, four mutants scored HUNG before this helper
// existed — §14.1's bucket never refusing, a refused send reported to the
// machine as a success, a duplicate address dropped without a Decline, and
// M=0 O=1 not switching in SELECTING6. Each is a KILL against a test that
// settles and then READS what happened.
func (r *rig6) settle(t *testing.T) {
	t.Helper()
	r.timers.fire(proto.TimerACD)
	select {
	case <-r.journal.markers:
	case <-r.server.closed:
		t.Fatal("the transport closed before the marker timer was dispatched")
	}
}
