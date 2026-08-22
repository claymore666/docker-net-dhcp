// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// allRoutePaths is the real routing table, so these tests build the same
// capture allowlist production does rather than a hand-written stand-in
// that could drift from it.
func allRoutePaths() []string { return capturablePaths((&Plugin{}).routes()) }

// bodyEcho is the downstream handler under every test here. It records
// what the handler ACTUALLY received, which is the property that
// matters: capture reads the body, so a bug in restoring it would make
// this middleware a fault injector on every RPC the daemon makes.
func bodyEcho(t *testing.T, got *[]string) http.Handler {
	t.Helper()
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("downstream handler could not read body: %v", err)
			return
		}
		mu.Lock()
		*got = append(*got, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// An empty directory is the shipped configuration. The handler must be
// returned untouched — no directory created, nothing written, and the
// body delivered unchanged.
func TestCaptureHandler_DisabledIsPassthrough(t *testing.T) {
	var got []string
	inner := bodyEcho(t, &got)
	h := captureHandler(inner, "", allRoutePaths())

	post(h, "/NetworkDriver.CreateEndpoint", `{"EndpointID":"abc"}`)

	if len(got) != 1 || got[0] != `{"EndpointID":"abc"}` {
		t.Fatalf("downstream body = %q, want the request body unchanged", got)
	}
}

func TestCaptureHandler_WritesBodyAndPreservesIt(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	const body = `{"NetworkID":"net1","EndpointID":"ep1"}`
	rec := post(h, "/NetworkDriver.CreateEndpoint", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — capture must not change the response", rec.Code)
	}
	if len(got) != 1 || got[0] != body {
		t.Fatalf("downstream body = %q, want %q", got, body)
	}

	want := filepath.Join(dir, "0001-NetworkDriver.CreateEndpoint.json")
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("reading captured file: %v", err)
	}
	if string(b) != body {
		t.Fatalf("captured %q, want %q", b, body)
	}
}

// Order is part of the fixture: CreateEndpoint before Join before Leave
// is the shape a replay has to preserve, and the sequence prefix is the
// only record of it.
func TestCaptureHandler_SequenceRecordsOrder(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	post(h, "/NetworkDriver.CreateEndpoint", `{"n":1}`)
	post(h, "/NetworkDriver.Join", `{"n":2}`)
	post(h, "/NetworkDriver.Leave", `{"n":3}`)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{
		"0001-NetworkDriver.CreateEndpoint.json",
		"0002-NetworkDriver.Join.json",
		"0003-NetworkDriver.Leave.json",
	}
	if len(names) != len(want) {
		t.Fatalf("captured %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("captured[%d] = %q, want %q (ReadDir is sorted, so this is the recorded order)", i, names[i], want[i])
		}
	}
}

// GetCapabilities and Plugin.Health carry no body. There is no request
// shape to record, and a directory full of empty files would make the
// fixture set harder to read for no gain.
func TestCaptureHandler_SkipsEmptyBodies(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	post(h, "/NetworkDriver.GetCapabilities", "")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("captured %d file(s) for an empty body, want 0", len(entries))
	}
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("downstream body = %q, want the empty body delivered", got)
	}
}

// An oversized body is not a request shape worth recording, but the
// request itself must still go through untouched.
func TestCaptureHandler_OversizedBodyIsNotWrittenButIsDelivered(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	big := strings.Repeat("x", captureMaxBodyBytes+1)
	post(h, "/NetworkDriver.CreateEndpoint", big)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("captured %d file(s) for an oversized body, want 0", len(entries))
	}
	if len(got) != 1 || got[0] != big {
		t.Fatalf("downstream received %d bytes, want the full %d — capture must not truncate the request",
			len(got[0]), len(big))
	}
}

