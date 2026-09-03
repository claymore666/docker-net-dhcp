package proto

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// Machine is the DHCPv4 client state machine. It is pure.
//
// The whole surface is Step: no clock, no scheduler, no goroutine, no I/O,
// enforced by the T1 gate rather than by this comment. Determinism is
// therefore a property of the type — the same Machine fed the same (now, rnd,
// event) sequence produces the same actions — which is what makes Replay exact
// and the acquisition path testable with no root and no network.
type Machine struct {
	params Params

	state State

	// nextAction is the ActionID counter. It is machine state rather than a
	// package global so two machines in one process do not interleave ids and
	// so a replay reproduces the ids exactly.
	nextAction ActionID

	// startedAt is the Instant of the Start that began the current
	// acquisition. It is what the 'secs' field counts from (RFC 2131 section
	// 2: "seconds elapsed since client began address acquisition or renewal
	// process").
	startedAt Instant
	started   bool

	// xid is the transaction id of the transaction in flight. A new one is
	// drawn for each DISCOVER cycle and REUSED for the REQUEST, because RFC
	// 2131 section 4.4.1's table says the REQUEST carries "'xid' from server
	// DHCPOFFER message" — which is the DISCOVER's xid echoed back.
	xid uint32

	// retransmits counts retransmissions of the message in flight. It is NOT
	// incremented by a send that failed locally: a message that never left the
	// host is not a retransmission, and counting it burns the budget for an
	// event the server never saw.
	retransmits int

	// sendFailures counts consecutive failed sends. Reset by any successful
	// transition that sends.
	sendFailures int

	// offer is the OFFER being requested, held so REQUEST can carry its
	// yiaddr and server identifier.
	offer *wire.Message

	// requestSentAt is the Instant the REQUEST in flight was first sent. RFC
	// 2131 section 4.4.5 computes the lease expiry from the time the REQUEST
	// was SENT, not from the ACK's arrival.
	//
	// It is set on the FIRST send of a REQUEST and re-set on each
	// retransmission, because a retransmitted REQUEST is the one the server
	// answered. Holding the first send's time would over-state the lease by
	// the whole retransmission interval.
	requestSentAt Instant

	// lease is the lease held in BOUND.
	lease   Lease
	haveLse bool

	// resume is the remembered lease this machine has NOT yet used, and it is
	// consumed by the first acquisition it starts. See takeResume: every exit
	// from REBOOTING other than BOUND restarts the configuration process, and
	// RFC 2131 section 3.2(3) says that restart uses "the (non-abbreviated)
	// procedure described in section 3.1" — so there is no second INIT-REBOOT
	// on one Resume, and nil here is what makes that true rather than a rule
	// each exit has to remember.
	resume *Resume

	// rebootAddr is the address the INIT-REBOOT DHCPREQUEST in flight names.
	// Held apart from resume because the retransmissions need it after the
	// Resume is gone, and because it is what Action.Requested reports.
	rebootAddr netip.Addr
}

// New builds a Machine in StateStopped.
func New(p Params) (*Machine, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	p.CHAddr = append([]byte(nil), p.CHAddr...)
	p.ClientID = append([]byte(nil), p.ClientID...)
	p.ParameterList = append([]wire.OptionCode(nil), p.parameterList()...)
	if p.FQDN.Name != "" {
		// validate() has already run this and refused a name or flag
		// combination that cannot be encoded, so the error here is
		// unreachable. Encoding ONCE, here, is what keeps base() free of an
		// error path it could only swallow: a base() that dropped option 81
		// on an encoding failure would send a conformant-looking message
		// missing the one option the caller asked for, and say nothing.
		v, err := wire.EncodeFQDN(p.FQDN.flags(), p.FQDN.Name)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadFQDN, err)
		}
		p.fqdn = v
	}
	// The Resume pointer is cloned, not shared. A caller holding the value it
	// passed in could otherwise move the remembered address out from under a
	// machine that has already decided to ask for it.
	p.Resume = p.Resume.Clone()
	return &Machine{params: p, state: StateStopped, resume: p.Resume}, nil
}

// State returns the current state.
func (m *Machine) State() State { return m.state }

// Lease returns the held lease, if any.
func (m *Machine) Lease() (Lease, bool) { return m.lease, m.haveLse }

// Params returns the machine's configuration.
//
// The Resume is cloned on the way out for the reason New clones it on the way
// in: it is the one pointer in Params, and a caller that could reach through it
// could change what this machine is going to ask for.
func (m *Machine) Params() Params {
	p := m.params
	p.Resume = p.Resume.Clone()
	return p
}

// Step is total: every (state, event) pair yields a defined result and no
// reachable panic. R1 tests that over the whole product of AllStates and
// AllEventKinds rather than sampling it.
//
// now and rnd are parameters, not ambients: rnd is journalled beside the event
// so a replay is bit-exact, where a PRNG inside the machine would make replay
// depend on a persisted seed AND a call count.
func (m *Machine) Step(now Instant, rnd uint64, ev Event) (State, []Action) {
	var out actions
	switch m.state {
	case StateStopped:
		m.stepStopped(now, rnd, ev, &out)
	case StateInit:
		m.stepInit(now, rnd, ev, &out)
	case StateSelecting:
		m.stepSelecting(now, rnd, ev, &out)
	case StateRequesting:
		m.stepRequesting(now, rnd, ev, &out)
	case StateBound:
		m.stepBound(now, rnd, ev, &out)
	case StateRenewing, StateRebinding:
		m.stepRenewal(now, rnd, ev, &out)
	case StateRebooting:
		m.stepRebooting(now, rnd, ev, &out)
	default:
		// Unreachable through the exported API — State is not settable from
		// outside — and handled anyway, because "unreachable" is a claim about
		// today's code and a panic here takes the plugin down with it.
		out.journal(m, fmt.Sprintf("event %s in unknown state %s: ignored", ev.Kind, m.state))
	}
	return m.state, out.list
}

// ---------------------------------------------------------------- states --

func (m *Machine) stepStopped(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStart:
		m.beginAcquisition(now, rnd, out, true)
	case EvStop:
		out.journal(m, "already stopped")
	case EvActionFailed:
		// The action that fails here is the DHCPRELEASE: release() sends it
		// and halts in the same step, so its failure arrives after the machine
		// has already stopped. Journalled by name rather than folded into the
		// default's "ignored", because RFC 2131 section 4.4.6 means the lease
		// is given up whether or not the message left the host — so the ONLY
		// place the server keeping a binding this client believes it released
		// can be read is this line.
		out.journal(m, fmt.Sprintf("%s failed (%s) after the machine stopped: the lease is given up locally, and the server may still hold the binding (RFC 2131 4.4.6)",
			ev.Action, ev.Reason))
	default:
		out.journal(m, fmt.Sprintf("%s ignored in STOPPED", ev.Kind))
	}
}

func (m *Machine) stepInit(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStart:
		// Idempotent: a second Start while INIT restarts the desync wait
		// rather than stacking a second acquisition. Two DISCOVER cycles with
		// two xids on one interface is the shape that produced two server
		// bindings in the v1.x IPv6 path.
		m.beginAcquisition(now, rnd, out, true)
	case EvStop:
		m.stop(out)
	case EvRelease:
		m.releaseBeforeBound(out)
	case EvTimerFired:
		switch ev.Timer {
		case TimerDesync:
			m.sendDiscover(now, rnd, out)

		case TimerRestart:
			// RFC 2131 section 3.1(5): the client "restarts the configuration
			// process", which begins at INIT with a transaction of its own —
			// section 4.4.1, "The client generates and records a random
			// transaction identifier". So this is a fresh acquisition, not a
			// DHCPDISCOVER on the declined transaction's xid and elapsed time.
			// TestRestartFiresAFreshTransaction drives the difference.
			//
			// withDesync is false: section 4.4.1's one-to-ten-second draw
			// desynchronises hosts booting together, and the wait that has
			// just elapsed is section 3.1(5)'s own. Both would be two waits
			// for two different reasons on one restart.
			m.beginAcquisition(now, rnd, out, false)

		default:
			out.journal(m, fmt.Sprintf("timer %s fired in INIT: ignored", ev.Timer))
		}
	case EvLinkUp:
		// The link came back after a LinkDown dropped us here. Start again.
		m.beginAcquisition(now, rnd, out, true)
	case EvActionFailed:
		m.noteActionFailed(now, rnd, ev, out)
	default:
		out.journal(m, fmt.Sprintf("%s ignored in INIT", ev.Kind))
	}
}

