// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestV6Fixture_ModesComeUpAsRequested is the v6-modes fixture's own
// contract test: each of the five modes brings up a segment that is
// actually in that mode, proven from the router advertisement on the
// wire and from the server's own log, both.
//
// It exists because the first version of that fixture came up in the
// WRONG MODE and said nothing. dnsmasq was started while the bridge's
// global IPv6 address was still tentative, so it could not send from
// it and its first router advertisement slipped from about one second
// to about nine — while logging "IPv6 router advertisement enabled"
// exactly as it does when everything is fine. Three of the four modes
// were silently degraded. The only visible symptom was a consumer
// test failing to observe behaviour that genuinely was not happening,
// and the cheapest-looking repair would have been to widen a timeout
// until the symptom went away.
//
// #815 is one consumer of this fixture; #816, #820 and #821 are the
// others, and #911's chassis round is the next.
//
// The assertions live in the fixture (NewV6Fixture fails the test if
// the segment is not in the mode asked for), so this is the thing that
// RUNS them — and it runs them for every mode rather than for
// whichever one a consumer happens to need today. V6ManagedSilent is
// in the list for everything client-independent; the one thing that
// separates it from V6Managed needs a client, and that is
// AssertExchange's business.
func TestV6Fixture_ModesComeUpAsRequested(t *testing.T) {
	for _, mode := range harness.V6Modes() {
		t.Run(mode.String(), func(t *testing.T) {
			f := harness.NewV6Fixture(t, mode)
			t.Cleanup(func() {
				if t.Failed() {
					f.DumpLogs(func(s string) { t.Log(s) })
				}
			})
			if f.Bridge() != harness.V6BridgeName {
				t.Errorf("fixture bridge = %q, want %q", f.Bridge(), harness.V6BridgeName)
			}
			if f.Mode() != mode {
				t.Errorf("fixture mode = %s, want %s", f.Mode(), mode)
			}

			// The design table's two "to be measured" wire cells are
			// measured HERE, on the lane, and this run's log is the
			// record: every mode prints the advertisement the fixture
			// accepted it on, decoded, with the delay from the moment
			// the server was started. The assertion is assertMode's;
			// this is the evidence a reader can check it against, and
			// it is also the first-advertisement bound the readiness
			// race needs, measured per mode rather than argued from
			// one.
			frames := f.RACapture().FramesAfter(f.StartedAt())
			if len(frames) == 0 {
				t.Logf("wire: no advertisement within %s of the server starting", harness.V6NoRAWindow())
				return
			}
			delay := frames[0].At.Sub(f.StartedAt())
			t.Logf("wire: %d advertisement(s), first %s after the server started: %s",
				len(frames), delay.Round(time.Millisecond), frames[0])
			// TWO CLOCKS, and they are not the same instant. The
			// number LOGGED above is measured from the server's
			// start, because that is what dnsmasq's schedule is
			// relative to and what the population in
			// v6signature.go's schedule block counts. The number
			// ASSERTED below is measured from the instant
			// assertMode's budget began -- after the readiness wait,
			// which sits between the two and costs whatever it costs.
			//
			// Round 2 asserted the logged number against RABudget()
			// and called it "the same budget assertMode spends". It
			// was not: it was a strictly shorter interval, so the
			// direction was safe and the claim was false, and a
			// bring-up whose readiness poll cost 500 ms could have
			// reddened here on a fixture assertMode accepted.
			//
			// The bound is asserted rather than only logged because
			// the first record of this measurement was a comment
			// claiming a range the lane had already falsified twice,
			// which is what an unasserted number buys.
			budgeted := frames[0].At.Sub(f.EvidenceStartedAt())
			if budgeted > harness.RABudget() {
				t.Errorf("first advertisement %s after assertMode's budget began (%s after the "+
					"server started), outside the derived budget %s; that budget is what "+
					"assertMode spends, so a segment this slow is one it would report as %s",
					budgeted.Round(time.Millisecond), delay.Round(time.Millisecond),
					harness.RABudget(), harness.V6NoRA)
			}
			// The bytes, so the fast-lane decoder can be pinned to a
			// frame THIS fixture produced on THIS lane rather than to
			// one captured elsewhere with a different argv.
			t.Logf("wire bytes (%s): %s", mode, hex.EncodeToString(frames[0].Raw))
		})
	}
}

