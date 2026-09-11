// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// TestRenewalWatch_AnAnsweredRenewalNeverMovesTheCounter is the
// opposite failure, and it is the one that decides whether this counter
// is worth having.
//
// The tempting implementation is RenewalsSent, or Sent minus Completed,
// and both move the moment a renewal leaves the host. Every healthy
// renewal is a request in flight for as long as the server takes to
// answer, so a counter of that shape reports an outage on a network
// that is working perfectly and an operator alerting on it learns to
// ignore it. A counter that moves on the send rather than on the
// silence is the wrong counter.
func TestRenewalWatch_AnAnsweredRenewalNeverMovesTheCounter(t *testing.T) {
	var w renewalWatch

	// Three renewals, each answered before the next: the client is
	// being served and nothing here is a fault. The answer arrives the
	// way the wire delivers it, as the lease event the chassis is
	// handed, rather than as a hand-built snapshot in the shape the
	// counter would like to see.
	for i := uint64(1); i <= 3; i++ {
		if got := w.fold(lease.Stats{RenewalsSent: i, RenewalsCompleted: i - 1}); got != 0 {
			t.Fatalf("renewal %d in flight reported %d unanswered request(s); a request that is "+
				"waiting for an answer has not been refused one", i, got)
		}
		acked := lease.Stats{RenewalsSent: i, RenewalsCompleted: i}
		if got := w.fold(acked); got != 0 {
			t.Fatalf("renewal %d answered reported %d unanswered request(s); the server answered "+
				"this one", i, got)
		}
		w.cycleEnded(acked)
	}
}

// TestRenewalWatch_AnEndingThatNeverCompletesIsStillAnEnding is the
// opposite failure in its most expensive form: a WARN line naming an
// endpoint on a network where the server answered every request.
//
// A DHCPNAK in RENEWING or REBINDING drops the lease with ReasonNak
// (proto/machine.go:603-617) and never reaches enterBound, which is the
// only producer of ActLeaseRenewed and therefore the only thing that
// bumps RenewalsCompleted (lease/manager.go:1065); its v6 twin is a
// Reply carrying NotOnLink, which ends the lease the same way
// (proto/machine6.go:1283-1286). The request was already counted sent
// (lease/manager.go:1364), so RenewalsSent stays permanently one ahead
// of RenewalsCompleted for the life of that manager, and
// Sent-minus-Completed-minus-one claims one unanswered request at the
// instant the NEXT renewal leaves the host, while it is still in
// flight. Once for every NAK the client ever takes, and the running
// maximum never gives it back.
//
// Both endings reach the chassis as a Lost event
// (lease/manager.go:1092), which is where the cycle's end is read from.
func TestRenewalWatch_AnEndingThatNeverCompletesIsStillAnEnding(t *testing.T) {
	var w renewalWatch
	var total uint64

	// The renewal at T1, answered by a DHCPNAK: the lease is dropped,
	// and RenewalsCompleted does not move, now or ever.
	naked := lease.Stats{RenewalsSent: 1, NaksSeen: 1, NaksAccepted: 1, LeasesLost: 1}
	total += w.fold(naked)
	w.cycleEnded(naked)

	// The client re-acquires. An acquisition's DHCPREQUEST carries a
	// zero ciaddr and is not a renewal, so nothing here moves
	// RenewalsSent.
	reacquired := naked
	reacquired.LeasesAcquired = 2
	total += w.fold(reacquired)
	w.cycleEnded(reacquired)

	// The next renewal, in flight: Sent is 2 and Completed is still 0.
	inFlight := reacquired
	inFlight.RenewalsSent = 2
	total += w.fold(inFlight)
	if total != 0 {
		t.Fatalf("a renewal answered by a DHCPNAK, then one still in flight, reported %d unanswered "+
			"request(s). The NAK ANSWERED the first request, and naks_received already carries it; a "+
			"counter reading RenewalsSent minus RenewalsCompleted claims it here because the NAK path "+
			"never bumps RenewalsCompleted, and claims one more for every NAK the client ever takes",
			total)
	}

	// And that one is acknowledged. Still nothing owed.
	acked := inFlight
	acked.RenewalsCompleted = 1
	total += w.fold(acked)
	w.cycleEnded(acked)
	if total != 0 {
		t.Fatalf("after the second renewal was acknowledged the counter reads %d; every request in "+
			"this test was answered", total)
	}
}

