package lease

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// recordAt is a Record in a phase, built the only way a record is ever built:
// by folding the events that get it there. Constructing one by assignment
// would let a test assert about a state the fold cannot reach.
func recordAt(t *testing.T, phase Phase) Record {
	t.Helper()
	var (
		rec Record
		seq uint64
	)
	apply := func(ev RecordEvent) {
		t.Helper()
		seq++
		ev.ID, ev.Seq = "rec-1", seq
		next, err := Fold(rec, ev)
		if err != nil {
			t.Fatalf("building a %s record: %v", phase, err)
		}
		rec = next
	}
	switch phase {
	case PhaseUnset:
		return Record{}
	case PhaseReserved:
		apply(RecordEvent{Op: OpReserve, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})
	case PhaseCreated:
		apply(RecordEvent{Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})
	case PhaseAdopted:
		apply(RecordEvent{Op: OpAdopt, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})
	case PhaseJoined:
		apply(RecordEvent{Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})
		apply(RecordEvent{Op: OpBind})
	case PhaseLeft:
		apply(RecordEvent{Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})
		apply(RecordEvent{Op: OpBind})
		apply(RecordEvent{Op: OpLeave})
	case PhaseRetained:
		apply(RecordEvent{Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})
		apply(RecordEvent{Op: OpBind})
		apply(RecordEvent{Op: OpLeave})
		apply(RecordEvent{Op: OpRetain, Deadline: testNow.Add(time.Hour)})
	case PhaseClosed:
		apply(RecordEvent{Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})
		apply(RecordEvent{Op: OpClose})
	default:
		t.Fatalf("recordAt has no construction for %s: a phase was added and this helper did not move", phase)
	}
	if rec.Phase != phase {
		t.Fatalf("recordAt(%s) built a %s record", phase, rec.Phase)
	}
	return rec
}

var (
	testMAC      = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	testMAC2     = []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x02}
	testIdentity = []byte{0xff, 0xde, 0xad, 0xbe, 0xef}
	testNow      = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
)

func testRecordLease(addr string) Lease {
	return Lease{
		Addr:     netip.MustParsePrefix(addr),
		Gateway:  netip.MustParseAddr("192.168.99.1"),
		DNS:      []netip.Addr{netip.MustParseAddr("192.168.99.1")},
		ServerID: netip.MustParseAddr("192.168.99.1"),
		Acquired: testNow,
		Expire:   testNow.Add(2 * time.Minute),
		Options:  wire.Options{wire.OptionCode(53): []byte{5}},
	}
}

