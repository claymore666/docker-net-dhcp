package proto

import (
	"fmt"

	"github.com/claymore666/dhcp-golib/wire"
)

// DADPhase reports where RFC 4862 §5.4's duplicate address detection stands,
// for a caller that has to show it or persist it.
//
// It is ACDPhase's counterpart and it is a SECOND type for State6's reason:
// the two checks are different protocols with different phases. RFC 5227's has
// a settling window and an ongoing defence; RFC 4862 §5.4's has neither — a
// node "MUST join the all-nodes multicast address and the solicited-node
// multicast address of the tentative address" and then either hears an answer
// or does not. A single enumeration would put ACDSettling in a v6 record's
// domain and TENTATIVE in a v4 one's.
//
// The names are RFC 4862 §5.4's own: an address is "tentative" while the check
// is running and "preferred" once it has passed, and §5.4.5 calls the failure
// "a duplicate address has been detected".
type DADPhase uint8

// The phases.
const (
	// DADIdle is not running: no address is being checked. It is the value
	// for a client that has not reached a Reply yet, and for every record
	// written by a v4 client — which reads correctly, because those clients
	// run RFC 5227 instead and report it in ACDPhase.
	DADIdle DADPhase = iota
	// DADTentative is RFC 4862 §5.4: the address has been granted by the
	// server and the Neighbor Solicitations are out. §5.4, the paragraph
	// before §5.4.1: "An address on which the Duplicate Address Detection
	// procedure is applied is said to be tentative until the procedure has
	// completed successfully."
	//
	// NOTHING IS BOUND IN THIS PHASE. §18.2.10.1: "The client performs the
	// duplicate address detection before using the received addresses for any
	// traffic."
	DADTentative
	// DADPassed is §5.4's other ending, stated in the paragraph that opens
	// the subsections: "An address is considered unique if none of the tests
	// indicate the presence of a duplicate address within RetransTimer
	// milliseconds after having sent DupAddrDetectTransmits Neighbor
	// Solicitations. Once an address is determined to be unique, it may be
	// assigned to an interface." This is the phase a bound v6 lease sits in
	// for the rest of its life.
	DADPassed
	// DADFailed is §5.4.5, "When Duplicate Address Detection Fails": "A
	// tentative address that is determined to be a duplicate as described
	// above MUST NOT be assigned to an interface, and the node SHOULD log a
	// system management error." The address is not usable on this link and
	// §18.2.10.1's Decline has been sent or is going out.
	DADFailed
)

func (p DADPhase) String() string {
	switch p {
	case DADIdle:
		return "idle"
	case DADTentative:
		return "tentative"
	case DADPassed:
		return "passed"
	case DADFailed:
		return "failed"
	default:
		return fmt.Sprintf("dad-phase(%d)", uint8(p))
	}
}

// AllDADPhases is every DADPhase. See AllStates for why this exists: a phase
// added to the constant block and not to this slice shrinks a totality test's
// domain rather than failing it.
func AllDADPhases() []DADPhase {
	return []DADPhase{DADIdle, DADTentative, DADPassed, DADFailed}
}

// DADPhase reports where this machine's duplicate address detection stands.
//
// It is derived from the machine's own state rather than stored, for the
// reason Machine.ACDPhase is: a second field tracking the same fact is a
// second thing to forget to update.
func (m *Machine6) DADPhase() DADPhase {
	switch {
	case m.state == State6DAD && len(m.dadBad) > 0:
		return DADFailed
	case m.state == State6DAD && m.msgType == wire.MsgDecline6:
		// The Decline exchange runs from State6DAD with the duplicate already
		// found, which is the other way into the failed phase: the wait list
		// has been emptied by the results that produced it.
		return DADFailed
	case m.state == State6DAD:
		return DADTentative
	case m.haveLse:
		return DADPassed
	default:
		return DADIdle
	}
}
