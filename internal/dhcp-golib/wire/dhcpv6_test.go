package wire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"net/netip"
	"strings"
	"testing"
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------- the captures --

// TestCapturedExchangeDecodesToWhatTheServerLogged is the row that A-1 and A-2
// turn on, and it is the only kind of row that can settle either.
//
// A codec whose encoder and decoder share a mistake round-trips perfectly. The
// 3-octet transaction-id of RFC 9915 §8 read as 4 octets shifts every option
// by one and still terminates on a plausible boundary; IA_NA's T1 and T2 are
// adjacent uint32s that nothing in a round trip can tell apart. So the values
// here are not this package's — they are the ones dnsmasq 2.91 printed into
// its own log while building the frame:
//
//	sent size: 40 option:  3 ia-na  IAID=168496141 T1=150 T2=259
//	nest size: 24 option:  5 iaaddr  fd00:99::183 PL=300 VL=300
//	sent size:  9 option: 13 status  0 success
//	sent size: 17 option: 24 domain-search  fixture.invalid
//	sent size: 16 option: 23 dns-server  fd00:99::1
//
// T1 and T2 DIFFER, which is what makes the swap visible. PL and VL do not:
// dnsmasq sets both to the lease time, so the preferred/valid half of A-2 is
// closed by TestIAAddrFieldsAreNotInterchangeable and its mutant, not here.
func TestCapturedExchangeDecodesToWhatTheServerLogged(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		typ  MessageTypeV6
		xid  uint32
	}{
		{"advertise", capAdvertise, MsgAdvertise, 0x1a2b3c},
		{"reply", capReply, MsgReply, 0x4d5e6f},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := DecodeV6(tc.raw)
			if err != nil {
				t.Fatalf("DecodeV6: %v", err)
			}
			if m.Type != tc.typ {
				t.Errorf("type %s, want %s", m.Type, tc.typ)
			}
			if m.XID != tc.xid {
				t.Errorf("xid %#06x, want %#06x — RFC 9915 section 8's transaction-id is THREE octets", m.XID, tc.xid)
			}

			ias, err := m.Options.IANAs()
			if err != nil {
				t.Fatalf("IANAs: %v", err)
			}
			if len(ias) != 1 {
				t.Fatalf("%d IA_NA option(s), want 1", len(ias))
			}
			ia := ias[0]
			// dnsmasq: "IAID=168496141 T1=150 T2=259".
			if ia.IAID != 168496141 {
				t.Errorf("IAID %d, want 168496141 (the value we sent, echoed)", ia.IAID)
			}
			if ia.T1 != 150 || ia.T2 != 259 {
				t.Errorf("T1=%d T2=%d, want T1=150 T2=259 as dnsmasq logged them", ia.T1, ia.T2)
			}

			addrs, err := ia.Options.Addrs()
			if err != nil {
				t.Fatalf("Addrs: %v", err)
			}
			if len(addrs) != 1 {
				t.Fatalf("%d IA Address option(s), want 1", len(addrs))
			}
			// dnsmasq: "iaaddr  fd00:99::183 PL=300 VL=300".
			if got, want := addrs[0].Addr, netip.MustParseAddr("fd00:99::183"); got != want {
				t.Errorf("address %s, want %s", got, want)
			}
			if addrs[0].PreferredLifetime != 300 || addrs[0].ValidLifetime != 300 {
				t.Errorf("PL=%d VL=%d, want 300 and 300", addrs[0].PreferredLifetime, addrs[0].ValidLifetime)
			}

			// dnsmasq: "status  0 success". Present, and Success.
			st, present, err := m.Options.Status()
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !present {
				t.Error("no Status Code option, but dnsmasq logged one")
			}
			if st.Code != StatusSuccess || st.Message != "success" {
				t.Errorf("status %v, want code 0 message %q", st, "success")
			}

			// dnsmasq: "dns-server  fd00:99::1".
			dns, err := m.Options.DNSServers()
			if err != nil {
				t.Fatalf("DNSServers: %v", err)
			}
			if len(dns) != 1 || dns[0] != netip.MustParseAddr("fd00:99::1") {
				t.Errorf("DNS %v, want [fd00:99::1]", dns)
			}

			// dnsmasq: "domain-search  fixture.invalid".
			search, err := m.Options.DomainSearch()
			if err != nil {
				t.Fatalf("DomainSearch: %v", err)
			}
			if len(search) != 1 || search[0] != "fixture.invalid" {
				t.Errorf("search %q, want [\"fixture.invalid\"]", search)
			}

			// The client identifier we sent, echoed: DUID-LL, hardware type 1,
			// the MAC dnsmasq logged as the client's.
			cid, ok := m.Options.First(OptV6ClientID)
			if !ok {
				t.Fatal("no Client Identifier in the server's answer")
			}
			want, err := DUIDLL(1, mustHex("ea494ee531ed"))
			if err != nil {
				t.Fatalf("DUIDLL: %v", err)
			}
			if !bytes.Equal(cid, want) {
				t.Errorf("client identifier %x, want %x", cid, want)
			}
		})
	}
}

