// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

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
// WHICH BRANCH CI RUNS (#386): always the containerized one. The
// integration job runs inside a container on every runner, so
// /run/systemd/system is never present. The systemd branch is not
// merely rare here — it is structurally unreachable, and executes only
// on a bare-metal systemd host, i.e. a manual local run.
//
// That asymmetry is smaller than it looks, and the reason belongs next
// to the code rather than in an issue. Both branches shut dockerd down
// GRACEFULLY: systemctl runs the unit's stop sequence, and the direct
// branch SIGTERMs and then waits for the graceful drain below. A
// graceful shutdown runs Leave on the endpoints, so both branches send
// a restarted container back through CreateEndpoint + tombstone rather
// than through recoverEndpoints. They differ in timing, not in which
// plugin path runs — the systemd branch is not hiding a distinct one.
//
// The genuinely uncovered case is an ABRUPT daemon death (SIGKILL,
// crash, power loss), where Leave never runs and recovery has to
// re-adopt a live endpoint. Nothing in the suite does that; it is #480,
// and it is not fixed by this comment.
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

// KillDockerDaemon takes the daemon down ABRUPTLY — SIGKILL, no
// shutdown sequence, no Leave on any endpoint — and blocks until a
// replacement daemon process exists. It is what RestartDockerDaemon
// deliberately is not: the OOM kill, the crash, the hung dockerd killed
// by hand.
//
// It answers to one supervisor rule rather than two branches. systemd
// and the containerized runner's relaunch loop both watch the process
// and both restart it after a SIGKILL, so unlike the graceful path
// there is nothing environment-specific to decide. If no supervisor
// brings a new daemon back, the test fails loudly — there is no skip
// path, for the same reason RestartDockerDaemon has none.
//
// WHAT THIS ACTUALLY EXERCISES, measured rather than assumed (#480).
// It is NOT "the plugin comes back to endpoints still attached":
//
//   - containerd is dockerd's child and dies with it, so the running
//     containers' shims are orphaned. The relaunched daemon cannot
//     reattach to them ("cleaning up dead shim"), removes each sandbox
//     as stale, and any restart policy then starts a FRESH container
//     with a fresh endpoint — new MAC, new address.
//   - the plugin itself never dies abruptly. Roughly a second after the
//     SIGKILL it receives a clean SIGTERM and runs its full shutdown,
//     releasing every lease. That release is asserted by the caller,
//     because it is the property an operator depends on: an abrupt
//     daemon death must not burn a pool address until it expires.
//
// So recovery has nothing to re-adopt here, and recovered_ok stays 0.
// Turning on --live-restore does not open that path either — it keeps
// the container AND the plugin process alive, so recovery never runs at
// all. The two settings are mutually exclusive and neither reaches it.
//
// **Side effects on the runner host**, larger than the graceful path's.
// Every container on the host is killed outright, and only a restart
// policy brings one back.
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

	// SIGKILL is not catchable, so this is the kernel reaping the
	// process, not a drain. Seconds rather than milliseconds only
	// because a large process image takes a moment to tear down.
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

	// The supervisor must produce a replacement. Same PID means nothing
	// restarted and we are reading a stale pidfile — fail, don't loop.
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
