package lease

import (
	"errors"
	"net/netip"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// The manager's half of RFC 5227: the ARP port, the counters, the record
// field, and the three modes as a caller sees them.

// fakeARP is the ARP port over two channels.
//
// It records what was SENT as decoded packets, because that is what every
// assertion here is about — a test that kept raw frames would be asserting on
// wire.EncodeARP, which wire/arp_test.go already covers at the offsets.
type fakeARP struct {
	mu     sync.Mutex
	sent   []*wire.ARPPacket
	raw    [][]byte
	err    error
	closed bool

	in   chan ARPInbound
	done chan struct{}
}

func newFakeARP() *fakeARP {
	return &fakeARP{in: make(chan ARPInbound, 32), done: make(chan struct{})}
}

func (a *fakeARP) Send(frame []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	p, err := wire.DecodeARP(frame)
	if err != nil {
		// The manager encoded something this library cannot read back, which
		// is a bug worth failing on rather than counting.
		return err
	}
	a.sent = append(a.sent, p)
	a.raw = append(a.raw, append([]byte(nil), frame...))
	return nil
}

func (a *fakeARP) Received() <-chan ARPInbound { return a.in }

func (a *fakeARP) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		close(a.done)
	}
	return nil
}

// deliver feeds one frame, or returns once the port is closed.
func (a *fakeARP) deliver(p *wire.ARPPacket) {
	raw, err := wire.EncodeARP(p)
	if err != nil {
		panic("lease: test fixture built an unencodable ARP packet: " + err.Error())
	}
	select {
	case a.in <- ARPInbound{Frame: raw}:
	case <-a.done:
	}
}

// deliverRaw feeds bytes that need not be a valid ARP packet.
func (a *fakeARP) deliverRaw(b []byte) {
	select {
	case a.in <- ARPInbound{Frame: b}:
	case <-a.done:
	}
}

func (a *fakeARP) sentPackets() []*wire.ARPPacket {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*wire.ARPPacket(nil), a.sent...)
}

func (a *fakeARP) fail(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}

// withARP puts an ARP port in the Config.
func withARP(a *fakeARP) rigOption { return func(c *Config) { c.ARP = a } }

// acdTestParams is testParams with conflict detection ON, and RFC 5227 section
// 1.1's constants scaled to nanoseconds.
//
// The scale is the only thing that changes: the counts, the order and the
// ratios are the RFC's. The real durations are pinned once, in
// proto.TestACDConstantsAreTheRFCValues, and measured once on a real wire by
// the netns run.
func acdTestParams(mode proto.ConflictMode) proto.Params {
	p := testParams()
	p.Conflict = mode
	p.ACD = proto.ACDParams{
		ProbeWait:         3 * proto.Nanosecond,
		ProbeNum:          3,
		ProbeMin:          4 * proto.Nanosecond,
		ProbeMax:          5 * proto.Nanosecond,
		AnnounceWait:      6 * proto.Nanosecond,
		AnnounceNum:       2,
		AnnounceInterval:  7 * proto.Nanosecond,
		MaxConflicts:      10,
		RateLimitInterval: 600 * proto.Nanosecond,
		DefendInterval:    8 * proto.Nanosecond,
	}
	return p
}

const acdAddr = "192.168.99.50"

var squatterMAC = []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x63}

func arpReply(hw []byte, sender, target string) *wire.ARPPacket {
	return &wire.ARPPacket{
		Op:       wire.ARPReply,
		SenderHW: hw,
		SenderIP: netip.MustParseAddr(sender),
		TargetIP: netip.MustParseAddr(target),
	}
}

// runACD fires TimerACD until the manager stops arming it, so the schedule is
// driven by the machine's own requests rather than by a count written here.
//
// THE BARRIER IS THE JOURNAL, not the armed channel. The last step of the
// schedule — RFC 5227 section 2.3's second Announcement — sends its packet and
// CANCELS the timer; it arms nothing. A helper that waited for an arm after
// every fire would wait two seconds for a barrier that, by design, never
// comes. Every dispatch appends exactly one journal entry whatever it decides,
// including the one that decides to send nothing, so the entry count is the
// one signal that is present on every step.
//
// The spin carries no deadline of its own, which is gate T2's rule and not a
// stylistic preference: a deadline written here is a wall-clock wait, and the
// way a wall-clock wait fails is that somebody lengthens it. An entry that
// never arrives hangs until go test's own -timeout, which names the test and
// prints every goroutine — strictly more than a Fatalf chosen here would say.
func runACD(t *testing.T, r *rig) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if _, armed := r.timers.armedAt(proto.TimerACD); !armed {
			return
		}
		before := len(r.journal.Entries())
		r.timers.fire(proto.TimerACD)
		for len(r.journal.Entries()) == before {
			goruntime.Gosched()
		}
	}
	t.Fatal("the ACD schedule did not terminate in 20 timer fires")
}

