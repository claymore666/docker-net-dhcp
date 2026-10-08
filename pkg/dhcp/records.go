// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
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

	store atomic.Pointer[dhcpruntime.RecordStore]
	lock  *os.File

	mu  sync.Mutex
	seq map[string]uint64

	// compacted is the file size after the last compaction, 0 at open; reopen is set while a swap is unfinished (#1182).
	compacted int64
	reopen    bool

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

	// A leftover is a compaction that died before its rename; the record file beside it is whole (#1182).
	if err := os.Remove(path + compactSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = lock.Close()
		return nil, fmt.Errorf("dhcp: removing an unfinished compaction of %s: %w", path, err)
	}

	store, err := openRecordStore(path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}

	r := &Records{path: path, lock: lock, instance: instance}
	r.store.Store(store)

	// The sequence floor comes from the file; restarting at 1 makes Fold reject later events as stale, silently (#950).
	evs, err := store.Load()
	if err != nil {
		_ = store.Close()
		_ = lock.Close()
		return nil, err
	}
	r.seq = seqFloor(evs)
	return r, nil
}

func seqFloor(evs []lease.RecordEvent) map[string]uint64 {
	seq := map[string]uint64{}
	for _, ev := range evs {
		if ev.Seq > seq[ev.ID] {
			seq[ev.ID] = ev.Seq
		}
	}
	return seq
}

// Close releases the file and the lock.
func (r *Records) Close() error {
	err := r.store.Load().Close()
	if cerr := r.lock.Close(); err == nil {
		err = cerr
	}
	return err
}

// Damage is what the store could not read, a torn tail or unreadable lines.
func (r *Records) Damage() lease.StoreDamage { return r.store.Load().Damage() }

// Path is the record file.
func (r *Records) Path() string { return r.path }

// Instance is the id every line this store writes is stamped with (#1047).
func (r *Records) Instance() string { return r.instance }

// Rebuilt folds the whole file.
func (r *Records) Rebuilt() (lease.Rebuilt, error) {
	evs, err := r.store.Load().Load()
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
	if r.reopen {
		return fmt.Errorf("dhcp: %s was compacted and could not be reopened; the event is not written (#1182)", r.path)
	}
	r.seq[ev.ID]++
	ev.Seq = r.seq[ev.ID]
	ev.Instance = r.instance
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if err := r.store.Load().Append(ev); err != nil {
		// An Append that did not land gives its sequence number back, so a reader sees no gap that is not a compaction's
		// (#950, #1192).
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

// compactSuffix names the rewrite's temporary file, in the record's own directory so the rename stays atomic (#1182).
const compactSuffix = ".compact"

// Swapped by the tests to inject a failed rename or reopen (#1182), or a wrong thinning rule (#1192).
var (
	openRecordStore = func(path string) (*dhcpruntime.RecordStore, error) { return dhcpruntime.OpenRecordStore(path) }
	renameRecords   = os.Rename
	droppableLine   = droppableRenewal
)

// A rewrite of S bytes runs only after S/2 bytes were appended since the last one, so it costs at most two written
// bytes per appended byte. 256 KiB is about 45 closed lifecycles at the measured 5.66 KB each, below which a rewrite
// saves less than a read the sweep already does. The baseline is 0 at open, so a large file left by an older
// version compacts on the first tick (#1182).
const compactMinSize = 256 << 10

// CompactIfDue runs from the 15 s record sweep: it first retries a reopen a failed swap left, then compacts when
// the file is due. It reports whether a rewrite happened (#1182).
func (r *Records) CompactIfDue(now time.Time, retain time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reopen {
		if err := r.reopenLocked(); err != nil {
			return false, err
		}
	}
	fi, err := os.Stat(r.path)
	if err != nil {
		return false, fmt.Errorf("dhcp: sizing %s for compaction: %w", r.path, err)
	}
	if fi.Size() < compactMinSize || fi.Size() < 2*r.compacted {
		return false, nil
	}
	return true, r.compactLocked(now, retain)
}

// compactLocked rewrites the file without the records nothing reads any more, under mu, which every append takes.
// Kept lines are copied byte for byte, unreadable ones too, so the fold of a kept record and Damage are unchanged.
// Before the rename the old file is whole; after it the old store points at an unlinked file and must go (#1182).
func (r *Records) compactLocked(now time.Time, retain time.Duration) error {
	b, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("dhcp: reading %s for compaction: %w", r.path, err)
	}
	kept, seq, folded := keptLines(b, now, retain)
	if folded > 0 {
		log.WithFields(log.Fields{"path": r.path, "lines": folded}).
			Debug("Compaction folded superseded renewal lines into the record's newest one (#1192)")
	}

	tmp := r.path + compactSuffix
	if err := writeSynced(tmp, kept); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dhcp: writing the compacted %s: %w", r.path, err)
	}
	if err := renameRecords(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dhcp: replacing %s with its compaction: %w", r.path, err)
	}
	dirErr := syncDir(filepath.Dir(r.path))

	r.compacted = int64(len(kept))
	r.seq = seq
	_ = r.store.Load().Close()
	r.reopen = true
	if err := r.reopenLocked(); err != nil {
		return err
	}
	if dirErr != nil {
		return fmt.Errorf("dhcp: syncing the directory of the compacted %s: %w", r.path, dirErr)
	}
	return nil
}

