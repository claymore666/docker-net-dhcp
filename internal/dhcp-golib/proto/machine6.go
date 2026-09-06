package proto

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// Machine6 is the DHCPv6 client state machine: RFC 9915 §18.2, pure.
//
// IT IS A SECOND MACHINE BESIDE Machine, NOT A MODE OF IT, and D30 is what
// makes that the same shape rather than a different one: the same Step(now,
// rnd, ev) contract, the same actions helper, the same journal and replay
// discipline, the same "the caller owns the deadline" rule. What differs is
// everything the protocol differs in — a Solicit/Advertise/Request/Reply
// four-message exchange instead of DISCOVER/OFFER/REQUEST/ACK, an identity
// association holding a set of addresses instead of one yiaddr, a server named
// by an opaque DUID instead of by an address, and duplicate address detection
// where v4 has RFC 5227's ACD.
//
// THE MACHINE NEVER GIVES UP SOLICITING ON ITS OWN. §18.2.1's Solicit schedule
// has "MRC: 0" and "MRD: 0", and §15 says what that means: "If both MRC and MRD
// are zero, the client continues to transmit the message until it receives a
// response." With SOL_MAX_RT at an hour (§7.6) a client that has found no
// server retransmits forever at an hour's interval. That is deliberate and it
// is the caller's deadline that ends it — design §A.3.1, and 1.9.0's
// lease_timeout before that. A machine that invented its own give-up would be
// a client that stops asking on a link whose server is merely slow to boot.
type Machine6 struct {
	params Params6

	state State6

	// nextAction is the ActionID counter, machine state for the reason
	// Machine.nextAction is: two machines in one process must not interleave
	// ids, and a replay reproduces them exactly.
	nextAction ActionID

	// The exchange in flight. msgType is zero when there is none.
	//
	// ONE EXCHANGE AT A TIME, and that is a protocol fact rather than a
	// simplification: §15's retransmission is defined over "the message" and
	// every §18.2 exchange ends before the next begins. The one place two
	// could overlap is the Decline, which is why State6DAD hosts it — see
	// declineAll.
	msgType wire.MessageTypeV6
	xid     uint32
	sched   Retransmit
	// exchangeStart is the Instant the FIRST message of this exchange was
	// sent. §21.9: "The elapsed time is measured from the time at which the
	// client sent the first message in the message exchange, and the
	// elapsed-time field is set to 0 in the first message in the message
	// exchange."
	exchangeStart Instant
	rt            Duration
	transmits     int
	sendFailures  int

	// pendingType is the exchange State6Init's delay is counting down to, so
	// one Init arm serves Solicit, Confirm and Information-request.
	pendingType wire.MessageTypeV6

	// adverts is what the §18.2.1 collection window gathered, in arrival
	// order, and windowDone says the first RT has elapsed.
	adverts    []advert6
	windowDone bool
	// tried is the DUIDs of servers whose Request already failed, so
	// §18.2.2's "Select another server from a list of servers known to the
	// client" does not re-select the one that just timed out.
	tried [][]byte

	// server is the DUID of the server this exchange is addressed to.
	server []byte

	lease   Lease6
	haveLse bool

	// pending is the lease a Reply granted and DAD has not cleared. It is NOT
	// m.lease: nothing is acquired, Lease() must keep saying so, and D22's
	// whole content is that an unverified address is not used. §18.2.10.1:
	// "The client performs the duplicate address detection before using the
	// received addresses for any traffic."
	pending     Lease6
	havePending bool
	// pendingRenewal says the pending lease came from a Renew or Rebind, so
	// the report is Renewed rather than Acquired when DAD clears it.
	pendingRenewal bool
	// dadWait is the addresses whose EvDADResult has not arrived, and dadBad
	// the ones that came back duplicate.
	dadWait []netip.Addr
	dadBad  []netip.Addr

	// declining is the addresses the Decline exchange in flight names, and
	// releasing the ones the Release exchange names with their lifetimes.
	declining []netip.Addr
	releasing []Addr6
	// afterDecline says what to do when the Decline exchange ends.
	afterDecline func(now Instant, rnd uint64, out *actions)

	resume *Resume6

	// router is what router discovery has seen, and rsCount how many Router
	// Solicitations have gone out.
	router  RouterObservation
	rsCount int

	// config is the last stateless configuration an Information-request
	// produced, and refresh the §21.23 time it carries.
	config  Config6
	refresh Duration
}

// New6 builds a Machine6 in State6Stopped.
func New6(p Params6) (*Machine6, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	p.DUID = append([]byte(nil), p.DUID...)
	p.ORO = append([]wire.OptionCodeV6(nil), p.ORO...)
	p.Resume = p.Resume.Clone()
	return &Machine6{params: p, state: State6Stopped, resume: p.Resume}, nil
}

// State returns the current state.
func (m *Machine6) State() State6 { return m.state }

// Lease returns the held lease, if any. A lease awaiting duplicate address
// detection is NOT held: see the pending field.
func (m *Machine6) Lease() (Lease6, bool) { return m.lease, m.haveLse }

// Params returns the machine's configuration, with SolMaxRT and InfMaxRT as
// the servers on this link have last set them (§21.24, §21.25).
func (m *Machine6) Params() Params6 {
	p := m.params
	p.DUID = append([]byte(nil), p.DUID...)
	p.ORO = append([]wire.OptionCodeV6(nil), p.ORO...)
	p.Resume = p.Resume.Clone()
	return p
}

// Router returns what router discovery has observed on this link.
func (m *Machine6) Router() RouterObservation { return m.router }

func (m *Machine6) takeActionID() ActionID {
	id := m.nextAction
	m.nextAction++
	return id
}

// advert6 is one Advertise the collection window kept.
type advert6 struct {
	server []byte
	pref   uint8
	addrs  []Addr6
	t1, t2 Duration
	// seq is the arrival order, which is what §18.2.9's tie is broken on:
	// the RFC says only that the highest preference SHOULD be preferred, so
	// first-arrived is this client's choice among equals and it is recorded
	// so the choice is reproducible rather than dependent on a sort.
	seq int
}

// Step is total: every (State6, EventKind) pair yields a defined result and no
// reachable panic. TestStep6IsTotal drives the whole product of AllStates6 and
// AllEventKinds.
func (m *Machine6) Step(now Instant, rnd uint64, ev Event) (State6, []Action) {
	var out actions

	// Router discovery runs BESIDE the DHCP exchange, in every state, which is
	// design §A.3.3 interlock 1 and lead ruling 7. It is handled before the
	// state switch so that "every state reports what the RA said" is one arm
	// rather than ten, and so that a state added later cannot silently drop
	// the diagnostic.
	switch {
	case ev.Kind == EvRouterAdvert:
		m.observeRouter(now, rnd, ev, &out)
		return m.state, out.list
	case ev.Kind == EvTimerFired && ev.Timer == Timer6RouterSolicit:
		m.routerSolicitTick(&out)
		return m.state, out.list
	case ev.Kind == EvTimerFired && ev.Timer == Timer6Expire:
		// The valid lifetime running out means the same thing in every state,
		// and it is handled here for interlock 1's reason above and for one
		// MEASURED this round: it was handled in BOUND6 and in the two renewal
		// states only, so a lease the caller had been told about and that the
		// machine still held while re-discovering — after a Renew answered
		// with an IA_NA carrying no address, after the duplicate address
		// detection deadline, after a duplicate on a renewal's new address —
		// reached its valid lifetime in INIT6 or DAD6 and was journalled
		// "ignored". The caller kept an expired IPv6 address installed with no
		// event, forever.
		m.expireLease(now, rnd, &out)
		return m.state, out.list
	case ev.Kind == EvActionFailed:
		// R2, in one arm rather than ten: an action that did not happen means
		// the same thing in every state, and a per-state arm is how the state
		// that forgot it comes to believe its message went out.
		m.noteActionFailed(now, rnd, ev, &out)
		return m.state, out.list
	case ev.Kind == EvARPReceived || ev.Kind == EvConflictDetected:
		// The v4 machine's two conflict inputs. They reach this machine only
		// if a caller wired an ARP socket to a DHCPv6 client, which is a
		// configuration error rather than a protocol event: v6's conflict
		// evidence is EvDADResult and EvAddressLost. Journalled by name so
		// the misconfiguration is visible, and never a transition.
		out.journal(m, fmt.Sprintf("%s is a v4 conflict event and this is the v6 machine: ignored in %s", ev.Kind, m.state))
		return m.state, out.list
	}

	switch m.state {
	case State6Stopped:
		m.stepStopped6(now, rnd, ev, &out)
	case State6Init:
		m.stepInit6(now, rnd, ev, &out)
	case State6Selecting:
		m.stepSelecting6(now, rnd, ev, &out)
	case State6Requesting:
		m.stepRequesting6(now, rnd, ev, &out)
	case State6Confirming:
		m.stepConfirming6(now, rnd, ev, &out)
	case State6InfoRequesting:
		m.stepInfoRequesting6(now, rnd, ev, &out)
	case State6DAD:
		m.stepDAD6(now, rnd, ev, &out)
	case State6Bound:
		m.stepBound6(now, rnd, ev, &out)
	case State6Renewing, State6Rebinding:
		m.stepRenewal6(now, rnd, ev, &out)
	default:
		// Unreachable through the exported API and handled anyway, for
		// Machine.Step's reason: "unreachable" is a claim about today's code
		// and a panic here takes the plugin down with it.
		out.journal(m, fmt.Sprintf("event %s in unknown state %s: ignored", ev.Kind, m.state))
	}
	return m.state, out.list
}