// TestOurEncodedMessagesAreTheOctetsThatCrossedTheLink closes the other half of
// the round-trip: the two frames dnsmasq answered were built by EncodeV6, and
// these are the octets recorded off the wire, not re-encoded. An encoder change
// shows up here as a diff rather than as a test agreeing with itself.
//
// dnsmasq's log for these two, verbatim:
//
//	DHCPSOLICIT(v6srv) 00:03:00:01:ea:49:4e:e5:31:ed
//	DHCPREQUEST(v6srv) 00:03:00:01:ea:49:4e:e5:31:ed
func TestOurEncodedMessagesAreTheOctetsThatCrossedTheLink(t *testing.T) {
	duid, err := DUIDLL(1, mustHex("ea494ee531ed"))
	if err != nil {
		t.Fatalf("DUIDLL: %v", err)
	}
	iana, err := EncodeIANA(&IANA{IAID: 0x0a0b0c0d})
	if err != nil {
		t.Fatalf("EncodeIANA: %v", err)
	}
	oro := []byte{0x00, byte(OptV6DNSServers), 0x00, byte(OptV6DomainList)}

	got, err := EncodeV6(&MessageV6{
		Type: MsgSolicit,
		XID:  0x1a2b3c,
		Options: OptionsV6{
			{Code: OptV6ClientID, Data: duid},
			{Code: OptV6ElapsedTime, Data: []byte{0x00, 0x00}},
			{Code: OptV6IANA, Data: iana},
			{Code: OptV6ORO, Data: oro},
		},
	})
	if err != nil {
		t.Fatalf("EncodeV6: %v", err)
	}
	if !bytes.Equal(got, capSolicit) {
		t.Errorf("Solicit encodes to\n  %x\nbut the frame dnsmasq answered was\n  %x", got, capSolicit)
	}

	// The Request echoes the whole IA_NA the server sent, addresses included,
	// which is what §18.2.2 asks for and what makes the echo path testable at
	// all: the IA_NA below is DECODED from the captured Advertise and
	// re-encoded.
	adv, err := DecodeV6(capAdvertise)
	if err != nil {
		t.Fatalf("DecodeV6 advertise: %v", err)
	}
	ias, err := adv.Options.IANAs()
	if err != nil || len(ias) != 1 {
		t.Fatalf("IANAs: %v %d", err, len(ias))
	}
	iaEcho, err := EncodeIANA(ias[0])
	if err != nil {
		t.Fatalf("EncodeIANA echo: %v", err)
	}
	sid, ok := adv.Options.First(OptV6ServerID)
	if !ok {
		t.Fatal("no Server Identifier in the captured Advertise")
	}
	got, err = EncodeV6(&MessageV6{
		Type: MsgRequest6,
		XID:  0x4d5e6f,
		Options: OptionsV6{
			{Code: OptV6ClientID, Data: duid},
			{Code: OptV6ServerID, Data: sid},
			{Code: OptV6ElapsedTime, Data: []byte{0x00, 0x0a}},
			{Code: OptV6IANA, Data: iaEcho},
			{Code: OptV6ORO, Data: oro},
		},
	})
	if err != nil {
		t.Fatalf("EncodeV6 request: %v", err)
	}
	if !bytes.Equal(got, capRequest) {
		t.Errorf("Request encodes to\n  %x\nbut the frame dnsmasq answered was\n  %x", got, capRequest)
	}
}

// ------------------------------------------------------------ the header --

// TestDecodeV6RefusesWhatIsNotAClientMessage is A-8. A Relay-forward's options
// begin at octet 34, not 4 (§9), so walking one from the client offset reads
// its link-address field as an option header and stops wherever the bytes
// happen to look terminal. §16: "A client or server MUST discard any received
// DHCP messages with an unknown message type."
func TestDecodeV6RefusesWhatIsNotAClientMessage(t *testing.T) {
	body := "0001000a00030001ea494ee531ed"
	for _, tc := range []struct {
		name string
		raw  []byte
		want error
	}{
		{"three octets", mustHex("011a2b"), ErrV6Short},
		{"empty", nil, ErrV6Short},
		{"reconfigure", mustHex("0a1a2b3c" + body), ErrV6NotForClient},
		{"relay-forward", mustHex("0c1a2b3c" + body), ErrV6NotForClient},
		{"relay-reply", mustHex("0d1a2b3c" + body), ErrV6NotForClient},
		{"type 0", mustHex("001a2b3c" + body), ErrV6UnknownType},
		{"type 14", mustHex("0e1a2b3c" + body), ErrV6UnknownType},
		{"type 255", mustHex("ff1a2b3c" + body), ErrV6UnknownType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := DecodeV6(tc.raw)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DecodeV6 = %v, %v; want error %v", m, err, tc.want)
			}
			if m != nil {
				t.Errorf("a refused decode returned a message: %v", m)
			}
		})
	}
}

// TestDecodeV6AcceptsEveryClientMessageType is the other direction of the row
// above: a refusal that refuses everything passes it. Every type §16 lists as
// one a client sends or accepts must decode.
func TestDecodeV6AcceptsEveryClientMessageType(t *testing.T) {
	for _, typ := range []MessageTypeV6{
		MsgSolicit, MsgAdvertise, MsgRequest6, MsgConfirm, MsgRenew,
		MsgRebind, MsgReply, MsgRelease6, MsgDecline6, MsgInformationRequest,
	} {
		if !typ.ForClient() {
			t.Errorf("%s reports ForClient() false", typ)
			continue
		}
		raw := append([]byte{byte(typ), 0x1a, 0x2b, 0x3c}, mustHex("0001000a00030001ea494ee531ed")...)
		m, err := DecodeV6(raw)
		if err != nil {
			t.Errorf("DecodeV6 %s: %v", typ, err)
			continue
		}
		if m.Type != typ || m.XID != 0x1a2b3c {
			t.Errorf("decoded %s xid %#x, want %s xid 0x1a2b3c", m.Type, m.XID, typ)
		}
	}
}

