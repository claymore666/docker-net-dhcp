package lease

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// testIdentity6 is a DUID-LLT followed by the IAID, which is what a v6 record
// carries: RFC 9915 section 21.2's Client Identifier bytes and section 21.4's
// IAID, AS SENT.
var testIdentity6 = append(append([]byte(nil), test6DUID...), 0x0a, 0x0b, 0x0c, 0x0d)

// TestAllEventKindsIsEveryDeclaredKind is M7a's defeat row D-1 for the
// enumeration this milestone widened.
//
// Configured was added to the constant block, and a slice that had not gained
// it would have SHRUNK the domain of every test that derives its rows from
// AllEventKinds — reporting a smaller domain as fully covered rather than
// failing. So this test's own domain is the EventKind integer space and not the
// slice: it walks up from zero and requires the run of declared kinds to be
// dense and to end where the slice ends.
func TestAllEventKindsIsEveryDeclaredKind(t *testing.T) {
	seen := map[EventKind]int{}
	for _, k := range AllEventKinds() {
		seen[k]++
	}
	for i := 0; i < 256; i++ {
		k := EventKind(i)
		named := !strings.HasPrefix(k.String(), "eventkind(")
		if named && seen[k] != 1 {
			t.Errorf("%s is a declared EventKind and appears %d time(s) in AllEventKinds()", k, seen[k])
		}
		if !named && seen[k] != 0 {
			t.Errorf("AllEventKinds() carries %d, which EventKind.String() does not name", i)
		}
		if !named {
			// Density from zero: the first unnamed value ends the
			// enumeration, so a gap in the constant block fails above rather
			// than shortening this walk in silence.
			for j := i; j < 256; j++ {
				if !strings.HasPrefix(EventKind(j).String(), "eventkind(") {
					t.Fatalf("eventkind(%d) is unnamed but eventkind(%d) is named: the enumeration is not dense", i, j)
				}
			}
			return
		}
	}
}

// TestAV6RecordIsRefusedWithoutTheDUIDAndIAID is the brief's "written once,
// refused empty", and RFC 9915 section 11's reason: the DUID "SHOULD NOT change
// over time if at all possible".
//
// THE v4 ROW IS THE PRESERVATION CONTROL. A v4 record identifies its client by
// option 61 OR by the hardware address in the exchange, so an empty identity
// there is a caller who has not adopted D10 and not a record that can never be
// resumed. Widening the refusal to both families would have been a behaviour
// change to a shipping client, so the row below proves it did not happen.
func TestAV6RecordIsRefusedWithoutTheDUIDAndIAID(t *testing.T) {
	for _, op := range []RecordOp{OpReserve, OpCreate, OpAdopt} {
		t.Run("v6 "+op.String(), func(t *testing.T) {
			ev := RecordEvent{ID: "rec-1", Seq: 1, Op: op, Scope: "net-a", Family: FamilyV6}
			got, err := Fold(Record{}, ev)
			var rej *Reject
			if !errors.As(err, &rej) || rej.Reason != RejectIdentity {
				t.Fatalf("Fold = %v, want a RejectIdentity", err)
			}
			if got.Phase != PhaseUnset {
				t.Errorf("the refused event brought a record into existence in phase %s", got.Phase)
			}

			// The same event WITH the identity is the other direction of the
			// guard: it must refuse only what it names.
			ev.Identity = testIdentity6
			ok, err := Fold(Record{}, ev)
			if err != nil {
				t.Fatalf("the same event carrying the identity was refused: %v", err)
			}
			if !reflect.DeepEqual(ok.Identity, testIdentity6) {
				t.Errorf("identity %x, want %x", ok.Identity, testIdentity6)
			}
			if ok.Family != FamilyV6 {
				t.Errorf("family %s, want %s", ok.Family, FamilyV6)
			}
		})
	}

	t.Run("a v4 record with no identity is untouched", func(t *testing.T) {
		got, err := Fold(Record{}, RecordEvent{ID: "rec-1", Seq: 1, Op: OpCreate, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC})
		if err != nil {
			t.Fatalf("a v4 create with no identity was refused: %v", err)
		}
		if got.Phase != PhaseCreated {
			t.Errorf("phase %s, want created", got.Phase)
		}
	})

	t.Run("a later event on a v6 record needs no identity of its own", func(t *testing.T) {
		rec, err := Fold(Record{}, RecordEvent{ID: "rec-1", Seq: 1, Op: OpCreate, Scope: "net-a", Family: FamilyV6, Identity: testIdentity6})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := Fold(rec, RecordEvent{ID: "rec-1", Seq: 2, Op: OpBind})
		if err != nil {
			t.Fatalf("a bind on a v6 record was refused: %v", err)
		}
		if !reflect.DeepEqual(got.Identity, testIdentity6) {
			t.Errorf("the identity changed to %x", got.Identity)
		}
	})
}

