// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"
)

// TestHostVethAddr_MakesTheConflictProbeUsable pins the properties
// HostVethAddr has to satisfy for the address-conflict detector to work
// on the macvlan/ipvlan fixture (#549).
//
// The detector was blind here for a release: the parent carried no
// address at all, so the probe fell back to a link-local source, and a
// host answers an ARP request only if it can route a reply back to the
// sender. Every result came back *undetermined*. Nothing failed — the
// run said so in a log line and went green, which is the same shape of
// mistake #524 was filed about, one level up.
//
// A static test cannot prove the address is actually applied to the
// link; the suite's own conflict-probe census is what reports that. What
// it can do is stop the constant from silently drifting into a value
// that makes the probe useless again — inside the DHCP pool (dnsmasq
// hands it to a client, and now two things own it), or equal to the
// server or the reserved static address.
func TestHostVethAddr_MakesTheConflictProbeUsable(t *testing.T) {
	ip, _, err := net.ParseCIDR(HostVethAddr)
	if err != nil {
		t.Fatalf("HostVethAddr %q does not parse: %v", HostVethAddr, err)
	}

	t.Run("carries a mask, so it is a source address and not a bare host route", func(t *testing.T) {
		_, ipnet, err := net.ParseCIDR(HostVethAddr)
		if err != nil || ipnet == nil {
			t.Fatalf("HostVethAddr %q has no prefix", HostVethAddr)
		}
		if ones, bits := ipnet.Mask.Size(); ones == bits {
			t.Errorf("HostVethAddr %q is a /%d: a host route gives the probe no on-subnet "+
				"source and the fallback path returns undetermined again", HostVethAddr, ones)
		}
	})

	t.Run("is on the leased subnet", func(t *testing.T) {
		if !Subnet().Contains(ip) {
			t.Errorf("HostVethAddr %s is outside %s. The probe needs a source the responder "+
				"can route back to; off-subnet is the failure this constant exists to fix",
				ip, SubnetCIDR)
		}
	})

	t.Run("is outside the DHCP pool", func(t *testing.T) {
		if IsInPool(ip) {
			t.Errorf("HostVethAddr %s falls inside the pool [%s, %s]. dnsmasq would hand it "+
				"to a container, which is a genuine address conflict manufactured by the harness",
				ip, DHCPPoolStart, DHCPPoolEnd)
		}
	})

	t.Run("is not the DHCP server's own address", func(t *testing.T) {
		serverIP, _, err := net.ParseCIDR(DHCPServerAddr)
		if err != nil {
			t.Fatalf("DHCPServerAddr %q does not parse: %v", DHCPServerAddr, err)
		}
		if ip.Equal(serverIP) {
			t.Errorf("HostVethAddr and DHCPServerAddr are both %s; both ends of the veth pair "+
				"would claim it", ip)
		}
	})

	t.Run("is not the reserved static test address", func(t *testing.T) {
		if ip.Equal(net.ParseIP(StaticTestIP)) {
			t.Errorf("HostVethAddr collides with StaticTestIP %s, which TestStaticReservation "+
				"expects a container to receive", StaticTestIP)
		}
	})
}
