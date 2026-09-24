// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
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
