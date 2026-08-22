// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	dNetwork "github.com/docker/docker/api/types/network"
)

// netOptions falls back to the docker API whenever it could not read the
// persisted file, and used to follow that fallback with a backfill save
// unconditionally. That turned every read failure into a WRITE over the
// file it had just failed to read.
//
// The two tests below are the two halves of the same rule, and they have
// to be read together: the write must not happen when there is a file,
// and it must still happen when there is not. Testing only the first
// would go green on a change that deleted the backfill outright, which
// is a different bug in the same line (#724).
//
// THEY ASSERT THE BYTES ON DISK, not the absence of an error. A schema
// refusal is not an error netOptions returns -- it falls back and
// succeeds -- so "no error" is exactly what the broken code produced
// while it was destroying the file. Only the file's contents can tell
// the two apart.

func backfillPlugin(opts map[string]string) *Plugin {
	return &Plugin{docker: &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net1": {Options: opts},
		},
	}}
}

// A file this build refuses to read must be exactly as it was
// afterwards. This is the downgrade: a v1 plugin started against a
// STATE_DIR a v2 plugin wrote. The file lives on a host bind mount that
// survives `docker plugin rm` and upgrade, so the v2 build WILL come
// back and read it -- unless the v1 build overwrote it in the meantime,
// in which case the network's real configuration is gone and there is no
// second copy anywhere.
func TestNetOptions_RefusedSchemaFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	path := filepath.Join(dir, "net1.json")
	// A file from a build that understands more than this one: a newer
	// schema, and a field this build has never heard of.
	future := []byte(`{"v":99,"mode":"macvlan","parent":"eth9","some_future_field":"keep me"}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}

	// Sanity: the refusal is a refusal, and it is distinguishable from
	// an absence. If this ever stops holding, the guard below is
	// passing for the wrong reason.
	if _, err := loadOptions("net1"); !errors.Is(err, errStateSchemaTooNew) {
		t.Fatalf("loadOptions on a v99 file = %v, want errStateSchemaTooNew", err)
	}
	if _, err := loadOptions("net1"); os.IsNotExist(err) {
		t.Fatal("a schema refusal reports as os.IsNotExist; a writer downstream cannot tell it from an absent file")
	}

	p := backfillPlugin(map[string]string{"mode": "macvlan", "parent": "eth0"})
	got, err := p.netOptions(context.Background(), "net1")
	if err != nil {
		t.Fatalf("netOptions: %v", err)
	}
	// The fallback itself is correct and must keep working: the docker
	// API is authoritative for everything in this struct.
	if got.Parent != "eth0" {
		t.Errorf("fallback returned parent %q, want the docker API's eth0", got.Parent)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the refused file is gone: %v", err)
	}
	if string(after) != string(future) {
		t.Errorf("a file this build refused to read was rewritten.\n before: %s\n  after: %s\n"+
			"A refusal must leave the file for the build that understands it. Overwriting it "+
			"turns a downgrade from 'declines to read' into 'destroys', which is the failure "+
			"the schema version exists to prevent (#724).", future, after)
	}
}

// The same rule for the failure nobody plans for. An unreadable file is
// not an empty one, and a disk that had a bad moment must not cost the
// network its configuration.
func TestNetOptions_UnreadableFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	path := filepath.Join(dir, "net1.json")
	// A symlink to a directory, for the reason spelled out in
	// state_durability_test.go: os.ReadFile follows it and gets EISDIR,
	// but rename(2) does not follow the final symlink component, so the
	// write is stopped only by the code's own choice and not by the
	// kernel refusing an unrelated operation. A plain directory here
	// would make this test pass against the pre-fix code.
	target := filepath.Join(dir, "not-a-file")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := loadOptions("net1"); err == nil || os.IsNotExist(err) {
		t.Fatalf("loadOptions on an unreadable file = %v, want a non-NotExist error", err)
	}

	p := backfillPlugin(map[string]string{"mode": "macvlan", "parent": "eth0"})
	if _, err := p.netOptions(context.Background(), "net1"); err != nil {
		t.Fatalf("netOptions: %v", err)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the path is gone: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("a transient read failure was answered with a write. An unreadable file " +
			"is not an empty one; the fallback is read-only and the backfill belongs to " +
			"absence alone (#724).")
	}
}

// And the other direction, which is the whole reason the backfill
// exists: a network created before persistence shipped has no file, and
// the next call must hit the disk path rather than the docker API
// forever.
func TestNetOptions_AbsentFileStillBackfills(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	path := filepath.Join(dir, "net1.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fixture is not absent: %v", err)
	}

	p := backfillPlugin(map[string]string{"mode": "macvlan", "parent": "eth0"})
	if _, err := p.netOptions(context.Background(), "net1"); err != nil {
		t.Fatalf("netOptions: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an absent options file was not backfilled: %v\n"+
			"Guarding the backfill must narrow it to absence, not remove it.", err)
	}
	// It must be readable by the path that will read it, and stamped,
	// or the next start falls back to the docker API again.
	got, err := loadOptions("net1")
	if err != nil {
		t.Fatalf("the backfilled file does not load: %v", err)
	}
	if got.Parent != "eth0" {
		t.Errorf("backfilled parent = %q, want eth0", got.Parent)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vo versionedOptions
	if err := json.Unmarshal(raw, &vo); err != nil {
		t.Fatal(err)
	}
	if vo.V != stateSchemaVersion {
		t.Errorf("backfilled file carries v%d, want v%d", vo.V, stateSchemaVersion)
	}
}
