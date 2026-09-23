// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddTombstone_SaveFailureBumpsHealthCounter(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte{}, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	withStateDir(t, filepath.Join(blocker, "state"))

	p := newPluginForTest()

	if got := p.tombstoneWriteFailures.Load(); got != 0 {
		t.Fatalf("counter should start at 0, got %d", got)
	}

	p.addTombstone("net-A", "alpha", "aa:bb:cc:dd:ee:ff", "10.0.0.1", "")

	if got := p.tombstoneWriteFailures.Load(); got != 1 {
		t.Errorf("save failure must bump tombstoneWriteFailures: got %d, want 1", got)
	}
}

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

func TestDeleteOptions_PermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based DAC tests don't apply to root")
	}
	dir := t.TempDir()
	withStateDir(t, dir)
	if err := saveOptions("net-perm", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("setup save: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := deleteOptions("net-perm"); err == nil {
		t.Fatal("expected error when state dir is read-only")
	}
}

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

// rename(2) onto a non-empty directory fails for any uid, root included (ENOTEMPTY or EISDIR).
func TestSaveOptions_RenameFailure(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
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
