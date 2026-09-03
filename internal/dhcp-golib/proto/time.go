package proto

import (
	"fmt"
	"strconv"
)

// Instant is a reading of a monotonic clock, in nanoseconds, on an epoch this
// package deliberately does not name.
//
// Ring 1 cannot import "time" — the T1 gate refuses it. Time is a PARAMETER of
// Step, so the machine cannot read a clock even if it wanted one and every
// deadline it computes is a function of the Instant it was handed, which is
// what makes a replay bit-exact.
//
// Ring 3 supplies these from CLOCK_BOOTTIME (see runtime.Clock). The machine
// requires only that successive Instants do not go backwards.
type Instant int64

// Duration is a length of time in nanoseconds.
type Duration int64

// Durations, spelled out because ring 1 cannot import "time" and therefore
// cannot say time.Second.
const (
	Nanosecond  Duration = 1
	Microsecond          = 1000 * Nanosecond
	Millisecond          = 1000 * Microsecond
	Second               = 1000 * Millisecond
	Minute               = 60 * Second
	Hour                 = 60 * Minute
)

// Add returns t advanced by d. A negative d moves backwards.
func (t Instant) Add(d Duration) Instant { return t + Instant(d) }

// Sub returns the interval from u to t.
func (t Instant) Sub(u Instant) Duration { return Duration(t - u) }

// Before reports whether t is strictly earlier than u.
func (t Instant) Before(u Instant) bool { return t < u }

// After reports whether t is strictly later than u.
func (t Instant) After(u Instant) bool { return t > u }

// Seconds returns d in whole seconds, truncating.
func (d Duration) Seconds() int64 { return int64(d / Second) }

// String renders a Duration the way time.Duration does for the units this
// library actually uses, without importing time.
func (d Duration) String() string {
	switch {
	case d == 0:
		return "0s"
	case d%Second == 0:
		return strconv.FormatInt(int64(d/Second), 10) + "s"
	case d%Millisecond == 0:
		return strconv.FormatInt(int64(d/Millisecond), 10) + "ms"
	default:
		return strconv.FormatInt(int64(d), 10) + "ns"
	}
}

func (t Instant) String() string { return fmt.Sprintf("t+%s", Duration(t)) }

// SecondsToDuration converts an RFC 2131 lease/T1/T2 value, which is carried on
// the wire as an unsigned 32-bit count of seconds.
//
// 0xFFFFFFFF is the protocol's "infinite" (RFC 2132 section 3.3 defines the
// lease time as a 32-bit value and 0xFFFFFFFF is the conventional infinite
// lease). It is mapped to Infinite rather than to 136 years of nanoseconds,
// which would overflow nothing but would make every deadline comparison depend
// on an arbitrary large number.
func SecondsToDuration(secs uint32) Duration {
	if secs == InfiniteSeconds {
		return Infinite
	}
	return Duration(secs) * Second
}

// InfiniteSeconds is the wire value for an infinite lease.
const InfiniteSeconds uint32 = 0xFFFFFFFF

// Infinite is the Duration an infinite lease maps to. It is a specific,
// comparable value rather than "very large": IsInfinite tests for it exactly,
// so an infinite lease never produces an expiry timer.
const Infinite Duration = -1

// IsInfinite reports whether d is the infinite-lease sentinel.
func (d Duration) IsInfinite() bool { return d == Infinite }