// TestTheFoldIsTotalOverEveryPhaseAndOperation is D17's totality row.
//
// The domain is a PRODUCT OF TWO DERIVED SETS. A table of pairs typed out here
// would be missing exactly the pair nobody thought of, and would go on passing
// after a phase or an operation was added — which is the shape this project
// keeps paying for. AllPhases and AllOps are derived from the String methods'
// own ranges, and the counts below refuse a run whose domain has collapsed.
func TestTheFoldIsTotalOverEveryPhaseAndOperation(t *testing.T) {
	phases, ops := AllPhases(), AllOps()
	if len(phases) != 8 || len(ops) != 12 {
		t.Fatalf("the domain is %d phase(s) x %d op(s), want 8 x 12; a constant moved and this table no longer covers it", len(phases), len(ops))
	}

	// THE ACCEPTED SET, written out. Totality alone — every pair yields a
	// verdict — is satisfied by a fold that accepts everything, so the table
	// says WHICH pairs are accepted and treats every other one as refused.
	// A widening is then a pair this table does not name, which is an error
	// with the pair in it rather than a silent extra transition.
	//
	// Read down the column: a record is created once, bound once, and after it
	// is left or closed the only things it takes are the two counter ops and
	// the transitions that end it. `rebind` consumes a tombstone and is the
	// only op RETAINED accepts beyond those.
	accepted := map[Phase][]RecordOp{
		PhaseUnset:    {OpReserve, OpCreate, OpAdopt},
		PhaseReserved: {OpCreate, OpLease, OpLost, OpRetain, OpClose, OpStats, OpExtra},
		PhaseCreated:  {OpBind, OpLease, OpLost, OpRetain, OpClose, OpStats, OpExtra},
		PhaseJoined:   {OpLease, OpLost, OpLeave, OpRetain, OpClose, OpStats, OpExtra},
		PhaseLeft:     {OpRetain, OpClose, OpStats, OpExtra},
		PhaseRetained: {OpRebind, OpClose, OpStats, OpExtra},
		PhaseAdopted:  {OpBind, OpLease, OpLost, OpRetain, OpClose, OpStats, OpExtra},
		PhaseClosed:   {OpClose, OpStats, OpExtra},
	}
	// The table's own domain, checked against the derived populations rather
	// than trusted: a phase added to the code and not to this table would
	// otherwise be a phase whose whole row went unasserted.
	if len(accepted) != len(phases) {
		t.Fatalf("the expectation names %d phase(s) and the code has %d", len(accepted), len(phases))
	}
	inTable := map[Phase]map[RecordOp]bool{}
	for _, p := range phases {
		row, ok := accepted[p]
		if !ok {
			t.Fatalf("phase %s has no row in the expectation", p)
		}
		inTable[p] = map[RecordOp]bool{}
		for _, op := range row {
			if isUnknown(op.String()) {
				t.Fatalf("the %s row names %s, which is not an operation", p, op)
			}
			if inTable[p][op] {
				t.Fatalf("the %s row names %s twice", p, op)
			}
			inTable[p][op] = true
		}
	}

	acceptedN, rejected := 0, 0
	for _, p := range phases {
		for _, op := range ops {
			rec := recordAt(t, p)
			ev := RecordEvent{ID: "rec-1", Op: op, Seq: rec.Seq + 1, Scope: "net-a"}
			switch op {
			case OpLease:
				ev.Kind, ev.Lease = Acquired, ptr(testRecordLease("192.168.99.100/24"))
			case OpLost:
				ev.Reason = proto.ReasonExpired
			case OpStats:
				ev.Stats, ev.Manager = &Stats{}, "mgr-1"
			case OpExtra:
				ev.Extra = map[string]uint64{"k": 1}
			}
			if p == PhaseUnset {
				ev.Seq = 1
			}

			next, err := Fold(rec, ev)
			if want := inTable[p][op]; want != (err == nil) {
				if want {
					t.Errorf("(%s, %s) is in the accepted set and was REFUSED: %v", p, op, err)
				} else {
					t.Errorf("(%s, %s) was ACCEPTED and is not in the accepted set; a transition was widened, or the row is missing", p, op)
				}
			}
			var rj *Reject
			switch {
			case err == nil:
				acceptedN++
				if next.Phase == PhaseUnset {
					t.Errorf("(%s, %s) was accepted and left the record in no phase at all", p, op)
				}
			case errors.As(err, &rj):
				rejected++
				if rj.Reason == RejectNone {
					t.Errorf("(%s, %s) was refused with no reason", p, op)
				}
				if next.Counters.Rejects != rec.Counters.Rejects+1 {
					t.Errorf("(%s, %s) was refused and the reject count did not move", p, op)
				}
			default:
				t.Errorf("(%s, %s) returned %T, which is neither an accepted transition nor a *Reject", p, op, err)
			}
		}
	}
	if acceptedN+rejected != len(phases)*len(ops) {
		t.Fatalf("%d of %d pairs produced a verdict", acceptedN+rejected, len(phases)*len(ops))
	}
	// Both directions non-vacuous: a fold that accepted everything and one
	// that refused everything are both total.
	if acceptedN == 0 || rejected == 0 {
		t.Fatalf("%d accepted, %d refused: a table with an empty half measures nothing", acceptedN, rejected)
	}
	want := 0
	for _, row := range accepted {
		want += len(row)
	}
	if acceptedN != want {
		t.Errorf("%d pair(s) accepted, the table names %d", acceptedN, want)
	}
	t.Logf("%d pair(s): %d accepted, %d refused", acceptedN+rejected, acceptedN, rejected)
}

