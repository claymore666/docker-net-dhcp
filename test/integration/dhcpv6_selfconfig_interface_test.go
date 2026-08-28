// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// The self-configuring half of the IPv6 release bar (#875): a segment
// that hands out addresses by router advertisement rather than by
// DHCPv6 -- SLAAC, or stateless DHCPv6 where the advertisement supplies
// the address and DHCPv6 supplies only configuration.
//
// #868 made a container START on these segments. It did not make IPv6
// WORK on them, and this file is the difference between those two
// statements.
//
// WHAT IS ALREADY KNOWN, AND WHY THAT IS NOT ENOUGH. Measured in an
// isolated network namespace with the plugin's own dhcpcd argv: a
// global address forms from the first advertisement, and from the
// moment dhcpcd writes accept_ra=0 and autoconf=0 the kernel's
// lifetimes only count down -- 1797 -> 1787 -> 1647 against a no-dhcpcd
// control reading 1797 -> 1796 -> 1705 -- while the RA-derived default
// route disappears within about ten seconds. That is honest evidence
// about a LINK. It is not evidence about a CONTAINER on this plugin,
// and the release bar is about containers. Everything below is read
// from inside one.
//
// WHY IT HAS NEVER BEEN CAUGHT. The address is present the whole time,
// so anything that looks once passes. On-link ping6 keeps succeeding
// with no default route at all, so the obvious reachability check
// passes too. The two things that actually fail are invisible to a
// single sample: whether the lifetime is being refreshed, and whether
// anything OFF-link is reachable.

// selfConfigWindow is how long the container is observed, and
// selfConfigInterval how often.
//
// PROVISIONAL, pending a measurement of what this fixture's dnsmasq
// actually advertises. The window has to contain at least one router
// advertisement AFTER the first, because the whole discrimination below
// is "did a later advertisement move the lifetime". In the isolated
// measurement the control refreshed inside the first ten seconds and
// again around t+58, so 150s contains at least two with margin. If the
// cadence turns out to be longer than this window the test cannot tell
// a refresh from a countdown, and it says so in its own failure text
// rather than reporting the defect -- see the vacuity guard in
// assertLifetimeRefreshed.
const (
	selfConfigWindow   = 150 * time.Second
	selfConfigInterval = 10 * time.Second
)

// refreshFloor is how much the lifetime ceiling must rise across the
// window before it counts as a refresh.
//
// The ceiling is valid_lft + elapsed (see assertLifetimeRefreshed). Its
// noise is the sum of two roundings -- `ip` prints whole seconds, and
// the elapsed clock is read separately -- so it is bounded by about two
// seconds. Ten is comfortably clear of that and far below the tens of
// seconds a real refresh moves it by, so this is a discriminator and
// not a threshold anybody has to tune.
const refreshFloor = 10

// selfConfigFormBy bounds how long self-configuration may take. DAD on
// the link-local is one RetransTimer (1s by default), and the
// solicit/advertise exchange that follows is one round trip on a veth.
// 60s is far past both and is a bound on something being WRONG, not a
// tuned wait.
const selfConfigFormBy = 60 * time.Second

