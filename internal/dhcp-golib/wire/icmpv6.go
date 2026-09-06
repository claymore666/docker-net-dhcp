package wire

import (
	"errors"
	"fmt"
	"net/netip"
)

// The ICMPv6 Neighbor Discovery codec, RFC 4861 and RFC 4862.
//
// It lives in ring 0 beside the DHCPv6 and ARP codecs for the same reason the
// ARP one does: RFC 4862's duplicate address detection is DEFINED as a
// particular Neighbor Solicitation, so the thing that builds it has to be able
// to say exactly which bytes go where — and one of those "bytes" is the source
// address of the packet, which is the whole of what makes it a DAD probe.
//
// ONLY WHAT THE CLIENT READS. Router Advertisement decode (the M and O flags,
// the router lifetime and the Prefix Information option), Neighbor
// Advertisement decode (the target and the R/S/O flags), and encode for Router
// Solicitation and the DAD Neighbor Solicitation. Redirect, MTU, Router
// Renumbering and the rest are not decoded; §4.2's rule covers them: "Future
// versions of this protocol may define new option types. Receivers MUST
// silently ignore any options they do not recognize and continue processing the
// message."
//
// PREFIX DELEGATION IS OUT (D25), SO THE P FLAG IS IGNORED. RFC 9762 adds a P
// flag to the Prefix Information option after L, A and R, and §9.1 amends RFC
// 4861 §4.2's note to read "If the M, O, or P (RFC 9762) flags are not set,
// this indicates that no information is available via DHCPv6." This client does
// not support prefix delegation, and RFC 9762 §7.1 is explicit about what such
// a client does: "Clients that do not support DHCPv6 prefix delegation MUST
// ignore the P flag."

// The ICMPv6 types of RFC 4861 §4.
const (
	ICMPv6RouterSolicit   uint8 = 133 // §4.1
	ICMPv6RouterAdvert    uint8 = 134 // §4.2
	ICMPv6NeighborSolicit uint8 = 135 // §4.3
	ICMPv6NeighborAdvert  uint8 = 136 // §4.4
)

// ICMPv6NextHeader is the Next Header value the pseudo-header carries for
// ICMPv6, RFC 4443 §2.3: "The Next Header value used in the pseudo-header is
// 58."
const ICMPv6NextHeader = 58

// The ND option types of RFC 4861 §4.6.
const (
	NDOptSourceLinkAddr uint8 = 1
	NDOptTargetLinkAddr uint8 = 2
	NDOptPrefixInfo     uint8 = 3
)

// The fixed lengths of the four messages, in octets, measured from the type
// field: §4.1 8, §4.2 16, §4.3 24, §4.4 24.
const (
	rsFixedLen = 8
	raFixedLen = 16
	nsFixedLen = 24
	naFixedLen = 24
	// pioLen is the Prefix Information option's whole length. §4.6.2 gives it
	// as Length 4, and §4.6's Length is "The length of the option (including
	// the type and length fields) in units of 8 octets."
	pioLen = 32
)

// The refusals.
var (
	// ErrICMPv6Short is a message shorter than its own fixed part.
	ErrICMPv6Short = errors.New("wire: ICMPv6 message is shorter than its fixed header")
	// ErrICMPv6Type is a message of a type the caller did not ask for.
	ErrICMPv6Type = errors.New("wire: ICMPv6 message is not the type being decoded")
	// ErrNDOption is an ND option that cannot be walked: §4.6's "The value 0
	// is invalid. Nodes MUST silently discard an ND packet that contains an
	// option with length zero", or one whose length runs past the message.
	ErrNDOption = errors.New("wire: ICMPv6 neighbor discovery option is malformed")
	// ErrICMPv6Validity is a message that fails one of RFC 4861's per-type
	// validity checks with the packet's own octets — the Code field, or the
	// Neighbor Advertisement's Target Address.
	//
	// SEPARATE FROM ErrICMPv6Short AND ErrICMPv6Type, because the caller's
	// next step differs: those two say the buffer is not this message, this
	// one says it IS this message and RFC 4861 tells a node to discard it
	// anyway. The checks this package CANNOT make are the ones whose evidence
	// is outside the ICMPv6 body — §6.1.2's "IP Source Address is a link-local
	// address" and "The IP Hop Limit field has a value of 255", and the
	// checksum, which VerifyICMPv6Checksum takes the addresses for. Ring 3
	// owns those three; this is the boundary between them.
	ErrICMPv6Validity = errors.New("wire: ICMPv6 message fails an RFC 4861 validity check")
	// ErrICMPv6Encode is a packet that cannot be encoded.
	ErrICMPv6Encode = errors.New("wire: ICMPv6 packet cannot be encoded")
	// ErrNotIPv6 is an address that is not an IPv6 address.
	ErrNotIPv6 = errors.New("wire: address is not IPv6")
)

