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

// TestStateFiles_AreNotWorldReadable pins the mode of every file the
// plugin creates under stateDir.
//
// /var/lib/net-dhcp is an rbind rw HOST mount, so at 0644 the container
// MACs, IPs, hostnames and the whole lease audit trail were readable by
// any user on the host. Nothing stored is a credential, so this is not a
// privilege boundary -- it is simply free, and a mode nobody asserted is
// a mode that drifts (#708).
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
		// The literal, not stateFileMode: asserting a constant against
		// itself passes whatever the constant becomes.
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", name, got)
		}
	}
}

// TestLedger_TightensAnExistingWorldReadableFile is the upgrade path.
// O_CREATE's mode applies only when the file is created, and this ledger
// outlives upgrades on a host bind mount -- so without the explicit
// chmod, a file written by an older version stays 0644 forever and the
// fix would be invisible on every host that already ran the plugin.
func TestLedger_TightensAnExistingWorldReadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ledgerFileName)
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

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

// TestStateDir_SweepTightensWhatAnUpgradeLeftBehind is #804.
//
// stateFileMode reaches a file when the plugin WRITES it, and an
// upgrade writes nothing: tombstones.json is rewritten only when a
// tombstone is laid or consumed. A production host upgraded to v1.8.0
// was observed with a 0600 network file beside a 0644 tombstones.json,
// and the docs claimed the upgrade had tightened both.
//
// It drives prepareStateDir, which is what NewPlugin calls, so deleting
// the sweep from the startup path fails here and not only in a test of
// a function nothing calls.
//
// The narrower files are the controls for "tightens only". Without
// them this passes against an implementation that chmods every file it
// finds to 0600, which WIDENS a mode an operator chose, and it does so
// for an incomparable mode (0440, 0500) as well as a narrower one. The
// subdirectory is the control for "does not recurse": a request
// capture directory can live under STATE_DIR and its contents are not
// state files.
func TestStateDir_SweepTightensWhatAnUpgradeLeftBehind(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	// The 1.x name and a leftover of a crashed 2.0 write. Neither is
	// enumerated by the sweep; both are just files in the directory.
	loose := []string{"tombstones.json", "x.json.tmp"}
	for _, name := range loose {
		seedFile(t, filepath.Join(dir, name), 0o644)
	}
	const alreadyTight = "lease-records.jsonl"
	seedFile(t, filepath.Join(dir, alreadyTight), 0o600)
	// The controls for "tightens only", in the two shapes that claim
	// can fail in.
	//
	// 0400 and 0000 are narrower than 0600, so the answer is the mode
	// they already have. Without them this passes against a sweep that
	// chmods every file it finds and WIDENS a mode an operator chose.
	//
	// 0440, 0500, 0444 and 0404 are neither wider nor narrower than
	// 0600, which is the case a family that stops at comparable modes
	// never reaches. A sweep that writes 0600 over them adds owner
	// write, which the release note says it does not do. The answer is
	// the seeded mode with every bit outside 0600 cleared.
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

	// Timestamps are stamped from a coarse clock, so two operations
	// inside one tick share a ctime. Without this wait an unchanged
	// reading below could be the clock and not the file.
	time.Sleep(30 * time.Millisecond)

	var failures stampedCounter
	if _, err := prepareStateDir(&failures); err != nil {
		t.Fatalf("prepareStateDir: %v", err)
	}
	if got := failures.Load(); got != 0 {
		t.Errorf("state_file_chmod_failures = %d after a sweep that had nothing to fail on; want 0", got)
	}

	for _, name := range loose {
		// The literal, not stateFileMode: asserting a constant against
		// itself passes whatever the constant becomes.
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

// TestStateDir_SweepFailuresAreCountedAndDoNotStopIt drives the arm a
// green run never reaches.
//
// Two things are asserted that a "return on the first error" sweep
// would break: the counter carries one tick per file, and the file
// AFTER the failing one is still tightened. The second is the outside
// evidence; the counter alone would pass against a sweep that gave up.
func TestStateDir_SweepFailuresAreCountedAndDoNotStopIt(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	// Sorted order is the readdir order os.ReadDir guarantees, so
	// "a.json" is reached before "b.json".
	for _, name := range []string{"a.json", "b.json"} {
		seedFile(t, filepath.Join(dir, name), 0o644)
	}

	// os.Chmod succeeds on both files for the user running these tests,
	// and always succeeds for root, so the refusal is injected at the
	// call instead of arranged on the filesystem.
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

// TestStateDir_SweepDoesNotChmodThroughASymlink is the containment
// control.
//
// chmod(2) follows symlinks, so a sweep that chmods every name readdir
// returns applies a state file's mode to whatever a link points at,
// which can be any file on the host. Nothing on disk inside STATE_DIR
// records that it happened, so the observer has to be the target's own
// mode, read from the other directory.
//
// A link is not a regular file, so skipping it is also the right answer
// for the link's own sake: its mode means nothing on Linux.
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
	// A skipped entry is not a failure to report: there was nothing the
	// sweep was entitled to tighten.
	if got := failures.Load(); got != 0 {
		t.Errorf("state_file_chmod_failures = %d after a symlink was skipped, want 0", got)
	}
}

// TestStateDir_SweepCountsADirectoryItCannotRead closes the vacuity
// hole. A sweep that examined nothing reports the same zero as a sweep
// that found nothing to tighten, and the first leaves every old file
// loose. Driven with a regular file where the directory should be,
// which readdir refuses for root as well.
func TestStateDir_SweepCountsADirectoryItCannotRead(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "state")
	seedFile(t, notADir, 0o600)

	var failures stampedCounter
	sweepStateDirModes(notADir, &failures)

	if got := failures.Load(); got != 1 {
		t.Errorf("state_file_chmod_failures = %d after an unreadable STATE_DIR, want 1", got)
	}
}

// seedFile writes path and puts it at exactly mode.
//
// os.WriteFile applies the process umask, so a file seeded 0644 under
// umask 077 arrives at 0600 and the case it was seeding is gone. The
// test then measures the umask and reports the result as the product's
// doing: the symlink case accuses the sweep of following a link out of
// STATE_DIR, and the failure case accuses the injected refusal of not
// taking. The chmod is what makes the mode the test's; the read-back is
// what makes that a measurement rather than an intention.
func seedFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}"), mode); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("seed %s: chmod %#o: %v", path, mode, err)
	}
	if got := permOf(t, path); got != mode {
		t.Fatalf("seed %s: mode = %#o, want %#o", path, got, mode)
	}
}

// permOf is the file's permission bits, or a fatal error.
func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// changeTime is the file's ctime, which chmod moves and which nothing
// else in these tests touches. mtime is no use here: chmod does not
// change it, so an untouched mtime is what a chmod'd file looks like.
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
