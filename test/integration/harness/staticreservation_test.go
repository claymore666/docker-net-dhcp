//go:build integration

package harness

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaticReservation_IsInsideThePool guards the half of the
// reservation that is easy to break by moving the pool.
//
// dnsmasq NAKs a request for an address outside every --dhcp-range, so
// StaticTestIP must stay within [DHCPPoolStart, DHCPPoolEnd]. Narrowing
// the pool without moving the reservation would turn
// TestStaticIP_DriverOpt from "wrong address" into "no address at all",
// which is a slower and more confusing failure than this one.
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

// TestStaticReservation_IsPassedToDnsmasq is the half that matters
// most: the reservation is what takes the address out of the dynamic
// pool. Drop the flag and TestStaticIP_DriverOpt goes back to being a
// coin flip — passing most runs and failing occasionally, which is the
// worst possible failure mode because it reads as flakiness rather
// than as a defect.
//
// Asserted against the fixture source rather than a live dnsmasq: the
// point is to catch the flag being deleted or edited, and that is a
// property of the source. StaticReservationArg is called (not
// restated) so a change to the flag's shape cannot pass by being
// mirrored in two places.
func TestStaticReservation_IsPassedToDnsmasq(t *testing.T) {
	want := StaticReservationArg()

	if !strings.HasPrefix(want, "--dhcp-host=") ||
		!strings.Contains(want, StaticTestMAC) ||
		!strings.Contains(want, StaticTestIP) {
		t.Fatalf("StaticReservationArg() = %q; want a --dhcp-host pinning %s to %s",
			want, StaticTestMAC, StaticTestIP)
	}

	// Keyed on the MAC, not the hostname: the plugin's hostname is
	// best-effort at DISCOVER time (initialDHCPHostname returns "" when
	// the endpoint is not bound yet), so a hostname key would make the
	// reservation racy in exactly the way this whole change removes.
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
	// Match the CALL SITE, not the identifier: fixture.go also contains
	// `func StaticReservationArg() string`, so a bare Contains check on
	// the name passes even with the flag deleted from the arg list.
	// Verified by deleting the call — the first version of this guard
	// stayed green, which is the whole argument for negative controls.
	const callSite = "StaticReservationArg(),"
	if !strings.Contains(string(src), callSite) {
		t.Errorf("fixture.go no longer passes %s to dnsmasq. "+
			"Without the --dhcp-host reservation, StaticTestIP returns to the dynamic "+
			"pool and TestStaticIP_DriverOpt becomes intermittent — it drew .89 and .12 "+
			"on the run that exposed this.", callSite)
	}
}

// TestStaticReservation_TestUsesTheConstants stops the literal address
// coming back. The failure this guards against already happened once:
// the address lived as a bare "192.168.99.95" in the test with a
// comment explaining why it was safe, and the explanation was wrong.
// A literal here would silently decouple the test from the
// reservation.
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
