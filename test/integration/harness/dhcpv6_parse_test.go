// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag: the subject is pure, so it is driven
// here rather than only on the lane that needs root and a bridge.

package harness

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

// Frames are assembled FIELD BY FIELD here rather than pasted as a hex
// blob, and that is the point of the file. ParseRA's own history is the
// argument: a decoder one byte off read Cur Hop Limit as the flags and
// reported the same answer for three different segment modes, and only
// a test that knows which byte it put where can catch that. A blob
// records what one capture happened to contain; a builder states what
// each field IS, so a decoder reading the wrong offset disagrees with
// the test about a value the test can name.

const (
	testV6SrcMAC = "02:42:ac:11:00:02"
	testV6DstMAC = "33:33:00:01:00:02"
)

// dhcpv6Option encodes RFC 9915 section 21.1's option: code, length,
// data.
func dhcpv6Option(code uint16, data []byte) []byte {
	b := make([]byte, 4+len(data))
	binary.BigEndian.PutUint16(b[0:2], code)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(data)))
	copy(b[4:], data)
	return b
}

// buildDHCPv6Frame assembles an ethernet frame carrying a DHCPv6
// datagram with the given ports, message type, transaction id and
// options.
func buildDHCPv6Frame(t *testing.T, srcPort, dstPort uint16, msgType uint8, xid uint32, opts ...[]byte) []byte {
	t.Helper()
	return buildDHCPv6FrameFrom(t, testV6SrcMAC, srcPort, dstPort, msgType, xid, opts...)
}

// buildDHCPv6FrameFrom is the same, with the ethernet source named.
// The verdict attributes what it reads to one client, and the only
// thing on a captured frame that says which client sent it is this
// address, so a test about two clients on one link has to be able to
// set it.
func buildDHCPv6FrameFrom(t *testing.T, srcMAC string, srcPort, dstPort uint16, msgType uint8, xid uint32, opts ...[]byte) []byte {
	t.Helper()

	payload := []byte{msgType, byte(xid >> 16), byte(xid >> 8), byte(xid)}
	for _, o := range opts {
		payload = append(payload, o...)
	}

	udp := make([]byte, udpHeaderLen)
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpHeaderLen+len(payload)))
	udp = append(udp, payload...)

	ip := make([]byte, ipv6HeaderLen)
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(udp)))
	ip[6] = protoUDP
	ip[7] = 1
	copy(ip[8:24], net.ParseIP("fe80::42:acff:fe11:2").To16())
	copy(ip[24:40], net.ParseIP("ff02::1:2").To16())
	ip = append(ip, udp...)

	src, err := net.ParseMAC(srcMAC)
	if err != nil {
		t.Fatalf("test MAC: %v", err)
	}
	dst, err := net.ParseMAC(testV6DstMAC)
	if err != nil {
		t.Fatalf("test MAC: %v", err)
	}
	eth := make([]byte, 0, ethHeaderLen+len(ip))
	eth = append(eth, dst...)
	eth = append(eth, src...)
	eth = append(eth, 0x86, 0xDD)
	return append(eth, ip...)
}

// reconfigureAcceptOption is section 21.20's option: "option-len: 0".
func reconfigureAcceptOption() []byte {
	return dhcpv6Option(DHCPv6OptReconfigureAccept, nil)
}

// clientIDOption is a Client Identifier whose payload is the DUID this
// plugin builds, with the two bytes 0x00 0x14 planted inside it.
//
// 0x0014 is 20, section 21.20's option code, and it sits where an
// option code would be if a reader started one option too early. A
// decoder that searched the datagram for those bytes, or that walked
// the options from the wrong offset, reports Reconfigure Accept present
// in a message carrying none. That is D-3 on the defeat list and it is
// the single most likely way this instrument reads as working while
// asserting nothing.
func clientIDOption() []byte {
	return dhcpv6Option(DHCPv6OptClientID, []byte{
		0x00, 0x03, 0x00, 0x01, 0x02, 0x42, 0x00, 0x14, 0x00, 0x00,
	})
}

func elapsedTimeOption() []byte {
	return dhcpv6Option(DHCPv6OptElapsedTime, []byte{0x00, 0x00})
}