// TestNewManagerRefusesConflictDetectionWithNoARPPort is defeat row M6-27.
//
// An error and not a downgrade to ConflictOff. A downgraded client acquires,
// binds and looks healthy; the one thing it does not do is the thing it was
// configured to do, and there is nothing to read that says so.
func TestNewManagerRefusesConflictDetectionWithNoARPPort(t *testing.T) {
	base := func() Config {
		return Config{
			Transport: NewFaultTransport(newFakeServer(answerNormally), Fault{}),
			Clock:     newFakeClock(),
			Timers:    newFakeTimers(),
			Entropy:   &fakeEntropy{},
		}
	}
	for _, mode := range proto.AllConflictModes() {
		cfg := base()
		cfg.Params = acdTestParams(mode)
		_, err := NewManager(cfg)
		if mode == proto.ConflictOff {
			if err != nil {
				t.Errorf("%s with no ARP port: %v, want no error", mode, err)
			}
			continue
		}
		if !errors.Is(err, ErrNoARP) {
			t.Errorf("%s with no ARP port: %v, want ErrNoARP", mode, err)
		}
	}
	// The preservation control: with a port, every mode builds.
	for _, mode := range proto.AllConflictModes() {
		cfg := base()
		cfg.Params = acdTestParams(mode)
		cfg.ARP = newFakeARP()
		if _, err := NewManager(cfg); err != nil {
			t.Errorf("%s with an ARP port: %v", mode, err)
		}
	}
}

// TestTheProbesGoOutOnTheARPPort reads the packets the manager actually
// handed to the socket.
//
// OUTSIDE EVIDENCE, in the sense this project means it: not the manager's own
// counters but the objects a socket implementation would have written. The
// all-zero sender IP is the field the whole milestone turns on — it is what
// makes the frame an ARP Probe rather than 1.x's cache-polluting datagram
// (design section 8.4).
func TestTheProbesGoOutOnTheARPPort(t *testing.T) {
	arp := newFakeARP()
	r := newRig(t, acdTestParams(proto.ConflictWait), answerNormally, Fault{}, withARP(arp))

	waitForTimer(t, r, proto.TimerACD)
	runACD(t, r)

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event is %s, want Acquired", ev.Kind)
	}

	sent := arp.sentPackets()
	var probes, announcements int
	for _, p := range sent {
		if p.IsProbe() {
			probes++
			if !p.SenderIP.IsUnspecified() {
				t.Errorf("a probe carries sender IP %s, want RFC 5227 2.1.1's all-zero", p.SenderIP)
			}
			if got := p.TargetIP.String(); got != acdAddr {
				t.Errorf("a probe targets %s, want the offered address %s", got, acdAddr)
			}
		} else {
			announcements++
			if got := p.SenderIP.String(); got != acdAddr {
				t.Errorf("an announcement carries sender IP %s, want %s", got, acdAddr)
			}
			if p.SenderIP != p.TargetIP {
				t.Errorf("an announcement has spa %s and tpa %s; RFC 5227 2.3 sets both to the new address", p.SenderIP, p.TargetIP)
			}
		}
		if string(p.SenderHW) != string(testCHAddr) {
			t.Errorf("a packet carries sender hardware address %x, want this client's %x", p.SenderHW, testCHAddr)
		}
	}
	if probes != 3 || announcements != 2 {
		t.Fatalf("%d probes and %d announcements, want 3 and 2", probes, announcements)
	}

	s := r.mgr.Stats()
	if s.ProbesSent != 3 || s.AnnouncementsSent != 2 {
		t.Errorf("Stats has %d probes and %d announcements, want 3 and 2", s.ProbesSent, s.AnnouncementsSent)
	}

	// And they are in the packet ring, which is what a caller dumps when it
	// wants to know what happened (G1).
	var ringed int
	for _, c := range r.mgr.Packets() {
		if c.ARP != nil && c.Dir == DirOut {
			ringed++
		}
	}
	if ringed != 5 {
		t.Errorf("the packet ring holds %d outbound ARP packets, want 5", ringed)
	}
}

