package runtime

import (
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
)

// The timer tests never name a duration to WAIT for. Every one of them arms a
// zero delay and then blocks on the Fired channel: the channel receive is the
// barrier, so the suite's runtime is the scheduler's latency and not a number
// somebody picked. A test that armed 50ms and slept 100ms would be a test that
// fails on a loaded machine and gets "fixed" by raising the number.

const forever = proto.Duration(1) * proto.Hour

func TestTimersFire(t *testing.T) {
	tm := NewTimers()
	defer func() { _ = tm.Close() }()

	for _, id := range proto.AllTimerIDs() {
		tm.Set(id, 0)
		if got := <-tm.Fired(); got != id {
			t.Fatalf("fired %s, want %s", got, id)
		}
	}
}

func TestSetReplacesRatherThanQueues(t *testing.T) {
	// The contract ring 1 depends on: it re-arms the retransmit timer freely
	// and never tracks what is armed. A Timers that queued a second fire
	// instead would produce a retransmission storm no ring-1 test could see.
	tm := NewTimers()
	defer func() { _ = tm.Close() }()

	tm.Set(proto.TimerRetransmit, forever)
	tm.Set(proto.TimerRetransmit, 0)

	if got := <-tm.Fired(); got != proto.TimerRetransmit {
		t.Fatalf("fired %s, want the replacement to fire", got)
	}

	// The sentinel is the absence drive: if the replaced arming had ALSO been
	// queued, its fire would be sitting in the channel ahead of this one.
	tm.Set(proto.TimerDesync, 0)
	if got := <-tm.Fired(); got != proto.TimerDesync {
		t.Fatalf("fired %s, want the sentinel — the replaced arming fired too", got)
	}
}

func TestCancelDisarms(t *testing.T) {
	tm := NewTimers()
	defer func() { _ = tm.Close() }()

	tm.Set(proto.TimerExpire, forever)
	tm.Cancel(proto.TimerExpire)

	tm.Set(proto.TimerDesync, 0)
	if got := <-tm.Fired(); got != proto.TimerDesync {
		t.Fatalf("fired %s, want the sentinel", got)
	}
}

func TestCancelDefeatsAnInFlightFire(t *testing.T) {
	// Stop cannot tell us whether an AfterFunc callback has already started,
	// so a cancel arriving while the callback is in flight is caught by the
	// generation counter instead. Driven here by racing them deliberately:
	// whichever side wins, the timer must never deliver MORE than one fire per
	// arming, and the sentinel must still be reachable.
	tm := NewTimers()
	defer func() { _ = tm.Close() }()

	const rounds = 200
	for i := 0; i < rounds; i++ {
		tm.Set(proto.TimerRetransmit, 0)
		tm.Cancel(proto.TimerRetransmit)
	}

	tm.Set(proto.TimerDesync, 0)
	fires := 0
	for got := range tm.Fired() {
		if got == proto.TimerDesync {
			break
		}
		fires++
		if fires > rounds {
			t.Fatalf("received %d retransmit fires for %d armings", fires, rounds)
		}
	}
}

func TestCancelOfAnUnarmedTimerIsNotAnError(t *testing.T) {
	// Ring 1 cancels every timer on every acquisition restart without
	// tracking which are armed, deliberately: the alternative is a second copy
	// of the timer state living in the pure ring.
	tm := NewTimers()
	defer func() { _ = tm.Close() }()
	for _, id := range proto.AllTimerIDs() {
		tm.Cancel(id)
		tm.Cancel(id)
	}
	tm.Set(proto.TimerDesync, 0)
	if got := <-tm.Fired(); got != proto.TimerDesync {
		t.Fatalf("fired %s after redundant cancels", got)
	}
}

func TestNegativeDelayFiresImmediately(t *testing.T) {
	// Ring 1 can legitimately ask for a non-positive delay: a lease whose
	// expiry has already passed by the time the ACK is processed. A timer
	// service that dropped it would strand the machine in BOUND holding a
	// dead lease.
	tm := NewTimers()
	defer func() { _ = tm.Close() }()
	tm.Set(proto.TimerExpire, -5*proto.Second)
	if got := <-tm.Fired(); got != proto.TimerExpire {
		t.Fatalf("fired %s, want the expiry", got)
	}
}

func TestUnknownTimerIDIsIgnored(t *testing.T) {
	// The table is sized from proto.AllTimerIDs. An id outside it must not
	// index past the end: a panic here takes the plugin down with it.
	tm := NewTimers()
	defer func() { _ = tm.Close() }()
	tm.Set(proto.TimerID(200), 0)
	tm.Cancel(proto.TimerID(200))

	tm.Set(proto.TimerDesync, 0)
	if got := <-tm.Fired(); got != proto.TimerDesync {
		t.Fatalf("fired %s", got)
	}
}

func TestCloseClosesTheStreamAndIsIdempotent(t *testing.T) {
	tm := NewTimers()
	tm.Set(proto.TimerExpire, forever)
	if err := tm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := <-tm.Fired(); ok {
		t.Fatal("the fired channel is still open after Close")
	}
	if err := tm.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Set after Close must not panic and must not resurrect the service.
	tm.Set(proto.TimerDesync, 0)
	if _, ok := <-tm.Fired(); ok {
		t.Fatal("Set after Close delivered a fire on a closed channel")
	}
}

func TestTimerTableIsSizedFromTheProtocol(t *testing.T) {
	// numTimers is derived from proto.AllTimerIDs rather than written as a
	// literal. Adding a TimerID must widen the table automatically; a constant
	// here would silently start ignoring the new one.
	if numTimers != len(proto.AllTimerIDs()) {
		t.Fatalf("numTimers = %d, proto declares %d timers", numTimers, len(proto.AllTimerIDs()))
	}
	tm := NewTimers()
	defer func() { _ = tm.Close() }()
	if len(tm.gen) != numTimers || len(tm.timer) != numTimers {
		t.Fatalf("timer table is %d/%d entries, want %d", len(tm.gen), len(tm.timer), numTimers)
	}
}
