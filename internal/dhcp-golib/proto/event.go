package proto

import (
	"fmt"

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
	default:
		return fmt.Sprintf("event(%d)", uint8(k))
	}
}

// AllEventKinds is every EventKind. See AllStates for why this exists.
func AllEventKinds() []EventKind {
	return []EventKind{
		EvStart, EvStop, EvReceived, EvTimerFired, EvLinkDown, EvLinkUp,
		EvConflictDetected, EvAddressLost, EvActionFailed, EvRelease,
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

	// Raw is the bytes Msg was decoded from, when available. The journal
	// stores it so a replay re-decodes rather than trusting an already-decoded
	// struct, which puts ring 0 back inside the replay.
	Raw []byte

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

// TimerFired builds an EvTimerFired event.
func TimerFired(id TimerID) Event { return Event{Kind: EvTimerFired, Timer: id} }

// ActionFailed builds an EvActionFailed event.
func ActionFailed(id ActionID, reason string) Event {
	return Event{Kind: EvActionFailed, Action: id, Reason: reason}
}

// Simple builds an event that carries nothing but its kind.
func Simple(k EventKind) Event { return Event{Kind: k} }

func (e Event) String() string {
	switch e.Kind {
	case EvReceived:
		return "Received " + e.Msg.Summary()
	case EvTimerFired:
		return "TimerFired " + e.Timer.String()
	case EvActionFailed:
		return fmt.Sprintf("ActionFailed %s: %s", e.Action, e.Reason)
	default:
		return e.Kind.String()
	}
}
