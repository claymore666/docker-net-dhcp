package proto

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// TimerID names a timer. The set is closed, so ring 3's timer table is a fixed
// array rather than a map keyed on something the machine invents.
type TimerID uint8

// The timers this milestone uses.
const (
	// TimerRetransmit is the RFC 2131 section 4.1 retransmission timer, live
	// in SELECTING and REQUESTING.
	TimerRetransmit TimerID = iota
	// TimerDesync is the "wait a random time between one and ten seconds to
	// desynchronize the use of DHCP at startup" of RFC 2131 section 4.4.1.
	TimerDesync
	// TimerExpire is the lease expiry, live in BOUND.
	TimerExpire
	// TimerRestart is the wait before restarting the configuration process
	// after a DHCPDECLINE: RFC 2131 section 3.1(5), "The client SHOULD wait a
	// minimum of ten seconds before restarting the configuration process to
	// avoid excessive network traffic in case of looping."
	//
	// Separate from TimerDesync because the two obligations differ in both
	// value and reason: desync is a RANDOM one-to-ten-second draw that
	// spreads a fleet booting together, this is a MINIMUM of ten seconds that
	// keeps a client whose address is permanently in use from looping. Nine
	// draws in ten of a desync window are shorter than this floor.
	TimerRestart
	// TimerRenew is T1, the moment the client enters RENEWING: RFC 2131
	// section 4.4.5, "T1 is the time at which the client enters the RENEWING
	// state and attempts to contact the server that originally issued the
	// client's network address."
	TimerRenew
	// TimerRebind is T2, the moment the client enters REBINDING: same
	// section, "T2 is the time at which the client enters the REBINDING state
	// and attempts to contact any server."
	//
	// It is a SEPARATE timer from TimerRenew and not a rearm of it, because
	// both are live at once while the machine is in RENEWING: T2 is what ends
	// a renewal that is getting no answer, and a machine that reused one
	// timer id for both would cancel its own deadline every time it
	// retransmitted.
	TimerRebind
	// TimerACD is RFC 5227's one conflict-detection timer: section 2.1.1's
	// initial random delay, then the PROBE_MIN-to-PROBE_MAX gaps between the
	// probes, then ANNOUNCE_WAIT, then the ANNOUNCE_INTERVAL between the
	// announcements.
	//
	// ONE timer for the whole schedule, because the schedule is sequential:
	// each of those waits begins when the previous one ends, so a second timer
	// id could only ever be armed by a phase that had already disarmed the
	// first. That is not true of TimerRenew and TimerRebind, which is why
	// those are two.
	TimerACD

	// The DHCPv6 timers. They share the TimerID space with the v4 ones rather
	// than forming a second one, because ring 3's timer table is indexed BY
	// the id (runtime.numTimers) and a second space would need a second table
	// keyed on something the port cannot see. One manager runs one family, so
	// no machine ever arms more than its own half; both machines nonetheless
	// cancel the WHOLE set on every path to idle, so "holds nothing" and
	// "nothing armed" stay the same state whichever machine is running.

	// Timer6Retransmit is RFC 9915 §15's RT for the message in flight, live
	// in every state that has one.
	//
	// IN SELECTING IT IS ALSO §18.2.1's COLLECTION WINDOW: "the client
	// collects valid Advertise messages until the first RT has elapsed". The
	// first firing therefore has two possible meanings — select what arrived,
	// or retransmit because nothing did — and they are one timer because they
	// are one deadline.
	Timer6Retransmit
	// Timer6Delay is the random pre-transmission delay §18.2.1, §18.2.3 and
	// §18.2.6 each put in front of the FIRST message of an exchange:
	// SOL_MAX_DELAY, CNF_MAX_DELAY and INF_MAX_DELAY.
	//
	// One id for all three, because they are sequential in exactly the sense
	// TimerACD's comment gives: a machine is delaying the start of ONE
	// exchange, and the next delay can only be armed by a state that has
	// already left this one.
	Timer6Delay
	// Timer6Expire is the moment the last valid lifetime in the IA runs out
	// (§21.6). It is the v6 lease expiry.
	Timer6Expire
	// Timer6Renew is T1 (§21.4, §18.2.4).
	Timer6Renew
	// Timer6Rebind is T2 (§21.4, §18.2.5). Separate from Timer6Renew for the
	// reason TimerRebind is separate from TimerRenew: both are live at once
	// while the machine is renewing.
	Timer6Rebind
	// Timer6DAD is the deadline for an EvDADResult that ring 3 owes the
	// machine.
	//
	// IT IS THE MACHINE'S OWN DEADLINE AND NOT RING 3's. Ring 3 sends the
	// Neighbor Solicitations and reads the answers; if it dies, is starved, or
	// simply loses the frame, no event ever arrives and the machine would sit
	// in DAD6 forever holding an address it has not announced and cannot use.
	// Params6.DADTimeout says how the value is derived.
	Timer6DAD
	// Timer6Refresh is §21.23's Information Refresh Time: when it fires, the
	// machine sends another Information-request.
	//
	// Separate from Timer6Renew because the two are not the same exchange and
	// can both be pending on a client that has an address AND took stateless
	// configuration: §21.23's option "is only used in Reply messages in
	// response to Information-request messages".
	Timer6Refresh
	// Timer6RouterSolicit is RFC 4861 §6.3.7's gap between Router
	// Solicitations, "each separated by at least RTR_SOLICITATION_INTERVAL
	// seconds".
	//
	// It runs BESIDE the DHCPv6 exchange and not inside it, which is why it is
	// its own id: design §A.3.3 interlock 1 has the machine solicit at
	// EvStart regardless of any router, so the two schedules are concurrent
	// and one timer for both would cancel the DHCP retransmission every time a
	// solicitation went out.
	Timer6RouterSolicit
)

