package lease

import (
	"github.com/claymore666/dhcp-golib/proto"
)

// RateLimit is RFC 9915 section 14.1's obligation, and it lives in ring 2
// because it is about the LINK rather than about any one exchange.
//
// Section 14.1, in full where it is normative: "A DHCPv6 client MUST limit the
// rate of DHCP messages it transmits or retransmits. This will minimize the
// impact of prolonged message bursts or loops, for example when a client
// rejects a server's response, repeats the request and gets the same server
// response, which, again, gets rejected by the client." It names the mechanism
// — "A recommended method for implementing the rate-limiting function is a
// token bucket (see Appendix A of [RFC3290]), limiting the average rate of
// transmission to a certain number in a certain time interval" — the default —
// "A transmission rate limit SHOULD be configurable. A possible default could
// be 20 messages in 20 seconds" — and the scope: "For a device that has
// multiple interfaces, the limit MUST be enforced on a per-interface basis."
//
// WHY IT IS NOT IN RING 1. Section 15's retransmission schedule already bounds
// each exchange, and section 14.1's loop is the one that schedule cannot see:
// a client that keeps ACQUIRING and rejecting starts a NEW exchange each time,
// with a fresh transaction id and a fresh RT, so every individual exchange is
// conformant and the interface is still flooded. What has to be bounded is the
// stream of messages leaving one interface, which is exactly what ring 2 owns
// and ring 1 — a pure function with no notion of a socket — does not.
//
// THE BOUND IS ONE MANAGER INSTANCE, WHICH IS ONE ENDPOINT LINK. Section
// 14.1's per-interface MUST is satisfied by that identity and only by it: two
// Managers built on two TransportV6s that happen to share a host interface
// have two buckets and would together exceed the limit. This library has no
// way to detect that — a Transport is an interface and the socket behind it is
// ring 3's — so the constraint is stated here rather than enforced: one v6
// Manager per interface.
//
// THE v4 PATH IS UNTOUCHED. RFC 2131 has no counterpart to section 14.1, and
// adding one would change the behaviour of a shipping client to satisfy a rule
// that does not apply to it.
type RateLimit struct {
	// Messages is the bucket's capacity and the number of messages allowed
	// per Interval. Zero means the default, 20.
	Messages int
	// Interval is the window Messages is measured over. Zero means the
	// default, 20 seconds.
	Interval proto.Duration
}

// DefaultRateLimit is section 14.1's "possible default": 20 messages in 20
// seconds.
func DefaultRateLimit() RateLimit {
	return RateLimit{Messages: 20, Interval: 20 * proto.Second}
}

// resolve fills in the defaults for a zero field, and reports whether the
// limit is disabled.
//
// A NEGATIVE Messages DISABLES THE BUCKET, and it is the only way to: the zero
// value has to be "the default" so that a caller who never heard of this field
// gets the MUST honoured, which leaves no spelling for "off" among the
// non-negative numbers. Turning it off is a deliberate act with a deliberate
// spelling, and the manager journals that it happened.
func (r RateLimit) resolve() (RateLimit, bool) {
	if r.Messages < 0 {
		return r, false
	}
	if r.Messages == 0 {
		r.Messages = 20
	}
	if r.Interval <= 0 {
		r.Interval = 20 * proto.Second
	}
	return r, true
}

// bucket is the token bucket itself, on the MONOTONIC clock.
//
// Tokens are held as a scaled integer rather than a float: the fill rate is
// Messages per Interval and a float accumulator would make the bucket's
// behaviour depend on how often it happened to be asked. The scale is the
// interval itself, so one token costs Interval and the bucket holds
// Messages*Interval at most — all integer arithmetic on proto.Duration, which
// is what ring 1 measures time in.
type bucket struct {
	limit RateLimit
	on    bool

	// level is the scaled token count, in units of Interval per Messages.
	level proto.Duration
	last  proto.Instant
	// started is false until the first take, so a bucket does not have to be
	// constructed with a clock reading.
	started bool

	// refused counts every message this bucket has turned away, for the
	// journal line and for Stats.
	refused uint64
}

func newBucket(r RateLimit) *bucket {
	limit, on := r.resolve()
	return &bucket{limit: limit, on: on}
}

// take reports whether one message may be sent now, spending a token if so.
//
// THE BUCKET STARTS FULL. A client's first act is a Solicit and section 14.1's
// concern is "prolonged message bursts or loops", not the first packet: a
// bucket that started empty would delay every acquisition on the network by
// one refill interval to protect against a loop that had not happened yet.
func (b *bucket) take(now proto.Instant) bool {
	if !b.on {
		return true
	}
	full := proto.Duration(b.limit.Messages) * b.limit.Interval
	if !b.started {
		b.level, b.last, b.started = full, now, true
	} else {
		// Refill at Messages tokens per Interval, which in this scale is one
		// unit of level per unit of elapsed time, times Messages.
		if d := now.Sub(b.last); d > 0 {
			b.level += d * proto.Duration(b.limit.Messages)
			if b.level > full {
				b.level = full
			}
		}
		b.last = now
	}
	if b.level < b.limit.Interval {
		b.refused++
		return false
	}
	b.level -= b.limit.Interval
	return true
}

// Refused is how many messages this bucket has turned away.
func (b *bucket) Refused() uint64 { return b.refused }
