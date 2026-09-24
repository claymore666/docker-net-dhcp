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

const foreignCtrID = "3f1a0c9d5e7b2a48c6d1f0938e5b7a2c4d6e8f0a1b2c3d4e5f60718293a4b5c6"

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

		{"v2, PID moved to a descendant of the scope (systemd in docker)",
			"0::/system.slice/docker-" + id + ".scope/init.scope\n", id, true},

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

// A task in a user-delegated cgroup subtree can name any container ID, and a bare `<id>` is what the
// cgroupfs driver emits, so the filter accepts it and openContainerProc proves identity (#695).
func TestCgroupNamesContainer_DelegatedSubtreeIsAcceptedByDesign(t *testing.T) {
	const id = "a0d1bfd9fa47a62f432c8e88db9dec21158008c6c87aae8f57dc66e7ec5b8abc"

	cases := []struct {
		name   string
		cgroup string
	}{
		{"a bare ID segment in a user's own slice", "0::/user.slice/" + id + "\n"},
		{"an ID segment with the task nested below it",
			"0::/user.slice/" + id + "/nested/task.scope\n"},
		{"the systemd spelling in a user's own slice",
			"0::/user.slice/docker-" + id + ".scope\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !cgroupNamesContainer(tc.cgroup, id) {
				t.Errorf("cgroupNamesContainer(%q, id) = false, want true — this is a"+
					" KNOWN and ACCEPTED hole, not a defect to close here; see the"+
					" doc comment on cgroupNamesContainer", tc.cgroup)
			}
		})
	}
}

func selfCgroupLeaf(t *testing.T, pid int) string {
	t.Helper()
	raw := string(selfCgroup(t, pid))
	for _, line := range strings.Split(raw, "\n") {
		// "hierarchy-ID:controllers:path": split at the second colon, since a cgroup directory name may contain one.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		segs := strings.Split(parts[2], "/")
		if leaf := segs[len(segs)-1]; leaf != "" {
			return leaf
		}
	}
	t.Fatalf("no line of /proc/%d/cgroup has a named leaf segment, so nothing names this "+
		"process and the guard's ACCEPT path cannot be exercised in this environment.\n"+
		"observed /proc/%d/cgroup:\n%s\n"+
		"(a bare \"0::/\" means the root cgroup; anything else means the path field "+
		"parsed empty and the layout is the thing to look at)", pid, pid, raw)
	return ""
}

// cgroupFileContents is a distinct type so a cgroup file cannot pass as a container ID, which made the accept path
// Contains(x, x) (#788).
type cgroupFileContents string

func selfCgroup(t *testing.T, pid int) cgroupFileContents {
	t.Helper()
	// proc-path-discipline: allow -- the harness reads its own cgroup, and os.Getpid() is not a recycled Docker PID.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/cgroup: %v (the guard this test covers reads the same file)", pid, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		t.Fatalf("/proc/%d/cgroup is empty; the guard has nothing to match on", pid)
	}
	return cgroupFileContents(s)
}

// The plugin runs in the host PID namespace, so a recycled PID would aim setns at an unrelated host process (#688).
func TestWriteContainerResolvConf_RefusesAPIDThatIsNotTheContainer(t *testing.T) {
	err := writeContainerResolvConf(os.Getpid(), foreignCtrID, []string{"192.0.2.53"}, nil, "", "")
	if err == nil {
		t.Fatal("expected a refusal: the plugin would have written resolv.conf into a process that is not the container")
	}
	if !errors.Is(err, errPIDNotContainer) {
		t.Fatalf("refused for the wrong reason: dns_propagation_pid_mismatches keys off this cause, and "+
			"the count is asserted in TestPropagateDNS_CountsAPIDMismatch: %v", err)
	}
}

func TestOpenContainerProc_RefusesAnEmptyContainerID(t *testing.T) {
	if _, err := openContainerProc(os.Getpid(), ""); !errors.Is(err, errPIDNotContainer) {
		t.Fatalf("empty container ID was not treated as unverifiable: %v", err)
	}
}

func TestOpenContainerProc_AcceptsThePIDItWasResolvedFrom(t *testing.T) {
	d, err := openContainerProc(os.Getpid(), selfCgroupLeaf(t, os.Getpid()))
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

	d, err := openContainerProc(pid, selfCgroupLeaf(t, pid))
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
