// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: a cell's wait must be checkable without a live plugin or root.

package harness

import (
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// RetransmitBudget is how long a cell may wait for something that exists only once the client's exchange completes.
// The client retransmits a lost reply after an RFC 2131 section 4.1 delay (4 s +/- 1 s first), read here from the
// library's own schedule and taken at the top of each jitter range. Past MaxRetransmissions the client restarts the
// transaction, so losses is clamped there (#1047).
func RetransmitBudget(losses int) time.Duration {
	b := proto.DefaultBackoff()
	if losses < 0 {
		losses = 0
	}
	if b.MaxRetransmissions > 0 && losses > b.MaxRetransmissions {
		losses = b.MaxRetransmissions
	}
	var total proto.Duration
	for i := 0; i < losses; i++ {
		total += b.Delay(i, slowestJitter(b))
	}
	return time.Duration(total)
}

// slowestJitter is the rnd that makes Backoff.Delay return +Jitter: the library computes off = rnd % (2*Jitter+1) - Jitter.
func slowestJitter(b proto.Backoff) uint64 {
	if b.Jitter <= 0 {
		return 0
	}
	return uint64(2 * b.Jitter)
}
