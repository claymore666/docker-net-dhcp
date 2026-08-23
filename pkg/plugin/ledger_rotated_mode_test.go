// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// The ROTATED generation must be tightened too, and it is the copy that
// carries the most: a full retention window of MACs, leased addresses
// and hostnames, on a host bind mount that survives `docker plugin rm`.
//
// rotateIfNeeded moves the file with os.Rename, which leaves the inode's
// mode alone, and nothing ever opens ".1" again -- so before #724 the
// active ledger was tightened on every append and the rotated one stayed
// exactly as an older build had left it, indefinitely.
//
// The population this matters for is not an edge case. #708's release
// note promises the upgrade tightens hosts that have been running a
// while; a host that HAS a rotated ledger is precisely a host that has
// been running a while, so the promise was falsest where it claimed the
// most.
func TestLedger_RotatedGenerationIsTightenedOnUpgrade(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)

	// A host that rotated under a build that wrote 0644: the rotated
	// file is there and world-readable, and the active one does not
	// exist yet because this build has not appended since starting.
	rotated := l.path + ".1"
	body := `{"ts":"t0","kind":"bound","mac":"02:00:00:00:00:01","ip":"192.0.2.7"}` + "\n"
	if err := os.WriteFile(rotated, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	l.Append(ledgerEntry{TS: "t1", Kind: "bound", Network: "n", Endpoint: "e"})

	fi, err := os.Stat(rotated)
	if err != nil {
		t.Fatalf("the rotated ledger is gone: %v", err)
	}
	if got := fi.Mode().Perm(); got != stateFileMode {
		t.Errorf("rotated ledger mode = %#o, want %#o.\n"+
			"rename(2) does not touch the mode and nothing reopens the rotated file, so an "+
			"upgrade that tightens only the active ledger leaves a world-readable lease audit "+
			"trail on every host that had already rotated (#724).", got, stateFileMode)
	}

	// A chmod, not a rewrite. The rotated generation is the only record
	// of its window; tightening it must not cost the contents.
	b, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "02:00:00:00:00:01") {
		t.Errorf("the rotated ledger's contents were altered: %q", b)
	}
}

// A host that has never rotated is the normal case, and the chmod's own
// ENOENT must not become a counted failure -- ledger_write_failures is
// read by the integration health floor, so a spurious increment there
// fails runs for a file that correctly does not exist.
func TestLedger_NoRotatedGenerationIsNotAFailure(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)

	l.Append(ledgerEntry{TS: "t1", Kind: "bound", Network: "n", Endpoint: "e"})

	if got := failures.Load(); got != 0 {
		t.Errorf("ledger_write_failures = %d with no rotated file present, want 0", got)
	}
	if _, err := os.Stat(l.path + ".1"); !os.IsNotExist(err) {
		t.Errorf("a rotated file was created out of nothing: %v", err)
	}
}
