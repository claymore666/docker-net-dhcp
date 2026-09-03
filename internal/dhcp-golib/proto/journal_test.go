package proto

import (
	"errors"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// recordedRun drives a machine and journals every Step exactly the way the
// manager does — through NewJournalEntry, which is the point: the recorder in
// a test and the recorder in production are the same function, so a replay
// that works here works there.
type recordedRun struct {
	m       *Machine
	entries []JournalEntry
	seq     uint64
}

func newRun(t *testing.T, p Params) *recordedRun {
	t.Helper()
	return &recordedRun{m: newMachine(t, p)}
}

func (r *recordedRun) step(now Instant, rnd uint64, ev Event) []Action {
	from := r.m.State()
	to, acts := r.m.Step(now, rnd, ev)
	r.entries = append(r.entries, NewJournalEntry(r.seq, now, rnd, ev, from, to, acts))
	r.seq++
	return acts
}

// acquire drives a full INIT-to-BOUND exchange and returns the run.
func acquire(t *testing.T) *recordedRun {
	t.Helper()
	r := newRun(t, testParams())
	acts := r.step(0, 0x1111, Simple(EvStart))
	disc := mustSend(t, acts, wire.MsgDiscover)

	// A retransmission in the middle, so the replay has to reproduce a timer
	// fire and a second send, not just the three-message happy path.
	acts = r.step(at(5), 0x2222, TimerFired(TimerRetransmit))
	mustSend(t, acts, wire.MsgDiscover)

	// A message for somebody else's transaction, which must be discarded
	// identically on replay.
	stray := offerFor(disc, "192.168.99.77", "192.168.99.1")
	stray.XID = disc.XID ^ 0x5A5A
	r.step(at(6), 0x3333, received(t, stray))

	acts = r.step(at(7), 0x4444, received(t, offerFor(disc, "192.168.99.50", "192.168.99.1")))
	req := mustSend(t, acts, wire.MsgRequest)
	sent, _ := find(acts, ActSend)

	// A failed send, so R2's path is in the recording too.
	r.step(at(8), 0x5555, ActionFailed(sent.ID, "ENETDOWN"))

	acts = r.step(at(9), 0x6666, received(t, ackFor(req, "192.168.99.50", "192.168.99.1", 3600)))
	if _, ok := find(acts, ActLeaseAcquired); !ok {
		t.Fatalf("fixture did not acquire a lease: %v", RenderActions(acts))
	}
	return r
}

// TestReplayReproducesTheLease is done-condition (b) at the unit level: the
// recorded exchange, replayed offline through ring 1, produces the identical
// lease. The ring-3 test replays a REAL server's exchange through the same
// function.
func TestReplayReproducesTheLease(t *testing.T) {
	r := acquire(t)
	live, held := r.m.Lease()
	if !held {
		t.Fatal("the live machine holds no lease")
	}

	res, err := Replay(testParams(), r.entries)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Steps != len(r.entries) {
		t.Fatalf("replayed %d steps, recorded %d", res.Steps, len(r.entries))
	}
	if res.State != r.m.State() {
		t.Fatalf("replay ended in %s, the live machine is in %s", res.State, r.m.State())
	}
	if !res.Held {
		t.Fatal("replay produced no lease")
	}
	if !res.Lease.Equal(live) {
		t.Fatalf("replayed lease %s != live lease %s", res.Lease, live)
	}
	// Equal deliberately ignores Start and Options, so those are checked
	// separately — otherwise a replay that got the lease clock wrong would
	// pass, and the lease clock is the field RFC 2131 section 4.4.5 is most
	// particular about.
	if res.Lease.Start != live.Start {
		t.Fatalf("replayed lease clock starts at %s, live at %s", res.Lease.Start, live.Start)
	}
	if len(res.Lease.Options) != len(live.Options) {
		t.Fatalf("replayed lease carries %d options, live %d", len(res.Lease.Options), len(live.Options))
	}
}

// The four mutations below are the ABSENCE drive for the test above: each
// breaks one thing the replay checks, and each must be caught. A replay that
// cannot fail is not a check, and "it replayed cleanly" would then be a
// sentence with no content.
func TestReplayDetectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*testing.T, []JournalEntry)
		field  string
	}{
		{
			"a different rnd",
			func(t *testing.T, es []JournalEntry) { es[0].Rnd ^= 0xFFFF },
			"",
		},
		{
			"a different timestamp",
			func(t *testing.T, es []JournalEntry) { es[len(es)-1].Now = at(100000) },
			"",
		},
		{
			"a recorded action that never happened",
			func(t *testing.T, es []JournalEntry) {
				es[0].Actions = append(es[0].Actions, "Journal invented")
			},
			"action count",
		},
		{
			"a rewritten action",
			func(t *testing.T, es []JournalEntry) { es[0].Actions[0] = "Send something else" },
			"action 0",
		},
		{
			"a rewritten to-state",
			func(t *testing.T, es []JournalEntry) { es[0].To = StateBound },
			"to-state",
		},
		{
			"a corrupted packet",
			func(t *testing.T, es []JournalEntry) {
				for i := range es {
					if es[i].Kind == EvReceived {
						es[i].Raw[236] ^= 0xFF // the magic cookie
						return
					}
				}
				t.Fatal("no received entry in the journal to corrupt")
			},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := acquire(t)
			// Deep-copy the entries so one case cannot corrupt another's.
			es := make([]JournalEntry, len(r.entries))
			for i, e := range r.entries {
				es[i] = e
				es[i].Raw = append([]byte(nil), e.Raw...)
				es[i].Actions = append([]string(nil), e.Actions...)
			}
			tc.break_(t, es)

			_, err := Replay(testParams(), es)
			if err == nil {
				t.Fatalf("replay accepted a journal with %s", tc.name)
			}
			if tc.field != "" {
				var d Divergence
				if !errors.As(err, &d) {
					t.Fatalf("error %v is not a Divergence", err)
				}
				if d.Field != tc.field {
					t.Fatalf("divergence reported field %q, want %q", d.Field, tc.field)
				}
			}
		})
	}
}

