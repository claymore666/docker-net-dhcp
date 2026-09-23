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

func withSandboxNetnsDirs(t *testing.T, dirs []string) {
	t.Helper()
	prev := sandboxNetnsDirs
	sandboxNetnsDirs = dirs
	t.Cleanup(func() { sandboxNetnsDirs = prev })
}

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

// libnetwork bind-mounts each namespace over an empty file, so without propagation the plugin sees
// an ordinary file (measured 2026-09-04); a symlink to /proc/self/ns/net gives real nsfs (#725).
func linkANetnsEntry(path string) error {
	return os.Symlink("/proc/self/ns/net", path)
}

func TestOpenSandboxNetNSByKey_OpensTheEntryOfAPermittedDirectory(t *testing.T) {
	dir := t.TempDir()
	const name = "8fc1a2b3c4d5"
	if err := linkANetnsEntry(filepath.Join(dir, name)); err != nil {
		t.Fatalf("link fixture entry: %v", err)
	}

	ns, err := openSandboxNetNSByKeyIn([]string{dir}, filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("refused a key naming an entry of a permitted directory: %v", err)
	}
	defer closeNsHandle(ns)

	var got, want unix.Stat_t
	if err := unix.Fstat(int(ns), &got); err != nil {
		t.Fatalf("fstat the returned descriptor: %v", err)
	}
	if err := unix.Stat("/proc/self/ns/net", &want); err != nil {
		t.Fatalf("stat /proc/self/ns/net: %v", err)
	}
	if got.Ino != want.Ino || got.Dev != want.Dev {
		t.Errorf("the descriptor names namespace %d:%d, want %d:%d — the openat resolved elsewhere",
			got.Dev, got.Ino, want.Dev, want.Ino)
	}
}

func TestOpenSandboxNetNSByKey_RefusesAnEntryThatIsNotANamespace(t *testing.T) {
	dir := t.TempDir()
	const name = "8fc1a2b3c4d5"
	if err := os.WriteFile(filepath.Join(dir, name), []byte{}, 0o600); err != nil {
		t.Fatalf("write fixture entry: %v", err)
	}

	ns, err := openSandboxNetNSByKeyIn([]string{dir}, filepath.Join(dir, name))
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("accepted an ordinary file as a sandbox network namespace. That is not a hypothetical: " +
			"it is exactly what the plugin sees when the daemon's sandbox mounts are not propagated " +
			"into its mount namespace, and accepting it costs the endpoint its persistent client")
	}
	if !errors.Is(err, errNoSandboxKey) {
		t.Errorf("err = %v, want errNoSandboxKey so the await refuses it without spending the "+
			"attach budget and the caller falls back at once", err)
	}
}

// NS_GET_NSTYPE answers for any namespace, and setns(CLONE_NEWNET) on a UTS namespace fails with EINVAL (#725).
func TestOpenSandboxNetNSByKey_RefusesANamespaceOfTheWrongType(t *testing.T) {
	dir := t.TempDir()
	const name = "8fc1a2b3c4d5"
	if err := os.Symlink("/proc/self/ns/uts", filepath.Join(dir, name)); err != nil {
		t.Fatalf("link fixture entry: %v", err)
	}

	ns, err := openSandboxNetNSByKeyIn([]string{dir}, filepath.Join(dir, name))
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("accepted a UTS namespace as a container's network namespace")
	}
	if !errors.Is(err, errNoSandboxKey) {
		t.Errorf("err = %v, want errNoSandboxKey", err)
	}
	if !strings.Contains(err.Error(), "not a network namespace") {
		t.Errorf("err = %v: it does not say the TYPE was wrong, which is the only thing that "+
			"distinguishes this from an entry that could not be opened at all", err)
	}
}

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

func TestAwaitSandboxNetNSByKey_WaitsForAnEntryThatArrivesLate(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "aa11bb22")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = linkANetnsEntry(key)
	}()

	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("gave up on an entry that arrived late: %v", err)
	}
	closeNsHandle(ns)
}

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

func TestOpenSandboxNetNS_CountsTheKeyRoute(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "cc33dd44")
	if err := linkANetnsEntry(key); err != nil {
		t.Fatalf("link fixture entry: %v", err)
	}
	withSandboxNetnsDirs(t, []string{dir})

	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

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
