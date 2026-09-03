package proto

import "testing"

// TestDefaultBackoffMatchesRFC2131 pins the RFC's worked example.
//
// RFC 2131 section 4.1: "the delay before the first retransmission SHOULD be 4
// seconds randomized by the value of a uniform random number chosen from the
// range -1 to +1 ... The delay before the next retransmission SHOULD be 8
// seconds ... The retransmission delay SHOULD be doubled with subsequent
// retransmissions up to a maximum of 64 seconds."
//
// And section 3.1(5): "a client retransmitting as described in section 4.1
// might retransmit the DHCPREQUEST message four times, for a total delay of 60
// seconds, before restarting the initialization procedure."
//
// 4 + 8 + 16 + 32 = 60. The defaults are pinned here so that changing them is
// a decision somebody makes on purpose rather than a constant somebody tidies.
func TestDefaultBackoffMatchesRFC2131(t *testing.T) {
	b := DefaultBackoff()
	nominal := []Duration{4 * Second, 8 * Second, 16 * Second, 32 * Second}

	var total Duration
	for n, want := range nominal {
		// rnd chosen so the jitter offset is exactly zero: span is 2s+1ns and
		// the offset is rnd%span - 1s, so rnd = 1e9 gives 0.
		got := b.Delay(n, uint64(1*Second))
		if got != want {
			t.Fatalf("Delay(%d) = %s, want %s", n, got, want)
		}
		total += want
	}
	if total != 60*Second {
		t.Fatalf("the four nominal delays total %s, want the RFC's 60s", total)
	}
	if b.MaxRetransmissions != 4 {
		t.Fatalf("MaxRetransmissions = %d, want the RFC's four tries", b.MaxRetransmissions)
	}
	if !b.Exhausted(4) || b.Exhausted(3) {
		t.Fatalf("budget boundary is wrong: Exhausted(3)=%v Exhausted(4)=%v", b.Exhausted(3), b.Exhausted(4))
	}
}

func TestBackoffClampsAtMax(t *testing.T) {
	b := DefaultBackoff()
	for n := 4; n < 200; n++ {
		got := b.Delay(n, uint64(1*Second))
		if got != 64*Second {
			t.Fatalf("Delay(%d) = %s, want the 64s ceiling", n, got)
		}
	}
}

// TestBackoffNeverOverflows is the reason the doubling clamps the SHIFT rather
// than the result. 4s doubled 62 times overflows int64 and comes back
// negative; a negative delay is a timer that fires immediately and a
// retransmission storm that no small-n test can see.
func TestBackoffNeverOverflows(t *testing.T) {
	b := Backoff{Initial: 4 * Second, Max: 0, Jitter: 0}
	for _, n := range []int{62, 63, 64, 1000, 1 << 20} {
		got := b.Delay(n, 0)
		if got < 0 {
			t.Fatalf("Delay(%d) = %s, which is negative", n, got)
		}
	}
}

func TestBackoffNegativeAttemptIsTheFirst(t *testing.T) {
	b := DefaultBackoff()
	if got, want := b.Delay(-5, uint64(1*Second)), b.Delay(0, uint64(1*Second)); got != want {
		t.Fatalf("Delay(-5) = %s, want the first delay %s", got, want)
	}
}

// TestJitterIsUniformOverTheRFCRange drives the whole range rather than
// sampling it: the offsets must cover [-1s, +1s] and must never leave it.
//
// The extremes are what matter. A half-open range that never produces +1s, or
// an off-by-one that produces +1s+1ns, is invisible in an average and is
// exactly what a "randomised" test with a fixed seed misses.
func TestJitterIsUniformOverTheRFCRange(t *testing.T) {
	const half = 1 * Second
	for _, rnd := range []uint64{0, uint64(half), uint64(2 * half), uint64(2*half) + 1} {
		got := jitter(10*Second, half, rnd)
		if got < 10*Second-half || got > 10*Second+half {
			t.Fatalf("jitter(10s, 1s, %d) = %s, outside the RFC's -1..+1 range", rnd, got)
		}
	}
	if got := jitter(10*Second, half, 0); got != 9*Second {
		t.Fatalf("the lowest rnd gives %s, want 9s (-1s)", got)
	}
	if got := jitter(10*Second, half, uint64(2*half)); got != 11*Second {
		t.Fatalf("the highest rnd gives %s, want 11s (+1s)", got)
	}
	// And the wrap: rnd = span produces the lowest offset again, not an
	// out-of-range one.
	if got := jitter(10*Second, half, uint64(2*half)+1); got != 9*Second {
		t.Fatalf("rnd == span gives %s, want the range to wrap to 9s", got)
	}

	// Uniformity, coarsely: the offsets must land across the whole window and
	// not clump. Asserting the extremes alone would pass for an implementation
	// that returned only the two endpoints, and asserting an average would
	// pass for one that returned only the middle.
	const buckets = 8
	var hist [buckets]int
	for i := uint64(0); i < 20000; i++ {
		off := jitter(10*Second, half, split(i, 0)) - 10*Second + half // 0 .. 2*half
		b := int(int64(off) * buckets / int64(2*half+1))
		if b < 0 || b >= buckets {
			t.Fatalf("offset %s fell outside the window", off-half)
		}
		hist[b]++
	}
	for b, n := range hist {
		if n == 0 {
			t.Fatalf("bucket %d of %d never occurred in 20000 samples: %v", b, buckets, hist)
		}
	}
}

