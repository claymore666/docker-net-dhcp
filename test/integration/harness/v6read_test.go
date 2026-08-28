// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The check on the check.
//
// AssertV6LifetimeRefreshed is the entire discrimination in #875's
// self-configuring arm: everything else in that test is plumbing, and
// this one function decides whether a container's address is being
// refreshed or is quietly counting down to nothing. Reaching it through
// TestDHCPv6_SelfConfiguring_AddressAndRouteSurvive needs root, a
// bridge, a dnsmasq and a container, and takes five minutes.
//
// The readers around it are in the same position and were in a worse
// one: FirstNonLoopback, ParseV6Addrs and GlobalInSubnet had NO test at
// all, and FirstNonLoopback exists only because a wrong device name
// once turned every v6 assertion into a red that named a product defect
// and measured a typo. GlobalInSubnet was wrong when this file was
// written -- see TestGlobalInSubnet_SLAACRenderingIsNotCompressed.
//
// These run as any user, in milliseconds, before anything is touched.

// ---------------------------------------------------------------------
// The ledger verdict
// ---------------------------------------------------------------------

// lftPoint is one synthetic reading: elapsed seconds, valid_lft,
// preferred_lft. A negative lifetime means `forever`.
type lftPoint struct{ elapsed, valid, pref int }

// synthAddrOutput renders one sample the way busybox `ip -6 addr` does.
//
// The address is a REAL SLAAC rendering, not a convenient one: an
// EUI-64 interface identifier from a container MAC, printed
// uncompressed because it contains a single zero group. Measured
// 2026-08-28 from alpine:3.20. Using the compressible pool form here
// would have hidden the GlobalInSubnet defect from this very table.
// slaacRendering is DERIVED from the fixture's own prefix rather than
// typed out, so a prefix change cannot leave this table testing a
// subnet the fixture no longer uses. V6Prefix ends in "::" -- the
// compressed spelling of the all-zero host part -- and a real SLAAC
// address fills those low 64 bits, so the "::" opens back up.
var slaacRendering = strings.TrimSuffix(V6Prefix, ":") + "0:42:acff:fe11:2"

func synthAddrOutput(p lftPoint) string {
	slaac := slaacRendering
	if p.valid < 0 {
		return "    inet6 " + slaac + "/64 scope global \n" +
			"       valid_lft forever preferred_lft forever\n"
	}
	return fmt.Sprintf(
		"    inet6 "+slaac+"/64 scope global dynamic mngtmpaddr proto kernel_ra \n"+
			"       valid_lft %dsec preferred_lft %dsec\n", p.valid, p.pref)
}

func synthSamples(series []lftPoint) []V6LftReading {
	var out []V6LftReading
	for _, p := range series {
		out = append(out, V6LftReading{
			Elapsed: time.Duration(p.elapsed) * time.Second,
			Addr:    synthAddrOutput(p),
		})
	}
	return out
}

// recordingReporter stands in for *testing.T so a verdict can be read
// rather than propagated. Driving the real T would fail THIS test on
// every row whose expected outcome is a failure.
type recordingReporter struct {
	failed bool
	msgs   []string
}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.failed = true
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recordingReporter) Logf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recordingReporter) Helper() {}

