// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"
)

func TestBridgeChallenger_AddressPlanIsUnambiguous(t *testing.T) {
	primaryStart := net.ParseIP(BridgeDHCPPoolStart)
	primaryEnd := net.ParseIP(BridgeDHCPPoolEnd)
	chalStart := net.ParseIP(BridgeChallengerPoolStart)
	chalEnd := net.ParseIP(BridgeChallengerPoolEnd)
	for name, ip := range map[string]net.IP{
		"BridgeDHCPPoolStart":       primaryStart,
		"BridgeDHCPPoolEnd":         primaryEnd,
		"BridgeChallengerPoolStart": chalStart,
		"BridgeChallengerPoolEnd":   chalEnd,
	} {
		if ip == nil || ip.To4() == nil {
			t.Fatalf("%s does not parse as IPv4", name)
		}
	}

	for name, ip := range map[string]net.IP{
		"BridgeChallengerPoolStart": chalStart,
		"BridgeChallengerPoolEnd":   chalEnd,
	} {
		if IsInBridgePool(ip) {
			t.Errorf("%s (%s) falls inside the primary pool %s-%s: a leased address "+
				"would no longer name the server that granted it, and every "+
				"server-policy assertion becomes a coin flip",
				name, ip, BridgeDHCPPoolStart, BridgeDHCPPoolEnd)
		}
	}
	for name, ip := range map[string]net.IP{
		"BridgeDHCPPoolStart": primaryStart,
		"BridgeDHCPPoolEnd":   primaryEnd,
	} {
		if IsInBridgeChallengerPool(ip) {
			t.Errorf("%s (%s) falls inside the challenger pool %s-%s: same ambiguity, "+
				"other direction", name, ip, BridgeChallengerPoolStart, BridgeChallengerPoolEnd)
		}
	}

	bridgeIP, _, err := net.ParseCIDR(BridgeAddr)
	if err != nil {
		t.Fatalf("BridgeAddr %q does not parse: %v", BridgeAddr, err)
	}
	chalIP, chalNet, err := net.ParseCIDR(BridgeChallengerAddr)
	if err != nil {
		t.Fatalf("BridgeChallengerAddr %q does not parse: %v", BridgeChallengerAddr, err)
	}
	absentIP := net.ParseIP(BridgeAbsentServerIP)
	if absentIP == nil {
		t.Fatalf("BridgeAbsentServerIP %q does not parse", BridgeAbsentServerIP)
	}
	for name, ip := range map[string]net.IP{
		"BridgeAddr":           bridgeIP,
		"BridgeChallengerAddr": chalIP,
		"BridgeAbsentServerIP": absentIP,
	} {
		if IsInBridgePool(ip) || IsInBridgeChallengerPool(ip) {
			t.Errorf("%s (%s) is inside a DHCP pool; it must not be leasable", name, ip)
		}
	}

	if chalIP.Equal(bridgeIP) {
		t.Errorf("challenger and primary share the address %s: there is only one server", chalIP)
	}
	if absentIP.Equal(bridgeIP) || absentIP.Equal(chalIP) {
		t.Errorf("BridgeAbsentServerIP %s is a live server; the tests that require "+
			"silence at that address would pass for the wrong reason", absentIP)
	}

	if !chalNet.Contains(absentIP) {
		t.Errorf("BridgeAbsentServerIP %s is outside the bridge subnet %s", absentIP, chalNet)
	}
	if !chalNet.Contains(bridgeIP) {
		t.Errorf("BridgeAddr %s and BridgeChallengerAddr %s are not on one subnet: "+
			"the two servers would not share a broadcast domain", bridgeIP, chalIP)
	}
}
