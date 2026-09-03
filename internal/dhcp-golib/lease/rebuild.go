package lease

import (
	"errors"
	"net/netip"
	"time"
)

// Rebuilt is what a journal folds to: the records the event stream names, in
// the order they were created, plus everything the fold refused.
type Rebuilt struct {
	Records []Record
	// Rejects is every refusal in the journal, including the ones that
	// arrived before their record existed and therefore appear in no
	// Record.Counters.Rejects. It is the journal's account, not the sum of
	// the records'.
	Rejects []Reject

	byID map[string]int
}

// ByID returns one record.
func (rb Rebuilt) ByID(id string) (Record, bool) {
	i, ok := rb.byID[id]
	if !ok {
		return Record{}, false
	}
	return rb.Records[i], true
}

// ByScopeAddr and ByScopeMAC are the ONLY two lookups.
//
// That is a constraint from the caller's side, not a shortage here. The three
// paths that ask a driver about an address — a fresh request, the request
// replayed after a restart, and the release — carry between them only a pool
// identifier, an address, and a hardware address if one was demanded. A lookup
// keyed on anything else works in the path that has that field and is a
// rewrite in the two that do not.
//
// Both are keyed on the PAIR. An index on the address alone collapses two
// networks that hand out the same private address into one record, and an
// index on the MAC alone does the same to one machine on two networks.
//
// A slice and not a single record: more than one record can match — a
// tombstone and its successor share a MAC — and "exactly one match" is a rule
// the caller applies to this set, with its own narrowing.
func (rb Rebuilt) ByScopeAddr(scope string, addr netip.Addr) []Record {
	var out []Record
	for _, r := range rb.Records {
		if r.Scope != scope {
			continue
		}
		if a, ok := r.Addr(); ok && a == addr {
			out = append(out, r)
		}
	}
	return out
}

// ByScopeMAC is ByScopeAddr keyed on the hardware address.
func (rb Rebuilt) ByScopeMAC(scope string, mac []byte) []Record {
	var out []Record
	for _, r := range rb.Records {
		if r.Scope == scope && len(mac) > 0 && bytesEqual(r.CHAddr, mac) {
			out = append(out, r)
		}
	}
	return out
}

// Tombstones is the re-bind candidate set for a scope: RETAINED records whose
// deadline has not passed. A record with no deadline is not a candidate — an
// unbounded tombstone is a lease nothing ever gives back.
func (rb Rebuilt) Tombstones(scope string, now time.Time) []Record {
	var out []Record
	for _, r := range rb.Records {
		if r.Scope == scope && r.Phase == PhaseRetained && !r.Deadline.IsZero() && r.Deadline.After(now) {
			out = append(out, r)
		}
	}
	return out
}

// Rebuild folds a whole journal.
//
// It does NOT abort on a refused event. A durable log's value is that the
// records before a bad line survive it; a rebuild that refuses the file loses
// every one of them, which is the opposite of what the log is for. Every
// refusal is collected, and a caller that wants strictness reads Rejects.
//
// The order of Records is the order the records were CREATED, which is a
// function of the input rather than of a map iteration: a rebuild that
// answered in a different order each run would make every downstream
// comparison a flake.
func Rebuild(evs []RecordEvent) Rebuilt {
	rb := Rebuilt{byID: make(map[string]int)}
	for _, ev := range evs {
		i, known := rb.byID[ev.ID]
		var cur Record
		if known {
			cur = rb.Records[i]
		}
		next, err := Fold(cur, ev)
		if err != nil {
			var rj *Reject
			if errors.As(err, &rj) {
				rb.Rejects = append(rb.Rejects, *rj)
			}
		}
		if known {
			rb.Records[i] = next
			continue
		}
		if next.Phase == PhaseUnset {
			// The event was refused before it could bring a record into
			// existence. Recording the empty record would invent one — a
			// record with no create behind it, answering lookups for an
			// endpoint nothing made.
			//
			// So the refusal is counted in one place only: the Rejects slice
			// has it, and no Record.Counters.Rejects ever will, because there
			// is no record it belongs to. A caller auditing a journal reads
			// this slice; one auditing an endpoint reads that counter; the two
			// numbers are different questions and do not have to agree. Pinned
			// by TestRefusalsBeforeTheCreateAreTheJournalsNotTheRecords.
			continue
		}
		rb.byID[ev.ID] = len(rb.Records)
		rb.Records = append(rb.Records, next)
	}
	return rb
}

// EventRecord turns one manager event into one durable line.
//
// It exists so that "a Lost is OpLost" is a data dependency rather than a
// convention every call site has to remember: OpLost's fold is the only arm
// that knows the trailing stop is not a loss, and an event routed to OpLease
// instead would be rejected rather than mis-folded — loud, but only because
// this function is the one place the mapping is made.
func EventRecord(id, instance string, seq uint64, at time.Time, ev Event) RecordEvent {
	out := RecordEvent{
		ID:       id,
		Seq:      seq,
		At:       at,
		Instance: instance,
		Kind:     ev.Kind,
		Reason:   ev.Reason,
		Note:     ev.Note,
	}
	if ev.Kind == Lost {
		out.Op = OpLost
		return out
	}
	out.Op = OpLease
	if ev.Kind != Failed {
		l := CloneLease(ev.Lease)
		out.Lease = &l
	}
	return out
}
