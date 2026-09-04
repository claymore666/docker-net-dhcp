package lease

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sync"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// A Record is one BINDING ATTEMPT: an identity that asked a network for an
// address, from the moment something decided to ask until the address is given
// up for good.
//
// Not one per endpoint. A container stop deletes the endpoint, so a restarted
// container is a new endpoint and a per-endpoint record would forget the
// address at every restart; the phases below are what bridges that gap.
//
// The record is DERIVED. Nothing constructs one directly: it is the fold of an
// append-only event stream (Fold, Rebuild), which is what makes the durable
// form the events rather than the record and what makes a restart a replay
// rather than a reconstruction.
//
// WHAT THIS RING OWNS. The DHCP half: the Params snapshot, the Lease, the
// identity bytes, the phase, the counters. Scope is an opaque namespace token
// supplied by the caller — this package never learns what it names. Everything
// a container runtime knows about an endpoint stays in the caller's envelope
// around these events; ring 2 cannot import it and does not want to.
type Record struct {
	ID    string
	Scope string

	// Family is the address family. Only FamilyV4 exists today.
	Family Family

	// CHAddr is the hardware address the link wears. It may change at a
	// re-bind: an address is kept across a restart by IDENTITY, not by MAC.
	CHAddr []byte

	// Identity is the option 61 / DUID+IAID bytes AS SENT, supplied by the
	// caller and written ONCE.
	//
	// Stored rather than re-derived, and that is the whole of D10's durable
	// half: a runtime that mints a fresh MAC per endpoint makes a MAC-derived
	// identifier a different identifier at every restart, and a conforming
	// server then hands out a different address (RFC 2131 section 4.2). A
	// second write with different bytes is a reject, not an overwrite.
	Identity []byte

	// Params is the parameter set the manager instance ran with, snapshotted
	// with every slice copied.
	//
	// It is here because proto.Replay takes Params: a step journal without the
	// Params that produced it is not replayable, so the record is what makes a
	// saved journal mean anything. One manager instance, one Params; a new
	// instance supplies its own.
	Params *proto.Params

	// Lease is the lease as the caller sees it, on the WALL CLOCK. Ring 1
	// computes every deadline on a monotonic Instant whose epoch is
	// meaningless to the next process; these are the same deadlines converted
	// once, at the event, by the manager's clock bridge, and they are the only
	// ones that survive a restart.
	Lease Lease
	Held  bool

	Phase Phase

	// ACD is where RFC 5227's conflict check stood at the last lease event
	// folded in. D23, and it is durable for one reason: a proto.ConflictAsync
	// client is told Acquired while the check is still running, so a process
	// that restarts inside that window has to know whether the address it is
	// resuming was ever cleared. A record saying ACDProbing has not been; one
	// saying ACDDefending has.
	//
	// It is proto.ACDIdle for a client running with proto.ConflictOff, and for
	// every record written before M6 — which reads correctly, because those
	// clients ran no check either.
	//
	// NOTHING IN THIS LIBRARY READS IT. Stated here, on the field, because the
	// chassis author (M6b) is the only consumer and this is where they will
	// look. MEASURED at round 2's head: the readers of Record.ACD and
	// RecordEvent.ACD are the fold, the JSON tag and the tests; proto.Resume
	// carries an address and an expiry and nothing else.
	//
	// So D23's "a restart during the window resumes the probe, not skips it"
	// is NOT delivered by this field. It is delivered by the machine: on the
	// INIT-REBOOT DHCPACK, afterAck runs the RFC 5227 section 2.1 check
	// unconditionally, whatever the record said — and if the ACK never comes,
	// the remembered lease is not used at all (RFC 2131 section 3.2(3)'s MAY
	// is declined; Machine's REBOOTING arm has the reason). The field is the
	// chassis's evidence for what a half-checked lease was, not this library's
	// control input.
	ACD proto.ACDPhase

	// Deadline is when a RETAINED record may be closed: the caller's
	// min(lease expiry, tombstone TTL). Zero outside RETAINED.
	Deadline time.Time

	// StepsRef points at the step-journal capture for this manager instance,
	// if one was dumped. Opaque to this package.
	StepsRef string

	Counters RecordCounters

	// Extra is the caller's own counters, carried on the same record so that a
	// health snapshot has ONE source.
	//
	// It exists because the counters a chassis reports are not a subset of the
	// ones a DHCP library can derive — a refusal counter, a mount failure, a
	// router-advertisement guard — and a chassis that reads half its numbers
	// from the record and half from somewhere else has two sources that can
	// disagree. The names are the caller's; this package only adds them up.
	Extra map[string]uint64

	// Seq is the sequence number of the last event folded in. It is per
	// record and strictly increasing, which is what makes two writers on one
	// file detectable rather than silently interleaved.
	Seq uint64

	// Instance names the writer of the last event folded in.
	Instance string

	// LastReject is the most recent rejected (phase, op) pair. Counters.Rejects
	// counts them.
	LastReject RejectReason

	// statsBase is the last Stats snapshot merged, and statsManager the
	// MANAGER it came from. A record outlives its managers and each new one
	// starts its counters at zero, so what is accumulated is the DELTA within
	// one manager; a change of manager resets the baseline.
	//
	// This is not the writer. One plugin process runs the CreateEndpoint
	// one-shot manager and then the Join manager, so keying the baseline on
	// the writing PROCESS makes the second manager's first snapshot read as
	// counters going backwards and freezes the wire half at the first
	// manager's numbers. The two identities are separate fields on the event
	// for that reason, and the fold reads this one.
	statsBase    Stats
	statsManager string
	// statsSeen is every manager id this record has folded counters for. It
	// exists only to refuse an id that comes back after another one; see
	// RecordEvent.Manager for what it can and cannot catch.
	statsSeen map[string]bool
}

