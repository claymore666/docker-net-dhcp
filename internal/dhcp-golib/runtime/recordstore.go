package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/claymore666/dhcp-golib/lease"
)

// RecordStore is lease.Store over one append-only JSONL file.
//
// One event per line, opened O_APPEND, one write per Append. O_APPEND is what
// makes two processes on one file safe: the kernel takes the offset and the
// write together, so two lines interleave as two lines rather than as one
// corrupted one. That is a property of the OPEN FLAG and of writing the line in
// a single call — a formatter that wrote the object and then the newline
// separately would give it up.
//
// The file is the whole store. Rotation, compaction and any retention policy
// are the caller's, and deliberately not here: what may be dropped from a lease
// history is a privacy decision, and this package has no way to make it.
type RecordStore struct {
	path string

	mu   sync.Mutex
	f    *os.File
	torn lease.StoreDamage
}

// OpenRecordStore opens or creates the file at path.
//
// 0600, because the contents are a history of which machine held which address
// and when.
//
// THE DIRECTORY IS FSYNCED WHEN THE FILE IS CREATED, and that is not the same
// fsync Append does. Syncing a file makes its CONTENTS durable; it says
// nothing about the directory entry that names it, so a machine that lost
// power after the first creating event could come back with the event synced
// and no file to find it in — the one case the sync policy says nothing can
// re-derive. ext4 with its default options usually saves this, which is a
// filesystem's behaviour and not a guarantee this store may make.
//
// O_EXCL is how creation is detected: O_CREATE alone cannot tell an open from
// a create, and a directory sync on every open would pay for it on every
// process start. A racing creator between the two opens is the one case the
// fallback reports as an error rather than papering over, and the winner of
// that race syncs the directory anyway.
//
// A FRAGMENT IS TERMINATED BEFORE THE FIRST APPEND. O_APPEND writes at the end
// of the FILE, not at the start of a line, so an existing file whose last byte
// is not a newline would take this process's first event onto the previous
// process's half-written one and lose both. That first event is the restart
// path's adopt — an event durableWrite fsyncs precisely because nothing can
// re-derive it — so the newline goes in at open, and what it cost is counted
// at that moment by the same parse a Load uses, instead of one number standing
// for two lost events. Append re-checks before every write: this covers only
// the fragment that was on disk when the process started.
//
// ORDER, and the reason for it. The terminator is written AND FSYNCED before
// any Append, not left to the first durable Append's fsync to carry. Two
// unsynced writes to one file are not ordered against each other across a
// power loss: the event's bytes could reach the disk while the newline before
// them did not, which is the defect this exists to prevent, reassembled. It
// costs one fsync on a path that by definition follows a crash. It does not
// interact with the directory sync above — that one runs only when the file
// was CREATED, and a created file has no fragment — but the rule the two share
// is the same: make the file findable and consistent BEFORE the first event
// depends on it.
//
// A SECOND, LIVE WRITER IS A HAZARD, and this path alone does not close it.
// Append writes a whole line in one call, so a file that does not end in a
// newline while another writer is between calls is a file that writer died
// inside — but that writer can die at any moment, including while THIS store
// is open and past this function. Append re-checks for exactly that reason;
// what follows here only covers the fragment that was already on disk when
// this process started. If another writer does append between the read below
// and the write, O_APPEND puts the terminator after its line, where it is a
// blank line — skipped, and not counted as damage.
func OpenRecordStore(path string) (*RecordStore, error) {
	created := true
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if errors.Is(err, os.ErrExist) {
		created = false
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("runtime: lease record store %s: %w", path, err)
	}
	s := &RecordStore{path: path, f: f}
	if created {
		if err := syncDir(filepath.Dir(path)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("runtime: making %s findable: %w", path, err)
		}
		return s, nil
	}
	fragment, err := endsMidLine(path)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("runtime: reading the end of %s: %w", path, err)
	}
	if fragment {
		d, err := damageOnceTerminated(path)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("runtime: reading %s to count what the fragment costs: %w", path, err)
		}
		if _, err := writeRecordLine(f, []byte("\n")); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("runtime: terminating the fragment at the end of %s: %w", path, err)
		}
		if err := syncRecordFile(f); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("runtime: syncing the terminated fragment in %s: %w", path, err)
		}
		s.torn = d
	}
	return s, nil
}

