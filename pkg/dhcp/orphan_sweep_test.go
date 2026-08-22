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
		// Orphaned by default: reparented to init. Tests that care about
		// a LIVE parent say so with setParent, so the ones that do not
		// keep asserting about orphans, which is what they were written
		// for.
		writeStatus(t, dir, 1)
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

// writeStatus writes the one field of /proc/<pid>/status this code
// reads, in the kernel's own layout. The surrounding lines are included
// because parentPID scans for a prefix and a file with exactly one line
// would not exercise that.
func writeStatus(t *testing.T, dir string, ppid int) {
	t.Helper()

	body := "Name:\tdhcpcd\nUmask:\t0022\nState:\tS (sleeping)\n" +
		"Tgid:\t1\nNgid:\t0\nPid:\t1\nPPid:\t" + strconv.Itoa(ppid) +
		"\nTracerPid:\t0\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}
}

// setParent re-points a fake process at a parent pid, after fakeProc has
// defaulted it to init.
func setParent(t *testing.T, pid, ppid int) {
	t.Helper()
	writeStatus(t, filepath.Join(procRoot, strconv.Itoa(pid)), ppid)
}

// pinSelfComm fixes what this process calls itself, so the "another live
// instance" tests do not depend on what the test binary happens to be
// named.
func pinSelfComm(t *testing.T, comm string) {
	t.Helper()
	old := selfComm
	selfComm = comm
	t.Cleanup(func() { selfComm = old })
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

// THE TWO POSITIONS IN A FIXTURE ARE NOT THE SAME KIND OF VALUE, and
// the asymmetry is deliberate.
//
// argv is ours: exec is handed dhcpcdBin, absolute, so the fixtures
// below build argv from dhcpcdBin and must keep doing so. comm is the
// KERNEL's: it writes the basename, truncated to 15 bytes, whatever we
// passed to exec — so the fixtures spell out realKernelComm instead.
//
// Until #761 both positions read dhcpcdBin and the tests passed, because
// making that constant absolute moved BOTH SIDES of `commOf(pid) !=
// dhcpcdBin` in step. Seventeen fixtures agreed with the bug and the
// suite stayed green. A mirror test cannot catch a change to the thing
// it mirrors, so the comm side is no longer a mirror.
//
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
		101: {clientArgv(ourDir), realKernelComm},
		102: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"def456")), realKernelComm},

		// A dhcpcd that is NOT ours — someone else's on the same host.
		// The plugin has no business signalling it.
		201: {dhcpcdBin + " -B -f /etc/dhcpcd.conf eth0", realKernelComm},

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
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"abc")), realKernelComm},
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
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"abc")), realKernelComm},
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
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"a")), realKernelComm},
		102: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"b")), realKernelComm},
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
		101: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"a")), realKernelComm},
		102: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"b")), realKernelComm},
		103: {clientArgv(filepath.Join(os.TempDir(), workDirPrefix+"c")), realKernelComm},
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
// TestSweepOrphans_SparesALiveClientOfAnotherInstance is the test that
// makes the function's name true.
//
// Two instances of this plugin side by side is a supported
// configuration — `docker plugin install --alias`, or one from each
// registry while an operator compares versions. Their clients are
// INDISTINGUISHABLE by argv and comm: the work directory comes from
// os.MkdirTemp in each plugin's own private /tmp, so the marker is the
// same string, and both are dhcpcd. Without a parent check the second
// instance to start SIGKILLs the first instance's live clients, and the
// first instance never learns — it is not waiting on a signal it did
// not send, its containers keep running, and their leases stop renewing
// at T2 with nothing counted anywhere.
//
// That is the same outcome the sweep exists to prevent, produced by the
// sweep, which is why this is not merely a nice-to-have negative.
func TestSweepOrphans_SparesALiveClientOfAnotherInstance(t *testing.T) {
	pinSelfComm(t, "net-dhcp")

	ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")
	fakeProc(t, map[int][2]string{
		// The other instance's plugin process, alive.
		900: {"/net-dhcp", "net-dhcp"},
		// Its client. Identical in every respect to one of ours.
		901: {clientArgv(ourDir), realKernelComm},
		// A genuine orphan: same marker, same comm, no plugin parent.
		902: {clientArgv(ourDir), realKernelComm},
		// init, so 902's parent is a real entry rather than a missing one.
		1: {"/sbin/init", "systemd"},
	})
	setParent(t, 901, 900)

	pids, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 1 {
		t.Errorf("killed %d, want 1", n)
	}
	if len(*pids) != 1 || (*pids)[0] != 902 {
		t.Errorf("killed %v, want [902] — 901 belongs to a plugin process "+
			"that is still running, and killing it stops a live container's "+
			"lease from renewing with nothing to show for it", *pids)
	}
}

