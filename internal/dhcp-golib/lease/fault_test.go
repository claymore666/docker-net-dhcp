package lease

import (
	"errors"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
)

// echoTransport is the inner transport the fault plan wraps: it records sends
// and lets the test push inbound packets by hand.
type echoTransport struct {
	sent    [][]byte
	inbound chan Inbound
	closed  bool
}

func newEcho() *echoTransport {
	return &echoTransport{inbound: make(chan Inbound, 16)}
}

func (e *echoTransport) Send(_ proto.Dest, p []byte) error {
	e.sent = append(e.sent, append([]byte(nil), p...))
	return nil
}
func (e *echoTransport) Received() <-chan Inbound { return e.inbound }
func (e *echoTransport) Close() error {
	if !e.closed {
		e.closed = true
		close(e.inbound)
	}
	return nil
}

func TestFaultTransportPreservesAnEmptyPlan(t *testing.T) {
	// The preservation control for every fault test: with nothing planted,
	// the wrapper must be transparent. It is always in the path — the manager
	// tests wrap even for the happy path — so a wrapper that dropped or
	// duplicated on its own would corrupt every test in the package.
	inner := newEcho()
	f := NewFaultTransport(inner, Fault{})
	defer func() { _ = f.Close() }()

	if err := f.Send(proto.Dest{Broadcast: true}, []byte{1, 2, 3}); err != nil {
		t.Fatalf("Send with an empty plan: %v", err)
	}
	if len(inner.sent) != 1 {
		t.Fatalf("inner saw %d sends, want 1", len(inner.sent))
	}
	inner.inbound <- Inbound{Payload: []byte{9}}
	got := <-f.Received()
	if len(got.Payload) != 1 || got.Payload[0] != 9 {
		t.Fatalf("payload = %v, want it passed through untouched", got.Payload)
	}
}

func TestFaultTransportFailsNamedSends(t *testing.T) {
	inner := newEcho()
	f := NewFaultTransport(inner, Fault{FailSends: []int{2, 3}})
	defer func() { _ = f.Close() }()

	var errs []error
	for i := 0; i < 4; i++ {
		errs = append(errs, f.Send(proto.Dest{Broadcast: true}, []byte{byte(i)}))
	}
	if errs[0] != nil || errs[3] != nil {
		t.Fatalf("unnamed sends failed: %v", errs)
	}
	for _, i := range []int{1, 2} {
		if !errors.Is(errs[i], ErrInjected) {
			t.Fatalf("send %d error = %v, want ErrInjected", i+1, errs[i])
		}
	}
	// A failed send must not reach the inner transport: injecting an error
	// while still transmitting would test nothing.
	if len(inner.sent) != 2 {
		t.Fatalf("inner saw %d sends, want the 2 that were not failed", len(inner.sent))
	}
	if sends, _ := f.Counts(); sends != 4 {
		t.Fatalf("Counts reports %d sends, want 4", sends)
	}
}

func TestFaultTransportFailEvery(t *testing.T) {
	inner := newEcho()
	f := NewFaultTransport(inner, Fault{FailEvery: 1})
	defer func() { _ = f.Close() }()
	for i := 0; i < 5; i++ {
		if err := f.Send(proto.Dest{Broadcast: true}, []byte{1}); !errors.Is(err, ErrInjected) {
			t.Fatalf("send %d = %v, want every send to fail", i+1, err)
		}
	}
	if len(inner.sent) != 0 {
		t.Fatalf("inner saw %d sends under FailEvery 1", len(inner.sent))
	}
}

func TestFaultTransportDropsDuplicatesAndCorrupts(t *testing.T) {
	inner := newEcho()
	f := NewFaultTransport(inner, Fault{
		DropInbound:      []int{1},
		DuplicateInbound: []int{2},
		CorruptInbound:   []int{3},
	})
	defer func() { _ = f.Close() }()

	inner.inbound <- Inbound{Payload: []byte{0xA1}}
	inner.inbound <- Inbound{Payload: []byte{0xB2}}
	inner.inbound <- Inbound{Payload: []byte{0xC3}}

	// 1 dropped; 2 delivered twice; 3 delivered once with its first byte
	// flipped. Three deliveries for three inbound packets.
	want := [][]byte{{0xB2}, {0xB2}, {^byte(0xC3)}}
	var got [][]byte
	for i := 0; i < len(want); i++ {
		in := <-f.Received()
		got = append(got, in.Payload)
	}
	for i := range want {
		if got[i][0] != want[i][0] {
			t.Fatalf("payload %d = %#x, want %#x", i, got[i][0], want[i][0])
		}
	}
	if _, inbounds := f.Counts(); inbounds != 3 {
		t.Fatalf("Counts reports %d inbound, want 3", inbounds)
	}
}

func TestFaultTransportDuplicateDoesNotAliasThePayload(t *testing.T) {
	// The duplicate must be a COPY. A duplicate that aliased the original
	// would let a consumer's in-place edit of one change the other, which is
	// the kind of bug that only appears once something downstream mutates a
	// payload — long after this code is trusted.
	inner := newEcho()
	f := NewFaultTransport(inner, Fault{DuplicateInbound: []int{1}})
	defer func() { _ = f.Close() }()

	inner.inbound <- Inbound{Payload: []byte{1, 2, 3}}
	a := <-f.Received()
	b := <-f.Received()
	a.Payload[0] = 0xFF
	if b.Payload[0] == 0xFF {
		t.Fatal("the duplicate aliases the original payload")
	}
}

func TestFaultTransportCloseIsIdempotent(t *testing.T) {
	f := NewFaultTransport(newEcho(), Fault{})
	if err := f.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestFaultTransportCountsCatchAPlanThatInjectsNothing(t *testing.T) {
	// The reason Counts exists. A plan naming send 7 on a run that makes three
	// sends injects nothing, and the test asserting "the client survived the
	// fault" then passes having faulted nothing at all. The counter is what
	// turns that from an invisible pass into a checkable number.
	inner := newEcho()
	f := NewFaultTransport(inner, Fault{FailSends: []int{7}})
	defer func() { _ = f.Close() }()
	for i := 0; i < 3; i++ {
		if err := f.Send(proto.Dest{Broadcast: true}, []byte{1}); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	sends, _ := f.Counts()
	if sends != 3 {
		t.Fatalf("Counts = %d", sends)
	}
	if sends >= 7 {
		t.Fatal("the fixture reached the planned ordinal; this test no longer demonstrates the gap")
	}
}
