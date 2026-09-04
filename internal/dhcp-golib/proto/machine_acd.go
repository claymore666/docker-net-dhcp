package proto

import (
	"fmt"
	"net/netip"

	"github.com/claymore666/dhcp-golib/wire"
)

// The Machine's half of RFC 5227: which of the sub-machine's verdicts becomes
// a DHCPDECLINE, where the probing phase sits in RFC 2131's state diagram, and
// what each of the three conflict modes does with an incoming DHCPACK.
//
// The DIVISION with acd.go is deliberate and is the ring rule applied one level
// down: acd.go owns the timing and the verdict and knows nothing about DHCP;
// this file owns the consequences and knows nothing about ARP timing.

// ACDPhase reports where RFC 5227's check is, for a caller that has to show it
// or record it.
//
// It is ACDIdle when Params.Conflict is ConflictOff, which is the truth: that
// client runs no check, so the check is not anywhere.
func (m *Machine) ACDPhase() ACDPhase {
	if m.acd == nil {
		return ACDIdle
	}
	return m.acd.phase
}

// ARPRelevant reports whether an ARP packet is worth feeding to Step.
//
// IT IS A SUBSCRIPTION FILTER AND NOT A VERDICT. Ring 2 sees every ARP frame
// on the link, and every event that reaches Step costs a journal entry — so
// without this, ordinary ARP traffic would wrap the bounded journal between one
// acquisition and the next and take the replay with it.
//
// It lives here, in the pure ring, because a filter written beside a channel is
// a protocol rule in a place with no test.
// TestTheRelevanceFilterCannotHideAConflict runs it against the conflict rules
// over the whole adversarial corpus: nothing the rules would call a conflict is
// something this drops.
func (m *Machine) ARPRelevant(p *wire.ARPPacket) bool {
	if m.acd == nil {
		return false
	}
	return m.acd.relevant(p)
}

// heldOrProbing is the lease this client holds OR has been granted and is
// checking.
//
// The distinction it erases is deliberate and narrow. A caller must not be told
// about a probed lease — D22 says the address is not usable — so Lease() keeps
// reporting nothing. But the SERVER has made the allocation either way: it
// answered a DHCPREQUEST with a DHCPACK and wrote a binding. So a DHCPDECLINE
// that names the address and its server identifier (RFC 2131 Table 5 makes
// both a MUST) has to be able to see it, and so does a DHCPRELEASE from a
// caller that changed its mind during the probe.
//
// A machine in neither condition returns the zero Lease, which terminalFields
// refuses by name rather than sending a message with an empty 'ciaddr'.
func (m *Machine) heldOrProbing() Lease {
	if m.haveLse {
		return m.lease
	}
	if m.haveProbe {
		return m.probing
	}
	return Lease{}
}

// afterAck is what a DHCPACK that grants a usable lease does, and it is the one
// place the three conflict modes differ.
//
// RFC 2131 figure 5 has one edge here — "DHCPACK/Record lease, set timers T1,
// T2" into BOUND — and D23 splits it three ways:
//
//   - ConflictOff takes the edge as written. Permitted because section 3.1(5)'s
//     pre-use check is a SHOULD and its DHCPDECLINE a MUST only "If the client
//     detects that the address is already in use"; see ConflictOff.
//   - ConflictAsync takes the edge as written AND starts section 2.1's probing
//     beside it. The lease is announced now and may be withdrawn seconds later.
//   - ConflictWait, the default, does not take the edge yet: the lease is held
//     back in StateProbing until section 2.1's check completes. D22.
func (m *Machine) afterAck(now Instant, rnd uint64, l Lease, out *actions) {
	switch m.params.Conflict {
	case ConflictWait:
		m.enterProbing(now, rnd, l, out)
	case ConflictAsync:
		// THE CHECK IS STARTED BEFORE THE LEASE IS ANNOUNCED, and that is not
		// a contradiction of "announce at once": starting it emits one
		// ActSetTimer and nothing else — no probe goes out here, and the
		// caller is told about the lease in the same action list either way.
		// What the order buys is that the Acquired event and the record it
		// produces carry ACDProbing rather than ACDIdle, which is the fact
		// D23 requires them to carry: a caller that restarts inside the
		// window has to resume the probe, and a record saying "idle" would
		// tell it there was nothing to resume.
		m.startACD(now, rnd, l.Addr.Addr(), out)
		m.enterBound(now, rnd, l, out, false)
	default:
		m.enterBound(now, rnd, l, out, false)
	}
}