// ---------------------------------------------------------------- states --

func (m *Machine6) stepStopped6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStart:
		m.begin(now, rnd, out)
	case EvReceived:
		// A Reply to the Release this machine sent on its way out. §18.2.10.2:
		// "When the client receives a valid Reply message in response to a
		// Release message, the client considers the Release event completed,
		// regardless of the Status Code option (see Section 21.13) returned by
		// the server."
		if m.msgType == wire.MsgRelease6 {
			if _, ok := m.admit(ev, out); ok {
				out.journal(m, "Reply to the Release: the release is complete")
				m.endExchange(out)
			}
			return
		}
		out.journal(m, "message received while stopped: ignored")
	case EvTimerFired:
		if ev.Timer == Timer6Retransmit && m.msgType == wire.MsgRelease6 {
			m.retransmit(now, rnd, out, func(*actions) {
				out.journal(m, "the Release exchange reached REL_MAX_RC with no Reply: the binding will be reclaimed when its valid lifetime expires (§18.2.7)")
				m.endExchange(out)
			})
			return
		}
		out.journal(m, fmt.Sprintf("timer %s fired while stopped: ignored", ev.Timer))
	case EvStop:
		out.journal(m, "already stopped")
	default:
		out.journal(m, fmt.Sprintf("event %s while stopped: ignored", ev.Kind))
	}
}

func (m *Machine6) stepInit6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvTimerFired:
		if ev.Timer != Timer6Delay {
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
			return
		}
		m.startExchange(now, rnd, m.pendingType, out)
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	case EvStart:
		out.journal(m, "already starting")
	case EvRelease:
		// Nothing has been granted, so there is nothing to give back.
		m.halt(out, ReasonReleased)
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

func (m *Machine6) stepSelecting6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvReceived:
		msg, ok := m.admit(ev, out)
		if !ok {
			return
		}
		if msg.Type != wire.MsgAdvertise {
			out.journal(m, fmt.Sprintf("a %s arrived while soliciting: ignored", msg.Type))
			return
		}
		m.takeAdvertise(now, rnd, msg, out)
	case EvTimerFired:
		switch ev.Timer {
		case Timer6Retransmit:
			if !m.windowDone {
				// §18.2.1: "A client MUST collect valid Advertise messages for
				// the first RT seconds". This firing is the end of that
				// window, not a retransmission — the two share one timer
				// because they are one deadline.
				m.windowDone = true
				if len(m.adverts) > 0 {
					out.journal(m, fmt.Sprintf("the first RT elapsed with %d Advertise message(s) collected", len(m.adverts)))
					m.selectAndRequest(now, rnd, out)
					return
				}
				out.journal(m, "the first RT elapsed with no Advertise message: retransmitting the Solicit (§18.2.1)")
			}
			m.retransmit(now, rnd, out, nil)
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
		}
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	case EvRelease:
		m.halt(out, ReasonReleased)
	case EvStart:
		out.journal(m, "already soliciting")
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

func (m *Machine6) stepRequesting6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvReceived:
		msg, ok := m.admit(ev, out)
		if !ok {
			return
		}
		if msg.Type != wire.MsgReply {
			out.journal(m, fmt.Sprintf("a %s arrived while requesting: ignored", msg.Type))
			return
		}
		m.takeReply(now, rnd, msg, out)
	case EvTimerFired:
		if ev.Timer != Timer6Retransmit {
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
			return
		}
		m.retransmit(now, rnd, out, func(o *actions) {
			// §18.2.2: "If the message exchange fails, the client takes an
			// action based on the client's local policy. Examples of actions
			// the client might take include the following: Select another
			// server from a list of servers known to the client ... Initiate
			// the server discovery process described in Section 18." This
			// client does both, in that order.
			m.tried = append(m.tried, m.server)
			if m.selectAndRequest(now, rnd, o) {
				o.journal(m, "the Request exchange reached REQ_MAX_RC: trying the next server that advertised (§18.2.2)")
				return
			}
			o.journal(m, "the Request exchange reached REQ_MAX_RC with no other server that advertised: restarting discovery (§18.2.2)")
			m.restartDiscovery(now, rnd, o)
		})
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	case EvRelease:
		m.halt(out, ReasonReleased)
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

func (m *Machine6) stepConfirming6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvReceived:
		msg, ok := m.admit(ev, out)
		if !ok {
			return
		}
		if msg.Type != wire.MsgReply {
			out.journal(m, fmt.Sprintf("a %s arrived while confirming: ignored", msg.Type))
			return
		}
		m.applyMaxRT(msg, out)
		st, _, err := msg.Options.Status()
		if err != nil {
			out.journal(m, "Reply to the Confirm carries a malformed Status Code option: "+err.Error())
			return
		}
		if st.Code == wire.StatusNotOnLink {
			// §18.2.10.3: "When the client only receives one or more Reply
			// messages with the NotOnLink status in response to a Confirm
			// message, the client performs DHCP server discovery as described
			// in Section 18."
			out.journal(m, "Reply to the Confirm says NotOnLink: the remembered addresses are not on this link, restarting discovery (§18.2.10.3)")
			m.resume = nil
			m.restartDiscovery(now, rnd, out)
			return
		}
		// §18.2.10.3: "If the client receives any Reply messages that indicate
		// a status of Success (explicit or implicit), the client can use the
		// addresses in the IA".
		out.journal(m, fmt.Sprintf("Reply to the Confirm says %s: the remembered addresses are on this link", st.Code))
		m.continueFromResume(now, rnd, out, "confirmed")
	case EvTimerFired:
		if ev.Timer != Timer6Retransmit {
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
			return
		}
		m.retransmit(now, rnd, out, func(o *actions) {
			// §18.2.3: "If the client receives no responses before the message
			// transmission process terminates, as described in Section 15, the
			// client SHOULD continue to use any leases, using the last known
			// lifetimes for those leases, and SHOULD continue to use any other
			// previously obtained configuration parameters."
			//
			// THE V4 MACHINE DOES THE OPPOSITE ON THE SAME SHAPE, and that is
			// the difference this arm carries: RFC 2131 §3.2(3) makes it a MAY
			// to keep using the address when an INIT-REBOOT gets no answer,
			// and the v4 machine declines the MAY and restarts. Here it is a
			// SHOULD and the addresses are kept.
			o.journal(m, "the Confirm exchange reached CNF_MAX_RD with no Reply: continuing with the last known lifetimes (§18.2.3)")
			m.continueFromResume(now, rnd, o, "unconfirmed")
		})
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	case EvRelease:
		m.halt(out, ReasonReleased)
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

func (m *Machine6) stepInfoRequesting6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvReceived:
		msg, ok := m.admit(ev, out)
		if !ok {
			return
		}
		if msg.Type != wire.MsgReply {
			out.journal(m, fmt.Sprintf("a %s arrived while information-requesting: ignored", msg.Type))
			return
		}
		m.applyMaxRT(msg, out)
		m.takeConfig(now, msg, out)
	case EvTimerFired:
		switch ev.Timer {
		case Timer6Retransmit:
			m.retransmit(now, rnd, out, nil)
		case Timer6Refresh:
			// §21.23: "When the client detects that the refresh time has
			// expired, it SHOULD try to update its configuration data by
			// sending an Information-request as specified in Section 18.2.6,
			// except that the client MUST delay sending the first
			// Information-request by a random amount of time between 0 and
			// INF_MAX_DELAY."
			d := randomDelay(m.params.InfMaxDelay, rnd)
			out.journal(m, "the information refresh time elapsed: another Information-request after "+d.String()+" (§21.23)")
			m.pendingType = wire.MsgInformationRequest
			out.set(m, Timer6Delay, d)
		case Timer6Delay:
			m.startExchange(now, rnd, wire.MsgInformationRequest, out)
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
		}
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	case EvRelease:
		m.halt(out, ReasonReleased)
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

func (m *Machine6) stepDAD6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvDADResult:
		m.takeDADResult(now, rnd, ev, out)
	case EvTimerFired:
		switch ev.Timer {
		case Timer6DAD:
			// A deadline with no result is a FAULT and never an acquisition.
			// D22's shape: an address nothing verified is not used, and the
			// evidence for a Decline — another node answering for the address
			// — is exactly what did not arrive.
			//
			// A LEASE ALREADY IN HAND — this is a renewal's new address, or a
			// resume being confirmed — IS NOT ENDED HERE. §18.2.8 says what
			// failing to acquire a binding means for a client that has one:
			// "The client SHOULD treat the failure to acquire a binding (due
			// to the conflict) as equivalent to not having received the
			// binding, insofar as how it behaves when sending Renew and Rebind
			// messages." Not having received it leaves the old one standing,
			// with its expiry armed; expireLease ends it on time.
			out.cancel(m, Timer6DAD)
			out.failed(m, ReasonDADIncomplete, fmt.Sprintf("no EvDADResult for %v within %s", m.dadWait, m.params.dadTimeout()))
			m.dropPending()
			m.restartDiscovery(now, rnd, out)
		case Timer6Retransmit:
			if m.msgType == wire.MsgDecline6 {
				m.retransmit(now, rnd, out, func(o *actions) {
					o.journal(m, "the Decline exchange reached DEC_MAX_RC with no Reply: done (§18.2.8)")
					m.finishDecline(now, rnd, o)
				})
				return
			}
			out.journal(m, "the retransmission timer fired with no v6 exchange in flight: ignored")
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
		}
	case EvReceived:
		if m.msgType != wire.MsgDecline6 {
			out.journal(m, "message received while waiting for duplicate address detection: ignored")
			return
		}
		msg, ok := m.admit(ev, out)
		if !ok {
			return
		}
		if msg.Type != wire.MsgReply {
			out.journal(m, fmt.Sprintf("a %s arrived while declining: ignored", msg.Type))
			return
		}
		// §18.2.10.2: "When the client receives a valid Reply message in
		// response to a Decline message, the client considers the Decline
		// event completed, regardless of the Status Code option(s) returned by
		// the server."
		out.journal(m, "Reply to the Decline: the decline is complete")
		m.finishDecline(now, rnd, out)
	case EvAddressLost:
		out.journal(m, "the address went away while duplicate address detection was running: nothing was bound")
		m.dropPending()
		m.restartDiscovery(now, rnd, out)
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	case EvRelease:
		m.halt(out, ReasonReleased)
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

