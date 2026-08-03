#!/usr/bin/env bash
# Queue watchdog (#392).
#
# `timeout-minutes` only starts counting once a runner has PICKED UP a
# job. A job nobody picks up has no clock at all, so "no runner
# available" does not surface as a red check — it surfaces as a run that
# looks like it is still working, indefinitely, with the required
# `integration` context stuck on Expected and the PR blocked behind it.
#
# Observed 2026-07-31: a fault on the runner host left one runner
# registered instead of the contracted eight. One suite job took it, the
# other queued at 15:51 and was still queued when a human cancelled it
# by hand eleven minutes later. Nothing timed out, nothing went red, no
# alert fired. Had nobody looked it would still be pending, holding the
# selfhosted-privileged concurrency group and blocking every other
# integration run behind it.
#
# It CANCELS the run it condemns (#477). Reporting alone left the
# original harm untouched: the queued jobs went on holding the
# selfhosted-privileged concurrency group for as long as they would have
# without a watchdog at all. Measured on 2026-08-02 — two runs opened
# after the runner pool was stood down for the night went red at the
# 10-minute budget and then sat queued until 06:12 the next morning,
# 7h08m, exactly the state the message above says must not happen. A
# watchdog that names a harm and does not act on it is a comment.
#
# Cancelling also routes the verdict into branch protection. `watchdog`
# is not a required context and the required `integration` aggregator
# does not depend on it, so its opinion had no path to the merge button;
# a cancelled run drives the suite rollup the aggregator DOES read.
#
# On a PARTIAL pickup — the 2026-07-31 incident, one runner assigned and
# the rest queued — this cancels the assigned job too. Deliberate. The
# run cannot complete, so the shard that did start produces no verdict
# anyone can act on: the aggregator fails on a non-success rollup either
# way. The choice is between losing that shard's work and holding the
# pool hostage, and the pool is worth more than a result nothing will
# read.
#
# Usage: ci-queue-watchdog.sh <run-id> <stuck-after-seconds> [poll-seconds]
# Env:   GATE_REPO=owner/repo, GH_TOKEN
#        WATCHDOG_NO_CANCEL=1 diagnoses without cancelling (tests only)
#
# Needs `actions: write` on the calling job for the cancel; without it
# the POST 403s, which is reported rather than swallowed.
#
# Exit: 0 nothing stuck (every job started or the run finished),
#       1 a job sat queued past the budget,
#       2 cannot check.
#
# Exit 1 is best-effort by construction: cancelling the run cancels the
# job running this script, so the process is often killed before it
# returns. That is why the whole diagnosis is printed BEFORE the cancel
# is issued — the log has to be complete at the moment the axe falls, or
# the operator inherits a cancelled run with no stated reason.
#
# NOT fail-open, unlike the suite-needed gate. That one decides whether
# to spend a runner and the expensive answer is the safe one. This one
# decides whether CI is lying about a run being alive, and a watchdog
# that goes quiet when it cannot see is the failure it exists to catch.
set -uo pipefail

RUN_ID="${1:-}"
BUDGET="${2:-600}"
POLL="${3:-30}"

if [ -z "$RUN_ID" ] || [ -z "${GATE_REPO:-}" ]; then
    echo "usage: $0 <run-id> <stuck-after-seconds> [poll-seconds]  (needs GATE_REPO)" >&2
    exit 2
fi

if ! command -v curl >/dev/null || ! command -v jq >/dev/null; then
    echo "watchdog: curl or jq missing — cannot judge whether CI is stuck" >&2
    exit 2
fi

api() {
    curl -sf --max-time 20 \
        -H "Authorization: Bearer ${GH_TOKEN:-}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/${GATE_REPO}/$1"
}

# cancel_run stops the run this script is running inside. Returns
# non-zero if the API refused, which the caller reports — a cancel that
# silently failed would leave the pool held with nobody told.
cancel_run() {
    curl -sf -X POST --max-time 20 \
        -o /dev/null \
        -H "Authorization: Bearer ${GH_TOKEN:-}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/${GATE_REPO}/actions/runs/${RUN_ID}/cancel"
}

# queued_names: prints the names of jobs currently in `queued`, one per
# line. A job this workflow's own watchdog runs in is excluded — it is
# obviously running, and including it would let the watchdog report
# itself.
queued_names() {
    printf '%s' "$1" | jq -r '
        [.jobs[]? | select(.status == "queued") | .name]
        | .[]' 2>/dev/null
}

deadline=$(( $(date +%s) + BUDGET ))
stuck_since=""

while :; do
    body=$(api "actions/runs/${RUN_ID}/jobs?per_page=100") || {
        echo "watchdog: could not read jobs for run ${RUN_ID}" >&2
        exit 2
    }

    status=$(printf '%s' "$body" | jq -r '[.jobs[]?.status] | unique | join(",")' 2>/dev/null)
    queued=$(queued_names "$body")

    if [ -z "$queued" ]; then
        # Nothing waiting. Either everything is running or the run is
        # over; both are fine and neither is this watchdog's business.
        echo "watchdog: no job is queued (job states: ${status:-none})"
        exit 0
    fi

    now=$(date +%s)
    [ -z "$stuck_since" ] && stuck_since="$now"

    if [ "$now" -ge "$deadline" ]; then
        waited=$(( now - stuck_since ))
        {
            echo
            echo "WATCHDOG: a job has been queued for ${waited}s without a runner picking it up."
            echo
            printf '%s\n' "$queued" | sed 's/^/  still queued: /'
            echo
            echo "timeout-minutes does not apply to a job that was never assigned, so this"
            echo "run would otherwise sit pending indefinitely — looking like work in"
            echo "progress, holding the selfhosted-privileged concurrency group, and"
            echo "blocking every other integration run behind it (#392)."
            echo
            echo "Most likely the self-hosted pool is short of runners. Check the pool"
            echo "before re-running: a re-run queues behind the same shortage."
            echo
            echo "Cancelling this run now so it stops holding the concurrency group (#477)."
        } >&2

        # Everything the operator needs is on stderr already. From here
        # the process may be killed at any moment by its own request.
        if [ -n "${WATCHDOG_NO_CANCEL:-}" ]; then
            echo "watchdog: WATCHDOG_NO_CANCEL set — diagnosed only, run left alone" >&2
            exit 1
        fi

        if cancel_run; then
            echo "watchdog: cancel requested for run ${RUN_ID}" >&2
        else
            {
                echo
                echo "WATCHDOG: the cancel request FAILED. The run is still queued and still"
                echo "holding the concurrency group — the condition above is unchanged and now"
                echo "needs a human. Cancel it by hand, then check that this job has"
                echo "'actions: write': without it the API refuses with 403."
            } >&2
        fi
        exit 1
    fi

    sleep "$POLL"
done