// enterProbing holds the ACKed lease back and begins RFC 5227 section 2.1's
// check.
//
// NO LEASE TIMER IS ARMED HERE, and that is the M1 rule kept rather than
// broken: "the lease is announced only after its expiry timer is armed" — and
// nothing is announced here. The deadlines are absolute Instants computed from
// the moment the DHCPREQUEST was sent (RFC 2131 section 4.4.5), so enterBound
// arms them correctly against the later `now` when probing finishes; the
// probing time comes out of the lease, exactly as the round-trip time already
// did. A lease short enough to expire inside the probe window arms its expiry
// for zero and is reported lost at once, which is what armDeadline does with
// any deadline already past.
func (m *Machine) enterProbing(now Instant, rnd uint64, l Lease, out *actions) {
	out.cancel(m, TimerRetransmit)
	m.probing = l
	m.haveProbe = true
	m.state = StateProbing
	m.offer = nil
	m.retransmits = 0
	m.sendFailures = 0
	out.journal(m, fmt.Sprintf("DHCPACK for %s: checking it is free before use (RFC 5227 2.1)", l.Addr.Addr()))
	m.startACD(now, rnd, l.Addr.Addr(), out)
}

// stepProbing is StateProbing: the DHCPACK is in hand, the address is not in
// use, and RFC 5227 section 2.1's check is running.
//
// WHAT MAKES IT A STATE AND NOT A PAUSE is that every event below has a
// different answer here than in BOUND, because there is no announced lease to
// lose. Nothing here emits ActLeaseLost: dropLease is guarded on m.haveLse and
// this machine has none, so a conflict found in the window produces a
// DHCPDECLINE and a restart and no loss report at all. That is what the chassis
// counts: seam row G-5's Lost{ReasonConflict} is the section 2.4 path, and this
// one is visible in Stats.ConflictsDetected and in the journal instead.
func (m *Machine) stepProbing(now Instant, rnd uint64, ev Event, out *actions) {
	switch ev.Kind {
	case EvStop:
		m.stop(out)
	case EvStart:
		out.journal(m, "Start in PROBING: already running")
	case EvRelease:
		// A REAL DHCPRELEASE, unlike releaseBeforeBound's silent halt. The
		// server has already written the binding — it sent the DHCPACK — so
		// section 4.4.6's "no longer requires use of its assigned network
		// address" applies to an address this client was assigned and never
		// used. Staying silent here would leak the binding for the whole lease
		// time on every caller that gives up during the probe.
		m.release(rnd, out)
	case EvTimerFired:
		switch ev.Timer {
		case TimerACD:
			m.acdTimer(now, rnd, out)
		default:
			out.journal(m, fmt.Sprintf("timer %s fired in PROBING: ignored", ev.Timer))
		}
	case EvARPReceived:
		m.onARP(now, rnd, ev, out)
	case EvConflictDetected:
		// The caller's own evidence, from ReportConflict. It is honoured in
		// this state for the same reason it is honoured in BOUND: the caller
		// may see things this client cannot — a kernel ARP cache entry, a
		// switch complaint, its own second interface.
		m.declineAndRestart(now, rnd, out)
	case EvLinkDown:
		// Nothing to give up: no lease is held and the address was never
		// configured. The ACKed allocation is abandoned WITHOUT a DHCPDECLINE,
		// because there is no conflict to report — RFC 2131 section 4.4.1
		// makes the DHCPDECLINE the answer to an address in use, and a link
		// that went down is evidence of nothing about the address.
		out.journal(m, "link down while probing: the ACKed address is abandoned unchecked")
		m.toInitIdle(out)
	case EvAddressLost:
		out.journal(m, "AddressLost in PROBING: no address is configured yet, ignored")
	case EvReceived:
		// A retransmitted DHCPACK for the transaction that got us here, or a
		// late DHCPOFFER. Neither restarts anything: the transaction is over.
		out.journal(m, "message in PROBING with no transaction open: discarded")
	case EvActionFailed:
		m.noteActionFailed(now, rnd, ev, out)
	default:
		out.journal(m, fmt.Sprintf("%s ignored in PROBING", ev.Kind))
	}
}

// onARP feeds one received ARP packet to RFC 5227's rules.
func (m *Machine) onARP(now Instant, rnd uint64, ev Event, out *actions) {
	if m.acd == nil {
		// ConflictOff, and ring 2 should not be delivering these at all —
		// there is no listener open. Journalled rather than dropped in
		// silence, because a frame arriving here means the socket outlived the
		// mode that justified it.
		out.journal(m, "ARP packet delivered to a client with conflict detection off: ignored")
		return
	}
	if ev.ARP == nil {
		out.journal(m, "ARP event with no packet: ignored")
		return
	}
	_, acts := m.acd.acdStep(now, rnd, acdEvent{kind: acdEvARP, pkt: ev.ARP})
	m.applyACD(now, rnd, acts, out)
}