// TestTransactionIDIsThreeOctets pins A-1 from the other side: the field is 24
// bits, the encoder refuses a value that does not fit, and the boundary is
// exact.
func TestTransactionIDIsThreeOctets(t *testing.T) {
	if MaxXID6 != 1<<24 {
		t.Fatalf("MaxXID6 = %d, want 1<<24 (§8's transaction-id is 3 octets)", MaxXID6)
	}
	for _, xid := range []uint32{0, 1, 0xFFFFFF} {
		b, err := EncodeV6(&MessageV6{Type: MsgSolicit, XID: xid})
		if err != nil {
			t.Fatalf("EncodeV6 xid %#x: %v", xid, err)
		}
		if len(b) != V6HeaderLen {
			t.Fatalf("a message with no options is %d octets, want %d", len(b), V6HeaderLen)
		}
		m, err := DecodeV6(b)
		if err != nil {
			t.Fatalf("DecodeV6 xid %#x: %v", xid, err)
		}
		if m.XID != xid {
			t.Errorf("xid round trip %#x -> %#x", xid, m.XID)
		}
	}
	if _, err := EncodeV6(&MessageV6{Type: MsgSolicit, XID: 1 << 24}); !errors.Is(err, ErrV6Encode) {
		t.Errorf("EncodeV6 with a 25-bit transaction-id: %v, want %v", err, ErrV6Encode)
	}
	if _, err := EncodeV6(nil); !errors.Is(err, ErrV6Encode) {
		t.Errorf("EncodeV6(nil): %v, want %v", err, ErrV6Encode)
	}
}

// ----------------------------------------------------------- the options --

// TestOptionWalkRefusesWhatRunsPastTheEnd is A-7, with D17's adversarial rows.
// A length field that runs past the buffer must produce a named error, not a
// clamped decode and not a panic.
func TestOptionWalkRefusesWhatRunsPastTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{"one octet of option header", "00", ErrV6TruncatedOption},
		{"three octets of option header", "000100", ErrV6TruncatedOption},
		{"length one past the end", "00010001", ErrV6OptionOverrun},
		{"length 0xffff on an empty value", "0001ffff", ErrV6OptionOverrun},
		{"good option then a truncated header", "0001000212340002", ErrV6TruncatedOption},
		{"good option then an overrun", "00010002123400020003ff", ErrV6OptionOverrun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseOptionsV6(mustHex(tc.body)); !errors.Is(err, tc.want) {
				t.Errorf("ParseOptionsV6(%s) error %v, want %v", tc.body, err, tc.want)
			}
			raw := append(mustHex("011a2b3c"), mustHex(tc.body)...)
			if _, err := DecodeV6(raw); !errors.Is(err, tc.want) {
				t.Errorf("DecodeV6 error %v, want %v", err, tc.want)
			}
		})
	}
}