// TestSweepOrphans_SparesOurOwnLiveClient covers the same rule for this
// process rather than a sibling. NewPlugin sweeps before recovery starts
// any client, so this cannot arise today; it exists so that a second
// call site added later cannot make the sweep eat its own clients
// without a test going red.
func TestSweepOrphans_SparesOurOwnLiveClient(t *testing.T) {
	pinSelfComm(t, "net-dhcp")

	ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")
	fakeProc(t, map[int][2]string{
		801: {clientArgv(ourDir), realKernelComm},
		1:   {"/sbin/init", "systemd"},
	})
	setParent(t, 801, os.Getpid())

	pids, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 0 || len(*pids) != 0 {
		t.Errorf("killed %d %v, want none — this client's parent is us", n, *pids)
	}
}

// TestSweepOrphans_ReparentedToASubreaperIsStillAnOrphan pins the reason
// the check is not `ppid == 1`.
//
// An orphan reparents to the nearest subreaper, and under a container
// runtime that is routinely the shim rather than init. A `ppid == 1`
// test would report such a process as live, skip it, and leave exactly
// the duplicate client this sweep exists to remove — on the hosts this
// plugin actually runs on.
func TestSweepOrphans_ReparentedToASubreaperIsStillAnOrphan(t *testing.T) {
	pinSelfComm(t, "net-dhcp")

	ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")
	fakeProc(t, map[int][2]string{
		700: {"/usr/bin/containerd-shim-runc-v2", "containerd-shim"},
		701: {clientArgv(ourDir), realKernelComm},
	})
	setParent(t, 701, 700)

	pids, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 1 || len(*pids) != 1 || (*pids)[0] != 701 {
		t.Errorf("killed %d %v, want [701] — a subreaper is not a plugin, "+
			"so this client has no one managing it", n, *pids)
	}
}