// TestARejectMovesNothingButItsOwnCount is defeat row M-5.
//
// A fold that counted a reject and then applied the event anyway satisfies
// every assertion about the counter. So the assertion is on the RECORD: apart
// from Rejects and LastReject it must be the one that went in.
func TestARejectMovesNothingButItsOwnCount(t *testing.T) {
	for _, c := range []struct {
		what  string
		phase Phase
		// before is folded in first, for refusals that need a history rather
		// than a phase. Each one is checked for acceptance, so a row cannot
		// pass by having its setup silently refused.
		before []RecordEvent
		ev     RecordEvent
		want   RejectReason
	}{
		{"a lease event after Leave, which would drag a left record back", PhaseLeft, nil,
			RecordEvent{Op: OpLease, Kind: Changed, Lease: ptr(testRecordLease("192.168.99.100/24"))}, RejectPhase},
		{"a bind on a retained record", PhaseRetained, nil, RecordEvent{Op: OpBind}, RejectPhase},
		{"a leave on a record nothing joined", PhaseCreated, nil, RecordEvent{Op: OpLeave}, RejectPhase},
		{"a reservation for a record that already exists", PhaseJoined, nil, RecordEvent{Op: OpReserve}, RejectExists},
		{"an adoption of a record that already exists", PhaseJoined, nil, RecordEvent{Op: OpAdopt}, RejectExists},
		{"a create on a joined record, which is a phase error rather than a duplicate", PhaseJoined, nil, RecordEvent{Op: OpCreate}, RejectPhase},
		{"a re-bind of something that is not a tombstone", PhaseJoined, nil, RecordEvent{Op: OpRebind}, RejectPhase},
		{"an event for a record nothing created", PhaseUnset, nil, RecordEvent{Op: OpBind}, RejectNoRecord},
		{"an event naming another record", PhaseJoined, nil, RecordEvent{Op: OpLeave, ID: "rec-2"}, RejectID},
		{"an event naming another scope", PhaseJoined, nil, RecordEvent{Op: OpLeave, Scope: "net-b"}, RejectScope},
		{"a second, different identity", PhaseJoined, nil, RecordEvent{Op: OpLeave, Identity: []byte{0x01}}, RejectIdentity},
		{"a lease event with no lease", PhaseJoined, nil, RecordEvent{Op: OpLease, Kind: Acquired}, RejectPayload},
		{"a Lost routed to the lease op", PhaseJoined, nil, RecordEvent{Op: OpLease, Kind: Lost}, RejectPayload},
		{"a counters event with no counters", PhaseJoined, nil, RecordEvent{Op: OpStats}, RejectPayload},
		{"a counters event naming no manager", PhaseJoined, nil, RecordEvent{Op: OpStats, Stats: &Stats{}}, RejectPayload},
		{"a second, different family", PhaseJoined, nil, RecordEvent{Op: OpLeave, Family: FamilyV6}, RejectFamily},
		{"a second, different family on the op that carries one", PhaseCreated, nil, RecordEvent{Op: OpBind, Family: FamilyV6}, RejectFamily},
		// A manager id that comes BACK. Under one id the fold cannot tell a
		// second manager from a renewal, so it counts (see
		// TestTwoManagersUnderOneIdCountAsOne); an id that returns after
		// another one is decidable, and is refused.
		{"a manager id that returns after another manager", PhaseJoined,
			[]RecordEvent{
				{Op: OpStats, Manager: "mgr-a", Stats: &Stats{Sent: 2}},
				{Op: OpStats, Manager: "mgr-b", Stats: &Stats{Sent: 1}},
			},
			RecordEvent{Op: OpStats, Manager: "mgr-a", Stats: &Stats{Sent: 5}}, RejectManager},
	} {
		t.Run(c.what, func(t *testing.T) {
			rec := recordAt(t, c.phase)
			for i, pre := range c.before {
				pre.ID, pre.Seq = "rec-1", rec.Seq+1
				next, err := Fold(rec, pre)
				if err != nil {
					t.Fatalf("setup event %d (%s) was refused: %v; the row would then test the wrong refusal", i, pre.Op, err)
				}
				rec = next
			}
			ev := c.ev
			if ev.ID == "" {
				ev.ID = "rec-1"
			}
			ev.Seq = rec.Seq + 1

			got, err := Fold(rec, ev)
			var rj *Reject
			if !errors.As(err, &rj) {
				t.Fatalf("accepted, and moved the record to %s", got.Phase)
			}
			if rj.Reason != c.want {
				t.Fatalf("refused as %q, want %q", rj.Reason, c.want)
			}
			if rj.Op != ev.Op || rj.Phase != rec.Phase {
				t.Fatalf("the refusal names (%s, %s); the pair was (%s, %s)", rj.Phase, rj.Op, rec.Phase, ev.Op)
			}

			want := rec
			want.Counters.Rejects++
			want.LastReject = c.want
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("a refused event changed the record.\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// TestTheTrailingStopIsNotALoss is defeat row M-1 and seam row P-7.
//
// Cancelling a manager makes ring 1 drop the lease with ReasonStopped, so every
// ordinary shutdown and every one-shot acquisition ends with a Lost carrying
// it. Folding that as a loss clears the address the record has just recorded.
func TestTheTrailingStopIsNotALoss(t *testing.T) {
	rec := recordAt(t, PhaseJoined)
	l := testRecordLease("192.168.99.100/24")

	var err error
	if rec, err = Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLease, Kind: Acquired, Lease: &l}); err != nil {
		t.Fatalf("acquiring: %v", err)
	}
	if rec, err = Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLost, Reason: proto.ReasonStopped}); err != nil {
		t.Fatalf("the stop was refused: %v", err)
	}

	if !rec.Held {
		t.Fatal("the record no longer holds a lease after its manager was stopped; the address it just acquired is gone")
	}
	if rec.Lease.Addr != l.Addr {
		t.Fatalf("the record holds %s, want the acquired %s", rec.Lease.Addr, l.Addr)
	}
	if rec.Counters.Losses != 0 {
		t.Fatalf("Losses = %d, want 0: a stopped manager did not lose anything", rec.Counters.Losses)
	}
	if rec.Counters.StoppedNotLost != 1 {
		t.Fatalf("StoppedNotLost = %d, want 1: the stop must stay visible rather than be dropped", rec.Counters.StoppedNotLost)
	}
	if _, ok := rec.Resume(testNow); !ok {
		t.Fatal("the record is not resumable, so nothing would ask the server to confirm the address it still holds")
	}

	// The other direction: every OTHER reason IS a loss. A fold that treated
	// them all as harmless would pass everything above.
	for _, reason := range []proto.Reason{proto.ReasonExpired, proto.ReasonNak, proto.ReasonConflict, proto.ReasonTransport, proto.ReasonReleased} {
		t.Run(reason.String(), func(t *testing.T) {
			lost, err := Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLost, Reason: reason})
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if lost.Held {
				t.Fatalf("the record still holds %s after losing it for %s", lost.Lease.Addr, reason)
			}
			if lost.Counters.Losses != 1 {
				t.Fatalf("Losses = %d after %s, want 1", lost.Counters.Losses, reason)
			}
		})
	}
}

