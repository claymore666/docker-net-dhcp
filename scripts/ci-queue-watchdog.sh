#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Queue watchdog (#392).
#
# `timeout-minutes` only starts counting once a runner has PICKED UP a
# job. A job nobody picks up has no clock at all, so "no runner
# available" does not surface as a red check — it surfaces as a run that
# looks like it is still working, indefinitely, with the required
# `integration` context stuck on Expected and the PR blocked behind it.
#
# Observed 2026-07-31: a fault on the runner host left ONE runner
# registered instead of the whole pool. One suite job took it, the
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
# It also says WHY nothing picked the job up (#513). Cancelling for a
# capacity shortage produced a red that looked exactly like a suite that
# ran and failed, on a run that had executed none of its own diff —
# "the debian bump broke the suite" is the natural misreading, and the
# only thing separating the two was opening this log. So the class is
# now stated explicitly and emitted as an annotation, a step summary and
# a step output. See classify_wait.
#
# Usage: ci-queue-watchdog.sh <run-id> <stuck-after-seconds> [poll-seconds]
# Env:   GATE_REPO=owner/repo, GH_TOKEN
#        WATCHDOG_POOL_WORKFLOWS=<paths>  which workflows use the pool
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
# The workflows whose jobs land on the self-hosted pool. Comma-separated
# workflow paths, matched against a run's `.path`.
POOL_WORKFLOWS="${WATCHDOG_POOL_WORKFLOWS:-.github/workflows/integration.yml,.github/workflows/coverage.yml}"

if [ -z "$RUN_ID" ] || [ -z "${GATE_REPO:-}" ]; then
    echo "usage: $0 <run-id> <stuck-after-seconds> [poll-seconds]  (needs GATE_REPO)" >&2
    exit 2
fi

if ! command -v curl >/dev/null || ! command -v jq >/dev/null; then
    echo "watchdog: curl or jq missing — cannot judge whether CI is stuck" >&2
    exit 2
fi

# The pool facts this script's operator guidance quotes: the pool size
# and how many jobs one integration run places on it (#879).
#
# NOT literals. Both were literals here until #879, in the two advise()
# messages below, and both were wrong: during a capacity incident the text
# told an operator a pool size and a per-run job count that the pool and
# the matrix had both moved past. A diagnostic that misstates its own
# operands sends the one person reading it in the wrong direction, which is
# worse than a comment that is merely stale.
#
# Deliberately no figures in that paragraph. It carried a pool size and a
# per-run count, written as "was X, when it was really Y" -- and the Y half
# is a claim about the then-current state, so it goes stale exactly like
# the messages it is explaining. Its per-run operand did, inside #879's own
# rebase, in the comment about why literals here are dangerous. For the
# live values, run check-pool-facts.sh --facts.
#
# So they are read from scripts/check-pool-facts.sh, which declares the
# pool size in one tracked file and DERIVES the per-run job count from
# integration.yml itself. If that refuses -- no python3, no PyYAML, no
# checkout -- this returns non-zero and advise() omits the arithmetic and
# says so. It never prints a fallback number: a wrong number here is the
# defect, and "unknown" is the honest answer.
POOL_SIZE=""
JOBS_PER_RUN=""
read_pool_facts() {
    local out
    out=$(bash "$(dirname "$0")/check-pool-facts.sh" --facts 2>/dev/null) || return 1
    POOL_SIZE=$(printf '%s\n' "$out" | sed -n 's/^pool-size=//p')
    JOBS_PER_RUN=$(printf '%s\n' "$out" | sed -n 's/^jobs-per-run=//p')
    case "$POOL_SIZE" in ""|*[!0-9]*) return 1 ;; esac
    case "$JOBS_PER_RUN" in ""|*[!0-9]*) return 1 ;; esac
    [ "$POOL_SIZE" -gt 0 ] && [ "$JOBS_PER_RUN" -gt 0 ] || return 1
    return 0
}

api() {
    curl -sf --max-time 20 \
        -H "Authorization: Bearer ${GH_TOKEN:-}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/${GATE_REPO}/$1"
}

# How hard cancel_run tries. Overridable so the self-tests can exercise
# the retry without sleeping through it; CI never sets either.
CANCEL_ATTEMPTS="${WATCHDOG_CANCEL_ATTEMPTS:-4}"
CANCEL_BACKOFF="${WATCHDOG_CANCEL_BACKOFF:-3}"

