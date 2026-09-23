// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// /var/lib/net-dhcp is a read-write host bind mount, so a 0644 state file is readable by every host user (#708).
func TestStateFiles_AreNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	if err := saveOptions("net1", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	if err := saveTombstones([]tombstone{{
		NetworkID: "net1", Hostname: "web1", MacAddress: "02:bb:b5:d1:0c:0a", IPAddress: "192.168.99.50", DeletedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("saveTombstones: %v", err)
	}

	var failures atomic.Int32
	l := newLeaseLedger(filepath.Join(dir, ledgerFileName), &failures)
	l.Append(ledgerEntry{Kind: "bound", Network: "net1", Endpoint: "ep1", IP: "192.168.99.50"})
	if failures.Load() != 0 {
		t.Fatalf("ledger append failed %d time(s)", failures.Load())
	}

	for _, name := range []string{"net1.json", "tombstones.json", ledgerFileName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", name, got)
		}
	}
}

func TestLedger_TightensAnExistingWorldReadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ledgerFileName)
	seedFile(t, path, 0o644)

	var failures atomic.Int32
	newLeaseLedger(path, &failures).Append(ledgerEntry{Kind: "bound", Network: "n", Endpoint: "e"})

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("pre-existing ledger mode = %#o, want 0600", got)
	}
}

// An upgrade rewrites no state file: a v1.8.0 host showed a 0644 tombstones.json beside a 0600 network file (#804).
func TestStateDir_SweepTightensWhatAnUpgradeLeftBehind(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	loose := []string{"tombstones.json", "x.json.tmp"}
	for _, name := range loose {
		seedFile(t, filepath.Join(dir, name), 0o644)
	}
	const alreadyTight = "lease-records.jsonl"
	seedFile(t, filepath.Join(dir, alreadyTight), 0o600)
	narrower := map[string]os.FileMode{
		"operator-chose-0400.json": 0o400,
		"operator-chose-0000.json": 0o000,
		"operator-chose-0440.json": 0o400,
		"operator-chose-0500.json": 0o400,
		"operator-chose-0444.json": 0o400,
		"operator-chose-0404.json": 0o400,
	}
	seeded := map[string]os.FileMode{
		"operator-chose-0400.json": 0o400,
		"operator-chose-0000.json": 0o000,
		"operator-chose-0440.json": 0o440,
		"operator-chose-0500.json": 0o500,
		"operator-chose-0444.json": 0o444,
		"operator-chose-0404.json": 0o404,
	}
	for name, mode := range seeded {
		seedFile(t, filepath.Join(dir, name), mode)
	}
	sub := filepath.Join(dir, "capture")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("seed capture dir: %v", err)
	}
	nested := filepath.Join(sub, "request.json")
	seedFile(t, nested, 0o644)

	before := changeTime(t, filepath.Join(dir, alreadyTight))

	// ctime comes from a coarse clock, so two operations in one tick share it.
	time.Sleep(30 * time.Millisecond)

	var failures stampedCounter
	if _, err := prepareStateDir(&failures); err != nil {
		t.Fatalf("prepareStateDir: %v", err)
	}
	if got := failures.Load(); got != 0 {
		t.Errorf("state_file_chmod_failures = %d after a sweep that had nothing to fail on; want 0", got)
	}

	for _, name := range loose {
		if got := permOf(t, filepath.Join(dir, name)); got != 0o600 {
			t.Errorf("%s mode = %#o after the sweep, want 0600", name, got)
		}
	}
	if got := permOf(t, filepath.Join(dir, alreadyTight)); got != 0o600 {
		t.Errorf("%s mode = %#o, want 0600 unchanged", alreadyTight, got)
	}
	if after := changeTime(t, filepath.Join(dir, alreadyTight)); !after.Equal(before) {
		t.Errorf("%s was already 0600 and the sweep touched it anyway (ctime %v -> %v)",
			alreadyTight, before, after)
	}
	for name, want := range narrower {
		if got := permOf(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s was seeded %#o and is %#o after the sweep; want %#o. The sweep granted "+
				"access a mode an operator chose did not, which is what it exists to remove",
				name, seeded[name], got, want)
		}
	}
	if got := permOf(t, nested); got != 0o644 {
		t.Errorf("%s mode = %#o; the sweep recursed into a subdirectory, want 0644", nested, got)
	}
}

func TestStateDir_SweepFailuresAreCountedAndDoNotStopIt(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	for _, name := range []string{"a.json", "b.json"} {
		seedFile(t, filepath.Join(dir, name), 0o644)
	}

	real := chmodFile
	t.Cleanup(func() { chmodFile = real })
	chmodFile = func(path string, mode os.FileMode) error {
		if filepath.Base(path) == "a.json" {
			return fmt.Errorf("injected: %w", os.ErrPermission)
		}
		return real(path, mode)
	}

	var failures stampedCounter
	if _, err := prepareStateDir(&failures); err != nil {
		t.Fatalf("prepareStateDir returned %v; a chmod that fails must not fail startup", err)
	}
	if got := failures.Load(); got != 1 {
		t.Errorf("state_file_chmod_failures = %d after one refused chmod, want 1", got)
	}
	if got := permOf(t, filepath.Join(dir, "a.json")); got != 0o644 {
		t.Errorf("a.json mode = %#o; the injected refusal did not take, so this test proves nothing", got)
	}
	if got := permOf(t, filepath.Join(dir, "b.json")); got != 0o600 {
		t.Errorf("b.json mode = %#o after the file before it was refused, want 0600: "+
			"one failure ended the sweep", got)
	}
}

// chmod(2) follows symlinks, so the sweep must skip a link, whose own mode means nothing on Linux (#804).
func TestStateDir_SweepDoesNotChmodThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	outside := filepath.Join(t.TempDir(), "not-a-state-file")
	seedFile(t, outside, 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "tombstones.json")); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	var failures stampedCounter
	if _, err := prepareStateDir(&failures); err != nil {
		t.Fatalf("prepareStateDir: %v", err)
	}

	if got := permOf(t, outside); got != 0o644 {
		t.Errorf("%s mode = %#o; the sweep followed a symlink out of STATE_DIR and "+
			"chmod'ed a file that is not its own, want 0644", outside, got)
	}
	if got := failures.Load(); got != 0 {
		t.Errorf("state_file_chmod_failures = %d after a symlink was skipped, want 0", got)
	}
}

func TestStateDir_SweepCountsADirectoryItCannotRead(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "state")
	seedFile(t, notADir, 0o600)

	var failures stampedCounter
	sweepStateDirModes(notADir, &failures)

	if got := failures.Load(); got != 1 {
		t.Errorf("state_file_chmod_failures = %d after an unreadable STATE_DIR, want 1", got)
	}
}

// seedFile sets the mode with chmod after writing, since os.WriteFile applies the umask.
func seedFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}\n"), mode); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("seed %s: chmod %#o: %v", path, mode, err)
	}
	if got := permOf(t, path); got != mode {
		t.Fatalf("seed %s: mode = %#o, want %#o", path, got, mode)
	}
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// changeTime is the file's ctime, which chmod moves and mtime does not record.
func changeTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no syscall.Stat_t, so the untouched-file assertion has no observer", path)
	}
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}
