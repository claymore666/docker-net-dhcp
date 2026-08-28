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
# Usage: bash scripts/run-gate-selftests.sh
# Env:   SELFTEST_DIR  directory to discover in (default: the scripts/
#                      directory this file lives in) — the seam the
#                      self-test drives.
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

echo "Discovered ${#tests[@]} gate self-test(s) in $DIR."
failed=()
skipped=()
undelegated=()
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
    echo "::group::${base}"
    if bash "$t"; then
        echo "::endgroup::"
    else
        echo "::endgroup::"
        echo "::error title=Gate self-test failed::${base}" >&2
        failed+=("$t")
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
