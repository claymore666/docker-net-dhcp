package runtime

import (
	"sync"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// Journal6 is the bounded in-memory journal of Machine6's steps.
//
// A second type beside Journal and not a generic one, for lease.Journal6's
// reason: proto.JournalEntry and proto.JournalEntry6 record two different
// state enumerations, and the recorded from- and to-states are the whole point
// of an entry. Everything else here — the wrap, the drop count and what a
// non-zero drop count costs proto.Replay6 — is Journal's, and the reason is
// Journal's.
type Journal6 struct {
	mu      sync.Mutex
	buf     []proto.JournalEntry6
	next    int
	full    bool
	dropped int
}

// NewJournal6 returns a v6 journal holding at most size entries. A size below
// 1 is raised to 1, for NewJournal's reason.
func NewJournal6(size int) *Journal6 {
	if size < 1 {
		size = 1
	}
	return &Journal6{buf: make([]proto.JournalEntry6, size)}
}

// Append records one entry, discarding the oldest if the ring is full.
func (j *Journal6) Append(e proto.JournalEntry6) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.full {
		j.dropped++
	}
	j.buf[j.next] = e
	j.next = (j.next + 1) % len(j.buf)
	if j.next == 0 {
		j.full = true
	}
}

// Entries returns the retained entries oldest-first.
func (j *Journal6) Entries() []proto.JournalEntry6 {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]proto.JournalEntry6, 0, len(j.buf))
	if j.full {
		out = append(out, j.buf[j.next:]...)
	}
	out = append(out, j.buf[:j.next]...)
	return out
}

// Dropped is how many entries the ring has discarded. Non-zero means Entries()
// is not replayable — see Journal.
func (j *Journal6) Dropped() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dropped
}

// PacketRingV6 is the bounded ring of every DHCPv6 message and admitted
// Router Advertisement in and out.
//
// Separate from PacketRing for PacketRing's own reason applied to a second
// wire: a Router Advertisement the codec refused never reaches ring 1, and
// with only the journal to look at, "the router advertised something we could
// not parse" and "no router advertised" are the same silence — which is
// exactly the vacuity trap the v6 fixture is built to close.
type PacketRingV6 struct {
	mu      sync.Mutex
	buf     []lease.CapturedPacketV6
	next    int
	full    bool
	dropped int
}

// NewPacketRingV6 returns a ring holding at most size packets. A size below 1
// is raised to 1, for NewPacketRing's reason.
func NewPacketRingV6(size int) *PacketRingV6 {
	if size < 1 {
		size = 1
	}
	return &PacketRingV6{buf: make([]lease.CapturedPacketV6, size)}
}

// Record stores one packet, discarding the oldest if the ring is full.
func (r *PacketRingV6) Record(p lease.CapturedPacketV6) {
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
func (r *PacketRingV6) Packets() []lease.CapturedPacketV6 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]lease.CapturedPacketV6, 0, len(r.buf))
	if r.full {
		out = append(out, r.buf[r.next:]...)
	}
	out = append(out, r.buf[:r.next]...)
	return out
}

// Dropped is how many packets the ring has discarded.
func (r *PacketRingV6) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}
