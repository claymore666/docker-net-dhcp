#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Coverage-run presence gate (#504).
#
# `coverage` is the one required context on main that is not required on
# dev — it IS the release ratchet. On the v1.5.0 release PR it was not
# red, not queued, not failed: no run existed at all, and `gh pr checks`
# reported it as `pending`, which is indistinguishable from "still
# working". The one gate between a coverage regression and a shipped
# release was skipped, and the symptom was a PR that looked busy.
#
# Mechanism. coverage.yml and integration.yml share a per-ref
# concurrency group with `cancel-in-progress: false`, and GitHub keeps
# exactly ONE pending run per group. A third run entering while one is
# in progress and one is queued EVICTS the queued one — cancels it, does
# not hold it. Measured on the 2026-08-03 release PR: run 30818819921
# (f515532) in progress, 30819508245 (902af9b) cancelled at the moment
# 30820049927 (31a2cb3) was created. Recovery was a manual `gh run
# rerun`.
#
# The evicted run has a signature this gate keys on: conclusion
# `cancelled` with ZERO jobs. Nothing was ever assigned, so no check run
# was created, so the required `coverage` context never appeared on the
# PR — that is the whole of why it reads as pending rather than red. A
# run cancelled AFTER a job started does have a check run, and shows up
# as a cancelled check; that is visible, and this gate lets it pass.
#
# Why detect and not prevent. Splitting coverage into its own
# concurrency group does not fix it: the eviction above was coverage
# evicting coverage on the same ref, so a separate group still keeps one
# pending slot. `cancel-in-progress: true` would fix presence, at the
# cost of letting a new coverage run kill an in-flight integration run
# sharing the group. Auto-requeueing from a `workflow_run` trigger needs
# `actions: write` and can loop when the requeued run is evicted in turn.
# Turning the silence into a red check on the PR is the cheap, bounded
# option, and it is where the release manager is already looking.
#
# Usage: check-coverage-run.sh <head-sha> [wait-minutes]
#   <head-sha>:     the PR head commit the coverage run must cover.
#   [wait-minutes]: how long a run may take to reach a terminal state
#                   before that counts as absent (default 75 — a
#                   coverage RUN is 18m42-36m35, median 22m45, MEASURED
#                   2026-08-28 over the 13 successful runs retained, and
#                   it queues behind the same ref's integration run).
#
# Env: GATE_REPO=owner/repo (default: inferred)
#      GATE_POLL_SECONDS=60
#      GATE_COVERAGE_WORKFLOW=coverage.yml
#
# Exit: 0 a coverage run for this head exists and reached a terminal
#         state (whatever its verdict — presence is what is judged here,
#         the ratchet judges the numbers),
#       1 the run was evicted, never appeared, or never finished,
#       2 cannot check.
#
# NOT fail-open. This exists because a silence was read as health; an
# unreadable API is reported, never treated as clean.
set -uo pipefail

SHA="${1:-}"
WAIT_MIN="${2:-75}"
POLL="${GATE_POLL_SECONDS:-60}"
WF="${GATE_COVERAGE_WORKFLOW:-coverage.yml}"

if [ -z "$SHA" ]; then
    echo "usage: check-coverage-run.sh <head-sha> [wait-minutes]" >&2
    exit 2
fi

if ! command -v gh >/dev/null || ! command -v jq >/dev/null; then
    echo "check-coverage-run: needs gh and jq" >&2
    exit 2
fi

REPO="${GATE_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)}"
[ -n "$REPO" ] || { echo "check-coverage-run: cannot determine the repository (set GATE_REPO)" >&2; exit 2; }

# An Actions annotation puts the verdict on the checks page itself. The
# complaint in #504 is that the truth was only reachable by opening a
# log, so the message has to survive without one.
annotate() {
    [ -n "${GITHUB_ACTIONS:-}" ] && printf '::error title=%s::%s\n' "$1" "$2"
    return 0
}

deadline=$(( $(date -u +%s) + WAIT_MIN * 60 ))
last_error=""
evicted=""

