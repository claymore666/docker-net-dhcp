// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, deliberately, and for the reason
// v6signature.go and raguard_parse.go give: everything in this file is
// a pure function over bytes, so it is driven in the fast lane against
// frames built by the client library's OWN encoder rather than being
// validated only in a world that needs root, a veth pair and a DHCP
// server to enter.
//
// The capture that feeds it (dhcpcapture.go) is tagged, gathers the
// evidence, and asks the function here what each frame is.

package harness

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// DHCPClientMessage is one captured DHCPv4 message sent BY a client,
// reduced to what a question about renewal turns on.
//
// Why the client's messages and not the server's: this instrument
// exists to count what left the host during an outage, and during an
// outage the server sends nothing at all. A capture that counted
// answers would report the same number -- zero -- for a client
// retransmitting into silence and for a client that had stopped asking,
// which is the one distinction the whole counter is about.
type DHCPClientMessage struct {
	At time.Time
	// Raw is the frame exactly as it came off the wire, carried so a
	// failing lane run can print the bytes the decoder was given.
	Raw []byte
	// SourceMAC is the ethernet source and ClientMAC is BOOTP's chaddr.
	// Both, because they are two claims about identity that a relay or
	// a misconfigured bridge can make disagree, and a test that reads
	// only one cannot notice.
	SourceMAC net.HardwareAddr
	ClientMAC net.HardwareAddr
	// Type is DHCP option 53. Zero when the message carried none, which
	// makes it a BOOTP message rather than a DHCP one.
	Type uint8
	// CIAddr is BOOTP's 'ciaddr' field, the client's own address.
	CIAddr net.IP
	// Broadcast reports that the frame went to 255.255.255.255 rather
	// than to one server: the difference between RFC 2131's REBINDING
	// and RENEWING on the wire.
	Broadcast bool
	XID       uint32
}

// The DHCP message types this file names. RFC 2131 section 3.1.
const (
	DHCPDiscover = 1
	DHCPRequest  = 3
	DHCPDecline  = 4
	DHCPRelease  = 7
	DHCPInform   = 8
)

// IsRenewalRequest reports whether this is a DHCPREQUEST extending a
// lease the client already holds.
//
// The predicate is the message's own content and not a guess about the
// client's state: RFC 2131 Table 5 gives 'ciaddr' as zero in the
// SELECTING and INIT-REBOOT columns and as the client's address in the
// RENEWING and REBINDING ones. It is deliberately the SAME predicate
// the library counts RenewalsSent with (lease/manager.go, countSent),
// because the assertion this instrument exists to carry compares the
// two numbers -- an instrument that counted a different population
// would make the comparison meaningless in whichever direction it
// happened to differ.
//
// REBINDING is included, for that reason and on purpose: a broadcast
// REQUEST with ciaddr set is still a renewal request that the server
// did not answer, and the counter under test moves for it.
func (m DHCPClientMessage) IsRenewalRequest() bool {
	return m.Type == DHCPRequest && m.CIAddr != nil && !m.CIAddr.Equal(net.IPv4zero)
}

func (m DHCPClientMessage) String() string {
	kind := fmt.Sprintf("type=%d", m.Type)
	switch m.Type {
	case DHCPDiscover:
		kind = "DISCOVER"
	case DHCPRequest:
		kind = "REQUEST"
		if m.IsRenewalRequest() {
			kind = "RENEWAL REQUEST"
		}
	case DHCPDecline:
		kind = "DECLINE"
	case DHCPRelease:
		kind = "RELEASE"
	case DHCPInform:
		kind = "INFORM"
	}
	dst := "unicast"
	if m.Broadcast {
		dst = "broadcast"
	}
	return fmt.Sprintf("%s %s %s chaddr=%s ciaddr=%s xid=%08x",
		m.At.Format("15:04:05.000"), kind, dst, m.ClientMAC, m.CIAddr, m.XID)
}

