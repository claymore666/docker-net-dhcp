// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// withSandboxNetnsDirs points the package's permitted sandbox-netns
// directories at a temporary one for the duration of a test.
//
// Swapping the package variable rather than only exercising the ...In
// seam is deliberate: openSandboxNetNS reads the variable, and a test
// that only ever calls the injectable form leaves the production route
// unexecuted -- which is the shape the counters in this file exist to
// stop being invisible.
func withSandboxNetnsDirs(t *testing.T, dirs []string) {
	t.Helper()
	prev := sandboxNetnsDirs
	sandboxNetnsDirs = dirs
	t.Cleanup(func() { sandboxNetnsDirs = prev })
}

// TestOpenSandboxNetNSByKey_RefusesAnythingOutsideThePermittedDirectories
// pins the refusal, not the acceptance. The key arrives from the daemon
// and is a path this plugin then opens with CAP_SYS_ADMIN behind it, so
// "anything the daemon says" is not an acceptable reading of it: an
// unrecognised shape is refused rather than guessed at.
func TestOpenSandboxNetNSByKey_RefusesAnythingOutsideThePermittedDirectories(t *testing.T) {
	dirs := []string{"/var/run/docker/netns"}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"another directory entirely", "/tmp/evil"},
		{"the directory itself", "/var/run/docker/netns"},
		{"a subdirectory of the permitted one", "/var/run/docker/netns/sub/key"},
		{"a traversal that lands elsewhere", "/var/run/docker/netns/../../../etc/passwd"},
		{"a bare name with no directory", "abcdef"},
		{"a relative path", "netns/abcdef"},
		{"the root", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, err := openSandboxNetNSByKeyIn(dirs, tc.key)
			if err == nil {
				closeNsHandle(ns)
				t.Fatalf("accepted %q as a sandbox key", tc.key)
			}
			if !errors.Is(err, errNoSandboxKey) {
				t.Fatalf("refused %q with %v, want errNoSandboxKey: only that sentinel stops the "+
					"await from spending the whole attach budget polling a permanent refusal", tc.key, err)
			}
		})
	}
}

// The acceptance control. Without it every case above is satisfied by
// an opener that refuses everything, and the refusal table would be
// measuring nothing.
//
// The entry is an ordinary file rather than a bind-mounted namespace:
// what is under test here is which path the openat resolves to, which
// needs no root. Whether the file is a usable netns is the integration
// suite's question and is answered there, per cell.
func TestOpenSandboxNetNSByKey_OpensTheEntryOfAPermittedDirectory(t *testing.T) {
	dir := t.TempDir()
	const name = "8fc1a2b3c4d5"
	want := []byte("this is the entry the key names")
	if err := os.WriteFile(filepath.Join(dir, name), want, 0o600); err != nil {
		t.Fatalf("write fixture entry: %v", err)
	}

	ns, err := openSandboxNetNSByKeyIn([]string{dir}, filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("refused a key naming an entry of a permitted directory: %v", err)
	}
	defer closeNsHandle(ns)

	got := make([]byte, len(want))
	if _, err := unix.Pread(int(ns), got, 0); err != nil {
		t.Fatalf("read back through the returned descriptor: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the descriptor reads %q, want %q — the openat resolved to a different entry", got, want)
	}
}

// TestAwaitSandboxNetNSByKey_DoesNotPollAPermanentRefusal is the #401
// lesson applied to this route. A key that names an unaccepted
// directory will name one at every retry, so polling it can only end in
// the deadline — and the budget it spends is the budget the PID
// fallback then does not have.
func TestAwaitSandboxNetNSByKey_DoesNotPollAPermanentRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{"/var/run/docker/netns"}, "/tmp/evil", 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("accepted a key outside the permitted directories")
	}
	if !errors.Is(err, errNoSandboxKey) {
		t.Fatalf("err = %v, want errNoSandboxKey", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("took %s to refuse a structurally impossible key: it was retried, and every retry "+
			"is attach budget the PID fallback will not have", elapsed)
	}
}

// The other direction: an entry that is simply not there YET is the
// ordinary case at Join, and must be waited for rather than refused.
// Without this the fast-refusal above would be satisfied by an await
// that never polls at all.
func TestAwaitSandboxNetNSByKey_WaitsForAnEntryThatArrivesLate(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "aa11bb22")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = os.WriteFile(key, []byte("x"), 0o600)
	}()

	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("gave up on an entry that arrived late: %v", err)
	}
	closeNsHandle(ns)
}