// TestTheModesDifferInWhenTheCallerIsToldItHasAnAddress is Amendment 1 as the
// caller sees it, and is the ring-2 half of the ring-1 test of the same shape.
//
// It reads the ACD PHASE ON THE EVENT, which is the field D23 adds and the one
// a restarting chassis depends on.
func TestTheModesDifferInWhenTheCallerIsToldItHasAnAddress(t *testing.T) {
	t.Run("wait", func(t *testing.T) {
		arp := newFakeARP()
		r := newRig(t, acdTestParams(proto.ConflictWait), answerNormally, Fault{}, withARP(arp))
		waitForTimer(t, r, proto.TimerACD)
		if n := len(arp.sentPackets()); n != 0 {
			t.Fatalf("%d ARP packets before the timer ran; the initial delay is missing", n)
		}
		if got := r.mgr.Stats().LeasesAcquired; got != 0 {
			t.Fatalf("the lease was announced %d time(s) before probing; D22 says the address is not usable yet", got)
		}
		runACD(t, r)
		ev := r.nextEvent(t)
		if ev.Kind != Acquired {
			t.Fatalf("event is %s, want Acquired", ev.Kind)
		}
		if ev.ACD != proto.ACDAnnouncing {
			t.Errorf("Acquired carries ACD phase %s, want announcing: in wait the check has passed by the time the caller is told", ev.ACD)
		}
	})

	t.Run("async", func(t *testing.T) {
		arp := newFakeARP()
		r := newRig(t, acdTestParams(proto.ConflictAsync), answerNormally, Fault{}, withARP(arp))
		ev := r.nextEvent(t)
		if ev.Kind != Acquired {
			t.Fatalf("event is %s, want Acquired", ev.Kind)
		}
		// The whole content of async: the caller has the address and the
		// check has not run.
		if ev.ACD != proto.ACDProbing {
			t.Errorf("Acquired carries ACD phase %s, want probing: async announces first and probes beside use", ev.ACD)
		}
		if n := len(arp.sentPackets()); n != 0 {
			t.Errorf("%d probes had already gone out when the caller was told; async announces before it probes", n)
		}
		runACD(t, r)
		if n := len(arp.sentPackets()); n != 5 {
			t.Errorf("%d ARP packets over the schedule, want 5: async still runs the whole check", n)
		}
	})

	t.Run("off", func(t *testing.T) {
		r := newRig(t, acdTestParams(proto.ConflictOff), answerNormally, Fault{})
		ev := r.nextEvent(t)
		if ev.Kind != Acquired {
			t.Fatalf("event is %s, want Acquired", ev.Kind)
		}
		if ev.ACD != proto.ACDIdle {
			t.Errorf("Acquired carries ACD phase %s, want idle: off runs no check", ev.ACD)
		}
		if _, armed := r.timers.armedAt(proto.TimerACD); armed {
			t.Error("an ACD timer is armed on a client with conflict detection off")
		}
		if got := r.mgr.Stats().ProbesSent; got != 0 {
			t.Errorf("%d probes on a client with conflict detection off", got)
		}
	})
}

// TestAConflictInTheProbeWindowDeclinesAndCountsOnce drives the squatter
// through the ARP port and reads the DHCPDECLINE off the transport.
func TestAConflictInTheProbeWindowDeclinesAndCountsOnce(t *testing.T) {
	arp := newFakeARP()
	r := newRig(t, acdTestParams(proto.ConflictWait), answerNormally, Fault{}, withARP(arp))
	waitForTimer(t, r, proto.TimerACD)
	// One probe out.
	r.timers.fire(proto.TimerACD)
	<-r.timers.armed

	arp.deliver(arpReply(squatterMAC, acdAddr, "192.168.99.9"))

	ev := r.nextEvent(t)
	if ev.Kind != Failed {
		t.Fatalf("event is %s, want Failed: nothing was acquired, so there is no lease to lose", ev.Kind)
	}
	if ev.Reason != proto.ReasonConflict {
		t.Fatalf("Failed carries %s, want conflict", ev.Reason)
	}

	msg := lastSent(t, r, wire.MsgDecline)
	got, ok := msg.Addr4(wire.OptRequestedIP)
	if !ok || got.String() != acdAddr {
		t.Errorf("the DHCPDECLINE names %s (present %v), want %s", got, ok, acdAddr)
	}

	s := r.mgr.Stats()
	if s.ConflictsDetected != 1 {
		t.Errorf("ConflictsDetected = %d, want 1", s.ConflictsDetected)
	}
	if s.LeasesLost != 0 {
		t.Errorf("LeasesLost = %d, want 0: nothing was ever acquired", s.LeasesLost)
	}
	if s.DeclinesSent != 1 {
		t.Errorf("DeclinesSent = %d, want 1", s.DeclinesSent)
	}
}

