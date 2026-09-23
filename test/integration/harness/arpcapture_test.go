// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"
)

// frame builds an ethernet + ARP-over-IPv4 packet.
func frame(ethertype uint16, op uint16, srcMAC, senderHW string, spa, tpa string) []byte {
	b := make([]byte, 14+28)
	copy(b[0:6], mustMAC("ff:ff:ff:ff:ff:ff"))
	copy(b[6:12], mustMAC(srcMAC))
	b[12], b[13] = byte(ethertype>>8), byte(ethertype)

	a := b[14:]
	a[0], a[1] = 0, 1 // hardware type: ethernet
	a[2], a[3] = 8, 0 // protocol type: IPv4
	a[4], a[5] = 6, 4 // address lengths
	a[6], a[7] = byte(op>>8), byte(op)
	copy(a[8:14], mustMAC(senderHW))
	copy(a[14:18], net.ParseIP(spa).To4())
	copy(a[24:28], net.ParseIP(tpa).To4())
	return b
}

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

const (
	ethARP  = 0x0806
	ethIPv4 = 0x0800
)

// A container on a network with no gateway resolves 0.0.0.0, and the kernel's ARP Requests for it are spa=0.0.0.0
// tpa=0.0.0.0, three a second apart, like an RFC 5227 section 2.1.1 probe (#882).
func TestARPFrame_ProbeNeedsAZeroSenderAndARealTarget(t *testing.T) {
	const src = "5e:b8:82:78:37:36"

	cases := []struct {
		name     string
		f        []byte
		probe    bool
		announce bool
		parses   bool
	}{
		{
			name:   "RFC 5227 section 2.1.1 probe",
			f:      frame(ethARP, 1, src, src, "0.0.0.0", "192.168.101.42"),
			probe:  true,
			parses: true,
		},
		{
			name:   "kernel resolving 0.0.0.0 with no source address to use",
			f:      frame(ethARP, 1, src, src, "0.0.0.0", "0.0.0.0"),
			probe:  false,
			parses: true,
		},
		{
			name:     "RFC 5227 section 2.3 announcement",
			f:        frame(ethARP, 1, src, src, "192.168.101.42", "192.168.101.42"),
			announce: true,
			parses:   true,
		},
		{
			name:   "an ordinary lookup from a configured host",
			f:      frame(ethARP, 1, src, src, "192.168.101.2", "192.168.101.42"),
			parses: true,
		},
		{
			name:   "a reply, which is what a squatter answers with",
			f:      frame(ethARP, 2, src, src, "192.168.101.42", "0.0.0.0"),
			parses: true,
		},
		{
			name:   "an IPv4 datagram on the same link",
			f:      frame(ethIPv4, 1, src, src, "0.0.0.0", "192.168.101.42"),
			parses: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseARP(c.f)
			if ok != c.parses {
				t.Fatalf("parseARP ok=%v, want %v", ok, c.parses)
			}
			if !ok {
				return
			}
			if got.IsProbe() != c.probe {
				t.Errorf("IsProbe = %v, want %v for %s", got.IsProbe(), c.probe, got)
			}
			if got.IsAnnouncement() != c.announce {
				t.Errorf("IsAnnouncement = %v, want %v for %s", got.IsAnnouncement(), c.announce, got)
			}
			if got.SenderMAC.String() != src {
				t.Errorf("SenderMAC = %s, want %s", got.SenderMAC, src)
			}
		})
	}
}

func TestARPFrame_ShortFrameIsNotHalfParsed(t *testing.T) {
	full := frame(ethARP, 1, "5e:b8:82:78:37:36", "5e:b8:82:78:37:36", "0.0.0.0", "192.168.101.42")
	for n := 0; n < len(full); n++ {
		if _, ok := parseARP(full[:n]); ok {
			t.Fatalf("parseARP accepted a %d-byte frame; the full one is %d", n, len(full))
		}
	}
	if _, ok := parseARP(full); !ok {
		t.Fatal("parseARP rejected the full frame, so the loop above proved nothing")
	}
}

// AF_PACKET takes the protocol in network order; host order binds to a protocol nothing uses and captures nothing (#882).
func TestCaptureEthertype_IsHtonsOfETHPALL(t *testing.T) {
	if got := captureEthertypeBE(); got != 0x0300 {
		t.Errorf("captureEthertypeBE = %#04x, want %#04x (htons(ETH_P_ALL))", got, 0x0300)
	}
}
