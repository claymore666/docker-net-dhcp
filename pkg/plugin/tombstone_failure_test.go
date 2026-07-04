package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAddTombstone_SaveFailureBumpsHealthCounter exercises the failure
// path of addTombstone -> saveTombstones -> tombstoneWriteFailures.
// Operators rely on /Plugin.Health.tombstone_write_failures going
// non-zero to detect a degraded restart-stability window (disk full,
// EROFS, etc.); without this test the counter could be silently
// disconnected from saveTombstones errors and nobody would notice
// until a real disk problem masked another disk problem.
//
// We trigger the failure by pointing stateDir at a path whose parent
// is a regular file — os.MkdirAll fails on "not a directory", which
// short-circuits saveTombstones with a clean error.
func TestAddTombstone_SaveFailureBumpsHealthCounter(t *testing.T) {
	parent := t.TempDir()
	// A regular file masquerading as the parent of our state dir.
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte{}, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// stateDir = blocker/state — MkdirAll on this fails because
	// `blocker` is a regular file, not a directory.
	withStateDir(t, filepath.Join(blocker, "state"))

	p := newPluginForTest()

	// Sanity: counter starts at zero.
	if got := p.tombstoneWriteFailures.Load(); got != 0 {
		t.Fatalf("counter should start at 0, got %d", got)
	}

	p.addTombstone("net-A", "alpha", "aa:bb:cc:dd:ee:ff", "10.0.0.1", "")

	if got := p.tombstoneWriteFailures.Load(); got != 1 {
		t.Errorf("save failure must bump tombstoneWriteFailures: got %d, want 1", got)
	}
}

// TestSaveTombstones_DirCreationFailure mirrors the above at the
// saveTombstones level — the stateDir-as-child-of-regular-file trick
// gives us the MkdirAll error path, which is what surfaces the disk
// problem to addTombstone in production.
func TestSaveTombstones_DirCreationFailure(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte{}, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	withStateDir(t, filepath.Join(blocker, "state"))

	if err := saveTombstones([]tombstone{{NetworkID: "net-A"}}); err == nil {
		t.Fatal("expected error when stateDir parent is a regular file")
	}
}

// TestSaveOptions_DirCreationFailure is the saveOptions analogue —
// covers the equivalent MkdirAll error in the options-persistence
// code path, the one netOptions tries to backfill from on first call.
func TestSaveOptions_DirCreationFailure(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte{}, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	withStateDir(t, filepath.Join(blocker, "state"))

	if err := saveOptions("net-Z", DHCPNetworkOptions{Bridge: "br0"}); err == nil {
		t.Fatal("expected error when stateDir parent is a regular file")
	}
}

// TestDeleteOptions_PermissionError covers the non-IsNotExist branch
// of deleteOptions: when the state file exists but cannot be removed
// (e.g. the parent directory is read-only), the wrapping error must
// propagate so DeleteNetwork's caller can log it.
//
// Skipped under root because chmod 0o500 doesn't prevent writes for
// privileged users.
func TestDeleteOptions_PermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based DAC tests don't apply to root")
	}
	dir := t.TempDir()
	withStateDir(t, dir)
	// Create a real options file first so loadOptions could see it.
	if err := saveOptions("net-perm", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("setup save: %v", err)
	}
	// Make the parent dir read-only so os.Remove on the child fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := deleteOptions("net-perm"); err == nil {
		t.Fatal("expected error when state dir is read-only")
	}
}

// TestSaveOptions_CreateTempFailure covers the CreateTemp error arm of
// the atomic-write pipeline (#305): the state dir exists but is not
// writable, the operational shape of EROFS / disk-full at temp-file
// creation time. The Write/Close/Chmod arms downstream share this
// cause and stay uncovered on purpose — see #305.
//
// Skipped under root because chmod 0o555 doesn't prevent writes for
// privileged users.
func TestSaveOptions_CreateTempFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based DAC tests don't apply to root")
	}
	dir := t.TempDir()
	withStateDir(t, dir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := saveOptions("net-tmpfail", DHCPNetworkOptions{Bridge: "br0"}); err == nil {
		t.Fatal("expected error when state dir is not writable")
	}
}

// TestSaveTombstones_CreateTempFailure is the tombstone-writer
// analogue: same read-only-dir injection, same skip rationale.
func TestSaveTombstones_CreateTempFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based DAC tests don't apply to root")
	}
	dir := t.TempDir()
	withStateDir(t, dir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := saveTombstones([]tombstone{{NetworkID: "net-A"}}); err == nil {
		t.Fatal("expected error when state dir is not writable")
	}
}

// TestSaveOptions_RenameFailure covers the final arm of the pipeline:
// os.Rename onto a path occupied by a non-empty directory fails for
// any uid (ENOTEMPTY/EISDIR), so unlike the chmod tests this one also
// runs in the coverage workflow's root unit-test pass. Beyond the
// error itself it pins the two cleanup contracts of a failed save:
// no stray temp file left in the state dir, and the occupying path
// untouched (a crash mid-save must never destroy existing state).
func TestSaveOptions_RenameFailure(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	// Occupy the final path with a non-empty directory.
	final := filepath.Join(dir, "net-rename.json")
	if err := os.MkdirAll(filepath.Join(final, "occupant"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := saveOptions("net-rename", DHCPNetworkOptions{Bridge: "br0"}); err == nil {
		t.Fatal("expected error when final path is a non-empty directory")
	}

	leftovers, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("failed save must remove its temp file, found %v", leftovers)
	}
	if _, err := os.Stat(filepath.Join(final, "occupant")); err != nil {
		t.Errorf("failed save must leave the occupying path untouched: %v", err)
	}
}

// TestSaveTombstones_RenameFailure is the tombstone-writer analogue,
// asserting the same error-plus-cleanup contract for the shared
// tombstones.json path.
func TestSaveTombstones_RenameFailure(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	final := filepath.Join(dir, "tombstones.json")
	if err := os.MkdirAll(filepath.Join(final, "occupant"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := saveTombstones([]tombstone{{NetworkID: "net-A"}}); err == nil {
		t.Fatal("expected error when tombstones.json is a non-empty directory")
	}

	leftovers, err := filepath.Glob(filepath.Join(dir, ".tombstones.*.tmp"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("failed save must remove its temp file, found %v", leftovers)
	}
	if _, err := os.Stat(filepath.Join(final, "occupant")); err != nil {
		t.Errorf("failed save must leave the occupying path untouched: %v", err)
	}
}
