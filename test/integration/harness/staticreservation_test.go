// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dnsmasq NAKs a request for an address outside every --dhcp-range (#425).
func TestStaticReservation_IsInsideThePool(t *testing.T) {
	ip := net.ParseIP(StaticTestIP)
	lo := net.ParseIP(DHCPPoolStart)
	hi := net.ParseIP(DHCPPoolEnd)
	for name, v := range map[string]string{
		"StaticTestIP":  StaticTestIP,
		"DHCPPoolStart": DHCPPoolStart,
		"DHCPPoolEnd":   DHCPPoolEnd,
	} {
		if net.ParseIP(v) == nil {
			t.Fatalf("%s = %q is not a valid IP", name, v)
		}
	}
	v4, l4, h4 := ip.To4(), lo.To4(), hi.To4()
	if v4 == nil || l4 == nil || h4 == nil {
		t.Fatal("pool constants must be IPv4")
	}
	inRange := func(a, lo, hi net.IP) bool {
		for i := range a {
			switch {
			case a[i] < lo[i] || a[i] > hi[i]:
				return false
			case a[i] > lo[i] && a[i] < hi[i]:
				return true
			}
		}
		return true
	}
	if !inRange(v4, l4, h4) {
		t.Errorf("StaticTestIP %s is outside the pool %s-%s. dnsmasq NAKs a request for "+
			"an address outside every --dhcp-range, so TestStaticIP_DriverOpt would get no "+
			"address at all. Move the reservation, not just the pool.",
			StaticTestIP, DHCPPoolStart, DHCPPoolEnd)
	}
}

func TestStaticReservation_IsPassedToDnsmasq(t *testing.T) {
	want := StaticReservationArg()

	if !strings.HasPrefix(want, "--dhcp-host=") ||
		!strings.Contains(want, StaticTestMAC) ||
		!strings.Contains(want, StaticTestIP) {
		t.Fatalf("StaticReservationArg() = %q; want a --dhcp-host pinning %s to %s",
			want, StaticTestMAC, StaticTestIP)
	}

	// Keyed on the MAC: initialDHCPHostname returns "" before the endpoint is bound, so a hostname key is racy (#425).
	if strings.Contains(want, StaticTestHostname) {
		t.Errorf("StaticReservationArg() = %q keys the reservation on the hostname. "+
			"The plugin may send no hostname at DISCOVER time — key on StaticTestMAC.", want)
	}
	if _, err := net.ParseMAC(StaticTestMAC); err != nil {
		t.Errorf("StaticTestMAC %q is not a valid MAC: %v", StaticTestMAC, err)
	}
	if first := StaticTestMAC[:2]; first != "02" {
		t.Errorf("StaticTestMAC %q is not locally administered (want a 02: prefix); "+
			"a globally-administered address could collide with a real NIC", StaticTestMAC)
	}

	src, err := os.ReadFile("fixture.go")
	if err != nil {
		t.Fatalf("read fixture.go: %v", err)
	}
	// fixture.go also declares `func StaticReservationArg`, so only the call site proves the flag is passed (#425).
	const callSite = "StaticReservationArg(),"
	if !strings.Contains(string(src), callSite) {
		t.Errorf("fixture.go no longer passes %s to dnsmasq. "+
			"Without the --dhcp-host reservation, StaticTestIP returns to the dynamic "+
			"pool and TestStaticIP_DriverOpt becomes intermittent — it drew .89 and .12 "+
			"on the run that exposed this.", callSite)
	}
}

func TestStaticReservation_TestUsesTheConstants(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "static_ip_test.go"))
	if err != nil {
		t.Fatalf("read static_ip_test.go: %v", err)
	}
	body := string(src)

	for _, want := range []string{"harness.StaticTestIP", "harness.StaticTestMAC"} {
		if !strings.Contains(body, want) {
			t.Errorf("static_ip_test.go does not reference %s; it must use the reserved "+
				"constants so the test and the --dhcp-host reservation cannot drift apart", want)
		}
	}
	if strings.Contains(body, `"`+StaticTestIP+`"`) {
		t.Errorf("static_ip_test.go hard-codes %q instead of using harness.StaticTestIP",
			StaticTestIP)
	}
}
