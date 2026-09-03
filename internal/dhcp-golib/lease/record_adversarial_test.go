package lease

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// This file is D17 applied to the journal design's own defeat list: one table
// test per row that the library half can be wrong about. The row numbers are
// the design note's.

// TestIdentityIsWrittenOnceAndSurvivesARebind is note row D-1, the one the
// record's key cannot survive being wrong about.
//
// A runtime that mints a fresh hardware address per endpoint makes any
// MAC-derived client identifier a different identifier at every restart, and a
// conforming server then hands out a different address (RFC 2131 section 4.2).
// So the identity is a stored field, written once, and a re-bind carries it
// forward under the NEW hardware address.
//
// The BOUND, because this closes one case and not the class: it makes an
// address stable for a container restarted alone, where exactly one tombstone
// can match. Two containers restarted together meet two fresh addresses with
// nothing to tell them apart, and no field the driver receives can narrow it.
func TestIdentityIsWrittenOnceAndSurvivesARebind(t *testing.T) {
	rec := recordAt(t, PhaseRetained)
	if len(rec.Identity) == 0 {
		t.Fatal("the retained record carries no identity, so this test measures nothing")
	}

	rebound, err := Fold(rec, RecordEvent{
		ID: "rec-1", Seq: rec.Seq + 1, Op: OpRebind, CHAddr: testMAC2,
	})
	if err != nil {
		t.Fatalf("the re-bind was refused: %v", err)
	}
	if !bytesEqual(rebound.Identity, testIdentity) {
		t.Fatalf("identity = %x after a re-bind, want the original %x", rebound.Identity, testIdentity)
	}
	if !bytesEqual(rebound.CHAddr, testMAC2) {
		t.Fatalf("chaddr = %x, want the new %x: a re-bind wears a new hardware address", rebound.CHAddr, testMAC2)
	}
	if rebound.Phase != PhaseCreated {
		t.Fatalf("phase = %s, want created", rebound.Phase)
	}
	if !rebound.Deadline.IsZero() {
		t.Fatal("the tombstone deadline survived the re-bind that consumed it")
	}

	// Written ONCE: a second, different identity is refused rather than
	// overwritten, whichever op carries it — including the re-bind, which is
	// the op that legitimately changes the hardware address and is therefore
	// the one an overwrite would hide behind. The record must come back
	// untouched, not merely with its identity intact: a refusal that applied
	// half of its event would be invisible to a check on Identity alone.
	//
	// The second identity is offered in two shapes. A SAME-LENGTH one is the
	// shape that matters: a refusal that compared lengths, or any prefix of
	// the bytes, would pass every different-length row and let the one
	// identifier the record is keyed on change under it.
	sameLen := append([]byte(nil), testIdentity...)
	sameLen[len(sameLen)-1] ^= 0xff
	if len(sameLen) != len(testIdentity) || bytesEqual(sameLen, testIdentity) {
		t.Fatal("the same-length identity is not a different identity of the same length")
	}
	tomb := recordAt(t, PhaseRetained)
	for _, tc := range []struct {
		op    RecordOp
		from  Record
		other []byte
	}{
		{OpRebind, tomb, sameLen},
		{OpRebind, tomb, []byte{0x00}},
		{OpBind, rebound, sameLen},
		{OpBind, rebound, []byte{0x00}},
		{OpLease, rebound, sameLen},
		{OpRetain, rebound, sameLen},
		{OpClose, rebound, sameLen},
	} {
		got, err := Fold(tc.from, RecordEvent{
			ID: "rec-1", Seq: tc.from.Seq + 1, Op: tc.op,
			CHAddr: testMAC2, Identity: tc.other,
		})
		var rej *Reject
		if !errors.As(err, &rej) || rej.Reason != RejectIdentity {
			t.Errorf("a second identity %x on %s gave %v, want a RejectIdentity", tc.other, tc.op, err)
			continue
		}
		want := tc.from
		want.Counters.Rejects++
		want.LastReject = rej.Reason
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s with %x: the refused event moved the record: got %+v, want the input plus its reject count", tc.op, tc.other, got)
		}
	}
	// And the same bytes again are not a rewrite.
	if _, err := Fold(rebound, RecordEvent{ID: "rec-1", Seq: rebound.Seq + 1, Op: OpBind, Identity: testIdentity}); err != nil {
		t.Fatalf("re-stating the same identity was refused: %v", err)
	}
}

