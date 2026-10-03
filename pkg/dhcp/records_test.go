// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dhcpruntime "github.com/claymore666/dhcp-golib/runtime"
	"golang.org/x/sys/unix"
)

func testRecords(t *testing.T) (*Records, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.jsonl")
	r, err := OpenRecords(path, "instance-a")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, path
}

// flock binds to the open file description, so a second open in one process conflicts like a second process (#950).

func TestRecords_SecondOpenIsRefused(t *testing.T) {
	_, path := testRecords(t)

	second, err := OpenRecords(path, "instance-b")
	if err == nil {
		_ = second.Close()
		t.Fatal("a second writer was admitted to the same record file")
	}
	if !errors.Is(err, ErrRecordsLocked) {
		t.Fatalf("second open failed for the wrong reason: %v", err)
	}
}

func TestRecords_LockIsReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.jsonl")

	first, err := OpenRecords(path, "instance-a")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := OpenRecords(path, "instance-b")
	if err != nil {
		t.Fatalf("the lock outlived its Records: %v", err)
	}
	_ = second.Close()
}

func TestRecords_AHeldLockNamesTheOtherTag(t *testing.T) {
	_, path := testRecords(t)

	second, err := OpenRecords(path, "instance-b")
	if err == nil {
		_ = second.Close()
		t.Fatal("a second writer was admitted to the same record file")
	}
	got := err.Error()
	if !strings.Contains(got, "disable it before enabling this one") {
		t.Errorf("the refusal does not name the action: %q", got)
	}
	if !strings.Contains(got, "another tag of this plugin is enabled") {
		t.Errorf("the refusal does not name the cause: %q", got)
	}
	if strings.Contains(got, "does not support locks") {
		t.Errorf("a held lock was reported as an unlockable filesystem: %q", got)
	}
	if !strings.Contains(got, path) {
		t.Errorf("the refusal does not name the record file: %q", got)
	}
}

// EWOULDBLOCK equals EAGAIN and EOPNOTSUPP equals ENOTSUP on Linux; a build that splits a pair turns this red (#950).

func TestRecords_TheLockRefusalReadsTheErrno(t *testing.T) {
	const path = "/state/lease-records.jsonl"

	const (
		held        = "another tag of this plugin is enabled and holds the lease record"
		unsupported = "does not support locks"
		generic     = "already open by another writer"
	)

	cases := []struct {
		name    string
		errno   error
		want    string
		notWant []string
	}{
		{"EWOULDBLOCK", unix.EWOULDBLOCK, held, []string{unsupported}},
		{"EAGAIN", unix.EAGAIN, held, []string{unsupported}},
		{"ENOLCK", unix.ENOLCK, unsupported, []string{held}},
		{"EOPNOTSUPP", unix.EOPNOTSUPP, unsupported, []string{held}},
		{"ENOTSUP", unix.ENOTSUP, unsupported, []string{held}},
		{"EINVAL", unix.EINVAL, unsupported, []string{held}},
		{"ENOSYS", unix.ENOSYS, unsupported, []string{held}},
		{"EINTR", unix.EINTR, generic, []string{held, unsupported}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := lockRefused(path, c.errno)
			if err == nil {
				t.Fatal("a failed lock produced no refusal")
			}
			got := err.Error()
			if !strings.Contains(got, c.want) {
				t.Errorf("refusal for %v does not say %q: %q", c.errno, c.want, got)
			}
			for _, nw := range c.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("refusal for %v also says %q, which is the other reading: %q", c.errno, nw, got)
				}
			}
			if !strings.Contains(got, path) {
				t.Errorf("refusal for %v does not name the record file: %q", c.errno, got)
			}
			if !errors.Is(err, ErrRecordsLocked) {
				t.Errorf("refusal for %v is not an ErrRecordsLocked", c.errno)
			}
			if !errors.Is(err, c.errno) {
				t.Errorf("refusal for %v dropped the errno", c.errno)
			}
		})
	}
}

func TestRecords_TheGenericRefusalCarriesTheErrno(t *testing.T) {
	got := lockRefused("/state/lease-records.jsonl", unix.EINTR).Error()
	if !strings.Contains(got, unix.EINTR.Error()) {
		t.Errorf("the generic refusal does not print the errno: %q", got)
	}
}

