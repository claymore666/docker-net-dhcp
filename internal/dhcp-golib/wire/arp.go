package wire

import (
	"errors"
	"fmt"
	"net/netip"
)

// The ARP wire format (RFC 826), for the one hardware and protocol pair this
// library speaks: Ethernet and IPv4.
//
// It lives in ring 0 beside the DHCP codec because it is a codec — bytes in,
// values out, no clock and no socket — and because RFC 5227's Probe and
// Announcement are DEFINED as particular ARP Request packets, so the thing
// that builds them has to be able to say exactly which bytes go where.
//
// THE ETHERNET/IPv4 RESTRICTION IS A REFUSAL, NOT AN ASSUMPTION. A frame with
// another 'hardware type', another 'protocol type', or an address length that
// does not match them is refused by name rather than parsed with the lengths
// it declares. RFC 5227's rules are all statements about the sender and target
// IPv4 addresses of an Ethernet ARP; a Token Ring or IPv6-over-ARP frame
// carries no such fields at the offsets this package would read, and decoding
// one leniently produces addresses that are not addresses.
const (
	// ARPHTypeEthernet is RFC 826's 'ar$hrd' for 10Mb Ethernet, which is what
	// every IEEE 802 link in RFC 5227 section 1.3 uses.
	ARPHTypeEthernet uint16 = 1
	// ARPPTypeIPv4 is 'ar$pro' for IPv4, the EtherType.
	ARPPTypeIPv4 uint16 = 0x0800
	// ARPHLenEthernet and ARPPLenIPv4 are 'ar$hln' and 'ar$pln'.
	ARPHLenEthernet uint8 = 6
	ARPPLenIPv4     uint8 = 4
	// ARPPacketLen is the encoded length of an Ethernet/IPv4 ARP packet: the
	// eight-octet fixed head plus two hardware and two protocol addresses.
	ARPPacketLen = 8 + 2*int(ARPHLenEthernet) + 2*int(ARPPLenIPv4)
)

// ARPOp is RFC 826's 'ar$op'.
type ARPOp uint16

// The two operations RFC 5227 section 2.4 names: "an ARP packet (Request *or*
// Reply)".
const (
	ARPRequest ARPOp = 1
	ARPReply   ARPOp = 2
)

func (o ARPOp) String() string {
	switch o {
	case ARPRequest:
		return "ARP-Request"
	case ARPReply:
		return "ARP-Reply"
	default:
		return fmt.Sprintf("arp-op(%d)", uint16(o))
	}
}

// ARPPacket is one decoded Ethernet/IPv4 ARP packet.
//
// The field names are RFC 5227 section 1.1's, which names them for exactly the
// reason this struct exists: "Wherever this document uses the term 'sender IP
// address' or 'target IP address' in the context of an ARP packet, it is
// referring to the fields of the ARP packet identified in the ARP
// specification [RFC826] as 'ar$spa' (Sender Protocol Address) and 'ar$tpa'
// (Target Protocol Address)". Every conflict rule in that document is a
// predicate over these four fields and the operation, so they are carried
// apart and never folded together.
type ARPPacket struct {
	Op ARPOp

	// SenderHW is 'ar$sha' and TargetHW is 'ar$tha', each six octets.
	SenderHW []byte
	TargetHW []byte

	// SenderIP is 'ar$spa' and TargetIP is 'ar$tpa'. Both are always IPv4 in
	// a packet this codec accepts; SenderIP is the ZERO address in an ARP
	// Probe, which is the whole of what makes a Probe a Probe.
	SenderIP netip.Addr
	TargetIP netip.Addr
}

// IsProbe reports whether p is RFC 5227's 'ARP Probe'.
//
// Section 1.1, verbatim: "the term 'ARP Probe' is used to refer to an ARP
// Request packet, broadcast on the local link, with an all-zero 'sender IP
// address'." Both halves are load-bearing and a Reply with a zero sender is
// NOT a Probe: section 2.1.1's second conflict rule applies to Probes only,
// and reading a Reply as one would treat a malformed reply as somebody else's
// probe.
func (p *ARPPacket) IsProbe() bool {
	return p.Op == ARPRequest && p.SenderIP.Is4() && p.SenderIP.IsUnspecified()
}

func (p *ARPPacket) String() string {
	return fmt.Sprintf("%s spa=%s sha=%s tpa=%s", p.Op, p.SenderIP, hwString(p.SenderHW), p.TargetIP)
}

func hwString(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 3*len(b)-1)
	for i, c := range b {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexd[c>>4], hexd[c&0x0f])
	}
	return string(out)
}

