// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// openCgroupFDs counts the descriptors this process currently holds on
// a procfs "cgroup" file.
//
// Counting by target rather than by total fd count is deliberate: the
// test binary opens and closes unrelated files while it runs, and a
// total that drifts by one would either flake or have to be given a
// tolerance — and a tolerance is how a leak of one per call goes back to
// being invisible. Nothing else in a test process holds a file whose
// name is "cgroup".
func openCgroupFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	n := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			// The fd ReadDir itself used is gone by now; that is normal.
			continue
		}
		if filepath.Base(target) == "cgroup" {
			n++
		}
	}
	return n
}

// selfCgroupID returns an ID that names this process, so
// openContainerProc's identity check passes and the SUCCESS path can be
// exercised against a real /proc entry — no container, no root, no
// daemon.
//
// It used to return the whole /proc/self/cgroup FILE, on the reasoning
// recorded here verbatim: "The check is strings.Contains(cgroup,
// ctrID), so any substring of the live value serves."
//
// That was true and it was the defect. The file is a substring of
// itself, so the check reduced to Contains(x, x) and passed for every
// input — against a correct guard and a broken one alike. The success
// path this comment claims to exercise was never exercised at all. It
// is the reason a name-substring guard survived a suite that looks like
// it covers it, and it is why the guard now matches a path SEGMENT and
// this helper returns one. See selfCgroupLeaf.
func selfCgroupID(t *testing.T) string {
	t.Helper()
	return selfCgroupLeaf(t, os.Getpid())
}

// TestOpenContainerProc_DoesNotLeakCgroupFD is the regression test for
// #729.
//
// openContainerProc wrapped the cgroup descriptor in an *os.File and
// never closed it — not on success, not on read error, not on cgroup
// mismatch. The d.Close() calls in those arms close the DIRECTORY fd;
// the cgroup fd is a second, independent one.
//
// The caller list is what makes it matter: every bound and every renew
// event with propagate_dns=true, and every attach. So a host with many
// endpoints renewing regularly grows descriptors until the GC happens to
// run a finalizer, in a process that also holds netlink sockets, FIFOs
// and the plugin's listening sockets. Exhaustion surfaces as accept
// failures on the libnetwork socket — container starts failing for a
// reason that looks nothing like its cause.
//
// The loop runs without forcing a GC on purpose. os.NewFile attaches a
// finalizer, so a runtime.GC() here would close the leaked descriptors
// and hide exactly the defect under test.
func TestOpenContainerProc_DoesNotLeakCgroupFD(t *testing.T) {
	const iterations = 64

	t.Run("on the success path", func(t *testing.T) {
		id := selfCgroupID(t)
		before := openCgroupFDs(t)

		for i := 0; i < iterations; i++ {
			d, err := openContainerProc(os.Getpid(), id)
			if err != nil {
				t.Fatalf("openContainerProc on our own pid: %v", err)
			}
			d.Close()
		}

		if after := openCgroupFDs(t); after != before {
			t.Errorf("cgroup descriptors: %d before, %d after %d calls — openContainerProc leaks one per call", before, after, iterations)
		}
	})

	t.Run("on the cgroup-mismatch path", func(t *testing.T) {
		// The identity check is the whole point of this function, so the
		// path it refuses on is the one it runs most often in anger.
		const notOurContainer = "0000000000000000000000000000000000000000000000000000000000000000"
		before := openCgroupFDs(t)

		for i := 0; i < iterations; i++ {
			d, err := openContainerProc(os.Getpid(), notOurContainer)
			if err == nil {
				d.Close()
				t.Fatal("openContainerProc accepted a pid whose cgroup names a different container")
			}
		}

		if after := openCgroupFDs(t); after != before {
			t.Errorf("cgroup descriptors: %d before, %d after %d refused calls — the refusal arm leaks one per call", before, after, iterations)
		}
	})
}

// TestOpenContainerProc_ReturnedDirFdIsCloexec pins the flag half of #729
// as far as a test can reach it: the directory fd openContainerProc
// returns is the
// one descriptor from that function a caller can still inspect, and it
// is held across the unshare/dhcpcd spawns the manager makes.
//
// The two unix.Openat calls in this file are the ones that were missing
// the flag, and both close their descriptor before any caller sees it,
// so neither is observable from here. The gate that covers those is
// named in the commit; this test covers what it can rather than nothing.
//
// Hence the name. It passes against the pre-fix tree, correctly, and a
// name reading "OpensCloexec" would have claimed the two call sites it
// cannot see -- the same defect as a gate header broader than its own
// pattern (#758), one file over. A comment saying so is not enough: a
// name is what a reader skims.
func TestOpenContainerProc_ReturnedDirFdIsCloexec(t *testing.T) {
	d, err := openContainerProc(os.Getpid(), selfCgroupID(t))
	if err != nil {
		t.Fatalf("openContainerProc: %v", err)
	}
	defer d.Close()

	flags, err := unix.FcntlInt(d.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Error("the returned /proc directory fd is not close-on-exec; a spawned dhcpcd inherits a handle on the container's procfs entry")
	}
}