// TestTheLookupsUseOnlyWhatEveryRequestCarries is note row D-2.
//
// The three paths that ask about an address carry, between them, only a pool
// identifier, an address, and a hardware address if one was demanded: a fresh
// request, the same request replayed after a daemon restart, and the release.
// A lookup keyed on an endpoint, a container or a hostname works in the path
// that has it and is a rewrite in the two that do not.
//
// The rows below are those three request shapes, each built from ITS OWN
// fields; the call does not compile if a field outside the intersection is
// used, which is the point.
func TestTheLookupsUseOnlyWhatEveryRequestCarries(t *testing.T) {
	evs := recordFixture(t)
	rb := Rebuild(evs)
	if len(rb.Records) != 2 {
		t.Fatalf("the fixture rebuilt %d record(s), want 2", len(rb.Records))
	}

	type request struct {
		what  string
		scope string
		addr  netip.Addr
		mac   []byte
	}
	for _, r := range []request{
		{"a fresh request, which carries a pool and a hardware address", "net-a", netip.Addr{}, testMAC},
		{"the replayed request, which carries a pool and the stored address", "net-a", netip.MustParseAddr("192.168.99.100"), nil},
		{"the release, which carries a pool and the address", "net-a", netip.MustParseAddr("192.168.99.100"), nil},
	} {
		t.Run(r.what, func(t *testing.T) {
			var got []Record
			if r.addr.IsValid() {
				got = rb.ByScopeAddr(r.scope, r.addr)
			} else {
				got = rb.ByScopeMAC(r.scope, r.mac)
			}
			if len(got) != 1 {
				t.Fatalf("resolved to %d record(s), want exactly 1", len(got))
			}
			if got[0].ID != "rec-1" {
				t.Fatalf("resolved to %s", got[0].ID)
			}
		})
	}
}

// TestAnIndexKeyedOnThePairSeparatesTwoScopes is defeat row M-7: two networks
// handing out the same private address, and one machine on two networks.
func TestAnIndexKeyedOnThePairSeparatesTwoScopes(t *testing.T) {
	rb := Rebuild(recordFixture(t))
	addr := netip.MustParseAddr("192.168.99.100")

	a := rb.ByScopeAddr("net-a", addr)
	b := rb.ByScopeAddr("net-b", addr)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("the same address in two scopes resolved to %d and %d records, want 1 and 1", len(a), len(b))
	}
	if a[0].ID == b[0].ID {
		t.Fatalf("both scopes resolved to %s: the index collapsed two networks into one record", a[0].ID)
	}
	if got := rb.ByScopeMAC("net-b", testMAC); len(got) != 1 || got[0].ID != "rec-2" {
		t.Fatalf("one hardware address on two networks resolved to %+v", got)
	}
	if got := rb.ByScopeAddr("net-c", addr); len(got) != 0 {
		t.Fatalf("a scope with no records resolved to %d", len(got))
	}
}

// TestAReleaseWithNoEndpointRetains is note row D-8 (ii), both sequences.
//
// A release arrives with no endpoint in two real orders: when the network
// driver's endpoint creation fails after an address was assigned, and when the
// daemon's start-up cleanup force-deletes a stale endpoint after the replay.
// A fold with no retain arm from those phases refuses the release, keeps the
// address held, and answers a later request from a record that should have
// been a tombstone.
func TestAReleaseWithNoEndpointRetains(t *testing.T) {
	for _, c := range []struct {
		what string
		from Phase
	}{
		{"an address request answered, then endpoint creation failed", PhaseReserved},
		{"the replay adopted an address, then the start-up cleanup released it", PhaseAdopted},
	} {
		t.Run(c.what, func(t *testing.T) {
			rec := recordAt(t, c.from)
			l := testRecordLease("192.168.99.100/24")
			var err error
			if rec, err = Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLease, Kind: Acquired, Lease: &l}); err != nil {
				t.Fatalf("acquiring: %v", err)
			}
			deadline := testNow.Add(10 * time.Minute)
			got, err := Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpRetain, Deadline: deadline})
			if err != nil {
				t.Fatalf("the release was refused, so the address stays held: %v", err)
			}
			if got.Phase != PhaseRetained {
				t.Fatalf("phase = %s, want retained", got.Phase)
			}
			if !got.Deadline.Equal(deadline) {
				t.Fatalf("deadline = %s, want %s", got.Deadline, deadline)
			}
			if _, ok := got.Resume(testNow); ok {
				t.Fatal("a tombstone offered its address for an INIT-REBOOT; a retained address is left to expire on the server")
			}
			rb := Rebuilt{Records: []Record{got}, byID: map[string]int{"rec-1": 0}}
			if n := len(rb.Tombstones("net-a", testNow)); n != 1 {
				t.Fatalf("the tombstone set holds %d record(s), want 1", n)
			}
			if n := len(rb.Tombstones("net-a", deadline.Add(time.Second))); n != 0 {
				t.Fatalf("a tombstone past its deadline is still a re-bind candidate (%d)", n)
			}
		})
	}
}

