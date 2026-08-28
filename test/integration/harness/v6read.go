// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Reading a container's IPv6 state out of `ip` output, and the verdict
// that decides whether an address is being REFRESHED or is quietly
// counting down to nothing (#875).
//
// WHY THIS IS IN harness AND NOT BESIDE THE TEST THAT USES IT. It was
// written in the integration package, and #868's lesson was supposed to
// be the reason it was factored out at all: a fold and a verdict step
// that no test could execute each hid a defect until the integration
// lane found it ten minutes later and one layer from the line at fault.
// Factoring the verdict behind a small interface did not deliver that,
// because package `integration` has a TestMain that stands up the whole
// fixture and exits non-zero when it is not run as root -- so every
// test in that package, including one that touches nothing, needs root,
// a working engine and an installed plugin. The logic was still
// unexecutable outside the heavy lane; only the reason had moved.
//
// The harness package has no TestMain. Here `go test -tags integration
// ./test/integration/harness/` runs these in milliseconds as any user,
// which is what the factoring was for.
//
// This is not hypothetical bookkeeping. Every function below was
// untested when it moved, and one of them was WRONG -- see
// GlobalInSubnet.

// V6Addr is one `inet6` entry with the flag text and lifetimes that
// belong to it.
type V6Addr struct {
	CIDR     string
	Flags    string // scope + any of tentative/dadfailed/deprecated/dynamic
	ValidLft string
	PrefLft  string
}

// ParseV6Addrs pairs each `inet6` line with the `valid_lft` line the
// kernel prints under it.
//
// Measured 2026-08-28 that this is parseable from the STOCK test
// image: alpine:3.20's `ip` is busybox, and busybox does print the
// lifetime fields and does render finite values --
//
//	inet 10.99.0.1/24 scope global dynamic eth0
//	   valid_lft 300sec preferred_lft 200sec
//
// -- so no iproute2 install is needed and the container under test
// stays the one every other test uses. The `dynamic` flag in that
// output is load-bearing here and not decoration: the kernel sets it
// on an address that carries a lifetime, so its ABSENCE beside
// `valid_lft forever` is what distinguishes an address libnetwork
// applied statically from one the kernel is ageing.
func ParseV6Addrs(out string) []V6Addr {
	var addrs []V6Addr
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "inet6" {
			addrs = append(addrs, V6Addr{CIDR: f[1], Flags: strings.Join(f[2:], " ")})
			continue
		}
		if len(f) >= 2 && f[0] == "valid_lft" && len(addrs) > 0 {
			a := &addrs[len(addrs)-1]
			a.ValidLft = f[1]
			if len(f) >= 4 && f[2] == "preferred_lft" {
				a.PrefLft = f[3]
			}
		}
	}
	return addrs
}

// GlobalInSubnet picks the address under test: a global-scope address
// that NUMERICALLY falls inside the fixture's own subnet. Keyed on the
// subnet rather than on "the first global address" so a leaked address
// from another segment cannot be mistaken for this server's.
//
// IT COMPARES ADDRESSES, NOT TEXT, AND THAT IS THE WHOLE POINT. This
// was written as strings.HasPrefix(a.CIDR, "fd00:6470:6865::") and was
// WRONG -- not fragile, wrong, on the arm the release bar is about.
//
// MEASURED 2026-08-28, alpine:3.20 busybox `ip`, addresses added by
// hand and read back:
//
//	fd00:6470:6865::89/128                  -> "fd00:6470:6865::89/128"
//	fd00:6470:6865:0:42:acff:fe11:2/64      -> "fd00:6470:6865:0:42:acff:fe11:2/64"
//
// Both are in the same /64. The second is NOT compressed, because
// RFC 5952 2.2 forbids "::" over a single zero group, and a SLAAC
// interface identifier -- EUI-64 from a container MAC, or a
// stable-privacy one -- essentially never contains two adjacent zero
// groups. So the prefix test matched the DHCPv6 pool address, which is
// low-numbered and does compress, and missed the SLAAC address every
// time. The self-configuring arms would have reported "no global IPv6
// address ever appeared" -- naming a product defect while measuring a
// text-rendering convention. That is the same class as the wrong
// device name this branch already had to correct once.
//
// A malformed or unparseable entry is skipped rather than matched: an
// address this cannot read is not an address it may claim to have
// found.
func GlobalInSubnet(addrs []V6Addr, subnetCIDR string) (V6Addr, bool) {
	_, subnet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return V6Addr{}, false
	}
	for _, a := range addrs {
		if !strings.Contains(a.Flags, "global") {
			continue
		}
		host, _, _ := strings.Cut(a.CIDR, "/")
		ip := net.ParseIP(strings.TrimSpace(host))
		if ip == nil {
			continue
		}
		if subnet.Contains(ip) {
			return a, true
		}
	}
	return V6Addr{}, false
}