func TestRecords_TheReferenceQuotesTheRefusals(t *testing.T) {
	doc := reference(t)

	const elided = "<the record file>"
	for _, errno := range []error{unix.EWOULDBLOCK, unix.ENOLCK} {
		msg := strings.TrimPrefix(lockRefused(elided, errno).Error(), "dhcp: ")
		for _, part := range strings.Split(msg, elided) {
			part = strings.Join(strings.Fields(part), " ")
			if part == "" {
				continue
			}
			if !strings.Contains(doc, part) {
				t.Errorf("docs/reference.md does not quote %q, which the refusal for %v says", part, errno)
			}
		}
	}

	generic := strings.TrimPrefix(ErrRecordsLocked.Error(), "dhcp: ")
	if !strings.Contains(doc, generic) {
		t.Errorf("docs/reference.md does not quote %q, the wording an unrecognised errno keeps", generic)
	}
}

// reference returns docs/reference.md with its line wrapping removed, so a quoted sentence is one string.
func reference(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference.md"))
	if err != nil {
		t.Fatalf("read the reference: %v", err)
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

func TestRecords_TheReferenceGivesEachRefusalItsOwnRemedy(t *testing.T) {
	doc := reference(t)

	const elided = "<the record file>"
	heldParts := strings.Split(strings.TrimPrefix(lockRefused(elided, unix.EWOULDBLOCK).Error(), "dhcp: "), elided)
	unsupParts := strings.Split(strings.TrimPrefix(lockRefused(elided, unix.ENOLCK).Error(), "dhcp: "), elided)

	anchors := []struct {
		name string
		text string
	}{
		{"the held refusal", strings.TrimSpace(heldParts[0])},
		{"the unsupported refusal", strings.TrimSpace(unsupParts[1])},
		{"the generic refusal", strings.TrimPrefix(ErrRecordsLocked.Error(), "dhcp: ")},
	}
	at := make([]int, len(anchors))
	for i, a := range anchors {
		at[i] = strings.Index(doc, a.text)
		if at[i] < 0 {
			t.Fatalf("docs/reference.md does not quote %s (%q)", a.name, a.text)
		}
	}
	if !(at[0] < at[1] && at[1] < at[2]) {
		t.Fatalf("the three refusals are not quoted in order in docs/reference.md: %v", at)
	}

	remedies := []struct {
		name   string
		text   string
		region int
	}{
		{"disable the holder", "Disable the old tag, then enable the new one.", 0},
		{"give the mount a filesystem that locks", "Back `/var/lib/net-dhcp` on the host with a filesystem that implements file locking.", 1},
		{"do not repoint STATE_DIR", "Do not repoint `STATE_DIR`", 1},
		{"read the owner and mode", "Check the owner and the mode of the host directory", 2},
	}
	for _, r := range remedies {
		if n := strings.Count(doc, r.text); n != 1 {
			t.Errorf("the remedy %q appears %d times in docs/reference.md, want 1: %q", r.name, n, r.text)
			continue
		}
		i := strings.Index(doc, r.text)
		lo := at[r.region]
		hi := len(doc)
		if r.region+1 < len(at) {
			hi = at[r.region+1]
		}
		if i < lo || i >= hi {
			t.Errorf("the remedy %q is not in %s's paragraph: it would be read as the answer to another refusal",
				r.name, anchors[r.region].name)
		}
	}

	if !strings.Contains(doc, "repointing this setting opts out") {
		t.Error("docs/reference.md no longer says repointing STATE_DIR opts out; the remedy above assumes it does")
	}
}

// Root ignores file modes, so the lock file is made unopenable by a missing parent directory (#950).

func TestRecords_AnUnopenableLockFileNamesItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", "leases.jsonl")

	r, err := OpenRecords(path, "instance-a")
	if err == nil {
		_ = r.Close()
		t.Fatal("a record store opened under a directory that does not exist")
	}
	got := err.Error()
	if !strings.Contains(got, path) {
		t.Errorf("the failure does not name the record file: %q", got)
	}
	if !errors.Is(err, unix.ENOENT) {
		t.Errorf("the failure dropped the errno: %q", got)
	}
	if errors.Is(err, ErrRecordsLocked) {
		t.Errorf("a lock file that could not be created was reported as a contended lock: %q", got)
	}

	if i := strings.Index(got, path); i > 0 {
		lead := strings.TrimSpace(strings.TrimPrefix(got[:i], "dhcp: "))
		if !strings.Contains(reference(t), lead) {
			t.Errorf("docs/reference.md does not quote %q, which is how this failure opens", lead)
		}
	}
}

func TestRecords_SequenceSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.jsonl")

	first, err := OpenRecords(path, "instance-a")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.Created("rec-1", "net-1", []byte{2, 0, 0, 0, 0, 1}, []byte{0, 9, 9}); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if err := first.Bound("rec-1"); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := OpenRecords(path, "instance-b")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()
	if err := second.Left("rec-1"); err != nil {
		t.Fatalf("Left: %v", err)
	}

	rb, err := second.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, ok := rb.ByID("rec-1")
	if !ok {
		t.Fatal("the record vanished across the reopen")
	}
	if rec.Phase != lease.PhaseLeft {
		t.Errorf("phase = %s, want left: the second process's event was refused as stale", rec.Phase)
	}
	if rec.Counters.Rejects != 0 {
		t.Errorf("the fold refused %d event(s) after the reopen; last reject %v", rec.Counters.Rejects, rec.LastReject)
	}
}

