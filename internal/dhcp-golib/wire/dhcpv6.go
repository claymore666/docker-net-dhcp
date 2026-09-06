package wire

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// The DHCPv6 client codec, RFC 9915 (STD 102).
//
// RFC 8415 is OBSOLETE and is cited nowhere in this package. Every section
// number below is RFC 9915's; where the two documents differ the difference is
// named at the line it reaches, because a reader who knows 8415 will otherwise
// read the omission as an oversight.
//
// WHAT THIS SPEAKS, AND WHAT IT REFUSES. Only the client half: the messages a
// client sends (§7.3's SOLICIT, REQUEST, CONFIRM, RENEW, REBIND, RELEASE,
// DECLINE, INFORMATION-REQUEST) and the two it accepts (ADVERTISE, REPLY). A
// Relay-forward, a Relay-reply or a Reconfigure is refused BY NAME rather than
// parsed, because their headers are not this one — a Relay message carries a
// hop-count, a link-address and a peer-address before its options (§9), so a
// decoder that walked its options from offset 4 would read the link address as
// an option code and terminate on whatever it found.
//
// D25 keeps IA_PD, IA_TA, Reconfigure, Rapid Commit and RDNSS out of the 2.0
// line. They are not implemented and not special-cased: an option this codec
// does not name survives decoding as bytes under its numeric code, which is
// what §16 requires of everyone ("Clients, relay agents, and servers MUST NOT
// discard messages that contain unknown options").
//
// The Server Unicast option (§21.12) and the UseMulticast status code (§21.13)
// are OBSOLETE, §16: "The Server Unicast option (see Section 21.12) and
// UseMulticast status code (see Section 21.13) have been obsoleted; hence,
// clients should no longer send messages to a server's unicast address nor
// receive the UseMulticast status code." Neither is implemented; both arrive
// here as an unknown option code and an unknown status code.

// MessageTypeV6 is the 1-octet msg-type of §8.
type MessageTypeV6 uint8

// The message types of §7.3. The client family is 1-9 and 11; 10, 12 and 13
// are named so a refusal can say which one arrived rather than printing a
// number.
const (
	MsgSolicit            MessageTypeV6 = 1
	MsgAdvertise          MessageTypeV6 = 2
	MsgRequest6           MessageTypeV6 = 3
	MsgConfirm            MessageTypeV6 = 4
	MsgRenew              MessageTypeV6 = 5
	MsgRebind             MessageTypeV6 = 6
	MsgReply              MessageTypeV6 = 7
	MsgRelease6           MessageTypeV6 = 8
	MsgDecline6           MessageTypeV6 = 9
	MsgReconfigure        MessageTypeV6 = 10
	MsgInformationRequest MessageTypeV6 = 11
	MsgRelayForw          MessageTypeV6 = 12
	MsgRelayRepl          MessageTypeV6 = 13
)

func (m MessageTypeV6) String() string {
	switch m {
	case MsgSolicit:
		return "SOLICIT"
	case MsgAdvertise:
		return "ADVERTISE"
	case MsgRequest6:
		return "REQUEST"
	case MsgConfirm:
		return "CONFIRM"
	case MsgRenew:
		return "RENEW"
	case MsgRebind:
		return "REBIND"
	case MsgReply:
		return "REPLY"
	case MsgRelease6:
		return "RELEASE"
	case MsgDecline6:
		return "DECLINE"
	case MsgReconfigure:
		return "RECONFIGURE"
	case MsgInformationRequest:
		return "INFORMATION-REQUEST"
	case MsgRelayForw:
		return "RELAY-FORW"
	case MsgRelayRepl:
		return "RELAY-REPL"
	default:
		return fmt.Sprintf("msgtype6(%d)", uint8(m))
	}
}

// ForClient reports whether a message of this type belongs to the client
// exchange this codec speaks: §7.3's types 1-9 and 11.
func (m MessageTypeV6) ForClient() bool {
	return (m >= MsgSolicit && m <= MsgDecline6) || m == MsgInformationRequest
}

// OptionCodeV6 is the 2-octet option-code of §21.1.
type OptionCodeV6 uint16

