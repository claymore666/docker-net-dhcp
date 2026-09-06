package proto

import (
	"fmt"

	"github.com/claymore666/dhcp-golib/wire"
)

// JournalEntry6 is one Machine6 Step, recorded.
//
// IT IS A SECOND TYPE BESIDE JournalEntry FOR State6's REASON: the recorded
// From and To are the whole point of a journal entry — "a replay that diverges
// is the finding, and without the recorded states there is nothing to diverge
// from" — and State and State6 are two enumerations. One entry type carrying
// both pairs would record two states per Step of which two are always zero,
// and a replay checking the wrong pair would agree with itself forever.
//
// Everything else is the same shape and the same rules, which is D30: Raw is
// the bytes an EvReceived was decoded from and is re-decoded on replay, RA is
// the same for a Router Advertisement, and Actions is text because an Action
// holds pointers into decoded messages.
type JournalEntry6 struct {
	Seq  uint64
	Now  Instant
	Rnd  uint64
	Kind EventKind

	Raw    []byte
	RA     []byte
	DAD    DADOutcome
	Timer  TimerID
	Action ActionID
	Reason string

	From State6
	To   State6

	Actions []string

	// Note marks an entry that records something ring 2 did rather than a
	// Step of the machine.
	//
	// IT EXISTS BECAUSE THE JOURNAL IS REPLAYED. RFC 9915 §14.1's refused
	// send is the case that forced it: the refusal happens in ring 2, between
	// two Steps, and it MUST be recorded where the exchange is recorded —
	// "journalled with the count, not dropped silently" — but it is not a
	// transition, has no event to feed and carries no From or To. Replayed as
	// if it were one, it diverges at the next entry and reports the machine as
	// broken when what actually happened is that the journal mixed two kinds
	// of line.
	//
	// Replay6 SKIPS these and steps over them, so a journal with notes in it
	// replays to the same states as the same journal without them. That is
	// the property TestTheManagersV6JournalReplays checks by replaying a
	// journal that contains one.
	Note bool
}

// NewJournalEntry6 builds the entry for one Machine6 Step, so the v6 journal's
// shape is defined once, for NewJournalEntry's reason: a test recorder that
// built entries its own way would be a probe derived differently from its
// subject.
func NewJournalEntry6(seq uint64, now Instant, rnd uint64, ev Event, from, to State6, acts []Action) JournalEntry6 {
	return JournalEntry6{
		Seq: seq, Now: now, Rnd: rnd, Kind: ev.Kind,
		Raw: ev.Raw, RA: ev.RARaw, DAD: ev.DAD,
		Timer: ev.Timer, Action: ev.Action, Reason: ev.Reason,
		From: from, To: to, Actions: RenderActions(acts),
	}
}

// Event reconstructs the Step input this entry records, re-decoding a received
// message through wire.DecodeV6.
func (e JournalEntry6) Event() (Event, error) {
	if ev, done, err := replayEvent(e.Seq, e.Kind, e.RA, e.DAD, e.Timer, e.Action, e.Reason); done {
		return ev, err
	}
	msg, err := wire.DecodeV6(e.Raw)
	if err != nil {
		return Event{}, fmt.Errorf("entry %d: %w", e.Seq, err)
	}
	return ReceivedV6(msg, e.Raw), nil
}

// ReplayResult6 is what a v6 replay produced.
type ReplayResult6 struct {
	State State6
	Lease Lease6
	Held  bool
	// Steps is how many entries were REPLAYED, which is the journal's length
	// minus the ring-2 notes JournalEntry6.Note marks. A caller comparing it
	// to len(entries) is asking "did the whole journal replay?" and the notes
	// are the answer's one legitimate difference.
	Steps int
}

// Replay6 re-runs a recorded v6 exchange through a fresh Machine6 and checks
// that it reproduces the recorded transitions exactly.
//
// Replay's counterpart, with Replay's guarantee and Replay's reason for
// existing: replaying a captured exchange offline with no network and no root
// is the support workflow the design document (§4.3) says dhcpcd structurally
// cannot give us.
func Replay6(p Params6, entries []JournalEntry6) (ReplayResult6, error) {
	m, err := New6(p)
	if err != nil {
		return ReplayResult6{}, err
	}
	steps := 0
	for _, e := range entries {
		if e.Note {
			// Ring 2's own line, not a Step. Skipped rather than refused: a
			// journal is a debugging artefact and refusing to replay one
			// because it also recorded a rate-limit refusal would take the
			// tool away exactly when the refusal is what you are looking at.
			continue
		}
		if m.State() != e.From {
			return ReplayResult6{}, Divergence{
				Seq: e.Seq, Field: "from-state",
				Recorded: e.From.String(), Replayed: m.State().String(),
			}
		}
		ev, err := e.Event()
		if err != nil {
			return ReplayResult6{}, err
		}
		to, acts := m.Step(e.Now, e.Rnd, ev)
		if to != e.To {
			return ReplayResult6{}, Divergence{
				Seq: e.Seq, Field: "to-state",
				Recorded: e.To.String(), Replayed: to.String(),
			}
		}
		got := RenderActions(acts)
		if len(got) != len(e.Actions) {
			return ReplayResult6{}, Divergence{
				Seq: e.Seq, Field: "action count",
				Recorded: fmt.Sprint(len(e.Actions)), Replayed: fmt.Sprint(len(got)),
			}
		}
		for i := range got {
			if got[i] != e.Actions[i] {
				return ReplayResult6{}, Divergence{
					Seq: e.Seq, Field: fmt.Sprintf("action %d", i),
					Recorded: e.Actions[i], Replayed: got[i],
				}
			}
		}
		steps++
	}
	l, held := m.Lease()
	return ReplayResult6{State: m.State(), Lease: l, Held: held, Steps: steps}, nil
}
