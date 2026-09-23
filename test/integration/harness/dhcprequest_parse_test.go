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

// Frames are built with the client library's own encoders, wire.Encode and runtime.BuildIPv4UDP; only the ethernet
// header, which the kernel prepends from the library's SockaddrLinklayer, is built here (#940).
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

// clientFrame builds an ethernet header, then BuildIPv4UDP over the encoded DHCP message.
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

// RFC 2131 Table 5: ciaddr is zero in SELECTING and INIT-REBOOT, the boundary the library's countSent draws too.
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

// A REBINDING REQUEST is broadcast with ciaddr, and the library counts it in RenewalsSent.
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

// DHCPRELEASE (RFC 2131 section 4.4.4) and DHCPINFORM (section 3.4) carry ciaddr too.
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

// The socket is ETH_P_ALL, so most frames reaching the parser are not client DHCP messages.
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

	// A BOOTREPLY to port 67 is relay traffic, which the port does not reject.
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

func TestParseDHCPv4Request_TruncationNeverPanics(t *testing.T) {
	frame := clientFrame(t, renewalMessage(t, "192.168.99.10"),
		netip.MustParseAddr("192.168.99.10"), netip.MustParseAddr("192.168.99.1"),
		testServerMAC, runtime.ClientPort, runtime.ServerPort)

	for i := 0; i <= len(frame); i++ {
		if m, ok := ParseDHCPv4Request(frame[:i]); ok && i < len(frame) {
			if m.IsRenewalRequest() && i < ethHeaderLen+20+udpHeaderLen+bootpMinLen {
				t.Fatalf("a %d-byte prefix was read as a renewal request", i)
			}
		}
	}
}

// Option 53 need not be first, and pad options may sit between options.
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