while :; do
    runs=$(gh api "repos/${REPO}/actions/workflows/${WF}/runs?head_sha=${SHA}&per_page=20" \
             --jq '.workflow_runs[] | [.id, .status, (.conclusion // "null")] | @tsv' 2>/dev/null </dev/null)
    if [ $? -ne 0 ]; then
        # Transient API trouble is worth retrying inside the wait, but it
        # must never be the reason this returns clean — see the deadline
        # branch below.
        last_error="could not list ${WF} runs for ${SHA:0:8}"
    else
        last_error=""
        present=""
        pending=0
        evicted=""
        while IFS=$'\t' read -r id status concl; do
            [ -z "$id" ] && continue
            if [ "$status" != "completed" ]; then
                pending=1
                continue
            fi
            if [ "$concl" != "cancelled" ]; then
                present="$id"
                break
            fi
            # Cancelled: the two cases look identical in the run list and
            # mean opposite things. Zero jobs means nothing was ever
            # assigned and no check run exists — the #504 shape. One or
            # more jobs means a check run exists and the PR shows it.
            jobs=$(gh api "repos/${REPO}/actions/runs/${id}/jobs?per_page=1" --jq '.total_count' 2>/dev/null </dev/null)
            case "$jobs" in
                ''|*[!0-9]*)
                    last_error="could not count jobs for run ${id}"
                    pending=1
                    continue
                    ;;
            esac
            if [ "$jobs" -gt 0 ]; then
                present="$id"
                break
            fi
            evicted="${evicted}${evicted:+ }${id}"
        done <<< "$runs"

        if [ -n "$present" ]; then
            echo "check-coverage-run: ${SHA:0:8} has a completed ${WF} run (${present})"
            exit 0
        fi

        # Every run for this head was evicted while pending, and nothing
        # is left that could still produce a check. Waiting cannot change
        # that, so say so now rather than at the deadline.
        if [ -n "$evicted" ] && [ "$pending" -eq 0 ]; then
            break
        fi
    fi

    now=$(date -u +%s)
    [ "$now" -ge "$deadline" ] && break
    remaining=$(( deadline - now ))
    [ "$POLL" -gt 0 ] && sleep "$(( POLL < remaining ? POLL : remaining ))"
    [ "$POLL" -le 0 ] && break
done

if [ -n "$last_error" ]; then
    annotate "Coverage presence unknown" "$last_error"
    cat >&2 <<EOF
check-coverage-run: ${last_error} — cannot judge.

Reporting clean here would reproduce the bug this gate exists for: a
coverage verdict nobody can see, read as health.
EOF
    exit 2
fi

if [ -n "$evicted" ]; then
    first=${evicted%% *}
    annotate "Coverage run evicted" \
        "The coverage run for ${SHA:0:8} was cancelled while queued and created no check. Recover with: gh run rerun ${first}"
    cat >&2 <<EOF

The coverage run for ${SHA:0:8} was EVICTED, not failed.

Run(s) ${evicted} completed as \`cancelled\` having run zero jobs. GitHub
keeps one pending run per concurrency group; a newer run on this ref
displaced this one before any job was assigned. No job means no check
run, which is why the required \`coverage\` context is absent rather than
red — and why \`gh pr checks\` calls it pending.

Nothing will re-queue it. To recover:

    gh run rerun ${first}

\`coverage\` is required on main only: it is the release ratchet. Merging
without it ships a release whose per-package coverage nobody measured.
EOF
    exit 1
fi

annotate "No coverage run" \
    "No ${WF} run for ${SHA:0:8} reached a terminal state within ${WAIT_MIN}m."
cat >&2 <<EOF

No ${WF} run for ${SHA:0:8} reached a terminal state within ${WAIT_MIN}m.

Either the run was never created — Actions can drop pull_request event
delivery while otherwise healthy (#418) — or it is still queued behind
the same ref's integration run for longer than this gate waits.

Check what exists, then re-run or dispatch against this head:

    gh run list --workflow ${WF} --commit ${SHA}
EOF
exit 1