// ---------------------------------------------------------------- checksum --

// ICMPv6Checksum computes RFC 4443 §2.3's checksum over body, prepended with
// RFC 8200 §8.1's pseudo-header of src, dst, the upper-layer length and the
// Next Header value 58.
//
// §2.3: "The checksum is the 16-bit one's complement of the one's complement
// sum of the entire ICMPv6 message, starting with the ICMPv6 message type
// field, and prepended with a 'pseudo-header' of IPv6 header fields, as
// specified in [IPv6, Section 8.1]. ... For computing the checksum, the
// checksum field is first set to zero."
//
// THE PSEUDO-HEADER IS NOT AN OPTIMISATION AND ITS ABSENCE IS INVISIBLE TO A
// ROUND TRIP. A codec that computed and verified without it agrees with itself
// on every frame it built and disagrees with every frame from anywhere else,
// which is why the fixture for this is a CAPTURED frame and not a generated
// one.
//
// The body's own checksum field (octets 2 and 3) is treated as zero regardless
// of what it holds, so this is the same function for computing and for
// verifying.
func ICMPv6Checksum(src, dst netip.Addr, body []byte) uint16 {
	var sum uint32
	s16, d16 := src.As16(), dst.As16()
	for i := 0; i+1 < len(s16); i += 2 {
		sum += uint32(s16[i])<<8 | uint32(s16[i+1])
	}
	for i := 0; i+1 < len(d16); i += 2 {
		sum += uint32(d16[i])<<8 | uint32(d16[i+1])
	}
	n := uint32(len(body))
	sum += n >> 16
	sum += n & 0xFFFF
	sum += ICMPv6NextHeader

	for i := 0; i < len(body); i += 2 {
		var w uint32
		// Octets 2 and 3 are the checksum field itself, summed as zero.
		if i == 2 {
			continue
		}
		w = uint32(body[i]) << 8
		if i+1 < len(body) {
			w |= uint32(body[i+1])
		}
		sum += w
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xFFFF)
	}
	return ^uint16(sum)
}

// VerifyICMPv6Checksum reports whether body's checksum field matches what
// ICMPv6Checksum computes over the same pseudo-header.
func VerifyICMPv6Checksum(src, dst netip.Addr, body []byte) bool {
	if len(body) < 4 {
		return false
	}
	return ube16(body[2:4]) == ICMPv6Checksum(src, dst, body)
}

// ICMPv6Packet is an ICMPv6 message together with the two addresses its
// checksum was computed over.
//
// THE ADDRESSES TRAVEL WITH THE BYTES, and that is the design. RFC 4443 §2.3's
// checksum covers the source and destination addresses, so a caller that sent
// these octets from a different address would send a packet every receiver
// drops — silently, because a bad ICMPv6 checksum produces no error anywhere.
// RFC 4862 §5.4.2 then makes the source address of a DAD solicitation a
// protocol requirement in its own right. One value carrying all three makes the
// two facts impossible to separate; two returns would let ring 3 pick its own
// source and still pass every test in this package.
type ICMPv6Packet struct {
	Src, Dst netip.Addr
	Body     []byte
}

// ------------------------------------------------------ Router Solicitation --