func TestCaptureHandler_StopsAtFileCap(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	for i := 0; i < captureMaxFiles+5; i++ {
		post(h, "/NetworkDriver.Join", fmt.Sprintf(`{"n":%d}`, i))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	if len(entries) != captureMaxFiles {
		t.Fatalf("captured %d file(s), want the cap of %d", len(entries), captureMaxFiles)
	}
	// Every request still reached the handler — the cap bounds the
	// disk, not the plugin.
	if len(got) != captureMaxFiles+5 {
		t.Fatalf("downstream saw %d request(s), want %d", len(got), captureMaxFiles+5)
	}
}

// A capture directory that cannot be created degrades to "no
// fixtures", never to a failed request. A full disk on the test box
// must not read as a plugin bug.
func TestCaptureHandler_UnusableDirIsPassthrough(t *testing.T) {
	// A path under a regular file cannot be created as a directory.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var got []string
	h := captureHandler(bodyEcho(t, &got), filepath.Join(f, "capture"), allRoutePaths())

	const body = `{"EndpointID":"ep1"}`
	rec := post(h, "/NetworkDriver.Join", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unusable capture dir must not fail the request", rec.Code)
	}
	if len(got) != 1 || got[0] != body {
		t.Fatalf("downstream body = %q, want %q", got, body)
	}
}

// The URL path reaches a filename. Whoever can drive these RPCs already
// owns the plugin socket, but a debug feature that can write outside
// its own directory is not a trade worth making.
// The filename fragment must come from a CLOSED set of constants, not
// from the request. The old version of this test asserted the weaker
// property that separators were stripped, which required trusting a
// character-level sanitiser; this asserts that a hostile path produces a
// name that was never derived from it at all.
func TestMethodName_IsAClosedSetFromTheRoutingTable(t *testing.T) {
	// Built the way production builds it, so this cannot pass against a
	// stand-in allowlist that the real one has diverged from.
	allowed := map[string]string{}
	for _, p := range capturablePaths((&Plugin{}).routes()) {
		allowed[p] = strings.TrimPrefix(p, "/")
	}
	st := &captureState{allowed: allowed}

	for _, tc := range []struct {
		in, want string
	}{
		{"/NetworkDriver.CreateEndpoint", "NetworkDriver.CreateEndpoint"},
		{"/Plugin.Health", "Plugin.Health"},

		// Everything below is unrouted, and every one of them must land
		// on the same constant rather than on anything shaped like the
		// input.
		{"/", "unknown"},
		{"", "unknown"},
		{"/../../etc/passwd", "unknown"},
		{"/a/b", "unknown"},
		{"/weird name\x00", "unknown"},
		{"/NetworkDriver.CreateEndpoint/../../x", "unknown"},
		// NOT "unknown": unrouted, but knowingly so, and the fixture
		// set's whole value here is knowing WHICH RPC the daemon sent
		// (#646). Erasing the name would erase the evidence.
		{"/NetworkDriver.ProgramExternalConnectivity", "NetworkDriver.ProgramExternalConnectivity"},
	} {
		got := st.methodName(tc.in)
		if got != tc.want {
			t.Errorf("methodName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("methodName(%q) = %q, which contains a path separator", tc.in, got)
		}
	}
}

// The allowlist and the mux must be built from the same table, or a new
// RPC would be served and silently never captured — a fixture set that
// looks complete while missing the request someone added last week.
func TestCapture_AllowlistCoversEveryServedRoute(t *testing.T) {
	p := &Plugin{}
	paths := capturablePaths(p.routes())
	if len(paths) == 0 {
		t.Fatal("routes() returned nothing; this test would pass having compared nothing")
	}

	dir := t.TempDir()
	h := captureHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), dir, paths)

	for _, path := range paths {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"x":1}`))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(paths) {
		t.Fatalf("captured %d files for %d routes", len(entries), len(paths))
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "unknown") {
			t.Errorf("a served route was captured as %q — the allowlist and the mux have drifted", e.Name())
		}
	}

	// The RPCs we knowingly do not serve must still be captured under
	// their own names; they are the evidence behind the 404 contract
	// (#646), and "unknown" would throw that away.
	for _, path := range unroutedRPCs() {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"x":1}`))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range unroutedRPCs() {
		name := strings.TrimPrefix(want, "/")
		var found bool
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), name+".json") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not captured under its own name", want)
		}
	}
}