// --- observing the fixture's own refusal --------------------------------

// errCapturedFatal unwinds a captured Fatalf. It is a sentinel rather
// than a bare panic so a real panic from the fixture — a nil map, a
// netlink surprise — still crashes the test instead of being read as
// "the fixture refused", which would make the drift matrix pass for
// entirely the wrong reason.
var errCapturedFatal = errors.New("v6 fixture refused (captured)")

// capturedT is the smallest thing that can watch the fixture fail, and
// it is the first of its kind in this harness — the repo's usual
// pattern is a pure predicate with a *testing.T wrapper, and
// V6ModeFindings is exactly that. It is not enough here on its own:
// the mutant this file has to kill is one that leaves the verdict
// correct and stops ACTING on it, and only a test that goes through the
// real constructor can see that.
//
// *testing.T is embedded rather than reimplemented, so Helper, Logf and
// Cleanup are the real ones: the fixture's teardown really is
// registered on the subtest and really runs before the next pair
// starts, which matters because all twenty-five of them share one
// bridge name.
type capturedT struct {
	*testing.T
	failed bool
	msg    string
}

func (c *capturedT) Fatalf(format string, args ...any) {
	c.failed = true
	c.msg = fmt.Sprintf(format, args...)
	panic(errCapturedFatal)
}

// startUnderName starts a segment with actual's dnsmasq flags, tells
// the fixture it is name, and reports whether the fixture refused it.
func startUnderName(t *testing.T, name, actual harness.V6Mode) (refused bool, msg string) {
	c := &capturedT{T: t}
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, errCapturedFatal) {
			panic(r)
		}
		refused, msg = c.failed, c.msg
	}()
	harness.NewV6FixtureWithArgs(c, name, harness.RangeArgsFor(actual))
	return false, ""
}

