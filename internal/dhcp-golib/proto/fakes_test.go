package proto

import (
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// The fixtures every proto test is built from. They live in their own file so
// that switching off a test file never takes the fixtures its neighbours are
// built on with it.
var testCHAddr = []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x02}

// at builds an Instant n seconds after the machine's zero. Instant and
// Duration are distinct types on purpose — a point in time and a length of
// time are not interchangeable, and the compiler enforcing that is the reason
// this helper exists rather than a plain multiplication at every call site.
func at(sec int64) Instant { return Instant(sec) * Instant(Second) }

// testParams is the parameter set the acquisition tests use. Desync is
// disabled so the DISCOVER leaves on the Start step: the delay is exercised by
// TestDesyncDelay on its own, and leaving it on here would make every other
// test carry a timer fire that has nothing to do with what it asserts.
func testParams() Params {
	p := DefaultParams(testCHAddr)
	p.DesyncMin = 0
	p.DesyncMax = 0
	// ConflictOff, EXPLICITLY, and not because RFC 5227 is optional here.
	//
	// The zero value of Params.Conflict is ConflictWait, so a client built
	// with no opinion gets the safe mode; that is deliberate and
	// TestTheDefaultConflictModeIsWait pins it. What these fixtures need is
	// the opposite: their subject is RFC 2131's state machine, and leaving
	// ACD on would put five and a half seconds of section 1.1 arithmetic and
	// a PROBING state between every DHCPACK and every assertion about BOUND —
	// so each of them would be measuring RFC 5227 instead of what it is named
	// for.
	//
	// The ACD fixtures say ConflictWait or ConflictAsync at their own line.
	// The same pattern as DesyncMin/DesyncMax above, which are zeroed here and
	// pinned to the RFC's window by TestDesyncWindowIsWithinTheRFC.
	p.Conflict = ConflictOff
	return p
}

// acdParams is testParams with conflict detection ON and RFC 5227 section
// 1.1's SCHEDULE constants scaled down to nanoseconds.
//
// THE SCALE IS THE ONLY THING THAT CHANGES: the counts, the ordering and the
// ratios are the RFC's, so a test that reads three probes here reads three
// probes in production. The real durations are pinned once, by
// TestACDConstantsAreTheRFCValues against DefaultACDParams, and measured once
// on the wire by the netns run — no other test pays for them.
//
// RATE_LIMIT_INTERVAL IS NOT SCALED, and round 2 is why. It is not a schedule
// constant: nothing waits on it, it is composed onto the DHCPDECLINE restart
// delay and the composition is the only place it acts. Round 1 scaled it to
// 600ns beside a RestartDelay of ten seconds, so the maximum of the two was
// the ten seconds whatever the rate limit said, and the reviewer's mutant
// deleting the composition survived: the fixture had made the two answers the
// same number. At the RFC's own 60s against the RFC's own 10s floor they
// differ by construction. TestTheACDFixtureCanSeeTheRateLimit refuses a
// future edit that scales it back down.
func acdParams(mode ConflictMode) Params {
	p := testParams()
	p.Conflict = mode
	p.ACD = ACDParams{
		ProbeWait:         3 * Nanosecond,
		ProbeNum:          3,
		ProbeMin:          4 * Nanosecond,
		ProbeMax:          5 * Nanosecond,
		AnnounceWait:      6 * Nanosecond,
		AnnounceNum:       2,
		AnnounceInterval:  7 * Nanosecond,
		MaxConflicts:      10,
		RateLimitInterval: 60 * Second,
		DefendInterval:    8 * Nanosecond,
	}
	return p
}

// testRebootAddr is the address the INIT-REBOOT fixtures remember. It is
// deliberately NOT the address the acquisition fixtures lease
// (192.168.99.50), so a test that confuses "the address we asked to keep"
// with "the address the server handed out" fails rather than passing by
// coincidence.
const testRebootAddr = "192.168.99.77"

// resumeParams is testParams with a remembered lease attached.
func resumeParams(addr string, expire Instant, hasExpire bool) Params {
	p := testParams()
	p.Resume = &Resume{
		Addr:      netip.MustParseAddr(addr),
		Expire:    expire,
		HasExpire: hasExpire,
	}
	return p
}

