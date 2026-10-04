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

// An empty regular file is "not yet"; every other non-namespace entry is final (#1185).
func TestOpenSandboxNetNSByKey_RefusesAnEntryThatIsNotANamespace(t *testing.T) {
	const name = "8fc1a2b3c4d5"

	for _, tc := range []struct {
		name        string
		make        func(t *testing.T, path string)
		placeholder bool
	}{
		{"an empty regular file is the placeholder, not yet", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
				t.Fatalf("write fixture entry: %v", err)
			}
		}, true},
		{"a non-empty regular file is final", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatalf("write fixture entry: %v", err)
			}
		}, false},
		{"a handle that is not a namespace is final", func(t *testing.T, path string) {
			if err := os.Symlink("/dev/null", path); err != nil {
				t.Fatalf("link fixture entry: %v", err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.make(t, filepath.Join(dir, name))

			ns, err := openSandboxNetNSByKeyIn([]string{dir}, filepath.Join(dir, name))
			if err == nil {
				closeNsHandle(ns)
				t.Fatal("accepted a file as a sandbox network namespace. That is not a hypothetical: " +
					"it is exactly what the plugin sees when the daemon's sandbox mounts are not propagated " +
					"into its mount namespace, and accepting it costs the endpoint its persistent client")
			}
			if !errors.Is(err, errNoSandboxKey) {
				t.Errorf("err = %v, want errNoSandboxKey so a direct caller still refuses it", err)
			}
			if !errors.Is(err, errSandboxKeyNotANamespace) {
				t.Errorf("err = %v, want errSandboxKeyNotANamespace so the arm counter still names it", err)
			}
			if got := errors.Is(err, errSandboxKeyPlaceholder); got != tc.placeholder {
				t.Errorf("errors.Is(err, errSandboxKeyPlaceholder) = %v, want %v: only an empty regular file "+
					"is \"not yet\"; anything else that is not a namespace never becomes one", got, tc.placeholder)
			}
		})
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

// makeThePlaceholderANamespace stands in for libnetwork's bind mount, by rename or by bind mount (#1185).
func makeThePlaceholderANamespace(t *testing.T, key string, bind bool) {
	t.Helper()
	if bind {
		if err := unix.Mount("/proc/self/ns/net", key, "", unix.MS_BIND, ""); err != nil {
			t.Errorf("bind-mount the namespace over the placeholder: %v", err)
			return
		}
		t.Cleanup(func() { _ = unix.Unmount(key, unix.MNT_DETACH) })
		return
	}
	tmp := key + ".swap"
	if err := linkANetnsEntry(tmp); err != nil {
		t.Errorf("link fixture entry: %v", err)
		return
	}
	if err := os.Rename(tmp, key); err != nil {
		t.Errorf("swap the placeholder for a namespace handle: %v", err)
	}
}

// canBindMountHere: whether a bind mount over a file works here (#1185).
func canBindMountHere(t *testing.T) bool {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if err := unix.Mount("/proc/self/ns/net", probe, "", unix.MS_BIND, ""); err != nil {
		return false
	}
	_ = unix.Unmount(probe, unix.MNT_DETACH)
	return true
}

func TestAwaitSandboxNetNSByKey_WaitsForAPlaceholderThatBecomesANamespace(t *testing.T) {
	variants := []bool{false}
	if canBindMountHere(t) {
		variants = append(variants, true)
	}
	for _, bind := range variants {
		name := "swapped for a handle"
		if bind {
			name = "bind-mounted over"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			key := filepath.Join(dir, "aa11bb22")
			if err := os.WriteFile(key, nil, 0o600); err != nil {
				t.Fatalf("write placeholder: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			swapped := make(chan struct{})
			defer func() { <-swapped }()
			go func() {
				defer close(swapped)
				time.Sleep(300 * time.Millisecond)
				makeThePlaceholderANamespace(t, key, bind)
			}()

			ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, 200*time.Millisecond)
			if err != nil {
				t.Fatalf("gave up on a placeholder that became a namespace: %v", err)
			}
			closeNsHandle(ns)
		})
	}
}

func TestOpenSandboxNetNSLazyPID_PlaceholderWindowMovesNoRefusalCounter(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "cc33dd44")
	if err := os.WriteFile(key, nil, 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}
	withSandboxNetnsDirs(t, []string{dir})

	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	swapped := make(chan struct{})
	defer func() { <-swapped }()
	go func() {
		defer close(swapped)
		time.Sleep(300 * time.Millisecond)
		makeThePlaceholderANamespace(t, key, false)
	}()

	noPID := func() (int, string, error) { return 0, "", errors.New("the PID route must not be needed") }
	ns, err := m.openSandboxNetNSLazyPID(ctx, key, 200*time.Millisecond, noPID)
	if err != nil {
		t.Fatalf("the key route gave up inside the placeholder window: %v", err)
	}
	closeNsHandle(ns)

	if got := p.sandboxKeyEntries.Load(); got != 1 {
		t.Errorf("sandbox_key_entries = %d, want 1", got)
	}
	for name, got := range map[string]int64{
		"sandbox_key_entry_failures":  int64(p.sandboxKeyEntryFailures.Load()),
		"sandbox_key_not_a_namespace": int64(p.sandboxKeyNotANamespace.Load()),
		"sandbox_key_unavailable":     int64(p.sandboxKeyUnavailable.Load()),
		"sandbox_pid_fallbacks":       int64(p.sandboxPIDFallbacks.Load()),
	} {
		if got != 0 {
			t.Errorf("%s = %d, want 0: a placeholder that became a namespace was counted as a refusal", name, got)
		}
	}
}

func TestAwaitSandboxNetNSByKey_SteadyPlaceholderFallsBackWithinTheBound(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "ee55ff66")
	if err := os.WriteFile(key, nil, 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const interval = 50 * time.Millisecond
	start := time.Now()
	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, interval)
	elapsed := time.Since(start)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("accepted an empty file as a namespace")
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("err = %v after %s: a steady placeholder was polled to the attach deadline", err, elapsed)
	}
	if !errors.Is(err, errNoSandboxKey) || !errors.Is(err, errSandboxKeyNotANamespace) {
		t.Errorf("err = %v, want errNoSandboxKey wrapping errSandboxKeyNotANamespace, so the caller falls back and "+
			"the same arm counts", err)
	}
	if elapsed < 2*interval {
		t.Errorf("returned after %s, before the %s a placeholder is given to turn into a namespace", elapsed, 2*interval)
	}
	if elapsed > 10*interval {
		t.Errorf("returned after %s, want within a few intervals of the bound %s", elapsed, 2*interval)
	}
}

