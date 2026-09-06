package proto

import (
	"errors"
	"fmt"

	"github.com/claymore666/dhcp-golib/wire"
)

// JournalEntry is one Step, recorded.
//
// It carries what Step consumed — now, rnd and the event — and nothing else,
// which is exactly what a replay needs (design document section 2.2).
//
// Raw carries the bytes an EvReceived was decoded FROM, not the decoded
// message, and that is what makes Replay worth running: replaying from a
// decoded struct re-runs ring 1 against a decode that already happened and
// would agree with itself even if the codec were wrong.
type JournalEntry struct {
	Seq  uint64
	Now  Instant
	Rnd  uint64
	Kind EventKind

	// Raw is the wire bytes for EvReceived.
	Raw []byte
	// RA is the ICMPv6 bytes for EvRouterAdvert, re-decoded on replay for the
	// reason Raw is: replaying from a decoded struct re-runs ring 1 against a
	// decode that already happened.
	//
	// A SEPARATE FIELD FROM Raw AND NOT A SECOND USE OF IT. The two carry
	// different protocols with different decoders, and one field would make
	// "which decoder does this entry want" a question answered from Kind in
	// two places instead of one — the shape that let the RA payload be dropped
	// in the first place (M7a carried row 2).
	RA []byte
	// DAD is the outcome for EvDADResult. It is a VALUE and not bytes because
	// there are no bytes: EvDADResult is ring 3 reporting a verdict it reached
	// from frames this ring never saw, so there is nothing to re-decode and
	// the address-and-bool IS the event.
	DAD DADOutcome
	// Timer is the fired timer for EvTimerFired.
	Timer TimerID
	// Action and Reason describe an EvActionFailed.
	Action ActionID
	Reason string

	// From and To are the states either side of the Step. They are recorded
	// so a replay can be CHECKED rather than merely re-run: a replay that
	// diverges is the finding, and without the recorded states there is
	// nothing to diverge from.
	From State
	To   State

	// Actions is what the Step emitted, rendered. Kept as text rather than as
	// []Action because an Action holds a *wire.Message, and a journal that
	// holds pointers into decoded messages is a journal whose size is not the
	// bound R3 claims.
	Actions []string
}

// NewJournalEntry builds the entry for one Step, so the journal's shape is
// defined ONCE. The manager records steps and so do the tests; a test recorder
// that built entries its own way would be a probe derived differently from its
// subject, and could replay perfectly while the manager's journal replayed not
// at all.
func NewJournalEntry(seq uint64, now Instant, rnd uint64, ev Event, from, to State, acts []Action) JournalEntry {
	return JournalEntry{
		Seq: seq, Now: now, Rnd: rnd, Kind: ev.Kind,
		Raw: ev.Raw, RA: ev.RARaw, DAD: ev.DAD,
		Timer: ev.Timer, Action: ev.Action, Reason: ev.Reason,
		From: from, To: to, Actions: RenderActions(acts),
	}
}

// Event reconstructs the Step input this entry records.
//
// A Received entry is re-DECODED here, so a corrupt or unparseable Raw is
// reported rather than silently replayed as a nil message.
func (e JournalEntry) Event() (Event, error) {
	if ev, done, err := replayEvent(e.Seq, e.Kind, e.RA, e.DAD, e.Timer, e.Action, e.Reason); done {
		return ev, err
	}
	msg, err := wire.Decode(e.Raw)
	if err != nil {
		return Event{}, fmt.Errorf("entry %d: %w", e.Seq, err)
	}
	return Received(msg, e.Raw), nil
}

