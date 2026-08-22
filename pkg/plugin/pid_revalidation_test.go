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

// TestCgroupNamesContainer_DelegatedSubtreeIsAcceptedByDesign records
// the residual hole as a TRUE, in the test name, so it is a decision on
// the record rather than something rediscovered later as a surprise.
//
// A task in a cgroup subtree its own user controls can name a component
// after any container ID it likes, and cgroupNamesContainer says yes.
// That is reachable without privilege in one command -- see the doc
// comment on the function -- and it is not something a better NAME
// match can fix: a bare `<id>` component is exactly what the cgroupfs
// driver emits, so refusing it would refuse legitimate containers, and
// `docker-<id>.scope` is namable in a delegated slice just as easily.
// The function is a filter; openContainerProc carries the identity
// proof.
//
// THE POINT OF ASSERTING IT TRUE is that these cases go red the day
// someone narrows the match and believes that closed the hole. A
// narrowing to whole path components leaves every one of these
// accepted, so a red here means the claim outran the change.
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

// selfCgroup returns this process's own cgroup contents, and a value to
// pass as the container ID that is guaranteed to match it. The whole
// trimmed file is used rather than a parsed-out component: the point
// here is to exercise the real /proc path with a matching ID on any
// host, whatever its cgroup layout -- the match RULE is pinned by
// TestCgroupNamesContainer instead.
// selfCgroupLeaf returns a container ID that genuinely NAMES this
// process: the leaf segment of the path field of its own cgroup line.
//
// # Why this exists, and what it replaces
//
// selfCgroup returns the whole /proc/<pid>/cgroup FILE. Seven call
// sites passed that file where a CONTAINER ID was expected, so the
// guard's accept path was
//
//	strings.Contains(cgroupFile, cgroupFile)   // Contains(x, x)
//
// which is true for every input. Every "the guard accepts a PID that
// IS the container" assertion in this package passed because a string
// contains itself, and would have passed against a correct guard and a
// broken one alike. The accept path had never been exercised with a
// container ID at all -- which is why the substring defect survived a
// suite that looks like it covers this.
//
// The leaf segment is a real ID: it is the name of the cgroup this
// process is actually in, so a guard that matches segments accepts it
// and a guard that does not, does not. Paired with foreignCtrID as the
// refusal control, the two together can tell the fix from the defect.
//
// A root cgroup ("0::/") has no segment to name, and then the accept
// path genuinely cannot be exercised in this environment. That is a
// FATAL with the reason, not a skip: skipping would restore exactly
// the blindness this replaces, quietly.
func selfCgroupLeaf(t *testing.T, pid int) string {
	t.Helper()
	raw := string(selfCgroup(t, pid))
	for _, line := range strings.Split(raw, "\n") {
		// "hierarchy-ID:controller-list:cgroup-path" in both cgroup
		// versions. Split at the SECOND colon and take everything after
		// it: a cgroup directory name may itself contain a colon, so
		// splitting at the last one would truncate the path.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		segs := strings.Split(parts[2], "/")
		if leaf := segs[len(segs)-1]; leaf != "" {
			return leaf
		}
	}
	// The observed value, not just the diagnosis. A red here has to name
	// the remedy from ONE run: "0::/" means this process sits in the root
	// cgroup and nothing can name it, which is a different cause with a
	// different fix than a layout we failed to parse.
	t.Fatalf("no line of /proc/%d/cgroup has a named leaf segment, so nothing names this "+
		"process and the guard's ACCEPT path cannot be exercised in this environment.\n"+
		"observed /proc/%d/cgroup:\n%s\n"+
		"(a bare \"0::/\" means the root cgroup; anything else means the path field "+
		"parsed empty and the layout is the thing to look at)", pid, pid, raw)
	return ""
}

// cgroupFileContents is the whole text of a /proc/<pid>/cgroup file.
//
// IT IS A DISTINCT TYPE SO IT CANNOT BE PASSED WHERE A CONTAINER ID IS
// WANTED. That is not decoration: seven call sites did exactly that,
// which made the guard's accept path Contains(x, x) and the suite unable
// to fail (#788).
//
// Repairing those call sites was not enough on its own. Reverting them
// to selfCgroup left selfCgroupLeaf in the file -- correct, tested, and
// on no path any test takes -- and the whole package stayed GREEN, so
// the suite silently became a mirror again. A test on a helper cannot
// see a caller that stops using the helper, and neither can a gate keyed
// on the helper.
//
// The property is about the CALLER, so it is enforced where the caller
// is written: passing this where a string ctrID is expected does not
// compile, so the seven accidental call sites cannot recur as
// accidents.
//
// It does NOT make the mirror impossible. string(...) still converts
// it, and selfCgroupLeaf above needs exactly that -- so a caller can
// still write cgroupNamesContainer(string(f), string(f)) and get
// Contains(x, x) back, compiling and green. Measured, not assumed. A
// named type proves a conversion was WRITTEN; it cannot prove the
// value is the right one. What this buys is that the mirror becomes a
// deliberate act instead of an accident, which is what the seven were.
type cgroupFileContents string

func selfCgroup(t *testing.T, pid int) cgroupFileContents {
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
	return cgroupFileContents(s)
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
		t.Fatalf("refused for the wrong reason: dns_propagation_pid_mismatches keys off this cause, and "+
			"the count is asserted in TestPropagateDNS_CountsAPIDMismatch: %v", err)
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