// Family is the address family a record binds in.
type Family uint8

// The families. Only v4 is implemented; v6 is a later decision and is named
// here so that a record written today says which one it is.
const (
	FamilyUnset Family = 0
	FamilyV4    Family = 4
	FamilyV6    Family = 6
)

func (f Family) String() string {
	switch f {
	case FamilyUnset:
		return "unset"
	case FamilyV4:
		return "v4"
	case FamilyV6:
		return "v6"
	default:
		return fmt.Sprintf("family(%d)", uint8(f))
	}
}

// Phase is where a record is in its life.
//
// PhaseUnset is the zero value and means THERE IS NO RECORD YET. It is a phase
// rather than an absence so that "an event arrived for a record nothing
// created" is one more row of the same total table instead of a nil check
// somebody can forget.
type Phase uint8

// The phases.
const (
	// PhaseUnset: nothing has created this record.
	PhaseUnset Phase = iota
	// PhaseReserved: an address was answered before any endpoint existed.
	PhaseReserved
	// PhaseCreated: the link exists and is bound to this record.
	PhaseCreated
	// PhaseJoined: a manager is running; lease events update the record.
	PhaseJoined
	// PhaseLeft: the manager stopped, the last lease snapshot is kept.
	PhaseLeft
	// PhaseRetained: the tombstone phase. Deadline is set.
	PhaseRetained
	// PhaseAdopted: an address was found already in use with no lease behind
	// it — a replay, or a recovery walk.
	PhaseAdopted
	// PhaseClosed: the deadline passed, or the pool went away.
	PhaseClosed
)

func (p Phase) String() string {
	switch p {
	case PhaseUnset:
		return "unset"
	case PhaseReserved:
		return "reserved"
	case PhaseCreated:
		return "created"
	case PhaseJoined:
		return "joined"
	case PhaseLeft:
		return "left"
	case PhaseRetained:
		return "retained"
	case PhaseAdopted:
		return "adopted"
	case PhaseClosed:
		return "closed"
	default:
		return fmt.Sprintf("phase(%d)", uint8(p))
	}
}

// RecordOp is what an event does to a record.
type RecordOp uint8

// The operations.
const (
	// OpReserve answers an address request that arrived before any endpoint
	// existed.
	OpReserve RecordOp = iota
	// OpCreate binds a link to the record.
	OpCreate
	// OpRebind re-uses a RETAINED record's identity under a new CHAddr.
	OpRebind
	// OpAdopt takes over an address that is already in use.
	OpAdopt
	// OpBind starts a manager on the record.
	OpBind
	// OpLease carries one Acquired, Changed, Renewed or Failed from a manager.
	OpLease
	// OpLost carries one Lost from a manager. Its own op, and not a Kind of
	// OpLease, because one of its reasons is not a loss — see Fold.
	OpLost
	// OpLeave stops the manager and keeps the lease snapshot.
	OpLeave
	// OpRetain lays the tombstone and sets the deadline.
	OpRetain
	// OpClose ends the record.
	OpClose
	// OpStats merges a manager's counters into the record.
	OpStats
	// OpExtra merges the caller's own counters into the record.
	OpExtra
)

func (o RecordOp) String() string {
	switch o {
	case OpReserve:
		return "reserve"
	case OpCreate:
		return "create"
	case OpRebind:
		return "rebind"
	case OpAdopt:
		return "adopt"
	case OpBind:
		return "bind"
	case OpLease:
		return "lease"
	case OpLost:
		return "lost"
	case OpLeave:
		return "leave"
	case OpRetain:
		return "retain"
	case OpClose:
		return "close"
	case OpStats:
		return "stats"
	case OpExtra:
		return "extra"
	default:
		return fmt.Sprintf("op(%d)", uint8(o))
	}
}

// AllPhases and AllOps are the populations, derived from the String methods
// above rather than written out a second time.
//
// They exist so that the totality table is a product of two DERIVED sets: a
// table typed out by hand is missing exactly the pair nobody thought of, and
// adding a phase silently makes it incomplete. The scan covers the whole uint8
// range rather than stopping at the first gap, so a constant added out of
// order is still found.
func AllPhases() []Phase {
	var out []Phase
	for i := 0; i < 256; i++ {
		if p := Phase(i); !isUnknown(p.String()) {
			out = append(out, p)
		}
	}
	return out
}

