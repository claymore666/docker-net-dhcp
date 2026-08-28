// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// An observation that has already been made must not be discarded
// because the deadline that produced it has expired.
//
// This drives the composition attemptGetIP actually performs -- its
// collector goroutine, then finishAcquisition: the real collector
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
// `ra.Seen && !ra.Managed`, so a managed segment
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

	// start reproduces attemptGetIP's collector goroutine exactly.
	//
	// Reproducing rather than executing is the whole reason
	// TestAttemptGetIP_TheDeadlineOnlyBoundsFinish exists: a defect in
	// the real composition is invisible from here.
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
				"outage (#868)", ra)
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
		// waitDone and the scanner has not yet reached EOF.
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
				"a segment that advertised as one that stayed silent (#868)", ra)
		}
		if last != nil {
			t.Errorf("lease = %+v, want none — an advertisement is not a lease", *last)
		}
	})

	t.Run("the stream has not closed and the collector is still running", func(t *testing.T) {
		// The production shape, and the one the old code could not
		// answer at all. Finish returning does NOT mean the collector
		// has finished: await returns on waitDone, and the reaper
		// closes waitDone right after closing the FIFO keep-alive
		// writer, while the scanner closes the events channel only
		// once it reaches EOF. This arm
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
				"what we were retrying against (#868)", ra)
		}
		if last != nil {
			t.Errorf("lease = %+v, want none — an advertisement is not a lease", *last)
		}

		// BOUNDED. A bare receive here would hang forever on a FIFO
		// held open by a hook process that outlived its reaped parent
		// (dhcpcd's hook processes open the FIFO by path, so our
		// close-on-exec write end is not the only one). Hanging
		// CreateEndpoint is worse than the bug this fixes.
		if elapsed > 30*time.Second {
			t.Errorf("settleAcquisition took %v against a stream that never ends; it "+
				"must be bounded by the grace, not by the collector", elapsed)
		}
	})
}

// TestFinishAcquisition_CarriesTheObservationOnBothPaths covers the
// verdict-forming step that no unit test could reach until it was split
// out of attemptGetIP.
//
// It is here because a mutation found the gap rather than a reading of
// the code did: replacing the error path's settle with the zero
// observation left the whole package green. That mutation is the
// fail-open #868 records, and the error path is the one a managed
// segment with a silent server takes — dhcpcd -1 -p -6 never gets a reply, the
// acquisition budget runs out, and Finish returns ctx.Err().
func TestFinishAcquisition_CarriesTheObservationOnBothPaths(t *testing.T) {
	// managed builds an accumulator that has already observed a managed
	// advertisement, through the real collector, with the stream closed
	// so the collector has demonstrably finished.
	managed := func(t *testing.T, extra ...Event) (*acquisition, chan struct{}) {
		t.Helper()
		events := make(chan Event, 1+len(extra))
		events <- Event{Type: "routeradvert", RouterFlags: "MO"}
		for _, e := range extra {
			events <- e
		}
		close(events)
		acq := &acquisition{}
		collected := make(chan struct{})
		go func() {
			defer close(collected)
			collectAcquisition(events, acq)
		}()
		<-collected
		return acq, collected
	}

	t.Run("Finish failed", func(t *testing.T) {
		acq, collected := managed(t)
		boom := errors.New("context deadline exceeded")

		_, ra, err := finishAcquisition(boom, collected, acq, raDrainGrace)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want the error Finish reported", err)
		}
		if !ra.Seen || !ra.Managed {
			t.Errorf("observation = %+v, want Seen and Managed. Finish failing is not "+
				"evidence the segment stayed silent — it is the expected outcome on a "+
				"managed segment whose DHCPv6 server answered nothing, and reporting "+
				"the zero observation there makes classifyV6Absence read v6NoRouter "+
				"and start the container with no IPv6 address (#868)", ra)
		}
	})

	t.Run("Finish succeeded but no lease arrived", func(t *testing.T) {
		acq, collected := managed(t)

		_, ra, err := finishAcquisition(nil, collected, acq, raDrainGrace)
		if !errors.Is(err, util.ErrNoLease) {
			t.Errorf("err = %v, want ErrNoLease", err)
		}
		if !ra.Seen || !ra.Managed {
			t.Errorf("observation = %+v, want Seen and Managed — the advertisement is "+
				"what makes a missing lease interpretable, so it must survive the "+
				"ErrNoLease path too", ra)
		}
	})

	t.Run("a lease arrived", func(t *testing.T) {
		acq, collected := managed(t, Event{Type: "bound", Data: Info{IP: "2001:db8::1/64"}})

		info, ra, err := finishAcquisition(nil, collected, acq, raDrainGrace)
		if err != nil {
			t.Fatalf("err = %v, want none", err)
		}
		if info.IP != "2001:db8::1/64" {
			t.Errorf("lease = %+v, want the bound address", info)
		}
		if !ra.Seen || !ra.Managed {
			t.Errorf("observation = %+v, want Seen and Managed on the success path too", ra)
		}
	})
}

