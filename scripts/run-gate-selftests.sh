#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Run every gate self-test, discovered rather than listed (#542).
#
# This replaced 28 hand-maintained `bash scripts/test-...sh` lines in
# the `Gate script self-tests` step of .github/workflows/test.yaml.
# Nothing reconciled that list against the directory, so adding a gate
# with its self-test and forgetting the line meant the self-test simply
# never ran: no error, no warning, and `scripts/` looked fully covered
# to anyone reading it while the gate it protects rotted silently.
#
# That is the same failure shape as every other blind spot this release
# turned up, applied to the machinery that is supposed to catch them.
#
# THE EMPTY-GLOB GUARD IS THE POINT. A discovered list that matches
# nothing is strictly worse than a hand-maintained one: it reports
# success having executed no tests at all. A renamed directory or an
# edited pattern must go red, not green.
#
# Every test runs even after one fails, and the failures are listed
# together at the end. The old list was fail-fast, which meant a commit
# breaking three gates took three CI rounds to diagnose.
#
# THEY RUN CONCURRENTLY, AND THE OUTPUT DOES NOT (D41). MEASURED on
# run 34065502420: this one step was 228s of `policy-gates`' 266s —
# ninety-odd independent bash suites executed one after another on a
# four-vCPU hosted runner. Nothing here shares state: every self-test
# builds its subject in its own `mktemp -d` (the three that do not use
# `$$`-suffixed paths), and none writes into the repository. So the
# EXECUTION is a worker pool, while the REPORTING is still the sorted
# serial walk it was: each test's stdout and stderr are captured to a
# file and replayed, in name order, under the same `::group::` heading,
# with the same verdict lines and the same exit codes. Two logs still
# diff line for line; what changed is only how long it took to produce
# them.
#
# The concurrency is bounded and it must stay bounded: an unbounded
# fan-out over ninety suites on a two-core runner thrashes and comes out
# slower than the serial walk it replaced. The default is `nproc`.
#
# Usage: bash scripts/run-gate-selftests.sh
# Env:   SELFTEST_DIR   directory to discover in (default: the scripts/
#                       directory this file lives in) — the seam the
#                       self-test drives.
#        SELFTEST_JOBS  how many self-tests run at once (default:
#                       `nproc`, floor 1). SELFTEST_JOBS=1 is the serial
#                       walk, and the self-test drives both.
# Exit:  0 all passed, 1 one or more failed, 2 nothing to run.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
DIR="${SELFTEST_DIR:-$HERE}"

if [ ! -d "$DIR" ]; then
    echo "::error title=Gate self-test directory missing::$DIR is not a directory" >&2
    exit 2
fi

shopt -s nullglob
tests=("$DIR"/test-*.sh)

if [ "${#tests[@]}" -eq 0 ]; then
    echo "::error title=No gate self-tests found::$DIR/test-*.sh matched nothing." \
         "This step would otherwise pass having executed no tests at all." >&2
    exit 2
fi

# Sorted, so the run order is the same on every machine and a diff of
# two logs lines up.
mapfile -t tests < <(printf '%s\n' "${tests[@]}" | sort)

# A self-test may legitimately belong to a different job — test-actionlint.sh
# needs the actionlint binary, which only the actionlint job installs, and
# running it here would fail for want of a tool rather than for a defect.
#
# Such a test declares itself with a marker line:
#
#     # gate-selftest-runs-in: <job name>
#
# and is skipped here. The declaration is NOT taken on trust: the runner
# then requires the file to be RUN by some workflow, so "delegated"
# cannot quietly mean "runs nowhere". Skips are printed, never silent —
# an unlisted skip would rebuild the hole this replaced.
#
# "RUN BY" IS NOT "MENTIONED IN", and that distinction was bought at a
# price. This check shipped as `grep -rq -- "$base"` over the whole
# workflow directory, which a COMMENT satisfies. Measured on #872: with
# `run: bash scripts/test-staticcheck-tag-views.sh` deleted from
# test.yaml and only the comment block above it still naming the file,
# this runner exited 0 and printed the test as delegated. A 14-assertion
# suite could be removed from CI with nothing going red.
#
# The mechanism was pre-existing and protects EVERY delegated self-test
# the same way, so it is fixed here rather than filed: the reference now
# has to appear in shell a workflow actually executes, which
# scripts/workflow-shell-lines.sh extracts. A step NAME containing the
# filename does not count either — that is the same defect one door
# along, and it is the one #872's own gate was caught by.
#
# THE BOUNDARY. This asks whether the filename appears in an executed
# line, not whether it is the command's argv[0]. `run: echo
# scripts/test-x.sh` would satisfy it. Narrowing further would mean
# parsing shell, and the failure that cost something was prose, not a
# contrived echo.
WORKFLOWS="${SELFTEST_WORKFLOWS:-$(cd "$HERE/.." && pwd)/.github/workflows}"

