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

func backfillPlugin(opts map[string]string) *Plugin {
	return &Plugin{docker: &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net1": {Options: opts},
		},
	}}
}

func TestNetOptions_RefusedSchemaFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	path := filepath.Join(dir, "net1.json")
	future := []byte(`{"v":99,"mode":"macvlan","parent":"eth9","some_future_field":"keep me"}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}

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

func TestNetOptions_UnreadableFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	path := filepath.Join(dir, "net1.json")
	// ReadFile follows the link and gets EISDIR; rename(2) would replace the link itself (#724).
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
	// A backfill is stamped with the base schema, which a v2.0 build still reads (#724).
	if vo.V != stateSchemaVersionBase {
		t.Errorf("backfilled file carries v%d, want v%d", vo.V, stateSchemaVersionBase)
	}
}