// A body that fails mid-read must reach the handler as the SAME
// failure. Restoring only the bytes read so far would turn a transport
// error into a JSON decode error — a different error on a different
// code path, which is precisely the kind of substitution that makes a
// production incident unreadable.
func TestCaptureHandler_ReadErrorIsReplayedNotSwallowed(t *testing.T) {
	dir := t.TempDir()

	wantErr := fmt.Errorf("connection reset by peer")
	var gotErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	h := captureHandler(inner, dir, allRoutePaths())

	req := httptest.NewRequest(http.MethodPost, "/NetworkDriver.Join",
		io.MultiReader(strings.NewReader(`{"partial":`), errReader{wantErr}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotErr == nil {
		t.Fatal("downstream handler read the body without error; the read failure was swallowed")
	}
	if !strings.Contains(gotErr.Error(), wantErr.Error()) {
		t.Fatalf("downstream error = %v, want it to carry %v", gotErr, wantErr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("captured %d file(s) from an unreadable body, want 0 — a partial body is not a fixture", len(entries))
	}
}

// The plugin serves RPCs concurrently — Join for one container overlaps
// CreateEndpoint for another. Every request must be recorded exactly
// once, under its own name.
func TestCaptureHandler_ConcurrentRequestsDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			post(h, "/NetworkDriver.Join", fmt.Sprintf(`{"n":%d}`, i))
		}(i)
	}
	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading capture dir: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("captured %d file(s) from %d concurrent requests, want %d", len(entries), n, n)
	}

	seen := make(map[string]bool, n)
	for _, e := range entries {
		if seen[e.Name()] {
			t.Fatalf("duplicate capture filename %q", e.Name())
		}
		seen[e.Name()] = true
	}
}

// #785. What lands in the capture directory is the raw libnetwork
// request -- container IDs, endpoint IDs, the sandbox key, MACs and
// addresses -- and the directory is a HOST bind mount, so 0755/0644 put
// all of it in reach of any user on the host.
//
// These assert a PROPERTY of the artifacts on disk -- no group or other
// bits -- rather than equality with captureDirMode / captureFileMode.
// Comparing against the constants would be a mirror: widen a constant to
// 0755 and the assertion widens with it and still passes. The property
// cannot be satisfied by editing the source it is checking, and it is
// the thing that was actually wrong, so it also survives a future
// deliberate 0640 without needing a third copy of the number kept in
// step.
//
// The two "already exists" cases are the ones that matter, and both are
// the NORMAL flow rather than an edge:
//
//   - `make capture-fixtures` mkdirs CAPTURE_HOST_DIR before enabling
//     the plugin, because a bind source that does not exist fails
//     `docker plugin enable` (#588). The plugin never creates this
//     directory in the flow it ships for.
//   - nextName's sequence restarts at 0001 in every plugin process, so
//     a second capture into the same directory rewrites the first
//     capture's filenames.
//
// A change of the two constants alone leaves both untouched, which is
// why these two tests exist beside the fresh-artifact ones.

func TestCaptureHandler_TightensAnExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// Explicit, because the MkdirAll above is subject to the test
	// process's umask and the premise is that it starts loose.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var got []string
	captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("capture dir mode = %04o, want no group or other bits (%04o) — an existing "+
			"directory was left as the operator's umask made it, and MkdirAll does not "+
			"tighten one", perm, captureDirMode)
	}
}

func TestCaptureHandler_FreshDirectoryIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capture")

	var got []string
	captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("capture dir mode = %04o, want no group or other bits (%04o)", perm, captureDirMode)
	}
}