func (r *Records) reopenLocked() error {
	s, err := openRecordStore(r.path)
	if err != nil {
		return fmt.Errorf("dhcp: reopening %s after compaction, appends fail until the next sweep retries: %w", r.path, err)
	}
	r.store.Store(s)
	r.reopen = false
	return nil
}

func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if err = os.Remove(path); err == nil {
			f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		}
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// keptLines is the file minus every line of a dropped record and minus the superseded renewal lines of a kept one,
// the sequence floor of what is left, and the number of lines folded away (#1182, #1192).
func keptLines(b []byte, now time.Time, retain time.Duration) ([]byte, map[string]uint64, int) {
	lines := bytes.Split(b, []byte("\n"))
	ids := make([]string, len(lines))
	evAt := make([]int, len(lines))
	parsed := make([]bool, len(lines))
	var evs []lease.RecordEvent
	last := map[string]time.Time{}
	for i, line := range lines {
		var ev lease.RecordEvent
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &ev) != nil {
			continue
		}
		ids[i], parsed[i], evAt[i] = ev.ID, true, len(evs)
		evs = append(evs, ev)
		if ev.At.After(last[ev.ID]) {
			last[ev.ID] = ev.At
		}
	}
	rb := lease.Rebuild(evs)
	keep := keptRecords(rb, last, now, retain)
	drop := thinnedEvents(evs, keep, rb)

	var out []byte
	var keptEvs []lease.RecordEvent
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 || (parsed[i] && (!keep[ids[i]] || drop[evAt[i]])) {
			continue
		}
		out = append(out, line...)
		// The last line keeps its missing newline, so a torn tail reads as torn after the rewrite too.
		if i < len(lines)-1 {
			out = append(out, '\n')
		}
	}
	folded := 0
	for k, ev := range evs {
		if keep[ev.ID] && !drop[k] {
			keptEvs = append(keptEvs, ev)
		}
		if drop[k] {
			folded++
		}
	}
	return out, seqFloor(keptEvs), folded
}

// droppableRenewal is the line's own half of the thinning rule: a renewal or a change whose whole effect, the lease
// and its phases, the next lease line of the record replaces. Any other field the fold copies makes the line
// load-bearing, the Params snapshot above all (#1192). cur is the record before the line.
func droppableRenewal(cur lease.Record, ev lease.RecordEvent) bool {
	if ev.Op != lease.OpLease || (ev.Kind != lease.Renewed && ev.Kind != lease.Changed) {
		return false
	}
	// Every lease line of a v6 record carries its family, which the record already holds.
	if ev.Family != lease.FamilyUnset && ev.Family != cur.Family {
		return false
	}
	return ev.Scope == "" && len(ev.CHAddr) == 0 && len(ev.Identity) == 0 && ev.Params == nil && ev.Params6 == nil &&
		len(ev.Declined6) == 0 && ev.Deadline.IsZero() && ev.StepsRef == "" && ev.Manager == "" &&
		ev.Config == nil && ev.Stats == nil && ev.Extra == nil
}

// replacesLease reports whether a line the fold accepted overwrites a record's lease wholesale (L record.go foldLease).
func replacesLease(ev lease.RecordEvent) bool {
	return ev.Op == lease.OpLease && (ev.Kind == lease.Acquired || ev.Kind == lease.Changed || ev.Kind == lease.Renewed)
}