func (t TimerID) String() string {
	switch t {
	case TimerRetransmit:
		return "retransmit"
	case TimerDesync:
		return "desync"
	case TimerExpire:
		return "expire"
	case TimerRestart:
		return "restart"
	case TimerRenew:
		return "renew"
	case TimerRebind:
		return "rebind"
	case TimerACD:
		return "acd"
	case Timer6Retransmit:
		return "retransmit6"
	case Timer6Delay:
		return "delay6"
	case Timer6Expire:
		return "expire6"
	case Timer6Renew:
		return "renew6"
	case Timer6Rebind:
		return "rebind6"
	case Timer6DAD:
		return "dad6"
	case Timer6Refresh:
		return "refresh6"
	case Timer6RouterSolicit:
		return "router-solicit6"
	default:
		return fmt.Sprintf("timer(%d)", uint8(t))
	}
}

// AllTimerIDs is every TimerID, both families.
//
// BOTH MACHINES CANCEL THE WHOLE SET, and that is deliberate rather than
// tidiness: cancelAll is what makes "holding nothing" and "nothing armed" the
// same state, and a per-family list would make that invariant a claim about
// which machine wrote the list. A cancel for a timer nothing armed is defined
// and does nothing (see ActCancelTimer), so the cost is action-list length and
// the benefit is that adding a timer to either family cannot leave the other
// family's idle paths short.
func AllTimerIDs() []TimerID {
	return []TimerID{
		TimerRetransmit, TimerDesync, TimerExpire, TimerRestart, TimerRenew,
		TimerRebind, TimerACD,
		Timer6Retransmit, Timer6Delay, Timer6Expire, Timer6Renew,
		Timer6Rebind, Timer6DAD, Timer6Refresh, Timer6RouterSolicit,
	}
}

// ActionKind is what an action asks the caller to do.
type ActionKind uint8

