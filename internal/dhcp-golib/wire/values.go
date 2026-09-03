package wire

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// The typed readers live on Options rather than on Message.
//
// A Lease carries its Options and no Message, so a caller asking "what did the
// server send for option 121" had nothing to ask at M2 — the accessors were
// methods on *Message and the message is gone by then. Message keeps the same
// method set and delegates, so nothing that called m.Addr4 has to change.

// Addr4 returns an option's value as a single IPv4 address.
func (o Options) Addr4(c OptionCode) (netip.Addr, bool) {
	v, ok := o[c]
	if !ok || len(v) != 4 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte(v[:4])), true
}

// Addrs4 returns an option's value as a list of IPv4 addresses.
//
// A value whose length is not a multiple of four is rejected outright rather
// than truncated to the whole addresses it contains: a router option with five
// octets is a malformed message, and quietly using the first address in it
// hands the caller a configuration nobody sent.
func (o Options) Addrs4(c OptionCode) ([]netip.Addr, bool) {
	v, ok := o[c]
	if !ok || len(v) == 0 || len(v)%4 != 0 {
		return nil, false
	}
	out := make([]netip.Addr, 0, len(v)/4)
	for i := 0; i+4 <= len(v); i += 4 {
		out = append(out, netip.AddrFrom4([4]byte(v[i:i+4])))
	}
	return out, true
}

// Uint32 returns an option's value as a big-endian uint32.
func (o Options) Uint32(c OptionCode) (uint32, bool) {
	v, ok := o[c]
	if !ok || len(v) != 4 {
		return 0, false
	}
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3]), true
}

// Int32 returns an option's value as a big-endian two's-complement int32. RFC
// 2132 section 3.4's time offset is signed, and reading it through Uint32
// turns every location west of Greenwich into one four billion seconds east.
func (o Options) Int32(c OptionCode) (int32, bool) {
	v, ok := o.Uint32(c)
	if !ok {
		return 0, false
	}
	return int32(v), true
}

// Uint16 returns an option's value as a big-endian uint16.
func (o Options) Uint16(c OptionCode) (uint16, bool) {
	v, ok := o[c]
	if !ok || len(v) != 2 {
		return 0, false
	}
	return uint16(v[0])<<8 | uint16(v[1]), true
}

// Text returns an option's value as text.
//
// A trailing NUL is stripped: some servers NUL-terminate the domain-name and
// host-name options, and a caller that writes the raw bytes into resolv.conf
// writes the NUL with them.
func (o Options) Text(c OptionCode) (string, bool) {
	v, ok := o[c]
	if !ok {
		return "", false
	}
	for len(v) > 0 && v[len(v)-1] == 0 {
		v = v[:len(v)-1]
	}
	return string(v), true
}

// Route is one entry of a routing table the server supplied.
//
// Router is the invalid zero Addr for an ON-LINK route: RFC 3442's "Local
// Subnet Routes" lets a server give 0.0.0.0 as the router for a subnet that
// shares the link. RFC 3442 tells "a DHCP client whose underlying TCP/IP stack
// does not provide this capability" to ignore those; Linux provides it (a
// route with a device and no gateway), so they are carried here and the caller
// decides. Dropping them in the library would be the library deciding on the
// stack's behalf, and it is the sort of silent subtraction this project has
// paid for.
type Route struct {
	Dest   netip.Prefix
	Router netip.Addr
}

// OnLink reports whether this route has no gateway.
func (r Route) OnLink() bool { return !r.Router.IsValid() || r.Router.IsUnspecified() }

func (r Route) String() string {
	if r.OnLink() {
		return r.Dest.String() + " on-link"
	}
	return r.Dest.String() + " via " + r.Router.String()
}

// IsDefault reports whether this route is the default route, 0.0.0.0/0.
func (r Route) IsDefault() bool { return r.Dest.Bits() == 0 && r.Dest.Addr().Is4() }

