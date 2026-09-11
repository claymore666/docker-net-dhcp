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
	// follows it, and by nothing else: not by having been sent, and not
	// by the acknowledgement that ends the renewal, which proves the
	// opposite about the request in flight. That request is not
	// counted, which is why a client that has sent N renewal requests
	// into silence reports N-1.
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

// renewalWatch turns the library's renewal counters into one monotonic
// count of renewal requests that went unanswered.
//
// WHAT PROVES A REQUEST UNANSWERED. Exactly one renewal request is in
// flight at a time: the library sends one and arms a retransmission
// timer. So a request is proven unanswered by the NEXT request leaving
// the host, and by nothing else. The count owed within a renewal cycle
// is therefore the number of requests it has sent, less the one still
// waiting for an answer, and base is where the cycle started.
//
// WHY THE CYCLE'S END IS TAKEN FROM THE EVENT STREAM AND NOT FROM
// RenewalsCompleted. The obvious arithmetic is
// RenewalsSent - RenewalsCompleted - 1, and it is wrong, because a
// renewal cycle the server ANSWERS can end without RenewalsCompleted
// moving. A DHCPNAK in RENEWING or REBINDING drops the lease with
// ReasonNak (proto/machine.go:603-617) and never reaches enterBound,
// which is the only producer of ActLeaseRenewed and so the only thing
// that bumps RenewalsCompleted (lease/manager.go:1065); its v6 twin is
// a Reply carrying NotOnLink, which ends the lease the same way
// (proto/machine6.go:1283-1286). The request was already counted sent.
// After either, Sent stays permanently one ahead of Completed, and the
// subtraction then reports one unanswered renewal at the instant the
// NEXT request leaves the host -- while that request is still in
// flight, on a network where the server answered every single one.
// That is the one direction this counter may never err in.
//
// Every one of those endings reaches the chassis as a lease event:
// ActLeaseLost emits Lost, ActLeaseRenewed emits Renewed
// (lease/manager.go:1067, :1092). So the end of a cycle is read off the
// event stream, where "the caller was told something happened to this
// lease" is the property, rather than off a list of the library's
// internal paths, which is the thing that was wrong. An event that did
// not in fact end a renewal cycle costs at most the requests proven
// unanswered before it, so an event kind this loop has not thought
// about makes the counter read LOW.
//
// The residual error is one request in every direction: the request in
// flight is never counted, so a client that has sent N requests into
// silence reports N-1, and a fold that lands between a send and the
// answer that ends it is the only one that sees the peak. Both err low,
// and low is the direction wanted: the counter may lag the wire, and it
// may never claim a request the server answered.
type renewalWatch struct {
	// base is RenewalsSent as it stood when the current renewal cycle
	// began, and counted is how many of that cycle's requests the
	// caller has already been told went unanswered. Both are touched
	// from the translate goroutine only.
	base    uint64
	counted uint64
}

// fold reads one counter snapshot and returns what the caller has not
// been told about yet.
func (w *renewalWatch) fold(s lease.Stats) uint64 {
	// A counter that went backwards is a manager that was replaced
	// under this watch, not a renewal: start the cycle again there.
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

// cycleEnded is called when a lease event arrives. Whatever the event
// says, the request that was in flight is no longer waiting for an
// answer, so the next renewal cycle starts from the requests sent so
// far and owes nothing.
//
// Call it AFTER folding the same snapshot, or the requests that cycle
// proved unanswered are forgotten instead of reported.
func (w *renewalWatch) cycleEnded(s lease.Stats) {
	if s.RenewalsSent > w.base {
		w.base = s.RenewalsSent
	}
	w.counted = 0
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
