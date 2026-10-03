// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"slices"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// Measured against dhcp-golib v1.3.0 (#1157): the lists gain these codes with no Params switch; the plugin's test of a
// 249-only server is #1030, logging 43 and 125 is #1034.

func TestBuildParams_TheDiscoverAsksForOption249AfterOption121AndForOptions43And125(t *testing.T) {
	p, err := buildParams(&DHCPClientOptions{MAC: testMAC(t)}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	pl := firstDiscover(t, p).Options[wire.OptParameterList]
	codes := make([]int, len(pl))
	for i, c := range pl {
		codes[i] = int(c)
	}
	t.Logf("DHCPDISCOVER option 55: %v", codes)

	// Numbers: the library names exist only from v1.3.0, and a build against v1.2.0 must fail here, not to compile.
	at121 := slices.Index(codes, 121)
	if at121 < 0 {
		t.Fatalf("option 55 %v does not name option 121", codes)
	}
	if got := slices.Index(codes, 249); got <= at121 {
		t.Errorf("option 249 is at index %d of option 55 %v, want it after option 121 at %d", got, codes, at121)
	}
	for _, c := range []int{43, 125} {
		if !slices.Contains(codes, c) {
			t.Errorf("option 55 %v does not name option %d", codes, c)
		}
	}
}

// The v6 half of the same release asks for the vendor, timezone and NTP options (#1034, #1033, #859).
func TestBuildParams6_TheSolicitAsksForOptions17And41And42And56(t *testing.T) {
	p, err := buildParams6(testOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	raw, ok := firstSolicit6(t, p).Options.First(wire.OptV6ORO)
	if !ok {
		t.Fatal("the Solicit carries no Option Request option")
	}
	var codes []int
	for i := 0; i+1 < len(raw); i += 2 {
		codes = append(codes, int(raw[i])<<8|int(raw[i+1]))
	}
	t.Logf("Solicit option 6 (ORO): %v", codes)

	for _, want := range []int{23, 24, 17, 41, 42, 56} {
		if !slices.Contains(codes, want) {
			t.Errorf("the Solicit's ORO %v does not name option %d", codes, want)
		}
	}
}
