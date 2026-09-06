package proto

import (
	"strings"
	"testing"
)

// record6 drives a Machine6 through a script, recording a journal as it goes.
type record6 struct {
	m       *Machine6
	entries []JournalEntry6
	seq     uint64
}

func newRecord6(t *testing.T, p Params6) *record6 {
	t.Helper()
	return &record6{m: newMachine6(t, p)}
}

func (r *record6) step(now Instant, rnd uint64, ev Event) (State6, []Action) {
	from := r.m.State()
	to, acts := r.m.Step(now, rnd, ev)
	r.entries = append(r.entries, NewJournalEntry6(r.seq, now, rnd, ev, from, to, acts))
	r.seq++
	return to, acts
}

// TestReplay6ReproducesARecordedExchange is the v6 half of the support
// workflow design §4.3 names: a captured exchange replayed offline, with no
// network and no root, reaching the same state through the same actions.
//
// THE EXCHANGE RECORDED IS THE ONE DRIVEN BY THE CAPTURED dnsmasq BYTES, so
// what the replay reproduces is a real server's answers and not a script this
// package wrote for itself.
func TestReplay6ReproducesARecordedExchange(t *testing.T) {
	p := testParams6()
	r := newRecord6(t, p)

	r.step(at(0), 0, Simple(EvStart))
	r.step(at(1), capXIDSolicit, TimerFired(Timer6Delay))
	r.step(at(2), capXIDRequest, receivedCaptured(t, capAdvertise6))
	r.step(at(3), 0, receivedCaptured(t, capReply6))
	if s, _ := r.step(at(4), 0, DADResult(addr6(dnsmasqLeasedAddr), false)); s != State6Bound {
		t.Fatalf("the recorded run did not bind: %s", s)
	}

	res, err := Replay6(p, r.entries)
	if err != nil {
		t.Fatalf("Replay6: %v", err)
	}
	if res.State != State6Bound {
		t.Errorf("the replay ended in %s, want %s", res.State, State6Bound)
	}
	if !res.Held || len(res.Lease.Addrs) != 1 || res.Lease.Addrs[0].Addr.String() != dnsmasqLeasedAddr {
		t.Errorf("the replay holds %v, want %s", res.Lease.Addrs, dnsmasqLeasedAddr)
	}
	if res.Steps != len(r.entries) {
		t.Errorf("the replay ran %d steps, the recording has %d", res.Steps, len(r.entries))
	}
}

// TestReplay6ReportsADivergence is the guarantee the replay exists to give:
// a recording that the current code no longer reproduces is a FINDING and not
// a quietly different answer.
//
// The mutation below is the smallest one that changes nothing visible in the
// final state — a different rnd on the step that draws the transaction id —
// and it must still be caught, because the actions diverge even though the
// machine ends up bound either way.
func TestReplay6ReportsADivergence(t *testing.T) {
	p := testParams6()
	r := newRecord6(t, p)
	r.step(at(0), 0, Simple(EvStart))
	r.step(at(1), capXIDSolicit, TimerFired(Timer6Delay))

	t.Run("a changed rnd", func(t *testing.T) {
		mutated := append([]JournalEntry6(nil), r.entries...)
		mutated[1].Rnd = capXIDSolicit ^ 0xff
		_, err := Replay6(p, mutated)
		var d Divergence
		if !asDivergence(err, &d) {
			t.Fatalf("Replay6 = %v, want a Divergence", err)
		}
		if !strings.HasPrefix(d.Field, "action") {
			t.Errorf("the divergence is on %q; a different transaction id changes the Solicit that goes out", d.Field)
		}
	})

	t.Run("a changed to-state", func(t *testing.T) {
		mutated := append([]JournalEntry6(nil), r.entries...)
		mutated[1].To = State6Bound
		_, err := Replay6(p, mutated)
		var d Divergence
		if !asDivergence(err, &d) {
			t.Fatalf("Replay6 = %v, want a Divergence", err)
		}
		if d.Field != "to-state" {
			t.Errorf("the divergence is on %q, want to-state", d.Field)
		}
		if d.Recorded != State6Bound.String() || d.Replayed != State6Selecting.String() {
			t.Errorf("Divergence{Recorded: %q, Replayed: %q}, want %q and %q",
				d.Recorded, d.Replayed, State6Bound, State6Selecting)
		}
	})

	t.Run("a changed from-state", func(t *testing.T) {
		mutated := append([]JournalEntry6(nil), r.entries...)
		mutated[1].From = State6Renewing
		_, err := Replay6(p, mutated)
		var d Divergence
		if !asDivergence(err, &d) {
			t.Fatalf("Replay6 = %v, want a Divergence", err)
		}
		if d.Field != "from-state" {
			t.Errorf("the divergence is on %q, want from-state", d.Field)
		}
	})
}