func TestSelfConfigLedger_DiscriminatesRefreshFromCountdown(t *testing.T) {
	cases := []struct {
		name     string
		series   []lftPoint
		wantFail bool
		why      string
	}{
		{
			name:     "the measured defect series counts down",
			series:   []lftPoint{{0, 1797, 1797}, {10, 1787, 1787}, {150, 1647, 1647}},
			wantFail: true,
			why: "this is the dhcpcd arm measured in #875. The lifetime falls exactly as " +
				"fast as the clock rises, so the ceiling is flat and nothing is refreshing " +
				"the address",
		},
		{
			name:     "the measured control series refreshes",
			series:   []lftPoint{{0, 1797, 1797}, {10, 1796, 1796}, {150, 1705, 1705}},
			wantFail: false,
			why: "the no-dhcpcd control from #875. Read WITHIN the arm -- which is the only " +
				"way it can be read -- its ceiling rises 1797 -> 1806 -> 1855, so an " +
				"advertisement reset it at least once. This row and the one above it are " +
				"the two the issue could not tell apart by comparing them to each other",
		},
		{
			name:     "a literal increase is a refresh",
			series:   []lftPoint{{0, 1790, 1790}, {30, 1760, 1760}, {60, 1795, 1795}},
			wantFail: false,
			why:      "the unambiguous case: the value itself goes back up",
		},
		{
			name:     "one second of sampling jitter is not a refresh",
			series:   []lftPoint{{0, 1797, 1797}, {10, 1788, 1788}, {150, 1648, 1648}},
			wantFail: true,
			why: "the ceiling moves by 1s, which is rounding between `ip`'s whole seconds " +
				"and a separately-read clock. V6RefreshFloor exists so this is not read " +
				"as a refresh; a floor of 0 would pass this row and pass the defect too",
		},
		{
			name:     "preferred refreshes while valid is pinned",
			series:   []lftPoint{{0, 7200, 1790}, {30, 7170, 1760}, {60, 7140, 1795}},
			wantFail: false,
			why: "RFC 4862 5.5.3(e) resets the preferred lifetime unconditionally but resets " +
				"the valid lifetime only under its two conditions. This row is why the " +
				"verdict is keyed on preferred: keyed on valid, it would report a defect " +
				"on a segment that is refreshing correctly",
		},
		{
			name:     "no address in any sample is not a silent pass",
			series:   nil,
			wantFail: true,
			why: "the vacuity direction. With no readings there is no sequence, and a " +
				"function that returned quietly here would report a container that had " +
				"LOST its address as a container whose address is fine",
		},
		{
			name:     "a single sample is not a sequence",
			series:   []lftPoint{{0, 1797, 1797}},
			wantFail: true,
			why:      "one reading cannot show a change; the comparison would be trivially satisfied",
		},
		{
			name:     "a statically applied address is reported, not measured",
			series:   []lftPoint{{0, -1, -1}, {10, -1, -1}},
			wantFail: true,
			why: "`forever` means nobody is ageing this address, so it did not come from an " +
				"advertisement. On a self-configuring segment that is itself wrong, and it " +
				"must not be silently unmeasurable",
		},
	}

	// NON-VACUITY, and it is the reason this file exists at all: a
	// table with no failing row and no passing row does not test a
	// discriminator, it tests a constant. Both polarities are required,
	// and the two MEASURED series must both be present -- they are the
	// pair the whole issue turns on, and a table that dropped either
	// would still look like a thorough test.
	pol := map[bool]int{}
	for _, tc := range cases {
		pol[tc.wantFail]++
	}
	if pol[true] < 1 || pol[false] < 1 {
		t.Fatalf("the table has %d rows expecting a verdict of FAIL and %d expecting PASS; "+
			"both are needed, or this file cannot tell a discriminator from a function "+
			"that always returns the same answer", pol[true], pol[false])
	}
	for _, needed := range []string{
		"the measured defect series counts down",
		"the measured control series refreshes",
	} {
		found := false
		for _, tc := range cases {
			if tc.name == needed {
				found = true
			}
		}
		if !found {
			t.Fatalf("the row %q is missing. The two measured series from #875 are the pair "+
				"this ledger exists to tell apart; a table without both is not checking "+
				"the claim the issue rests on", needed)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingReporter{}
			AssertV6LifetimeRefreshed(r, synthSamples(tc.series), V6SLAAC,
				V6SubnetV6CIDR, V6ObserveWindow)
			if r.failed != tc.wantFail {
				verdict, want := "PASS", "FAIL"
				if r.failed {
					verdict, want = "FAIL", "PASS"
				}
				t.Errorf("the ledger returned %s and should have returned %s.\n\nWhy this row "+
					"exists: %s\n\nWhat it reported:\n%s",
					verdict, want, tc.why, strings.Join(r.msgs, "\n"))
			}
		})
	}
}

// ---------------------------------------------------------------------
// The readers the verdict rests on
// ---------------------------------------------------------------------