// TestAnAdoptedAddressIsHeldWithNoExchange is note row D-6.
//
// At daemon start every stored address is re-requested. A driver with no record
// that ran a fresh exchange would return a DIFFERENT address, which the caller
// then stores — or an error, which is only logged, leaving the address in use
// and unrenewed. So "we already hold this" is a PHASE, reached with no wire
// traffic, and not something inferred later.
func TestAnAdoptedAddressIsHeldWithNoExchange(t *testing.T) {
	rec := recordAt(t, PhaseAdopted)
	l := testRecordLease("192.168.99.100/24")
	l.ServerID = netip.Addr{}
	l.Expire = time.Time{}

	got, err := Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLease, Kind: Acquired, Lease: &l})
	if err != nil {
		t.Fatalf("an adopted record could not be told what address it holds: %v", err)
	}
	if a, ok := got.Addr(); !ok || a.String() != "192.168.99.100" {
		t.Fatalf("the adopted record holds %v (%v)", a, ok)
	}
	if got.Counters.Wire.Sent != 0 {
		t.Fatalf("%d frame(s) are accounted to an adoption", got.Counters.Wire.Sent)
	}
	resumed, ok := got.Resume(testNow)
	if !ok {
		t.Fatal("an adopted address is not resumable, so the join would run a fresh exchange for an address already in use")
	}
	if !resumed.Expire.IsZero() {
		t.Fatalf("expiry = %s; an adopted address has no lease behind it and its expiry is unknown, not now", resumed.Expire)
	}
	joined, err := Fold(got, RecordEvent{ID: "rec-1", Seq: got.Seq + 1, Op: OpBind})
	if err != nil {
		t.Fatalf("an adopted record could not be joined: %v", err)
	}
	if joined.Phase != PhaseJoined {
		t.Fatalf("phase = %s after the join", joined.Phase)
	}
}

// TestResumeRefusesWhatCannotBeConfirmed is note row D-7, at the library's
// edge.
//
// A REQUEST naming an address the client believes it holds is answered by a
// server with knowledge and IGNORED by one without (RFC 2131 section 4.3.2),
// so a stale expiry buys latency rather than corruption — provided nothing is
// applied to the link before the ACK. The record's part is to store WALL-CLOCK
// deadlines and to refuse to offer an expired one.
func TestResumeRefusesWhatCannotBeConfirmed(t *testing.T) {
	live := testRecordLease("192.168.99.100/24")
	dead := testRecordLease("192.168.99.100/24")
	dead.Expire = testNow.Add(-time.Second)
	forever := testRecordLease("192.168.99.100/24")
	forever.Expire = time.Time{}

	for _, phase := range AllPhases() {
		if phase == PhaseUnset {
			continue
		}
		for _, c := range []struct {
			what  string
			lease Lease
			want  bool
		}{
			{"a live lease", live, true},
			{"a lease that expired a second ago", dead, false},
			{"an infinite lease", forever, true},
		} {
			t.Run(phase.String()+"/"+c.what, func(t *testing.T) {
				rec := recordAt(t, phase)
				rec.Lease, rec.Held = c.lease, true

				_, ok := rec.Resume(testNow)
				wantPhase := phase != PhaseRetained && phase != PhaseClosed
				want := c.want && wantPhase
				if ok != want {
					t.Fatalf("Resume = %v, want %v", ok, want)
				}
			})
		}
	}

	held := recordAt(t, PhaseJoined)
	held.Lease = live
	if _, ok := held.Resume(testNow); ok {
		t.Fatal("a record that holds nothing offered a lease to resume")
	}
}

