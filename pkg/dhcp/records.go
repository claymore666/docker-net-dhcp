// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// A second opener, in another process or this one, is refused with ErrRecordsLocked: flock binds to the open file
// description, so two os.OpenFile calls in one process conflict too. A filesystem that cannot lock (NFS without lockd)
// is refused the same way, and NewPlugin does not start (D51, #950). Since v1.5.0 the state directory is a bind of a
// fixed host path, so the host chooses the filesystem under the lock.

// Records is the plugin's durable lease record, one append-only JSONL file written by exactly one process (#950).
type Records struct {
	path string

	store *dhcpruntime.RecordStore
	lock  *os.File

	mu  sync.Mutex
	seq map[string]uint64

	// instance names the process, not the manager; one id for two managers freezes the wire counters (#950).
	instance string

	managers atomic.Uint64
}

// ErrRecordsLocked is the refused start when the exclusive lock on the lease record was not taken.
var ErrRecordsLocked = errors.New("dhcp: the lease record file is already open by another writer")

// lockRefusal is one reading of a failed flock; Unwrap yields ErrRecordsLocked and the errno it came from.
type lockRefusal struct {
	msg   string
	errno error
}

func (e *lockRefusal) Error() string   { return e.msg }
func (e *lockRefusal) Unwrap() []error { return []error{ErrRecordsLocked, e.errno} }

// EAGAIN equals EWOULDBLOCK and EOPNOTSUPP equals ENOTSUP on Linux, so each case names one of a pair (#950).

// lockRefused turns the flock errno into the remedy, as a held lock and an unlockable mount need opposite actions.
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

// OpenRecords opens or creates the record file at path for the writing process instance.
func OpenRecords(path, instance string) (*Records, error) {
	if instance == "" {
		return nil, fmt.Errorf("dhcp: a record store needs an instance id: an unattributed line cannot be told from another process's")
	}

	// Locked before the store opens: OpenRecordStore repairs a torn tail with a write (#950).
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

	// The sequence floor comes from the file; restarting at 1 makes Fold reject later events as stale, silently (#950).
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
	if cerr := r.lock.Close(); err == nil {
		err = cerr
	}
	return err
}

// Damage is what the store could not read, a torn tail or unreadable lines.
func (r *Records) Damage() lease.StoreDamage { return r.store.Damage() }

// Instance is the id every line this store writes is stamped with (#1047).
func (r *Records) Instance() string { return r.instance }

// Rebuilt folds the whole file.
func (r *Records) Rebuilt() (lease.Rebuilt, error) {
	evs, err := r.store.Load()
	if err != nil {
		return lease.Rebuilt{}, err
	}
	return lease.Rebuild(evs), nil
}

// NewManagerID mints an id unique per manager instance across processes and restarts.
func (r *Records) NewManagerID() string {
	return fmt.Sprintf("%s-m%d", r.instance, r.managers.Add(1))
}

// append stamps the record's next sequence number and the writing process, and writes one line.
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
		// An Append that did not land gives its sequence number back; a gap reads as a lost line in the fold (#950).
		r.seq[ev.ID]--
		return err
	}
	return nil
}

// Created opens the record at CreateEndpoint, binding the option-61 identity as sent, write-once (D10).
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

// Reserved opens a record for an address answered by the IPAM driver before any endpoint exists (#110).
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

// Rebound consumes a tombstone under a new hardware address, keeping the write-once identity (#110).
func (r *Records) Rebound(id string, chaddr []byte) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpRebind, CHAddr: chaddr})
}

// Scope6 is the record scope of a DHCPv6 endpoint, so a dual-stack endpoint's two records do not collide (#911).
func Scope6(networkID string) string { return networkID + scope6Marker }

// scope6Marker is the suffix that makes a v6 scope; '#' is not in a Docker network id (#984).
const scope6Marker = "#v6"

// NetworkOfScope is Scope6 backwards: the network id and whether the scope is the v6 half (#984).
func NetworkOfScope(scope string) (networkID string, v6 bool) {
	if id, found := strings.CutSuffix(scope, scope6Marker); found {
		return id, true
	}
	return scope, false
}

