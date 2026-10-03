// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestLoadIIDSecret_CreatesOneOwnerOnlyFileAndReadsItBack(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	// The mode must come from the code, not the umask the test runs under (#1032).
	prev := syscall.Umask(0)
	defer syscall.Umask(prev)

	first, err := loadIIDSecret()
	if err != nil {
		t.Fatalf("loadIIDSecret: %v", err)
	}
	if len(first) != iidSecretLen {
		t.Errorf("the secret holds %d octets, want %d", len(first), iidSecretLen)
	}
	if bytes.Equal(first, make([]byte, len(first))) {
		t.Error("the secret is all zero: it was not drawn from crypto/rand")
	}
	info, err := os.Stat(filepath.Join(dir, "ipv6-iid-secret"))
	if err != nil {
		t.Fatalf("the secret file is not at STATE_DIR/ipv6-iid-secret: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the secret file has mode %#o, want 0600: STATE_DIR is a host bind mount (#708, #1032)", info.Mode().Perm())
	}
	second, err := loadIIDSecret()
	if err != nil || !bytes.Equal(first, second) {
		t.Errorf("a second reader got %x, %v, want the same %x: a new secret moves every address (#1032)", second, err, first)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("STATE_DIR holds %d entries after two reads, want only the secret: the temporary file leaked", len(entries))
	}
}

func TestLoadIIDSecret_NeverRewritesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	want := bytes.Repeat([]byte{0xa5}, 20)
	path := filepath.Join(dir, "ipv6-iid-secret")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	for range 3 {
		got, err := loadIIDSecret()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("loadIIDSecret = %x, %v, want the operator's file %x", got, err, want)
		}
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Error("the secret file was replaced or rewritten by a reader (#1032)")
	}
}

func TestLoadIIDSecret_RefusesAShortFileAndLeavesItAlone(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":     {},
		"15 octets": bytes.Repeat([]byte{1}, 15),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			withStateDir(t, dir)
			path := filepath.Join(dir, "ipv6-iid-secret")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := loadIIDSecret()
			if err == nil {
				t.Fatalf("loadIIDSecret accepted a %d-octet secret and returned %x (RFC 7217 section 5)", len(content), got)
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "16") {
				t.Errorf("the refusal %q names neither the file nor the 16-octet floor", err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, content) {
				t.Errorf("the short file was changed to %x: regenerating it would move every address (#1032)", after)
			}
		})
	}
}

func TestLoadIIDSecret_ConcurrentFirstReadersAgree(t *testing.T) {
	withStateDir(t, t.TempDir())
	const readers = 16
	got := make([][]byte, readers)
	errs := make([]error, readers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i], errs[i] = loadIIDSecret()
		}()
	}
	close(start)
	wg.Wait()
	for i := range readers {
		if errs[i] != nil {
			t.Fatalf("reader %d: %v", i, errs[i])
		}
		if !bytes.Equal(got[i], got[0]) {
			t.Fatalf("reader %d got %x, reader 0 got %x: a lost create race must re-read the winner's file (#1032)", i, got[i], got[0])
		}
	}
}

func TestLoadIIDSecret_AMissingStateDirIsAnErrorNotAPanic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	withStateDir(t, dir)
	got, err := loadIIDSecret()
	if err == nil {
		t.Fatalf("loadIIDSecret = %x with no STATE_DIR, want an error", got)
	}
	if len(got) != 0 {
		t.Errorf("an error came with a secret of %d octets", len(got))
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error %q does not name the missing directory", err)
	}
}

func TestLoadIIDSecret_LogsTheCreationOnceAtInfo(t *testing.T) {
	withStateDir(t, t.TempDir())
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()
	for range 3 {
		if _, err := loadIIDSecret(); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "stable-privacy") {
			n++
			if e.Level != log.InfoLevel {
				t.Errorf("the creation was logged at %v, want Info", e.Level)
			}
		}
	}
	if n != 1 {
		t.Errorf("the creation was logged %d times across three reads, want once (#1032)", n)
	}
}

func TestStateDirSweep_LeavesTheSecretAt0600AndTheNetworkListIgnoresIt(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	if _, err := loadIIDSecret(); err != nil {
		t.Fatal(err)
	}
	var failures atomic.Int32
	sweepStateDirModes(dir, &failures)
	info, err := os.Stat(filepath.Join(dir, "ipv6-iid-secret"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("after the startup sweep the secret is %v, %v, want a file at 0600", info, err)
	}
	ids, err := listStateNetworks()
	if err != nil || len(ids) != 0 {
		t.Errorf("listStateNetworks = %v, %v: the secret file was read as a network's options", ids, err)
	}
}
