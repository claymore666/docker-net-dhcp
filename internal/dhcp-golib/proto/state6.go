package proto

import "fmt"

// State6 is a DHCPv6 client state.
//
// IT IS A SECOND ENUMERATION AND NOT A WIDENING OF State, for the reason
// Machine6 is a second machine: D30 says IPv6 takes the same SHAPE as IPv4,
// and the same shape is two machines with the same Step contract, not one
// machine whose states half apply. A single State whose members were BOUND,
// SELECTING, REBOOTING and CONFIRMING would put REBOOTING in the v6 machine's
// totality domain and CONFIRMING in the v4 machine's, and the only assertion
// either could make about the other's states is that nothing happens — which
// is M7a's D-1 in reverse: a LARGER domain reported as fully covered.
//
// Seven of the ten names are the v4 machine's, and they are the same names
// because they are the same meaning. Where the meaning differs the name does:
// v4's REBOOTING is a broadcast DHCPREQUEST naming a remembered address, and
// v6's answer to the same question is CONFIRMING (RFC 9915 §18.2.3), which is
// a different message with a different reply and a different failure.
type State6 uint8

// The states this machine can be in.
const (
	// State6Stopped is before Start and after Stop. Not an RFC state; it
	// exists so Step is total over the events that arrive during teardown.
	State6Stopped State6 = iota
	// State6Init is entered but not yet sending: RFC 9915 §18.2.1's "The
	// first Solicit message from the client on the interface SHOULD be
	// delayed by a random amount of time between 0 and SOL_MAX_DELAY."
	State6Init
	// State6Selecting is §18.2.1's collection window and the Solicit
	// retransmissions after it: a Solicit is in flight and Advertise messages
	// are being collected.
	//
	// §18.2.1: "A client MUST collect valid Advertise messages for the first
	// RT seconds, unless it receives a valid Advertise message with a
	// preference value of 255."
	State6Selecting
	// State6Requesting is §18.2.2: a Request is in flight to the selected
	// server, awaiting a Reply.
	State6Requesting
	// State6Confirming is §18.2.3: a Confirm is in flight, asking whether the
	// remembered addresses are still on this link.
	//
	// IT IS NOT v4's REBOOTING. The message is answered by a Reply that either
	// says NotOnLink — §18.2.10.3, restart discovery — or says Success, and
	// the SILENCE case is the one that differs most: §18.2.3's client "SHOULD
	// continue to use any leases, using the last known lifetimes", where
	// RFC 2131 §3.2(3)'s MAY to do the same is declined by the v4 machine.
	State6Confirming
	// State6InfoRequesting is §18.2.6: an Information-request is in flight,
	// AND the state the machine sits in between one exchange and the next.
	// There is no address in this exchange and none will be bound; what comes
	// back is configuration, reported as ActConfigured.
	//
	// It holds §21.23's refresh timer as well as the exchange, because the two
	// are one lifecycle: "When the client detects that the refresh time has
	// expired, it SHOULD try to update its configuration data by sending an
	// Information-request as specified in Section 18.2.6". A separate
	// INFO-BOUND state would be a state whose only event is the timer that
	// leaves it.
	State6InfoRequesting
	// State6DAD is the window between the Reply that granted an address and
	// the EvDADResult that says it is free.
	//
	// It is the v6 analogue of StateProbing and it exists for the same reason
	// that one does: a client SITS in it — RFC 4862 §5.1's
	// DupAddrDetectTransmits times RetransTimer, plus RFC 7527 §4.1's
	// looped-back continuation — and events arrive while it does. §18.2.10.1:
	// "The client performs the duplicate address detection before using the
	// received addresses for any traffic." So no Acquired is emitted from
	// here, which is D22's shape at the v6 layer.
	State6DAD
	// State6Bound holds the lease.
	State6Bound
	// State6Renewing is §18.2.4, entered at T1: a Renew is in flight to the
	// server that granted the lease, which the message names in a Server
	// Identifier option.
	State6Renewing
	// State6Rebinding is §18.2.5, entered at T2: a Rebind is in flight to any
	// server, and it carries NO Server Identifier.
	State6Rebinding
)

func (s State6) String() string {
	switch s {
	case State6Stopped:
		return "STOPPED6"
	case State6Init:
		return "INIT6"
	case State6Selecting:
		return "SELECTING6"
	case State6Requesting:
		return "REQUESTING6"
	case State6Confirming:
		return "CONFIRMING6"
	case State6InfoRequesting:
		return "INFO-REQUESTING6"
	case State6DAD:
		return "DAD6"
	case State6Bound:
		return "BOUND6"
	case State6Renewing:
		return "RENEWING6"
	case State6Rebinding:
		return "REBINDING6"
	default:
		return fmt.Sprintf("state6(%d)", uint8(s))
	}
}

// AllStates6 is every State6, so the totality test enumerates the domain from
// one place.
//
// M7a's defeat row D-1 applies here: a state added to the constant block and
// not to this slice SHRINKS the totality test's domain rather than failing it,
// which reports a smaller domain as fully covered.
// TestAllStates6IsEveryDeclaredState is the test whose own domain is the State6
// space rather than this slice, which is why it is the one that can see a
// member go missing.
func AllStates6() []State6 {
	return []State6{
		State6Stopped, State6Init, State6Selecting, State6Requesting,
		State6Confirming, State6InfoRequesting, State6DAD, State6Bound,
		State6Renewing, State6Rebinding,
	}
}
