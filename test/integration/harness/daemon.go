// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dockerdPidFile is dockerd's default PID file.
const dockerdPidFile = "/var/run/docker.pid"

// RestartDockerDaemon restarts dockerd gracefully and returns once a new daemon process exists, not once the API answers.
// On a systemd host it runs `systemctl restart docker`; otherwise it SIGTERMs dockerd and relies on the runner's
// supervisor to relaunch it (#145). CI always takes the second branch (#386). Both run Leave on every endpoint, so a
// restarted container comes back through CreateEndpoint, never recoverEndpoints; an abrupt death is #480.
func RestartDockerDaemon(t *testing.T, ctx context.Context) {
	t.Helper()

	if _, err := os.Stat("/run/systemd/system"); err == nil {
		if _, err := exec.LookPath("systemctl"); err == nil {
			t.Log("daemon restart: systemd path (systemctl restart docker)")
			out, err := exec.CommandContext(ctx, "systemctl", "restart", "docker").CombinedOutput()
			if err != nil {
				t.Fatalf("systemctl restart docker: %v\n%s", err, out)
			}
			return
		}
	}

	oldPID, err := dockerdPID()
	if err != nil {
		t.Fatalf("daemon restart: no systemd and no running dockerd found (%v) — "+
			"this environment cannot restart the daemon. Containerized runners "+
			"must supervise dockerd as a restartable child process (issue #145).", err)
	}
	t.Logf("daemon restart: direct path (no systemd) — SIGTERM dockerd pid %d, "+
		"relying on the container's process supervisor to relaunch it", oldPID)

	if err := syscall.Kill(oldPID, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM dockerd (pid %d): %v", oldPID, err)
	}

	// dockerd's graceful shutdown normally takes a few seconds; 15 s absorbs a slow containerd drain.
	exitDeadline := time.Now().Add(15 * time.Second)
	for processAlive(oldPID) {
		if time.Now().After(exitDeadline) {
			t.Fatalf("dockerd (pid %d) still alive 15s after SIGTERM", oldPID)
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			t.Fatalf("daemon restart interrupted: %v", err)
		}
	}

	// The same PID means nothing restarted.
	spawnDeadline := time.Now().Add(30 * time.Second)
	for {
		if newPID, err := dockerdPID(); err == nil {
			if newPID == oldPID {
				t.Fatalf("dockerd PID unchanged (%d) after restart — stale pidfile or nothing actually restarted", oldPID)
			}
			t.Logf("daemon restart: new dockerd pid %d", newPID)
			return
		}
		if time.Now().After(spawnDeadline) {
			t.Fatalf("no new dockerd appeared within 30s of the old one (pid %d) exiting — "+
				"the environment does not supervise dockerd. Containerized runners must "+
				"run dockerd as a restartable child process (issue #145).", oldPID)
		}
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			t.Fatalf("daemon restart interrupted: %v", err)
		}
	}
}

// KillDockerDaemon SIGKILLs dockerd and returns once a supervisor has started a new one. Measured (#480): containerd
// dies with dockerd, the new daemon cannot reattach the orphaned shims and removes each sandbox, and a restart policy
// starts a fresh container with a new MAC and address. The plugin gets a clean SIGTERM about a second later and
// releases every lease, so recovered_ok stays 0. --live-restore keeps the containers and the plugin alive, so recovery
// never runs either. Every container on the host is killed.
func KillDockerDaemon(t *testing.T, ctx context.Context) {
	t.Helper()

	oldPID, err := dockerdPID()
	if err != nil {
		t.Fatalf("abrupt daemon death: no running dockerd found (%v) — this environment "+
			"cannot be tested for it. Containerized runners must supervise dockerd as a "+
			"restartable child process (issue #145).", err)
	}
	t.Logf("abrupt daemon death: SIGKILL dockerd pid %d (no shutdown sequence, no Leave)", oldPID)

	if err := syscall.Kill(oldPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL dockerd (pid %d): %v", oldPID, err)
	}

	// SIGKILL is not catchable; this waits for the kernel to reap the process.
	exitDeadline := time.Now().Add(15 * time.Second)
	for processAlive(oldPID) {
		if time.Now().After(exitDeadline) {
			t.Fatalf("dockerd (pid %d) still alive 15s after SIGKILL — it is not the process "+
				"the pidfile names, or it is unkillable (uninterruptible sleep)", oldPID)
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			t.Fatalf("abrupt daemon death interrupted: %v", err)
		}
	}

	// The same PID means a stale pidfile and no restart.
	spawnDeadline := time.Now().Add(60 * time.Second)
	for {
		if newPID, err := dockerdPID(); err == nil {
			if newPID == oldPID {
				t.Fatalf("dockerd PID unchanged (%d) after SIGKILL — stale pidfile or nothing actually restarted", oldPID)
			}
			t.Logf("abrupt daemon death: new dockerd pid %d", newPID)
			return
		}
		if time.Now().After(spawnDeadline) {
			t.Fatalf("no new dockerd appeared within 60s of killing pid %d — the environment "+
				"does not supervise dockerd (issue #145)", oldPID)
		}
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			t.Fatalf("abrupt daemon death interrupted: %v", err)
		}
	}
}