// RFC 9915 section 11: a DUID "SHOULD NOT change over time if at all possible".

// Created6 is Created for a DHCPv6 endpoint, with the DUID and IAID as its required identity (#911).
func (r *Records) Created6(id, networkID string, chaddr, identity []byte) error {
	// The library drops an identity-less v6 record at rebuild with no error, and each restart would mint a new DUID
	// (#911).
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

// The lease makes the first message a Confirm (RFC 9915 section 18.2.12, #820); a new DUID would be told NotOnLink.

// Resume6 is Resume in the v6 scope, handing back the stored identity as well as the lease (#911).
func (r *Records) Resume6(networkID string, chaddr []byte, now time.Time) (string, Resumption, Identity6, bool) {
	id, res, ok := r.Resume(Scope6(networkID), chaddr, now)
	if !ok {
		return "", Resumption{}, Identity6{}, false
	}
	return id, res, r.identity6(id), true
}

// identity6 reads back a record's DUID and IAID, or the zero value, which buildParams6 refuses to send.
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

// Bound starts a manager on the record, moving CREATED or ADOPTED to JOINED.
func (r *Records) Bound(id string) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpBind})
}

// Adopted takes over an address Docker reports with no record behind it.
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

// Observed writes one manager event, with the Params snapshot riding the manager's first event when params is non-nil.
func (r *Records) Observed(id string, ev lease.Event, params *proto.Params) error {
	rev := lease.EventRecord(id, r.instance, 0, time.Now(), ev)
	if params != nil {
		p := lease.SnapshotParams(*params)
		rev.Params = &p
	}
	return r.append(rev)
}

// Left stops the manager and keeps the last lease snapshot, for a teardown that released nothing (#800, #962).
func (r *Records) Left(id string) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpLeave})
}

// Retained lays the tombstone with deadline min(lease expiry, tombstone TTL).
func (r *Records) Retained(id string, deadline time.Time) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpRetain, Deadline: deadline})
}

// Closed ends the record.
func (r *Records) Closed(id string) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpClose})
}

// Counted merges the counter snapshot of the manager NewManagerID named into the record.
func (r *Records) Counted(id, manager string, s lease.Stats) error {
	return r.append(lease.RecordEvent{ID: id, Op: lease.OpStats, Manager: manager, Stats: &s})
}

// Lease is an INIT-REBOOT DHCPREQUEST (RFC 2131 section 3.2); Prefer is option 50 in a DHCPDISCOVER (RFC 2131 section
// 4.4.1). The library never sets both.

// Resumption is what a record offers a manager about to start: a lease to resume or an address to prefer.
type Resumption struct {
	Lease  *lease.Lease
	Prefer string
	Phase  string

	// ACD is RFC 5227's phase at the last lease event; a ConflictAsync client dying mid-check leaves Probing or
	// Settling (D23, #882).
	ACD proto.ACDPhase

	// Measured on the lane (#1047): a derived identity was NAKed and moved .10 to .11; the record's got .10 back.

	// Identity is the option-61 value the record was created with, empty for records that have none (#1047).
	Identity []byte
}

// ACDIdle is what every ordinary acquisition ends in, so reading idle as unfinished warned on every start, measured on
// the 2.x lane 2026-09-04 (#882).

// ACDUnfinished reports whether the record shows an RFC 5227 check still running when its writer stopped (#882).
func (r Resumption) ACDUnfinished() bool {
	switch r.ACD {
	case proto.ACDAnnouncing, proto.ACDDefending, proto.ACDIdle:
		return false
	default:
		return true
	}
}

// Keyed on scope and hardware address; of several matches the newest non-closed record wins, by file position (#353).

// Resume finds the record for one identity on one network and says what a new manager may ask for, and as whom (#1047).
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
		res := Resumption{Phase: rec.Phase.String(), ACD: rec.ACD, Identity: rec.Identity}
		if l, ok := rec.Resume(now); ok {
			res.Lease = &l
		} else if a, ok := rec.Prefer(now); ok {
			res.Prefer = a.String()
		}
		return rec.ID, res, true
	}
	return "", Resumption{}, false
}