// TestASequenceRegressionIsRefused is note row D-9: an old plugin process and a
// new one appending to one file during an upgrade.
//
// The sequence is per record and strictly increasing. It is NOT relaxed for a
// writer that leaves it at zero: two processes' interleaved lines folding into
// one plausible record is the silent outcome this refuses, and refusing loudly
// is the only direction that can be seen.
func TestASequenceRegressionIsRefused(t *testing.T) {
	rec := recordAt(t, PhaseJoined)
	for _, c := range []struct {
		what string
		seq  uint64
	}{
		{"the same sequence number again", rec.Seq},
		{"an older sequence number", rec.Seq - 1},
		{"an unsequenced writer", 0},
	} {
		t.Run(c.what, func(t *testing.T) {
			_, err := Fold(rec, RecordEvent{ID: "rec-1", Seq: c.seq, Op: OpLeave, Instance: "other-process"})
			var rj *Reject
			if !errors.As(err, &rj) || rj.Reason != RejectSeq {
				t.Fatalf("err = %v, want a sequence refusal", err)
			}
		})
	}
	if _, err := Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLeave}); err != nil {
		t.Fatalf("the next sequence number was refused: %v", err)
	}
}

// TestParamsAreSnapshotNotAliased is defeat row M-8.
//
// proto.Params is a value with four slice fields. A shallow copy leaves the
// record pointing at the caller's memory, and proto.Replay then replays a
// configuration that never ran — which is the one thing the snapshot exists to
// prevent.
func TestParamsAreSnapshotNotAliased(t *testing.T) {
	p := proto.DefaultParams([]byte{1, 2, 3, 4, 5, 6})
	p.ClientID = []byte{0xff, 0x01, 0x02}
	p.Servers.Allow = []netip.Addr{netip.MustParseAddr("192.168.99.1")}

	rec, err := Fold(Record{}, RecordEvent{ID: "rec-1", Seq: 1, Op: OpCreate, Scope: "net-a", Params: &p})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Params == nil {
		t.Fatal("the record kept no Params, so nothing it wrote is replayable")
	}

	p.ClientID[0] = 0x00
	p.CHAddr[0] = 0x99
	p.Servers.Allow[0] = netip.MustParseAddr("10.0.0.1")
	p.ParameterList[0] = 0

	if rec.Params.ClientID[0] != 0xff {
		t.Error("ClientID aliases the caller's slice")
	}
	if rec.Params.CHAddr[0] != 1 {
		t.Error("CHAddr aliases the caller's slice")
	}
	if rec.Params.Servers.Allow[0].String() != "192.168.99.1" {
		t.Error("Servers.Allow aliases the caller's slice")
	}
	if rec.Params.ParameterList[0] != proto.DefaultParams(nil).ParameterList[0] {
		t.Error("ParameterList aliases the caller's slice")
	}

	// And the lease the same way.
	l := testRecordLease("192.168.99.100/24")
	rec, err = Fold(rec, RecordEvent{ID: "rec-1", Seq: 2, Op: OpLease, Kind: Acquired, Lease: &l})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	l.DNS[0] = netip.MustParseAddr("10.0.0.1")
	l.Options[wire53] = []byte{0}
	if rec.Lease.DNS[0].String() != "192.168.99.1" {
		t.Error("Lease.DNS aliases the caller's slice")
	}
	if rec.Lease.Options[wire53][0] != 5 {
		t.Error("Lease.Options aliases the caller's map")
	}
}