// AllOps is AllPhases for operations.
func AllOps() []RecordOp {
	var out []RecordOp
	for i := 0; i < 256; i++ {
		if o := RecordOp(i); !isUnknown(o.String()) {
			out = append(out, o)
		}
	}
	return out
}

func isUnknown(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '(' {
			return true
		}
	}
	return false
}

// RecordEvent is one durable line: what happened, to which record, in what
// order, written by whom.
//
// It is a flat union rather than an interface per op. The durable form is one
// JSON object per line, and a shape whose fields depend on a discriminator is
// what a reader has to guess at; every field here is optional and its meaning
// is fixed by Op.
type RecordEvent struct {
	ID  string    `json:"id"`
	Op  RecordOp  `json:"op"`
	Seq uint64    `json:"seq"`
	At  time.Time `json:"at"`

	// Instance names the PROCESS that wrote this line. Two plugin processes
	// during an upgrade both append to one file; without this, their lines are
	// indistinguishable and their sequence numbers collide silently.
	//
	// It is NOT the manager: one process runs several managers in sequence and
	// they share it. See Manager.
	Instance string `json:"instance,omitempty"`

	// Manager names the MANAGER whose counters an OpStats event carries, and
	// is read by nothing else. A manager's Stats start at zero and only ever
	// climb, so the fold accumulates the delta within one manager and
	// rebaselines when this changes; without it, the second manager in a
	// process looks like the first one counting backwards.
	//
	// An OpStats event that does not name one is REFUSED rather than folded
	// under the empty string, because two anonymous managers are exactly the
	// collision this field exists to prevent.
	//
	// UNIQUENESS IS THE CALLER'S OBLIGATION, and it is not free-form. Every
	// manager instance must get an id no other manager instance ever gets —
	// per manager, not per endpoint, per interface or per process, all three
	// of which repeat. Give two managers one id and the fold reads the second
	// one's counters as a continuation of the first's, which is what a
	// snapshot HIGHER than the previous one means by definition; the wire half
	// then undercounts by the first manager's total and nothing is refused,
	// because a higher snapshot under one id is also exactly what a renewal
	// looks like. There is no reading of the numbers that separates the two,
	// so the fold counts, and says so here, rather than pretending to detect.
	//
	// The half that IS decidable is refused: an id that comes BACK after a
	// different manager has been seen is a cycling id, not a continuation, and
	// gives RejectManager. That catches an id derived from something that
	// repeats; it cannot catch two managers handed one id back to back.
	//
	// The cost of getting it wrong is bounded to the wire counters. Phase,
	// lease, identity and the folded counters are derived from the events
	// themselves and are untouched by it.
	Manager string `json:"manager,omitempty"`

	Scope    string        `json:"scope,omitempty"`
	Family   Family        `json:"family,omitempty"`
	CHAddr   []byte        `json:"chaddr,omitempty"`
	Identity []byte        `json:"identity,omitempty"`
	Params   *proto.Params `json:"params,omitempty"`
	Deadline time.Time     `json:"deadline,omitzero"`
	StepsRef string        `json:"steps_ref,omitempty"`

	// Kind, Lease, Reason and Note carry a manager event, for OpLease and
	// OpLost.
	Kind   EventKind    `json:"kind,omitempty"`
	Lease  *Lease       `json:"lease,omitempty"`
	Reason proto.Reason `json:"reason,omitempty"`
	Note   string       `json:"note,omitempty"`

	// ACD is the conflict-detection phase the manager event carried, for
	// OpLease and OpLost. See Record.ACD.
	ACD proto.ACDPhase `json:"acd,omitempty"`

	// Stats is a manager snapshot, for OpStats.
	Stats *Stats `json:"stats,omitempty"`

	// Extra is the caller's own counters, for OpExtra.
	Extra map[string]uint64 `json:"extra,omitempty"`
}

// RejectReason says why a (phase, op) pair was refused.
type RejectReason uint8

// The rejects.
const (
	// RejectNone is the zero value: nothing was rejected.
	RejectNone RejectReason = iota
	// RejectNoRecord: the op needs a record and the phase is unset.
	RejectNoRecord
	// RejectExists: a creating op arrived for a record that already exists.
	RejectExists
	// RejectPhase: the op is not defined from this phase.
	RejectPhase
	// RejectIdentity: a second, different write to a field written once.
	RejectIdentity
	// RejectSeq: the sequence number did not advance.
	RejectSeq
	// RejectScope: the event names a different scope than the record.
	RejectScope
	// RejectID: the event names a different record.
	RejectID
	// RejectStats: a counter snapshot went backwards inside one manager
	// instance.
	RejectStats
	// RejectPayload: the op is defined from this phase but the event does not
	// carry what the op needs.
	RejectPayload
	// RejectFamily: a second, different address family. Written once, like the
	// identity: the family decides which wire the record's address is on, and
	// a record that changed it would answer both lookups.
	RejectFamily
	// RejectManager: a manager id came back after a different manager. Manager
	// ids are unique per manager instance; one that returns is an id derived
	// from something that repeats, and its counters would be folded as one
	// manager's.
	RejectManager
)