// TestZeroLengthOptionIsLegalAndKept states the boundary the row above must not
// swallow. §21.1 puts no floor on option-len; a walk that refuses length 0
// would refuse a Rapid Commit option, and one that SKIPS it silently drops a
// signal.
func TestZeroLengthOptionIsLegalAndKept(t *testing.T) {
	opts, err := ParseOptionsV6(mustHex("000e0000" + "00010002abcd"))
	if err != nil {
		t.Fatalf("ParseOptionsV6: %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("%d option(s), want 2: a zero-length option is legal and must be kept", len(opts))
	}
	if opts[0].Code != 14 || len(opts[0].Data) != 0 {
		t.Errorf("first option %v, want code 14 with an empty value", opts[0])
	}
	if opts[1].Code != OptV6ClientID || !bytes.Equal(opts[1].Data, mustHex("abcd")) {
		t.Errorf("second option %v, want the client identifier: the walk lost its place", opts[1])
	}
}

// TestOptionsKeepWireOrderAndRepeats is why OptionsV6 is a slice.
//
// DHCPv6 has no RFC 3396 concatenation and repeats are ordinary: several IA_NA
// options, several IA Address options inside one. A repeat that is NOT ordinary
// — two Server Identifiers in one Reply, which §16.10 makes grounds to discard
// the message — is policy for the state machine, and it can only judge what the
// decoder let it see. A map keyed by option code silently keeps one.
func TestOptionsKeepWireOrderAndRepeats(t *testing.T) {
	raw := mustHex("071a2b3c" +
		"00020004aaaaaaaa" +
		"00020004bbbbbbbb" +
		"00010002cccc")
	m, err := DecodeV6(raw)
	if err != nil {
		t.Fatalf("DecodeV6: %v", err)
	}
	if n := m.Options.Count(OptV6ServerID); n != 2 {
		t.Fatalf("Count(server-id) = %d, want 2: §16.10 says a Reply with two Server Identifiers is discarded, "+
			"and the machine cannot discard what the decoder folded away", n)
	}
	first, ok := m.Options.First(OptV6ServerID)
	if !ok || !bytes.Equal(first, mustHex("aaaaaaaa")) {
		t.Errorf("First(server-id) = %x %v, want aaaaaaaa: First must mean FIRST ON THE WIRE", first, ok)
	}
	all := m.Options.All(OptV6ServerID)
	if len(all) != 2 || !bytes.Equal(all[0], mustHex("aaaaaaaa")) || !bytes.Equal(all[1], mustHex("bbbbbbbb")) {
		t.Errorf("All(server-id) = %x, want [aaaaaaaa bbbbbbbb] in wire order", all)
	}
	if m.Options[2].Code != OptV6ClientID {
		t.Errorf("the third option is %s, want the client identifier: the slice is not in wire order", m.Options[2].Code)
	}
	if _, ok := m.Options.First(OptV6IANA); ok {
		t.Error("First reported an option that is not in the message")
	}
	if n := m.Options.Count(OptV6IANA); n != 0 {
		t.Errorf("Count of an absent option = %d, want 0", n)
	}
	if all := m.Options.All(OptV6IANA); len(all) != 0 {
		t.Errorf("All of an absent option = %v, want empty", all)
	}

	// The decoded options re-encode to the octets they came from, repeats and
	// order intact.
	back, err := EncodeV6(m)
	if err != nil {
		t.Fatalf("EncodeV6: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Errorf("round trip\n  %x\n->\n  %x", raw, back)
	}
}

// TestOptionsDataIsNotAliasedToTheInput guards the shape where a decoded
// message shares memory with the buffer a socket will reuse for the next
// datagram — a defect that looks like a message changing after it was read.
func TestOptionsDataIsNotAliasedToTheInput(t *testing.T) {
	raw := mustHex("071a2b3c00010004deadbeef")
	m, err := DecodeV6(raw)
	if err != nil {
		t.Fatalf("DecodeV6: %v", err)
	}
	for i := range raw {
		raw[i] = 0
	}
	v, _ := m.Options.First(OptV6ClientID)
	if !bytes.Equal(v, mustHex("deadbeef")) {
		t.Errorf("the option value became %x after its buffer was overwritten: the decoder aliased the input", v)
	}
}

// ------------------------------------------------------------ IA_NA rows --

// TestIANARoundTripsAndRefusesShortOptions covers the nested walk: an IA_NA
// carries a 12-octet header and then its own options area, which may itself
// hold another IA_NA header-shaped run of bytes.
func TestIANARoundTripsAndRefusesShortOptions(t *testing.T) {
	if IANAFixedLen != 12 {
		t.Fatalf("IANAFixedLen = %d, want 12 (§21.4: IAID, T1, T2)", IANAFixedLen)
	}
	for _, n := range []int{0, 1, 11} {
		if _, err := DecodeIANA(make([]byte, n)); !errors.Is(err, ErrV6BadOption) {
			t.Errorf("DecodeIANA on %d octet(s): %v, want %v", n, err, ErrV6BadOption)
		}
	}
	ia := &IANA{
		IAID:    0xdeadbeef,
		T1:      150,
		T2:      259,
		Options: OptionsV6{{Code: OptV6StatusCode, Data: mustHex("0002")}},
	}
	b, err := EncodeIANA(ia)
	if err != nil {
		t.Fatalf("EncodeIANA: %v", err)
	}
	got, err := DecodeIANA(b)
	if err != nil {
		t.Fatalf("DecodeIANA: %v", err)
	}
	if got.IAID != ia.IAID || got.T1 != ia.T1 || got.T2 != ia.T2 {
		t.Errorf("round trip IAID=%08x T1=%d T2=%d, want %08x %d %d", got.IAID, got.T1, got.T2, ia.IAID, ia.T1, ia.T2)
	}

	// D17: an IA_NA nested inside an IA_NA. §21.4 does not define it and the
	// walk must not recurse into it, but neither may it lose its place.
	inner, err := EncodeIANA(&IANA{IAID: 1, T1: 2, T2: 3})
	if err != nil {
		t.Fatalf("EncodeIANA inner: %v", err)
	}
	outer, err := EncodeIANA(&IANA{IAID: 9, T1: 8, T2: 7, Options: OptionsV6{{Code: OptV6IANA, Data: inner}}})
	if err != nil {
		t.Fatalf("EncodeIANA outer: %v", err)
	}
	dec, err := DecodeIANA(outer)
	if err != nil {
		t.Fatalf("DecodeIANA nested: %v", err)
	}
	if dec.IAID != 9 || len(dec.Options) != 1 || dec.Options[0].Code != OptV6IANA {
		t.Fatalf("nested decode: IAID=%d options=%v", dec.IAID, dec.Options)
	}
	if nested, err := dec.Options.IANAs(); err != nil || len(nested) != 1 || nested[0].IAID != 1 {
		t.Errorf("the inner IA_NA decoded to %v (%v), want one with IAID 1", nested, err)
	}

	// An IA_NA option whose value is too short must fail the accessor rather
	// than being skipped, or a malformed server answer reads as "no addresses".
	bad := OptionsV6{{Code: OptV6IANA, Data: mustHex("0011")}}
	if _, err := bad.IANAs(); !errors.Is(err, ErrV6BadOption) {
		t.Errorf("IANAs over a 2-octet IA_NA: %v, want %v", err, ErrV6BadOption)
	}
}

// TestIAAddrFieldsAreNotInterchangeable is A-2's second half and A-9.
//
// SYNTHETIC, and it says so: no captured frame can serve here, because dnsmasq
// set the preferred and valid lifetimes to the same value. The two are adjacent
// uint32s, so only differing values separate them.
//
// §21.6: "The client MUST discard any addresses for which the preferred
// lifetime is greater than the valid lifetime." That is a rule for the state
// machine. Ring 0 decodes both and states the predicate; a decoder that
// enforced it would delete the very address M7b has to Decline.
func TestIAAddrFieldsAreNotInterchangeable(t *testing.T) {
	if IAAddrFixedLen != 24 {
		t.Fatalf("IAAddrFixedLen = %d, want 24 (§21.6: address, preferred, valid)", IAAddrFixedLen)
	}
	addr := netip.MustParseAddr("2001:db8::1")
	for _, tc := range []struct {
		name             string
		preferred, valid uint32
		ok               bool
	}{
		{"ordinary", 300, 600, true},
		{"equal", 300, 300, true},
		{"preferred greater than valid", 601, 600, false},
		{"one greater", 1, 0, false},
		{"both zero", 0, 0, true},
		{"infinite valid", 300, math.MaxUint32, true},
		{"infinite preferred, finite valid", math.MaxUint32, 300, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := EncodeIAAddr(&IAAddr{Addr: addr, PreferredLifetime: tc.preferred, ValidLifetime: tc.valid})
			if err != nil {
				t.Fatalf("EncodeIAAddr: %v", err)
			}
			got, err := DecodeIAAddr(b)
			if err != nil {
				t.Fatalf("DecodeIAAddr: %v — §21.6's discard is the machine's rule, not the decoder's", err)
			}
			if got.Addr != addr {
				t.Errorf("address %s, want %s", got.Addr, addr)
			}
			if got.PreferredLifetime != tc.preferred {
				t.Errorf("preferred %d, want %d (the fields are adjacent uint32s; a swap is invisible when they are equal)",
					got.PreferredLifetime, tc.preferred)
			}
			if got.ValidLifetime != tc.valid {
				t.Errorf("valid %d, want %d", got.ValidLifetime, tc.valid)
			}
			if got.Valid() != tc.ok {
				t.Errorf("Valid() = %v, want %v", got.Valid(), tc.ok)
			}
		})
	}

	for _, n := range []int{0, 1, 23} {
		if _, err := DecodeIAAddr(make([]byte, n)); !errors.Is(err, ErrV6BadOption) {
			t.Errorf("DecodeIAAddr on %d octet(s): %v, want %v", n, err, ErrV6BadOption)
		}
	}
	if _, err := EncodeIAAddr(&IAAddr{Addr: netip.MustParseAddr("192.0.2.1")}); !errors.Is(err, ErrV6Encode) {
		t.Errorf("EncodeIAAddr with an IPv4 address: %v, want %v", err, ErrV6Encode)
	}
	if _, err := EncodeIAAddr(&IAAddr{}); !errors.Is(err, ErrV6Encode) {
		t.Errorf("EncodeIAAddr with the zero address: %v, want %v", err, ErrV6Encode)
	}
}

// TestAddrsWalksEveryIAAddressInWireOrder guards the accessor a state machine
// reads: several addresses in one IA_NA is ordinary, and the option that is not
// an IA Address must be skipped rather than mis-parsed.
func TestAddrsWalksEveryIAAddressInWireOrder(t *testing.T) {
	one, err := EncodeIAAddr(&IAAddr{Addr: netip.MustParseAddr("2001:db8::1"), PreferredLifetime: 10, ValidLifetime: 20})
	if err != nil {
		t.Fatalf("EncodeIAAddr: %v", err)
	}
	two, err := EncodeIAAddr(&IAAddr{Addr: netip.MustParseAddr("2001:db8::2"), PreferredLifetime: 30, ValidLifetime: 40})
	if err != nil {
		t.Fatalf("EncodeIAAddr: %v", err)
	}
	opts := OptionsV6{
		{Code: OptV6IAAddr, Data: one},
		{Code: OptV6StatusCode, Data: mustHex("0000")},
		{Code: OptV6IAAddr, Data: two},
	}
	got, err := opts.Addrs()
	if err != nil {
		t.Fatalf("Addrs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d address(es), want 2", len(got))
	}
	if got[0].Addr.String() != "2001:db8::1" || got[1].Addr.String() != "2001:db8::2" {
		t.Errorf("addresses %s and %s, want ::1 then ::2 in wire order", got[0].Addr, got[1].Addr)
	}
	bad := OptionsV6{{Code: OptV6IAAddr, Data: mustHex("00")}}
	if _, err := bad.Addrs(); !errors.Is(err, ErrV6BadOption) {
		t.Errorf("Addrs over a 1-octet IA Address: %v, want %v", err, ErrV6BadOption)
	}
}

// ------------------------------------------------------------ status code --

// TestStatusDistinguishesAbsentFromSuccess is A-4.
//
// §7.5: "If the Status Code option does not appear in a message in which the
// option could appear, the status of the message is assumed to be Success."
// Success is therefore the ordinary answer, and an accessor that folds absence
// into it passes every happy-path row while making a NoAddrsAvail Advertise
// indistinguishable from an ordinary one.
func TestStatusDistinguishesAbsentFromSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    OptionsV6
		present bool
		code    StatusCode
		msg     string
	}{
		{"absent", OptionsV6{{Code: OptV6ClientID, Data: mustHex("00")}}, false, StatusSuccess, ""},
		{"present, success, no message", OptionsV6{{Code: OptV6StatusCode, Data: mustHex("0000")}}, true, StatusSuccess, ""},
		{"present, NoAddrsAvail", OptionsV6{{Code: OptV6StatusCode, Data: mustHex("0002")}}, true, StatusNoAddrsAvail, ""},
		{"present, with a message", OptionsV6{{Code: OptV6StatusCode, Data: mustHex("000273756363657373")}}, true, StatusNoAddrsAvail, "success"},
		{"empty message list", OptionsV6{}, false, StatusSuccess, ""},
		{"nil options", nil, false, StatusSuccess, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, present, err := tc.opts.Status()
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if present != tc.present {
				t.Fatalf("present = %v, want %v — absence and Success are different facts", present, tc.present)
			}
			if st.Code != tc.code || st.Message != tc.msg {
				t.Errorf("status %v (code %d, message %q), want code %d message %q", st, st.Code, st.Message, tc.code, tc.msg)
			}
		})
	}

	if _, _, err := (OptionsV6{{Code: OptV6StatusCode, Data: mustHex("00")}}).Status(); !errors.Is(err, ErrV6BadOption) {
		t.Errorf("a 1-octet Status Code option: want %v", ErrV6BadOption)
	}
	if _, err := DecodeStatus(nil); !errors.Is(err, ErrV6BadOption) {
		t.Errorf("DecodeStatus(nil): want %v", ErrV6BadOption)
	}
	s := Status{Code: StatusNoBinding, Message: "gone"}
	back, err := DecodeStatus(EncodeStatus(s))
	if err != nil || back != s {
		t.Errorf("Status round trip %v -> %v (%v)", s, back, err)
	}
	// §16 obsoletes the Server Unicast option and the UseMulticast status
	// code. Code 5 must therefore arrive with NO name, exactly as any code
	// IANA assigns after today does — a named constant is an invitation to
	// implement it.
	if s := StatusCode(5).String(); !strings.Contains(s, "5") || strings.Contains(strings.ToLower(s), "multicast") {
		t.Errorf("status code 5 renders as %q; §16 obsoleted UseMulticast, so it must read as an unknown code", s)
	}
	if s := StatusCode(6).String(); !strings.Contains(s, "6") {
		t.Errorf("status code 6 renders as %q; prefix delegation is out of scope (D25) and NoPrefixAvail must stay unnamed", s)
	}
}