// The actions the machine emits.
const (
	// ActSend transmits a message. Dest says where.
	ActSend ActionKind = iota
	// ActSetTimer arms a timer. Re-arming a live timer replaces it.
	ActSetTimer
	// ActCancelTimer disarms a timer. Cancelling a timer that is not armed is
	// defined and does nothing — a machine that has to track what is armed in
	// order to cancel correctly has duplicated ring 3's bookkeeping.
	ActCancelTimer
	// ActLeaseAcquired reports a lease the caller did not have.
	ActLeaseAcquired
	// ActLeaseChanged reports a lease whose contents differ from the one the
	// caller already had: a renewal that came back with a different router,
	// resolver, MTU or prefix.
	ActLeaseChanged
	// ActLeaseRenewed reports that the lease was EXTENDED — a DHCPACK
	// accepted in RENEWING or REBINDING — whether or not anything in it
	// changed.
	//
	// Separate from ActLeaseChanged, and emitted even when the contents are
	// identical, because the two answer different questions. "Reconfigure the
	// interface" is Changed; "the lease is still ours and now runs until T"
	// is this, and it is the ordinary case, the one with no other evidence
	// anywhere. A caller told only about changes cannot tell a lease being
	// renewed every T1 from a client that has silently stopped renewing.
	ActLeaseRenewed
	// ActLeaseLost reports that the lease is gone, with a reason.
	ActLeaseLost
	// ActFailed reports that acquisition failed in a way the caller should
	// hear about, with a typed reason. This is what U5 branches on.
	ActFailed
	// ActJournal records something that changed no state. It is how a
	// silently-discarded packet becomes visible: RFC 2131 says "silently
	// discard", and a client that is silent to its own operator is the reason
	// this project has debugging requirements at all.
	ActJournal
	// ActSendARP broadcasts one ARP packet: an RFC 5227 section 2.1.1 Probe or
	// a section 2.3 Announcement. ARP says which.
	//
	// It is a SEPARATE kind from ActSend and not a Dest on it. The two go out
	// of different sockets — ActSend's is AF_PACKET/ETH_P_IP carrying a UDP
	// datagram this library builds itself, this one's is AF_PACKET/ETH_P_ARP
	// carrying no IP header at all — so a caller that folded them together
	// would have to inspect the payload to know where to write it, which is
	// ring 3 parsing ring 0's output to route it.
	ActSendARP
	// ActSendRouterSolicit asks ring 3 to send an RFC 4861 section 4.1 Router
	// Solicitation, to prompt a Router Advertisement rather than wait for the
	// next periodic one.
	//
	// It carries no packet, where ActSendARP carries one. The Router
	// Solicitation's source address is an address the LIBRARY does not have —
	// section 4.1's Source Address is "An IP address assigned to the sending
	// interface, or the unspecified address if no address is assigned to the
	// sending interface", and only ring 3 can read the link's link-local
	// address — and its checksum
	// covers that address (RFC 4443 section 2.3). So ring 1 asks, ring 3
	// builds with wire.EncodeRouterSolicit, and the address the checksum
	// covers is by construction the address it goes out from.
	ActSendRouterSolicit
	// ActStartDAD asks ring 3 to run RFC 4862 section 5.4's duplicate address
	// detection on Target and report the outcome as EvDADResult.
	//
	// A REQUEST TO OBSERVE, not to configure. The library never installs an
	// address (the seam design's rule), so DAD here is the probe-and-listen of
	// section 5.4.2 performed on an address nothing is using yet — the same
	// shape as M6's RFC 5227 probe, which is D30's rule applied: v6 takes v4's
	// shape unless there is a reason not to.
	ActStartDAD
	// ActConfigured reports the outcome of an Information-request exchange:
	// configuration and no address (RFC 9915 section 18.2.6).
	//
	// ITS OWN KIND, not an ActLeaseAcquired with a zero address. There is no
	// lease, no T1, no T2 and nothing to renew — only a refresh time — and a
	// lease-shaped value with every lease field empty is an error folded into
	// a value: every caller would have to test the address before trusting the
	// rest, and the one that forgot would install a route to "::".
	ActConfigured
	// ActRouterObserved reports what router discovery saw, for a caller that
	// has to tell "no DHCPv6 server answered" from "there is no DHCPv6 server
	// here".
	//
	// A DIAGNOSTIC, not a lease event. RFC 4861 section 4.2: "If neither M nor
	// O flags are set, this indicates that no information is available via
	// DHCPv6." A client that waits out its Solicit schedule on such a link has
	// not failed; it has been told, and this is the action that carries the
	// telling out to where an operator can read it.
	ActRouterObserved
	// ActSendV6 transmits one DHCPv6 message. Dest says where.
	//
	// A SEPARATE KIND FROM ActSend, for the reason ActSendARP is one. The two
	// leave the host by different routes — ActSend's payload goes out an
	// AF_PACKET socket on ETH_P_IP with an IP and UDP header this library
	// builds itself, this one's goes out an ordinary UDP socket bound to port
	// 546 — and they are built by different encoders (wire.Encode versus
	// wire.EncodeV6). A caller that folded them into one kind would have to
	// look at which pointer was non-nil to decide where to write it, which is
	// ring 3 reading ring 0's output to route it.
	//
	// Dest.Src IS ZERO HERE AND RING 3 FILLS IT. RFC 9915 §5: "The client uses
	// a link-local source address or addresses determined through other
	// mechanisms for transmitting and receiving DHCP messages", and the only
	// one a client has before it is bound is the link-local the kernel formed
	// — which ring 1 cannot read, for the same reason ActSendRouterSolicit
	// carries no packet.
	ActSendV6
)

