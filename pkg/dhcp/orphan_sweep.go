// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
)

// procRoot is /proc, overridable in tests. The plugin runs with
// "pidhost": true (config.json), so this is the HOST's /proc and every
// dhcpcd this plugin has ever spawned is visible in it — including ones
// spawned by a previous, dead instance of the plugin process.
var procRoot = "/proc"

// killProcess is syscall.Kill, overridable in tests. A sweep that
// actually signals is not something a unit test can run.
var killProcess = syscall.Kill

// selfComm is this process's own comm, and it is what tells a LIVE
// client apart from an orphaned one.
//
// The parent of a live persistent client IS the plugin process that
// spawned it, directly: renderArgs always passes dhcpcd -B so it never
// backgrounds itself, and `unshare -m /bin/sh -c '... exec "$0" "$@"'`
// execs rather than forks at every step, so the chain collapses to one
// process (see NewDHCPClient). An ORPHAN, by definition, has lost that
// parent and been reparented.
//
// Comparing the parent's comm against our OWN comm rather than a
// hardcoded binary name is deliberate: a second instance of this plugin
// installed alongside the first — `docker plugin install --alias`, or
// one from each registry while an operator compares versions — runs the
// same binary under the same name, so this recognises its live clients
// without needing to know it exists. It also inherits the kernel's
// 15-byte comm truncation on both sides instead of reproducing it.
//
// Read from the real /proc, never procRoot: this is us, not a scanned
// process, and a test that redirects the scan must not redirect it.
var selfComm = readSelfComm()

func readSelfComm() string {
	b, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(b))
}

// SweepOrphans finds dhcpcd processes left behind by a previous plugin
// process and kills them, returning how many it killed.
//
// WHY THIS EXISTS. The dhcpcd child is not bound to the plugin's
// lifetime: there is no Pdeathsig (see NewDHCPClient) and the plugin shares
// the host PID namespace, so a plugin that dies without running its
// shutdown path — SIGKILL, an OOM kill, a panic that skips Close —
// leaves every persistent client running and renewing inside the
// container's still-live netns. The plugin then restarts,
// recoverEndpoints starts a SECOND client per endpoint with the same
// DUID, IAID and client-id, and two clients manage one binding. On the
// eventual Leave one sends a DHCPRELEASE while the other keeps
// renewing, and the server may reallocate an address that is still in
// use (#722).
//
// WHY SIGKILL, AND NOT SIGTERM. This is the part that is easy to get
// backwards. When the plugin restarts, the containers are still
// RUNNING and still using their addresses. The persistent client omits
// dhcpcd's -p, so it releases its lease when it is asked to stop
// politely — which means a SIGTERM sweep would send a DHCPRELEASE for
// every address a live container currently holds, and invite the
// server to hand those addresses to somebody else. That is #524's
// duplicate assignment, manufactured by the cleanup. SIGKILL leaves the
// binding untouched at the server, so the address stays allocated to
// this host until the replacement client claims it back.
//
// This is the same asymmetry #720 turns on: a missed reclaim leaves a
// lease to expire on its own, a wrong one takes an address away from
// something using it.
//
// WHAT IT MATCHES, IN THREE PARTS. Every client's argv carries the
// absolute path of its own work directory, which is created with
// workDirPrefix — dhcpcd's `-f <workdir>/dhcpcd.conf`. That prefix is
// the marker; nothing else on the host writes it. The process's own
// comm must ALSO be dhcpcd, so a process that merely mentions the path
// (a shell, a grep, an unshare that has not exec'd yet) is not a
// candidate. And its parent must NOT be a live plugin process, which is
// what makes the word "orphaned" in this function's name true rather
// than assumed — see selfComm.
//
// Without that third test the predicate is "is a dhcpcd of ours", and
// every live client of every RUNNING plugin process satisfies it
// identically: same marker in argv, same comm. A second instance
// starting up would then SIGKILL the first instance's live clients, and
// the first instance would never learn — it is not waiting on a signal
// it did not send, its containers keep running, and their leases simply
// stop renewing at T2 with no counter moving anywhere. That is the same
// user-visible outcome this function exists to prevent, caused by it.
// Two instances is a supported configuration (`--alias`), and the work
// directory comes from os.MkdirTemp in the plugin's own private /tmp,
// so the paths are identical STRINGS across instances rather than
// merely similar.
//
// This runs as root against the host PID namespace and sends SIGKILL,
// so every read that fails is a "no". PID reuse is real, so the whole
// match is re-read immediately before the kill rather than trusted from
// the scan: a pid recycled between the two reads fails the second check
// and is skipped.
func SweepOrphans() (int, error) {
	if selfComm == "" {
		// Without our own comm there is no way to tell a live client of
		// another instance from an orphan, and this is not a thing to
		// attempt on a guess: the failure mode is killing a running
		// plugin's clients. Refuse, and let the caller say so — an
		// unusable sweep must not report a confident zero.
		return 0, fmt.Errorf("cannot read /proc/self/comm, so a live dhcpcd cannot be told from an orphaned one")
	}

	pids, err := candidatePIDs()
	if err != nil {
		return 0, err
	}

	killed := 0
	for _, pid := range pids {
		// Deliberate re-validation, not the primary filter — candidatePIDs
		// has already applied the same predicate. Between that scan and
		// here the pid may have exited and been recycled onto something
		// else entirely, and the thing we are about to signal is chosen
		// by a number we read earlier. This narrows the window to one
		// call.
		if !isOrphanedClient(pid) {
			log.WithField("pid", pid).
				Debug("Skipping sweep candidate: it is no longer an orphaned dhcpcd of ours")
			continue
		}
		if err := killProcess(pid, syscall.SIGKILL); err != nil {
			// ESRCH means it exited on its own between the two reads,
			// which is the outcome we wanted anyway. Anything else is
			// worth seeing: it is the case where a client survives the
			// sweep and recovery is about to start a second one.
			if err == syscall.ESRCH {
				continue
			}
			log.WithError(err).WithField("pid", pid).
				Warn("Could not kill orphaned dhcpcd; recovery may start a second client for its endpoint")
			continue
		}
		killed++
		log.WithField("pid", pid).
			Info("Killed a dhcpcd left behind by a previous plugin process")
	}
	return killed, nil
}

