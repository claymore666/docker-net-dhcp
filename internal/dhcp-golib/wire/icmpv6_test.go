package wire

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

// ---------------------------------------------------------------- checksum --

// TestChecksumVerifiesEveryCapturedFrame is B-4, and it is the only kind of row
// that can settle it. A checksum computed without RFC 8200 §8.1's pseudo-header
// agrees with itself on every frame this package builds and disagrees with
// every frame from anywhere else — so the fixture has to come from anywhere
// else.
//
// Two of these frames are ours and seven are not. The two ours are here for a
// reason of their own: the Router Solicitation was sent on a raw ICMPv6 socket,
// where the Linux kernel computes the checksum itself, and the octets recorded
// off the wire are byte for byte the octets EncodeRouterSolicit produced. Two
// implementations, one answer, on a frame neither could see the other compute.
//
// The two Router Advertisements are the SAME body with different destinations
// — unicast to the solicitor, then the periodic multicast one second later —
// and they carry different checksums. That pair is what makes "the destination
// is in the sum" visible rather than assumed.
func TestChecksumVerifiesEveryCapturedFrame(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, dst string
		body     []byte
	}{
		{"our router solicitation", capRouterSolicitSrc, capRouterSolicitDst, capRouterSolicit},
		{"dnsmasq's unicast router advertisement", capRouterAdvertSrc, capRouterAdvertDst, capRouterAdvert},
		{"dnsmasq's multicast router advertisement", capRouterAdvertMcastSrc, capRouterAdvertMcastDst, capRouterAdvertMcast},
		{"the kernel's neighbor advertisement", capNeighborAdvertSrc, capNeighborAdvertDst, capNeighborAdvert},
		{"the kernel's DAD solicitation", capKernelDADSrc, capKernelDADDst, capKernelDAD},
		{"our DAD solicitation", capOurDADSrc, capOurDADDst, capOurDAD},
		{"the defence it provoked", capDefenceSrc, capDefenceDst, capDefence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := addr(t, tc.src), addr(t, tc.dst)
			if !VerifyICMPv6Checksum(src, dst, tc.body) {
				t.Fatalf("the checksum on a frame that crossed a real link does not verify:\n  src %s\n  dst %s\n  %x", src, dst, tc.body)
			}
			// Computing must give back exactly the octets the sender wrote.
			want := uint16(tc.body[2])<<8 | uint16(tc.body[3])
			if got := ICMPv6Checksum(src, dst, tc.body); got != want {
				t.Errorf("ICMPv6Checksum = %#04x, the frame carries %#04x", got, want)
			}
		})
	}
}

// TestChecksumCoversThePseudoHeader moves one variable at a time through the
// four fields RFC 8200 §8.1 puts in front of the message. Each must change the
// answer; a checksum that ignores any of them passes the row above only because
// it was handed the right addresses.
func TestChecksumCoversThePseudoHeader(t *testing.T) {
	src := addr(t, capRouterAdvertSrc)
	dst := addr(t, capRouterAdvertDst)
	base := ICMPv6Checksum(src, dst, capRouterAdvert)

	if got := ICMPv6Checksum(addr(t, "fe80::4416:80ff:fe75:e6ac"), dst, capRouterAdvert); got == base {
		t.Error("changing the SOURCE address left the checksum unchanged; the pseudo-header's source is not in the sum")
	}
	if got := ICMPv6Checksum(src, addr(t, "ff02::1"), capRouterAdvert); got == base {
		t.Error("changing the DESTINATION address left the checksum unchanged; the pseudo-header's destination is not in the sum")
	}
	shorter := append([]byte(nil), capRouterAdvert...)
	shorter = shorter[:len(shorter)-8]
	if got := ICMPv6Checksum(src, dst, shorter); got == base {
		t.Error("dropping eight octets left the checksum unchanged; the upper-layer length is not in the sum")
	}
	body := append([]byte(nil), capRouterAdvert...)
	body[4] ^= 0x01
	if got := ICMPv6Checksum(src, dst, body); got == base {
		t.Error("changing an octet of the body left the checksum unchanged")
	}

	// §2.3: "For computing the checksum, the checksum field is first set to
	// zero." So whatever the field holds must not reach the sum, or computing
	// and verifying are two different functions.
	zeroed := append([]byte(nil), capRouterAdvert...)
	zeroed[2], zeroed[3] = 0, 0
	if got := ICMPv6Checksum(src, dst, zeroed); got != base {
		t.Errorf("zeroing the checksum field changed the result (%#04x vs %#04x): the field is being summed", got, base)
	}
	garbage := append([]byte(nil), capRouterAdvert...)
	garbage[2], garbage[3] = 0xAA, 0x55
	if got := ICMPv6Checksum(src, dst, garbage); got != base {
		t.Errorf("a different checksum field changed the result (%#04x vs %#04x)", got, base)
	}
	if VerifyICMPv6Checksum(src, dst, garbage) {
		t.Error("a frame carrying the wrong checksum verified")
	}
	if VerifyICMPv6Checksum(src, dst, nil) {
		t.Error("an empty body verified; there is no checksum field to check")
	}
	if VerifyICMPv6Checksum(src, dst, []byte{0x86, 0x00, 0x00}) {
		t.Error("a 3-octet body verified")
	}
}

// ------------------------------------------------------ Router Advertisement --

