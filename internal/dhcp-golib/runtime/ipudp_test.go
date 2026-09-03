package runtime

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"testing"
)

var (
	anyAddr       = netip.AddrFrom4([4]byte{0, 0, 0, 0})
	broadcastAddr = netip.AddrFrom4([4]byte{255, 255, 255, 255})
)

// goldenFrame is a DHCP-shaped IPv4/UDP frame computed OUTSIDE this package,
// by an independent implementation of RFC 1071's checksum, and pasted here.
//
// Independence is the whole value. A golden produced by running BuildIPv4UDP
// and pasting its output agrees with whatever the encoder does today,
// including whatever it does wrong — and a checksum is exactly the field where
// "it round-trips with itself" is worthless, because a receiver on the far
// side is the only party that ever disagrees.
//
// src 0.0.0.0, dst 255.255.255.255, sport 68, dport 67, ident 0x1234, ttl 1,
// payload "hello!". IPv4 header checksum 0xa798, UDP checksum 0xbb58.
const goldenFrame = "45000022123400000111a79800000000ffffffff00440043000ebb5868656c6c6f21"

func TestBuildMatchesAnIndependentGolden(t *testing.T) {
	want, err := hex.DecodeString(goldenFrame)
	if err != nil {
		t.Fatalf("bad golden: %v", err)
	}
	got, err := BuildIPv4UDP(anyAddr, broadcastAddr, ClientPort, ServerPort, 0x1234, 1, []byte("hello!"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	if hex.EncodeToString(got) != goldenFrame {
		t.Fatalf("frame differs from the independently computed golden\n got: %x\nwant: %x", got, want)
	}
}

// TestReceiverInvariants checks what a REAL receiver checks: the one's
// complement sum over the header, including its checksum field, is 0xFFFF —
// so the complement is zero. This is the property the far side applies, and it
// is not the same statement as "our two functions agree".
func TestReceiverInvariants(t *testing.T) {
	frame, err := BuildIPv4UDP(
		netip.MustParseAddr("192.168.99.50"), netip.MustParseAddr("192.168.99.1"),
		ClientPort, ServerPort, 0xBEEF, 64, []byte("a DHCP payload of odd length"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	if got := checksum(frame[:20]); got != 0 {
		t.Fatalf("IPv4 header does not verify: complement = %#04x, want 0", got)
	}
	var s4, d4 [4]byte
	copy(s4[:], frame[12:16])
	copy(d4[:], frame[16:20])
	if got := udpChecksumVerify(s4, d4, frame[20:]); got != 0 {
		t.Fatalf("UDP datagram does not verify: complement = %#04x, want 0", got)
	}
}

func TestBuildParseRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"empty", nil},
		{"one octet", []byte{0x5A}},
		{"odd length", []byte("odd")},
		{"even length", []byte("even")},
		{"a full DHCP message", make([]byte, 300)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A REPLY: server port to client port. ParseIPv4UDP is the
			// receive side and refuses anything not addressed to the client
			// port, which on a raw socket is most of the segment's traffic.
			frame, err := BuildIPv4UDP(anyAddr, broadcastAddr, ServerPort, ClientPort, 1, 1, tc.payload)
			if err != nil {
				t.Fatalf("BuildIPv4UDP: %v", err)
			}
			dg, err := ParseIPv4UDP(frame)
			if err != nil {
				t.Fatalf("ParseIPv4UDP: %v", err)
			}
			if dg.Checksum != ChecksumVerified {
				t.Fatalf("a frame this package built parsed as %s, want verified", dg.Checksum)
			}
			got, src := dg.Payload, dg.Src
			if len(got) != len(tc.payload) {
				t.Fatalf("payload length %d, want %d", len(got), len(tc.payload))
			}
			for i := range got {
				if got[i] != tc.payload[i] {
					t.Fatalf("payload octet %d = %#x, want %#x", i, got[i], tc.payload[i])
				}
			}
			if src != anyAddr {
				t.Fatalf("source = %s, want 0.0.0.0", src)
			}
		})
	}
}