// acdTimer is TimerACD firing.
func (m *Machine) acdTimer(now Instant, rnd uint64, out *actions) {
	if m.acd == nil {
		out.journal(m, "ACD timer fired on a client with conflict detection off: ignored")
		return
	}
	_, acts := m.acd.acdStep(now, rnd, acdEvent{kind: acdEvTimer})
	m.applyACD(now, rnd, acts, out)
}

// startACD begins RFC 5227 section 2.1's check for addr.
func (m *Machine) startACD(now Instant, rnd uint64, addr netip.Addr, out *actions) {
	if m.acd == nil {
		return
	}
	_, acts := m.acd.acdStep(now, rnd, acdEvent{kind: acdEvStart, addr: addr})
	m.applyACD(now, rnd, acts, out)
}

// acdIdle returns the sub-machine to ACDIdle and forgets the probed lease.
//
// The CONFLICT COUNT SURVIVES — acd.stop keeps it — because RFC 5227 section
// 2.1.1's rate limit is there for the looping case, and a loop is exactly a
// run of acquisitions each of which ended one of these.
func (m *Machine) acdIdle() {
	if m.acd != nil {
		m.acd.stop()
	}
	m.probing = Lease{}
	m.haveProbe = false
	m.haveARPAction = false
}

// applyACD turns the sub-machine's output into this machine's actions.
//
// THE ORDER OF THE LIST IS THE CONTRACT, not an artefact: a ring-2 caller
// executes it in order and may act on ActLeaseAcquired the moment it sees one.
// So the announcement is emitted before the acquisition (RFC 5227 section 2.3
// permits use "immediately after sending the first of the two ARP
// Announcements"), and the DHCPDECLINE before anything else on a conflict.
func (m *Machine) applyACD(now Instant, rnd uint64, acts []acdAction, out *actions) {
	for _, a := range acts {
		switch a.kind {
		case acdSetTimer:
			out.set(m, TimerACD, a.after)

		case acdCancelTimer:
			out.cancel(m, TimerACD)

		case acdSendProbe:
			m.sendARP(m.acd.probe(), out)

		case acdSendAnnounce:
			m.sendARP(m.acd.announcement(), out)

		case acdReady:
			m.acdReady(now, rnd, out)

		case acdConflict:
			out.journal(m, "address conflict: "+a.note)
			// WHAT THE CHASSIS COUNTS WHEN NOTHING WAS ACQUIRED. A conflict
			// found in the probe window of a ConflictWait client produces no
			// ActLeaseLost, because no lease was ever announced — dropLease is
			// guarded on holding one. Seam row G-5's Lost{ReasonConflict} is
			// the section 2.4 path and stays exactly that; this is the pre-use
			// path, and without this action it would be visible only in the
			// journal, which is not something a counter can be derived from.
			//
			// The two are MUTUALLY EXCLUSIVE, which is what lets ring 2 add
			// them into one ConflictsDetected without double-counting: a lease
			// is either held here or it is not.
			held := m.haveLse
			m.declineAndRestart(now, rnd, out)
			if !held {
				out.failed(m, ReasonConflict, a.note)
			}

		case acdNote:
			out.journal(m, a.note)
		}
	}
}

// acdReady is RFC 5227 section 2.1's check completing with no conflict.
func (m *Machine) acdReady(now Instant, rnd uint64, out *actions) {
	if !m.haveProbe {
		// ConflictAsync, or a ConflictWait client re-probing an address a
		// renewal moved it onto. Either way the lease is already announced and
		// there is nothing to release: the check has simply passed.
		out.journal(m, "RFC 5227 2.1 probing completed with no conflict")
		return
	}
	l := m.probing
	m.probing = Lease{}
	m.haveProbe = false
	out.journal(m, fmt.Sprintf("%s is free (RFC 5227 2.1): taking the lease", l.Addr.Addr()))
	m.enterBound(now, rnd, l, out, false)
}

// sendARP emits one ActSendARP and remembers its id.
//
// The id is remembered so that a failure can be told apart from a failed DHCP
// send without asking what state the machine is in; noteActionFailed says why
// that difference matters.
func (m *Machine) sendARP(p *wire.ARPPacket, out *actions) {
	out.stamp(m, Action{Kind: ActSendARP, ARP: p})
	m.arpAction = out.list[len(out.list)-1].ID
	m.haveARPAction = true
}
