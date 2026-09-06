package runtime

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// The captured IPv6 frames these tests are measured against, and where they
// came from.
//
// PROVENANCE. Hand-run 2026-09-06 on the session box, entirely inside
// `unshare -Urn` — a private network namespace with one veth pair, `v6srv` and
// `v6cli`, and no route to anything. No LAN traffic was involved and no LAN
// address appears here; the fixture prefix is fd00:99::/64, which is RFC 4193
// unique-local.
//
// The server was dnsmasq 2.91 with `--enable-ra --ra-param=v6srv,60,300
// --dhcp-range=fd00:99::100,fd00:99::1ff,64,300`, bound to v6srv. The client
// was this package's own Client6 on v6cli, and it acquired fd00:99::1a3. The
// capture is `tcpdump -Z root -i v6cli -s 0 -w cli.pcap 'icmp6 or udp port 546
// or udp port 547'`.
//
// EACH LITERAL IS THE IPv6 PACKET, from the version nibble to the last octet
// of the payload — the Ethernet header stripped, which is exactly what an
// AF_PACKET SOCK_DGRAM read hands back and therefore exactly what the
// functions under test are given at run time.
//
// WHAT THIS CANNOT SEE, said rather than left to be found. Nothing re-runs the
// capture; it is a table of literals. The netns proofs in this package repeat
// the exchange live, and the two are meant to disagree loudly if dnsmasq
// changes.
var (
	// The Advertise dnsmasq sent, and the single most useful frame here. Its
	// UDP checksum field holds Linux's CHECKSUM_PARTIAL — the folded
	// pseudo-header sum with the payload not yet summed in — which is what
	// makes it an INDEPENDENT computation of RFC 8200 section 8.1's
	// pseudo-header to check ours against. See
	// TestTheKernelsPartialChecksumIsOurPseudoHeaderSum.
	capV6Advertise = mustHex6("6c07a060006a1140fe8000000000000030e6f9fffe2faa1efe80000000000000" +
		"38025efffee7dfe002230222006a467c0241eb2e0001000a000300013a025ee7" +
		"dfe00002000e00010001322f76195afdcd053b47000300280a0b0c0d00000096" +
		"0000010300050018fd0000990000000000000000000001a30000012c0000012c" +
		"000d000900007375636365737300070001ff")

	// This client's own Solicit, recorded off the wire. Its checksum is
	// COMPLETE — BuildIPv6UDP computed it — and it verifies against the two
	// addresses in its own header, which is the row that says the builder and
	// the parser agree about the pseudo-header on a frame that actually
	// crossed a link.
	capV6Solicit = mustHex6("6000000000361101" +
		"fe8000000000000038025efffee7dfe0" +
		"ff020000000000000000000000010002" +
		"0222022300360c18" +
		"0141eb2e0001000a000300013a025ee7dfe00003000c0a0b0c0d000000000000" +
		"0000000600020052000800020000")

	// The Router Advertisement dnsmasq sent to ff02::1. Hop limit 255, which
	// is RFC 4861 section 6.1.2's check, and a complete ICMPv6 checksum the
	// kernel computed.
	capV6RouterAdvert = mustHex6("6c09880500583aff" +
		"fe8000000000000030e6f9fffe2faa1e" +
		"ff020000000000000000000000000001" +
		"8600297440c0012c0000000000000000" +
		"030440800000012c0000012c00000000fd000099000000000000000000000000" +
		"05010000000005dc" +
		"010132e6f92faa1e" +
		"190300000000012cfd000099000000000000000000000001")

	// The peer's Neighbor Solicitation for this client's link-local address —
	// an ADDRESS RESOLUTION, not a duplicate-address probe: its IPv6 source is
	// the peer's link-local and it carries a Source Link-Layer Address option.
	// RFC 4862 section 5.4.3 says such a solicitation "should be silently
	// ignored", and this is the frame that drives that arm.
	capV6NeighborSolicit = mustHex6("6000000000203aff" +
		"fe8000000000000030e6f9fffe2faa1e" +
		"ff0200000000000000000001ffe7dfe0" +
		"87007ca100000000" +
		"fe8000000000000038025efffee7dfe0" +
		"010132e6f92faa1e")

	// This client's kernel answering it. Solicited and Override set.
	capV6NeighborAdvert = mustHex6("6000000000203aff" +
		"fe8000000000000038025efffee7dfe0" +
		"fe8000000000000030e6f9fffe2faa1e" +
		"8800e28c60000000" +
		"fe8000000000000038025efffee7dfe0" +
		"02013a025ee7dfe0")

	// The ICMPv6 Destination Unreachable this client's own kernel sent back
	// for the Advertise above, because nothing was bound to UDP port 546.
	// It is here as the frame an ND socket must SKIP: a valid ICMPv6 message
	// of a type Neighbor Discovery does not use.
	capV6Unreachable = mustHex6("600d9dcd009a3a40" +
		"fe8000000000000038025efffee7dfe0" +
		"fe8000000000000030e6f9fffe2faa1e" +
		"0104c50900000000" +
		"6c07a060006a1140fe8000000000000030e6f9fffe2faa1efe80000000000000" +
		"38025efffee7dfe002230222006a467c0241eb2e0001000a000300013a025ee7" +
		"dfe00002000e00010001322f76195afdcd053b47000300280a0b0c0d00000096" +
		"0000010300050018fd0000990000000000000000000001a30000012c0000012c" +
		"000d000900007375636365737300070001ff")
)

