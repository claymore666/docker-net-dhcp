// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
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
func TestV6Fixture_RefusesASegmentInAnotherModesShape(t *testing.T) {
	exempt := map[[2]harness.V6Mode]bool{}
	for _, p := range harness.V6IndistinguishableModes() {
		exempt[p] = true
		exempt[[2]harness.V6Mode{p[1], p[0]}] = true
	}
	t.Logf("indistinguishable at fixture time, exempted: %v", harness.V6IndistinguishableModes())

	for _, name := range harness.V6Modes() {
		for _, actual := range harness.V6Modes() {
			if exempt[[2]harness.V6Mode{name, actual}] {
				continue
			}
			t.Run(name.String()+"/flags-of-"+actual.String(), func(t *testing.T) {
				refused, msg := startUnderName(t, name, actual)

				if name == actual {
					if refused {
						t.Fatalf("the fixture refused a segment in its OWN mode: %s", msg)
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
				// failed" leaves the next person to reproduce it.
				if !strings.Contains(msg, name.String()) {
					t.Errorf("the refusal does not name the mode asked for (%s): %s", name, msg)
				}
				if !strings.Contains(msg, actual.String()) {
					t.Errorf("the refusal does not name the mode observed (%s): %s", actual, msg)
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
		frames := f.AwaitRAAfter(f.StartedAt(), 5*time.Second)
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
// Nothing in this round can drive it positively: the 2.x branch refuses
// ipv6=true at network creation, so no run on this branch constructs a
// DHCPv6 client and no live log here can contain an exchange. The
// positive is driven in the fast lane against the server logs a real
// exchange produced, one per mode; the live positive belongs to the
// chassis round that removes the refusal.
//
// What IS drivable here, and is the half that would rot silently, is
// that the function refuses a log with no exchange in it. A contract
// whose must-set had been emptied would pass here and would then pass
// in M7d against a segment where nothing happened.
func TestV6Fixture_AssertExchangeRefusesASegmentNoClientEverUsed(t *testing.T) {
	refused, msg := captureFixtureCall(t, harness.V6Managed, func(f *harness.V6Fixture) {
		// A budget, not a wait: no client exists on this branch, so
		// nothing can arrive however long it polls.
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