// TestSweepOrphans_UnreadableParentIsNotAMatch keeps the unknown case on
// the safe side. A missing or unparseable status file means we cannot
// tell whether anything still manages the process, and the question this
// answers is whether to send SIGKILL.
func TestSweepOrphans_MissingPPidIsNotAMatch(t *testing.T) {
	pinSelfComm(t, "net-dhcp")

	ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")
	fakeProc(t, map[int][2]string{
		601: {clientArgv(ourDir), realKernelComm},
		602: {clientArgv(ourDir), realKernelComm},
	})
	// 601: no status file at all.
	if err := os.Remove(filepath.Join(procRoot, "601", "status")); err != nil {
		t.Fatalf("remove status: %v", err)
	}
	// 602: a status file with no PPid line — present, unreadable for
	// this purpose, which is a different failure from absent.
	//
	// BOTH OF THESE FAIL INSIDE parentPID and return at its !ok branch.
	// Neither reaches the comm read that decides whether the parent is a
	// live sibling, and this test was called
	// ...UnreadableParentIsNotAMatch for two pull requests while covering
	// one of the two ways a parent can be unreadable — the one that was
	// already safe. See TestSweepOrphans_ParentCommUnreadableIsNotAMatch
	// for the other, which was a kill.
	if err := os.WriteFile(filepath.Join(procRoot, "602", "status"),
		[]byte("Name:\tdhcpcd\nState:\tS (sleeping)\n"), 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	pids, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 0 || len(*pids) != 0 {
		t.Errorf("killed %d %v, want none — an unknown parent must not "+
			"authorise a kill", n, *pids)
	}
}

// TestSweepOrphans_WithoutOurOwnCommRefuses covers the dependency the
// parent check introduces. If /proc/self/comm cannot be read there is no
// way to recognise a sibling instance, and a sweep that cannot tell a
// live client from an orphan must refuse rather than guess — the same
// rule as a missing /proc root. Reporting zero would read as "there was
// nothing to do".
// TestSweepOrphans_ParentCommUnreadableIsNotAMatch covers the one read
// in isOrphanedClient whose failure direction is KILL.
//
// Every other read there fails safe: cmdline err -> false, parentPID
// !ok -> false, ppid == getpid -> false. The last line is
//
//	return commOf(ppid) != selfComm
//
// and commOf folds its error into "", so an unreadable /proc/<ppid>/comm
// produces "" != selfComm -> true -> orphan -> SIGKILL, as root, against
// the host PID namespace. The function's own doc comment forbids exactly
// that: "treating an unreadable /proc entry as a match would make the
// sweep kill on absence of evidence."
//
// THE THREE CASES DIFFER IN ONE PIECE OF STATE and give three different
// verdicts, which is what makes the middle one admissible. Without the
// controls, a test asserting "no kill" cannot distinguish the fix from a
// fixture that was never going to kill anything.
func TestSweepOrphans_ParentCommUnreadableIsNotAMatch(t *testing.T) {
	const parent = 700

	// setup builds a client 601 parented to 700, and hands the caller
	// the parent's directory to modify. Identical in all three cases.
	setup := func(t *testing.T) string {
		t.Helper()
		pinSelfComm(t, "net-dhcp")
		ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")
		fakeProc(t, map[int][2]string{
			601:    {clientArgv(ourDir), realKernelComm},
			parent: {"/usr/local/bin/net-dhcp", "net-dhcp"},
		})
		setParent(t, 601, parent)
		return filepath.Join(procRoot, strconv.Itoa(parent))
	}

	t.Run("parent alive and readable is spared", func(t *testing.T) {
		setup(t)
		pids, _ := recordKills(t, nil)
		n, err := SweepOrphans()
		if err != nil {
			t.Fatalf("SweepOrphans: %v", err)
		}
		if n != 0 || len(*pids) != 0 {
			t.Errorf("killed %d %v, want none — the parent is a live "+
				"instance of this plugin and its clients are not ours", n, *pids)
		}
	})

	t.Run("parent gone is an orphan", func(t *testing.T) {
		dir := setup(t)
		// The whole /proc entry, not just comm: the parent has exited.
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		pids, _ := recordKills(t, nil)
		n, err := SweepOrphans()
		if err != nil {
			t.Fatalf("SweepOrphans: %v", err)
		}
		if n != 1 || len(*pids) != 1 || (*pids)[0] != 601 {
			t.Errorf("killed %d %v, want [601] — nothing is managing this "+
				"client, which is the case the sweep exists for", n, *pids)
		}
	})

	t.Run("parent present but comm unreadable is not a match", func(t *testing.T) {
		dir := setup(t)
		// ONE FILE, and it is the difference between the two cases
		// above. The parent is still there; we simply cannot read what
		// it calls itself. "Cannot tell" is not "gone".
		if err := os.Remove(filepath.Join(dir, "comm")); err != nil {
			t.Fatalf("remove comm: %v", err)
		}
		pids, _ := recordKills(t, nil)
		n, err := SweepOrphans()
		if err != nil {
			t.Fatalf("SweepOrphans: %v", err)
		}
		if n != 0 || len(*pids) != 0 {
			t.Errorf("killed %d %v, want none — an unreadable parent comm "+
				"is absence of evidence, and this sends SIGKILL as root "+
				"against the host PID namespace", n, *pids)
		}
	})
}

// TestSweepOrphans_NoUnreadableProcFileAuthorisesAKill is keyed on the
// PROPERTY, not on the mechanism, and that is the difference between a
// test that outlives this fix and one that does not.
//
// The mechanism-keyed version of this — "commOf returns (string, bool)
// and the parent check reads the bool" — is green forever and blind to
// the fifth /proc read somebody adds below it in v1.9.0. The property is
// the thing isOrphanedClient's own doc comment already states:
//
//	No unreadable /proc entry, at ANY read, may produce a kill.
//
// So this does not enumerate the reads. It enumerates the FIXTURE:
// every file the fake /proc contains, removed one at a time, with the
// parent's entry left in place. A read added later reaches for a file
// that is either already in this walk or has to be added to the
// fixture — and either way it is covered without anyone remembering to
// come back here.
//
// REMOVAL RATHER THAN chmod 000. CI runs these as root inside a
// container, where a 000 file is still readable, so a permissions-based
// fixture would pass locally and cover nothing where it matters.
//
// The parent entry itself is never removed: a vanished parent IS a kill,
// correctly, and TestSweepOrphans_ParentCommUnreadableIsNotAMatch pins
// that direction with a one-file control against this one.
func TestSweepOrphans_NoUnreadableProcFileAuthorisesAKill(t *testing.T) {
	const parent = 700

	build := func(t *testing.T) {
		t.Helper()
		pinSelfComm(t, "net-dhcp")
		ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")
		fakeProc(t, map[int][2]string{
			601:    {clientArgv(ourDir), realKernelComm},
			parent: {"/usr/local/bin/net-dhcp", "net-dhcp"},
		})
		setParent(t, 601, parent)
	}

	// One build to discover the fixture's files. The paths are relative
	// so they survive the rebuild each subtest does under a fresh
	// TempDir.
	build(t)
	var files []string
	root := procRoot
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, rel)
		return nil
	}); err != nil {
		t.Fatalf("walk fixture: %v", err)
	}
	slices.Sort(files)

	// A walk over an empty list is a green test that read nothing.
	if len(files) < 6 {
		t.Fatalf("fixture yielded %d files (%v), want at least 6 — a walk "+
			"over too few files passes without covering the reads", len(files), files)
	}

	// The unmodified fixture must NOT kill, or every case below is
	// green for a reason that has nothing to do with the removals.
	pids, _ := recordKills(t, nil)
	if n, err := SweepOrphans(); err != nil || n != 0 || len(*pids) != 0 {
		t.Fatalf("intact fixture killed %d %v (err %v), want none — the "+
			"walk below cannot mean anything if the baseline kills", n, *pids, err)
	}

	for _, rel := range files {
		t.Run("without "+rel, func(t *testing.T) {
			build(t)
			victim := filepath.Join(procRoot, rel)
			if err := os.Remove(victim); err != nil {
				t.Fatalf("remove %s: %v", rel, err)
			}
			pids, _ := recordKills(t, nil)
			n, err := SweepOrphans()
			if err != nil {
				// A refusal is a legitimate answer to "I cannot see".
				// A kill is not.
				return
			}
			if n != 0 || len(*pids) != 0 {
				t.Errorf("removing %s produced %d kill(s) %v — an unreadable "+
					"/proc entry must never authorise a SIGKILL, and this one "+
					"does it as root against the host PID namespace",
					rel, n, *pids)
			}
		})
	}
}

func TestSweepOrphans_WithoutOurOwnCommRefuses(t *testing.T) {
	pinSelfComm(t, "")

	ourDir := filepath.Join(os.TempDir(), workDirPrefix+"abc123")
	fakeProc(t, map[int][2]string{
		501: {clientArgv(ourDir), realKernelComm},
	})

	pids, _ := recordKills(t, nil)

	n, err := SweepOrphans()
	if err == nil {
		t.Fatal("SweepOrphans returned nil error with no comm of its own; " +
			"an unusable sweep must not report a confident zero")
	}
	if n != 0 || len(*pids) != 0 {
		t.Errorf("killed %d %v, want none", n, *pids)
	}
}

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