// ----------------------------------------------------------- elapsed time --

// TestElapsedTimeSaturates is A-3. §21.9: "The client uses the value 0xffff to
// represent any elapsed-time values greater than the largest time value that
// can be represented." A wrap only shows after 655.36 seconds of one exchange,
// which no unit test reaches by accident and a real Rebind does.
func TestElapsedTimeSaturates(t *testing.T) {
	if MaxElapsedHundredths != 0xFFFF {
		t.Fatalf("MaxElapsedHundredths = %#x, want 0xffff", MaxElapsedHundredths)
	}
	for _, tc := range []struct {
		in   uint64
		want uint16
	}{
		{0, 0},
		{1, 1},
		{0xFFFE, 0xFFFE},
		{0xFFFF, 0xFFFF},
		{0x10000, 0xFFFF},
		{0x10001, 0xFFFF},
		{1 << 32, 0xFFFF},
		{math.MaxUint64, 0xFFFF},
	} {
		if got := ElapsedHundredths(tc.in); got != tc.want {
			t.Errorf("ElapsedHundredths(%d) = %#x, want %#x", tc.in, got, tc.want)
		}
	}

	got, present, err := (OptionsV6{{Code: OptV6ElapsedTime, Data: mustHex("1234")}}).ElapsedTime()
	if err != nil || !present || got != 0x1234 {
		t.Errorf("ElapsedTime = %#x %v %v, want 0x1234 present", got, present, err)
	}
	if _, present, err := (OptionsV6{}).ElapsedTime(); err != nil || present {
		t.Errorf("ElapsedTime with no option: present=%v err=%v, want absent and no error", present, err)
	}
	for _, bad := range []string{"", "12", "123456"} {
		if _, _, err := (OptionsV6{{Code: OptV6ElapsedTime, Data: mustHex(bad)}}).ElapsedTime(); !errors.Is(err, ErrV6BadOption) {
			t.Errorf("a %d-octet Elapsed Time option: want %v", len(bad)/2, ErrV6BadOption)
		}
	}
}