// TestANakIsCountedOnceAcrossTheTwoEventsItProduces. One DHCPNAK that costs a
// held lease produces Lost and then Failed, both carrying ReasonNak, and a fold
// that counted the reason wherever it saw it would report one refusal as two.
func TestANakIsCountedOnceAcrossTheTwoEventsItProduces(t *testing.T) {
	rec := recordAt(t, PhaseJoined)
	l := testRecordLease("192.168.99.100/24")
	steps := []RecordEvent{
		{Op: OpLease, Kind: Acquired, Lease: &l},
		{Op: OpLost, Reason: proto.ReasonNak},
		{Op: OpLease, Kind: Failed, Reason: proto.ReasonNak},
	}
	for _, ev := range steps {
		ev.ID, ev.Seq = "rec-1", rec.Seq+1
		next, err := Fold(rec, ev)
		if err != nil {
			t.Fatalf("%s: %v", ev.Op, err)
		}
		rec = next
	}
	if rec.Counters.Naks != 1 {
		t.Fatalf("Naks = %d for one DHCPNAK, want 1", rec.Counters.Naks)
	}
	if rec.Counters.Losses != 1 || rec.Counters.Failures != 1 {
		t.Fatalf("Losses = %d, Failures = %d, want 1 and 1", rec.Counters.Losses, rec.Counters.Failures)
	}
}

