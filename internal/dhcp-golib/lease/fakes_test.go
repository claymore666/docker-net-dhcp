package lease

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// The fakes below are what make requirement T3 real: the whole acquisition
// path runs with no root, no namespace, no network and no clock.
//
// Not one of them waits. Every barrier in these tests is a CHANNEL HANDOFF —
// an event read, a timer arming observed — because a test that waits on a
// duration is a test whose failures come back as flakes, and this project has
// a gate (T2) that refuses one.

// fakeClock returns whatever the test set. Both clocks move together and only
// when the test moves them.
type fakeClock struct {
	mu   sync.Mutex
	mono proto.Instant
	wall time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{wall: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Mono() proto.Instant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *fakeClock) Wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *fakeClock) advance(d proto.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono = c.mono.Add(d)
	c.wall = c.wall.Add(time.Duration(d))
}

// fakeEntropy is a counter run through the same mixer ring 1 uses. Determinism
// is the point: a test that cannot reproduce its own run cannot bisect a
// failure.
type fakeEntropy struct {
	mu sync.Mutex
	n  uint64
}

func (e *fakeEntropy) Uint64() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	z := e.n * 0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	return z ^ (z >> 31)
}

// fakeTimers never fires on its own. The test fires timers explicitly, and
// every Set is announced on armed so a test can use "the retransmit timer was
// re-armed" as a barrier instead of a sleep.
//
// Nothing here closes the fired channel, and that is deliberate. The manager
// treats a closed timer channel as "something took the timers away" and exits
// with an error — correct in production, and wrong as a way to end a test. A
// test ends by cancelling the context. Close only releases whatever is still
// blocked in fire or waitArmed, so a helper goroutine cannot outlive the test
// it belongs to; a helper that survives its test either leaks or sends on a
// closed channel, and the second one is a panic in an unrelated test.
type fakeTimers struct {
	mu     sync.Mutex
	set    map[proto.TimerID]proto.Duration
	closed bool

	armed chan proto.TimerID
	fired chan proto.TimerID
	done  chan struct{}
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{
		set:   map[proto.TimerID]proto.Duration{},
		armed: make(chan proto.TimerID, 64),
		fired: make(chan proto.TimerID, 8),
		done:  make(chan struct{}),
	}
}

func (t *fakeTimers) Set(id proto.TimerID, d proto.Duration) {
	t.mu.Lock()
	t.set[id] = d
	t.mu.Unlock()
	select {
	case t.armed <- id:
	default:
	}
}

func (t *fakeTimers) Cancel(id proto.TimerID) {
	t.mu.Lock()
	delete(t.set, id)
	t.mu.Unlock()
}

func (t *fakeTimers) Fired() <-chan proto.TimerID { return t.fired }

func (t *fakeTimers) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.done)
	}
	return nil
}

// fire delivers one timer fire, or returns once the timers are closed.
func (t *fakeTimers) fire(id proto.TimerID) {
	select {
	case t.fired <- id:
	case <-t.done:
	}
}

func (t *fakeTimers) armedAt(id proto.TimerID) (proto.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.set[id]
	return d, ok
}

// waitArmed blocks until the given timer is armed, draining others, and gives
// up when the timers are closed.
func (t *fakeTimers) waitArmed(id proto.TimerID) bool {
	for {
		select {
		case got := <-t.armed:
			if got == id {
				return true
			}
		case <-t.done:
			return false
		}
	}
}

// serverBehaviour decides what a fake server answers with.
type serverBehaviour func(req *wire.Message, n int) []*wire.Message

// fakeServer is a Transport that answers DHCP messages the way a server on the
// same segment would.
//
// The reply is pushed on an UNBUFFERED channel from a goroutine, so the push
// completes only once the manager has taken it. Tests use that, and the event
// stream, as their only barriers.
type fakeServer struct {
	behaviour serverBehaviour

	mu   sync.Mutex
	sent []*wire.Message
	n    int

	inbound chan Inbound
	closed  chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func newFakeServer(b serverBehaviour) *fakeServer {
	return &fakeServer{
		behaviour: b,
		inbound:   make(chan Inbound),
		closed:    make(chan struct{}),
	}
}

func (s *fakeServer) Send(_ proto.Dest, payload []byte) error {
	msg, err := wire.Decode(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.n++
	n := s.n
	s.sent = append(s.sent, msg)
	s.mu.Unlock()

	for _, reply := range s.behaviour(msg, n) {
		raw, err := wire.Encode(reply)
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func(raw []byte) {
			defer s.wg.Done()
			select {
			case s.inbound <- Inbound{Payload: raw, From: netip.MustParseAddr("192.168.99.1")}:
			case <-s.closed:
			}
		}(raw)
	}
	return nil
}

func (s *fakeServer) Received() <-chan Inbound { return s.inbound }

func (s *fakeServer) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.wg.Wait()
		close(s.inbound)
	})
	return nil
}

