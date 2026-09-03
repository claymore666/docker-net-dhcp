package runtime

import (
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

func TestJournalKeepsOrderAndBound(t *testing.T) {
	j := NewJournal(4)
	for i := 0; i < 4; i++ {
		j.Append(proto.JournalEntry{Seq: uint64(i)})
	}
	got := j.Entries()
	if len(got) != 4 {
		t.Fatalf("held %d entries, want 4", len(got))
	}
	for i, e := range got {
		if e.Seq != uint64(i) {
			t.Fatalf("entry %d has seq %d; the ring is not oldest-first", i, e.Seq)
		}
	}
	if j.Dropped() != 0 {
		t.Fatalf("Dropped = %d before the ring wrapped", j.Dropped())
	}
}

func TestJournalWrapsAndCountsWhatItLost(t *testing.T) {
	// R3: every buffer here has a fixed maximum, because a long-lived client
	// renews for months and an unbounded journal is a memory leak with a
	// respectable name.
	//
	// Dropped is the load-bearing half. Replay needs a CONTIGUOUS run from the
	// machine's start; a wrapped journal cannot supply one, and Entries() would
	// otherwise hand back a plausible prefix-less sequence that replays into a
	// divergence nobody could explain.
	j := NewJournal(4)
	for i := 0; i < 10; i++ {
		j.Append(proto.JournalEntry{Seq: uint64(i)})
	}
	got := j.Entries()
	if len(got) != 4 {
		t.Fatalf("held %d entries, want the bound of 4", len(got))
	}
	for i, e := range got {
		if want := uint64(6 + i); e.Seq != want {
			t.Fatalf("entry %d has seq %d, want %d (the newest four)", i, e.Seq, want)
		}
	}
	if j.Dropped() != 6 {
		t.Fatalf("Dropped = %d, want 6", j.Dropped())
	}
}

func TestJournalSizeIsNeverZero(t *testing.T) {
	// A zero-capacity recorder and a working one are indistinguishable from
	// the outside, which is the shape this project keeps paying for.
	for _, size := range []int{0, -1, -1000} {
		j := NewJournal(size)
		j.Append(proto.JournalEntry{Seq: 7})
		got := j.Entries()
		if len(got) != 1 || got[0].Seq != 7 {
			t.Fatalf("NewJournal(%d) recorded %v, want the one entry", size, got)
		}
	}
}

func TestJournalEntriesIsASnapshot(t *testing.T) {
	// A caller that ranges over Entries while the client keeps running must
	// not see the slice change underneath it.
	j := NewJournal(8)
	j.Append(proto.JournalEntry{Seq: 1})
	snap := j.Entries()
	j.Append(proto.JournalEntry{Seq: 2})
	if len(snap) != 1 {
		t.Fatalf("the snapshot grew to %d entries", len(snap))
	}
}

func TestPacketRingWrapsAndCounts(t *testing.T) {
	r := NewPacketRing(3)
	for i := 0; i < 5; i++ {
		r.Record(lease.CapturedPacket{At: time.Unix(int64(i), 0), Dir: lease.DirIn})
	}
	got := r.Packets()
	if len(got) != 3 {
		t.Fatalf("held %d packets, want 3", len(got))
	}
	for i, p := range got {
		if want := int64(2 + i); p.At.Unix() != want {
			t.Fatalf("packet %d is from %d, want %d", i, p.At.Unix(), want)
		}
	}
	if r.Dropped() != 2 {
		t.Fatalf("Dropped = %d, want 2", r.Dropped())
	}
}

func TestPacketRingSizeIsNeverZero(t *testing.T) {
	for _, size := range []int{0, -3} {
		r := NewPacketRing(size)
		r.Record(lease.CapturedPacket{Dir: lease.DirOut})
		if got := r.Packets(); len(got) != 1 {
			t.Fatalf("NewPacketRing(%d) recorded %d packets, want 1", size, len(got))
		}
	}
}

func TestEntropyIsDeterministicWhenSeeded(t *testing.T) {
	// Replay of a recorded journal is a production feature, not a test
	// fixture, and it needs a reproducible stream.
	a := NewEntropySeeded(42)
	b := NewEntropySeeded(42)
	for i := 0; i < 1000; i++ {
		x, y := a.Uint64(), b.Uint64()
		if x != y {
			t.Fatalf("two sources seeded alike diverged at %d: %d != %d", i, x, y)
		}
	}
	// And a different seed gives a different stream: a "deterministic" source
	// that ignored its seed would pass the check above.
	c := NewEntropySeeded(43)
	d := NewEntropySeeded(42)
	same := 0
	for i := 0; i < 100; i++ {
		if c.Uint64() == d.Uint64() {
			same++
		}
	}
	if same > 1 {
		t.Fatalf("%d of 100 values matched across different seeds", same)
	}
}

func TestEntropyDoesNotRepeatImmediately(t *testing.T) {
	// The xid derives from this. A source that returned a constant, or walked
	// a short cycle, would produce clients whose transaction ids collide — and
	// every acquisition test would still pass, because a fixture answers
	// whatever xid it is asked with.
	e := NewEntropySeeded(1)
	seen := make(map[uint64]int, 100000)
	for i := 0; i < 100000; i++ {
		v := e.Uint64()
		if prev, dup := seen[v]; dup {
			t.Fatalf("value %d repeated at draws %d and %d", v, prev, i)
		}
		seen[v] = i
	}
}

func TestEntropyFromCryptoRandWorks(t *testing.T) {
	e, err := NewEntropy()
	if err != nil {
		t.Fatalf("NewEntropy: %v", err)
	}
	a, b := e.Uint64(), e.Uint64()
	if a == b {
		t.Fatalf("two consecutive draws are both %d", a)
	}
}

func TestClockMonoAdvancesAndWallIsReal(t *testing.T) {
	// The two readings are not interchangeable, which is the whole reason the
	// Clock has both. Mono must be a monotonic count; Wall must be a plausible
	// wall-clock time.
	var c Clock
	a := c.Mono()
	b := c.Mono()
	if b < a {
		t.Fatalf("the monotonic clock went backwards: %d then %d", a, b)
	}
	if a <= 0 {
		t.Fatalf("Mono = %d; CLOCK_BOOTTIME should be the uptime in nanoseconds", a)
	}
	w := c.Wall()
	if w.Year() < 2020 {
		t.Fatalf("Wall = %s, which is not a plausible wall-clock reading", w)
	}
}

func TestFixedClockAdvancesBothTogether(t *testing.T) {
	c := &FixedClock{WallAt: time.Unix(1000, 0)}
	c.Advance(90 * proto.Second)
	if c.Mono() != proto.Instant(90*proto.Second) {
		t.Fatalf("Mono = %d", c.Mono())
	}
	if c.Wall().Unix() != 1090 {
		t.Fatalf("Wall = %s", c.Wall())
	}
}
