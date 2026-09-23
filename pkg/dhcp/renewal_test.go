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

func TestRenewalWatch_AnAnsweredRenewalNeverMovesTheCounter(t *testing.T) {
	var w renewalWatch

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

// A DHCPNAK in RENEWING drops the lease with ReasonNak and never reaches enterBound, the only bump of
// RenewalsCompleted; the v6 twin is NotOnLink, and both reach the chassis as Lost (#940).

func TestRenewalWatch_AnEndingThatNeverCompletesIsStillAnEnding(t *testing.T) {
	var w renewalWatch
	var total uint64

	// A DHCPNAK at T1: RenewalsCompleted never moves for this cycle (#940).
	naked := lease.Stats{RenewalsSent: 1, NaksSeen: 1, NaksAccepted: 1, LeasesLost: 1}
	total += w.fold(naked)
	w.cycleEnded(naked)

	// An acquisition's DHCPREQUEST has a zero ciaddr and is not a renewal (RFC 2131 Table 5).
	reacquired := naked
	reacquired.LeasesAcquired = 2
	total += w.fold(reacquired)
	w.cycleEnded(reacquired)

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

	acked := inFlight
	acked.RenewalsCompleted = 1
	total += w.fold(acked)
	w.cycleEnded(acked)
	if total != 0 {
		t.Fatalf("after the second renewal was acknowledged the counter reads %d; every request in "+
			"this test was answered", total)
	}
}

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

// Four renewal requests over 7h52m, the fourth answered, as reported in #940.

func TestRenewalWatch_MovesOncePerRetransmission(t *testing.T) {
	var w renewalWatch
	total := uint64(0)

	total += w.fold(lease.Stats{RenewalsSent: 1})
	if total != 0 {
		t.Fatalf("the first renewal request alone reported %d unanswered; it is still in flight", total)
	}

	for send := uint64(2); send <= 4; send++ {
		before := total
		total += w.fold(lease.Stats{RenewalsSent: send})
		if total != before+1 {
			t.Fatalf("retransmission %d moved the counter from %d to %d; a retransmission proves "+
				"exactly one earlier request unanswered", send, before, total)
		}
	}

	total += w.fold(lease.Stats{RenewalsSent: 4, RenewalsCompleted: 1})
	if total != 3 {
		t.Fatalf("after the fourth request was answered the counter reads %d; the first three went "+
			"unanswered and the answer to the fourth does not undo that", total)
	}
}

// A Prometheus counter decrease is a reset that repays the whole value as a rate spike (D-2, #730).

func TestRenewalWatch_NeverFalls(t *testing.T) {
	var w renewalWatch
	var total uint64

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

// RFC 2131 Table 5: only RENEWING and REBINDING send a DHCPREQUEST with a non-zero ciaddr (D-4, #940).

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

// fakeLib is a libClient whose counters the test controls.
type fakeLib struct {
	mu    sync.Mutex
	stats lease.Stats
	src   chan lease.Event
}

func (f *fakeLib) Run(ctx context.Context) error { return nil }
func (f *fakeLib) Events() <-chan lease.Event    { return f.src }
func (f *fakeLib) Lease() (lease.Lease, bool)    { return lease.Lease{}, false }

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

func TestTranslate_ARetransmissionIsReportedWithNoLeaseEvent(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RenewalStats, 8)
	c := &DHCPClient{
		iface:  "test0",
		opts:   DHCPClientOptions{OnRenewalStats: func(s RenewalStats) { reports <- s }},
		events: newEventChan(),
		src:    lib.src,
		runner: lib,
		// Far below RFC 2131 section 4.4.5's one-minute floor; the test places the retransmissions itself.
		pollEvery: time.Millisecond,
	}
	go c.translate()

	lib.send(1)
	select {
	case s := <-reports:
		t.Fatalf("a renewal request still in flight was reported as %d unanswered", s.Unanswered)
	case <-time.After(50 * time.Millisecond):
	}

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

// RFC 2131 section 4.4.5 floors the renewal retransmission wait at one minute (proto.RenewRetransmitFloor).

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

func TestTranslate_ALeaseEventFoldsToo(t *testing.T) {
	lib := &fakeLib{src: make(chan lease.Event)}

	reports := make(chan RenewalStats, 8)
	c := &DHCPClient{
		iface:  "test0",
		opts:   DHCPClientOptions{OnRenewalStats: func(s RenewalStats) { reports <- s }},
		events: newEventChan(),
		src:    lib.src,
		runner: lib,
		// Longer than the test runs, so only the event arm can report (#940).
		pollEvery: time.Hour,
	}
	go c.translate()

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

	lib.send(1)
	lib.src <- lease.Event{Kind: lease.Lost, Reason: proto.ReasonNak}

	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("the Lost event was not translated within %v", wedgeBudget)
	}

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

// advertsSeen moves the library's advertisement count, which no lease event accompanies (#814).
func (f *fakeLib) advertsSeen(n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats.RouterAdvertsSeen = n
}