func TestRecords_ResumeCarriesTheLeaseAcrossManagers(t *testing.T) {
	r, _ := testRecords(t)

	if err := r.Created("rec-1", "net-1", []byte{2, 0, 0, 0, 0, 1}, []byte{0, 9, 9}); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if err := r.Bound("rec-1"); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	held := lease.Lease{
		Addr:     netip.MustParsePrefix("192.0.2.15/24"),
		Gateway:  netip.MustParseAddr("192.0.2.1"),
		ServerID: netip.MustParseAddr("192.0.2.1"),
		Expire:   time.Now().Add(time.Hour),
	}
	if err := r.Observed("rec-1", lease.Event{Kind: lease.Acquired, Lease: held}, nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	// ReasonStopped is the chassis's own cancel, not a loss (#899).
	if err := r.Observed("rec-1", lease.Event{Kind: lease.Lost, Reason: proto.ReasonStopped}, nil); err != nil {
		t.Fatalf("Observed(stopped): %v", err)
	}

	rb, err := r.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, ok := rb.ByID("rec-1")
	if !ok {
		t.Fatal("no record")
	}
	resume, ok := rec.Resume(time.Now())
	if !ok {
		t.Fatal("the record offers nothing to resume: the Join manager would DISCOVER and can be given a different address")
	}
	if resume.Addr != held.Addr {
		t.Errorf("resume address = %s, want %s", resume.Addr, held.Addr)
	}
}

func TestRecords_ResumeCarriesTheIdentity(t *testing.T) {
	mac := []byte{2, 0, 0, 0, 0, 1}
	identity := []byte{0, 9, 9}

	t.Run("the stored identity comes back", func(t *testing.T) {
		r, _ := testRecords(t)
		if err := r.Created("rec-1", "net-1", mac, identity); err != nil {
			t.Fatalf("Created: %v", err)
		}

		id, res, ok := r.Resume("net-1", mac, time.Now())
		if !ok {
			t.Fatal("no record to resume")
		}
		if id != "rec-1" {
			t.Fatalf("resumed %q, want rec-1", id)
		}
		if !bytes.Equal(res.Identity, identity) {
			t.Errorf("identity = %x, want %x. The manager re-derives option 61 when this is "+
				"empty, and a re-bound address is filed under this and under nothing else",
				res.Identity, identity)
		}
	})

	t.Run("a re-bind under a new hardware address keeps it", func(t *testing.T) {
		// Docker mints a fresh MAC for a restarting container's endpoint; the write-once identity stays the first one
		// (#1047).
		r, _ := testRecords(t)
		if err := r.Created("rec-1", "net-1", mac, identity); err != nil {
			t.Fatalf("Created: %v", err)
		}
		if err := r.Retained("rec-1", time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("Retained: %v", err)
		}
		fresh := []byte{2, 0, 0, 0, 0, 2}
		if err := r.Rebound("rec-1", fresh); err != nil {
			t.Fatalf("Rebound: %v", err)
		}

		id, res, ok := r.Resume("net-1", fresh, time.Now())
		if !ok {
			t.Fatal("the re-bound record is not resumable under the new hardware address")
		}
		if id != "rec-1" {
			t.Fatalf("resumed %q, want the re-bound rec-1", id)
		}
		if !bytes.Equal(res.Identity, identity) {
			t.Errorf("identity = %x, want the record's original %x. The reservation asked under "+
				"the original and was given the address back; the client asking under anything "+
				"else is NAKed off it", res.Identity, identity)
		}
	})

	t.Run("a record with no identity offers none", func(t *testing.T) {
		r, _ := testRecords(t)
		if err := r.Adopted("rec-1", "net-1", mac, nil); err != nil {
			t.Fatalf("Adopted: %v", err)
		}

		_, res, ok := r.Resume("net-1", mac, time.Now())
		if !ok {
			t.Fatal("no record to resume")
		}
		if len(res.Identity) != 0 {
			t.Errorf("identity = %x, want none", res.Identity)
		}
	})
}

func TestRecords_TwoManagersGetTwoIDs(t *testing.T) {
	r, _ := testRecords(t)

	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := r.NewManagerID()
		if id == "" {
			t.Fatal("empty manager id: OpStats naming no manager is refused outright")
		}
		if seen[id] {
			t.Fatalf("manager id %q handed out twice", id)
		}
		seen[id] = true
	}
}

