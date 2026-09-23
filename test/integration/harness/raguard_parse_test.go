// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"testing"
	"time"
)

// Fixture strings are verbatim from alpine:3.20, the suite's container image, or from the failing CI run; busybox
// emits no `proto ra` field, which an iproute2-validated observer relied on (#875).

// The container's own `ip -6 route show default` from that CI run: no `proto` field, busybox's double spaces.
const ciRouteShowDefault = `default via fe80::8ab:aaff:fe85:2df5 dev dh-itest-br20  metric 1024  expires 0sec`

// Captured from alpine:3.20, `ip -6 -o addr show scope global`.
const busyboxAddrShow = `2: dh-itest-br20    inet6 fd00:6470:6864::42/64 scope global \       valid_lft forever preferred_lft forever`

func TestV6IfaceFromAddrShow(t *testing.T) {
	if got := V6IfaceFromAddrShow(busyboxAddrShow, "fd00:6470:6864::42"); got != "dh-itest-br20" {
		t.Errorf("busybox addr show: got %q, want %q — the observer would read sysctls "+
			"from the wrong path and measure nothing (#875)", got, "dh-itest-br20")
	}
	// The interface is not eth0; assuming it gave three "No such file or directory" reads in CI.
	if got := V6IfaceFromAddrShow(busyboxAddrShow, "fd00:6470:6864::42"); got == "eth0" {
		t.Error("derived eth0; that hardcoded guess is exactly what failed in CI")
	}
	if got := V6IfaceFromAddrShow(busyboxAddrShow, "fd00:dead::1"); got != "" {
		t.Errorf("invented interface %q for an absent address", got)
	}
	if got := V6IfaceFromAddrShow("", "fd00:6470:6864::42"); got != "" {
		t.Errorf("invented interface %q from empty output", got)
	}
	// #875: a prefix of a present address is not that address. Two global addresses on one link make it reachable:
	// a multi-network container, SLAAC (#818) or an operator's address, even with #821's autoconf=0.
	if got := V6IfaceFromAddrShow(busyboxAddrShow, "fd00:6470:6864::4"); got != "" {
		t.Errorf("matched %q on a PREFIX of a present address; the observer would then "+
			"read sysctls from the wrong interface and report what it found there", got)
	}
	const twoAddrs = "" +
		"2: eth0    inet6 fd00:6470:6864::32/128 scope global \\    valid_lft forever preferred_lft forever\n" +
		"3: dh-itest-br20    inet6 fd00:6470:6864::3/64 scope global \\    valid_lft 1800sec preferred_lft 1800sec\n"
	if got := V6IfaceFromAddrShow(twoAddrs, "fd00:6470:6864::3"); got != "dh-itest-br20" {
		t.Errorf("got %q, want %q: the shorter address must not be answered by the line "+
			"that merely CONTAINS it", got, "dh-itest-br20")
	}
	if got := V6IfaceFromAddrShow(twoAddrs, "fd00:6470:6864::32"); got != "eth0" {
		t.Errorf("got %q, want %q on an exact address that IS present", got, "eth0")
	}
}

