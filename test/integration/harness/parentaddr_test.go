// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"
)

// TestHostVethAddr_IsAUsableParentAddress pins the properties
// HostVethAddr has to satisfy on the macvlan/ipvlan fixture (#549).
//
// THE ORIGINAL REASON IS GONE AND THE CONSTRAINTS ARE NOT. This test was
// written because the chassis's datagram conflict probe was blind here
// for a release: the parent carried no address at all, so the probe fell
// back to a link-local source, and a host answers an ARP request only if
// it can route a reply back to the sender. Every result came back
// *undetermined*, nothing failed, and the run went green — the same
// shape of mistake #524 was filed about, one level up.
//
// That probe is gone. RFC 5227 section 2.1.1 probes with an ALL-ZERO
// sender protocol address, and Linux answers such a request whenever the
// target is a local address, without any routing decision at all. So a
// bare parent no longer blinds the check, and the "on-subnet source"
// requirement that half of this file was written for has expired.
//
// The rows below are kept because each one is load-bearing for a reason
// that never depended on the probe, and they are re-justified here
// rather than left standing on a premise that is no longer true:
//
//   - inside the DHCP pool, dnsmasq would hand this address to a
//     container and manufacture a genuine conflict — which now really
//     would be detected, and would fail the shard;
//   - equal to the server's address or to the reserved static address,
//     two things claim one address on the fixture's own segment;
//   - off-subnet or a bare /32, the parent is not a participant on the
//     segment at all, which is what the section 2.4 tests ping to make
//     a squatter announce itself.
func TestHostVethAddr_IsAUsableParentAddress(t *testing.T) {
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
			t.Errorf("HostVethAddr %q is a /%d: a host route makes the parent a bystander "+
				"rather than a participant on the segment, and there is then nothing on "+
				"it for a squatter to ARP for", HostVethAddr, ones)
		}
	})

	t.Run("is on the leased subnet", func(t *testing.T) {
		if !Subnet().Contains(ip) {
			t.Errorf("HostVethAddr %s is outside %s, so the parent is not on the segment its "+
				"own children are on", ip, SubnetCIDR)
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