// TestGlobalInSubnet_SLAACRenderingIsNotCompressed pins the defect this
// file was written to catch, and it is the reason the readers moved.
//
// GlobalInSubnet was strings.HasPrefix(cidr, "fd00:6470:6865::"). The
// DHCPv6 pool address fd00:6470:6865::89 matches it, because three zero
// groups in a row compress to "::". A SLAAC address does not: its
// interface identifier fills the low 64 bits, leaving ONE zero group,
// and RFC 5952 2.2 forbids "::" over a single group -- so it renders
// fd00:6470:6865:0:42:acff:fe11:2 and the prefix test says no.
//
// MEASURED 2026-08-28, alpine:3.20 busybox `ip`, both addresses added
// to lo by hand and read back verbatim. The consequence on the branch
// as written: both self-configuring arms would report "no global IPv6
// address ever appeared", which is a red naming a product defect while
// measuring a text convention.
func TestGlobalInSubnet_SLAACRenderingIsNotCompressed(t *testing.T) {
	const measured = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 state UNKNOWN qlen 1000
    inet6 fd00:6470:6865:0:42:acff:fe11:2/64 scope global dynamic
       valid_lft 1797sec preferred_lft 1797sec
    inet6 fe80::42:acff:fe11:2/64 scope link
       valid_lft forever preferred_lft forever
`
	a, ok := GlobalInSubnet(ParseV6Addrs(measured), V6SubnetV6CIDR)
	if !ok {
		t.Fatalf("the SLAAC address in this MEASURED busybox output was not found in %s. "+
			"It is in that /64; only its rendering differs from the pool address's. A "+
			"reader that misses it turns both self-configuring arms of #875 into a red "+
			"that measures nothing:\n%s", V6SubnetV6CIDR, measured)
	}
	if a.CIDR != "fd00:6470:6865:0:42:acff:fe11:2/64" {
		t.Errorf("picked %q, want the global SLAAC address", a.CIDR)
	}
	// The one it must NOT pick, in the same output: the link-local is
	// numerically outside the subnet and is not scope global.
	if strings.Contains(a.Flags, "link") {
		t.Errorf("picked a link-scope address (%q); a link-local is never the address "+
			"under test", a.Flags)
	}
}

func TestGlobalInSubnet(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string // "" means: must find nothing
		why  string
	}{
		{
			name: "the compressible DHCPv6 pool address",
			out: "    inet6 fd00:6470:6865::89/128 scope global \n" +
				"       valid_lft forever preferred_lft forever\n",
			want: "fd00:6470:6865::89/128",
			why:  "the managed arm's address, a /128 inside the fixture's /64",
		},
		{
			name: "the uncompressed SLAAC address",
			out: "    inet6 fd00:6470:6865:0:42:acff:fe11:2/64 scope global dynamic \n" +
				"       valid_lft 1797sec preferred_lft 1797sec\n",
			want: "fd00:6470:6865:0:42:acff:fe11:2/64",
			why:  "same /64, different rendering; the string-prefix version missed this",
		},
		{
			name: "an address from ANOTHER fixture's segment",
			out: "    inet6 fd00:6470:6864::12/64 scope global \n" +
				"       valid_lft 100sec preferred_lft 100sec\n",
			want: "",
			why: "the bridge fixture's prefix. Selecting it would let a leaked address " +
				"from another segment stand in for this server's",
		},
		{
			name: "a link-local is not the address under test",
			out: "    inet6 fe80::42:acff:fe11:2/64 scope link \n" +
				"       valid_lft forever preferred_lft forever\n",
			want: "",
			why:  "outside the subnet AND not global; both reasons must hold it out",
		},
		{
			name: "a global address that is not parseable is skipped, not matched",
			out: "    inet6 not-an-address/64 scope global \n" +
				"       valid_lft 10sec preferred_lft 10sec\n",
			want: "",
			why: "an entry the reader cannot parse is not an entry it may claim to have " +
				"found; folding the parse error into a match is how a garbage line " +
				"becomes a pass",
		},
		{
			// The ONLY shape that separates the two conditions this
			// reader ANDs together. Every other row is held out by
			// containment alone -- a link-local is outside the /64
			// by construction -- so without this row the scope test
			// is a branch with one reachable verdict and could be
			// deleted with every test still green. `ip` will print
			// exactly this for an in-subnet address somebody added
			// with an explicit scope, which is legal.
			name: "an in-subnet address that is not global scope",
			out: "    inet6 fd00:6470:6865::7/64 scope link \n" +
				"       valid_lft forever preferred_lft forever\n",
			want: "",
			why: "containment cannot hold this out -- it is in the fixture's own /64. " +
				"Only the scope test can, and a reader that returned it would hand " +
				"the verdict an address that is not usable as a global source",
		},
		{
			name: "the subnet's own boundary addresses are inside it",
			out: "    inet6 fd00:6470:6865:0:ffff:ffff:ffff:ffff/64 scope global \n" +
				"       valid_lft 10sec preferred_lft 10sec\n",
			want: "fd00:6470:6865:0:ffff:ffff:ffff:ffff/64",
			why:  "the top of the /64; containment, not a numeric range somebody typed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := GlobalInSubnet(ParseV6Addrs(tc.out), V6SubnetV6CIDR)
			got := ""
			if ok {
				got = a.CIDR
			}
			if got != tc.want {
				t.Errorf("got %q, want %q.\n\nWhy this row exists: %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestGlobalInSubnet_UnreadableSubnetRefuses drives the failure
// direction of the reader's own parameter. A subnet it cannot parse
// must find nothing, not everything: the alternative is a typo in a
// constant turning every assertion green.
func TestGlobalInSubnet_UnreadableSubnetRefuses(t *testing.T) {
	out := "    inet6 fd00:6470:6865::89/128 scope global \n" +
		"       valid_lft forever preferred_lft forever\n"
	if _, ok := GlobalInSubnet(ParseV6Addrs(out), "not-a-cidr"); ok {
		t.Errorf("an unparseable subnet matched an address. An error folded into a value " +
			"has no direction; this one must refuse")
	}
}

func TestParseV6Addrs(t *testing.T) {
	// MEASURED 2026-08-28 from alpine:3.20 busybox `ip -6 addr show lo`
	// with two addresses added by hand.
	const measured = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 state UNKNOWN qlen 1000
    inet6 fd00:6470:6865::89/128 scope global 
       valid_lft forever preferred_lft forever
    inet6 fd00:6470:6865:0:42:acff:fe11:2/64 scope global dynamic 
       valid_lft 1797sec preferred_lft 1787sec
    inet6 ::1/128 scope host 
       valid_lft forever preferred_lft forever
`
	got := ParseV6Addrs(measured)
	if len(got) != 3 {
		t.Fatalf("parsed %d addresses, want 3:\n%#v", len(got), got)
	}
	if got[0].CIDR != "fd00:6470:6865::89/128" || got[0].ValidLft != "forever" || got[0].PrefLft != "forever" {
		t.Errorf("first entry parsed as %#v", got[0])
	}
	if got[1].ValidLft != "1797sec" || got[1].PrefLft != "1787sec" {
		t.Errorf("the lifetime line did not attach to the address above it: %#v", got[1])
	}
	if !strings.Contains(got[1].Flags, "dynamic") {
		t.Errorf("the `dynamic` flag was dropped (%q). Its ABSENCE beside `valid_lft "+
			"forever` is what distinguishes a statically applied address from one the "+
			"kernel is ageing, so it has to survive the parse", got[1].Flags)
	}
	// The failure direction: a lifetime line with no address above it
	// must not attach itself to anything.
	if orphan := ParseV6Addrs("       valid_lft 10sec preferred_lft 10sec\n"); len(orphan) != 0 {
		t.Errorf("a lifetime line with no address above it produced %#v", orphan)
	}
}

