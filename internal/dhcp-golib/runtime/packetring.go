package runtime

import (
	"sync"

	"github.com/claymore666/dhcp-golib/lease"
)

// PacketRing is the bounded ring of every message in and out (G1, R3).
//
// Separate from the Journal on purpose: the journal records the machine
// stepping, this records the wire. A packet the codec REFUSED never reaches
// ring 1 and so never appears in the journal — and with only the journal to
// look at, "the server sent something we could not parse" and "the server sent
// nothing" are the same silence.
type PacketRing struct {
	mu      sync.Mutex
	buf     []lease.CapturedPacket
	next    int
	full    bool
	dropped int
}

// DefaultPacketRingSize is the number of packets a PacketRing keeps.
const DefaultPacketRingSize = 256

// NewPacketRing returns a ring holding at most size packets. A size below 1 is
// raised to 1, for the same reason as NewJournal: a recorder that records
// nothing looks exactly like one that works.
func NewPacketRing(size int) *PacketRing {
	if size < 1 {
		size = 1
	}
	return &PacketRing{buf: make([]lease.CapturedPacket, size)}
}

// Record stores one packet, discarding the oldest if the ring is full.
func (r *PacketRing) Record(p lease.CapturedPacket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		r.dropped++
	}
	r.buf[r.next] = p
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Packets returns the retained packets oldest-first.
func (r *PacketRing) Packets() []lease.CapturedPacket {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]lease.CapturedPacket, 0, len(r.buf))
	if r.full {
		out = append(out, r.buf[r.next:]...)
	}
	out = append(out, r.buf[:r.next]...)
	return out
}

// Dropped is how many packets the ring has discarded.
func (r *PacketRing) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}
