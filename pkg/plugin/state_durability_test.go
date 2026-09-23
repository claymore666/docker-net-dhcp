// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func readRawTombstones(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(tombstoneFilePath())
	if err != nil {
		t.Fatalf("read tombstones: %v", err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("tombstones file is not a JSON array of objects (%s): %v", data, err)
	}
	return raw
}

func TestSaveOptions_StampsSchemaVersion(t *testing.T) {
	withStateDir(t, t.TempDir())

	if err := saveOptions("net-versioned", DHCPNetworkOptions{
		Mode:   ModeMacvlan,
		Parent: "eth0",
		IPv6:   true,
	}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}

	path, err := stateFilePath("net-versioned")
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read options: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("options file is not a JSON object (%s): %v", data, err)
	}

	v, ok := raw["v"]
	if !ok {
		t.Fatalf("options file carries no schema version: %s", data)
	}
	// A null-IPAM network's file is unchanged since schema 1, so it carries the base version (#110, #724).
	if got := v.(float64); int(got) != stateSchemaVersionBase {
		t.Errorf(`"v" = %v, want %d`, got, stateSchemaVersionBase)
	}
	if _, ok := raw["Parent"]; !ok {
		t.Errorf("option fields are no longer at the top level, so an older build cannot read this file: %s", data)
	}
}

func TestLoadOptions_LegacyFileHasNoVersion(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	legacy := `{"Mode":"macvlan","Parent":"eth0","IPv6":true}`
	if err := os.WriteFile(filepath.Join(dir, "net-legacy.json"), []byte(legacy), stateFileMode); err != nil {
		t.Fatalf("write legacy options: %v", err)
	}

	got, err := loadOptions("net-legacy")
	if err != nil {
		t.Fatalf("loadOptions on a pre-version file: %v", err)
	}
	if got.Mode != ModeMacvlan || got.Parent != "eth0" || !got.IPv6 {
		t.Errorf("legacy options decoded wrong: %+v", got)
	}
}

func TestLoadOptions_RefusesFutureSchema(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	future := `{"v":` + strconv.Itoa(stateSchemaVersion+1) + `,"Mode":"macvlan","Parent":"eth0"}`
	if err := os.WriteFile(filepath.Join(dir, "net-future.json"), []byte(future), stateFileMode); err != nil {
		t.Fatalf("write future options: %v", err)
	}

	if _, err := loadOptions("net-future"); err == nil {
		t.Fatal("loadOptions accepted a schema version it does not understand; a v1 reading of a v2 file can attach a network on the wrong parent")
	}
}

// The tombstone file stays unversioned: a 60 s cache has nothing to migrate, and an envelope around its top-level
// array reads as corrupt to older builds (#724).
func TestSaveTombstones_CarriesNoSchemaVersion(t *testing.T) {
	withStateDir(t, t.TempDir())

	if err := saveTombstones([]tombstone{
		{NetworkID: "net1", MacAddress: "02:42:ac:11:00:02", DeletedAt: time.Now()},
	}); err != nil {
		t.Fatalf("saveTombstones: %v", err)
	}

	raw := readRawTombstones(t)
	if len(raw) != 1 {
		t.Fatalf("got %d records, want 1", len(raw))
	}
	if _, ok := raw[0]["v"]; ok {
		t.Errorf("tombstone records carry a schema version; a 60s cache does not need one and an envelope for it would break downgrades: %+v", raw[0])
	}
}

func TestLoadTombstones_RecordsWithoutVersionLoad(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	legacy := `[{"network_id":"net1","mac_address":"02:42:ac:11:00:02","deleted_at":"` +
		time.Now().Format(time.RFC3339Nano) + `"}]`
	if err := os.WriteFile(tombstoneFilePath(), []byte(legacy), stateFileMode); err != nil {
		t.Fatalf("write legacy tombstones: %v", err)
	}

	ts, err := loadTombstones()
	if err != nil {
		t.Fatalf("loadTombstones: %v", err)
	}
	if len(ts) != 1 || ts[0].MacAddress != "02:42:ac:11:00:02" {
		t.Fatalf("a pre-version tombstone was dropped: %+v", ts)
	}
}

