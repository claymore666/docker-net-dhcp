package wire

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"
)

// arpFrame builds an encoded Ethernet/IPv4 ARP packet from its five fields, so
// that a test case can name what it is varying and nothing else.
//
// It builds the bytes DIRECTLY rather than through EncodeARP, and that is the
// point: a decoder test whose fixtures come from the encoder under test cannot
// see an error the two share. The offsets here are RFC 826's, written out once.
func arpFrame(op uint16, sha []byte, spa string, tha []byte, tpa string) []byte {
	b := make([]byte, ARPPacketLen)
	b[0], b[1] = 0, 1 // htype: Ethernet
	b[2], b[3] = 0x08, 0x00
	b[4] = 6
	b[5] = 4
	b[6], b[7] = byte(op>>8), byte(op)
	copy(b[8:14], sha)
	copy(b[14:18], netip.MustParseAddr(spa).AsSlice())
	copy(b[18:24], tha)
	copy(b[24:28], netip.MustParseAddr(tpa).AsSlice())
	return b
}

var (
	ourMAC   = []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x02}
	otherMAC = []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x63}
	zeroMAC  = []byte{0, 0, 0, 0, 0, 0}
)

// TestDecodeARPReadsRFC826sFields drives one well-formed frame of each shape
// RFC 5227 names and reads every field back.
//
// The four addresses are all DIFFERENT in each row on purpose: a decoder that
// read 'ar$spa' where 'ar$tpa' belongs — which is the mistake the offsets
// invite — passes any fixture where the two are equal, and an ARP Announcement
// is exactly such a fixture (RFC 5227 section 2.3 sets both to the same
// address). So the Announcement row is not the only row.
func TestDecodeARPReadsRFC826sFields(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		op      ARPOp
		sha     []byte
		spa     string
		tpa     string
		isProbe bool
	}{
		{
			// RFC 5227 section 1.1: "the term 'ARP Probe' is used to refer to
			// an ARP Request packet, broadcast on the local link, with an
			// all-zero 'sender IP address'."
			name:    "an RFC 5227 2.1.1 ARP Probe",
			raw:     arpFrame(1, ourMAC, "0.0.0.0", zeroMAC, "192.168.99.50"),
			op:      ARPRequest,
			sha:     ourMAC,
			spa:     "0.0.0.0",
			tpa:     "192.168.99.50",
			isProbe: true,
		},
		{
			// Section 2.3: "An ARP Announcement is identical to the ARP Probe
			// described above, except that now the sender and target IP
			// addresses are both set to the host's newly selected IPv4
			// address."
			name: "an RFC 5227 2.3 ARP Announcement",
			raw:  arpFrame(1, ourMAC, "192.168.99.50", zeroMAC, "192.168.99.50"),
			op:   ARPRequest,
			sha:  ourMAC,
			spa:  "192.168.99.50",
			tpa:  "192.168.99.50",
		},
		{
			name: "an ordinary ARP Request, every address different",
			raw:  arpFrame(1, otherMAC, "192.168.99.7", zeroMAC, "192.168.99.9"),
			op:   ARPRequest,
			sha:  otherMAC,
			spa:  "192.168.99.7",
			tpa:  "192.168.99.9",
		},
		{
			name: "an ARP Reply, every address different",
			raw:  arpFrame(2, otherMAC, "192.168.99.50", ourMAC, "192.168.99.8"),
			op:   ARPReply,
			sha:  otherMAC,
			spa:  "192.168.99.50",
			tpa:  "192.168.99.8",
		},
		{
			// A Reply with a zero sender is NOT a Probe. Section 1.1 defines
			// the Probe as a Request; reading this as one would treat a
			// malformed reply as somebody else's probe and trip section
			// 2.1.1's second rule on it.
			name: "an ARP Reply with an all-zero sender IP is not a Probe",
			raw:  arpFrame(2, otherMAC, "0.0.0.0", ourMAC, "192.168.99.50"),
			op:   ARPReply,
			sha:  otherMAC,
			spa:  "0.0.0.0",
			tpa:  "192.168.99.50",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := DecodeARP(c.raw)
			if err != nil {
				t.Fatalf("DecodeARP: %v", err)
			}
			if p.Op != c.op {
				t.Errorf("Op = %s, want %s", p.Op, c.op)
			}
			if !bytes.Equal(p.SenderHW, c.sha) {
				t.Errorf("SenderHW = %x, want %x", p.SenderHW, c.sha)
			}
			if got := p.SenderIP.String(); got != c.spa {
				t.Errorf("SenderIP = %s, want %s", got, c.spa)
			}
			if got := p.TargetIP.String(); got != c.tpa {
				t.Errorf("TargetIP = %s, want %s", got, c.tpa)
			}
			if p.IsProbe() != c.isProbe {
				t.Errorf("IsProbe() = %v, want %v", p.IsProbe(), c.isProbe)
			}
		})
	}
}