# shellcheck source=scripts/workflow-shell-lines.sh
. "$HERE/workflow-shell-lines.sh"

# Extracted once: this runs per delegated test, and re-reading every
# workflow each time would make the cost quadratic in the skip list.
if [ -d "$WORKFLOWS" ]; then
    workflow_shell="$(workflow_shell_lines "$WORKFLOWS")"
else
    workflow_shell=""
fi

jobs="${SELFTEST_JOBS:-$(nproc 2>/dev/null || echo 1)}"
case "$jobs" in
    ''|*[!0-9]*|0) jobs=1 ;;
esac

echo "Discovered ${#tests[@]} gate self-test(s) in $DIR, running up to ${jobs} at a time."
failed=()
skipped=()
undelegated=()

# The delegation walk stays serial and stays FIRST: it runs no test, it
# is a sed and a case glob per file, and it decides which files the pool
# is given. Deciding that inside the pool would mean the skip list
# arrived out of order.
run=()
for t in "${tests[@]}"; do
    base="$(basename "$t")"
    owner=$(sed -n 's/^#[[:space:]]*gate-selftest-runs-in:[[:space:]]*\(.*\)$/\1/p' "$t" | head -1)
    if [ -n "$owner" ]; then
        skipped+=("$base -> $owner")
        # A case glob rather than a pipeline into grep: under pipefail a
        # consumer that exits early kills the producer with SIGPIPE and
        # the pipeline reports failure on success. $workflow_shell is
        # empty when there is no workflow directory, which falls to the
        # same arm — "no workflows" and "no execution" are both
        # "delegated to nowhere".
        case "$workflow_shell" in
            *"$base"*) : ;;
            *) undelegated+=("$base") ;;
        esac
        continue
    fi
    run+=("$t")
done

OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

# One file per test, named by index so the replay order is the discovery
# order regardless of which worker finished first. The exit code is
# written as the last thing the worker does; a worker killed before that
# leaves no .rc file, which the replay reads as a failure rather than as
# a pass — the same direction as every other refusal here.
worker() { # <index> <path>
    local i="$1" t="$2"
    bash "$t" > "$OUT/$i.out" 2>&1
    echo "$?" > "$OUT/$i.rc"
}

running=0
for i in "${!run[@]}"; do
    worker "$i" "${run[$i]}" &
    running=$(( running + 1 ))
    if [ "$running" -ge "$jobs" ]; then
        wait -n 2>/dev/null || true
        running=$(( running - 1 ))
    fi
done
wait

for i in "${!run[@]}"; do
    base="$(basename "${run[$i]}")"
    echo "::group::${base}"
    [ -f "$OUT/$i.out" ] && cat "$OUT/$i.out"
    echo "::endgroup::"
    rc=1
    [ -f "$OUT/$i.rc" ] && rc="$(cat "$OUT/$i.rc")"
    if [ "$rc" -ne 0 ]; then
        echo "::error title=Gate self-test failed::${base}" >&2
        failed+=("${run[$i]}")
    fi
done

if [ "${#skipped[@]}" -ne 0 ]; then
    echo "Delegated to another job (declared in the file itself):"
    printf '  %s\n' "${skipped[@]}"
fi

if [ "${#undelegated[@]}" -ne 0 ]; then
    echo >&2
    echo "::error title=Self-test delegated to nowhere::the following declare" \
         "gate-selftest-runs-in but are not RUN by anything under $WORKFLOWS" \
         "-- a step name or a comment naming the file does not count --" \
         "so they run in no job at all:" >&2
    printf '  %s\n' "${undelegated[@]}" >&2
    exit 1
fi

if [ "${#failed[@]}" -ne 0 ]; then
    echo >&2
    echo "${#failed[@]} gate self-test(s) failed:" >&2
    printf '  %s\n' "${failed[@]}" >&2
    exit 1
fi

ran=$(( ${#tests[@]} - ${#skipped[@]} ))
echo "All ${ran} gate self-test(s) run here passed (${#skipped[@]} delegated)."