// A Solicit carrying section 21.20's option decodes into every field
// the verdict reads, and each is asserted by name.
//
// THE TRANSACTION ID AND THE TYPE ARE ASSERTED FOR ParseRA's REASON,
// not because a test needs them: they are the values that change when
// an offset moves, so a decoder reading the options from the wrong
// place disagrees with this test about a number it can print, instead
// of quietly walking garbage that happens to contain no option 20.
func TestParseDHCPv6_ASolicitDecodesIntoItsFields(t *testing.T) {
	frame := buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort, DHCPv6Solicit, 0xABCDEF,
		clientIDOption(), reconfigureAcceptOption(), elapsedTimeOption())

	m, ok := ParseDHCPv6(frame)
	if !ok {
		t.Fatalf("ParseDHCPv6 refused a well-formed Solicit")
	}
	if m.Type != DHCPv6Solicit {
		t.Errorf("Type = %d (%s), want %d (SOLICIT). A wrong type here is an offset that has "+
			"moved, and every option code read after it is read from the wrong place",
			m.Type, DHCPv6MsgName(m.Type), DHCPv6Solicit)
	}
	if m.TransactionID != 0xABCDEF {
		t.Errorf("TransactionID = %06x, want abcdef — RFC 9915 section 7.3 makes it the three "+
			"octets after msg-type, and a uint32 read one byte early gives a different number",
			m.TransactionID)
	}
	if !m.FromClient {
		t.Errorf("FromClient is false for a datagram from port %d to port %d (RFC 9915 section "+
			"7.2 makes that the client's direction)", dhcpv6ClientPort, dhcpv6ServerPort)
	}
	if m.SourceMAC.String() != testV6SrcMAC {
		t.Errorf("SourceMAC = %s, want %s", m.SourceMAC, testV6SrcMAC)
	}
	want := []uint16{DHCPv6OptClientID, DHCPv6OptReconfigureAccept, DHCPv6OptElapsedTime}
	if len(m.Options) != len(want) {
		t.Fatalf("Options = %v, want %v — the option walk did not end where the datagram did",
			m.Options, want)
	}
	for i := range want {
		if m.Options[i] != want[i] {
			t.Errorf("Options[%d] = %d, want %d (whole list: %v)", i, m.Options[i], want[i], m.Options)
		}
	}
	if !m.AnnouncesReconfigureAccept() {
		t.Errorf("AnnouncesReconfigureAccept is false for a Solicit carrying option %d",
			DHCPv6OptReconfigureAccept)
	}
}

// THE OPPOSITE DIRECTION, and the reason the check is a check. The same
// builder, the same Client Identifier with 0x0014 buried in its
// payload, and no Reconfigure Accept option: the decoder must say so.
//
// A decoder that searched the 10 octets of that DUID for two bytes
// passes the test above and fails this one, which is the only way to
// tell the two implementations apart from the outside.
func TestParseDHCPv6_ADUIDContainingTheOptionCodeIsNotAnAnnouncement(t *testing.T) {
	frame := buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort, DHCPv6Solicit, 1,
		clientIDOption(), elapsedTimeOption())

	m, ok := ParseDHCPv6(frame)
	if !ok {
		t.Fatalf("ParseDHCPv6 refused a well-formed Solicit")
	}
	if m.AnnouncesReconfigureAccept() {
		t.Errorf("a Solicit with NO Reconfigure Accept option was read as announcing one. Its "+
			"Client Identifier payload contains the octets 00 14, which are option code %d "+
			"where an option begins and are a DUID's contents where this one is. Options "+
			"read: %v", DHCPv6OptReconfigureAccept, m.Options)
	}
	if len(m.Options) != 2 {
		t.Errorf("Options = %v, want exactly the two options the frame carries", m.Options)
	}
}

// A server's Reply is not a client message, and the direction is read
// off the ports.
func TestParseDHCPv6_AServerReplyIsNotAClientMessage(t *testing.T) {
	frame := buildDHCPv6Frame(t, dhcpv6ServerPort, dhcpv6ClientPort, DHCPv6Reply, 2,
		clientIDOption())

	m, ok := ParseDHCPv6(frame)
	if !ok {
		t.Fatalf("ParseDHCPv6 refused a well-formed Reply")
	}
	if m.FromClient {
		t.Errorf("FromClient is true for a datagram from port %d to port %d, which RFC 9915 "+
			"section 7.2 makes the server's direction", dhcpv6ServerPort, dhcpv6ClientPort)
	}
}