// TestStatsAccumulateAcrossManagerInstances is defeat row M-2.
//
// A record outlives its managers: one for the acquisition that answers an
// address request, one for the join, one more per restart. Each starts its
// counters at zero, so a record that ASSIGNS the latest snapshot reports the
// last manager's numbers as the endpoint's whole history.
func TestStatsAccumulateAcrossManagerInstances(t *testing.T) {
	rec := recordAt(t, PhaseJoined)
	apply := func(manager string, s Stats) {
		t.Helper()
		next, err := Fold(rec, RecordEvent{
			ID: "rec-1", Seq: rec.Seq + 1, Op: OpStats,
			Instance: "plugin-1", Manager: manager, Stats: &s,
		})
		if err != nil {
			t.Fatalf("merging %s: %v", manager, err)
		}
		rec = next
	}

	apply("mgr-1", Stats{Sent: 2, Received: 1})
	apply("mgr-1", Stats{Sent: 5, Received: 4})
	if got := rec.Counters.Wire.Sent; got != 5 {
		t.Fatalf("Sent = %d after two snapshots of one manager, want 5: within a manager the counters are cumulative already", got)
	}

	apply("mgr-2", Stats{Sent: 3, Received: 2})
	if got := rec.Counters.Wire.Sent; got != 8 {
		t.Fatalf("Sent = %d after a second manager sent 3 more, want 8", got)
	}
	if got := rec.Counters.Wire.Received; got != 6 {
		t.Fatalf("Received = %d, want 6", got)
	}

	// Backwards inside one manager is not a new manager, it is a lie.
	if _, err := Fold(rec, RecordEvent{
		ID: "rec-1", Seq: rec.Seq + 1, Op: OpStats,
		Instance: "plugin-1", Manager: "mgr-2", Stats: &Stats{Sent: 1},
	}); err == nil {
		t.Fatal("a snapshot whose counters went backwards inside one manager was accepted")
	}
}

// TestTwoManagersInOneProcessBothCount is the design's ORDINARY sequence, and
// the shape that froze the wire counters while one field carried two
// identities.
//
// The plugin runs the CreateEndpoint one-shot manager and then the Join
// manager in ONE process, so both write under the same Instance. A fold that
// rebaselines on the writer sees the second manager's first snapshot as the
// first manager's counters going backwards, refuses it, and reports the
// endpoint's whole life as whatever the one-shot managed to send.
func TestTwoManagersInOneProcessBothCount(t *testing.T) {
	const writer = "plugin-1"
	rec := recordAt(t, PhaseJoined)

	// The one-shot: two messages out, two in.
	oneShot := Stats{Sent: 4, Received: 2, LeasesAcquired: 1}
	next, err := Fold(rec, RecordEvent{
		ID: "rec-1", Seq: rec.Seq + 1, Op: OpStats,
		Instance: writer, Manager: "create-endpoint", Stats: &oneShot,
	})
	if err != nil {
		t.Fatalf("the one-shot manager's counters were refused: %v", err)
	}
	rec = next

	// The Join manager, in the SAME process, starting from zero as every
	// manager does. Its numbers are LOWER than the one-shot's, which is the
	// whole difficulty: they are not a regression, they are a new manager.
	join := Stats{Sent: 2, Received: 1, LeasesAcquired: 1}
	next, err = Fold(rec, RecordEvent{
		ID: "rec-1", Seq: rec.Seq + 1, Op: OpStats,
		Instance: writer, Manager: "join", Stats: &join,
	})
	if err != nil {
		t.Fatalf("the second manager in one process was refused: %v; the record froze at the first manager's numbers", err)
	}
	rec = next

	if got := rec.Counters.Wire.Sent; got != 6 {
		t.Errorf("Sent = %d after two managers under one writer, want 6 (4 + 2); 4 means the second manager was refused and the counter froze", got)
	}
	if got := rec.Counters.Wire.Received; got != 3 {
		t.Errorf("Received = %d, want 3 (2 + 1)", got)
	}
	if rec.Counters.Rejects != 0 {
		t.Errorf("%d reject(s) folding two managers under one writer id, want 0", rec.Counters.Rejects)
	}
	if rec.Instance != writer {
		t.Errorf("Instance = %q, want %q: the record still names the process that wrote the line", rec.Instance, writer)
	}
}

