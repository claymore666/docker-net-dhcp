// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"testing"
	"time"
)

// An observation that has already been made must not be discarded
// because the deadline that produced it has expired.
//
// This drives the composition attemptGetIP actually performs
// (client.go:994-999 then :1002/:1006): the real collector goroutine
// folding real events into the real accumulator, read back through the
// real settle. The observation under test is therefore BUILT by the
// production code rather than asserted into existence, which is what
// separates this from TestSettleAcquisition_* above — those fold by
// hand and so cannot see a defect that lives in the handover.
//
// It matters because of who reads the result. classifyV6Absence
// (pkg/plugin/v6_absence.go) maps the ZERO observation to v6NoRouter,
// which noteV6Absence TOLERATES: the endpoint is created without a v6
// address. So a discarded observation is not a lost log line, it is a
// managed-DHCPv6 outage silently reclassified as "no router here" and
// waved through. That is the failure
// TestDHCPv6_Managed_ServerSilent_IsStillFatal reports.
//
// On the managed path the acquisition context is ALWAYS already expired
// when this runs. GetIP's early exit fires only for
// `ra.Seen && !ra.Managed` (client.go:1092), so a managed segment
// deliberately retries until the acquisition budget is gone; the
// attempt that ends the loop ends it by deadline. Both arms below
// therefore run with an expired context in hand, exactly as production
// does. That the context cannot reach the decision at all is the
// point, and is pinned separately and structurally by
// TestSettleAcquisition_TakesNoContext.
//
// Measured against the pre-fix shape (the ctx-vs-raCh select this
// replaced): arm one failed 101/200, arm two 200/200.
func TestSettleAcquisition_KeepsAnObservationTakenUnderAnExpiredDeadline(t *testing.T) {
	// The wire input, identical in both arms: one router advertisement
	// carrying the managed-address flag.
	const advertisement = "MO"

	// start reproduces attemptGetIP's collector goroutine exactly
	// (client.go:994-999).
	start := func(events <-chan Event) (*acquisition, chan struct{}) {
		acq := &acquisition{}
		collected := make(chan struct{})
		go func() {
			defer close(collected)
			collectAcquisition(events, acq)
		}()
		return acq, collected
	}

	// expired is the acquisition context as this code always finds it.
	expired := func(t *testing.T) context.Context {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		<-ctx.Done()
		return ctx
	}

	t.Run("the stream closed and the collector finished", func(t *testing.T) {
		events := make(chan Event, 1)
		events <- Event{Type: "routeradvert", RouterFlags: advertisement}
		close(events)

		acq, collected := start(events)
		<-collected // the collector has demonstrably finished

		ctx := expired(t)
		if ctx.Err() == nil {
			t.Fatal("the context is not expired; this arm is not testing what it claims")
		}

		last, ra := settleAcquisition(collected, acq, raDrainGrace)
		if !ra.Seen || !ra.Managed {
			t.Errorf("observation = %+v, want Seen and Managed. The segment advertised "+
				"the managed flag and the collector had already taken it; discarding it "+
				"makes classifyV6Absence read v6NoRouter and tolerate a real DHCPv6 "+
				"outage (#873)", ra)
		}
		if last != nil {
			t.Errorf("lease = %+v, want none — an advertisement is not a lease", *last)
		}
	})

	t.Run("the collector has not folded yet and the wait is what catches it", func(t *testing.T) {
		// The grace is not only a bound, it is a WAIT: its job is to
		// give the collector the chance to hand over events already in
		// the pipe. The two arms below cannot see that job being done
		// — each of them has the fold already complete before settle is
		// called — so a settle that dropped the select entirely and
		// snapshotted immediately would pass both.
		//
		// Here the fold happens only once the collector is scheduled,
		// which is the state Finish leaves us in: the reaper has closed
		// waitDone (client.go:733) and the scanner has not yet reached
		// EOF (:698).
		//
		// This is a one-directional detector and deliberately so.
		// Correct code ALWAYS passes: collected closes only after
		// collectAcquisition has returned, so every event is folded
		// before the select can release. It is only the code that
		// declines to wait that can be caught here, and it is caught
		// whenever the collector has not run yet.
		events := make(chan Event, 1)
		events <- Event{Type: "routeradvert", RouterFlags: advertisement}
		close(events)

		acq, collected := start(events)
		// Deliberately NOT waiting for collected here.

		last, ra := settleAcquisition(collected, acq, raDrainGrace)
		if !ra.Seen || !ra.Managed {
			t.Errorf("observation = %+v, want Seen and Managed. The advertisement was "+
				"already in the stream and the collector had not yet taken it; the "+
				"grace exists to cover exactly that handover, and skipping it reports "+
				"a segment that advertised as one that stayed silent (#873)", ra)
		}
		if last != nil {
			t.Errorf("lease = %+v, want none — an advertisement is not a lease", *last)
		}
	})

	t.Run("the stream has not closed and the collector is still running", func(t *testing.T) {
		// The production shape, and the one the old code could not
		// answer at all. Finish returning does NOT mean the collector
		// has finished: await returns on waitDone (client.go:791), and
		// the reaper closes waitDone (:733) right after closing the
		// FIFO keep-alive writer (:731), while the scanner closes the
		// events channel (:698) only once it reaches EOF. This arm
		// pins that window open and never lets it shut.
		//
		// The handshake is deterministic, not timed. events is
		// UNBUFFERED, so the second send completes only after the
		// collector has received the first and come back round the
		// loop — which it can only do after folding it. No sleep, no
		// poll, no retry.
		events := make(chan Event)
		acq, collected := start(events)

		events <- Event{Type: "routeradvert", RouterFlags: advertisement}
		events <- Event{Type: "config"} // returns only once the advert is folded

		select {
		case <-collected:
			t.Fatal("the collector finished; this arm must hold the stream open, " +
				"otherwise it silently degrades into the arm above")
		default:
		}

		ctx := expired(t)
		if ctx.Err() == nil {
			t.Fatal("the context is not expired; this arm is not testing what it claims")
		}

		start := time.Now()
		last, ra := settleAcquisition(collected, acq, raDrainGrace)
		elapsed := time.Since(start)

		if !ra.Seen || !ra.Managed {
			t.Errorf("observation = %+v, want Seen and Managed. The advertisement was on "+
				"the wire and the collector had folded it; a stream that has not ended "+
				"is not evidence the segment stayed silent, and the expired deadline is "+
				"what we were retrying against (#873)", ra)
		}
		if last != nil {
			t.Errorf("lease = %+v, want none — an advertisement is not a lease", *last)
		}

		// BOUNDED. A bare receive here would hang forever on a FIFO
		// held open by a hook process that outlived its reaped parent
		// (client.go:539-544 — the hooks open it by path, so our
		// close-on-exec write end is not the only one). Hanging
		// CreateEndpoint is worse than the bug this fixes.
		if elapsed > 30*time.Second {
			t.Errorf("settleAcquisition took %v against a stream that never ends; it "+
				"must be bounded by the grace, not by the collector", elapsed)
		}
	})
}
