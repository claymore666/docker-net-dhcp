// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A container ID that is emphatically not ours: 64 hex characters, the
// shape Docker hands out, and not present in any cgroup path on any
// host running this test.
const foreignCtrID = "3f1a0c9d5e7b2a48c6d1f0938e5b7a2c4d6e8f0a1b2c3d4e5f60718293a4b5c6"

// TestCgroupNamesContainer covers the match rule against the layouts a
// /proc/<pid>/cgroup file actually takes, so the rule is pinned without
// depending on how the host running the suite happens to be arranged.
// Both directions matter: a rule that accepted too much would let a
// recycled PID through, and one that accepted too little would refuse
// legitimate containers and silently disable DNS propagation.
func TestCgroupNamesContainer(t *testing.T) {
	const id = "a0d1bfd9fa47a62f432c8e88db9dec21158008c6c87aae8f57dc66e7ec5b8abc"

	cases := []struct {
		name   string
		cgroup string
		ctrID  string
		want   bool
	}{
		{"v2 systemd driver", "0::/system.slice/docker-" + id + ".scope\n", id, true},
		{"v2 cgroupfs driver", "0::/docker/" + id + "\n", id, true},
		{"v2 inside a private cgroup namespace", "0::/../docker-" + id + ".scope\n", id, true},
		{"v2 under a custom cgroup parent", "0::/myparent.slice/docker-" + id + ".scope\n", id, true},
		{"v1, one line per controller",
			"12:pids:/docker/" + id + "\n11:memory:/docker/" + id + "\n1:name=systemd:/docker/" + id + "\n", id, true},

		{"a different container", "0::/system.slice/docker-" + foreignCtrID + ".scope\n", id, false},
		{"a host process", "0::/user.slice/user-1000.slice/session-3.scope\n", id, false},
		{"the root cgroup", "0::/\n", id, false},
		{"empty container ID is never a match", "0::/system.slice/docker-" + id + ".scope\n", "", false},
		{"empty container ID against an empty cgroup", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cgroupNamesContainer(tc.cgroup, tc.ctrID); got != tc.want {
				t.Errorf("cgroupNamesContainer(%q, %q) = %v, want %v", tc.cgroup, tc.ctrID, got, tc.want)
			}
		})
	}
}

// selfCgroup returns this process's own cgroup contents, and a value to
// pass as the container ID that is guaranteed to match it. The whole
// trimmed file is used rather than a parsed-out component: the point
// here is to exercise the real /proc path with a matching ID on any
// host, whatever its cgroup layout -- the match RULE is pinned by
// TestCgroupNamesContainer instead.
func selfCgroup(t *testing.T, pid int) string {
	t.Helper()
	// proc-path-discipline: allow -- this is the test harness reading
	// its OWN cgroup to build a matching container ID. The hazard the
	// gate guards is a path built from a PID that came from Docker and
	// may have been recycled since; this PID is os.Getpid().
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/cgroup: %v (the guard this test covers reads the same file)", pid, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		t.Fatalf("/proc/%d/cgroup is empty; the guard has nothing to match on", pid)
	}
	return s
}

// TestWriteContainerResolvConf_RefusesAPIDThatIsNotTheContainer is the
// #688 guard. The PID reaching writeContainerResolvConf was resolved
// through Docker some time earlier; the plugin runs in the HOST PID
// namespace, so if the container exited in that window and the kernel
// recycled the PID, the setns lands in an unrelated host process and
// DHCP-server-supplied content is written over its /etc/resolv.conf.
//
// This drives the real writer with a live PID that is emphatically not
// the named container -- the test process itself -- and asserts it is
// refused before any namespace is touched.
func TestWriteContainerResolvConf_RefusesAPIDThatIsNotTheContainer(t *testing.T) {
	err := writeContainerResolvConf(os.Getpid(), foreignCtrID, []string{"192.0.2.53"}, nil, "")
	if err == nil {
		t.Fatal("expected a refusal: the plugin would have written resolv.conf into a process that is not the container")
	}
	if !errors.Is(err, errPIDNotContainer) {
		t.Fatalf("refused for the wrong reason, so the counter would not fire: %v", err)
	}
}

// An empty container ID must not degrade into "check nothing" on the
// real /proc path either.
func TestOpenContainerProc_RefusesAnEmptyContainerID(t *testing.T) {
	if _, err := openContainerProc(os.Getpid(), ""); !errors.Is(err, errPIDNotContainer) {
		t.Fatalf("empty container ID was not treated as unverifiable: %v", err)
	}
}

// The opposite direction on the real /proc path: a guard that refused
// every PID would satisfy the two tests above and silently disable DNS
// propagation for every user.
func TestOpenContainerProc_AcceptsThePIDItWasResolvedFrom(t *testing.T) {
	d, err := openContainerProc(os.Getpid(), selfCgroup(t, os.Getpid()))
	if err != nil {
		t.Fatalf("a PID whose cgroup names the expected ID was rejected: %v", err)
	}
	defer d.Close()

	if fd, err := unix.Openat(int(d.Fd()), "ns/mnt", unix.O_RDONLY, 0); err != nil {
		t.Errorf("ns/mnt is not reachable below the returned fd: %v — the writer could not use it", err)
	} else {
		_ = unix.Close(fd)
	}
}

// The ordinary production case for this guard is not an attack, it is a
// container that exited before the renewal got to it. openContainerProc
// must refuse that cleanly rather than opening something else.
func TestOpenContainerProc_RefusesAPIDThatIsGone(t *testing.T) {
	c := exec.Command("true")
	if err := c.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	pid := c.Process.Pid
	if err := c.Wait(); err != nil {
		t.Fatalf("helper did not exit cleanly: %v", err)
	}

	d, err := openContainerProc(pid, foreignCtrID)
	if err == nil {
		_ = d.Close()
		t.Fatal("a PID with no process behind it was accepted")
	}
}

// TestOpenContainerProc_FdCannotFollowARecycledPID pins the second half
// of the fix: the cgroup check is only worth anything if what it
// validated is what gets used. procfs invalidates a /proc/<pid>
// directory entry when the task exits, so every openat below the
// returned fd fails with ESRCH rather than reaching whatever process
// later holds that PID. Re-deriving "/proc/<pid>/ns/mnt" as a string
// after the check would reopen exactly the window #688 is about.
func TestOpenContainerProc_FdCannotFollowARecycledPID(t *testing.T) {
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	pid := c.Process.Pid
	defer func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	}()

	d, err := openContainerProc(pid, selfCgroup(t, pid))
	if err != nil {
		t.Fatalf("openContainerProc on a live helper: %v", err)
	}
	defer d.Close()

	if fd, err := unix.Openat(int(d.Fd()), "ns/mnt", unix.O_RDONLY, 0); err != nil {
		t.Fatalf("ns/mnt should be reachable while the task lives: %v", err)
	} else {
		_ = unix.Close(fd)
	}

	_ = c.Process.Kill()
	_ = c.Wait()

	_, err = unix.Openat(int(d.Fd()), "ns/mnt", unix.O_RDONLY, 0)
	if !errors.Is(err, unix.ESRCH) {
		t.Fatalf("openat below the pinned /proc fd returned %v after the task exited, want ESRCH: "+
			"the fd is not pinning the task and a recycled PID could be reached through it", err)
	}
}
