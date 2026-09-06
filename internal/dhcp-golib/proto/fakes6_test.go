package proto

import (
	"encoding/hex"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// The v6 machine's test fixtures. They sit beside fakes_test.go's v4 ones and
// are built the same way, which is D30 at the level of the test suite: a
// helper that assembled a v6 message differently from the way the v4 helpers
// assemble a v4 one would make every comparison between the two families an
// argument about the helpers.

// The captured exchange's own identifiers, so a test that drives the machine
// with the captured bytes can configure it to be the client that sent them.
// See captured6_test.go for where the bytes came from.
const (
	capXIDSolicit uint64 = 0x1a2b3c
	capXIDRequest uint64 = 0x4d5e6f
	capIAID       uint32 = 0x0a0b0c0d
)

// capDUID is the DUID-LL the captured client sent: DUID type 3, hardware type
// 1, link-layer address ea:49:4e:e5:31:ed. dnsmasq printed it back in its own
// log as "00:03:00:01:ea:49:4e:e5:31:ed".
var capDUID = mustHexBytes("00030001ea494ee531ed")

func mustHexBytes(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func testParams6() Params6 {
	p := DefaultParams6()
	p.DUID = append([]byte(nil), capDUID...)
	p.IAID = capIAID
	p.ORO = DefaultORO()
	return p
}

func newMachine6(t *testing.T, p Params6) *Machine6 {
	t.Helper()
	m, err := New6(p)
	if err != nil {
		t.Fatalf("New6: %v", err)
	}
	return m
}

func mustSendV6(t *testing.T, acts []Action, want wire.MessageTypeV6) *wire.MessageV6 {
	t.Helper()
	for _, a := range acts {
		if a.Kind != ActSendV6 {
			continue
		}
		if a.MsgV6 == nil {
			t.Fatalf("an ActSendV6 carried no message: %v", a)
		}
		if a.MsgV6.Type != want {
			t.Fatalf("sent a %s, want a %s", a.MsgV6.Type, want)
		}
		return a.MsgV6
	}
	t.Fatalf("no %s was sent: %v", want, acts)
	return nil
}

func hasSendV6(acts []Action, want wire.MessageTypeV6) bool {
	for _, a := range acts {
		if a.Kind == ActSendV6 && a.MsgV6 != nil && a.MsgV6.Type == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------ message building --

func optClientID(duid []byte) wire.OptionV6 {
	return wire.OptionV6{Code: wire.OptV6ClientID, Data: duid}
}

func optServerID(duid []byte) wire.OptionV6 {
	return wire.OptionV6{Code: wire.OptV6ServerID, Data: duid}
}

func optStatus(c wire.StatusCode) wire.OptionV6 {
	return wire.OptionV6{Code: wire.OptV6StatusCode, Data: wire.EncodeStatus(wire.Status{Code: c})}
}

func optPreference(v uint8) wire.OptionV6 {
	return wire.OptionV6{Code: wire.OptV6Preference, Data: []byte{v}}
}

func optU32(c wire.OptionCodeV6, v uint32) wire.OptionV6 {
	return wire.OptionV6{Code: c, Data: []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}}
}

type iaAddrSpec struct {
	addr            string
	preferred, vali uint32
}

func optIANA(t *testing.T, iaid, t1, t2 uint32, addrs []iaAddrSpec, extra ...wire.OptionV6) wire.OptionV6 {
	t.Helper()
	ia := &wire.IANA{IAID: iaid, T1: t1, T2: t2}
	for _, a := range addrs {
		v, err := wire.EncodeIAAddr(&wire.IAAddr{
			Addr:              netip.MustParseAddr(a.addr),
			PreferredLifetime: a.preferred,
			ValidLifetime:     a.vali,
		})
		if err != nil {
			t.Fatalf("EncodeIAAddr: %v", err)
		}
		ia.Options = append(ia.Options, wire.OptionV6{Code: wire.OptV6IAAddr, Data: v})
	}
	ia.Options = append(ia.Options, extra...)
	v, err := wire.EncodeIANA(ia)
	if err != nil {
		t.Fatalf("EncodeIANA: %v", err)
	}
	return wire.OptionV6{Code: wire.OptV6IANA, Data: v}
}

// optIAAddrTop is one §21.6 IA Address option at the TOP LEVEL of a message,
// outside any IA_NA. It exists because that is a shape a server can send and
// this client must refuse, and because a fixture that can only build the legal
// nesting cannot drive the refusal.
func optIAAddrTop(t *testing.T, a iaAddrSpec) wire.OptionV6 {
	t.Helper()
	v, err := wire.EncodeIAAddr(&wire.IAAddr{
		Addr:              netip.MustParseAddr(a.addr),
		PreferredLifetime: a.preferred,
		ValidLifetime:     a.vali,
	})
	if err != nil {
		t.Fatalf("EncodeIAAddr: %v", err)
	}
	return wire.OptionV6{Code: wire.OptV6IAAddr, Data: v}
}

var testServerDUID = mustHexBytes("00010001322ecbbfea494ee531ed")

// msgV6 assembles a server message, ENCODED AND RE-DECODED rather than handed
// to the machine as a struct.
//
// The round trip is the point: a test that built the struct directly would
// drive ring 1 with a message ring 0 might not be able to produce, and the
// admission gate this suite spends most of its rows on would be tested against
// inputs the wire cannot deliver.
func msgV6(t *testing.T, typ wire.MessageTypeV6, xid uint32, opts ...wire.OptionV6) (*wire.MessageV6, []byte) {
	t.Helper()
	raw, err := wire.EncodeV6(&wire.MessageV6{Type: typ, XID: xid, Options: opts})
	if err != nil {
		t.Fatalf("EncodeV6: %v", err)
	}
	msg, err := wire.DecodeV6(raw)
	if err != nil {
		t.Fatalf("DecodeV6: %v", err)
	}
	return msg, raw
}

func receivedV6(t *testing.T, typ wire.MessageTypeV6, xid uint32, opts ...wire.OptionV6) Event {
	t.Helper()
	msg, raw := msgV6(t, typ, xid, opts...)
	return ReceivedV6(msg, raw)
}

// advertise is a plain, usable Advertise for xid with one address.
func advertise(t *testing.T, xid uint32, pref uint8) Event {
	t.Helper()
	return receivedV6(t, wire.MsgAdvertise, xid,
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, []iaAddrSpec{{"fd00:99::183", 300, 300}}),
		optPreference(pref))
}

// reply is a plain, usable Reply for xid with one address.
func reply(t *testing.T, xid uint32, addr string) Event {
	t.Helper()
	return receivedV6(t, wire.MsgReply, xid,
		optClientID(capDUID), optServerID(testServerDUID),
		optIANA(t, capIAID, 150, 240, []iaAddrSpec{{addr, 300, 300}}))
}

// ------------------------------------------------------------- driving --

// solicit6 takes a fresh machine as far as SELECTING and returns it with the
// Solicit it sent.
func solicit6(t *testing.T, p Params6) (*Machine6, *wire.MessageV6) {
	t.Helper()
	m := newMachine6(t, p)
	if s, _ := m.Step(at(0), 0, Simple(EvStart)); s != State6Init {
		t.Fatalf("EvStart left the machine in %s, want %s", s, State6Init)
	}
	s, acts := m.Step(at(1), capXIDSolicit, TimerFired(Timer6Delay))
	if s != State6Selecting {
		t.Fatalf("the delay expiring left the machine in %s, want %s", s, State6Selecting)
	}
	return m, mustSendV6(t, acts, wire.MsgSolicit)
}

// bind6 takes a fresh machine all the way to BOUND on addr, through the
// Advertise, the Reply and the duplicate address detection that gates it.
func bind6(t *testing.T, p Params6, addr string) *Machine6 {
	t.Helper()
	m, _ := solicit6(t, p)
	if s, _ := m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255)); s != State6Requesting {
		t.Fatalf("a preference-255 Advertise left the machine in %s", s)
	}
	s, _ := m.Step(at(3), 0, reply(t, uint32(capXIDRequest), addr))
	if s != State6DAD {
		t.Fatalf("the Reply left the machine in %s, want %s", s, State6DAD)
	}
	s, _ = m.Step(at(4), 0, DADResult(netip.MustParseAddr(addr), false))
	if s != State6Bound {
		t.Fatalf("a clean DAD result left the machine in %s, want %s", s, State6Bound)
	}
	return m
}