// TestRenewalWatch_EveryEventKindEndsTheCycle takes its population from
// the library rather than from a list written here, so a kind added to
// lease.AllEventKinds arrives in this test on the next bump.
//
// The watch does not read the kind, and that is the design: "the caller
// was told something happened to this lease" is the property, and an
// event that did not in fact end a renewal cycle costs at most the
// requests already proven unanswered. That is the direction this
// counter is allowed to err in.
func TestRenewalWatch_EveryEventKindEndsTheCycle(t *testing.T) {
	for _, k := range lease.AllEventKinds() {
		t.Run(k.String(), func(t *testing.T) {
			var w renewalWatch
			var total uint64

			ended := lease.Stats{RenewalsSent: 1}
			total += w.fold(ended)
			w.cycleEnded(ended)

			total += w.fold(lease.Stats{RenewalsSent: 2})
			if total != 0 {
				t.Fatalf("a %s event ended the renewal cycle, and the request that followed it was "+
					"reported unanswered (%d) while it was still in flight", k, total)
			}
		})
	}
}

// TestRenewalWatch_MovesOncePerRetransmission is the counter's own
// contract, driven on the sequence #940 was reported from: four renewal
// requests over 7h52m, the fourth answered.
func TestRenewalWatch_MovesOncePerRetransmission(t *testing.T) {
	var w renewalWatch
	total := uint64(0)

	// The renewal at T1. Nothing is proven yet: this request may still
	// be answered.
	total += w.fold(lease.Stats{RenewalsSent: 1})
	if total != 0 {
		t.Fatalf("the first renewal request alone reported %d unanswered; it is still in flight", total)
	}

	// Each retransmission proves the request before it was never
	// answered, and proves exactly one.
	for send := uint64(2); send <= 4; send++ {
		before := total
		total += w.fold(lease.Stats{RenewalsSent: send})
		if total != before+1 {
			t.Fatalf("retransmission %d moved the counter from %d to %d; a retransmission proves "+
				"exactly one earlier request unanswered", send, before, total)
		}
	}

	// The fourth request is answered. The three before it stay
	// unanswered: an acknowledgement does not retract them.
	total += w.fold(lease.Stats{RenewalsSent: 4, RenewalsCompleted: 1})
	if total != 3 {
		t.Fatalf("after the fourth request was answered the counter reads %d; the first three went "+
			"unanswered and the answer to the fourth does not undo that", total)
	}
}

