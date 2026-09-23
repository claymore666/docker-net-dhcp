// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// os.Rename keeps the inode's mode and nothing reopens the rotated file, so it is
// tightened on its own (#724).
func TestLedger_RotatedGenerationIsTightenedOnUpgrade(t *testing.T) {
	var failures atomic.Int32
	l := testLedger(t, &failures)

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

	b, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "02:00:00:00:00:01") {
		t.Errorf("the rotated ledger's contents were altered: %q", b)
	}
}

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
