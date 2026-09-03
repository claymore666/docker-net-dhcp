package proto

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// The INIT-REBOOT half of RFC 2131, driven in the pure ring. What a real
// server does with these messages is measured against dnsmasq in
// runtime/reboot_dnsmasq_linux_test.go; this file is about what leaves the
// machine, on the ENCODED bytes rather than on the struct, because the struct
// is what the machine believes and the bytes are what a server reads.

// encoded round-trips a sent message through the codec, so an assertion here
// is about what a server would parse and not about a field this package
// happens to have set.
func encoded(t *testing.T, m *wire.Message) *wire.Message {
	t.Helper()
	raw, err := wire.Encode(m)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec, err := wire.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return dec
}

func journalText(acts []Action) string {
	var sb strings.Builder
	for _, a := range acts {
		if a.Kind == ActJournal {
			sb.WriteString(a.Note)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// TestTheInitRebootRequestCarriesOnlyWhatSection432Allows is the message
// itself, cell by cell.
//
// RFC 2131 section 4.3.2, "DHCPREQUEST generated during INIT-REBOOT state":
// "'server identifier' MUST NOT be filled in, 'requested IP address' option
// MUST be filled in with client's notion of its previously assigned address.
// 'ciaddr' MUST be zero." Table 4 (section 4.3.6) repeats those three and adds
// broadcast; Table 5 (section 4.4.1) makes option 50 a MUST "in SELECTING or
// INIT-REBOOT" and option 54 a MUST NOT "after INIT-REBOOT".
//
// The server-identifier row is the one that costs a hang rather than a wrong
// answer: section 4.3.2 has a server read a DHCPREQUEST carrying one as a
// SELECTING request, check it against its own identifier, and stay silent when
// it does not match.
func TestTheInitRebootRequestCarriesOnlyWhatSection432Allows(t *testing.T) {
	p := resumeParams(testRebootAddr, at(3600), true)
	p.ClientID = []byte{0xFF, 0x01, 0x02, 0x03}
	p.Hostname = "m5-client"
	m := newMachine(t, p)

	st, acts := m.Step(0, 0xC0FFEE, Simple(EvStart))
	if st != StateRebooting {
		t.Fatalf("after Start with a remembered lease: %s, want REBOOTING", st)
	}
	msg := encoded(t, mustSend(t, acts, wire.MsgRequest))

	got, ok := msg.Addr4(wire.OptRequestedIP)
	if !ok || got.String() != testRebootAddr {
		t.Fatalf("requested-IP = %v/%v, want %s (RFC 2131 4.3.2, a MUST)", got, ok, testRebootAddr)
	}
	if v, ok := msg.Addr4(wire.OptServerID); ok {
		t.Fatalf("the INIT-REBOOT DHCPREQUEST carries a server identifier (%s); "+
			"RFC 2131 Table 5 makes it a MUST NOT and section 4.3.2 has the server answer with silence", v)
	}
	if msg.CIAddr.IsValid() && !msg.CIAddr.IsUnspecified() {
		t.Fatalf("ciaddr = %s, want zero (RFC 2131 4.3.2 and Table 4)", msg.CIAddr)
	}
	if msg.Flags&wire.FlagBroadcast == 0 {
		t.Fatal("the INIT-REBOOT DHCPREQUEST does not set the BROADCAST flag; a raw-socket client cannot receive a unicast reply")
	}
	if msg.Op != wire.BootRequest {
		t.Fatalf("op = %s, want BOOTREQUEST", msg.Op)
	}
	if _, ok := msg.Options[wire.OptParameterList]; !ok {
		t.Fatal("no parameter request list; section 4.4.2 lets the client ask, and section 4.4.1 makes carrying it in every subsequent message a MUST once it was asked for")
	}
	// Section 3.2(1): "If the client used a 'client identifier' to obtain its
	// address, the client MUST use the same 'client identifier' in the
	// DHCPREQUEST message."
	if v, ok := msg.Options[wire.OptClientID]; !ok || string(v) != string(p.ClientID) {
		t.Fatalf("client identifier = %v/%v, want the one the lease was obtained with (RFC 2131 3.2(1), a MUST)", v, ok)
	}
	// Section 4.4.2: "The client generates and records a random transaction
	// identifier"; section 2's 'secs' counts from the moment the client "began
	// address acquisition", which is this Step.
	if msg.Secs != 0 {
		t.Fatalf("secs = %d at the first send of a fresh transaction, want 0", msg.Secs)
	}
	if a, ok := find(acts, ActSetTimer); !ok || a.Timer != TimerRetransmit {
		t.Fatalf("no retransmit timer armed after the INIT-REBOOT DHCPREQUEST: %v", RenderActions(acts))
	}
	if n := count(acts, ActSend); n != 1 {
		t.Fatalf("%d messages sent, want exactly the one DHCPREQUEST", n)
	}
	// Table 4's fourth cell, which is a property of the DESTINATION and not
	// of the message: "'DHCPREQUEST' generated during INIT-REBOOT state ...
	// MUST be broadcast to the 0xffffffff IP broadcast address". The
	// BROADCAST flag above is Params.Broadcast's doing and would still be set
	// on a message this machine had decided to unicast.
	send, _ := find(acts, ActSend)
	if !send.Dest.Broadcast {
		t.Fatalf("the INIT-REBOOT DHCPREQUEST is addressed to %s, want a broadcast: the client has no address and no server to unicast to", send.Dest)
	}
}

// TestTheInitRebootRequestIsNotTheRenewalRequest is the control on the builder
// separation.
//
// Both messages are DHCPREQUESTs and RFC 2131 Table 4 gives them opposite
// values in two of four cells: INIT-REBOOT is broadcast with 'ciaddr' zero and
// option 50 a MUST, RENEWING is unicast with 'ciaddr' set and option 50 a MUST
// NOT. A single builder serving both would satisfy either test alone.
func TestTheInitRebootRequestIsNotTheRenewalRequest(t *testing.T) {
	reboot := machineIn(t, StateRebooting)
	_, acts := reboot.Step(at(1), 1, TimerFired(TimerRetransmit))
	rb := encoded(t, mustSend(t, acts, wire.MsgRequest))

	renew := machineIn(t, StateRenewing)
	_, acts = renew.Step(at(4000), 1, TimerFired(TimerRetransmit))
	rn := encoded(t, mustSend(t, acts, wire.MsgRequest))

	if _, ok := rb.Addr4(wire.OptRequestedIP); !ok {
		t.Error("the INIT-REBOOT request carries no option 50 (Table 4: MUST)")
	}
	if v, ok := rn.Addr4(wire.OptRequestedIP); ok {
		t.Errorf("the renewal request carries option 50 = %s (Table 4: MUST NOT)", v)
	}
	if rb.CIAddr.IsValid() && !rb.CIAddr.IsUnspecified() {
		t.Errorf("the INIT-REBOOT request's ciaddr = %s (Table 4: zero)", rb.CIAddr)
	}
	if !rn.CIAddr.IsValid() || rn.CIAddr.IsUnspecified() {
		t.Error("the renewal request's ciaddr is zero (Table 4: the client's IP address)")
	}
}

// TestDesyncDoesNotDelayTheInitRebootRequest.
//
// DECISION 2026-09-03. RFC 2131 section 4.4.1 puts the one-to-ten-second wait
// on the DHCPDISCOVER — "The client begins in INIT state and forms a
// DHCPDISCOVER message. The client SHOULD wait a random time between one and
// ten seconds" — and section 4.4.2, which enumerates what a client in
// INIT-REBOOT does, has no wait in it. A wait here would add up to ten seconds
// to every endpoint of every plugin restart for an obligation the RFC does not
// state.
func TestDesyncDoesNotDelayTheInitRebootRequest(t *testing.T) {
	p := resumeParams(testRebootAddr, at(3600), true)
	p.DesyncMin, p.DesyncMax = 1*Second, 10*Second
	m := newMachine(t, p)

	st, acts := m.Step(0, 0x1234, Simple(EvStart))
	if st != StateRebooting {
		t.Fatalf("state = %s, want REBOOTING on the Start step itself", st)
	}
	mustSend(t, acts, wire.MsgRequest)
	for _, a := range acts {
		if a.Kind == ActSetTimer && a.Timer == TimerDesync {
			t.Fatal("a desync timer was armed before the INIT-REBOOT DHCPREQUEST; section 4.4.2 has no wait in it")
		}
	}

	// The control, in the same test: the same window still delays a DISCOVER,
	// so this is a property of the reboot path and not of a desync that has
	// stopped working.
	p2 := testParams()
	p2.DesyncMin, p2.DesyncMax = 1*Second, 10*Second
	m2 := newMachine(t, p2)
	st, acts = m2.Step(0, 0x1234, Simple(EvStart))
	if st != StateInit {
		t.Fatalf("without a remembered lease the same window left the machine in %s, want INIT waiting", st)
	}
	if a, ok := find(acts, ActSetTimer); !ok || a.Timer != TimerDesync {
		t.Fatalf("the desync window no longer delays a DISCOVER, so the assertion above measures nothing: %v", RenderActions(acts))
	}
}

// TestInitRebootBindsOnTheAck is Figure 5's "DHCPACK/Record lease, set timers
// T1, T2" edge out of REBOOTING, and section 4.4.2's lease arithmetic: "The
// client records the lease expiration time as the sum of the time at which the
// DHCPREQUEST message was sent and the duration of the lease from the DHCPACK
// message."
func TestInitRebootBindsOnTheAck(t *testing.T) {
	m := newMachine(t, resumeParams(testRebootAddr, at(3600), true))
	_, acts := m.Step(at(10), 1, Simple(EvStart))
	req := mustSend(t, acts, wire.MsgRequest)

	st, acts := m.Step(at(11), 2, received(t, ackFor(req, testRebootAddr, "192.168.99.1", 3600)))
	if st != StateBound {
		t.Fatalf("after the DHCPACK: %s, want BOUND", st)
	}
	a, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatalf("no lease announced: %v", RenderActions(acts))
	}
	if got := a.Lease.Addr.Addr().String(); got != testRebootAddr {
		t.Fatalf("bound to %s, want the remembered %s", got, testRebootAddr)
	}
	if a.Requested.String() != testRebootAddr {
		t.Fatalf("Action.Requested = %s, want the address the client asked to keep, %s", a.Requested, testRebootAddr)
	}
	// The DHCPREQUEST was sent at t=10, not t=11 when the ACK arrived.
	if a.Lease.Start != at(10) {
		t.Fatalf("the lease starts at %s, want the moment the DHCPREQUEST was sent, %s (RFC 2131 4.4.2)", a.Lease.Start, at(10))
	}
	// The timers come from the ACK, and all three are re-derived: the
	// remembered lease's deadlines are gone the moment a server answers.
	var expire, renew, rebind bool
	for _, x := range acts {
		if x.Kind != ActSetTimer {
			continue
		}
		switch x.Timer {
		case TimerExpire:
			expire = true
		case TimerRenew:
			renew = true
		case TimerRebind:
			rebind = true
		}
	}
	if !expire || !renew || !rebind {
		t.Fatalf("expiry/renew/rebind armed = %v/%v/%v, want all three from the ACK: %v",
			expire, renew, rebind, RenderActions(acts))
	}
}

// TestInitRebootNakRestartsWithADiscover is Figure 5's "DHCPNAK/Restart" edge
// and section 3.2(3): "It must instead request a new address by restarting the
// configuration process, this time using the (non-abbreviated) procedure
// described in section 3.1."
//
// NON-ABBREVIATED is the whole assertion. A restart that sent a second
// INIT-REBOOT DHCPREQUEST for the address the server has just refused would
// satisfy "the client restarted" and would loop forever against a server doing
// exactly what section 4.3.2 tells it to.
func TestInitRebootNakRestartsWithADiscover(t *testing.T) {
	m := newMachine(t, resumeParams(testRebootAddr, at(3600), true))
	_, acts := m.Step(0, 1, Simple(EvStart))
	req := mustSend(t, acts, wire.MsgRequest)

	st, acts := m.Step(at(1), 2, received(t, nakFor(req, "192.168.99.1", "wrong network")))
	if st != StateSelecting {
		t.Fatalf("after the DHCPNAK: %s, want SELECTING — the restart is a DHCPDISCOVER", st)
	}
	f, ok := find(acts, ActFailed)
	if !ok || f.Reason != ReasonNak {
		t.Fatalf("no typed NAK failure reported: %v", RenderActions(acts))
	}
	disc := encoded(t, mustSend(t, acts, wire.MsgDiscover))
	if v, ok := disc.Addr4(wire.OptRequestedIP); ok {
		t.Fatalf("the restart's DHCPDISCOVER asks for %s, the address the server just refused", v)
	}
	if _, ok := m.Lease(); ok {
		t.Fatal("a lease is held after a DHCPNAK")
	}

	// Driven to the end, because the restart's own acquisition is where a
	// remembered address that was not let go shows up: it would be reported
	// as the thing this client asked for, on a DHCPDISCOVER that asked for
	// nothing.
	_, acts = m.Step(at(2), 3, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req2 := mustSend(t, acts, wire.MsgRequest)
	st, acts = m.Step(at(3), 4, received(t, ackFor(req2, "192.168.99.50", "192.168.99.1", 3600)))
	if st != StateBound {
		t.Fatalf("the restart did not complete: %s", st)
	}
	a, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatalf("no acquisition announced: %v", RenderActions(acts))
	}
	if a.Requested.IsValid() {
		t.Fatalf("the acquisition after the DHCPNAK reports having asked for %s; the refused address was not let go", a.Requested)
	}
}

// TestInitRebootTimeoutAcquiresFromInit is the OTHER branch of section 3.2(3),
// and it is a decision rather than a reading: "If the client receives neither a
// DHCPACK or a DHCPNAK message after employing the retransmission algorithm,
// the client MAY choose to use the previously allocated network address and
// configuration parameters for the remainder of the unexpired lease."
//
// The MAY is not taken. See stepRebooting for why, and note what this test
// asserts to hold it: no lease is held after the budget runs out. A machine
// that took the MAY would be in BOUND here with a lease no server confirmed.
func TestInitRebootTimeoutAcquiresFromInit(t *testing.T) {
	p := resumeParams(testRebootAddr, at(3600), true)
	m := newMachine(t, p)
	m.Step(0, 1, Simple(EvStart))

	var st State
	var acts []Action
	for i := 0; i <= p.Request.MaxRetransmissions; i++ {
		st, acts = m.Step(at(int64(10*(i+1))), uint64(i+3), TimerFired(TimerRetransmit))
	}
	if st != StateSelecting {
		t.Fatalf("after the retransmission budget: %s, want SELECTING", st)
	}
	f, ok := find(acts, ActFailed)
	if !ok || f.Reason != ReasonNoServer {
		t.Fatalf("no typed no-server failure reported: %v", RenderActions(acts))
	}
	if _, ok := m.Lease(); ok {
		t.Fatal("the machine holds a lease after an unanswered INIT-REBOOT; RFC 2131 3.2(3)'s MAY is deliberately not taken")
	}
	sent := encoded(t, mustSend(t, acts, wire.MsgDiscover))
	if v, ok := sent.Addr4(wire.OptRequestedIP); ok {
		t.Fatalf("the restart's DHCPDISCOVER asks for %s; the Resume is consumed by one attempt", v)
	}
}

// TestALinkFlapDuringRebootingAcquiresFromInit pins the third exit, which is
// the one the RFC says nothing about.
//
// DECISION 2026-09-03: a link that drops during REBOOTING parks in INIT like
// every other acquisition state, and the link coming back starts an ordinary
// DHCPDISCOVER. The Resume is consumed by the attempt it started; see
// takeResume for the alternative and its price.
func TestALinkFlapDuringRebootingAcquiresFromInit(t *testing.T) {
	m := newMachine(t, resumeParams(testRebootAddr, at(3600), true))
	m.Step(0, 1, Simple(EvStart))

	st, _ := m.Step(at(1), 2, Simple(EvLinkDown))
	if st != StateInit {
		t.Fatalf("after LinkDown in REBOOTING: %s, want INIT", st)
	}
	st, acts := m.Step(at(2), 3, Simple(EvLinkUp))
	if st != StateSelecting {
		t.Fatalf("after LinkUp: %s, want SELECTING", st)
	}
	sent := encoded(t, mustSend(t, acts, wire.MsgDiscover))
	if v, ok := sent.Addr4(wire.OptRequestedIP); ok {
		t.Fatalf("the DHCPDISCOVER after the flap asks for %s; the Resume was already consumed", v)
	}
}

// TestAnExpiredRememberedLeaseIsNotRebooted.
//
// RFC 2131 section 4.3.2: "If the DHCP server has no record of this client,
// then it MUST remain silent, and MAY output a warning to the network
// administrator." An expired lease is the case where no server has a record, so
// an INIT-REBOOT of one buys a retransmission budget of silence and then the
// DHCPDISCOVER that should have gone first.
func TestAnExpiredRememberedLeaseIsNotRebooted(t *testing.T) {
	// The deadline is BEFORE the Instant Start is fed, which is the whole
	// input: ring 1 cannot read a clock, so "expired" is a comparison against
	// the now it is handed.
	m := newMachine(t, resumeParams(testRebootAddr, at(100), true))

	st, acts := m.Step(at(101), 1, Simple(EvStart))
	if st != StateSelecting {
		t.Fatalf("with an expired remembered lease: %s, want SELECTING", st)
	}
	sent := encoded(t, mustSend(t, acts, wire.MsgDiscover))
	if v, ok := sent.Addr4(wire.OptRequestedIP); ok {
		t.Fatalf("the DHCPDISCOVER asks for %s; an expired lease is not offered as a hint by this ring", v)
	}
	if j := journalText(acts); !strings.Contains(j, "has expired") {
		t.Fatalf("nothing in the journal says the remembered lease was dropped for age; the wire shows nothing either.\n%s", j)
	}

	// The control: the SAME machine configuration, one nanosecond earlier,
	// still reboots. Without it this test passes against a resume that never
	// works.
	m2 := newMachine(t, resumeParams(testRebootAddr, at(100), true))
	if st, _ := m2.Step(at(100)-1, 1, Simple(EvStart)); st != StateRebooting {
		t.Fatalf("a lease that has not expired left the machine in %s, want REBOOTING; the expiry check refuses everything", st)
	}

	// The boundary itself. RFC 2131 section 4.4.5 puts the client in INIT
	// when "the lease expires", so the deadline is the first instant at which
	// it is gone rather than the last at which it is held.
	m3 := newMachine(t, resumeParams(testRebootAddr, at(100), true))
	if st, _ := m3.Step(at(100), 1, Simple(EvStart)); st != StateSelecting {
		t.Fatalf("at the expiry instant exactly the machine is in %s, want SELECTING", st)
	}
}

// TestAnInfiniteRememberedLeaseIsAlwaysRebooted. A zero Expire is the
// protocol's 0xFFFFFFFF, which lease.Lease represents as a zero Time and
// proto.Resume as HasExpire false.
func TestAnInfiniteRememberedLeaseIsAlwaysRebooted(t *testing.T) {
	m := newMachine(t, resumeParams(testRebootAddr, 0, false))
	if st, _ := m.Step(at(1_000_000), 1, Simple(EvStart)); st != StateRebooting {
		t.Fatalf("an infinite remembered lease left the machine in %s, want REBOOTING", st)
	}
}

// TestNewRefusesAResumeWithNoUsableAddress. Refused at construction, not
// ignored at Start: a caller that meant to remember an address and handed over
// a zero one would otherwise get the behaviour it was trying to replace, and no
// way to find out.
func TestNewRefusesAResumeWithNoUsableAddress(t *testing.T) {
	for _, c := range []struct {
		what string
		addr netip.Addr
	}{
		{"the zero Addr", netip.Addr{}},
		{"0.0.0.0", netip.MustParseAddr("0.0.0.0")},
		{"an IPv6 address", netip.MustParseAddr("2001:db8::1")},
	} {
		p := testParams()
		p.Resume = &Resume{Addr: c.addr}
		if _, err := New(p); err == nil {
			t.Errorf("%s: New accepted a Resume that cannot fill option 50", c.what)
		}
	}
}

// TestNewClonesTheResumeSoACallerCannotMoveIt. Resume is the one pointer in
// Params; a shared one lets a caller change what the machine is going to ask
// for, between construction and Start.
func TestNewClonesTheResumeSoACallerCannotMoveIt(t *testing.T) {
	r := &Resume{Addr: netip.MustParseAddr(testRebootAddr), Expire: at(3600), HasExpire: true}
	p := testParams()
	p.Resume = r
	m := newMachine(t, p)

	r.Addr = netip.MustParseAddr("192.168.99.200")
	r.HasExpire, r.Expire = true, at(1)

	_, acts := m.Step(0, 1, Simple(EvStart))
	sent := encoded(t, mustSend(t, acts, wire.MsgRequest))
	got, ok := sent.Addr4(wire.OptRequestedIP)
	if !ok || got.String() != testRebootAddr {
		t.Fatalf("the machine asked for %v/%v after the caller moved its Resume; want the address it was constructed with, %s", got, ok, testRebootAddr)
	}
	// And the other direction: Params() must not hand the caller a pointer
	// back into the machine.
	out := m.Params()
	if out.Resume == m.params.Resume {
		t.Fatal("Params() returns the machine's own Resume pointer")
	}
}

// TestEveryAnswerToTheInitRebootRequest is the (state, answer) table for
// REBOOTING, including the four adversarial shapes: an ACK for a different
// address, an ACK from a different server, a DHCPNAK carrying no server
// identifier, and a second ACK after the first.
func TestEveryAnswerToTheInitRebootRequest(t *testing.T) {
	const otherAddr = "192.168.99.88"
	const server = "192.168.99.1"
	const otherServer = "192.168.99.2"

	for _, c := range []struct {
		what  string
		reply func(req *wire.Message) *wire.Message
		state State
		held  bool
	}{
		{
			what:  "an ACK for the remembered address",
			reply: func(r *wire.Message) *wire.Message { return ackFor(r, testRebootAddr, server, 3600) },
			state: StateBound, held: true,
		},
		{
			// RFC 2131 section 4.4.2 conditions acceptance on the xid and on
			// nothing else: "Once a DHCPACK message with an 'xid' field
			// matching that in the client's DHCPREQUEST message arrives from
			// any server, the client is initialized and moves to BOUND state."
			// Accepted, and reported through Action.Requested.
			what:  "an ACK for a DIFFERENT address",
			reply: func(r *wire.Message) *wire.Message { return ackFor(r, otherAddr, server, 3600) },
			state: StateBound, held: true,
		},
		{
			what:  "an ACK from a DIFFERENT server",
			reply: func(r *wire.Message) *wire.Message { return ackFor(r, testRebootAddr, otherServer, 3600) },
			state: StateBound, held: true,
		},
		{
			what: "an ACK with no lease time",
			reply: func(r *wire.Message) *wire.Message {
				a := ackFor(r, testRebootAddr, server, 3600)
				delete(a.Options, wire.OptLeaseTime)
				return a
			},
			state: StateRebooting, held: false,
		},
		{
			what: "an ACK with no yiaddr",
			reply: func(r *wire.Message) *wire.Message {
				a := ackFor(r, testRebootAddr, server, 3600)
				a.YIAddr = netip.Addr{}
				return a
			},
			state: StateRebooting, held: false,
		},
		{
			what: "an ACK on a stale xid",
			reply: func(r *wire.Message) *wire.Message {
				a := ackFor(r, testRebootAddr, server, 3600)
				a.XID = r.XID ^ 0xFFFF
				return a
			},
			state: StateRebooting, held: false,
		},
		{
			what: "an ACK for another host's chaddr",
			reply: func(r *wire.Message) *wire.Message {
				a := ackFor(r, testRebootAddr, server, 3600)
				a.CHAddr = []byte{0x02, 0x42, 0xAC, 0x11, 0x00, 0x99}
				return a
			},
			state: StateRebooting, held: false,
		},
		{
			what:  "a DHCPNAK",
			reply: func(r *wire.Message) *wire.Message { return nakFor(r, server, "wrong network") },
			state: StateSelecting, held: false,
		},
		{
			// RFC 2131 Table 3 makes the server identifier a MUST in a
			// DHCPNAK, and nothing in section 4.4 tells a CLIENT to check it.
			// This machine acts on it: refusing would leave a client bound to
			// an address the only server that knows about it has refused,
			// which is the failure the NAK exists to prevent. Recorded as a
			// decision rather than left as an oversight.
			what: "a DHCPNAK carrying no server identifier",
			reply: func(r *wire.Message) *wire.Message {
				n := nakFor(r, server, "wrong network")
				delete(n.Options, wire.OptServerID)
				return n
			},
			state: StateSelecting, held: false,
		},
		{
			// Figure 5 draws "DHCPOFFER/Discard" on REBOOTING. A server
			// answering this request with an OFFER is treating it as a
			// SELECTING one, which is what section 4.3.2 says the
			// server-identifier option causes.
			what:  "a DHCPOFFER",
			reply: func(r *wire.Message) *wire.Message { return offerFor(r, otherAddr, server) },
			state: StateRebooting, held: false,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			m := newMachine(t, resumeParams(testRebootAddr, at(3600), true))
			_, acts := m.Step(0, 1, Simple(EvStart))
			req := mustSend(t, acts, wire.MsgRequest)

			st, acts := m.Step(at(1), 2, received(t, c.reply(req)))
			if st != c.state {
				t.Fatalf("state = %s, want %s: %v", st, c.state, RenderActions(acts))
			}
			l, held := m.Lease()
			if held != c.held {
				t.Fatalf("holds a lease = %v, want %v", held, c.held)
			}
			if !held {
				return
			}
			// The lease's server identifier comes out of the ACK, never out of
			// the remembered lease — ring 1 was never told what that was.
			sid, _ := c.reply(req).Addr4(wire.OptServerID)
			if l.ServerID != sid {
				t.Fatalf("the lease names server %s, want the one that sent the DHCPACK, %s", l.ServerID, sid)
			}
			a, ok := find(acts, ActLeaseAcquired)
			if !ok {
				t.Fatalf("no acquisition announced: %v", RenderActions(acts))
			}
			if a.Requested.String() != testRebootAddr {
				t.Fatalf("Action.Requested = %s, want %s: a caller cannot see a substituted address otherwise", a.Requested, testRebootAddr)
			}
		})
	}
}

// TestASecondAckAfterRebootingIsDiscarded. RFC 2131 section 3.2's figure 4 is
// explicit about this — "(Subsequent DHCPACKS ignored)" — and the cost of
// getting it wrong is the timers restarting from an ACK for a transaction that
// is over.
func TestASecondAckAfterRebootingIsDiscarded(t *testing.T) {
	m := newMachine(t, resumeParams(testRebootAddr, at(3600), true))
	_, acts := m.Step(0, 1, Simple(EvStart))
	req := mustSend(t, acts, wire.MsgRequest)

	ack := ackFor(req, testRebootAddr, "192.168.99.1", 3600)
	m.Step(at(1), 2, received(t, ack))
	first, held := m.Lease()
	if !held {
		t.Fatal("the first DHCPACK did not bind")
	}

	st, acts := m.Step(at(2), 3, received(t, ack))
	if st != StateBound {
		t.Fatalf("after the duplicate DHCPACK: %s, want BOUND", st)
	}
	if n := count(acts, ActSetTimer); n != 0 {
		t.Fatalf("the duplicate DHCPACK re-armed %d timer(s): %v", n, RenderActions(acts))
	}
	if n := count(acts, ActLeaseAcquired) + count(acts, ActLeaseRenewed) + count(acts, ActLeaseChanged); n != 0 {
		t.Fatalf("the duplicate DHCPACK produced %d lease event(s): %v", n, RenderActions(acts))
	}
	second, _ := m.Lease()
	if second.Start != first.Start {
		t.Fatalf("the duplicate DHCPACK moved the lease start from %s to %s", first.Start, second.Start)
	}
}

// TestASendFailureInRebootingRearmsTheRetransmit is R2 in this state.
//
// Without the REBOOTING arm in noteActionFailed the machine sits with no timer
// armed and no event coming — stopped, silently, in the one state whose whole
// purpose is to be answered or to give up.
func TestASendFailureInRebootingRearmsTheRetransmit(t *testing.T) {
	m := newMachine(t, resumeParams(testRebootAddr, at(3600), true))
	_, acts := m.Step(0, 1, Simple(EvStart))
	send, ok := find(acts, ActSend)
	if !ok {
		t.Fatal("nothing was sent")
	}

	st, acts := m.Step(at(1), 2, ActionFailed(send.ID, "network is down"))
	if st != StateRebooting {
		t.Fatalf("after a failed send: %s, want REBOOTING", st)
	}
	a, ok := find(acts, ActSetTimer)
	if !ok || a.Timer != TimerRetransmit {
		t.Fatalf("no retransmit timer re-armed after a failed send: %v", RenderActions(acts))
	}
}

// TestARequestedAddressIsReportedWhenTheServerSubstitutes is the other half of
// the same rule, on the INIT path: RFC 2131 section 4.4.1 makes option 50 in a
// DHCPDISCOVER a MAY, so a server is free to offer something else, and a caller
// that cannot see the difference applies an address it did not ask for.
func TestARequestedAddressIsReportedWhenTheServerSubstitutes(t *testing.T) {
	p := testParams()
	p.RequestedIP = netip.MustParseAddr("192.168.99.60")
	m := newMachine(t, p)

	_, acts := m.Step(0, 1, Simple(EvStart))
	disc := encoded(t, mustSend(t, acts, wire.MsgDiscover))
	if got, ok := disc.Addr4(wire.OptRequestedIP); !ok || got != p.RequestedIP {
		t.Fatalf("the DHCPDISCOVER asks for %v/%v, want %s (RFC 2131 4.4.1, a MAY the caller took)", got, ok, p.RequestedIP)
	}

	_, acts = m.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.61", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	_, acts = m.Step(at(2), 3, received(t, ackFor(req, "192.168.99.61", "192.168.99.1", 3600)))

	a, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatalf("no acquisition announced: %v", RenderActions(acts))
	}
	if a.Requested != p.RequestedIP {
		t.Fatalf("Action.Requested = %s, want the address the DHCPDISCOVER asked for, %s", a.Requested, p.RequestedIP)
	}
	if a.Requested == a.Lease.Addr.Addr() {
		t.Fatal("this fixture no longer substitutes an address, so it cannot show the report")
	}
	if !strings.Contains(a.String(), "asked for") {
		t.Fatalf("the rendered action hides the substitution: %s", a)
	}
}