func (s *fakeServer) sentMessages() []*wire.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*wire.Message(nil), s.sent...)
}

// injectRaw pushes arbitrary bytes at the manager, for the cases a
// well-behaved server would never produce.
func (s *fakeServer) injectRaw(raw []byte) {
	select {
	case s.inbound <- Inbound{Payload: raw, From: netip.MustParseAddr("192.168.99.1")}:
	case <-s.closed:
	}
}

// injectErr pushes a transport error.
func (s *fakeServer) injectErr(err error) {
	select {
	case s.inbound <- Inbound{Err: err}:
	case <-s.closed:
	}
}

// -------------------------------------------------------- message builders --

const (
	testYIAddr   = "192.168.99.50"
	testServerID = "192.168.99.1"
)

func addr4(s string) []byte {
	a := netip.MustParseAddr(s).As4()
	return a[:]
}

func u32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

func offerFor(req *wire.Message) *wire.Message {
	return &wire.Message{
		Op: wire.BootReply, HType: wire.HTypeEthernet, XID: req.XID,
		YIAddr: netip.MustParseAddr(testYIAddr),
		CHAddr: append([]byte(nil), req.CHAddr...),
		Options: wire.Options{
			wire.OptMessageType: {byte(wire.MsgOffer)},
			wire.OptServerID:    addr4(testServerID),
			wire.OptSubnetMask:  {255, 255, 255, 0},
			wire.OptRouter:      addr4(testServerID),
			wire.OptDNSServer:   addr4(testServerID),
			wire.OptLeaseTime:   u32(3600),
		},
	}
}

func ackFor(req *wire.Message, leaseSecs uint32) *wire.Message {
	m := offerFor(req)
	m.Options[wire.OptMessageType] = []byte{byte(wire.MsgAck)}
	m.Options[wire.OptLeaseTime] = u32(leaseSecs)
	m.Options[wire.OptDomainName] = []byte("example.test")
	m.Options[wire.OptInterfaceMTU] = []byte{0x05, 0xDC}
	return m
}

func nakFor(req *wire.Message) *wire.Message {
	return &wire.Message{
		Op: wire.BootReply, HType: wire.HTypeEthernet, XID: req.XID,
		CHAddr: append([]byte(nil), req.CHAddr...),
		Options: wire.Options{
			wire.OptMessageType: {byte(wire.MsgNak)},
			wire.OptServerID:    addr4(testServerID),
			wire.OptMessage:     []byte("no leases available"),
		},
	}
}

// answerTheRequestedAddress is a server that HAS the client's record: it ACKs
// whatever option 50 names, which is what RFC 2131 section 4.3.2 says a server
// with a matching binding does with an INIT-REBOOT DHCPREQUEST.
//
// Separate from answerNormally, which always answers with testYIAddr, because
// a fixture that hands back one fixed address cannot tell "the client asked for
// this" from "the fixture always says this".
func answerTheRequestedAddress(req *wire.Message, _ int) []*wire.Message {
	t, ok := req.Type()
	if !ok {
		return nil
	}
	want, asked := req.Addr4(wire.OptRequestedIP)
	switch t {
	case wire.MsgDiscover:
		// It answers a DHCPDISCOVER as well, so that a test whose client
		// falls back to INIT still completes. A fixture that answered only
		// DHCPREQUESTs would turn every such case into a hang, which is a
		// whole-package timeout with no failing assertion.
		m := offerFor(req)
		if asked {
			m.YIAddr = want
		}
		return []*wire.Message{m}
	case wire.MsgRequest:
		m := ackFor(req, 3600)
		if asked {
			m.YIAddr = want
		}
		return []*wire.Message{m}
	}
	return nil
}

// answerNormally is the ordinary server: OFFER to a DISCOVER, ACK to a REQUEST.
func answerNormally(req *wire.Message, _ int) []*wire.Message {
	t, ok := req.Type()
	if !ok {
		return nil
	}
	switch t {
	case wire.MsgDiscover:
		return []*wire.Message{offerFor(req)}
	case wire.MsgRequest:
		return []*wire.Message{ackFor(req, 3600)}
	}
	return nil
}