func (r RejectReason) String() string {
	switch r {
	case RejectNone:
		return "none"
	case RejectNoRecord:
		return "no such record"
	case RejectExists:
		return "record exists"
	case RejectPhase:
		return "wrong phase"
	case RejectIdentity:
		return "identity rewrite"
	case RejectSeq:
		return "sequence did not advance"
	case RejectScope:
		return "scope mismatch"
	case RejectID:
		return "record id mismatch"
	case RejectStats:
		return "counters went backwards"
	case RejectPayload:
		return "event carries nothing for this op"
	case RejectFamily:
		return "family rewrite"
	case RejectManager:
		return "manager id reused"
	default:
		return fmt.Sprintf("reject(%d)", uint8(r))
	}
}

// Reject is a refused event. It names the pair as well as the reason, because
// "wrong phase" without the phase sends the reader to a diff.
type Reject struct {
	Reason RejectReason
	Op     RecordOp
	Phase  Phase
	ID     string
	Seq    uint64
	Note   string
}

func (r *Reject) Error() string {
	s := fmt.Sprintf("lease: record %s: %s in phase %s: %s", r.ID, r.Op, r.Phase, r.Reason)
	if r.Note != "" {
		s += " (" + r.Note + ")"
	}
	return s
}

// RecordCounters are what a record reports about itself.
//
// TWO HALVES, and they are disjoint on purpose. The upper half is folded from
// this record's own events; Wire is accumulated from the manager Stats
// snapshots. A number that appeared in both would be one fact derived twice,
// and the two derivations part company the moment a record outlives a manager
// — which is the ordinary case, not the exotic one.
// TestTheTwoCounterHalvesAreDisjoint holds the split.
type RecordCounters struct {
	Acquisitions uint64 `json:"acquisitions,omitempty"`
	Changes      uint64 `json:"changes,omitempty"`
	Renewals     uint64 `json:"renewals,omitempty"`
	Losses       uint64 `json:"losses,omitempty"`

	// StoppedNotLost counts the trailing Lost a stopped manager always emits.
	//
	// IT IS NOT A LOSS, and it is counted separately rather than dropped so
	// that "this endpoint's manager was stopped eleven times" stays visible.
	// Cancelling a manager makes ring 1 drop the lease with ReasonStopped, so
	// EVERY acquisition — the one-shot that answers an address request, and
	// every ordinary shutdown — ends with one. A fold that treats it as a loss
	// clears the address it has just recorded and the rebuilt record holds
	// nothing.
	StoppedNotLost uint64 `json:"stopped_not_lost,omitempty"`

	Failures  uint64 `json:"failures,omitempty"`
	Naks      uint64 `json:"naks,omitempty"`
	Timeouts  uint64 `json:"timeouts,omitempty"`
	Conflicts uint64 `json:"conflicts,omitempty"`
	Expiries  uint64 `json:"expiries,omitempty"`
	// Rejects counts refusals THIS RECORD received. It does not count events
	// refused before the record existed: a refusal that precedes the creating
	// event leaves nothing to carry the number, and Rebuild deliberately
	// discards the reject-bumped empty record rather than invent one that no
	// event created. Those refusals live in Rebuilt.Rejects, which is the
	// journal's account and not any record's.
	//
	// This is stated rather than fixed because the alternative is worse: a
	// record materialised by a refusal is a record with no create behind it,
	// and every lookup would then answer for an endpoint that was never made.
	// A caller auditing a journal reads Rebuilt.Rejects; a caller auditing an
	// endpoint reads this.
	Rejects uint64 `json:"rejects,omitempty"`

	Wire WireCounters `json:"wire,omitzero"`
}

