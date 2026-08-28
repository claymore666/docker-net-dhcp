// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import "testing"

// Every fixture string below is VERBATIM output, captured either from
// alpine:3.20 (the image the suite runs containers in) or from the
// failing CI run itself. That is the whole point of this file: the
// previous version of this observer was validated against a probe
// image with full iproute2 and keyed on a `proto ra` field busybox
// does not emit, so it could never have passed where it actually runs.

// Captured from the CI run that caught it — the container's own
// `ip -6 route show default`. Note: no `proto` field, and the double
// spaces are busybox's.
const ciRouteShowDefault = `default via fe80::8ab:aaff:fe85:2df5 dev dh-itest-br20  metric 1024  expires 0sec`

// Captured from alpine:3.20, `ip -6 -o addr show scope global`.
const busyboxAddrShow = `2: dh-itest-br20    inet6 fd00:6470:6864::42/64 scope global \       valid_lft forever preferred_lft forever`

func TestV6IfaceFromAddrShow(t *testing.T) {
	if got := V6IfaceFromAddrShow(busyboxAddrShow, "fd00:6470:6864::42"); got != "dh-itest-br20" {
		t.Errorf("busybox addr show: got %q, want %q — the observer would read sysctls "+
			"from the wrong path and measure nothing (#875)", got, "dh-itest-br20")
	}
	// The defect this replaced: the interface is NOT eth0, and assuming
	// it was produced three "No such file or directory" reads in CI.
	if got := V6IfaceFromAddrShow(busyboxAddrShow, "fd00:6470:6864::42"); got == "eth0" {
		t.Error("derived eth0; that hardcoded guess is exactly what failed in CI")
	}
	// Absence: an address that is not there must not yield an interface.
	if got := V6IfaceFromAddrShow(busyboxAddrShow, "fd00:dead::1"); got != "" {
		t.Errorf("invented interface %q for an absent address", got)
	}
	if got := V6IfaceFromAddrShow("", "fd00:6470:6864::42"); got != "" {
		t.Errorf("invented interface %q from empty output", got)
	}
}

func TestHasLinkLocalDefaultRoute(t *testing.T) {
	if !HasLinkLocalDefaultRoute(ciRouteShowDefault) {
		t.Error("the real CI route line was not recognised as an RA-derived default " +
			"route; this is the exact string the `proto ra` version failed on")
	}
	// Drive the absence in both directions that matter.
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
	// Verbatim from alpine:3.20 when the path does not exist — the
	// vacuity case. It must be distinguishable from a value, or a test
	// reports success having measured nothing.
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

// Verbatim DHCPREPLY lines from the CI fixture's dnsmasq in the run
// that exposed the RA-assertion ordering bug (#875). Pinned as literal
// text rather than reconstructed: the anchor these feed decides
// whether the RA-guard assertions are allowed to run at all, so its
// matcher has to be driven against the real rendering.
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

// The MAC scoping is the whole point of the discriminator, so drive
// its absence: a reply for the SAME address from a DIFFERENT container
// must not count. Without this the anchor fires early on a reused
// pooled address and the RA assertions go back to racing the guard.
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

// An address that is a PREFIX of another pool address must not be
// matched by substring alone in a way the caller would not expect.
// Recorded as the known bound rather than fixed: the matcher is
// substring-based, so ::9 matches ::91. Callers pass whole addresses
// taken from the link, never truncated ones.
func TestCountDHCPv6Binds_SubstringMatchingIsTheDocumentedBound(t *testing.T) {
	if got := CountDHCPv6Binds(ciBindMacvlan, "fd00:6470:6863::9"); got != 1 {
		t.Errorf("documented bound (substring match): got %d, want 1", got)
	}
}
