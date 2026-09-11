// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// RenewalStats is what a manager has learned, since the last report,
// about renewal requests that went unanswered.
type RenewalStats struct {
	// Unanswered is how many further DHCPREQUESTs sent to extend a held
	// lease were PROVEN to have gone unanswered since the previous
	// report. A request is proven unanswered by the retransmission that
	// follows it or by the acknowledgement that ends its renewal, never
	// by having been sent: a request still in flight is not counted,
	// which is why a client that has sent N renewal requests into
	// silence reports N-1.
	Unanswered uint64
}

// renewalPollInterval is how often the manager's counters are folded
// while the client runs, with no lease event to ride on.
//
// DERIVED from the shortest gap the library can leave between two
// renewal requests. RFC 2131 section 4.4.5 puts a floor of one minute
// under the wait before a renewal is retransmitted, and the library
// spells that floor proto.RenewRetransmitFloor. A quarter of it places
// three folds in the shortest gap the wire can produce, so the reading
// survives two missed ticks, and it follows the library down if that
// floor is ever lowered.
const renewalPollInterval = time.Duration(proto.RenewRetransmitFloor) / 4

// renewalPoll is the interval translate folds the counters on.
func (c *DHCPClient) renewalPoll() time.Duration {
	if c.pollEvery > 0 {
		return c.pollEvery
	}
	return renewalPollInterval
}

// renewalWatch turns the library's two monotonic renewal counters into
// one monotonic count of renewal requests that went unanswered.
//
// THE ARITHMETIC, AND WHY IT IS NOT A SUBTRACTION. RenewalsSent counts
// every DHCPREQUEST sent to extend a held lease, retransmissions
// included; RenewalsCompleted counts the DHCPACKs that ended one. Their
// difference is not the answer: one request is legitimately outstanding
// for as long as the server is being waited on, so a plain
// Sent-minus-Completed moves for a renewal that is answered a
// millisecond later, and the counter then reports an outage on a
// perfectly healthy network. Subtracting the outstanding request gives
// the requests actually proven unanswered, and taking the RUNNING
// MAXIMUM of that is what keeps the result monotonic: the difference
// itself falls when an acknowledgement lands, and a counter that falls
// is a reset to Prometheus.
//
// The maximum is reached at a send -- that is the only moment the count
// of proven-unanswered requests rises -- and a fold that lands between
// that send and the acknowledgement ending it reads the peak. A fold
// that misses the peak reads one low until the next retransmission, and
// that is the direction the error is wanted in: the counter may lag the
// wire, and it may never claim a request the server answered.
type renewalWatch struct {
	// max is the highest proven-unanswered count seen so far, and
	// reported is how much of it the caller has already been told
	// about. Both are touched from the translate goroutine only.
	max      uint64
	reported uint64
}

// fold reads one counter snapshot and returns what the caller has not
// been told about yet.
func (w *renewalWatch) fold(s lease.Stats) uint64 {
	if s.RenewalsSent > s.RenewalsCompleted+1 {
		if proven := s.RenewalsSent - s.RenewalsCompleted - 1; proven > w.max {
			w.max = proven
		}
	}
	delta := w.max - w.reported
	w.reported = w.max
	return delta
}

// report folds one snapshot and hands the gain to the caller, if there
// is one and there is a gain.
func (w *renewalWatch) report(s lease.Stats, cb func(RenewalStats)) {
	if cb == nil {
		return
	}
	if d := w.fold(s); d > 0 {
		cb(RenewalStats{Unanswered: d})
	}
}