// TestDecodeARPRefusesWhatItCannotRead is D17's adversarial corpus for the
// codec: every frame here would, if accepted, produce addresses that are not
// addresses and hand RFC 5227's rules a verdict about a host that does not
// exist.
//
// THE SHORT FRAMES ARE THE POINT. A decoder that checked the header and then
// trusted 'ar$hln' would index past the slice on the first of them, and ring 2
// feeds this every frame on the link — so the panic would be reachable from
// anything that can put bytes on a wire.
func TestDecodeARPRefusesWhatItCannotRead(t *testing.T) {
	good := arpFrame(1, ourMAC, "0.0.0.0", zeroMAC, "192.168.99.50")

	shortHead := make([]byte, 8)
	copy(shortHead, good[:8])

	badHType := append([]byte(nil), good...)
	badHType[1] = 6 // IEEE 802 networks

	badPType := append([]byte(nil), good...)
	badPType[2], badPType[3] = 0x86, 0xDD // IPv6

	badHLen := append([]byte(nil), good...)
	badHLen[4] = 8 // an 8-octet hardware address in a 6-octet layout

	badPLen := append([]byte(nil), good...)
	badPLen[5] = 16

	cases := []struct {
		name string
		raw  []byte
		want error
	}{
		{"nothing at all", nil, ErrARPShort},
		{"one octet", []byte{1}, ErrARPShort},
		{"the fixed head and no addresses", shortHead, ErrARPShort},
		{"one octet short of a whole packet", good[:ARPPacketLen-1], ErrARPShort},
		{"hardware type is not Ethernet", badHType, ErrARPFamily},
		{"protocol type is not IPv4", badPType, ErrARPFamily},
		{"hlen does not match Ethernet", badHLen, ErrARPAddrLen},
		{"plen does not match IPv4", badPLen, ErrARPAddrLen},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := DecodeARP(c.raw)
			if err == nil {
				t.Fatalf("DecodeARP accepted it and returned %s", p)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("DecodeARP error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestDecodeARPAcceptsEthernetPadding is the preservation control for the
// length refusal above.
//
// The minimum Ethernet payload is 46 octets and an ARP packet is 28, so a
// conformant sender pads with 18 zeroes. A length check written as an equality
// would refuse most of the ARP on a real link — and would do it while every
// row of the refusal table above still passed, which is why this is a separate
// named test and not a comment on one.
func TestDecodeARPAcceptsEthernetPadding(t *testing.T) {
	padded := append(arpFrame(1, otherMAC, "192.168.99.50", zeroMAC, "192.168.99.50"),
		make([]byte, 46-ARPPacketLen)...)
	if len(padded) != 46 {
		t.Fatalf("fixture is %d octets, want the 46-octet Ethernet minimum", len(padded))
	}
	p, err := DecodeARP(padded)
	if err != nil {
		t.Fatalf("DecodeARP refused a padded frame: %v", err)
	}
	if got := p.SenderIP.String(); got != "192.168.99.50" {
		t.Fatalf("SenderIP = %s, want 192.168.99.50; the padding moved the fields", got)
	}
}

// TestEncodeARPPutsEveryFieldWhereRFC826SaysItGoes reads the ENCODED BYTES at
// RFC 826's offsets rather than decoding them back.
//
// A round trip through this package's own decoder would pass with every offset
// consistently wrong. These offsets are written out a second time, here, and
// that duplication is the check.
func TestEncodeARPPutsEveryFieldWhereRFC826SaysItGoes(t *testing.T) {
	p := &ARPPacket{
		Op:       ARPRequest,
		SenderHW: ourMAC,
		SenderIP: netip.MustParseAddr("0.0.0.0"),
		TargetIP: netip.MustParseAddr("192.168.99.50"),
	}
	b, err := EncodeARP(p)
	if err != nil {
		t.Fatalf("EncodeARP: %v", err)
	}
	if len(b) != 28 {
		t.Fatalf("encoded length = %d, want RFC 826's 28 for Ethernet/IPv4", len(b))
	}
	want := []struct {
		name string
		at   int
		b    []byte
	}{
		{"ar$hrd", 0, []byte{0, 1}},
		{"ar$pro", 2, []byte{0x08, 0x00}},
		{"ar$hln", 4, []byte{6}},
		{"ar$pln", 5, []byte{4}},
		{"ar$op", 6, []byte{0, 1}},
		{"ar$sha", 8, ourMAC},
		{"ar$spa", 14, []byte{0, 0, 0, 0}},
		{"ar$tha", 18, zeroMAC},
		{"ar$tpa", 24, []byte{192, 168, 99, 50}},
	}
	for _, w := range want {
		if got := b[w.at : w.at+len(w.b)]; !bytes.Equal(got, w.b) {
			t.Errorf("%s at offset %d = %x, want %x", w.name, w.at, got, w.b)
		}
	}
}

// TestEncodeARPRefusesAnUnsendablePacket drives the four refusals.
//
// RFC 5227 section 2.1.1 makes the sender hardware address of a Probe a MUST,
// and a Probe that went out with a truncated or padded one would be answered
// to nobody — which is indistinguishable, from the client, from an address
// that is free. So these are refusals and not best-effort encodings.
func TestEncodeARPRefusesAnUnsendablePacket(t *testing.T) {
	v4 := netip.MustParseAddr("192.168.99.50")
	v6 := netip.MustParseAddr("2001:db8::1")

	cases := []struct {
		name string
		p    *ARPPacket
	}{
		{"nil", nil},
		{"no sender hardware address", &ARPPacket{Op: ARPRequest, SenderIP: v4, TargetIP: v4}},
		{"a short sender hardware address", &ARPPacket{Op: ARPRequest, SenderHW: ourMAC[:4], SenderIP: v4, TargetIP: v4}},
		{"a long sender hardware address", &ARPPacket{Op: ARPRequest, SenderHW: append(append([]byte(nil), ourMAC...), 9, 9), SenderIP: v4, TargetIP: v4}},
		{"a wrong-length target hardware address", &ARPPacket{Op: ARPRequest, SenderHW: ourMAC, TargetHW: ourMAC[:3], SenderIP: v4, TargetIP: v4}},
		{"an IPv6 sender", &ARPPacket{Op: ARPRequest, SenderHW: ourMAC, SenderIP: v6, TargetIP: v4}},
		{"an IPv6 target", &ARPPacket{Op: ARPRequest, SenderHW: ourMAC, SenderIP: v4, TargetIP: v6}},
		{"an unset sender", &ARPPacket{Op: ARPRequest, SenderHW: ourMAC, TargetIP: v4}},
		{"an unset target", &ARPPacket{Op: ARPRequest, SenderHW: ourMAC, SenderIP: v4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := EncodeARP(c.p)
			if err == nil {
				t.Fatalf("EncodeARP accepted it and produced %x", b)
			}
			if !errors.Is(err, ErrARPEncode) {
				t.Fatalf("error = %v, want ErrARPEncode", err)
			}
		})
	}
}

// TestEncodeARPAcceptsAnEmptyTargetHardwareAddress is the preservation control
// for the target-length refusal.
//
// RFC 5227 section 2.1.1: the Probe's "'target hardware address' field is
// ignored and SHOULD be set to all zeroes". A caller leaves it nil and gets
// six zero octets; a refusal here would make the ordinary Probe unbuildable
// while every row of the refusal table still passed.
func TestEncodeARPAcceptsAnEmptyTargetHardwareAddress(t *testing.T) {
	b, err := EncodeARP(&ARPPacket{
		Op:       ARPRequest,
		SenderHW: ourMAC,
		SenderIP: netip.MustParseAddr("0.0.0.0"),
		TargetIP: netip.MustParseAddr("192.168.99.50"),
	})
	if err != nil {
		t.Fatalf("EncodeARP refused a Probe with no target hardware address: %v", err)
	}
	if !bytes.Equal(b[18:24], zeroMAC) {
		t.Fatalf("ar$tha = %x, want all zeroes", b[18:24])
	}
}
