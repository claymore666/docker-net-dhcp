// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"math"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// TestRetransmitBudget_IsTheClientsOwnScheduleAtItsSlowest pins the
// sizes, so a schedule change in the library fails here, in the local
// lane, with the numbers in the message, instead of reddening a pool
// cell that looks like a flake.
//
// RFC 2131 section 4.1's example schedule is 4 s, then 8 s, then 16 s,
// each randomised by +/- 1 s, and it stops doubling at 64 s. The
// slowest run of the first n retransmissions is the running sum of
// those plus one second each: 5 s, 14 s, 31 s, 64 s.
func TestRetransmitBudget_IsTheClientsOwnScheduleAtItsSlowest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		losses int
		want   time.Duration
	}{
		{"a negative count is no wait at all", -1, 0},
		{"no losses is no wait", 0, 0},
		{"one lost reply", 1, 5 * time.Second},
		{"two lost replies", 2, 14 * time.Second},
		{"three lost replies", 3, 31 * time.Second},
		{"the whole budget the client gives itself", 4, 64 * time.Second},
		{"past the point the client gives up, clamped", 9, 64 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetransmitBudget(tc.losses); got != tc.want {
				t.Errorf("RetransmitBudget(%d) = %v, want %v", tc.losses, got, tc.want)
			}
		})
	}
}

// TestRetransmitBudget_NoDelayTheClientCanDrawIsLonger is the claim the
// budget rests on: the entropy value it builds each delay from really
// is the slowest of that delay's range. A budget built on the middle of
// the range would be a second short per retransmission, which is the
// same defect one size up and would look like a flake.
func TestRetransmitBudget_NoDelayTheClientCanDrawIsLonger(t *testing.T) {
	b := proto.DefaultBackoff()
	slowest := slowestJitter(b)

	rnds := []uint64{0, 1, 2, uint64(b.Jitter) - 1, uint64(b.Jitter), uint64(b.Jitter) + 1,
		slowest - 1, slowest, slowest + 1, 1 << 32, math.MaxUint64 / 3, math.MaxUint64}
	for i := 0; i <= b.MaxRetransmissions; i++ {
		ceiling := b.Delay(i, slowest)
		for _, rnd := range rnds {
			if got := b.Delay(i, rnd); got > ceiling {
				t.Errorf("Delay(%d, %d) = %v, longer than the %v the budget allows for it",
					i, rnd, time.Duration(got), time.Duration(ceiling))
			}
		}
	}
}
