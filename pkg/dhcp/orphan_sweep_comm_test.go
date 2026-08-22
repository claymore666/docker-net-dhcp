// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The kernel's comm is not our constant, and this file is the only place
// that says so in a value it does not derive.
//
// `/proc/<pid>/comm` is written by the kernel from task_struct.comm: the
// BASENAME of the executable, truncated to TASK_COMM_LEN-1 = 15 bytes.
// It can never contain a `/`, and it is not affected by how the process
// was invoked. `dhcpcdBin` is ours, it is an absolute path, and #761
// made it absolute — at which point `commOf(pid) != dhcpcdBin` became
// true for every process alive and the sweep began returning a confident
// zero.
//
// WHY THIS FILE EXISTS SEPARATELY. The fixtures in orphan_sweep_test.go
// wrote `dhcpcdBin` into the comm position at seventeen sites, so when
// the constant moved, both sides of the comparison moved with it and the
// suite stayed green. A mirror test cannot catch a change to the thing
// it mirrors. Every literal below is written out on purpose.
//
// WHICH SUBSTITUTION ACTUALLY BREAKS IT, measured rather than asserted,
// because the two obvious ways to "remove the duplication" are not
// equally bad and an over-broad warning is the same defect as an
// over-broad claim:
//
//   realKernelComm = dhcpcdBin              -> restores the mirror in
//     full. Revert commComparand and the seven SweepOrphans tests go
//     GREEN under the bug again. Only ComparandIsAComm still fires --
//     one test, but it fires, which is why that one is a property and
//     not a case.
//
//   realKernelComm = filepath.Base(dhcpcdBin) -> does NOT break it.
//     Revert commComparand and all nine go red, because Base(x) and x
//     can no longer move in step. It makes one assertion tautological
//     and nothing else. It also cannot be written as it stands: a const
//     initializer may not call a function, so reaching this spelling
//     takes a deliberate const-to-var change first.
//
// So: `dhcpcdBin` is the one to refuse. It is a mirror, and a mirror is
// what let the bug ship.

// realKernelComm is what the kernel actually puts in /proc/<pid>/comm
// for the plugin's dhcpcd children. Deliberately NOT derived from
// dhcpcdBin.
const realKernelComm = "dhcpcd"

// TestIsOrphanedClient_MatchesTheKernelsComm is the regression test for
// the inert sweep. It fails on the tree where `:212` compares against
// the absolute path.
func TestIsOrphanedClient_MatchesTheKernelsComm(t *testing.T) {
	ourDir := "/tmp/" + workDirPrefix + "abc123"

	fakeProc(t, map[int][2]string{
		// The argv carries the absolute path, because that is what
		// exec was handed. The comm carries the basename, because that
		// is what the kernel wrote. Both are true of the same process
		// at the same time, and that asymmetry is the whole defect.
		101: {"/sbin/dhcpcd -B --noconfigure -f " + ourDir + "/dhcpcd.conf -4 eth0", realKernelComm},
	})
	killed, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans() error = %v", err)
	}
	if n != 1 || len(*killed) != 1 || (*killed)[0] != 101 {
		t.Fatalf("SweepOrphans() = %d, killed %v; want 1 and [101].\n"+
			"An orphaned dhcpcd whose comm is exactly what the kernel writes was "+
			"not matched. A sweep that returns zero on a host full of orphans "+
			"reports the same as a sweep that found none, and #722's duplicate "+
			"client is back: recoverEndpoints starts a second dhcpcd per endpoint "+
			"on the same DUID/IAID/client-id.", n, *killed)
	}
}

