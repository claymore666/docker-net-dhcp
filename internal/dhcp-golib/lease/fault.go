package lease

import (
	"errors"
	"fmt"
	"sync"

	"github.com/claymore666/dhcp-golib/proto"
)

// R2, written BEFORE the happy path on purpose: every action can fail, and the
// failure re-enters the machine as an event. The requirements document calls
// it "the requirement most likely to be quietly dropped, because a
// fire-and-forget action list is easier to write and looks identical while
// everything works", and it is unretrofittable — once the machine assumes its
// sends succeeded, every transition has to be revisited. Building the fault
// transport first leaves no moment at which the happy path exists and the
// failure path does not.

// ErrInjected is the error a FaultTransport returns for a send it was told to
// fail. It is a distinct value so a test can assert the failure it planted is
// the failure it observed, rather than matching on text.
var ErrInjected = errors.New("lease: injected transport fault")

// Fault is a deterministic fault plan.
//
// A list of ordinals rather than a probability: a randomly failing transport
// produces a test that fails one run in twenty and gets "fixed" by removing
// the fault. Ordinals count from 1, so FailSends{1} fails the first send.
type Fault struct {
	// FailSends names the send ordinals that return ErrInjected instead of
	// transmitting.
	FailSends []int
	// DropInbound names the inbound ordinals that are swallowed before the
	// manager sees them. This is packet loss, which ring 1 by construction
	// cannot see (design document section 2.3) and which the retransmission
	// logic exists for.
	DropInbound []int
	// DuplicateInbound names the inbound ordinals delivered twice. A
	// duplicated ACK must not produce two leases.
	DuplicateInbound []int
	// CorruptInbound names the inbound ordinals whose first payload byte is
	// flipped before delivery. That is the 'op' field, so the message decodes
	// and then fails ring 1's BOOTREPLY check — a hostile-but-well-formed
	// input rather than a codec error.
	CorruptInbound []int
	// FailEvery, when non-zero, additionally fails every Nth send. It exists
	// for the "the transport is simply broken" case, which is what drives
	// MaxSendFailures.
	FailEvery int
}

func contains(xs []int, n int) bool {
	for _, x := range xs {
		if x == n {
			return true
		}
	}
	return false
}

// FaultTransport wraps a Transport and applies a Fault plan.
//
// It wraps rather than replaces so one plan drives both the fake transport in
// a unit test and the real AF_PACKET transport in a ring-3 test: a fault
// injector that only exists for the fake proves nothing about the real one.
type FaultTransport struct {
	inner Transport
	plan  Fault

	out chan Inbound

	mu       sync.Mutex
	sends    int
	inbounds int

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// NewFaultTransport wraps inner.
func NewFaultTransport(inner Transport, plan Fault) *FaultTransport {
	f := &FaultTransport{
		inner: inner,
		plan:  plan,
		out:   make(chan Inbound, 16),
		done:  make(chan struct{}),
	}
	f.wg.Add(1)
	go f.pump()
	return f
}

// Send applies the send half of the plan.
func (f *FaultTransport) Send(dst proto.Dest, payload []byte) error {
	f.mu.Lock()
	f.sends++
	n := f.sends
	f.mu.Unlock()

	if contains(f.plan.FailSends, n) {
		return fmt.Errorf("%w: send %d", ErrInjected, n)
	}
	if f.plan.FailEvery > 0 && n%f.plan.FailEvery == 0 {
		return fmt.Errorf("%w: send %d (every %d)", ErrInjected, n, f.plan.FailEvery)
	}
	return f.inner.Send(dst, payload)
}

// Received returns the filtered inbound stream.
func (f *FaultTransport) Received() <-chan Inbound { return f.out }

// Close closes the wrapped transport and the filtered stream.
func (f *FaultTransport) Close() error {
	err := f.inner.Close()
	f.closeOnce.Do(func() { close(f.done) })
	f.wg.Wait()
	return err
}

// Counts reports how many sends and inbound packets have passed through, so a
// test can measure that the fault was applied: a plan naming send 7 on a run
// that makes three sends injects nothing and otherwise looks like a pass.
func (f *FaultTransport) Counts() (sends, inbounds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends, f.inbounds
}

func (f *FaultTransport) pump() {
	defer f.wg.Done()
	defer close(f.out)
	src := f.inner.Received()
	for {
		select {
		case <-f.done:
			return
		case in, ok := <-src:
			if !ok {
				return
			}
			f.mu.Lock()
			f.inbounds++
			n := f.inbounds
			f.mu.Unlock()

			if contains(f.plan.DropInbound, n) {
				continue
			}
			if contains(f.plan.CorruptInbound, n) && len(in.Payload) > 0 {
				p := append([]byte(nil), in.Payload...)
				p[0] ^= 0xFF
				in.Payload = p
			}
			if !f.emit(in) {
				return
			}
			if contains(f.plan.DuplicateInbound, n) {
				dup := in
				dup.Payload = append([]byte(nil), in.Payload...)
				if !f.emit(dup) {
					return
				}
			}
		}
	}
}

func (f *FaultTransport) emit(in Inbound) bool {
	select {
	case f.out <- in:
		return true
	case <-f.done:
		return false
	}
}
