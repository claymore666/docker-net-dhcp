package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// The IPv6 and UDP headers, built and parsed by hand — ipudp.go's counterpart,
// and it exists for the same reason that one does rather than for symmetry.
//
// A DHCPv6 client's first message goes out from a link-local address the
// kernel formed and to ff02::1:2, and the library installs nothing: no address
// of its own, no route, no sysctl (seam design section 5). A UDP socket cannot
// send that datagram — it would need the source address bound, and binding a
// source the caller has not been given is the one thing this library does not
// do — so the transport is AF_PACKET here as it is for v4, and the headers are
// ours to build.

// ClientPort6 and ServerPort6 are the DHCPv6 ports. RFC 9915 section 7.2:
// "Clients MUST listen for DHCP messages on UDP port 546.  Servers and relay
// agents MUST listen for DHCP messages on UDP port 547.  Therefore, clients
// MUST send DHCP messages to UDP destination port 547."
const (
	ClientPort6 = 546
	ServerPort6 = 547
)

// AllDHCPRelayAgentsAndServers is the only destination this client sends a
// DHCPv6 message to. RFC 9915 section 7.1:
// "All_DHCP_Relay_Agents_and_Servers (ff02::1:2) A link-scoped multicast
// address used by a client to communicate with neighboring (i.e., on-link)
// relay agents and servers.  All servers and relay agents are members of this
// multicast group."
//
// THERE IS NO UNICAST DESTINATION, and that is RFC 9915's change rather than a
// simplification here. Section 16: "Servers SHOULD NOT accept unicast traffic
// from clients.  The Server Unicast option (see Section 21.12) and
// UseMulticast status code (see Section 21.13) have been obsoleted; hence,
// clients should no longer send messages to a server's unicast address nor
// receive the UseMulticast status code." So every message of every exchange —
// the Renew to the server that granted the lease included — is multicast here.
var AllDHCPRelayAgentsAndServers = netip.MustParseAddr("ff02::1:2")

const (
	ipv6HeaderLen = 40
	ipv6Version   = 6
	// NDHopLimit is RFC 4861 section 6.1.2's "The IP Hop Limit field has a
	// value of 255, i.e., the packet could not possibly have been forwarded
	// by a router." It is written on every Neighbor Discovery message this
	// library sends and required on every one it admits.
	NDHopLimit = 255
	// dhcpHopLimit is what a link-scoped multicast DHCPv6 message carries. It
	// is not 255: nothing in RFC 9915 gives the client's messages a hop-limit
	// rule, and ff02:: is link-scoped by definition (RFC 4291 section 2.7),
	// so a router that forwards it is already violating the scope.
	dhcpHopLimit = 1
	// The extension headers this walk understands, from RFC 8200 section 4.
	extHopByHop  = 0
	extRouting   = 43
	extFragment  = 44
	extDestOpts  = 60
	extNoNextHdr = 59
)

// The refusals of a received IPv6 frame. Like ipudp.go's, most of these mean
// "not for us" rather than "something is wrong": a packet socket sees every
// frame on the link.
var (
	// ErrNotIPv6Frame is a frame whose version nibble is not 6.
	ErrNotIPv6Frame = errors.New("runtime: not IPv6")
	// ErrExtensionHeader is an IPv6 extension header chain that cannot be
	// walked: a header that runs past the payload, or a Fragment header,
	// which this transport does not reassemble.
	ErrExtensionHeader = errors.New("runtime: IPv6 extension header chain cannot be walked")
	// ErrZeroChecksum6 is an inbound UDP datagram whose checksum field is
	// zero.
	//
	// IT IS A REFUSAL AND NOT A STATE, which is the one place this file
	// deliberately does NOT follow ipudp.go. RFC 8200 section 8.1: "IPv6
	// receivers must discard UDP packets containing a zero checksum and
	// should log the error." ipudp.go's ChecksumAbsent accepts the v4
	// equivalent because RFC 768 allows it over IPv4; the sentence pinned in
	// that file's ChecksumAbsent doc — "NOT TRANSFERABLE TO IPv6" — is this
	// error.
	ErrZeroChecksum6 = errors.New("runtime: IPv6 UDP datagram carries a zero checksum, which RFC 8200 section 8.1 says to discard")
	// ErrNotICMPv6 is a frame whose upper-layer protocol is not ICMPv6.
	ErrNotICMPv6 = errors.New("runtime: not ICMPv6")
)

