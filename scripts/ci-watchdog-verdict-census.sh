#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Count the CI queue watchdog's verdicts, and go red on the second
# POOL SHORT (#848).
#
# WHAT #848 IS. A suite job was never assigned to a runner WHILE THE
# POOL WAS IDLE -- the watchdog's POOL SHORT branch, not the contention
# branch the two-run concurrency cap addresses. n=1, no reproducer, and
# deliberately no probing: forcing the condition costs pool slots, and
# the pool is the scarce resource. The issue's remedy was "recognise it
# on its next occurrence" -- which, written as prose, is a rule enforced
# by whoever happens to remember it. This is that rule as something that
# fires.
#
# THE INFORMATION WAS ALREADY THERE. #848 says the watchdog
# "distinguishes the two causes and then discards which one fired". It
# does not. `ci-queue-watchdog.sh` emits
# `::error title=CI ${class}: this run never got a runner::...`, and the
# class survives verbatim in the annotation TITLE -- measured on job
# 98518937187, which reads `CI POOL SHORT: this run never got a runner`.
# Nothing needed instrumenting; what was missing was anything that READ
# it. So this adds no counter to the watchdog and changes no CI path.
#
# EVERY ATTEMPT, NOT THE CURRENT CONCLUSION. This is the whole reason
# #848 was wrongly refuted once. A sweep keyed on each run's CURRENT
# conclusion excluded the one run carrying the verdict, because it had
# been re-run mid-investigation -- the filter could not return the
# answer, and an empty result read as a refutation. The evidence sat one
# level down under `attempts/1` the entire time. So this walks
# `runs/<id>/attempts/<n>` for every attempt of every run in the window,
# and filters on NOTHING except the presence of a watchdog job.
#
# VERDICTS ARE NOT INCIDENTS, and conflating them is how this would cry
# wolf on its first real use. Runs 33072714075 and 33072774992 both
# carry POOL SHORT on attempt 1 -- created 12:36:59 and 12:37:44, FORTY-
# FIVE SECONDS apart. Two runs starting together against a pool that is
# briefly not assigning are two observations of ONE event, not two
# occurrences of the fault. A raw verdict count reads that as "it
# recurred" and sends someone diagnosing a recurrence that has not
# happened yet. So verdicts within CLUSTER_SECS of each other collapse
# to one incident, and every verdict is printed with its timestamp so
# the grouping can be checked rather than trusted.
#
# (This also corrects #848's n=1: the second run was already in the data
# and nobody had looked. It remains ONE incident.)
#
# ABSENT IS NOT ZERO. A failed query is a refusal, never a count of nil.
# An unreadable window would otherwise report the safest possible answer
# -- "no POOL SHORT verdicts" -- from an environment that could not see
# any.
#
# Inputs (environment):
#   REPO          owner/name to query (default: the canonical repository)
#   WINDOW_RUNS   how many recent runs of each pool workflow to walk
#   THRESHOLD     POOL SHORT INCIDENTS tolerated in the window (default
#                 1 -- see the clustering note below)
#   CLUSTER_SECS  verdicts closer together than this are one incident
#                 (default 1800)
#   GH            the command used to reach the API. Exists so the
#                 self-test can stub the TRANSPORT rather than the
#                 verdict -- stubbing a classified result would leave
#                 the classification below unexecuted, which is the
#                 defect #827 found in check-attestation-parity.
#
# Exit: 0 POOL SHORT verdicts are at or under the threshold
#       1 the threshold is exceeded -- #848 has recurred and is now
#         worth diagnosing rather than watching
#       2 CANNOT JUDGE -- a query failed, or the window held no
#         watchdog job at all
set -uo pipefail

REPO="${REPO:-claymore666/docker-net-dhcp}"
WINDOW_RUNS="${WINDOW_RUNS:-40}"
THRESHOLD="${THRESHOLD:-1}"
CLUSTER_SECS="${CLUSTER_SECS:-1800}"
GH="${GH:-gh}"
POOL_WORKFLOWS="${WATCHDOG_POOL_WORKFLOWS:-.github/workflows/integration.yml,.github/workflows/coverage.yml}"

refuse() {
    echo "::error title=Watchdog verdict census cannot be judged::$*" >&2
    exit 2
}

api() { # api <path> <jq> -> body on stdout, non-zero on failure
    $GH api "$1" --jq "$2" 2>/dev/null
}

case "$THRESHOLD" in ''|*[!0-9]*) refuse "THRESHOLD is '$THRESHOLD', which is not a number." ;; esac
case "$WINDOW_RUNS" in ''|*[!0-9]*) refuse "WINDOW_RUNS is '$WINDOW_RUNS', which is not a number." ;; esac
case "$CLUSTER_SECS" in ''|*[!0-9]*) refuse "CLUSTER_SECS is '$CLUSTER_SECS', which is not a number." ;; esac