func TestRecords_StatsAccumulateAcrossManagers(t *testing.T) {
	r, _ := testRecords(t)

	if err := r.Created("rec-1", "net-1", []byte{2, 0, 0, 0, 0, 1}, []byte{0, 9, 9}); err != nil {
		t.Fatalf("Created: %v", err)
	}
	if err := r.Bound("rec-1"); err != nil {
		t.Fatalf("Bound: %v", err)
	}

	oneShot := r.NewManagerID()
	if err := r.Counted("rec-1", oneShot, lease.Stats{Sent: 3, Received: 2}); err != nil {
		t.Fatalf("Counted(one-shot): %v", err)
	}
	join := r.NewManagerID()
	if err := r.Counted("rec-1", join, lease.Stats{Sent: 1, Received: 1}); err != nil {
		t.Fatalf("Counted(join): %v", err)
	}

	rb, err := r.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, _ := rb.ByID("rec-1")
	if rec.Counters.Rejects != 0 {
		t.Fatalf("the fold refused a counter snapshot: %v", rec.LastReject)
	}
	if got := rec.Counters.Wire.Sent; got != 4 {
		t.Errorf("Sent = %d, want 4 (3 from the one-shot plus 1 from the Join manager)", got)
	}
}

func TestRecords_OneRecordStoreCallSite(t *testing.T) {
	// Assembled so this file is not its own second hit (#950).
	const fn = "OpenRecord" + "Store("

	var hits []string
	roots := []string{filepath.Join("..", "..", "pkg"), filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "test")}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, form := range []string{"dhcpruntime." + fn, "runtime." + fn} {
				if strings.Contains(string(b), form) {
					hits = append(hits, path)
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(hits) != 1 {
		t.Fatalf("runtime.OpenRecord"+"Store is called from %d files (%v); exactly one is the one-writer guarantee. "+
			"A second opener bypasses the lock Records takes and its sequence numbering, and the damage is silent: "+
			"the loser's events fold as stale and a restart resumes from a record missing them.", len(hits), hits)
	}
	if !strings.HasSuffix(hits[0], "records.go") {
		t.Errorf("the only OpenRecordStore call site is %s, not pkg/dhcp/records.go", hits[0])
	}
}

// RFC 5227 section 2.1 still runs when an async address is handed out, so the phase must survive a restart (D23).

func TestRecords_TheACDPhaseSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	mac := []byte{2, 0, 0, 0, 0, 1}

	held := lease.Lease{
		Addr:     netip.MustParsePrefix("192.0.2.15/24"),
		Gateway:  netip.MustParseAddr("192.0.2.1"),
		ServerID: netip.MustParseAddr("192.0.2.1"),
		Expire:   time.Now().Add(time.Hour),
	}

	cases := []struct {
		name       string
		phase      proto.ACDPhase
		unfinished bool
	}{
		{"probing", proto.ACDProbing, true},
		{"settling", proto.ACDSettling, true},
		{"announcing", proto.ACDAnnouncing, false},
		{"defending", proto.ACDDefending, false},
		// Idle also ends every ordinary acquisition, so reading it as unfinished warns on every start (#882).
		{"idle", proto.ACDIdle, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, c.name+".jsonl")
			first, err := OpenRecords(p, "instance-a")
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := first.Created("rec-1", "net-1", mac, []byte{0, 9, 9}); err != nil {
				t.Fatalf("Created: %v", err)
			}
			if err := first.Bound("rec-1"); err != nil {
				t.Fatalf("Bound: %v", err)
			}
			if err := first.Observed("rec-1", lease.Event{Kind: lease.Acquired, Lease: held, ACD: c.phase}, nil); err != nil {
				t.Fatalf("Observed: %v", err)
			}
			if err := first.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			second, err := OpenRecords(p, "instance-b")
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer func() { _ = second.Close() }()
			id, resume, ok := second.Resume("net-1", mac, time.Now())
			if !ok {
				t.Fatal("nothing to resume after the restart")
			}
			if id != "rec-1" {
				t.Fatalf("resumed %q, want rec-1", id)
			}
			if resume.Lease == nil {
				t.Fatal("the lease did not survive the restart")
			}
			if resume.ACD != c.phase {
				t.Errorf("Resumption.ACD = %v, want %v: the phase did not reach Resume", resume.ACD, c.phase)
			}
			if got := resume.ACDUnfinished(); got != c.unfinished {
				t.Errorf("ACDUnfinished() = %v, want %v for phase %v", got, c.unfinished, c.phase)
			}
		})
	}

	p := filepath.Join(dir, "probing.jsonl")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !strings.Contains(string(raw), `"acd":`) {
		t.Error("the record file carries no acd field; a restart reads the phase from this file and nothing else")
	}
}