func TestAwaitSandboxNetNSByKey_PlaceholderBoundRunsFromItsFirstSighting(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "0a1b2c3d")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const interval = 50 * time.Millisecond
	created := make(chan struct{})
	defer func() { <-created }()
	go func() {
		defer close(created)
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(key, nil, 0o600)
	}()

	start := time.Now()
	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, interval)
	elapsed := time.Since(start)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("accepted an empty file as a namespace")
	}
	if !errors.Is(err, errSandboxKeyNotANamespace) {
		t.Fatalf("err = %v after %s, want errSandboxKeyNotANamespace", err, elapsed)
	}
	if elapsed < 400*time.Millisecond+2*interval {
		t.Errorf("returned after %s: it did not wait the bound from the placeholder's first sighting at 400 ms", elapsed)
	}
}

func TestAwaitSandboxNetNSByKey_AnEntryThatIsNotAPlaceholderIsFinalAtOnce(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "99887766")
	if err := os.WriteFile(key, []byte("not empty"), 0o600); err != nil {
		t.Fatalf("write entry: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("accepted a non-empty file as a namespace")
	}
	if !errors.Is(err, errNoSandboxKey) || !errors.Is(err, errSandboxKeyNotANamespace) {
		t.Errorf("err = %v, want errNoSandboxKey wrapping errSandboxKeyNotANamespace", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("took %s: an entry that can never become a namespace was polled", elapsed)
	}
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

// An entry that stops being a placeholder (here: it vanishes) starts the bound over when a placeholder returns (#1185).
func TestAwaitSandboxNetNSByKey_PlaceholderBoundRestartsAfterTheEntryStopsBeingOne(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "5e6f7a8b")
	if err := os.WriteFile(key, nil, 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	const interval = 200 * time.Millisecond
	const recreatedAt = 900 * time.Millisecond
	flipped := make(chan struct{})
	defer func() { <-flipped }()
	go func() {
		defer close(flipped)
		time.Sleep(interval + interval/2)
		_ = os.Remove(key)
		time.Sleep(recreatedAt - interval - interval/2)
		_ = os.WriteFile(key, nil, 0o600)
	}()

	start := time.Now()
	ns, err := awaitSandboxNetNSByKeyIn(ctx, []string{dir}, key, interval)
	elapsed := time.Since(start)
	if err == nil {
		closeNsHandle(ns)
		t.Fatal("accepted an empty file as a namespace")
	}
	if !errors.Is(err, errSandboxKeyPlaceholder) {
		t.Fatalf("err = %v after %s, want errSandboxKeyPlaceholder", err, elapsed)
	}
	if elapsed < recreatedAt+2*interval {
		t.Errorf("returned after %s: the bound ran from the sighting before the entry vanished, not from the one at %s",
			elapsed, recreatedAt)
	}
}
