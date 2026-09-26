// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// Frames are built field by field so a decoder reading a wrong offset disagrees about a value the test names; an
// off-by-one ParseRA once returned the same flags for three segment modes (#925).

const (
	testV6SrcMAC = "02:42:ac:11:00:02"
	testV6DstMAC = "33:33:00:01:00:02"
)

// dhcpv6Option encodes an RFC 9915 section 21.1 option: code, length, data.
func dhcpv6Option(code uint16, data []byte) []byte {
	b := make([]byte, 4+len(data))
	binary.BigEndian.PutUint16(b[0:2], code)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(data)))
	copy(b[4:], data)
	return b
}

func buildDHCPv6Frame(t *testing.T, srcPort, dstPort uint16, msgType uint8, xid uint32, opts ...[]byte) []byte {
	t.Helper()
	return buildDHCPv6FrameFrom(t, testV6SrcMAC, srcPort, dstPort, msgType, xid, opts...)
}

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

// reconfigureAcceptOption is RFC 9915 section 21.20's option, with option-len 0.
func reconfigureAcceptOption() []byte {
	return dhcpv6Option(DHCPv6OptReconfigureAccept, nil)
}

// clientIDOption is the plugin's DUID with 0x00 0x14 (option 20, RFC 9915 section 21.20) planted inside it, which a
// decoder that searches bytes or walks from a wrong offset reads as Reconfigure Accept.
func clientIDOption() []byte {
	return dhcpv6Option(DHCPv6OptClientID, []byte{
		0x00, 0x03, 0x00, 0x01, 0x02, 0x42, 0x00, 0x14, 0x00, 0x00,
	})
}

func elapsedTimeOption() []byte {
	return dhcpv6Option(DHCPv6OptElapsedTime, []byte{0x00, 0x00})
}

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

func TestParseDHCPv6_RefusesWhatItCannotRead(t *testing.T) {
	good := buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort, DHCPv6Solicit, 3,
		reconfigureAcceptOption())

	notIPv6 := append([]byte(nil), good...)
	notIPv6[12], notIPv6[13] = 0x08, 0x00

	notUDP := append([]byte(nil), good...)
	notUDP[ethHeaderLen+6] = 58

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

func TestParseDHCPv6_AnOptionRunningOffTheEndStopsTheWalk(t *testing.T) {
	frame := buildDHCPv6Frame(t, dhcpv6ClientPort, dhcpv6ServerPort, DHCPv6Solicit, 5,
		reconfigureAcceptOption(), dhcpv6Option(DHCPv6OptClientID, []byte{1, 2, 3, 4}))

	// The last option claims more data than the datagram holds; the UDP and IPv6 lengths stay valid.
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

// RFC 9915 section 20.4.2 picks the reconfigure key in Request, Solicit or Information-request exchanges and section
// 18.2 names the option for no Renew or Rebind, so a Renew without it is correct.
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

// An empty capture is what a wrong vantage produces (#524).
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

// A stateless client sends only Information-request, one of RFC 9915 section 20.4.2's three exchanges.
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

func TestParseDHCPv6_KeepsTheClientFQDNValue(t *testing.T) {
	value := ClientFQDNOption(0x01, "web1")
	if want := []byte{0x01, 4, 'w', 'e', 'b', '1'}; !bytes.Equal(value, want) {
		t.Fatalf("ClientFQDNOption(0x01, web1) = % x, want % x (RFC 4704 section 4.1)", value, want)
	}
	if got := ClientFQDNOption(0x00, "a.bc"); !bytes.Equal(got, []byte{0, 1, 'a', 2, 'b', 'c'}) {
		t.Fatalf("ClientFQDNOption(0, a.bc) = % x", got)
	}

	frame := buildDHCPv6Frame(t, 546, 547, DHCPv6Solicit, 0x0a0b0c, clientIDOption(),
		dhcpv6Option(DHCPv6OptClientFQDN, value), dhcpv6Option(DHCPv6OptClientFQDN, ClientFQDNOption(0x04, "x")))
	m, ok := ParseDHCPv6(frame)
	if !ok {
		t.Fatal("the frame did not parse")
	}
	if !bytes.Equal(m.ClientFQDN, value) || !m.HasOption(DHCPv6OptClientFQDN) {
		t.Errorf("ClientFQDN = % x, want the first option 39's value % x", m.ClientFQDN, value)
	}

	m, ok = ParseDHCPv6(buildDHCPv6Frame(t, 546, 547, DHCPv6Solicit, 0x0a0b0c, clientIDOption()))
	if !ok || m.ClientFQDN != nil {
		t.Errorf("a Solicit with no option 39 has ClientFQDN % x (ok %v), want nil", m.ClientFQDN, ok)
	}
}

// The expected value must equal the client's encoding, so a mismatch is caught here, not as a wire fault (#1029).
func TestClientFQDNOption_MatchesTheLibrarysEncoding(t *testing.T) {
	for _, name := range []string{"web1", "dh-itest-fqdn6-ctr", "a.b"} {
		want, err := wire.EncodeClientFQDN(wire.ClientFQDNFlagS, name)
		if err != nil {
			t.Fatalf("EncodeClientFQDN(%q): %v", name, err)
		}
		if got := ClientFQDNOption(0x01, name); !bytes.Equal(got, want) {
			t.Errorf("ClientFQDNOption(0x01, %q) = %x, the library encodes %x", name, got, want)
		}
	}
}