// TestAConfiguredEventGrantsNothing is the Configured arm of the fold, and its
// whole content is what it does NOT do.
//
// RFC 9915 section 18.2.6's exchange carries no address. A record that set Held
// here would claim an address on the strength of a message that has none, and
// one that cleared it would drop a live lease because the client refreshed its
// DNS servers — so the lease and Held are asserted UNCHANGED against the record
// as it stood.
func TestAConfiguredEventGrantsNothing(t *testing.T) {
	rec := v6RecordHoldingALease(t)
	before := rec

	cfg := Configuration{
		DNS:     []netip.Addr{netip.MustParseAddr(test6DNS)},
		Search:  []string{test6Search},
		Refresh: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	got, err := Fold(rec, RecordEvent{
		ID: rec.ID, Seq: rec.Seq + 1, Op: OpLease, Kind: Configured, Config: &cfg,
	})
	if err != nil {
		t.Fatalf("Fold(configured): %v", err)
	}
	if !got.Held {
		t.Error("a configured event cleared Held on a record holding a lease")
	}
	if !reflect.DeepEqual(got.Lease, before.Lease) {
		t.Errorf("a configured event changed the lease: %+v", got.Lease)
	}
	if got.Counters.Acquisitions != before.Counters.Acquisitions {
		t.Errorf("acquisitions %d, want %d", got.Counters.Acquisitions, before.Counters.Acquisitions)
	}
	if got.Counters.Configurations != before.Counters.Configurations+1 {
		t.Errorf("configurations %d, want %d", got.Counters.Configurations, before.Counters.Configurations+1)
	}
	if !reflect.DeepEqual(got.Config, cfg) {
		t.Errorf("the record's configuration is %+v, want %+v", got.Config, cfg)
	}

	// The stored configuration is a COPY: a caller that keeps its slice and
	// writes to it must not reach into the record.
	cfg.DNS[0] = netip.MustParseAddr("2001:db8::dead")
	cfg.Search[0] = "elsewhere.invalid"
	if got.Config.DNS[0].String() != test6DNS || got.Config.Search[0] != test6Search {
		t.Errorf("the record aliased the caller's slices: %+v", got.Config)
	}

	// And a Configured event with nothing in it is a reject rather than a
	// record that says the client was configured with no configuration.
	if _, err := Fold(rec, RecordEvent{ID: rec.ID, Seq: rec.Seq + 1, Op: OpLease, Kind: Configured}); err == nil {
		t.Error("a configured event with no configuration was folded")
	}
}

// TestTheDADPhaseIsFoldedLikeTheACDPhase walks every phase through the fold,
// on the lease arm and on the loss arm.
//
// The domain comes from proto.AllDADPhases() rather than from a list here, so
// a phase added to that enumeration and not to this test is a compile-time
// nothing and a runtime row that simply appears.
func TestTheDADPhaseIsFoldedLikeTheACDPhase(t *testing.T) {
	for _, ph := range proto.AllDADPhases() {
		t.Run(ph.String(), func(t *testing.T) {
			rec := v6RecordHoldingALease(t)
			got, err := Fold(rec, RecordEvent{
				ID: rec.ID, Seq: rec.Seq + 1, Op: OpLease, Kind: Renewed,
				Lease: ptr(testRecordLease6()), DAD: ph,
			})
			if err != nil {
				t.Fatalf("Fold(renewed): %v", err)
			}
			if got.DAD != ph {
				t.Errorf("the record reports %s, want %s", got.DAD, ph)
			}
			if got.ACD != proto.ACDIdle {
				t.Errorf("a v6 record reports the RFC 5227 phase %s", got.ACD)
			}

			lost, err := Fold(got, RecordEvent{
				ID: rec.ID, Seq: got.Seq + 1, Op: OpLost, Reason: proto.ReasonConflict, DAD: ph,
			})
			if err != nil {
				t.Fatalf("Fold(lost): %v", err)
			}
			if lost.DAD != ph {
				t.Errorf("after the loss the record reports %s, want %s", lost.DAD, ph)
			}
		})
	}
}

// TestTheManagersV6JournalReplays is the v4 journal-replay test's counterpart,
// and it is the observer for the journal round trip: an entry whose payload the
// journal dropped replays into a different action list, which Replay6 reports
// as a divergence rather than as a pass.
func TestTheManagersV6JournalReplays(t *testing.T) {
	p := testParams6()
	r := newRig6(t, p, answerNormally6(t))
	r.acquire6(t)

	// The Router Advertisement is in the journal on purpose: its payload is
	// the one this milestone had to add to the round trip.
	r.nd.inject(raManagedOther)
	r.journal.waitAppended(t, "the Router Advertisement", func(e proto.JournalEntry6) bool {
		return e.Kind == proto.EvRouterAdvert
	})

	// Stop first, so the journal is complete rather than being appended to
	// while it is read.
	_ = r.stop()

	entries := r.mgr.Journal6()
	if len(entries) == 0 {
		t.Fatal("the journal is empty")
	}
	res, err := proto.Replay6(p, entries)
	if err != nil {
		t.Fatalf("the manager's own v6 journal does not replay: %v", err)
	}

	// A ring-2 note is not a Step, and the journal this manager wrote has one
	// in it: the duplicate-address-detection request the fake answered. It is
	// counted here so "everything that was a Step replayed" is asserted
	// against the population rather than against the journal's length.
	notes := 0
	for _, e := range entries {
		if e.Note {
			notes++
		}
	}
	if notes == 0 {
		t.Fatal("this journal was expected to contain a ring-2 note; without one the skip below is untested")
	}
	if res.Steps != len(entries)-notes {
		t.Fatalf("replayed %d of %d entries (%d notes)", res.Steps, len(entries), notes)
	}

	// Replaying to the bind is the assertion that matters: a replay that only
	// reproduced the final state would agree with a machine that never leased.
	var upto []proto.JournalEntry6
	for _, e := range entries {
		upto = append(upto, e)
		if !e.Note && e.To == proto.State6Bound {
			break
		}
	}
	if len(upto) == len(entries) {
		t.Fatal("the journal never reached BOUND6")
	}
	res, err = proto.Replay6(p, upto)
	if err != nil {
		t.Fatalf("replay to BOUND6: %v", err)
	}
	if !res.Held {
		t.Fatal("replay to BOUND6 produced no lease")
	}
	if got := res.Lease.Addrs[0].Addr.String(); got != test6Addr {
		t.Errorf("the replayed lease is %s, want %s", got, test6Addr)
	}

	// A journal entry whose Router Advertisement payload went missing is a
	// DIVERGENCE and not a quiet pass: this is the mutant the round trip
	// exists to kill.
	stripped := append([]proto.JournalEntry6(nil), entries...)
	for i := range stripped {
		if stripped[i].Kind == proto.EvRouterAdvert {
			stripped[i].RA = nil
		}
	}
	if _, err := proto.Replay6(p, stripped); err == nil {
		t.Error("a journal with its Router Advertisement payloads removed replayed cleanly")
	}
}

// v6RecordHoldingALease is a created, bound v6 record with a lease in it.
func v6RecordHoldingALease(t *testing.T) Record {
	t.Helper()
	rec, err := Fold(Record{}, RecordEvent{
		ID: "rec-6", Seq: 1, Op: OpCreate, Scope: "net-a",
		Family: FamilyV6, Identity: testIdentity6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rec, err = Fold(rec, RecordEvent{ID: "rec-6", Seq: 2, Op: OpBind})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	rec, err = Fold(rec, RecordEvent{
		ID: "rec-6", Seq: 3, Op: OpLease, Kind: Acquired,
		Lease: ptr(testRecordLease6()), DAD: proto.DADPassed,
	})
	if err != nil {
		t.Fatalf("acquired: %v", err)
	}
	return rec
}

// testRecordLease6 is what the v6 manager reports on the fixture exchange.
func testRecordLease6() Lease {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	return Lease{
		Addr:       netip.MustParsePrefix(test6Addr + "/128"),
		ServerDUID: append([]byte(nil), test6ServerDUID...),
		IAID:       test6IAID,
		Preferred:  now.Add(300 * time.Second),
		Valid:      now.Add(300 * time.Second),
		Expire:     now.Add(300 * time.Second),
		Renew:      now.Add(150 * time.Second),
		Rebind:     now.Add(240 * time.Second),
	}
}
