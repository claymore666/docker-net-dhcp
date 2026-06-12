package plugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLedger(t *testing.T, failures *atomic.Int32) *leaseLedger {
	t.Helper()
	return newLeaseLedger(filepath.Join(t.TempDir(), ledgerFileName), failures)
}

func readLedgerLines(t *testing.T, path string) []ledgerEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer f.Close()
	var entries []ledgerEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e ledgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("ledger line %d is not valid JSON: %v\n%s", len(entries)+1, err, sc.Text())
		}
		entries = append(entries, e)
	}
	return entries
}

func TestLedger_AppendsEventsInOrder(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)
	base := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	tick := 0
	l.now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Minute) }

	for _, kind := range []string{"bound", "renew", "release"} {
		l.Append(ledgerEntry{
			Kind:      kind,
			Network:   "net1",
			Endpoint:  "ep1",
			Container: "ctr1",
			Hostname:  "host1",
			IP:        "192.168.99.50",
			MAC:       "02:bb:b5:d1:0c:0a",
		})
	}

	entries := readLedgerLines(t, l.path)
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	wantKinds := []string{"bound", "renew", "release"}
	var prev time.Time
	for i, e := range entries {
		if e.Kind != wantKinds[i] {
			t.Errorf("entry %d kind = %q, want %q", i, e.Kind, wantKinds[i])
		}
		ts, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			t.Errorf("entry %d ts %q is not RFC3339: %v", i, e.TS, err)
		}
		if !ts.After(prev) {
			t.Errorf("entry %d ts %v not after previous %v", i, ts, prev)
		}
		prev = ts
		if e.Network != "net1" || e.Endpoint != "ep1" || e.Container != "ctr1" ||
			e.Hostname != "host1" || e.IP != "192.168.99.50" || e.MAC != "02:bb:b5:d1:0c:0a" {
			t.Errorf("entry %d fields wrong: %+v", i, e)
		}
	}
	if failures.Load() != 0 {
		t.Errorf("failures = %d, want 0", failures.Load())
	}
}

func TestLedger_RotationBySize(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)
	l.maxSize = 512

	// Append until the first rotation fires, then assert no line was
	// lost across that boundary. (Multiple rotations deliberately drop
	// the oldest generation — retention is bounded to one rotated file
	// — so the invariant under test is per-boundary, not global.)
	appended := 0
	for ; appended < 100; appended++ {
		if _, err := os.Stat(l.path + ".1"); err == nil {
			break
		}
		l.Append(ledgerEntry{Kind: "renew", Network: "net1", Endpoint: fmt.Sprintf("ep%02d", appended), IP: "192.168.99.50"})
	}
	if _, err := os.Stat(l.path + ".1"); err != nil {
		t.Fatalf("no rotation within %d appends at maxSize=%d: %v", appended, l.maxSize, err)
	}

	rotated := readLedgerLines(t, l.path+".1")
	active := readLedgerLines(t, l.path)
	if got := len(rotated) + len(active); got != appended {
		t.Errorf("entries across rotation = %d, want %d", got, appended)
	}
	if len(active) == 0 {
		t.Error("active file empty — the rotation-triggering line should land in the fresh file")
	}
	// Order preserved across the boundary: rotated holds the oldest.
	if len(rotated) == 0 || rotated[0].Endpoint != "ep00" {
		t.Errorf("rotated file should start at ep00, got %+v", rotated)
	}
	if failures.Load() != 0 {
		t.Errorf("failures = %d, want 0", failures.Load())
	}
}

func TestLedger_RotationByAge(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)
	base := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	now := base
	l.now = func() time.Time { return now }

	l.Append(ledgerEntry{Kind: "bound", Network: "net1", Endpoint: "ep1"})
	now = base.Add(l.maxAge + time.Hour)
	l.Append(ledgerEntry{Kind: "renew", Network: "net1", Endpoint: "ep1"})

	old := readLedgerLines(t, l.path+".1")
	if len(old) != 1 || old[0].Kind != "bound" {
		t.Errorf("rotated file = %+v, want the single bound entry", old)
	}
	active := readLedgerLines(t, l.path)
	if len(active) != 1 || active[0].Kind != "renew" {
		t.Errorf("active file = %+v, want the single renew entry", active)
	}
}

func TestLedger_AgeAnchorSurvivesRestart(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)
	base := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return base }
	l.Append(ledgerEntry{Kind: "bound", Network: "net1", Endpoint: "ep1"})

	// Fresh leaseLedger over the same file = plugin restart. The age
	// anchor must come from the file's first entry, not the restart.
	l2 := newLeaseLedger(l.path, &failures)
	l2.now = func() time.Time { return base.Add(l2.maxAge + time.Hour) }
	l2.Append(ledgerEntry{Kind: "renew", Network: "net1", Endpoint: "ep1"})

	if old := readLedgerLines(t, l.path+".1"); len(old) != 1 || old[0].Kind != "bound" {
		t.Errorf("rotated file = %+v, want the pre-restart bound entry", old)
	}
}

func TestLedger_WriteFailureIsNonFatal(t *testing.T) {
	var failures atomic.Int32
	// A path whose parent is a regular file can never be created —
	// fails for root and non-root alike (ENOTDIR).
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := newLeaseLedger(filepath.Join(blocker, ledgerFileName), &failures)

	l.Append(ledgerEntry{Kind: "bound", Network: "net1", Endpoint: "ep1"})
	l.Append(ledgerEntry{Kind: "release", Network: "net1", Endpoint: "ep1"})

	if got := failures.Load(); got != 2 {
		t.Errorf("failures = %d, want 2 (one per append)", got)
	}
}

func TestLedger_ConcurrentAppendsLoseNothing(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)

	const goroutines, perG = 10, 20
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				l.Append(ledgerEntry{Kind: "renew", Network: "net1", Endpoint: fmt.Sprintf("ep%d", g)})
			}
		}(g)
	}
	wg.Wait()

	if got := len(readLedgerLines(t, l.path)); got != goroutines*perG {
		t.Errorf("entries = %d, want %d", got, goroutines*perG)
	}
	if failures.Load() != 0 {
		t.Errorf("failures = %d, want 0", failures.Load())
	}
}

func TestLedger_AuditDisabledByDefault(t *testing.T) {
	var failures atomic.Int32
	p := &Plugin{}
	p.ledger = newLeaseLedger(filepath.Join(t.TempDir(), ledgerFileName), &failures)

	// No audit_log opt: audit must be a no-op — zero filesystem
	// activity, not just an empty file.
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"}, DHCPNetworkOptions{}).withPlugin(p)
	m.audit("bound", "192.168.99.50")
	if _, err := os.Stat(p.ledger.path); !os.IsNotExist(err) {
		t.Fatalf("ledger file exists despite audit_log not set (stat err: %v)", err)
	}

	// Opt-in: same call writes.
	m2 := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"}, DHCPNetworkOptions{AuditLog: true}).withPlugin(p)
	m2.audit("bound", "192.168.99.50")
	entries := readLedgerLines(t, p.ledger.path)
	if len(entries) != 1 || entries[0].Kind != "bound" || entries[0].IP != "192.168.99.50" {
		t.Fatalf("opt-in audit entries = %+v, want one bound entry", entries)
	}
}
