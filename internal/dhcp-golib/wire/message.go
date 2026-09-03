package wire

import (
	"fmt"
	"net/netip"
)

// OpCode is the BOOTP 'op' field (RFC 2131 section 2).
type OpCode uint8

// BOOTREQUEST is client-to-server; BOOTREPLY is server-to-client.
const (
	BootRequest OpCode = 1
	BootReply   OpCode = 2
)

func (o OpCode) String() string {
	switch o {
	case BootRequest:
		return "BOOTREQUEST"
	case BootReply:
		return "BOOTREPLY"
	default:
		return fmt.Sprintf("op(%d)", uint8(o))
	}
}

// MessageType is the value of option 53 (RFC 2132 section 9.6).
type MessageType uint8

// The DHCP message types. M1 constructs DISCOVER and REQUEST and interprets
// OFFER, ACK and NAK; the rest are named so a decoded message of an
// unimplemented type has a name in a journal entry rather than a number.
const (
	MsgDiscover MessageType = 1
	MsgOffer    MessageType = 2
	MsgRequest  MessageType = 3
	MsgDecline  MessageType = 4
	MsgAck      MessageType = 5
	MsgNak      MessageType = 6
	MsgRelease  MessageType = 7
	MsgInform   MessageType = 8
)

func (m MessageType) String() string {
	switch m {
	case MsgDiscover:
		return "DHCPDISCOVER"
	case MsgOffer:
		return "DHCPOFFER"
	case MsgRequest:
		return "DHCPREQUEST"
	case MsgDecline:
		return "DHCPDECLINE"
	case MsgAck:
		return "DHCPACK"
	case MsgNak:
		return "DHCPNAK"
	case MsgRelease:
		return "DHCPRELEASE"
	case MsgInform:
		return "DHCPINFORM"
	default:
		return fmt.Sprintf("msgtype(%d)", uint8(m))
	}
}

// HTypeEthernet is the 'htype' value for 10Mb Ethernet, per the Assigned
// Numbers registry that RFC 2131 section 2 defers to.
const HTypeEthernet uint8 = 1

// FlagBroadcast is the top bit of the 'flags' field (RFC 2131 section 2).
const FlagBroadcast uint16 = 0x8000

// Message is one DHCPv4 message: the fixed BOOTP header plus the options.
//
// Addresses are netip.Addr rather than [4]byte because the rest of the library
// speaks netip and because a message under construction can leave a field
// invalid and have Encode write the all-zero address for it.
//
// The bound, stated rather than left to be discovered: this does NOT survive a
// round trip. Decode always produces a valid Addr, so "the server sent
// 0.0.0.0" and "nobody set this" are the same value on a decoded message. The
// distinction is only available on messages this process built.
type Message struct {
	Op    OpCode
	HType uint8
	// HLen is what arrived on the wire. Encode does NOT write this field: it
	// writes len(CHAddr), so an encoded message cannot claim a hardware
	// address length it does not carry.
	HLen uint8
	Hops uint8

	XID   uint32
	Secs  uint16
	Flags uint16

	CIAddr netip.Addr // client address, when the client already has one
	YIAddr netip.Addr // "your" address — what the server is offering
	SIAddr netip.Addr // next server
	GIAddr netip.Addr // relay agent

	// CHAddr is the client hardware address, at most 16 octets. HLen is
	// written from len(CHAddr) on encode; a CHAddr longer than 16 is a
	// programming error and Encode refuses it rather than truncating.
	CHAddr []byte

	SName []byte // 64 octets on the wire; nil unless the server used it
	File  []byte // 128 octets on the wire

	Options Options
}

// Type returns the message type from option 53.
func (m *Message) Type() (MessageType, bool) {
	v, ok := m.Options[OptMessageType]
	if !ok || len(v) != 1 {
		return 0, false
	}
	return MessageType(v[0]), true
}

// SetType sets option 53.
func (m *Message) SetType(t MessageType) {
	if m.Options == nil {
		m.Options = Options{}
	}
	m.Options[OptMessageType] = []byte{byte(t)}
}

// The typed option readers delegate to Options, which is where they live now:
// a Lease carries its Options and no Message, and a caller asking what the
// server sent had nothing to ask them of. These four are kept as methods so
// every existing call site reads the same.

// Addr4 returns an option's value as a single IPv4 address.
func (m *Message) Addr4(c OptionCode) (netip.Addr, bool) { return m.Options.Addr4(c) }

// Addrs4 returns an option's value as a list of IPv4 addresses.
func (m *Message) Addrs4(c OptionCode) ([]netip.Addr, bool) { return m.Options.Addrs4(c) }

// Uint32 returns an option's value as a big-endian uint32.
func (m *Message) Uint32(c OptionCode) (uint32, bool) { return m.Options.Uint32(c) }

// Uint16 returns an option's value as a big-endian uint16.
func (m *Message) Uint16(c OptionCode) (uint16, bool) { return m.Options.Uint16(c) }

// Text returns an option's value as text, with a trailing NUL stripped.
func (m *Message) Text(c OptionCode) (string, bool) { return m.Options.Text(c) }

// Clone returns a deep copy.
func (m *Message) Clone() *Message {
	if m == nil {
		return nil
	}
	c := *m
	c.CHAddr = append([]byte(nil), m.CHAddr...)
	c.SName = append([]byte(nil), m.SName...)
	c.File = append([]byte(nil), m.File...)
	c.Options = m.Options.Clone()
	return &c
}

// Summary is a one-line rendering for a journal entry or a packet-ring dump.
func (m *Message) Summary() string {
	if m == nil {
		return "<nil>"
	}
	t, ok := m.Type()
	name := "BOOTP"
	if ok {
		name = t.String()
	}
	return fmt.Sprintf("%s xid=%#08x ci=%s yi=%s si=%s opts=%d",
		name, m.XID, addrText(m.CIAddr), addrText(m.YIAddr), addrText(m.SIAddr), len(m.Options))
}

func addrText(a netip.Addr) string {
	if !a.IsValid() {
		return "-"
	}
	return a.String()
}
