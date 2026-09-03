package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// The IPv4 and UDP headers, built and parsed by hand: a DHCP client's first
// message goes out over an interface the kernel has no address on, to a
// destination that is not routable, carrying source 0.0.0.0 (RFC 2131 section
// 4.1, "the client MUST set the IP source address to 0"). No socket API will
// produce that packet, which is why the transport is AF_PACKET.

// ClientPort and ServerPort are the DHCP ports, bootpc and bootps. RFC 2131
// section 4.1.
const (
	ClientPort = 68
	ServerPort = 67
)

const (
	ipv4HeaderLen = 20
	udpHeaderLen  = 8
	protoUDP      = 17
	ipv4Version   = 4
)

// Errors from parsing a received frame. These mean "not for us", not
// "something is wrong": a raw socket sees every packet on the link.
var (
	ErrNotIPv4      = errors.New("runtime: not IPv4")
	ErrNotUDP       = errors.New("runtime: not UDP")
	ErrShortFrame   = errors.New("runtime: frame shorter than its headers")
	ErrWrongPort    = errors.New("runtime: not a DHCP client port")
	ErrFragmented   = errors.New("runtime: fragmented IPv4 datagram")
	ErrBadChecksum  = errors.New("runtime: bad checksum")
	ErrPayloadShort = errors.New("runtime: UDP length exceeds frame")
)

// BuildIPv4UDP wraps payload in a UDP datagram inside an IPv4 packet.
//
// The UDP checksum is optional over IPv4 (RFC 768 allows the all-zero "not
// computed" value) and is computed anyway: zero is legal and is also what a
// broken implementation emits, so computing it removes a reading.
//
// ident is caller-supplied so that this stays a pure function of its inputs
// and a golden-bytes test is possible. RFC 6864 section 4.1 permits any value
// for a datagram that will not be fragmented, and a DHCP message is far below
// any link MTU.
func BuildIPv4UDP(src, dst netip.Addr, sport, dport uint16, ident uint16, ttl uint8, payload []byte) ([]byte, error) {
	if !src.Is4() || !dst.Is4() {
		return nil, fmt.Errorf("%w: src %s dst %s", ErrNotIPv4, src, dst)
	}
	total := ipv4HeaderLen + udpHeaderLen + len(payload)
	if total > 0xFFFF {
		return nil, fmt.Errorf("runtime: datagram %d bytes exceeds IPv4 maximum", total)
	}
	s4, d4 := src.As4(), dst.As4()

	buf := make([]byte, total)
	buf[0] = ipv4Version<<4 | ipv4HeaderLen/4
	buf[1] = 0 // DSCP/ECN
	binary.BigEndian.PutUint16(buf[2:4], uint16(total))
	binary.BigEndian.PutUint16(buf[4:6], ident)
	binary.BigEndian.PutUint16(buf[6:8], 0) // no flags, no fragment offset
	buf[8] = ttl
	buf[9] = protoUDP
	// buf[10:12] is the header checksum, left zero while it is computed.
	copy(buf[12:16], s4[:])
	copy(buf[16:20], d4[:])
	binary.BigEndian.PutUint16(buf[10:12], checksum(buf[:ipv4HeaderLen]))

	u := buf[ipv4HeaderLen:]
	binary.BigEndian.PutUint16(u[0:2], sport)
	binary.BigEndian.PutUint16(u[2:4], dport)
	binary.BigEndian.PutUint16(u[4:6], uint16(udpHeaderLen+len(payload)))
	copy(u[udpHeaderLen:], payload)
	binary.BigEndian.PutUint16(u[6:8], udpChecksum(s4, d4, u))

	return buf, nil
}

// ChecksumState says what a received datagram's UDP checksum field bought us.
// Three states and not a bool because "not checked" has two causes with
// different diagnoses, and an operator cannot act on them merged.
type ChecksumState uint8