// Measured on the 2.x lane 2026-09-04: idle read as unfinished warned on every healthy container start (#882).

func TestResumption_IdleIsNotEvidenceOfAnUncheckedAddress(t *testing.T) {
	if (Resumption{}).ACDUnfinished() {
		t.Error("the zero Resumption reports an unfinished check; the zero phase is idle, which is no check at all")
	}
	for _, phase := range proto.AllACDPhases() {
		want := phase == proto.ACDProbing || phase == proto.ACDSettling
		if got := (Resumption{ACD: phase}).ACDUnfinished(); got != want {
			t.Errorf("ACDUnfinished(%v) = %v, want %v", phase, got, want)
		}
	}
	unknown := proto.ACDPhase(len(proto.AllACDPhases()) + 7)
	if !(Resumption{ACD: unknown}).ACDUnfinished() {
		t.Errorf("ACDUnfinished(%v) = false for a phase this build does not know; an unknown phase must cost a log line, not read as clean", unknown)
	}
}

var compactMAC = net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}

func compactLease(expire time.Time) lease.Lease {
	return lease.Lease{
		Addr:    netip.MustParsePrefix("192.0.2.10/24"),
		Gateway: netip.MustParseAddr("192.0.2.1"),
		Expire:  expire,
	}
}

func mustRecord(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// joinedRecord is the findings' lifecycle up to the running manager (#1182).
func joinedRecord(t *testing.T, r *Records, id string, mac net.HardwareAddr, expire time.Time) {
	t.Helper()
	ls := compactLease(expire)
	mustRecord(t, r.Created(id, "net-c", mac, []byte{1, 2, 3, 4, 5, 6, 7}))
	mustRecord(t, r.Observed(id, lease.Event{Kind: lease.Acquired, Lease: ls}, nil))
	mustRecord(t, r.Counted(id, r.NewManagerID(), lease.Stats{}))
	mustRecord(t, r.Bound(id))
	mustRecord(t, r.Observed(id, lease.Event{Kind: lease.Acquired, Lease: ls}, nil))
	mustRecord(t, r.Observed(id, lease.Event{Kind: lease.Renewed, Lease: ls}, nil))
	mustRecord(t, r.Counted(id, r.NewManagerID(), lease.Stats{}))
}

func retainedRecord(t *testing.T, r *Records, id string, mac net.HardwareAddr, expire, deadline time.Time) {
	t.Helper()
	joinedRecord(t, r, id, mac, expire)
	mustRecord(t, r.Left(id))
	mustRecord(t, r.Retained(id, deadline))
}

func closedRecord(t *testing.T, r *Records, id string, mac net.HardwareAddr) {
	t.Helper()
	retainedRecord(t, r, id, mac, time.Now().Add(time.Hour), time.Now().Add(time.Minute))
	mustRecord(t, r.Closed(id))
}

func compactNow(t *testing.T, r *Records, now time.Time) error {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.compactLocked(now, compactRetain)
}

const compactRetain = time.Minute

func seqLen(r *Records) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seq)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func uniqueMAC(i int) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x42, byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}
}

func TestRecords_CompactionBoundsTheFile(t *testing.T) {
	now := time.Now()
	sets := map[string]func(t *testing.T, r *Records, id string, mac net.HardwareAddr){
		"closed": closedRecord,
		"retained-expired": func(t *testing.T, r *Records, id string, mac net.HardwareAddr) {
			retainedRecord(t, r, id, mac, now.Add(time.Hour), now.Add(time.Minute))
		},
	}
	for name, spend := range sets {
		for _, n := range []int{100, 1000} {
			t.Run(fmt.Sprintf("%s-%d", name, n), func(t *testing.T) {
				r, path := testRecords(t)
				joinedRecord(t, r, "live-1", uniqueMAC(-1), time.Time{})
				retainedRecord(t, r, "live-2", uniqueMAC(-2), now.Add(5*time.Hour), now.Add(time.Minute))
				live := fileSize(t, path)
				for i := 0; i < n; i++ {
					spend(t, r, fmt.Sprintf("spent-%d", i), uniqueMAC(i))
				}
				before := fileSize(t, path)

				if err := compactNow(t, r, now.Add(3*time.Hour)); err != nil {
					t.Fatalf("compaction: %v", err)
				}
				rb, err := r.Rebuilt()
				if err != nil {
					t.Fatal(err)
				}
				after := fileSize(t, path)
				t.Logf("N=%d %s: %d B before, %d B after, live set %d B, seq %d, records %d",
					n, name, before, after, live, seqLen(r), len(rb.Records))
				if after != live {
					t.Errorf("the file is %d B after compaction, want the live set's %d B", after, live)
				}
				if got := seqLen(r); got != 2 {
					t.Errorf("seq holds %d ids, want the 2 live ones", got)
				}
				if len(rb.Records) != 2 {
					t.Errorf("the file folds to %d records, want the 2 live ones", len(rb.Records))
				}
			})
		}
	}
}