func (k ActionKind) String() string {
	switch k {
	case ActSend:
		return "Send"
	case ActSetTimer:
		return "SetTimer"
	case ActCancelTimer:
		return "CancelTimer"
	case ActLeaseAcquired:
		return "LeaseAcquired"
	case ActLeaseChanged:
		return "LeaseChanged"
	case ActLeaseRenewed:
		return "LeaseRenewed"
	case ActLeaseLost:
		return "LeaseLost"
	case ActFailed:
		return "Failed"
	case ActJournal:
		return "Journal"
	case ActSendARP:
		return "SendARP"
	case ActSendRouterSolicit:
		return "SendRouterSolicit"
	case ActStartDAD:
		return "StartDAD"
	case ActConfigured:
		return "Configured"
	case ActRouterObserved:
		return "RouterObserved"
	case ActSendV6:
		return "SendV6"
	default:
		return fmt.Sprintf("action(%d)", uint8(k))
	}
}

// ActionID identifies one emitted action so a failure can name it.
//
// It is a monotonically increasing counter owned by the Machine, so an id is
// unique within one machine's lifetime and is reproduced exactly on replay.
type ActionID uint64

func (a ActionID) String() string { return fmt.Sprintf("action#%d", uint64(a)) }

// Dest says where a Send goes.
type Dest struct {
	// Broadcast sends to 255.255.255.255 on the link. Every message M1 sends
	// is broadcast: the client has no address until it is BOUND, and RFC 2131
	// section 4.1 requires the IP source address to be 0 for a message
	// broadcast before the client has its address.
	Broadcast bool
	// Addr is the unicast destination when Broadcast is false.
	Addr netip.Addr
	// Src is the IP source address a unicast must be sent FROM, and it is
	// carried here because ring 3 cannot derive it: the transport is an
	// AF_PACKET socket on a link the kernel has no address on, so nothing
	// below this struct knows what the client's address is.
	//
	// RFC 2131 section 4.4.4 unicasts the DHCPRELEASE to the server, and
	// section 4.4.6's message identifies the binding by the address it is
	// released from — Table 5 carries it in 'ciaddr'. A release sent from
	// 0.0.0.0 is a datagram the server can neither route back nor match.
	//
	// Zero for a broadcast, where RFC 2131 section 4.1 requires the source to
	// be 0.0.0.0 anyway.
	Src netip.Addr
}

func (d Dest) String() string {
	if d.Broadcast {
		return "broadcast"
	}
	if d.Src.IsValid() && !d.Src.IsUnspecified() {
		return d.Src.String() + "->" + d.Addr.String()
	}
	return d.Addr.String()
}

// Reason is a typed cause. It is what U5 asks for: a caller branches on this,
// never on text.
type Reason uint8

// The reasons this milestone can produce.
const (
	ReasonNone Reason = iota
	// ReasonNoServer means the retransmission budget ran out with no usable
	// reply. This is "no server answered".
	ReasonNoServer
	// ReasonNak means the server refused the REQUEST with a DHCPNAK.
	ReasonNak
	// ReasonExpired means the lease reached its expiry.
	ReasonExpired
	// ReasonStopped means the caller stopped the client.
	ReasonStopped
	// ReasonLinkDown means the interface lost carrier.
	ReasonLinkDown
	// ReasonAddressLost means the address went away underneath us.
	ReasonAddressLost
	// ReasonConflict means another host is using the address.
	ReasonConflict
	// ReasonTransport means the transport could not send, repeatedly. This is
	// R2's visible consequence: without it a machine whose sends all fail sits
	// in SELECTING forever looking healthy.
	ReasonTransport
	// ReasonDADIncomplete means duplicate address detection was asked for and
	// never answered: RFC 4862 section 5.4's check neither succeeded nor found
	// a duplicate, because ring 3 produced no result before the deadline.
	//
	// IT IS NOT ReasonConflict AND THE DIFFERENCE IS THE OPERATOR's NEXT STEP.
	// A conflict is another host answering for the address, which is a network
	// fact and is followed by a Decline (RFC 9915 section 18.2.10.1). This is
	// this host's own machinery not reporting, which is a local fault and is
	// followed by nothing being said to the server at all — declining an
	// address on evidence nobody produced would hand a perfectly good address
	// back and mark it suspect at the server.
	ReasonDADIncomplete
	// ReasonReleased means the caller asked for the lease to be given back and
	// a DHCPRELEASE was sent. Distinct from ReasonStopped: a stopped client
	// still holds its binding at the server until the lease runs out, a
	// released one does not (RFC 2131 section 4.3.4).
	ReasonReleased
)