// TestV6Fixture_RefusesASegmentInAnotherModesShape is the drift matrix,
// and it is the plugin-side twin of the library's v6-fixture-mode-drift
// oracle scenario.
//
// Trap 2 is a test that is green because the fixture answered from a
// different mode than the test named: a "managed" segment that in fact
// ran stateless still answers Information-requests, so "the container
// got DNS" passes in the wrong mode. The defence is that the fixture
// itself refuses, before any consumer's body runs. A defence nobody has
// watched refuse is not known to work, so this starts every ordered
// pair of distinct modes the wrong way round and requires the refusal —
// and runs the diagonal, so a fixture that refused everything would
// fail here too.
//
// The exempt pairs are DERIVED, by V6IndistinguishableModes, from the
// signature table itself: two modes no fixture-time evidence can
// separate are two modes this matrix cannot ask about. That is a
// property of managed and managed-silent, which differ only in what the
// server does once a client speaks, and it is pinned to exactly that
// one pair by a fast-lane test — so a third collision arriving later is
// named rather than silently exempted.
//
// WHAT IT PROVES: the fixture refuses a segment whose dnsmasq flags are
// another mode's, before the consumer's body runs, and its refusal
// names both modes by whole name. Every ordered pair is started the
// wrong way round; the diagonal is started too, so a fixture that
// refused everything is red here as well.
//
// WHY ITS WALL CLOCK STANDS (D41). Measured 65.44s, of which ~38s is
// spent in exactly five of the 25 cells — the ones whose ACTUAL mode is
// nora, where the only evidence of the mode is that no router
// advertisement arrives (4 refusal cells at ~6.4s = one RABudget each,
// and the nora/flags-of-nora diagonal at 12.4s = the full
// V6NoRAWindow). The remaining 20 cells cost ~1.4s each and are already
// nothing but a fixture start. The absence windows are DERIVED in
// harness/v6signature.go from dnsmasq's own first-RA bound, and
// shortening one is the precise failure they were written to guard: a
// no-RA check that passes because it did not wait is a check with one
// possible verdict. So this test keeps its clock, and the shard
// partition is what absorbs it: at 65.36s it is the fifth-longest
// main-suite test, well under the 196s longest shard, and the
// longest-first packer places it before the filler.
func TestV6Fixture_RefusesASegmentInAnotherModesShape(t *testing.T) {
	exempt := map[[2]harness.V6Mode]bool{}
	for _, p := range harness.V6IndistinguishableModes() {
		exempt[p] = true
		exempt[[2]harness.V6Mode{p[1], p[0]}] = true
	}
	t.Logf("indistinguishable at fixture time, exempted: %v", harness.V6IndistinguishableModes())

	for _, name := range harness.V6Modes() {
		for _, actual := range harness.V6Modes() {
			t.Run(name.String()+"/flags-of-"+actual.String(), func(t *testing.T) {
				refused, msg := startUnderName(t, name, actual)

				// The exempt pair is DRIVEN, not skipped. Skipping it
				// asserted nothing about the exemption, and these are
				// the two cells whose names overlap -- exactly where
				// the pair assertion below used to be satisfied by the
				// wrong half.
				if name == actual || exempt[[2]harness.V6Mode{name, actual}] {
					if refused {
						t.Fatalf("the fixture refused a segment it cannot tell from the mode "+
							"asked for (%s under %s): %s", actual, name, msg)
					}
					return
				}
				if !refused {
					t.Fatalf("a segment running %s's dnsmasq flags was accepted as %s; "+
						"every consumer of the %s mode would then be asserting against a "+
						"%s segment", actual, name, name, actual)
				}
				// The message has to name the pair, because that is the
				// whole diagnosis: a refusal that says only "mode check
				// failed" leaves the next person to reproduce it. Whole
				// names, not substrings: `managed` is a prefix of
				// `managed-silent`, and a refusal naming only the
				// latter satisfied a Contains check for the former.
				if !harness.V6ModeNamed(msg, name) {
					t.Errorf("the refusal does not name the mode asked for (%s); names %v: %s",
						name, harness.V6ModeNamesIn(msg), msg)
				}
				if !harness.V6ModeNamed(msg, actual) {
					t.Errorf("the refusal does not name the mode observed (%s); names %v: %s",
						actual, harness.V6ModeNamesIn(msg), msg)
				}
			})
		}
	}
}

// --- the capture's vantage point ----------------------------------------

// TestV6RACapture_SeesTheAdvertisementOnTheBridgeAndNotOnAQuietLink is
// the measurement behind the vantage-point paragraph in racapture.go,
// and it is here rather than argued there because the ARP capture next
// door reaches the OPPOSITE conclusion for its own frames — a macvlan
// child's transmits never pass its parent's taps — and "the same
// reasoning applies" is exactly the kind of claim that is wrong once.
//
// The failure it closes: a capture opened on a link the server never
// transmits on sees nothing, whereupon AssertNoRAWithin passes for
// every mode and the no-RA row becomes a gate with one possible
// verdict. Both verdicts are therefore produced in one run, by the same
// code, on a segment that is definitely advertising.
func TestV6RACapture_SeesTheAdvertisementOnTheBridgeAndNotOnAQuietLink(t *testing.T) {
	const quietLink = "dh-itest-quiet6"

	// A link of our own that no router advertises on. Removed first in
	// case a panicked run left it behind, and torn down here rather
	// than by the fixture, which does not know about it.
	if l, err := netlink.LinkByName(quietLink); err == nil {
		_ = netlink.LinkDel(l)
	}
	la := netlink.NewLinkAttrs()
	la.Name = quietLink
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("LinkAdd %s: %v", quietLink, err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(quietLink); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	l, err := netlink.LinkByName(quietLink)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", quietLink, err)
	}
	if err := netlink.LinkSetUp(l); err != nil {
		t.Fatalf("LinkSetUp %s: %v", quietLink, err)
	}

	elsewhere := harness.StartRACapture(t, quietLink)

	f := harness.NewV6Fixture(t, harness.V6Managed)
	t.Cleanup(func() {
		if t.Failed() {
			f.DumpLogs(func(s string) { t.Log(s) })
			elsewhere.Dump(func(s string) { t.Log(s) })
		}
	})

	onBridge := f.RACapture().FramesAfter(f.StartedAt())
	if len(onBridge) == 0 {
		t.Fatalf("no advertisement captured on %s, on a segment the fixture just proved is "+
			"advertising — the capture's vantage point is wrong and every 'no RA arrived' "+
			"assertion in this suite is vacuous", f.Bridge())
	}
	t.Logf("vantage point: %d advertisement(s) on %s, first %s after the server started",
		len(onBridge), f.Bridge(), onBridge[0].At.Sub(f.StartedAt()).Round(time.Millisecond))
	t.Logf("first advertisement decoded: %s", onBridge[0])

	if got := elsewhere.Frames(); len(got) != 0 {
		t.Errorf("%d advertisement(s) captured on %s, a link with no router on it; the capture "+
			"is not bound to the link it names, so 'no RA here' proves nothing about here",
			len(got), quietLink)
	}
}

