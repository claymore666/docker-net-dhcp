// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// RFC 9915 section 21.20: "In the absence of this option, the default behavior is that the client is unwilling to
// accept Reconfigure messages", so #925's opt-in is two bytes on the wire, read here off captured frames.

// DHCPv6Message is one captured DHCPv6 datagram, either direction, so a capture with server traffic and no client
// traffic reads as a wrong vantage (#925).
type DHCPv6Message struct {
	At time.Time
	// Raw is the frame as captured, printed on a failing run.
	Raw []byte
	// SourceMAC is the ethernet source.
	SourceMAC net.HardwareAddr
	// SourceIP and DestIP are the datagram's IPv6 addresses; a Solicit goes to ff02::1:2 (RFC 9915 section 7.1).
	SourceIP, DestIP net.IP
	// FromClient is true when the datagram went from port 546 to port 547 (RFC 9915 section 7.2).
	FromClient bool
	// Type is RFC 9915 section 7.3's msg-type octet.
	Type uint8
	// TransactionID is the 24-bit transaction-id that follows it.
	TransactionID uint32
	// Options is every option code carried, in order with repeats; section 21.20's option has option-len 0.
	Options []uint16
	// ClientFQDN is the first option 39's value (flags, then the encoded name), nil when absent (RFC 4704, #1029).
	ClientFQDN []byte
}

// RFC 9915 section 7.3's message types used here.
const (
	DHCPv6Solicit            uint8 = 1
	DHCPv6Advertise          uint8 = 2
	DHCPv6Request            uint8 = 3
	DHCPv6Confirm            uint8 = 4
	DHCPv6Renew              uint8 = 5
	DHCPv6Rebind             uint8 = 6
	DHCPv6Reply              uint8 = 7
	DHCPv6Reconfigure        uint8 = 10
	DHCPv6InformationRequest uint8 = 11
)

// Option codes from RFC 9915 section 21.
const (
	// DHCPv6OptClientID is RFC 9915 section 21.2.
	DHCPv6OptClientID uint16 = 1
	// DHCPv6OptElapsedTime is section 21.9.
	DHCPv6OptElapsedTime uint16 = 8
	// DHCPv6OptReconfigureAccept is section 21.20, the subject of #925.
	DHCPv6OptReconfigureAccept uint16 = 20
	// DHCPv6OptClientFQDN is RFC 4704 section 4.1's Client FQDN option (#1029).
	DHCPv6OptClientFQDN uint16 = 39
)

// Offsets from RFC 9915 sections 7.2, 7.3 and 21.1.
const (
	dhcpv6ClientPort = 546
	dhcpv6ServerPort = 547

	dhcpv6HeaderLen     = 4
	dhcpv6OptionStart   = 4
	dhcpv6OptHeaderLen  = 4
	ipv6NextHeaderIndex = 6
	ipv6SrcStart        = 8
	ipv6DstStart        = 24
	ipv6AddrLen         = 16
)