func TestHasLinkLocalDefaultRoute(t *testing.T) {
	if !HasLinkLocalDefaultRoute(ciRouteShowDefault) {
		t.Error("the real CI route line was not recognised as an RA-derived default " +
			"route; this is the exact string the `proto ra` version failed on")
	}
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"no default route at all", "fe80::/64 dev eth0  metric 256", false},
		{"default via a GLOBAL next hop is not RA-derived", "default via fd00:6470:6864::1 dev eth0  metric 1024", false},
		{"full iproute2 rendering still matches", "default via fe80::1 dev eth0 proto ra metric 1024 expires 1780sec", true},
	} {
		if got := HasLinkLocalDefaultRoute(tc.in); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSysctlReadFailed(t *testing.T) {
	// Verbatim from alpine:3.20 when the path does not exist.
	if !SysctlReadFailed(`cat: can't open '/proc/sys/net/ipv6/conf/eth0/accept_ra': No such file or directory`) {
		t.Error("a failed read was scored as a value; the observer would report a " +
			"wrong-value failure, or a pass, for an assertion that never ran")
	}
	if !SysctlReadFailed("") {
		t.Error("empty output is not a measurement")
	}
	for _, ok := range []string{"2", "1", "0", " 2\n"} {
		if SysctlReadFailed(ok) {
			t.Errorf("real sysctl value %q scored as a failed read", ok)
		}
	}
}

// Verbatim DHCPREPLY lines from the CI fixture's dnsmasq in the run that exposed the RA-assertion ordering bug (#875).
const (
	ciBindBridge  = `Aug 28 13:57:39 dnsmasq-dhcp[6902]: 3874478 DHCPREPLY(dh-itest-br2) fd00:6470:6864::32 00:03:00:01:ea:eb:ed:a4:b0:f5 `
	ciBindMacvlan = `Aug 28 13:57:33 dnsmasq-dhcp[6947]: 4883247 DHCPREPLY(dh-itest-dhcp) fd00:6470:6863::91 00:03:00:01:26:54:5f:ae:24:20`
	ciSolicit     = `Aug 28 13:57:32 dnsmasq-dhcp[6947]: 6042079 sent size: 40 option:  3 ia-na  IAID=1605248032 T1=60 T2=105`
)

func TestCountDHCPv6Binds_CountsARealBind(t *testing.T) {
	if got := CountDHCPv6Binds(ciBindBridge, "fd00:6470:6864::32"); got != 1 {
		t.Errorf("bind for the address: got %d, want 1", got)
	}
	if got := CountDHCPv6Binds(ciBindBridge+"\n"+ciBindBridge, "fd00:6470:6864::32"); got != 2 {
		t.Errorf("two binds: got %d, want 2", got)
	}
}

func TestCountDHCPv6Binds_ADifferentMACOnTheSameAddressDoesNotCount(t *testing.T) {
	const otherClient = `Aug 28 13:40:01 dnsmasq-dhcp[6902]: 1111111 DHCPREPLY(dh-itest-br2) fd00:6470:6864::32 00:03:00:01:aa:bb:cc:dd:ee:ff`

	mine := "ea:eb:ed:a4:b0:f5"
	log := otherClient + "\n" + ciBindBridge

	if got := CountDHCPv6Binds(log, "fd00:6470:6864::32"); got != 2 {
		t.Fatalf("precondition: address alone must match both lines, got %d, want 2 "+
			"— if this is not 2 the test below proves nothing", got)
	}
	if got := CountDHCPv6Binds(log, "fd00:6470:6864::32", mine); got != 1 {
		t.Errorf("address+mac: got %d, want 1 — the other container's reply was counted", got)
	}
}

func TestCountDHCPv6Binds_IgnoresNonReplyLines(t *testing.T) {
	if got := CountDHCPv6Binds(ciSolicit, "iaid=1605248032"); got != 0 {
		t.Errorf("a solicit line is not a bind: got %d, want 0", got)
	}
	if got := CountDHCPv6Binds("", "fd00:6470:6864::32"); got != 0 {
		t.Errorf("empty log: got %d, want 0", got)
	}
}

func TestCountDHCPv6Binds_IsCaseInsensitiveOnBothSides(t *testing.T) {
	if got := CountDHCPv6Binds(ciBindMacvlan, "FD00:6470:6863::91", "26:54:5F:AE:24:20"); got != 1 {
		t.Errorf("uppercase needles: got %d, want 1", got)
	}
}

// The known bound: the matcher is substring-based, so ::9 matches ::91; callers pass whole addresses (#875).
func TestCountDHCPv6Binds_SubstringMatchingIsTheDocumentedBound(t *testing.T) {
	if got := CountDHCPv6Binds(ciBindMacvlan, "fd00:6470:6863::9"); got != 1 {
		t.Errorf("documented bound (substring match): got %d, want 1", got)
	}
}

func TestLastDHCPv6BindAt_ReadsTheServersOwnStamp(t *testing.T) {
	ref := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.Local)

	got, ok := LastDHCPv6BindAt(ciBindMacvlan, ref, "fd00:6470:6863::91")
	if !ok {
		t.Fatal("a real DHCPREPLY line was not read")
	}
	want := time.Date(2026, time.August, 28, 13, 57, 33, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("stamp: got %s, want %s", got, want)
	}
}