// The refusals. Distinct values so a caller can tell a truncated frame from a
// well-formed frame for a family this codec does not speak — the first is a
// damaged link or a bug, the second is ordinary traffic on a shared segment.
var (
	// ErrARPShort is a frame shorter than an Ethernet/IPv4 ARP packet.
	ErrARPShort = errors.New("wire: ARP frame is shorter than an Ethernet/IPv4 ARP packet")
	// ErrARPFamily is a frame whose hardware or protocol type is not
	// Ethernet/IPv4.
	ErrARPFamily = errors.New("wire: ARP frame is not Ethernet/IPv4")
	// ErrARPAddrLen is a frame whose declared address lengths do not match the
	// family it declares.
	ErrARPAddrLen = errors.New("wire: ARP frame declares address lengths that are not Ethernet/IPv4's")
	// ErrARPEncode is a packet that cannot be encoded.
	ErrARPEncode = errors.New("wire: ARP packet cannot be encoded")
)

// DecodeARP parses one Ethernet/IPv4 ARP packet.
//
// The length check comes FIRST and is against the whole packet, not against
// the head: a frame carrying the eight-octet head and nothing else declares
// four addresses that are not there, and a decoder that trusted 'ar$hln' after
// reading it would index past the slice. RFC 5227 section 2.4's rule is
// evaluated on every ARP packet a host receives, which on a shared link
// includes whatever else is on it.
//
// TRAILING OCTETS ARE ACCEPTED AND IGNORED. The minimum Ethernet payload is 46
// octets and an ARP packet is 28, so a conformant sender pads; refusing the
// padding would refuse most of the ARP on a real link.
func DecodeARP(b []byte) (*ARPPacket, error) {
	if len(b) < ARPPacketLen {
		return nil, fmt.Errorf("%w: %d octets, want at least %d", ErrARPShort, len(b), ARPPacketLen)
	}
	htype := ube16(b[0:2])
	ptype := ube16(b[2:4])
	if htype != ARPHTypeEthernet || ptype != ARPPTypeIPv4 {
		return nil, fmt.Errorf("%w: hardware type %d, protocol type %#04x", ErrARPFamily, htype, ptype)
	}
	if b[4] != ARPHLenEthernet || b[5] != ARPPLenIPv4 {
		return nil, fmt.Errorf("%w: hlen %d, plen %d", ErrARPAddrLen, b[4], b[5])
	}
	p := &ARPPacket{
		Op:       ARPOp(ube16(b[6:8])),
		SenderHW: append([]byte(nil), b[8:14]...),
		SenderIP: getAddr(b[14:18]),
		TargetHW: append([]byte(nil), b[18:24]...),
		TargetIP: getAddr(b[24:28]),
	}
	return p, nil
}

// EncodeARP renders one Ethernet/IPv4 ARP packet.
//
// It refuses a hardware address that is not six octets and a protocol address
// that is not IPv4, rather than truncating or padding either. RFC 5227 section
// 2.1.1 makes the sender hardware address a MUST ("The client MUST fill in the
// 'sender hardware address' field of the ARP Request with the hardware address
// of the interface through which it is sending the packet"), and a probe that
// went out with a truncated one would be answered to nobody — which is
// indistinguishable, from here, from an address that is free.
func EncodeARP(p *ARPPacket) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil packet", ErrARPEncode)
	}
	if len(p.SenderHW) != int(ARPHLenEthernet) {
		return nil, fmt.Errorf("%w: sender hardware address is %d octets, want %d",
			ErrARPEncode, len(p.SenderHW), ARPHLenEthernet)
	}
	// The target hardware address is "ignored and SHOULD be set to all zeroes"
	// in a Probe (section 2.1.1), so an empty slice is the ordinary case and
	// encodes as zeroes. A slice of the wrong non-zero length is still a
	// caller error.
	if len(p.TargetHW) != 0 && len(p.TargetHW) != int(ARPHLenEthernet) {
		return nil, fmt.Errorf("%w: target hardware address is %d octets, want %d or none",
			ErrARPEncode, len(p.TargetHW), ARPHLenEthernet)
	}
	if !p.SenderIP.Is4() {
		return nil, fmt.Errorf("%w: sender IP %s is not IPv4", ErrARPEncode, p.SenderIP)
	}
	if !p.TargetIP.Is4() {
		return nil, fmt.Errorf("%w: target IP %s is not IPv4", ErrARPEncode, p.TargetIP)
	}

	b := make([]byte, ARPPacketLen)
	be16(b[0:2], ARPHTypeEthernet)
	be16(b[2:4], ARPPTypeIPv4)
	b[4] = ARPHLenEthernet
	b[5] = ARPPLenIPv4
	be16(b[6:8], uint16(p.Op))
	copy(b[8:14], p.SenderHW)
	putAddr(b[14:18], p.SenderIP)
	copy(b[18:24], p.TargetHW)
	putAddr(b[24:28], p.TargetIP)
	return b, nil
}
