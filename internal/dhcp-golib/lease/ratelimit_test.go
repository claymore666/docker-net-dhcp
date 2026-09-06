package lease

import (
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// TestTheDefaultIsTwentyInTwenty is RFC 9915 section 14.1's "A possible default
// could be 20 messages in 20 seconds", asserted on the CONSTRUCTED bucket
// rather than on DefaultRateLimit's return value.
//
// The distinction is the whole test: a caller who never heard of RateLimit
// leaves the field zero, and the MUST is honoured only if the zero value
// resolves to the default. Reading DefaultRateLimit() would assert that a
// function returns what it says and leave the path every caller takes unchecked.
func TestTheDefaultIsTwentyInTwenty(t *testing.T) {
	if got := DefaultRateLimit(); got.Messages != 20 || got.Interval != 20*proto.Second {
		t.Fatalf("DefaultRateLimit() = %+v, want 20 messages per 20s", got)
	}

	var zero RateLimit
	b := newBucket(zero)
	if !b.on {
		t.Fatal("the zero RateLimit disabled the bucket; section 14.1 is a MUST and the zero value is what an unaware caller passes")
	}
	if b.limit != DefaultRateLimit() {
		t.Errorf("the zero RateLimit resolved to %+v, want %+v", b.limit, DefaultRateLimit())
	}

	// A zero Interval beside a set Messages is the half-filled case, and it
	// takes the default for the field the caller did not set rather than a
	// zero window, which would divide by an interval of nothing.
	half := newBucket(RateLimit{Messages: 5})
	if half.limit.Messages != 5 || half.limit.Interval != 20*proto.Second {
		t.Errorf("RateLimit{Messages: 5} resolved to %+v, want 5 per 20s", half.limit)
	}
}

// TestTheBucketSpendsExactlyItsCapacityBeforeRefusing is the off-by-one, in
// both directions: the Nth message goes and the N+1st does not.
//
// THE CLOCK NEVER MOVES IN THE FIRST LOOP, so nothing refills and the count is
// the capacity and not the capacity plus whatever the test took to run.
func TestTheBucketSpendsExactlyItsCapacityBeforeRefusing(t *testing.T) {
	for _, n := range []int{1, 2, 20} {
		clk := newFakeClock()
		b := newBucket(RateLimit{Messages: n, Interval: 20 * proto.Second})
		for i := 0; i < n; i++ {
			if !b.take(clk.Mono()) {
				t.Fatalf("capacity %d: message %d was refused; the bucket starts full", n, i+1)
			}
		}
		if b.take(clk.Mono()) {
			t.Errorf("capacity %d: message %d was allowed", n, n+1)
		}
		if got := b.Refused(); got != 1 {
			t.Errorf("capacity %d: Refused() = %d, want 1", n, got)
		}
	}
}

// TestTheBucketRefillsAtItsOwnRate drives the refill arithmetic on the
// monotonic clock, one row per fraction of the interval.
//
// It is the property that makes this a token bucket rather than a fixed window:
// a client refused at t is allowed again after one interval divided by the
// message count, and not only when the whole window has rolled over.
func TestTheBucketRefillsAtItsOwnRate(t *testing.T) {
	const interval = 20 * proto.Second
	for _, tc := range []struct {
		name  string
		wait  proto.Duration
		allow int
	}{
		{"nothing elapsed", 0, 0},
		{"a fifth of one token", interval / 20 / 5, 0},
		{"exactly one token", interval / 20, 1},
		{"two tokens", 2 * interval / 20, 2},
		{"a whole interval refills the bucket and no more", interval, 20},
		{"a day does not overfill it", 24 * 3600 * proto.Second, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newFakeClock()
			b := newBucket(RateLimit{Messages: 20, Interval: interval})
			for i := 0; i < 20; i++ {
				b.take(clk.Mono())
			}
			if b.take(clk.Mono()) {
				t.Fatal("the bucket was not empty before the wait")
			}

			clk.advance(tc.wait)
			got := 0
			for b.take(clk.Mono()) {
				got++
				if got > 25 {
					t.Fatal("the bucket never emptied; it is refilling faster than it is spent")
				}
			}
			if got != tc.allow {
				t.Errorf("after %s the bucket allowed %d message(s), want %d", tc.wait, got, tc.allow)
			}
		})
	}
}

