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