// WireCounters is the half of a manager's Stats that the fold does NOT derive
// for itself, accumulated across every manager instance the record has had.
//
// Membership is decided by one rule: a Stats field belongs here unless the
// fold already counts the same fact from the record's own events.
// TestEveryStatsFieldIsEitherWiredOrFolded drives that rule over all of Stats,
// so a counter added there cannot be silently dropped here.
type WireCounters struct {
	Steps            uint64 `json:"steps,omitempty"`
	Sent             uint64 `json:"sent,omitempty"`
	SendFailures     uint64 `json:"send_failures,omitempty"`
	Received         uint64 `json:"received,omitempty"`
	DecodeFailures   uint64 `json:"decode_failures,omitempty"`
	TransportErrors  uint64 `json:"transport_errors,omitempty"`
	TimerFires       uint64 `json:"timer_fires,omitempty"`
	EventsDropped    uint64 `json:"events_dropped,omitempty"`
	ActionsExecuted  uint64 `json:"actions_executed,omitempty"`
	ActionsFailedFed uint64 `json:"actions_failed_fed,omitempty"`
	DeclinesSent     uint64 `json:"declines_sent,omitempty"`
	ReleasesSent     uint64 `json:"releases_sent,omitempty"`
	RequestsDropped  uint64 `json:"requests_dropped,omitempty"`
	RenewalsSent     uint64 `json:"renewals_sent,omitempty"`
	NaksSeen         uint64 `json:"naks_seen,omitempty"`

	// The RFC 5227 wire counters. They belong here and not in the folded half
	// for the rule this type states: nothing in the record's own event stream
	// counts an ARP frame. A record carries lease events, and a probe that
	// went out, a frame that was ignored and a frame that would not decode
	// each produce none.
	ProbesSent        uint64 `json:"probes_sent,omitempty"`
	AnnouncementsSent uint64 `json:"announcements_sent,omitempty"`
	ARPSendFailures   uint64 `json:"arp_send_failures,omitempty"`
	ARPSeen           uint64 `json:"arp_seen,omitempty"`
	ARPIgnored        uint64 `json:"arp_ignored,omitempty"`
	ARPDecodeFailures uint64 `json:"arp_decode_failures,omitempty"`
	ARPErrors         uint64 `json:"arp_errors,omitempty"`
}

// The six Stats fields WireCounters deliberately does not carry, because the
// fold derives the same fact from the record's own events: LeasesAcquired,
// LeasesLost, AcquireFailures, RenewalsCompleted, NaksAccepted and
// ConflictsDetected.
//
// Named here as data rather than in prose so the disjointness test can read it.
var statsFoldedInstead = map[string]string{
	"LeasesAcquired":    "Acquisitions",
	"LeasesLost":        "Losses",
	"AcquireFailures":   "Failures",
	"RenewalsCompleted": "Renewals",
	"NaksAccepted":      "Naks",
	// Both paths, in two arms that cannot both fire for one conflict:
	// foldLost's for a conflict on a lease already in service (RFC 5227
	// section 2.4), foldLease's Failed arm for one found before the address
	// was ever used (section 2.1).
	"ConflictsDetected": "Conflicts",
}

// The reflective arithmetic over WireCounters.
//
// Reflective and not fifteen lines of field-by-field addition, and the reason
// is the rule the implementer role gives: a guard should be a data dependency
// rather than a neighbour. A hand-written add is an enumeration beside the
// struct, and a field added to the struct and forgotten in the add is invisible
// — the counter simply stays zero. This way the struct definition is the only
// enumeration there is.
var wireFields = sync.OnceValue(func() []int {
	t := reflect.TypeOf(WireCounters{})
	idx := make([]int, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Type.Kind() != reflect.Uint64 {
			panic("lease: WireCounters." + t.Field(i).Name + " is not a uint64; the accumulation cannot carry it")
		}
		idx = append(idx, i)
	}
	return idx
})

// statsIntoWire maps a manager Stats onto the wire half BY FIELD NAME.
//
// A name missing from Stats panics rather than reading zero: a counter that is
// silently always zero is the failure this whole file is written against, and
// the two types are in this package, so the panic is unreachable without an
// edit that means to break it.
func statsIntoWire(s Stats) WireCounters {
	var w WireCounters
	wv := reflect.ValueOf(&w).Elem()
	sv := reflect.ValueOf(s)
	wt := wv.Type()
	for _, i := range wireFields() {
		f := sv.FieldByName(wt.Field(i).Name)
		if !f.IsValid() {
			panic("lease: Stats has no field " + wt.Field(i).Name + ", so that counter would always read zero")
		}
		wv.Field(i).SetUint(f.Uint())
	}
	return w
}

// addDelta adds (now - base) to w, and reports the first field that went
// backwards.
func (w *WireCounters) addDelta(now, base WireCounters) (string, bool) {
	wv := reflect.ValueOf(w).Elem()
	nv := reflect.ValueOf(now)
	bv := reflect.ValueOf(base)
	for _, i := range wireFields() {
		n, b := nv.Field(i).Uint(), bv.Field(i).Uint()
		if n < b {
			return wv.Type().Field(i).Name, false
		}
		wv.Field(i).SetUint(wv.Field(i).Uint() + (n - b))
	}
	return "", true
}

