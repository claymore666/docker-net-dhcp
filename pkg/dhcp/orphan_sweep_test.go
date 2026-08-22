// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// fakeProc builds a /proc-shaped tree under t.TempDir() and points
// procRoot at it. Each entry is a pid with a cmdline (NUL-separated, as
// the kernel writes it) and a comm.
//
// A real /proc cannot be used here: the test would have to spawn a
// process that survives the assertion, and the assertion is "we killed
// it". The shape of /proc is the only part of the kernel this code
// depends on, and it is stable.
func fakeProc(t *testing.T, procs map[int][2]string) {
	t.Helper()

	root := t.TempDir()
	for pid, entry := range procs {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		argv := strings.Join(strings.Split(entry[0], " "), "\x00") + "\x00"
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(argv), 0o644); err != nil {
			t.Fatalf("write cmdline: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(entry[1]+"\n"), 0o644); err != nil {
			t.Fatalf("write comm: %v", err)
		}
	}
	// Non-pid entries: /proc is full of them and the scanner must not
	// trip over one.
	for _, name := range []string{"self", "meminfo", "sys"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

// recordKills swaps killProcess for a recorder and returns the slice it
// writes to, plus a way to make a given pid fail.
func recordKills(t *testing.T, fail map[int]error) (*[]int, *[]syscall.Signal) {
	t.Helper()

	var mu sync.Mutex
	pids := []int{}
	sigs := []syscall.Signal{}

	old := killProcess
	killProcess = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		defer mu.Unlock()
		if err, ok := fail[pid]; ok {
			return err
		}
		pids = append(pids, pid)
		sigs = append(sigs, sig)
		return nil
	}
	t.Cleanup(func() { killProcess = old })
	return &pids, &sigs
}

// clientArgv is the argv shape a real client produces: the marker
// reaches the child only through dhcpcd's -f, pointing at the work
// directory. If newClient ever stops putting the work dir in argv this
// helper is a lie, which is what TestSweepOrphans_MarkerIsReallyInTheArgv
// is for.
func clientArgv(workDir string) string {
	return dhcpcdBin + " -B --noconfigure -L -A -c /hook -f " +
		filepath.Join(workDir, "dhcpcd.conf") + " -4 eth0"
}

// TestSweepOrphans_KillsOnlyOurDhcpcds is the core contract: both halves
// of the match must hold.
//
// The negatives are the point. This sweep runs at plugin startup, on a
// host PID namespace, and it sends SIGKILL. A false positive is not a
// missed cleanup — it is the plugin killing an unrelated process on the
// operator's host.
func TestSweepOrphans_KillsOnlyOurDhcpcds(t *testing.T) {
	ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")

	fakeProc(t, map[int][2]string{
		// Ours: marker in argv, comm is dhcpcd.
		101: {clientArgv(ourDir), dhcpcdBin},
		102: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"def456")), dhcpcdBin},

		// A dhcpcd that is NOT ours — someone else's on the same host.
		// The plugin has no business signalling it.
		201: {dhcpcdBin + " -B -f /etc/dhcpcd.conf eth0", dhcpcdBin},

		// Names our work dir but is not dhcpcd: a shell, and the
		// unshare wrapper before it has exec'd. Killing the wrapper
		// would abort a client that is mid-start.
		202: {"/bin/sh -c cat " + ourDir + "/dhcpcd.conf", "sh"},
		203: {"/usr/bin/unshare -m /bin/sh -c mount... " + ourDir, "unshare"},

		// Unrelated entirely.
		204: {"/usr/bin/dockerd --debug", "dockerd"},
	})
	killed, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans() error = %v", err)
	}

	slices.Sort(*killed)
	want := []int{101, 102}
	if !slices.Equal(*killed, want) {
		t.Errorf("killed %v, want %v — a sweep that signals the wrong pid is "+
			"the plugin killing an unrelated process on the operator's host", *killed, want)
	}
	if n != len(want) {
		t.Errorf("SweepOrphans() = %d, want %d", n, len(want))
	}
}

