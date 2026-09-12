// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"net"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/runtime"
	"github.com/claymore666/dhcp-golib/wire"
)

// The frames below are built by the CLIENT LIBRARY'S OWN encoders --
// wire.Encode for the DHCP message, runtime.BuildIPv4UDP for the IPv4
// and UDP headers -- and not typed out by hand.
//
// That is the whole point of testing this parser in the fast lane. A
// decoder pinned to bytes no client in this repo produces is a decoder
// tested against nothing (v6signature.go records the run where exactly
// that happened: frames captured with an argv the fixture does not
// use). These are the bytes the plugin's client puts on the wire,
// because they come out of the same two functions that put them there;
// the only part built here is the ethernet header, which the kernel
// prepends from the SockaddrLinklayer the library hands it.
const (
	testClientMAC = "02:42:c0:a8:63:0a"
	testServerMAC = "02:42:c0:a8:63:01"
)

func mustParseMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return m
}

// clientFrame assembles one frame the way the library's packet
// transport does: ethernet header, then BuildIPv4UDP over the encoded
// DHCP message.
func clientFrame(t *testing.T, m *wire.Message, src, dst netip.Addr, dstMAC string, sport, dport uint16) []byte {
	t.Helper()
	payload, err := wire.Encode(m)
	if err != nil {
		t.Fatalf("wire.Encode: %v", err)
	}
	ttl := uint8(64)
	if dst == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		ttl = 1
	}
	datagram, err := runtime.BuildIPv4UDP(src, dst, sport, dport, 1, ttl, payload)
	if err != nil {
		t.Fatalf("runtime.BuildIPv4UDP: %v", err)
	}
	frame := make([]byte, 0, ethHeaderLen+len(datagram))
	frame = append(frame, mustParseMAC(t, dstMAC)...)
	frame = append(frame, mustParseMAC(t, testClientMAC)...)
	frame = append(frame, 0x08, 0x00)
	return append(frame, datagram...)
}

func renewalMessage(t *testing.T, ciaddr string) *wire.Message {
	t.Helper()
	m := &wire.Message{
		Op:     wire.BootRequest,
		HType:  1,
		XID:    0xdeadbeef,
		CIAddr: netip.MustParseAddr(ciaddr),
		CHAddr: mustParseMAC(t, testClientMAC),
	}
	m.SetType(wire.MsgRequest)
	return m
}

// TestParseDHCPv4Request_RenewalRequest is the frame the whole
// instrument exists for: the DHCPREQUEST a client sends to extend a
// lease it already holds.
func TestParseDHCPv4Request_RenewalRequest(t *testing.T) {
	frame := clientFrame(t, renewalMessage(t, "192.168.99.10"),
		netip.MustParseAddr("192.168.99.10"), netip.MustParseAddr("192.168.99.1"),
		testServerMAC, runtime.ClientPort, runtime.ServerPort)

	m, ok := ParseDHCPv4Request(frame)
	if !ok {
		t.Fatal("the renewal request the client library encodes was not recognised at all; " +
			"every count this instrument takes would be zero on a wire full of them")
	}
	if !m.IsRenewalRequest() {
		t.Errorf("%s was not read as a renewal request", m)
	}
	if m.ClientMAC.String() != testClientMAC {
		t.Errorf("chaddr = %s, want %s", m.ClientMAC, testClientMAC)
	}
	if m.CIAddr.String() != "192.168.99.10" {
		t.Errorf("ciaddr = %s, want 192.168.99.10", m.CIAddr)
	}
	if m.Broadcast {
		t.Error("a unicast renewal was read as a broadcast; that is the RENEWING/REBINDING " +
			"distinction, and reading it backwards would misreport which state an outage reached")
	}
	if m.XID != 0xdeadbeef {
		t.Errorf("xid = %08x, want deadbeef", m.XID)
	}
}