func keptOnly(rb lease.Rebuilt, ids ...string) []lease.Record {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []lease.Record
	for _, rec := range rb.Records {
		if want[rec.ID] {
			out = append(out, rec)
		}
	}
	return out
}

func TestRecords_CompactionKeepsWhatIsStillRead(t *testing.T) {
	r, _ := testRecords(t)
	now := time.Now()
	at := now.Add(3 * time.Hour)

	joinedRecord(t, r, "joined-expired-lease", uniqueMAC(1), now.Add(time.Minute))
	joinedRecord(t, r, "left", uniqueMAC(2), now.Add(time.Minute))
	mustRecord(t, r.Left("left"))
	retainedRecord(t, r, "tombstone-unexpired", uniqueMAC(3), now.Add(time.Hour), at.Add(time.Minute))
	retainedRecord(t, r, "lease-not-yet-expired", uniqueMAC(4), at.Add(-compactRetain+time.Second), now)
	retainedRecord(t, r, "infinite-lease", uniqueMAC(5), time.Time{}, now)
	mac6 := uniqueMAC(6)
	duid, err := DUIDLL(mac6)
	mustRecord(t, err)
	iaid, err := IAIDFromMAC(mac6)
	mustRecord(t, err)
	mustRecord(t, r.Created6("v6", "net-c", mac6, Identity6{DUID: duid, IAID: iaid}.Bytes()))
	closedRecord(t, r, "spent-closed", uniqueMAC(7))
	retainedRecord(t, r, "spent-retained", uniqueMAC(8), now.Add(time.Hour), now)
	mustRecord(t, r.Created("spent-addressless", "net-c", uniqueMAC(10), nil))
	mustRecord(t, r.Retained("spent-addressless", time.Time{}))

	kept := []string{"joined-expired-lease", "left", "tombstone-unexpired", "lease-not-yet-expired", "infinite-lease", "v6"}
	before, err := r.Rebuilt()
	mustRecord(t, err)
	_, _, ident6, ok := r.Resume6("net-c", mac6, at)
	if !ok {
		t.Fatal("the v6 record does not resume before compaction")
	}

	if err := compactNow(t, r, at); err != nil {
		t.Fatalf("compaction: %v", err)
	}
	after, err := r.Rebuilt()
	mustRecord(t, err)
	if !reflect.DeepEqual(keptOnly(before, kept...), after.Records) {
		t.Errorf("the kept records fold differently after compaction:\nbefore %+v\nafter  %+v", keptOnly(before, kept...), after.Records)
	}
	if _, _, got, ok := r.Resume6("net-c", mac6, at); !ok || !reflect.DeepEqual(got, ident6) {
		t.Errorf("the v6 identity after compaction is %+v (resumed %v), want %+v", got, ok, ident6)
	}
}

func TestRecords_CompactionKeepsTheNewestClosedRecordOverAnOlderOne(t *testing.T) {
	r, _ := testRecords(t)
	now := time.Now()
	retainedRecord(t, r, "older", compactMAC, time.Time{}, now)
	closedRecord(t, r, "newer", compactMAC)
	closedRecord(t, r, "alone-a", uniqueMAC(9))
	closedRecord(t, r, "alone-b", uniqueMAC(9))

	if err := compactNow(t, r, now.Add(3*time.Hour)); err != nil {
		t.Fatalf("compaction: %v", err)
	}
	rb, err := r.Rebuilt()
	mustRecord(t, err)
	matches := rb.ByScopeMAC("net-c", compactMAC)
	if len(matches) != 2 || matches[1].ID != "newer" || matches[1].Phase != lease.PhaseClosed {
		t.Fatalf("the group reads %+v, want the older record under the newer CLOSED one, "+
			"or retainRecordFor would re-tombstone an address already handed back (#962)", matches)
	}
	if got := rb.ByScopeMAC("net-c", uniqueMAC(9)); len(got) != 0 {
		t.Errorf("a group with nothing kept still holds %d records", len(got))
	}
}