func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return "none"
	case ReasonNoServer:
		return "no-server"
	case ReasonNak:
		return "nak"
	case ReasonExpired:
		return "expired"
	case ReasonStopped:
		return "stopped"
	case ReasonLinkDown:
		return "link-down"
	case ReasonAddressLost:
		return "address-lost"
	case ReasonConflict:
		return "conflict"
	case ReasonTransport:
		return "transport"
	case ReasonDADIncomplete:
		return "dad-incomplete"
	case ReasonReleased:
		return "released"
	default:
		return fmt.Sprintf("reason(%d)", uint8(r))
	}
}

// Action is one thing the caller must do, in the order returned.
type Action struct {
	ID   ActionID
	Kind ActionKind

	Msg  *wire.Message // ActSend
	Dest Dest          // ActSend, ActSendV6

	// MsgV6 is the DHCPv6 message to send, on ActSendV6 only.
	MsgV6 *wire.MessageV6

	// ARP is the packet to broadcast, on ActSendARP only.
	ARP *wire.ARPPacket

	// Target is the address to run duplicate address detection on, on
	// ActStartDAD only.
	Target netip.Addr

	// Config is the stateless configuration, on ActConfigured only.
	Config Config6

	// Router is what router discovery saw, on ActRouterObserved only.
	Router RouterObservation

	Timer TimerID  // ActSetTimer, ActCancelTimer
	After Duration // ActSetTimer

	Lease  Lease  // ActLeaseAcquired, ActLeaseChanged, ActLeaseRenewed (v4)
	Lease6 Lease6 // ActLeaseAcquired, ActLeaseChanged, ActLeaseRenewed (v6)
	Reason Reason // ActLeaseLost, ActFailed
	Note   string // ActJournal, and detail beside Reason

	// Requested is the address this client ASKED FOR, on ActLeaseAcquired
	// only, and the zero value means it asked for none.
	//
	// It is data and not a verdict. Two paths put an address in option 50 —
	// Params.RequestedIP in a DHCPDISCOVER (RFC 2131 section 4.4.1, a MAY) and
	// Params.Resume in the INIT-REBOOT DHCPREQUEST (section 4.3.2, a MUST) —
	// and neither obliges the server to honour it: section 4.4.2 accepts "a
	// DHCPACK message with an 'xid' field matching that in the client's
	// DHCPREQUEST message ... from any server" and conditions acceptance on
	// nothing else. So the machine takes the lease and reports what it had
	// asked for beside it; whether a different address is acceptable is a
	// question about the caller's endpoint, not about the protocol.
	//
	// NOT set on ActLeaseRenewed or ActLeaseChanged. A renewal asks for the
	// address it already holds, and a renewal that comes back on a different
	// one is journalled by name in enterBound.
	Requested netip.Addr
}

func (a Action) String() string {
	switch a.Kind {
	case ActSend:
		return fmt.Sprintf("Send %s to %s", a.Msg.Summary(), a.Dest)
	case ActSetTimer:
		return fmt.Sprintf("SetTimer %s after %s", a.Timer, a.After)
	case ActCancelTimer:
		return fmt.Sprintf("CancelTimer %s", a.Timer)
	case ActLeaseAcquired:
		if a.Requested.IsValid() && !a.Requested.IsUnspecified() && a.Requested != a.leaseAddr() {
			return fmt.Sprintf("LeaseAcquired %s (asked for %s)", a.leaseText(), a.Requested)
		}
		return fmt.Sprintf("LeaseAcquired %s", a.leaseText())
	case ActLeaseChanged:
		return fmt.Sprintf("LeaseChanged %s", a.leaseText())
	case ActLeaseRenewed:
		return fmt.Sprintf("LeaseRenewed %s", a.leaseText())
	case ActLeaseLost:
		return fmt.Sprintf("LeaseLost %s", a.Reason)
	case ActFailed:
		return fmt.Sprintf("Failed %s: %s", a.Reason, a.Note)
	case ActJournal:
		return "Journal " + a.Note
	case ActSendARP:
		return "SendARP " + a.ARP.String()
	case ActSendRouterSolicit:
		return "SendRouterSolicit"
	case ActStartDAD:
		return "StartDAD " + a.Target.String()
	case ActConfigured:
		return "Configured " + a.Config.String()
	case ActRouterObserved:
		return "RouterObserved " + a.Router.String()
	case ActSendV6:
		return fmt.Sprintf("SendV6 %s to %s", a.MsgV6.Summary(), a.Dest)
	default:
		return a.Kind.String()
	}
}