// TestCapturedRouterAdvertDecodes is B-1 and B-2 against the frame dnsmasq
// 2.91 actually sent, with --enable-ra over a DHCPv6 range.
//
// tcpdump reads this frame as
//
//	router advertisement, length 120
//	  hop limit 64, Flags [managed, other stateful], pref medium, router lifetime 300s
//	  prefix info option (3), length 32: fd00:99::/64, Flags [onlink]
//
// and the two flag pairs it names are the point. M and O are both SET, so this
// frame alone cannot tell them apart — the synthetic rows below do that. L and
// A are NOT both set: the prefix is on-link and NOT autonomous, because dnsmasq
// is handing the addresses out over DHCPv6 rather than letting the host form
// them. A decoder that read A from the L bit would report autonomous here.
func TestCapturedRouterAdvertDecodes(t *testing.T) {
	ra, err := DecodeRouterAdvert(capRouterAdvert)
	if err != nil {
		t.Fatalf("DecodeRouterAdvert: %v", err)
	}
	if ra.CurHopLimit != 64 {
		t.Errorf("CurHopLimit = %d, want 64", ra.CurHopLimit)
	}
	if !ra.Managed || !ra.Other {
		t.Errorf("Managed=%v Other=%v, want both set: dnsmasq served a DHCPv6 range and tcpdump read [managed, other stateful]",
			ra.Managed, ra.Other)
	}
	if ra.RouterLifetime != 300 {
		t.Errorf("RouterLifetime = %d, want 300 (--ra-param=v6srv,60,300)", ra.RouterLifetime)
	}
	if ra.ReachableTime != 0 || ra.RetransTimer != 0 {
		t.Errorf("ReachableTime=%d RetransTimer=%d, want 0 and 0 as dnsmasq left them",
			ra.ReachableTime, ra.RetransTimer)
	}
	if len(ra.Prefixes) != 1 {
		t.Fatalf("%d Prefix Information option(s), want 1", len(ra.Prefixes))
	}
	p := ra.Prefixes[0]
	if p.PrefixLen != 64 || p.Prefix != addr(t, "fd00:99::") {
		t.Errorf("prefix %s/%d, want fd00:99::/64", p.Prefix, p.PrefixLen)
	}
	if !p.OnLink {
		t.Error("OnLink is clear; tcpdump read Flags [onlink]")
	}
	if p.Autonomous {
		t.Error("Autonomous is set; tcpdump read Flags [onlink] only, and dnsmasq is handing addresses out over DHCPv6")
	}
	if p.ValidLifetime != 300 || p.PreferredLifetime != 300 {
		t.Errorf("prefix lifetimes valid=%d preferred=%d, want 300 and 300", p.ValidLifetime, p.PreferredLifetime)
	}

	// The frame carries a Source Link-Layer Address option (type 1), an MTU
	// option (type 5), a Route Information option (24) and a DNSSL option
	// (31) — none of which this decoder reads. Walking past all four and
	// still finding the Prefix Information option is the property.
	if s := ra.String(); !strings.Contains(s, "MO") || !strings.Contains(s, "fd00:99::/64") {
		t.Errorf("String() = %q, want the flags and the prefix", s)
	}

	// The same body, multicast: identical content, different checksum.
	mc, err := DecodeRouterAdvert(capRouterAdvertMcast)
	if err != nil {
		t.Fatalf("DecodeRouterAdvert (multicast): %v", err)
	}
	if mc.String() != ra.String() {
		t.Errorf("the two captured advertisements decoded differently:\n  %s\n  %s", ra, mc)
	}
}

// TestRouterAdvertFlagsAreReadFromTheirOwnBits is B-1, B-2 and B-3, and it is
// SYNTHETIC because it has to be: the captured frame sets M and O together and
// no fixture on this box sets RFC 9762's P.
//
// RFC 4861 §4.2 draws the octet "|M|O|  Reserved |" most significant bit first,
// so M is 0x80 and O is 0x40. §4.6.2 draws "|L|A| Reserved1 |", so L is 0x80
// and A is 0x40. RFC 9762 puts R at 0x20 and P at 0x10 in the RA octet, and
// §7.1 says: "Clients that do not support DHCPv6 prefix delegation MUST ignore
// the P flag." Prefix delegation is v2.2 (D25), so this decoder reads M and O
// and nothing else in that octet changes its answer.
func TestRouterAdvertFlagsAreReadFromTheirOwnBits(t *testing.T) {
	ra := func(flags uint8) []byte {
		b := make([]byte, raFixedLen)
		b[0] = ICMPv6RouterAdvert
		b[5] = flags
		return b
	}
	for _, tc := range []struct {
		name           string
		flags          uint8
		managed, other bool
	}{
		{"neither", 0x00, false, false},
		{"managed only", 0x80, true, false},
		{"other only", 0x40, false, true},
		{"both", 0xC0, true, true},
		{"RFC 9762 R set, nothing else", 0x20, false, false},
		{"RFC 9762 P set, nothing else", 0x10, false, false},
		{"every reserved bit set", 0x3F, false, false},
		{"managed with every reserved bit", 0xBF, true, false},
		{"other with every reserved bit", 0x7F, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeRouterAdvert(ra(tc.flags))
			if err != nil {
				t.Fatalf("DecodeRouterAdvert: %v", err)
			}
			if got.Managed != tc.managed || got.Other != tc.other {
				t.Errorf("flags %#02x decoded to Managed=%v Other=%v, want %v and %v",
					tc.flags, got.Managed, got.Other, tc.managed, tc.other)
			}
		})
	}

	pio := func(flags uint8) []byte {
		b := make([]byte, raFixedLen+pioLen)
		b[0] = ICMPv6RouterAdvert
		o := b[raFixedLen:]
		o[0] = NDOptPrefixInfo
		o[1] = pioLen / 8
		o[2] = 64
		o[3] = flags
		return b
	}
	for _, tc := range []struct {
		name             string
		flags            uint8
		onLink, autonomo bool
	}{
		{"neither", 0x00, false, false},
		{"on-link only", 0x80, true, false},
		{"autonomous only", 0x40, false, true},
		{"both", 0xC0, true, true},
		{"RFC 9762 P set on the prefix", 0x10, false, false},
		{"on-link with every reserved bit", 0xBF, true, false},
		{"autonomous with every reserved bit", 0x7F, false, true},
	} {
		t.Run("prefix "+tc.name, func(t *testing.T) {
			got, err := DecodeRouterAdvert(pio(tc.flags))
			if err != nil {
				t.Fatalf("DecodeRouterAdvert: %v", err)
			}
			if len(got.Prefixes) != 1 {
				t.Fatalf("%d prefix option(s), want 1", len(got.Prefixes))
			}
			p := got.Prefixes[0]
			if p.OnLink != tc.onLink || p.Autonomous != tc.autonomo {
				t.Errorf("prefix flags %#02x decoded to OnLink=%v Autonomous=%v, want %v and %v",
					tc.flags, p.OnLink, p.Autonomous, tc.onLink, tc.autonomo)
			}
			if s := p.String(); !strings.Contains(s, "/64") {
				t.Errorf("PrefixInfo.String() = %q, want the prefix length", s)
			}
		})
	}
}