func TestParseLft(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		finite bool
	}{
		{"1797sec", 1797, true},
		{" 0sec ", 0, true},
		{"forever", 0, false},
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, tc := range cases {
		got, finite := ParseLft(tc.in)
		if got != tc.want || finite != tc.finite {
			t.Errorf("ParseLft(%q) = (%d, %v), want (%d, %v)", tc.in, got, finite, tc.want, tc.finite)
		}
	}
	// `forever` and `0sec` must not be the same answer: one is an
	// address nobody is ageing, the other is one that has just expired.
	if _, a := ParseLft("forever"); a {
		t.Errorf("`forever` reported as a finite lifetime")
	}
	if _, b := ParseLft("0sec"); !b {
		t.Errorf("`0sec` reported as no lifetime at all")
	}
}

func TestFirstNonLoopback(t *testing.T) {
	// MEASURED 2026-08-28, alpine:3.20 busybox `ip -o link show`,
	// verbatim including the literal backslash busybox emits.
	const measured = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
1817: eth0@if1818: <BROADCAST,MULTICAST,UP,LOWER_UP,M-DOWN> mtu 1500 qdisc noqueue state UP \    link/ether 02:42:ac:11:00:05 brd ff:ff:ff:ff:ff:ff`
	if got, ok := FirstNonLoopback(measured); !ok || got != "eth0" {
		t.Errorf("FirstNonLoopback(measured parent-attached output) = (%q, %v), want (eth0, true)", got, ok)
	}

	// The case the function EXISTS for: a bridge endpoint's link is
	// named after the bridge, not eth0. Assuming eth0 is what made
	// every v6 assertion red for a wrong device name.
	const bridged = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
7: dh-itest-br60@if8: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP \    link/ether 02:42:ac:11:00:06 brd ff:ff:ff:ff:ff:ff`
	if got, ok := FirstNonLoopback(bridged); !ok || got != "dh-itest-br60" {
		t.Errorf("FirstNonLoopback(bridge output) = (%q, %v), want (dh-itest-br60, true); "+
			"a bridge endpoint's link is named after the bridge", got, ok)
	}

	// The failure direction. Loopback-only, and empty, must both be a
	// refusal -- the caller fails the test on !ok, and a silent "eth0"
	// fallback is the defect this replaced.
	const loOnly = `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00`
	if got, ok := FirstNonLoopback(loOnly); ok {
		t.Errorf("loopback-only output yielded %q; there is no interface to sample", got)
	}
	if got, ok := FirstNonLoopback(""); ok {
		t.Errorf("empty output yielded %q", got)
	}
}