// damageOnceTerminated is what a Load will report about this file after the
// terminator goes in, derived by running Load's own parse over the bytes plus
// that newline.
//
// It exists so the repair paths do not COUNT for themselves. A repair that
// wrote its own number is a second derivation of one fact, and the two answers
// disagreed: a tail that is a whole event whose newline alone was lost is not
// damage — parseRecordLines keeps it on purpose — yet the open counted it as a
// skipped line, so Damage said 1 and the Load right after it said 0.
//
// The full read is paid only on a path that follows a crash or a failed write,
// and it is the same read Load does.
func damageOnceTerminated(path string) (lease.StoreDamage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return lease.StoreDamage{}, err
	}
	_, d := parseRecordLines(append(b, '\n'))
	return d, nil
}

// endsMidLine reports whether the file's LAST BYTE is something other than a
// newline, which is the shape a process killed inside Append leaves.
//
// It reads one byte rather than the file: a journal is unbounded and this runs
// on every open. An empty file is not a fragment — there is nothing before the
// first event to run into.
func endsMidLine(path string) (bool, error) {
	r, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = r.Close() }()
	fi, err := r.Stat()
	if err != nil {
		return false, err
	}
	if fi.Size() == 0 {
		return false, nil
	}
	var last [1]byte
	if _, err := r.ReadAt(last[:], fi.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// syncDir fsyncs a directory so that an entry created in it survives a power
// loss. It goes through syncRecordFile for the same reason Append does: it is
// the only way a test can see that it happened.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return syncRecordFile(d)
}

// Close closes the file. Safe to call more than once.
func (s *RecordStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// Append writes one event as one line.
//
// SYNC POLICY, per event kind. Creation, the first lease and the closing
// transitions are fsynced; renewals and counter snapshots are not. The
// asymmetry is deliberate and the direction is what makes it safe: a lost
// renewal line costs an INIT-REBOOT that asks for an address the record already
// held, which the server answers; a lost creation line costs the identity, and
// nothing can answer that.
//
// THE FILE IS CHECKED FOR A FRAGMENT BEFORE EVERY WRITE, not only when the
// store was opened. O_APPEND writes at the end of the FILE, so an event that
// follows a half-written line joins it and both are lost — and a fragment can
// appear at any moment while this store is open, by two routes that no state
// kept in this process can see:
//
//   - this store's own Write returned short. RLIMIT_FSIZE reaches it in a
//     test; ENOSPC and EIO reach it in the field, on a journal this package
//     ships without rotation. The error is returned, the partial bytes stay on
//     disk, and the store stays open and usable.
//   - another process on the same file died inside ITS Append. That is defeat
//     row D-9, and it is why a flag set by our own failed write is not enough:
//     the fragment is not ours.
//
// So the check is a read of the file's last byte, per Append. It is two
// syscalls on a log that records lease transitions, not packets, and it is the
// only construction that covers a writer this process never hears about.
//
// THE TERMINATOR RIDES IN THE SAME WRITE as the event. That keeps the one
// property another writer depends on — one line, one write, so two writers
// interleave as whole lines — and it removes the ordering question the open
// path has to answer with an fsync: there is no "before" between a newline and
// an event written in one call.
func (s *RecordStore) Append(ev lease.RecordEvent) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("runtime: encoding record event: %w", err)
	}
	if bytes.ContainsRune(line, '\n') {
		return fmt.Errorf("runtime: encoded record event contains a newline, which would split one event across two lines")
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return fmt.Errorf("runtime: lease record store %s is closed", s.path)
	}
	fragment, err := endsMidLine(s.path)
	if err != nil {
		return fmt.Errorf("runtime: reading the end of %s before appending: %w", s.path, err)
	}
	if fragment {
		d, err := damageOnceTerminated(s.path)
		if err != nil {
			return fmt.Errorf("runtime: reading %s to count what the fragment costs: %w", s.path, err)
		}
		line = append([]byte{'\n'}, line...)
		s.torn = d
	}
	if _, err := writeRecordLine(s.f, line); err != nil {
		return fmt.Errorf("runtime: appending to %s: %w", s.path, err)
	}
	if durableWrite(ev) {
		if err := syncRecordFile(s.f); err != nil {
			return fmt.Errorf("runtime: syncing %s: %w", s.path, err)
		}
	}
	return nil
}

// syncRecordFile is the fsync itself, indirected for ONE reason: an fsync has
// no effect a same-process test can observe, so without this the policy below
// can be pinned exhaustively while the line that consults it is wired to
// nothing. TestTheSyncPolicyIsAppliedToEveryAppend swaps it and reads back
// which events were synced.
var syncRecordFile = (*os.File).Sync