// TestRebuildKeepsWhatCameBeforeARefusedEvent is defeat row M-9's other half
// and the reason Rebuild collects refusals rather than returning on the first
// one: the records written before a bad line are the whole value of a durable
// log, and a rebuild that refused the file would lose every one of them.
func TestRebuildKeepsWhatCameBeforeARefusedEvent(t *testing.T) {
	evs := recordFixture(t)
	bad := RecordEvent{ID: "rec-1", Seq: 2, Op: OpLeave, Instance: "another-process"}
	evs = append(evs[:2], append([]RecordEvent{bad}, evs[2:]...)...)

	rb := Rebuild(evs)
	if len(rb.Rejects) != 1 {
		t.Fatalf("%d refusal(s) recorded, want 1", len(rb.Rejects))
	}
	if rb.Rejects[0].Reason != RejectSeq {
		t.Fatalf("the refusal is %q", rb.Rejects[0].Reason)
	}
	if len(rb.Records) != 2 {
		t.Fatalf("%d record(s) survived the bad line, want 2", len(rb.Records))
	}
	one, ok := rb.ByID("rec-1")
	if !ok || one.Phase != PhaseJoined {
		t.Fatalf("rec-1 is %s (%v) after a bad line in the middle of its history", one.Phase, ok)
	}
	if one.Counters.Rejects != 1 {
		t.Fatalf("the record carries %d refusal(s)", one.Counters.Rejects)
	}
}

// TestRebuildAnswersInAppendOrder. The order of Records is the order the
// records were created, so it is a function of the input; a rebuild that
// answered in map order would make every downstream comparison a flake.
func TestRebuildAnswersInAppendOrder(t *testing.T) {
	evs := recordFixture(t)
	first := Rebuild(evs)
	for i := 0; i < 20; i++ {
		got := Rebuild(evs)
		for j := range got.Records {
			if got.Records[j].ID != first.Records[j].ID {
				t.Fatalf("run %d answered %s at position %d, the first run answered %s", i, got.Records[j].ID, j, first.Records[j].ID)
			}
		}
	}
	if first.Records[0].ID != "rec-1" || first.Records[1].ID != "rec-2" {
		t.Fatalf("the order is %s, %s; the fixture creates rec-1 first", first.Records[0].ID, first.Records[1].ID)
	}
	if _, ok := Rebuild(nil).ByID("rec-1"); ok {
		t.Fatal("an empty journal produced a record")
	}
}

// TestARecordIsNeverInventedByTheFold is note row D-4 at the library's edge:
// the journal is the authority for identity, the lease and the deadlines, and
// for nothing else. It cannot conjure a record that no event created, which is
// what keeps "does this endpoint still exist" a question for whoever owns the
// endpoints.
func TestARecordIsNeverInventedByTheFold(t *testing.T) {
	rb := Rebuild([]RecordEvent{
		{ID: "ghost", Seq: 1, Op: OpBind},
		{ID: "ghost", Seq: 2, Op: OpLease, Kind: Acquired, Lease: ptr(testRecordLease("192.168.99.100/24"))},
		{ID: "ghost", Seq: 3, Op: OpRetain, Deadline: testNow.Add(time.Hour)},
	})
	if len(rb.Records) != 0 {
		t.Fatalf("%d record(s) were built from events that never created one", len(rb.Records))
	}
	if len(rb.Rejects) != 3 {
		t.Fatalf("%d refusal(s), want 3", len(rb.Rejects))
	}
	if n := len(rb.ByScopeAddr("net-a", netip.MustParseAddr("192.168.99.100"))); n != 0 {
		t.Fatalf("the address index answers %d record(s) for an invented one", n)
	}
}

// TestEventRecordRoutesALostToItsOwnOperation. The mapping from a manager event
// to a durable line is made in one place so that "a Lost is OpLost" is a data
// dependency and not a convention each call site remembers; a Lost routed to
// OpLease is refused by the fold rather than folded as something else.
func TestEventRecordRoutesALostToItsOwnOperation(t *testing.T) {
	l := testRecordLease("192.168.99.100/24")
	for _, c := range []struct {
		ev       Event
		wantOp   RecordOp
		hasLease bool
	}{
		{Event{Kind: Acquired, Lease: l}, OpLease, true},
		{Event{Kind: Changed, Lease: l}, OpLease, true},
		{Event{Kind: Renewed, Lease: l}, OpLease, true},
		{Event{Kind: Failed, Reason: proto.ReasonNoServer}, OpLease, false},
		{Event{Kind: Lost, Reason: proto.ReasonStopped}, OpLost, false},
	} {
		got := EventRecord("rec-1", "mgr-1", 7, testNow, c.ev)
		if got.Op != c.wantOp {
			t.Errorf("%s became %s, want %s", c.ev.Kind, got.Op, c.wantOp)
		}
		if (got.Lease != nil) != c.hasLease {
			t.Errorf("%s carries a lease = %v, want %v", c.ev.Kind, got.Lease != nil, c.hasLease)
		}
		if got.Kind != c.ev.Kind || got.Reason != c.ev.Reason || got.Seq != 7 || got.Instance != "mgr-1" {
			t.Errorf("%s lost something on the way to a line: %+v", c.ev.Kind, got)
		}
	}
}