// BuildIPv6UDP wraps payload in a UDP datagram inside an IPv6 packet.
//
// THE CHECKSUM IS MANDATORY AND THERE IS NO ZERO CASE. RFC 8200 section 8.1:
// "Unlike IPv4, the default behavior when UDP packets are originated by an
// IPv6 node is that the UDP checksum is not optional.  That is, whenever
// originating a UDP packet, an IPv6 node must compute a UDP checksum over the
// packet and the pseudo-header, and, if that computation yields a result of
// zero, it must be changed to hex FFFF for placement in the UDP header."
//
// The pseudo-header is the section 8.1 one — the two 128-bit addresses, the
// upper-layer length and the Next Header value — and not v4's. A codec that
// used the v4 pseudo-header would agree with itself and with nothing else.
func BuildIPv6UDP(src, dst netip.Addr, sport, dport uint16, hopLimit uint8, payload []byte) ([]byte, error) {
	if err := requireIPv6Addr(src, "source"); err != nil {
		return nil, err
	}
	if err := requireIPv6Addr(dst, "destination"); err != nil {
		return nil, err
	}
	ulen := udpHeaderLen + len(payload)
	if ulen > 0xFFFF {
		return nil, fmt.Errorf("runtime: UDP datagram %d bytes exceeds the IPv6 payload length field", ulen)
	}
	s16, d16 := src.As16(), dst.As16()

	buf := make([]byte, ipv6HeaderLen+ulen)
	buf[0] = ipv6Version << 4
	// buf[1:4] is the rest of the traffic class and the flow label, zero.
	binary.BigEndian.PutUint16(buf[4:6], uint16(ulen))
	buf[6] = protoUDP
	buf[7] = hopLimit
	copy(buf[8:24], s16[:])
	copy(buf[24:40], d16[:])

	u := buf[ipv6HeaderLen:]
	binary.BigEndian.PutUint16(u[0:2], sport)
	binary.BigEndian.PutUint16(u[2:4], dport)
	binary.BigEndian.PutUint16(u[4:6], uint16(ulen))
	copy(u[udpHeaderLen:], payload)
	binary.BigEndian.PutUint16(u[6:8], udpChecksum6(s16, d16, u))

	return buf, nil
}

// BuildIPv6ICMP wraps one wire.ICMPv6Packet in an IPv6 header.
//
// THE HEADER'S ADDRESSES ARE THE PACKET'S OWN AND NOTHING ELSE CHOOSES THEM.
// RFC 4443 section 2.3 computes the ICMPv6 checksum over a pseudo-header made
// of the source and destination addresses, so ring 0 has already committed to
// a pair; writing any other pair here produces a frame every receiver drops,
// silently, because a bad ICMPv6 checksum produces no error anywhere. That is
// why lease.ND.Send takes a wire.ICMPv6Packet and not a []byte.
//
// The hop limit is 255 for every message this builds: RFC 4861 section 6.1.2
// makes it a validity check on the receiver, so a Router Solicitation or a
// duplicate-address Neighbor Solicitation sent with any other value is
// discarded by a conforming node.
func BuildIPv6ICMP(pkt wire.ICMPv6Packet) ([]byte, error) {
	if err := requireIPv6Addr(pkt.Src, "ICMPv6 source"); err != nil {
		return nil, err
	}
	if err := requireIPv6Addr(pkt.Dst, "ICMPv6 destination"); err != nil {
		return nil, err
	}
	if len(pkt.Body) < 4 {
		return nil, fmt.Errorf("runtime: ICMPv6 body is %d octet(s), shorter than its own header", len(pkt.Body))
	}
	s16, d16 := pkt.Src.As16(), pkt.Dst.As16()
	buf := make([]byte, ipv6HeaderLen+len(pkt.Body))
	buf[0] = ipv6Version << 4
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(pkt.Body)))
	buf[6] = wire.ICMPv6NextHeader
	buf[7] = NDHopLimit
	copy(buf[8:24], s16[:])
	copy(buf[24:40], d16[:])
	copy(buf[ipv6HeaderLen:], pkt.Body)
	return buf, nil
}

// DatagramV6 is one parsed UDP datagram off an IPv6 frame.
//
// IT CARRIES A ChecksumState AND ONE OF THAT TYPE'S VALUES CAN NEVER APPEAR
// HERE, which is the difference from ipudp.go's Datagram rather than a
// duplicate of it: over IPv6 ONE of the two states v4 reports as "accepted but
// unchecked" is a refusal. The other is not, and that is measured rather than
// assumed — see ParseIPv6UDP.
type DatagramV6 struct {
	// Payload aliases the frame passed in.
	Payload []byte
	// Src and Dst are the IPv6 addresses of the frame.
	Src, Dst netip.Addr
	// Checksum says what the checksum field bought, and it is the SAME
	// ChecksumState ipudp.go declares because it is the same enumeration —
	// with one value this function never returns. ChecksumAbsent is a refusal
	// over IPv6 (ErrZeroChecksum6), so a DatagramV6 is Verified or
	// Uncompleted and nothing else, and a caller that reads it as a bool gets
	// the same answer here as there.
	Checksum ChecksumState
}