# cancel_run stops the run this script is running inside. Returns
# non-zero if the API refused every attempt, which the caller reports —
# a cancel that silently failed would leave the pool held with nobody
# told.
#
# WHY IT RETRIES (#611)
#
# On 2026-08-17 three runs were condemned within 33 seconds and each
# POSTed this same endpoint. The first succeeded; the next two were
# refused, and both stayed queued for 21 hours before executing on a
# verdict a day old, holding their concurrency groups throughout.
#
# Identical token, identical code path, different outcomes — so the
# cause is not configuration. It cannot be the permission the old
# failure message guessed at: the job log's GITHUB_TOKEN Permissions
# group shows `Actions: write`, and a missing scope would have failed
# all three alike. It is transient, and the single attempt this
# replaces had no answer to a transient.
#
# WHY IT RECORDS THE STATUS
#
# `curl -sf ... -o /dev/null` discarded the status code and the body,
# leaving a boolean. That is why the incident above still cannot name
# its own mechanism: 403, 409 and a network timeout were
# indistinguishable after the fact. Dropping -f and keeping %{http_code}
# costs nothing and makes the next occurrence diagnosable, which is the
# only way the retry count above ever gets tuned on evidence.
cancel_run() {
    local attempt=1 code rc
    while :; do
        code=$(curl -s -X POST --max-time 20 \
            -o /dev/null -w '%{http_code}' \
            -H "Authorization: Bearer ${GH_TOKEN:-}" \
            -H "Accept: application/vnd.github+json" \
            "https://api.github.com/repos/${GATE_REPO}/actions/runs/${RUN_ID}/cancel")
        rc=$?

        case "$code" in
            2*) echo "watchdog: cancel accepted (HTTP $code) on attempt $attempt" >&2
                return 0 ;;
        esac

        # Reported per attempt rather than only at the end: if the first
        # POST is refused and a later one lands, that pattern is the
        # evidence that names the mechanism, and it is invisible if only
        # the final verdict is logged.
        echo "watchdog: cancel attempt ${attempt}/${CANCEL_ATTEMPTS} refused (HTTP ${code:-none}, curl exit ${rc})" >&2

        if [ "$attempt" -ge "$CANCEL_ATTEMPTS" ]; then
            CANCEL_LAST_CODE="${code:-none}"
            return 1
        fi
        attempt=$((attempt + 1))
        [ "$CANCEL_BACKOFF" -gt 0 ] && sleep "$CANCEL_BACKOFF"
    done
}

