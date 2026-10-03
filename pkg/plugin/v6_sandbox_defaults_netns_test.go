// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/vishvananda/netns"
)

// readDefaultAcceptRA reads default/accept_ra as seen from ns, on a locked thread that returns to its own namespace.
func readDefaultAcceptRA(t *testing.T, ns netns.NsHandle) string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		orig, err := netns.Get()
		if err != nil {
			out <- "orig: " + err.Error()
			return
		}
		defer orig.Close()
		if err := netns.Set(ns); err != nil {
			out <- "set: " + err.Error()
			return
		}
		b, err := os.ReadFile("/proc/sys/net/ipv6/conf/default/accept_ra")
		if serr := netns.Set(orig); serr != nil {
			// The thread is in the wrong namespace: end the goroutine locked so Go discards the thread.
			out <- "restore: " + serr.Error()
			return
		}
		if err != nil {
			out <- "read: " + err.Error()
			return
		}
		out <- strings.TrimSpace(string(b))
	}()
	return <-out
}

func TestWriteV6SandboxDefaults_GuardsTheNamedSandboxAndNotTheCallersNamespace(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	// A sleeping child's namespace stands in for the sandbox, reached through a symlink key as the opener follows one (#1145).
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if err := child.Start(); err != nil {
		t.Fatalf("cannot create the sandbox namespace inside the namespaced run: %v", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })

	dir := t.TempDir()
	key := filepath.Join(dir, "sandbox")
	// proc-path-discipline: allow -- the key is a test fixture proving the opener follows a symlink into a namespace (#1145).
	if err := os.Symlink("/proc/"+strconv.Itoa(child.Process.Pid)+"/ns/net", key); err != nil {
		t.Fatal(err)
	}
	prev := sandboxNetnsDirs
	sandboxNetnsDirs = []string{dir}
	t.Cleanup(func() { sandboxNetnsDirs = prev })

	sandbox, err := netns.GetFromPid(child.Process.Pid)
	if err != nil {
		t.Fatalf("open the sandbox: %v", err)
	}
	defer sandbox.Close()
	self, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()

	if got := readDefaultAcceptRA(t, sandbox); got != "1" {
		t.Fatalf("a fresh namespace reads default/accept_ra %q, want 1: the case observes nothing", got)
	}
	res := writeV6SandboxDefaults(key)
	if res.Failures != 0 || res.Err != nil {
		t.Fatalf("writeV6SandboxDefaults: %d failure(s): %v", res.Failures, res.Err)
	}
	if res.PriorAcceptRA != "1" {
		t.Errorf("PriorAcceptRA = %q, want 1", res.PriorAcceptRA)
	}
	if got := readDefaultAcceptRA(t, sandbox); got != "0" {
		t.Errorf("the sandbox's default/accept_ra reads %q after the write, want 0", got)
	}
	if got := readDefaultAcceptRA(t, self); got != "1" {
		t.Errorf("the caller's own default/accept_ra reads %q: the write landed in the wrong namespace", got)
	}
}