// ParseIPv6UDP extracts the UDP payload of a DHCPv6 reply from a raw IPv6
// frame, refusing anything not addressed from the server port to the client
// port.
//
// BOTH PORTS ARE CHECKED, and the source port is the half v4 does not check.
// It excludes another client's message to 547 on this link, and anything else
// that happens to be talking to 546. It does NOT exist to exclude this
// client's own transmissions: those never arrive here at all, which is
// MEASURED and not assumed — see PacketTransportV6's bounds.
//
// THE ZERO CHECKSUM IS DISCARDED AND THE PSEUDO-HEADER SUM IS NOT, and the
// difference between those two is the whole of what this function knows that
// ParseIPv4UDP does not.
//
// RFC 8200 section 8.1: "IPv6 receivers must discard UDP packets containing a
// zero checksum and should log the error." That is ErrZeroChecksum6, and it is
// the sentence ipudp.go's ChecksumAbsent doc points at when it says its own
// acceptance is NOT TRANSFERABLE TO IPv6.
//
// Linux's CHECKSUM_PARTIAL is a different value with a different cause and it
// is accepted, exactly as v4 accepts it. A locally generated datagram leaves
// the kernel with the FOLDED PSEUDO-HEADER SUM in the checksum field and the
// completion deferred, so an AF_PACKET reader on the far side of a veth pair
// sees the frame before anything finishes it. MEASURED in this milestone
// rather than argued: dnsmasq 2.91's DHCPv6 Advertise over a veth pair carried
// checksum 0x28a0, which is precisely the folded pseudo-header sum of that
// frame's two addresses, its length 106 and Next Header 17 — and the receiving
// kernel accepted the same datagram, answering with an ICMPv6 port
// unreachable. Refusing it would not produce a stricter client; it would
// produce one that cannot lease from a server on the same host, which is every
// test this library has. The bound is acceptUDPChecksum's, unchanged: read
// Uncompleted as "unchecked", never as "probably fine", because its accepting
// value is a pure function of fields read from the frame itself.
//
// THE ORDER OF THE TWO ACCEPTING ARMS IS LOAD-BEARING, for acceptUDPChecksum's
// reason: a frame can satisfy both and must be reported Verified.
//
// BOUND: fragments are refused rather than reassembled, for ParseIPv4UDP's
// reason. A Fragment header (RFC 8200 section 4.5) ends the walk with
// ErrExtensionHeader.
func ParseIPv6UDP(frame []byte) (DatagramV6, error) {
	src, dst, next, body, err := ipv6Upper(frame)
	if err != nil {
		return DatagramV6{}, err
	}
	if next != protoUDP {
		return DatagramV6{}, ErrNotUDP
	}
	if len(body) < udpHeaderLen {
		return DatagramV6{}, ErrShortFrame
	}
	if binary.BigEndian.Uint16(body[0:2]) != ServerPort6 ||
		binary.BigEndian.Uint16(body[2:4]) != ClientPort6 {
		return DatagramV6{}, ErrWrongPort
	}
	ulen := int(binary.BigEndian.Uint16(body[4:6]))
	if ulen < udpHeaderLen || ulen > len(body) {
		return DatagramV6{}, ErrPayloadShort
	}
	u := body[:ulen]
	s16, d16 := src.As16(), dst.As16()
	var state ChecksumState
	switch got := binary.BigEndian.Uint16(u[6:8]); {
	case got == 0:
		return DatagramV6{}, ErrZeroChecksum6
	case udpChecksumVerify6(s16, d16, u) == 0:
		state = ChecksumVerified
	case got == pseudoHeaderSum6(s16, d16, len(u)):
		state = ChecksumUncompleted
	default:
		return DatagramV6{}, fmt.Errorf("%w: UDP over IPv6", ErrBadChecksum)
	}
	return DatagramV6{Payload: u[udpHeaderLen:], Src: src, Dst: dst, Checksum: state}, nil
}

// pseudoHeaderSum6 is the folded one's-complement sum of RFC 8200 section
// 8.1's pseudo-header alone — no payload. It is what Linux leaves in the
// checksum field of a datagram it has not finished checksumming; see
// ParseIPv6UDP.
func pseudoHeaderSum6(src, dst [16]byte, ulen int) uint16 {
	return sum16(nil, pseudoBase6(src, dst, ulen, protoUDP))
}

// ICMPv6Frame is one ICMPv6 message off an IPv6 frame, with the three fields
// RFC 4861 section 6.1.2's validity checks are made against that are not in
// the ICMPv6 body: the two addresses and the hop limit.
type ICMPv6Frame struct {
	Src, Dst netip.Addr
	HopLimit uint8
	// Body aliases the frame passed in.
	Body []byte
}