// DaemonLogCount counts the lines of the running dockerd's stderr that carry every needle, and ok is false when that stderr is not a regular file to read.
// The file is scanned, never read whole: the arm64 runner's dockerd log was 842 MB and 875 MB at the two kills and the
// old whole-file read held about 2.1x of it at once (#1180).
func DaemonLogCount(needles ...string) (n int, path string, ok bool) {
	pid, err := dockerdPID()
	if err != nil {
		return 0, "", false
	}
	fd := "/proc/" + strconv.Itoa(pid) + "/fd/2"
	path, _ = os.Readlink(fd)
	if fi, err := os.Stat(fd); err != nil || !fi.Mode().IsRegular() {
		return 0, path, false
	}
	f, err := os.Open(fd)
	if err != nil {
		return 0, path, false
	}
	defer f.Close()
	n, err = countLinesWithNeedles(f, needles...)
	if err != nil {
		return 0, path, false
	}
	return n, path, true
}

// logScanBuf is the scan buffer of countLinesWithNeedles; a line longer than it arrives in fragments (#1180).
const logScanBuf = 64 << 10

// countLinesWithNeedles counts the non-empty lines of r that carry every needle, holding one buffer and never the input.
// A line longer than the buffer arrives in fragments: each needle is looked for in the fragment and in the last
// len(needle)-1 bytes of the one before, so a needle on a fragment boundary still counts and the line counts once (#1180).
func countLinesWithNeedles(r io.Reader, needles ...string) (int, error) {
	br := bufio.NewReaderSize(r, logScanBuf)
	keep := 0
	pats := make([][]byte, len(needles))
	for i, nd := range needles {
		pats[i] = []byte(nd)
		keep = max(keep, len(nd)-1)
	}
	seen := make([]bool, len(needles))
	missing := len(needles)
	var carry, scratch []byte
	n, lineLen := 0, 0
	endLine := func() {
		if lineLen > 0 && missing == 0 {
			n++
		}
		lineLen, carry, missing = 0, carry[:0], len(needles)
		clear(seen)
	}
	for {
		frag, err := br.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return n, err
		}
		complete := len(frag) > 0 && frag[len(frag)-1] == '\n'
		body := frag
		if complete {
			body = frag[:len(frag)-1]
		}
		lineLen += len(body)
		if missing > 0 && len(body) > 0 {
			win := body
			if len(carry) > 0 {
				scratch = append(append(scratch[:0], carry...), body...)
				win = scratch
			}
			for i, pat := range pats {
				if !seen[i] && bytes.Contains(win, pat) {
					seen[i] = true
					missing--
				}
			}
			carry = append(carry[:0], win[max(0, len(win)-keep):]...)
		}
		if complete {
			endLine()
		}
		if err == io.EOF {
			endLine()
			return n, nil
		}
	}
}

// dockerdPID reads the pidfile, falling back to a /proc comm scan for a non-default --pidfile.
func dockerdPID() (int, error) {
	if b, err := os.ReadFile(dockerdPidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && processAlive(pid) {
			return pid, nil
		}
	}
	matches, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, comm := range matches {
		b, err := os.ReadFile(comm)
		if err != nil || strings.TrimSpace(string(b)) != "dockerd" {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(comm)))
		if err == nil {
			return pid, nil
		}
	}
	return 0, os.ErrProcessDone
}

// processAlive probes pid with signal 0; EPERM counts as alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// sleepCtx sleeps for d or until ctx is done, returning ctx.Err().
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