func (m *Machine6) stepBound6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvTimerFired:
		switch ev.Timer {
		case Timer6Renew:
			m.enterRenewing(now, rnd, out)
		case Timer6Rebind:
			// Reachable from BOUND when the server sent no T1 and §21.4's
			// recommendation produced no renewal time, or when a renewal was
			// never armed.
			m.enterRebinding(now, rnd, out)
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
		}
	case EvAddressLost:
		// Lead ruling 8, sequencing §8.2 and RFC 4429 §3.3: an address can be
		// withdrawn under a bound lease, and the withdrawal is evidence that
		// somebody else has it. The server is told, because a binding this
		// client cannot use is one the server should not hand back to it.
		out.journal(m, "the address was withdrawn under a bound lease: declining it and restarting discovery (sequencing §8.2)")
		lost := m.lease
		m.loseLease(out, ReasonConflict)
		m.declineAll(now, rnd, lost, out, func(n Instant, r uint64, o *actions) {
			m.restartDiscovery(n, r, o)
		})
	case EvDADResult:
		if !ev.DAD.Duplicate {
			out.journal(m, "duplicate address detection reported "+ev.DAD.String()+" for a bound lease: nothing to do")
			return
		}
		out.journal(m, "duplicate address detection reported "+ev.DAD.String()+" for a bound lease: declining it and restarting discovery")
		lost := m.lease
		m.loseLease(out, ReasonConflict)
		m.declineAll(now, rnd, lost, out, func(n Instant, r uint64, o *actions) {
			m.restartDiscovery(n, r, o)
		})
	case EvRelease:
		m.release(now, rnd, out)
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	case EvReceived:
		out.journal(m, "message received while bound with no exchange in flight: ignored")
	case EvStart:
		out.journal(m, "already bound")
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

func (m *Machine6) stepRenewal6(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvReceived:
		msg, ok := m.admit(ev, out)
		if !ok {
			return
		}
		if msg.Type != wire.MsgReply {
			out.journal(m, fmt.Sprintf("a %s arrived while %s: ignored", msg.Type, m.state))
			return
		}
		m.takeReply(now, rnd, msg, out)
	case EvTimerFired:
		switch ev.Timer {
		case Timer6Retransmit:
			m.retransmit(now, rnd, out, nil)
		case Timer6Rebind:
			if m.state == State6Renewing {
				// §18.2.4: "The message exchange is terminated when the
				// earliest time T2 is reached, at which point the client
				// begins the Rebind message exchange (see Section 18.2.5)."
				m.enterRebinding(now, rnd, out)
				return
			}
			out.journal(m, "the rebind timer fired while already rebinding: ignored")
		case Timer6Renew:
			out.journal(m, "the renew timer fired while already renewing: ignored")
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
		}
	case EvAddressLost:
		out.journal(m, "the address was withdrawn while "+m.state.String()+": declining it and restarting discovery")
		lost := m.lease
		m.loseLease(out, ReasonConflict)
		m.declineAll(now, rnd, lost, out, func(n Instant, r uint64, o *actions) {
			m.restartDiscovery(n, r, o)
		})
	case EvRelease:
		m.release(now, rnd, out)
	case EvStop:
		m.halt(out, ReasonStopped)
	case EvLinkDown:
		m.halt(out, ReasonLinkDown)
	default:
		out.journal(m, fmt.Sprintf("event %s in %s: ignored", ev.Kind, m.state))
	}
}

// --------------------------------------------------------------- driving --

// begin starts the client: router discovery unconditionally (design §A.3.3
// interlock 1) and then whichever exchange the configuration calls for.
//
// NOTHING WAITS FOR A ROUTER ADVERTISEMENT. Interlock 1's whole content is that
// the Solicit goes out at EvStart regardless of what router discovery says or
// whether it says anything: a link with no router, or one whose advertisements
// are being lost, is a link where a DHCPv6 server may still be listening, and a
// client that waited for an M flag would be silent on exactly the networks
// where it is needed most.
func (m *Machine6) begin(now Instant, rnd uint64, out *actions) {
	m.adverts, m.windowDone, m.tried = nil, false, nil
	m.sendFailures = 0
	m.rsCount = 0
	m.solicitRouter(out)

	t := wire.MsgSolicit
	delay := m.params.SolMaxDelay
	switch {
	case m.resume.live():
		// Design §A.3.3 interlock 3 and §18.2.12: a client with remembered
		// addresses and no delegated prefixes confirms rather than solicits.
		t = wire.MsgConfirm
		delay = m.params.CnfMaxDelay
	}
	m.pendingType = t
	m.state = State6Init
	d := randomDelay(delay, rnd)
	out.journal(m, fmt.Sprintf("starting: first %s after %s", t, d))
	out.set(m, Timer6Delay, d)
}

// restartDiscovery is §18's server discovery, entered from every path that
// found the current binding unusable.
//
// RESTARTING DISCOVERY DOES NOT END A LEASE, and that is the one rule that
// makes every arm below consistent. A lease ends when its valid lifetimes run
// out (expireLease), when the address is taken by another node (the Decline
// arms), or when the caller stops, releases or loses the link — never merely
// because the machine went looking for a server again. §18.2.10.1 is explicit
// for the arm that forced the question, a Renew answered with an IA_NA that
// carries no address: "Leave unchanged any information about leases the client
// has recorded in the IA but that were not included in the IA from the server."
// §18.2.8 says the same of a Decline's aftermath: "The client SHOULD treat the
// failure to acquire a binding (due to the conflict) as equivalent to not
// having received the binding, insofar as how it behaves when sending Renew and
// Rebind messages."
//
// THE TIMERS ARE MADE TO AGREE WITH THAT. The renewal schedule belongs to the
// exchange that just ended and is cancelled; the expiry belongs to the lease
// and survives exactly as long as the lease does.
func (m *Machine6) restartDiscovery(now Instant, rnd uint64, out *actions) {
	m.resume = nil
	out.cancel(m, Timer6Renew)
	out.cancel(m, Timer6Rebind)
	if !m.haveLse {
		out.cancel(m, Timer6Expire)
	}
	m.begin(now, rnd, out)
}

// startExchange draws a transaction id, resets the Elapsed Time origin and
// sends the first message of an exchange.
//
// THE ELAPSED TIME ORIGIN IS RESET HERE AND NOWHERE ELSE, which is §21.9's
// "the elapsed-time field is set to 0 in the first message in the message
// exchange". A machine that reset it on every send would tell every server it
// had just started trying, and one that never reset it would saturate the
// field within eleven minutes of the client's boot and stay there.
func (m *Machine6) startExchange(now Instant, rnd uint64, t wire.MessageTypeV6, out *actions) {
	m.msgType = t
	m.xid = uint32(rnd) & (wire.MaxXID6 - 1)
	m.exchangeStart = now
	m.transmits = 0
	m.sched = m.schedule(t)
	m.rt = m.sched.First(rnd)

	switch t {
	case wire.MsgSolicit:
		// §18.2.1: "Also, the first RT MUST be selected to be strictly greater
		// than IRT by choosing RAND to be strictly greater than 0." First()
		// draws RAND over the whole ±0.1 range because §15 does; the Solicit's
		// extra constraint is applied here, where the exception belongs.
		if m.rt <= m.sched.IRT {
			m.rt = m.sched.IRT + 1
		}
		m.state = State6Selecting
		m.windowDone = false
		m.adverts = nil
	case wire.MsgRequest6:
		m.state = State6Requesting
	case wire.MsgConfirm:
		m.state = State6Confirming
	case wire.MsgInformationRequest:
		m.state = State6InfoRequesting
	case wire.MsgRenew:
		m.state = State6Renewing
	case wire.MsgRebind:
		m.state = State6Rebinding
	}
	m.transmit(now, out)
}

// transmit sends the message in flight and arms the retransmission timer.
func (m *Machine6) transmit(now Instant, out *actions) {
	msg, err := m.build(now, m.msgType)
	if err != nil {
		// Unreachable for a validated Params6 and handled anyway: a machine
		// that cannot build its own message must say so rather than emit
		// nothing and arm a timer that will try again forever.
		out.failed(m, ReasonTransport, "cannot build a "+m.msgType.String()+": "+err.Error())
		return
	}
	m.transmits++
	out.sendV6(m, msg, Dest{Addr: wire.AllDHCPRelayAgentsAndServers})
	out.set(m, Timer6Retransmit, m.rt)
}

