//go:build integration

package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dockerdPidFile is where dockerd writes its PID by default. Used by
// the direct restart path to find and then fence the old daemon.
const dockerdPidFile = "/var/run/docker.pid"

// RestartDockerDaemon restarts the Docker daemon in a
// supervisor-agnostic way and blocks until a *new* daemon process
// exists. It does NOT wait for the API to answer — callers poll the
// socket themselves (the daemon-restart test already does), because
// how long "ready" takes is part of what that test measures.
//
// Two environments are supported, detected at runtime:
//
//   - systemd host (bare-metal runner): `systemctl restart docker`.
//     systemctl itself blocks until the unit is started again.
//
//   - containerized runner (no systemd): SIGTERM the running dockerd
//     and rely on the container's process supervisor to relaunch it.
//     This requires the runner image to run dockerd as a *supervised
//     child* — NOT as the container's main process the way stock
//     docker:dind does, where dockerd's exit tears down the whole
//     environment. See issue #145 for the runner-image requirement.
//
// If neither environment is detected, or the supervisor fails to
// produce a new daemon process (PID must change), the test fails
// loudly. There is deliberately no skip path: silently dropping the
// daemon-restart recovery scenario on containerized runners would
// remove coverage of a core recovery path exactly where all CI runs.
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

	// Phase 1: the old daemon must actually exit. dockerd's graceful
	// shutdown (lease releases, plugin teardown) is normally a few
	// seconds; 15s absorbs a slow containerd drain.
	exitDeadline := time.Now().Add(15 * time.Second)
	for processAlive(oldPID) {
		if time.Now().After(exitDeadline) {
			t.Fatalf("dockerd (pid %d) still alive 15s after SIGTERM", oldPID)
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			t.Fatalf("daemon restart interrupted: %v", err)
		}
	}

	// Phase 2: the supervisor must bring up a replacement. A PID equal
	// to the old one means nothing was restarted — fail, don't loop.
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

// dockerdPID locates the running dockerd: pidfile first (authoritative
// when present and alive), /proc comm scan as fallback for daemons
// started with a non-default --pidfile.
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

// processAlive reports whether pid exists (signal 0 probe). EPERM
// counts as alive: the process exists but isn't ours.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// sleepCtx sleeps for d or until ctx is done, returning ctx.Err() in
// the latter case so pollers fail fast on test timeout.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
