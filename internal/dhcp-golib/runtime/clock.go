package runtime

import (
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// Clock is the two-clock implementation of the design document's section 8.2.
//
// Mono is CLOCK_BOOTTIME, not CLOCK_MONOTONIC, and that is why this type
// exists rather than a wrapper around time.Now: CLOCK_MONOTONIC does not
// advance across a host suspend. A host that suspends for an hour with
// CLOCK_MONOTONIC driving the lease timers wakes believing an hour of lease
// time is unspent and holds an address the server has already reissued.
//
// Wall is used for one thing: absolute times reported outward and (from the
// next milestone) persisted. A monotonic reading means nothing to the next
// process, so an expiry that must survive a restart can only be stored as
// wall-clock absolute.
type Clock struct{}

// Mono returns a reading of CLOCK_BOOTTIME.
func (Clock) Mono() proto.Instant { return monoNow() }

func (Clock) Wall() time.Time { return time.Now() }

// FixedClock is a Clock a test drives by hand. It is here rather than in a
// _test.go file because ring-3 tests in other packages need it too, and
// because a test-only build tag is the shape that lets a fake leak into
// production code without anything noticing.
type FixedClock struct {
	MonoAt proto.Instant
	WallAt time.Time
}

// Mono returns MonoAt.
func (c *FixedClock) Mono() proto.Instant { return c.MonoAt }

// Wall returns WallAt.
func (c *FixedClock) Wall() time.Time { return c.WallAt }

// Advance moves both clocks forward by the same amount.
func (c *FixedClock) Advance(d proto.Duration) {
	c.MonoAt = c.MonoAt.Add(d)
	c.WallAt = c.WallAt.Add(time.Duration(d))
}