func TestLastDHCPv6BindAt_TakesTheLastMatch(t *testing.T) {
	ref := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.Local)
	earlier := `Aug 28 13:40:01 dnsmasq-dhcp[6947]: 1111111 DHCPREPLY(dh-itest-dhcp) fd00:6470:6863::91 00:03:00:01:26:54:5f:ae:24:20`

	got, ok := LastDHCPv6BindAt(earlier+"\n"+ciBindMacvlan, ref, "fd00:6470:6863::91")
	if !ok {
		t.Fatal("no line read")
	}
	if got.Minute() != 57 {
		t.Errorf("took the earlier line: got %s, want the 13:57:33 one", got)
	}
}

func TestLastDHCPv6BindAt_AnUnparseableStampIsNotFound(t *testing.T) {
	ref := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.Local)
	noStamp := `dnsmasq-dhcp[6947]: 4883247 DHCPREPLY(dh-itest-dhcp) fd00:6470:6863::91 00:03:00:01:26:54:5f:ae:24:20`

	if got, ok := LastDHCPv6BindAt(noStamp, ref, "fd00:6470:6863::91"); ok {
		t.Errorf("an unstamped line was accepted as an anchor at %s", got)
	}
	if _, ok := LastDHCPv6BindAt(ciSolicit, ref, "IAID=1605248032"); ok {
		t.Error("a non-DHCPREPLY line was accepted as a bind")
	}
	if _, ok := LastDHCPv6BindAt("", ref, "fd00:6470:6863::91"); ok {
		t.Error("an empty log produced an anchor")
	}
}

// dnsmasq's stamp has no year, so a 31 December line read on 1 January belongs to the previous year.
func TestLastDHCPv6BindAt_RollsBackOverNewYear(t *testing.T) {
	ref := time.Date(2027, time.January, 1, 0, 0, 30, 0, time.Local)
	line := `Dec 31 23:59:58 dnsmasq-dhcp[6947]: 4883247 DHCPREPLY(dh-itest-dhcp) fd00:6470:6863::91 00:03:00:01:26:54:5f:ae:24:20`

	got, ok := LastDHCPv6BindAt(line, ref, "fd00:6470:6863::91")
	if !ok {
		t.Fatal("no line read")
	}
	if got.Year() != 2026 {
		t.Errorf("year: got %d, want 2026 (the stamp is 32 seconds before the reference)", got.Year())
	}
	if got.After(ref) {
		t.Errorf("the anchor is in the future: %s after %s", got, ref)
	}
}

func TestLastDHCPv6BindAt_ADifferentMACIsNotTheAnchor(t *testing.T) {
	ref := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.Local)
	other := `Aug 28 14:10:00 dnsmasq-dhcp[6947]: 1111111 DHCPREPLY(dh-itest-dhcp) fd00:6470:6863::91 00:03:00:01:aa:bb:cc:dd:ee:ff`
	log := ciBindMacvlan + "\n" + other

	if _, ok := LastDHCPv6BindAt(log, ref, "fd00:6470:6863::91"); !ok {
		t.Fatal("precondition: the address alone must match")
	}
	got, ok := LastDHCPv6BindAt(log, ref, "fd00:6470:6863::91", "26:54:5f:ae:24:20")
	if !ok {
		t.Fatal("scoped to my own client: no line read")
	}
	if got.Minute() != 57 {
		t.Errorf("anchored on another client's reply: got %s, want the 13:57:33 one", got)
	}
}

func TestCountDefaultRoutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want int
	}{
		{"empty table", "", 0},
		{"trailing newline only", "\n\n", 0},
		{"one route", "default via fe80::1 dev eth0 metric 1024\n", 1},
		{"two routes: the failure the guard prevents",
			"default via fe80::1 dev eth0 metric 1024\ndefault via fe80::2 dev eth0 proto ra metric 1024\n", 2},
		{"a non-default line does not count",
			"2001:db8::/64 dev eth0 metric 256\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountDefaultRoutes(tc.out); got != tc.want {
				t.Errorf("CountDefaultRoutes = %d, want %d", got, tc.want)
			}
		})
	}
}

// The scope zone is kept: it is what is under test on a link-local resolver (RFC 4007 section 11).
func TestResolvNameservers(t *testing.T) {
	got := ResolvNameservers("# generated\nsearch corp.example\nnameserver fe80::1%eth0\nnameserver 2001:db8::53\n")
	want := []string{"fe80::1%eth0", "2001:db8::53"}
	if len(got) != len(want) {
		t.Fatalf("ResolvNameservers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ResolvNameservers = %v, want %v", got, want)
		}
	}
}