// ErrMalformedRoutes is returned by ClasslessRoutes and StaticRoutes for a
// value that is not a whole list of routes.
//
// It is returned rather than "the routes that did parse", and that is the
// decision worth stating: a partially decoded routing table is a host that
// works for most destinations and silently cannot reach one. There is no way
// to tell, from inside the option, whether the truncation lost one route or
// twenty.
var ErrMalformedRoutes = errors.New("wire: malformed route option")

// ClasslessRoutes decodes option 121, RFC 3442.
//
// The encoding is a destination descriptor — one octet of mask width, then
// only the significant octets of the subnet number, "the width of the subnet
// mask divided by eight, rounding up" — followed by a four-octet router.
//
// The subnet number is MASKED before it is returned: RFC 3442 requires it
// ("the subnet number installed in the routing table is the logical AND of the
// subnet number and subnet mask given in the Classless Static Routes option"),
// and its own worked example is a destination of 129.210.177.132/25 that must
// install as 129.210.177.128/25.
//
// ok is false with no routes when the option is absent, and
// ErrMalformedRoutes when it is present and does not decode.
func (o Options) ClasslessRoutes() ([]Route, error) {
	v, ok := o[OptClasslessStaticRte]
	if !ok {
		return nil, nil
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("%w: option 121 is empty", ErrMalformedRoutes)
	}
	var out []Route
	for i := 0; i < len(v); {
		width := int(v[i])
		i++
		if width > 32 {
			return nil, fmt.Errorf("%w: option 121 mask width %d exceeds 32", ErrMalformedRoutes, width)
		}
		sig := (width + 7) / 8
		if i+sig+4 > len(v) {
			return nil, fmt.Errorf("%w: option 121 truncated: a /%d route needs %d more octet(s), %d remain",
				ErrMalformedRoutes, width, sig+4, len(v)-i)
		}
		var dst [4]byte
		copy(dst[:], v[i:i+sig])
		i += sig
		router := netip.AddrFrom4([4]byte(v[i : i+4]))
		i += 4
		out = append(out, Route{
			Dest:   netip.PrefixFrom(netip.AddrFrom4(dst), width).Masked(),
			Router: router,
		})
	}
	return out, nil
}

// StaticRoutes decodes option 33, RFC 2132 section 5.8: pairs of four-octet
// destination and four-octet router, with the mask left implicit.
//
// The mask cannot be recovered from the option — that is the whole reason
// RFC 3442 exists — so each destination comes back as a /32. A caller that
// wants classful masks has to invent them, and inventing them in the library
// is how a client ends up with a route it was never given.
func (o Options) StaticRoutes() ([]Route, error) {
	v, ok := o[OptStaticRoute]
	if !ok {
		return nil, nil
	}
	if len(v) == 0 || len(v)%8 != 0 {
		return nil, fmt.Errorf("%w: option 33 is %d octet(s), not a whole number of 8-octet pairs",
			ErrMalformedRoutes, len(v))
	}
	out := make([]Route, 0, len(v)/8)
	for i := 0; i+8 <= len(v); i += 8 {
		out = append(out, Route{
			Dest:   netip.PrefixFrom(netip.AddrFrom4([4]byte(v[i:i+4])), 32),
			Router: netip.AddrFrom4([4]byte(v[i+4 : i+8])),
		})
	}
	return out, nil
}

// ErrMalformedNames is returned by DomainSearch for a value that is not a
// whole list of RFC 1035 names.
var ErrMalformedNames = errors.New("wire: malformed domain-search option")

// maxNameJumps bounds the compression pointers one name may follow.
//
// RFC 3397 borrows RFC 1035 section 4.1.4's compression, and a pointer can
// point anywhere in the option — including at itself. Without a bound, a
// hostile or merely broken server hangs the decoder inside ring 0, which in
// the plugin is the whole daemon. The bound is the number of LABELS a 255-octet
// option could hold, so no legitimate encoding reaches it.
const maxNameJumps = 128

