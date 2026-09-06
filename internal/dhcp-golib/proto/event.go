package proto

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// EventKind identifies what happened. It is a closed set: AllEventKinds
// enumerates it, and the totality test crosses that with AllStates.
type EventKind uint8

// The events the machine understands.
const (
	// EvStart begins the acquisition. In INIT it sends the first DISCOVER.
	EvStart EventKind = iota
	// EvStop ends it. Every timer is cancelled and the lease, if any, is
	// reported lost with reason Stopped.
	EvStop
	// EvReceived carries a decoded message that arrived on the wire. The
	// machine does the xid and message-type filtering, not the transport:
	// "silently discard a DHCPOFFER whose xid does not match" (RFC 2131
	// section 4.4.1) is a protocol rule and belongs in the pure ring where it
	// can be tested without a socket.
	EvReceived
	// EvTimerFired carries the id of a timer that ring 3 set and has now
	// fired.
	EvTimerFired
	// EvLinkDown says the interface lost carrier.
	EvLinkDown
	// EvLinkUp says it came back.
	EvLinkUp
	// EvConflictDetected says something else is using the address we hold.
	// M1 does not detect conflicts — that is M4 — but the event exists from
	// the start so the machine is total over it rather than acquiring a
	// meaning later.
	EvConflictDetected
	// EvAddressLost says the address went away underneath us.
	EvAddressLost
	// EvActionFailed says an action the machine emitted did not happen.
	//
	// R2, and the reason the action list carries ids: a machine that assumes
	// its actions succeeded believes things that are not true. Built now
	// because it is unretrofittable — every transition that emits an action
	// has to have decided what happens when that action fails.
	EvActionFailed
	// EvRelease says the caller no longer requires the address and wants the
	// lease given back: RFC 2131 section 4.4.6's "the client no longer
	// requires use of its assigned network address (e.g., the client is
	// gracefully shut down)".
	//
	// It is an input rather than a method because the machine is pure and
	// because a release must serialise with everything else the machine is
	// doing. It is NOT EvStop: Stop ends the client and keeps the binding at
	// the server, release gives the binding back.
	EvRelease
	// EvARPReceived carries one decoded ARP packet that arrived on the link.
	//
	// It is DISTINCT from EvConflictDetected, which is a caller's verdict.
	// This is evidence: RFC 5227's rules decide whether it is a conflict, and
	// they decide differently depending on the phase — section 2.1.1's rules
	// during the probe window, section 2.4's afterwards — so a packet that is
	// a conflict at one moment is ordinary traffic at another. Collapsing the
	// two would move that decision to whoever owns the socket.
	EvARPReceived
	// EvRouterAdvert carries one decoded ICMPv6 Router Advertisement, RFC
	// 4861 section 4.2. It is the ONE thing router discovery contributes to a
	// DHCPv6 client: whether the network says addresses are available over
	// DHCPv6 (the M flag), whether it says other configuration is (the O
	// flag), and which prefixes it advertises.
	//
	// DECLARED HERE, CONSUMED IN M7b. No state has an arm for it today, so it
	// takes the same path EvARPReceived takes in a state that does not handle
	// it: journalled as ignored. The kind exists now so that the ring gates,
	// the totality test and the journal round trip see the shape before the
	// machine that acts on it, rather than after.
	EvRouterAdvert
	// EvDADResult carries the outcome of RFC 4862 section 5.4's duplicate
	// address detection on an address this client was offered.
	//
	// It is the v6 analogue of EvConflictDetected and it is NOT the same
	// thing. During acquisition it is a GATE and not a fault: RFC 9915
	// section 18.2.10.1 makes the client perform DAD before it uses an
	// address, so the machine emits no Acquired until a result with
	// Duplicate false arrives. EvConflictDetected is a verdict about an
	// address already in use; this is the answer to a question the client
	// asked.
	EvDADResult
)

func (k EventKind) String() string {
	switch k {
	case EvStart:
		return "Start"
	case EvStop:
		return "Stop"
	case EvReceived:
		return "Received"
	case EvTimerFired:
		return "TimerFired"
	case EvLinkDown:
		return "LinkDown"
	case EvLinkUp:
		return "LinkUp"
	case EvConflictDetected:
		return "ConflictDetected"
	case EvAddressLost:
		return "AddressLost"
	case EvActionFailed:
		return "ActionFailed"
	case EvRelease:
		return "Release"
	case EvARPReceived:
		return "ARPReceived"
	case EvRouterAdvert:
		return "RouterAdvert"
	case EvDADResult:
		return "DADResult"
	default:
		return fmt.Sprintf("event(%d)", uint8(k))
	}
}

// AllEventKinds is every EventKind. See AllStates for why this exists.
func AllEventKinds() []EventKind {
	return []EventKind{
		EvStart, EvStop, EvReceived, EvTimerFired, EvLinkDown, EvLinkUp,
		EvConflictDetected, EvAddressLost, EvActionFailed, EvRelease,
		EvARPReceived, EvRouterAdvert, EvDADResult,
	}
}