// TestParseDHCPv4Request_AcquisitionIsNotARenewal is the population
// control, and it is the same boundary the library's countSent draws.
//
// RFC 2131 Table 5 gives ciaddr as zero in the SELECTING and
// INIT-REBOOT columns. A DISCOVER and a SELECTING REQUEST are how a
// client that has NO lease behaves, and a client with no lease has
// nothing to renew: an instrument that counted them would report a
// clean acquisition as an outage.
func TestParseDHCPv4Request_AcquisitionIsNotARenewal(t *testing.T) {
	bcast := netip.AddrFrom4([4]byte{255, 255, 255, 255})
	zero := netip.AddrFrom4([4]byte{})

	discover := &wire.Message{Op: wire.BootRequest, HType: 1, XID: 1, CHAddr: mustParseMAC(t, testClientMAC)}
	discover.SetType(wire.MsgDiscover)

	selecting := &wire.Message{Op: wire.BootRequest, HType: 1, XID: 2, CHAddr: mustParseMAC(t, testClientMAC)}
	selecting.SetType(wire.MsgRequest)

	for name, msg := range map[string]*wire.Message{
		"DISCOVER":          discover,
		"REQUEST/SELECTING": selecting,
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := ParseDHCPv4Request(clientFrame(t, msg, zero, bcast,
				"ff:ff:ff:ff:ff:ff", runtime.ClientPort, runtime.ServerPort))
			if !ok {
				t.Fatalf("%s did not parse; the capture would show an empty wire during an "+
					"acquisition and could not prove it was ever able to see this client", name)
			}
			if m.IsRenewalRequest() {
				t.Errorf("%s was counted as a renewal request: %s", name, m)
			}
			if !m.Broadcast {
				t.Errorf("%s was not read as a broadcast", name)
			}
		})
	}
}

// TestParseDHCPv4Request_RebindIsARenewalRequest. A REBINDING REQUEST
// is broadcast and carries ciaddr, and the library counts it in
// RenewalsSent. The instrument must count the same population or the
// comparison the integration test makes is between two different
// questions.
func TestParseDHCPv4Request_RebindIsARenewalRequest(t *testing.T) {
	frame := clientFrame(t, renewalMessage(t, "192.168.99.10"),
		netip.MustParseAddr("192.168.99.10"), netip.AddrFrom4([4]byte{255, 255, 255, 255}),
		"ff:ff:ff:ff:ff:ff", runtime.ClientPort, runtime.ServerPort)

	m, ok := ParseDHCPv4Request(frame)
	if !ok {
		t.Fatal("a broadcast REQUEST carrying ciaddr did not parse")
	}
	if !m.IsRenewalRequest() {
		t.Errorf("a REBINDING request was not counted: %s", m)
	}
	if !m.Broadcast {
		t.Errorf("a REBINDING request was not read as a broadcast: %s", m)
	}
}

// TestParseDHCPv4Request_OtherClientMessagesCarryCIAddrToo is the
// second half of the population boundary, and the message type is what
// draws it.
//
// A DHCPRELEASE names the binding it is giving back in 'ciaddr' (RFC
// 2131 section 4.4.4) and a DHCPINFORM carries the client's address
// there too (section 3.4). Both are client messages, both are unicast
// to port 67, and both would sail through a predicate that tested only
// for a non-zero 'ciaddr' -- which would count a container being
// removed as a server that stopped answering.
func TestParseDHCPv4Request_OtherClientMessagesCarryCIAddrToo(t *testing.T) {
	for name, typ := range map[string]wire.MessageType{
		"RELEASE": wire.MsgRelease,
		"INFORM":  wire.MsgInform,
	} {
		t.Run(name, func(t *testing.T) {
			msg := renewalMessage(t, "192.168.99.10")
			msg.SetType(typ)

			m, ok := ParseDHCPv4Request(clientFrame(t, msg,
				netip.MustParseAddr("192.168.99.10"), netip.MustParseAddr("192.168.99.1"),
				testServerMAC, runtime.ClientPort, runtime.ServerPort))
			if !ok {
				t.Fatalf("a %s did not parse at all", name)
			}
			if m.IsRenewalRequest() {
				t.Errorf("a %s carrying ciaddr was counted as a renewal request: %s", name, m)
			}
		})
	}
}