// leaseOrigin6 is how a machine came to hold the lease a test is about.
//
// THE TWO ORIGINS ARE A FIXTURE DIMENSION AND NOT TWO TESTS, and the reason is
// MEASURED 2026-09-06: every v6 fixture in round 1 came through bind6, which
// is the Solicit path, and the Solicit path was the only one that set the
// machine's server identity. Everything a Decline needs was therefore present
// in every test and absent from every resume — a client that came back from a
// restart, confirmed its addresses and found one in use journalled "declining
// it" and sent nothing. A test that asserts a Decline runs against BOTH.
type leaseOrigin6 struct {
	name string
	// dad leaves the machine in DAD6 with dnsmasqLeasedAddr pending.
	dad func(t *testing.T, p Params6) *Machine6
	// bound leaves the machine in BOUND6 holding dnsmasqLeasedAddr.
	bound func(t *testing.T, p Params6) *Machine6
}

// leaseOrigins6 is every way this machine can come to hold a lease.
func leaseOrigins6() []leaseOrigin6 {
	return []leaseOrigin6{
		{
			name: "solicited",
			dad: func(t *testing.T, p Params6) *Machine6 {
				t.Helper()
				m, _ := solicit6(t, p)
				m.Step(at(2), capXIDRequest, advertise(t, uint32(capXIDSolicit), 255))
				if s, _ := m.Step(at(3), 0, reply(t, uint32(capXIDRequest), dnsmasqLeasedAddr)); s != State6DAD {
					t.Fatalf("the solicited Reply left the machine in %s, want %s", s, State6DAD)
				}
				return m
			},
			bound: func(t *testing.T, p Params6) *Machine6 {
				t.Helper()
				return bind6(t, p, dnsmasqLeasedAddr)
			},
		},
		{
			name: "resumed",
			dad:  confirmed6,
			bound: func(t *testing.T, p Params6) *Machine6 {
				t.Helper()
				m := confirmed6(t, p)
				if s, _ := m.Step(at(3), 3, DADResult(addr6(dnsmasqLeasedAddr), false)); s != State6Bound {
					t.Fatalf("the confirmed address did not bind: %s", s)
				}
				return m
			},
		},
	}
}