// HasDefaultV6Route reports whether `ip -6 route show` output carries a
// default route.
func HasDefaultV6Route(routeOut string) bool {
	for _, line := range strings.Split(routeOut, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "default") {
			return true
		}
	}
	return false
}

// FirstNonLoopback picks the interface name out of `ip -o link show`
// output. Shared so the in-container and the nsenter callers cannot
// drift apart.
//
// The plugin does not always produce "eth0". pkg/plugin/network.go
// builds the Join response's DstPrefix from the endpoint mode: a
// parent-attached endpoint (macvlan/ipvlan) gets "eth", so libnetwork
// names the link eth0 -- but a BRIDGE endpoint gets the bridge name as
// the prefix, and the link inside the container is named "<bridge>0".
// The v6 fixtures are bridges.
//
// This mattered: every v6 sample used to read "dev eth0", `ip`
// answered "can't find device", and the assertions reported that as
// "no global IPv6 address on the interface" -- a red that named a
// product defect and measured only a wrong device name.
//
// Measured 2026-08-28 against alpine:3.20's busybox, which prints the
// whole record on one line with a literal backslash separator:
//
//	1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 ... \    link/loopback ...
//	1817: eth0@if1818: <BROADCAST,...> mtu 1500 ... \    link/ether ...
func FirstNonLoopback(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		name := strings.TrimSpace(rest)
		if i := strings.IndexAny(name, ":@"); i >= 0 {
			name = name[:i]
		}
		if name = strings.TrimSpace(name); name == "" || name == "lo" {
			continue
		}
		return name, true
	}
	return "", false
}

// LinkLocal returns an IPv6 link-local address (fe80::/10) from a
// parsed `ip -6 addr` dump, if there is one.
//
// IT EXISTS TO SEPARATE TWO REDS THAT LOOK IDENTICAL. On a stateless or
// SLAAC segment the endpoint carries no IPv6 address, and the engine
// disables IPv6 outright on a sandbox interface whose endpoint has
// none. The plugin clears that switch before starting its DHCPv6 client
// (#868); if the clear ever fails, the container has NO IPv6 on that
// interface at all.
//
// "No global address in the advertised prefix" is then the symptom of
// both #868's enable failing and #875's advertisements not being
// processed, and they are different bugs with different owners. A
// link-local is the discriminator: the kernel forms one on any
// IPv6-enabled interface without needing a router, so its presence says
// IPv6 is on and the missing global address is about advertisements,
// and its absence says IPv6 was never enabled and nothing downstream of
// that is a statement about #875.
//
// Scope rather than prefix text: a link-local renders fe80:: with
// several zero groups compressed, so a string test would be a second
// instance of the RFC 5952 trap GlobalInSubnet already fell into.
func LinkLocal(addrs []V6Addr) (V6Addr, bool) {
	for _, a := range addrs {
		host, _, _ := strings.Cut(a.CIDR, "/")
		ip := net.ParseIP(strings.TrimSpace(host))
		if ip == nil {
			continue
		}
		if ip.To4() == nil && ip.IsLinkLocalUnicast() {
			return a, true
		}
	}
	return V6Addr{}, false
}

