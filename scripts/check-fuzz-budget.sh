#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert the CI fuzz budget is expressed in EXECUTIONS, not wall clock
# (#324), and that each fuzz invocation carries a bounding -timeout.
#
# Why this is a gate and not a comment:
#
# `go test -fuzz -fuzztime <duration>` installs a deadline context in
# the fuzzing coordinator. When it expires, the coordinator's select
# wakes on the parent's Done channel and calls stop(ctx.Err()). The
# suppression that is supposed to turn that into a clean stop compares
# the error against the CHILD context's Err() — and context.cancelCtx
# closes its Done channel BEFORE it propagates cancellation to its
# children (context.go, cancelCtx.cancel). Lose that race and the child
# still reads nil, the DeadlineExceeded is not suppressed, and the run
# fails with a bare "context deadline exceeded" and no crashing input.
#
# It is a race in the coordinator's shutdown, not a shortage of CPU for
# the workers. That distinction cost us a wrong fix once already: #350
# capped the worker count on the theory that the coordinator needed
# scheduling headroom, and the same failure came back on dev on
# 2026-08-14 with the runner executing a healthy 22k execs/sec.
#
# An execution budget (`-fuzztime 200000x`) sets opts.Limit instead of
# opts.Timeout, so the deadline context is never created and the racy
# branch is unreachable. It also makes CI fuzz the same AMOUNT on every
# runner rather than however much a contended one manages in 20s.
#
# A wall-clock budget would pass review, read fine, and reintroduce a
# flake that has twice landed on a release PR. Hence a check that goes
# red rather than a paragraph someone has to remember.
#
# Usage: bash scripts/check-fuzz-budget.sh
# Exit:  0 ok, 1 a budget is wall-clock or unbounded, 2 cannot see.

set -uo pipefail

WORKFLOW="${FUZZ_WORKFLOW:-.github/workflows/test.yaml}"

if [ ! -f "$WORKFLOW" ]; then
    echo "check-fuzz-budget: $WORKFLOW does not exist" >&2
    exit 2
fi

# Deliberately a SUPERSET match: every -fuzztime occurrence, whatever
# follows it, including malformed ones. A pattern that only recognised
# well-formed budgets would be blind to exactly the spelling that
# breaks — the lesson from check-version-pins, which matched only valid
# pins and so could not see a broken one for months.
#
# YAML comments are skipped — this file explains the rule in prose right
# above the step it governs, and a comment cannot execute. Everything
# else is judged, well-formed or not.
mapfile -t LINES < <(grep -nE -- '-fuzztime' "$WORKFLOW" | grep -vE '^[0-9]+:[[:space:]]*#')

if [ "${#LINES[@]}" -eq 0 ]; then
    echo "check-fuzz-budget: no -fuzztime found in $WORKFLOW." >&2
    echo "Either the fuzz step was removed (say so deliberately, and delete this gate)" >&2
    echo "or it was renamed/reshaped and this check is now watching nothing." >&2
    exit 2
fi

rc=0
for entry in "${LINES[@]}"; do
    lineno="${entry%%:*}"
    line="${entry#*:}"

    budget=$(printf '%s\n' "$line" | grep -oE -- '-fuzztime[= ]+[^ "'"'"']+' | sed -E 's/^-fuzztime[= ]+//')
    if [ -z "$budget" ]; then
        echo "$WORKFLOW:$lineno: -fuzztime with no value: $(printf '%s' "$line" | sed 's/^ *//')" >&2
        rc=1
        continue
    fi

    for b in $budget; do
        case "$b" in
            *x)
                case "${b%x}" in
                    ''|*[!0-9]*)
                        echo "$WORKFLOW:$lineno: -fuzztime $b is not a valid execution count" >&2
                        rc=1
                        ;;
                esac
                ;;
            *)
                echo "$WORKFLOW:$lineno: -fuzztime $b is a wall-clock budget (#324)." >&2
                echo "  Use an execution count, e.g. -fuzztime 200000x. A duration installs the" >&2
                echo "  deadline context whose shutdown race fails the run with a bare" >&2
                echo "  'context deadline exceeded' and no crashing input." >&2
                rc=1
                ;;
        esac
    done

    # An execution budget is unbounded in wall clock on a pathologically
    # slow runner, so the invocation must still carry a -timeout. That
    # failure is a real signal ("this runner is not fit to fuzz on"),
    # unlike the flake it replaces.
    if ! printf '%s\n' "$line" | grep -E -- '-timeout[= ]+[0-9]' >/dev/null; then
        echo "$WORKFLOW:$lineno: fuzz invocation has no -timeout." >&2
        echo "  An execution budget has no wall-clock ceiling of its own; add one," >&2
        echo "  e.g. -timeout 5m, so a stalled runner fails loudly instead of hanging." >&2
        rc=1
    fi
done

if [ "$rc" -eq 0 ]; then
    echo "check-fuzz-budget: ${#LINES[@]} fuzz invocation(s) use a bounded execution budget"
fi
exit "$rc"
