#!/bin/bash
# Entrypoint for the ephemeral CI runner container. tini is PID 1;
# this script brings up a SUPERVISED nested dockerd (the daemon-restart
# integration test depends on the daemon being a restartable child —
# see harness.RestartDockerDaemon and issue #145), seeds the image
# store, then execs the Actions runner with its single-use JIT config.
#
# Modes:
#   (default)   needs RUNNER_JIT_CONFIG; runs exactly one job and exits
#   selftest    infra check without contacting GitHub: daemon up with
#               a real storage driver, seeds loaded, one supervised
#               daemon restart survives
set -euo pipefail

log() { echo "[entrypoint] $*" >&2; }

# --- cgroup v2 nesting prep --------------------------------------------
# We run dockerd bare (no systemd, no docker:dind entrypoint), so without
# this every container process lands directly in the cgroup-namespace root.
# cgroup v2's "no internal processes" rule then forbids that root from being
# a clean domain parent, so the nested daemon is forced to create plugin /
# container cgroups as *threaded*. runc (docker-ce >= 29) tears tasks down
# with cgroup.kill, which the kernel rejects on threaded cgroups with
# EOPNOTSUPP — breaking `docker plugin disable && enable` and any task whose
# teardown needs a kill (issue #158, caught by
# TestRecovery_PluginDisableEnable_PreservesEndpoint on the dhcp-ci runner).
#
# Fix, the same move systemd / docker:dind make: evacuate the root cgroup
# into a leaf so it holds no processes, then delegate every controller to
# child subtrees. The nested daemon's cgroups are then proper *domain*
# cgroups and cgroup.kill works. Idempotent; cgroup v2 only.
prepare_cgroups() {
    [ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)" = "cgroup2fs" ] || return 0
    mkdir -p /sys/fs/cgroup/init
    # Move our own shell into the leaf FIRST: every command we fork below
    # (cat, wc, dockerd...) then inherits the leaf instead of repopulating
    # the root we are trying to empty.
    echo $$ > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
    # Sweep the remaining processes (tini, strays) out of the root. The
    # proc list is a live kernel file, so snapshot it with $(cat) rather
    # than streaming it, and repeat until the root is empty — a single
    # pass can miss PIDs that move/fork mid-sweep. Root must be empty
    # before delegation: a cgroup with member processes can't enable
    # controllers for its children (cgroup v2 "no internal processes").
    for _ in 1 2 3 4 5; do
        moved=0
        for pid in $(cat /sys/fs/cgroup/cgroup.procs 2>/dev/null); do
            echo "$pid" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
            moved=1
        done
        [ "$moved" -eq 0 ] && break
    done
    # Delegate all available controllers down to child cgroups.
    for c in $(cat /sys/fs/cgroup/cgroup.controllers); do
        echo "+$c" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
    done
    log "cgroup prep: root type=$(cat /sys/fs/cgroup/cgroup.type), root procs=$(wc -l < /sys/fs/cgroup/cgroup.procs), subtree=[$(cat /sys/fs/cgroup/cgroup.subtree_control)]"
}
prepare_cgroups

# --- supervised dockerd ------------------------------------------------
# Plain relaunch loop. dockerd must NOT be the container's main
# process: the daemon-restart test SIGTERMs it and expects a fresh
# daemon to appear while the environment stays up.
supervise_dockerd() {
    while :; do
        dockerd >>/var/log/dockerd.log 2>&1 || true
        log "dockerd exited; relaunching in 1s"
        sleep 1
    done
}
supervise_dockerd &

wait_daemon() {
    local deadline=$((SECONDS + 60))
    until docker info >/dev/null 2>&1; do
        if ((SECONDS >= deadline)); then
            log "dockerd did not become ready within 60s; tail of its log:"
            tail -20 /var/log/dockerd.log >&2 || true
            return 1
        fi
        sleep 1
    done
}
wait_daemon

# --- seed images -------------------------------------------------------
# Baked at image build (skopeo); loading locally beats pulling the
# golang builder over the network on every ephemeral job.
for tarball in /seed/*.tar; do
    [ -e "$tarball" ] || break
    docker load -qi "$tarball" || log "seed load failed for $tarball (continuing; job will pull)"
done

# --- selftest ----------------------------------------------------------
if [[ "${1:-}" == "selftest" ]]; then
    # Accept either real overlay driver: classic overlay2 or the
    # containerd snapshotter (reported as "overlayfs", docker-ce 29's
    # default on fresh installs). The check exists to catch the vfs
    # fallback that hits when /var/lib/docker sits on overlayfs.
    driver=$(docker info --format '{{.Driver}}')
    [[ "$driver" == "overlay2" || "$driver" == "overlayfs" ]] \
        || { log "FAIL: storage driver is $driver, want overlay2/overlayfs (vfs = missing volume)"; exit 1; }
    docker image inspect golang:1.25-alpine >/dev/null || { log "FAIL: golang seed missing"; exit 1; }
    docker image inspect alpine:3.20 >/dev/null || { log "FAIL: alpine seed missing"; exit 1; }

    # Host tooling the suites shell out to. The failure-injection suite's
    # L2-reachability check runs `ping` on the runner itself (not in a
    # container) — failure_test.go, TestFailure_LeaseExpiry (#158).
    command -v ping >/dev/null || { log "FAIL: ping missing (failure-injection L2 check needs it on the runner)"; exit 1; }

    # cgroup-nesting guard (#158): prepare_cgroups must have evacuated the
    # namespace-root so the nested daemon makes *domain* (not threaded)
    # cgroups — the precondition for runc's cgroup.kill task teardown. An
    # unprepped root shows "domain threaded" with the container's processes
    # still in it; that is exactly what made plugin disable/enable EOPNOTSUPP.
    if [ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)" = "cgroup2fs" ]; then
        root_type=$(cat /sys/fs/cgroup/cgroup.type 2>/dev/null || echo unknown)
        root_procs=$(wc -l < /sys/fs/cgroup/cgroup.procs)
        [ "$root_type" = "domain" ] && [ "$root_procs" -eq 0 ] \
            || { log "FAIL: cgroup root type='$root_type' procs=$root_procs (want domain/0); prepare_cgroups did not take — plugin task teardown would EOPNOTSUPP (#158)"; exit 1; }
        log "selftest: cgroup posture OK (root=domain, root procs=0)"
    fi

    old_pid=$(cat /var/run/docker.pid)
    log "selftest: SIGTERM dockerd (pid ${old_pid}), expecting supervised relaunch"
    kill "$old_pid"
    deadline=$((SECONDS + 45))
    while :; do
        new_pid=$(cat /var/run/docker.pid 2>/dev/null || true)
        if [[ -n "$new_pid" && "$new_pid" != "$old_pid" ]] && docker info >/dev/null 2>&1; then
            break
        fi
        ((SECONDS >= deadline)) && { log "FAIL: no relaunched daemon within 45s"; exit 1; }
        sleep 1
    done
    docker image inspect golang:1.25-alpine >/dev/null || { log "FAIL: seed lost across restart"; exit 1; }
    log "selftest OK: driver=overlay2, seeds present, supervised restart ${old_pid} -> ${new_pid}"
    exit 0
fi

# --- run exactly one job -----------------------------------------------
: "${RUNNER_JIT_CONFIG:?RUNNER_JIT_CONFIG is required (single-use encoded JIT config; see README)}"
export RUNNER_ALLOW_RUNASROOT=1
cd /opt/runner
log "starting Actions runner (single job, then exit)"
exec ./run.sh --jitconfig "${RUNNER_JIT_CONFIG}"
