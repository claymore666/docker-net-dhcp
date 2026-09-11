// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Records is the plugin's durable lease record: one append-only JSONL
// file, folded on read, written by exactly one process.
//
// WHY THE CHASSIS OWNS IT AND NOT THE LIBRARY. The library's ring 2
// declares lease.Store and ships a JSONL implementation, and the fold is
// total over (phase, op) — all of that is the library's. What is NOT the
// library's is which endpoint, which container, which Docker network a
// line belongs to: the ring gate forbids the library learning a Docker
// field. So the chassis holds the file, supplies the record id and the
// scope, and hands the library nothing but its own event types.
//
// G-10, THE ONE-WRITER GUARANTEE, AND WHERE IT COMES FROM. lease.Store's
// contract is that one Append is one write, which makes two writers
// interleave as two whole lines rather than one corrupt one. It does not
// make two writers CORRECT: RecordEvent.Seq is strictly increasing per
// record, and two writers minting sequence numbers from their own
// memories produce a stream where one writer's events are rejected as
// stale — silently, because a rejected event still folds into a record
// with its Rejects counter bumped and nothing else moved. A restart
// would then resume from a record that is missing half its history.
//
// The guarantee is CONSTRUCTED, not asserted, and it is constructed
// twice because the two hazards are different:
//
//   - Another PROCESS on the same file — the upgrade window, where the
//     old plugin has not exited when the new one starts. Closed by an
//     flock(LOCK_EX|LOCK_NB) held on a sidecar lock file for the life of
//     the Records. A second opener is refused with ErrRecordsLocked
//     rather than admitted to a file it would corrupt.
//   - Another Records in THIS process. flock also closes this one: the
//     lock is associated with the open file description, and a second
//     os.OpenFile creates a second description, so the second flock in
//     one process conflicts exactly as a second process's would.
//     TestRecords_SecondOpenIsRefused drives it in-process for that
//     reason.
//
// The filesystem can refuse to lock at all — an NFS mount without lockd.
// That does not leave either hazard open: flock failing for ANY reason is
// refused as ErrRecordsLocked and NewPlugin gives up on it (D51), so the
// FIRST opener does not start and the guarantee is kept by refusing to
// run. Since v1.5.0 the state directory is a bind of a fixed host path,
// not the plugin's own rootfs, so which filesystem sits under the lock is
// the host's choice. The bound is on where the plugin can run, not on
// whether two writers can overlap; docs/reference.md states it where an
// operator picks the mount. Which of the two the operator is looking at
// is read off the errno and said in the refusal — lockRefused.
type Records struct {
	path string

	store *dhcpruntime.RecordStore
	lock  *os.File

	mu  sync.Mutex
	seq map[string]uint64

	// instance names this PROCESS, and is what distinguishes two plugin
	// processes' lines in one file during an upgrade. It is not the
	// manager: one process runs the CreateEndpoint one-shot manager and
	// then the Join manager, and giving those two one id freezes the
	// wire counters at the first one's totals.
	instance string

	managers atomic.Uint64
}

// ErrRecordsLocked is a refused start: the exclusive lock on the lease
// record was not taken. Every reading lockRefused produces matches it,
// including the ones that are not a second writer.
var ErrRecordsLocked = errors.New("dhcp: the lease record file is already open by another writer")

// lockRefusal is one reading of a failed flock. Error prints the
// reading; Unwrap hands back both ErrRecordsLocked, which is what a
// caller matches a refused start on, and the errno the reading was
// derived from.
type lockRefusal struct {
	msg   string
	errno error
}

func (e *lockRefusal) Error() string   { return e.msg }
func (e *lockRefusal) Unwrap() []error { return []error{ErrRecordsLocked, e.errno} }

