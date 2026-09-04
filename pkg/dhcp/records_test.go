// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
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

// TestRecords_SecondOpenIsRefused drives G-10 in the only direction
// that can be driven without a second process.
//
// flock is associated with the OPEN FILE DESCRIPTION, not with the
// process, so a second os.OpenFile in this process conflicts exactly as
// a second plugin process's would. If this ever passes silently — a
// second Records handed back — the guarantee has become an assertion,
// and the failure it admits is not a corrupt file but a silently
// truncated history: the second writer's sequence numbers restart, its
// events fold as stale, and a restart resumes from a record missing
// everything the loser wrote.
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

// TestRecords_LockIsReleasedOnClose is the control for the test above:
// a refusal that never lifts would make a plugin restart impossible,
// and a test that only checks the refusal cannot tell the two apart.
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

// TestRecords_SequenceSurvivesAReopen is the defect a per-process
// counter starting at zero produces, driven end to end.
//
// The fold refuses an event whose Seq does not advance, and it refuses
// it QUIETLY — the record comes back with Rejects bumped and nothing
// else moved. So the observable is not an error from Append; it is a
// record whose phase never left where the first process put it.
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

// TestRecords_ResumeCarriesTheLeaseAcrossManagers is the seam's whole
// reason for a durable record: the CreateEndpoint one-shot's lease has
// to reach the Join manager as an INIT-REBOOT, not as a fresh DISCOVER.
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
	// The acquisition manager's last event. It is a cancellation the
	// chassis itself asked for, and if the record treated it as a loss
	// the Join manager would resume nothing.
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

// TestRecords_TwoManagersGetTwoIDs pins the obligation the fold cannot
// check for itself.
//
// Two managers handed ONE id are folded as one and NOTHING detects it:
// a higher snapshot under one id is exactly what a renewal looks like,
// so the wire half silently undercounts by the first manager's total.
// The library says so in RecordEvent.Manager and leaves the uniqueness
// to the caller; this is the caller's side of it.
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

// TestRecords_StatsAccumulateAcrossManagers drives the pair of
// obligations together: distinct ids, and a rebaseline when the manager
// changes. The second manager's counters start at zero, so a chassis
// that reused one id would have the fold read the second snapshot as
// the first one going backwards.
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

// TestRecords_OneRecordStoreCallSite is the structural half of G-10.
//
// The lock above stops a second writer that OPENS the file. It cannot
// stop a second writer that never went through OpenRecords, and the way
// one appears is a second runtime.OpenRecordStore somewhere in the
// plugin. Counting the call sites is the only check that sees a caller
// nobody has written yet.
func TestRecords_OneRecordStoreCallSite(t *testing.T) {
	// Assembled rather than written out: a literal here would make
	// this file its own second hit and the count would never be 1.
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
			// Both spellings: the import alias this package uses and
			// the package's own name, so a second call site that
			// imported it plainly is not invisible to the count.
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

// TestRecords_TheACDPhaseSurvivesARestart is D23's durable half. In
// async the address is handed to the container while RFC 5227 section
// 2.1 is still running; if the plugin restarts inside that window the
// next process has to know the check never finished. The phase is read
// back OUT OF THE FILE here, by a second Records over the same path,
// because a fold that only kept it in memory would pass an in-process
// assertion and lose it on the restart this exists for.
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
		// async: handed out DURING the probe schedule. These are the
		// only two a restart is evidence about.
		{"probing", proto.ACDProbing, true},
		{"settling", proto.ACDSettling, true},
		// wait: handed out only after section 2.1 cleared it.
		{"announcing", proto.ACDAnnouncing, false},
		{"defending", proto.ACDDefending, false},
		// off ran no check -- and so does the END of every ordinary
		// acquisition, because cancelling a manager drops the lease and
		// the sub-machine goes idle with it. Reading this as unfinished
		// warns on every healthy container start.
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

			// The restart.
			second, err := OpenRecords(p, "instance-b")
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer func() { _ = second.Close() }()
			// Through the chassis's OWN resume path — the one
			// dhcp_manager.go calls — not the library record directly.
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

	// The phase must be on the WIRE of the record, not only in the
	// rebuilt struct: a reader that never wrote it would still pass the
	// rows above if Resume defaulted to the same value.
	p := filepath.Join(dir, "probing.jsonl")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !strings.Contains(string(raw), `"acd":`) {
		t.Error("the record file carries no acd field; a restart reads the phase from this file and nothing else")
	}
}

// TestResumption_IdleIsNotEvidenceOfAnUncheckedAddress pins the
// distinction the beta lane taught on 2026-09-04: proto.ACDIdle is the
// ABSENCE of evidence, not evidence of an unchecked address.
//
// The first version of this predicate was spelled as the negation of
// "cleared", so idle read as unfinished -- and idle is what the fold
// writes at the end of every ordinary acquisition, because cancelling
// the one-shot drops the lease and takes the sub-machine idle with it.
// The result was a WARNING on the healthy path of every container
// start, which is how a log stops being read.
//
// The asymmetry the first version was reaching for is kept: a phase the
// library adds later is not in the finished set and not idle, so it
// reads as unfinished and costs a log line rather than silently
// reading as clean. That is asserted here over the library's own
// enumeration, so a new phase arrives with a decision rather than a
// default.
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
	// The unknown-phase direction, driven rather than described: a
	// value the library does not print today must warn.
	unknown := proto.ACDPhase(len(proto.AllACDPhases()) + 7)
	if !(Resumption{ACD: unknown}).ACDUnfinished() {
		t.Errorf("ACDUnfinished(%v) = false for a phase this build does not know; an unknown phase must cost a log line, not read as clean", unknown)
	}
}
