package proto

import "fmt"

// State is a DHCPv4 client state, named as RFC 2131 section 4.4 names them.
type State uint8

// The states this machine implements, plus Stopped.
//
// INIT-REBOOT IS DELIBERATELY NOT ONE OF THEM. RFC 2131 Figure 5 leaves
// INIT-REBOOT on "-/Send DHCPREQUEST", with no event in between, exactly as it
// leaves INIT for SELECTING on "-/Send DHCPDISCOVER". INIT is nonetheless a
// state because a client can SIT in it — section 4.4.1's desync wait, section
// 3.1(5)'s restart wait, a link that is down. Nothing holds a client in
// INIT-REBOOT: section 4.4.2 describes no wait, and this machine adds none
// (Machine.beginReboot says why). A StateInitReboot would therefore be a value
// no event could be observed in, and it would grow the totality test's (state,
// event) product by ten pairs whose only possible assertion is that nothing
// happens — reporting a larger domain as fully covered, which is the shape the
// paragraph this one replaced refused REBOOTING for.
//
// So INIT-REBOOT is a transition here and REBOOTING is the state.
const (
	// StateStopped is the state before Start and after Stop. It is not an RFC
	// state; it exists so that Step is total over "events that arrive when we
	// are not running", which is otherwise the gap a real client falls into
	// during teardown.
	StateStopped State = iota
	// StateInit is RFC 2131's INIT. Nothing has been sent.
	StateInit
	// StateSelecting is RFC 2131's SELECTING: DISCOVER sent, collecting OFFERs.
	StateSelecting
	// StateRequesting is RFC 2131's REQUESTING: REQUEST sent, awaiting ACK/NAK.
	StateRequesting
	// StateBound is RFC 2131's BOUND: the lease is held.
	StateBound
	// StateRenewing is RFC 2131's RENEWING, entered at T1: the lease is still
	// held and a DHCPREQUEST is in flight, unicast to the server that issued
	// it (section 4.4.5).
	StateRenewing
	// StateRebinding is RFC 2131's REBINDING, entered at T2: the lease is
	// still held and the DHCPREQUEST is broadcast, so that ANY server may
	// answer (section 4.4.5).
	StateRebinding
	// StateRebooting is RFC 2131's REBOOTING: a broadcast DHCPREQUEST naming a
	// remembered address is in flight and NO lease is held yet.
	//
	// Section 4.4.2: "The client begins in INIT-REBOOT state and sends a
	// DHCPREQUEST message ... Once a DHCPACK message with an 'xid' field
	// matching that in the client's DHCPREQUEST message arrives from any
	// server, the client is initialized and moves to BOUND state." Figure 5's
	// other edges out of it are "DHCPNAK/Restart" to INIT and
	// "DHCPOFFER/Discard".
	//
	// It is NOT a renewal state even though the message is a DHCPREQUEST. The
	// three differences are the whole of P-3: 'ciaddr' is zero rather than the
	// client's address, option 50 is a MUST rather than a MUST NOT, and no
	// lease is held, so there is nothing to lose here and nothing to unicast
	// to.
	StateRebooting
	// StateProbing is RFC 5227 section 2.1's check, between the DHCPACK and
	// the point where the address may be used. It is NOT an RFC 2131 state:
	// figure 5 goes from REQUESTING straight to BOUND on "DHCPACK/Record lease,
	// set timers T1, T2".
	//
	// It is a state here by the same test the paragraph above applies to
	// INIT-REBOOT, and it passes where INIT-REBOOT failed: a client SITS in it,
	// for a mean of 5.5 seconds by section 1.1's arithmetic, and observable
	// events arrive while it does — ARP packets, the probe timer, a link going
	// down, the caller releasing. Every one of those has a different answer
	// here than in BOUND, because in BOUND there is a lease to lose and here
	// there is not.
	//
	// ONLY ConflictWait REACHES IT. ConflictAsync probes from BOUND, because
	// its whole definition is that the lease is announced first; ConflictOff
	// probes not at all. So the state is where the caller is WAITING, which is
	// the thing that needs a name.
	StateProbing
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "STOPPED"
	case StateInit:
		return "INIT"
	case StateSelecting:
		return "SELECTING"
	case StateRequesting:
		return "REQUESTING"
	case StateBound:
		return "BOUND"
	case StateRenewing:
		return "RENEWING"
	case StateRebinding:
		return "REBINDING"
	case StateRebooting:
		return "REBOOTING"
	case StateProbing:
		return "PROBING"
	default:
		return fmt.Sprintf("state(%d)", uint8(s))
	}
}

// AllStates is every State this machine can be in, so that the totality test
// (R1) enumerates the domain from one place: a test that hand-lists the states
// drifts the day one is added, in the direction that reports a smaller domain
// as fully covered.
func AllStates() []State {
	return []State{
		StateStopped, StateInit, StateSelecting, StateRequesting, StateBound,
		StateRenewing, StateRebinding, StateRebooting, StateProbing,
	}
}