// An empty list marshals to `null`, not `[]`, and must not be quarantined (#724).
func TestLoadTombstones_LegacyNullFile(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	if err := os.WriteFile(tombstoneFilePath(), []byte("null"), stateFileMode); err != nil {
		t.Fatalf("write null tombstones: %v", err)
	}

	ts, err := loadTombstones()
	if err != nil {
		t.Fatalf("loadTombstones on a legacy null file: %v", err)
	}
	if len(ts) != 0 {
		t.Errorf("got %d records from a null file, want 0: %+v", len(ts), ts)
	}
	if aside := quarantinedFiles(t, dir); len(aside) != 0 {
		t.Errorf("a legacy null file was quarantined as corrupt: %v", aside)
	}
}

func TestLoadTombstones_RoundtripsAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	if err := saveTombstones(nil); err != nil {
		t.Fatalf("saveTombstones(nil): %v", err)
	}
	ts, err := loadTombstones()
	if err != nil {
		t.Fatalf("loadTombstones: %v", err)
	}
	if len(ts) != 0 {
		t.Errorf("got %d records, want 0: %+v", len(ts), ts)
	}
}

func TestLoadTombstones_QuarantinesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	const corrupt = `[{"network_id":"net1","mac_address":"02:42:ac:11:00:02"`
	if err := os.WriteFile(tombstoneFilePath(), []byte(corrupt), stateFileMode); err != nil {
		t.Fatalf("write corrupt tombstones: %v", err)
	}

	_, err := loadTombstones()
	if err == nil {
		t.Fatal("loadTombstones accepted a truncated file")
	}
	if !errors.Is(err, errTombstonesQuarantined) {
		t.Errorf("error does not wrap errTombstonesQuarantined, so a caller cannot tell a refusal from an absence: %v", err)
	}
	if _, err := os.Stat(tombstoneFilePath()); !os.IsNotExist(err) {
		t.Errorf("the corrupt file is still at its original name (stat err = %v); the next write will land on top of it", err)
	}

	aside := quarantinedFiles(t, dir)
	if len(aside) != 1 {
		t.Fatalf("got %d quarantined files, want 1: %v", len(aside), aside)
	}
	got, err := os.ReadFile(filepath.Join(dir, aside[0]))
	if err != nil {
		t.Fatalf("read quarantined file: %v", err)
	}
	if string(got) != corrupt {
		t.Errorf("quarantined bytes differ from the original:\n  got  %s\n  want %s", got, corrupt)
	}
}

func TestTombstoneStore_CorruptFileSurvivesTheNextWrite(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	const corrupt = `[{"network_id":"doomed","mac_address":"02:42:ac:11:00:99"`
	if err := os.WriteFile(tombstoneFilePath(), []byte(corrupt), stateFileMode); err != nil {
		t.Fatalf("write corrupt tombstones: %v", err)
	}

	var s tombstoneStore
	if err := s.add("net-new", "host-new", "02:42:ac:11:00:07", "192.0.2.7", ""); err != nil {
		t.Fatalf("add: %v", err)
	}

	raw := readRawTombstones(t)
	if len(raw) != 1 || raw[0]["network_id"] != "net-new" {
		t.Fatalf("the new tombstone was not written: %+v", raw)
	}

	aside := quarantinedFiles(t, dir)
	if len(aside) != 1 {
		t.Fatalf("the corrupt file was overwritten rather than quarantined; %d quarantined files: %v", len(aside), aside)
	}
	got, err := os.ReadFile(filepath.Join(dir, aside[0]))
	if err != nil {
		t.Fatalf("read quarantined file: %v", err)
	}
	if string(got) != corrupt {
		t.Errorf("quarantined bytes differ from the original:\n  got  %s\n  want %s", got, corrupt)
	}

	if n := s.quarantines.Load(); n != 1 {
		t.Errorf("tombstone_quarantines = %d, want 1; a corrupt file that moves no counter reads exactly like a clean run", n)
	}
}

// The fixture is a symlink to a directory: ReadFile fails with EISDIR even as root, and rename replaces the symlink,
// so only the code's choice stops the write (#724).
func TestTombstoneStore_TransientReadFailureWritesNothing(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	target := filepath.Join(dir, "unreadable")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, tombstoneFilePath()); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var s tombstoneStore
	err := s.add("net-new", "host-new", "02:42:ac:11:00:07", "192.0.2.7", "")
	if err == nil {
		t.Fatal("add rewrote the tombstone file after failing to read it; a transient read failure is not an empty list, and the contents it overwrote may have been perfectly good")
	}

	fi, statErr := os.Lstat(tombstoneFilePath())
	if statErr != nil {
		t.Fatalf("lstat: %v", statErr)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the unreadable path was replaced by a fresh file (mode %v); whatever it held is gone", fi.Mode())
	}
	if aside := quarantinedFiles(t, dir); len(aside) != 0 {
		t.Errorf("a transient read failure was quarantined as corruption: %v", aside)
	}
	if n := s.quarantines.Load(); n != 0 {
		t.Errorf("tombstone_quarantines = %d, want 0; a transient errno is not a quarantine and must not page anyone", n)
	}
}

func quarantinedFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tombstones.json.corrupt-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// The options file is fsynced, since it outlives plugin rm and upgrade (#440). tombstones.json is not: its 60 s TTL
// has expired by the time a host is back from a power loss. fsync is not observable from a test, so this reads the
// source (#724).
func TestStateWritesUseTheRightSyncPolicy(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "state.go", nil, 0)
	if err != nil {
		t.Fatalf("parse state.go: %v", err)
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
			funcs[fd.Name.Name] = fd
		}
	}

	writer, ok := funcs["writeStateFileAtomic"]
	if !ok {
		t.Fatal("state.go has no writeStateFileAtomic; if the durable write path was renamed, update this test to name it — do not delete the check")
	}

	syncPos, renamePos := token.NoPos, token.NoPos
	ast.Inspect(writer, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Sync":
			if syncPos == token.NoPos {
				syncPos = call.Pos()
			}
		case "Rename":
			if renamePos == token.NoPos {
				renamePos = call.Pos()
			}
		}
		return true
	})
	if syncPos == token.NoPos {
		t.Error("writeStateFileAtomic never syncs the temp file; the write is atomic against a crash but not durable against a power cut (#724)")
	}
	if renamePos == token.NoPos {
		t.Fatal("writeStateFileAtomic no longer renames; this test can no longer see what it is checking")
	}
	if syncPos != token.NoPos && syncPos > renamePos {
		t.Errorf("the file sync is after the rename (sync at %s, rename at %s); the rename is what publishes the file, so the bytes must be down first",
			fset.Position(syncPos), fset.Position(renamePos))
	}

	// The directory must be synced too, or the rename itself can be lost (#724).
	if !callsFunc(writer, "syncDir") {
		t.Error("writeStateFileAtomic never syncs the containing directory; the rename can be absent after a power cut even though the file's bytes are down (#724)")
	}

	wantPolicy := map[string]string{
		"saveNetwork":    "syncDurable",
		"saveTombstones": "syncEphemeral",
	}
	why := map[string]string{
		"saveNetwork":    "the options file is written from CreateNetwork, lives on a host bind mount, survives `docker plugin rm` and upgrade (#440), and is read after every daemon restart including the one following a power cut",
		"saveTombstones": "tombstoneTTL is 60 seconds. An fsync only changes what survives power loss or a panic, and no host boots, starts dockerd and reads this file within 60 seconds of losing power — every record in it prunes as stale first. So durability here protects data that is guaranteed worthless by the time anything reads it, and it charges for that on the endpoint path: `add` runs on every DeleteEndpoint and `consume` writes whenever a prune changed something. If you are here because you noticed a missing fsync: it is missing on purpose (#724)",
	}
	for name, want := range wantPolicy {
		fd, ok := funcs[name]
		if !ok {
			t.Errorf("state.go has no %s", name)
			continue
		}
		if !callsFunc(fd, "writeStateFileAtomic") {
			t.Errorf("%s does not write through writeStateFileAtomic, so its sync policy is no longer stated in one place (#724)", name)
			continue
		}
		if got := policyArg(fd); got != want {
			t.Errorf("%s writes with sync policy %q, want %q.\n  %s", name, got, want, why[name])
		}
		if callsSelector(fd, "Rename") {
			t.Errorf("%s open-codes its own rename; that is the second copy the shared writer exists to remove", name)
		}
	}

	if q, ok := funcs["quarantineTombstones"]; ok {
		if !callsFunc(q, "syncDir") {
			t.Error("quarantineTombstones does not sync the directory; the quarantine rename can be lost by the same power cut that corrupted the file")
		}
	} else {
		t.Error("state.go has no quarantineTombstones; a corrupt tombstone file is destroyed rather than preserved (#724)")
	}
}

func policyArg(n ast.Node) string {
	got := ""
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "writeStateFileAtomic" || len(call.Args) == 0 {
			return true
		}
		if arg, ok := call.Args[len(call.Args)-1].(*ast.Ident); ok {
			got = arg.Name
		}
		return true
	})
	return got
}

func callsFunc(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return true
	})
	return found
}

func callsSelector(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return true
	})
	return found
}