const (
	// ChecksumVerified: the field held a correct checksum over the datagram
	// and its pseudo-header.
	ChecksumVerified ChecksumState = iota
	// ChecksumAbsent: the field was all zeroes, which RFC 768 reserves for
	// "no checksum computed" and which is legal over IPv4.
	//
	// NOT TRANSFERABLE TO IPv6. RFC 8200 section 8.1 requires an IPv6
	// receiver to DISCARD a UDP packet with a zero checksum, so copying this
	// acceptance into a v6 path unchanged is a conformance break, not a
	// widening. Narrow exceptions exist for tunnelled traffic. Whoever writes
	// v6 reads that section itself — a summarised RFC has already been
	// observed in this project asserting the inverse of its own MUST with the
	// section number intact.
	ChecksumAbsent
	// ChecksumUncompleted: the field held the pseudo-header sum alone, which
	// is what Linux leaves in a datagram whose checksum it has deferred to
	// hardware (CHECKSUM_PARTIAL). See acceptUDPChecksum.
	//
	// It does NOT say the sender is on this host. The code sees a field
	// holding a value; locality is an INFERENCE about the commonest producer
	// of that value and runs one way only.
	ChecksumUncompleted
)

// Verified reports whether the payload was actually checked.
func (c ChecksumState) Verified() bool { return c == ChecksumVerified }

func (c ChecksumState) String() string {
	switch c {
	case ChecksumVerified:
		return "verified"
	case ChecksumAbsent:
		return "absent"
	case ChecksumUncompleted:
		return "uncompleted"
	default:
		return fmt.Sprintf("checksumstate(%d)", uint8(c))
	}
}

// Datagram is one parsed UDP datagram. Checksum travels with the payload
// because a caller that cannot see it cannot report it.
type Datagram struct {
	// Payload aliases the frame passed in.
	Payload []byte
	// Src is the IPv4 source address of the frame.
	Src netip.Addr
	// Checksum says whether the payload was verified, and if not, why not.
	Checksum ChecksumState
}

// ParseIPv4UDP extracts the UDP payload of a DHCP reply from a raw IPv4 frame.
// It returns ErrWrongPort for anything not addressed to the client port, which
// on a shared link is most of what arrives.
//
// BOUND: fragments are refused, not reassembled — reassembly is timers and a
// denial-of-service surface for a case that means the server is doing
// something exotic. A fragmented reply is dropped and the client retransmits
// until it gives up.
func ParseIPv4UDP(frame []byte) (Datagram, error) {
	if len(frame) < ipv4HeaderLen {
		return Datagram{}, ErrShortFrame
	}
	if frame[0]>>4 != ipv4Version {
		return Datagram{}, ErrNotIPv4
	}
	ihl := int(frame[0]&0x0F) * 4
	if ihl < ipv4HeaderLen || len(frame) < ihl {
		return Datagram{}, ErrShortFrame
	}
	if frame[9] != protoUDP {
		return Datagram{}, ErrNotUDP
	}
	// More-fragments set, or a non-zero fragment offset.
	if frame[6]&0x20 != 0 || (uint16(frame[6]&0x1F)<<8|uint16(frame[7])) != 0 {
		return Datagram{}, ErrFragmented
	}
	if checksum(frame[:ihl]) != 0 {
		return Datagram{}, fmt.Errorf("%w: IPv4 header", ErrBadChecksum)
	}

	// The IPv4 total-length field, not len(frame): a SOCK_DGRAM read can hand
	// back the trailing padding an Ethernet frame carries up to its 60-octet
	// minimum, which would break the UDP checksum below.
	// TestParseUsesTheTotalLengthField.
	total := int(binary.BigEndian.Uint16(frame[2:4]))
	if total < ihl || total > len(frame) {
		return Datagram{}, ErrShortFrame
	}
	u := frame[ihl:total]
	if len(u) < udpHeaderLen {
		return Datagram{}, ErrShortFrame
	}
	if binary.BigEndian.Uint16(u[2:4]) != ClientPort {
		return Datagram{}, ErrWrongPort
	}
	ulen := int(binary.BigEndian.Uint16(u[4:6]))
	if ulen < udpHeaderLen || ulen > len(u) {
		return Datagram{}, ErrPayloadShort
	}
	u = u[:ulen]

	var s4, d4 [4]byte
	copy(s4[:], frame[12:16])
	copy(d4[:], frame[16:20])
	state, ok := acceptUDPChecksum(s4, d4, u)
	if !ok {
		return Datagram{}, fmt.Errorf("%w: UDP", ErrBadChecksum)
	}
	return Datagram{
		Payload:  u[udpHeaderLen:],
		Src:      netip.AddrFrom4(s4),
		Checksum: state,
	}, nil
}