func mustHex6(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func addr6(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

// --------------------------------------------------------------- checksum --

// TestTheKernelsPartialChecksumIsOurPseudoHeaderSum is the row that settles
// whether RFC 8200 section 8.1's pseudo-header is computed the way section 8.1
// describes it, and it is the only kind of row that can.
//
// A pseudo-header this package computed wrongly — the v4 shape, the addresses
// left out, the length taken from the wrong field, the Next Header value
// omitted — agrees with itself on every frame this package builds and
// disagrees with every frame from anywhere else. So the witness has to come
// from anywhere else, and here it is Linux: for a datagram whose checksum it
// has deferred, the kernel writes the FOLDED PSEUDO-HEADER SUM into the
// checksum field and nothing more. That value is an independent computation of
// exactly the quantity under test, made by a different implementation, over a
// frame neither could see the other compute.
//
// The four mutation rows below it are what turn "the numbers matched" into
// "each input is in the sum": every one of the pseudo-header's four
// constituents is moved by one and the answer has to move with it. A sum that
// ignored the destination address would still match the first row.
func TestTheKernelsPartialChecksumIsOurPseudoHeaderSum(t *testing.T) {
	src := addr6(t, "fe80::30e6:f9ff:fe2f:aa1e").As16()
	dst := addr6(t, "fe80::3802:5eff:fee7:dfe0").As16()
	const ulen = 106

	field := binary.BigEndian.Uint16(capV6Advertise[40+6 : 40+8])
	got := pseudoHeaderSum6(src, dst, ulen)
	if got != field {
		t.Fatalf("pseudoHeaderSum6 = %#04x, but the kernel wrote %#04x into the frame it deferred; the two implementations disagree about RFC 8200 section 8.1's pseudo-header", got, field)
	}

	other := src
	other[15] ^= 0x01
	odst := dst
	odst[15] ^= 0x01
	for _, tc := range []struct {
		name             string
		src, dst         [16]byte
		ulen             int
		next             uint8
		wantSameAsKernel bool
	}{
		{name: "as captured", src: src, dst: dst, ulen: ulen, next: protoUDP, wantSameAsKernel: true},
		{name: "one bit of the source address", src: other, dst: dst, ulen: ulen, next: protoUDP},
		{name: "one bit of the destination address", src: src, dst: odst, ulen: ulen, next: protoUDP},
		{name: "one octet of the upper-layer length", src: src, dst: dst, ulen: ulen + 1, next: protoUDP},
		{name: "the Next Header value", src: src, dst: dst, ulen: ulen, next: wire.ICMPv6NextHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sum := sum16(nil, pseudoBase6(tc.src, tc.dst, tc.ulen, tc.next))
			if (sum == field) != tc.wantSameAsKernel {
				t.Errorf("pseudo-header sum %#04x, kernel %#04x, want equal=%v — this input is not in the sum", sum, field, tc.wantSameAsKernel)
			}
		})
	}
}

// TestARealAdvertiseIsAcceptedAsUncompleted is the fix for the defect this
// milestone found by running: a v6 parser that verified strictly discarded
// every reply dnsmasq sent over a veth pair and reported nothing but silence.
//
// RFC 8200 section 8.1's MUST is about a checksum field of ZERO. Linux's
// CHECKSUM_PARTIAL is a different value with a different cause, and refusing
// it produces a client that cannot lease from a server on the same host —
// which is every test this library has.
func TestARealAdvertiseIsAcceptedAsUncompleted(t *testing.T) {
	dg, err := ParseIPv6UDP(capV6Advertise)
	if err != nil {
		t.Fatalf("ParseIPv6UDP(real Advertise): %v", err)
	}
	if dg.Checksum != ChecksumUncompleted {
		t.Errorf("Checksum = %s, want uncompleted", dg.Checksum)
	}
	if dg.Checksum.Verified() {
		t.Errorf("Verified() = true on a payload nothing checked")
	}
	if want := addr6(t, "fe80::30e6:f9ff:fe2f:aa1e"); dg.Src != want {
		t.Errorf("Src = %s, want %s", dg.Src, want)
	}
	// DHCPv6 message type 2 is Advertise, and the transaction id follows it.
	if len(dg.Payload) == 0 || dg.Payload[0] != 2 {
		t.Fatalf("payload does not start with an Advertise: % x", dg.Payload[:min(8, len(dg.Payload))])
	}
	if _, err := wire.DecodeV6(dg.Payload); err != nil {
		t.Errorf("the payload this parser handed on does not decode: %v", err)
	}
}

// TestARealSolicitVerifiesAgainstItsOwnAddresses closes the other half. The
// captured Solicit is this package's own output recorded off the wire, so this
// row says the builder and the parser agree about the pseudo-header on a frame
// that actually crossed a link — and it is the row that would go on passing if
// ParseIPv6UDP's verify arm were deleted, which is why the Uncompleted row
// above asserts the STATE and not merely the absence of an error.
func TestARealSolicitVerifiesAgainstItsOwnAddresses(t *testing.T) {
	// The Solicit goes client port to server port, which ParseIPv6UDP refuses
	// by design, so the checksum is checked directly.
	src := addr6(t, "fe80::3802:5eff:fee7:dfe0").As16()
	dst := AllDHCPRelayAgentsAndServers.As16()
	u := capV6Solicit[40:]
	if got := udpChecksumVerify6(src, dst, u); got != 0 {
		t.Errorf("udpChecksumVerify6 = %#04x, want 0 on a frame this package built and a link carried", got)
	}
	// And the same frame against the WRONG destination, so the row above is
	// not passing because the destination is ignored.
	if got := udpChecksumVerify6(src, wire.AllNodesMulticast.As16(), u); got == 0 {
		t.Errorf("udpChecksumVerify6 verified against a destination the frame was not sent to")
	}
}

// TestParseIPv6UDPDiscardsAZeroChecksum is the one place the v4 answer must
// NOT be copied, and it is copied by default: ipudp.go accepts a zero checksum
// and counts it.
//
// RFC 8200 section 8.1: "IPv6 receivers must discard UDP packets containing a
// zero checksum and should log the error."
func TestParseIPv6UDPDiscardsAZeroChecksum(t *testing.T) {
	frame := bytes.Clone(capV6Advertise)
	frame[40+6], frame[40+7] = 0, 0
	dg, err := ParseIPv6UDP(frame)
	if !errors.Is(err, ErrZeroChecksum6) {
		t.Fatalf("err = %v, want ErrZeroChecksum6", err)
	}
	if dg.Payload != nil {
		t.Errorf("a discarded datagram returned a payload")
	}
}

// TestParseIPv6UDPRefusesACorruptChecksum drives the third value: neither zero
// nor the pseudo-header sum nor correct.
//
// Without it the two accepting arms are the whole function, and a parser that
// accepted everything would pass every other row in this file.
func TestParseIPv6UDPRefusesACorruptChecksum(t *testing.T) {
	frame := bytes.Clone(capV6Advertise)
	binary.BigEndian.PutUint16(frame[40+6:40+8], 0x1234)
	if _, err := ParseIPv6UDP(frame); !errors.Is(err, ErrBadChecksum) {
		t.Fatalf("err = %v, want ErrBadChecksum", err)
	}
}

// TestAVerifiedFrameIsNotReportedUncompleted pins the ORDER of the two
// accepting arms.
//
// A datagram can satisfy both at once — it needs the payload to sum to zero,
// which the empty payload of a bare UDP header does — and it must be reported
// Verified. With the arms the other way round, every such datagram would be
// reported as unchecked and the difference between "the server checksummed
// this" and "nobody did" would collapse.
func TestAVerifiedFrameIsNotReportedUncompleted(t *testing.T) {
	src := addr6(t, "fe80::1")
	dst := addr6(t, "fe80::2")
	frame, err := BuildIPv6UDP(dst, src, ServerPort6, ClientPort6, 64, nil)
	if err != nil {
		t.Fatalf("BuildIPv6UDP: %v", err)
	}
	u := frame[40:]
	if got, want := binary.BigEndian.Uint16(u[6:8]), pseudoHeaderSum6(dst.As16(), src.As16(), len(u)); got != want {
		t.Skipf("this frame does not satisfy both arms (%#04x vs %#04x); the ordering row needs one that does", got, want)
	}
	dg, err := ParseIPv6UDP(frame)
	if err != nil {
		t.Fatalf("ParseIPv6UDP: %v", err)
	}
	if dg.Checksum != ChecksumVerified {
		t.Errorf("Checksum = %s on a frame that verifies, want verified", dg.Checksum)
	}
}

// TestTheV6ChecksumIsBoundedByThePayloadLengthAndNotTheFrame drives the bound
// ipv6Upper's comment claims.
//
// A SOCK_DGRAM read hands back whatever the link padded the frame to. Summing
// that padding breaks the checksum, and the symptom is a client that works on
// a link with large frames and refuses every reply on one that pads — which is
// the hardest kind of failure to attribute.
func TestTheV6ChecksumIsBoundedByThePayloadLengthAndNotTheFrame(t *testing.T) {
	padded := append(bytes.Clone(capV6Advertise), make([]byte, 26)...)
	dg, err := ParseIPv6UDP(padded)
	if err != nil {
		t.Fatalf("ParseIPv6UDP on a padded frame: %v", err)
	}
	clean, err := ParseIPv6UDP(capV6Advertise)
	if err != nil {
		t.Fatalf("ParseIPv6UDP: %v", err)
	}
	if !bytes.Equal(dg.Payload, clean.Payload) {
		t.Errorf("the padded frame yielded a payload of %d octets, the unpadded one %d", len(dg.Payload), len(clean.Payload))
	}
	if dg.Checksum != clean.Checksum {
		t.Errorf("padding changed the checksum verdict: %s against %s", dg.Checksum, clean.Checksum)
	}
}

// ------------------------------------------------------------------ ports --

// TestParseIPv6UDPChecksBothPorts drives the half ParseIPv4UDP does not have.
//
// The last row is the interesting one: it is this client's OWN Solicit, and it
// is refused. Nothing depends on that refusal — such a frame never reaches
// this socket, measured — but a parser that checked only the destination port
// would accept another client's Solicit as a reply to this one, decode a
// message of the wrong type, and count a decode failure with no explanation.
func TestParseIPv6UDPChecksBothPorts(t *testing.T) {
	set := func(sport, dport uint16) []byte {
		f := bytes.Clone(capV6Advertise)
		binary.BigEndian.PutUint16(f[40:42], sport)
		binary.BigEndian.PutUint16(f[42:44], dport)
		return f
	}
	for _, tc := range []struct {
		name         string
		sport, dport uint16
		wantErr      error
	}{
		{name: "server to client", sport: ServerPort6, dport: ClientPort6},
		{name: "server to some other port", sport: ServerPort6, dport: 1546, wantErr: ErrWrongPort},
		{name: "some other port to the client", sport: 1547, dport: ClientPort6, wantErr: ErrWrongPort},
		{name: "this client's own message, client to server", sport: ClientPort6, dport: ServerPort6, wantErr: ErrWrongPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseIPv6UDP(set(tc.sport, tc.dport))
			if tc.wantErr == nil {
				// Changing the ports invalidates the checksum, so the accepted
				// row is only asserted not to be refused for the PORT.
				if errors.Is(err, ErrWrongPort) {
					t.Fatalf("%d -> %d refused for its ports", tc.sport, tc.dport)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// --------------------------------------------------------- header walking --

// TestIPv6UpperRefusesWhatItCannotWalk drives every refusal of the header
// chain, including the two that are deliberate limits rather than errors.
func TestIPv6UpperRefusesWhatItCannotWalk(t *testing.T) {
	// A frame with one Hop-by-Hop header ahead of the UDP one, which the walk
	// must step over rather than refuse.
	withHopByHop := func() []byte {
		ip := bytes.Clone(capV6Advertise[:40])
		ip[6] = extHopByHop
		binary.BigEndian.PutUint16(ip[4:6], uint16(8+len(capV6Advertise)-40))
		ext := []byte{protoUDP, 0, 0, 0, 0, 0, 0, 0}
		return append(append(ip, ext...), capV6Advertise[40:]...)
	}
	for _, tc := range []struct {
		name  string
		frame []byte
		want  error
	}{
		{name: "shorter than an IPv6 header", frame: capV6Advertise[:39], want: ErrShortFrame},
		{
			name: "not IPv6 at all",
			frame: func() []byte {
				f := bytes.Clone(capV6Advertise)
				f[0] = 0x45
				return f
			}(),
			want: ErrNotIPv6Frame,
		},
		{
			name: "a payload length longer than the frame",
			frame: func() []byte {
				f := bytes.Clone(capV6Advertise)
				binary.BigEndian.PutUint16(f[4:6], 4096)
				return f
			}(),
			want: ErrShortFrame,
		},
		{
			// RFC 8200 section 4.5. Refused rather than reassembled: see
			// ipv6Upper.
			name: "a Fragment header",
			frame: func() []byte {
				f := bytes.Clone(capV6Advertise)
				f[6] = extFragment
				return f
			}(),
			want: ErrExtensionHeader,
		},
		{
			// RFC 8200 section 4.7's "No Next Header": there is nothing above.
			name: "no next header",
			frame: func() []byte {
				f := bytes.Clone(capV6Advertise)
				f[6] = extNoNextHdr
				return f
			}(),
			want: ErrExtensionHeader,
		},
		{
			name: "an extension header longer than what is left",
			frame: func() []byte {
				f := bytes.Clone(capV6Advertise)
				f[6] = extDestOpts
				f[41] = 0xFF
				return f
			}(),
			want: ErrExtensionHeader,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := ipv6Upper(tc.frame); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("a Hop-by-Hop header is stepped over", func(t *testing.T) {
		_, _, next, body, err := ipv6Upper(withHopByHop())
		if err != nil {
			t.Fatalf("ipv6Upper: %v", err)
		}
		if next != protoUDP {
			t.Errorf("next = %d, want %d", next, protoUDP)
		}
		if !bytes.Equal(body, capV6Advertise[40:]) {
			t.Errorf("the body after the extension header is %d octets, want %d", len(body), len(capV6Advertise)-40)
		}
	})

	t.Run("a chain of nine extension headers is refused rather than walked", func(t *testing.T) {
		ip := bytes.Clone(capV6Advertise[:40])
		ip[6] = extDestOpts
		var chain []byte
		for range 9 {
			chain = append(chain, extDestOpts, 0, 0, 0, 0, 0, 0, 0)
		}
		chain[len(chain)-8] = protoUDP
		binary.BigEndian.PutUint16(ip[4:6], uint16(len(chain)+len(capV6Advertise)-40))
		frame := append(append(ip, chain...), capV6Advertise[40:]...)
		if _, _, _, _, err := ipv6Upper(frame); !errors.Is(err, ErrExtensionHeader) {
			t.Fatalf("err = %v, want ErrExtensionHeader — an unbounded walk is how a parser becomes a loop", err)
		}
	})
}

// ------------------------------------------------------------------ build --

// TestBuildIPv6UDPWritesTheHeaderTheChecksumCovers is defeat row T-1 from the
// sending side: the addresses in the IPv6 header and the addresses in the
// pseudo-header have to be the same two, and nothing but a round trip through
// a receiver would notice if they were not.
func TestBuildIPv6UDPWritesTheHeaderTheChecksumCovers(t *testing.T) {
	src := addr6(t, "fe80::3802:5eff:fee7:dfe0")
	dst := AllDHCPRelayAgentsAndServers
	payload := []byte{1, 2, 3, 4, 5}
	frame, err := BuildIPv6UDP(src, dst, ClientPort6, ServerPort6, dhcpHopLimit, payload)
	if err != nil {
		t.Fatalf("BuildIPv6UDP: %v", err)
	}
	if got := frame[0] >> 4; got != ipv6Version {
		t.Errorf("version nibble = %d, want %d", got, ipv6Version)
	}
	if got, want := binary.BigEndian.Uint16(frame[4:6]), uint16(udpHeaderLen+len(payload)); got != want {
		t.Errorf("payload length = %d, want %d", got, want)
	}
	if frame[6] != protoUDP {
		t.Errorf("next header = %d, want %d", frame[6], protoUDP)
	}
	if frame[7] != dhcpHopLimit {
		t.Errorf("hop limit = %d, want %d", frame[7], dhcpHopLimit)
	}
	if got := netip.AddrFrom16([16]byte(frame[8:24])); got != src {
		t.Errorf("header source = %s, want %s", got, src)
	}
	if got := netip.AddrFrom16([16]byte(frame[24:40])); got != dst {
		t.Errorf("header destination = %s, want %s", got, dst)
	}
	if got := udpChecksumVerify6(src.As16(), dst.As16(), frame[40:]); got != 0 {
		t.Errorf("the frame does not verify against the addresses in its own header (%#04x)", got)
	}
	if binary.BigEndian.Uint16(frame[40+6:40+8]) == 0 {
		t.Errorf("the checksum field is zero, which RFC 8200 section 8.1 tells every receiver to discard")
	}
}

// TestBuildRefusesAnAddressItCannotSend drives the refusals.
func TestBuildRefusesAnAddressItCannotSend(t *testing.T) {
	v4 := addr6(t, "192.0.2.1")
	ok := addr6(t, "fe80::1")
	if _, err := BuildIPv6UDP(v4, ok, ClientPort6, ServerPort6, 1, nil); !errors.Is(err, ErrNotIPv6Frame) {
		t.Errorf("an IPv4 source: err = %v, want ErrNotIPv6Frame", err)
	}
	if _, err := BuildIPv6UDP(ok, v4, ClientPort6, ServerPort6, 1, nil); !errors.Is(err, ErrNotIPv6Frame) {
		t.Errorf("an IPv4 destination: err = %v, want ErrNotIPv6Frame", err)
	}
	if _, err := BuildIPv6UDP(netip.Addr{}, ok, ClientPort6, ServerPort6, 1, nil); !errors.Is(err, ErrNotIPv6Frame) {
		t.Errorf("the zero Addr: err = %v, want ErrNotIPv6Frame", err)
	}
}

// ----------------------------------------------------------------- ICMPv6 --

// TestBuildIPv6ICMPPutsExactlyTheChecksumsAddressesInTheHeader is defeat row
// N-2, and it is the reason lease.ND takes a wire.ICMPv6Packet rather than a
// []byte.
//
// An ICMPv6 message is not self-contained: RFC 4443 section 2.3 computes its
// checksum over a pseudo-header made of the source and destination addresses,
// so those two are part of the encoded message whether or not they are part of
// its bytes. If ring 3 chose its own source — the interface's address, say,
// instead of the unspecified address a duplicate-address probe must use — the
// frame would carry a checksum computed over a different pseudo-header than
// the one it advertises, and every receiver would drop it silently.
func TestBuildIPv6ICMPPutsExactlyTheChecksumsAddressesInTheHeader(t *testing.T) {
	target := addr6(t, "fd00:99::1a3")
	pkt, err := wire.EncodeDADNeighborSolicit(target)
	if err != nil {
		t.Fatalf("EncodeDADNeighborSolicit: %v", err)
	}
	frame, err := BuildIPv6ICMP(pkt)
	if err != nil {
		t.Fatalf("BuildIPv6ICMP: %v", err)
	}
	if got := netip.AddrFrom16([16]byte(frame[8:24])); got != pkt.Src {
		t.Errorf("header source = %s, but the checksum was computed over %s", got, pkt.Src)
	}
	if got := netip.AddrFrom16([16]byte(frame[24:40])); got != pkt.Dst {
		t.Errorf("header destination = %s, but the checksum was computed over %s", got, pkt.Dst)
	}
	// RFC 4861 section 4.3: the hop limit of every Neighbor Discovery message
	// is 255, which is what section 6.1.1's receiver check tests.
	if frame[7] != NDHopLimit {
		t.Errorf("hop limit = %d, want %d", frame[7], NDHopLimit)
	}
	if frame[6] != wire.ICMPv6NextHeader {
		t.Errorf("next header = %d, want %d", frame[6], wire.ICMPv6NextHeader)
	}
	// And the frame verifies as a whole — the round trip through this
	// package's own parser and the codec's verifier, which read the addresses
	// out of the header rather than being told them.
	f, err := ParseIPv6ICMP(frame)
	if err != nil {
		t.Fatalf("ParseIPv6ICMP: %v", err)
	}
	if !wire.VerifyICMPv6Checksum(f.Src, f.Dst, f.Body) {
		t.Errorf("the built frame does not verify against the addresses in its own header")
	}
	if f.Src != netip.IPv6Unspecified() {
		t.Errorf("source = %s, but RFC 4862 section 5.4.2 sets it to the unspecified address", f.Src)
	}
}

// TestParseIPv6ICMPReadsARealRouterAdvertisement is the boundary between this
// file and the codec, driven from both sides.
//
// ParseIPv6ICMP says WHERE the message is and which addresses its checksum
// covers; it deliberately makes neither of RFC 4861 section 6.1.2's two checks
// that need those fields. NDSocket makes them, so that a frame failing one
// lands on its own counter instead of disappearing into a parse error.
func TestParseIPv6ICMPReadsARealRouterAdvertisement(t *testing.T) {
	f, err := ParseIPv6ICMP(capV6RouterAdvert)
	if err != nil {
		t.Fatalf("ParseIPv6ICMP: %v", err)
	}
	if f.HopLimit != NDHopLimit {
		t.Errorf("HopLimit = %d, want %d", f.HopLimit, NDHopLimit)
	}
	if want := addr6(t, "fe80::30e6:f9ff:fe2f:aa1e"); f.Src != want {
		t.Errorf("Src = %s, want %s", f.Src, want)
	}
	if want := wire.AllNodesMulticast; f.Dst != want {
		t.Errorf("Dst = %s, want %s", f.Dst, want)
	}
	if f.Body[0] != wire.ICMPv6RouterAdvert {
		t.Errorf("type = %d, want %d", f.Body[0], wire.ICMPv6RouterAdvert)
	}
	if !wire.VerifyICMPv6Checksum(f.Src, f.Dst, f.Body) {
		t.Errorf("a captured Router Advertisement does not verify against its own addresses")
	}
	if _, err := wire.DecodeRouterAdvert(f.Body); err != nil {
		t.Errorf("the body this parser handed on does not decode: %v", err)
	}

	t.Run("a corrupt checksum is NOT this function's refusal", func(t *testing.T) {
		bad := bytes.Clone(capV6RouterAdvert)
		bad[40+2] ^= 0xFF
		f, err := ParseIPv6ICMP(bad)
		if err != nil {
			t.Fatalf("ParseIPv6ICMP refused a frame whose only fault is its checksum: %v", err)
		}
		if wire.VerifyICMPv6Checksum(f.Src, f.Dst, f.Body) {
			t.Errorf("the corrupted frame still verifies")
		}
	})

	t.Run("a hop limit below 255 is NOT this function's refusal either", func(t *testing.T) {
		bad := bytes.Clone(capV6RouterAdvert)
		bad[7] = 64
		f, err := ParseIPv6ICMP(bad)
		if err != nil {
			t.Fatalf("ParseIPv6ICMP refused a frame whose only fault is its hop limit: %v", err)
		}
		if f.HopLimit != 64 {
			t.Errorf("HopLimit = %d, want the 64 that was written into the frame", f.HopLimit)
		}
	})
}

// TestParseIPv6ICMPRefusesWhatIsNotICMPv6 keeps the two parsers apart: the
// DHCPv6 Advertise is a valid IPv6 frame and is not an ICMPv6 message.
func TestParseIPv6ICMPRefusesWhatIsNotICMPv6(t *testing.T) {
	if _, err := ParseIPv6ICMP(capV6Advertise); !errors.Is(err, ErrNotICMPv6) {
		t.Errorf("err = %v, want ErrNotICMPv6", err)
	}
	if _, err := ParseIPv6UDP(capV6RouterAdvert); !errors.Is(err, ErrNotUDP) {
		t.Errorf("err = %v, want ErrNotUDP", err)
	}
}
