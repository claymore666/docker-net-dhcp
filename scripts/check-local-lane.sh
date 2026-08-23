#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The local lane must not test less than CI does (#636).
#
# WHY THIS EXISTS
#
# `scripts/local-lane.sh` lets a developer run the fast CI lane before
# pushing. The moment it lists fewer gates than `test.yaml` runs, it
# starts lying by omission: it exits 0, the developer reads that as
# "green", and the gate that was added last week never ran.
#
# That is #542 one level up. There, 28 hand-maintained `bash
# scripts/test-*.sh` lines had nothing reconciling them against the
# directory, so adding a self-test and forgetting the line meant it
# silently never ran. A local target that hand-lists the workflow's gates
# rebuilds exactly that hole — which is why the lane ships with this
# rather than on its own.
#
# WHAT IT CHECKS
#
#   1. every in-scope script test.yaml invokes is in the lane, or is
#      DECLARED out of lane with a reason
#   2. no declaration is empty, and none contradicts the lane by
#      appearing in both
#   3. no declaration is stale — exempting a script the workflow no
#      longer runs hides that the exemption stopped meaning anything
#   4. no ORPHAN gate: every scripts/check-*.sh is invoked by some
#      workflow, or declared out of lane
#
# (4) closes this gate's own blind direction, and it was found the way
# these things are always found — by writing a gate and forgetting to
# wire it up. Rules 1-3 all start from what test.yaml runs, so a script
# that NO workflow runs is invisible to every one of them: it exists,
# it passes when run by hand, it is in neither list, and nothing
# reports anything. `scripts/check-plugin-set-order.sh` shipped in that
# state, and the only reason it was noticed is that someone grepped for
# it. A guard fails in one direction; name the opposite failure and
# check whether anything covers it.
#
# SCOPE, and why it is drawn here
#
# For rules 1-3: every `scripts/*.sh` invoked from a non-comment `run:`
# line in test.yaml, EXCEPT `test-*.sh`. For rule 4: every
# `scripts/check-*.sh` on disk, judged against EVERY workflow rather
# than test.yaml alone — a gate that runs only in release.yml or on a
# schedule is wired up, just not here. The self-tests are DISCOVERED by
# run-gate-selftests.sh rather than listed, so they are #542's domain and
# already cannot drift; listing them here would mean maintaining the same
# set twice.
#
# Comment lines are excluded deliberately: this file's own prose names
# scripts, and so does test.yaml's. A gate that counted those would
# demand exemptions for scripts nobody runs.
#
# WHAT IT CANNOT DO
#
# It judges membership, not behaviour. A lane entry that invokes a gate
# with different arguments than CI does still passes here. Said plainly
# because a gate described as keeping the lane honest invites the belief
# that the lane is therefore identical.
#
# Usage: check-local-lane.sh [<workflow>] [<lane script>] [<scripts dir>]
# Exit: 0 in sync, 1 drift, 2 cannot check.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

WF="${1:-.github/workflows/test.yaml}"
LANE_SH="${2:-scripts/local-lane.sh}"
SCRIPTS_DIR="${3:-scripts}"

for f in "$WF" "$LANE_SH"; do
    [ -f "$f" ] || { echo "check-local-lane: $f does not exist" >&2; exit 2; }
done

# --- what the workflow actually runs ----------------------------------
invoked=$(grep -vE '^[[:space:]]*#' "$WF" \
    | grep -oE 'scripts/[A-Za-z0-9_.-]+\.sh' \
    | grep -vE '^scripts/test-' \
    | sort -u)

if [ -z "$invoked" ]; then
    # Inspecting nothing is not a pass. If the extraction breaks, this
    # gate would otherwise go green having compared two empty sets.
    echo "::error title=No scripts found::check-local-lane found no script invocations in ${WF}." >&2
    echo "The extraction is broken, or the workflow changed shape. Either way this" >&2
    echo "gate just compared two empty sets, which is not a result." >&2
    exit 2
fi

lane=$(bash "$LANE_SH" --list 2>/dev/null | sort -u)
exempt_raw=$(bash "$LANE_SH" --list-exempt 2>/dev/null)
exempt=$(printf '%s\n' "$exempt_raw" | grep -v '^$' | cut -f1 | sort -u)

if [ -z "$lane" ]; then
    echo "check-local-lane: ${LANE_SH} --list printed nothing — cannot judge coverage." >&2
    exit 2
fi

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