// TestANegativeMessageCountTurnsTheBucketOff is the documented escape, and the
// only spelling of it.
func TestANegativeMessageCountTurnsTheBucketOff(t *testing.T) {
	clk := newFakeClock()
	b := newBucket(RateLimit{Messages: -1})
	if b.on {
		t.Fatal("Messages: -1 left the bucket on")
	}
	for i := 0; i < 1000; i++ {
		if !b.take(clk.Mono()) {
			t.Fatalf("a disabled bucket refused message %d", i+1)
		}
	}
	if got := b.Refused(); got != 0 {
		t.Errorf("Refused() = %d on a disabled bucket, want 0", got)
	}
}

// TestARefusedSendIsJournalledAndCounted is section 14.1 through the manager,
// and it is the arm that separates this from a silent drop.
//
// THE ROUTER SOLICITATION IS THE CONTROL. begin() emits one before the Solicit,
// so a bucket of one token that covered ICMPv6 would have spent it there and
// the Solicit below would never have reached the server — which is exactly what
// this asserts did not happen.
func TestARefusedSendIsJournalledAndCounted(t *testing.T) {
	r := newRig6(t, testParams6(), silent6, withRateLimit(RateLimit{Messages: 1, Interval: 20 * proto.Second}))

	// The retransmission the machine owes section 15 is the second message,
	// and a bucket of one has nothing left for it. Firing the timer here is
	// ordered behind the pre-transmission delay the rig already fired, so the
	// Solicit is always the first message and the retransmission the second.
	//
	// THE REFUSAL NOTE IS THE BARRIER AND THE SOLICIT IS NOT, deliberately: a
	// bucket that also covered the Router Solicitation would refuse the
	// SOLICIT, and a test that waited for the Solicit to reach the server
	// would hang there rather than fail. Waiting for the note — which arrives
	// either way — and then reading what the server saw turns that mutant
	// into a failure with a number in it.
	//
	// AND THE BARRIER IS A SECOND TIMER BEHIND IT, not the note. Waiting for
	// the note makes every mutant that removes the note a HANG — mutate.sh's
	// own verdict, and explicitly not a kill. MEASURED, twice: a bucket whose
	// take() always succeeds, and a refusal reported to the machine as a
	// success, were both scored HUNG until this barrier changed. Manager
	// dispatch takes one event at a time and drains that event's whole
	// cascade before the next, so the ignored timer's journal entry appearing
	// is the retransmission having already been refused or sent — whichever
	// it was — and every assertion below is then a read, not a race.
	r.timers.fire(proto.Timer6Retransmit)
	r.settle(t)

	if len(r.nd.sentPackets()) == 0 {
		t.Fatal("no Router Solicitation went out")
	}
	sent := r.server.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("the server saw %d message(s), want exactly the Solicit: the one token belongs to the DHCP exchange and not to RFC 4861's Router Solicitation", len(sent))
	}
	if sent[0].Type != wire.MsgSolicit {
		t.Fatalf("the one message that got a token was %s, want SOLICIT", sent[0].Type)
	}

	if !journalHas6(r, "rate limit") {
		t.Fatal("the bucket refused a message and nothing in the journal says so: §14.1's refusal is journalled with the count, not dropped silently")
	}
	line := findEntry6(t, r, "the rate-limit refusal", func(e proto.JournalEntry6) bool {
		return strings.Contains(e.Reason, "rate limit")
	}).Reason
	for _, want := range []string{"§14.1", "SOLICIT", "1 refused", "limit 1 per 20s"} {
		if !strings.Contains(line, want) {
			t.Errorf("the journal line %q does not name %q", line, want)
		}
	}

	if got := r.mgr.Stats().RateLimited; got != 1 {
		t.Errorf("Stats().RateLimited = %d, want 1", got)
	}
}

// TestABucketTurnedOffSaysSoOnce is the other half of the escape: a caller who
// disabled a MUST gets that recorded where the exchange is recorded.
func TestABucketTurnedOffSaysSoOnce(t *testing.T) {
	r := newRig6(t, testParams6(), silent6, withRateLimit(RateLimit{Messages: -1}))
	r.waitSent(t, wire.MsgSolicit)

	if !journalHas6(r, "rate limit is disabled") {
		t.Error("nothing in the journal records that RFC 9915 §14.1's limit was turned off")
	}
	n := 0
	for _, e := range r.mgr.Journal6() {
		if strings.Contains(e.Reason, "rate limit is disabled") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the disabled-limit note appears %d times, want once: it is a fact about the Config and not about any message", n)
	}
}