// renewalChangesThenNak is the one server that drives every outward event
// kind: it acquires, answers the first renewal with a lease whose router has
// moved (Renewed AND Changed), refuses the second (Lost AND Failed), and then
// says nothing.
//
// The renewals are told apart by counting RENEWALS, not messages: a renewal is
// the DHCPREQUEST that names the address it already holds, and keying on the
// client's message ordinal would keep passing while quietly answering a
// different message than the one this fixture means.
func renewalChangesThenNak() serverBehaviour {
	renewals := 0
	return func(req *wire.Message, n int) []*wire.Message {
		t, ok := req.Type()
		if !ok {
			return nil
		}
		renewal := t == wire.MsgRequest && req.CIAddr.IsValid() && !req.CIAddr.IsUnspecified()
		switch {
		case t == wire.MsgDiscover && n == 1:
			return []*wire.Message{offerFor(req)}
		case renewal:
			renewals++
			if renewals > 1 {
				return []*wire.Message{nakFor(req)}
			}
			m := ackFor(req, 3600)
			// The router moves, so the renewed lease is not the one the
			// caller had and a Changed rides beside the Renewed.
			m.Options[wire.OptRouter] = addr4("192.168.99.2")
			return []*wire.Message{m}
		case t == wire.MsgRequest:
			return []*wire.Message{ackFor(req, 3600)}
		}
		return nil
	}
}

// answerTheAcquisitionThenGoSilent hands out one lease and then says nothing:
// no answer to the renewal, no answer to a second acquisition.
//
// It is what holds a test in RENEWING. A server that answered the renewal
// would put the machine back in BOUND while the test was still injecting, and
// BOUND discards every inbound message before the xid is ever looked at — so a
// test that means to exercise the xid guard has to keep a transaction open.
func answerTheAcquisitionThenGoSilent(req *wire.Message, n int) []*wire.Message {
	t, ok := req.Type()
	if !ok {
		return nil
	}
	switch {
	case t == wire.MsgDiscover && n == 1:
		return []*wire.Message{offerFor(req)}
	case t == wire.MsgRequest && req.CIAddr.IsValid() && !req.CIAddr.IsUnspecified():
		return nil
	case t == wire.MsgRequest:
		return []*wire.Message{ackFor(req, 3600)}
	}
	return nil
}

// nakTheRenewalThenGoSilent answers the acquisition, refuses the renewal, and
// then says nothing at all.
//
// The silence is the point. A server that kept answering would let the client
// finish its post-NAK re-acquisition while the test was still asking what the
// DHCPNAK cost it, and the answer would depend on which goroutine won — which
// is exactly the race review round 3 found in the netns test. With no answer
// coming, "the manager holds nothing" is a question with one answer.
//
// The renewal is recognised by its ciaddr, not by a message count: a renewal
// is the DHCPREQUEST that names the address it already holds (RFC 2131
// section 4.3.2), and keying on the count would keep passing if the client
// started sending a different number of messages first.
func nakTheRenewalThenGoSilent(req *wire.Message, n int) []*wire.Message {
	t, ok := req.Type()
	if !ok {
		return nil
	}
	switch {
	case t == wire.MsgDiscover && n == 1:
		return []*wire.Message{offerFor(req)}
	case t == wire.MsgRequest && req.CIAddr.IsValid() && !req.CIAddr.IsUnspecified():
		return []*wire.Message{nakFor(req)}
	case t == wire.MsgRequest:
		return []*wire.Message{ackFor(req, 3600)}
	}
	return nil
}

// The rig and the recorders live here, beside the fakes they drive, so that
// switching off a test file never takes the fixtures its neighbours are built
// on with it.
var testCHAddr = []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x02}

func testParams() proto.Params {
	p := proto.DefaultParams(testCHAddr)
	// Desync off: the delay is ring 1's and is tested there. Leaving it on
	// would make every manager test start by firing a timer that has nothing
	// to do with what it asserts.
	p.DesyncMin, p.DesyncMax = 0, 0
	return p
}

// journalRecorder is a Journal that keeps everything, so a test can replay it.
//
// Locked, because the manager appends from its own goroutine while the test
// reads. An unlocked recorder is a data race, and -race reports it against
// whichever test happens to be running.
type journalRecorder struct {
	mu       sync.Mutex
	entries  []proto.JournalEntry
	appended chan proto.JournalEntry
}

func newJournalRecorder() *journalRecorder {
	return &journalRecorder{appended: make(chan proto.JournalEntry, 64)}
}

func (j *journalRecorder) Append(e proto.JournalEntry) {
	j.mu.Lock()
	j.entries = append(j.entries, e)
	j.mu.Unlock()
	select {
	case j.appended <- e:
	default:
	}
}

// waitAppended blocks until a journalled Step satisfies want.
//
// It is a DIFFERENT barrier from packetRecorder.waitRecorded, and the
// difference is the whole reason it exists: a packet is recorded BEFORE it is
// fed to ring 1 (manager.go, onInbound), so waiting on the packet ring proves
// only that the packet arrived. A journal entry is appended AFTER Step and
// after the previous event's actions have drained, so it is the only barrier
// in this package that proves the machine has SEEN something.
func (j *journalRecorder) waitAppended(t *testing.T, what string, want func(proto.JournalEntry) bool) {
	t.Helper()
	for e := range j.appended {
		if want(e) {
			return
		}
	}
	t.Fatalf("the journal closed before %s was recorded", what)
}

