package wire

import (
	"errors"
	"fmt"
	"net/netip"
)

// The fixed-format section of a BOOTP/DHCP message, RFC 2131 section 2
// figure 1: 236 octets, then the four-octet magic cookie, then the options.
const (
	fixedLen  = 236
	cookieLen = 4
	// HeaderLen is the number of octets before the first option.
	HeaderLen = fixedLen + cookieLen

	chaddrLen = 16
	snameLen  = 64
	fileLen   = 128

	snameOff = 44
	fileOff  = 108
)

// MinMessageLen is the length a client message is padded to.
//
// RFC 951 fixed a BOOTP packet at 300 octets, and enough deployed relays and
// servers still assume that floor that sending a 250-octet DHCPDISCOVER is a
// real interoperability risk. This is PRACTICE, not a MUST anywhere in RFC
// 2131 — recorded as such so a later reader does not go looking for the
// citation. The padding is PAD options after END, which every conformant
// parser ignores.
const MinMessageLen = 300

var magicCookie = [4]byte{99, 130, 83, 99}

// Decode errors. They are distinct values so a caller can tell a message that
// is not DHCP at all (ErrBadCookie, ErrShort) from one that is DHCP and
// malformed (ErrTruncatedOption, ErrOptionOverrun) — the first is ordinary
// traffic on a shared segment, the second is worth a counter.
var (
	ErrShort           = errors.New("wire: message shorter than the fixed header")
	ErrBadCookie       = errors.New("wire: bad magic cookie")
	ErrTruncatedOption = errors.New("wire: option header runs past the end of the buffer")
	ErrOptionOverrun   = errors.New("wire: option length runs past the end of the buffer")
	ErrCHAddrTooLong   = errors.New("wire: chaddr longer than 16 octets")
	ErrOptionTooLong   = errors.New("wire: option value longer than 255 octets")
)

// Encode renders a message to its wire form.
//
// The output is deterministic: options are written in ascending code order
// (Options.Codes sorts), so the same Message always produces the same bytes.
// That is a requirement rather than a nicety — a golden-bytes test and a
// bit-exact replay both depend on it, and Go map iteration is randomised.
func Encode(m *Message) ([]byte, error) {
	if len(m.CHAddr) > chaddrLen {
		return nil, fmt.Errorf("%w: %d", ErrCHAddrTooLong, len(m.CHAddr))
	}

	buf := make([]byte, HeaderLen, MinMessageLen)
	buf[0] = byte(m.Op)
	buf[1] = m.HType
	buf[2] = uint8(len(m.CHAddr))
	buf[3] = m.Hops
	be32(buf[4:8], m.XID)
	be16(buf[8:10], m.Secs)
	be16(buf[10:12], m.Flags)
	putAddr(buf[12:16], m.CIAddr)
	putAddr(buf[16:20], m.YIAddr)
	putAddr(buf[20:24], m.SIAddr)
	putAddr(buf[24:28], m.GIAddr)
	copy(buf[28:28+chaddrLen], m.CHAddr)
	copy(buf[snameOff:snameOff+snameLen], m.SName)
	copy(buf[fileOff:fileOff+fileLen], m.File)
	copy(buf[fixedLen:HeaderLen], magicCookie[:])

	for _, c := range m.Options.Codes() {
		if c == OptPad || c == OptEnd {
			// PAD and END are structure, not content. A caller that puts
			// them in the map is describing framing the encoder owns; writing
			// them out here would emit a second END and truncate the message
			// for every parser that stops at the first one.
			continue
		}
		v := m.Options[c]
		if len(v) > 255 {
			return nil, fmt.Errorf("%w: %s is %d octets", ErrOptionTooLong, c, len(v))
		}
		buf = append(buf, byte(c), byte(len(v)))
		buf = append(buf, v...)
	}
	buf = append(buf, byte(OptEnd))
	for len(buf) < MinMessageLen {
		buf = append(buf, byte(OptPad))
	}
	return buf, nil
}