// TestTheWriterIdIsNotTheManagerId drives the two fields apart in both
// directions, so neither can be quietly derived from the other.
func TestTheWriterIdIsNotTheManagerId(t *testing.T) {
	base := recordAt(t, PhaseJoined)

	// ONE writer, TWO managers: both count. (The case above, as a table row.)
	// TWO writers, ONE manager: the same manager's counters continue across an
	// upgrade that replaced the process, and are NOT rebaselined.
	//
	// A fold keyed on the writer gets the first wrong; a fold keyed on nothing
	// gets the second wrong by double-counting the manager's whole history.
	for _, tc := range []struct {
		name            string
		writers         [2]string
		managers        [2]string
		second          Stats
		wantSent        uint64
		wantRejects     uint64
		whatItWouldMean string
	}{
		{
			name: "one process, two managers", writers: [2]string{"p1", "p1"},
			managers: [2]string{"m1", "m2"}, second: Stats{Sent: 2},
			wantSent: 6, whatItWouldMean: "the second manager's counters were dropped",
		},
		{
			name: "two processes, one manager", writers: [2]string{"p1", "p2"},
			managers: [2]string{"m1", "m1"}, second: Stats{Sent: 7},
			wantSent: 7, whatItWouldMean: "one manager's history was counted twice across an upgrade",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			for i := range 2 {
				s := Stats{Sent: 4}
				if i == 1 {
					s = tc.second
				}
				next, err := Fold(rec, RecordEvent{
					ID: "rec-1", Seq: rec.Seq + 1, Op: OpStats,
					Instance: tc.writers[i], Manager: tc.managers[i], Stats: &s,
				})
				if err != nil {
					t.Fatalf("snapshot %d (writer %s, manager %s) was refused: %v",
						i+1, tc.writers[i], tc.managers[i], err)
				}
				rec = next
			}
			if got := rec.Counters.Wire.Sent; got != tc.wantSent {
				t.Errorf("Sent = %d, want %d: %s", got, tc.wantSent, tc.whatItWouldMean)
			}
			if rec.Counters.Rejects != tc.wantRejects {
				t.Errorf("%d reject(s), want %d", rec.Counters.Rejects, tc.wantRejects)
			}
		})
	}

	// And an OpStats that names no manager is refused rather than folded under
	// the empty string, which would make every anonymous manager one manager.
	got, err := Fold(base, RecordEvent{
		ID: "rec-1", Seq: base.Seq + 1, Op: OpStats,
		Instance: "p1", Stats: &Stats{Sent: 1},
	})
	var rej *Reject
	if !errors.As(err, &rej) || rej.Reason != RejectPayload {
		t.Fatalf("an OpStats naming no manager gave %v, want a RejectPayload", err)
	}
	if got.Counters.Wire.Sent != 0 {
		t.Errorf("the refused snapshot was folded anyway: Sent = %d", got.Counters.Wire.Sent)
	}
}

// TestTheTwoCounterHalvesAreDisjoint is defeat row M-3: the record must not
// derive one fact twice. It reads the two structs rather than trusting the
// comment above them.
func TestTheTwoCounterHalvesAreDisjoint(t *testing.T) {
	folded := map[string]bool{}
	rc := reflect.TypeOf(RecordCounters{})
	for i := 0; i < rc.NumField(); i++ {
		if rc.Field(i).Name != "Wire" {
			folded[rc.Field(i).Name] = true
		}
	}
	if len(folded) == 0 {
		t.Fatal("RecordCounters has no folded half, so this comparison is vacuous")
	}

	wt := reflect.TypeOf(WireCounters{})
	if wt.NumField() == 0 {
		t.Fatal("WireCounters is empty, so this comparison is vacuous")
	}
	for i := 0; i < wt.NumField(); i++ {
		if folded[wt.Field(i).Name] {
			t.Errorf("%s is both folded from the record's events and accumulated from Stats; the two part company as soon as a record outlives a manager", wt.Field(i).Name)
		}
	}
}