// TestParseUsesTheTotalLengthField is the Ethernet padding case, and it is a
// real defect rather than a hypothetical: a short DHCP reply on Ethernet is
// padded to the 60-octet minimum frame more often than not, and a parser that
// summed to len(frame) fails the UDP checksum on exactly those replies —
// which is to say, on the ones from a real server.
func TestParseUsesTheTotalLengthField(t *testing.T) {
	frame, err := BuildIPv4UDP(anyAddr, broadcastAddr, ServerPort, ClientPort, 1, 1, []byte("short"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	// The padding is NON-ZERO on purpose. Zero padding adds nothing to a
	// one's-complement sum, so a parser that summed to len(frame) would pass
	// this test anyway — MEASURED by mutation on 2026-08-29, where replacing
	// frame[ihl:total] with frame[ihl:] survived a zero-padded fixture. Real
	// padding is not reliably zero either: CVE-2003-0001 was an entire class
	// of drivers padding short frames with whatever was in the buffer.
	pad := make([]byte, 27)
	for i := range pad {
		pad[i] = byte(0xA0 + i)
	}
	padded := append(append([]byte(nil), frame...), pad...)

	dg, err := ParseIPv4UDP(padded)
	if err != nil {
		t.Fatalf("a padded frame was rejected: %v", err)
	}
	got := dg.Payload
	if string(got) != "short" {
		t.Fatalf("payload = %q, want %q", got, "short")
	}
}

func TestParseRejects(t *testing.T) {
	good, err := BuildIPv4UDP(anyAddr, broadcastAddr, ServerPort, ClientPort, 1, 1, []byte("x"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	mutate := func(f func([]byte)) []byte {
		b := append([]byte(nil), good...)
		f(b)
		return b
	}

	cases := []struct {
		name  string
		frame []byte
		want  error
	}{
		{"too short for a header", good[:10], ErrShortFrame},
		{"not IPv4", mutate(func(b []byte) { b[0] = 0x65 }), ErrNotIPv4},
		{"IHL below the minimum", mutate(func(b []byte) { b[0] = 0x43 }), ErrShortFrame},
		{"not UDP", mutate(func(b []byte) { b[9] = 6 }), ErrNotUDP},
		{"more fragments", mutate(func(b []byte) { b[6] |= 0x20 }), ErrFragmented},
		{"non-zero fragment offset", mutate(func(b []byte) { b[7] = 1 }), ErrFragmented},
		{"bad IPv4 checksum", mutate(func(b []byte) { b[10] ^= 0xFF }), ErrBadChecksum},
		{"wrong destination port", mutate(func(b []byte) {
			binary.BigEndian.PutUint16(b[22:24], 9999)
			// The IP header is untouched, so only the port check can reject
			// this one. Its UDP checksum is now wrong too, which is why the
			// port test must come first for this case to mean anything.
		}), ErrWrongPort},
		{"total length past the frame", mutate(func(b []byte) {
			binary.BigEndian.PutUint16(b[2:4], 9000)
			binary.BigEndian.PutUint16(b[10:12], 0)
			binary.BigEndian.PutUint16(b[10:12], checksum(b[:20]))
		}), ErrShortFrame},
		{"UDP length past the datagram", mutate(func(b []byte) {
			binary.BigEndian.PutUint16(b[24:26], 500)
		}), ErrPayloadShort},
		{"bad UDP checksum", mutate(func(b []byte) { b[28] ^= 0xFF }), ErrBadChecksum},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseIPv4UDP(tc.frame)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// The preservation control: the unmutated frame still parses. Eleven
	// refusals prove nothing if the parser refuses everything.
	if _, err := ParseIPv4UDP(good); err != nil {
		t.Fatalf("the unmutated frame was rejected: %v", err)
	}
}

func TestParseAcceptsAZeroUDPChecksum(t *testing.T) {
	// RFC 768 allows an all-zero UDP checksum over IPv4 to mean "not
	// computed". Refusing it would drop replies from servers that do not
	// compute one.
	frame, err := BuildIPv4UDP(anyAddr, broadcastAddr, ServerPort, ClientPort, 1, 1, []byte("x"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	binary.BigEndian.PutUint16(frame[26:28], 0)
	dg, err := ParseIPv4UDP(frame)
	if err != nil {
		t.Fatalf("a zero UDP checksum was rejected: %v", err)
	}
	if dg.Checksum != ChecksumAbsent {
		t.Fatalf("a zero checksum parsed as %s, want absent", dg.Checksum)
	}
}

func TestParseSkipsOptionedHeaders(t *testing.T) {
	// A 24-octet IPv4 header with four octets of options. Nothing we send has
	// them; a router or a server on the path can add them, and a parser that
	// assumed 20 would read the UDP header four octets early and reject a
	// perfectly good reply.
	inner, err := BuildIPv4UDP(anyAddr, broadcastAddr, ServerPort, ClientPort, 1, 1, []byte("opt"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	frame := make([]byte, 0, len(inner)+4)
	frame = append(frame, inner[:20]...)
	frame = append(frame, 0, 0, 0, 0) // four octets of IPv4 options (all NOP/EOL)
	frame = append(frame, inner[20:]...)
	frame[0] = 0x46                                            // IHL = 6 words
	binary.BigEndian.PutUint16(frame[2:4], uint16(len(frame))) // total length
	binary.BigEndian.PutUint16(frame[10:12], 0)                // clear checksum
	binary.BigEndian.PutUint16(frame[10:12], checksum(frame[:24]))

	dg, err := ParseIPv4UDP(frame)
	if err != nil {
		t.Fatalf("an optioned IPv4 header was rejected: %v", err)
	}
	got := dg.Payload
	if string(got) != "opt" {
		t.Fatalf("payload = %q", got)
	}
}

func TestBuildRejectsNonIPv4(t *testing.T) {
	v6 := netip.MustParseAddr("2001:db8::1")
	if _, err := BuildIPv4UDP(v6, broadcastAddr, 68, 67, 1, 1, nil); !errors.Is(err, ErrNotIPv4) {
		t.Fatalf("a v6 source was accepted: %v", err)
	}
	if _, err := BuildIPv4UDP(anyAddr, v6, 68, 67, 1, 1, nil); !errors.Is(err, ErrNotIPv4) {
		t.Fatalf("a v6 destination was accepted: %v", err)
	}
}

func TestBuildRejectsAnOversizeDatagram(t *testing.T) {
	// 65535 is the whole IPv4 total-length field. Silently truncating the
	// length would produce a frame whose header disagrees with its contents.
	if _, err := BuildIPv4UDP(anyAddr, broadcastAddr, 68, 67, 1, 1, make([]byte, 65516)); err == nil {
		t.Fatal("a datagram larger than the IPv4 maximum was accepted")
	}
	// And the boundary below it is accepted: a refusal that is one octet too
	// eager would pass the check above.
	if _, err := BuildIPv4UDP(anyAddr, broadcastAddr, 68, 67, 1, 1, make([]byte, 65507)); err != nil {
		t.Fatalf("the largest legal datagram was refused: %v", err)
	}
}

func TestSum16HandlesTheOddTrailingOctet(t *testing.T) {
	// RFC 1071 section 4.1: the odd trailing octet is the HIGH byte of the
	// final word. Treating it as the low byte is the classic error, it is
	// invisible for a payload whose last octet is zero, and it produces a
	// checksum the far side rejects for everything else.
	if got, want := sum16([]byte{0xAB}, 0), uint16(0xAB00); got != want {
		t.Fatalf("sum16([0xAB]) = %#04x, want %#04x", got, want)
	}
	if got, want := sum16([]byte{0x00, 0xAB}, 0), uint16(0x00AB); got != want {
		t.Fatalf("sum16([0x00,0xAB]) = %#04x, want %#04x", got, want)
	}
}

func TestUDPChecksumNeverTransmitsZero(t *testing.T) {
	// RFC 768: zero is reserved to mean "no checksum", so a computed zero is
	// transmitted as all ones. A datagram whose real checksum is zero would
	// otherwise be sent claiming it has none — legal, but it silently turns
	// off the receiver's check.
	//
	// The datagram is CONSTRUCTED rather than searched for. A search needs a
	// give-up branch, and a give-up branch in a test is an assertion that
	// stops asserting on exactly the machine where it stops working.
	var src, dst [4]byte
	u := make([]byte, 12)
	binary.BigEndian.PutUint16(u[0:2], ServerPort)
	binary.BigEndian.PutUint16(u[2:4], ClientPort)
	binary.BigEndian.PutUint16(u[4:6], 12)
	// Solve the final word: in one's-complement arithmetic the sum with the
	// last word set to x folds to 0xFFFF exactly when x = 0xFFFF - S, where S
	// is the sum with that word zero. A total of 0xFFFF complements to zero.
	s0 := pseudoSum(src, dst, u)
	binary.BigEndian.PutUint16(u[10:12], 0xFFFF-s0)

	if got := ^pseudoSum(src, dst, u); got != 0 {
		t.Fatalf("the constructed datagram checksums to %#04x, not 0; the construction is wrong and this test measures nothing", got)
	}
	if got := udpChecksum(src, dst, u); got != 0xFFFF {
		t.Fatalf("a computed-zero checksum was transmitted as %#04x, want 0xFFFF", got)
	}

	// The preservation control: an ordinary datagram is not rewritten to
	// 0xFFFF. A function that always returned 0xFFFF would pass the check
	// above.
	binary.BigEndian.PutUint16(u[10:12], 0x1234)
	if got := udpChecksum(src, dst, u); got == 0xFFFF {
		t.Fatal("an ordinary datagram was also given the all-ones checksum")
	}
}

// ---------------------------------------------------------------------------
// The uncompleted checksum of a locally generated reply.
//
// realOfferFrame is a DHCPOFFER dnsmasq 2.91 actually sent, captured off the
// client end of a veth pair on 2026-08-29 with AF_PACKET. It is here, byte for
// byte, because the defect it pins is invisible to any frame this package
// builds: the sending kernel had not computed the UDP checksum yet, so the
// field holds the folded pseudo-header sum and every completed-checksum test
// in this file passes while a real server cannot be leased from.
//
// The two constants beside it were computed in Python from these bytes, not by
// the code under test.
const realOfferFrame = "45c0014fab3d00004011a9f7c0a86301ffffffff00430044013b24f602010600908e74380000800000000000c0a86380c0a86301000000009e43dea897ac00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000638253633501023604c0a863013304000000783a040000003c3b04000000690104ffffff001c04c0a863ff1a0205780f09646863702e746573740604c0a863010304c0a86301ff"

const (
	// realOfferField is what is in the frame's checksum field.
	realOfferField = 0x24f6
	// realOfferCompleted is what a completed checksum for that datagram would
	// be. It differs from the field, which is what makes the fixture exercise
	// the partial-checksum path rather than the ordinary one.
	realOfferCompleted = 0xe58c
)

func realOffer(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(realOfferFrame)
	if err != nil {
		t.Fatalf("the fixture is not hex: %v", err)
	}
	return b
}

func TestParseAcceptsARealServersUncompletedChecksum(t *testing.T) {
	frame := realOffer(t)

	// First, the control on the fixture itself: the field is NOT the completed
	// checksum. Without this, a fixture that quietly acquired a correct
	// checksum would pass through the ordinary path and this test would prove
	// nothing about the one it is named for.
	u := frame[20:]
	if got := binary.BigEndian.Uint16(u[6:8]); got != realOfferField {
		t.Fatalf("fixture checksum field = %#04x, want %#04x", got, realOfferField)
	}
	var src, dst [4]byte
	copy(src[:], frame[12:16])
	copy(dst[:], frame[16:20])
	zeroed := append([]byte(nil), u...)
	binary.BigEndian.PutUint16(zeroed[6:8], 0)
	if got := udpChecksum(src, dst, zeroed); got != realOfferCompleted {
		t.Fatalf("completed checksum = %#04x, want %#04x", got, realOfferCompleted)
	}
	if realOfferField == realOfferCompleted {
		t.Fatal("the fixture cannot distinguish the two paths")
	}
	if got := pseudoHeaderSum(src, dst, len(u)); got != realOfferField {
		t.Fatalf("pseudo-header sum = %#04x, want the field %#04x", got, realOfferField)
	}

	dg, err := ParseIPv4UDP(frame)
	if err != nil {
		t.Fatalf("a real DHCPOFFER was rejected: %v", err)
	}
	if dg.Checksum != ChecksumUncompleted {
		t.Fatalf("the reply parsed as %s, want uncompleted", dg.Checksum)
	}
	if dg.Checksum.Verified() {
		t.Fatal("an uncompleted checksum was reported as verifying the payload")
	}
	if got := dg.Src.String(); got != "192.168.99.1" {
		t.Fatalf("source = %s, want the server", got)
	}
	if len(dg.Payload) != 307 {
		t.Fatalf("payload = %d octets, want 307", len(dg.Payload))
	}
	if dg.Payload[0] != 2 {
		t.Fatalf("op = %d, want 2 (BOOTREPLY)", dg.Payload[0])
	}
}

// TestParseRefusesAUDPLengthPastTheIPTotalLength is the case the total-length
// slice actually guards, and it is not the padding case next to it.
//
// MEASURED 2026-08-29 by mutation: replacing frame[ihl:total] with frame[ihl:]
// SURVIVED the padded-frame test even with non-zero padding, because the later
// u = u[:ulen] truncates to the UDP length anyway. What the IP total length
// bounds is a datagram whose UDP length field claims MORE than the IP header
// allows: without it, the extra octets are link padding, and handing padding
// to the caller as DHCP payload is CVE-2003-0001's whole class of bug.
func TestParseRefusesAUDPLengthPastTheIPTotalLength(t *testing.T) {
	frame, err := BuildIPv4UDP(anyAddr, broadcastAddr, ServerPort, ClientPort, 1, 1, []byte("five!"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	// Pad the frame the way a link does, then claim the padding is payload by
	// growing the UDP length. The IPv4 total length is left alone, which is
	// exactly the disagreement being tested.
	pad := make([]byte, 20)
	for i := range pad {
		pad[i] = byte(0xB0 + i)
	}
	padded := append(append([]byte(nil), frame...), pad...)
	udpLen := binary.BigEndian.Uint16(padded[24:26])
	binary.BigEndian.PutUint16(padded[24:26], udpLen+8)

	if _, err := ParseIPv4UDP(padded); !errors.Is(err, ErrPayloadShort) {
		t.Fatalf("err = %v, want %v — link padding was accepted as payload", err, ErrPayloadShort)
	}

	// Preservation control: the same frame with the UDP length left honest
	// still parses, so the refusal above is about the disagreement and not
	// about the padding.
	padded = append(append([]byte(nil), frame...), pad...)
	dg, err := ParseIPv4UDP(padded)
	if err != nil {
		t.Fatalf("the honest padded frame was rejected: %v", err)
	}
	if string(dg.Payload) != "five!" {
		t.Fatalf("payload = %q, want %q", dg.Payload, "five!")
	}
}

// TestThePseudoHeaderSumIsBlindToThePayload is the property the whole
// acceptance rests on, driven rather than asserted in prose: the value that
// gets an uncompleted datagram through depends on the source, the destination
// and the length, and on nothing else. Flip a payload octet and the completed
// checksum moves while the accepting value does not.
//
// It is also the check that would have caught a wrong number in a comment:
// both constants are recomputed here from the fixture's own bytes.
func TestThePseudoHeaderSumIsBlindToThePayload(t *testing.T) {
	frame := realOffer(t)
	var src, dst [4]byte
	copy(src[:], frame[12:16])
	copy(dst[:], frame[16:20])

	completed := func(f []byte) uint16 {
		u := append([]byte(nil), f[20:]...)
		binary.BigEndian.PutUint16(u[6:8], 0)
		return udpChecksum(src, dst, u)
	}

	if got := completed(frame); got != realOfferCompleted {
		t.Fatalf("completed checksum = %#04x, want %#04x", got, realOfferCompleted)
	}

	flipped := realOffer(t)
	flipped[120] ^= 0xFF
	moved := completed(flipped)
	if moved == realOfferCompleted {
		t.Fatal("a flipped payload octet left the completed checksum alone; the fixture cannot show the difference")
	}

	// The accepting value, over both frames, is the same and is the field.
	for _, f := range [][]byte{frame, flipped} {
		if got := pseudoHeaderSum(src, dst, len(f[20:])); got != realOfferField {
			t.Fatalf("pseudo-header sum = %#04x, want %#04x", got, realOfferField)
		}
	}
	t.Logf("completed checksum moved %#04x -> %#04x while the accepting value stayed %#04x",
		realOfferCompleted, moved, realOfferField)
}

// TestParseStillRefusesAWrongChecksum is the preservation control for the
// case above. Accepting an uncompleted checksum must not become accepting any
// checksum: a field that is neither zero, nor correct, nor the pseudo-header
// sum is still a refusal.
func TestParseStillRefusesAWrongChecksum(t *testing.T) {
	frame := realOffer(t)
	// 0x1234 is none of the three accepted values.
	binary.BigEndian.PutUint16(frame[26:28], 0x1234)
	if _, err := ParseIPv4UDP(frame); !errors.Is(err, ErrBadChecksum) {
		t.Fatalf("err = %v, want %v", err, ErrBadChecksum)
	}

	// And the other direction: the same frame carrying the COMPLETED checksum
	// parses, and reports that the payload was verified. Without this the test
	// above could pass on a parser that had simply stopped checking.
	frame = realOffer(t)
	binary.BigEndian.PutUint16(frame[26:28], realOfferCompleted)
	dg, err := ParseIPv4UDP(frame)
	if err != nil {
		t.Fatalf("a completed checksum was rejected: %v", err)
	}
	if dg.Checksum != ChecksumVerified {
		t.Fatalf("a completed checksum parsed as %s, want verified", dg.Checksum)
	}
}

// TestAnUncheckedChecksumAcceptsACorruptPayload is the BOUND, pinned as a case
// rather than left in a comment — and it covers BOTH unchecked states, because
// stating only the uncompleted half was itself a half-true bound.
//
// These tests assert the wrong answer on purpose. They go red if anyone ever
// narrows the acceptance, which is the moment to come back and read this.
func TestAnUncheckedChecksumAcceptsACorruptPayload(t *testing.T) {
	// The uncompleted case: the field holds the pseudo-header sum, which is a
	// pure function of source, destination and UDP length — every one of them
	// in the frame — so it is CONSTRUCTIBLE, not a collision somebody has to
	// get lucky with.
	t.Run("uncompleted", func(t *testing.T) {
		frame := realOffer(t)
		frame[100] ^= 0xFF
		dg, err := ParseIPv4UDP(frame)
		if err != nil {
			t.Fatalf("the bound has moved: %v", err)
		}
		if dg.Checksum != ChecksumUncompleted {
			t.Fatalf("parsed as %s, want uncompleted", dg.Checksum)
		}
	})

	// And the cheaper half: RFC 768's zero needs no computation at all. A
	// reader who takes the uncompleted case as THE bound would think the
	// exposure is arithmetic; it is a constant.
	t.Run("absent", func(t *testing.T) {
		frame := realOffer(t)
		frame[100] ^= 0xFF
		binary.BigEndian.PutUint16(frame[26:28], 0)
		dg, err := ParseIPv4UDP(frame)
		if err != nil {
			t.Fatalf("the bound has moved: %v", err)
		}
		if dg.Checksum != ChecksumAbsent {
			t.Fatalf("parsed as %s, want absent", dg.Checksum)
		}
	})
}

// TestAFrameThatIsBothVerifiedAndPseudoHeaderSum pins the ORDER of the arms in
// acceptUDPChecksum, which round 1 of review flagged as untested and round 2
// re-derived as still untested: swapping the verify arm and the pseudo-header
// arm left the whole suite green, and the witness that justified the order
// existed only in a review note outside the repository.
//
// The two arms are not disjoint. A frame whose checksum field happens to equal
// the pseudo-header sum AND is a correct checksum satisfies both, and it must
// be reported as Verified: the payload really was checked, and reporting it as
// Uncompleted would count a verified datagram as unverified.
//
// It is constructed rather than asserted to be rare: choosing the field and
// then solving for one 16-bit payload word gives a witness in one pass.
//
// The zero arm overlaps too. TestUDPChecksumNeverTransmitsZero, two hundred
// lines up, constructs a datagram whose real checksum is zero. A frame
// carrying that
// datagram with a zero field satisfies the zero arm AND the verify arm, and
// must be reported Absent: RFC 768 reserves zero for "no checksum computed",
// so the receiver must not treat it as checked. The zero subtest below drives
// that, so all three arms now have their order pinned.
func TestAFrameThatIsBothVerifiedAndPseudoHeaderSum(t *testing.T) {
	frame := realOffer(t)
	var src, dst [4]byte
	copy(src[:], frame[12:16])
	copy(dst[:], frame[16:20])

	field := pseudoHeaderSum(src, dst, len(frame[20:]))
	if field == 0 {
		t.Fatal("the pseudo-header sum is zero for this fixture; the two arms cannot overlap")
	}
	binary.BigEndian.PutUint16(frame[26:28], field)

	// Solve for the payload word that makes the same field a CORRECT checksum.
	found := false
	for v := 0; v <= 0xFFFF; v++ {
		binary.BigEndian.PutUint16(frame[100:102], uint16(v))
		if udpChecksumVerify(src, dst, frame[20:]) == 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no payload word makes this field a correct checksum; the witness cannot be built")
	}

	// The premise, asserted rather than assumed: the frame satisfies BOTH arms.
	if got := udpChecksumVerify(src, dst, frame[20:]); got != 0 {
		t.Fatalf("verify = %#04x, want 0", got)
	}
	if got := binary.BigEndian.Uint16(frame[26:28]); got != pseudoHeaderSum(src, dst, len(frame[20:])) {
		t.Fatalf("field %#04x is not the pseudo-header sum %#04x", got, pseudoHeaderSum(src, dst, len(frame[20:])))
	}

	state, ok := acceptUDPChecksum(src, dst, frame[20:])
	if !ok {
		t.Fatal("a correct checksum was refused")
	}
	if state != ChecksumVerified {
		t.Fatalf("state = %s, want verified: the verify arm must be reached before the pseudo-header arm", state)
	}

	dg, err := ParseIPv4UDP(frame)
	if err != nil {
		t.Fatalf("ParseIPv4UDP refused the witness: %v", err)
	}
	if dg.Checksum != ChecksumVerified {
		t.Fatalf("ParseIPv4UDP reported %s, want verified", dg.Checksum)
	}

	t.Run("zero", func(t *testing.T) {
		// A datagram whose REAL checksum is zero, carried with a zero field:
		// the zero arm and the verify arm both match. Zero must win — RFC 768
		// reserves it for "no checksum computed", so a receiver that reports
		// this as verified reports a check it did not perform.
		var zsrc, zdst [4]byte
		u := make([]byte, 12)
		binary.BigEndian.PutUint16(u[0:2], ServerPort)
		binary.BigEndian.PutUint16(u[2:4], ClientPort)
		binary.BigEndian.PutUint16(u[4:6], 12)
		s0 := pseudoSum(zsrc, zdst, u)
		binary.BigEndian.PutUint16(u[10:12], 0xFFFF-s0)

		if got := binary.BigEndian.Uint16(u[6:8]); got != 0 {
			t.Fatalf("the checksum field is %#04x, not zero", got)
		}
		if got := udpChecksumVerify(zsrc, zdst, u); got != 0 {
			t.Fatalf("verify = %#04x: the constructed datagram does not also verify, so the arms do not overlap here", got)
		}
		state, ok := acceptUDPChecksum(zsrc, zdst, u)
		if !ok {
			t.Fatal("a zero checksum was refused")
		}
		if state != ChecksumAbsent {
			t.Fatalf("state = %s, want absent: the zero arm must be reached before the verify arm", state)
		}
	})
}