// ParseDHCPv6 decodes an ethernet frame carrying a DHCPv6 datagram and reports false for anything else. Extension
// headers and fragments are refused, not walked (#925).
func ParseDHCPv6(b []byte) (DHCPv6Message, bool) {
	if len(b) < ethHeaderLen+ipv6HeaderLen+udpHeaderLen+dhcpv6HeaderLen {
		return DHCPv6Message{}, false
	}
	if binary.BigEndian.Uint16(b[12:14]) != ethertypeIPv6 {
		return DHCPv6Message{}, false
	}
	ip := b[ethHeaderLen:]
	if ip[0]>>4 != 6 {
		return DHCPv6Message{}, false
	}
	if ip[ipv6NextHeaderIndex] != protoUDP {
		return DHCPv6Message{}, false
	}
	payloadLen := int(binary.BigEndian.Uint16(ip[4:6]))
	if payloadLen < udpHeaderLen+dhcpv6HeaderLen || len(ip) < ipv6HeaderLen+payloadLen {
		return DHCPv6Message{}, false
	}

	udp := ip[ipv6HeaderLen:]
	src := binary.BigEndian.Uint16(udp[0:2])
	dst := binary.BigEndian.Uint16(udp[2:4])
	fromClient := src == dhcpv6ClientPort && dst == dhcpv6ServerPort
	fromServer := src == dhcpv6ServerPort && dst == dhcpv6ClientPort
	if !fromClient && !fromServer {
		return DHCPv6Message{}, false
	}
	// The UDP length (RFC 768) excludes link-layer padding from the options.
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < udpHeaderLen+dhcpv6HeaderLen || udpLen > len(udp) {
		return DHCPv6Message{}, false
	}

	dh := udp[udpHeaderLen:udpLen]
	m := DHCPv6Message{
		Raw:        append([]byte(nil), b...),
		SourceMAC:  net.HardwareAddr(append([]byte(nil), b[6:12]...)),
		SourceIP:   net.IP(append([]byte(nil), ip[ipv6SrcStart:ipv6SrcStart+ipv6AddrLen]...)),
		DestIP:     net.IP(append([]byte(nil), ip[ipv6DstStart:ipv6DstStart+ipv6AddrLen]...)),
		FromClient: fromClient,
		Type:       dh[0],
		// RFC 9915 section 7.3's transaction-id is three octets.
		TransactionID: uint32(dh[1])<<16 | uint32(dh[2])<<8 | uint32(dh[3]),
	}

	// RFC 9915 section 21.1 options are walked, never searched: a DUID holding 0x0014 is not option 20.
	for o := dh[dhcpv6OptionStart:]; len(o) >= dhcpv6OptHeaderLen; {
		code := binary.BigEndian.Uint16(o[0:2])
		dataLen := int(binary.BigEndian.Uint16(o[2:4]))
		if dhcpv6OptHeaderLen+dataLen > len(o) {
			// An option running off the end stops the walk; the whole options before it stand.
			break
		}
		m.Options = append(m.Options, code)
		if code == DHCPv6OptClientFQDN && m.ClientFQDN == nil {
			m.ClientFQDN = append([]byte{}, o[dhcpv6OptHeaderLen:dhcpv6OptHeaderLen+dataLen]...)
		}
		o = o[dhcpv6OptHeaderLen+dataLen:]
	}
	return m, true
}

// HasOption reports whether the message carried an option with this code.
func (m DHCPv6Message) HasOption(code uint16) bool {
	for _, c := range m.Options {
		if c == code {
			return true
		}
	}
	return false
}

// AnnouncesReconfigureAccept reports whether RFC 9915 section 21.20's option is present.
func (m DHCPv6Message) AnnouncesReconfigureAccept() bool {
	return m.HasOption(DHCPv6OptReconfigureAccept)
}

// DHCPv6MsgName renders a section 7.3 msg-type by name, or by number when unnamed.
func DHCPv6MsgName(t uint8) string {
	switch t {
	case DHCPv6Solicit:
		return "SOLICIT"
	case DHCPv6Advertise:
		return "ADVERTISE"
	case DHCPv6Request:
		return "REQUEST"
	case DHCPv6Confirm:
		return "CONFIRM"
	case DHCPv6Renew:
		return "RENEW"
	case DHCPv6Rebind:
		return "REBIND"
	case DHCPv6Reply:
		return "REPLY"
	case DHCPv6Reconfigure:
		return "RECONFIGURE"
	case DHCPv6InformationRequest:
		return "INFORMATION-REQUEST"
	default:
		return fmt.Sprintf("msg-type %d", t)
	}
}

// String renders one captured message for a failing test.
func (m DHCPv6Message) String() string {
	dir := "server->client"
	if m.FromClient {
		dir = "client->server"
	}
	codes := make([]string, 0, len(m.Options))
	for _, c := range m.Options {
		codes = append(codes, fmt.Sprint(c))
	}
	opts := "none"
	if len(codes) > 0 {
		opts = strings.Join(codes, ",")
	}
	return fmt.Sprintf("%s %s %s src=%s dst=%s xid=%06x options=[%s]",
		m.At.Format("15:04:05.000"), DHCPv6MsgName(m.Type), dir,
		m.SourceIP, m.DestIP, m.TransactionID, opts)
}

// ReconfigureAcceptAnnouncers returns the three message kinds in which RFC 9915 section 20.4.2 lets a server pick
// a reconfigure key: "during the Request/Reply, Solicit/Reply, or Information-request/Reply message exchange."
func ReconfigureAcceptAnnouncers() []uint8 {
	return []uint8{DHCPv6Solicit, DHCPv6Request, DHCPv6InformationRequest}
}