// TestIsOrphanedClient_ComparandIsAComm states the invariant as a
// property rather than as a case, so it holds against a value nobody has
// written a fixture for — including the next one somebody absolutizes.
//
// WHAT IT PINS, and what it does not: the COMPARAND. Regress
// commComparand's body and this fires. Leave that function correct and
// change the CALL SITE to bypass it, and this stays GREEN — it would be
// asserting about a helper nobody calls, over an inert sweep. Measured:
// a body revert reddens nine tests, a call-site revert reddens eight,
// and this is the one missing. That case is caught by the de-mirrored
// fixtures and by MatchesTheKernelsComm, which assert on behaviour
// rather than on the constant, so the bug is caught either way — but
// not by this test, and a comment claiming otherwise would be the same
// defect this file exists to fix, one level up.
// TestComm_IsAlwaysABasename below proves the PREMISE it rests on,
// against the kernel rather than against us; this one applies that
// premise to the constant we control. Neither subsumes the other.
func TestIsOrphanedClient_ComparandIsAComm(t *testing.T) {
	got := commComparand()

	if strings.ContainsRune(got, '/') {
		t.Errorf("the value compared against /proc/<pid>/comm is %q, which contains a '/'.\n"+
			"The kernel writes a basename there and never a path, so this comparison "+
			"can never be true and the sweep is inert.", got)
	}
	if len(got) > 15 {
		t.Errorf("the value compared against /proc/<pid>/comm is %q (%d bytes).\n"+
			"The kernel truncates comm to TASK_COMM_LEN-1 = 15 bytes, so a longer "+
			"comparand can never match.", got, len(got))
	}
	if got != realKernelComm {
		t.Errorf("comm comparand = %q, want %q — the literal the kernel writes for "+
			"this binary, spelled out rather than derived.", got, realKernelComm)
	}
}

// TestComm_IsAlwaysABasename asks the KERNEL what it writes into comm,
// rather than asking our own fixtures.
//
// Every other test in this package builds a /proc and then asserts
// against what it built — including the seventeen fixture sites that
// wrote `dhcpcdBin` into the comm position and therefore agreed with the
// bug. That is the plugin's own opinion, not outside evidence. This test
// is the only place the premise is checked against the thing that
// actually decides it.
//
// Pure stdlib, no dhcpcd, no root, no network, milliseconds. It runs in
// the ordinary unit lane.
func TestComm_IsAlwaysABasename(t *testing.T) {
	// NO PLATFORM GUARD, deliberately. A `runtime.GOOS != "linux"` skip
	// here would be unreachable: this package does not compile off Linux
	// at all — orphan_sweep_test.go uses SysProcAttr.Pdeathsig, which
	// only exists there — so the test binary cannot be built on a host
	// where the guard would fire. A skip that can never run is not
	// portability, it is a comment that looks like code.
	//
	// An absolute path is handed to exec, exactly as the plugin hands
	// dhcpcdBin to it. What lands in comm is the question.
	const bin = "/bin/sleep"
	cmd := exec.Command(bin, "10")
	if err := cmd.Start(); err != nil {
		// Fatal, not Skip. If this host cannot start /bin/sleep then the
		// premise underneath the whole package is unverified, and a green
		// run would say the opposite. A broken environment must not
		// report the same as a working one.
		t.Fatalf("cannot start %s: %v — the kernel-comm premise is unverified", bin, err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// proc-path-discipline: allow — this is the test that proves the
	// premise, so it must reach /proc the way the kernel exposes it
	// rather than through a helper of ours. The hazard that rule guards
	// does not arise: the pid is our OWN child, just started and held
	// alive by this function until the deferred Kill, so it cannot have
	// been recycled to name another task. openContainerProc is the wrong
	// tool here for the same reason — it confirms a cgroup names a
	// CONTAINER, and this process is not in one.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", cmd.Process.Pid))
	if err != nil {
		t.Fatalf("read comm: %v", err)
	}
	got := strings.TrimSpace(string(b))

	if strings.ContainsRune(got, '/') {
		t.Errorf("kernel wrote %q into comm for %q — it contains a '/'.\n"+
			"Everything in this package assumes comm is never a path.", got, bin)
	}
	if got != filepath.Base(bin) {
		t.Errorf("kernel wrote %q into comm for %q, want the basename %q.\n"+
			"isOrphanedClient compares a constant of ours against this value; if the "+
			"kernel does not write the basename, that constant is wrong.",
			got, bin, filepath.Base(bin))
	}
	if len(got) > 15 {
		t.Errorf("kernel wrote %q (%d bytes) — longer than TASK_COMM_LEN-1 = 15, "+
			"which contradicts the truncation this package relies on", got, len(got))
	}
}