const wire53 = 53

// recordFixture is two records in two scopes that share an address, and one
// hardware address that appears in both.
func recordFixture(t *testing.T) []RecordEvent {
	t.Helper()
	l := testRecordLease("192.168.99.100/24")
	return []RecordEvent{
		{ID: "rec-1", Seq: 1, Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity, Instance: "p1"},
		{ID: "rec-1", Seq: 2, Op: OpBind, Instance: "p1"},
		{ID: "rec-2", Seq: 1, Op: OpCreate, Scope: "net-b", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity, Instance: "p1"},
		{ID: "rec-2", Seq: 2, Op: OpBind, Instance: "p1"},
		{ID: "rec-1", Seq: 3, Op: OpLease, Kind: Acquired, Lease: &l, Instance: "p1"},
		{ID: "rec-2", Seq: 3, Op: OpLease, Kind: Acquired, Lease: &l, Instance: "p1"},
	}
}

// TestTheFamilyIsWrittenOnceLikeTheIdentity is review round 1's finding 4.
//
// Of the three fields a record must not change under its own id — scope,
// identity, family — the family was the one that could be overwritten in
// silence: a v6 event on a v4 record left the record claiming v6 while the
// same event's scope change was refused. The family decides which wire the
// address is on, so a record that changed it would answer both lookups.
func TestTheFamilyIsWrittenOnceLikeTheIdentity(t *testing.T) {
	rec := recordAt(t, PhaseJoined)
	if rec.Family != FamilyV4 {
		t.Fatalf("the fixture record is in family %s, so this test measures nothing", rec.Family)
	}

	// Restating the same family is not a rewrite, and an event that names none
	// leaves the record's alone. Without both of these the refusal below could
	// be satisfied by refusing every event that carries a family at all.
	for _, ev := range []RecordEvent{
		{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLeave, Family: FamilyV4},
		{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLeave},
	} {
		got, err := Fold(rec, ev)
		if err != nil {
			t.Fatalf("an event carrying family %s was refused: %v", ev.Family, err)
		}
		if got.Family != FamilyV4 {
			t.Fatalf("family = %s after an event carrying %s, want v4", got.Family, ev.Family)
		}
	}

	// A second, different one is refused, whichever op carries it, and the
	// refused event moves nothing else.
	for _, op := range []RecordOp{OpLeave, OpRetain, OpClose, OpLease, OpStats} {
		ev := RecordEvent{
			ID: "rec-1", Seq: rec.Seq + 1, Op: op, Family: FamilyV6,
			CHAddr: testMAC2, Note: "would have been applied",
		}
		got, err := Fold(rec, ev)
		var rej *Reject
		if !errors.As(err, &rej) || rej.Reason != RejectFamily {
			t.Errorf("a v6 event on a v4 record via %s gave %v, want a RejectFamily", op, err)
			continue
		}
		want := rec
		want.Counters.Rejects++
		want.LastReject = rej.Reason
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the refused event moved the record: got %+v", op, got)
		}
	}

	// The family is still WRITABLE on a record that has none: the rule is
	// written once, not never.
	fresh, err := Fold(Record{}, RecordEvent{ID: "rec-9", Seq: 1, Op: OpCreate, Scope: "net-a", Family: FamilyV6})
	if err != nil {
		t.Fatalf("creating a v6 record was refused: %v", err)
	}
	if fresh.Family != FamilyV6 {
		t.Fatalf("family = %s on a record created as v6", fresh.Family)
	}
}

