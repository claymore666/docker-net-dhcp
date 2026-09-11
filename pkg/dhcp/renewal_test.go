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
	// being served and nothing here is a fault.
	for i := uint64(1); i <= 3; i++ {
		if got := w.fold(lease.Stats{RenewalsSent: i, RenewalsCompleted: i - 1}); got != 0 {
			t.Fatalf("renewal %d in flight reported %d unanswered request(s); a request that is "+
				"waiting for an answer has not been refused one", i, got)
		}
		if got := w.fold(lease.Stats{RenewalsSent: i, RenewalsCompleted: i}); got != 0 {
			t.Fatalf("renewal %d answered reported %d unanswered request(s); the server answered "+
				"this one", i, got)
		}
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
	steps := []lease.Stats{
		{RenewalsSent: 1},
		{RenewalsSent: 2},
		{RenewalsSent: 3},
		{RenewalsSent: 3, RenewalsCompleted: 1},
		{RenewalsSent: 4, RenewalsCompleted: 1},
		{RenewalsSent: 5, RenewalsCompleted: 1},
		{RenewalsSent: 6, RenewalsCompleted: 1},
		{RenewalsSent: 6, RenewalsCompleted: 2},
	}
	last := uint64(0)
	for i, s := range steps {
		total += w.fold(s)
		if total < last {
			t.Fatalf("step %d took the total from %d to %d. A Prometheus counter that decreases is "+
				"a RESET, and the next scrape repays the whole accumulated value as a rate spike; "+
				"Sent-minus-Completed falls at every acknowledgement, which is why this is a "+
				"running maximum and not a subtraction", i, last, total)
		}
		last = total
	}
	if total != 4 {
		t.Fatalf("two renewal cycles of three requests each, one answered per cycle, reported %d "+
			"unanswered; want 4", total)
	}

	// The gain is never negative because it cannot be: it is unsigned.
	// What can happen is a fold that hands out the same ground twice,
	// so re-folding a snapshot already seen must report nothing.
	if again := w.fold(steps[len(steps)-1]); again != 0 {
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