// Fold applies one event to one record and is TOTAL over (phase, op): every
// pair either produces a record or a *Reject.
//
// A rejected event still returns a record — the input with Rejects bumped and
// LastReject set — because "the fold refused this" is a fact about the record
// that a caller reads later, and a count kept somewhere else is a count nobody
// joins back up. Nothing else about the record moves; the reject tests assert
// that rather than asserting the counter.
func Fold(rec Record, ev RecordEvent) (Record, error) {
	reject := func(reason RejectReason, note string) (Record, error) {
		rec.Counters.Rejects++
		rec.LastReject = reason
		return rec, &Reject{Reason: reason, Op: ev.Op, Phase: rec.Phase, ID: ev.ID, Seq: ev.Seq, Note: note}
	}

	creating := ev.Op == OpReserve || ev.Op == OpAdopt || (ev.Op == OpCreate && rec.Phase == PhaseUnset)

	switch {
	case ev.ID == "":
		return reject(RejectID, "the event names no record")
	case rec.Phase == PhaseUnset && !creating:
		return reject(RejectNoRecord, "only reserve, create and adopt bring a record into existence")
	case rec.Phase != PhaseUnset && ev.ID != rec.ID:
		return reject(RejectID, "the record is "+rec.ID)
	case rec.Phase != PhaseUnset && creating:
		return reject(RejectExists, "already in phase "+rec.Phase.String())
	case rec.Phase != PhaseUnset && ev.Scope != "" && ev.Scope != rec.Scope:
		return reject(RejectScope, "the record is in scope "+rec.Scope)
	}

	// Strictly increasing, per record, and it is NOT relaxed for an
	// unsequenced writer: a caller that leaves Seq at zero has every event
	// after the first rejected, loudly, rather than having two processes'
	// interleaved lines fold into one plausible record. The creating event
	// sets the floor.
	if !creating && ev.Seq <= rec.Seq {
		return reject(RejectSeq, fmt.Sprintf("seq %d does not advance past %d", ev.Seq, rec.Seq))
	}

	// Written once, and this refusal is the ONLY thing that makes it so: the
	// assignment below is unconditional, because a second guard there could
	// only ever restate this one and would be untestable while it held.
	// An identity that changes is the defect a record exists to make
	// impossible, so it is refused rather than quietly overwritten.
	if len(ev.Identity) > 0 && len(rec.Identity) > 0 && !bytesEqual(ev.Identity, rec.Identity) {
		return reject(RejectIdentity, "the identity is written once and is already set")
	}

	// The family, on the same rule and for the same reason. It was the one
	// write-once field of the three that was silently overwritable: a v6 event
	// on a v4 record left the record claiming v6 while the same event's scope
	// change was refused.
	if ev.Family != FamilyUnset && rec.Family != FamilyUnset && ev.Family != rec.Family {
		return reject(RejectFamily, "the record is in family "+rec.Family.String())
	}

	next := rec
	switch ev.Op {
	case OpReserve:
		next.Phase = PhaseReserved
	case OpCreate:
		if rec.Phase != PhaseUnset && rec.Phase != PhaseReserved {
			return reject(RejectPhase, "create follows a reservation, or nothing")
		}
		next.Phase = PhaseCreated
	case OpRebind:
		if rec.Phase != PhaseRetained {
			return reject(RejectPhase, "a re-bind consumes a tombstone")
		}
		next.Phase = PhaseCreated
		next.Deadline = time.Time{}
	case OpAdopt:
		next.Phase = PhaseAdopted
	case OpBind:
		if rec.Phase != PhaseCreated && rec.Phase != PhaseAdopted {
			return reject(RejectPhase, "a manager starts on a created or adopted record")
		}
		next.Phase = PhaseJoined
	case OpLease:
		if !holdsAnAddress(rec.Phase) {
			return reject(RejectPhase, "a manager event needs a record that owns an address")
		}
		var err error
		if next, err = foldLease(next, ev); err != nil {
			var rj *Reject
			if errors.As(err, &rj) {
				return reject(rj.Reason, rj.Note)
			}
			return reject(RejectPayload, err.Error())
		}
	case OpLost:
		if !holdsAnAddress(rec.Phase) {
			return reject(RejectPhase, "a manager event needs a record that owns an address")
		}
		next = foldLost(next, ev)
	case OpLeave:
		if rec.Phase != PhaseJoined {
			return reject(RejectPhase, "only a joined record can be left")
		}
		next.Phase = PhaseLeft
	case OpRetain:
		if !holdsAnAddress(rec.Phase) && rec.Phase != PhaseLeft {
			return reject(RejectPhase, "nothing to retain")
		}
		next.Phase = PhaseRetained
	case OpClose:
		next.Phase = PhaseClosed
	case OpStats:
		if ev.Stats == nil {
			return reject(RejectPayload, "OpStats with no Stats")
		}
		if ev.Manager == "" {
			return reject(RejectPayload, "OpStats naming no manager")
		}
		now := statsIntoWire(*ev.Stats)
		base := statsIntoWire(next.statsBase)
		if ev.Manager != next.statsManager {
			if next.statsSeen[ev.Manager] {
				return reject(RejectManager, "manager "+ev.Manager+" already ran and was replaced by "+next.statsManager)
			}
			base = WireCounters{}
		}
		if bad, ok := next.Counters.Wire.addDelta(now, base); !ok {
			return reject(RejectStats, "Stats."+bad+" went backwards inside manager "+ev.Manager)
		}
		next.statsBase, next.statsManager = *ev.Stats, ev.Manager
		if !next.statsSeen[ev.Manager] {
			// Copy on write: next shares the map with the input record until
			// something is added, and a fold that mutated its argument would
			// make Rebuild's answer depend on who else held the record.
			seen := make(map[string]bool, len(next.statsSeen)+1)
			for k := range next.statsSeen {
				seen[k] = true
			}
			seen[ev.Manager] = true
			next.statsSeen = seen
		}
	case OpExtra:
		if len(ev.Extra) == 0 {
			return reject(RejectPayload, "OpExtra with no counters")
		}
		if next.Extra == nil {
			next.Extra = make(map[string]uint64, len(ev.Extra))
		} else {
			next.Extra = cloneCounts(next.Extra)
		}
		for k, v := range ev.Extra {
			next.Extra[k] += v
		}
	default:
		return reject(RejectPhase, "no such operation")
	}

	next.ID = ev.ID
	next.Seq = ev.Seq
	next.Instance = ev.Instance
	if ev.Scope != "" {
		next.Scope = ev.Scope
	}
	if ev.Family != FamilyUnset {
		next.Family = ev.Family
	}
	if len(ev.CHAddr) > 0 {
		next.CHAddr = append([]byte(nil), ev.CHAddr...)
	}
	if len(ev.Identity) > 0 {
		next.Identity = append([]byte(nil), ev.Identity...)
	}
	if ev.Params != nil {
		p := SnapshotParams(*ev.Params)
		next.Params = &p
	}
	if ev.StepsRef != "" {
		next.StepsRef = ev.StepsRef
	}
	if !ev.Deadline.IsZero() {
		next.Deadline = ev.Deadline
	}
	if next.Phase != PhaseRetained && ev.Op != OpRetain {
		// A deadline outside RETAINED would be read by nothing and would
		// outlive the tombstone it belonged to.
		if ev.Op == OpRebind || ev.Op == OpCreate || ev.Op == OpBind {
			next.Deadline = time.Time{}
		}
	}
	return next, nil
}

