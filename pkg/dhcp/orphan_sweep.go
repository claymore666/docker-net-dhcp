// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
// WHAT IT MATCHES. Every client's argv carries the absolute path of its
// own work directory, which is created with workDirPrefix — dhcpcd's
// `-f <workdir>/dhcpcd.conf`. That prefix is the marker; nothing else
// on the host writes it. The process's own comm must ALSO be dhcpcd, so
// a process that merely mentions the path (a shell, a grep, an
// unshare that has not exec'd yet) is not a candidate.
//
// PID reuse is real and this is the host PID namespace, so the match is
// re-read immediately before the kill rather than trusted from the
// scan. A pid recycled between the two reads fails the second check and
// is skipped.
func SweepOrphans() (int, error) {
	pids, err := candidatePIDs()
	if err != nil {
		return 0, err
	}

	killed := 0
	for _, pid := range pids {
		// Re-verify. Between the scan and here the pid may have exited
		// and been recycled onto something else entirely, and the thing
		// we are about to signal is chosen by a number we read earlier.
		if !isOrphanedClient(pid) {
			log.WithField("pid", pid).
				Debug("Skipping sweep candidate: no longer a dhcpcd of ours")
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

// candidatePIDs returns the pids under procRoot whose argv carries the
// work-directory marker and whose comm is dhcpcd.
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

// isOrphanedClient reports whether pid is a dhcpcd this plugin family
// spawned. Both halves must hold: the marker says the process was
// started by some instance of this plugin, and comm says the process is
// dhcpcd itself rather than something that merely names the path.
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

	comm, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	return string(bytes.TrimSpace(comm)) == dhcpcdBin
}