// TestRenewalWatch_NeverFalls is D-2: the plugin's counter is a
// Prometheus counter, and a decrease is a RESET, which repays the whole
// accumulated value as a rate spike on the next scrape (#730, one
// counter over). Sent-minus-Completed falls at every acknowledgement,
// so the value handed out has to be a running maximum and the reported
// gain can never be negative.
func TestRenewalWatch_NeverFalls(t *testing.T) {
	var w renewalWatch
	var total uint64

	// Two renewal cycles, each with two retransmissions before the ACK.
	// Sent-minus-Completed goes 1,2,3,2 and then 3,4,5,4 across them.
	steps := []struct {
		s    lease.Stats
		ends bool
	}{
		{s: lease.Stats{RenewalsSent: 1}},
		{s: lease.Stats{RenewalsSent: 2}},
		{s: lease.Stats{RenewalsSent: 3}},
		{s: lease.Stats{RenewalsSent: 3, RenewalsCompleted: 1}, ends: true},
		{s: lease.Stats{RenewalsSent: 4, RenewalsCompleted: 1}},
		{s: lease.Stats{RenewalsSent: 5, RenewalsCompleted: 1}},
		{s: lease.Stats{RenewalsSent: 6, RenewalsCompleted: 1}},
		{s: lease.Stats{RenewalsSent: 6, RenewalsCompleted: 2}, ends: true},
	}
	last := uint64(0)
	for i, step := range steps {
		total += w.fold(step.s)
		if step.ends {
			w.cycleEnded(step.s)
		}
		if total < last {
			t.Fatalf("step %d took the total from %d to %d. A Prometheus counter that decreases is "+
				"a RESET, and the next scrape repays the whole accumulated value as a rate spike; "+
				"the reported gain is what has NOT been handed out yet, and the end of a renewal "+
				"cycle may never take anything back", i, last, total)
		}
		last = total
	}
	if total != 4 {
		t.Fatalf("two renewal cycles of three requests each, one answered per cycle, reported %d "+
			"unanswered; want 4", total)
	}

	// What can still go wrong is a fold that hands out the same ground
	// twice, so re-folding a snapshot already seen must report nothing.
	// Taken mid-cycle, where there is ground to hand out.
	mid := lease.Stats{RenewalsSent: 9, RenewalsCompleted: 2}
	if first := w.fold(mid); first != 2 {
		t.Fatalf("three requests into the third cycle reported %d unanswered, want 2", first)
	}
	if again := w.fold(mid); again != 0 {
		t.Fatalf("re-folding the same snapshot reported %d more unanswered request(s); the gain is "+
			"what the caller has NOT been told, and a counter fed twice from one reading is a "+
			"counter that double-counts every fold", again)
	}
}

// TestRenewalWatch_DoesNotCountAcquisition is D-4. The library counts
// RenewalsSent at countSent, on a DHCPREQUEST with a non-zero ciaddr
// only, which RFC 2131 Table 5 gives as the RENEWING and REBINDING
// column; an acquisition's DISCOVER/REQUEST and the INIT-REBOOT REQUEST
// carry a zero ciaddr and are counted elsewhere. This asserts the
// chassis side of that boundary: a manager that is acquiring, however
// hard, moves nothing here.
func TestRenewalWatch_DoesNotCountAcquisition(t *testing.T) {
	var w renewalWatch
	busy := lease.Stats{
		Steps: 40, Sent: 12, Received: 3, TimerFires: 11,
		LeasesAcquired: 1, LeasesLost: 1, AcquireFailures: 2,
		NaksSeen: 2, NaksAccepted: 1, DeclinesSent: 1,
	}
	if got := w.fold(busy); got != 0 {
		t.Fatalf("a manager that sent 12 messages and no renewal reported %d unanswered renewal "+
			"request(s); this counter's population is RenewalsSent and nothing else", got)
	}
}

// fakeLib is a libClient whose counters the test controls. It stands in
// for the library client at the seam the chassis already declares, so
// the translate goroutine can be driven with no socket and no wire.
type fakeLib struct {
	mu       sync.Mutex
	stats    lease.Stats
	src      chan lease.Event
	releases int
}

func (f *fakeLib) Run(ctx context.Context) error { return nil }
func (f *fakeLib) Events() <-chan lease.Event    { return f.src }
func (f *fakeLib) Lease() (lease.Lease, bool)    { return lease.Lease{}, false }

// Release is the seam's fourth method. This fake counts the call and
// moves no counter, which is the shape releaseHeldLease must read as a
// FAILED release: a library that was asked and put nothing on the wire.
func (f *fakeLib) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
}

func (f *fakeLib) Stats() lease.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *fakeLib) send(n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats.RenewalsSent = n
}

