// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"
)

// HostVethAddr constraints on the macvlan/ipvlan fixture (#549): inside the DHCP pool dnsmasq would hand it to a
// container; equal to the server or static address, two hosts claim one address; off-subnet or a bare /32, the parent
// is not on the segment the RFC 5227 section 2.4 tests ping. Linux answers an RFC 5227 section 2.1.1 all-zero-sender
// probe for any local target, so a bare parent no longer blinds the probe.
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