// TestRefusalsBeforeTheCreateAreTheJournalsNotTheRecords pins the boundary
// between the two reject counts, in the direction the second review measured:
// a journal whose create arrives LAST refuses everything ahead of it, and the
// record that finally exists carries none of those numbers.
//
// The alternative — materialising a record so it can carry them — is the thing
// TestARecordIsNeverInventedByTheFold forbids, and it is worse than the
// asymmetry: every lookup would then answer for an endpoint no create ever
// made. So the two counts answer two questions, Rebuilt.Rejects for the file
// and Record.Counters.Rejects for the endpoint, and this test is what stops
// either from being quietly redefined as the other.
func TestRefusalsBeforeTheCreateAreTheJournalsNotTheRecords(t *testing.T) {
	var evs []RecordEvent
	seq := uint64(0)
	add := func(ev RecordEvent) {
		seq++
		ev.ID, ev.Seq = "rec-1", seq
		evs = append(evs, ev)
	}
	// Five events for a record nothing has created yet.
	add(RecordEvent{Op: OpBind})
	add(RecordEvent{Op: OpLease, Kind: Acquired, Lease: ptr(testRecordLease("192.168.99.100/24"))})
	add(RecordEvent{Op: OpLeave})
	add(RecordEvent{Op: OpStats, Manager: "mgr-1", Stats: &Stats{Sent: 1}})
	add(RecordEvent{Op: OpRetain})
	// And then, late, the create.
	add(RecordEvent{Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity})

	rb := Rebuild(evs)

	if len(rb.Rejects) != 5 {
		t.Fatalf("the journal reports %d refusal(s), want 5: every event before the create is one", len(rb.Rejects))
	}
	if len(rb.Records) != 1 {
		t.Fatalf("%d record(s), want 1: a refusal before the create must not invent one", len(rb.Records))
	}
	rec, ok := rb.ByID("rec-1")
	if !ok {
		t.Fatal("the created record is missing")
	}
	if rec.Phase != PhaseCreated {
		t.Fatalf("phase = %s, want %s: the refusals ahead of the create must not have moved it", rec.Phase, PhaseCreated)
	}
	if rec.Counters.Rejects != 0 {
		t.Fatalf("the record carries %d refusal(s) of its own, want 0; five belong to the journal and none to an endpoint that did not exist when they arrived", rec.Counters.Rejects)
	}
	if rec.LastReject != RejectNone {
		t.Fatalf("LastReject = %s on a record whose own events were all accepted", rec.LastReject)
	}

	// The other direction, so the zero above is a boundary and not a fold that
	// stopped counting: a refusal AFTER the create lands on the record, and on
	// the journal as well.
	after := append(append([]RecordEvent(nil), evs...), RecordEvent{ID: "rec-1", Seq: seq + 1, Op: OpLeave})
	rb2 := Rebuild(after)
	rec2, _ := rb2.ByID("rec-1")
	if rec2.Counters.Rejects != 1 {
		t.Fatalf("a refusal after the create gave the record %d, want 1", rec2.Counters.Rejects)
	}
	if len(rb2.Rejects) != 6 {
		t.Fatalf("the journal reports %d, want 6", len(rb2.Rejects))
	}
}