func (j *journalRecorder) Entries() []proto.JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]proto.JournalEntry(nil), j.entries...)
}

// packetRecorder is a PacketRing that also announces what it recorded.
//
// The announcement is the barrier the inbound tests need, and it is not a
// convenience. The manager selects over TWO channels — inbound packets and
// timer fires — so "push a packet, then fire a timer" does not order the two:
// the manager may take the timer first. A test that ordered them that way
// passes alone and fails in a suite, which is exactly how this one was found.
type packetRecorder struct {
	mu       sync.Mutex
	packets  []CapturedPacket
	recorded chan CapturedPacket
}

func newPacketRecorder() *packetRecorder {
	return &packetRecorder{recorded: make(chan CapturedPacket, 64)}
}

func (p *packetRecorder) Record(c CapturedPacket) {
	p.mu.Lock()
	p.packets = append(p.packets, c)
	p.mu.Unlock()
	select {
	case p.recorded <- c:
	default:
	}
}

func (p *packetRecorder) Packets() []CapturedPacket {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]CapturedPacket(nil), p.packets...)
}

// waitRecorded blocks until a captured packet satisfies want.
func (p *packetRecorder) waitRecorded(t *testing.T, what string, want func(CapturedPacket) bool) {
	t.Helper()
	for c := range p.recorded {
		if want(c) {
			return
		}
	}
	t.Fatalf("the packet ring closed before %s was recorded", what)
}

type rig struct {
	mgr     *Manager
	server  *fakeServer
	fault   *FaultTransport
	timers  *fakeTimers
	clock   *fakeClock
	journal *journalRecorder
	packets *packetRecorder
	cancel  context.CancelFunc
	done    chan error

	// Run's result is read exactly once, through wait, and cached. A test
	// that stops the manager itself AND a Cleanup that stops it again is the
	// ordinary case, and a second receive on a one-shot channel is a deadlock
	// that presents as a whole-package timeout with no failing assertion.
	waitOnce sync.Once
	runErr   error
}

// wait returns Run's result, reading it from the channel at most once.
func (r *rig) wait() error {
	r.waitOnce.Do(func() { r.runErr = <-r.done })
	return r.runErr
}

// stop cancels the context and waits for Run to return.
func (r *rig) stop() error {
	r.cancel()
	return r.wait()
}

// rigOption sets a Config field that is not Params. Variadic so that every
// existing call site stays the three-argument one it was: an option nobody
// passes must not change what the other tests build.
type rigOption func(*Config)

// withResume supplies the remembered lease that turns the first message into
// an INIT-REBOOT DHCPREQUEST.
func withResume(l Lease) rigOption { return func(c *Config) { c.Resume = &l } }

// newRig assembles a manager over the fakes and starts Run.
func newRig(t *testing.T, p proto.Params, behaviour serverBehaviour, plan Fault, opts ...rigOption) *rig {
	t.Helper()
	return newRigOn(t, newFakeClock(), p, behaviour, plan, opts...)
}

// newRigOn is newRig with the clock supplied, for the tests that need the two
// clocks moved apart BEFORE the manager is built — the wall-to-monotonic
// conversion happens once, at construction, so a test that advances the clock
// afterwards measures nothing about it.
func newRigOn(t *testing.T, clk *fakeClock, p proto.Params, behaviour serverBehaviour, plan Fault, opts ...rigOption) *rig {
	t.Helper()

	srv := newFakeServer(behaviour)
	// The fault transport is ALWAYS in the path, even with an empty plan.
	// R2 says the failure path is not a special mode, and a wrapper only
	// present in the fault tests is a wrapper the happy-path tests never
	// exercise.
	ft := NewFaultTransport(srv, plan)

	r := &rig{
		server:  srv,
		fault:   ft,
		timers:  newFakeTimers(),
		clock:   clk,
		journal: newJournalRecorder(),
		packets: newPacketRecorder(),
		done:    make(chan error, 1),
	}

	cfg := Config{
		Params:      p,
		Transport:   ft,
		Clock:       r.clock,
		Timers:      r.timers,
		Entropy:     &fakeEntropy{},
		Journal:     r.journal,
		Packets:     r.packets,
		EventBuffer: 16,
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

	t.Cleanup(func() {
		_ = r.stop()
		_ = ft.Close()
		_ = r.timers.Close()
	})
	return r
}

// nextEvent reads one outward event. It blocks on the channel, which is the
// barrier the whole suite is built on — no duration appears anywhere.
func (r *rig) nextEvent(t *testing.T) Event {
	t.Helper()
	e, ok := <-r.mgr.Events()
	if !ok {
		t.Fatal("the event channel closed before the expected event arrived")
	}
	return e
}