// holdsAnAddress reports whether a record in this phase owns an address a
// manager can be running against.
func holdsAnAddress(p Phase) bool {
	switch p {
	case PhaseReserved, PhaseCreated, PhaseJoined, PhaseAdopted:
		return true
	default:
		return false
	}
}

func foldLease(rec Record, ev RecordEvent) (Record, error) {
	switch ev.Kind {
	case Acquired, Changed, Renewed:
		if ev.Lease == nil {
			return rec, &Reject{Reason: RejectPayload, Op: ev.Op, Phase: rec.Phase, ID: ev.ID, Seq: ev.Seq,
				Note: ev.Kind.String() + " with no lease"}
		}
		rec.Lease = CloneLease(*ev.Lease)
		rec.Held = true
		rec.ACD = ev.ACD
		switch ev.Kind {
		case Acquired:
			rec.Counters.Acquisitions++
		case Changed:
			rec.Counters.Changes++
		case Renewed:
			rec.Counters.Renewals++
		}
	case Failed:
		rec.Counters.Failures++
		switch ev.Reason {
		case proto.ReasonNak:
			// Counted HERE and not in the Lost arm. A DHCPNAK that costs a
			// held lease produces Lost and then Failed, both carrying
			// ReasonNak; counting both would report one refusal as two.
			// Failed is the event the manager's own contract says reports the
			// NAK counters.
			rec.Counters.Naks++
		case proto.ReasonNoServer:
			rec.Counters.Timeouts++
		case proto.ReasonConflict:
			// RFC 5227 section 2.1's check failing BEFORE the address was
			// used: nothing was acquired, so no Lost arrives and foldLost's
			// bump never runs. Counting it here is what makes
			// RecordCounters.Conflicts the whole population rather than the
			// half that happened to a lease already in service — and the two
			// arms cannot double-count, because ring 1 emits Failed with this
			// reason only when it holds no lease.
			rec.Counters.Conflicts++
		}
	case Lost:
		return rec, &Reject{Reason: RejectPayload, Op: ev.Op, Phase: rec.Phase, ID: ev.ID, Seq: ev.Seq,
			Note: "a lost lease is OpLost, whose stopped reason is not a loss"}
	default:
		return rec, &Reject{Reason: RejectPayload, Op: ev.Op, Phase: rec.Phase, ID: ev.ID, Seq: ev.Seq,
			Note: "no such event kind"}
	}
	return rec, nil
}

func foldLost(rec Record, ev RecordEvent) Record {
	// The phase at the loss, whatever the reason. After a conflict it is
	// ACDIdle — the sub-machine stops when it declines — and that is the fact
	// a resuming process needs: there is nothing in flight to resume.
	rec.ACD = ev.ACD
	if ev.Reason == proto.ReasonStopped {
		// P-7. Cancelling a manager makes ring 1 drop the lease with this
		// reason, so it arrives at the end of every ordinary shutdown and at
		// the end of every one-shot acquisition. The lease lives on in the
		// record and the next manager resumes it.
		rec.Counters.StoppedNotLost++
		return rec
	}
	rec.Counters.Losses++
	switch ev.Reason {
	case proto.ReasonConflict:
		rec.Counters.Conflicts++
	case proto.ReasonExpired:
		rec.Counters.Expiries++
	}
	rec.Lease, rec.Held = Lease{}, false
	return rec
}