// funcBody returns the lines strictly inside the named top-level
// function's braces.
//
// It fails rather than returns on both edges a source-reading gate can
// fall through: a signature found more than once (this gate cannot
// arbitrate between copies) and a signature found not at all (a
// universal over an empty body is not a check).
func funcBody(t *testing.T, srcFile, signature string) string {
	t.Helper()
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read %v: %v", srcFile, err)
	}
	lines := strings.Split(string(src), "\n")

	var starts []int
	for i, l := range lines {
		if strings.HasPrefix(l, signature) {
			starts = append(starts, i+1)
		}
	}
	if len(starts) != 1 {
		t.Fatalf("%v: %d functions start with %q (lines %v), want exactly one — this "+
			"gate reads the source, so it can neither arbitrate between copies nor "+
			"pass by finding nothing", srcFile, len(starts), signature, starts)
	}
	for i := starts[0]; i < len(lines); i++ {
		if lines[i] == "}" {
			return strings.Join(lines[starts[0]:i], "\n")
		}
	}
	t.Fatalf("%v: no closing brace at column 0 after %q", srcFile, signature)
	return ""
}

// stripLineComments removes // comments so a gate reading source judges
// the CODE and not the prose beside it. Sound here because the function
// under test contains no string literal carrying a "//".
func stripLineComments(body string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestAttemptGetIP_TheDeadlineOnlyBoundsFinish is the deterministic
// observer over attemptGetIP's COMPOSITION — the dozen lines this whole
// fix turns on, and the only part of it that nothing was watching.
//
// MEASURED, on this tree: restoring the pre-fix shape into the real
// attemptGetIP — a publish-on-close channel read back through a select
// whose other arm is ctx.Done() — leaves the entire unit lane GREEN.
// Every test of the retry loop stubs the function out through
// attemptGetIPFunc, and TestSettleAcquisition_* above REPRODUCES the
// collector wiring by hand rather than executing it. Splitting
// finishAcquisition out gave the verdict step an observer and left the
// composition around it with none.
//
// The integration test is not a substitute, because it is not a guard:
// TestDHCPv6_Managed_ServerSilent_IsStillFatal PASSED with the defect
// present, FAILED with the defect present, and passes with it fixed
// (measured across three CI runs of this branch). A coin-flip detector
// is what a race looks like from the outside; it cannot hold a
// regression out.
//
// Source-reading rather than behavioural for the same reason
// TestStart_EnablesIPv6BeforeWaitingForTheLinkLocal
// (pkg/plugin/v6_link_test.go) reads source: executing this function
// needs a live dhcpcd, so the alternative to a gate here is no observer
// at all.
//
// KEYED ON THE PROPERTY, NOT ON A SPELLING. The property is "the
// acquisition context bounds Finish and nothing else": its only
// permitted appearance in this body is as Finish's argument, and what
// Finish returns is handed straight to the verdict step so there is no
// gap between them for a second reading of the deadline. A gate that
// merely banned the literal `ctx.Done()` would be satisfied by handing
// the same context to the accumulator instead — `&acquisition{ctx: ctx}`
// honoured in settleAcquisition — which is a genuine defect and which
// passes TestSettleAcquisition_TakesNoContext, that one keying on the
// parameter list alone.
func TestAttemptGetIP_TheDeadlineOnlyBoundsFinish(t *testing.T) {
	const (
		srcFile   = "client.go"
		signature = "func attemptGetIP("
		// The composition itself: Finish's error is the verdict step's
		// first argument, so nothing sits between the two.
		delegation = "finishAcquisition(client.Finish(ctx)"
		// The historical spelling of the defect, named separately from
		// the count below so a reader who is looking for it finds it.
		defect = "ctx.Done()"
	)

	code := stripLineComments(funcBody(t, srcFile, signature))

	if !strings.Contains(code, delegation) {
		t.Errorf("%v: attemptGetIP does not contain %q.\n"+
			"The attempt's verdict must be formed by handing Finish's error straight to "+
			"finishAcquisition. Anything between them is a place where the observation "+
			"can be read a second time, under a deadline that has already expired — "+
			"which is the #868 defect: a managed segment whose DHCPv6 server went "+
			"silent classified as v6NoRouter and TOLERATED.\nbody:\n%s", srcFile, delegation, code)
	}

	if strings.Contains(code, defect) {
		t.Errorf("%v: attemptGetIP contains %q. The acquisition context is ALWAYS "+
			"already expired here — the managed path reaches this function only by "+
			"running the budget out — so a select arm on it is ready while the "+
			"observation arm is not, and the caller takes the zero observation for a "+
			"segment that advertised the managed flag.\nbody:\n%s", srcFile, defect, code)
	}

	// One appearance, and the check above says which one it is. This is
	// the half that keys on the property rather than on the spelling:
	// a context reaching the accumulator, the settle, or the verdict
	// step by any route at all is a second appearance and fails here.
	if n := strings.Count(code, "ctx"); n != 1 {
		t.Errorf("%v: attemptGetIP mentions ctx %d times, want exactly 1 (as "+
			"client.Finish's argument). The acquisition deadline bounds how long we "+
			"WAIT and must never reach what we are allowed to have SEEN — passing it "+
			"to the accumulator or the settle restores #868's fail-open exactly, and "+
			"does it in a shape no signature test can see.\nbody:\n%s", srcFile, n, code)
	}
}

// TestAcquisition_CarriesNoContext is the type-level half of the gate
// above, and it exists because the source gate and this one fail in
// different directions.
//
// A context can re-enter this path without ever being spelled inside
// attemptGetIP: stored on the accumulator by a constructor, or defaulted
// onto the struct. The absence has to be a property of the TYPE, not
// only of one function's text — the same argument
// TestSettleAcquisition_TakesNoContext makes about the signature, one
// level down.
func TestAcquisition_CarriesNoContext(t *testing.T) {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	at := reflect.TypeOf((*acquisition)(nil)).Elem()

	if at.NumField() == 0 {
		t.Fatalf("acquisition has no fields; this gate would be a universal over an " +
			"empty set and would pass over any accumulator at all")
	}
	for i := 0; i < at.NumField(); i++ {
		f := at.Field(i)
		if f.Type == ctxType || f.Type.Implements(ctxType) {
			t.Fatalf("acquisition.%s is a %v. The accumulator must not be able to see "+
				"the acquisition deadline: it is reached only on paths where that "+
				"deadline has already expired, so anything that consults it there "+
				"returns the zero observation and a managed segment with a silent "+
				"DHCPv6 server becomes a running container with no IPv6 (#868).",
				f.Name, f.Type)
		}
	}
}