// TestSweepOrphans_UsesSIGKILLNotSIGTERM pins the one decision in this
// file that is easy to get backwards, and that no other test would
// notice.
//
// When the plugin restarts, the containers are still RUNNING. The
// persistent client omits dhcpcd's -p, so it releases its lease when
// asked to stop politely — a SIGTERM sweep would send a DHCPRELEASE for
// every address a live container currently holds, and invite the server
// to hand those addresses to somebody else. That is #524's duplicate
// assignment, manufactured by the cleanup itself, and it is the same
// asymmetry #720 turns on.
//
// A signal is a one-character edit away from being wrong here and the
// wrong one still passes every other test in this file.
func TestSweepOrphans_UsesSIGKILLNotSIGTERM(t *testing.T) {
	fakeProc(t, map[int][2]string{
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"abc")), dhcpcdBin},
	})
	_, sigs := recordKills(t, nil)

	if _, err := SweepOrphans(); err != nil {
		t.Fatalf("SweepOrphans() error = %v", err)
	}
	if len(*sigs) != 1 {
		t.Fatalf("signals sent = %d, want 1", len(*sigs))
	}
	if (*sigs)[0] != syscall.SIGKILL {
		t.Errorf("signal = %v, want SIGKILL — SIGTERM makes dhcpcd release the "+
			"lease of a container that is still running", (*sigs)[0])
	}
}

// TestSweepOrphans_EmptyProcIsNotAnError: nothing to sweep is the normal
// case on a clean start. It must report zero, not fail — the caller runs
// this before recovery on every boot.
func TestSweepOrphans_EmptyProcIsNotAnError(t *testing.T) {
	fakeProc(t, map[int][2]string{
		204: {"/usr/bin/dockerd", "dockerd"},
	})
	killed, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans() error = %v", err)
	}
	if n != 0 || len(*killed) != 0 {
		t.Errorf("SweepOrphans() = %d killing %v, want 0 killing nothing", n, *killed)
	}
}

// TestSweepOrphans_UnreadableProcEntryIsNotAMatch: a pid that vanishes
// mid-scan, or one this plugin cannot read, must be a "no".
//
// The direction matters. Treating an unreadable entry as a match would
// make the sweep kill on ABSENCE of evidence, on a host PID namespace,
// with SIGKILL.
func TestSweepOrphans_UnreadableProcEntryIsNotAMatch(t *testing.T) {
	fakeProc(t, map[int][2]string{
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"abc")), dhcpcdBin},
	})
	// A pid directory with neither cmdline nor comm — the shape left by
	// a process that exited between ReadDir and the read.
	if err := os.MkdirAll(filepath.Join(procRoot, "999"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// And one with a matching cmdline but no comm at all.
	dir := filepath.Join(procRoot, "998")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"),
		[]byte(clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"x"))), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
	killed, _ := recordKills(t, nil)

	if _, err := SweepOrphans(); err != nil {
		t.Fatalf("SweepOrphans() error = %v", err)
	}
	if !slices.Equal(*killed, []int{101}) {
		t.Errorf("killed %v, want [101] — an unreadable /proc entry must be a "+
			"'no', not a kill on absence of evidence", *killed)
	}
}

// TestSweepOrphans_ESRCHIsNotCounted: a process that exits between the
// scan and the kill produced the outcome we wanted, but it is not
// something this sweep did. Counting it would inflate the number an
// operator reads.
func TestSweepOrphans_ESRCHIsNotCounted(t *testing.T) {
	fakeProc(t, map[int][2]string{
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"a")), dhcpcdBin},
		102: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"b")), dhcpcdBin},
	})
	recordKills(t, map[int]error{101: syscall.ESRCH})

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans() error = %v", err)
	}
	if n != 1 {
		t.Errorf("SweepOrphans() = %d, want 1 — a pid that had already exited "+
			"was not killed by this sweep", n)
	}
}

