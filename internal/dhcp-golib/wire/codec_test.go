package wire

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"
)

// discoverFixture is the message every encode test below starts from. It is a
// realistic DHCPDISCOVER: broadcast flag set, no addresses, a client
// identifier and a parameter request list.
func discoverFixture() *Message {
	m := &Message{
		Op:     BootRequest,
		HType:  HTypeEthernet,
		XID:    0xDEADBEEF,
		Secs:   4,
		Flags:  FlagBroadcast,
		CHAddr: []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x02},
		Options: Options{
			OptClientID:      {0x01, 0x02, 0x42, 0xAC, 0x11, 0x00, 0x02},
			OptParameterList: {byte(OptSubnetMask), byte(OptRouter), byte(OptDNSServer)},
		},
	}
	m.SetType(MsgDiscover)
	return m
}

// buildDiscoverGolden lays out the expected wire form BY OFFSET, from RFC 2131
// section 2's field table, without calling Encode.
//
// That independence is the whole point. A golden built by running the encoder
// and pasting its output is a change detector, not a conformance check: it
// agrees with whatever the encoder does today, including whatever it does
// wrong. This one disagrees with the encoder the moment the encoder disagrees
// with the RFC's layout.
func buildDiscoverGolden() []byte {
	b := make([]byte, 300)
	b[0] = 1                                     // op = BOOTREQUEST
	b[1] = 1                                     // htype = Ethernet
	b[2] = 6                                     // hlen
	b[3] = 0                                     // hops
	copy(b[4:8], []byte{0xDE, 0xAD, 0xBE, 0xEF}) // xid
	copy(b[8:10], []byte{0x00, 0x04})            // secs
	copy(b[10:12], []byte{0x80, 0x00})           // flags: BROADCAST
	// ciaddr, yiaddr, siaddr, giaddr are all zero: b[12:28].
	copy(b[28:34], []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x02}) // chaddr
	// sname b[44:108] and file b[108:236] are zero.
	copy(b[236:240], []byte{99, 130, 83, 99}) // magic cookie

	// Options, ascending by code — 53, 55, 61 — then END, then PAD to 300.
	o := b[240:240]
	o = append(o, 53, 1, 1)                                        // message type = DHCPDISCOVER
	o = append(o, 55, 3, 1, 3, 6)                                  // parameter request list
	o = append(o, 61, 7, 0x01, 0x02, 0x42, 0xAC, 0x11, 0x00, 0x02) // client identifier
	o = append(o, 255)                                             // END
	copy(b[240:], o)
	return b
}

func TestEncodeGoldenBytes(t *testing.T) {
	got, err := Encode(discoverFixture())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := buildDiscoverGolden()
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded form differs from the RFC 2131 section 2 layout\n got %d octets: %x\nwant %d octets: %x", len(got), got, len(want), want)
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	// Go randomises map iteration, so an encoder that walked the map directly
	// would pass a single-run golden test and fail intermittently in replay.
	// One run cannot see that; this asserts over many.
	first, err := Encode(discoverFixture())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for i := 0; i < 200; i++ {
		again, err := Encode(discoverFixture())
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("iteration %d produced different bytes\nfirst: %x\nagain: %x", i, first, again)
		}
	}
}

func TestEncodePadsToBootpFloor(t *testing.T) {
	m := &Message{Op: BootRequest, HType: HTypeEthernet, CHAddr: []byte{1, 2, 3, 4, 5, 6}}
	m.SetType(MsgDiscover)
	b, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(b) != MinMessageLen {
		t.Fatalf("len = %d, want the %d-octet BOOTP floor", len(b), MinMessageLen)
	}
	// Everything after END must be PAD, not leftover.
	end := bytes.IndexByte(b[HeaderLen:], byte(OptEnd))
	if end < 0 {
		t.Fatalf("no END option in %x", b)
	}
	for i, c := range b[HeaderLen+end+1:] {
		if c != byte(OptPad) {
			t.Fatalf("octet %d after END is %#x, want PAD", i, c)
		}
	}
}

