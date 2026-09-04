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
	default:
		return fmt.Sprintf("timer(%d)", uint8(t))
	}
}

// AllTimerIDs is every TimerID.
func AllTimerIDs() []TimerID {
	return []TimerID{
		TimerRetransmit, TimerDesync, TimerExpire, TimerRestart, TimerRenew,
		TimerRebind, TimerACD,
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
	Dest Dest          // ActSend

	// ARP is the packet to broadcast, on ActSendARP only.
	ARP *wire.ARPPacket

	Timer TimerID  // ActSetTimer, ActCancelTimer
	After Duration // ActSetTimer

	Lease  Lease  // ActLeaseAcquired, ActLeaseChanged, ActLeaseRenewed
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
		if a.Requested.IsValid() && !a.Requested.IsUnspecified() && a.Requested != a.Lease.Addr.Addr() {
			return fmt.Sprintf("LeaseAcquired %s (asked for %s)", a.Lease, a.Requested)
		}
		return fmt.Sprintf("LeaseAcquired %s", a.Lease)
	case ActLeaseChanged:
		return fmt.Sprintf("LeaseChanged %s", a.Lease)
	case ActLeaseRenewed:
		return fmt.Sprintf("LeaseRenewed %s", a.Lease)
	case ActLeaseLost:
		return fmt.Sprintf("LeaseLost %s", a.Reason)
	case ActFailed:
		return fmt.Sprintf("Failed %s: %s", a.Reason, a.Note)
	case ActJournal:
		return "Journal " + a.Note
	case ActSendARP:
		return "SendARP " + a.ARP.String()
	default:
		return a.Kind.String()
	}
}