// EncodeRouterSolicit builds RFC 4861 §4.1's Router Solicitation from src to
// AllRoutersMulticast.
//
// §4.1's source is "An IP address assigned to the sending interface, or the
// unspecified address if no address is assigned to the sending interface", and
// the Source Link-Layer Address option "MUST NOT be included if the Source
// Address is the unspecified address. Otherwise, it SHOULD be included on link
// layers that have addresses."
//
// THE TWO HALVES ARE NOT THE SAME STRENGTH AND ARE NOT ENFORCED THE SAME WAY.
// The unspecified-source half is a MUST NOT, so an option attached there is
// refused. The other half is a SHOULD, and the condition it is conditioned on
// — "on link layers that have addresses" — is one only the caller can answer:
// a caller that passes no address is either on a link layer without one, where
// §4.1 asks for nothing, or has made a mistake this function cannot tell apart
// from that. So it is encoded without the option and no error is returned.
// Refusing was this function's first shape and it rendered a SHOULD as a MUST
// (M7a review finding 5).
func EncodeRouterSolicit(src netip.Addr, linkHW []byte) (ICMPv6Packet, error) {
	if err := requireIPv6(src, "router solicitation source"); err != nil {
		return ICMPv6Packet{}, err
	}
	body := make([]byte, rsFixedLen)
	body[0] = ICMPv6RouterSolicit
	if !src.IsUnspecified() && len(linkHW) != 0 {
		opt, err := encodeLinkAddrOption(NDOptSourceLinkAddr, linkHW)
		if err != nil {
			return ICMPv6Packet{}, err
		}
		body = append(body, opt...)
	} else if src.IsUnspecified() && len(linkHW) != 0 {
		return ICMPv6Packet{}, fmt.Errorf("%w: a Router Solicitation from the unspecified address MUST NOT carry a Source Link-Layer Address option (RFC 4861 section 4.1)",
			ErrICMPv6Encode)
	}
	dst := AllRoutersMulticast
	be16(body[2:4], ICMPv6Checksum(src, dst, body))
	return ICMPv6Packet{Src: src, Dst: dst, Body: body}, nil
}

// ---------------------------------------------------- Router Advertisement --

// PrefixInfo is a decoded Prefix Information option, RFC 4861 §4.6.2.
type PrefixInfo struct {
	// PrefixLen is "the number of leading bits in the Prefix that are valid".
	PrefixLen uint8
	// OnLink is the L flag: "When set, indicates that this prefix can be used
	// for on-link determination."
	OnLink bool
	// Autonomous is the A flag: "When set indicates that this prefix can be
	// used for stateless address configuration."
	Autonomous bool
	// ValidLifetime and PreferredLifetime are in seconds; 0xffffffff is
	// infinity.
	ValidLifetime     uint32
	PreferredLifetime uint32
	Prefix            netip.Addr
}

func (p PrefixInfo) String() string {
	f := ""
	if p.OnLink {
		f += "L"
	}
	if p.Autonomous {
		f += "A"
	}
	if f == "" {
		f = "-"
	}
	return fmt.Sprintf("%s/%d [%s]", p.Prefix, p.PrefixLen, f)
}

// RouterAdvert is a decoded Router Advertisement, RFC 4861 §4.2 — only the
// fields a DHCPv6 client reads.
type RouterAdvert struct {
	// CurHopLimit is "The default value that should be placed in the Hop Count
	// field of the IP header for outgoing IP packets. A value of zero means
	// unspecified (by this router)."
	CurHopLimit uint8

	// Managed is the M flag, §4.2: "1-bit 'Managed address configuration'
	// flag. When set, it indicates that addresses are available via Dynamic
	// Host Configuration Protocol [DHCPv6]."
	Managed bool

	// Other is the O flag, §4.2: "1-bit 'Other configuration' flag. When set,
	// it indicates that other configuration information is available via
	// DHCPv6."
	//
	// §4.2's note, which is the whole of why this decoder exists: "If neither M
	// nor O flags are set, this indicates that no information is available via
	// DHCPv6." That sentence is what turns "the DHCPv6 server did not answer"
	// into "there is no DHCPv6 server here, and the router said so" — a
	// distinction the 1.9.0 plugin could not make. RFC 9762 §9.1 amends the
	// note to include its P flag; prefix delegation is out of scope (D25) and
	// §7.1 tells such a client to ignore P, so this decoder reads M and O and
	// the two of them decide.
	Other bool

	// RouterLifetime is in seconds. "A Lifetime of 0 indicates that the router
	// is not a default router."
	RouterLifetime uint16

	// ReachableTime and RetransTimer are in MILLISECONDS (§4.2), not seconds
	// like every other duration in this file. Zero means unspecified.
	ReachableTime uint32
	RetransTimer  uint32

	// Prefixes are the Prefix Information options, in wire order.
	Prefixes []PrefixInfo
}