// writeRecordLine is the write itself, indirected for the same reason
// syncRecordFile is. "One line, one write" is a property of HOW MANY TIMES
// this is called, and the file that results is byte-for-byte identical whether
// a repaired append took one call or two — so without this the property another
// writer on the same file depends on can be given up with nothing going red.
// TestARepairedAppendIsStillOneWrite counts the calls.
var writeRecordLine = (*os.File).Write

// durableWrite decides whether this event is fsynced.
//
// The rule is what the line cannot be reconstructed from. The ops that bring a
// record into existence carry the identity and the address, and nothing later
// can re-derive either; the first lease of a manager instance carries the
// binding itself. Everything else — renewals, tombstone transitions, the close,
// counter snapshots — is either re-derivable from a later exchange or costs
// only an earlier INIT-REBOOT, which is the safe direction.
//
// TestTheSyncPolicyIsClassifiedForEveryOperation walks lease.AllOps so a new
// operation cannot arrive unclassified.
func durableWrite(ev lease.RecordEvent) bool {
	switch ev.Op {
	case lease.OpReserve, lease.OpCreate, lease.OpRebind, lease.OpAdopt, lease.OpBind:
		return true
	case lease.OpLease:
		return ev.Kind == lease.Acquired
	default:
		return false
	}
}

// Load reads every event in append order.
//
// It reads the file rather than the handle it writes through, so a Load is
// valid on a store another process is appending to.
func (s *RecordStore) Load() ([]lease.RecordEvent, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runtime: reading %s: %w", s.path, err)
	}
	evs, damage := parseRecordLines(b)
	s.mu.Lock()
	s.torn = damage
	s.mu.Unlock()
	return evs, nil
}

// Damage reports the damaged lines this store knows about.
//
// It is a COUNT and not a log line: a store that quietly drops a record is the
// failure this exists against, and a number nobody reads is the same failure
// with extra steps.
//
// IT IS NOT A SURVEY, and that is the whole of the contract. A Load reads the
// file and REPLACES the count with what the file holds. Before any Load the
// number is what this store had to REPAIR — at open, or in an Append that
// found a fragment under it — and nothing else: a file whose damage this store
// never had to touch reads 0 until a Load, because reading a journal on every
// process start is what the one-byte check at open exists to avoid.
//
// Where it does report a repair, the number is exactly what the next Load will
// report, because both come from parseRecordLines over the same bytes
// (damageOnceTerminated). That is a derivation and not a promise:
// TestDamageAfterARepairIsWhatTheNextLoadReports drives every torn shape
// through both.
func (s *RecordStore) Damage() lease.StoreDamage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.torn
}

// parseRecordLines is Load's body as a pure function of the bytes, so that
// every torn shape is drivable without a filesystem.
//
// THE LAST LINE IS SPECIAL, and only the last one. A file that does not end in
// a newline has a fragment at the end, which is exactly what a process killed
// inside Append leaves; an unreadable one there is TornTail. An unreadable line
// anywhere else cannot be a crash — the writer had already written the newline
// after it — so it is a different number.
//
// A final line with no newline that DOES parse is kept, and that is not
// leniency. Append writes the object and its newline in one call, so a
// truncation lands inside the object far more often than exactly after it; and
// a JSON object is not prefix-valid — cut anywhere before the closing brace it
// does not parse. So a tail that parses is a whole event whose newline did not
// make it to disk, and dropping it would lose a record for the sake of a
// symmetry.
func parseRecordLines(b []byte) ([]lease.RecordEvent, lease.StoreDamage) {
	var (
		out    []lease.RecordEvent
		damage lease.StoreDamage
	)
	if len(b) == 0 {
		return nil, damage
	}
	lines := bytes.Split(b, []byte("\n"))
	// tailIdx is the ONLY carrier of "which line is the fragment", and it
	// names no line at all when the file ends in a newline: there is then
	// nothing half-written, and the empty final element Split leaves is
	// skipped below like any other blank.
	tailIdx := -1
	if !bytes.HasSuffix(b, []byte("\n")) {
		tailIdx = len(lines) - 1
	}
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev lease.RecordEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if i == tailIdx {
				damage.TornTail++
			} else {
				damage.Skipped++
			}
			continue
		}
		out = append(out, ev)
	}
	return out, damage
}

// The port this implements. A compile error is the only guard that cannot be
// forgotten.
var _ lease.Store = (*RecordStore)(nil)
