#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# Assert that the fixture capture still runs ON THE INTEGRATION LANE (#644).
#
# WHY THIS EXISTS SEPARATELY FROM THE DRIFT GATE. The two look like they
# cover the same ground and do not. check-fixture-engine-drift.sh asks
# "were these fixtures recorded on the engine I am running on?" — and
# that question is answered CONSISTENTLY, and wrongly, if the capture
# moves to a hosted runner:
#
#   capture on ubuntu-latest  ->  manifest says the hosted engine
#   drift gate on ubuntu-latest ->  compares against the hosted engine
#   ...agreement. Green. And the suite, which runs on the dhcp-ci lane
#   against a different daemon, is replaying a transcript of a daemon it
#   never speaks to — the exact defect #644 was filed about.
#
# The drift gate cannot see this because both halves moved together. A
# guard fails in one direction; this is the other one. The property that
# has to hold is not "the numbers match" but "the capture happened where
# the suite runs", and only the workflow's `runs-on` says that.
#
# WHY IT IS TEXT AND NOT A RUNTIME CHECK. There is no run to inspect at
# gate time, and the failure this prevents is somebody editing the
# workflow — usually for a good reason, like wanting the capture to stop
# occupying a privileged pool slot. The edit is where it must be caught.
#
# WHAT IT ASSERTS, against the workflow with comments stripped so the
# prose above cannot satisfy or trip its own check:
#
#   1. the workflow declares exactly one `runs-on`
#   2. that `runs-on` names both `self-hosted` and the `dhcp-ci` label
#   3. the workflow actually invokes `make capture-fixtures`
#   4. it re-runs the drift gate afterwards, so a capture that took
#      nothing fails there rather than downstream — and that the gate it
#      names EXISTS. Naming a script is not running one: a workflow step
#      referring to a file nobody shipped is the #487 shape, a remedy
#      documented into being without ever being written.
#
# On (1): this workflow has one job today. If it ever grows a second,
# this check goes red rather than quietly passing, and whoever adds the
# job has to decide which job is the capture and teach this script to
# map steps to jobs. That is the intended outcome, not a limitation to
# work around by relaxing the count.
#
# Usage: bash scripts/check-capture-lane.sh [workflow-file]
# Exit: 0 in order, 1 the property is broken, 2 cannot check.
set -u

WF="${1:-.github/workflows/capture-fixtures.yml}"

# Seam for the self-test, which builds synthetic workflows in a temp dir
# and needs the referenced gate to be present or absent on demand.
SCRIPTS_DIR="${SCRIPTS_DIR:-$(dirname "$0")}"
DRIFT_GATE="check-fixture-engine-drift.sh"

if [ ! -r "$WF" ]; then
    echo "FAIL  cannot read capture workflow '$WF'" >&2
    echo "  The fixture capture is what keeps pkg/plugin/testdata/requests" >&2
    echo "  honest about the engine. If it was renamed, point this check at" >&2
    echo "  the new path; if it was deleted, the drift gate now names a" >&2
    echo "  remedy nobody can run." >&2
    exit 2
fi

# Whole-line comments only. YAML comments are where this file's own
# rationale lives, and every literal it forbids appears there.
body=$(grep -vE '^[[:space:]]*#' "$WF")

if [ -z "${body//[[:space:]]/}" ]; then
    echo "FAIL  '$WF' has no content outside comments" >&2
    exit 2
fi

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

# --- 1 + 2. the capture runs on the lane ------------------------------
runs_on=$(printf '%s\n' "$body" | grep -E '^[[:space:]]*runs-on:')
n=$(printf '%s\n' "$runs_on" | grep -c . )

if [ "$n" -eq 0 ]; then
    note "'$WF' declares no runs-on at all."
elif [ "$n" -ne 1 ]; then
    note "'$WF' declares $n runs-on lines; this check assumes the single capture job:"
    printf '  %s\n' "$runs_on" >&2
    echo "  Teach this script which job is the capture before adding another." >&2
else
    if ! printf '%s' "$runs_on" | grep -F 'self-hosted' >/dev/null; then
        note "the capture job is not on a self-hosted runner:${runs_on}"
        echo "  A hosted runner has a different Docker Engine, so the capture" >&2
        echo "  would record a daemon the integration suite never talks to." >&2
    fi
    if ! printf '%s' "$runs_on" | grep -F 'dhcp-ci' >/dev/null; then
        note "the capture job does not request the dhcp-ci label:${runs_on}"
        echo "  That label IS the integration lane — the pool whose nested" >&2
        echo "  daemon the suite replays these fixtures against." >&2
    fi
fi

# --- 3. it still captures ---------------------------------------------
if ! printf '%s\n' "$body" | grep -E 'make[[:space:]]+capture-fixtures' >/dev/null; then
    note "'$WF' never invokes 'make capture-fixtures'."
    echo "  Without it this workflow is a lane reservation that records nothing." >&2
fi

# --- 4. it verifies its own claim -------------------------------------
if ! printf '%s\n' "$body" | grep -F "$DRIFT_GATE" >/dev/null; then
    note "'$WF' does not re-run the drift gate after capturing."
    echo "  The capture can pass having written nothing — capture_one_flow" >&2
    echo "  reports that case and leaves the previous fixtures in place. The" >&2
    echo "  drift gate is what turns it into a red run here instead of a" >&2
    echo "  surprise on somebody else's PR." >&2
elif [ ! -r "${SCRIPTS_DIR}/${DRIFT_GATE}" ]; then
    note "'$WF' runs ${DRIFT_GATE}, which is not in ${SCRIPTS_DIR}."
    echo "  The step would fail at dispatch time, on the one run somebody" >&2
    echo "  reaches for when the fixtures are already known to be stale." >&2
    echo "  A named remedy that does not exist is worse than none: it reads" >&2
    echo "  as covered. Land the gate first, or fix the reference." >&2
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi

echo "PASS  fixture capture runs on the integration lane, captures, and re-checks itself"
