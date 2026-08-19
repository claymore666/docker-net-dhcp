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
	h := captureHandler(inner, "")

	post(h, "/NetworkDriver.CreateEndpoint", `{"EndpointID":"abc"}`)

	if len(got) != 1 || got[0] != `{"EndpointID":"abc"}` {
		t.Fatalf("downstream body = %q, want the request body unchanged", got)
	}
}

func TestCaptureHandler_WritesBodyAndPreservesIt(t *testing.T) {
	dir := t.TempDir()
	var got []string
	h := captureHandler(bodyEcho(t, &got), dir)

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
	h := captureHandler(bodyEcho(t, &got), dir)

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
	h := captureHandler(bodyEcho(t, &got), dir)

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
	h := captureHandler(bodyEcho(t, &got), dir)

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
	h := captureHandler(bodyEcho(t, &got), dir)

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
	h := captureHandler(bodyEcho(t, &got), filepath.Join(f, "capture"))

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
func TestMethodName_NeutralisesPathSeparators(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"/NetworkDriver.CreateEndpoint", "NetworkDriver.CreateEndpoint"},
		{"/Plugin.Health", "Plugin.Health"},
		{"/", "unknown"},
		{"", "unknown"},
		{"/../../etc/passwd", ".._.._etc_passwd"},
		{"/a/b", "a_b"},
		{"/weird name\x00", "weird_name_"},
	} {
		got := methodName(tc.in)
		if got != tc.want {
			t.Errorf("methodName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("methodName(%q) = %q, which still contains a path separator", tc.in, got)
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
	h := captureHandler(inner, dir)

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
	h := captureHandler(bodyEcho(t, &got), dir)

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