func (m *Machine) stepSelecting(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStop:
		m.stop(out)
	case EvRelease:
		m.releaseBeforeBound(out)
	case EvReceived:
		msg, ok := m.acceptable(ev.Msg, out)
		if !ok {
			return
		}
		t, _ := msg.Type()
		switch t {
		case wire.MsgOffer:
			sid, hasSID := msg.Addr4(wire.OptServerID)
			if !msg.YIAddr.Is4() || msg.YIAddr.IsUnspecified() || !hasSID || sid.IsUnspecified() {
				// RFC 2131 section 4.4.1 has the client extract the server
				// address from the OFFER's server-identifier option, and the
				// REQUEST that follows MUST carry it. An OFFER without one, or
				// without a yiaddr, cannot produce a conformant REQUEST, so it
				// is discarded rather than half-used.
				out.journal(m, "DHCPOFFER without a usable yiaddr and server identifier: discarded")
				return
			}
			m.offer = msg.Clone()
			m.retransmits = 0
			m.sendRequest(now, rnd, out)
		case wire.MsgAck, wire.MsgNak:
			// RFC 2131 section 4.4.1: "Any arriving DHCPACK messages must be
			// silently discarded." Silent to the wire, not to the operator.
			out.journal(m, fmt.Sprintf("%s in SELECTING: discarded", t))
		default:
			out.journal(m, fmt.Sprintf("%s in SELECTING: discarded", t))
		}
	case EvTimerFired:
		if ev.Timer != TimerRetransmit {
			out.journal(m, fmt.Sprintf("timer %s fired in SELECTING: ignored", ev.Timer))
			return
		}
		if m.params.Discover.Exhausted(m.retransmits) {
			// RFC 2131 section 3.1(5): after the retransmission algorithm is
			// exhausted "the client reverts to INIT state and restarts the
			// initialization process. The client SHOULD notify the user that
			// the initialization process has failed and is restarting."
			// ActFailed is that notification, typed so U5 can branch on it.
			out.failed(m, ReasonNoServer, "no DHCPOFFER after the retransmission budget; restarting")
			m.beginAcquisition(now, split(rnd, 1), out, false)
			return
		}
		m.retransmits++
		m.sendDiscover(now, rnd, out)
	case EvLinkDown:
		m.linkDown(out)
	case EvActionFailed:
		m.noteActionFailed(now, rnd, ev, out)
	default:
		out.journal(m, fmt.Sprintf("%s ignored in SELECTING", ev.Kind))
	}
}

func (m *Machine) stepRequesting(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStop:
		m.stop(out)
	case EvRelease:
		m.releaseBeforeBound(out)
	case EvReceived:
		msg, ok := m.acceptable(ev.Msg, out)
		if !ok {
			return
		}
		t, _ := msg.Type()
		switch t {
		case wire.MsgAck:
			lse, note, ok := leaseFromAck(msg, m.requestSentAt)
			if !ok {
				// An ACK with no yiaddr or no lease time cannot be applied.
				// The retransmission timer is still armed, so this is not a
				// dead end: the machine keeps asking.
				out.journal(m, "DHCPACK without a usable yiaddr and lease time: discarded")
				return
			}
			if note != "" {
				out.journal(m, note)
			}
			m.enterBound(now, lse, out, false)
		case wire.MsgNak:
			// RFC 2131 section 3.1(5): "If the client receives a DHCPNAK
			// message, the client restarts the configuration process."
			out.cancel(m, TimerRetransmit)
			out.failed(m, ReasonNak, m.nakText(msg))
			m.beginAcquisition(now, split(rnd, 1), out, false)
		case wire.MsgOffer:
			// A second server's OFFER arriving late. We have already selected.
			out.journal(m, "DHCPOFFER in REQUESTING: discarded")
		default:
			out.journal(m, fmt.Sprintf("%s in REQUESTING: discarded", t))
		}
	case EvTimerFired:
		if ev.Timer != TimerRetransmit {
			out.journal(m, fmt.Sprintf("timer %s fired in REQUESTING: ignored", ev.Timer))
			return
		}
		if m.params.Request.Exhausted(m.retransmits) {
			out.failed(m, ReasonNoServer, "no DHCPACK after the retransmission budget; restarting")
			m.beginAcquisition(now, split(rnd, 1), out, false)
			return
		}
		m.retransmits++
		m.sendRequest(now, rnd, out)
	case EvLinkDown:
		m.linkDown(out)
	case EvActionFailed:
		m.noteActionFailed(now, rnd, ev, out)
	default:
		out.journal(m, fmt.Sprintf("%s ignored in REQUESTING", ev.Kind))
	}
}

// stepRebooting is RFC 2131 Figure 5's REBOOTING: the INIT-REBOOT DHCPREQUEST
// is in flight, no lease is held, and three things can end it.
//
// It is NOT stepRequesting with a different message, and the difference that
// forbids sharing them is the DHCPOFFER arm. In REQUESTING an OFFER is a second
// server answering a DHCPDISCOVER this client already acted on; here no
// DHCPDISCOVER was ever sent, so an OFFER is a server answering the
// INIT-REBOOT request as though it were one — Figure 5 draws that edge
// explicitly as "DHCPOFFER/Discard" and it is worth its own journal line,
// because a link where it happens is a link where some server is treating this
// message as a SELECTING one, which is exactly what section 4.3.2 warns the
// server-identifier option causes.
func (m *Machine) stepRebooting(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStop:
		m.stop(out)
	case EvStart:
		out.journal(m, "Start in REBOOTING: already running")
	case EvRelease:
		// No lease is held here: section 4.4.2's client is "seeking to verify
		// a previously allocated, cached configuration" and has not had it
		// confirmed. releaseBeforeBound says why nothing is sent.
		m.releaseBeforeBound(out)
	case EvReceived:
		msg, ok := m.acceptable(ev.Msg, out)
		if !ok {
			return
		}
		t, _ := msg.Type()
		switch t {
		case wire.MsgAck:
			lse, note, ok := leaseFromAck(msg, m.requestSentAt)
			if !ok {
				// The retransmission timer is still armed, so the machine
				// keeps asking.
				out.journal(m, "DHCPACK without a usable yiaddr and lease time: discarded")
				return
			}
			if note != "" {
				out.journal(m, note)
			}
			// SECTION 4.4.2 ACCEPTS THIS ACK FROM ANY SERVER AND FOR ANY
			// ADDRESS. "Once a DHCPACK message with an 'xid' field matching
			// that in the client's DHCPREQUEST message arrives from any
			// server, the client is initialized and moves to BOUND state" —
			// there is no clause about the address matching the one asked for,
			// and no clause about the server being the one that issued the
			// remembered lease. Both are reported rather than enforced:
			// Action.Requested carries what was asked for, and the lease's
			// ServerID is read out of this ACK.
			m.enterBound(now, lse, out, false)
		case wire.MsgNak:
			// Figure 5's "DHCPNAK/Restart" edge, and section 3.2(3): "If the
			// client receives a DHCPNAK message, it cannot reuse its
			// remembered network address. It must instead request a new
			// address by restarting the configuration process, this time using
			// the (non-abbreviated) procedure described in section 3.1."
			//
			// Non-abbreviated is the load-bearing word: the restart below is a
			// DHCPDISCOVER and not another INIT-REBOOT, and it is so because
			// takeResume has already consumed the Resume.
			out.cancel(m, TimerRetransmit)
			out.failed(m, ReasonNak, m.nakText(msg))
			m.beginAcquisition(now, split(rnd, 1), out, false)
		case wire.MsgOffer:
			out.journal(m, "DHCPOFFER in REBOOTING: discarded (RFC 2131 Figure 5)")
		default:
			out.journal(m, fmt.Sprintf("%s in REBOOTING: discarded", t))
		}
	case EvTimerFired:
		if ev.Timer != TimerRetransmit {
			out.journal(m, fmt.Sprintf("timer %s fired in REBOOTING: ignored", ev.Timer))
			return
		}
		if m.params.Request.Exhausted(m.retransmits) {
			// SECTION 3.2(3) OFFERS A CHOICE HERE AND THIS IS THE OTHER ONE.
			// "If the client receives neither a DHCPACK or a DHCPNAK message
			// after employing the retransmission algorithm, the client MAY
			// choose to use the previously allocated network address and
			// configuration parameters for the remainder of the unexpired
			// lease."
			//
			// DECISION 2026-09-03: the MAY is not taken. Silence is what
			// section 4.3.2 requires of a server that "has no record of this
			// client" — the case where the address has been reissued to
			// somebody else is indistinguishable, from here, from the case
			// where the server is merely down. Taking the MAY puts a host on
			// the network with an address nothing has confirmed in this boot,
			// and this library has no conflict probe until M6. The cost of not
			// taking it is an outage where a client could have carried on
			// using an address it still held; the cost of taking it is two
			// hosts on one address, which is the failure this plugin exists to
			// avoid.
			out.failed(m, ReasonNoServer, "no answer to the INIT-REBOOT DHCPREQUEST after the retransmission budget; acquiring from INIT")
			m.beginAcquisition(now, split(rnd, 1), out, false)
			return
		}
		m.retransmits++
		m.sendReboot(now, rnd, out)
	case EvLinkDown:
		m.linkDown(out)
	case EvActionFailed:
		m.noteActionFailed(now, rnd, ev, out)
	default:
		out.journal(m, fmt.Sprintf("%s ignored in REBOOTING", ev.Kind))
	}
}