// leaseText renders whichever of the two lease fields this action carries.
//
// THE DISCRIMINATOR IS THE V6 ADDRESS LIST AND NOT A FAMILY FLAG, because a
// flag would be a third thing to keep in step with the two values: an action
// whose flag said v6 and whose Lease6 was empty would render an empty string
// and the journal line would say a lease was acquired with nothing in it. A
// v6 lease with no addresses is never acquired — leaseFromReply refuses it —
// so the list is present exactly when the v6 field is the live one.
func (a Action) leaseText() string {
	if len(a.Lease6.Addrs) > 0 {
		return a.Lease6.String()
	}
	return a.Lease.String()
}

// leaseAddr is the address this action's lease carries, for the one comparison
// Requested needs.
func (a Action) leaseAddr() netip.Addr {
	if len(a.Lease6.Addrs) > 0 {
		return a.Lease6.Addrs[0].Addr
	}
	return a.Lease.Addr.Addr()
}

// Config6 is the outcome of an RFC 9915 section 18.2.6 Information-request:
// configuration parameters with no address bound to them.
type Config6 struct {
	// DNS is option 23's list, RFC 3646 section 3.
	DNS []netip.Addr
	// Search is option 24's list, RFC 3646 section 4.
	Search []string
	// RefreshTime is when this client will ask again: option 32's
	// information-refresh-time (RFC 9915 section 21.23) AFTER that section's
	// two rules have been applied — "If the Reply to an Information-request
	// message does not contain this option, the client MUST behave as if the
	// option with the value IRT_DEFAULT was provided." and "A client MUST use
	// the refresh time IRT_MINIMUM if it receives the option with a value
	// less than IRT_MINIMUM."
	//
	// IT IS THE BOUNDED VALUE AND NOT THE RAW ONE, changed 2026-09-06 and the
	// reason is worth keeping. It carried the raw value, so that a caller
	// could tell a server that chose 86400 from one that said nothing. What
	// that cost was the field's only USE: the machine armed its refresh from
	// the bounded value and told the caller the raw one, so an absent option
	// reported "never" against 86400s armed, and 60s reported 60s against
	// 600s armed. One fact derived twice gives two answers, and the looser
	// derivation is the one that reaches the caller. The distinction that was
	// lost is not lost: refreshTime journals which rule it applied, by name
	// and with the value that arrived.
	RefreshTime Duration
}

// String renders the VALUES and not their counts. "dns=2" in a journal cannot
// distinguish a client that installed the right resolvers from one that
// installed somebody else's, which is the diagnosis this line exists to
// support; Lease.String() sets the same precedent one ring over. The lists are
// short by construction — RFC 3646 configurations carry a handful of entries —
// so nothing here needs a cap.
func (c Config6) String() string {
	return fmt.Sprintf("dns=%v search=%v refresh=%s", c.DNS, c.Search, c.RefreshTime)
}

// RouterObservation is what the library saw of RFC 4861 router discovery on
// this link.
//
// SEEN IS SEPARATE FROM THE TWO FLAGS, and that is the point of the type. An
// RA with M and O both clear and NO RA AT ALL are different facts with the same
// pair of booleans: the first is a router saying there is no DHCPv6 here, the
// second is a link with no router on it, or one whose advertisements are not
// reaching us. Folding them would make an absent observation look like a
// negative one, and the operator's next step differs.
type RouterObservation struct {
	Seen    bool
	Managed bool
	Other   bool
}

func (r RouterObservation) String() string {
	if !r.Seen {
		return "no router advertisement seen"
	}
	return fmt.Sprintf("router advertisement M=%t O=%t", r.Managed, r.Other)
}