func newMachine(t *testing.T, p Params) *Machine {
	t.Helper()
	m, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// find returns the first action of the given kind.
func find(acts []Action, k ActionKind) (Action, bool) {
	for _, a := range acts {
		if a.Kind == k {
			return a, true
		}
	}
	return Action{}, false
}

func count(acts []Action, k ActionKind) int {
	n := 0
	for _, a := range acts {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func mustSend(t *testing.T, acts []Action, want wire.MessageType) *wire.Message {
	t.Helper()
	a, ok := find(acts, ActSend)
	if !ok {
		t.Fatalf("no ActSend in %v", RenderActions(acts))
	}
	got, ok := a.Msg.Type()
	if !ok {
		t.Fatalf("sent message carries no DHCP message type")
	}
	if got != want {
		t.Fatalf("sent %s, want %s", got, want)
	}
	return a.Msg
}

// offerFor builds a DHCPOFFER answering the given request.
func offerFor(req *wire.Message, yiaddr, serverID string) *wire.Message {
	m := &wire.Message{
		Op:     wire.BootReply,
		HType:  wire.HTypeEthernet,
		XID:    req.XID,
		YIAddr: netip.MustParseAddr(yiaddr),
		CHAddr: append([]byte(nil), req.CHAddr...),
		Options: wire.Options{
			wire.OptMessageType: {byte(wire.MsgOffer)},
			wire.OptServerID:    addr4(serverID),
			wire.OptSubnetMask:  {255, 255, 255, 0},
			wire.OptRouter:      addr4("192.168.99.1"),
			wire.OptDNSServer:   addr4("192.168.99.1"),
			wire.OptLeaseTime:   u32(3600),
		},
	}
	return m
}

func ackFor(req *wire.Message, yiaddr, serverID string, lease uint32) *wire.Message {
	m := offerFor(req, yiaddr, serverID)
	m.Options[wire.OptMessageType] = []byte{byte(wire.MsgAck)}
	m.Options[wire.OptLeaseTime] = u32(lease)
	m.Options[wire.OptDomainName] = []byte("example.test")
	m.Options[wire.OptInterfaceMTU] = []byte{0x05, 0xDC}
	return m
}

func nakFor(req *wire.Message, serverID, text string) *wire.Message {
	return &wire.Message{
		Op: wire.BootReply, HType: wire.HTypeEthernet, XID: req.XID,
		CHAddr: append([]byte(nil), req.CHAddr...),
		Options: wire.Options{
			wire.OptMessageType: {byte(wire.MsgNak)},
			wire.OptServerID:    addr4(serverID),
			wire.OptMessage:     []byte(text),
		},
	}
}

func addr4(s string) []byte {
	a := netip.MustParseAddr(s).As4()
	return a[:]
}

func u32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// received wraps a message as an EvReceived carrying its wire form, the way
// ring 2 does. Encoding it is not ceremony: the journal replays from Raw, so a
// test that fed a hand-built struct would exercise a path replay never takes.
func received(t *testing.T, m *wire.Message) Event {
	t.Helper()
	raw, err := wire.Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec, err := wire.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return Received(dec, raw)
}

func machineIn(t *testing.T, s State) *Machine {
	t.Helper()
	m := newMachine(t, testParams())
	switch s {
	case StateStopped:
		return m
	case StateInit:
		// INIT with nothing armed: start, then drop the link.
		m.Step(0, 1, Simple(EvStart))
		m.Step(at(1), 1, Simple(EvLinkDown))
	case StateSelecting:
		m.Step(0, 1, Simple(EvStart))
	case StateRequesting:
		_, acts := m.Step(0, 1, Simple(EvStart))
		req := mustSend(t, acts, wire.MsgDiscover)
		m.Step(at(1), 2, received(t, offerFor(req, "192.168.99.50", "192.168.99.1")))
	case StateBound:
		_, acts := m.Step(0, 1, Simple(EvStart))
		disc := mustSend(t, acts, wire.MsgDiscover)
		_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
		req := mustSend(t, acts, wire.MsgRequest)
		m.Step(at(2), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	case StateRenewing:
		// Reached by its only door: T1 on a held lease. The instant comes
		// from the lease itself rather than from a number written here, so
		// the fixture cannot drift from the arithmetic it is standing on.
		m = machineIn(t, StateBound)
		t1, ok := m.lease.RenewAt()
		if !ok {
			t.Fatal("the BOUND fixture's lease has no T1; RENEWING is unreachable")
		}
		m.Step(t1, 4, TimerFired(TimerRenew))
	case StateRebooting:
		// Reached by its only door: a Start on a machine that was handed a
		// remembered lease. Building it by assignment would prove nothing, and
		// here it would prove less than nothing — REBOOTING's whole content is
		// what the transition into it put on the wire.
		m = newMachine(t, resumeParams(testRebootAddr, at(3600), true))
		m.Step(0, 1, Simple(EvStart))
	case StateRebinding:
		m = machineIn(t, StateRenewing)
		t2, ok := m.lease.RebindAt()
		if !ok {
			t.Fatal("the RENEWING fixture's lease has no T2; REBINDING is unreachable")
		}
		m.Step(t2, 5, TimerFired(TimerRebind))
	case StateProbing:
		// Reached by its only door: a DHCPACK at a ConflictWait client. There
		// is no other way in, and building one by assignment would skip the
		// thing PROBING is — an ACKed lease that has not been announced.
		m = newMachine(t, acdParams(ConflictWait))
		_, acts := m.Step(0, 1, Simple(EvStart))
		disc := mustSend(t, acts, wire.MsgDiscover)
		_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
		req := mustSend(t, acts, wire.MsgRequest)
		m.Step(at(2), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	default:
		t.Fatalf("machineIn does not know how to reach %s — a new state was added without extending the totality fixture", s)
	}
	if m.State() != s {
		t.Fatalf("fixture reached %s, want %s", m.State(), s)
	}
	return m
}

// ------------------------------------------------------------ acquisition --

// TestAcquisition is done-condition (c): the whole INIT-to-BOUND path, table
// driven, with no root, no namespace, no network and no clock.
