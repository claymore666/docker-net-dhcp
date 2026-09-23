// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func allRoutePaths() []string { return capturablePaths((&Plugin{}).routes()) }

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
	if len(got) != captureMaxFiles+5 {
		t.Fatalf("downstream saw %d request(s), want %d", len(got), captureMaxFiles+5)
	}
}

func TestCaptureHandler_UnusableDirIsPassthrough(t *testing.T) {
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

func TestMethodName_IsAClosedSetFromTheRoutingTable(t *testing.T) {
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

		{"/", "unknown"},
		{"", "unknown"},
		{"/../../etc/passwd", "unknown"},
		{"/a/b", "unknown"},
		{"/weird name\x00", "unknown"},
		{"/NetworkDriver.CreateEndpoint/../../x", "unknown"},
		// Unrouted RPCs keep their names: which RPC the daemon sent is the evidence (#646).
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

	// Unserved RPCs are captured under their own names, the evidence behind the 404 contract (#646).
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

// The capture directory is a host bind mount holding raw libnetwork requests, so no artifact may carry group or
// other bits (#785). Both pre-existing cases are the normal flow: `make capture-fixtures` creates the directory
// first (#588), and the sequence restarts at 0001 in every process.

func TestCaptureHandler_TightensAnExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seeding: %v", err)
	}
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
	if fi, err := os.Stat(blocked); err != nil || !fi.IsDir() {
		t.Fatalf("stat %s = (%v, %v), want it still a directory — the case did not exercise a write failure", blocked, fi, err)
	}
}

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

// A non-empty directory makes Remove fail with ENOTEMPTY without privilege, so this asserts which operation
// reported the failure (#786).
func TestCreateCaptureFile_ReportsAFailedUnlinkRatherThanFallingThrough(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "0001-NetworkDriver.Join.json")

	if err := os.MkdirAll(filepath.Join(name, "occupied"), 0o700); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	f, err := createCaptureFile(name)
	if err == nil {
		f.Close()
		t.Fatalf("createCaptureFile succeeded over a name it could not unlink")
	}

	if rmErr := os.Remove(name); rmErr == nil {
		t.Fatalf("the seeded name was removable after all — this case did not " +
			"exercise a failed unlink and its verdict means nothing")
	}

	var pe *os.PathError
	if !errors.As(err, &pe) || pe.Op != "remove" {
		t.Errorf("createCaptureFile reported %q, want the failed REMOVE.\n"+
			"An error from the open means the unlink failure was swallowed and the "+
			"open proceeded over whatever was already at the name — the silent hole "+
			"#786 exists to close.", err)
	}
}

// createCaptureFile's own test cannot see writeBody stop using it, so this matches os.PathError.Op from the log
// entry (#786).
func TestCaptureHandler_AFailedUnlinkIsReportedThroughTheHandler(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "0001-NetworkDriver.Join.json")
	if err := os.MkdirAll(filepath.Join(name, "occupied"), 0o700); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	var got []string
	h := captureHandler(bodyEcho(t, &got), dir, allRoutePaths())
	post(h, "/NetworkDriver.Join", `{"EndpointID":"ep1"}`)

	if err := os.Remove(name); err == nil {
		t.Fatalf("the seeded name was removable after all — this case did not exercise " +
			"a failed unlink and its verdict means nothing")
	}

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatalf("the capture failed and NOTHING was logged; a silent failure is the " +
			"condition this whole change is about")
	}
	raw, ok := entry.Data[log.ErrorKey]
	if !ok {
		t.Fatalf("the warning carried no error value: %+v", entry.Data)
	}
	err, _ := raw.(error)
	var pe *os.PathError
	if !errors.As(err, &pe) || pe.Op != "remove" {
		t.Errorf("the reported failure was %v, want the failed REMOVE.\n"+
			"An error from the OPEN means the unlink failure was swallowed and the "+
			"write proceeded over whatever was already at the name — the silent hole "+
			"this change exists to close.", err)
	}
}