// The option codes this milestone names. Everything else round-trips as bytes
// under its numeric code.
const (
	OptV6ClientID     OptionCodeV6 = 1  // §21.2
	OptV6ServerID     OptionCodeV6 = 2  // §21.3
	OptV6IANA         OptionCodeV6 = 3  // §21.4
	OptV6IAAddr       OptionCodeV6 = 5  // §21.6
	OptV6ORO          OptionCodeV6 = 6  // §21.7
	OptV6Preference   OptionCodeV6 = 7  // §21.8
	OptV6ElapsedTime  OptionCodeV6 = 8  // §21.9
	OptV6StatusCode   OptionCodeV6 = 13 // §21.13
	OptV6DNSServers   OptionCodeV6 = 23 // RFC 3646 section 3
	OptV6DomainList   OptionCodeV6 = 24 // RFC 3646 section 4
	OptV6InfoRefresh  OptionCodeV6 = 32 // §21.23
	OptV6SolMaxRTCode OptionCodeV6 = 82 // §21.24
	OptV6InfMaxRTCode OptionCodeV6 = 83 // §21.25
)

var optionV6Names = map[OptionCodeV6]string{
	OptV6ClientID:     "client-id",
	OptV6ServerID:     "server-id",
	OptV6IANA:         "ia-na",
	OptV6IAAddr:       "ia-addr",
	OptV6ORO:          "oro",
	OptV6Preference:   "preference",
	OptV6ElapsedTime:  "elapsed-time",
	OptV6StatusCode:   "status-code",
	OptV6DNSServers:   "dns-servers",
	OptV6DomainList:   "domain-list",
	OptV6InfoRefresh:  "info-refresh-time",
	OptV6SolMaxRTCode: "sol-max-rt",
	OptV6InfMaxRTCode: "inf-max-rt",
}

func (c OptionCodeV6) String() string {
	if n, ok := optionV6Names[c]; ok {
		return n
	}
	return fmt.Sprintf("option6(%d)", uint16(c))
}

// The refusals. Distinct values, because a caller counts them separately: a
// message for somebody else is ordinary traffic on a multicast group, a
// malformed one is worth a counter, and an unknown type is §16's "MUST
// discard".
var (
	// ErrV6Short is a datagram shorter than §8's four-octet header.
	ErrV6Short = errors.New("wire: DHCPv6 message shorter than the 4-octet header")
	// ErrV6NotForClient is a Relay-forward, Relay-reply or Reconfigure: a
	// well-formed DHCPv6 message that this client neither sends nor accepts.
	ErrV6NotForClient = errors.New("wire: DHCPv6 message is not one a client sends or accepts")
	// ErrV6UnknownType is §16's "A client or server MUST discard any received
	// DHCP messages with an unknown message type."
	ErrV6UnknownType = errors.New("wire: DHCPv6 message type is not one RFC 9915 defines")
	// ErrV6TruncatedOption is an option header that does not fit.
	ErrV6TruncatedOption = errors.New("wire: DHCPv6 option header runs past the end of the buffer")
	// ErrV6OptionOverrun is an option-len that runs past the buffer.
	ErrV6OptionOverrun = errors.New("wire: DHCPv6 option length runs past the end of the buffer")
	// ErrV6BadOption is an option whose value cannot be the shape its code
	// defines: an IA_NA under twelve octets, an IA Address under twenty-four,
	// a Status Code under two, a DNS server list that is not a multiple of
	// sixteen.
	ErrV6BadOption = errors.New("wire: DHCPv6 option value is not the shape its code defines")
	// ErrV6Encode is a message or option that cannot be encoded.
	ErrV6Encode = errors.New("wire: DHCPv6 message cannot be encoded")
	// ErrV6Name is a domain name in option 24 that RFC 9915 section 10
	// forbids or RFC 1035 section 3.1 does not allow.
	ErrV6Name = errors.New("wire: DHCPv6 domain name is not RFC 1035 section 3.1 uncompressed form")
)

// OptionV6 is one option, in the order it appeared.
type OptionV6 struct {
	Code OptionCodeV6
	Data []byte
}

// OptionsV6 is a message's options IN WIRE ORDER, repeats included.
//
// A slice and not a map, which is the one structural difference from the v4
// codec's Options and is deliberate. RFC 9915 has no analogue of RFC 3396's
// concatenation: §21.4 says "A DHCP message may contain multiple IA_NA options
// (though each must have a unique IAID)", and §21.6 says "More than one IA
// Address option can appear in an IA_NA option", so repetition is the ordinary
// case and not a malformation. A map keyed on the code would have to pick one
// of two Server Identifiers, and picking is a policy this ring does not hold —
// §16.10's "exactly one" is M7b's rule to enforce, on evidence this type
// preserves.
type OptionsV6 []OptionV6

// First returns the first instance of code.
func (o OptionsV6) First(c OptionCodeV6) ([]byte, bool) {
	for _, opt := range o {
		if opt.Code == c {
			return opt.Data, true
		}
	}
	return nil, false
}