runs=""
IFS=, read -r -a wfs <<< "$POOL_WORKFLOWS"
for wf in "${wfs[@]}"; do
    [ -n "$wf" ] || continue
    ids=$(api "repos/$REPO/actions/workflows/${wf##*/}/runs?per_page=$WINDOW_RUNS" '.workflow_runs[]?.id') \
        || refuse "could not list runs of '$wf'. An unreadable window would otherwise report the safest possible answer from an environment that could not see any."
    runs="$runs $ids"
done

runs=$(printf '%s' "$runs" | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -u)
[ -n "$runs" ] || refuse "no runs of $POOL_WORKFLOWS in the window, so there is nothing to count."

short=0; starve=0; unknown=0; jobs_seen=0
declare -a SHORT_AT=()

while IFS= read -r run; do
    [ -n "$run" ] || continue
    meta=$(api "repos/$REPO/actions/runs/$run" '"\(.run_attempt) \(.created_at) \(.head_branch)"') || continue
    attempts="${meta%% *}"; rest="${meta#* }"; created="${rest%% *}"; branch="${rest#* }"
    case "$attempts" in ''|*[!0-9]*) attempts=1 ;; esac
    for a in $(seq 1 "$attempts"); do
        # EVERY attempt. A re-run does not delete the earlier one's
        # verdict; it only stops the run's current conclusion from
        # reflecting it.
        ids=$(api "repos/$REPO/actions/runs/$run/attempts/$a/jobs" \
                  '.jobs[]? | select(.name=="watchdog") | .id') || continue
        for jid in $ids; do
            jobs_seen=$((jobs_seen + 1))
            titles=$(api "repos/$REPO/check-runs/$jid/annotations" '.[]?.title') || continue
            case "$titles" in
                *"CI POOL SHORT"*)  short=$((short + 1));  SHORT_AT+=("$created  run $run attempt $a  $branch") ;;
                *"CI STARVATION"*)  starve=$((starve + 1)) ;;
                *"CI POOL UNKNOWN"*) unknown=$((unknown + 1)) ;;
            esac
        done
    done
done <<< "$runs"

# NON-VACUITY. "No POOL SHORT verdicts" is true of a window holding no
# watchdog jobs, and that is the strongest possible pass produced by
# having measured nothing.
[ "$jobs_seen" -gt 0 ] || refuse "walked every attempt of $(printf '%s\n' "$runs" | wc -l) run(s) and found NO watchdog job at all. Either the job was renamed -- this greps for a job literally named 'watchdog' -- or the window is older than the watchdog itself. Reporting zero POOL SHORT verdicts from here would be a count of nothing."

# Collapse verdicts closer together than CLUSTER_SECS into one incident.
incidents=0
if [ "$short" -gt 0 ]; then
    incidents=$(printf '%s\n' "${SHORT_AT[@]}" | sort | awk -v gap="$CLUSTER_SECS" '
        {
            t = $1
            gsub(/[-:TZ]/, " ", t)
            split(t, p, " ")
            # days-since-epoch arithmetic is not needed: mktime handles it.
            now = mktime(p[1] " " p[2] " " p[3] " " p[4] " " p[5] " " p[6])
            if (now == -1) { bad = 1; next }
            if (n == 0 || now - last > gap) n++
            last = now
        }
        END { if (bad) print "?" ; else print n }')
fi
case "$incidents" in ''|*[!0-9]*)
    refuse "a POOL SHORT timestamp did not parse, so verdicts could not be grouped into incidents. Counting them raw would report a recurrence that may be one event." ;;
esac

echo "watchdog verdicts across $jobs_seen job(s): POOL SHORT $short (in $incidents incident(s)), STARVATION $starve, POOL UNKNOWN $unknown"
[ "$short" -eq 0 ] || printf '  %s\n' "${SHORT_AT[@]}"

if [ "$incidents" -gt "$THRESHOLD" ]; then
    echo "::error title=The idle-pool non-assignment recurred::POOL SHORT was emitted in $incidents separate incident(s) in this window, over a threshold of $THRESHOLD." \
         "#848 recorded this shape and said a SECOND incident is when it becomes worth diagnosing rather than watching. This is that second incident." \
         "It is NOT the contention shape the two-run cap addresses: the watchdog's own measurement is that nothing else held the pool." \
         "The runner host is not reachable from CI, so host-side diagnosis needs the admins." >&2
    exit 1
fi
exit 0