func TestCaptureHandler_TightensAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "0001-NetworkDriver.CreateEndpoint.json")
	if err := os.WriteFile(name, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	const body = `{"NetworkID":"net1","EndpointID":"ep1"}`
	post(h, "/NetworkDriver.CreateEndpoint", body)

	fi, err := os.Stat(name)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("captured file mode = %04o, want no group or other bits (%04o) — O_CREATE's "+
			"mode applies only to a file that did not exist, and these names recur across "+
			"plugin processes", perm, captureFileMode)
	}

	// The premise of the case: it really did rewrite the file, so the
	// mode above is the mode of a file holding a fresh request body and
	// not of one the handler declined to touch.
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading captured file: %v", err)
	}
	if string(b) != body {
		t.Fatalf("captured %q, want %q — the case did not exercise a rewrite", b, body)
	}
}

func TestCaptureHandler_FreshFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	post(h, "/NetworkDriver.Join", `{"EndpointID":"ep1"}`)

	name := filepath.Join(dir, "0001-NetworkDriver.Join.json")
	fi, err := os.Stat(name)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("captured file mode = %04o, want no group or other bits (%04o)", perm, captureFileMode)
	}
}

// A capture failure is never a request failure — the doctrine at the top
// of capture.go — and the unlink-then-create path has to keep it. A
// directory sitting at the name defeats both halves: Remove fails
// because it is not empty, and the create fails because it is not a
// file.
func TestCaptureHandler_UnwritableNameIsNotARequestFailure(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "0001-NetworkDriver.Join.json")
	if err := os.MkdirAll(filepath.Join(blocked, "occupied"), 0o700); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	const body = `{"EndpointID":"ep1"}`
	rec := post(h, "/NetworkDriver.Join", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a capture that cannot be written must not fail the request", rec.Code)
	}
	if len(got) != 1 || got[0] != body {
		t.Fatalf("downstream body = %q, want %q", got, body)
	}
	// The premise: the name really was unwritable, so the case exercised
	// the failure rather than quietly succeeding somewhere else.
	if fi, err := os.Stat(blocked); err != nil || !fi.IsDir() {
		t.Fatalf("stat %s = (%v, %v), want it still a directory — the case did not exercise a write failure", blocked, fi, err)
	}
}

// The comment on createCaptureFile claims a symlink at one of these
// names is unlinked rather than written through. Nothing observed that
// claim until this existed, which is the same shape as the modes
// themselves: a property stated in prose and checked by nobody.
//
// Not a live vulnerability, and it is not dressed as one. After
// ensureCaptureDir the only writers in that directory are root and its
// owner, and the owner is the operator who ran `make capture-fixtures`.
// This is defence in depth, and the test is here because the sentence
// is here.
func TestCaptureHandler_DoesNotWriteThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	const original = "do not overwrite me"
	if err := os.WriteFile(victim, []byte(original), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	name := filepath.Join(dir, "0001-NetworkDriver.Join.json")
	if err := os.Symlink(victim, name); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())

	const body = `{"EndpointID":"ep1"}`
	post(h, "/NetworkDriver.Join", body)

	if b, err := os.ReadFile(victim); err != nil || string(b) != original {
		t.Errorf("victim = (%q, %v), want %q unchanged — the capture was written through the symlink",
			b, err, original)
	}

	// And the capture still happened, into a real file at the restricted
	// mode. A version that merely refused to follow the link would pass
	// the assertion above while silently recording nothing.
	fi, err := os.Lstat(name)
	if err != nil {
		t.Fatalf("lstat %s: %v", name, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is still a symlink — nothing was created in its place", name)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file (%v)", name, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("captured file mode = %04o, want no group or other bits (%04o)", perm, captureFileMode)
	}
	if b, err := os.ReadFile(name); err != nil || string(b) != body {
		t.Errorf("captured %q (%v), want %q", b, err, body)
	}
}
