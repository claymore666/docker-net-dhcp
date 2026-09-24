// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// RenewalStats is what a manager learned since the last report about renewal requests that went unanswered.
type RenewalStats struct {
	// Unanswered is the renewal DHCPREQUESTs proven unanswered by a following retransmission; N requests into silence
	// read N-1 (#940).
	Unanswered uint64
}

// RFC 2131 section 4.4.5 puts a one-minute floor under a renewal retransmission (proto.RenewRetransmitFloor); a quarter
// of it folds three times in the shortest gap and follows the library if the floor drops (#940).
const renewalPollInterval = time.Duration(proto.RenewRetransmitFloor) / 4

func (c *DHCPClient) renewalPoll() time.Duration {
	if c.pollEvery > 0 {
		return c.pollEvery
	}
	return renewalPollInterval
}

// One renewal request is in flight at a time, so a request is proven unanswered only by the next one leaving the host.
// The end of a cycle is read off the lease events, not RenewalsCompleted: a DHCPNAK in RENEWING or REBINDING (proto
// ReasonNak) or a v6 NotOnLink Reply ends the lease without enterBound, so Sent - Completed - 1 would count an answered
// request (#940). Every error is low: the request in flight is never counted.

// renewalWatch turns the library's renewal counters into one monotonic count of renewal requests that went unanswered.
type renewalWatch struct {
	// base is RenewalsSent at the cycle's start, counted what it already reported; translate goroutine only.
	base    uint64
	counted uint64
}

// fold reads one counter snapshot and returns what the caller has not been told yet.
func (w *renewalWatch) fold(s lease.Stats) uint64 {
	// A counter that went backwards is a replaced manager: start the cycle again there (#940).
	if s.RenewalsSent < w.base {
		w.base, w.counted = s.RenewalsSent, 0
		return 0
	}
	var proven uint64
	if s.RenewalsSent > w.base+1 {
		proven = s.RenewalsSent - w.base - 1
	}
	if proven <= w.counted {
		return 0
	}
	gain := proven - w.counted
	w.counted = proven
	return gain
}

// cycleEnded starts the next renewal cycle from the requests sent so far; call it after folding the same snapshot.
func (w *renewalWatch) cycleEnded(s lease.Stats) {
	if s.RenewalsSent > w.base {
		w.base = s.RenewalsSent
	}
	w.counted = 0
}

// report folds one snapshot and hands a non-zero gain to cb, if cb is set.
func (w *renewalWatch) report(s lease.Stats, cb func(RenewalStats)) {
	if cb == nil {
		return
	}
	if d := w.fold(s); d > 0 {
		cb(RenewalStats{Unanswered: d})
	}
}