// parseLft turns `ip`'s lifetime field into seconds. "forever" is
// reported as not-finite rather than as a large number, because the two
// mean different things here: a finite lifetime is the kernel ageing an
// address it learned from an advertisement, and `forever` is an address
// somebody applied statically.
func parseLft(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "forever" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "sec"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// selfConfigSample is one observation of the container's v6 state.
type selfConfigSample struct {
	elapsed time.Duration
	addr    string // ip -6 addr, UNFILTERED: no device is named
	links   string // ip link, so the log always carries the real names
	route   string

	// The sysctls are RECORDED AND NOT ASSERTED ON, deliberately.
	// They are the mechanism this defect happens to run through; the
	// bar is about the address and the route. A fix that keeps the
	// address refreshing and the route present by some other means is
	// a fix, and a test keyed on the sysctl values would fail it.
	// They are logged because they are the first thing a reader of a
	// failure will want.
	acceptRA string
	autoconf string
}

// assertLifetimeRefreshed is the central claim of this file.
//
// THE LEDGER, AND WHY IT IS SHAPED LIKE THIS. A SLAAC address's
// valid_lft is reset to the advertised prefix lifetime by every
// advertisement the kernel accepts, and counts down in between. So
// valid_lft ALONE cannot answer the question: a sample taken just
// before a refresh and one taken just after are both "some number",
// and comparing two of them measures where in the cycle the samples
// landed.
//
// lifetime + elapsed does answer it. If no advertisement is ever
// accepted the sum is constant, because the lifetime falls exactly as
// fast as the clock rises -- that is the defect. If advertisements ARE
// accepted the sum steps up by the interval between them. The ceiling
// is therefore monotone non-decreasing in a healthy configuration and
// flat in a broken one, whatever the advertised lifetime happens to be
// and whatever the advertisement cadence is. Neither number has to be
// known, which matters because both are the server's choice.
//
// IT IS A WITHIN-ARM COMPARISON, AND THAT IS THE POINT. The
// measurement that opened #875 compared a dhcpcd arm against a
// no-dhcpcd control and concluded one refreshed; both series were
// countdowns, and the comparison only holds if both arms were sampled
// at identical elapsed times. Nothing here compares arms. A rise in
// this ledger is a rise in one address's own lifetime between two of
// its own samples, which nothing but a reset can produce.
//
// WHY PREFERRED IS THE ASSERTED ONE AND VALID IS CORROBORATION.
// RFC 4862 section 5.5.3(e) resets the preferred lifetime on every
// accepted advertisement, "regardless of whether the valid lifetime is
// also reset or ignored" -- unconditionally, with no exceptions. The
// valid lifetime is reset only when the advertised value "is greater
// than 2 hours or greater than RemainingLifetime". Both branches
// normally hold on a refresh, so valid usually rises too; but preferred
// is the one the standard guarantees, so it is the one a failure is
// keyed on. Reading only valid_lft would put the two-hour rule between
// this test and its verdict for no benefit.
// reporter is the subset of *testing.T that assertLifetimeRefreshed
// uses.
//
// IT IS AN INTERFACE SO THE VERDICT CAN BE DRIVEN DIRECTLY. This
// function is the whole discrimination in this file, and reaching it
// through a real test requires root, a bridge, a dnsmasq and a
// container -- so on any machine without those, the one piece of logic
// that decides "refresh or countdown" is unexecuted. That is the shape
// #868 paid for twice: a fold and a verdict step that no unit test
// could run, each hiding a defect until the integration lane found it
// ten minutes later and one layer from the line at fault.
//
// A *testing.T satisfies this, so callers are unchanged.
type reporter interface {
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
	Helper()
}

func assertLifetimeRefreshed(t reporter, samples []selfConfigSample, mode harness.V6Mode) {
	t.Helper()

	type point struct {
		elapsed  time.Duration
		valid    int
		pref     int
		ceiling  int // valid + elapsed
		prefCeil int // preferred + elapsed
	}
	var pts []point
	for _, s := range samples {
		a, ok := globalFromPrefix(parseV6Addrs(s.addr), harness.V6Prefix)
		if !ok {
			continue
		}
		lft, finite := parseLft(a.validLft)
		pref, prefFinite := parseLft(a.prefLft)
		if !finite || !prefFinite {
			// An address with no lifetime at all is not a
			// SLAAC address. On this segment nothing should be
			// applying one statically, so say what was found
			// rather than silently having nothing to measure.
			t.Errorf("on a %s segment the address %s has valid_lft=%q preferred_lft=%q, so "+
				"it is not an address the kernel is ageing from a router advertisement. "+
				"Something applied it statically; that is not how this segment hands "+
				"out addresses", mode, a.cidr, a.validLft, a.prefLft)
			return
		}
		e := int(s.elapsed.Seconds())
		pts = append(pts, point{s.elapsed, lft, pref, lft + e, pref + e})
	}

	// VACUITY GUARD. Everything below is a claim about a sequence of
	// observations; with fewer than two there is no sequence and the
	// comparison is trivially satisfied. This is the shape that turns
	// "the lifetime refreshes" into a statement about nothing -- a
	// container that lost its address entirely would otherwise reach
	// the end of this function without a single Errorf.
	if len(pts) < 2 {
		t.Errorf("only %d of %d samples on the %s segment had a global address on "+
			"the fixture's prefix, so there is no sequence to test for a refresh. "+
			"The address is not merely failing to refresh, it is absent",
			len(pts), len(samples), mode)
		return
	}

	first, last := pts[0], pts[len(pts)-1]
	rise := last.prefCeil - first.prefCeil
	validRise := last.ceiling - first.ceiling

	for _, p := range pts {
		t.Logf("  t+%-5s preferred_lft=%-6d ceiling=%-6d | valid_lft=%-6d ceiling=%d",
			p.elapsed.Round(time.Second), p.pref, p.prefCeil, p.valid, p.ceiling)
	}

	// The OTHER direction this could be wrong in, named beside the
	// verdict rather than left for the reader to think of. If the
	// advertised lifetime is longer than the window AND the cadence is
	// longer than the window, a healthy segment also produces a flat
	// ceiling, and the two are indistinguishable from inside this
	// function. S2 is what rules that out -- it requires the server to
	// have advertised at least twice -- so the caveat is a pointer to
	// the check that settles it, not a hedge.
	windowMayBeTooShort := first.pref > int(selfConfigWindow.Seconds())

	if rise < refreshFloor {
		caveat := ""
		if windowMayBeTooShort {
			caveat = fmt.Sprintf("\n\nBEFORE WIDENING THE WINDOW: the initial preferred lifetime is %ds, "+
				"longer than the %s observed here, so a segment whose advertisement "+
				"cadence also exceeded the window would look identical from inside this "+
				"check. S2 is what separates those — it requires the server to have "+
				"advertised at least twice in the same window. If S2 passed and this "+
				"failed, advertisements were sent and the container did not act on them, "+
				"which is the defect. Widening the window to make this pass is how the "+
				"test stops measuring anything.", first.pref, selfConfigWindow)
		}
		t.Errorf("on a %s segment the address's preferred-lifetime ceiling "+
			"(preferred_lft + elapsed) rose by only %ds across %s, want at least %ds. "+
			"RFC 4862 5.5.3(e) resets the preferred lifetime on EVERY accepted "+
			"advertisement, unconditionally, so a ceiling that does not rise is an "+
			"address no advertisement is reaching: preferred_lft went %d -> %d, "+
			"falling as fast as the clock, and the valid ceiling moved %ds over the "+
			"same window. The address is on the interface the whole time, which is "+
			"what hides this; it expires in about %d seconds from the start and the "+
			"container silently loses IPv6%s",
			mode, rise, selfConfigWindow, refreshFloor,
			first.pref, last.pref, validRise, first.valid, caveat)
	}
}

// TestDHCPv6_SelfConfiguring_AddressAndRouteSurvive is the SLAAC and
// stateless half of #875.
//
// Each arm gets its OWN segment and its OWN container and asserts
// against those. There is no aggregate claim here: "self-configuring
// segments work" is not the same statement as "each of them works",
// and the fixture's own header says why that distinction is the one
// that matters.
func TestDHCPv6_SelfConfiguring_AddressAndRouteSurvive(t *testing.T) {
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
	}{
		{"slaac", harness.V6SLAAC, "dh-itest-v6slif"},
		{"stateless", harness.V6Stateless, "dh-itest-v6slsif"},
	}

	// NON-VACUITY. This table IS the acceptance criterion for the
	// self-configuring half of #875 -- emptying it leaves this test
	// green, the lane green and check-test-weakening.sh clean, and
	// "IPv6 works on self-configuring networks" would then be a
	// statement about no network at all.
	//
	// Keyed on the two modes rather than on a row count, because a
	// duplicated row satisfies a count. Both are needed and they are
	// not interchangeable: SLAAC has no DHCPv6 server on the segment
	// at all, stateless has one that answers configuration but hands
	// out no address, and the plugin runs a DHCPv6 client on both. A
	// fix that restored advertisements only on the path where no
	// DHCPv6 client ever bound would pass a SLAAC-only table.
	//
	// It runs BEFORE the fixture and the engine are touched, so it is
	// reachable without root and cannot be reported as an environment
	// failure.
	want := map[harness.V6Mode]bool{
		harness.V6SLAAC:     false,
		harness.V6Stateless: false,
	}
	for _, tc := range cases {
		want[tc.mode] = true
	}
	for mode, present := range want {
		if !present {
			t.Fatalf("no arm for the %s segment. It is one of the two ways a network "+
				"hands out an address by advertisement, and an arm that is not here "+
				"is a shape nothing checks", mode)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := harness.NewV6Fixture(t, tc.mode)
			dumpOnFailure(t, f)

			id, err := startOnV6Segment(t, ctx, cli, f, tc.net)
			if err != nil {
				t.Fatalf("a container failed to start on a %s segment. #868 made this "+
					"work; nothing below can be measured:\n%v", tc.mode, err)
			}

			ifname := containerIfname(t, ctx, id)
			t.Logf("%s segment: container interface discovered as %q", tc.mode, ifname)

			start := time.Now()
			var samples []selfConfigSample
			for {
				elapsed := time.Since(start)
				s := selfConfigSample{
					elapsed:  elapsed,
					addr:     harness.ExecOutput(t, ctx, id, "ip", "-6", "addr"),
					links:    harness.ExecOutput(t, ctx, id, "ip", "link"),
					route:    harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show"),
					acceptRA: harness.ExecOutput(t, ctx, id, "cat", fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", ifname)),
					autoconf: harness.ExecOutput(t, ctx, id, "cat", fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/autoconf", ifname)),
				}
				samples = append(samples, s)
				if elapsed >= selfConfigWindow {
					break
				}
				time.Sleep(selfConfigInterval)
			}

			f0, fN := samples[0], samples[len(samples)-1]
			t.Logf("=== %s segment, t+0 ===\nip -6 addr:\n%s\nip -6 route:\n%s\n"+
				"accept_ra=%s autoconf=%s",
				tc.mode, f0.addr, f0.route,
				strings.TrimSpace(f0.acceptRA), strings.TrimSpace(f0.autoconf))
			t.Logf("=== %s segment, t+%s ===\nip -6 addr:\n%s\nip -6 route:\n%s\n"+
				"accept_ra=%s autoconf=%s",
				tc.mode, fN.elapsed.Round(time.Second), fN.addr, fN.route,
				strings.TrimSpace(fN.acceptRA), strings.TrimSpace(fN.autoconf))

			// ---- S1: does an address FORM, in bounded time, usable? ----
			//
			// This asserted on samples[0], at t+0, and failed there on a
			// segment that does configure itself correctly. That is not
			// a product defect, it is a premise no host can satisfy:
			// at t+0 the link-local is still `tentative`, and until DAD
			// completes there is no source address to send a Router
			// Solicitation from, so no advertisement has been answered
			// and no global address can exist yet. The run that caught
			// this shows exactly that -- link-local `tentative` and no
			// global at t+0, both address and default route present
			// later in the same container.
			//
			// The bar is not "instantly"; it is that the address APPEARS
			// and then STAYS usable. So the formation is bounded rather
			// than instantaneous, and the durability assertions below
			// carry the rest. Bounding it is STRONGER than asserting at
			// t+0, which could only ever fail: an address that never
			// arrives still fails here, and one that arrives and later
			// rots still fails S3/S4/S5.
			var a0 v6Addr
			var formed *selfConfigSample
			for i := range samples {
				if a, ok := globalFromPrefix(parseV6Addrs(samples[i].addr), harness.V6Prefix); ok {
					a0, formed = a, &samples[i]
					break
				}
			}
			if formed == nil {
				t.Fatalf("S1 FAILED: no global IPv6 address on the fixture's prefix %s ever "+
					"appeared on ANY interface inside the container across %s, on a %s "+
					"segment whose router advertisements carry that prefix. Last state:\n"+
					"ip -6 addr:\n%s\nip link:\n%s",
					harness.V6Prefix, selfConfigWindow, tc.mode, fN.addr, fN.links)
			}
			if formed.elapsed > selfConfigFormBy {
				t.Errorf("S1 FAILED: the address %s took %s to appear, past the %s bound. "+
					"Self-configuration is a link-local DAD plus one solicit/advertise "+
					"exchange; taking this long means something is retrying, not "+
					"configuring.", a0.cidr, formed.elapsed.Round(time.Second), selfConfigFormBy)
			}
			t.Logf("S1: address %s formed by t+%s", a0.cidr, formed.elapsed.Round(time.Second))
			for _, bad := range []string{"tentative", "dadfailed", "deprecated"} {
				if strings.Contains(a0.flags, bad) {
					t.Errorf("S1 FAILED: the address %s is %s (%q) — it is on the interface "+
						"but not usable as a source address", a0.cidr, bad, a0.flags)
				}
			}

			// ---- S2: does the segment agree it advertised? ----
			//
			// Outside evidence, and the negative control for S3/S4: if
			// the server never advertised, then a missing route says
			// nothing about the plugin. The fixture already asserts an
			// advertisement at startup; this asserts it kept going,
			// which is the premise every claim below rests on.
			if n := f.CountLogLines("RTR-ADVERT("); n < 2 {
				t.Errorf("S2: the server logged %d router advertisements across %s, want at "+
					"least 2. Without a second advertisement there is nothing that COULD "+
					"have refreshed the address, so the refresh claim below is not a "+
					"statement about the plugin", n, selfConfigWindow)
			}

			// ---- S3: does the address stay valid? ----
			t.Logf("S3: lifetime ledger on the %s segment", tc.mode)
			assertLifetimeRefreshed(t, samples, tc.mode)

			// ---- S4: is there a default route, at EVERY sample? ----
			//
			// Not just at the end: the measured shape is a route that
			// is present at +3s and gone by +13s, so a test that looked
			// only at the ends would still catch it but would not say
			// WHEN it went, and a fix that reinstalled the route late
			// would look identical to one that never lost it.
			for _, s := range samples {
				if !hasDefaultV6Route(s.route) {
					t.Errorf("S4 FAILED at t+%s on the %s segment: the container has NO IPv6 "+
						"default route. On-link traffic still works, which is what hides "+
						"this; nothing off-link is reachable over IPv6:\n%s",
						s.elapsed.Round(time.Second), tc.mode, s.route)
				}
			}

			// ---- S5: off-link reachability ----
			//
			// On-link ping6 succeeds with no default route at all, so
			// it proves nothing. This targets a different /64 to force
			// the route to be consulted, and reads the ERROR CLASS
			// rather than reachability: nothing answers at the far end
			// either way. "Network unreachable" is the kernel refusing
			// for want of a route; a timeout means a route existed and
			// was used.
			const offLink = "fd00:6470:6865:ffff::1"
			ping := harness.ExecOutput(t, ctx, id, "ping", "-6", "-c", "1", "-W", "2", offLink)
			t.Logf("S5: ping -6 %s from the container:\n%s", offLink, ping)
			if strings.Contains(strings.ToLower(ping), "unreachable") {
				t.Errorf("S5 FAILED on the %s segment: sending to the off-link address %s "+
					"failed with a network-unreachable error, which is the kernel saying "+
					"it has no route at all — not the far end being silent. IPv6 is "+
					"on-link only:\n%s", tc.mode, offLink, ping)
			}
		})
	}
}
