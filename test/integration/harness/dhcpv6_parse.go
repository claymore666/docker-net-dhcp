// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, deliberately, and for the reason
// dhcprequest_parse.go and v6signature.go give: everything in this file
// is a pure function over bytes, so it is driven in the fast lane
// against frames built field by field rather than being validated only
// in a world that needs root, a bridge and a DHCP server to enter.
//
// The capture that feeds it (dhcpv6capture.go) is tagged, gathers the
// evidence, and asks the functions here for the verdict. The split is
// the point: an instrument whose verdict can only be exercised on the
// privileged lane is one nobody can drive in both directions.

package harness

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// What this file is for: #925's opt-in, read off the wire.
//
// RFC 9915 section 21.20 makes the Reconfigure Accept option the whole
// of whether a client may be reconfigured at all -- "In the absence of
// this option, the default behavior is that the client is unwilling to
// accept Reconfigure messages" -- so the announcement is the feature,
// and the announcement is two bytes in a datagram and nothing else.
//
// THE PLUGIN'S OWN PARAMETERS ARE NOT EVIDENCE OF IT. There is already
// a unit test that asks buildParams6 for its AcceptReconfigure field
// (pkg/dhcp/v6mode_test.go). That test can pass over a client that
// never reached the wire, over an encoder that drops the option, and
// over a library whose emission is behind a condition the plugin does
// not meet. What is on the link is the only reading that closes those,
// and it is what this instrument takes.

// DHCPv6Message is one captured DHCPv6 datagram, reduced to what a
// question about announced options turns on.
//
// BOTH DIRECTIONS ARE KEPT, and that is not symmetry for its own sake.
// A capture that held only client messages reports the same thing --
// nothing -- for a segment where the client never spoke and for a
// capture placed where a client's frames do not pass. Those need
// different repairs, and the server's half of the exchange is what
// tells them apart: server traffic present with no client traffic is a
// vantage on the wrong side, and neither present is a segment that did
// nothing.
type DHCPv6Message struct {
	At time.Time
	// Raw is the frame exactly as it came off the wire, carried so a
	// failing lane run can print the bytes the decoder was given.
	Raw []byte
	// SourceMAC is the ethernet source.
	SourceMAC net.HardwareAddr
	// SourceIP and DestIP are the IPv6 addresses of the datagram. A
	// Solicit goes to ff02::1:2 (section 7.1's All_DHCP_Relay_Agents_and_Servers)
	// and a Reply comes back to the client's link-local, so these say
	// which leg of an exchange a frame is without consulting the ports
	// a second time.
	SourceIP, DestIP net.IP
	// FromClient is true when the datagram left the client port for the
	// server port (section 7.2: 546 and 547). It is the direction, read
	// off the ports rather than guessed from the message type, because
	// the message type is exactly what a decoder with a wrong offset
	// gets wrong.
	FromClient bool
	// Type is section 7.3's msg-type octet.
	Type uint8
	// TransactionID is the 24-bit transaction-id that follows it.
	TransactionID uint32
	// Options is every option code carried, in the order they appeared
	// and with repeats kept. Codes and not values: this instrument
	// answers which options were announced, and section 21.20's option
	// has no value to read -- "option-len: 0", the option IS the
	// announcement.
	Options []uint16
}

// Section 7.3's message types, as far as this instrument names them.
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

// The option codes this file names. Section 21.
const (
	// DHCPv6OptClientID is section 21.2. Named because the fast-lane
	// test hides a 0x0014 inside its payload: an instrument that
	// searched the datagram for two bytes rather than walking it would
	// report section 21.20's option present in a message that carries
	// none, and that is the failure this whole file exists to not have.
	DHCPv6OptClientID uint16 = 1
	// DHCPv6OptElapsedTime is section 21.9.
	DHCPv6OptElapsedTime uint16 = 8
	// DHCPv6OptReconfigureAccept is section 21.20, the subject of #925.
	DHCPv6OptReconfigureAccept uint16 = 20
)

// The offsets this file reads. Written out rather than inlined so the
// numbers appear once and a reader can check them against RFC 9915
// section 7.2, section 7.3 and section 21.1 without counting.
const (
	// Section 7.2: "Clients listen for DHCP messages on UDP port 546.
	// Servers and relay agents listen for DHCP messages on UDP port
	// 547."
	dhcpv6ClientPort = 546
	dhcpv6ServerPort = 547

	// Section 7.3's client/server message header: one octet of
	// msg-type, three of transaction-id, then the options.
	dhcpv6HeaderLen     = 4
	dhcpv6OptionStart   = 4
	dhcpv6OptHeaderLen  = 4
	ipv6NextHeaderIndex = 6
	ipv6SrcStart        = 8
	ipv6DstStart        = 24
	ipv6AddrLen         = 16
)