// TestSweepOrphans_KillFailureDoesNotStopTheSweep: one EPERM must not
// leave the rest of the orphans running. Recovery is about to start a
// second client for every one this misses.
func TestSweepOrphans_KillFailureDoesNotStopTheSweep(t *testing.T) {
	fakeProc(t, map[int][2]string{
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"a")), dhcpcdBin},
		102: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"b")), dhcpcdBin},
		103: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"c")), dhcpcdBin},
	})
	killed, _ := recordKills(t, map[int]error{102: syscall.EPERM})

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans() error = %v", err)
	}
	slices.Sort(*killed)
	if !slices.Equal(*killed, []int{101, 103}) || n != 2 {
		t.Errorf("killed %v (n=%d), want [101 103] (n=2) — one refused kill must "+
			"not abandon the remaining orphans", *killed, n)
	}
}

// TestSweepOrphans_MissingProcRootIsAnError: the sweep runs on a host
// where /proc is always there. If it is not, the plugin is about to
// start clients for endpoints whose old clients it could not look for,
// and the caller must be told rather than reading a confident zero.
//
// Absent data is not a zero.
func TestSweepOrphans_MissingProcRootIsAnError(t *testing.T) {
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { procRoot = old })

	if _, err := SweepOrphans(); err == nil {
		t.Error("SweepOrphans() over a missing /proc = nil error, want an error — " +
			"a sweep that could not look must not report a clean zero")
	}
}

// TestSweepOrphans_MarkerIsReallyInTheArgv is the join between this file
// and newClient, and it is the case that keeps the sweep from quietly
// retiring.
//
// SweepOrphans finds nothing except through workDirPrefix appearing in a
// child's argv, and it gets there only because dhcpcd's -f points at the
// work directory. Nothing else forces that: a refactor that moved the
// config elsewhere, or renamed the temp prefix in one of the two places,
// would leave a sweep that is green, runs on every boot, and matches
// nothing forever.
func TestSweepOrphans_MarkerIsReallyInTheArgv(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	t.Cleanup(func() {
		_ = c.fifoRead.Close()
		_ = c.fifoKeep.Close()
		c.closeLogPipes()
		_ = os.RemoveAll(c.workDir)
	})

	joined := strings.Join(c.cmd.Args, "\x00")
	if !strings.Contains(joined, workDirPrefix) {
		t.Fatalf("client argv %v carries no %q — SweepOrphans matches on that "+
			"prefix and can no longer find anything", c.cmd.Args, workDirPrefix)
	}
	if !strings.Contains(c.workDir, workDirPrefix) {
		t.Errorf("workDir %q does not carry %q", c.workDir, workDirPrefix)
	}
}

// TestNewClient_ChildGetsItsOwnProcessGroup pins the other half of #722.
//
// Without Setpgid the child shares the plugin's process group, so a
// signal aimed at the group — a supervisor's shutdown, a terminal, a
// `kill -- -<pgid>` — reaches every live dhcpcd too. The persistent
// client omits dhcpcd's -p, so each one would RELEASE the lease of a
// container that is still running.
func TestNewClient_ChildGetsItsOwnProcessGroup(t *testing.T) {
	c, err := NewDHCPClient("eth0", &DHCPClientOptions{})
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	t.Cleanup(func() {
		_ = c.fifoRead.Close()
		_ = c.fifoKeep.Close()
		c.closeLogPipes()
		_ = os.RemoveAll(c.workDir)
	})

	if c.cmd.SysProcAttr == nil {
		t.Fatal("cmd.SysProcAttr is nil: the child shares the plugin's process " +
			"group, so a group signal releases every live container's lease")
	}
	if !c.cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid is false: the child shares the plugin's process group")
	}
	// Pdeathsig is deliberately NOT set; see the comment in NewDHCPClient.
	// Linux delivers it on the death of the spawning THREAD, and Start's
	// netns-restore-failure path kills its thread on purpose.
	if c.cmd.SysProcAttr.Pdeathsig != 0 {
		t.Errorf("Pdeathsig = %v, want 0 — it fires on the spawning thread's "+
			"death, and Start deliberately kills that thread on the "+
			"netns-restore-failure path", c.cmd.SysProcAttr.Pdeathsig)
	}
}