// Everything the decoder must refuse rather than half-read.
func TestParseDHCPv6_RefusesWhatItCannotRead(t *testing.T) {
	good := buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort, DHCPv6Solicit, 3,
		reconfigureAcceptOption())

	notIPv6 := append([]byte(nil), good...)
	notIPv6[12], notIPv6[13] = 0x08, 0x00

	notUDP := append([]byte(nil), good...)
	notUDP[ethHeaderLen+6] = 58 // ICMPv6 next header: an extension header would land here too

	wrongPorts := buildDHCPv6Frame(t, 1234, 5678, DHCPv6Solicit, 4, reconfigureAcceptOption())

	shortPayload := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(shortPayload[ethHeaderLen+4:ethHeaderLen+6], 2)

	for _, tc := range []struct {
		name  string
		frame []byte
		why   string
	}{
		{"truncated", good[:ethHeaderLen+ipv6HeaderLen], "a frame with no UDP header at all"},
		{"not IPv6", notIPv6, "ethertype 0800"},
		{"not UDP", notUDP, "next header 58; an IPv6 extension header lands here too, and is refused rather than walked"},
		{"neither DHCPv6 port", wrongPorts, "ports 1234/5678"},
		{"IPv6 payload length below a DHCPv6 header", shortPayload, "a header claiming 2 octets of payload"},
		{"empty", nil, "no frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ParseDHCPv6(tc.frame); ok {
				t.Errorf("ParseDHCPv6 accepted %s. A frame this instrument half-understands is "+
					"worse than one it refuses: it becomes evidence about a datagram that was "+
					"never there", tc.why)
			}
		})
	}

	if _, ok := ParseDHCPv6(good); !ok {
		t.Errorf("the control frame was refused, so every refusal above may be refusing the " +
			"control's own shape rather than the defect each case names")
	}
}

// An option whose length runs off the end of the datagram stops the
// walk and does not invent the rest.
func TestParseDHCPv6_AnOptionRunningOffTheEndStopsTheWalk(t *testing.T) {
	frame := buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort, DHCPv6Solicit, 5,
		reconfigureAcceptOption(), dhcpv6Option(DHCPv6OptClientID, []byte{1, 2, 3, 4}))

	// Lie about the last option's length so it claims more data than
	// the datagram holds. The UDP and IPv6 lengths are untouched: this
	// is a malformed option inside a well-formed datagram.
	frame[len(frame)-6] = 0xFF

	m, ok := ParseDHCPv6(frame)
	if !ok {
		t.Fatalf("ParseDHCPv6 refused a datagram whose header lengths are correct")
	}
	if len(m.Options) != 1 || m.Options[0] != DHCPv6OptReconfigureAccept {
		t.Errorf("Options = %v, want just [%d]: the options before the malformed one were whole "+
			"and stand, and the walk stops at the one that is not",
			m.Options, DHCPv6OptReconfigureAccept)
	}
}

// --- the verdict, driven in both directions -----------------------------

func clientMsg(t *testing.T, typ uint8, opts ...[]byte) DHCPv6Message {
	t.Helper()
	m, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort, typ, 9, opts...))
	if !ok {
		t.Fatalf("building a %s for the verdict: ParseDHCPv6 refused it", DHCPv6MsgName(typ))
	}
	return m
}

func serverMsg(t *testing.T, typ uint8) DHCPv6Message {
	t.Helper()
	m, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ServerPort, dhcpv6ClientPort, typ, 9, clientIDOption()))
	if !ok {
		t.Fatalf("building a %s for the verdict: ParseDHCPv6 refused it", DHCPv6MsgName(typ))
	}
	return m
}

