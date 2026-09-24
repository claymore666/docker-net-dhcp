// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

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
			continue
		}
		if filepath.Base(target) == "cgroup" {
			n++
		}
	}
	return n
}

func selfCgroupID(t *testing.T) string {
	t.Helper()
	return selfCgroupLeaf(t, os.Getpid())
}

// No runtime.GC: os.NewFile's finalizer would close a leaked descriptor and hide the defect (#729).
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