// lockRefused turns the errno flock returned into the sentence an
// operator can act on (#950).
//
// THE TWO READINGS LEAD TO OPPOSITE ACTIONS, which is why one text for
// both was a defect rather than a wording preference: a held lock is
// cleared by disabling whatever holds it, and a mount that cannot lock
// is not cleared by disabling anything. The errno is the only evidence
// that separates them at the moment of the refusal.
//
// EAGAIN and EOPNOTSUPP each stand for a PAIR: EWOULDBLOCK is the same
// value as the first on Linux and ENOTSUP the same as the second, so
// spelling both members is a duplicate case. The test table names all
// four and goes red on a build that splits a pair.
//
// An errno in neither list keeps the generic text and prints the number
// beside it. Guessing a remedy from an errno we have not thought about
// is how one text came to cover two causes.
func lockRefused(path string, err error) error {
	switch {
	case errors.Is(err, unix.EAGAIN):
		return &lockRefusal{
			msg: fmt.Sprintf("dhcp: another tag of this plugin is enabled and holds the lease record %s; "+
				"disable it before enabling this one", path),
			errno: err,
		}
	case errors.Is(err, unix.ENOLCK), errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return &lockRefusal{
			msg: fmt.Sprintf("dhcp: the filesystem under %s does not support locks; "+
				"the plugin refuses to start rather than risk two writers", path),
			errno: err,
		}
	default:
		return &lockRefusal{
			msg:   fmt.Sprintf("%v (%s): %v", ErrRecordsLocked, path, err),
			errno: err,
		}
	}
}

// OpenRecords opens or creates the record file at path.
//
// instance names the writing process. Callers pass the plugin's instance
// id, which is minted per process, so a line can always be attributed
// even when two plugin processes overlapped.
func OpenRecords(path, instance string) (*Records, error) {
	if instance == "" {
		return nil, fmt.Errorf("dhcp: a record store needs an instance id: an unattributed line cannot be told from another process's")
	}

	// Taken BEFORE the store is opened. OpenRecordStore repairs a torn
	// tail by appending a newline, which is a write, and a repair racing
	// another live writer is the corruption this lock exists to prevent.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dhcp: lock file for %s: %w", path, err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, lockRefused(path, err)
	}

	store, err := dhcpruntime.OpenRecordStore(path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}

	r := &Records{path: path, store: store, lock: lock, seq: map[string]uint64{}, instance: instance}

	// The sequence floor comes from the FILE, not from zero. A process
	// that restarted and began numbering at 1 would have every event
	// after the first refused as stale, and a refusal is not an error
	// the writer sees: Fold returns the record with Rejects bumped.
	evs, err := store.Load()
	if err != nil {
		_ = store.Close()
		_ = lock.Close()
		return nil, err
	}
	for _, ev := range evs {
		if ev.Seq > r.seq[ev.ID] {
			r.seq[ev.ID] = ev.Seq
		}
	}
	return r, nil
}

// Close releases the file and the lock.
func (r *Records) Close() error {
	err := r.store.Close()
	// The lock is released by the close of the last descriptor on the
	// open file description, so this is the release.
	if cerr := r.lock.Close(); err == nil {
		err = cerr
	}
	return err
}

// Damage is what the store could not read: a torn tail from a crash, or
// unreadable lines anywhere else.
func (r *Records) Damage() lease.StoreDamage { return r.store.Damage() }

// Rebuilt folds the whole file.
func (r *Records) Rebuilt() (lease.Rebuilt, error) {
	evs, err := r.store.Load()
	if err != nil {
		return lease.Rebuilt{}, err
	}
	return lease.Rebuild(evs), nil
}

// NewManagerID mints an id no other manager instance in any process will
// get.
//
// Unique per MANAGER, which is narrower than per endpoint, per interface
// or per process — all three of which repeat, and any of which folded
// two managers' counters into one. The instance id is random per plugin
// process and the counter is per process, so the pair is unique across
// restarts as well as within one.
func (r *Records) NewManagerID() string {
	return fmt.Sprintf("%s-m%d", r.instance, r.managers.Add(1))
}

