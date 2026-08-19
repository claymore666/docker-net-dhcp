#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# arm64 lane presence gate (#531).
#
# The rc tag is the human gate of a release. Everything an rc proves has
# to follow from that tag on its own — integration-arm64 fires on
# `v*-rc*` rather than waiting for someone to remember `gh workflow run`.
#
# But its suite runs on a self-hosted JIT runner that only exists during
# the rc window. With no runner carrying `dhcp-ci-arm64`, the job does
# not fail: it sits QUEUED, and GitHub holds it there for hours before
# giving up. Queued renders as "in progress", so a lane nobody ever ran
# looks exactly like a lane still working, and the rc reads as clean
# because nothing went red. That is the shape of #504 and #418 a third
# time: absence wearing the costume of patience.
#
# This runs hosted, next to the suite, and turns that silence into a red
# check: if arm64-suite has not STARTED by the deadline, no runner was
# there and the rc has no arm64 verdict.
#
# It judges the start, never the outcome. Once the suite is running its
# own timeout-minutes bounds a hang and its conclusion is the verdict —
# a failing arm64 suite must reach the reader as a failing suite, not as
# this gate's opinion of one.
#
# Usage: check-arm64-lane.sh <run-id> [wait-minutes]
#   <run-id>:       the workflow run whose arm64 job must start.
#   [wait-minutes]: how long a runner may take to pick the job up
#                   (default 25 — minting and launching a JIT runner on
#                   the Pi is a couple of minutes when someone is on it).
#
# Env: GATE_REPO=owner/repo (default: inferred)
#      GATE_POLL_SECONDS=30
#      GATE_ARM64_JOB=arm64-suite
#
# Exit: 0 the job started (or already finished),
#       1 it never started — no arm64 runner was online,
#       2 cannot check.
#
# NOT fail-open. An unreadable API is reported as unreadable; it is
# never allowed to look like a runner that showed up.
set -uo pipefail

RUN_ID="${1:-}"
WAIT_MIN="${2:-25}"
POLL="${GATE_POLL_SECONDS:-30}"
JOB="${GATE_ARM64_JOB:-arm64-suite}"

if [ -z "$RUN_ID" ]; then
    echo "usage: check-arm64-lane.sh <run-id> [wait-minutes]" >&2
    exit 2
fi

if ! command -v gh >/dev/null || ! command -v jq >/dev/null; then
    echo "check-arm64-lane: needs gh and jq" >&2
    exit 2
fi

REPO="${GATE_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)}"
[ -n "$REPO" ] || { echo "check-arm64-lane: cannot determine the repository (set GATE_REPO)" >&2; exit 2; }

# The verdict belongs on the checks page. Someone reading a red rc should
# not have to open a log to learn that the lane never ran.
annotate() {
    [ -n "${GITHUB_ACTIONS:-}" ] && printf '::error title=%s::%s\n' "$1" "$2"
    return 0
}

deadline=$(( $(date -u +%s) + WAIT_MIN * 60 ))
last_error=""

while :; do
    status=$(gh api "repos/${REPO}/actions/runs/${RUN_ID}/jobs?per_page=100" \
               --jq ".jobs[] | select(.name == \"${JOB}\") | .status" 2>/dev/null </dev/null | head -1)
    if [ $? -ne 0 ]; then
        # Retry inside the wait, but never let this be the reason the
        # gate returns clean — see the deadline branch below.
        last_error="could not list jobs for run ${RUN_ID}"
    else
        last_error=""
        case "$status" in
            in_progress|completed)
                echo "check-arm64-lane: ${JOB} started (status: ${status})"
                exit 0
                ;;
            *)
                # queued, waiting, or absent: the job exists in the run
                # but no runner has claimed it, or the run is still
                # materialising its jobs. Both are "not yet", not "no".
                :
                ;;
        esac
    fi

    now=$(date -u +%s)
    [ "$now" -ge "$deadline" ] && break
    sleep "$POLL"
done

if [ -n "$last_error" ]; then
    annotate "arm64 lane unverifiable" "${last_error}; the lane's state is unknown, not clean."
    echo "check-arm64-lane: ${last_error}" >&2
    exit 2
fi

annotate "arm64 lane never started" \
    "${JOB} was still queued after ${WAIT_MIN} minutes: no runner with label dhcp-ci-arm64 was online, so this release candidate has no arm64 verdict."
echo "check-arm64-lane: ${JOB} still queued after ${WAIT_MIN} minutes — no runner with label dhcp-ci-arm64 was online" >&2
exit 1