// TestReplayPreservesACleanJournal is the preservation control beside the
// tampering table: the same copying and re-running, WITHOUT a mutation, must
// still replay clean. Without it, a Replay that rejected everything would pass
// every case above.
func TestReplayPreservesACleanJournal(t *testing.T) {
	r := acquire(t)
	es := make([]JournalEntry, len(r.entries))
	for i, e := range r.entries {
		es[i] = e
		es[i].Raw = append([]byte(nil), e.Raw...)
		es[i].Actions = append([]string(nil), e.Actions...)
	}
	if _, err := Replay(testParams(), es); err != nil {
		t.Fatalf("an untampered copy of the journal failed to replay: %v", err)
	}
}

// TestReplayNeedsTheSameParams pins a real bound rather than a behaviour: the
// journal records the STEPS, not the configuration, so replaying under
// different parameters is not expected to work and must not silently appear
// to. Anyone reading a replay must know it is only as good as the params they
// hand it.
func TestReplayNeedsTheSameParams(t *testing.T) {
	r := acquire(t)
	other := testParams()
	other.Hostname = "somethingelse"
	if _, err := Replay(other, r.entries); err == nil {
		t.Fatal("replay under different params succeeded; the bound in the doc comment is not real")
	}
}

func TestReplayRejectsAWrongStartState(t *testing.T) {
	// A journal whose first entry does not start from a fresh machine cannot
	// be replayed — this is what a WRAPPED ring journal produces, which is why
	// runtime.Journal counts what it dropped.
	r := acquire(t)
	if _, err := Replay(testParams(), r.entries[2:]); err == nil {
		t.Fatal("replay accepted a journal with its beginning missing")
	}
}

func TestJournalEntryEventRoundTrips(t *testing.T) {
	// Every event kind must survive the journal. A kind that does not
	// reconstruct replays as something else, silently.
	msg := &wire.Message{Op: wire.BootReply, HType: wire.HTypeEthernet,
		CHAddr: testCHAddr, Options: wire.Options{wire.OptMessageType: {byte(wire.MsgAck)}}}
	raw, err := wire.Encode(msg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	for _, k := range AllEventKinds() {
		t.Run(k.String(), func(t *testing.T) {
			var ev Event
			switch k {
			case EvReceived:
				ev = Received(msg, raw)
			case EvTimerFired:
				ev = TimerFired(TimerExpire)
			case EvActionFailed:
				ev = ActionFailed(ActionID(42), "ENETDOWN")
			default:
				ev = Simple(k)
			}
			e := NewJournalEntry(0, at(1), 7, ev, StateInit, StateSelecting, nil)
			back, err := e.Event()
			if err != nil {
				t.Fatalf("Event: %v", err)
			}
			if back.Kind != ev.Kind {
				t.Fatalf("kind %s round-tripped to %s", ev.Kind, back.Kind)
			}
			switch k {
			case EvTimerFired:
				if back.Timer != ev.Timer {
					t.Fatalf("timer %s round-tripped to %s", ev.Timer, back.Timer)
				}
			case EvActionFailed:
				if back.Action != ev.Action || back.Reason != ev.Reason {
					t.Fatalf("failure %v/%q round-tripped to %v/%q", ev.Action, ev.Reason, back.Action, back.Reason)
				}
			case EvReceived:
				if back.Msg == nil {
					t.Fatal("received event round-tripped to a nil message")
				}
				if got, _ := back.Msg.Type(); got != wire.MsgAck {
					t.Fatalf("message type %s after the round trip", got)
				}
			}
		})
	}
}

func TestJournalEntryReportsAnUndecodablePacket(t *testing.T) {
	// A Raw that will not decode must be an error, not a nil message quietly
	// replayed as "nothing arrived".
	e := JournalEntry{Kind: EvReceived, Raw: []byte{1, 2, 3}}
	if _, err := e.Event(); err == nil {
		t.Fatal("an undecodable Raw reconstructed without error")
	}
}