// append stamps the envelope this chassis owns — the record id's next
// sequence number and the writing process — and writes one line.
//
// Sequence numbers are handed out under the same lock that writes, so
// two goroutines on two endpoints cannot swap their line order within
// one record. Across records the order does not matter: Seq is per
// record, and Fold compares it only against that record's own.
func (r *Records) append(ev lease.RecordEvent) error {
	if ev.ID == "" {
		return fmt.Errorf("dhcp: a record event with no record id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq[ev.ID]++
	ev.Seq = r.seq[ev.ID]
	ev.Instance = r.instance
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if err := r.store.Append(ev); err != nil {
		// Give the number back: an Append that did not land must not
		// burn a sequence number, or the next event is a gap and the
		// fold has no way to tell a gap from a lost line.
		r.seq[ev.ID]--
		return err
	}
	return nil
}

// Created is the CREATED record: the link exists and this identity is
// bound to it. One per endpoint, written at CreateEndpoint.
//
// Identity is the option-61 value AS SENT, including its type byte
// (D10). It is write-once in the fold: a second Created with different
// bytes is refused rather than overwritten, which is the whole point of
// storing it instead of re-deriving it from a MAC that changes.
func (r *Records) Created(id, scope string, chaddr, identity []byte) error {
	return r.append(lease.RecordEvent{
		ID:       id,
		Op:       lease.OpCreate,
		Scope:    scope,
		Family:   lease.FamilyV4,
		CHAddr:   chaddr,
		Identity: identity,
	})
}

// Reserved opens a record for an address answered BEFORE any endpoint
// exists: the IPAM driver's RequestAddress, which runs before libnetwork
// has a link to bind to (#110).
//
// It is a distinct phase and not an early Created because the two are
// answerable by different questions. A RESERVED record holds an address
// and no link, so a restart must be able to tell "Docker was told about
// this address" from "a container is using it": the first is swept and
// retained, the second is resumed. The fold admits Create after Reserve,
// which is how CreateEndpoint later binds the link to THIS record rather
// than opening a second one for the same address.
//
// Identity is written here and once, for the same reason Created writes
// it: the option-61 value as sent is what the server files the lease
// under, and the reserve's exchange is the one that put it there.
func (r *Records) Reserved(id, scope string, chaddr, identity []byte) error {
	return r.append(lease.RecordEvent{
		ID:       id,
		Op:       lease.OpReserve,
		Scope:    scope,
		Family:   lease.FamilyV4,
		CHAddr:   chaddr,
		Identity: identity,
	})
}

// Rebound consumes a tombstone under a new hardware address.
//
// The identity is NOT re-sent and must not be: it is write-once in the
// fold and it is the whole reason a re-bind can keep an address at all.
// Docker mints a fresh MAC for every endpoint, so the address survives a
// restart only because the client-id the server files the lease under
// does not change with it. The CHAddr does change, and the fold accepts
// that -- it is the one identifying field of the three that is not
// write-once.
func (r *Records) Rebound(id string, chaddr []byte) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpRebind, CHAddr: chaddr})
}

// Scope6 is the record scope a DHCPv6 endpoint's record lives in: the
// Docker network id, marked.
//
// A DUAL-STACK ENDPOINT NEEDS TWO RECORDS AND THEY MUST NOT COLLIDE.
// lease.Record binds ONE family (Record.Family is write-once) and one
// Identity (also write-once, and a v6 record with an empty one is
// refused), so the v4 client-id and the v6 DUID cannot share a record.
// The index that finds a record is (scope, chaddr) — Rebuilt.ByScopeMAC
// — and a dual-stack endpoint has one chaddr, so with one scope the two
// records would be two matches on every lookup and "the newest wins"
// would hand a v4 manager the v6 record roughly half the time.
//
// The scope is the only half of the key the chassis owns, so it is the
// half that carries the split. Marked rather than hashed so that a
// human reading the record file can see which family a line belongs to;
// '#' is not in a Docker network id.
//
// It is applied INSIDE Created6 and Resume6 rather than by the caller,
// which is what makes "a v6 record cannot be filed under the v4 scope"
// a property of this file instead of a rule every call site remembers.
func Scope6(networkID string) string { return networkID + "#v6" }

// Created6 is Created for a DHCPv6 endpoint.
//
// identity is Identity6.Bytes(): the DUID as sent, with the IAID. It is
// write-once in the fold and it is REQUIRED — the library refuses a v6
// record without one — because it is the whole reason the record
// exists. RFC 9915 section 11: a DUID "SHOULD NOT change over time if
// at all possible", and an identity re-derived on every plugin start
// from whatever the plumbing happens to look like then is one that
// changes.
func (r *Records) Created6(id, networkID string, chaddr, identity []byte) error {
	// REFUSED HERE AND NOT LEFT TO THE FOLD. The library rejects a v6
	// create with no identity, but it rejects it at REBUILD, and the
	// rebuild drops the offending record and carries on -- Append
	// returns nil, Rebuilt returns nil, and the endpoint simply has no
	// v6 record from then on. Every plugin restart then mints a fresh
	// DUID, the server files each one as a new client, and the only
	// symptom is an address that changes for no reason.
	if len(identity) == 0 {
		return fmt.Errorf("dhcp: a DHCPv6 record for %v carries no identity "+
			"(RFC 9915 section 11: the DUID is what makes this the same client after a restart)", id)
	}
	return r.append(lease.RecordEvent{
		ID:       id,
		Op:       lease.OpCreate,
		Scope:    Scope6(networkID),
		Family:   lease.FamilyV6,
		CHAddr:   chaddr,
		Identity: identity,
	})
}