func (m *Machine) stepBound(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStop:
		m.stop(out)
	case EvTimerFired:
		switch ev.Timer {
		case TimerExpire:
			// RFC 2131 section 4.4.5: "If the lease expires ... the client moves
			// to INIT state, MUST immediately stop any other network processing
			// and requests network initialization parameters as if the client
			// were uninitialized." LeaseLost is how the caller is told to stop.
			m.dropLease(out, ReasonExpired)
			// No desync wait here. Section 4.4.1's one-to-ten-second delay is
			// about desynchronising hosts BOOTING together; a lease that has just
			// expired needs re-acquiring now, and adding the delay would extend
			// every outage by up to ten seconds for no benefit.
			m.beginAcquisition(now, rnd, out, false)
		case TimerRenew:
			m.enterRenewing(now, rnd, out)
		case TimerRebind:
			// T2 reached straight from BOUND. This is the ordinary path when
			// T1 could not be used — a lease with no server identifier — and
			// the safety net when the renewal timer never fired at all.
			m.enterRebinding(now, rnd, out, true)
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in BOUND: ignored", ev.Timer))
		}
	case EvLinkDown:
		m.dropLease(out, ReasonLinkDown)
		m.toInitIdle(out)
	case EvAddressLost:
		m.dropLease(out, ReasonAddressLost)
		m.beginAcquisition(now, rnd, out, false)
	case EvConflictDetected:
		m.declineAndRestart(rnd, out)
	case EvRelease:
		m.release(rnd, out)
	case EvStart:
		out.journal(m, "Start in BOUND: already running")
	case EvReceived:
		// BOUND has no transaction open: the renewal transaction is opened at
		// T1, in RENEWING. Anything arriving here is unsolicited — including
		// a DHCPACK for the transaction that produced this lease, which the
		// server may retransmit and which must not restart the timers.
		out.journal(m, "message in BOUND with no transaction open: discarded")
	case EvActionFailed:
		m.noteActionFailed(now, rnd, ev, out)
	default:
		out.journal(m, fmt.Sprintf("%s ignored in BOUND", ev.Kind))
	}
}

// stepRenewal is RENEWING and REBINDING, both, in one function.
//
// The two states differ in exactly three places — where the DHCPREQUEST goes,
// which timer promotes the machine, and what the retransmission delay is
// measured against — and RFC 2131 section 4.4.5 requires everything else of
// them to be identical: the same DHCPACK ends both, the same DHCPNAK halts
// both, the same expiry loses the address in both. Two functions drift on the
// parts that must not; this one cannot, and a mutant that changes an arm
// changes it for both states at once, where a test on either kills it.
func (m *Machine) stepRenewal(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStop:
		m.stop(out)
	case EvStart:
		out.journal(m, fmt.Sprintf("Start in %s: already running", m.state))
	case EvRelease:
		// A lease IS held here, so this is section 4.4.6's real DHCPRELEASE
		// and not releaseBeforeBound's silent halt. The renewal in flight is
		// abandoned; release halts, which cancels every timer.
		m.release(rnd, out)
	case EvConflictDetected:
		m.declineAndRestart(rnd, out)
	case EvReceived:
		msg, ok := m.acceptable(ev.Msg, out)
		if !ok {
			return
		}
		t, _ := msg.Type()
		switch t {
		case wire.MsgAck:
			lse, note, ok := leaseFromAck(msg, m.requestSentAt)
			if !ok {
				// The retransmission timer is still armed and so are T2 and
				// the expiry, so this is not a dead end: the machine keeps
				// asking on the lease it still holds.
				out.journal(m, "DHCPACK without a usable yiaddr and lease time: discarded")
				return
			}
			if note != "" {
				out.journal(m, note)
			}
			m.enterBound(now, lse, out, true)
		case wire.MsgNak:
			// RFC 2131 Figure 5: the edges leaving RENEWING and REBINDING on
			// a DHCPNAK are both labelled "DHCPNAK / Halt network" and both
			// land in INIT. Section 4.4.5's prose says only that the client
			// "restarts the ... process"; Figure 5 is where GIVING THE
			// ADDRESS UP FIRST is written, and it is the difference that
			// matters — a NAK during renewal means the server no longer
			// holds this binding, so continuing to use the address puts a
			// host on the network with an address somebody else may have.
			//
			// The loss is announced before the new acquisition begins, for
			// the reason enterBound gives about ordering: a caller draining
			// this list tears the interface down when it sees ActLeaseLost.
			m.dropLease(out, ReasonNak)
			out.failed(m, ReasonNak, m.nakText(msg))
			m.beginAcquisition(now, split(rnd, 1), out, false)
		default:
			// A DHCPOFFER, most likely: some server answering the broadcast
			// REBINDING request as though it were a DISCOVER.
			out.journal(m, fmt.Sprintf("%s in %s: discarded", t, m.state))
		}
	case EvTimerFired:
		switch ev.Timer {
		case TimerRetransmit:
			m.sendRenewal(now, out)
		case TimerRebind:
			if m.state != StateRenewing {
				out.journal(m, "timer rebind fired in REBINDING: ignored")
				return
			}
			// Section 4.4.5: "If no DHCPACK arrives before time T2, the
			// client moves to REBINDING state". The transaction carries over
			// rather than restarting; enterRebinding says why.
			m.enterRebinding(now, rnd, out, false)
		case TimerExpire:
			// Section 4.4.5: "If the lease expires before the client receives
			// a DHCPACK, the client moves to INIT state, MUST immediately
			// stop any other network processing and requests network
			// initialization parameters as if the client were uninitialized."
			m.dropLease(out, ReasonExpired)
			m.beginAcquisition(now, rnd, out, false)
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in %s: ignored", ev.Timer, m.state))
		}
	case EvLinkDown:
		m.dropLease(out, ReasonLinkDown)
		m.toInitIdle(out)
	case EvAddressLost:
		m.dropLease(out, ReasonAddressLost)
		m.beginAcquisition(now, rnd, out, false)
	case EvActionFailed:
		m.noteActionFailed(now, rnd, ev, out)
	default:
		out.journal(m, fmt.Sprintf("%s ignored in %s", ev.Kind, m.state))
	}
}