// ------------------------------------------------------------ RFC 3646 --

// TestDNSServersRefusesAShortList guards the accessor's arithmetic: RFC 3646 §3
// makes the option a whole number of 16-octet addresses, and a remainder must
// be an error rather than a silently dropped server.
func TestDNSServersRefusesAShortList(t *testing.T) {
	a := "20010db8000000000000000000000001"
	b := "20010db8000000000000000000000002"
	got, err := (OptionsV6{{Code: OptV6DNSServers, Data: mustHex(a + b)}}).DNSServers()
	if err != nil {
		t.Fatalf("DNSServers: %v", err)
	}
	if len(got) != 2 || got[0].String() != "2001:db8::1" || got[1].String() != "2001:db8::2" {
		t.Fatalf("DNSServers = %v, want two addresses in wire order", got)
	}
	if got, err := (OptionsV6{}).DNSServers(); err != nil || len(got) != 0 {
		t.Errorf("DNSServers with no option = %v %v, want empty and no error", got, err)
	}
	for _, bad := range []string{a + "00", "00", a[:30]} {
		if _, err := (OptionsV6{{Code: OptV6DNSServers, Data: mustHex(bad)}}).DNSServers(); !errors.Is(err, ErrV6BadOption) {
			t.Errorf("a %d-octet DNS server list: want %v", len(bad)/2, ErrV6BadOption)
		}
	}
	// Two options, both walked: §21.1 allows the repeat and a client that read
	// only the first would lose half its resolvers.
	got, err = (OptionsV6{
		{Code: OptV6DNSServers, Data: mustHex(a)},
		{Code: OptV6DNSServers, Data: mustHex(b)},
	}).DNSServers()
	if err != nil || len(got) != 2 {
		t.Errorf("two DNS server options gave %v (%v), want both addresses", got, err)
	}
}

