// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag, for
// the reason awaitsettled.go and counterwindow.go give: what a cell
// waits before it calls the plugin wrong has to be checkable without a
// live plugin and without root.

package harness

import (
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// RetransmitBudget is how long a cell may wait for something that only
// exists once the client's exchange has completed, when what the cell
// claims is that the thing appears and not how fast.
//
// losses is how many lost replies the wait absorbs. The client answers
// a lost reply by retransmitting after a delay it takes from RFC 2131
// section 4.1, so a cell that waits less than that delay reds on a
// working plugin as soon as the fixture drops one packet, which it
// does: 4 s +/- 1 s before the first retransmission is longer than the
// 5 s several cells used to allow for everything together.
//
// The schedule is read from the library the plugin runs, not typed in
// here, so a change to it moves this budget with it. Each delay is
// taken at the top of its jitter range, because a budget built on the
// middle is short exactly when it matters. The sizes that follow for
// the default schedule are 5 s, 14 s and 31 s; they are pinned by
// TestRetransmitBudget_IsTheClientsOwnScheduleAtItsSlowest.
//
// More losses than the client will make is not a longer wait: past
// MaxRetransmissions the client abandons the transaction and starts
// over, so waiting further is waiting for an exchange that is no
// longer running. losses is clamped there.
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

// slowestJitter is the entropy value that makes Backoff.Delay return
// the longest delay of its range.
//
// Delay hands rnd to the library's jitter, which computes
// `off = rnd % (2*Jitter+1) - Jitter`. The offset is therefore +Jitter,
// the top of the range, exactly when rnd is 2*Jitter.
func slowestJitter(b proto.Backoff) uint64 {
	if b.Jitter <= 0 {
		return 0
	}
	return uint64(2 * b.Jitter)
}