// retransmit applies §15 to the exchange in flight, calling exhausted when the
// exchange has failed. A nil exhausted means the exchange cannot fail — MRC and
// MRD both zero, which is Solicit, Renew and Rebind.
func (m *Machine6) retransmit(now Instant, rnd uint64, out *actions, exhausted func(*actions)) {
	if m.sched.Exhausted(m.transmits, now.Sub(m.exchangeStart)) {
		if exhausted != nil {
			exhausted(out)
			return
		}
		// A schedule with no MRC and no MRD cannot report exhaustion, so this
		// is unreachable rather than ignored; journalled because "unreachable"
		// is a claim about the schedules above, not about the protocol.
		out.journal(m, "the "+m.msgType.String()+" exchange reported exhaustion with no bound configured: retransmitting anyway")
	}
	m.rt = m.sched.Next(m.rt, rnd)
	m.transmit(now, out)
}

// endExchange forgets the message in flight and disarms its timer.
func (m *Machine6) endExchange(out *actions) {
	m.msgType = 0
	m.declining = nil
	m.afterDecline = nil
	out.cancel(m, Timer6Retransmit)
}

func (m *Machine6) schedule(t wire.MessageTypeV6) Retransmit {
	switch t {
	case wire.MsgSolicit:
		return m.params.Solicit()
	case wire.MsgRequest6:
		return m.params.Request()
	case wire.MsgConfirm:
		return m.params.Confirm()
	case wire.MsgRenew:
		return m.params.Renew()
	case wire.MsgRebind:
		return m.params.Rebind()
	case wire.MsgInformationRequest:
		return m.params.InfoRequest()
	case wire.MsgRelease6:
		return m.params.Release()
	case wire.MsgDecline6:
		return m.params.Decline()
	default:
		return m.params.Solicit()
	}
}

// halt cancels everything and parks the machine, reporting a held lease lost.
func (m *Machine6) halt(out *actions, r Reason) {
	if m.haveLse {
		m.loseLease(out, r)
	}
	m.dropPending()
	m.msgType = 0
	m.declining = nil
	m.afterDecline = nil
	m.state = State6Stopped
	out.cancelAll(m)
}

// expireLease ends a lease whose valid lifetimes have all run out, in whatever
// state the machine is in.
//
// §18.2.5: "The message exchange is terminated when the valid lifetimes of all
// leases across all IAs have expired, at which time the client uses the Solicit
// message to locate a new DHCP server and sends a Request for the expired IAs
// to the new server."
//
// IT RESTARTS DISCOVERY ONLY FROM THE THREE STATES THAT WERE USING THE LEASE.
// From BOUND6, RENEWING6 and REBINDING6 the expiry is the end of the client's
// relationship with its server and §18.2.5 says what follows. From the
// discovery states the machine is ALREADY looking for a server — that is why
// it is holding an expiring lease there at all — and restarting would draw a
// fresh transaction id and reset the retransmission schedule of an exchange
// that is in flight. So the expiry there ends the lease and nothing else.
func (m *Machine6) expireLease(now Instant, rnd uint64, out *actions) {
	out.cancel(m, Timer6Expire)
	if !m.haveLse {
		out.journal(m, "the expiry timer fired with no lease held: nothing to end")
		return
	}
	out.journal(m, "every valid lifetime in the IA has expired while "+m.state.String()+" (§18.2.5)")
	m.loseLease(out, ReasonExpired)
	switch m.state {
	case State6Bound, State6Renewing, State6Rebinding:
		m.restartDiscovery(now, rnd, out)
	}
}

func (m *Machine6) loseLease(out *actions, r Reason) {
	if !m.haveLse {
		return
	}
	m.haveLse = false
	l := m.lease
	m.lease = Lease6{}
	out.stamp(m, Action{Kind: ActLeaseLost, Reason: r, Lease6: l})
}

func (m *Machine6) dropPending() {
	m.pending, m.havePending, m.pendingRenewal = Lease6{}, false, false
	m.dadWait, m.dadBad = nil, nil
}

// addrsOf is the addresses of a lease. It is a function of the LEASE rather
// than a method that reads m.lease, because every caller needs the addresses of
// a lease it is about to stop holding, and reading the field after loseLease
// has cleared it is how a decline loses its subject.
func addrsOf(l Lease6) []netip.Addr {
	out := make([]netip.Addr, 0, len(l.Addrs))
	for _, a := range l.Addrs {
		out = append(out, a.Addr)
	}
	return out
}

// noteActionFailed is R2: an action the machine emitted did not happen.
func (m *Machine6) noteActionFailed(now Instant, rnd uint64, ev Event, out *actions) {
	m.sendFailures++
	out.journal(m, fmt.Sprintf("%s failed (%s), consecutive failures %d", ev.Action, ev.Reason, m.sendFailures))
	if m.state == State6Renewing || m.state == State6Rebinding || m.state == State6Bound {
		// A HELD LEASE IS NEVER GIVEN UP FOR A SEND FAILURE, which is the v4
		// machine's rule and holds here for its reason: the lease already has
		// an expiry timer armed to report the loss when it arrives, so the
		// send-failure budget would only make the loss earlier and less
		// accurate.
		return
	}
	if m.sendFailures >= m.params.maxSendFailures() {
		out.failed(m, ReasonTransport, fmt.Sprintf("%d consecutive send failures: %s", m.sendFailures, ev.Reason))
		m.halt(out, ReasonTransport)
	}
}

// ------------------------------------------------------ message admission --

// admit is the §16 gate, applied before any §18.2 processing.
//
// Ring 0 refuses what it can see from the octets alone — a message type that is
// not a client type, a truncated option area. What is left needs the machine's
// own state, and §16.3 and §16.10 list it: the transaction id of the exchange
// in flight, a Server Identifier, and a Client Identifier that is this client's
// DUID.
//
// EVERY REFUSAL IS JOURNALLED BY NAME. Sequencing §2.6's rule is that a discard
// is invisible in a passing test: a machine that dropped a Reply for the wrong
// reason and a machine that dropped it for the right one look identical from
// outside, and the journal line is the only thing that separates them.
func (m *Machine6) admit(ev Event, out *actions) (*wire.MessageV6, bool) {
	msg := ev.MsgV6
	if msg == nil {
		out.journal(m, "nil v6 message: discarded")
		return nil, false
	}
	if m.msgType == 0 {
		out.journal(m, fmt.Sprintf("a %s arrived with no exchange in flight: discarded", msg.Type))
		return nil, false
	}
	// §16.1: "A client MUST leave the transaction ID unchanged in
	// retransmissions of a message." §16.3 and §16.10 both make a mismatch a
	// MUST-discard: "the 'transaction-id' field value does not match the value
	// the client used in its Solicit message".
	if msg.XID != m.xid {
		out.journal(m, fmt.Sprintf("%s xid=%06x does not match the outstanding %06x: discarded (§16.3, §16.10)", msg.Type, msg.XID, m.xid))
		return nil, false
	}
	// §16.3 and §16.10: "the message does not include a Server Identifier
	// option (see Section 21.3)."
	sids := msg.Options.Count(wire.OptV6ServerID)
	if sids == 0 {
		out.journal(m, fmt.Sprintf("%s carries no Server Identifier option: discarded (§16.3, §16.10)", msg.Type))
		return nil, false
	}
	if sids > 1 {
		// §21.3 defines one Server Identifier per message and §16 gives the
		// recipient the choice to "ignore such a message completely and just
		// discard it". Two of them make "the server that sent this" ambiguous,
		// and every later step — the Request's destination, the Renew's Server
		// Identifier, the Decline's — depends on that answer being one value.
		out.journal(m, fmt.Sprintf("%s carries %d Server Identifier options: discarded, the sender is ambiguous (§16, §21.3)", msg.Type, sids))
		return nil, false
	}
	// §16.3: "the message does not include a Client Identifier option (see
	// Section 21.2)" and "the contents of the Client Identifier option do not
	// match the client's DUID". §16.10 says the same of a Reply to a message
	// that carried one, and every message this client sends carries one.
	cid, ok := msg.Options.First(wire.OptV6ClientID)
	if !ok {
		out.journal(m, fmt.Sprintf("%s carries no Client Identifier option: discarded (§16.3, §16.10)", msg.Type))
		return nil, false
	}
	if !sameDUID(cid, m.params.DUID) {
		out.journal(m, fmt.Sprintf("%s carries a Client Identifier that is not ours: discarded (§16.3, §16.10)", msg.Type))
		return nil, false
	}
	return msg, true
}