// acceptUDPChecksum decides whether a received datagram's checksum field lets
// it through, and says which state it is in. Only the second case verifies
// anything.
//
// The third case, the pseudo-header sum alone, is Linux's CHECKSUM_PARTIAL and
// not corruption: for a locally generated datagram the kernel writes
// ~csum_tcpudp_magic(saddr, daddr, len, IPPROTO_UDP, 0) — the folded
// pseudo-header sum — and leaves completing it to the hardware, so an
// AF_PACKET reader on the far side of a veth pair sees the frame first.
// Refusing it would not produce a stricter client, it would produce one that
// cannot lease from a server on the same host. Measured against a real dnsmasq
// OFFER in ipudp_test.go: realOfferField, realOfferCompleted,
// TestParseAcceptsARealServersUncompletedChecksum.
//
// BOUND: the zero case and the pseudo-header case both accept a corrupt
// payload, the zero case more cheaply — its accepting value is the constant
// zero. The pseudo-header case is no harder to hit on purpose: its accepting
// value is a pure function of source, destination and UDP length, all read
// from the frame itself, so anyone who can put a frame on the link can compute
// it. Read both as "unchecked", never as "probably fine":
// TestAnUncheckedChecksumAcceptsACorruptPayload,
// TestThePseudoHeaderSumIsBlindToThePayload.
//
// The ORDER of the arms below is load-bearing and is not obvious: a frame can
// satisfy both the verify arm and the pseudo-header arm at once, and it must
// be reported as Verified. TestAFrameThatIsBothVerifiedAndPseudoHeaderSum
// constructs one.
//
// PACKET_AUXDATA's TP_STATUS_CSUMNOTREADY would close it — the kernel saying
// outright that it deferred the sum, which would let a frame without the flag
// be held to a correct checksum. New I/O with its own failure modes; it
// belongs to a milestone.
func acceptUDPChecksum(src, dst [4]byte, u []byte) (state ChecksumState, ok bool) {
	got := binary.BigEndian.Uint16(u[6:8])
	switch {
	case got == 0:
		return ChecksumAbsent, true
	case udpChecksumVerify(src, dst, u) == 0:
		return ChecksumVerified, true
	case got == pseudoHeaderSum(src, dst, len(u)):
		return ChecksumUncompleted, true
	default:
		return ChecksumVerified, false
	}
}

// checksum is the one's-complement sum of 16-bit words, complemented (RFC 1071).
func checksum(b []byte) uint16 {
	return ^sum16(b, 0)
}

// sum16 folds b into sum as 16-bit big-endian words and returns the folded
// one's-complement sum. RFC 1071 section 4.1.
func sum16(b []byte, sum uint32) uint16 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 == 1 {
		// An odd trailing byte is the HIGH byte of the final word: the
		// datagram is padded on the right with a zero, and that pad is not
		// transmitted.
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	return uint16(sum)
}

// udpChecksum computes the UDP checksum over the pseudo-header and datagram.
// u must have its checksum field already zeroed.
func udpChecksum(src, dst [4]byte, u []byte) uint16 {
	c := ^pseudoSum(src, dst, u)
	// RFC 768: a computed checksum of zero is transmitted as all ones, because
	// zero is reserved to mean "no checksum".
	if c == 0 {
		return 0xFFFF
	}
	return c
}

// udpChecksumVerify sums a datagram whose checksum field is populated. A
// correct datagram sums to 0xFFFF, so the complement is zero.
func udpChecksumVerify(src, dst [4]byte, u []byte) uint16 {
	return ^pseudoSum(src, dst, u)
}

// pseudoHeaderSum is the folded one's-complement sum of the UDP pseudo-header
// alone — no payload. It is what Linux leaves in the checksum field of a
// datagram it has not finished checksumming; see acceptUDPChecksum.
func pseudoHeaderSum(src, dst [4]byte, ulen int) uint16 {
	return sum16(nil, pseudoBase(src, dst, ulen))
}

func pseudoBase(src, dst [4]byte, ulen int) uint32 {
	var sum uint32
	sum += uint32(binary.BigEndian.Uint16(src[0:2]))
	sum += uint32(binary.BigEndian.Uint16(src[2:4]))
	sum += uint32(binary.BigEndian.Uint16(dst[0:2]))
	sum += uint32(binary.BigEndian.Uint16(dst[2:4]))
	sum += uint32(protoUDP)
	sum += uint32(ulen)
	return sum
}

func pseudoSum(src, dst [4]byte, u []byte) uint16 {
	return sum16(u, pseudoBase(src, dst, len(u)))
}