// TestTwoManagersUnderOneIdCountAsOne is the direction chosen for the second
// review's finding 3, made explicit rather than left to be discovered.
//
// One manager id, two managers, the second's snapshot HIGHER than the first's
// final one: the fold reads it as one manager still running, because that is
// also exactly what a renewal looks like. There is no reading of the numbers
// that separates the two, so the wire counters undercount by the first
// manager's total and nothing is refused. The obligation is the caller's, and
// it is stated beside RecordEvent.Manager.
//
// The half that IS decidable — an id that comes BACK after another one — is
// refused, and has a row in TestARejectMovesNothingButItsOwnCount.
//
// This test exists to bound the damage: it asserts the undercount is confined
// to the wire counters and that phase, lease, identity and the folded counters
// are the truth regardless.
func TestTwoManagersUnderOneIdCountAsOne(t *testing.T) {
	rec := recordAt(t, PhaseJoined)
	l := testRecordLease("192.168.99.100/24")
	var err error
	if rec, err = Fold(rec, RecordEvent{ID: "rec-1", Seq: rec.Seq + 1, Op: OpLease, Kind: Acquired, Lease: &l}); err != nil {
		t.Fatalf("acquiring: %v", err)
	}

	// Manager one, under the shared id, ending at 8 sent.
	for _, sent := range []uint64{3, 8} {
		if rec, err = Fold(rec, RecordEvent{
			ID: "rec-1", Seq: rec.Seq + 1, Op: OpStats,
			Manager: "shared", Stats: &Stats{Sent: sent, Received: sent - 1, LeasesAcquired: 1},
		}); err != nil {
			t.Fatalf("manager one's snapshot at %d was refused: %v", sent, err)
		}
	}
	// Manager two, same id, its own first snapshot already past 8.
	if rec, err = Fold(rec, RecordEvent{
		ID: "rec-1", Seq: rec.Seq + 1, Op: OpStats,
		Manager: "shared", Stats: &Stats{Sent: 9, Received: 8, LeasesAcquired: 1},
	}); err != nil {
		t.Fatalf("the second manager's snapshot under the shared id was refused: %v", err)
	}

	// 9, not 17: the second manager's 9 is folded as one more than the first's
	// 8. That is the undercount, and it is the price of the field being the
	// caller's to make unique.
	if got := rec.Counters.Wire.Sent; got != 9 {
		t.Fatalf("Wire.Sent = %d, want 9; the fold treats a higher snapshot under one id as a continuation, which is what a renewal is", got)
	}
	if rec.Counters.Rejects != 0 {
		t.Fatalf("%d refusal(s), want 0: nothing in the numbers distinguishes a second manager from a renewal", rec.Counters.Rejects)
	}

	// The bound. Everything below is derived from the events themselves and is
	// unaffected by which manager wrote which snapshot.
	if !rec.Held || rec.Lease.Addr != l.Addr {
		t.Fatalf("the record holds %v (%s); the collision reached the lease", rec.Held, rec.Lease.Addr)
	}
	if rec.Phase != PhaseJoined {
		t.Fatalf("phase = %s, want %s", rec.Phase, PhaseJoined)
	}
	if !reflect.DeepEqual(rec.Identity, testIdentity) {
		t.Fatalf("identity = %x, want %x", rec.Identity, testIdentity)
	}
	if rec.Counters.Acquisitions != 1 {
		t.Fatalf("Acquisitions = %d, want 1: the folded counters come from the events, not the snapshots", rec.Counters.Acquisitions)
	}
}

// TestAFoldDoesNotChangeTheRecordItWasGiven drives the purity Fold's signature
// advertises, on the one piece of state that is a map and therefore shared by
// a plain struct copy: the set of manager ids the record has seen.
//
// A fold that writes into that map through its argument makes the OLD record
// answer differently after a fold it did not take part in. Two branches from
// one record is not hypothetical — the reject path returns the record it was
// given, so every refusal hands the caller a record another fold has already
// been applied to.
func TestAFoldDoesNotChangeTheRecordItWasGiven(t *testing.T) {
	before := recordAt(t, PhaseJoined)
	var err error
	if before, err = Fold(before, RecordEvent{
		ID: "rec-1", Seq: before.Seq + 1, Op: OpStats,
		Manager: "mgr-a", Stats: &Stats{Sent: 2},
	}); err != nil {
		t.Fatalf("the first manager's counters were refused: %v", err)
	}

	// One branch: mgr-b takes over.
	if _, err = Fold(before, RecordEvent{
		ID: "rec-1", Seq: before.Seq + 1, Op: OpStats,
		Manager: "mgr-b", Stats: &Stats{Sent: 1},
	}); err != nil {
		t.Fatalf("the second manager was refused: %v", err)
	}

	// The other branch, from the SAME record. mgr-b has never been seen by
	// this one, so it must be accepted exactly as it was above. If the fold
	// wrote mgr-b into the map it shares with `before`, this is refused as a
	// returning manager — a refusal invented by a fold on another branch.
	if _, err = Fold(before, RecordEvent{
		ID: "rec-1", Seq: before.Seq + 1, Op: OpStats,
		Manager: "mgr-b", Stats: &Stats{Sent: 1},
	}); err != nil {
		t.Fatalf("the second branch was refused: %v; a fold on the first branch changed the record it was handed", err)
	}
}
