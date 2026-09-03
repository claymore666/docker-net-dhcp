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
		Raw: ev.Raw, Timer: ev.Timer, Action: ev.Action, Reason: ev.Reason,
		From: from, To: to, Actions: RenderActions(acts),
	}
}

// Event reconstructs the Step input this entry records.
//
// A Received entry is re-DECODED here, so a corrupt or unparseable Raw is
// reported rather than silently replayed as a nil message.
func (e JournalEntry) Event() (Event, error) {
	switch e.Kind {
	case EvReceived:
		msg, err := wire.Decode(e.Raw)
		if err != nil {
			return Event{}, fmt.Errorf("entry %d: %w", e.Seq, err)
		}
		return Received(msg, e.Raw), nil
	case EvTimerFired:
		return TimerFired(e.Timer), nil
	case EvActionFailed:
		return ActionFailed(e.Action, e.Reason), nil
	default:
		return Simple(e.Kind), nil
	}
}

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