# classify_wait says WHY nothing picked the jobs up, rather than leaving
# the reader to guess (#513).
#
# This is the difference the issue is about. A run cancelled for never
# being assigned presents in the checks UI exactly like a suite that ran
# and failed, and reading the third concurrent PR's red as "the debian
# bump broke the suite" is the natural mistake — the only thing that
# distinguishes them is opening this log. So the log has to say which
# one it is, in a line that reaches the annotation surface.
#
# The three answers need three different responses from a human:
#
#   STARVATION   other privileged runs are holding the pool. Nothing is
#                broken, including the change under test. Re-run when
#                the pool drains.
#   POOL SHORT   nothing else was using the pool and this job was still
#                not picked up: the pool is short, offline, or not
#                assigning. A re-run queues behind the same condition.
#
# It prints "<CLASS>|<detail>". An unreadable answer is its own class,
# never silently one of the above: this file's whole premise is that a
# watchdog which goes quiet when it cannot see is the failure it exists
# to catch.
#
# It asks about competing RUNS rather than about the runner pool, and
# that is a constraint, not a preference: listing self-hosted runners
# needs repo administration rights, which the workflow GITHUB_TOKEN
# cannot be granted at all — `administration` is not one of its
# permission scopes. `actions: read` can see other runs, so the question
# becomes "is anything else holding the pool right now", which is the
# one the reader actually needs answered.
classify_wait() {
    local body others count
    body=$(api "actions/runs?status=in_progress&per_page=50") || {
        echo "POOL UNKNOWN|could not list the other runs in flight, so the cause of the wait is unknown"
        return
    }

    # Only the workflows that put jobs on the self-hosted pool count.
    # A docs build in flight is not why nobody took this job.
    others=$(printf '%s' "$body" | jq -r --arg me "$RUN_ID" --arg wf "$POOL_WORKFLOWS" '
        ($wf | split(",")) as $paths
        | [.workflow_runs[]?
           | . as $r
           | select(($r.id | tostring) != $me)
           | select(($paths | index($r.path)) != null)
           | "#\($r.run_number) \($r.name)"]
        | join(", ")' 2>/dev/null)

    count=$(printf '%s' "$body" | jq -r --arg me "$RUN_ID" --arg wf "$POOL_WORKFLOWS" '
        ($wf | split(",")) as $paths
        | [.workflow_runs[]?
           | . as $r
           | select(($r.id | tostring) != $me)
           | select(($paths | index($r.path)) != null)]
        | length' 2>/dev/null)
    case "$count" in ''|*[!0-9]*)
        echo "POOL UNKNOWN|the run list did not parse, so the cause of the wait is unknown"
        return ;;
    esac

    if [ "$count" -gt 0 ]; then
        echo "STARVATION|${count} other privileged run(s) are holding the pool right now: ${others}"
    else
        echo "POOL SHORT|nothing else is using the pool, and this job was still not picked up"
    fi
}

# advise prints what the reader should DO about each class.
advise() {
    case "$1" in
        STARVATION)
            echo "This red says NOTHING about the change under test. The pool was full of other"
            echo "work and this run never got a runner, so no suite executed. Re-run it once the"
            echo "pool drains; do not go looking for a bug in the diff."
            echo
            if read_pool_facts; then
                fit=$(( POOL_SIZE / JOBS_PER_RUN ))
                rem=$(( POOL_SIZE % JOBS_PER_RUN ))
                echo "Concurrent runs are expected to fit: each integration run puts ${JOBS_PER_RUN} jobs on"
                echo "the pool (the suite matrix; gate, watchdog and the aggregator are hosted), and"
                if [ "$fit" -lt 1 ]; then
                    echo "the pool has only ${POOL_SIZE} runners — fewer than one run needs. No run can start"
                    echo "reliably in this shape; that is a pool problem, not a queue problem (#513)."
                elif [ "$rem" -eq 0 ]; then
                    echo "the pool has ${POOL_SIZE} runners. So ${fit} concurrent runs fit exactly; the next one gets"
                    echo "no runner at all and queues past the budget (#513)."
                else
                    echo "the pool has ${POOL_SIZE} runners. So ${fit} concurrent runs fit; the next one gets a"
                    echo "PARTIAL pickup — ${rem} of its ${JOBS_PER_RUN} jobs assigned and the rest queued past the"
                    echo "budget, which is why a red here can name only some of the shards (#513)."
                fi
            else
                echo "The pool size and the per-run job count could not be derived from this checkout"
                echo "(scripts/check-pool-facts.sh refused), so the fit arithmetic is omitted rather"
                echo "than guessed. Read .github/ci-pool-facts.env and integration.yml by hand (#879)."
            fi ;;
        "POOL SHORT")
            echo "Nothing else was competing for the pool, so this is not capacity — the pool"
            echo "itself is short, offline, or not being assigned. A re-run queues behind the"
            echo "same condition, so check the orchestrator and the runner host first. This is"
            echo "the 2026-07-31 shape, where a host fault left ONE runner registered instead"
            echo "of the whole pool (#392)."
            if read_pool_facts; then
                echo "The pool is contracted at ${POOL_SIZE} runners; count what is actually registered"
                echo "before looking anywhere else."
            else
                echo "The contracted pool size could not be derived from this checkout"
                echo "(scripts/check-pool-facts.sh refused), so it is not quoted here rather than"
                echo "quoted wrongly. Read it from .github/ci-pool-facts.env (#879)."
            fi
            echo
            echo "It says nothing about the change under test either: no suite executed." ;;
        *)
            echo "The runner pool could not be read, so why nothing picked this up is unknown."
            echo "Treat it as unknown rather than as either a capacity problem or a code"
            echo "problem, and check the pool by hand." ;;
    esac
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

        # Ask the pool BEFORE saying anything, so the diagnosis is one
        # complete story and the class can lead it.
        verdict=$(classify_wait)
        class="${verdict%%|*}"
        detail="${verdict#*|}"

        # An annotation, so the reason is visible on the checks page
        # without opening this log. That is the whole point of #513: a
        # capacity red and a product red looked identical there.
        echo "::error title=CI ${class}: this run never got a runner::${detail}. No suite executed, so this result says nothing about the change under test."

        {
            echo
            echo "WATCHDOG: a job has been queued for ${waited}s without a runner picking it up."
            echo
            printf '%s\n' "$queued" | sed 's/^/  still queued: /'
            echo
            echo "  CLASS: ${class} — ${detail}"
            echo
            advise "$class"
            echo
            echo "timeout-minutes does not apply to a job that was never assigned, so this"
            echo "run would otherwise sit pending indefinitely — looking like work in"
            echo "progress, holding the selfhosted-privileged concurrency group, and"
            echo "blocking every other integration run behind it (#392)."
            echo
            echo "Cancelling this run now so it stops holding the concurrency group (#477)."
        } >&2

        # Machine-readable, for a release checklist or a triage query
        # that wants to tell these reds apart in bulk.
        if [ -n "${GITHUB_OUTPUT:-}" ]; then
            {
                echo "wait_class=${class}"
                echo "starved=$([ "$class" = "STARVATION" ] && echo true || echo false)"
            } >> "$GITHUB_OUTPUT"
        fi
        if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
            {
                echo "### ⛔ Run cancelled: ${class}"
                echo
                echo "${detail}."
                echo
                echo "Queued ${waited}s without a runner. **No suite executed**, so this run"
                echo "carries no verdict on the change under test."
                echo
                printf '%s\n' "$queued" | sed 's/^/- still queued: `/; s/$/`/'
            } >> "$GITHUB_STEP_SUMMARY"
        fi

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
                echo "WATCHDOG: the cancel request FAILED after ${CANCEL_ATTEMPTS} attempts, last"
                echo "status HTTP ${CANCEL_LAST_CODE:-none}. The run is still queued and still"
                echo "holding the concurrency group — the condition above is unchanged and now"
                echo "needs a human. Cancel it by hand."
                echo
                echo "Read the status above before guessing: 403 is a permission or a rate"
                echo "limit, 409 means the run was not in a cancellable state, and 000 means"
                echo "the request never got an answer. This message used to assert the job"
                echo "was missing 'actions: write', which sent the one real investigation of"
                echo "this failure down the wrong path — the permission was present (#611)."
            } >&2
        fi
        exit 1
    fi

    sleep "$POLL"
done
