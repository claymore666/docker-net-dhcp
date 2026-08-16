#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert that an unreadable path under .claude/ does not break the docker
# build context (#530).
#
# The defect this guards: .dockerignore did not exclude .claude/, so the
# whole directory was sent as build context. Per-instance git worktrees
# live under .claude/worktrees/, and a worktree that ever ran
# `sudo make create` leaves a root-owned plugin/rootfs/ behind. From then
# on every unprivileged `docker build` from the repo root died with
#
#     ERROR: failed to solve: error from sender:
#       open .claude/worktrees/<name>/plugin/rootfs/root: permission denied
#
# breaking `make build`, `make create`, `make multiarch` and
# `make push-multiarch` — in a place unrelated to whatever the developer
# had changed. CI never saw it: checkouts are fresh, and the integration
# and coverage jobs run as root, for which the permission does not apply.
#
# This gate asserts the *property* (an unreadable path under .claude/ is
# harmless), not the spelling of the ignore rule. Asserting only that
# .dockerignore contains the string ".claude/" would keep passing after a
# refactor moved the sensitive path somewhere else — the same "the gate
# matched the wrong thing" shape as #487.
#
# The fixture is an unreadable *directory*, not an unreadable file. A
# mode-000 file only fails when its content is actually transferred, which
# BuildKit skips once the layer is cached; a mode-000 directory fails the
# context walk itself, every time.
#
# Step 1 is a negative control against a synthetic context with no ignore
# rule. If the probe stops detecting the defect — a BuildKit change, a
# different builder — this gate would otherwise go quietly green forever
# while proving nothing. A broken probe exits 2, loudly.
#
# Usage:
#   bash scripts/check-build-context.sh            # checks the repo root
#   CONTEXT_DIR=path bash scripts/check-build-context.sh
#
# Requires: docker.

set -uo pipefail

CONTEXT_DIR="${CONTEXT_DIR:-${1:-.}}"

if ! command -v docker >/dev/null 2>&1; then
    echo "docker is required" >&2
    exit 2
fi

# FROM scratch keeps this to a context walk: no base image is pulled and
# nothing is compiled. COPY . / is deliberately the superset of what the
# real Dockerfile copies — it forces the whole context to be walked, in
# every BuildKit version. cacheonly output leaves no image behind.
PROBE_DOCKERFILE=$'FROM scratch\nCOPY . /\n'
probe() {
    printf '%s' "$PROBE_DOCKERFILE" |
        DOCKER_BUILDKIT=1 docker build -o type=cacheonly -f - "$1" 2>&1
}

# plant_fixture DIR -> creates DIR/.claude/<name>/blocked, mode 000.
FIXTURE=""
plant_fixture() {
    FIXTURE="$1/.claude/check-build-context-fixture"
    mkdir -p "$FIXTURE/blocked" || return 1
    : > "$FIXTURE/blocked/file"
    chmod 000 "$FIXTURE/blocked"
}
# The fixture directory is mode 000, so it has to be made traversable
# again before anything can remove it.
drop_fixture() {
    [ -n "${1:-}" ] && [ -d "$1" ] || return 0
    chmod 755 "$1/blocked" 2>/dev/null
    rm -rf "$1"
    # Leave no trace: if we created .claude/ ourselves, drop it again.
    rmdir "$(dirname "$1")" 2>/dev/null
    return 0
}
cleanup() {
    drop_fixture "${CONTROL:-}/.claude/check-build-context-fixture"
    drop_fixture "$FIXTURE"
    [ -n "${CONTROL:-}" ] && rm -rf "$CONTROL"
    return 0
}
trap cleanup EXIT

# --- Step 1: negative control -------------------------------------------
# A synthetic context with no .dockerignore at all. The probe MUST fail
# here; if it does not, the probe is no longer able to see the defect and
# the real assertion below would be meaningless.
CONTROL="$(mktemp -d)"
: > "$CONTROL/go.mod"
plant_fixture "$CONTROL"
control_out="$(probe "$CONTROL")"
control_rc=$?
FIXTURE=""   # cleaned with $CONTROL

if [ "$control_rc" -eq 0 ] || ! grep -q "permission denied" <<<"$control_out"; then
    echo "PROBE BROKEN: an unreadable directory in an un-ignored context did not" >&2
    echo "fail the build, so this gate cannot detect the defect it exists for." >&2
    echo "Fix the probe before trusting a green result here." >&2
    sed 's/^/    /' <<<"$control_out" >&2
    exit 2
fi

# --- Step 2: the assertion ----------------------------------------------
if ! plant_fixture "$CONTEXT_DIR"; then
    echo "could not create the fixture under $CONTEXT_DIR/.claude/" >&2
    exit 2
fi
out="$(probe "$CONTEXT_DIR")"
rc=$?

if [ "$rc" -ne 0 ]; then
    echo "FAIL: an unreadable path under .claude/ breaks the build context." >&2
    echo "Add .claude/ to .dockerignore (#530)." >&2
    sed 's/^/    /' <<<"$out" >&2
    exit 1
fi

echo "OK: .claude/ is excluded from the build context."