// Event is one input to Step.
type Event struct {
	Kind EventKind

	// Msg is set when Kind is EvReceived, and may be nil even then. A
	// transport handing the machine a nil message is a bug; the machine
	// ignores it rather than panicking, because R1 says Step is total and a
	// panic in ring 1 takes the whole plugin down.
	Msg *wire.Message

	// MsgV6 is set when Kind is EvReceived on the v6 machine, and may be nil
	// even then, for the reason Msg may.
	//
	// A SECOND FIELD RATHER THAN AN INTERFACE, because the two are decoded by
	// two functions with two error sets and consumed by two machines: an
	// Event carrying "a message" would put a type switch in front of every
	// arm that reads one, and the arm that forgot it would compile.
	MsgV6 *wire.MessageV6

	// Raw is the bytes Msg was decoded from, when available. The journal
	// stores it so a replay re-decodes rather than trusting an already-decoded
	// struct, which puts ring 0 back inside the replay.
	Raw []byte

	// ARP is set when Kind is EvARPReceived, and may be nil even then, for
	// the reason Msg may: Step is total, and ring 1 does not panic.
	ARP *wire.ARPPacket

	// RA is set when Kind is EvRouterAdvert, and may be nil even then, for
	// the same reason.
	RA *wire.RouterAdvert

	// RARaw is the ICMPv6 bytes RA was decoded from, when available. The
	// journal stores them and a replay re-decodes, which is what Raw does for
	// a DHCP packet and for the same reason: a replay from an already-decoded
	// struct agrees with itself even when the codec is wrong.
	RARaw []byte

	// DAD is set when Kind is EvDADResult.
	DAD DADOutcome

	// Timer is set when Kind is EvTimerFired.
	Timer TimerID

	// Action is set when Kind is EvActionFailed: the id of the action that
	// failed.
	Action ActionID

	// Reason carries the failure text for EvActionFailed, and a human note
	// for the others. It is never parsed — U5 is served by typed values, not
	// by string matching — and it exists for the journal.
	Reason string
}

// Received builds an EvReceived event.
func Received(m *wire.Message, raw []byte) Event {
	return Event{Kind: EvReceived, Msg: m, Raw: raw}
}

// ReceivedV6 builds an EvReceived event carrying a DHCPv6 message.
func ReceivedV6(m *wire.MessageV6, raw []byte) Event {
	return Event{Kind: EvReceived, MsgV6: m, Raw: raw}
}

// TimerFired builds an EvTimerFired event.
func TimerFired(id TimerID) Event { return Event{Kind: EvTimerFired, Timer: id} }

// ActionFailed builds an EvActionFailed event.
func ActionFailed(id ActionID, reason string) Event {
	return Event{Kind: EvActionFailed, Action: id, Reason: reason}
}

// ARPReceived builds an EvARPReceived event.
func ARPReceived(p *wire.ARPPacket) Event { return Event{Kind: EvARPReceived, ARP: p} }

// DADOutcome is the result of duplicate address detection on one address.
//
// Duplicate is a bool and not an error, and Addr travels with it, because both
// answers are ordinary: RFC 4862 section 5.4.5 makes a duplicate a reason to
// stop using the address, and RFC 9915 section 18.2.10.1's client then sends a
// Decline for THAT address. A result that did not name its address would leave
// the machine declining whichever one it happened to be holding.
type DADOutcome struct {
	Addr      netip.Addr
	Duplicate bool
}

func (d DADOutcome) String() string {
	if d.Duplicate {
		return d.Addr.String() + " duplicate"
	}
	return d.Addr.String() + " free"
}

// RouterAdvert builds an EvRouterAdvert event with no bytes behind it.
//
// A JOURNAL OF THESE CANNOT BE REPLAYED, and Replay says so rather than
// replaying a client with no router: see ErrJournalNoRA. Use RouterAdvertRaw
// wherever the frame is at hand, which is everywhere a real transport
// delivered it; this constructor stays for the callers that are testing the
// decoded value itself.
func RouterAdvert(ra *wire.RouterAdvert) Event { return Event{Kind: EvRouterAdvert, RA: ra} }

// RouterAdvertRaw builds an EvRouterAdvert event that can be replayed.
func RouterAdvertRaw(ra *wire.RouterAdvert, raw []byte) Event {
	return Event{Kind: EvRouterAdvert, RA: ra, RARaw: raw}
}

// DADResult builds an EvDADResult event.
func DADResult(addr netip.Addr, duplicate bool) Event {
	return Event{Kind: EvDADResult, DAD: DADOutcome{Addr: addr, Duplicate: duplicate}}
}

// Simple builds an event that carries nothing but its kind.
func Simple(k EventKind) Event { return Event{Kind: k} }

func (e Event) String() string {
	switch e.Kind {
	case EvReceived:
		if e.MsgV6 != nil {
			return "Received " + e.MsgV6.Summary()
		}
		if e.Msg == nil {
			return "Received <nil>"
		}
		return "Received " + e.Msg.Summary()
	case EvTimerFired:
		return "TimerFired " + e.Timer.String()
	case EvActionFailed:
		return fmt.Sprintf("ActionFailed %s: %s", e.Action, e.Reason)
	case EvARPReceived:
		if e.ARP == nil {
			return "ARPReceived <nil>"
		}
		return "ARPReceived " + e.ARP.String()
	case EvRouterAdvert:
		if e.RA == nil {
			return "RouterAdvert <nil>"
		}
		return "RouterAdvert " + e.RA.String()
	case EvDADResult:
		return "DADResult " + e.DAD.String()
	default:
		return e.Kind.String()
	}
}
