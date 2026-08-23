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

// Tests for #724: STATE_DIR persistence is durable and versioned where
// that is worth paying for, deliberately neither where it is not, and
// never destroys a file it could not read.
//
// Each test below is red against the pre-fix tree. Rehearsed by
// extracting the pre-fix pkg/plugin with `git archive` and running this
// file against it, rather than against a hand-built fixture -- a
// fixture proves the test can fail, not that it fails on the code the
// issue was filed about.

// readRawTombstones returns the tombstone file exactly as it is on
// disk, decoded only as far as "a list of objects". Deliberately not
// decoded into []tombstone: the point of these tests is the BYTES the
// plugin writes, and decoding through the same struct that wrote them
// would agree with any mistake symmetrically.
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

// TestSaveOptions_StampsSchemaVersion pins the on-disk shape of the
// options file: the schema version is present, and it is a sibling of
// the option fields rather than a wrapper around them. The second half
// is the half that matters. A nested {"v":1,"options":{...}} would pass
// any test that only looked for the version, and would be unreadable to
// every build that predates it.
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
	if got := v.(float64); int(got) != stateSchemaVersion {
		t.Errorf(`"v" = %v, want %d`, got, stateSchemaVersion)
	}
	// The option fields stay at the top level. If this fails, the
	// version was added as an envelope and older builds can no longer
	// read the file.
	if _, ok := raw["Parent"]; !ok {
		t.Errorf("option fields are no longer at the top level, so an older build cannot read this file: %s", data)
	}
}

// TestLoadOptions_LegacyFileHasNoVersion is the compatibility half: a
// file written before the version field existed must still load. It is
// green before the fix and after it -- that is the point. It fails only
// if the version check is written as "reject anything that does not
// declare a version", which would make every upgrade lose its state.
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

// TestLoadOptions_RefusesFutureSchema covers the branch the version
// field exists to enable. A file this build does not understand is
// refused, so the caller falls back to the docker API -- which is
// authoritative for everything in this struct -- instead of attaching a
// network in whatever mode a v1 reading of a v2 file happens to yield.
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

// TestSaveTombstones_CarriesNoSchemaVersion pins the DECISION not to
// version this file, so that a later pass adding one for symmetry with
// the options file has to read why first.
//
// A 60-second cache has nothing to migrate: by the time any build other
// than the writer can read the file, every record in it has expired.
// And the shape a version would take here is actively harmful -- the
// file is a top-level array, so a versioned envelope makes every older
// build read it as corrupt, which since #724 quarantines it and loses
// the lot. Discard is the correct handling, and it needs no field.
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

// TestLoadTombstones_RecordsWithoutVersionLoad is the regression guard
// under that decision: nothing in the read path may start demanding a
// version field. If it ever does, every tombstone written by the build
// before it is dropped on upgrade.
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

// TestLoadTombstones_LegacyNullFile covers the other shape that has
// been written to this file: an empty list marshals to the JSON literal
// `null`, not to `[]`. It must read back as an empty list and must NOT
// be mistaken for corruption -- quarantining it would move a perfectly
// good file aside and page an operator over an empty cache.
//
// This was an unwritten assumption until now. The release it lands in
// is the one about durability, so every shape that has ever been
// written to the file gets a test proving the current reader takes it.
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
	// And it must not have been mistaken for corruption and quarantined.
	if aside := quarantinedFiles(t, dir); len(aside) != 0 {
		t.Errorf("a legacy null file was quarantined as corrupt: %v", aside)
	}
}

// TestLoadTombstones_RoundtripsAnEmptyList closes the loop on the shape
// question: whatever the writer produces for an empty list, the reader
// must take it back. Same reasoning as the null case, from the other
// direction.
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

// TestLoadTombstones_QuarantinesCorruptFile is the direct test of the
// third defect in #724: an unreadable file is moved aside with its
// bytes intact, not left in place to be overwritten.
func TestLoadTombstones_QuarantinesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	const corrupt = `[{"network_id":"net1","mac_address":"02:42:ac:11:00:02"` // truncated mid-object
	if err := os.WriteFile(tombstoneFilePath(), []byte(corrupt), stateFileMode); err != nil {
		t.Fatalf("write corrupt tombstones: %v", err)
	}

	_, err := loadTombstones()
	if err == nil {
		t.Fatal("loadTombstones accepted a truncated file")
	}
	// The error must be distinguishable, not merely present. This is the
	// signal `add` branches on to decide whether continuing is safe.
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

// TestTombstoneStore_CorruptFileSurvivesTheNextWrite is the defect as a
// user would meet it, and the reason the quarantine exists at all.
//
// Before the fix: loadTombstones returned an error, store.add logged a
// warning, set the list to nil, and saved ONE entry over the unreadable
// file -- destroying every other tombstone in it, silently, at exactly
// the moment an operator would want the bytes.
//
// The assertion is on the file system, not on a counter or a log line:
// the original bytes must still exist somewhere under stateDir.
func TestTombstoneStore_CorruptFileSurvivesTheNextWrite(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	const corrupt = `[{"network_id":"doomed","mac_address":"02:42:ac:11:00:99"` // truncated
	if err := os.WriteFile(tombstoneFilePath(), []byte(corrupt), stateFileMode); err != nil {
		t.Fatalf("write corrupt tombstones: %v", err)
	}

	var s tombstoneStore
	if err := s.add("net-new", "host-new", "02:42:ac:11:00:07", "192.0.2.7", ""); err != nil {
		t.Fatalf("add: %v", err)
	}

	// The new tombstone landed.
	raw := readRawTombstones(t)
	if len(raw) != 1 || raw[0]["network_id"] != "net-new" {
		t.Fatalf("the new tombstone was not written: %+v", raw)
	}

	// And the unreadable one was not destroyed to make room for it.
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

	// And it is counted, so the health surface says this happened. A
	// quarantine that is only logged is invisible to the one thing
	// operators alert on.
	if n := s.quarantines.Load(); n != 1 {
		t.Errorf("tombstone_quarantines = %d, want 1; a corrupt file that moves no counter reads exactly like a clean run", n)
	}
}

