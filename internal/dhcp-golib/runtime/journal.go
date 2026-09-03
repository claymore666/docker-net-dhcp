package runtime

import (
	"sync"

	"github.com/claymore666/dhcp-golib/proto"
)

// Journal is the bounded in-memory journal (requirements G2, R3).
//
// Bounded because R3 says every buffer here has a fixed maximum: a long-lived
// client renews for months. When the ring wraps, the OLDEST entries go and
// Dropped counts them.
//
// Dropped matters because proto.Replay needs a CONTIGUOUS run of entries from
// the machine's start. A wrapped journal cannot supply one, and Entries()
// would hand back a plausible prefix-less sequence that replays into an
// unexplainable divergence, so a caller that wants replayability checks that
// Dropped is zero.
type Journal struct {
	mu      sync.Mutex
	buf     []proto.JournalEntry
	next    int
	full    bool
	dropped int
}

// DefaultJournalSize covers an acquisition and a long run of renewals for a
// few hundred kilobytes. A default, not a limit.
const DefaultJournalSize = 4096

// NewJournal returns a journal holding at most size entries. A size below 1 is
// raised to 1: a zero-capacity recorder and a working one are
// indistinguishable from the outside.
func NewJournal(size int) *Journal {
	if size < 1 {
		size = 1
	}
	return &Journal{buf: make([]proto.JournalEntry, size)}
}

// Append records one entry, discarding the oldest if the ring is full.
func (j *Journal) Append(e proto.JournalEntry) {
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
func (j *Journal) Entries() []proto.JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]proto.JournalEntry, 0, len(j.buf))
	if j.full {
		out = append(out, j.buf[j.next:]...)
	}
	out = append(out, j.buf[:j.next]...)
	return out
}

// Dropped is how many entries the ring has discarded. Non-zero means Entries()
// is not replayable — see the type comment.
func (j *Journal) Dropped() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dropped
}