// thinnedEvents marks, per event, the lines of a kept record to fold away: an accepted droppable line whose next
// line of the same record is an accepted one that replaces the lease. A record's last line is never marked, so the
// sequence floor and the newest lease survive. The fold of the thinned lines must equal the original but for the
// folded counters; a record where it does not keeps every line and is named in one warning (#1192).
func thinnedEvents(evs []lease.RecordEvent, keep map[string]bool, rb lease.Rebuilt) []bool {
	drop := make([]bool, len(evs))
	cur := map[string]lease.Record{}
	pending := map[string]int{}
	thinned := map[string]bool{}
	for k, ev := range evs {
		pre := cur[ev.ID]
		next, err := lease.Fold(pre, ev)
		accepted := err == nil
		if next.Phase != lease.PhaseUnset {
			cur[ev.ID] = next
		}
		if j, ok := pending[ev.ID]; ok {
			if accepted && replacesLease(ev) {
				drop[j], thinned[ev.ID] = true, true
			}
			delete(pending, ev.ID)
		}
		if accepted && keep[ev.ID] && droppableLine(pre, ev) {
			pending[ev.ID] = k
		}
	}
	if len(thinned) == 0 {
		return drop
	}

	var thin []lease.RecordEvent
	renewed, changed := map[string]uint64{}, map[string]uint64{}
	for k, ev := range evs {
		switch {
		case drop[k] && ev.Kind == lease.Renewed:
			renewed[ev.ID]++
		case drop[k] && ev.Kind == lease.Changed:
			changed[ev.ID]++
		}
		if keep[ev.ID] && !drop[k] {
			thin = append(thin, ev)
		}
	}
	rb2 := lease.Rebuild(thin)
	var bad []string
	for id := range thinned {
		if !sameFold(rb, rb2, id, renewed[id], changed[id]) {
			bad = append(bad, id)
		}
	}
	if len(bad) == 0 {
		return drop
	}
	sort.Strings(bad)
	log.WithField("records", bad).Warn("Compaction kept these records whole: the fold of their thinned lines differs " +
		"from the fold of the original lines (#1192)")
	for k, ev := range evs {
		if slices.Contains(bad, ev.ID) {
			drop[k] = false
		}
	}
	return drop
}

// sameFold compares a record and its rejects across the two folds, the thinned one given its folded counters (#1192).
func sameFold(orig, thin lease.Rebuilt, id string, renewed, changed uint64) bool {
	want, ok := orig.ByID(id)
	got, ok2 := thin.ByID(id)
	if !ok || !ok2 {
		return false
	}
	got.Counters.Renewals += renewed
	got.Counters.Changes += changed
	return reflect.DeepEqual(want, got) && reflect.DeepEqual(rejectsOf(orig, id), rejectsOf(thin, id))
}

func rejectsOf(rb lease.Rebuilt, id string) []lease.Reject {
	var out []lease.Reject
	for _, rj := range rb.Rejects {
		if rj.ID == id {
			out = append(out, rj)
		}
	}
	return out
}

// The newest record of a (scope, hardware address) group stays while an older one does: retainRecordFor and
// releaseRecord stop at a CLOSED newest record, and without it they would act on the older one (#962, #1182).
func keptRecords(rb lease.Rebuilt, last map[string]time.Time, now time.Time, retain time.Duration) map[string]bool {
	keep := make(map[string]bool, len(rb.Records))
	newest := map[string]int{}
	others := map[string]bool{}
	for i, rec := range rb.Records {
		keep[rec.ID] = !spent(rec, last[rec.ID], now, retain)
		if len(rec.CHAddr) == 0 {
			continue
		}
		group := rec.Scope + "\x00" + string(rec.CHAddr)
		if j, seen := newest[group]; seen && keep[rb.Records[j].ID] {
			others[group] = true
		}
		newest[group] = i
	}
	for group, i := range newest {
		if others[group] {
			keep[rb.Records[i].ID] = true
		}
	}
	return keep
}

// spent: a CLOSED record is read by nothing once its last line is retain old; an expired RETAINED one can offer only
// option 50 once its server lease ran out retain ago. A zero expiry on a held address is an infinite lease (#1182).
func spent(rec lease.Record, last, now time.Time, retain time.Duration) bool {
	switch rec.Phase {
	case lease.PhaseClosed:
		return !now.Before(last.Add(retain))
	case lease.PhaseRetained:
		if !rec.Deadline.IsZero() && now.Before(rec.Deadline) {
			return false
		}
		if !rec.Lease.Addr.IsValid() {
			return true
		}
		return !rec.Lease.Expire.IsZero() && !now.Before(rec.Lease.Expire.Add(retain))
	default:
		return false
	}
}