// TestRouterAdvertLifetimesComeFromTheirOwnOffsets separates the three
// durations that a captured frame cannot: dnsmasq left Reachable Time and
// Retrans Timer at zero, and set the prefix's valid and preferred lifetimes to
// the same value. Adjacent uint32s at fixed offsets are exactly the fields a
// transposition hides in.
func TestRouterAdvertLifetimesComeFromTheirOwnOffsets(t *testing.T) {
	b := make([]byte, raFixedLen+pioLen)
	b[0] = ICMPv6RouterAdvert
	b[4] = 64
	b[5] = raFlagManaged
	be16(b[6:8], 1111)
	be32(b[8:12], 2222)
	be32(b[12:16], 3333)
	o := b[raFixedLen:]
	o[0] = NDOptPrefixInfo
	o[1] = pioLen / 8
	o[2] = 48
	o[3] = pioFlagOnLink
	be32(o[4:8], 4444)
	be32(o[8:12], 5555)
	pfx := addr(t, "2001:db8:1::").As16()
	copy(o[16:32], pfx[:])

	ra, err := DecodeRouterAdvert(b)
	if err != nil {
		t.Fatalf("DecodeRouterAdvert: %v", err)
	}
	if ra.RouterLifetime != 1111 {
		t.Errorf("RouterLifetime = %d, want 1111 (§4.2, octets 6..8, SECONDS)", ra.RouterLifetime)
	}
	if ra.ReachableTime != 2222 {
		t.Errorf("ReachableTime = %d, want 2222 (§4.2, octets 8..12, MILLISECONDS)", ra.ReachableTime)
	}
	if ra.RetransTimer != 3333 {
		t.Errorf("RetransTimer = %d, want 3333 (§4.2, octets 12..16, MILLISECONDS)", ra.RetransTimer)
	}
	if len(ra.Prefixes) != 1 {
		t.Fatalf("%d prefix option(s), want 1", len(ra.Prefixes))
	}
	p := ra.Prefixes[0]
	if p.ValidLifetime != 4444 {
		t.Errorf("prefix ValidLifetime = %d, want 4444 (§4.6.2, octets 4..8)", p.ValidLifetime)
	}
	if p.PreferredLifetime != 5555 {
		t.Errorf("prefix PreferredLifetime = %d, want 5555 (§4.6.2, octets 8..12)", p.PreferredLifetime)
	}
	if p.PrefixLen != 48 || p.Prefix != addr(t, "2001:db8:1::") {
		t.Errorf("prefix %s/%d, want 2001:db8:1::/48", p.Prefix, p.PrefixLen)
	}
}

// TestRouterAdvertRefusesWhatItCannotWalk is A-7's ICMPv6 half and B-6.
func TestRouterAdvertRefusesWhatItCannotWalk(t *testing.T) {
	full := func() []byte {
		b := make([]byte, raFixedLen)
		b[0] = ICMPv6RouterAdvert
		return b
	}
	for _, tc := range []struct {
		name string
		body []byte
		want error
	}{
		{"empty", nil, ErrICMPv6Short},
		{"one octet short of the fixed part", make([]byte, raFixedLen-1), ErrICMPv6Short},
		{"a neighbor advertisement", append(full()[:0:0], append([]byte{ICMPv6NeighborAdvert}, make([]byte, raFixedLen-1)...)...), ErrICMPv6Type},
		{"a router solicitation", append([]byte{ICMPv6RouterSolicit}, make([]byte, raFixedLen-1)...), ErrICMPv6Type},
		{"a one-octet option tail", append(full(), 0x01), ErrNDOption},
		{"an option of length zero", append(full(), 0x01, 0x00, 0, 0, 0, 0, 0, 0), ErrNDOption},
		{"an option running past the end", append(full(), 0x01, 0x02, 0, 0), ErrNDOption},
		{"a prefix option of the wrong length", append(full(), NDOptPrefixInfo, 0x01, 0, 0, 0, 0, 0, 0), ErrNDOption},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ra, err := DecodeRouterAdvert(tc.body)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DecodeRouterAdvert = %v, %v; want %v", ra, err, tc.want)
			}
			if ra != nil {
				t.Errorf("a refused decode returned an advertisement: %v", ra)
			}
		})
	}

	// The other direction: an option this decoder does not read must be walked
	// past, not refused. The captured frame carries four such options; here is
	// the smallest case, an unknown type between two known ones.
	b := full()
	b = append(b, NDOptSourceLinkAddr, 0x01, 0xea, 0x49, 0x4e, 0xe5, 0x31, 0xed)
	b = append(b, 0xFE, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	pio := make([]byte, pioLen)
	pio[0], pio[1], pio[2], pio[3] = NDOptPrefixInfo, pioLen/8, 64, pioFlagOnLink
	b = append(b, pio...)
	ra, err := DecodeRouterAdvert(b)
	if err != nil {
		t.Fatalf("an unknown ND option was refused: %v", err)
	}
	if len(ra.Prefixes) != 1 {
		t.Errorf("%d prefix option(s) after walking past an unknown one, want 1", len(ra.Prefixes))
	}
}