# --- 1. everything invoked is covered ---------------------------------
missing=$(comm -23 <(printf '%s\n' "$invoked") <(printf '%s\n' "$lane" "$exempt" | sort -u))
if [ -n "$missing" ]; then
    note "invoked by ${WF}, but neither in the lane nor declared out of it:"
    printf '  %s\n' $missing >&2
    echo >&2
    echo "  Add it to LANE in ${LANE_SH} so a local run covers it, or to" >&2
    echo "  OUT_OF_LANE with the reason it cannot run on a workstation." >&2
    echo "  Leaving it out means 'make check' silently tests less than CI." >&2
fi

# --- 2. declarations must be real and not self-contradictory ----------
while IFS=$'\t' read -r script reason; do
    [ -n "$script" ] || continue
    if [ -z "${reason// /}" ]; then
        note "${script} is declared out of lane with no reason."
        echo "  'Out of lane' without a reason is indistinguishable from an oversight." >&2
    fi
done <<< "$exempt_raw"

both=$(comm -12 <(printf '%s\n' "$lane") <(printf '%s\n' "$exempt"))
if [ -n "$both" ]; then
    note "declared out of lane AND run by the lane:"
    printf '  %s\n' $both >&2
    echo "  One of the two is wrong; the declaration is what a reader trusts." >&2
fi

# --- 3. no stale declarations -----------------------------------------
stale=$(comm -23 <(printf '%s\n' "$exempt") <(printf '%s\n' "$invoked"))
if [ -n "$stale" ]; then
    note "declared out of lane, but ${WF} does not run it:"
    printf '  %s\n' $stale >&2
    echo "  The exemption has stopped meaning anything. Drop it, or point it at" >&2
    echo "  the workflow that does run the script." >&2
fi

# A lane entry the workflow does not run is not an error — the lane may
# legitimately check more than CI. Say it out loud rather than hiding it,
# so 'the lane mirrors test.yaml' cannot quietly become untrue.
extra=$(comm -23 <(printf '%s\n' "$lane") <(printf '%s\n' "$invoked"))
[ -n "$extra" ] && { echo "NOTE  in the lane but not run by ${WF} (allowed — the lane may check more):"; printf '  %s\n' $extra; }

# --- 4. no orphan gates -----------------------------------------------
# Judged against every workflow, not just $WF: a gate that runs only in
# release.yml or on a schedule is wired up. An orphan is a gate NO
# workflow runs, which is the one state rules 1-3 cannot see, because
# all three start from what a workflow invokes.
# BASENAMES, not paths, so this rule survives being pointed at fixture
# directories by its own self-test — and so a gate invoked as
# `./scripts/check-x.sh` or `bash "$HERE/check-x.sh"` is still seen as
# invoked. What is being asked is "does anything run this file", and
# the prefix a caller happens to write is not part of that question.
WF_DIR="$(dirname "$WF")"
[ -d "$WF_DIR" ] || { echo "check-local-lane: ${WF_DIR} is not a directory — cannot judge orphans." >&2; exit 2; }

all_wf_invoked=$(cat "$WF_DIR"/*.y*ml 2>/dev/null \
    | grep -vE '^[[:space:]]*#' \
    | grep -oE '[A-Za-z0-9_.-]+\.sh' \
    | sort -u)
on_disk=$(find "$SCRIPTS_DIR" -maxdepth 1 -name 'check-*.sh' -printf '%f\n' 2>/dev/null | sort -u)
if [ -z "$on_disk" ]; then
    echo "check-local-lane: no ${SCRIPTS_DIR}/check-*.sh found — cannot judge orphans." >&2
    exit 2
fi
exempt_base=$(printf '%s\n' "$exempt" | sed 's|.*/||' | grep -v '^$' | sort -u)
orphans=$(comm -23 <(printf '%s\n' "$on_disk") \
                   <(printf '%s\n' "$all_wf_invoked" "$exempt_base" | sort -u))
if [ -n "$orphans" ]; then
    note "gate script(s) no workflow runs:"
    printf '  %s\n' $orphans >&2
    echo >&2
    echo "  A gate nothing invokes passes when run by hand and protects nothing." >&2
    echo "  Add it to a workflow, or to OUT_OF_LANE in ${LANE_SH} with the reason" >&2
    echo "  it cannot run in CI. Rules 1-3 above start from what a workflow runs," >&2
    echo "  so they cannot see this state at all." >&2
    echo >&2
    echo "  Being exercised by its own self-test does not count: that is coverage" >&2
    echo "  by accident, and it disappears the moment the self-test is rewritten." >&2
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi

echo "PASS  local lane covers $(printf '%s\n' "$invoked" | grep -c .) script(s) from ${WF}: $(printf '%s\n' "$lane" | grep -c .) run, $(printf '%s\n' "$exempt" | grep -c .) declared out of lane; $(printf '%s\n' "$on_disk" | grep -c .) check-*.sh on disk, none orphaned"
exit 0