// The verdict passes a capture in which every announcing message
// announced.
func TestReconfigureAcceptFindings_AnAnnouncingClientHasNoFindings(t *testing.T) {
	msgs := []DHCPv6Message{
		clientMsg(t, DHCPv6Solicit, clientIDOption(), reconfigureAcceptOption()),
		serverMsg(t, DHCPv6Advertise),
		clientMsg(t, DHCPv6Request, clientIDOption(), reconfigureAcceptOption()),
		serverMsg(t, DHCPv6Reply),
	}
	if got := ReconfigureAcceptFindings(msgs, DHCPv6Solicit, DHCPv6Request); len(got) > 0 {
		t.Errorf("ReconfigureAcceptFindings = %v on a capture in which both announcing messages "+
			"carried option %d", got, DHCPv6OptReconfigureAccept)
	}
}

// A Renew carries no Reconfigure Accept option and MUST NOT be counted
// against the client.
//
// RFC 9915 section 20.4.2: "The server selects a reconfigure key for a
// client during the Request/Reply, Solicit/Reply, or
// Information-request/Reply message exchange." No section 18.2 text
// names the option for a Renew or a Rebind, so a verdict that demanded
// it there would redden on a correct client at T1 — which is every
// long-running endpoint, so the check would be discharged within a day.
func TestReconfigureAcceptFindings_ARenewIsNotAnAnnouncingMessage(t *testing.T) {
	msgs := []DHCPv6Message{
		clientMsg(t, DHCPv6Solicit, clientIDOption(), reconfigureAcceptOption()),
		clientMsg(t, DHCPv6Request, clientIDOption(), reconfigureAcceptOption()),
		clientMsg(t, DHCPv6Renew, clientIDOption()),
		clientMsg(t, DHCPv6Rebind, clientIDOption()),
	}
	if got := ReconfigureAcceptFindings(msgs, DHCPv6Solicit, DHCPv6Request); len(got) > 0 {
		t.Errorf("ReconfigureAcceptFindings = %v: a Renew and a Rebind carry no Reconfigure "+
			"Accept option by RFC 9915 section 18.2.4 and section 18.2.5, and holding a client "+
			"to one there fails every renewal", got)
	}
}

// A Solicit that did not announce is a finding, and the finding names
// the message kind.
func TestReconfigureAcceptFindings_ASilentSolicitIsAFinding(t *testing.T) {
	msgs := []DHCPv6Message{
		clientMsg(t, DHCPv6Solicit, clientIDOption()),
		clientMsg(t, DHCPv6Request, clientIDOption(), reconfigureAcceptOption()),
	}
	got := ReconfigureAcceptFindings(msgs, DHCPv6Solicit, DHCPv6Request)
	if len(got) != 1 {
		t.Fatalf("ReconfigureAcceptFindings = %v, want exactly one finding for the Solicit", got)
	}
	if !strings.Contains(got[0], "SOLICIT") {
		t.Errorf("the finding does not name the message kind that failed: %q", got[0])
	}
}

// The Request half of the same, so the check is not a Solicit check
// wearing a general name. #925's opt-in has to hold in every one of
// section 21.20's three kinds, and a verdict that only ever looked at
// the first is D-5.
func TestReconfigureAcceptFindings_ASilentRequestIsAFinding(t *testing.T) {
	msgs := []DHCPv6Message{
		clientMsg(t, DHCPv6Solicit, clientIDOption(), reconfigureAcceptOption()),
		clientMsg(t, DHCPv6Request, clientIDOption()),
	}
	got := ReconfigureAcceptFindings(msgs, DHCPv6Solicit, DHCPv6Request)
	if len(got) != 1 {
		t.Fatalf("ReconfigureAcceptFindings = %v, want exactly one finding for the Request", got)
	}
	if !strings.Contains(got[0], "REQUEST") {
		t.Errorf("the finding does not name the message kind that failed: %q", got[0])
	}
}

// A required kind that never arrived is a finding, not a pass. This is
// D-5's other half: the client announced in its Solicit and the
// exchange never reached a Request, so nothing checked the Request.
func TestReconfigureAcceptFindings_ARequiredKindThatNeverArrivedIsAFinding(t *testing.T) {
	msgs := []DHCPv6Message{
		clientMsg(t, DHCPv6Solicit, clientIDOption(), reconfigureAcceptOption()),
	}
	got := ReconfigureAcceptFindings(msgs, DHCPv6Solicit, DHCPv6Request)
	if len(got) != 1 {
		t.Fatalf("ReconfigureAcceptFindings = %v, want one finding for the absent REQUEST", got)
	}
	if !strings.Contains(got[0], "REQUEST") {
		t.Errorf("the finding does not name the kind that never arrived: %q", got[0])
	}
}