// TestAwaitSandboxNetNSByKey_DeadlineNamesTheLastAttempt pins the third
// outcome: the key was ACCEPTABLE and the entry never appeared.
//
// The deadline is the only thing separating "the sandbox is still being
// assembled" from "the container went away", so the error a caller sees
// is the whole of what it has to work with. Without the cause, both
// arrive as a bare context deadline and read identically — the same
// collapse #317 was diagnosed through, one layer down. The claim was
// written in the comment on awaitSandboxNetNSByKey and asserted
// nowhere: a mutant that dropped the wrapped cause survived the suite.
func TestAwaitSandboxNetNSByKey_DeadlineNamesTheLastAttempt(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "never-appears")

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, 10*time.Millisecond)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("succeeded on an entry that was never created")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, but it does not carry the last attempt's cause. A bare deadline cannot "+
			"be told apart from a sandbox that is merely slow to appear, and the two want opposite "+
			"responses from whoever reads the log", err)
	}
	if !strings.Contains(err.Error(), "never-appears") {
		t.Errorf("err = %v: it does not name the entry it waited for", err)
	}
}

// TestOpenSandboxNetNS_CountsTheKeyRoute drives the production opener —
// the one that reads the package variable — and asserts the three
// counters that carry the whole PR-1 measurement.
//
// The domain assertion is the one that matters. sandbox_pid_fallbacks
// is read as "the PID route was not needed", and on its own that
// reading is satisfied by a plugin that opened nothing at all; only
// sandbox_key_entries says the question had a subject.
func TestOpenSandboxNetNS_CountsTheKeyRoute(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "cc33dd44")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture entry: %v", err)
	}
	withSandboxNetnsDirs(t, []string{dir})

	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The PID is a real one that does NOT name the container: if the
	// key route were skipped, the fallback would refuse and the case
	// would fail loudly rather than pass by another route.
	ns, err := m.openSandboxNetNS(ctx, key, os.Getpid(), foreignCtrID, time.Millisecond)
	if err != nil {
		t.Fatalf("the key route refused a key naming a real entry: %v", err)
	}
	closeNsHandle(ns)

	if got := p.sandboxKeyEntries.Load(); got != 1 {
		t.Errorf("sandbox_key_entries = %d, want 1", got)
	}
	if got := p.sandboxPIDFallbacks.Load(); got != 0 {
		t.Errorf("sandbox_pid_fallbacks = %d, want 0", got)
	}
	if got := p.sandboxKeyEntryFailures.Load(); got != 0 {
		t.Errorf("sandbox_key_entry_failures = %d, want 0", got)
	}
	if got := p.netnsPIDMismatches.Load(); got != 0 {
		t.Errorf("netns_pid_mismatches = %d after a key-route open, want 0", got)
	}
}

// The fallback arm, driven for real: the key is refused and the PID
// route carries the open. This is the mutant-killer for "the fallback
// is dead code" and for "the failure counter is wired to the success
// arm".
func TestOpenSandboxNetNS_CountsTheFallbackWhenTheKeyIsRefused(t *testing.T) {
	withSandboxNetnsDirs(t, []string{"/var/run/docker/netns"})

	p := &Plugin{}
	m := &dhcpManager{plugin: p}
	pid := os.Getpid()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ns, err := m.openSandboxNetNS(ctx, "/tmp/not-a-sandbox-key", pid, selfCgroupLeaf(t, pid), time.Millisecond)
	if err != nil {
		t.Fatalf("both routes failed: %v", err)
	}
	closeNsHandle(ns)

	if got := p.sandboxKeyEntries.Load(); got != 0 {
		t.Errorf("sandbox_key_entries = %d after a refused key, want 0", got)
	}
	if got := p.sandboxKeyEntryFailures.Load(); got != 1 {
		t.Errorf("sandbox_key_entry_failures = %d, want 1", got)
	}
	if got := p.sandboxPIDFallbacks.Load(); got != 1 {
		t.Errorf("sandbox_pid_fallbacks = %d, want 1: an endpoint running on the PID route is the "+
			"only thing that keeps the host PID namespace and CAP_SYS_PTRACE load-bearing, and it "+
			"has to be countable before that can be reasoned about", got)
	}
}

// Both routes failing must report BOTH causes. Reporting only the
// second is how the reason the fallback was reached at all became
// invisible.
func TestOpenSandboxNetNS_BothRoutesFailingNamesTheKeyErrorToo(t *testing.T) {
	withSandboxNetnsDirs(t, []string{"/var/run/docker/netns"})

	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	ns, err := m.openSandboxNetNS(ctx, "/tmp/not-a-sandbox-key", os.Getpid(), foreignCtrID, time.Millisecond)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("both routes should have failed")
	}
	if !errors.Is(err, errNoSandboxKey) {
		t.Errorf("the combined error does not carry errNoSandboxKey: %v", err)
	}
	if !errors.Is(err, errPIDNotContainer) {
		t.Errorf("the combined error does not carry errPIDNotContainer: %v", err)
	}
}