func TestRecords_ARecentlyClosedRecordIsKept(t *testing.T) {
	r, _ := testRecords(t)
	closedRecord(t, r, "recent", compactMAC)
	closedAt := time.Now()

	if err := compactNow(t, r, closedAt.Add(compactRetain-time.Second)); err != nil {
		t.Fatalf("compaction: %v", err)
	}
	rb, err := r.Rebuilt()
	mustRecord(t, err)
	if _, ok := rb.ByID("recent"); !ok {
		t.Fatal("a record closed 59 s ago was dropped; a reader that read it before the rewrite may still write to it")
	}
	if err := compactNow(t, r, closedAt.Add(compactRetain+time.Second)); err != nil {
		t.Fatalf("compaction: %v", err)
	}
	rb, err = r.Rebuilt()
	mustRecord(t, err)
	if _, ok := rb.ByID("recent"); ok {
		t.Error("a record closed 61 s ago was kept")
	}
}

func TestRecords_SeqAfterCompactionIsWhatAFreshOpenReads(t *testing.T) {
	r, path := testRecords(t)
	joinedRecord(t, r, "live", uniqueMAC(1), time.Time{})
	closedRecord(t, r, "spent", uniqueMAC(2))
	mustRecord(t, compactNow(t, r, time.Now().Add(time.Hour)))
	r.mu.Lock()
	got := maps.Clone(r.seq)
	r.mu.Unlock()
	mustRecord(t, r.Close())

	fresh, err := OpenRecords(path, "instance-b")
	mustRecord(t, err)
	defer func() { _ = fresh.Close() }()
	if !reflect.DeepEqual(got, fresh.seq) {
		t.Errorf("seq after compaction is %v, a fresh open of the compacted file reads %v", got, fresh.seq)
	}
}

func TestRecords_AppendsAfterCompactionLandInTheFile(t *testing.T) {
	r, path := testRecords(t)
	closedRecord(t, r, "spent", uniqueMAC(1))
	mustRecord(t, compactNow(t, r, time.Now().Add(time.Hour)))
	joinedRecord(t, r, "after", uniqueMAC(2), time.Time{})
	mustRecord(t, r.Close())

	fresh, err := OpenRecords(path, "instance-b")
	mustRecord(t, err)
	defer func() { _ = fresh.Close() }()
	rb, err := fresh.Rebuilt()
	mustRecord(t, err)
	if rec, ok := rb.ByID("after"); !ok || rec.Phase != lease.PhaseJoined {
		t.Errorf("a record written after compaction reads back as %+v (found %v), want JOINED", rec, ok)
	}
}

func TestRecords_ALeftoverCompactFileIsRemovedAtOpen(t *testing.T) {
	r, path := testRecords(t)
	joinedRecord(t, r, "live", uniqueMAC(1), time.Time{})
	before, err := r.Rebuilt()
	mustRecord(t, err)
	mustRecord(t, r.Close())
	mustRecord(t, os.WriteFile(path+compactSuffix, []byte("{\"id\":\"half"), 0o600))

	fresh, err := OpenRecords(path, "instance-b")
	mustRecord(t, err)
	defer func() { _ = fresh.Close() }()
	if _, err := os.Stat(path + compactSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the unfinished compaction is still there after open: %v", err)
	}
	after, err := fresh.Rebuilt()
	mustRecord(t, err)
	if !reflect.DeepEqual(before.Records, after.Records) {
		t.Errorf("the record folds differently after a crashed compaction")
	}
}

func TestRecords_AFailedRenameLeavesTheOriginal(t *testing.T) {
	r, path := testRecords(t)
	closedRecord(t, r, "spent", uniqueMAC(1))
	orig, err := os.ReadFile(path)
	mustRecord(t, err)
	renameRecords = func(string, string) error { return errors.New("injected") }
	t.Cleanup(func() { renameRecords = os.Rename })

	if err := compactNow(t, r, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a failed rename reported success")
	}
	got, err := os.ReadFile(path)
	mustRecord(t, err)
	if !bytes.Equal(got, orig) {
		t.Error("the record file changed although the rename failed")
	}
	if _, err := os.Stat(path + compactSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary file is left behind: %v", err)
	}
	joinedRecord(t, r, "after", uniqueMAC(2), time.Time{})
	rb, err := r.Rebuilt()
	mustRecord(t, err)
	if _, ok := rb.ByID("after"); !ok {
		t.Error("an append after a failed rename did not land")
	}
}