// ParseIPv6ICMP extracts the ICMPv6 message of a raw IPv6 frame.
//
// IT DOES NOT VERIFY THE CHECKSUM AND IT DOES NOT CHECK THE HOP LIMIT. Both
// are RFC 4861 section 6.1.2 checks and both are made by the caller —
// NDSocket — because a frame that fails one is COUNTED there, and a parse
// error and a validity failure are two different counters. This function's
// job is to say where the ICMPv6 message is and which addresses its checksum
// was computed over.
func ParseIPv6ICMP(frame []byte) (ICMPv6Frame, error) {
	src, dst, next, body, err := ipv6Upper(frame)
	if err != nil {
		return ICMPv6Frame{}, err
	}
	if next != wire.ICMPv6NextHeader {
		return ICMPv6Frame{}, ErrNotICMPv6
	}
	if len(body) < 4 {
		return ICMPv6Frame{}, ErrShortFrame
	}
	return ICMPv6Frame{Src: src, Dst: dst, HopLimit: frame[7], Body: body}, nil
}

// ipv6Upper walks RFC 8200 section 4's header chain and returns the two
// addresses, the upper-layer protocol number and the upper-layer body.
//
// The body is bounded by the PAYLOAD LENGTH FIELD and not by len(frame), for
// ParseIPv4UDP's reason: a SOCK_DGRAM read hands back whatever the link
// padded the frame to, and summing that padding into a checksum breaks it.
//
// A Fragment header ends the walk. Reassembly is timers and a denial-of-
// service surface for a case that means the server is doing something exotic
// with a message that fits in one frame by construction.
func ipv6Upper(frame []byte) (src, dst netip.Addr, next uint8, body []byte, err error) {
	if len(frame) < ipv6HeaderLen {
		return src, dst, 0, nil, ErrShortFrame
	}
	if frame[0]>>4 != ipv6Version {
		return src, dst, 0, nil, ErrNotIPv6Frame
	}
	plen := int(binary.BigEndian.Uint16(frame[4:6]))
	if ipv6HeaderLen+plen > len(frame) {
		return src, dst, 0, nil, ErrShortFrame
	}
	src = netip.AddrFrom16([16]byte(frame[8:24]))
	dst = netip.AddrFrom16([16]byte(frame[24:40]))

	next = frame[6]
	rest := frame[ipv6HeaderLen : ipv6HeaderLen+plen]
	// A bound on the walk itself, not on the chain's contents: eight headers
	// is more than any legitimate DHCPv6 reply carries, and an unbounded walk
	// over attacker-supplied lengths is how a parser becomes a loop.
	for range 8 {
		switch next {
		case extHopByHop, extRouting, extDestOpts:
			if len(rest) < 8 {
				return src, dst, 0, nil, ErrExtensionHeader
			}
			n := (int(rest[1]) + 1) * 8
			if n > len(rest) {
				return src, dst, 0, nil, ErrExtensionHeader
			}
			next, rest = rest[0], rest[n:]
		case extFragment, extNoNextHdr:
			return src, dst, 0, nil, ErrExtensionHeader
		default:
			return src, dst, next, rest, nil
		}
	}
	return src, dst, 0, nil, ErrExtensionHeader
}

// udpChecksum6 computes RFC 8200 section 8.1's UDP checksum. u must have its
// checksum field already zeroed.
func udpChecksum6(src, dst [16]byte, u []byte) uint16 {
	c := ^pseudoSum6(src, dst, u)
	// Section 8.1: "if that computation yields a result of zero, it must be
	// changed to hex FFFF for placement in the UDP header."
	if c == 0 {
		return 0xFFFF
	}
	return c
}

// udpChecksumVerify6 sums a datagram whose checksum field is populated. A
// correct datagram sums to 0xFFFF, so the complement is zero.
func udpChecksumVerify6(src, dst [16]byte, u []byte) uint16 {
	return ^pseudoSum6(src, dst, u)
}

// pseudoBase6 is the folded sum of RFC 8200 section 8.1's pseudo-header: the
// two 128-bit addresses, the Upper-Layer Packet Length and the Next Header
// value in the low octet of a 16-bit word whose high octet is the "zero" the
// figure names.
func pseudoBase6(src, dst [16]byte, ulen int, next uint8) uint32 {
	var sum uint32
	for i := 0; i+1 < len(src); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(src[i : i+2]))
	}
	for i := 0; i+1 < len(dst); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(dst[i : i+2]))
	}
	sum += uint32(ulen) >> 16
	sum += uint32(ulen) & 0xFFFF
	sum += uint32(next)
	return sum
}

func pseudoSum6(src, dst [16]byte, u []byte) uint16 {
	return sum16(u, pseudoBase6(src, dst, len(u), protoUDP))
}

func requireIPv6Addr(a netip.Addr, what string) error {
	if !a.Is6() || a.Is4In6() {
		return fmt.Errorf("%w: %s %s", ErrNotIPv6Frame, what, a)
	}
	return nil
}