func TestJitterNeverNegative(t *testing.T) {
	// A delay shorter than the jitter would otherwise go negative.
	for rnd := uint64(0); rnd < 100; rnd++ {
		if got := jitter(100*Millisecond, 1*Second, rnd); got < 0 {
			t.Fatalf("jitter(100ms, 1s, %d) = %s", rnd, got)
		}
	}
}

func TestJitterOfZeroIsExact(t *testing.T) {
	// The preservation control: with jitter disabled the delay must be the
	// nominal one, not the nominal one plus something small. A test suite that
	// only ever measures a jittered value cannot tell those apart.
	b := Backoff{Initial: 4 * Second, Max: 64 * Second, Jitter: 0}
	for n, want := range []Duration{4 * Second, 8 * Second, 16 * Second, 32 * Second, 64 * Second} {
		for _, rnd := range []uint64{0, 1, 1 << 40, ^uint64(0)} {
			if got := b.Delay(n, rnd); got != want {
				t.Fatalf("Delay(%d, %d) = %s with jitter disabled, want exactly %s", n, rnd, got, want)
			}
		}
	}
}

// TestSplitSeparatesItsOutputs is what lets one journalled rnd drive two
// unrelated random quantities in a single Step. If split(r,0) and split(r,1)
// were correlated, a fresh xid and its retransmission delay would move
// together, which is the shape that makes two clients on one segment collide
// repeatedly rather than once.
func TestSplitSeparatesItsOutputs(t *testing.T) {
	for _, r := range []uint64{0, 1, 42, 1 << 63, ^uint64(0)} {
		a, b, c, d := split(r, 0), split(r, 1), split(r, 2), split(r, 3)
		vals := []uint64{a, b, c, d}
		for i := range vals {
			for j := i + 1; j < len(vals); j++ {
				if vals[i] == vals[j] {
					t.Fatalf("split(%d, %d) == split(%d, %d) == %d", r, i, r, j, vals[i])
				}
			}
			if vals[i] == r {
				t.Fatalf("split(%d, %d) returned its own input", r, i)
			}
		}
	}
}

func TestSplitIsDeterministic(t *testing.T) {
	// Replay depends on this exactly as much as it depends on the journal.
	for i := uint64(0); i < 1000; i++ {
		if split(i, 7) != split(i, 7) {
			t.Fatalf("split is not a function at i=%d", i)
		}
	}
}

func TestDesyncWindowIsWithinTheRFC(t *testing.T) {
	// RFC 2131 section 4.4.1: "The client SHOULD wait a random time between
	// one and ten seconds to desynchronize the use of DHCP at startup."
	p := DefaultParams([]byte{1, 2, 3, 4, 5, 6})
	lo, hi := Duration(1<<62), Duration(0)
	for i := uint64(0); i < 20000; i++ {
		d := p.desync(split(i, 5))
		if d < p.DesyncMin || d > p.DesyncMax {
			t.Fatalf("desync %s outside [%s, %s]", d, p.DesyncMin, p.DesyncMax)
		}
		if d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	// The window must actually be used, not collapsed onto one value: a
	// desync that always returns the same delay desynchronises nothing, and
	// every bound check above would still pass.
	if hi-lo < 8*Second {
		t.Fatalf("desync spread is only %s (%s..%s); the window is not being used", hi-lo, lo, hi)
	}
}

func TestDesyncDisabledIsZero(t *testing.T) {
	p := DefaultParams([]byte{1, 2, 3, 4, 5, 6})
	p.DesyncMin, p.DesyncMax = 0, 0
	for i := uint64(0); i < 100; i++ {
		if d := p.desync(i); d != 0 {
			t.Fatalf("desync with a zero window returned %s", d)
		}
	}
}
