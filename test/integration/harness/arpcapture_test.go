// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"net"
	"testing"
)

// The instrument's own observer.
//
// arpcapture.go decides, frame by frame, whether RFC 5227 ran. Every
// conflict_check assertion in the suite is downstream of parseARP and
// IsProbe, and neither had a test — so an instrument defect arrived on
// the lane looking like a product defect, twice. This file drives the
// predicates on bytes, with no socket and no privilege, so the next one
// arrives here instead.

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

// TestARPFrame_ProbeNeedsAZeroSenderAndARealTarget is the case that
// failed on the lane.
//
// A container on a network with no gateway resolves 0.0.0.0. The kernel
// emits an ordinary ARP Request for it, and inet_select_addr cannot
// choose a sender address for a zero target either, so the frame is
// spa=0.0.0.0 tpa=0.0.0.0 — three of them a second apart, which is the
// neighbour retransmission schedule and looks exactly like RFC 5227
// section 2.1.1's. A predicate keyed only on the zero SENDER called
// those three a probe and failed conflict_check=off on a plugin that
// had correctly sent nothing.
//
// The row that matters is "kernel resolving 0.0.0.0". The others are
// there so a fix that tightens the predicate into uselessness — a probe
// is never recognised at all — fails too: a check with one possible
// verdict is worthless in whichever direction it points.
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
			// The socket is ETH_P_ALL now, so this is reachable and the
			// ethertype filter is the harness's own job.
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

// TestARPFrame_ShortFrameIsNotHalfParsed pins the other direction of the
// same rule: a truncated frame must be dropped, not read out of the
// bytes that happen to be there. A partially parsed frame with a zero
// tail reads as an announcement of 0.0.0.0, which is not a thing.
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

// TestCaptureEthertype_IsHtonsOfETHPALL pins the byte order.
//
// AF_PACKET wants the protocol in NETWORK order. Getting it wrong does
// not fail: it binds to a protocol nothing uses and the capture stays
// empty, which reads as "nothing was on the wire" — the exact false
// negative this whole file exists to prevent.
func TestCaptureEthertype_IsHtonsOfETHPALL(t *testing.T) {
	if got := captureEthertypeBE(); got != 0x0300 {
		t.Errorf("captureEthertypeBE = %#04x, want %#04x (htons(ETH_P_ALL))", got, 0x0300)
	}
}