// TestAConflictAfterAcquisitionIsLostWithReasonConflict is seam row G-5, in
// both modes that have a listener.
func TestAConflictAfterAcquisitionIsLostWithReasonConflict(t *testing.T) {
	for _, mode := range []proto.ConflictMode{proto.ConflictWait, proto.ConflictAsync} {
		t.Run(mode.String(), func(t *testing.T) {
			arp := newFakeARP()
			r := newRig(t, acdTestParams(mode), answerNormally, Fault{}, withARP(arp))
			waitForTimer(t, r, proto.TimerACD)
			runACD(t, r)

			ev := r.nextEvent(t)
			if ev.Kind != Acquired {
				t.Fatalf("event is %s, want Acquired", ev.Kind)
			}
			if got := r.mgr.ACDPhase(); got != proto.ACDDefending {
				t.Fatalf("after the schedule the phase is %s, want defending", got)
			}

			// The squatter turns up afterwards: RFC 5227 section 2.4.
			arp.deliver(arpReply(squatterMAC, acdAddr, "192.168.99.9"))

			ev = r.nextEvent(t)
			if ev.Kind != Lost || ev.Reason != proto.ReasonConflict {
				t.Fatalf("event is %s/%s, want Lost/conflict (seam row G-5)", ev.Kind, ev.Reason)
			}
			lastSent(t, r, wire.MsgDecline)
			if got := r.mgr.Stats().ConflictsDetected; got != 1 {
				t.Errorf("ConflictsDetected = %d, want 1", got)
			}
		})
	}
}

// TestOrdinaryARPTrafficNeverReachesTheMachine is defeat row M6-13's cost side.
//
// Every event that reaches Step costs a journal entry, and the journal is
// bounded (R3). A client on a link with ordinary ARP on it must not wrap its
// own journal between one acquisition and the next.
func TestOrdinaryARPTrafficNeverReachesTheMachine(t *testing.T) {
	arp := newFakeARP()
	r := newRig(t, acdTestParams(proto.ConflictWait), answerNormally, Fault{}, withARP(arp))
	waitForTimer(t, r, proto.TimerACD)
	runACD(t, r)
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("event is %s, want Acquired", ev.Kind)
	}
	before := r.mgr.Stats().Steps

	const noise = 50
	for i := 0; i < noise; i++ {
		arp.deliver(arpReply(squatterMAC, "192.168.99.200", "192.168.99.201"))
	}
	// A frame that is not an ARP packet at all, and one that is short.
	arp.deliverRaw([]byte{1, 2, 3})
	arp.deliverRaw(make([]byte, 28)) // htype 0: not Ethernet

	// Drain: send one frame that IS relevant but is not a conflict — our own
	// announcement echoed back — and wait for the counters to settle on it.
	for {
		s := r.mgr.Stats()
		if s.ARPSeen >= noise+2 {
			if s.ARPIgnored != noise {
				t.Errorf("ARPIgnored = %d, want %d: the relevance filter let ordinary link traffic through", s.ARPIgnored, noise)
			}
			if s.ARPDecodeFailures != 2 {
				t.Errorf("ARPDecodeFailures = %d, want 2", s.ARPDecodeFailures)
			}
			if s.Steps != before {
				t.Errorf("the machine stepped %d time(s) for %d ignored frames; the bounded journal would wrap on ordinary link traffic", s.Steps-before, noise)
			}
			return
		}
		goruntime.Gosched()
	}
}