// --- trap 1's observers -------------------------------------------------

// TestV6Fixture_AwaitRAAfterAndItsNegative drives both observers M7d
// will call, in both directions, on live segments.
//
// AwaitRAAfter is trap 1's positive: "an advertisement arrived AFTER
// this instant" is the premise every v6 scenario rests on, and it is a
// different claim from "an advertisement exists" — the trap is a test
// that passes because the RA came before the client started, or never,
// while the client reported no router and the test only checked that
// the endpoint came up.
//
// AssertNoRAWithin is its negative, and the window it spends is derived
// from dnsmasq's own scheduling rather than chosen: a window shorter
// than the interval at which the server would have advertised passes
// because it did not wait.
func TestV6Fixture_AwaitRAAfterAndItsNegative(t *testing.T) {
	t.Run("an advertising segment satisfies AwaitRAAfter", func(t *testing.T) {
		f := harness.NewV6Fixture(t, harness.V6Managed)
		t.Cleanup(func() {
			if t.Failed() {
				f.DumpLogs(func(s string) { t.Log(s) })
			}
		})
		frames := f.AwaitRAAfter(f.StartedAt(), harness.RABudget())
		if len(frames) == 0 {
			t.Fatal("AwaitRAAfter returned no frames without failing the test")
		}
		if !frames[0].Managed {
			t.Errorf("the managed segment's advertisement has M clear: %s", frames[0])
		}
	})

	t.Run("a silent segment satisfies AssertNoRAWithin", func(t *testing.T) {
		f := harness.NewV6Fixture(t, harness.V6NoRA)
		t.Cleanup(func() {
			if t.Failed() {
				f.DumpLogs(func(s string) { t.Log(s) })
			}
		})
		f.AssertNoRAWithin(harness.V6NoRAWindow())
	})

	// The other direction for each, observed rather than argued. An
	// advertising segment must FAIL AssertNoRAWithin, and a silent one
	// must FAIL AwaitRAAfter — otherwise both are functions with one
	// possible verdict.
	t.Run("an advertising segment fails AssertNoRAWithin", func(t *testing.T) {
		refused, msg := captureFixtureCall(t, harness.V6Managed, func(f *harness.V6Fixture) {
			// The full derived window, at no cost: the fixture has
			// already captured this segment's advertisement, so the
			// refusal comes on the first poll.
			f.AssertNoRAWithin(harness.V6NoRAWindow())
		})
		if !refused {
			t.Fatal("AssertNoRAWithin passed on a segment that advertises")
		}
		if !strings.Contains(msg, "must not advertise") {
			t.Errorf("unexpected refusal: %s", msg)
		}
	})

	// The direction that could go quiet. On an ADVERTISING segment the
	// log column is satisfied forever -- the fixture logged an
	// RTR-ADVERT line at construction -- so if the wire column were
	// ever dropped, or read without the `since` filter, this call
	// would pass while claiming something false. `since` is set past
	// every advertisement the segment has produced, and the NEXT one
	// is at least five seconds out -- `new_timeout` is
	// `now + 5 + rand16()/4400`, radv.c:977, so five is the floor and
	// not the mean -- and the budget below is two seconds, which is
	// inside that floor with room for the call's own start-up. A
	// budget of five would race the earliest possible next frame.
	t.Run("an advertising segment fails AwaitRAAfter for an instant after its advertisement", func(t *testing.T) {
		refused, msg := captureFixtureCall(t, harness.V6Managed, func(f *harness.V6Fixture) {
			seen := f.RACapture().FramesAfter(f.StartedAt())
			if len(seen) == 0 {
				t.Fatalf("the managed fixture came up with no advertisement captured, so this " +
					"case cannot set `since` past one")
			}
			after := seen[len(seen)-1].At.Add(time.Millisecond)
			f.AwaitRAAfter(after, 2*time.Second)
		})
		if !refused {
			t.Fatal("AwaitRAAfter passed for an instant after the segment's only advertisement; " +
				"the log column is satisfied for this segment's whole life, so this is what a " +
				"missing wire column looks like")
		}
		if !strings.Contains(msg, "no router advertisement after") {
			t.Errorf("unexpected refusal: %s", msg)
		}
		if !strings.Contains(msg, "which is the column that answers") {
			t.Errorf("the refusal does not say which column carried the \"after\" claim: %s", msg)
		}
	})

	t.Run("a silent segment fails AwaitRAAfter", func(t *testing.T) {
		refused, msg := captureFixtureCall(t, harness.V6NoRA, func(f *harness.V6Fixture) {
			// dnsmasq's own worst case for a first advertisement, so
			// this waits exactly as long as one would have taken to
			// arrive rather than a number picked to be short.
			f.AwaitRAAfter(f.StartedAt(), harness.DnsmasqFirstRAUpperBound())
		})
		if !refused {
			t.Fatal("AwaitRAAfter passed on a segment that never advertises")
		}
		if !strings.Contains(msg, "no router advertisement after") {
			t.Errorf("unexpected refusal: %s", msg)
		}
	})
}