// Resume is the hook INIT-REBOOT will pull on: the lease this record would ask
// the server to confirm, and whether asking is worth anything.
//
// M4 stores it; M5 sends it. The rule is RFC 2131 section 4.3.2's: a REQUEST
// naming an address the client believes it still holds is answered by a server
// with knowledge and ignored by one without, so it is worth sending only while
// the lease has not expired. A zero Expire is an infinite lease and always
// qualifies.
//
// RETAINED and CLOSED are excluded deliberately: a tombstone's address is left
// to expire on the server and is re-acquired through an exchange the server
// decides, not re-claimed.
func (r Record) Resume(now time.Time) (Lease, bool) {
	if !r.Held {
		return Lease{}, false
	}
	switch r.Phase {
	case PhaseReserved, PhaseCreated, PhaseJoined, PhaseLeft, PhaseAdopted:
	default:
		return Lease{}, false
	}
	if !r.Lease.Addr.IsValid() {
		return Lease{}, false
	}
	if r.Lease.Expire.IsZero() || r.Lease.Expire.After(now) {
		return CloneLease(r.Lease), true
	}
	return Lease{}, false
}

// Prefer is the address this record would ask for as a PREFERENCE rather than
// claim as a binding: option 50 in a DHCPDISCOVER, which RFC 2131 section
// 4.4.1 makes a MAY the server is free to ignore.
//
// It is the OTHER half of Resume and the two are disjoint by construction —
// Prefer refuses whatever Resume answers — so a caller writes the pair once
// and cannot get both:
//
//	if l, ok := rec.Resume(now); ok {
//		cfg.Resume = &l                  // INIT-REBOOT: a claim
//	} else if a, ok := rec.Prefer(now); ok {
//		cfg.Params.RequestedIP = a       // DISCOVER + option 50: a request
//	}
//
// THE RETAINED CASE IS WHY IT EXISTS. A tombstone's address was given up: the
// server is free to have handed it to somebody else, and section 4.3.2 has a
// server with no record of the client answer an INIT-REBOOT DHCPREQUEST with
// silence, so claiming it back costs a retransmission budget before the
// DHCPDISCOVER that should have gone first. Asking for it inside a
// DHCPDISCOVER costs nothing and usually works, because a server that still
// has the binding free will re-offer it (section 4.3.1).
//
// An EXPIRED lease in any other phase lands here for the same reason, which is
// what makes the pair total: a record holding an address either believes the
// lease is live, and claims it, or does not, and asks.
//
// PhaseUnset holds nothing and PhaseClosed is over; both answer false.
func (r Record) Prefer(now time.Time) (netip.Addr, bool) {
	if _, ok := r.Resume(now); ok {
		return netip.Addr{}, false
	}
	switch r.Phase {
	case PhaseUnset, PhaseClosed:
		return netip.Addr{}, false
	}
	if !r.Lease.Addr.IsValid() {
		return netip.Addr{}, false
	}
	addr := r.Lease.Addr.Addr()
	if !addr.Is4() || addr.IsUnspecified() {
		return netip.Addr{}, false
	}
	return addr, true
}

// Addr is the address the record holds, if it holds one.
func (r Record) Addr() (netip.Addr, bool) {
	if !r.Lease.Addr.IsValid() {
		return netip.Addr{}, false
	}
	return r.Lease.Addr.Addr(), true
}

// SnapshotParams deep-copies a Params so that a record says what was sent
// rather than what the caller's slice happens to hold now.
//
// Params is a value, but four of its fields are slices; a shallow copy leaves
// the record aliasing the caller's memory, and a replay then replays a
// configuration that never ran.
func SnapshotParams(p proto.Params) proto.Params {
	p.CHAddr = append([]byte(nil), p.CHAddr...)
	p.ClientID = append([]byte(nil), p.ClientID...)
	p.ParameterList = append([]wire.OptionCode(nil), p.ParameterList...)
	p.Servers.Allow = append([]netip.Addr(nil), p.Servers.Allow...)
	p.Servers.Deny = append([]netip.Addr(nil), p.Servers.Deny...)
	// Resume is the one POINTER in Params, so a shallow copy leaves the record
	// and the caller sharing one struct: the caller can change what the
	// snapshot says was sent, after it was sent. The four slices above are the
	// same defect in the shape Go makes easier to spot.
	p.Resume = p.Resume.Clone()
	return p
}

// CloneLease deep-copies a Lease for the same reason SnapshotParams exists.
func CloneLease(l Lease) Lease {
	l.DNS = append([]netip.Addr(nil), l.DNS...)
	l.Routes = append([]wire.Route(nil), l.Routes...)
	l.DomainSearch = append([]string(nil), l.DomainSearch...)
	l.Options = l.Options.Clone()
	return l
}

func cloneCounts(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
