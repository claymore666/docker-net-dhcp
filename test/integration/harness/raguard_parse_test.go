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