// TestTranslate_ARetransmissionIsReportedWithNoLeaseEvent is #940
// itself, at the seam the defect lives at.
//
// THE DEFECT. A renewal request leaves the host from the library's
// retransmission timer and, when it is not answered, produces no lease
// event: the state machine stays in RENEWING and asks again. translate
// folded the library's counters on the event arm only, so for the whole
// of an outage there was nothing to fold on, /Plugin.Health read
// unchanged, and the first counter to move was dhcp_timeouts at the end
// of the lease -- 24 hours later on the lease this was reported from.
//
// So the test delivers NO event at all. It drives the counters the way
// the wire does, from underneath, and asserts the plugin side is told.
// Deleting the ticker from translate leaves every other test in this
// package green and kills this one.
func TestTranslate_ARetransmissionIsReportedWithNoLeaseEvent(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RenewalStats, 8)
	c := &DHCPClient{
		iface:  "test0",
		opts:   DHCPClientOptions{OnRenewalStats: func(s RenewalStats) { reports <- s }},
		events: newEventChan(),
		src:    lib.src,
		runner: lib,
		// Far below RFC 2131 section 4.4.5's one-minute floor on the
		// real schedule: this test places the retransmissions itself
		// and only needs the fold to run between them.
		pollEvery: time.Millisecond,
	}
	go c.translate()

	// The renewal at T1. In flight, so nothing is owed yet.
	lib.send(1)
	select {
	case s := <-reports:
		t.Fatalf("a renewal request still in flight was reported as %d unanswered", s.Unanswered)
	case <-time.After(50 * time.Millisecond):
	}

	// The retransmission. The request before it is now refused, and no
	// lease event has been delivered on this stream at any point.
	lib.send(2)
	select {
	case s := <-reports:
		if s.Unanswered != 1 {
			t.Fatalf("the retransmission reported %d unanswered request(s), want 1", s.Unanswered)
		}
	case <-time.After(wedgeBudget):
		t.Fatalf("no unanswered renewal was reported within %v of a retransmission, with no lease "+
			"event on the stream. That is #940: the fold runs on the event arm only, so an "+
			"outage moves nothing until the lease expires.", wedgeBudget)
	}

	close(lib.src)
}

// TestRenewalPoll_IsDerivedFromTheProtocolFloor holds the tick to the
// thing it has to observe rather than to a number someone liked.
//
// RFC 2131 section 4.4.5 puts a floor of one minute under the wait
// before a renewal is retransmitted, and the library spells it
// proto.RenewRetransmitFloor. A tick at or above that floor can miss
// every retransmission there is, and the counter then reports an
// outage only when something else happens to fold.
func TestRenewalPoll_IsDerivedFromTheProtocolFloor(t *testing.T) {
	floor := time.Duration(proto.RenewRetransmitFloor)
	if renewalPollInterval >= floor {
		t.Fatalf("the fold runs every %v and the shortest gap between two renewal requests is %v; "+
			"a tick that is not shorter than the gap can fall outside every one of them",
			renewalPollInterval, floor)
	}
	if got := (&DHCPClient{}).renewalPoll(); got != renewalPollInterval {
		t.Fatalf("a client with no override folds every %v, want %v", got, renewalPollInterval)
	}
}

// TestTranslate_ALeaseEventFoldsTooKeeps the other fold site honest.
//
// The tick is what makes an outage observable while nothing else
// happens; the event arm is what keeps the reading CURRENT when
// something does. Without it the counter is up to one tick stale at
// every moment a lease event is handled -- including the last one
// before the client stops, whose value is what the deferred final
// report hands over.
//
// Driven with the ticker effectively switched off, so the only thing
// that can produce a report here is the event arm.
func TestTranslate_ALeaseEventFoldsToo(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RenewalStats, 8)
	c := &DHCPClient{
		iface:  "test0",
		opts:   DHCPClientOptions{OnRenewalStats: func(s RenewalStats) { reports <- s }},
		events: newEventChan(),
		src:    lib.src,
		runner: lib,
		// Far longer than this test can run: a report arriving here is
		// the event arm's or it is nothing.
		pollEvery: time.Hour,
	}
	go c.translate()

	// Two requests, the first of them therefore refused, and no tick
	// will ever come.
	lib.send(2)
	lib.src <- lease.Event{}

	select {
	case s := <-reports:
		if s.Unanswered != 1 {
			t.Fatalf("the event arm reported %d unanswered request(s), want 1", s.Unanswered)
		}
	case <-time.After(wedgeBudget):
		t.Fatalf("a lease event was handled and the counters were not folded within %v; every "+
			"reading taken between two ticks is then up to one tick stale, including the last one "+
			"before the client stops", wedgeBudget)
	}

	close(lib.src)
}