// TestAnAcquisitionThatAskedForNothingReportsNothing is the preservation
// control on Action.Requested: the ordinary acquisition, which asks for no
// particular address, must carry the zero value and not the address it got.
func TestAnAcquisitionThatAskedForNothingReportsNothing(t *testing.T) {
	m2 := newMachine(t, testParams())
	_, acts := m2.Step(0, 1, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)
	_, acts = m2.Step(at(1), 2, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	_, acts = m2.Step(at(2), 3, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))

	a, ok := find(acts, ActLeaseAcquired)
	if !ok {
		t.Fatalf("no acquisition announced: %v", RenderActions(acts))
	}
	if a.Requested.IsValid() {
		t.Fatalf("Action.Requested = %s on an acquisition that asked for nothing", a.Requested)
	}
	if strings.Contains(a.String(), "asked for") {
		t.Fatalf("the rendered action invents a request: %s", a)
	}
}

// TestARebootedLeaseRenewsLikeAnyOther is the join between P-3 and M3: a lease
// obtained through INIT-REBOOT reaches RENEWING at T1 and unicasts to the
// server that ACKed it, which is the server the remembered lease never told
// ring 1 about.
func TestARebootedLeaseRenewsLikeAnyOther(t *testing.T) {
	m := newMachine(t, resumeParams(testRebootAddr, at(3600), true))
	_, acts := m.Step(0, 1, Simple(EvStart))
	req := mustSend(t, acts, wire.MsgRequest)
	m.Step(at(1), 2, received(t, ackFor(req, testRebootAddr, "192.168.99.1", 3600)))

	t1, ok := m.lease.RenewAt()
	if !ok {
		t.Fatal("the rebooted lease has no T1")
	}
	st, acts := m.Step(t1, 3, TimerFired(TimerRenew))
	if st != StateRenewing {
		t.Fatalf("at T1: %s, want RENEWING", st)
	}
	a, ok := find(acts, ActSend)
	if !ok {
		t.Fatalf("nothing sent at T1: %v", RenderActions(acts))
	}
	if a.Dest.Broadcast || a.Dest.Addr.String() != "192.168.99.1" {
		t.Fatalf("the renewal went to %s, want a unicast to the server that ACKed the reboot", a.Dest)
	}
	renewal := encoded(t, a.Msg)
	if v, ok := renewal.Addr4(wire.OptRequestedIP); ok {
		t.Fatalf("the renewal carries option 50 = %s (Table 4: MUST NOT in RENEWING)", v)
	}
	if renewal.CIAddr.String() != testRebootAddr {
		t.Fatalf("the renewal's ciaddr = %s, want %s", renewal.CIAddr, testRebootAddr)
	}
}