// ParseLft turns `ip`'s lifetime field into seconds. "forever" is
// reported as not-finite rather than as a large number, because the two
// mean different things here: a finite lifetime is the kernel ageing an
// address it learned from an advertisement, and `forever` is an address
// somebody applied statically.
func ParseLft(s string) (int, bool) {
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

// V6RefreshFloor is how much the lifetime ceiling must rise across the
// observation window before it counts as a refresh.
//
// The ceiling is preferred_lft + elapsed (see AssertV6LifetimeRefreshed).
// Its noise is the sum of two roundings -- `ip` prints whole seconds,
// and the elapsed clock is read separately -- so it is bounded by about
// two seconds. Ten is comfortably clear of that and far below the tens
// of seconds a real refresh moves it by, so this is a discriminator and
// not a threshold anybody has to tune.
const V6RefreshFloor = 10

// V6ObserveWindow is how long a container is observed for #875's
// durability claims, and V6ObserveInterval how often.
//
// The window has to contain at least one router advertisement AFTER
// the first, because the whole discrimination is "did a later
// advertisement move the lifetime". The fixture pins the advertisement
// interval at 30s (see raParam), so 150s contains four with margin. If
// the cadence were ever longer than this window the verdict could not
// tell a refresh from a countdown, and it says so in its own failure
// text rather than reporting the defect -- see the vacuity guard and
// the too-short caveat in AssertV6LifetimeRefreshed.
const (
	V6ObserveWindow   = 150 * time.Second
	V6ObserveInterval = 10 * time.Second
)

// v6MinAdvertsInWindow is how many advertisements AFTER the first the
// window must be able to contain.
//
// One is the logical minimum -- a refresh needs a second advertisement
// to exist at all -- and one leaves no margin for a lost multicast, so
// the window is required to hold two beyond the first. This is the
// invariant that stops the window being tuned downward until the
// verdict cannot see a refresh and reports the defect either way.
const v6MinAdvertsInWindow = 2

// V6LftReading is one observation of a container's `ip -6 addr` output
// at a known elapsed time. It is deliberately the whole UNFILTERED
// output rather than a parsed address: no device is named anywhere in
// the sampling path, so a wrong device name cannot masquerade as a
// missing address.
type V6LftReading struct {
	Elapsed time.Duration
	Addr    string
}

// V6Reporter is the subset of *testing.T that AssertV6LifetimeRefreshed
// uses.
//
// IT IS AN INTERFACE SO THE VERDICT CAN BE DRIVEN DIRECTLY, against
// cases whose answer is known in advance, rather than only through a
// container. A *testing.T satisfies it, so callers are unchanged.
type V6Reporter interface {
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
	Helper()
}

// AssertV6LifetimeRefreshed is the central claim of #875's
// self-configuring arm.
//
// THE LEDGER, AND WHY IT IS SHAPED LIKE THIS. A SLAAC address's
// lifetime is reset to the advertised prefix lifetime by every
// advertisement the kernel accepts, and counts down in between. So the
// lifetime ALONE cannot answer the question: a sample taken just
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
// this verdict and its subject for no benefit.
func AssertV6LifetimeRefreshed(t V6Reporter, samples []V6LftReading, mode V6Mode, subnetCIDR string, window time.Duration) {
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
		a, ok := GlobalInSubnet(ParseV6Addrs(s.Addr), subnetCIDR)
		if !ok {
			continue
		}
		lft, finite := ParseLft(a.ValidLft)
		pref, prefFinite := ParseLft(a.PrefLft)
		if !finite || !prefFinite {
			// An address with no lifetime at all is not a
			// SLAAC address. On this segment nothing should be
			// applying one statically, so say what was found
			// rather than silently having nothing to measure.
			t.Errorf("on a %s segment the address %s has valid_lft=%q preferred_lft=%q, so "+
				"it is not an address the kernel is ageing from a router advertisement. "+
				"Something applied it statically; that is not how this segment hands "+
				"out addresses", mode, a.CIDR, a.ValidLft, a.PrefLft)
			return
		}
		e := int(s.Elapsed.Seconds())
		pts = append(pts, point{s.Elapsed, lft, pref, lft + e, pref + e})
	}

	// VACUITY GUARD. Everything below is a claim about a sequence of
	// observations; with fewer than two there is no sequence and the
	// comparison is trivially satisfied. This is the shape that turns
	// "the lifetime refreshes" into a statement about nothing -- a
	// container that lost its address entirely would otherwise reach
	// the end of this function without a single Errorf.
	if len(pts) < 2 {
		t.Errorf("only %d of %d samples on the %s segment had a global address in the "+
			"fixture's subnet %s, so there is no sequence to test for a refresh. "+
			"The address is not merely failing to refresh, it is absent",
			len(pts), len(samples), mode, subnetCIDR)
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
	windowMayBeTooShort := first.pref > int(window.Seconds())

	if rise < V6RefreshFloor {
		caveat := ""
		if windowMayBeTooShort {
			caveat = fmt.Sprintf("\n\nBEFORE WIDENING THE WINDOW: the initial preferred lifetime is %ds, "+
				"longer than the %s observed here, so a segment whose advertisement "+
				"cadence also exceeded the window would look identical from inside this "+
				"check. S2 is what separates those — it requires the server to have "+
				"advertised at least twice in the same window. If S2 passed and this "+
				"failed, advertisements were sent and the container did not act on them, "+
				"which is the defect. Widening the window to make this pass is how the "+
				"test stops measuring anything.", first.pref, window)
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
			mode, rise, window, V6RefreshFloor,
			first.pref, last.pref, validRise, first.valid, caveat)
	}
}
