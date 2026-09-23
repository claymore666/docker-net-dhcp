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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The first fixture started dnsmasq while the bridge's global IPv6 address was tentative, so its first router
// advertisement slipped from about one to about nine seconds while it logged "IPv6 router advertisement enabled" as
// usual, and three of four modes came up degraded (#911). NewV6Fixture fails the test when the segment is not in the
// mode asked for; V6ManagedSilent differs from V6Managed only once a client speaks, which is AssertExchange's check.

// TestV6Fixture_ModesComeUpAsRequested checks that each of the five modes brings up a segment in that mode, from the wire and from the server's log (#815, #816, #820, #821, #911).
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

			// Each mode logs the advertisement the fixture accepted, decoded, with its delay from the server's start (#911).
			frames := f.RACapture().FramesAfter(f.StartedAt())
			if len(frames) == 0 {
				t.Logf("wire: no advertisement within %s of the server starting", harness.V6NoRAWindow())
				return
			}
			delay := frames[0].At.Sub(f.StartedAt())
			t.Logf("wire: %d advertisement(s), first %s after the server started: %s",
				len(frames), delay.Round(time.Millisecond), frames[0])
			// The logged delay counts from the server's start, which dnsmasq's schedule is relative to; the asserted one counts
			// from when assertMode's budget began, after the readiness wait, so it is the interval RABudget bounds (#911).
			budgeted := frames[0].At.Sub(f.EvidenceStartedAt())
			if budgeted > harness.RABudget() {
				t.Errorf("first advertisement %s after assertMode's budget began (%s after the "+
					"server started), outside the derived budget %s; that budget is what "+
					"assertMode spends, so a segment this slow is one it would report as %s",
					budgeted.Round(time.Millisecond), delay.Round(time.Millisecond),
					harness.RABudget(), harness.V6NoRA)
			}
			// The bytes pin the fast-lane decoder to a frame this fixture produced on the lane (#911).
			t.Logf("wire bytes (%s): %s", mode, hex.EncodeToString(frames[0].Raw))
		})
	}
}

// With the bridge's port removed on a hosted runner (run 34603031325), a gate reading IFF_RUNNING refused in two modes
// of five and /nora passed on a bridge with carrier 0, because a new bridge reports IFF_RUNNING while its operstate is
// unknown (#942). The port is attached and left down because a portless bridge transmits on some kernels only.

// TestV6Fixture_RefusesASegmentThatCannotTransmit checks that the carrier gate refuses a segment that cannot carry a frame, in every mode (#942).
func TestV6Fixture_RefusesASegmentThatCannotTransmit(t *testing.T) {
	for _, mode := range harness.V6Modes() {
		t.Run(mode.String(), func(t *testing.T) {
			refused, msg := startWithADeadBridgePort(t, mode)
			if !refused {
				t.Fatalf("the fixture accepted a %s segment on a bridge with no carrier; every "+
					"wire assertion about it is then about a link nothing can speak on, and in "+
					"%s, whose assertion is that no advertisement arrives, it PASSES (#942)",
					mode, harness.V6NoRA)
			}
			if !strings.Contains(msg, "no carrier") {
				t.Errorf("the fixture refused the %s segment for another reason than the link: "+
					"%s. A refusal that arrives later, from the mode assertion, is the "+
					"three-causes-one-message state the gate exists to prevent", mode, msg)
			}
		})
	}
}

// startWithADeadBridgePort starts a segment through the fixture's constructor with its bridge port attached and down, and reports whether the fixture refused it.
func startWithADeadBridgePort(t *testing.T, mode harness.V6Mode) (refused bool, msg string) {
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
	harness.NewV6FixtureWithADeadBridgePort(c, mode)
	return false, ""
}

// errCapturedFatal unwinds a captured Fatalf, so a real panic from the fixture still crashes the test.
var errCapturedFatal = errors.New("v6 fixture refused (captured)")

// capturedT embeds *testing.T and records a Fatalf, so a test can watch the fixture's real constructor refuse.
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

// startUnderName starts a segment with actual's dnsmasq flags, tells the fixture it is name, and reports whether the fixture refused it.
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

// Pairs no fixture-time evidence can separate are derived by V6IndistinguishableModes from the signature table and are
// pinned to managed and managed-silent by a fast-lane test (#911). The nora cells wait a full RABudget or
// V6NoRAWindow, derived from dnsmasq's first-advertisement bound, because a no-RA check that did not wait has one
// verdict (#911).

// TestV6Fixture_RefusesASegmentInAnotherModesShape checks that the fixture refuses every ordered pair of modes started the wrong way round, and accepts the diagonal (#911).
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
				// `managed` is a prefix of `managed-silent`, so the refusal must name both modes by whole name (#911).
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

// A macvlan child's transmits never pass its parent's taps, so the ARP capture needs a different vantage point, and a
// capture on a link the server never uses would pass AssertNoRAWithin for every mode (#911).

// TestV6RACapture_SeesTheAdvertisementOnTheBridgeAndNotOnAQuietLink checks that the capture sees the bridge's advertisement and nothing on a quiet link.
func TestV6RACapture_SeesTheAdvertisementOnTheBridgeAndNotOnAQuietLink(t *testing.T) {
	const quietLink = "dh-itest-quiet6"

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

// The AssertNoRAWithin window is derived from dnsmasq's own scheduling, since a window shorter than its interval
// passes because it did not wait (#911).

// TestV6Fixture_AwaitRAAfterAndItsNegative checks AwaitRAAfter and AssertNoRAWithin in both directions on live segments.
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

	t.Run("an advertising segment fails AssertNoRAWithin", func(t *testing.T) {
		refused, msg := captureFixtureCall(t, harness.V6Managed, func(f *harness.V6Fixture) {
			f.AssertNoRAWithin(harness.V6NoRAWindow())
		})
		if !refused {
			t.Fatal("AssertNoRAWithin passed on a segment that advertises")
		}
		if !strings.Contains(msg, "must not advertise") {
			t.Errorf("unexpected refusal: %s", msg)
		}
	})

	// On an advertising segment the log column is satisfied forever, so this catches a dropped wire column or a missing
	// `since` filter. dnsmasq schedules the next advertisement at `now + 5 + rand16()/4400` (radv.c:977), so five seconds
	// is the floor and the two-second budget stays inside it (#911).
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
			// dnsmasq's worst case for a first advertisement, so the wait is exactly as long as one would take.
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

// TestV6Fixture_AssertExchangeRefusesASegmentNoClientEverUsed checks that AssertExchange refuses a server log with no exchange in it (#911).
func TestV6Fixture_AssertExchangeRefusesASegmentNoClientEverUsed(t *testing.T) {
	refused, msg := captureFixtureCall(t, harness.V6Managed, func(f *harness.V6Fixture) {
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

// captureFixtureCall builds a fixture in mode, which must succeed, then runs call under a T that records a Fatalf.
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
