package runtime

import (
	"bufio"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/claymore666/dhcp-golib/lease"
)

// m4TornChildEnv carries the journal path to the child half of
// TestAKilledWritersFragmentDoesNotEatALiveWritersEvent. Its presence is what
// switches that test into the writer that dies.
const m4TornChildEnv = "DHCP_GOLIB_TORN_WRITER_PATH"

// capFileSize lowers RLIMIT_FSIZE to n bytes for the caller's process, so that
// a Write past n returns a SHORT WRITE with the bytes it managed already on
// disk, and returns the function that puts the limit back.
//
// It is the only mechanism here that produces a real partial line from a real
// write: ENOSPC and EIO do the same in the field and neither can be arranged in
// a test. SIGXFSZ has to be ignored first, or the kernel kills the process
// instead of letting the write fail.
//
// The caller LIFTS THE CEILING BEFORE ASSERTING, and that is not tidiness: the
// whole claim is that the store still works after a short write, and a store
// still under the ceiling cannot write anything, so a test that left it on
// would be measuring the ceiling instead of the repair. The limit is
// process-wide, so it is also restored by t.Cleanup if an assertion fails
// first.
func capFileSize(t *testing.T, n int64) func() {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatalf("reading RLIMIT_FSIZE: %v", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: uint64(n), Max: old.Max}); err != nil {
		t.Fatalf("lowering RLIMIT_FSIZE to %d: %v", n, err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Fatalf("restoring RLIMIT_FSIZE: %v", err)
		}
		signal.Reset(syscall.SIGXFSZ)
	}
	t.Cleanup(restore)
	return restore
}

// tearByShortWrite appends one event under a file-size ceiling so that the
// write lands partially, and returns the fragment now on disk. It fails the
// test if the write SUCCEEDED, because a passing assertion after a successful
// write would be testing nothing.
func tearByShortWrite(t *testing.T, s *RecordStore, path string) []byte {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Twenty bytes past the end: the next line does not fit and is cut inside.
	restore := capFileSize(t, fi.Size()+20)
	err = s.Append(lease.RecordEvent{ID: "rec-2", Seq: 1, Op: lease.OpCreate, Scope: "net-a"})
	restore()
	if err == nil {
		t.Fatal("the Append under a file-size ceiling succeeded; nothing was torn and the assertions below would pass for the wrong reason")
	}
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		t.Fatalf("the file still ends at a line boundary after the short write: %q", b)
	}
	return b
}

// TestAShortWriteLeavesTheStoreUsableAndTheNextEventSurvives is review round
// 3's blocking finding, scenario (a): ONE process, no crash at all.
//
// A Write that returns short leaves its partial bytes on disk and returns an
// error; the store stays open and the caller goes on using it. Nothing about
// that is exotic — ENOSPC and EIO reach it in the field, on a journal this
// package ships without rotation — and until this round the next event was
// written onto the fragment and both were lost, with Damage reporting one line
// for the two of them and Rebuild reporting nothing at all.
//
// The event written next is the restart path's adopt: durableWrite fsyncs it
// precisely because nothing can re-derive it.
func TestAShortWriteLeavesTheStoreUsableAndTheNextEventSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Append(lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate, Scope: "net-a"}); err != nil {
		t.Fatalf("the first Append: %v", err)
	}
	fragment := tearByShortWrite(t, s, path)
	t.Logf("the file after the short write: %q", fragment)

	adopt := lease.RecordEvent{ID: "rec-9", Seq: 1, Op: lease.OpAdopt, Scope: "net-a", Family: lease.FamilyV4}
	if err := s.Append(adopt); err != nil {
		t.Fatalf("the Append after the short write: %v", err)
	}

	// The repair happened inside that Append, so the count is readable BEFORE
	// any Load — and it is already what the Load below reports. Without this
	// the store can write the terminator and count nothing, and then say
	// "no damage" about a file it has just repaired.
	if d := s.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage after the repairing Append and before any Load = %s, want 1 skipped", d)
	}
	evs, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rb := lease.Rebuild(evs)
	if _, ok := rb.ByID("rec-9"); !ok {
		ids := make([]string, len(evs))
		for i, ev := range evs {
			ids[i] = ev.ID + "/" + ev.Op.String()
		}
		t.Fatalf("the fsynced adopt is gone; Load returned %v. It was written onto the short write's fragment and both were lost", ids)
	}
	if _, ok := rb.ByID("rec-1"); !ok {
		t.Error("the event written before the short write is gone")
	}
	if d := s.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage = %s, want exactly 1 skipped: the short write's own line, and not one number for two", d)
	}
}