func TestEncodeDropsPadAndEndFromTheMap(t *testing.T) {
	// A caller that puts END in the options map is describing framing the
	// encoder owns. Emitting it would truncate the message for every parser
	// that stops at the first END — and the options after it would vanish
	// while the encode still succeeded.
	m := discoverFixture()
	m.Options[OptPad] = nil
	m.Options[OptEnd] = nil
	b, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := buildDiscoverGolden()
	if !bytes.Equal(b, want) {
		t.Fatalf("PAD/END in the map changed the output\n got: %x\nwant: %x", b, want)
	}
	// And the round trip still finds every real option.
	back, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, ok := back.Options[OptClientID]; !ok {
		t.Fatal("client identifier was lost, which is what a spurious END does")
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  *Message
	}{
		{"discover", discoverFixture()},
		{
			"offer",
			&Message{
				Op: BootReply, HType: HTypeEthernet, XID: 1,
				YIAddr: netip.MustParseAddr("192.168.99.50"),
				SIAddr: netip.MustParseAddr("192.168.99.1"),
				CHAddr: []byte{0, 1, 2, 3, 4, 5},
				SName:  []byte("dnsmasq"),
				File:   []byte("pxelinux.0"),
				Options: Options{
					OptMessageType: {byte(MsgOffer)},
					OptServerID:    {192, 168, 99, 1},
					OptSubnetMask:  {255, 255, 255, 0},
					OptLeaseTime:   {0, 0, 0x0E, 0x10},
					OptDNSServer:   {192, 168, 99, 1, 8, 8, 8, 8},
					OptDomainName:  []byte("example.test"),
				},
			},
		},
		{
			"16-octet chaddr",
			&Message{
				Op: BootRequest, HType: 0, CHAddr: bytes.Repeat([]byte{0xAB}, 16),
				Options: Options{OptMessageType: {byte(MsgRequest)}},
			},
		},
		{
			"255-octet option value",
			&Message{
				Op: BootReply, HType: HTypeEthernet, CHAddr: []byte{1},
				Options: Options{
					OptMessageType: {byte(MsgAck)},
					OptDomainName:  bytes.Repeat([]byte{'x'}, 255),
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Encode(tc.msg)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(b)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			assertSameMessage(t, tc.msg, got)

			// Encoding the decoded form reproduces the bytes exactly. This is
			// the property replay needs and it is stronger than field
			// equality: a field the decoder silently dropped and the encoder
			// silently defaulted would survive a field-by-field comparison.
			again, err := Encode(got)
			if err != nil {
				t.Fatalf("re-Encode: %v", err)
			}
			if !bytes.Equal(b, again) {
				t.Fatalf("encode(decode(x)) != x\n got: %x\nwant: %x", again, b)
			}
		})
	}
}

func assertSameMessage(t *testing.T, want, got *Message) {
	t.Helper()
	if got.Op != want.Op || got.HType != want.HType || got.XID != want.XID ||
		got.Secs != want.Secs || got.Flags != want.Flags || got.Hops != want.Hops {
		t.Fatalf("header mismatch\n got %+v\nwant %+v", got, want)
	}
	// norm4, not raw equality, and the reason is a real asymmetry rather than
	// test convenience: the four address fields are four fixed octets on the
	// wire with no "absent" encoding, so a decoded message always carries a
	// valid Addr. An unset netip.Addr and a wire 0.0.0.0 are therefore the
	// SAME message. TestDecodeHasNoAbsentAddress pins that bound so it is a
	// stated property and not a hole this helper quietly papers over.
	if norm4(got.CIAddr) != norm4(want.CIAddr) || norm4(got.YIAddr) != norm4(want.YIAddr) ||
		norm4(got.SIAddr) != norm4(want.SIAddr) || norm4(got.GIAddr) != norm4(want.GIAddr) {
		t.Fatalf("address mismatch\n got %s/%s/%s/%s\nwant %s/%s/%s/%s",
			got.CIAddr, got.YIAddr, got.SIAddr, got.GIAddr,
			want.CIAddr, want.YIAddr, want.SIAddr, want.GIAddr)
	}
	if !bytes.Equal(got.CHAddr, want.CHAddr) {
		t.Fatalf("chaddr %x, want %x", got.CHAddr, want.CHAddr)
	}
	if !bytes.Equal(got.SName, want.SName) {
		t.Fatalf("sname %q, want %q", got.SName, want.SName)
	}
	if !bytes.Equal(got.File, want.File) {
		t.Fatalf("file %q, want %q", got.File, want.File)
	}
	for _, c := range want.Options.Codes() {
		if c == OptPad || c == OptEnd {
			continue
		}
		if !bytes.Equal(got.Options[c], want.Options[c]) {
			t.Fatalf("option %s = %x, want %x", c, got.Options[c], want.Options[c])
		}
	}
	for _, c := range got.Options.Codes() {
		if _, ok := want.Options[c]; !ok {
			t.Fatalf("decoder invented option %s = %x", c, got.Options[c])
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	good, err := Encode(discoverFixture())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	cases := []struct {
		name string
		of   func() []byte
		want error
	}{
		{
			"short of the fixed header",
			func() []byte { return good[:HeaderLen-1] },
			ErrShort,
		},
		{
			"empty",
			func() []byte { return nil },
			ErrShort,
		},
		{
			"bad magic cookie",
			func() []byte {
				b := append([]byte(nil), good...)
				b[236] ^= 0xFF
				return b
			},
			ErrBadCookie,
		},
		{
			"option with no length octet",
			func() []byte {
				b := append([]byte(nil), good[:HeaderLen]...)
				return append(b, byte(OptDomainName))
			},
			ErrTruncatedOption,
		},
		{
			"option length runs past the buffer",
			func() []byte {
				b := append([]byte(nil), good[:HeaderLen]...)
				return append(b, byte(OptDomainName), 40, 'a', 'b')
			},
			ErrOptionOverrun,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.of())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Decode error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeToleratesMissingEnd(t *testing.T) {
	// A server that fills the options field exactly has nowhere to put an END.
	// Refusing the message would refuse a lease over a framing detail.
	b := append([]byte(nil), make([]byte, HeaderLen)...)
	copy(b[236:240], []byte{99, 130, 83, 99})
	b = append(b, byte(OptMessageType), 1, byte(MsgAck))
	m, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got, ok := m.Type(); !ok || got != MsgAck {
		t.Fatalf("type = %v/%v, want DHCPACK", got, ok)
	}
}

func TestDecodeConcatenatesRepeatedOptions(t *testing.T) {
	// RFC 2131 section 4.1 and RFC 3396: repeated instances of one code are
	// concatenated. Last-wins is the obvious implementation and it silently
	// drops half a long option — a truncated DNS list is a working lease with
	// a missing resolver, which nobody reports as a codec bug.
	b := append([]byte(nil), make([]byte, HeaderLen)...)
	copy(b[236:240], []byte{99, 130, 83, 99})
	b = append(b, byte(OptMessageType), 1, byte(MsgAck))
	b = append(b, byte(OptDNSServer), 4, 192, 168, 99, 1)
	b = append(b, byte(OptDNSServer), 4, 8, 8, 8, 8)
	b = append(b, byte(OptEnd))

	m, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []byte{192, 168, 99, 1, 8, 8, 8, 8}
	if !bytes.Equal(m.Options[OptDNSServer], want) {
		t.Fatalf("dns = %x, want %x (concatenated, not last-wins)", m.Options[OptDNSServer], want)
	}
	addrs, ok := m.Addrs4(OptDNSServer)
	if !ok || len(addrs) != 2 {
		t.Fatalf("Addrs4 = %v/%v, want two addresses", addrs, ok)
	}
}

func TestDecodeOptionOverload(t *testing.T) {
	// Overload 3: options continue in BOTH the file and sname fields. The
	// order matters — overload itself lives in the options field, so that
	// field must be parsed first or the extra options are never looked for.
	b := make([]byte, HeaderLen)
	copy(b[236:240], []byte{99, 130, 83, 99})
	// file field carries the server identifier.
	copy(b[108:], []byte{byte(OptServerID), 4, 192, 168, 99, 1, byte(OptEnd)})
	// sname field carries the lease time.
	copy(b[44:], []byte{byte(OptLeaseTime), 4, 0, 0, 0x0E, 0x10, byte(OptEnd)})
	b = append(b, byte(OptMessageType), 1, byte(MsgAck))
	b = append(b, byte(OptOverload), 1, 3)
	b = append(b, byte(OptEnd))

	m, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if sid, ok := m.Addr4(OptServerID); !ok || sid != netip.MustParseAddr("192.168.99.1") {
		t.Fatalf("server id = %v/%v, want 192.168.99.1 from the overloaded file field", sid, ok)
	}
	if lt, ok := m.Uint32(OptLeaseTime); !ok || lt != 3600 {
		t.Fatalf("lease time = %v/%v, want 3600 from the overloaded sname field", lt, ok)
	}
	// An overloaded field is options, not text: it must not also surface as
	// File or SName.
	if len(m.File) != 0 || len(m.SName) != 0 {
		t.Fatalf("overloaded fields leaked as text: file=%q sname=%q", m.File, m.SName)
	}
}

func TestDecodeIgnoresOverloadWhenNotSet(t *testing.T) {
	// The preservation control for the test above: with no overload option,
	// the same bytes in file and sname are TEXT and must not be parsed as
	// options. A decoder that always parsed them would pass the overload test
	// and corrupt every ordinary message.
	b := make([]byte, HeaderLen)
	copy(b[236:240], []byte{99, 130, 83, 99})
	copy(b[108:], []byte{byte(OptServerID), 4, 192, 168, 99, 1, byte(OptEnd)})
	b = append(b, byte(OptMessageType), 1, byte(MsgAck), byte(OptEnd))

	m, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if _, ok := m.Options[OptServerID]; ok {
		t.Fatal("file field was parsed as options with no overload option present")
	}
	if len(m.File) == 0 {
		t.Fatal("file field vanished; it should have been kept as text")
	}
}

func TestEncodeRejects(t *testing.T) {
	t.Run("chaddr too long", func(t *testing.T) {
		m := &Message{Op: BootRequest, CHAddr: bytes.Repeat([]byte{1}, 17)}
		if _, err := Encode(m); !errors.Is(err, ErrCHAddrTooLong) {
			t.Fatalf("err = %v, want ErrCHAddrTooLong", err)
		}
	})
	t.Run("option too long", func(t *testing.T) {
		m := &Message{Op: BootRequest, CHAddr: []byte{1},
			Options: Options{OptDomainName: bytes.Repeat([]byte{'x'}, 256)}}
		if _, err := Encode(m); !errors.Is(err, ErrOptionTooLong) {
			t.Fatalf("err = %v, want ErrOptionTooLong", err)
		}
	})
}

func TestAddrs4RejectsRagged(t *testing.T) {
	// A five-octet DNS list is not "one address and a spare byte" — it is a
	// message we do not understand, and taking the prefix would hand the
	// caller a resolver the server never sent.
	m := &Message{Options: Options{OptDNSServer: {192, 168, 0, 1, 7}}}
	if addrs, ok := m.Addrs4(OptDNSServer); ok {
		t.Fatalf("Addrs4 accepted a ragged list: %v", addrs)
	}
}

func TestTextStripsTrailingNuls(t *testing.T) {
	m := &Message{Options: Options{OptDomainName: []byte("lan\x00\x00")}}
	got, ok := m.Text(OptDomainName)
	if !ok || got != "lan" {
		t.Fatalf("Text = %q/%v, want \"lan\"", got, ok)
	}
}

func TestCodesAreSorted(t *testing.T) {
	o := Options{OptEnd: nil, OptMessageType: {1}, OptServerID: {1, 2, 3, 4}, OptSubnetMask: {255}}
	got := o.Codes()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Codes not ascending: %v", got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("Codes returned %d entries, want 4", len(got))
	}
}

func TestCloneIsDeep(t *testing.T) {
	m := discoverFixture()
	c := m.Clone()
	c.Options[OptClientID][0] = 0xFF
	c.CHAddr[0] = 0xFF
	if m.Options[OptClientID][0] == 0xFF {
		t.Fatal("Clone shared the option backing array")
	}
	if m.CHAddr[0] == 0xFF {
		t.Fatal("Clone shared the chaddr backing array")
	}
}

// norm4 maps the zero netip.Addr onto 0.0.0.0. See assertSameMessage.
func norm4(a netip.Addr) netip.Addr {
	if !a.Is4() {
		return netip.AddrFrom4([4]byte{})
	}
	return a
}

func TestDecodeHasNoAbsentAddress(t *testing.T) {
	// The bound behind norm4, driven directly: a message encoded with an UNSET
	// ciaddr decodes to 0.0.0.0, not to an invalid Addr. Ring 1 must therefore
	// never ask "did the server set this field" — it asks whether the value is
	// unspecified, which is a different question with a different answer for
	// a server that deliberately sent zero.
	m := &Message{Op: BootReply, HType: HTypeEthernet, CHAddr: []byte{1, 2, 3, 4, 5, 6},
		Options: Options{OptMessageType: {byte(MsgAck)}}}
	if m.CIAddr.IsValid() {
		t.Fatal("fixture ciaddr should be the zero Addr")
	}
	b, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !back.CIAddr.IsValid() || back.CIAddr != netip.AddrFrom4([4]byte{}) {
		t.Fatalf("decoded ciaddr = %v, want 0.0.0.0", back.CIAddr)
	}
}

func TestEncodeIgnoresHLenAndUsesCHAddr(t *testing.T) {
	// HLen on a Message is what ARRIVED. Encode writes len(CHAddr) instead, so
	// a message decoded with a lying hlen cannot be re-encoded into a
	// different lie. Driven here because it is the kind of field that gets
	// "fixed" into a straight copy by someone tidying up.
	m := &Message{Op: BootRequest, HType: HTypeEthernet, HLen: 99,
		CHAddr: []byte{1, 2, 3, 4, 5, 6}, Options: Options{OptMessageType: {1}}}
	b, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if b[2] != 6 {
		t.Fatalf("hlen octet = %d, want 6 (len(CHAddr)), not the stale 99", b[2])
	}
}

func TestDecodeClampsOverlongHLen(t *testing.T) {
	// An hlen of 200 would index past the 16-octet chaddr field. A decoder
	// that trusted it would panic on a hostile packet, and the packet costs
	// one line to send.
	b := make([]byte, HeaderLen)
	copy(b[236:240], []byte{99, 130, 83, 99})
	b[2] = 200
	b = append(b, byte(OptMessageType), 1, byte(MsgAck), byte(OptEnd))
	m, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(m.CHAddr) != 16 {
		t.Fatalf("chaddr length %d, want it clamped to 16", len(m.CHAddr))
	}
}