// ----------------------------------------------------------- transitions --

// beginAcquisition starts a fresh DISCOVER cycle: new xid, counters reset.
//
// withDesync applies RFC 2131 section 4.4.1's startup delay. It is false on the
// paths that are already reacting to something (an expiry, a NAK, a budget
// exhaustion) because there is nothing to desynchronise from there.
func (m *Machine) beginAcquisition(now Instant, rnd uint64, out *actions, withDesync bool) {
	m.startedAt = now
	m.started = true
	m.retransmits = 0
	m.sendFailures = 0
	m.offer = nil
	m.rebootAddr = netip.Addr{}
	m.xid = uint32(split(rnd, 0))
	m.state = StateInit
	out.cancelAll(m)

	// RFC 2131 section 3.2's abbreviated exchange, when there is a remembered
	// lease to abbreviate it with. It is tried HERE, in the one function every
	// path into INIT goes through, rather than at EvStart: the alternative was
	// a check at the Start arm of stepStopped, which would have left every
	// other entry to INIT — an expiry, a NAK, a link coming back — silently
	// unable to use a remembered lease, and the difference between those
	// entries is not the remembered lease's business. What decides is
	// takeResume, once.
	if m.takeResume(now, out) {
		m.sendReboot(now, split(rnd, 3), out)
		return
	}

	d := Duration(0)
	if withDesync {
		d = m.params.desync(split(rnd, 2))
	}
	if d <= 0 {
		m.sendDiscover(now, split(rnd, 3), out)
		return
	}
	out.set(m, TimerDesync, d)
	out.journal(m, fmt.Sprintf("INIT: waiting %s to desynchronise (RFC 2131 4.4.1)", d))
}

// takeResume consumes the remembered lease and reports whether this
// acquisition is RFC 2131 section 3.2's abbreviated one.
//
// IT CONSUMES WHICHEVER WAY IT ANSWERS, and that is the whole of the "one
// INIT-REBOOT per Resume" rule. The three ways out of REBOOTING that are not
// BOUND all end in beginAcquisition: a DHCPNAK, an exhausted retransmission
// budget, and a link that dropped and came back. Section 3.2(3) makes the first
// two "the (non-abbreviated) procedure", and the third is the only one where
// asking again would be defensible — distinguishing it would cost a second
// lifetime rule in ring 1 for a case a caller can drive itself by constructing
// a client with the same Resume. So: one attempt, and after it this machine
// acquires the ordinary way.
//
// The expired arm is not a silent fallback. Section 4.3.2: a server "MUST
// remain silent" for a DHCPREQUEST from a client of which it "has no record",
// and an expired lease is precisely the case where no server has a record — so
// rebooting one buys a full retransmission budget of silence and then the
// DHCPDISCOVER that should have gone first. The journal line is the only place
// that is visible from outside, because the wire shows nothing.
func (m *Machine) takeResume(now Instant, out *actions) bool {
	r := m.resume
	if r == nil {
		return false
	}
	// Consumed whichever way this answers, so that every non-BOUND exit from
	// REBOOTING is automatically RFC 2131 section 3.2(3)'s "(non-abbreviated)
	// procedure" without each exit having to remember the rule.
	m.resume = nil
	if !r.live(now) {
		out.journal(m, "the remembered lease on "+r.Addr.String()+
			" has expired: acquiring from INIT, because a server with no record of a client must stay silent (RFC 2131 4.3.2)")
		return false
	}
	m.rebootAddr = r.Addr
	out.journal(m, "INIT-REBOOT: asking to keep "+r.Addr.String()+" (RFC 2131 4.4.2)")
	return true
}

// sendReboot builds and sends the DHCPREQUEST of INIT-REBOOT.
//
// RFC 2131 section 4.3.2, the server's reading of it: "'server identifier' MUST
// NOT be filled in, 'requested IP address' option MUST be filled in with
// client's notion of its previously assigned address. 'ciaddr' MUST be zero."
// Table 4 (section 4.3.6) says the same three cells and adds that the message
// is broadcast; Table 5 (section 4.4.1) makes option 50 a MUST "in SELECTING or
// INIT-REBOOT" and option 54 a MUST NOT "after INIT-REBOOT".
//
// IT IS A SEPARATE BUILDER FROM sendRequest AND FROM sendRenewal, and the
// separation is the guard rather than a style. sendRequest fills option 54 from
// the offer it holds and takes option 50 from that offer's yiaddr; sendRenewal
// fills 'ciaddr' and neither option. Reusing either one here would produce a
// message this state must not send, and section 4.3.2 says what a server does
// with the wrong one: a DHCPREQUEST carrying a server identifier is read as one
// "generated during SELECTING state", checked against the server's own
// identifier, and answered with SILENCE when it does not match. Silence is also
// what a message that never left the host produces, so the defect would be a
// timeout that looks like a slow server. Three builders that cannot reach each
// other's fields is a compile-time separation;
// TestTheInitRebootRequestCarriesOnlyWhatSection432Allows is the run-time one.
func (m *Machine) sendReboot(now Instant, rnd uint64, out *actions) {
	msg := m.base(now, wire.MsgRequest)
	v := m.rebootAddr.As4()
	msg.Options[wire.OptRequestedIP] = v[:]
	m.state = StateRebooting
	// Section 4.4.2: "The client records its own local time for later use in
	// computing the lease expiration", and section 4.4.5's arithmetic runs from
	// the moment the DHCPREQUEST was SENT. Re-set on every attempt, because the
	// retransmission the server answered is the one that counts.
	m.requestSentAt = now
	out.send(m, msg, Dest{Broadcast: true})
	out.set(m, TimerRetransmit, m.params.Request.Delay(m.retransmits, rnd))
}

// requestedAddr is the address this machine ASKED FOR in option 50, or the zero
// Addr when it asked for none. See Action.Requested.
func (m *Machine) requestedAddr() netip.Addr {
	if m.rebootAddr.IsValid() && !m.rebootAddr.IsUnspecified() {
		return m.rebootAddr
	}
	return m.params.RequestedIP
}

// toInitIdle parks in INIT with nothing armed, waiting for a LinkUp or Start.
func (m *Machine) toInitIdle(out *actions) {
	m.state = StateInit
	m.offer = nil
	m.retransmits = 0
	out.cancelAll(m)
}

func (m *Machine) linkDown(out *actions) {
	out.journal(m, "link down during acquisition: parked in INIT")
	m.toInitIdle(out)
}

func (m *Machine) stop(out *actions) { m.halt(out, ReasonStopped) }

// halt gives up whatever is held, cancels everything and parks in STOPPED.
func (m *Machine) halt(out *actions, r Reason) {
	m.dropLease(out, r)
	out.cancelAll(m)
	m.state = StateStopped
	m.started = false
	m.offer = nil
	m.retransmits = 0
	m.sendFailures = 0
}

// declineAndRestart is RFC 2131 section 3.1(5): send a DHCPDECLINE, give up
// the address, and wait before restarting the configuration process.
//
// THE DECLINE IS SENT BEFORE THE LOSS IS ANNOUNCED, for the reason enterBound
// gives about the reverse order: a ring-3 caller drains this list in order and
// may tear the interface down the moment it sees ActLeaseLost.
// TestDeclineAndReleaseAreSentBeforeTheLossIsAnnounced holds the order.
//
// The machine lands in INIT, not STOPPED: RFC 2131 section 3.2(3), "This
// action corresponds to the client moving to the INIT state in the DHCP state
// diagram."
func (m *Machine) declineAndRestart(rnd uint64, out *actions) {
	m.sendDecline(rnd, out)
	m.dropLease(out, ReasonConflict)
	m.toInitIdle(out)
	d := m.params.restartDelay()
	out.set(m, TimerRestart, d)
	out.journal(m, fmt.Sprintf("INIT: waiting %s before restarting after DHCPDECLINE (RFC 2131 3.1)", d))
}