// TestAKilledWritersFragmentDoesNotEatALiveWritersEvent is defeat row D-9,
// driven at last by a writer that is really killed.
//
// The row — two writers, one file — was marked CLOSED on "one line per write
// under O_APPEND, an instance id, a per-record Seq the fold rejects a
// regression of", and its test appended whole lines from two stores. None of
// that says anything about the case the row is named for: one writer dying
// with half a line on disk while the other is mid-flight. Review round 3
// reopened the row.
//
// What is REAL here: a second process, a partial line produced by a real
// write() in that process, and a real SIGKILL. What is not: the kill does not
// land inside the write syscall — no test can place it there deterministically
// — so the sequence is arranged as short write, then kill. The on-disk state
// the survivor meets is the same one either order leaves, which is the state
// the row is about.
func TestAKilledWritersFragmentDoesNotEatALiveWritersEvent(t *testing.T) {
	if path := os.Getenv(m4TornChildEnv); path != "" {
		tornWriterChild(t, path)
		return
	}

	path := filepath.Join(t.TempDir(), "records.jsonl")
	survivor, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("OpenRecordStore, the writer that stays up: %v", err)
	}
	defer func() { _ = survivor.Close() }()

	// The name is taken from t.Name() rather than written out: a filter spelled
	// twice stops matching the moment the test is renamed, and a child that
	// runs no test exits 0.
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v=true", "-test.count=1")
	cmd.Env = append(os.Environ(), m4TornChildEnv+"="+path)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the writer that dies: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	// The child says TORN once its half line is on disk. Reading the marker is
	// what orders the kill after the tear, with no clock in it.
	sc := bufio.NewScanner(out)
	torn := false
	for sc.Scan() {
		if sc.Text() == "TORN" {
			torn = true
			break
		}
	}
	if !torn {
		t.Fatalf("the child never reported a torn write (scanner error: %v)", sc.Err())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the writer: %v", err)
	}
	killed = true
	_ = cmd.Wait()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b) == 0 || b[len(b)-1] == '\n' {
		t.Fatalf("the dead writer left no fragment: %q", b)
	}

	// The survivor never reopened the file and cannot know the peer is gone.
	adopt := lease.RecordEvent{ID: "rec-9", Seq: 1, Op: lease.OpAdopt, Scope: "net-a", Family: lease.FamilyV4}
	if err := survivor.Append(adopt); err != nil {
		t.Fatalf("the survivor's Append: %v", err)
	}

	// The repair happened inside that Append, so the count is readable BEFORE
	// any Load — and it is already what the Load below reports. Without this
	// the store can write the terminator and count nothing, and then say
	// "no damage" about a file it has just repaired.
	if d := survivor.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage after the repairing Append and before any Load = %s, want 1 skipped", d)
	}
	evs, err := survivor.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rb := lease.Rebuild(evs)
	if _, ok := rb.ByID("rec-9"); !ok {
		ids := make([]string, len(evs))
		for i, ev := range evs {
			ids[i] = ev.ID + "/" + ev.Op.String()
		}
		t.Fatalf("the survivor's fsynced adopt is gone; Load returned %v", ids)
	}
	if _, ok := rb.ByID("rec-1"); !ok {
		t.Error("the line the dead writer completed before its short write is gone")
	}
	if d := survivor.Damage(); d.Skipped != 1 || d.TornTail != 0 {
		t.Errorf("damage = %s, want exactly 1 skipped: the dead writer's half line", d)
	}
}

// tornWriterChild is the half of the test above that runs in the second
// process: it writes one whole line, tears the next with a real short write,
// says so, and then blocks until it is killed.
func tornWriterChild(t *testing.T, path string) {
	t.Helper()
	s, err := OpenRecordStore(path)
	if err != nil {
		t.Fatalf("child: OpenRecordStore: %v", err)
	}
	if err := s.Append(lease.RecordEvent{ID: "rec-1", Seq: 1, Op: lease.OpCreate, Scope: "net-a"}); err != nil {
		t.Fatalf("child: the first Append: %v", err)
	}
	tearByShortWrite(t, s, path)
	if _, err := os.Stdout.WriteString("TORN\n"); err != nil {
		t.Fatalf("child: reporting the tear: %v", err)
	}
	// Block on a read that never completes. The parent kills this process; a
	// child that exited on its own would be a writer that shut down cleanly,
	// which is not the case the row is about.
	var one [1]byte
	_, _ = os.Stdin.Read(one[:])
	t.Fatal("child: the parent did not kill it")
}