func (r *RouterAdvert) String() string {
	f := ""
	if r.Managed {
		f += "M"
	}
	if r.Other {
		f += "O"
	}
	if f == "" {
		f = "-"
	}
	s := fmt.Sprintf("RA [%s] lifetime=%ds", f, r.RouterLifetime)
	for _, p := range r.Prefixes {
		s += " " + p.String()
	}
	return s
}

// The flag masks. §4.2's octet is drawn "|M|O|  Reserved |" most significant
// bit first, so M is 0x80 and O is 0x40; §4.6.2's is "|L|A| Reserved1 |", so L
// is 0x80 and A is 0x40. RFC 9762 puts R at 0x20 and P at 0x10 in the same
// octet; neither is read.
const (
	raFlagManaged uint8 = 0x80
	raFlagOther   uint8 = 0x40
	pioFlagOnLink uint8 = 0x80
	pioFlagAuto   uint8 = 0x40
)

// DecodeRouterAdvert parses one Router Advertisement, RFC 4861 §4.2.
func DecodeRouterAdvert(b []byte) (*RouterAdvert, error) {
	if len(b) < raFixedLen {
		return nil, fmt.Errorf("%w: Router Advertisement is %d octet(s), want at least %d",
			ErrICMPv6Short, len(b), raFixedLen)
	}
	if b[0] != ICMPv6RouterAdvert {
		return nil, fmt.Errorf("%w: ICMPv6 type %d, want %d", ErrICMPv6Type, b[0], ICMPv6RouterAdvert)
	}
	if err := requireZeroCode(b[1], "Router Advertisement", "6.1.2"); err != nil {
		return nil, err
	}
	ra := &RouterAdvert{
		CurHopLimit:    b[4],
		Managed:        b[5]&raFlagManaged != 0,
		Other:          b[5]&raFlagOther != 0,
		RouterLifetime: ube16(b[6:8]),
		ReachableTime:  ube32(b[8:12]),
		RetransTimer:   ube32(b[12:16]),
	}
	err := walkNDOptions(b[raFixedLen:], func(typ uint8, opt []byte) error {
		if typ != NDOptPrefixInfo {
			return nil
		}
		if len(opt) != pioLen {
			return fmt.Errorf("%w: Prefix Information option is %d octet(s), RFC 4861 section 4.6.2 gives Length 4 (%d octets)",
				ErrNDOption, len(opt), pioLen)
		}
		ra.Prefixes = append(ra.Prefixes, PrefixInfo{
			PrefixLen:         opt[2],
			OnLink:            opt[3]&pioFlagOnLink != 0,
			Autonomous:        opt[3]&pioFlagAuto != 0,
			ValidLifetime:     ube32(opt[4:8]),
			PreferredLifetime: ube32(opt[8:12]),
			Prefix:            netip.AddrFrom16([16]byte(opt[16:32])),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ra, nil
}

// -------------------------------------------------- Neighbor Solicitation --

// EncodeDADNeighborSolicit builds the Neighbor Solicitation RFC 4862 §5.4.2
// sends to check whether target is already in use.
//
// §5.4.2, verbatim: "To check an address, a node sends DupAddrDetectTransmits
// Neighbor Solicitations, each separated by RetransTimer milliseconds. The
// solicitation's Target Address is set to the address being checked, the IP
// source is set to the unspecified address, and the IP destination is set to
// the solicited-node multicast address of the target address."
//
// ALL THREE ARE SET HERE AND NONE IS A PARAMETER. A DAD probe sent from the
// address it is probing for answers its own question: the tentative address is
// already on the link, so the sender is the duplicate. That is the v4 defect
// this project already has a name for — an ARP Probe with a real sender IP —
// arriving at the v6 layer, and the shape that prevents it is the same one:
// the caller cannot supply the field it would get wrong.
//
// No Source Link-Layer Address option, RFC 4861 §4.3: the option "MUST NOT be
// included when the source IP address is the unspecified address".
func EncodeDADNeighborSolicit(target netip.Addr) (ICMPv6Packet, error) {
	if err := requireIPv6(target, "neighbor solicitation target"); err != nil {
		return ICMPv6Packet{}, err
	}
	if target.IsMulticast() {
		return ICMPv6Packet{}, fmt.Errorf("%w: target %s is multicast, and RFC 4861 section 4.3 says the Target Address MUST NOT be a multicast address",
			ErrICMPv6Encode, target)
	}
	dst, err := SolicitedNodeMulticast(target)
	if err != nil {
		return ICMPv6Packet{}, err
	}
	src := netip.IPv6Unspecified()
	body := make([]byte, nsFixedLen)
	body[0] = ICMPv6NeighborSolicit
	t16 := target.As16()
	copy(body[8:24], t16[:])
	be16(body[2:4], ICMPv6Checksum(src, dst, body))
	return ICMPv6Packet{Src: src, Dst: dst, Body: body}, nil
}

// NeighborSolicit is a decoded Neighbor Solicitation, RFC 4861 section 4.3 —
// only the field a duplicate address detection reads.
//
// IT EXISTS FOR RFC 4862 section 5.4.3's DUPLICATE CASE and for nothing else.
// A node running duplicate address detection has to read the solicitations it
// receives, not only the advertisements: "If the source address of the
// Neighbor Solicitation is the unspecified address, the solicitation is from a
// node performing Duplicate Address Detection.  If the solicitation is from
// another node, the tentative address is a duplicate and should not be used
// (by either node)." Without a decoder for the inbound message that arm cannot
// be implemented, and the check then reports a duplicate as free whenever the
// other node also runs DAD rather than answering.
//
// The SOURCE ADDRESS is not in this struct because it is not in the ICMPv6
// message: it is the IPv6 header's, which is ring 3's to read and to pass to
// the same VerifyICMPv6Checksum call.
type NeighborSolicit struct {
	Target netip.Addr
	// HasSourceLinkAddr says whether a Source Link-Layer Address option was
	// present. It is carried because section 7.1.1 makes it a validity
	// condition against the IP source address — "If the IP source address is
	// the unspecified address, there is no source link-layer address option
	// in the message" — and that source address is outside this message.
	HasSourceLinkAddr bool
}

func (n *NeighborSolicit) String() string {
	if n.HasSourceLinkAddr {
		return fmt.Sprintf("NS target=%s +sllao", n.Target)
	}
	return fmt.Sprintf("NS target=%s", n.Target)
}

// DecodeNeighborSolicit parses one Neighbor Solicitation, RFC 4861 section 4.3.
//
// It makes the section 7.1.1 checks whose evidence is inside the ICMPv6
// message — the Code octet, the length, the Target Address, and the option
// walk — and no others. The remaining four are about the IPv6 header (hop
// limit, checksum, and the two conditions on the source address) and are ring
// 3's, exactly as DecodeRouterAdvert's are: ErrICMPv6Validity's doc draws that
// boundary and this decoder sits on the same side of it.
func DecodeNeighborSolicit(b []byte) (*NeighborSolicit, error) {
	if len(b) < nsFixedLen {
		return nil, fmt.Errorf("%w: Neighbor Solicitation is %d octet(s), want at least %d",
			ErrICMPv6Short, len(b), nsFixedLen)
	}
	if b[0] != ICMPv6NeighborSolicit {
		return nil, fmt.Errorf("%w: ICMPv6 type %d, want %d", ErrICMPv6Type, b[0], ICMPv6NeighborSolicit)
	}
	if err := requireZeroCode(b[1], "Neighbor Solicitation", "7.1.1"); err != nil {
		return nil, err
	}
	target := netip.AddrFrom16([16]byte(b[8:24]))
	if target.IsMulticast() {
		return nil, fmt.Errorf("%w: Neighbor Solicitation Target Address %s is multicast, which RFC 4861 section 7.1.1 makes the packet one to silently discard",
			ErrICMPv6Validity, target)
	}
	ns := &NeighborSolicit{Target: target}
	err := walkNDOptions(b[nsFixedLen:], func(typ uint8, _ []byte) error {
		if typ == NDOptSourceLinkAddr {
			ns.HasSourceLinkAddr = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ns, nil
}

// ------------------------------------------------- Neighbor Advertisement --

// NeighborAdvert is a decoded Neighbor Advertisement, RFC 4861 §4.4.
type NeighborAdvert struct {
	// Router is the R flag: "When set, the R-bit indicates that the sender is
	// a router."
	Router bool
	// Solicited is the S flag: "When set, the S-bit indicates that the
	// advertisement was sent in response to a Neighbor Solicitation from the
	// Destination address."
	//
	// A DAD failure is signalled by an advertisement for the tentative address
	// with S CLEAR — RFC 4862 §5.4.4 treats a solicited advertisement as the
	// answer to somebody else's question — so this flag is carried and not
	// folded away.
	Solicited bool
	// Override is the O flag: "When set, the O-bit indicates that the
	// advertisement should override an existing cache entry."
	Override bool
	Target   netip.Addr
}

func (n *NeighborAdvert) String() string {
	f := ""
	for _, p := range []struct {
		set bool
		c   string
	}{{n.Router, "R"}, {n.Solicited, "S"}, {n.Override, "O"}} {
		if p.set {
			f += p.c
		}
	}
	if f == "" {
		f = "-"
	}
	return fmt.Sprintf("NA [%s] target=%s", f, n.Target)
}

// The flag masks of §4.4's "|R|S|O| Reserved |" octet.
const (
	naFlagRouter    uint8 = 0x80
	naFlagSolicited uint8 = 0x40
	naFlagOverride  uint8 = 0x20
)

// DecodeNeighborAdvert parses one Neighbor Advertisement, RFC 4861 §4.4.
func DecodeNeighborAdvert(b []byte) (*NeighborAdvert, error) {
	if len(b) < naFixedLen {
		return nil, fmt.Errorf("%w: Neighbor Advertisement is %d octet(s), want at least %d",
			ErrICMPv6Short, len(b), naFixedLen)
	}
	if b[0] != ICMPv6NeighborAdvert {
		return nil, fmt.Errorf("%w: ICMPv6 type %d, want %d", ErrICMPv6Type, b[0], ICMPv6NeighborAdvert)
	}
	if err := requireZeroCode(b[1], "Neighbor Advertisement", "7.1.2"); err != nil {
		return nil, err
	}
	target := netip.AddrFrom16([16]byte(b[8:24]))
	if target.IsMulticast() {
		// §7.1.2's fifth check, "Target Address is not a multicast address".
		// It is the one on this list that changes what a CONSUMER does rather
		// than only whether the frame parses: a duplicate-address check reads
		// the target to decide whether the advertisement answers its probe, and
		// a multicast target is an answer about nobody.
		return nil, fmt.Errorf("%w: Neighbor Advertisement Target Address %s is multicast, which RFC 4861 section 7.1.2 makes the packet one to silently discard",
			ErrICMPv6Validity, target)
	}
	// The options are walked and discarded: nothing here reads a Target
	// Link-Layer Address, but a packet carrying a malformed option is one
	// §4.6 says to discard, and discarding it here rather than ignoring it is
	// what keeps a zero-length option from being a decode that succeeded.
	if err := walkNDOptions(b[naFixedLen:], func(uint8, []byte) error { return nil }); err != nil {
		return nil, err
	}
	return &NeighborAdvert{
		Router:    b[4]&naFlagRouter != 0,
		Solicited: b[4]&naFlagSolicited != 0,
		Override:  b[4]&naFlagOverride != 0,
		Target:    target,
	}, nil
}

// requireZeroCode is RFC 4861's "ICMP Code is 0", which appears in the
// MUST-silently-discard list of every validation section this package decodes
// for (§6.1.2 for a Router Advertisement, §7.1.2 for a Neighbor
// Advertisement).
//
// IT IS NOT A FORMATTING CHECK. §6.1.2, immediately after the list:
// "backward-incompatible changes may use different Code values." So a non-zero
// Code is a frame written to a protocol this decoder does not implement, and
// reading its flag octet — which is where the M and O bits that decide whether
// DHCPv6 runs at all live — is reading a field whose MEANING the sender has
// told us we do not know. Refusing costs nothing today: no fixture and no
// captured frame on this box carries one.
func requireZeroCode(code uint8, what, section string) error {
	if code == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s ICMP Code is %d, and RFC 4861 section %s says a node MUST silently discard one that is not 0",
		ErrICMPv6Validity, what, code, section)
}

// ------------------------------------------------------------ ND options --

// walkNDOptions walks RFC 4861 §4.6's option area, calling fn with each
// option's type and its WHOLE octets, type and length included.
//
// The zero length is refused rather than skipped, §4.6: "Length ... The length
// of the option (including the type and length fields) in units of 8 octets.
// The value 0 is invalid. Nodes MUST silently discard an ND packet that
// contains an option with length zero." Refusing it is also what stops the walk
// from advancing by nothing and looping forever, which a suite reports as a
// timeout rather than as a failure.
func walkNDOptions(b []byte, fn func(typ uint8, opt []byte) error) error {
	for i := 0; i < len(b); {
		if len(b)-i < 2 {
			return fmt.Errorf("%w: %d octet(s) left at offset %d, an option header is 2",
				ErrNDOption, len(b)-i, i)
		}
		n := int(b[i+1]) * 8
		if n == 0 {
			return fmt.Errorf("%w: option type %d at offset %d declares length 0, which RFC 4861 section 4.6 says makes the whole packet one to discard",
				ErrNDOption, b[i], i)
		}
		if i+n > len(b) {
			return fmt.Errorf("%w: option type %d at offset %d declares %d octet(s), %d remain",
				ErrNDOption, b[i], i, n, len(b)-i)
		}
		if err := fn(b[i], b[i:i+n]); err != nil {
			return err
		}
		i += n
	}
	return nil
}

func encodeLinkAddrOption(typ uint8, hw []byte) ([]byte, error) {
	// §4.6's length is in units of 8 octets, so the two header octets plus the
	// address have to reach a multiple of 8. Ethernet's six make exactly one
	// unit; anything else is refused rather than padded, because a padded
	// address is a different address.
	if len(hw) != int(ARPHLenEthernet) {
		return nil, fmt.Errorf("%w: a link-layer address option needs %d octets, got %d",
			ErrICMPv6Encode, ARPHLenEthernet, len(hw))
	}
	out := make([]byte, 2, 8)
	out[0] = typ
	out[1] = 1
	return append(out, hw...), nil
}

// ------------------------------------------------------- address helpers --

// AllNodesMulticast and AllRoutersMulticast are RFC 4291 §2.7.1's link-scoped
// well-known groups: "All Nodes Addresses: FF01:0:0:0:0:0:0:1 /
// FF02:0:0:0:0:0:0:1" and "All Routers Addresses: FF01:0:0:0:0:0:0:2 /
// FF02:0:0:0:0:0:0:2". Only the link-local scope is used here.
var (
	AllNodesMulticast   = netip.MustParseAddr("ff02::1")
	AllRoutersMulticast = netip.MustParseAddr("ff02::2")
)

// SolicitedNodeMulticast returns RFC 4291 §2.7.1's solicited-node multicast
// address for a: "A Solicited-Node multicast address is formed by taking the
// low-order 24 bits of an address (unicast or anycast) and appending those bits
// to the prefix FF02:0:0:0:0:1:FF00::/104".
func SolicitedNodeMulticast(a netip.Addr) (netip.Addr, error) {
	if err := requireIPv6(a, "solicited-node multicast source"); err != nil {
		return netip.Addr{}, err
	}
	b := a.As16()
	var out [16]byte
	out[0], out[1] = 0xFF, 0x02
	out[11] = 0x01
	out[12] = 0xFF
	out[13], out[14], out[15] = b[13], b[14], b[15]
	return netip.AddrFrom16(out), nil
}

// LinkLocalFromMAC returns the RFC 4291 Appendix A modified EUI-64 link-local
// address of a 48-bit MAC: fe80::/64 with the universal/local bit of the
// interface identifier inverted and fffe inserted in the middle.
//
// FOR FIXTURES AND TESTS ONLY. The product NEVER forms an address: the library
// does not install addresses or routes (the seam design's rule), the DHCPv6
// server hands out the address, and the kernel forms the SLAAC and link-local
// ones. This exists so a netns fixture can predict the address the kernel will
// give an interface it just created, and so this package's own tests can build
// a plausible source address without hard-coding one.
func LinkLocalFromMAC(mac []byte) (netip.Addr, error) {
	if len(mac) != int(ARPHLenEthernet) {
		return netip.Addr{}, fmt.Errorf("%w: a MAC is %d octets, got %d",
			ErrICMPv6Encode, ARPHLenEthernet, len(mac))
	}
	var out [16]byte
	out[0], out[1] = 0xFE, 0x80
	out[8] = mac[0] ^ 0x02
	out[9], out[10] = mac[1], mac[2]
	out[11], out[12] = 0xFF, 0xFE
	out[13], out[14], out[15] = mac[3], mac[4], mac[5]
	return netip.AddrFrom16(out), nil
}

func requireIPv6(a netip.Addr, what string) error {
	if !a.Is6() || a.Is4In6() {
		return fmt.Errorf("%w: %s %s", ErrNotIPv6, what, a)
	}
	return nil
}