// release is RFC 2131 section 4.4.6: the caller no longer requires the address.
//
// Sent before the loss is announced, and here the ordering is not only about
// what the caller might do: a DHCPRELEASE is unicast FROM the released address
// (section 4.4.4), so a caller that removed the address on ActLeaseLost would
// leave the message with no source.
// TestDeclineAndReleaseAreSentBeforeTheLossIsAnnounced holds the order.
//
// DECISION 2026-08-30, because RFC 2131 does not settle it: the machine lands
// in STOPPED. Figure 5 has no DHCPRELEASE edge and section 4.4.6 names no
// state. INIT was the alternative and it is wrong for this library — a
// released machine in INIT re-acquires on the next EvLinkUp, silently undoing
// what the caller asked for. STOPPED is reversible with EvStart, which is the
// caller saying so a second time.
func (m *Machine) release(rnd uint64, out *actions) {
	m.sendRelease(rnd, out)
	m.halt(out, ReasonReleased)
}

// releaseBeforeBound is EvRelease arriving in INIT, SELECTING or REQUESTING,
// where no lease exists yet.
//
// It halts with ReasonReleased and sends NOTHING. There is nothing to
// relinquish: RFC 2131 section 4.4.6 is about "its assigned network address"
// and section 3.1(6) about "its lease on a network address", and this client
// has neither. dropLease is guarded on a lease being held, so no ActLeaseLost
// is stamped and the caller is not told it lost what it never had.
//
// Continuing was the alternative and it is the defect this replaces. The
// caller had said it no longer wants an address; the machine went on to take
// one anyway, and the only thing that could have released it had already been
// told to stop.
//
// A LATE DHCPACK IS DISCARDED, NOT DECLINED OR RELEASED. Releasing from
// REQUESTING leaves a DHCPREQUEST on the wire that a server may still answer,
// binding an address this client will never use. Nothing here cleans that up,
// and the RFC asks for nothing: section 4.4.1 says "Any arriving DHCPACK
// messages must be silently discarded", which STOPPED does. DHCPDECLINE is
// the answer to an address that "appears to be in use" (sections 3.1(5),
// 4.4.4) — a different fact from an address not wanted. A DHCPRELEASE cannot
// even be formed: section 4.4.4 unicasts it from the released address, and at
// the halt no ACK has arrived, so there is no address to send from. The
// binding lapses at its expiry, which section 4.4.6 anticipates in saying
// "the correct operation of DHCP does not depend on the transmission of
// DHCPRELEASE messages".
func (m *Machine) releaseBeforeBound(out *actions) {
	out.journal(m, "release with no lease held: stopping, nothing sent (RFC 2131 4.4.6)")
	m.halt(out, ReasonReleased)
}

// enterBound installs the lease and arms the expiry timer.
//
// Armed for exp-now, NOT for the lease duration: the lease clock starts when
// the REQUEST was sent (RFC 2131 section 4.4.5), so by the time the ACK is in
// hand some of it is spent. Arming for the full duration holds the address
// past its expiry by the round-trip time — invisible on a fixture, real on a
// slow or retransmitting link.
//
// THE EXPIRY TIMER IS ARMED BEFORE THE ACQUISITION IS ANNOUNCED, and the order
// is the point of TestBoundArmsExpiryBeforeAnnouncing. A ring-3 caller executes
// this action list in order and may act on ActLeaseAcquired as soon as it sees
// it — configure the interface, hand the address out — so any action after the
// announcement is one the caller can run ahead of.
//
//	STANDARD: RFC 2131 section 4.4.5 requires the client to stop using the
//	address when the lease expires, and measures the lease from the moment the
//	DHCPREQUEST was sent, not from the ACK. So the bound on use is already
//	running when the ACK lands.
//	INFERRED: the RFC describes one sequential client and does not order these
//	two steps as such. What makes the order load-bearing here is this machine's
//	own invariant — dropLease cancels TimerExpire, so "holding a lease" and
//	"an expiry armed" are meant to be the same state — and announcing first
//	opens a window in which a caller is told it holds an address that nothing
//	yet bounds.
//
// MEASURED 2026-08-30: with the announcement first, twelve concurrent
// `go test -race -count=100 -run TestManagerReportsExpiry ./lease/` gave 2 failing
// processes, every one of them `no expiry timer armed after acquisition`.
// -race does not flag it, because the two sides are synchronised; the ordering
// was simply wrong.
func (m *Machine) enterBound(now Instant, l Lease, out *actions, renewal bool) {
	prev, hadPrev := m.lease, m.haveLse
	out.cancel(m, TimerRetransmit)
	m.lease = l
	m.haveLse = true
	m.state = StateBound
	m.offer = nil
	m.retransmits = 0
	m.sendFailures = 0

	d := l.Deadlines()
	if d.Note != "" {
		out.journal(m, d.Note)
	}
	if d.HasExpire && d.Expire.Sub(now) < 0 {
		// The ACK arrived after the lease it grants had already run out.
		// armDeadline arms for zero rather than for a negative delay, so ring
		// 3 fires it at once and the machine reports the loss, instead of a
		// negative duration meaning whatever the timer implementation happens
		// to do with one.
		out.journal(m, "DHCPACK grants a lease that has already expired")
	}
	if !d.HasExpire {
		out.journal(m, "lease is infinite: no expiry timer armed")
	}
	m.armDeadline(now, TimerExpire, d.Expire, d.HasExpire, out)
	m.armDeadline(now, TimerRenew, d.Renew, d.HasRenew, out)
	m.armDeadline(now, TimerRebind, d.Rebind, d.HasRebind, out)

	if !renewal {
		out.stamp(m, Action{Kind: ActLeaseAcquired, Lease: l, Requested: m.requestedAddr()})
		return
	}
	// G-3: a Renewed action on EVERY ACK that extends a held lease, whether
	// or not the contents changed. What the chassis has to record is that the
	// lease is still ours and now runs to a later moment, and that fact is
	// invisible in a diff of the contents — the contents are precisely what
	// did not change. A caller that only saw Changed would log nothing for a
	// year of successful renewals and could not tell a renewing client from a
	// silently stuck one.
	out.stamp(m, Action{Kind: ActLeaseRenewed, Lease: l})
	if hadPrev && !prev.Equal(l) {
		if prev.Addr != l.Addr {
			// The server extended the lease onto a DIFFERENT address. RFC
			// 2131 does not forbid it — section 4.4.5's DHCPACK carries a
			// 'yiaddr' like any other — and the honest reading is that the
			// binding moved. Journalled by name because it is the one lease
			// change that invalidates everything the caller configured, and
			// because a server doing it by accident is worth seeing.
			out.journal(m, "renewal moved the address from "+prev.Addr.String()+" to "+l.Addr.String())
		}
		// AFTER the renewal, not before: ActLeaseRenewed carries the new
		// lease and is the caller's record of it, and ActLeaseChanged is the
		// instruction to act on the difference. A caller reconfiguring on
		// Changed must already hold what it is reconfiguring to.
		out.stamp(m, Action{Kind: ActLeaseChanged, Lease: l})
	}
}