// TestTombstoneStore_TransientReadFailureWritesNothing is the other half
// of the same rule, and the one that is easy to get wrong while fixing
// the first.
//
// A refusal must not look like an absence. `loadTombstones` returns one
// error type for two very different situations: the contents were
// unparseable (nothing to save, quarantine and move on) and the file
// could not be READ at all (EIO, EMFILE, a read racing a writer). The
// second says nothing about the contents, which may be perfectly good —
// so treating it as "start fresh" destroys live data because a
// descriptor was briefly unavailable.
//
// THE FIXTURE IS A SYMLINK TO A DIRECTORY, and the shape is load-
// bearing. os.ReadFile follows it, opens a directory and returns EISDIR
// — a read error, not a parse error — and unlike a chmod it behaves the
// same when the suite runs as root, which it does on the integration
// runner. A plain directory at that path does NOT work: rename(2) then
// fails too, so the pre-fix code returns an error for the wrong reason
// and the test passes against the bug. rename does not follow a symlink
// in its final component, so it happily REPLACES this one — which means
// the only thing stopping the write is the code choosing not to do it.
// That is precisely what is under test.
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

	// Nothing was written over, nothing was moved aside, nobody paged.
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

// quarantinedFiles lists the tombstones.json.corrupt-* files in dir.
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

// TestStateWritesUseTheRightSyncPolicy reads state.go itself and pins
// the DECISION each writer made, not just the mechanism: the options
// file is durable, tombstones.json is deliberately not.
//
// # WHY EACH HALF EXISTS
//
// The options file is written from CreateNetwork, lives on a host bind
// mount, and survives `docker plugin rm` and upgrade by design (#440).
// It is read after every daemon restart, including the restart that
// follows a power cut. It needs the fsyncs.
//
// tombstones.json must NOT get them, and this is the half that will
// look like a bug to the next reader. tombstoneTTL is 60 seconds. The
// only crash an fsync survives is power loss or a panic -- a clean
// `systemctl restart docker` never loses the page cache -- and no host
// boots, starts dockerd and reaches this file within 60 seconds of
// losing power, so every record in it prunes as stale on the first read
// afterwards. An fsync there buys durability for data guaranteed
// worthless by the time anything reads it, and charges for it on the
// endpoint path: `add` runs on every DeleteEndpoint and `consume`
// writes whenever a prune changed something. #724 asked for fsync on
// both files, on the grounds that "both files exist specifically to
// survive restarts". That is true of one of them.
//
// IT IS A SOURCE-LEVEL CHECK ON PURPOSE. fsync is not observable from a
// Go test: the only difference it makes is what survives a power cut,
// and nothing short of a crashing block device or an instrumented
// filesystem can show that. The alternatives were worse -- a swappable
// `var syncFile = ...` seam would assert that our code calls our own
// hook, which is the plugin's opinion of itself rather than outside
// evidence, and would go green for any future writer that simply did
// not use the seam.
//
// So this asserts on the one artefact that is real: the source. It goes
// red if the syncs are removed, if a writer stops routing through
// writeStateFileAtomic, if the file sync drifts to after the rename
// where it guarantees nothing -- or if a well-meaning "you forgot an
// fsync" patch puts one back on the tombstone hot path.
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

	// The file sync must happen, and must happen before the rename.
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

	// The directory holding the renamed file must be synced too, or the
	// rename itself can be lost.
	if !callsFunc(writer, "syncDir") {
		t.Error("writeStateFileAtomic never syncs the containing directory; the rename can be absent after a power cut even though the file's bytes are down (#724)")
	}

	// Both persisted files go through it, and each states its policy. A
	// writer that open-codes its own rename is the two-copies problem
	// the helper exists to end.
	wantPolicy := map[string]string{
		"saveOptions":    "syncDurable",
		"saveTombstones": "syncEphemeral",
	}
	why := map[string]string{
		"saveOptions":    "the options file is written from CreateNetwork, lives on a host bind mount, survives `docker plugin rm` and upgrade (#440), and is read after every daemon restart including the one following a power cut",
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

	// The quarantine rename is durable for the same reason: an operator
	// reads that file after the crash that produced it.
	if q, ok := funcs["quarantineTombstones"]; ok {
		if !callsFunc(q, "syncDir") {
			t.Error("quarantineTombstones does not sync the directory; the quarantine rename can be lost by the same power cut that corrupted the file")
		}
	} else {
		t.Error("state.go has no quarantineTombstones; a corrupt tombstone file is destroyed rather than preserved (#724)")
	}
}

// policyArg returns the identifier passed as the last argument of the
// writeStateFileAtomic call inside n, or "" if there is none or it is
// not a plain identifier. A non-identifier is reported as a miss rather
// than accepted: a computed sync policy would put the decision
// somewhere this check cannot read it, which is the same as not having
// the check.
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

// callsFunc reports whether n contains a call to the plain function
// named name.
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

// callsSelector reports whether n contains a call to any x.name(...).
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