// ---------------------------------------------------- Router Solicitation --

// TestRouterSolicitCarriesItsSourceAndItsOption pins RFC 4861 §4.1's two
// halves, which are a pair: the Source Link-Layer Address option "MUST NOT be
// included if the Source Address is the unspecified address. Otherwise, it
// SHOULD be included on link layers that have addresses."
//
// The captured frame is the one dnsmasq answered — its log reads
// "RTR-SOLICIT(v6srv) ea:49:4e:e5:31:ed", which is the MAC out of the option
// below, so the option was not merely present but read.
func TestRouterSolicitCarriesItsSourceAndItsOption(t *testing.T) {
	mac := mustHex("ea494ee531ed")
	ll := addr(t, capRouterSolicitSrc)
	pkt, err := EncodeRouterSolicit(ll, mac)
	if err != nil {
		t.Fatalf("EncodeRouterSolicit: %v", err)
	}
	if pkt.Src != ll {
		t.Errorf("source %s, want %s", pkt.Src, ll)
	}
	if pkt.Dst != AllRoutersMulticast {
		t.Errorf("destination %s, want %s (§4.1)", pkt.Dst, AllRoutersMulticast)
	}
	if !bytes.Equal(pkt.Body, capRouterSolicit) {
		t.Errorf("encoded\n  %x\nbut the octets that crossed the link were\n  %x", pkt.Body, capRouterSolicit)
	}
	if !VerifyICMPv6Checksum(pkt.Src, pkt.Dst, pkt.Body) {
		t.Error("the encoder's own checksum does not verify against its own addresses")
	}
	if pkt.Body[0] != ICMPv6RouterSolicit {
		t.Errorf("type %d, want %d", pkt.Body[0], ICMPv6RouterSolicit)
	}

	// From the unspecified address the option MUST NOT be there, and asking
	// for it is an error rather than a silently dropped option.
	pkt, err = EncodeRouterSolicit(netip.IPv6Unspecified(), nil)
	if err != nil {
		t.Fatalf("EncodeRouterSolicit from ::: %v", err)
	}
	if len(pkt.Body) != rsFixedLen {
		t.Errorf("a solicitation from :: is %d octets, want %d with no option (§4.1)", len(pkt.Body), rsFixedLen)
	}
	if _, err := EncodeRouterSolicit(netip.IPv6Unspecified(), mac); !errors.Is(err, ErrICMPv6Encode) {
		t.Errorf("a solicitation from :: carrying a link-layer address: %v, want %v", err, ErrICMPv6Encode)
	}
	// A REAL SOURCE WITH NO LINK-LAYER ADDRESS IS ENCODED, NOT REFUSED, and
	// this row replaces one that asserted the refusal (M7a review finding 5).
	// §4.1: "Otherwise, it SHOULD be included on link layers that have
	// addresses." The condition is about the LINK LAYER, which this function
	// cannot see; a caller passing nothing is either on a link layer with no
	// address, where §4.1 asks for nothing at all, or has made a mistake that
	// looks identical from here. Rendering that SHOULD as a refusal made a
	// conformant packet unconstructible.
	bare, err := EncodeRouterSolicit(ll, nil)
	if err != nil {
		t.Fatalf("a solicitation from a real source with no link-layer address: %v, want it encoded (§4.1's SHOULD)", err)
	}
	if len(bare.Body) != rsFixedLen {
		t.Errorf("it is %d octets, want %d — the option must be absent, not empty", len(bare.Body), rsFixedLen)
	}
	if !VerifyICMPv6Checksum(bare.Src, bare.Dst, bare.Body) {
		t.Error("its checksum does not verify, so the SHOULD half was encoded wrong rather than omitted")
	}
	if _, err := EncodeRouterSolicit(addr(t, "192.0.2.1"), mac); !errors.Is(err, ErrNotIPv6) {
		t.Errorf("a solicitation from an IPv4 address: %v, want %v", err, ErrNotIPv6)
	}
	if _, err := EncodeRouterSolicit(netip.Addr{}, mac); !errors.Is(err, ErrNotIPv6) {
		t.Errorf("a solicitation from the zero Addr: %v, want %v", err, ErrNotIPv6)
	}
	if _, err := EncodeRouterSolicit(ll, mustHex("ea494ee5")); !errors.Is(err, ErrICMPv6Encode) {
		t.Errorf("a 4-octet link-layer address: %v, want %v", err, ErrICMPv6Encode)
	}
}