// TestV6Fixture_AssertExchangeRefusesASegmentNoClientEverUsed is
// AssertExchange's live negative control.
//
// The positive is elsewhere, deliberately: the fast lane drives the
// verdict against captured server logs, one per mode, and
// dhcpv6_noaddress_modes_test.go drives it against live segments whose
// clients really completed the exchange. This is the half that would
// rot silently either way: a contract whose must-set had been emptied
// passes every positive above and only fails here, on a log with no
// exchange in it at all.
func TestV6Fixture_AssertExchangeRefusesASegmentNoClientEverUsed(t *testing.T) {
	refused, msg := captureFixtureCall(t, harness.V6Managed, func(f *harness.V6Fixture) {
		// A budget, not a wait: no container has joined a network on
		// this segment, so nothing can arrive however long it polls.
		f.AssertExchange(time.Second)
	})
	if !refused {
		t.Fatal("AssertExchange passed on a segment no client has spoken to; every scenario " +
			"whose client failed to start would pass it too")
	}
	if !strings.Contains(msg, "DHCPSOLICIT") {
		t.Errorf("the refusal does not name the message that is missing: %s", msg)
	}
}

// captureFixtureCall builds a fixture in mode — which must succeed —
// and then runs call under a T that records a Fatalf instead of
// suffering it.
//
// The phase flag is what keeps the two apart. Without it a fixture that
// failed to come up at all would be reported as "the call under test
// refused", and the negative control would be green for a reason that
// has nothing to do with the function it names.
func captureFixtureCall(t *testing.T, mode harness.V6Mode, call func(*harness.V6Fixture)) (refused bool, msg string) {
	c := &capturedT{T: t}
	inCall := false
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, errCapturedFatal) {
			panic(r)
		}
		if !inCall {
			t.Fatalf("the fixture refused its own mode before the call under test: %s", c.msg)
			return
		}
		refused, msg = c.failed, c.msg
	}()
	f := harness.NewV6FixtureWithArgs(c, mode, harness.RangeArgsFor(mode))
	inCall = true
	call(f)
	return false, ""
}