// TestDomainSearchRefusesACompressionPointer is A-5, and it is the row the v4
// codec makes easy to get wrong: wire/values.go's readName FOLLOWS RFC 1035
// §4.1.4 pointers, because a DHCPv4 message is one buffer to point into.
//
// RFC 9915 §10: "The message compression scheme in Section 4.1.4 of [RFC1035]
// MUST NOT be used." An option value is not a DNS message; a pointer in one
// points at whatever the option happens to start with.
func TestDomainSearchRefusesACompressionPointer(t *testing.T) {
	ok := "076669787475726507696e76616c696400"
	got, err := (OptionsV6{{Code: OptV6DomainList, Data: mustHex(ok)}}).DomainSearch()
	if err != nil {
		t.Fatalf("DomainSearch: %v", err)
	}
	if len(got) != 1 || got[0] != "fixture.invalid" {
		t.Fatalf("DomainSearch = %q, want [\"fixture.invalid\"]", got)
	}

	for _, tc := range []struct {
		name string
		data string
	}{
		{"a bare pointer", "c000"},
		{"a name then a pointer", "076578616d706c65c000"},
		{"a pointer to the second label", "07666978747572650769" + "c002"},
		{"the 0x40 form", "40"},
		{"the 0x80 form", "80"},
		{"a label running past the end", "0f6669787475726500"},
		{"no root label", "07666978747572650769"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (OptionsV6{{Code: OptV6DomainList, Data: mustHex(tc.data)}}).DomainSearch()
			if !errors.Is(err, ErrV6Name) {
				t.Errorf("DomainSearch(%s) = %v, want %v", tc.data, err, ErrV6Name)
			}
		})
	}

	// Two options, both walked, for the reason DNSServers gives: a client
	// that read only the first would lose half its search list.
	got, err = (OptionsV6{
		{Code: OptV6DomainList, Data: mustHex("0161" + "00")},
		{Code: OptV6DomainList, Data: mustHex("0162" + "00")},
	}).DomainSearch()
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("two domain search options gave %q (%v), want [a b] in wire order", got, err)
	}

	// Two names in one option, the shape the option is FOR.
	got, err = (OptionsV6{{Code: OptV6DomainList, Data: mustHex("0161" + "00" + "0162" + "00")}}).DomainSearch()
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("two names decoded to %q (%v), want [a b]", got, err)
	}
	// The root label alone is the empty name and must not become a phantom
	// entry: a search list of one empty string sends a resolver to the root.
	got, err = (OptionsV6{{Code: OptV6DomainList, Data: mustHex("00")}}).DomainSearch()
	if err != nil {
		t.Fatalf("DomainSearch of a lone root label: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a lone root label decoded to %q, want nothing", got)
	}
}

// TestEncodeDomainSearchRoundTrips is the encoder half, including the two ways
// a name cannot be represented.
func TestEncodeDomainSearchRoundTrips(t *testing.T) {
	names := []string{"fixture.invalid", "a.b.c", "x"}
	b, err := EncodeDomainSearch(names)
	if err != nil {
		t.Fatalf("EncodeDomainSearch: %v", err)
	}
	got, err := (OptionsV6{{Code: OptV6DomainList, Data: b}}).DomainSearch()
	if err != nil {
		t.Fatalf("DomainSearch: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("round trip %q -> %q", names, got)
	}
	for i := range names {
		if got[i] != names[i] {
			t.Errorf("round trip [%d] %q -> %q", i, names[i], got[i])
		}
	}
	if !bytes.Equal(b[:17], mustHex("076669787475726507696e76616c696400")) {
		t.Errorf("the first name encoded to %x, want the octets dnsmasq put on the wire", b[:17])
	}

	// A trailing dot is the ordinary fully-qualified spelling of the same
	// name. It is normalised, not refused; the pair below must encode to the
	// same octets, or a caller's spelling changes what goes on the wire.
	withDot, err := EncodeDomainSearch([]string{"fixture.invalid."})
	if err != nil {
		t.Fatalf("EncodeDomainSearch with a trailing dot: %v", err)
	}
	without, err := EncodeDomainSearch([]string{"fixture.invalid"})
	if err != nil {
		t.Fatalf("EncodeDomainSearch: %v", err)
	}
	if !bytes.Equal(withDot, without) {
		t.Errorf("%q encoded to %x and %q to %x; a trailing dot names the same domain", "fixture.invalid.", withDot, "fixture.invalid", without)
	}

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"a label over 63 octets", strings.Repeat("a", 64)},
		{"an empty label", "a..b"},
		{"an empty name", ""},
		{"a name over 255 octets", strings.Repeat("ab.", 90) + "c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncodeDomainSearch([]string{tc.in}); !errors.Is(err, ErrV6Name) {
				t.Errorf("EncodeDomainSearch(%q) = %v, want %v", tc.in, err, ErrV6Name)
			}
		})
	}
}

// ------------------------------------------------------------------ DUIDs --