// applyMaxRT is §18.2.9's and §18.2.10's MUST, applied to every admitted
// message including the ones the machine then discards.
//
// §18.2.9: "The client MUST process any SOL_MAX_RT option (see Section 21.24)
// and INF_MAX_RT option (see Section 21.25) present in an Advertise message,
// even if the message contains a Status Code option (see Section 21.13)
// indicating a failure, and the Advertise message will be discarded by the
// client." §18.2.10 repeats it for a Reply.
//
// THE ORDER MATTERS AND IS WHY THIS IS A SEPARATE STEP: a client that applied
// the option only on the path where it kept the message would take a server's
// back-off instruction from the servers that were working and ignore it from
// the one that was telling it to slow down.
func (m *Machine6) applyMaxRT(msg *wire.MessageV6, out *actions) {
	applied, ignored, err := m.params.ApplyOptions(msg.Options)
	if err != nil {
		out.journal(m, "SOL_MAX_RT/INF_MAX_RT option: "+err.Error())
		return
	}
	for _, c := range applied {
		var v Duration
		if c == wire.OptV6SolMaxRTCode {
			v = m.params.SolMaxRT
		} else {
			v = m.params.InfMaxRT
		}
		out.journal(m, fmt.Sprintf("%s from the %s: now %s", c, msg.Type, v))
	}
	for _, c := range ignored {
		out.journal(m, fmt.Sprintf("%s from the %s is outside 60..86400: ignored (§21.24, §21.25)", c, msg.Type))
	}
}

// ------------------------------------------------------------- selecting --

// takeAdvertise applies §18.2.9 to one Advertise.
func (m *Machine6) takeAdvertise(now Instant, rnd uint64, msg *wire.MessageV6, out *actions) {
	m.applyMaxRT(msg, out)

	sid, _ := msg.Options.First(wire.OptV6ServerID)
	st, _, err := msg.Options.Status()
	if err != nil {
		// The error is checked BEFORE the value, which is the whole of carried
		// row 4: wire.StatusMalformed makes the value non-Success-identical so
		// that forgetting this check is visible, and checking it is what makes
		// forgetting impossible.
		out.journal(m, "Advertise carries a malformed Status Code option: ignored for selection, its SOL_MAX_RT was applied")
		return
	}
	if st.Code != wire.StatusSuccess {
		out.journal(m, fmt.Sprintf("Advertise says %s: ignored for selection, its SOL_MAX_RT was applied (§18.2.9)", st.Code))
		return
	}

	res, notes := readIA(msg.Options, m.params.IAID)
	for _, n := range notes {
		out.journal(m, n)
	}
	if len(res.addrs) == 0 {
		// §18.2.9: "The client MUST ignore any Advertise message that contains
		// no addresses (IA Address options (see Section 21.6) encapsulated in
		// IA_NA options (see Section 21.4)) and no delegated prefixes ... with
		// the exception that the client: MUST process an included SOL_MAX_RT
		// option and MUST process an included INF_MAX_RT option."
		out.journal(m, "Advertise offers no address in our IA_NA: ignored for selection, its SOL_MAX_RT was applied (§18.2.9)")
		return
	}
	pref, _, perr := msg.Options.Preference()
	if perr != nil {
		// §21.8's option is one octet. A malformed one is not evidence of a
		// preference, and "Any valid Advertise that does not include a
		// Preference option is considered to have a preference value of 0" is
		// the value for an absent one — which is what a malformed one is
		// closest to.
		out.journal(m, "Advertise carries a malformed Preference option: "+perr.Error()+"; treated as preference 0")
		pref = 0
	}

	a := advert6{server: sid, pref: pref, addrs: res.addrs, t1: res.t1, t2: res.t2, seq: len(m.adverts)}
	m.adverts = append(m.adverts, a)
	out.journal(m, fmt.Sprintf("Advertise collected: preference %d, %d address(es)", pref, len(res.addrs)))

	switch {
	case pref == 255:
		// §18.2.1: "If the client receives a valid Advertise message that
		// includes a Preference option with a preference value of 255, the
		// client immediately begins a client-initiated message exchange (as
		// described in Section 18.2.2) by sending a Request message to the
		// server from which the Advertise message was received."
		out.journal(m, "the Advertise carries preference 255: requesting immediately without waiting out the collection window (§18.2.1)")
		m.windowDone = true
		m.selectAndRequest(now, rnd, out)
	case m.windowDone:
		// §18.2.1: "The client terminates the retransmission process as soon as
		// it receives any valid Advertise message, and the client acts on the
		// received Advertise message without waiting for any additional
		// Advertise messages." That sentence is about the period AFTER the
		// first RT, when the client is retransmitting rather than collecting.
		out.journal(m, "an Advertise arrived after the collection window: acting on it at once (§18.2.1)")
		m.selectAndRequest(now, rnd, out)
	}
}

// selectAndRequest picks a server per §18.2.9 and sends the Request. It reports
// whether there was one to pick.
//
// HIGHEST PREFERENCE, TIES TO FIRST-ARRIVED. §18.2.9 makes only the first half
// normative — "Those Advertise messages with the highest server preference
// value SHOULD be preferred over all other Advertise messages" — and leaves the
// rest to the client ("The client MAY choose a less preferred server if that
// server has a better set of advertised parameters"). First-arrived is this
// client's tie-break and it is recorded in advert6.seq rather than left to the
// slice order, so the choice is a stated rule and not an artefact.
func (m *Machine6) selectAndRequest(now Instant, rnd uint64, out *actions) bool {
	best := -1
	for i, a := range m.adverts {
		if m.alreadyTried(a.server) {
			continue
		}
		if best < 0 || a.pref > m.adverts[best].pref ||
			(a.pref == m.adverts[best].pref && a.seq < m.adverts[best].seq) {
			best = i
		}
	}
	if best < 0 {
		return false
	}
	a := m.adverts[best]
	m.server = a.server
	m.pending = Lease6{IAID: m.params.IAID, Addrs: a.addrs, ServerDUID: a.server, T1: a.t1, T2: a.t2}
	out.journal(m, fmt.Sprintf("selected the Advertise with preference %d (arrival %d of %d): requesting", a.pref, a.seq+1, len(m.adverts)))
	m.startExchange(now, rnd, wire.MsgRequest6, out)
	return true
}