func TestRecords_AFailedReopenFailsAppendsAndIsRetried(t *testing.T) {
	r, path := testRecords(t)
	closedRecord(t, r, "spent", uniqueMAC(1))
	opener := openRecordStore
	openRecordStore = func(string) (*dhcpruntime.RecordStore, error) { return nil, errors.New("injected") }
	restore := func() { openRecordStore = opener }
	t.Cleanup(restore)

	err := compactNow(t, r, time.Now().Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("a failed reopen reported %v, want an error naming %s", err, path)
	}
	if err := r.Created("lost", "net-c", uniqueMAC(2), nil); err == nil || !strings.Contains(err.Error(), "compacted") {
		t.Fatalf("an append while the store is not reopened reported %v, want an error that says the file was compacted", err)
	}
	if _, err := r.CompactIfDue(time.Now(), compactRetain); err == nil {
		t.Fatal("the retry reported success while the reopen still fails")
	}

	restore()
	if _, err := r.CompactIfDue(time.Now(), compactRetain); err != nil {
		t.Fatalf("the next tick did not reopen the store: %v", err)
	}
	joinedRecord(t, r, "after", uniqueMAC(3), time.Time{})
	rb, err := r.Rebuilt()
	mustRecord(t, err)
	if _, ok := rb.ByID("after"); !ok {
		t.Error("an append after the retried reopen did not land")
	}
	if _, ok := rb.ByID("lost"); ok {
		t.Error("the append refused during the failure is in the file")
	}
}

func TestRecords_CompactionKeepsDamagedLines(t *testing.T) {
	r, path := testRecords(t)
	joinedRecord(t, r, "live", uniqueMAC(1), time.Time{})
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	mustRecord(t, err)
	_, err = f.WriteString("not json\n")
	mustRecord(t, err)
	closedRecord(t, r, "spent", uniqueMAC(2))
	_, err = f.WriteString("{\"id\":\"torn")
	mustRecord(t, err)
	mustRecord(t, f.Close())
	_, err = r.Rebuilt()
	mustRecord(t, err)
	before := r.Damage()

	mustRecord(t, compactNow(t, r, time.Now().Add(time.Hour)))
	_, err = r.Rebuilt()
	mustRecord(t, err)
	after := r.Damage()
	if before.Skipped+before.TornTail != 2 || after.Skipped+after.TornTail != 2 {
		t.Errorf("damaged lines before %+v, after %+v, want both lines kept", before, after)
	}
}

func TestRecords_AQuietFileIsNotRewritten(t *testing.T) {
	r, path := testRecords(t)
	for i := 0; i < 10; i++ {
		closedRecord(t, r, fmt.Sprintf("spent-%d", i), uniqueMAC(i))
	}
	before, err := os.Stat(path)
	mustRecord(t, err)
	if before.Size() >= compactMinSize {
		t.Fatalf("the quiet file is %d B, not below the %d B floor", before.Size(), compactMinSize)
	}

	done, err := r.CompactIfDue(time.Now().Add(time.Hour), compactRetain)
	mustRecord(t, err)
	after, err := os.Stat(path)
	mustRecord(t, err)
	if done || !os.SameFile(before, after) || after.Size() != before.Size() {
		t.Errorf("a %d B file was rewritten (reported %v, same inode %v)", before.Size(), done, os.SameFile(before, after))
	}
}

func TestRecords_ALargeFileCompactsOnTheFirstTick(t *testing.T) {
	r, path := testRecords(t)
	for i := 0; fileSize(t, path) < compactMinSize; i++ {
		closedRecord(t, r, fmt.Sprintf("spent-%d", i), uniqueMAC(i))
	}
	mustRecord(t, r.Close())
	fresh, err := OpenRecords(path, "instance-b")
	mustRecord(t, err)
	defer func() { _ = fresh.Close() }()

	done, err := fresh.CompactIfDue(time.Now().Add(time.Hour), compactRetain)
	mustRecord(t, err)
	if !done || fileSize(t, path) != 0 {
		t.Errorf("the first tick after open compacted %v and left %d B, want a rewrite to 0 B", done, fileSize(t, path))
	}
}

func TestRecords_CompactionDoesNotRaceReaders(t *testing.T) {
	r, _ := testRecords(t)
	for i := 0; i < 50; i++ {
		closedRecord(t, r, fmt.Sprintf("spent-%d", i), uniqueMAC(i))
	}
	joinedRecord(t, r, "live", uniqueMAC(-1), time.Time{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			rb, err := r.Rebuilt()
			if err != nil {
				t.Errorf("a reader during compaction failed: %v", err)
				return
			}
			if _, ok := rb.ByID("live"); !ok {
				t.Error("a reader during compaction did not see the live record")
				return
			}
			_ = r.Damage()
		}
	}()
	for i := 0; i < 5; i++ {
		mustRecord(t, compactNow(t, r, time.Now().Add(time.Hour)))
		mustRecord(t, r.Observed("live", lease.Event{Kind: lease.Renewed, Lease: compactLease(time.Time{})}, nil))
	}
	close(stop)
	<-done
}