// TestEveryStatsFieldIsEitherWiredOrFolded is the other half of M-3, and it is
// the one that sees a counter ADDED to Stats. Every field there must be
// accumulated by name in WireCounters or be named in statsFoldedInstead as
// derived from the record's own events; a field in neither would silently
// vanish from a record's account of itself.
func TestEveryStatsFieldIsEitherWiredOrFolded(t *testing.T) {
	wired := map[string]bool{}
	wt := reflect.TypeOf(WireCounters{})
	for i := 0; i < wt.NumField(); i++ {
		wired[wt.Field(i).Name] = true
	}

	st := reflect.TypeOf(Stats{})
	if st.NumField() == 0 {
		t.Fatal("Stats is empty")
	}
	seenFolded := 0
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		switch {
		case wired[name]:
		case statsFoldedInstead[name] != "":
			seenFolded++
		default:
			t.Errorf("Stats.%s is neither accumulated in WireCounters nor named in statsFoldedInstead; a record would never report it", name)
		}
	}
	if seenFolded != len(statsFoldedInstead) {
		t.Errorf("statsFoldedInstead names %d field(s) and Stats has %d of them; a name there that Stats no longer declares excuses nothing", len(statsFoldedInstead), seenFolded)
	}
	// And the fold really does produce the counter each of them points at.
	rc := reflect.TypeOf(RecordCounters{})
	for stat, folded := range statsFoldedInstead {
		if _, ok := rc.FieldByName(folded); !ok {
			t.Errorf("Stats.%s is excused as folded into RecordCounters.%s, which does not exist", stat, folded)
		}
	}
}

// TestEveryWireCounterSurvivesTheArithmetic drives every field of WireCounters
// through the two operations that touch it — the mapping from Stats and the
// delta accumulation — with a value unique to that field.
//
// It exists because both operations are reflective loops: a loop that skipped a
// field, or read the wrong one, produces zeros that no assertion about one
// counter would find.
func TestEveryWireCounterSurvivesTheArithmetic(t *testing.T) {
	wt := reflect.TypeOf(WireCounters{})
	n := wt.NumField()
	if n == 0 {
		t.Fatal("WireCounters is empty")
	}

	var s Stats
	sv := reflect.ValueOf(&s).Elem()
	for i := 0; i < n; i++ {
		f := sv.FieldByName(wt.Field(i).Name)
		if !f.IsValid() {
			t.Fatalf("Stats has no %s, so statsIntoWire would panic", wt.Field(i).Name)
		}
		f.SetUint(uint64(i + 1))
	}

	got := statsIntoWire(s)
	gv := reflect.ValueOf(got)
	for i := 0; i < n; i++ {
		if want := uint64(i + 1); gv.Field(i).Uint() != want {
			t.Errorf("statsIntoWire dropped %s: got %d, want %d", wt.Field(i).Name, gv.Field(i).Uint(), want)
		}
	}

	var acc WireCounters
	if bad, ok := acc.addDelta(got, WireCounters{}); !ok {
		t.Fatalf("addDelta refused a rise in %s", bad)
	}
	if bad, ok := acc.addDelta(got, got); !ok {
		t.Fatalf("addDelta refused a zero delta in %s", bad)
	}
	av := reflect.ValueOf(acc)
	for i := 0; i < n; i++ {
		if want := uint64(i + 1); av.Field(i).Uint() != want {
			t.Errorf("addDelta lost %s: got %d, want %d", wt.Field(i).Name, av.Field(i).Uint(), want)
		}
	}
	for i := 0; i < n; i++ {
		base := got
		reflect.ValueOf(&base).Elem().Field(i).SetUint(uint64(i + 2))
		var sink WireCounters
		if bad, ok := sink.addDelta(got, base); ok {
			t.Errorf("a fall in %s was accepted", wt.Field(i).Name)
		} else if bad != wt.Field(i).Name {
			t.Errorf("a fall in %s was reported as %s", wt.Field(i).Name, bad)
		}
	}
}

func ptr[T any](v T) *T { return &v }