// The offsets this file reads. Written out rather than inlined so the
// numbers appear once and a reader can check them against RFC 2131
// section 2 without counting.
const (
	ethertypeIPv4 = 0x0800
	protoUDP      = 17

	udpHeaderLen = 8
	// serverPortBOOTPS is the port a client sends to. RFC 2131 section 4.1.
	serverPortBOOTPS = 67

	bootpMinLen      = 240 // through the magic cookie
	bootpOpOffset    = 0
	bootpXIDOffset   = 4
	bootpCIAddrStart = 12
	bootpCHAddrStart = 28
	bootpCookieStart = 236
	bootpOptionStart = 240

	bootRequest  = 1
	optMsgType   = 53
	optPad       = 0
	optEnd       = 255
	magicCookie  = 0x63825363
	chaddrMaxLen = 16
)

// ParseDHCPv4Request decodes an ethernet frame carrying a DHCPv4
// message from a client to a server, and reports false for everything
// else.
//
// Everything else is most of what arrives: the socket underneath is
// ETH_P_ALL (see captureEthertypeBE), so ARP, the container's own
// traffic and the server's replies all pass through here. Each is
// dropped rather than mis-parsed -- a frame that is not a client's
// DHCP message is not evidence of anything this instrument claims.
//
// BOUND, and it is the same one ParseIPv4UDP in the client library
// takes: a fragmented datagram is refused rather than reassembled. A
// DHCP message is far below any link MTU, so a fragment here means
// something exotic is happening and the honest answer is that this
// instrument did not read it.
func ParseDHCPv4Request(b []byte) (DHCPClientMessage, bool) {
	if len(b) < ethHeaderLen+20+udpHeaderLen+bootpMinLen {
		return DHCPClientMessage{}, false
	}
	if binary.BigEndian.Uint16(b[12:14]) != ethertypeIPv4 {
		return DHCPClientMessage{}, false
	}
	ip := b[ethHeaderLen:]
	if ip[0]>>4 != 4 {
		return DHCPClientMessage{}, false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl+udpHeaderLen {
		return DHCPClientMessage{}, false
	}
	if ip[9] != protoUDP {
		return DHCPClientMessage{}, false
	}
	// Fragment offset non-zero, or "more fragments" set: refused.
	if binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 {
		return DHCPClientMessage{}, false
	}
	dstIP := net.IP(append([]byte(nil), ip[16:20]...))

	udp := ip[ihl:]
	if binary.BigEndian.Uint16(udp[2:4]) != serverPortBOOTPS {
		return DHCPClientMessage{}, false
	}

	bootp := udp[udpHeaderLen:]
	if len(bootp) < bootpMinLen {
		return DHCPClientMessage{}, false
	}
	if bootp[bootpOpOffset] != bootRequest {
		return DHCPClientMessage{}, false
	}
	if binary.BigEndian.Uint32(bootp[bootpCookieStart:bootpCookieStart+4]) != magicCookie {
		return DHCPClientMessage{}, false
	}

	hlen := int(bootp[2])
	if hlen <= 0 || hlen > chaddrMaxLen {
		hlen = 6
	}

	m := DHCPClientMessage{
		Raw:       append([]byte(nil), b...),
		SourceMAC: net.HardwareAddr(append([]byte(nil), b[6:12]...)),
		ClientMAC: net.HardwareAddr(append([]byte(nil), bootp[bootpCHAddrStart:bootpCHAddrStart+hlen]...)),
		CIAddr:    net.IP(append([]byte(nil), bootp[bootpCIAddrStart:bootpCIAddrStart+4]...)),
		Broadcast: dstIP.Equal(net.IPv4bcast),
		XID:       binary.BigEndian.Uint32(bootp[bootpXIDOffset : bootpXIDOffset+4]),
	}
	m.Type = dhcpMessageType(bootp[bootpOptionStart:])
	return m, true
}

// dhcpMessageType walks the option field for option 53, and returns 0
// when there is none.
//
// The walk stops at End and skips Pad, per RFC 2132 section 3.1. An
// option whose length runs off the end of the buffer ends the walk
// rather than panicking: a truncated capture is a frame this instrument
// could not read, never a frame it may guess about.
func dhcpMessageType(opts []byte) uint8 {
	for i := 0; i < len(opts); {
		switch opts[i] {
		case optPad:
			i++
			continue
		case optEnd:
			return 0
		}
		if i+1 >= len(opts) {
			return 0
		}
		code, length := opts[i], int(opts[i+1])
		if i+2+length > len(opts) {
			return 0
		}
		if code == optMsgType && length == 1 {
			return opts[i+2]
		}
		i += 2 + length
	}
	return 0
}
