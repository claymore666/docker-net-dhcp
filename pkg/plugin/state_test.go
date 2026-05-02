package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withStateDir overrides the package-level stateDir for the duration of
// the test, restoring the previous value via t.Cleanup.
func withStateDir(t *testing.T, dir string) {
	t.Helper()
	prev := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = prev })
}

func TestSaveLoadOptions_Roundtrip(t *testing.T) {
	withStateDir(t, t.TempDir())

	want := DHCPNetworkOptions{
		Mode:         ModeMacvlan,
		Parent:       "ens18",
		IPv6:         true,
		LeaseTimeout: 45 * time.Second,
		Gateway:      "192.168.0.1",
	}
	if err := saveOptions("net123", want); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	got, err := loadOptions("net123")
	if err != nil {
		t.Fatalf("loadOptions: %v", err)
	}
	if got != want {
		t.Errorf("roundtrip mismatch:\n  got  %+v\n  want %+v", got, want)
	}
}

func TestLoadOptions_Missing(t *testing.T) {
	withStateDir(t, t.TempDir())
	_, err := loadOptions("never-saved")
	if err == nil {
		t.Fatal("expected error for missing options, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist (or wrapped); got %T %v", err, err)
	}
}

func TestLoadOptions_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	// Write a deliberately corrupt file
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := loadOptions("bad")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	// Important: this must NOT be an os.ErrNotExist, because callers
	// distinguish "not persisted yet" (fall back to docker API) from
	// "corrupt" (log and still fall back, but loudly).
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("corrupt JSON should NOT report as ErrNotExist; got %v", err)
	}
}

func TestDeleteOptions_Idempotent(t *testing.T) {
	withStateDir(t, t.TempDir())
	// Delete on a never-saved id must not error
	if err := deleteOptions("ghost"); err != nil {
		t.Errorf("deleteOptions on missing file should be nil, got %v", err)
	}
	// Save and delete, then delete again
	if err := saveOptions("real", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	if err := deleteOptions("real"); err != nil {
		t.Errorf("first delete: %v", err)
	}
	if err := deleteOptions("real"); err != nil {
		t.Errorf("second delete should still be nil, got %v", err)
	}
}

// TestSaveOptions_AtomicNoTornFile verifies the temp-then-rename
// strategy: if a save fails between Write and Rename (we can't
// realistically trigger that, but we can at least confirm there's no
// leftover .tmp file polluting the state dir on a successful save).
func TestSaveOptions_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	for i := 0; i < 5; i++ {
		if err := saveOptions("net-many", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
			t.Fatalf("save iter %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSaveOptions_CreatesStateDir(t *testing.T) {
	parent := t.TempDir()
	// Point at a sub-path that does NOT yet exist
	subdir := filepath.Join(parent, "nested", "state")
	withStateDir(t, subdir)
	if err := saveOptions("net1", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions should auto-create state dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subdir, "net1.json")); err != nil {
		t.Errorf("expected file to exist: %v", err)
	}
}