// TestReplay6ReportsUndecodableBytes is the other failure a recording can
// carry: the octets were truncated on their way into the record.
func TestReplay6ReportsUndecodableBytes(t *testing.T) {
	p := testParams6()
	r := newRecord6(t, p)
	r.step(at(0), 0, Simple(EvStart))
	r.step(at(1), capXIDSolicit, TimerFired(Timer6Delay))
	r.step(at(2), capXIDRequest, receivedCaptured(t, capAdvertise6))

	mutated := append([]JournalEntry6(nil), r.entries...)
	mutated[2].Raw = mutated[2].Raw[:2]
	if _, err := Replay6(p, mutated); err == nil {
		t.Fatal("Replay6 accepted a recording whose message bytes cannot be decoded")
	}
}

func asDivergence(err error, out *Divergence) bool {
	d, ok := err.(Divergence)
	if ok {
		*out = d
	}
	return ok
}

// TestReplay6StepsOverARing2Note is the JournalEntry6.Note contract.
//
// RFC 9915 §14.1's refused send is recorded in the journal with the count
// rather than dropped silently — and it happens between
// two Steps, in ring 2, with no event and no transition. THE MUTANT THIS KILLS
// is a Replay6 that treats such a line as a Step: it reads From as the zero
// State6, finds the machine somewhere else, and reports a divergence that says
// the machine is broken when what actually happened is that the journal
// carried two kinds of line.
//
// The control is the same journal without the note, which must replay to the
// same place: a skip that also dropped a real entry would pass the first half
// of this test and fail the comparison.
func TestReplay6StepsOverARing2Note(t *testing.T) {
	p := testParams6()
	r := newRecord6(t, p)
	r.step(at(0), 1, Simple(EvStart))
	r.step(at(1), 2, TimerFired(Timer6Delay))

	clean := r.entries
	if len(clean) < 2 {
		t.Fatalf("the recorder captured %d entries, want at least 2", len(clean))
	}

	withNote := []JournalEntry6{clean[0], {
		Kind:   EvActionFailed,
		Reason: "RFC 9915 §14.1 rate limit: SOLICIT refused, 1 refused on this interface so far",
		Note:   true,
	}}
	withNote = append(withNote, clean[1:]...)

	got, err := Replay6(p, withNote)
	if err != nil {
		t.Fatalf("a journal carrying one ring-2 note did not replay: %v", err)
	}
	want, err := Replay6(p, clean)
	if err != nil {
		t.Fatalf("the same journal without the note did not replay: %v", err)
	}
	if got.State != want.State {
		t.Errorf("with the note the replay ended in %s, without it in %s", got.State, want.State)
	}
	if got.Steps != want.Steps {
		t.Errorf("with the note %d steps were replayed, without it %d: the note was counted as a Step", got.Steps, want.Steps)
	}
	if got.Steps != len(clean) {
		t.Errorf("replayed %d of %d real entries", got.Steps, len(clean))
	}
}