// armDeadline arms t for the deadline at ts, or CANCELS t when there is none.
//
// The cancel is emitted rather than nothing, because enterBound is reached two
// ways. From a fresh acquisition, beginAcquisition has already disarmed
// everything and the cancel is redundant. From a renewal, the previous lease's
// timers are still armed: a lease whose successor carries no T2 would keep the
// old one's, and rebind at a moment computed from a lease that no longer
// exists. Emitting set-or-cancel for all three makes "armed" a function of the
// lease in hand and of nothing else.
func (m *Machine) armDeadline(now Instant, t TimerID, ts Instant, has bool, out *actions) {
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

func (m *Machine) dropLease(out *actions, r Reason) {
	if !m.haveLse {
		return
	}
	m.haveLse = false
	m.lease = Lease{}
	// All three lease timers, not only the expiry. "Holding a lease" and
	// "the deadlines of that lease are armed" are meant to be the same state,
	// and after M3 a lease has three deadlines. Every caller happens to reach
	// cancelAll afterwards today; that is a fact about the callers, and this
	// is the invariant.
	out.cancel(m, TimerExpire)
	out.cancel(m, TimerRenew)
	out.cancel(m, TimerRebind)
	// stamp, not add: every action carries a unique id so an EvActionFailed
	// can name exactly which one did not happen (R2). An unstamped action
	// carries id 0, which collides with the first stamped action of the
	// machine's life — and the collision is silent.
	out.stamp(m, Action{Kind: ActLeaseLost, Reason: r})
}

// noteActionFailed is R2: an action the machine emitted did not happen.
//
// A failed Send is the case that matters. The retransmission counter is NOT
// advanced — the server never saw anything — and the retransmit timer is
// re-armed at the current attempt's delay. After MaxSendFailures consecutive
// failures the transport is reported broken with a typed reason, instead of
// the machine sitting in SELECTING looking healthy.
func (m *Machine) noteActionFailed(now Instant, rnd uint64, ev Event, out *actions) {
	m.sendFailures++
	out.journal(m, fmt.Sprintf("%s failed (%s), consecutive failures %d",
		ev.Action, ev.Reason, m.sendFailures))
	if m.state == StateRenewing || m.state == StateRebinding {
		// A HELD LEASE IS NEVER GIVEN UP FOR A SEND FAILURE. MaxSendFailures
		// exists so a machine with a broken transport does not sit in
		// SELECTING looking healthy; in RENEWING there is nothing to look
		// healthy about, because the lease already has a deadline and the
		// expiry timer is already armed to report the loss when it arrives.
		//
		// This is the answer to the relay case (runtime.PacketTransport's
		// BOUNDS): a unicast the transport refuses because the server is
		// behind a relay fails EVERY time, so escalating on a count would
		// drop a perfectly good lease within seconds of T1. Instead the
		// machine keeps retransmitting on section 4.4.5's schedule until T2
		// promotes it to REBINDING, where the DHCPREQUEST is broadcast and
		// reaches the relay. TestRenewalSurvivesATransportThatRefusesUnicast
		// drives exactly that: every unicast refused, the lease still held,
		// and a broadcast on the wire at T2.
		out.set(m, TimerRetransmit, m.renewalDelay(now))
		return
	}
	if m.params.MaxSendFailures > 0 && m.sendFailures >= m.params.MaxSendFailures {
		out.failed(m, ReasonTransport, fmt.Sprintf("%d consecutive send failures: %s",
			m.sendFailures, ev.Reason))
		m.dropLease(out, ReasonTransport)
		if m.state != StateInit {
			// Parking is what INIT already is, and toInitIdle would also
			// cancelAll — taking with it the restart wait armed by
			// declineAndRestart, which is the ONE timer in INIT that nothing
			// re-arms. A failed DHCPDECLINE send would then leave the machine
			// in INIT with no lease, no timer and no event coming: the exact
			// shape the restart wait exists to close.
			// TestAFailedDeclineDoesNotCancelTheRestart.
			m.toInitIdle(out)
		}
		return
	}
	switch m.state {
	case StateSelecting:
		out.set(m, TimerRetransmit, m.params.Discover.Delay(m.retransmits, rnd))
	case StateRequesting, StateRebooting:
		// REBOOTING shares REQUESTING's schedule because section 3.2(3) points
		// at the same algorithm: "The client retransmits the DHCPREQUEST
		// according to the retransmission algorithm in section 4.1." Without
		// this arm a failed send in REBOOTING leaves no timer armed and no
		// event coming — a machine that has stopped, silently, in the one
		// state whose whole purpose is to be answered or to give up.
		out.set(m, TimerRetransmit, m.params.Request.Delay(m.retransmits, rnd))
	}
}

// enterRenewing is T1. RFC 2131 section 4.4.5: "the client moves to RENEWING
// state and sends (via unicast) a DHCPREQUEST message to the server to extend
// its lease".
//
// A FRESH TRANSACTION IS OPENED HERE AND SPANS RENEWING AND REBINDING BOTH.
// Section 2 defines 'secs' as "seconds elapsed since client began address
// acquisition OR RENEWAL process", which names the renewal as one process with
// one start; and drawing a second xid at T2 would make unacceptable every
// DHCPACK the server had already sent to the RENEWING transaction, throwing
// away an answer that was on its way. TestRenewalKeepsOneTransactionAcrossT2
// holds it.
func (m *Machine) enterRenewing(now Instant, rnd uint64, out *actions) {
	sid := m.lease.ServerID
	if !sid.Is4() || sid.IsUnspecified() {
		// Section 4.4.5 unicasts the renewal "to the server", and the server
		// is the lease's server identifier. Without one there is no unicast
		// to address, so the machine STAYS IN BOUND and waits for T2, where
		// the same DHCPREQUEST is broadcast and needs no server identifier.
		//
		// Nothing is at risk in waiting: T2 and the expiry are both armed, so
		// this costs the renewal attempt between T1 and T2 and not the lease.
		// The alternative — rebinding immediately at T1 — would broadcast
		// during the window RFC 2131 reserves for the leasing server alone.
		out.journal(m, "T1 reached with no server identifier in the lease: staying in BOUND until T2, where the DHCPREQUEST is broadcast (RFC 2131 4.4.5)")
		return
	}
	m.beginRenewalTransaction(now, rnd)
	m.state = StateRenewing
	m.sendRenewal(now, out)
}

// enterRebinding is T2. RFC 2131 section 4.4.5: "the client moves to REBINDING
// state and sends (via broadcast) a DHCPREQUEST message to extend its lease".
//
// fresh distinguishes the two ways in. From BOUND — a lease with no server
// identifier, so RENEWING was never entered — the transaction starts here.
// From RENEWING it does NOT: see enterRenewing on why one transaction spans
// both.
func (m *Machine) enterRebinding(now Instant, rnd uint64, out *actions, fresh bool) {
	if fresh {
		m.beginRenewalTransaction(now, rnd)
	}
	m.state = StateRebinding
	m.sendRenewal(now, out)
}

// beginRenewalTransaction opens the renewal transaction: a new xid and a new
// 'secs' origin, counters cleared.
//
// It does NOT touch the lease. That is the whole difference from
// beginAcquisition, which is the same reset for a machine that holds nothing.
func (m *Machine) beginRenewalTransaction(now Instant, rnd uint64) {
	m.xid = uint32(split(rnd, 0))
	m.startedAt = now
	m.started = true
	m.retransmits = 0
	m.sendFailures = 0
	m.offer = nil
}

// RenewRetransmitFloor is the floor RFC 2131 section 4.4.5 puts under the wait
// between renewal retransmissions, in both RENEWING and REBINDING: "one-half
// of the remaining time until T2 ..., down to a minimum of 60 seconds".
const RenewRetransmitFloor = 60 * Second

// renewalDelay is section 4.4.5's retransmission schedule: half the time
// remaining to the deadline that ends the current state, floored.
//
// The deadline is T2 in RENEWING and the lease expiry in REBINDING, which is
// the section's own wording twice over — "one-half of the remaining time until
// T2" and "one-half of the remaining lease time".
//
// IT IS THE ONE SCHEDULE IN THIS MACHINE THAT IS NOT Backoff. Params.Discover
// and Params.Request are budgeted retransmissions: jittered, doubling, with an
// exhaustion point. This one has no budget — the T2 and expiry deadlines end
// it, not a count — and must not jitter, because the halving is exactly what
// converges the last attempt onto the deadline instead of past it.
func (m *Machine) renewalDelay(now Instant) Duration {
	d := m.lease.Deadlines()
	var until Instant
	switch {
	case m.state == StateRenewing && d.HasRebind:
		until = d.Rebind
	case d.HasExpire:
		until = d.Expire
	default:
		// REBINDING on an infinite lease, or RENEWING on one whose T2 the
		// server did not send: there is no deadline to converge on, so the
		// floor is the whole schedule. Retransmitting once a minute forever
		// is the right shape for a lease that never ends.
		return RenewRetransmitFloor
	}
	half := until.Sub(now) / 2
	if half < RenewRetransmitFloor {
		return RenewRetransmitFloor
	}
	return half
}

// -------------------------------------------------------------- messages --

// sendRenewal builds and sends the DHCPREQUEST of RENEWING and REBINDING.
//
// RFC 2131 Table 5's "DHCPREQUEST generated during RENEWING state" column:
// 'ciaddr' is the client's IP address (MUST), the 'requested IP address'
// option MUST NOT be filled in, and the 'server identifier' option MUST NOT be
// filled in. Section 4.3.2 is why the last one matters most: a server reads a
// DHCPREQUEST carrying a server identifier as one "generated during SELECTING
// state" and answers a different question entirely — it checks the identifier
// against itself and stays silent if it does not match, so a renewal sent with
// one to a server that has since changed its identifier is never answered at
// all.
//
// base() adds neither option: they are added at sendDiscover's and
// sendRequest's own call sites. TestRenewalOmitsTheRequestedAddressAndServerIdentifier
// is what holds that, because "base adds neither" is a fact about today's
// base() and the MUST NOT is about every future one.
//
// The BROADCAST FLAG follows Params.Broadcast here as everywhere else, and
// that is a deliberate bound rather than an oversight. Section 4.4.5 says the
// RENEWING request is unicast "so no relay agents will be involved", which
// makes the flag's stated purpose — telling a relay how to return the reply —
// moot; a server that honours it broadcasts the DHCPACK, which this library's
// AF_PACKET transport reads either way. MEASURED 2026-09-02 against dnsmasq
// 2.91: it ignores the flag once 'ciaddr' is set and unicasts the ACK to the
// address in it.
func (m *Machine) sendRenewal(now Instant, out *actions) {
	msg := m.base(now, wire.MsgRequest)
	msg.CIAddr = m.lease.Addr.Addr()
	// Section 4.4.5 measures the new lease from the moment the DHCPREQUEST is
	// SENT, and a retransmitted request is the one the server answered, so
	// this is re-set on every attempt and not only the first.
	m.requestSentAt = now
	if m.state == StateRenewing {
		// Section 4.4.5: unicast to the server. Src is the leased address —
		// the datagram carries it in 'ciaddr' and must come FROM it, or the
		// server has no return path and ring 3 has nothing to build an IP
		// header from. Same reason as the DHCPRELEASE above.
		out.send(m, msg, Dest{Addr: m.lease.ServerID, Src: msg.CIAddr})
	} else {
		out.send(m, msg, Dest{Broadcast: true})
	}
	out.set(m, TimerRetransmit, m.renewalDelay(now))
}
func (m *Machine) sendDiscover(now Instant, rnd uint64, out *actions) {
	msg := m.base(now, wire.MsgDiscover)
	if m.params.RequestedIP.Is4() && !m.params.RequestedIP.IsUnspecified() {
		v := m.params.RequestedIP.As4()
		msg.Options[wire.OptRequestedIP] = v[:]
	}
	m.state = StateSelecting
	out.cancel(m, TimerDesync)
	out.send(m, msg, Dest{Broadcast: true})
	out.set(m, TimerRetransmit, m.params.Discover.Delay(m.retransmits, rnd))
}

func (m *Machine) sendRequest(now Instant, rnd uint64, out *actions) {
	if m.offer == nil {
		// Cannot happen through the state machine — REQUESTING is only
		// entered from an OFFER — and handled rather than asserted, because a
		// nil dereference in ring 1 is a plugin crash.
		out.journal(m, "REQUEST with no offer held: restarting")
		m.toInitIdle(out)
		return
	}
	msg := m.base(now, wire.MsgRequest)
	// RFC 2131 section 4.4.1's table: in SELECTING the REQUEST carries the
	// 'requested IP address' (MUST) and the 'server identifier' (MUST), and
	// 'ciaddr' is zero.
	yi := m.offer.YIAddr.As4()
	msg.Options[wire.OptRequestedIP] = yi[:]
	if sid, ok := m.offer.Addr4(wire.OptServerID); ok {
		v := sid.As4()
		msg.Options[wire.OptServerID] = v[:]
	}
	m.state = StateRequesting
	m.requestSentAt = now
	out.send(m, msg, Dest{Broadcast: true})
	out.set(m, TimerRetransmit, m.params.Request.Delay(m.retransmits, rnd))
}

// terminalFields returns the address and the server identifier a DHCPDECLINE
// or DHCPRELEASE must carry, or a note naming what the held lease lacks.
//
// RFC 2131 Table 5 makes the server identifier a MUST in both messages, so a
// lease without one cannot produce a conformant message at all. Neither
// message is answered, so an unusable one would be indistinguishable from a
// correct one on the wire; it is not sent, and the note says why.
func (m *Machine) terminalFields() (addr, sid netip.Addr, why string) {
	addr = m.lease.Addr.Addr()
	sid = m.lease.ServerID
	switch {
	case !addr.Is4() || addr.IsUnspecified():
		return addr, sid, "the held lease carries no IPv4 address"
	case !sid.Is4() || sid.IsUnspecified():
		return addr, sid, "the held lease carries no server identifier (RFC 2131 Table 5 makes it a MUST)"
	}
	return addr, sid, ""
}

// terminalBase builds the part of a DHCPDECLINE or DHCPRELEASE that is common
// to both.
//
// It does NOT call base(). RFC 2131 Table 5 gives these two messages their own
// column, and six of base()'s outputs are wrong in it: 'secs' is 0 rather than
// the elapsed time, 'flags' is 0 so the BROADCAST bit stays clear, and the
// host name, vendor class, parameter request list and requested lease time are
// all forbidden by the column's "All others: MUST NOT".
// TestDeclineAndReleaseCarryOnlyThePermittedOptions is what holds that apart from
// base(), which is otherwise the natural thing to reuse.
//
// xid is drawn fresh because Table 5's cell for this column reads "selected by
// client", where the DHCPREQUEST column reuses the DHCPOFFER's.
func (m *Machine) terminalBase(t wire.MessageType, xid uint32) *wire.Message {
	msg := &wire.Message{
		Op:      wire.BootRequest,
		HType:   wire.HTypeEthernet,
		XID:     xid,
		CHAddr:  append([]byte(nil), m.params.CHAddr...),
		Options: wire.Options{},
	}
	msg.SetType(t)
	// Table 5 says MAY; RFC 2131 section 3.1(6) upgrades it to a MUST for a
	// client that used a client identifier to obtain the lease. One builder
	// for both messages is what makes that MUST unbreakable by editing a
	// single call site.
	if len(m.params.ClientID) > 0 {
		msg.Options[wire.OptClientID] = append([]byte(nil), m.params.ClientID...)
	}
	return msg
}

// sendDecline emits the DHCPDECLINE for the held lease.
func (m *Machine) sendDecline(rnd uint64, out *actions) {
	addr, sid, why := m.terminalFields()
	if why != "" {
		out.journal(m, "DHCPDECLINE not sent: "+why)
		return
	}
	msg := m.terminalBase(wire.MsgDecline, uint32(split(rnd, 4)))
	// Table 5: the declined address is the 'requested IP address' option
	// (MUST) and 'ciaddr' is 0. The DHCPRELEASE column reverses BOTH cells;
	// TestDeclineAndReleaseCarryTheAddressInTheRightPlace holds them apart.
	v := addr.As4()
	msg.Options[wire.OptRequestedIP] = v[:]
	sv := sid.As4()
	msg.Options[wire.OptServerID] = sv[:]
	msg.Options[wire.OptMessage] = []byte(declineMessage)
	// RFC 2131 section 4.4.4: "Because the client is declining the use of the
	// IP address supplied by the server, the client broadcasts DHCPDECLINE
	// messages."
	out.send(m, msg, Dest{Broadcast: true})
}

// sendRelease emits the DHCPRELEASE for the held lease.
func (m *Machine) sendRelease(rnd uint64, out *actions) {
	addr, sid, why := m.terminalFields()
	if why != "" {
		out.journal(m, "DHCPRELEASE not sent: "+why)
		return
	}
	msg := m.terminalBase(wire.MsgRelease, uint32(split(rnd, 5)))
	// Table 5, the other way round from the DHCPDECLINE above: the released
	// address is the 'ciaddr' FIELD, and the 'requested IP address' option is
	// a MUST NOT here.
	msg.CIAddr = addr
	sv := sid.As4()
	msg.Options[wire.OptServerID] = sv[:]
	msg.Options[wire.OptMessage] = []byte(releaseMessage)
	// RFC 2131 section 4.4.4: "The client unicasts DHCPRELEASE messages to the
	// server." Src is the released address: the datagram carries it in
	// 'ciaddr' and must also come FROM it, or the server has no return path
	// and ring 3 has nothing to build an IP header from.
	out.send(m, msg, Dest{Addr: sid, Src: addr})
}

// The option 56 text of the two messages. Table 5 makes 'message' a SHOULD for
// both, and it is the only place a server's log can say why the client did it.
const (
	declineMessage = "address already in use"
	releaseMessage = "no longer required"
)

// base builds the common part of a client message.
func (m *Machine) base(now Instant, t wire.MessageType) *wire.Message {
	msg := &wire.Message{
		Op:      wire.BootRequest,
		HType:   wire.HTypeEthernet,
		XID:     m.xid,
		Secs:    m.secs(now),
		CHAddr:  append([]byte(nil), m.params.CHAddr...),
		Options: wire.Options{},
	}
	if m.params.Broadcast {
		msg.Flags |= wire.FlagBroadcast
	}
	msg.SetType(t)
	if len(m.params.ClientID) > 0 {
		msg.Options[wire.OptClientID] = append([]byte(nil), m.params.ClientID...)
	}
	if len(m.params.fqdn) > 0 {
		// RFC 4702 section 3.1: a client sending option 81 "MUST NOT also
		// send the Host Name option". The two carry the same name, and a
		// server that got both would have to choose (section 4 tells it to
		// ignore option 12) — so the choice is made here, where the caller's
		// intent is known, and option 12 is not sent at all.
		msg.Options[wire.OptFQDN] = append([]byte(nil), m.params.fqdn...)
	} else if m.params.Hostname != "" {
		msg.Options[wire.OptHostName] = []byte(m.params.Hostname)
	}
	if m.params.VendorClass != "" {
		msg.Options[wire.OptVendorClassID] = []byte(m.params.VendorClass)
	}
	if pl := m.params.parameterList(); len(pl) > 0 {
		b := make([]byte, 0, len(pl))
		for _, c := range pl {
			b = append(b, byte(c))
		}
		msg.Options[wire.OptParameterList] = b
	}
	if m.params.RequestedLease > 0 && !m.params.RequestedLease.IsInfinite() {
		secs := uint32(m.params.RequestedLease.Seconds())
		msg.Options[wire.OptLeaseTime] = []byte{
			byte(secs >> 24), byte(secs >> 16), byte(secs >> 8), byte(secs),
		}
	}
	return msg
}

// secs is RFC 2131 section 2's 'secs': "Filled in by client, seconds elapsed
// since client began address acquisition or renewal process."
//
// It saturates at 65535 rather than wrapping. A wrap makes a client that has
// been trying for eighteen hours look like one that just started, which is the
// opposite of what a relay agent reads the field for.
func (m *Machine) secs(now Instant) uint16 {
	if !m.started {
		return 0
	}
	d := now.Sub(m.startedAt)
	if d <= 0 {
		return 0
	}
	s := d.Seconds()
	if s > 65535 {
		return 65535
	}
	return uint16(s)
}

// acceptable applies the filtering RFC 2131 requires before a message is
// looked at: it must be a reply, it must carry a message type, and its xid
// must match the transaction in flight.
//
// This lives in ring 1 and not in the transport on purpose. "If the 'xid' of
// an arriving DHCPOFFER message does not match the 'xid' of the most recent
// DHCPDISCOVER message, the DHCPOFFER message must be silently discarded"
// (RFC 2131 section 4.4.1) is a protocol rule, and a protocol rule enforced in
// ring 3 is a protocol rule with no fast test.
func (m *Machine) acceptable(msg *wire.Message, out *actions) (*wire.Message, bool) {
	if msg == nil {
		out.journal(m, "nil message: discarded")
		return nil, false
	}
	if msg.Op != wire.BootReply {
		out.journal(m, fmt.Sprintf("%s is not a BOOTREPLY: discarded", msg.Op))
		return nil, false
	}
	if _, ok := msg.Type(); !ok {
		out.journal(m, "message carries no DHCP message type: discarded")
		return nil, false
	}
	if msg.XID != m.xid {
		out.journal(m, fmt.Sprintf("xid %#08x does not match %#08x: discarded", msg.XID, m.xid))
		return nil, false
	}
	sid, hasSID := msg.Addr4(wire.OptServerID)
	if ok, why := m.params.Servers.permits(sid, hasSID); !ok {
		// P-4. Applied HERE, in the one predicate every inbound message
		// passes through, rather than at the three places a server
		// identifier is read. A filter attached to the DHCPOFFER alone would
		// let a denied server NAK this client out of its lease, and one
		// attached to OFFER and ACK would still let it do so during a
		// renewal.
		out.journal(m, why+": discarded")
		return nil, false
	}
	if !m.chaddrMatches(msg) {
		// Not required by RFC 2131 in so many words, and cheap: a reply whose
		// chaddr is somebody else's is either a relay bug or a collision, and
		// accepting it configures this host with another host's address.
		out.journal(m, "chaddr does not match: discarded")
		return nil, false
	}
	return msg, true
}

func (m *Machine) chaddrMatches(msg *wire.Message) bool {
	if len(msg.CHAddr) != len(m.params.CHAddr) {
		return false
	}
	for i := range msg.CHAddr {
		if msg.CHAddr[i] != m.params.CHAddr[i] {
			return false
		}
	}
	return true
}

func (m *Machine) nakText(msg *wire.Message) string {
	if s, ok := msg.Text(wire.OptMessage); ok && s != "" {
		return "DHCPNAK: " + s
	}
	return "DHCPNAK"
}

// ---------------------------------------------------------------- output --

// actions accumulates the action list, stamping each with the machine's next
// ActionID so a failure can name exactly which one did not happen.
type actions struct{ list []Action }

func (a *actions) stamp(m *Machine, x Action) {
	x.ID = m.nextAction
	m.nextAction++
	a.list = append(a.list, x)
}

func (a *actions) send(m *Machine, msg *wire.Message, d Dest) {
	a.stamp(m, Action{Kind: ActSend, Msg: msg, Dest: d})
}

func (a *actions) set(m *Machine, t TimerID, d Duration) {
	a.stamp(m, Action{Kind: ActSetTimer, Timer: t, After: d})
}

func (a *actions) cancel(m *Machine, t TimerID) {
	a.stamp(m, Action{Kind: ActCancelTimer, Timer: t})
}

// cancelAll disarms every timer, enumerated from AllTimerIDs rather than
// hand-listed. It replaced three hand-lists that all had to be edited together;
// TestEveryPathToIdleCancelsEveryTimer drives the property they were keeping.
func (a *actions) cancelAll(m *Machine) {
	for _, t := range AllTimerIDs() {
		a.cancel(m, t)
	}
}

func (a *actions) journal(m *Machine, note string) {
	a.stamp(m, Action{Kind: ActJournal, Note: note})
}

func (a *actions) failed(m *Machine, r Reason, note string) {
	a.stamp(m, Action{Kind: ActFailed, Reason: r, Note: note})
}