// TestTranslate_ANakTerminatedCycleReportsNothing is the NAK scenario
// at the seam, with the fold running on the real tick and the ending
// delivered as the real lease event.
//
// It is the preservation control on the tick: the whole change is "fold
// while nothing happens", and the cost of getting that wrong is a
// counter that moves on a network where every renewal was answered. A
// watch that reads RenewalsSent minus RenewalsCompleted passes every
// other test in this file and fails this one on the first tick after
// the second request leaves the host.
func TestTranslate_ANakTerminatedCycleReportsNothing(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RenewalStats, 8)
	c := &DHCPClient{
		iface:     "test0",
		opts:      DHCPClientOptions{OnRenewalStats: func(s RenewalStats) { reports <- s }},
		events:    newEventChan(),
		src:       lib.src,
		runner:    lib,
		pollEvery: time.Millisecond,
	}
	go c.translate()

	// The renewal at T1 leaves the host, and the server answers it with
	// a DHCPNAK: the lease is dropped and RenewalsCompleted never moves
	// for this cycle.
	lib.send(1)
	lib.src <- lease.Event{Kind: lease.Lost, Reason: proto.ReasonNak}

	// Drain the translated event. The loop emits it AFTER the counters
	// are folded and the cycle is closed, so taking it here is the
	// barrier that puts the next request unambiguously in the next
	// cycle rather than in a sleep.
	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("the Lost event was not translated within %v", wedgeBudget)
	}

	// The client re-acquires and renews again. That request is in
	// flight, and RenewalsSent is now 2 against a RenewalsCompleted
	// that is still 0.
	lib.send(2)

	select {
	case s := <-reports:
		t.Fatalf("%d unanswered renewal request(s) reported on a client whose every request was "+
			"answered: the first by a DHCPNAK, the second still in flight. The NAK path never "+
			"bumps RenewalsCompleted, so a counter built on Sent minus Completed reports an outage "+
			"here, logs the WARN line naming the endpoint, and does it once more for every NAK the "+
			"client ever takes", s.Unanswered)
	case <-time.After(200 * time.Millisecond):
	}

	close(lib.src)
}

// TestDHCPClientRelease_ForwardsToTheRunnerOrDoesNothing drives the
// chassis method the plugin's `release_lease=on_stop` calls.
//
// It is here and not in pkg/plugin because this is where the seam is:
// the plugin's release path can only see a client that already exists,
// and the two states that matter to it -- a client with a running
// machine behind it and a client whose Start never got that far -- are
// states of this struct. A forward that went missing would leave
// `release_lease=on_stop` calling a method that returns quietly, which
// reads at every layer above exactly like a server that ignored the
// packet.
func TestDHCPClientRelease_ForwardsToTheRunnerOrDoesNothing(t *testing.T) {
	f := &fakeLib{}
	c := &DHCPClient{runner: f}
	c.Release()
	c.Release()

	f.mu.Lock()
	got := f.releases
	f.mu.Unlock()
	if got != 2 {
		t.Errorf("the runner saw %d release call(s), want 2; a chassis that swallows the call makes "+
			"release_lease=on_stop a no-op that still logs a release", got)
	}

	// A client whose Start failed before the machine existed. The
	// plugin publishes it to the teardown path anyway, on purpose, so
	// this call happens and must not take the daemon down with it.
	var never DHCPClient
	never.Release()
}