// ParseDHCPv6 decodes an ethernet frame carrying a DHCPv6 datagram, and
// reports false for everything else.
//
// Everything else is most of what arrives: the socket underneath is
// ETH_P_ALL (see captureEthertypeBE), so the router advertisements, the
// neighbour discovery and the container's own traffic all pass through
// here. Each is dropped rather than mis-parsed -- a frame that is not a
// DHCPv6 datagram is not evidence of anything this instrument claims.
//
// BOUND, and it is ParseRA's: IPv6 extension headers are not walked and
// a fragmented datagram is not reassembled. A frame carrying either is
// REFUSED rather than half-read. A DHCPv6 client message is far below
// any link MTU and this fixture's client sends neither, so the bound
// costs nothing here; and the direction it fails in is the safe one,
// because a refused frame reads as absence and ReconfigureAcceptFindings
// makes absence a finding rather than a pass.
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
	// Next Header, and it must be UDP right here. A Hop-by-Hop or
	// Fragment header would put the UDP header somewhere else, and this
	// decoder refuses rather than guesses.
	if ip[ipv6NextHeaderIndex] != protoUDP {
		return DHCPv6Message{}, false
	}
	// The payload length the header claims, checked against the frame
	// that actually arrived. A shorter frame than its own header
	// describes is truncated, and a decoder that read on would be
	// reading whatever the capture buffer held before it.
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
	// The UDP length covers the header and the payload (RFC 768). Taken
	// from the datagram rather than from the frame, so trailing padding
	// a link layer added is not read as options.
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
		// Section 7.3's transaction-id is three octets, so it is read as
		// three and not as a uint32 starting one byte early.
		TransactionID: uint32(dh[1])<<16 | uint32(dh[2])<<8 | uint32(dh[3]),
	}

	// Section 21.1's option format: two octets of code, two of length,
	// then that many octets of data. WALKED, never searched: an option
	// code is only an option code where an option begins, and a DUID or
	// an IA address holding the same two bytes is not an announcement.
	// The fast-lane test drives exactly that frame.
	for o := dh[dhcpv6OptionStart:]; len(o) >= dhcpv6OptHeaderLen; {
		code := binary.BigEndian.Uint16(o[0:2])
		dataLen := int(binary.BigEndian.Uint16(o[2:4]))
		if dhcpv6OptHeaderLen+dataLen > len(o) {
			// An option that runs off the end of the datagram. The
			// options read so far stand -- they were whole -- and the
			// walk stops rather than inventing the rest.
			break
		}
		m.Options = append(m.Options, code)
		o = o[dhcpv6OptHeaderLen+dataLen:]
	}
	return m, true
}

// HasOption reports whether the message carried an option with this
// code.
func (m DHCPv6Message) HasOption(code uint16) bool {
	for _, c := range m.Options {
		if c == code {
			return true
		}
	}
	return false
}

// AnnouncesReconfigureAccept is section 21.20's option, present.
func (m DHCPv6Message) AnnouncesReconfigureAccept() bool {
	return m.HasOption(DHCPv6OptReconfigureAccept)
}

// DHCPv6MsgName renders section 7.3's msg-type for a human, and its
// number for one this file does not name.
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

// ReconfigureAcceptAnnouncers is section 21.20's three message kinds,
// which are the three exchanges section 20.4.2 lets a server grant a
// reconfigure key in: "The server selects a reconfigure key for a
// client during the Request/Reply, Solicit/Reply, or
// Information-request/Reply message exchange."
//
// A CLIENT THAT ANNOUNCED IN ONE OF THREE HAS NOT ANNOUNCED. Which
// exchange grants the key is the server's choice, so an announcement
// missing from any of the three is a server that may never hand this
// client a key, and the client is then deaf to Reconfigure with nothing
// to read that says so. That is why the verdict below is over every
// captured message of these kinds rather than over the first one.
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

// ReconfigureAcceptFindings is #925's verdict over one capture: every
// client message of section 21.20's three kinds announced the option,
// and the capture actually saw the kinds `required` names.
//
// IT REFUSES AN EMPTY CAPTURE, and that is the reason it exists as a
// function rather than as a loop in the test. "Every captured Solicit
// announced it" is true of a capture that took no Solicit, and a
// capture placed where the client's frames do not pass takes none: the
// assertion would then be a statement about nothing, which is the #524
// fault in its purest form. So absence is a finding here, and it is
// worded so the reader can tell the two absences apart -- a vantage
// that saw the server and not the client, and a segment that was
// silent.
//
// IT REFUSES A LINK WITH MORE THAN ONE CLIENT ON IT. Every message
// here is attributed to the caller's client, and the only thing that
// selects them is direction and message type. A second DHCPv6 client
// on the same link -- a container left behind by an earlier case, or
// anything else on a shared bridge -- would have its Solicit read as
// this plugin's, which is a false accusation in one direction and a
// verdict about the wrong endpoint in the other. So distinct ethernet
// sources are counted, and more than one ends the verdict: nothing
// here can say which client is the subject, and saying so is the only
// honest answer.
//
// `required` is the message kinds this caller's exchange must have
// produced. The empty-capture refusal above is unconditional and runs
// ahead of it, so a caller passing none is still refused an empty
// capture; what it gives up is the check that a PARTICULAR kind
// arrived. Every caller in this repo passes the kinds its mode sends:
// the managed case passes SOLICIT and REQUEST, and the stateless case
// passes INFORMATION-REQUEST, which is the only announcing message a
// stateless client ever sends.
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
		// COUNTED HERE AND NOT ONE LINE HIGHER, and the difference is
		// a direction. The question this set answers is whose
		// ANNOUNCEMENTS are being judged, so a second client that
		// sends no announcing message -- a Renew, a Rebind, a Release
		// -- does not take the verdict away from an endpoint whose
		// Solicit and Request are unambiguous. Counting every client
		// message instead would withhold a verdict this capture can
		// give, which is a gate that cries wolf on any shared link.
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
