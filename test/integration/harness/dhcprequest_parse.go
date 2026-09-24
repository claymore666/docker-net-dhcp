// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: pure functions over frames built by the client library's own encoder, driven in the unit job.

package harness

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// DHCPClientMessage is one captured client DHCPv4 message; during an outage the server sends nothing, so clients' messages are counted (#940).
type DHCPClientMessage struct {
	At time.Time
	// Raw is the frame as captured.
	Raw []byte
	// SourceMAC is the ethernet source and ClientMAC is BOOTP's chaddr, which a relay or bridge can make disagree.
	SourceMAC net.HardwareAddr
	ClientMAC net.HardwareAddr
	// Type is DHCP option 53, zero for a BOOTP message.
	Type uint8
	// CIAddr is BOOTP's 'ciaddr' field, the client's own address.
	CIAddr net.IP
	// Broadcast reports a frame to 255.255.255.255: REBINDING rather than RENEWING on the wire (RFC 2131 section 4.4.5).
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

// IsRenewalRequest reports a DHCPREQUEST with ciaddr set: RFC 2131 Table 5 gives ciaddr as zero in SELECTING and
// INIT-REBOOT and as the client's address in RENEWING and REBINDING. It is the predicate the library's countSent
// (lease/manager.go) counts RenewalsSent with, so the two populations match (#940).
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

// Offsets from RFC 2131 section 2.
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

// ParseDHCPv4Request decodes a client-to-server DHCPv4 frame and reports false for anything else, including a fragment,
// which the client library's ParseIPv4UDP also refuses (#940).
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

// dhcpMessageType returns option 53, or 0; the walk skips Pad, stops at End (RFC 2132 section 3.1) and at a length past the buffer.
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