// candidatePIDs returns the pids under procRoot that isOrphanedClient
// accepts: our work-directory marker in argv, comm of dhcpcd, and no
// live plugin process as a parent.
func candidatePIDs() ([]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", procRoot, err)
	}

	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			// Not a pid directory. /proc carries plenty of those.
			continue
		}
		if pid == os.Getpid() {
			continue
		}
		if isOrphanedClient(pid) {
			out = append(out, pid)
		}
	}
	return out, nil
}

// isOrphanedClient reports whether pid is a dhcpcd that some instance of
// this plugin spawned AND whose plugin process is gone. All three parts
// must hold: the marker says it was started by some instance of this
// plugin, comm says it is dhcpcd itself rather than something that
// merely names the path, and the parent says nobody is still managing
// it.
//
// A read that fails is a "no". The process may have exited mid-scan, or
// be one this plugin has no business signalling; neither is an error to
// report, and treating an unreadable /proc entry as a match would make
// the sweep kill on absence of evidence.
func isOrphanedClient(pid int) bool {
	cmdline, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	// /proc/<pid>/cmdline is NUL-separated, so a plain substring search
	// over the whole blob is enough — the marker cannot straddle two
	// arguments, because it contains no NUL.
	if !bytes.Contains(cmdline, []byte(workDirPrefix)) {
		return false
	}

	if commOf(pid) != dhcpcdBin {
		return false
	}

	ppid, ok := parentPID(pid)
	if !ok {
		// We cannot tell whether anything is still managing it, and the
		// question this answers is whether to SIGKILL. Unknown is "no".
		return false
	}
	if ppid == os.Getpid() {
		// Our own, and alive. Reachable only if a caller sweeps after
		// starting clients; NewPlugin sweeps before recovery starts any,
		// so this guards a future second call site rather than a case
		// that arises today.
		return false
	}
	// A parent running the same binary as us is another live instance of
	// this plugin, and its clients are not ours to kill. Anything else —
	// init, a subreaper, a shim — means the plugin that spawned this
	// client is gone.
	//
	// Deliberately not `ppid == 1`: an orphan reparents to the nearest
	// subreaper, which under a container runtime is often the shim
	// rather than init, so testing for 1 would MISS real orphans and
	// leave the duplicate-client bug in place on exactly the hosts this
	// plugin runs on.
	return commOf(ppid) != selfComm
}

// commOf returns a process's comm, or "" if it cannot be read.
func commOf(pid int) string {
	comm, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(comm))
}

// parentPID reads a process's parent pid, reporting whether it could.
//
// From /proc/<pid>/status rather than /proc/<pid>/stat on purpose.
// stat's second field is the comm, in parentheses, and a comm may
// contain spaces and ')' — so splitting stat on whitespace to reach
// field 4 is a parsing trap that surfaces only on a process somebody
// named awkwardly. Here that would mean either killing the wrong thing
// or missing an orphan, decided by another process's name.
func parentPID(pid int) (int, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, found := strings.CutPrefix(line, "PPid:")
		if !found {
			continue
		}
		ppid, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return 0, false
		}
		return ppid, true
	}
	return 0, false
}