// Decode parses a wire-form message.
//
// Repeated option codes are CONCATENATED rather than last-wins: RFC 2131
// section 4.1 says "The client concatenates the values of multiple instances
// of the same option into a single parameter list", and RFC 3396 makes that
// the general rule for options split across instances. Last-wins is the
// obvious implementation and it silently drops half of a long option.
func Decode(b []byte) (*Message, error) {
	if len(b) < HeaderLen {
		return nil, fmt.Errorf("%w: %d octets", ErrShort, len(b))
	}
	if [4]byte(b[fixedLen:HeaderLen]) != magicCookie {
		return nil, ErrBadCookie
	}

	m := &Message{
		Op:      OpCode(b[0]),
		HType:   b[1],
		HLen:    b[2],
		Hops:    b[3],
		XID:     ube32(b[4:8]),
		Secs:    ube16(b[8:10]),
		Flags:   ube16(b[10:12]),
		CIAddr:  getAddr(b[12:16]),
		YIAddr:  getAddr(b[16:20]),
		SIAddr:  getAddr(b[20:24]),
		GIAddr:  getAddr(b[24:28]),
		Options: Options{},
	}
	hlen := int(m.HLen)
	if hlen > chaddrLen {
		hlen = chaddrLen
	}
	m.CHAddr = append([]byte(nil), b[28:28+hlen]...)

	// The 'options' field is parsed first, so that an option-overload option
	// inside it is interpreted before 'file' and 'sname' are looked at, and
	// then 'file', then 'sname'. That order is RFC 2131 section 4.1's, and it
	// is not arbitrary: overload lives in 'options'.
	if err := parseOptions(b[HeaderLen:], m.Options); err != nil {
		return nil, err
	}
	overload := byte(0)
	if v, ok := m.Options[OptOverload]; ok && len(v) == 1 {
		overload = v[0]
	}
	// Overload values: 1 = 'file' holds options, 2 = 'sname', 3 = both.
	if overload&1 != 0 {
		if err := parseOptions(b[fileOff:fileOff+fileLen], m.Options); err != nil {
			return nil, err
		}
	} else {
		m.File = trimNul(b[fileOff : fileOff+fileLen])
	}
	if overload&2 != 0 {
		if err := parseOptions(b[snameOff:snameOff+snameLen], m.Options); err != nil {
			return nil, err
		}
	} else {
		m.SName = trimNul(b[snameOff : snameOff+snameLen])
	}
	return m, nil
}

// parseOptions appends the options found in b into out.
//
// A missing END is tolerated — the loop ends at the end of the buffer — because
// a server that fills the options field exactly has nowhere to put one, and
// refusing such a message would refuse a lease over a framing detail. A
// truncated or overrunning option is NOT tolerated: there is no way to know
// what was cut, and guessing produces a lease built from half an option.
func parseOptions(b []byte, out Options) error {
	for i := 0; i < len(b); {
		c := OptionCode(b[i])
		if c == OptPad {
			i++
			continue
		}
		if c == OptEnd {
			return nil
		}
		if i+1 >= len(b) {
			return fmt.Errorf("%w: %s has no length octet", ErrTruncatedOption, c)
		}
		n := int(b[i+1])
		if i+2+n > len(b) {
			return fmt.Errorf("%w: %s claims %d octets, %d remain", ErrOptionOverrun, c, n, len(b)-i-2)
		}
		out[c] = append(out[c], b[i+2:i+2+n]...)
		i += 2 + n
	}
	return nil
}

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			b = b[:i]
			break
		}
	}
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

func be16(dst []byte, v uint16) { dst[0], dst[1] = byte(v>>8), byte(v) }

func be32(dst []byte, v uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func ube16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func ube32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func putAddr(dst []byte, a netip.Addr) {
	if !a.Is4() {
		dst[0], dst[1], dst[2], dst[3] = 0, 0, 0, 0
		return
	}
	v := a.As4()
	copy(dst, v[:])
}

func getAddr(b []byte) netip.Addr { return netip.AddrFrom4([4]byte(b[:4])) }