// confirmed6 takes a machine through the resume path to DAD6: EvStart with a
// Resume6, the Confirm, and a Reply saying Success. §18.2.10.3: "If the client
// receives any Reply messages that indicate a status of Success (explicit or
// implicit), the client can use the addresses in the IA".
func confirmed6(t *testing.T, p Params6) *Machine6 {
	t.Helper()
	m, acts := confirming6(t, p)
	conf := mustSendV6(t, acts, wire.MsgConfirm)
	s, _ := m.Step(at(2), 3, receivedV6(t, wire.MsgReply, conf.XID,
		optClientID(capDUID), optServerID(testServerDUID), optStatus(wire.StatusSuccess)))
	if s != State6DAD {
		t.Fatalf("a confirming Reply left the machine in %s, want %s", s, State6DAD)
	}
	return m
}

// addr6 parses a v6 literal for a test.
func addr6(s string) netip.Addr { return netip.MustParseAddr(s) }

// confirming6 takes a fresh machine to CONFIRMING through the resume path and
// returns it with the actions that carried the Confirm.
func confirming6(t *testing.T, p Params6) (*Machine6, []Action) {
	t.Helper()
	p.Resume = &Resume6{
		Addrs:      []Addr6{{Addr: addr6(dnsmasqLeasedAddr), Preferred: 300 * Second, Valid: 300 * Second}},
		ServerDUID: append([]byte(nil), testServerDUID...),
		T1:         150 * Second,
		T2:         240 * Second,
	}
	m := newMachine6(t, p)
	if s, _ := m.Step(at(0), 0, Simple(EvStart)); s != State6Init {
		t.Fatalf("EvStart with a resume left the machine in %s", s)
	}
	s, acts := m.Step(at(1), capXIDSolicit, TimerFired(Timer6Delay))
	if s != State6Confirming {
		t.Fatalf("a live resume left the machine in %s, want %s (§18.2.12)", s, State6Confirming)
	}
	return m, acts
}