// THE ONE THAT MATTERS: an empty capture is a finding.
//
// Without this arm every assertion in the integration case is true of
// the empty set, which is what a capture on the wrong vantage produces
// and what #524 was. Driven in both of its shapes, because they call
// for different repairs.
func TestReconfigureAcceptFindings_AnEmptyCaptureIsAFinding(t *testing.T) {
	t.Run("nothing at all", func(t *testing.T) {
		got := ReconfigureAcceptFindings(nil, DHCPv6Solicit, DHCPv6Request)
		if len(got) != 1 {
			t.Fatalf("ReconfigureAcceptFindings(nil) = %v, want one finding. An empty capture "+
				"makes every statement about what the client announced true by construction", got)
		}
		if !strings.Contains(got[0], "either direction") {
			t.Errorf("the finding does not say the capture was empty in both directions: %q", got[0])
		}
	})

	t.Run("the server's half only", func(t *testing.T) {
		msgs := []DHCPv6Message{serverMsg(t, DHCPv6Advertise), serverMsg(t, DHCPv6Reply)}
		got := ReconfigureAcceptFindings(msgs, DHCPv6Solicit, DHCPv6Request)
		if len(got) != 1 {
			t.Fatalf("ReconfigureAcceptFindings = %v, want one finding", got)
		}
		if !strings.Contains(got[0], "vantage") {
			t.Errorf("a capture holding the server's half and none of the client's is a capture "+
				"on the wrong side, and the finding must say so rather than report a silent "+
				"segment: %q", got[0])
		}
	})
}

// A second DHCPv6 client on the link takes the verdict away, and does
// not get this plugin blamed for what it did or did not announce.
//
// THE CAPTURE CANNOT NAME ITS SUBJECT. ReconfigureAcceptFindings
// selects on direction and on message type, and a Solicit is a Solicit
// whoever sent it: a container left behind by an earlier case, or
// anything else that speaks DHCPv6 on a shared bridge, is read as the
// endpoint under test. That fails both ways -- a stranger's silent
// Solicit reddens the lane against this plugin, and a stranger's
// announcing Solicit would satisfy an assertion the plugin never met.
// So more than one ethernet source among the client messages ends the
// verdict instead of producing one.
func TestReconfigureAcceptFindings_TwoClientsOnTheLinkVoidTheVerdict(t *testing.T) {
	const strangerMAC = "02:42:ac:11:00:09"

	ours, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort,
		DHCPv6Solicit, 0x111111, clientIDOption(), reconfigureAcceptOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused this plugin's Solicit")
	}
	stranger, ok := ParseDHCPv6(buildDHCPv6FrameFrom(t, strangerMAC, dhcpv6ClientPort,
		dhcpv6ServerPort, DHCPv6Solicit, 0x222222, clientIDOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused the stranger's Solicit")
	}

	findings := ReconfigureAcceptFindings([]DHCPv6Message{ours, stranger}, DHCPv6Solicit)
	if len(findings) != 1 {
		t.Fatalf("two clients on the link produced %d finding(s), want exactly one that withholds "+
			"the verdict: %v", len(findings), findings)
	}
	got := findings[0]
	for _, want := range []string{testV6SrcMAC, strangerMAC, "more than one"} {
		if !strings.Contains(got, want) {
			t.Errorf("the finding does not contain %q, so the reader cannot tell which speakers "+
				"were on the link: %s", want, got)
		}
	}
	if strings.Contains(got, "carried no Reconfigure Accept option") {
		t.Errorf("the finding accuses a client of not announcing, on a link where the verdict "+
			"cannot say whose message that was: %s", got)
	}
}