// All returns every instance of code, in wire order.
func (o OptionsV6) All(c OptionCodeV6) [][]byte {
	var out [][]byte
	for _, opt := range o {
		if opt.Code == c {
			out = append(out, opt.Data)
		}
	}
	return out
}

// Count returns how many instances of code are present.
func (o OptionsV6) Count(c OptionCodeV6) int {
	n := 0
	for _, opt := range o {
		if opt.Code == c {
			n++
		}
	}
	return n
}

// MaxXID6 is one past the largest §8 transaction-id, which is three octets.
const MaxXID6 uint32 = 1 << 24

// MessageV6 is one decoded client/server DHCPv6 message, §8.
type MessageV6 struct {
	Type MessageTypeV6

	// XID is the 3-octet transaction-id, right-aligned in a uint32. Values at
	// or above MaxXID6 are refused by EncodeV6 rather than truncated: a
	// truncated xid matches nothing, and §16.1 makes the client discard every
	// reply whose xid does not match its own.
	XID uint32

	Options OptionsV6
}

// V6HeaderLen is the number of octets before the first option: msg-type plus
// the 3-octet transaction-id (§8).
const V6HeaderLen = 4

// DecodeV6 parses one client/server DHCPv6 message.
//
// The type is checked BEFORE the options are walked. A Relay-forward's options
// begin at octet 34, not 4 (§9), so walking a relay message from the client
// offset reads its link-address as an option header and stops wherever the
// bytes happen to look terminal — a decode that succeeds and means nothing.
func DecodeV6(b []byte) (*MessageV6, error) {
	if len(b) < V6HeaderLen {
		return nil, fmt.Errorf("%w: %d octet(s)", ErrV6Short, len(b))
	}
	t := MessageTypeV6(b[0])
	switch {
	case t.ForClient():
	case t == MsgReconfigure || t == MsgRelayForw || t == MsgRelayRepl:
		return nil, fmt.Errorf("%w: %s", ErrV6NotForClient, t)
	default:
		return nil, fmt.Errorf("%w: %d", ErrV6UnknownType, uint8(b[0]))
	}
	opts, err := ParseOptionsV6(b[V6HeaderLen:])
	if err != nil {
		return nil, err
	}
	return &MessageV6{
		Type:    t,
		XID:     uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]),
		Options: opts,
	}, nil
}

// EncodeV6 renders a message to its wire form.
//
// The options go out in the order the slice holds them, so the same MessageV6
// always produces the same bytes without a sort — §8 imposes no order and the
// v4 codec's ascending-code rule exists only because a Go map has none.
func EncodeV6(m *MessageV6) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil message", ErrV6Encode)
	}
	if m.XID >= MaxXID6 {
		return nil, fmt.Errorf("%w: transaction-id %#x does not fit 3 octets", ErrV6Encode, m.XID)
	}
	body, err := EncodeOptionsV6(m.Options)
	if err != nil {
		return nil, err
	}
	out := make([]byte, V6HeaderLen, V6HeaderLen+len(body))
	out[0] = byte(m.Type)
	out[1] = byte(m.XID >> 16)
	out[2] = byte(m.XID >> 8)
	out[3] = byte(m.XID)
	return append(out, body...), nil
}

// ParseOptionsV6 walks an options area: §21.1's 2-octet code, 2-octet length,
// value, repeated, with no padding and no terminator.
//
// A zero-length option is LEGAL and is kept with an empty value; §21.1 puts no
// floor on option-len and several options defined elsewhere carry none. What is
// refused is a header that does not fit and a length that runs past the end.
func ParseOptionsV6(b []byte) (OptionsV6, error) {
	var out OptionsV6
	for i := 0; i < len(b); {
		if len(b)-i < 4 {
			return nil, fmt.Errorf("%w: %d octet(s) left at offset %d", ErrV6TruncatedOption, len(b)-i, i)
		}
		code := OptionCodeV6(ube16(b[i : i+2]))
		n := int(ube16(b[i+2 : i+4]))
		i += 4
		if n > len(b)-i {
			return nil, fmt.Errorf("%w: option %s declares %d octet(s), %d remain",
				ErrV6OptionOverrun, code, n, len(b)-i)
		}
		out = append(out, OptionV6{Code: code, Data: append([]byte(nil), b[i:i+n]...)})
		i += n
	}
	return out, nil
}

// EncodeOptionsV6 renders an options area.
func EncodeOptionsV6(o OptionsV6) ([]byte, error) {
	var out []byte
	for _, opt := range o {
		if len(opt.Data) > 0xFFFF {
			return nil, fmt.Errorf("%w: option %s is %d octet(s), the length field holds 2",
				ErrV6Encode, opt.Code, len(opt.Data))
		}
		var hdr [4]byte
		be16(hdr[0:2], uint16(opt.Code))
		be16(hdr[2:4], uint16(len(opt.Data)))
		out = append(out, hdr[:]...)
		out = append(out, opt.Data...)
	}
	return out, nil
}

