package runtime

import (
	"sync"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// Timers is the real timer service: one goroutine, one table, Set-replaces
// semantics.
//
// A table indexed by TimerID rather than a heap or a map, because
// proto.AllTimerIDs is a CLOSED set: that makes "Set on an armed timer
// REPLACES it" a property of the data structure rather than a rule to
// remember. No count is written here or below — this said "three" until
// TimerRestart made it four, and TestAllTimerIDsIsEveryDeclaredTimer is what
// keeps the set closed now. A queue would let two fires for one id exist at once, and a
// duplicated retransmit fire is a storm no ring-1 test could see.
//
// One goroutine owns every time.Timer, so a Cancel racing a fire resolves in
// one place. A fire that loses that race is dropped rather than delivered: the
// generation counter below means a cancelled timer's callback finds a stale
// generation and returns.
type Timers struct {
	mu    sync.Mutex
	gen   []uint64
	timer []*time.Timer

	fired  chan proto.TimerID
	closed bool
	done   chan struct{}
}

// numTimers is derived from the protocol's closed set, not written as a
// literal, so adding a TimerID cannot silently index past the end here. Hence
// slices: an array length must be constant, and a constant would be a second
// copy of a fact proto already owns.
var numTimers = len(proto.AllTimerIDs())

// NewTimers returns a running timer service.
//
// The fired channel is buffered by the number of timers: every armed timer can
// deliver exactly once without the owning goroutine blocking, which matters
// because ring 2 drains an action list before it reads Fired again.
func NewTimers() *Timers {
	return &Timers{
		gen:   make([]uint64, numTimers),
		timer: make([]*time.Timer, numTimers),
		fired: make(chan proto.TimerID, numTimers),
		done:  make(chan struct{}),
	}
}

// Set arms id to fire after d, replacing any existing arming of id.
//
// A non-positive d fires immediately rather than never. Ring 1 can legitimately
// ask for a zero delay — a lease whose expiry has already passed by the time
// the ACK is processed — and a timer service that dropped it would strand the
// machine in BOUND with a dead lease.
func (t *Timers) Set(id proto.TimerID, d proto.Duration) {
	if int(id) >= numTimers {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.stopLocked(id)
	t.gen[id]++
	g := t.gen[id]
	if d < 0 {
		d = 0
	}
	t.timer[id] = time.AfterFunc(time.Duration(d), func() { t.fire(id, g) })
}

// Cancel disarms id. Cancelling an unarmed timer is not an error: ring 1
// cancels EVERY timer on every acquisition restart without tracking which are
// armed, and that is deliberate — the alternative is a second copy of the
// timer state living in the pure ring.
func (t *Timers) Cancel(id proto.TimerID) {
	if int(id) >= numTimers {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopLocked(id)
	t.gen[id]++
}

// Fired is the stream of timer fires.
func (t *Timers) Fired() <-chan proto.TimerID { return t.fired }

// Close stops every timer and closes the fired channel. It is safe to call
// more than once.
func (t *Timers) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	for i := range t.timer {
		t.stopLocked(proto.TimerID(i))
		t.gen[i]++
	}
	close(t.done)
	close(t.fired)
	return nil
}

func (t *Timers) stopLocked(id proto.TimerID) {
	if t.timer[id] != nil {
		t.timer[id].Stop()
		t.timer[id] = nil
	}
}

// fire delivers one fire if it is still the current arming of id.
//
// The generation check is the whole race resolution: Stop cannot tell us
// whether an AfterFunc callback has already started, so a cancel that arrives
// while the callback is in flight is caught here instead.
func (t *Timers) fire(id proto.TimerID, g uint64) {
	t.mu.Lock()
	if t.closed || t.gen[id] != g {
		t.mu.Unlock()
		return
	}
	t.timer[id] = nil
	t.mu.Unlock()

	select {
	case t.fired <- id:
	case <-t.done:
	}
}