// TestDUIDShapes covers §11.4 and §11.5. D30 makes DUID-LL the primary form,
// because a bridge or macvlan endpoint has a stable link address, and
// DUID-UUID the form for ipvlan, where it does not.
func TestDUIDShapes(t *testing.T) {
	mac := mustHex("ea494ee531ed")
	ll, err := DUIDLL(1, mac)
	if err != nil {
		t.Fatalf("DUIDLL: %v", err)
	}
	if !bytes.Equal(ll, mustHex("00030001ea494ee531ed")) {
		t.Errorf("DUID-LL = %x, want 0003 0001 followed by the address (§11.4)", ll)
	}
	if len(ll) != 4+len(mac) {
		t.Errorf("DUID-LL is %d octets, want %d", len(ll), 4+len(mac))
	}
	if _, err := DUIDLL(1, nil); !errors.Is(err, ErrDUID) {
		t.Errorf("DUIDLL with no address: want %v", ErrDUID)
	}

	uuid := mustHex("0123456789abcdef0123456789abcdef")
	du, err := DUIDUUID(uuid)
	if err != nil {
		t.Fatalf("DUIDUUID: %v", err)
	}
	if !bytes.Equal(du, append(mustHex("0004"), uuid...)) {
		t.Errorf("DUID-UUID = %x, want 0004 followed by the 16-octet UUID (§11.5)", du)
	}
	for _, n := range []int{0, 15, 17} {
		if _, err := DUIDUUID(make([]byte, n)); !errors.Is(err, ErrDUID) {
			t.Errorf("DUIDUUID on %d octet(s): want %v", n, ErrDUID)
		}
	}
	if UUIDLen != 16 {
		t.Errorf("UUIDLen = %d, want 16", UUIDLen)
	}

	// A DUID the encoder built must survive a message round trip unchanged: it
	// is the client's name, and §11 makes a client that changes it a different
	// client.
	m, err := EncodeV6(&MessageV6{Type: MsgSolicit, Options: OptionsV6{{Code: OptV6ClientID, Data: ll}}})
	if err != nil {
		t.Fatalf("EncodeV6: %v", err)
	}
	dec, err := DecodeV6(m)
	if err != nil {
		t.Fatalf("DecodeV6: %v", err)
	}
	back, _ := dec.Options.First(OptV6ClientID)
	if !bytes.Equal(back, ll) {
		t.Errorf("client identifier round trip %x -> %x", ll, back)
	}
}

// ------------------------------------------------------- names and ports --

// TestV6NamesAndPorts keeps the constants honest. The ports are §7.2's, and
// they are the one pair a transport gets wrong in a way that produces silence
// rather than an error.
func TestV6NamesAndPorts(t *testing.T) {
	if PortV6Client != 546 || PortV6Server != 547 {
		t.Errorf("ports %d/%d, want 546/547 (§7.2)", PortV6Client, PortV6Server)
	}
	if AllDHCPRelayAgentsAndServers.String() != "ff02::1:2" {
		t.Errorf("All_DHCP_Relay_Agents_and_Servers = %s, want ff02::1:2 (§7.1)", AllDHCPRelayAgentsAndServers)
	}
	for _, typ := range []MessageTypeV6{MsgSolicit, MsgAdvertise, MsgRequest6, MsgReply, MsgRelayForw, MsgRelayRepl} {
		if s := typ.String(); s == "" || strings.HasPrefix(s, "MessageTypeV6(") {
			t.Errorf("message type %d has no name: %q", uint8(typ), s)
		}
	}
	if s := MessageTypeV6(200).String(); !strings.Contains(s, "200") {
		t.Errorf("an unknown message type renders as %q, which does not say which one it was", s)
	}
	for _, c := range []OptionCodeV6{OptV6ClientID, OptV6ServerID, OptV6IANA, OptV6IAAddr, OptV6ORO,
		OptV6ElapsedTime, OptV6StatusCode, OptV6DNSServers, OptV6DomainList, OptV6InfoRefresh,
		OptV6SolMaxRTCode, OptV6InfMaxRTCode} {
		if s := c.String(); s == "" || !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") {
			t.Errorf("option code %d has no name: %q", uint16(c), s)
		}
	}
	if s := OptionCodeV6(999).String(); !strings.Contains(s, "999") {
		t.Errorf("an unassigned option code renders as %q, which does not say which one it was", s)
	}
	m, err := DecodeV6(capAdvertise)
	if err != nil {
		t.Fatalf("DecodeV6: %v", err)
	}
	sum := m.Summary()
	for _, want := range []string{"ADVERTISE", "1a2b3c", "client-id", "server-id", "ia-na"} {
		if !strings.Contains(sum, want) {
			t.Errorf("Summary() = %q, missing %q", sum, want)
		}
	}
}

// ------------------------------------------------------------------ fuzz --

// FuzzDecodeV6Message drives the walk B-6 and A-7 describe with arbitrary
// octets. The property is not "it decodes": it is that a decode either fails or
// round-trips, and never panics, hangs or invents octets.
func FuzzDecodeV6Message(f *testing.F) {
	for _, seed := range [][]byte{capSolicit, capAdvertise, capRequest, capReply} {
		f.Add(seed)
	}
	f.Add([]byte{})
	f.Add(mustHex("011a2b3c0001ffff"))
	f.Add(mustHex("011a2b3c00010000"))
	f.Add(mustHex("0c1a2b3c"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := DecodeV6(b)
		if err != nil {
			if m != nil {
				t.Fatalf("DecodeV6 returned both a message and an error")
			}
			return
		}
		back, err := EncodeV6(m)
		if err != nil {
			t.Fatalf("a decoded message did not re-encode: %v", err)
		}
		if !bytes.Equal(back, b) {
			t.Fatalf("round trip changed the octets:\n  %x\n  %x", b, back)
		}
		// The accessors must be total over anything that decoded.
		_, _, _ = m.Options.Status()
		_, _, _ = m.Options.ElapsedTime()
		_, _ = m.Options.DNSServers()
		_, _ = m.Options.DomainSearch()
		if ias, err := m.Options.IANAs(); err == nil {
			for _, ia := range ias {
				_, _ = ia.Options.Addrs()
			}
		}
		_ = m.Summary()
	})
}