// TestReportConflictStillWorksWithDetectionOff is defeat row M6-23.
func TestReportConflictStillWorksWithDetectionOff(t *testing.T) {
	r := newRig(t, acdTestParams(proto.ConflictOff), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("event is %s, want Acquired", ev.Kind)
	}
	r.mgr.ReportConflict()
	ev := r.nextEvent(t)
	if ev.Kind != Lost || ev.Reason != proto.ReasonConflict {
		t.Fatalf("event is %s/%s, want Lost/conflict", ev.Kind, ev.Reason)
	}
	lastSent(t, r, wire.MsgDecline)
	if got := r.mgr.Stats().ConflictsDetected; got != 1 {
		t.Errorf("ConflictsDetected = %d, want 1: a caller's own evidence is still a conflict", got)
	}
}

// TestAFailedProbeSendDoesNotStopTheAcquisition.
//
// An ARP send that fails is counted and journalled and costs nothing else. The
// alternative — folding it into MaxSendFailures — would let five unsendable
// packets end an acquisition the DHCP server had already answered.
func TestAFailedProbeSendDoesNotStopTheAcquisition(t *testing.T) {
	arp := newFakeARP()
	arp.fail(errors.New("no such device"))
	r := newRig(t, acdTestParams(proto.ConflictWait), answerNormally, Fault{}, withARP(arp))
	waitForTimer(t, r, proto.TimerACD)
	runACD(t, r)

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("event is %s, want Acquired: a client that cannot send probes still has a lease the server granted", ev.Kind)
	}
	s := r.mgr.Stats()
	if s.ARPSendFailures != 5 {
		t.Errorf("ARPSendFailures = %d, want 5", s.ARPSendFailures)
	}
	if s.ProbesSent != 0 || s.AnnouncementsSent != 0 {
		t.Errorf("ProbesSent = %d and AnnouncementsSent = %d, want 0: neither left the host", s.ProbesSent, s.AnnouncementsSent)
	}
	if s.SendFailures != 0 {
		t.Errorf("SendFailures = %d, want 0: an ARP failure is not a DHCP transport failure", s.SendFailures)
	}
	// And the journal says the check did not happen, which is the only place
	// that fact exists.
	findEntry(t, r, "an incomplete conflict check", func(e proto.JournalEntry) bool {
		for _, a := range e.Actions {
			if containsSub(a, "conflict check is incomplete") {
				return true
			}
		}
		return false
	})
}

// TestTheRecordCarriesTheACDPhaseAcrossARestart is defeat row M6-24.
//
// A chassis persists the event stream and rebuilds the record after a restart.
// In async the lease is announced while the check is still running, so a
// record that did not carry the phase would tell a resuming process the
// address had been cleared when it had not.
func TestTheRecordCarriesTheACDPhaseAcrossARestart(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want proto.ACDPhase
	}{
		{"a lease announced while the check is still running", Event{Kind: Acquired, ACD: proto.ACDProbing}, proto.ACDProbing},
		{"a lease whose check has completed", Event{Kind: Acquired, ACD: proto.ACDDefending}, proto.ACDDefending},
		{"a client that runs no check at all", Event{Kind: Acquired, ACD: proto.ACDIdle}, proto.ACDIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := Lease{Addr: netip.MustParsePrefix(acdAddr + "/24")}
			ev := c.ev
			ev.Lease = l
			evs := []RecordEvent{
				{ID: "e1", Op: OpCreate, Seq: 1, At: time.Unix(1, 0), Instance: "p1",
					Scope: "s", Family: FamilyV4, CHAddr: testCHAddr, Identity: []byte{1}},
				EventRecord("e1", "p1", 2, time.Unix(2, 0), ev),
			}
			rb := Rebuild(evs)
			rec, ok := rb.ByID("e1")
			if !ok {
				t.Fatalf("the record did not rebuild: %v", rb.Rejects)
			}
			if rec.ACD != c.want {
				t.Fatalf("the rebuilt record says ACD %s, want %s", rec.ACD, c.want)
			}
		})
	}
}

// containsSub is strings.Contains, spelled here so the assertion above reads
// as one line.
func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// waitForTimer blocks until id has been armed.
func waitForTimer(t *testing.T, r *rig, id proto.TimerID) {
	t.Helper()
	for {
		if _, armed := r.timers.armedAt(id); armed {
			return
		}
		goruntime.Gosched()
	}
}
