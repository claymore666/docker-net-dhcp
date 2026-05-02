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

func TestTombstones_RoundtripAndConsume(t *testing.T) {
	withStateDir(t, t.TempDir())

	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		endpointMACs:   make(map[string]string),
	}

	// Empty state: no tombstones to consume.
	if mac, ok := p.consumeTombstone("net-A"); ok {
		t.Errorf("consumeTombstone on empty state returned (%q, true), want (\"\", false)", mac)
	}

	// One tombstone for net-A → next consumeTombstone for net-A wins.
	p.addTombstone("net-A", "02:42:ac:11:00:01")
	mac, ok := p.consumeTombstone("net-A")
	if !ok || mac != "02:42:ac:11:00:01" {
		t.Errorf("consumeTombstone net-A: got (%q, %v), want (02:42:ac:11:00:01, true)", mac, ok)
	}
	// Tombstone is consumed exactly once.
	if mac, ok := p.consumeTombstone("net-A"); ok {
		t.Errorf("second consumeTombstone returned (%q, true); should be empty after consume", mac)
	}
}

func TestTombstones_DifferentNetworksDoNotMix(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		endpointMACs:   make(map[string]string),
	}
	p.addTombstone("net-A", "aa:aa:aa:aa:aa:aa")
	p.addTombstone("net-B", "bb:bb:bb:bb:bb:bb")

	if mac, ok := p.consumeTombstone("net-A"); !ok || mac != "aa:aa:aa:aa:aa:aa" {
		t.Errorf("net-A consume: got (%q, %v)", mac, ok)
	}
	if mac, ok := p.consumeTombstone("net-B"); !ok || mac != "bb:bb:bb:bb:bb:bb" {
		t.Errorf("net-B consume: got (%q, %v)", mac, ok)
	}
}

func TestTombstones_TwoOnSameNetworkBothSkipped(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		endpointMACs:   make(map[string]string),
	}
	p.addTombstone("net-A", "aa:aa:aa:aa:aa:aa")
	p.addTombstone("net-A", "bb:bb:bb:bb:bb:bb")

	// Two matches on same network → ambiguous, return ok=false.
	// The point is to avoid handing one container's MAC to a
	// concurrently-restarting peer.
	if mac, ok := p.consumeTombstone("net-A"); ok {
		t.Errorf("consumeTombstone with 2 candidates should return ok=false, got (%q, true)", mac)
	}
}

func TestTombstones_ExpiredEntriesPruned(t *testing.T) {
	withStateDir(t, t.TempDir())
	// Hand-craft an expired entry directly to disk (faster than
	// sleeping for tombstoneTTL in a test).
	old := []tombstone{{
		NetworkID:  "net-A",
		MacAddress: "ff:ff:ff:ff:ff:ff",
		DeletedAt:  time.Now().Add(-2 * tombstoneTTL),
	}}
	if err := saveTombstones(old); err != nil {
		t.Fatalf("saveTombstones: %v", err)
	}
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		endpointMACs:   make(map[string]string),
	}
	if mac, ok := p.consumeTombstone("net-A"); ok {
		t.Errorf("expired tombstone should not be consumed, got (%q, true)", mac)
	}
}

func TestRememberAndTakeEndpointMAC(t *testing.T) {
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		endpointMACs:   make(map[string]string),
	}
	p.rememberEndpointMAC("ep-1", "02:42:ac:11:00:02")
	mac, ok := p.takeEndpointMAC("ep-1")
	if !ok || mac != "02:42:ac:11:00:02" {
		t.Errorf("take after remember: got (%q, %v)", mac, ok)
	}
	if _, ok := p.takeEndpointMAC("ep-1"); ok {
		t.Errorf("take must remove the entry it returned")
	}
	// Empty MAC must not be remembered (avoids polluting map for
	// failed CreateEndpoints).
	p.rememberEndpointMAC("ep-2", "")
	if _, ok := p.takeEndpointMAC("ep-2"); ok {
		t.Errorf("rememberEndpointMAC on empty MAC must be a no-op")
	}
}