// DomainSearch decodes option 119, RFC 3397.
//
// The names are RFC 1035 section 4.1.4 wire format WITH compression, and the
// pointers are offsets "within the data portion of the DHCP option (not
// including the preceding DHCP option code byte or DHCP option length byte)".
// This library's decoder concatenates multi-instance options before this point
// (see Decode), so the offsets are into one aggregate block, which is what
// RFC 3397 requires.
func (o Options) DomainSearch() ([]string, error) {
	v, ok := o[OptDomainSearch]
	if !ok {
		return nil, nil
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("%w: option 119 is empty", ErrMalformedNames)
	}
	var out []string
	for i := 0; i < len(v); {
		name, next, err := readName(v, i)
		if err != nil {
			return nil, err
		}
		i = next
		if name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// readName reads one possibly-compressed RFC 1035 name starting at off, and
// returns the offset just past it IN THE OUTER BLOCK — which is not where the
// name finished reading, because a pointer sends the reader elsewhere and the
// outer walk must resume after the pointer.
func readName(v []byte, off int) (string, int, error) {
	var labels []string
	next := -1
	jumps := 0
	for {
		if off >= len(v) {
			return "", 0, fmt.Errorf("%w: option 119 ends mid-name", ErrMalformedNames)
		}
		n := int(v[off])
		switch {
		case n == 0:
			off++
			if next < 0 {
				next = off
			}
			return strings.Join(labels, "."), next, nil
		case n&0xC0 == 0xC0:
			if off+1 >= len(v) {
				return "", 0, fmt.Errorf("%w: option 119 ends inside a compression pointer", ErrMalformedNames)
			}
			target := (n&0x3F)<<8 | int(v[off+1])
			if next < 0 {
				next = off + 2
			}
			if target >= len(v) {
				return "", 0, fmt.Errorf("%w: option 119 compression pointer to offset %d, past the %d-octet block",
					ErrMalformedNames, target, len(v))
			}
			jumps++
			if jumps > maxNameJumps {
				return "", 0, fmt.Errorf("%w: option 119 compression pointers loop", ErrMalformedNames)
			}
			off = target
		case n&0xC0 != 0:
			return "", 0, fmt.Errorf("%w: option 119 label length octet %#02x uses a reserved form", ErrMalformedNames, n)
		default:
			if off+1+n > len(v) {
				return "", 0, fmt.Errorf("%w: option 119 label of %d octet(s) runs past the block", ErrMalformedNames, n)
			}
			labels = append(labels, string(v[off+1:off+1+n]))
			off += 1 + n
		}
	}
}

// The RFC 4702 section 2.1 flag bits of option 81. The octet is
// "MBZ MBZ MBZ MBZ N E O S" with S in the least significant bit.
const (
	// FQDNFlagS asks the server to update the A RR as well as the PTR RR.
	FQDNFlagS uint8 = 0x01
	// FQDNFlagO is the server's "I overrode your S bit". "A client MUST set
	// this bit to 0."
	FQDNFlagO uint8 = 0x02
	// FQDNFlagE says the Domain Name field is RFC 1035 canonical wire format.
	// "This encoding SHOULD be used by clients and MUST be supported by
	// servers."
	FQDNFlagE uint8 = 0x04
	// FQDNFlagN asks the server to perform no DNS updates at all. "If the N
	// bit is 1, the S bit MUST be 0."
	FQDNFlagN uint8 = 0x08
	// FQDNFlagMBZ is the reserved nibble. "DHCP clients and servers that send
	// the Client FQDN option MUST clear the MBZ bits".
	FQDNFlagMBZ uint8 = 0xF0
)

// ErrBadFQDNFlags is returned by EncodeFQDN for a flags octet RFC 4702
// forbids a CLIENT to send. It is a distinct error rather than a silent
// correction because each of the three shapes means the caller believes
// something about DNS that is not going to happen.
var ErrBadFQDNFlags = errors.New("wire: option 81 flags a client may not send")

// EncodeFQDN builds option 81's value: flags, the two deprecated RCODE octets,
// then the name (RFC 4702 section 2).
//
// The RCODEs are 0 because "a client SHOULD set these to 0 when sending the
// option" (section 2.2).
//
// The name is encoded per the E bit, which is the caller's to set: 1 is RFC
// 1035 canonical wire format, 0 is the deprecated ASCII form that "a server
// that does not support the deprecated ASCII encoding MUST ignore". Encoding
// canonically while the E bit says ASCII, or the reverse, is a message no
// server can read, so the bit and the encoding are decided in one place here
// rather than by two call sites that have to agree.
//
// A name with a trailing dot is encoded as fully qualified; one without is
// sent as the partial name RFC 4702 explicitly allows ("it MAY send a name
// that is not fully qualified"). In canonical form the difference is the
// terminating root label, so it is not cosmetic.
func EncodeFQDN(flags uint8, name string) ([]byte, error) {
	switch {
	case flags&FQDNFlagO != 0:
		return nil, fmt.Errorf("%w: the O bit is the server's, and RFC 4702 section 2.1 says a client MUST set it to 0", ErrBadFQDNFlags)
	case flags&FQDNFlagN != 0 && flags&FQDNFlagS != 0:
		return nil, fmt.Errorf("%w: N and S are both set, and RFC 4702 section 2.1 says that if the N bit is 1, the S bit MUST be 0", ErrBadFQDNFlags)
	case flags&FQDNFlagMBZ != 0:
		return nil, fmt.Errorf("%w: the MBZ nibble is %#02x, and RFC 4702 section 2.1 says a client MUST clear it", ErrBadFQDNFlags, flags&FQDNFlagMBZ)
	}
	out := []byte{flags, 0, 0}
	if flags&FQDNFlagE == 0 {
		out = append(out, []byte(strings.TrimSuffix(name, "."))...)
	} else {
		body, err := encodeName(name)
		if err != nil {
			return nil, err
		}
		out = append(out, body...)
	}
	// The length bound is on the ASSEMBLED OPTION, not on one branch of it.
	// It sat inside encodeName until review round 1, which left the ASCII
	// form (E clear) unbounded: a long name was accepted by proto.New,
	// refused by Encode on every outgoing message, and reported as a broken
	// transport after the send budget ran out. A value refused here is
	// refused at construction, which is what ErrBadFQDN promises.
	//
	// 255 is what a single option instance carries; the flags octet and the
	// two RCODEs are three of them, so no caller has to know about RFC 3396
	// concatenation to send a hostname.
	if len(out) > 255 {
		return nil, fmt.Errorf("%w: %q makes a %d-octet option 81, over the 255 a single option carries", ErrBadName, name, len(out))
	}
	return out, nil
}

// ErrBadName is returned for a name that cannot be put in RFC 1035 wire form.
var ErrBadName = errors.New("wire: name cannot be encoded in RFC 1035 wire format")

// encodeName writes a name in RFC 1035 section 3.1 wire format, uncompressed.
//
// A trailing dot makes the name fully qualified and adds the root label; its
// absence leaves the name partial and adds none. RFC 4702 section 2.3 turns on
// that distinction — a partial name tells the server the client "knows part of
// the name but does not necessarily know the zone".
func encodeName(name string) ([]byte, error) {
	root := strings.HasSuffix(name, ".")
	trimmed := strings.TrimSuffix(name, ".")
	var out []byte
	if trimmed != "" {
		for _, label := range strings.Split(trimmed, ".") {
			if label == "" {
				return nil, fmt.Errorf("%w: %q has an empty label", ErrBadName, name)
			}
			if len(label) > 63 {
				return nil, fmt.Errorf("%w: %q has a %d-octet label, over RFC 1035's 63", ErrBadName, name, len(label))
			}
			out = append(out, byte(len(label)))
			out = append(out, label...)
		}
	}
	if root {
		out = append(out, 0)
	}
	return out, nil
}