// ---------------------------------------------------------------- DAD --

// TestDADSolicitIsSentFromTheUnspecifiedAddress is B-5, the v6 shape of the
// defect this project already has a name for at the v4 layer.
//
// RFC 4862 §5.4.2: "the solicitation's Target Address is set to the address
// being checked, the IP source is set to the unspecified address, and the IP
// destination is set to the solicited-node multicast address of the target
// address." A probe sent FROM the address it is probing for answers its own
// question.
//
// The outside evidence is capOurDAD and capDefence: these octets, checksum
// included, were put on a link from an AF_PACKET socket — no kernel recomputed
// anything — against an address a peer already held, and the peer's stack
// answered with RFC 4862 §5.4.3's defence. A frame with a bad checksum is
// dropped silently, so the answer is somebody else's stack saying our
// arithmetic was right.
func TestDADSolicitIsSentFromTheUnspecifiedAddress(t *testing.T) {
	target := addr(t, "fd00:99::defe")
	pkt, err := EncodeDADNeighborSolicit(target)
	if err != nil {
		t.Fatalf("EncodeDADNeighborSolicit: %v", err)
	}
	if !pkt.Src.IsUnspecified() {
		t.Fatalf("source %s, want :: — a DAD probe from the address it is probing for answers its own question", pkt.Src)
	}
	if want := addr(t, capOurDADDst); pkt.Dst != want {
		t.Errorf("destination %s, want %s", pkt.Dst, want)
	}
	if len(pkt.Body) != nsFixedLen {
		t.Errorf("body is %d octets, want %d: RFC 4861 §4.3 says the Source Link-Layer Address option MUST NOT be "+
			"included when the source is the unspecified address", len(pkt.Body), nsFixedLen)
	}
	if !bytes.Equal(pkt.Body, capOurDAD) {
		t.Errorf("encoded\n  %x\nbut the octets that provoked a defence were\n  %x", pkt.Body, capOurDAD)
	}
	if got, want := netip.AddrFrom16([16]byte(pkt.Body[8:24])), target; got != want {
		t.Errorf("target field %s, want %s", got, want)
	}

	for _, tc := range []struct {
		name string
		in   netip.Addr
		want error
	}{
		{"IPv4", addr(t, "192.0.2.1"), ErrNotIPv6},
		{"the zero Addr", netip.Addr{}, ErrNotIPv6},
		{"a multicast target", addr(t, "ff02::1"), ErrICMPv6Encode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncodeDADNeighborSolicit(tc.in); !errors.Is(err, tc.want) {
				t.Errorf("EncodeDADNeighborSolicit(%s) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

// TestCapturedNeighborAdvertsDecode reads the two real advertisements: this
// host's kernel answering an ordinary solicitation, and a peer defending an
// address against our DAD probe. Their flags DIFFER, which is what makes the
// three-bit field readable at all.
//
// RFC 4862 §5.4.4 is why the S flag is carried rather than folded away: a DAD
// failure is an advertisement for the tentative address, and one sent in
// answer to somebody else's solicitation is not one.
func TestCapturedNeighborAdvertsDecode(t *testing.T) {
	solicited, err := DecodeNeighborAdvert(capNeighborAdvert)
	if err != nil {
		t.Fatalf("DecodeNeighborAdvert: %v", err)
	}
	if solicited.Router {
		t.Error("the kernel's advertisement decoded with R set; the host is not a router")
	}
	if !solicited.Solicited || !solicited.Override {
		t.Errorf("flags S=%v O=%v, want both set: this answered a solicitation from a real address",
			solicited.Solicited, solicited.Override)
	}
	if want := addr(t, "fe80::e849:4eff:fee5:31ed"); solicited.Target != want {
		t.Errorf("target %s, want %s", solicited.Target, want)
	}

	defence, err := DecodeNeighborAdvert(capDefence)
	if err != nil {
		t.Fatalf("DecodeNeighborAdvert (defence): %v", err)
	}
	if defence.Solicited {
		t.Error("the defence decoded with S SET; RFC 4862 §5.4.3 sends it unsolicited, because the solicitation came from ::")
	}
	if !defence.Override {
		t.Error("the defence decoded with O clear")
	}
	if want := addr(t, "fd00:99::defe"); defence.Target != want {
		t.Errorf("target %s, want %s — this is the address our probe asked about", defence.Target, want)
	}
	if s := defence.String(); !strings.Contains(s, "fd00:99::defe") {
		t.Errorf("String() = %q, want the target", s)
	}
}

// TestNeighborAdvertFlagsAreReadFromTheirOwnBits separates R, S and O, which
// the two captured frames cannot do alone: neither has R set. §4.4's octet is
// "|R|S|O| Reserved |".
func TestNeighborAdvertFlagsAreReadFromTheirOwnBits(t *testing.T) {
	na := func(flags uint8) []byte {
		b := make([]byte, naFixedLen)
		b[0] = ICMPv6NeighborAdvert
		b[4] = flags
		return b
	}
	for _, tc := range []struct {
		flags                     uint8
		router, solicited, overri bool
	}{
		{0x00, false, false, false},
		{0x80, true, false, false},
		{0x40, false, true, false},
		{0x20, false, false, true},
		{0xE0, true, true, true},
		{0x1F, false, false, false},
		{0x9F, true, false, false},
	} {
		got, err := DecodeNeighborAdvert(na(tc.flags))
		if err != nil {
			t.Fatalf("DecodeNeighborAdvert(%#02x): %v", tc.flags, err)
		}
		if got.Router != tc.router || got.Solicited != tc.solicited || got.Override != tc.overri {
			t.Errorf("flags %#02x decoded to R=%v S=%v O=%v, want %v %v %v",
				tc.flags, got.Router, got.Solicited, got.Override, tc.router, tc.solicited, tc.overri)
		}
	}

	for _, tc := range []struct {
		name string
		body []byte
		want error
	}{
		{"empty", nil, ErrICMPv6Short},
		{"one short", make([]byte, naFixedLen-1), ErrICMPv6Short},
		{"a router advertisement", append([]byte{ICMPv6RouterAdvert}, make([]byte, naFixedLen-1)...), ErrICMPv6Type},
		{"an option of length zero", append(na(0), 0x02, 0x00, 0, 0, 0, 0, 0, 0), ErrNDOption},
		{"an option running past the end", append(na(0), 0x02, 0x09), ErrNDOption},
		{"a one-octet option tail", append(na(0), 0x02), ErrNDOption},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeNeighborAdvert(tc.body); !errors.Is(err, tc.want) {
				t.Errorf("DecodeNeighborAdvert = %v, want %v", err, tc.want)
			}
		})
	}

	// RFC 7527's Nonce option, type 14, is one the Linux kernel puts on every
	// DAD solicitation it sends. This package does not decode it and must walk
	// past it; capKernelDAD is the frame that carries one.
	if err := walkNDOptions(capKernelDAD[nsFixedLen:], func(typ uint8, opt []byte) error {
		if typ != 14 || len(opt) != 8 {
			t.Errorf("the kernel's DAD solicitation carried option type %d of %d octet(s), want type 14 of 8", typ, len(opt))
		}
		return nil
	}); err != nil {
		t.Errorf("walking the kernel's own DAD solicitation: %v", err)
	}
}

// ------------------------------------------------------ derived addresses --

// TestSolicitedNodeMulticast is B-7, and the first row is RFC 4291 §2.7.1's own
// worked example: "for example, the Solicited-Node multicast address
// corresponding to the IPv6 address 4037::01:800:200E:8C6C is
// FF02::1:FF0E:8C6C."
//
// Every address maps to something in ff02::1:ff00:0/104, so a decoder taking
// the wrong 24 bits produces a plausible answer for every input. Only a fixed
// pair settles it — and the second pair is one a real stack produced: dnsmasq's
// host solicited fe80::e849:4eff:fee5:31ed at ff02::1:ffe5:31ed, and our own
// probe for fd00:99::defe went to ff02::1:ff00:defe and was answered.
func TestSolicitedNodeMulticast(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"4037::01:800:200E:8C6C", "ff02::1:ff0e:8c6c"},
		{"fe80::e849:4eff:fee5:31ed", "ff02::1:ffe5:31ed"},
		{"fd00:99::defe", "ff02::1:ff00:defe"},
		{"fd00:99::5150", "ff02::1:ff00:5150"},
		{"::", "ff02::1:ff00:0"},
		{"::1", "ff02::1:ff00:1"},
		{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "ff02::1:ffff:ffff"},
	} {
		got, err := SolicitedNodeMulticast(addr(t, tc.in))
		if err != nil {
			t.Fatalf("SolicitedNodeMulticast(%s): %v", tc.in, err)
		}
		if want := addr(t, tc.want); got != want {
			t.Errorf("SolicitedNodeMulticast(%s) = %s, want %s", tc.in, got, want)
		}
	}
	if _, err := SolicitedNodeMulticast(addr(t, "192.0.2.1")); !errors.Is(err, ErrNotIPv6) {
		t.Errorf("SolicitedNodeMulticast of an IPv4 address: want %v", ErrNotIPv6)
	}
	if _, err := SolicitedNodeMulticast(netip.Addr{}); !errors.Is(err, ErrNotIPv6) {
		t.Errorf("SolicitedNodeMulticast of the zero Addr: want %v", ErrNotIPv6)
	}
	if AllNodesMulticast.String() != "ff02::1" || AllRoutersMulticast.String() != "ff02::2" {
		t.Errorf("the well-known groups are %s and %s, want ff02::1 and ff02::2 (RFC 4291 §2.7.1)",
			AllNodesMulticast, AllRoutersMulticast)
	}
}

// TestLinkLocalFromMAC checks the fixture helper against the two addresses the
// kernel actually formed in the capture: veth ea:49:4e:e5:31:ed became
// fe80::e849:4eff:fee5:31ed, with the universal/local bit inverted and fffe
// inserted (RFC 4291 Appendix A).
func TestLinkLocalFromMAC(t *testing.T) {
	for _, tc := range []struct{ mac, want string }{
		{"ea494ee531ed", "fe80::e849:4eff:fee5:31ed"},
		{"46168075e6ab", "fe80::4416:80ff:fe75:e6ab"},
		{"fa90458ce1a1", "fe80::f890:45ff:fe8c:e1a1"},
	} {
		got, err := LinkLocalFromMAC(mustHex(tc.mac))
		if err != nil {
			t.Fatalf("LinkLocalFromMAC(%s): %v", tc.mac, err)
		}
		if want := addr(t, tc.want); got != want {
			t.Errorf("LinkLocalFromMAC(%s) = %s, want %s (the address the kernel gave that interface)", tc.mac, got, want)
		}
	}
	for _, n := range []int{0, 5, 7} {
		if _, err := LinkLocalFromMAC(make([]byte, n)); !errors.Is(err, ErrICMPv6Encode) {
			t.Errorf("LinkLocalFromMAC on %d octet(s): want %v", n, ErrICMPv6Encode)
		}
	}
}

// ------------------------------------------------------------------ fuzz --

// FuzzDecodeRouterAdvert drives the ND option walk B-6 names. The property is
// that no input hangs, panics, or produces a prefix the frame does not carry.
func FuzzDecodeRouterAdvert(f *testing.F) {
	for _, seed := range [][]byte{capRouterAdvert, capRouterAdvertMcast, capNeighborAdvert, capKernelDAD, capOurDAD, capDefence, capRouterSolicit} {
		f.Add(seed)
	}
	f.Add([]byte{})
	f.Add([]byte{ICMPv6RouterAdvert})
	f.Add(append(make([]byte, raFixedLen), 0x03, 0x00))
	f.Fuzz(func(t *testing.T, b []byte) {
		// Both decoders see the same octets, and each is driven twice: once as
		// given, once with the type octet forced to its own, so that the walk
		// is reached and not turned away at the type check.
		for _, typ := range []uint8{0, ICMPv6RouterAdvert, ICMPv6NeighborAdvert} {
			in := append([]byte(nil), b...)
			if typ != 0 && len(in) > 0 {
				in[0] = typ
			}
			ra, err := DecodeRouterAdvert(in)
			if err != nil && ra != nil {
				t.Fatal("DecodeRouterAdvert returned both an advertisement and an error")
			}
			if err == nil {
				for _, p := range ra.Prefixes {
					if p.PrefixLen > 128 {
						continue
					}
					if _, err := p.Prefix.Prefix(int(p.PrefixLen)); err != nil {
						t.Fatalf("a decoded prefix %s/%d is not a prefix: %v", p.Prefix, p.PrefixLen, err)
					}
				}
				_ = ra.String()
			}
			na, err := DecodeNeighborAdvert(in)
			if err != nil && na != nil {
				t.Fatal("DecodeNeighborAdvert returned both an advertisement and an error")
			}
			if err == nil {
				_ = na.String()
				// The target is exactly the sixteen octets it came from —
				// the invariant an offset error breaks.
				if got := na.Target.As16(); !bytes.Equal(got[:], in[8:24]) {
					t.Fatalf("target %s is not the octets at 8..24: %x vs %x", na.Target, got, in[8:24])
				}
				// And it has a solicited-node address unless it is
				// IPv4-mapped. That exception is not a gap: netip renders
				// ::ffff:a.b.c.d as a 4-in-6 address, requireIPv6 refuses
				// those on purpose, and no such address can be the target of
				// a real Neighbor Advertisement. FOUND BY THIS FUZZ TARGET
				// 2026-09-05; the corpus entry that found it is kept.
				if !na.Target.Is4In6() {
					if _, err := SolicitedNodeMulticast(na.Target); err != nil {
						t.Fatalf("a decoded target %s has no solicited-node address: %v", na.Target, err)
					}
				}
			}
		}
	})
}

// ------------------------------------------------- Neighbor Solicitation --

// TestTheKernelsOwnDADSolicitDecodes settles the decoder against a frame this
// package did not build.
//
// The fixture is Linux's own duplicate-address-detection solicitation for
// fd00:99::5150, captured in the namespace. It is the message RFC 4862 §5.4.3
// is written about, and it is worth more than a generated one for two reasons:
// its source is the unspecified address, which is the condition that separates
// a duplicate from an address resolution, and it carries RFC 7527's Nonce
// option (type 14) — an option this package does not decode and therefore has
// to WALK PAST. A decoder that refused an unknown option would refuse every
// Linux host's DAD probe and report every contested address free.
func TestTheKernelsOwnDADSolicitDecodes(t *testing.T) {
	ns, err := DecodeNeighborSolicit(capKernelDAD)
	if err != nil {
		t.Fatalf("DecodeNeighborSolicit(kernel DAD): %v", err)
	}
	if want := addr(t, "fd00:99::5150"); ns.Target != want {
		t.Errorf("Target = %s, want %s", ns.Target, want)
	}
	// §4.3: the Source Link-Layer Address option "MUST NOT be included when
	// the source IP address is the unspecified address", and the kernel does
	// not include one. A decoder reporting it present here would make ring 3's
	// §7.1.1 check refuse the frame.
	if ns.HasSourceLinkAddr {
		t.Errorf("HasSourceLinkAddr = true on a solicitation whose only option is the Nonce")
	}
	// The frame's own checksum verifies over the pseudo-header of the two
	// addresses it was sent between, which is what says the octets above are
	// the octets that crossed the link.
	if !VerifyICMPv6Checksum(addr(t, capKernelDADSrc), addr(t, capKernelDADDst), capKernelDAD) {
		t.Errorf("the captured kernel DAD solicitation does not verify against its own addresses")
	}
}

// TestOurOwnDADSolicitDecodesBack is the round trip, and it is here to keep
// the pair honest rather than to prove the encoder: capOurDAD is the frame
// EncodeDADNeighborSolicit produced, recorded off an AF_PACKET socket where no
// kernel touched it. Decoding it back is the only row that says the two halves
// of this package agree about where the Target Address sits.
func TestOurOwnDADSolicitDecodesBack(t *testing.T) {
	ns, err := DecodeNeighborSolicit(capOurDAD)
	if err != nil {
		t.Fatalf("DecodeNeighborSolicit(our DAD): %v", err)
	}
	if want := addr(t, "fd00:99::defe"); ns.Target != want {
		t.Errorf("Target = %s, want %s", ns.Target, want)
	}
	if ns.HasSourceLinkAddr {
		t.Errorf("HasSourceLinkAddr = true on a message the encoder puts no option in")
	}
}

// TestNeighborSolicitReadsTheSourceLinkAddrOption drives the OTHER value of
// the flag, which no captured frame in this package carries.
//
// It is the address-resolution solicitation an ordinary node sends: a unicast
// source, and therefore §4.3's option present. Without this row
// HasSourceLinkAddr is false in every test and a decoder that never set it
// would pass them all — and ring 3's §7.1.1 check ("If the IP source address
// is the unspecified address, there is no source link-layer address option in
// the message") would then never refuse anything.
func TestNeighborSolicitReadsTheSourceLinkAddrOption(t *testing.T) {
	target := addr(t, "fd00:99::100")
	body := make([]byte, nsFixedLen)
	body[0] = ICMPv6NeighborSolicit
	t16 := target.As16()
	copy(body[8:24], t16[:])
	opt, err := encodeLinkAddrOption(NDOptSourceLinkAddr, []byte{0xea, 0x49, 0x4e, 0xe5, 0x31, 0xed})
	if err != nil {
		t.Fatalf("encodeLinkAddrOption: %v", err)
	}
	body = append(body, opt...)

	ns, err := DecodeNeighborSolicit(body)
	if err != nil {
		t.Fatalf("DecodeNeighborSolicit: %v", err)
	}
	if ns.Target != target {
		t.Errorf("Target = %s, want %s", ns.Target, target)
	}
	if !ns.HasSourceLinkAddr {
		t.Errorf("HasSourceLinkAddr = false on a solicitation carrying option type %d", NDOptSourceLinkAddr)
	}
	if got, want := ns.String(), "NS target=fd00:99::100 +sllao"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestNeighborSolicitRefusesWhatSection711Refuses drives every refusal the
// decoder can make from the message's own octets, and names the one it must
// NOT make.
//
// The last row is the point of the table. §7.1.1's remaining checks are about
// the IPv6 header — the hop limit, the checksum and the two conditions on the
// source address — and a decoder that tried to make them here would have to
// invent the evidence. runtime.NDSocket makes them, and the "unknown option"
// row is the same boundary from the other side: an option this package cannot
// read is not a malformed packet.
func TestNeighborSolicitRefusesWhatSection711Refuses(t *testing.T) {
	good := func() []byte {
		b := make([]byte, nsFixedLen)
		b[0] = ICMPv6NeighborSolicit
		a := addr(t, "fd00:99::100").As16()
		copy(b[8:24], a[:])
		return b
	}
	for _, tc := range []struct {
		name string
		body func() []byte
		want error
	}{
		{
			// §7.1.1: "ICMP length (derived from the IP length) is 24 or more
			// octets."
			name: "shorter than the fixed part",
			body: func() []byte { return good()[:nsFixedLen-1] },
			want: ErrICMPv6Short,
		},
		{
			name: "a different ICMPv6 type",
			body: func() []byte { b := good(); b[0] = ICMPv6NeighborAdvert; return b },
			want: ErrICMPv6Type,
		},
		{
			// §7.1.1: "ICMP Code is 0."
			name: "a non-zero Code",
			body: func() []byte { b := good(); b[1] = 1; return b },
			want: ErrICMPv6Validity,
		},
		{
			// §7.1.1: "Target Address is not a multicast address."
			name: "a multicast Target Address",
			body: func() []byte {
				b := good()
				m := addr(t, "ff02::1").As16()
				copy(b[8:24], m[:])
				return b
			},
			want: ErrICMPv6Validity,
		},
		{
			// §4.6: "The value 0 is invalid. Nodes MUST silently discard an ND
			// packet that contains an option with length zero."
			name: "an option declaring length zero",
			body: func() []byte { return append(good(), NDOptSourceLinkAddr, 0, 0, 0, 0, 0, 0, 0) },
			want: ErrNDOption,
		},
		{
			name: "an option running past the message",
			body: func() []byte { return append(good(), NDOptSourceLinkAddr, 4, 0, 0) },
			want: ErrNDOption,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, err := DecodeNeighborSolicit(tc.body())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if ns != nil {
				t.Errorf("a refused solicitation returned %v", ns)
			}
		})
	}

	t.Run("an option this package cannot read is walked past", func(t *testing.T) {
		// RFC 7527's Nonce is one such option and the kernel sends it; so is
		// anything a future RFC adds. §4.6: "Future versions of this protocol
		// may define new option types.  Receivers MUST silently ignore any
		// options they do not recognize and continue processing the message."
		body := append(good(), 0xFE, 1, 0, 0, 0, 0, 0, 0)
		ns, err := DecodeNeighborSolicit(body)
		if err != nil {
			t.Fatalf("DecodeNeighborSolicit with an unknown option: %v", err)
		}
		if want := addr(t, "fd00:99::100"); ns.Target != want {
			t.Errorf("Target = %s, want %s", ns.Target, want)
		}
		if ns.HasSourceLinkAddr {
			t.Errorf("HasSourceLinkAddr = true, but the only option present is type 0xFE")
		}
	})
}