// Resume6 is Resume in the v6 scope, and it hands back the stored
// identity as well as the lease.
//
// TWO ANSWERS BECAUSE A v6 MANAGER NEEDS BOTH, and only one of them is
// optional. The lease is what makes the first message on the wire RFC
// 9915 section 18.2.12's Confirm instead of a Solicit (#820); the
// identity is what makes it the SAME client either way. A restart that
// resumed the lease under a freshly minted DUID would Confirm a binding
// the server files under a different client and be told NotOnLink.
func (r *Records) Resume6(networkID string, chaddr []byte, now time.Time) (string, Resumption, Identity6, bool) {
	id, res, ok := r.Resume(Scope6(networkID), chaddr, now)
	if !ok {
		return "", Resumption{}, Identity6{}, false
	}
	return id, res, r.identity6(id), true
}

// identity6 reads back the DUID and IAID a record was created with.
//
// An unreadable or absent identity comes back as the zero value, which
// the caller reads as "mint a fresh one": that is the honest answer for
// a record written by a build that had none, and buildParams6 refuses
// the zero value rather than sending it.
func (r *Records) identity6(id string) Identity6 {
	rb, err := r.Rebuilt()
	if err != nil {
		return Identity6{}
	}
	for _, rec := range rb.Records {
		if rec.ID != id || len(rec.Identity) == 0 {
			continue
		}
		ident, err := ParseIdentity6(rec.Identity)
		if err != nil {
			log.WithError(err).WithField("record", id).
				Warn("The stored DHCPv6 identity could not be read back; a fresh one will be minted and the server will see a new client")
			return Identity6{}
		}
		return ident
	}
	return Identity6{}
}

// Bound starts a manager on the record: CREATED (or ADOPTED) becomes
// JOINED.
//
// Written by the PLUGIN and not by the chassis, because it is a
// statement about the endpoint's lifecycle rather than about the
// exchange: the CreateEndpoint one-shot runs against a CREATED record
// and must not move it, and a record that a previous plugin process
// already left JOINED must not be bound a second time. The manager's
// own half — its events, its Params snapshot and its counters — is
// written by the chassis.
func (r *Records) Bound(id string) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpBind})
}

// Adopted takes over an address Docker reports with no record behind
// it: the endpoint predates this file, or the file was lost.
func (r *Records) Adopted(id, scope string, chaddr, identity []byte) error {
	return r.append(lease.RecordEvent{
		ID:       id,
		Op:       lease.OpAdopt,
		Scope:    scope,
		Family:   lease.FamilyV4,
		CHAddr:   chaddr,
		Identity: identity,
	})
}

// Observed writes one manager event.
//
// It routes through lease.EventRecord rather than choosing the op here,
// because "a Lost is OpLost" is the one arm of the fold that knows
// ReasonStopped is not a loss. A call site that picked OpLease for it
// would be refused rather than mis-folded, but only because that mapping
// lives in one function.
// params, when non-nil, is the manager's Params snapshot. It rides the
// manager's FIRST event rather than a line of its own, because a record
// without the Params that produced its journal is not replayable
// (proto.Replay takes Params) and a separate line could be the one lost.
func (r *Records) Observed(id string, ev lease.Event, params *proto.Params) error {
	rev := lease.EventRecord(id, r.instance, 0, time.Now(), ev)
	if params != nil {
		p := lease.SnapshotParams(*params)
		rev.Params = &p
	}
	return r.append(rev)
}

// Left stops the manager and keeps the last lease snapshot. No RELEASE
// goes on the wire (D-7, #800): the address is left to expire on the
// server, exactly as any other host on the segment leaves it.
func (r *Records) Left(id string) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpLeave})
}

// Retained lays the tombstone. deadline is the chassis's
// min(lease expiry, tombstone TTL).
func (r *Records) Retained(id string, deadline time.Time) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpRetain, Deadline: deadline})
}

// Closed ends the record.
func (r *Records) Closed(id string) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpClose})
}

// Counted merges one manager's counter snapshot into the record.
//
// manager MUST be the id NewManagerID returned for that manager
// instance and no other. A snapshot naming no manager is refused; an id
// that comes back after a different one is refused; two managers handed
// ONE id are folded as one and nothing detects it — see
// lease.RecordEvent.Manager.
func (r *Records) Counted(id, manager string, s lease.Stats) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpStats, Manager: manager, Stats: &s})
}