// ------------------------------------------------------------------ IA_NA --

// IANA is a decoded Identity Association for Non-temporary Addresses, §21.4.
type IANA struct {
	IAID uint32
	// T1 and T2 are §21.4's two renewal times, in seconds. 0 means "the
	// client decides" and 0xffffffff means infinity (§7.7); neither is
	// interpreted here.
	T1, T2 uint32
	// Options is the IA_NA-options field: the IA Address options and any
	// Status Code scoped to this IA.
	Options OptionsV6
}

// IANAFixedLen is §21.4's "12 + length of IA_NA-options field".
const IANAFixedLen = 12

// DecodeIANA parses one IA_NA option value.
func DecodeIANA(v []byte) (*IANA, error) {
	if len(v) < IANAFixedLen {
		return nil, fmt.Errorf("%w: IA_NA is %d octet(s), want at least %d",
			ErrV6BadOption, len(v), IANAFixedLen)
	}
	opts, err := ParseOptionsV6(v[IANAFixedLen:])
	if err != nil {
		return nil, err
	}
	return &IANA{
		IAID:    ube32(v[0:4]),
		T1:      ube32(v[4:8]),
		T2:      ube32(v[8:12]),
		Options: opts,
	}, nil
}

// EncodeIANA renders one IA_NA option value.
func EncodeIANA(ia *IANA) ([]byte, error) {
	if ia == nil {
		return nil, fmt.Errorf("%w: nil IA_NA", ErrV6Encode)
	}
	body, err := EncodeOptionsV6(ia.Options)
	if err != nil {
		return nil, err
	}
	out := make([]byte, IANAFixedLen, IANAFixedLen+len(body))
	be32(out[0:4], ia.IAID)
	be32(out[4:8], ia.T1)
	be32(out[8:12], ia.T2)
	return append(out, body...), nil
}

// IANAs returns every IA_NA in the options area, decoded.
func (o OptionsV6) IANAs() ([]*IANA, error) {
	var out []*IANA
	for _, v := range o.All(OptV6IANA) {
		ia, err := DecodeIANA(v)
		if err != nil {
			return nil, err
		}
		out = append(out, ia)
	}
	return out, nil
}

// ------------------------------------------------------------- IA Address --

// IAAddr is a decoded IA Address option, §21.6.
type IAAddr struct {
	Addr netip.Addr
	// PreferredLifetime and ValidLifetime are §21.6's two lifetimes, in
	// seconds. 0xffffffff is infinity (§7.7).
	PreferredLifetime uint32
	ValidLifetime     uint32
	// Options is the IAaddr-options field, which for this milestone means a
	// Status Code scoped to this address (§21.4: "The status of any operations
	// involving this IA_NA is indicated in a Status Code option").
	Options OptionsV6
}

// IAAddrFixedLen is §21.6's "24 + length of IAaddr-options field".
const IAAddrFixedLen = 24

// DecodeIAAddr parses one IA Address option value.
//
// THE §21.6 DISCARD RULE IS NOT APPLIED HERE. §21.6: "The client MUST discard
// any addresses for which the preferred lifetime is greater than the valid
// lifetime." That is a decision about what to do with a well-formed option, and
// ring 0 holds no policy: an address dropped by the decoder cannot be counted,
// journalled or Declined, and the operator asking why the lease failed would
// find nothing anywhere. So both values are exposed, Valid reports the
// predicate, and M7b's machine performs the discard.
func DecodeIAAddr(v []byte) (*IAAddr, error) {
	if len(v) < IAAddrFixedLen {
		return nil, fmt.Errorf("%w: IA Address is %d octet(s), want at least %d",
			ErrV6BadOption, len(v), IAAddrFixedLen)
	}
	opts, err := ParseOptionsV6(v[IAAddrFixedLen:])
	if err != nil {
		return nil, err
	}
	return &IAAddr{
		Addr:              netip.AddrFrom16([16]byte(v[0:16])),
		PreferredLifetime: ube32(v[16:20]),
		ValidLifetime:     ube32(v[20:24]),
		Options:           opts,
	}, nil
}