func announcesReconfigure(t uint8) bool {
	for _, k := range ReconfigureAcceptAnnouncers() {
		if k == t {
			return true
		}
	}
	return false
}

// ReconfigureAcceptFindings is #925's verdict: every client message of the three kinds announced the option and every
// required kind arrived. An empty capture is a finding (#524), and more than one announcing ethernet source ends the
// verdict, since nothing here can tell which client is the subject.
func ReconfigureAcceptFindings(msgs []DHCPv6Message, required ...uint8) []string {
	var findings []string

	var fromClient, fromServer int
	seen := map[uint8]int{}
	missing := map[uint8][]DHCPv6Message{}
	speakers := map[string]int{}
	for _, m := range msgs {
		if !m.FromClient {
			fromServer++
			continue
		}
		fromClient++
		if !announcesReconfigure(m.Type) {
			continue
		}
		// Only announcing messages count as speakers: a second client's Renew leaves this endpoint's verdict standing (#925).
		speakers[m.SourceMAC.String()]++
		seen[m.Type]++
		if !m.AnnouncesReconfigureAccept() {
			missing[m.Type] = append(missing[m.Type], m)
		}
	}

	if fromClient == 0 {
		switch {
		case fromServer > 0:
			findings = append(findings, fmt.Sprintf(
				"the capture took %d message(s) from the server and NONE from the client, so it is "+
					"on a vantage the client's frames do not pass; every statement about what the "+
					"client announced is void for this run", fromServer))
		default:
			findings = append(findings, "the capture took no DHCPv6 message in either direction: "+
				"the segment did no DHCPv6 at all, or the capture was not running")
		}
		return findings
	}

	if len(speakers) > 1 {
		macs := make([]string, 0, len(speakers))
		for mac := range speakers {
			macs = append(macs, mac)
		}
		sort.Strings(macs)
		var parts []string
		for _, mac := range macs {
			parts = append(parts, fmt.Sprintf("%s (%d message(s))", mac, speakers[mac]))
		}
		findings = append(findings, fmt.Sprintf(
			"announcing messages came from %d different ethernet sources -- %s -- so more than "+
				"one DHCPv6 client announced on this link and nothing here can say which of them "+
				"is the endpoint under test. Every verdict about what \"the client\" announced "+
				"is withheld for this run; capture on a link this endpoint has to itself",
			len(speakers), strings.Join(parts, ", ")))
		return findings
	}

	for _, want := range required {
		if seen[want] == 0 {
			findings = append(findings, fmt.Sprintf(
				"no %s reached the capture, so nothing was checked for RFC 9915 section 21.20's "+
					"Reconfigure Accept option in that message (%d client message(s) of other "+
					"kinds were seen)", DHCPv6MsgName(want), fromClient))
		}
	}

	kinds := make([]int, 0, len(missing))
	for k := range missing {
		kinds = append(kinds, int(k))
	}
	sort.Ints(kinds)
	for _, k := range kinds {
		ms := missing[uint8(k)]
		findings = append(findings, fmt.Sprintf(
			"%d of %d %s message(s) carried no Reconfigure Accept option (code %d). RFC 9915 "+
				"section 21.20: \"In the absence of this option, the default behavior is that the "+
				"client is unwilling to accept Reconfigure messages.\" A client that does not "+
				"announce is never sent one and never reconfigured (#925). First such message: %s",
			len(ms), seen[uint8(k)], DHCPv6MsgName(uint8(k)), DHCPv6OptReconfigureAccept, ms[0]))
	}
	return findings
}

// FormatDHCPv6Messages renders a capture for a failing test.
func FormatDHCPv6Messages(msgs []DHCPv6Message) string {
	if len(msgs) == 0 {
		return "  (none)"
	}
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString("  " + m.String() + "\n")
	}
	return b.String()
}

// ClientFQDNOption is the RFC 4704 option 39 value for a partial name, to compare ClientFQDN with (#1029).
func ClientFQDNOption(flags uint8, name string) []byte {
	out := []byte{flags}
	for _, label := range strings.Split(name, ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return out
}
