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