func TestHasDefaultV6Route(t *testing.T) {
	const withDefault = `fd00:6470:6865::/64 dev dh-itest-br60 proto kernel metric 256 expires 1799sec
default via fe80::1 dev dh-itest-br60 proto ra metric 1024 expires 1799sec`
	if !HasDefaultV6Route(withDefault) {
		t.Errorf("a default route was present and not seen")
	}

	// The defect's shape, MEASURED on the managed segment at t+150s in
	// run 33144952381: the on-link prefix route survives and the
	// default route is gone. A reader keyed on "any route at all"
	// would call this healthy.
	const prefixOnly = `fd00:6470:6865::/64 dev dh-itest-br60 proto kernel metric 256 expires 1799sec
fe80::/64 dev dh-itest-br60 proto kernel metric 256`
	if HasDefaultV6Route(prefixOnly) {
		t.Errorf("a table with an on-link prefix route and NO default route was read as " +
			"having one. That table IS the #875 defect")
	}
	if HasDefaultV6Route("") {
		t.Errorf("empty route output was read as having a default route")
	}
	// A route TO a host whose name merely begins with the word must
	// not count; anchoring is on the line, not on a substring.
	if HasDefaultV6Route("fd00:6470:6865::1 dev x  # default gateway") {
		t.Errorf("a prose mention of the word satisfied the check")
	}
}

// TestV6ObserveWindowCoversTheAdvertisementCadence is the guard on the
// window itself.
//
// The window is a SAMPLING parameter, so no table of synthetic series
// can see it being shortened: the rows carry their own elapsed times
// and the verdict returns the same answers. Shrinking it below the
// fixture's advertisement cadence would leave a healthy segment
// producing a flat ceiling -- indistinguishable, from inside the
// verdict, from the defect -- and the test would report #875 on a
// segment that works. That is a false red on the release bar itself.
//
// So the relationship is checked here, DERIVED from the interval the
// fixture actually pins in raParam rather than from a second copy of
// the number. Both halves are checked: too short is the dangerous
// direction, and a window with no upper relation to the cadence is how
// a five-minute test becomes a fifty-minute one.
func TestV6ObserveWindowCoversTheAdvertisementCadence(t *testing.T) {
	interval := time.Duration(v6RAIntervalSec) * time.Second
	need := interval * time.Duration(v6MinAdvertsInWindow+1)
	if V6ObserveWindow < need {
		t.Errorf("the observation window is %s but the fixture advertises every %s, so it "+
			"can contain fewer than %d advertisements after the first. A healthy "+
			"segment would then show a flat lifetime ceiling and be reported as the "+
			"#875 defect: want a window of at least %s",
			V6ObserveWindow, interval, v6MinAdvertsInWindow, need)
	}
	if V6ObserveInterval >= V6ObserveWindow {
		t.Errorf("the sampling interval %s is not shorter than the window %s, so there is "+
			"at most one sample and no sequence to compare", V6ObserveInterval, V6ObserveWindow)
	}
	// The advertised Router Lifetime must outlast the window by a wide
	// margin, or a default route that merely AGED OUT would be read as
	// one that was removed -- the whole point of pinning it.
	if lifetime := time.Duration(v6RALifetimeSec) * time.Second; lifetime < 10*V6ObserveWindow {
		t.Errorf("the advertised Router Lifetime is %s against a %s window. Pinning it is "+
			"what makes 'the route disappeared' distinguishable from 'the route expired'; "+
			"at this ratio the two are not separable", lifetime, V6ObserveWindow)
	}
}