// EncodeIAAddr renders one IA Address option value.
func EncodeIAAddr(a *IAAddr) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil IA Address", ErrV6Encode)
	}
	if !a.Addr.Is6() || a.Addr.Is4In6() {
		return nil, fmt.Errorf("%w: IA Address %s is not an IPv6 address", ErrV6Encode, a.Addr)
	}
	body, err := EncodeOptionsV6(a.Options)
	if err != nil {
		return nil, err
	}
	out := make([]byte, IAAddrFixedLen, IAAddrFixedLen+len(body))
	b16 := a.Addr.As16()
	copy(out[0:16], b16[:])
	be32(out[16:20], a.PreferredLifetime)
	be32(out[20:24], a.ValidLifetime)
	return append(out, body...), nil
}

// Valid reports §21.6's usability predicate: an address whose preferred
// lifetime exceeds its valid lifetime is one "the client MUST discard".
func (a *IAAddr) Valid() bool { return a.PreferredLifetime <= a.ValidLifetime }

// Addrs returns every IA Address in the options area, decoded.
//
// ITS DOMAIN IS AN IA_NA's OPTIONS FIELD, NOT A MESSAGE's. §21.6, of the IA
// Address option: "In this document, it is only specified to be encapsulated
// within an IA_NA." A caller that reaches this method through
// MessageV6.Options is reading the TOP-LEVEL area, where an IA Address is a
// misplaced option: it arrives with no IAID and no T1/T2, so an address taken
// from there is one nothing can renew, rebind or release. Ring 0 decodes it
// anyway — §16 forbids discarding a whole message over a misplaced option, and
// the caller is the only one that can say whether it wanted the top level —
// and ring 1 (proto.Machine6) reads addresses ONLY through IANAs(), journals
// what it found at the top level, and never binds it.
//
// The other half of §21.6, preferred greater than valid, is NOT enforced here
// either: it is IAAddr.Valid(), for the reason that comment gives.
func (o OptionsV6) Addrs() ([]*IAAddr, error) {
	var out []*IAAddr
	for _, v := range o.All(OptV6IAAddr) {
		a, err := DecodeIAAddr(v)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// ------------------------------------------------------------ Status Code --

// StatusCode is §21.13's 2-octet status-code.
type StatusCode uint16

// The status codes a client acts on. UseMulticast (5) is ABSENT ON PURPOSE:
// §16 obsoletes it, so it arrives here as a code with no name, which is what
// this codec does with every code IANA adds after today.
//
// NoPrefixAvail (6) is absent for the other reason: prefix delegation is
// v2.2's (D25), so a client that never sends an IA_PD cannot be told there are
// none.
const (
	StatusSuccess      StatusCode = 0
	StatusUnspecFail   StatusCode = 1
	StatusNoAddrsAvail StatusCode = 2
	StatusNoBinding    StatusCode = 3
	StatusNotOnLink    StatusCode = 4
)

// StatusMalformed is what Status() returns beside ErrV6BadOption, and it is
// NOT a code RFC 9915 defines.
//
// It exists because the value on the error path used to be Status{}, which is
// byte-identical to Success — defeat row A-4's own defect shape surviving in
// the value, and the M7a review's finding 4. A caller that read the value and
// forgot the error was told the server had said Success; a client that acted
// on that would bind an address a NoAddrsAvail Reply never offered. Now the
// forgetful caller gets a code that matches nothing it can branch on and
// renders as "malformed".
//
// 0xffff rather than the next free small integer, because IANA allocates
// status codes upward from 0 and a sentinel in that range becomes somebody
// else's code the day it is registered.
const StatusMalformed StatusCode = 0xFFFF

func (s StatusCode) String() string {
	switch s {
	case StatusMalformed:
		return "malformed"
	case StatusSuccess:
		return "Success"
	case StatusUnspecFail:
		return "UnspecFail"
	case StatusNoAddrsAvail:
		return "NoAddrsAvail"
	case StatusNoBinding:
		return "NoBinding"
	case StatusNotOnLink:
		return "NotOnLink"
	default:
		return fmt.Sprintf("status(%d)", uint16(s))
	}
}

// Status is a decoded Status Code option, §21.13.
type Status struct {
	Code StatusCode
	// Message is §21.13's status-message: "A UTF-8 encoded [RFC3629] text
	// string suitable for display to an end user. MUST NOT be NUL-terminated."
	// It is carried as it arrived and never parsed.
	Message string
}

func (s Status) String() string {
	if s.Message == "" {
		return s.Code.String()
	}
	return s.Code.String() + ": " + s.Message
}

// DecodeStatus parses one Status Code option value.
func DecodeStatus(v []byte) (Status, error) {
	if len(v) < 2 {
		return Status{}, fmt.Errorf("%w: Status Code is %d octet(s), want at least 2",
			ErrV6BadOption, len(v))
	}
	return Status{Code: StatusCode(ube16(v[0:2])), Message: string(v[2:])}, nil
}

// EncodeStatus renders one Status Code option value.
func EncodeStatus(s Status) []byte {
	out := make([]byte, 2, 2+len(s.Message))
	be16(out[0:2], uint16(s.Code))
	return append(out, s.Message...)
}

// Status returns the Status Code option scoped to this options area.
//
// THE THIRD RETURN IS NOT DECORATION. §7.5: "If the Status Code option (see
// Section 21.13) does not appear in a message in which the option could appear,
// the status of the message is assumed to be Success." Absent and
// present-saying-Success are therefore the same VERDICT and different FACTS,
// and an accessor that returned only a StatusCode would report a malformed
// option as Success too. The boolean separates absent from present; the error
// separates present-and-malformed from both.
func (o OptionsV6) Status() (Status, bool, error) {
	v, ok := o.First(OptV6StatusCode)
	if !ok {
		return Status{Code: StatusSuccess}, false, nil
	}
	s, err := DecodeStatus(v)
	if err != nil {
		return Status{Code: StatusMalformed}, true, err
	}
	return s, true, nil
}

// Preference returns §21.8's one-octet pref-value, and whether the option was
// there.
//
// §21.8: "pref-value: The preference value for the server in this message.
// Allowed values are from 0 (least) to 255 (most preferred). Absence of option
// means preference 0." So the zero this returns when the option is absent is
// the RFC's own answer and not a fallback — but the boolean is still here,
// because §18.2.9's rule for 255 is that "the client immediately begins a
// client-initiated message exchange (as described in Section 18.2.2) by
// sending a Request message to the server from which the Advertise message was
// received", and a caller that could not tell absence from an explicit
// 0 could not report which Advertise carried a preference at all.
//
// A length other than one is refused rather than read from the first octet:
// §21.8 gives "option-len: 1", so anything else is a server that did not build
// this option, and its first octet is not a preference.
func (o OptionsV6) Preference() (uint8, bool, error) {
	v, ok := o.First(OptV6Preference)
	if !ok {
		return 0, false, nil
	}
	if len(v) != 1 {
		return 0, true, fmt.Errorf("%w: Preference is %d octet(s), §21.8 says 1", ErrV6BadOption, len(v))
	}
	return v[0], true, nil
}

// ----------------------------------------------------------- Elapsed Time --

// MaxElapsedHundredths is §21.9's saturation value: "The client uses the value
// 0xffff to represent any elapsed-time values greater than the largest time
// value that can be represented in the Elapsed Time option."
const MaxElapsedHundredths uint16 = 0xFFFF

// ElapsedHundredths converts an elapsed time in hundredths of a second to
// §21.9's 16-bit field, SATURATING rather than wrapping.
//
// The wrap is the failure this exists to make unconstructible. §21.9's field is
// two octets of hundredths, so it runs out at 655.36 seconds — well inside a
// Solicit exchange, where SOL_MAX_RT is 3600 seconds (§7.6). A client that
// wrapped would tell the server it had just started trying, which is the exact
// input §21.9 says servers use as policy ("the Elapsed Time option allows a
// secondary DHCP server to respond to a request when a primary server has not
// answered in a reasonable time").
func ElapsedHundredths(h uint64) uint16 {
	if h > uint64(MaxElapsedHundredths) {
		return MaxElapsedHundredths
	}
	return uint16(h)
}

// ElapsedTime returns the Elapsed Time option's value in hundredths of a
// second.
func (o OptionsV6) ElapsedTime() (uint16, bool, error) {
	v, ok := o.First(OptV6ElapsedTime)
	if !ok {
		return 0, false, nil
	}
	if len(v) != 2 {
		return 0, true, fmt.Errorf("%w: Elapsed Time is %d octet(s), §21.9 says 2", ErrV6BadOption, len(v))
	}
	return ube16(v), true, nil
}

// ----------------------------------------------- fixed-width option values --

// Uint32V6 returns a 4-octet option value: Information Refresh Time (§21.23),
// SOL_MAX_RT (§21.24) and INF_MAX_RT (§21.25) all have this shape.
func (o OptionsV6) Uint32V6(c OptionCodeV6) (uint32, bool, error) {
	v, ok := o.First(c)
	if !ok {
		return 0, false, nil
	}
	if len(v) != 4 {
		return 0, true, fmt.Errorf("%w: option %s is %d octet(s), want 4", ErrV6BadOption, c, len(v))
	}
	return ube32(v), true, nil
}

// DNSServers returns option 23's list, RFC 3646 section 3.
//
// A length that is not a multiple of sixteen is refused rather than truncated:
// RFC 3646 says "option-len: Length of the list of DNS recursive name servers
// in octets; must be a multiple of 16", so the remainder is not a short address
// but evidence that the offsets are wrong, and every address read from those
// offsets would be wrong with it.
//
// EVERY instance of the option is walked, not the first. RFC 3646 defines one
// option carrying a list and says nothing about a repeat, so a server that
// sends two is outside the text either way — and of the two readings available
// to a decoder, "the union, in wire order" hides nothing while "the first" is a
// silent deletion of half the resolvers. The raw options are still there for a
// caller that wants to judge the repeat itself: All(OptV6DNSServers).
func (o OptionsV6) DNSServers() ([]netip.Addr, error) {
	var out []netip.Addr
	for _, v := range o.All(OptV6DNSServers) {
		if len(v)%16 != 0 {
			return nil, fmt.Errorf("%w: option 23 is %d octet(s), not a multiple of 16", ErrV6BadOption, len(v))
		}
		for i := 0; i+16 <= len(v); i += 16 {
			out = append(out, netip.AddrFrom16([16]byte(v[i:i+16])))
		}
	}
	return out, nil
}

// DomainSearch returns option 24's list, RFC 3646 section 4.
//
// COMPRESSION IS A DECODE ERROR, not a name to follow. RFC 9915 §10: "So that
// domain names may be encoded uniformly, a domain name or a list of domain
// names is encoded using the technique described in Section 3.1 of [RFC1035].
// The message compression scheme in Section 4.1.4 of [RFC1035] MUST NOT be
// used." The v4 codec's reader in values.go DOES follow pointers, because RFC
// 3397 permits them in option 119; calling it here would accept a message this
// standard forbids and would resolve its pointers against an offset base that
// does not exist in DHCPv6, where the option value is not the whole message.
// Every instance of the option is walked, for the reason DNSServers gives.
func (o OptionsV6) DomainSearch() ([]string, error) {
	var out []string
	for _, v := range o.All(OptV6DomainList) {
		for i := 0; i < len(v); {
			name, next, err := readNameUncompressed(v, i)
			if err != nil {
				return nil, err
			}
			i = next
			if name != "" {
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// readNameUncompressed reads one RFC 1035 section 3.1 name and returns the
// offset just past it. It has no jump budget and no pointer target resolution
// because it refuses the pointer form outright, which is also why it cannot
// loop.
func readNameUncompressed(v []byte, off int) (string, int, error) {
	var labels []string
	for {
		if off >= len(v) {
			return "", 0, fmt.Errorf("%w: option 24 ends mid-name", ErrV6Name)
		}
		n := int(v[off])
		switch {
		case n == 0:
			return strings.Join(labels, "."), off + 1, nil
		case n&0xC0 == 0xC0:
			return "", 0, fmt.Errorf("%w: option 24 carries an RFC 1035 section 4.1.4 compression pointer at offset %d, which RFC 9915 section 10 says MUST NOT be used",
				ErrV6Name, off)
		case n&0xC0 != 0:
			return "", 0, fmt.Errorf("%w: option 24 label length octet %#02x uses a reserved form", ErrV6Name, n)
		default:
			if off+1+n > len(v) {
				return "", 0, fmt.Errorf("%w: option 24 label of %d octet(s) runs past the block", ErrV6Name, n)
			}
			labels = append(labels, string(v[off+1:off+1+n]))
			off += 1 + n
		}
	}
}

// EncodeDomainSearch renders option 24's value in RFC 1035 section 3.1 form,
// uncompressed.
func EncodeDomainSearch(names []string) ([]byte, error) {
	var out []byte
	for _, n := range names {
		enc, err := encodeNameUncompressed(n)
		if err != nil {
			return nil, err
		}
		out = append(out, enc...)
	}
	return out, nil
}

// MaxNameLen is RFC 1035 section 2.3.4's "names 255 octets or less", counted in
// the wire form: every label's length octet, every label, and the root label.
//
// It is enforced on the ENCODE side only. A name longer than this cannot be
// represented in DNS and must not leave here; one that ARRIVES over-long is
// still unambiguously readable, and ring 0 hands the state machine what the
// server actually said rather than deciding on its behalf that it said nothing.
const MaxNameLen = 255

func encodeNameUncompressed(name string) ([]byte, error) {
	// A trailing dot is the ordinary fully-qualified spelling and means the
	// same name; it is normalised away rather than refused. An EMPTY name is
	// not the same thing: it encodes to a lone root label, which is a search
	// list entry that decodes back to nothing, so a caller that asks for one
	// has a bug and is told.
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return nil, fmt.Errorf("%w: the empty name encodes to a lone root label, which is not a search list entry", ErrV6Name)
	}
	var out []byte
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return nil, fmt.Errorf("%w: %q has an empty label", ErrV6Name, name)
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("%w: label %q is %d octet(s), RFC 1035 section 3.1 allows 63", ErrV6Name, label, len(label))
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	out = append(out, 0)
	if len(out) > MaxNameLen {
		return nil, fmt.Errorf("%w: %q is %d octet(s) on the wire, RFC 1035 section 2.3.4 allows %d", ErrV6Name, name, len(out), MaxNameLen)
	}
	return out, nil
}

// ------------------------------------------------------------------- DUID --

// DUIDType is the 2-octet type code that opens every DUID, §11.1.
type DUIDType uint16

// The two DUID forms this library constructs. DUID-LLT (1) and DUID-EN (2) are
// not constructed: LLT needs persistent storage for the time it embeds, which
// a container endpoint does not have, and EN needs an enterprise number.
const (
	DUIDTypeLL   DUIDType = 3 // §11.4
	DUIDTypeUUID DUIDType = 4 // §11.5
)

// UUIDLen is §11.5's "16 octets containing a 128-bit UUID".
const UUIDLen = 16

// ErrDUID is a DUID that cannot be constructed.
var ErrDUID = errors.New("wire: DUID cannot be constructed")

// DUIDLL builds a DUID-LL from a hardware type and a link-layer address,
// §11.4: "2 octets containing a DUID type of 3 and a 2-octet network hardware
// type code, followed by the link-layer address".
//
// This is the PRIMARY constructor for this project (D30/Q4): the bridge and
// macvlan identity is derived from the endpoint's MAC, exactly as v4's option
// 61 is, so a container that restarts on the same MAC gets the same lease.
// DUID-UUID is the ipvlan path, where several endpoints share the parent's
// hardware address and a MAC-derived identity would collide.
//
// WHAT THIS DOES NOT PROMISE. §11 makes the DUID opaque to everybody who reads
// it — "Clients and servers MUST treat DUIDs as opaque values and MUST only
// compare DUIDs for equality" — so this is a way to MINT a stable identifier
// from something stable, not a claim that a reader can recover the MAC from it.
// The message type carries bytes (D10: identity is the caller's).
func DUIDLL(hwType uint16, lladdr []byte) ([]byte, error) {
	if len(lladdr) == 0 {
		return nil, fmt.Errorf("%w: DUID-LL needs a link-layer address", ErrDUID)
	}
	out := make([]byte, 4, 4+len(lladdr))
	be16(out[0:2], uint16(DUIDTypeLL))
	be16(out[2:4], hwType)
	return append(out, lladdr...), nil
}

// DUIDUUID builds a DUID-UUID, §11.5: "2 octets containing a DUID type of 4"
// and a 16-octet UUID.
//
// The length is refused rather than padded or truncated. A DUID is compared for
// equality and nothing else (§11), so a 15-octet "UUID" silently zero-extended
// would be a different identity from the one the caller meant, on every future
// boot, with no symptom but a lease that never comes back to the same endpoint.
func DUIDUUID(uuid []byte) ([]byte, error) {
	if len(uuid) != UUIDLen {
		return nil, fmt.Errorf("%w: DUID-UUID needs %d octets, got %d", ErrDUID, UUIDLen, len(uuid))
	}
	out := make([]byte, 2, 2+UUIDLen)
	be16(out[0:2], uint16(DUIDTypeUUID))
	return append(out, uuid...), nil
}

// ------------------------------------------------------------------ ports --

// The UDP ports of §7.2: "Clients MUST listen for DHCP messages on UDP port
// 546. Servers and relay agents MUST listen for DHCP messages on UDP port 547."
const (
	PortV6Client = 546
	PortV6Server = 547
)

// AllDHCPRelayAgentsAndServers is §7.1's ff02::1:2, "A link-scoped multicast
// address used by a client to communicate with neighboring (i.e., on-link)
// relay agents and servers."
//
// It is the ONLY destination this client has. §16 obsoletes the Server Unicast
// option, so there is no path by which a server can move a conformant client
// off this address.
var AllDHCPRelayAgentsAndServers = netip.MustParseAddr("ff02::1:2")

// Summary renders a message for a journal line.
func (m *MessageV6) Summary() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s xid=%06x", m.Type, m.XID))
	for _, o := range m.Options {
		b.WriteString(" ")
		b.WriteString(o.Code.String())
	}
	return b.String()
}