func (m *Machine6) alreadyTried(sid []byte) bool {
	for _, t := range m.tried {
		if sameDUID(t, sid) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------- reply --

// takeReply is §18.2.10 for a Reply to a Request, a Renew or a Rebind.
func (m *Machine6) takeReply(now Instant, rnd uint64, msg *wire.MessageV6, out *actions) {
	m.applyMaxRT(msg, out)
	renewal := m.state == State6Renewing || m.state == State6Rebinding

	st, _, err := msg.Options.Status()
	if err != nil {
		out.journal(m, "Reply carries a malformed Status Code option: ignored, the exchange continues")
		return
	}
	switch st.Code {
	case wire.StatusUnspecFail:
		// §18.2.10: "If the client receives a Reply message with a status code
		// of UnspecFail, the server is indicating that it was unable to
		// process the client's message due to an unspecified failure
		// condition. If the client retransmits the original message to the
		// same server to retry the desired operation, the client MUST limit
		// the rate at which it retransmits the message and limit the duration
		// of the time during which it retransmits the message (see
		// Section 14.1)." The retransmission schedule already in flight IS
		// that limit, and the §14.1 bucket in ring 2 is the other half; so the
		// Reply is noted and nothing else changes.
		out.journal(m, "Reply says UnspecFail: the retransmission schedule continues unchanged (§18.2.10)")
		return
	case wire.StatusNotOnLink:
		// §18.2.10.1: "If the client receives a NotOnLink status from the
		// server in response to a Solicit (with a Rapid Commit option; see
		// Section 21.14) or a Request, the client can either reissue the
		// message without specifying any addresses or restart the DHCP server
		// discovery process (see Section 18 or Section 18.2.13)."
		//
		// A STATED BOUND, not a quotation: that sentence and §18.2.10.3's
		// ("When the client only receives one or more Reply messages with the
		// NotOnLink status in response to a Confirm message, the client
		// performs DHCP server discovery as described in Section 18.") are
		// both written for a client that holds no lease yet, and the RFC says
		// nothing about a NotOnLink answering a Renew or a Rebind. THIS
		// CLIENT ENDS THE LEASE AT THE TRANSITION, because NotOnLink is the
		// server saying the address is not on the link the client is on, and
		// that is the one status under which continuing to use it is the
		// failure this plugin exists to avoid. It is v4's DHCPNAK arm
		// (RFC 2131 3.2(3), "it cannot reuse its remembered network address")
		// answered the same way, which is D30.
		if m.haveLse {
			out.journal(m, "Reply says NotOnLink while a lease is held: the address is not on this link, ending the lease (stated bound; §18.2.10.1 and §18.2.10.3 both address a client that holds none)")
			m.loseLease(out, ReasonNak)
		}
		out.journal(m, "Reply says NotOnLink: restarting discovery (§18.2.10.1)")
		m.restartDiscovery(now, rnd, out)
		return
	}

	l, notes, iaStatus, ok := leaseFromReply(msg, m.params.IAID, m.exchangeStart)
	for _, n := range notes {
		out.journal(m, n)
	}
	if iaStatus == wire.StatusNoBinding && renewal {
		// §18.2.10.1: the client "Sends a Request message to the server that
		// responded if any of the IAs in the Reply message contain the
		// NoBinding status code. The client places IA options in this message
		// for all IAs."
		out.journal(m, "the IA_NA says NoBinding: requesting from the server that answered (§18.2.10.1)")
		m.server, _ = msg.Options.First(wire.OptV6ServerID)
		m.startExchange(now, rnd, wire.MsgRequest6, out)
		return
	}
	if !ok {
		if iaStatus == wire.StatusNoAddrsAvail {
			out.journal(m, "the IA_NA says NoAddrsAvail: this server has nothing for us (§18.2.10.1)")
		}
		// §18.2.10.1: "If the Reply message contains any IAs but the client
		// finds no usable addresses and/or delegated prefixes in any of these
		// IAs, the client may either try another server (perhaps restarting the
		// DHCP server discovery process) or use the Information-request message
		// to obtain other configuration information only."
		if !renewal {
			m.tried = append(m.tried, m.server)
			if m.selectAndRequest(now, rnd, out) {
				out.journal(m, "no usable address in the Reply: trying the next server that advertised (§18.2.10.1)")
				return
			}
		}
		// THE LEASE IN HAND SURVIVES THIS, and §18.2.10.1 says so of exactly
		// this case: "Leave unchanged any information about leases the client
		// has recorded in the IA but that were not included in the IA from the
		// server." So discovery restarts with the lease still held and its
		// expiry still armed; expireLease ends it when the valid lifetimes run
		// out, in whatever state the search has reached by then.
		out.journal(m, "no usable address in the Reply: restarting discovery (§18.2.10.1)")
		m.restartDiscovery(now, rnd, out)
		return
	}

	m.server = l.ServerDUID
	m.pending, m.havePending, m.pendingRenewal = l, true, renewal
	out.cancel(m, Timer6Retransmit)
	m.msgType = 0

	// §18.2.10.1: "The client MUST perform duplicate address detection as per
	// Section 5.4 of [RFC4862], which does list some exceptions, on each of the
	// received addresses in any IAs on which it has not performed duplicate
	// address detection during processing of any of the previous Reply messages
	// from the server. The client performs the duplicate address detection
	// before using the received addresses for any traffic."
	//
	// "ON WHICH IT HAS NOT PERFORMED" IS THE WHOLE OF THE RENEWAL ARM: a Renew
	// that comes back with the address the client already holds and has already
	// checked runs no DAD, and one that comes back with a new address runs it
	// on the new address only.
	fresh := m.unchecked(l)
	if len(fresh) == 0 {
		m.enterBound(now, rnd, l, out)
		return
	}
	m.startDAD(fresh, out)
}

// unchecked is the addresses of l that this machine has not already had a
// duplicate address detection result for.
func (m *Machine6) unchecked(l Lease6) []netip.Addr {
	var fresh []netip.Addr
	for _, a := range l.Addrs {
		held := false
		for _, h := range m.lease.Addrs {
			if h.Addr == a.Addr {
				held = true
				break
			}
		}
		if !held {
			fresh = append(fresh, a.Addr)
		}
	}
	return fresh
}

// startDAD asks ring 3 for RFC 4862 §5.4 on each address and arms the
// machine's own deadline for the answers.
func (m *Machine6) startDAD(addrs []netip.Addr, out *actions) {
	m.dadWait = append([]netip.Addr(nil), addrs...)
	m.dadBad = nil
	m.state = State6DAD
	for _, a := range addrs {
		out.stamp(m, Action{Kind: ActStartDAD, Target: a})
	}
	out.set(m, Timer6DAD, m.params.dadTimeout())
}

// takeDADResult is the gate: no Acquired is emitted until every address this
// machine asked about has come back free.
func (m *Machine6) takeDADResult(now Instant, rnd uint64, ev Event, out *actions) {
	want := -1
	for i, a := range m.dadWait {
		if a == ev.DAD.Addr {
			want = i
			break
		}
	}
	if want < 0 {
		// A result for an address this machine never asked about. It is not an
		// error and it is not a conflict: ring 3 runs DAD for the chassis too,
		// and a client that acted on somebody else's answer would decline an
		// address it does not hold.
		out.journal(m, "duplicate address detection reported "+ev.DAD.String()+", which this machine did not ask about: ignored")
		return
	}
	m.dadWait = append(m.dadWait[:want], m.dadWait[want+1:]...)
	if ev.DAD.Duplicate {
		m.dadBad = append(m.dadBad, ev.DAD.Addr)
	}
	out.journal(m, "duplicate address detection reported "+ev.DAD.String())
	if len(m.dadWait) > 0 {
		return
	}
	out.cancel(m, Timer6DAD)

	if len(m.dadBad) == 0 {
		l := m.pending
		m.dropPending()
		m.enterBound(now, rnd, l, out)
		return
	}

	// ANY DUPLICATE FAILS THE WHOLE IA, and that is a BOUND rather than a
	// reading of §18.2.8. The section says "The client SHOULD NOT send a
	// Release message for other bindings it may have received just because it
	// sent a Decline message. The client SHOULD retain the non-conflicting
	// bindings." Retaining part of an IA means running a Decline exchange and a
	// bound lease at the same time, which is a second exchange in flight and a
	// second retransmission schedule; this client asks for one IA_NA with one
	// address (Params6.IAID, Params6.Hint) and the chassis installs one
	// address, so the retained set is empty in every configuration this library
	// can produce. The SHOULD is declined here, in writing, rather than
	// implemented untested.
	bad := m.pending
	note := fmt.Sprintf("duplicate address detection found %d of %d address(es) in use: declining the IA (§18.2.10.1)", len(m.dadBad), len(bad.Addrs))
	out.journal(m, note)

	// WHAT THE CHASSIS IS TOLD WHEN NOTHING WAS ACQUIRED, and it is
	// machine_acd.go's acdConflict arm read for this family. A duplicate found
	// here produces no ActLeaseLost, because loseLease is guarded on holding a
	// lease and this address was only pending; without this action the whole
	// event would exist in the journal alone, which is not something a counter
	// can be derived from. The asymmetry it removes is sharper than the v4
	// one: the DAD DEADLINE arm above already reports ReasonDADIncomplete, so
	// before this line "no answer arrived" reached the caller and "the answer
	// was: another node has it" did not.
	//
	// held is read BEFORE declineAll, and the two outcomes are MUTUALLY
	// EXCLUSIVE for ring 2's reason: a renewal that moved onto a new address
	// runs this check with a lease still held, and that path announces the
	// loss through ActLeaseLost instead. Ring 2 adds the two into one
	// ConflictsDetected, so a machine that emitted both would double-count.
	held := m.haveLse
	m.dropPending()
	m.declineAll(now, rnd, bad, out, func(n Instant, r uint64, o *actions) {
		m.restartDiscovery(n, r, o)
	})
	if !held {
		out.failed(m, ReasonConflict, note)
	}
}

// ---------------------------------------------------------- bound states --

// enterBound installs the lease and arms T1, T2 and the expiry.
func (m *Machine6) enterBound(now Instant, rnd uint64, l Lease6, out *actions) {
	m.msgType = 0
	out.cancel(m, Timer6Retransmit)
	m.sendFailures = 0

	had, prev := m.haveLse, m.lease
	m.lease, m.haveLse = l, true
	m.state = State6Bound
	m.resume = nil

	d := l.Deadlines()
	if d.Note != "" {
		out.journal(m, d.Note)
	}
	switch {
	case !had:
		out.stamp(m, Action{Kind: ActLeaseAcquired, Lease6: l, Requested: m.params.hintAddr()})
	default:
		out.stamp(m, Action{Kind: ActLeaseRenewed, Lease6: l})
		if !prev.Equal(l) {
			out.stamp(m, Action{Kind: ActLeaseChanged, Lease6: l})
		}
	}
	m.armDeadline(now, Timer6Renew, d.Renew, d.HasRenew, out)
	m.armDeadline(now, Timer6Rebind, d.Rebind, d.HasRebind, out)
	m.armDeadline(now, Timer6Expire, d.Expire, d.HasExpire, out)
}

// armDeadline arms t for ts, or cancels it when there is none.
//
// A deadline already in the past arms for zero rather than for a negative
// interval: ring 3's timer table takes a duration, and a negative one is a
// timer that either never fires or fires immediately depending on whose
// implementation it reaches.
func (m *Machine6) armDeadline(now Instant, t TimerID, ts Instant, has bool, out *actions) {
	if !has {
		out.cancel(m, t)
		return
	}
	d := ts.Sub(now)
	if d < 0 {
		d = 0
	}
	out.set(m, t, d)
}

// enterRenewing is T1. §18.2.4: "At time T1, the client initiates a Renew/Reply
// message exchange to extend the lifetimes on any leases in the IA."
func (m *Machine6) enterRenewing(now Instant, rnd uint64, out *actions) {
	if len(m.lease.ServerDUID) == 0 {
		// §18.2.4: "The client MUST include a Server Identifier option (see
		// Section 21.3) in the Renew message, identifying the server with
		// which the client most recently communicated." A lease with no server
		// DUID cannot produce that option, so the machine goes straight to the
		// Rebind, which §18.2.5 says carries no Server Identifier at all.
		out.journal(m, "the lease names no server: rebinding instead of renewing (§18.2.4 requires a Server Identifier)")
		m.enterRebinding(now, rnd, out)
		return
	}
	m.server = m.lease.ServerDUID
	out.journal(m, "T1: renewing")
	m.startExchange(now, rnd, wire.MsgRenew, out)
}

// enterRebinding is T2. §18.2.5: "At time T2 (which will only be reached if the
// server to which the Renew message was sent starting at time T1 has not
// responded), the client initiates a Rebind/Reply message exchange with any
// available server."
func (m *Machine6) enterRebinding(now Instant, rnd uint64, out *actions) {
	m.server = nil
	out.journal(m, "T2: rebinding")
	m.startExchange(now, rnd, wire.MsgRebind, out)
}

// continueFromResume turns the remembered binding into a lease, which is both
// §18.2.10.3's confirmed path and §18.2.3's silence path.
//
// DAD RUNS ON BOTH, which is lead ruling 2 and the v4 machine's shape: the
// INIT-REBOOT DHCPACK runs RFC 5227's check unconditionally because the chassis
// re-installs the address on restart and nothing has verified it on THIS link.
// A Confirm that was answered says the addresses belong to this link; it says
// nothing about whether another node has since taken one.
func (m *Machine6) continueFromResume(now Instant, rnd uint64, out *actions, how string) {
	r := m.resume
	if !r.live() {
		out.journal(m, "nothing remembered to continue with: restarting discovery")
		m.restartDiscovery(now, rnd, out)
		return
	}
	l := Lease6{
		IAID:       m.params.IAID,
		Addrs:      append([]Addr6(nil), r.Addrs...),
		ServerDUID: append([]byte(nil), r.ServerDUID...),
		T1:         r.T1,
		T2:         r.T2,
		Start:      now,
	}
	m.pending, m.havePending, m.pendingRenewal = l, true, false
	m.msgType = 0
	out.cancel(m, Timer6Retransmit)
	out.journal(m, fmt.Sprintf("continuing with the %s addresses and their last known lifetimes", how))
	addrs := make([]netip.Addr, 0, len(l.Addrs))
	for _, a := range l.Addrs {
		addrs = append(addrs, a.Addr)
	}
	m.startDAD(addrs, out)
}

// -------------------------------------------------- release and decline --

// release is §18.2.7. The lease is reported lost BEFORE the Release goes out,
// because §18.2.7 requires it: "The client MUST stop using all of the leases
// being released before the client begins the Release message exchange process.
// For an address, this means the address MUST have been removed from the
// interface."
func (m *Machine6) release(now Instant, rnd uint64, out *actions) {
	if !m.haveLse {
		out.journal(m, "release with no lease held: nothing to give back")
		m.halt(out, ReasonReleased)
		return
	}
	m.server = m.lease.ServerDUID
	addrs := m.lease.Addrs
	m.loseLease(out, ReasonReleased)
	out.cancel(m, Timer6Renew)
	out.cancel(m, Timer6Rebind)
	out.cancel(m, Timer6Expire)
	m.releasing = addrs
	m.state = State6Stopped
	if len(m.server) == 0 {
		// §18.2.7's Server Identifier is a MUST and this client cannot invent
		// one. Nothing is sent; the binding is reclaimed when its valid
		// lifetime expires, which the same section says is what happens when a
		// release fails.
		out.journal(m, "the lease names no server: no Release can be sent, the binding will be reclaimed when its valid lifetime expires (§18.2.7)")
		out.cancelAll(m)
		return
	}
	m.startExchange(now, rnd, wire.MsgRelease6, out)
}

// declineAll is §18.2.8, run from State6DAD.
//
// IT RUNS IN State6DAD RATHER THAN IN A STATE OF ITS OWN, and the reason is the
// state enumeration rather than convenience: State6DAD is the window from the
// Reply that granted an address to the moment that address is either bound or
// given back, and the Decline is the second of those two endings. A state per
// exchange would put DECLINING and RELEASING in AllStates6 with one arm each,
// and the totality test's domain would grow by two states in which the only
// thing that can happen is the exchange that named them.
//
// THE SERVER IDENTIFIER COMES FROM THE LEASE BEING DECLINED AND NOT FROM
// m.server, and that is §18.2.8 read literally: "The client MUST include a
// Server Identifier option (see Section 21.3) in the Decline message,
// identifying the server that allocated the lease(s)." The server that
// allocated the lease is a property of the lease; m.server is the server of
// the exchange currently in flight, which is a different fact and is not set
// on every path that can reach a Decline.
//
// MEASURED 2026-09-06, and it is why this takes a Lease6 rather than a list of
// addresses: m.server is assigned only on the Solicit path, in the NoBinding
// arm, in takeReply, in enterRenewing and in release, and is set to nil in
// enterRebinding. continueFromResume — the whole resume path — never set it,
// so a client that came back from a restart, confirmed its addresses and then
// found one in use journalled "declining it" immediately followed by "nothing
// to decline", and no Decline ever left the host. §18.2.5 makes the same hole
// in REBINDING, where the Rebind carries no Server Identifier at all: the
// exchange has no server, and the LEASE still names the one that allocated it.
func (m *Machine6) declineAll(now Instant, rnd uint64, l Lease6, out *actions, then func(Instant, uint64, *actions)) {
	addrs := addrsOf(l)
	if len(addrs) == 0 {
		out.journal(m, "nothing to decline: the lease carries no address (§18.2.8)")
		then(now, rnd, out)
		return
	}
	if len(l.ServerDUID) == 0 {
		// §18.2.8's Server Identifier is a MUST and this client cannot invent
		// one. This is a STATED BOUND rather than a silent refusal: a lease
		// with no server DUID can only come from a Resume6 the caller built
		// without one, and the server learns the address is in use when the
		// binding is not renewed.
		out.journal(m, "the lease names no server: no Decline can be sent (§18.2.8 requires a Server Identifier identifying the server that allocated the lease)")
		then(now, rnd, out)
		return
	}
	m.server = append([]byte(nil), l.ServerDUID...)
	m.declining = append([]netip.Addr(nil), addrs...)
	m.afterDecline = then
	m.state = State6DAD
	out.cancel(m, Timer6DAD)
	m.startExchange(now, rnd, wire.MsgDecline6, out)
	m.state = State6DAD
}

func (m *Machine6) finishDecline(now Instant, rnd uint64, out *actions) {
	then := m.afterDecline
	m.endExchange(out)
	if then == nil {
		m.restartDiscovery(now, rnd, out)
		return
	}
	then(now, rnd, out)
}

// ------------------------------------------------------------ stateless --

// takeConfig is §18.2.10.4 and §21.23: what an Information-request Reply gives
// and when to ask again.
func (m *Machine6) takeConfig(now Instant, msg *wire.MessageV6, out *actions) {
	var c Config6
	if dns, err := msg.Options.DNSServers(); err != nil {
		out.journal(m, "DNS Recursive Name Server option: "+err.Error())
	} else {
		c.DNS = dns
	}
	if s, err := msg.Options.DomainSearch(); err != nil {
		out.journal(m, "Domain Search List option: "+err.Error())
	} else {
		c.Search = s
	}
	secs, ok, err := msg.Options.Uint32V6(wire.OptV6InfoRefresh)
	switch {
	case err != nil:
		out.journal(m, "Information Refresh Time option: "+err.Error())
	case ok:
		c.RefreshTime = SecondsToDuration(secs)
	}

	// ONE FACT, DERIVED ONCE. §21.23's floor and its default are what this
	// client will actually do, so they are what the caller is TOLD: the raw
	// option value is used to compute the bounded one and then never seen
	// again. MEASURED 2026-09-06: reported and armed were derived separately
	// and disagreed — an absent option reported 0 (which Configuration.Refresh
	// documents as "never") while 86400s was armed, and 60s reported 60s while
	// 600s was armed. A caller that logged the reported value could not have
	// told when the next Information-request was due.
	c.RefreshTime = m.refreshTime(c.RefreshTime, ok, out)
	m.config = c
	m.refresh = c.RefreshTime
	m.msgType = 0
	out.cancel(m, Timer6Retransmit)
	out.stamp(m, Action{Kind: ActConfigured, Config: c})
	if m.refresh.IsInfinite() {
		// §21.23: "As per Section 7.7, the value 0xffffffff is taken to mean
		// 'infinity' and implies that the client should not refresh its
		// configuration data without some other trigger (such as detecting
		// movement to a new link)."
		out.journal(m, "the information refresh time is infinite: no refresh is armed (§21.23)")
		out.cancel(m, Timer6Refresh)
		return
	}
	out.set(m, Timer6Refresh, m.refresh)
}

// refreshTime applies §21.23's two rules to the value that arrived.
func (m *Machine6) refreshTime(v Duration, present bool, out *actions) Duration {
	if !present {
		// §21.23: "If the Reply to an Information-request message does not
		// contain this option, the client MUST behave as if the option with
		// the value IRT_DEFAULT was provided."
		out.journal(m, "the Reply carries no Information Refresh Time option: using IRT_DEFAULT "+m.params.IRTDefault.String()+" (§21.23)")
		return m.params.IRTDefault
	}
	if v.IsInfinite() {
		return Infinite
	}
	if v < m.params.IRTMinimum {
		// §21.23: "A client MUST use the refresh time IRT_MINIMUM if it
		// receives the option with a value less than IRT_MINIMUM."
		out.journal(m, "the Information Refresh Time "+v.String()+" is below IRT_MINIMUM: using "+m.params.IRTMinimum.String()+" (§21.23)")
		return m.params.IRTMinimum
	}
	return v
}

// ------------------------------------------------------ router discovery --

// solicitRouter emits one RFC 4861 §4.1 Router Solicitation and arms the next.
//
// §6.3.7: "To obtain Router Advertisements quickly, a host SHOULD transmit up
// to MAX_RTR_SOLICITATIONS Router Solicitation messages, each separated by at
// least RTR_SOLICITATION_INTERVAL seconds."
func (m *Machine6) solicitRouter(out *actions) {
	if m.router.Seen || m.rsCount >= m.params.routerSolicitations() {
		return
	}
	m.rsCount++
	out.stamp(m, Action{Kind: ActSendRouterSolicit})
	if m.rsCount < m.params.routerSolicitations() {
		out.set(m, Timer6RouterSolicit, m.params.routerSolicitInterval())
		return
	}
	out.cancel(m, Timer6RouterSolicit)
}

func (m *Machine6) routerSolicitTick(out *actions) {
	if m.router.Seen {
		// Unreachable while observeRouter cancels the timer, and handled
		// anyway: a timer that had already fired when the RA arrived is still
		// on its way here.
		out.journal(m, "the router solicitation timer fired after a Router Advertisement had arrived: ignored")
		out.cancel(m, Timer6RouterSolicit)
		return
	}
	m.solicitRouter(out)
}

// observeRouter reports every Router Advertisement and applies design §A.3.3
// interlock 1.
//
// EVERY RA PRODUCES ActRouterObserved, IN EVERY STATE. It is a diagnostic, not
// a lease event (design Q2): a caller that waited out its own deadline on a
// link whose router says there is no DHCPv6 here has not failed, and this is
// the only thing that tells it which of the two happened.
func (m *Machine6) observeRouter(now Instant, rnd uint64, ev Event, out *actions) {
	if ev.RA == nil {
		out.journal(m, "nil Router Advertisement: ignored")
		return
	}
	first := !m.router.Seen
	m.router = RouterObservation{Seen: true, Managed: ev.RA.Managed, Other: ev.RA.Other}
	out.stamp(m, Action{Kind: ActRouterObserved, Router: m.router})
	if first {
		// §6.3.7's schedule exists "To obtain Router Advertisements quickly".
		// One has arrived, so the remaining solicitations would be asking a
		// question that has been answered.
		out.cancel(m, Timer6RouterSolicit)
	}

	switch {
	case ev.RA.Managed:
		// M=1 is the network saying addresses are available over DHCPv6, which
		// is what this client is already doing. Interlock 1: nothing changes.
		out.journal(m, "Router Advertisement M=1: the Solicit exchange already under way is what M asks for")
	case ev.RA.Other && m.state == State6Selecting:
		// M=0, O=1 is "no addresses here, but other configuration is". The
		// Solicit will never be answered on such a link, so the client
		// switches to §18.2.6's stateless exchange.
		out.journal(m, "Router Advertisement M=0 O=1 while soliciting: switching to Information-request (design §A.3.3 interlock 1)")
		out.cancel(m, Timer6Retransmit)
		m.adverts, m.windowDone = nil, false
		m.startExchange(now, rnd, wire.MsgInformationRequest, out)
	case ev.RA.Other:
		out.journal(m, "Router Advertisement M=0 O=1 in "+m.state.String()+": noted, the exchange in flight is unchanged")
	default:
		// RFC 4861 §4.2: "If neither M nor O flags are set, this indicates
		// that no information is available via DHCPv6." The Solicit continues
		// anyway — interlock 1 — because a router that has not been told about
		// the DHCPv6 server is a configuration this client cannot verify, and
		// the caller's deadline is what ends the attempt.
		out.journal(m, "Router Advertisement M=0 O=0: RFC 4861 §4.2 says no information is available via DHCPv6; the exchange continues and the caller's deadline ends it")
	}
}

// -------------------------------------------------------------- messages --

// build renders one message of the exchange in flight.
//
// THE OPTIONS EVERY MESSAGE CARRIES ARE ADDED HERE ONCE. §18.2.1 through
// §18.2.8 each repeat two of them — "The client MUST include a Client
// Identifier option (see Section 21.2) to identify itself to the server" and
// "The client MUST include an Elapsed Time option (see Section 21.9) to
// indicate how long the client has been trying to complete the current DHCP
// message exchange" — and a per-message builder is how one of the eight comes
// to be missing one of the two.
func (m *Machine6) build(now Instant, t wire.MessageTypeV6) (*wire.MessageV6, error) {
	msg := &wire.MessageV6{Type: t, XID: m.xid}
	msg.Options = append(msg.Options, wire.OptionV6{
		Code: wire.OptV6ClientID,
		Data: append([]byte(nil), m.params.DUID...),
	})

	// §18.2.4 and §18.2.7 and §18.2.8 each make the Server Identifier a MUST;
	// §18.2.5's Rebind is the one message built "as described in
	// Section 18.2.4, with the following differences: ... The client does not
	// include the Server Identifier option (see Section 21.3) in the Rebind
	// message."
	// §18.2.1's Solicit, §18.2.3's Confirm and §18.2.6's Information-request
	// have no server yet to name.
	switch t {
	case wire.MsgRequest6, wire.MsgRenew, wire.MsgRelease6, wire.MsgDecline6:
		if len(m.server) == 0 {
			return nil, fmt.Errorf("a %s requires a Server Identifier option and this machine has no server DUID", t)
		}
		msg.Options = append(msg.Options, wire.OptionV6{
			Code: wire.OptV6ServerID,
			Data: append([]byte(nil), m.server...),
		})
	}

	if ia, ok, err := m.buildIA(t); err != nil {
		return nil, err
	} else if ok {
		msg.Options = append(msg.Options, ia)
	}

	if codes := m.params.oro(t); len(codes) > 0 {
		v := make([]byte, 0, 2*len(codes))
		for _, c := range codes {
			v = append(v, byte(uint16(c)>>8), byte(uint16(c)))
		}
		msg.Options = append(msg.Options, wire.OptionV6{Code: wire.OptV6ORO, Data: v})
	}

	// §21.9's Elapsed Time is LAST because it is the value most sensitive to
	// where it is computed, and putting it at the end of the builder keeps the
	// computation next to the send. Order carries no meaning on the wire —
	// §21.1's options "may appear in any order" — so this is a readability
	// choice and nothing depends on it.
	el := wire.ElapsedHundredths(uint64(nonNegative(now.Sub(m.exchangeStart)) / (10 * Millisecond)))
	msg.Options = append(msg.Options, wire.OptionV6{
		Code: wire.OptV6ElapsedTime,
		Data: []byte{byte(el >> 8), byte(el)},
	})
	return msg, nil
}

// buildIA renders the IA_NA this message carries, if it carries one.
func (m *Machine6) buildIA(t wire.MessageTypeV6) (wire.OptionV6, bool, error) {
	var addrs []Addr6
	zeroLifetimes := false
	switch t {
	case wire.MsgSolicit:
		// §18.2.1: "The client MAY include addresses in IA Address options
		// (see Section 21.6) encapsulated within IA_NA option as hints to the
		// server about the addresses for which the client has a preference."
		if h := m.params.hintAddr(); h.IsValid() {
			addrs = []Addr6{{Addr: h}}
		}
		zeroLifetimes = true
	case wire.MsgRequest6:
		addrs = m.pending.Addrs
	case wire.MsgConfirm:
		if m.resume != nil {
			addrs = m.resume.Addrs
		}
		// §18.2.3: "The client SHOULD set the T1 and T2 fields in any IA_NA
		// options (see Section 21.4) and the preferred-lifetime and
		// valid-lifetime fields in the IA Address options (see Section 21.6)
		// to 0, as the server will ignore these fields."
		zeroLifetimes = true
	case wire.MsgRenew, wire.MsgRebind:
		addrs = m.lease.Addrs
	case wire.MsgRelease6:
		addrs = m.releasing
	case wire.MsgDecline6:
		addrs = make([]Addr6, 0, len(m.declining))
		for _, a := range m.declining {
			addrs = append(addrs, Addr6{Addr: a})
		}
		zeroLifetimes = true
	default:
		return wire.OptionV6{}, false, nil
	}

	ia := &wire.IANA{IAID: m.params.IAID}
	for _, a := range addrs {
		if !a.Addr.Is6() || a.Addr.Is4In6() || a.Addr.IsUnspecified() {
			continue
		}
		e := &wire.IAAddr{Addr: a.Addr}
		if !zeroLifetimes {
			e.PreferredLifetime = durationToSeconds(a.Preferred)
			e.ValidLifetime = durationToSeconds(a.Valid)
		}
		v, err := wire.EncodeIAAddr(e)
		if err != nil {
			return wire.OptionV6{}, false, err
		}
		ia.Options = append(ia.Options, wire.OptionV6{Code: wire.OptV6IAAddr, Data: v})
	}
	v, err := wire.EncodeIANA(ia)
	if err != nil {
		return wire.OptionV6{}, false, err
	}
	return wire.OptionV6{Code: wire.OptV6IANA, Data: v}, true, nil
}

// durationToSeconds is the inverse of SecondsToDuration for a lifetime the
// client sends back. §7.7's infinity survives the round trip.
func durationToSeconds(d Duration) uint32 {
	if d.IsInfinite() {
		return InfiniteSeconds
	}
	if d <= 0 {
		return 0
	}
	s := d.Seconds()
	if s > int64(InfiniteSeconds-1) {
		return InfiniteSeconds - 1
	}
	return uint32(s)
}

// randomDelay draws §18.2.1's, §18.2.3's and §18.2.6's pre-transmission delay:
// "a random amount of time between 0 and SOL_MAX_DELAY".
//
// It is uniform over [0, max] inclusive at both ends and takes its entropy from
// the one rnd Step was handed, so a replay reproduces it exactly.
func randomDelay(max Duration, rnd uint64) Duration {
	if max <= 0 {
		return 0
	}
	return Duration(rnd % uint64(max+1))
}