// Resumption is what a record offers a manager that is about to start.
//
// Two fields and not one, because the two are different messages on the
// wire and the difference is the whole of RFC 2131 section 3.2 versus
// section 4.4.1. Lease non-nil means the record still holds an
// unexpired address, and the first packet is an INIT-REBOOT
// DHCPREQUEST: the server either confirms the address or NAKs, and a
// container keeps the IP it had across a plugin restart. Prefer means
// the record's address is one this endpoint would LIKE but has no claim
// to — a tombstone's, or an expired lease's — and it goes out as option
// 50 in an ordinary DHCPDISCOVER, which a server may ignore.
//
// They are never both set: Record.Prefer refuses whatever Record.Resume
// answers, by construction in the library.
type Resumption struct {
	Lease  *lease.Lease
	Prefer string
	Phase  string

	// ACD is where RFC 5227's check stood at this record's last lease
	// event, and it is a DIFFERENT phase from the field above: Phase is
	// the endpoint's lifecycle (created, joined, left, retained) and
	// this is the conflict-detection sub-machine's.
	//
	// D23 IS WHY IT IS DURABLE. A proto.ConflictAsync client is told
	// Acquired while section 2.1's check is still running, so a plugin
	// that dies in that window and rebuilds from this file is resuming
	// an address nothing ever cleared -- and without the phase that
	// record is byte-for-byte the record of an address that passed.
	// proto.ACDProbing and proto.ACDSettling are the unchecked values;
	// proto.ACDAnnouncing and proto.ACDDefending mean section 2.1
	// completed; proto.ACDIdle is the honest value for a
	// proto.ConflictOff client, which runs no check at all.
	ACD proto.ACDPhase
}

// ACDUnfinished reports whether this record is EVIDENCE OF A CHECK
// THAT WAS STILL RUNNING when the process that wrote it stopped.
//
// IT IS NOT THE NEGATION OF "cleared", and the difference is the whole
// function. proto.ACDIdle means the sub-machine holds nothing: it is
// what a proto.ConflictOff client writes, what every record written
// before M6 says, and -- the case that matters here -- what the fold
// records at the end of EVERY ordinary acquisition, because cancelling
// a manager drops the lease and the ACD sub-machine goes idle with it
// (the library's Record.ACD says "the phase at the loss, whatever the
// reason"). Reading idle as "not cleared" put a warning on the healthy
// path of every single container start; MEASURED on the 2.x lane,
// 2026-09-04.
//
// So idle is named here explicitly, with its reason, rather than left
// to fall out of a negation. Everything else that is not one of the
// two finished phases -- proto.ACDProbing, proto.ACDSettling, and any
// phase the library adds later -- is unfinished, which keeps the
// asymmetry that matters: an unknown phase costs a log line rather
// than silently reading as clean.
//
// It is DIAGNOSTIC ONLY. D23's "a restart during the window resumes
// the check" is delivered by the library, which re-runs section 2.1 on
// the INIT-REBOOT DHCPACK whatever the record said; this is the
// chassis's evidence about what the previous process was in the middle
// of, and the only place that evidence exists at all.
func (r Resumption) ACDUnfinished() bool {
	switch r.ACD {
	case proto.ACDAnnouncing, proto.ACDDefending, proto.ACDIdle:
		return false
	default:
		return true
	}
}

// Resume finds the record for one identity on one network and says what
// a new manager may ask for.
//
// Keyed on scope AND hardware address. Either alone is wrong for a
// reason the library states: an index on the address alone collapses
// two networks that hand out the same private address into one record,
// and an index on the MAC alone does the same to one machine on two
// networks.
//
// More than one record can match — a tombstone and its successor share
// a MAC — so the newest non-closed one wins. "Newest" is position in
// the file: Rebuild appends in creation order, and a re-bind consumes
// the tombstone rather than making a second record.
func (r *Records) Resume(scope string, chaddr []byte, now time.Time) (string, Resumption, bool) {
	rb, err := r.Rebuilt()
	if err != nil {
		return "", Resumption{}, false
	}
	matches := rb.ByScopeMAC(scope, chaddr)
	for i := len(matches) - 1; i >= 0; i-- {
		rec := matches[i]
		if rec.Phase == lease.PhaseClosed {
			continue
		}
		res := Resumption{Phase: rec.Phase.String(), ACD: rec.ACD}
		if l, ok := rec.Resume(now); ok {
			res.Lease = &l
		} else if a, ok := rec.Prefer(now); ok {
			res.Prefer = a.String()
		}
		return rec.ID, res, true
	}
	return "", Resumption{}, false
}