// The same two messages from ONE client are judged, and this is the
// control for the test above.
//
// Without it, the refusal above is satisfied by a function that
// withheld its verdict always, and #925's whole assertion would be
// silently gone. Here both frames carry the same ethernet source: the
// silent one IS reported.
func TestReconfigureAcceptFindings_OneClientThatWentSilentOnceIsStillJudged(t *testing.T) {
	announced, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort,
		DHCPv6Solicit, 0x111111, clientIDOption(), reconfigureAcceptOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused the announcing Solicit")
	}
	silent, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort,
		DHCPv6Request, 0x222222, clientIDOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused the silent Request")
	}

	findings := ReconfigureAcceptFindings([]DHCPv6Message{announced, silent},
		DHCPv6Solicit, DHCPv6Request)
	if len(findings) != 1 {
		t.Fatalf("one client whose REQUEST carried no Reconfigure Accept option produced %d "+
			"finding(s), want exactly one: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0], "REQUEST") ||
		!strings.Contains(findings[0], "carried no Reconfigure Accept option") {
		t.Errorf("the finding does not name the silent REQUEST: %s", findings[0])
	}
}

// An INFORMATION-REQUEST is an announcing message, and a stateless
// client's only one.
//
// RFC 9915 section 20.4.2 names Information-request/Reply as one of the
// three exchanges a server may choose a reconfigure key in, and a
// stateless endpoint performs no other: it sends no Solicit and no
// Request, so if this kind were left out of the announcing set, a
// stateless client could never be reconfigured and nothing would say
// so. The integration case on the stateless fixture requires exactly
// this kind, and this is the fast-lane half of it.
func TestReconfigureAcceptFindings_ASilentInformationRequestIsAFinding(t *testing.T) {
	silent, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort,
		DHCPv6InformationRequest, 0x333333, clientIDOption(), elapsedTimeOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused a well-formed Information-request")
	}

	findings := ReconfigureAcceptFindings([]DHCPv6Message{silent}, DHCPv6InformationRequest)
	if len(findings) != 1 {
		t.Fatalf("a silent INFORMATION-REQUEST produced %d finding(s), want exactly one: %v",
			len(findings), findings)
	}
	if !strings.Contains(findings[0], "INFORMATION-REQUEST") {
		t.Errorf("the finding does not name the message kind: %s", findings[0])
	}

	announcing, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort,
		DHCPv6InformationRequest, 0x444444, clientIDOption(), reconfigureAcceptOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused the announcing Information-request")
	}
	if f := ReconfigureAcceptFindings([]DHCPv6Message{announcing}, DHCPv6InformationRequest); len(f) != 0 {
		t.Errorf("an announcing INFORMATION-REQUEST produced findings, so the stateless case "+
			"would be red on a client that did everything right: %v", f)
	}
}

// A second client that never announces does NOT take the verdict away.
//
// THE REFUSAL HAS A DOMAIN AND THIS IS IT. The set that decides whether
// the subject is ambiguous is the ANNOUNCING messages, not every client
// datagram on the link: a stranger renewing its own lease says nothing
// about section 21.20 and leaves this endpoint's Solicit unambiguous.
// Counting every client message instead would withhold a verdict the
// capture can give, and on a shared bridge it would do so on every run.
func TestReconfigureAcceptFindings_AStrangerThatNeverAnnouncesLeavesTheVerdictStanding(t *testing.T) {
	const strangerMAC = "02:42:ac:11:00:09"

	silent, ok := ParseDHCPv6(buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort,
		DHCPv6Solicit, 0x555555, clientIDOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused this endpoint's Solicit")
	}
	strangerRenew, ok := ParseDHCPv6(buildDHCPv6FrameFrom(t, strangerMAC, dhcpv6ClientPort,
		dhcpv6ServerPort, DHCPv6Renew, 0x666666, clientIDOption()))
	if !ok {
		t.Fatal("ParseDHCPv6 refused the stranger's Renew")
	}

	findings := ReconfigureAcceptFindings([]DHCPv6Message{silent, strangerRenew}, DHCPv6Solicit)
	if len(findings) != 1 {
		t.Fatalf("a silent SOLICIT beside a stranger's RENEW produced %d finding(s), want the one "+
			"about the SOLICIT: %v", len(findings), findings)
	}
	if !strings.Contains(findings[0], "carried no Reconfigure Accept option") {
		t.Errorf("the verdict was withheld because another client sent a RENEW, which announces "+
			"nothing and leaves this endpoint's SOLICIT unambiguous: %s", findings[0])
	}
}