// replayEvent reconstructs every event kind whose payload does not depend on
// which family's codec decodes it, and reports whether it did.
//
// ONE COPY, READ BY BOTH JournalEntry.Event AND JournalEntry6.Event. The two
// entry types differ in exactly one arm — a v4 packet is decoded by wire.Decode
// and a v6 one by wire.DecodeV6 — and the rest of the reconstruction is the
// same rules. Written twice, the second copy is where the next payload gets
// dropped, which is the defect M7a's carried row 2 recorded: the default arm
// reconstructed EvRouterAdvert and EvDADResult as bare kinds, and the test
// that was supposed to catch it built them as bare kinds too.
func replayEvent(seq uint64, kind EventKind, ra []byte, dad DADOutcome, timer TimerID, action ActionID, reason string) (Event, bool, error) {
	switch kind {
	case EvReceived:
		return Event{}, false, nil
	case EvTimerFired:
		return TimerFired(timer), true, nil
	case EvActionFailed:
		return ActionFailed(action, reason), true, nil
	case EvRouterAdvert:
		if len(ra) == 0 {
			// A recorded Router Advertisement with no bytes is an event whose
			// payload the recorder dropped, and replaying it as a bare kind is
			// exactly the silent divergence this arm exists to stop: the M and
			// O flags decide whether the machine switches to
			// Information-request, so a nil advertisement replays a different
			// client. Reported rather than reconstructed.
			return Event{}, true, fmt.Errorf("entry %d: %w", seq, ErrJournalNoRA)
		}
		adv, err := wire.DecodeRouterAdvert(ra)
		if err != nil {
			return Event{}, true, fmt.Errorf("entry %d: %w", seq, err)
		}
		return RouterAdvertRaw(adv, ra), true, nil
	case EvDADResult:
		return DADResult(dad.Addr, dad.Duplicate), true, nil
	default:
		return Simple(kind), true, nil
	}
}

// ErrJournalNoRA is a recorded EvRouterAdvert whose bytes are missing.
var ErrJournalNoRA = errors.New("proto: journal entry records a Router Advertisement with no bytes to re-decode")

// RenderActions turns an action list into the strings a JournalEntry stores.
func RenderActions(as []Action) []string {
	if len(as) == 0 {
		return nil
	}
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.String())
	}
	return out
}

// ErrReplayDiverged is returned when a replayed Step does not reproduce the
// recorded transition.
var ErrReplayDiverged = errors.New("proto: replay diverged from the journal")

// Divergence describes where a replay stopped agreeing with the journal.
type Divergence struct {
	Seq         uint64
	Field       string
	Recorded    string
	Replayed    string
	Description string
}

func (d Divergence) Error() string {
	return fmt.Sprintf("%s at entry %d: %s recorded %q, replay produced %q",
		ErrReplayDiverged.Error(), d.Seq, d.Field, d.Recorded, d.Replayed)
}

// Unwrap makes errors.Is(err, ErrReplayDiverged) work on a Divergence.
func (d Divergence) Unwrap() error { return ErrReplayDiverged }

// ReplayResult is what a replay produced.
type ReplayResult struct {
	State State
	Lease Lease
	Held  bool
	Steps int
}

// Replay re-runs a recorded exchange through a fresh Machine and checks that
// it reproduces the recorded transitions exactly.
//
// The public entry point requirement T4 asks for, not a test-only hook:
// replaying a captured exchange offline with no network and no root is the
// support workflow the design document (section 4.3) says dhcpcd structurally
// cannot give us.
//
// Exact rather than approximate because ring 1 is pure: now and rnd came in as
// parameters and are recorded, so the replayed machine sees the same inputs in
// the same order and has nothing else to consult.
func Replay(p Params, entries []JournalEntry) (ReplayResult, error) {
	m, err := New(p)
	if err != nil {
		return ReplayResult{}, err
	}
	for _, e := range entries {
		if m.State() != e.From {
			return ReplayResult{}, Divergence{
				Seq: e.Seq, Field: "from-state",
				Recorded: e.From.String(), Replayed: m.State().String(),
			}
		}
		ev, err := e.Event()
		if err != nil {
			return ReplayResult{}, err
		}
		to, acts := m.Step(e.Now, e.Rnd, ev)
		if to != e.To {
			return ReplayResult{}, Divergence{
				Seq: e.Seq, Field: "to-state",
				Recorded: e.To.String(), Replayed: to.String(),
			}
		}
		got := RenderActions(acts)
		if len(got) != len(e.Actions) {
			return ReplayResult{}, Divergence{
				Seq: e.Seq, Field: "action count",
				Recorded: fmt.Sprint(len(e.Actions)), Replayed: fmt.Sprint(len(got)),
			}
		}
		for i := range got {
			if got[i] != e.Actions[i] {
				return ReplayResult{}, Divergence{
					Seq: e.Seq, Field: fmt.Sprintf("action %d", i),
					Recorded: e.Actions[i], Replayed: got[i],
				}
			}
		}
	}
	l, held := m.Lease()
	return ReplayResult{State: m.State(), Lease: l, Held: held, Steps: len(entries)}, nil
}