// TestParseDHCPv4Request_RejectsWhatIsNotAClientMessage. The socket
// underneath is ETH_P_ALL, so most of what reaches this function is
// something else. Each rejection here is a frame that would otherwise
// be counted as a renewal request nobody sent.
func TestParseDHCPv4Request_RejectsWhatIsNotAClientMessage(t *testing.T) {
	reply := &wire.Message{
		Op: 2, HType: 1, XID: 9,
		YIAddr: netip.MustParseAddr("192.168.99.10"),
		CHAddr: mustParseMAC(t, testClientMAC),
	}
	reply.SetType(wire.MsgAck)
	serverFrame := clientFrame(t, reply,
		netip.MustParseAddr("192.168.99.1"), netip.MustParseAddr("192.168.99.10"),
		testClientMAC, runtime.ServerPort, runtime.ClientPort)

	// A BOOTREPLY addressed to port 67 is what a relay agent's traffic
	// looks like, and it is the one shape the destination port does not
	// reject. Without the op check it would be read as a client asking.
	relayed := clientFrame(t, reply,
		netip.MustParseAddr("192.168.99.1"), netip.MustParseAddr("192.168.99.2"),
		testServerMAC, runtime.ServerPort, runtime.ServerPort)

	arp := make([]byte, 60)
	copy(arp[12:14], []byte{0x08, 0x06})

	for name, frame := range map[string][]byte{
		"a server's DHCPACK":           serverFrame,
		"a DHCPACK relayed to port 67": relayed,
		"an ARP frame":                 arp,
		"an empty frame":               {},
		"a runt":                       make([]byte, 42),
	} {
		t.Run(name, func(t *testing.T) {
			if m, ok := ParseDHCPv4Request(frame); ok {
				t.Errorf("%s was read as a client DHCP message: %s", name, m)
			}
		})
	}
}

// TestParseDHCPv4Request_TruncationNeverPanics. The capture hands this
// function whatever came off the wire, including frames cut short by
// the read buffer. A panic in the read loop kills the capture, and a
// dead capture reports an empty wire -- which is the answer that makes
// every count below it look like a passing measurement.
func TestParseDHCPv4Request_TruncationNeverPanics(t *testing.T) {
	frame := clientFrame(t, renewalMessage(t, "192.168.99.10"),
		netip.MustParseAddr("192.168.99.10"), netip.MustParseAddr("192.168.99.1"),
		testServerMAC, runtime.ClientPort, runtime.ServerPort)

	for i := 0; i <= len(frame); i++ {
		if m, ok := ParseDHCPv4Request(frame[:i]); ok && i < len(frame) {
			// A short frame may legitimately still carry everything
			// this parser reads; what must not happen is a panic or a
			// renewal claimed out of bytes that were never there.
			if m.IsRenewalRequest() && i < ethHeaderLen+20+udpHeaderLen+bootpMinLen {
				t.Fatalf("a %d-byte prefix was read as a renewal request", i)
			}
		}
	}
}

// TestDHCPMessageType_WalksPadsAndStopsAtEnd. Option 53 is not
// guaranteed to be first and the field is not guaranteed to be tightly
// packed; a walk that assumed either would read the type out of
// whatever option happened to be at the front.
func TestDHCPMessageType_WalksPadsAndStopsAtEnd(t *testing.T) {
	cases := map[string]struct {
		opts []byte
		want uint8
	}{
		"type first":           {[]byte{53, 1, 3, 255}, 3},
		"pads then type":       {[]byte{0, 0, 53, 1, 3, 255}, 3},
		"another option first": {[]byte{61, 7, 1, 2, 3, 4, 5, 6, 7, 53, 1, 3, 255}, 3},
		"end before the type":  {[]byte{255, 53, 1, 3}, 0},
		"no options at all":    {nil, 0},
		"length runs off":      {[]byte{61, 40, 1, 2, 3}, 0},
		"truncated type":       {[]byte{53}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := dhcpMessageType(c.opts); got != c.want {
				t.Errorf("dhcpMessageType = %d, want %d", got, c.want)
			}
		})
	}
}
