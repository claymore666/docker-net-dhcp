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
# THE SECOND HALF OF THIS GATE EXISTS BECAUSE THE FIRST HALF CERTIFIED A
# DEAD STEP (#1010). From 1c2e0ae until now the step fuzzed
# FuzzBuildEvent and FuzzEventUnmarshal, two targets deleted with the
# 1.x lease parsers. `go test -fuzz '^FuzzBuildEvent$'` over a package
# with no such target prints PASS and exits 0 — measured, 0.002s — so a
# required check ran for weeks with one possible verdict. This gate saw
# nothing wrong the whole time: it read the -fuzztime value and never
# asked whether the name beside it named anything. A gate keyed on the
# shape of a command cannot see that the command is addressed to
# nobody.
#
# So every name the step fuzzes is RESOLVED against the package it names
# (`go test -list`), every target in the tree is required to appear in
# the step, and the tree is required to hold at least one target that
# Scorecard's own pattern can see.
#
# Usage: bash scripts/check-fuzz-budget.sh
# Exit:  0 ok, 1 a budget is wall-clock or unbounded, or a name resolves
#        to no target, or a target is not smoked; 2 cannot see.

set -uo pipefail

WORKFLOW="${FUZZ_WORKFLOW:-.github/workflows/test.yaml}"
# Seams, so the self-test drives this script rather than a copy of its
# logic: the tree it enumerates and the command that resolves a name.
TREE_ROOT="${FUZZ_TREE_ROOT:-.}"
LIST_CMD="${FUZZ_LIST_CMD:-go test -list}"

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

# Scorecard's Go rule, copied from ossf/scorecard checks/raw/fuzzing.go
# line 53 and applied the way it applies it: per LINE, to files matching
# *_test.go, excluding anything under testdata/ (checks/fileparser/
# listing.go, isMatchingPath and isTestdataFile). Copied rather than
# paraphrased — a paraphrase that drifts would report a check we do not
# pass. The one deviation is the '.' before F, escaped here and literal
# upstream, which only makes this stricter.
SCORECARD_FUZZ_RE='func[[:space:]]+Fuzz[_[:alnum:]]+[[:space:]]*\([_[:alnum:]]+[[:space:]]+\*testing\.F\)'

mapfile -t TREE_TARGETS < <(
    find "$TREE_ROOT" -name '*_test.go' -not -path '*/.git/*' -not -path '*/testdata/*' -print0 |
        xargs -0 -r grep -hoE -- "$SCORECARD_FUZZ_RE" 2>/dev/null |
        sed -E 's/^func[[:space:]]+(Fuzz[_[:alnum:]]+).*/\1/' |
        sort -u
)

if [ "${#TREE_TARGETS[@]}" -eq 0 ]; then
    echo "check-fuzz-budget: no *_test.go under $TREE_ROOT holds a target Scorecard can see." >&2
    echo "  The pattern is ossf/scorecard checks/raw/fuzzing.go:53, matched per line:" >&2
    echo "    func Fuzz<Name>(<arg> *testing.F)" >&2
    echo "  A signature split across two lines, a bare 'Fuzz', a second parameter, or a" >&2
    echo "  file under testdata/ is invisible to it, and the Fuzzing check reads 0." >&2
    exit 2
fi

# Every name the step fuzzes must name a target in the package the same
# command names. A target pattern that is not a literal (a shell
# variable, say) is refused: a name this gate cannot resolve is a name
# nothing resolves until the run is already green.
declare -A SMOKED=()
for entry in "${LINES[@]}"; do
    lineno="${entry%%:*}"
    line="${entry#*:}"

    pkg=$(printf '%s\n' "$line" | grep -oE -- '\./[^ "'"'"']*' | head -1)
    target=$(printf '%s\n' "$line" | grep -oE -- "-fuzz[= ]+['\"]?\^?Fuzz[_[:alnum:]]*" |
        sed -E "s/^-fuzz[= ]+['\"]?\^?//")

    if [ -z "$pkg" ] || [ -z "$target" ]; then
        echo "$WORKFLOW:$lineno: cannot read a package and a literal -fuzz target from this line." >&2
        echo "  Write one invocation per target, with both spelled out, e.g." >&2
        echo "    go test ./pkg/dhcp/ -run '^\$' -fuzz '^FuzzX\$' -fuzztime 200000x -timeout 5m" >&2
        echo "  A name assembled at run time is a name this gate cannot resolve, and an" >&2
        echo "  unresolvable name is how a step comes to fuzz nothing (#1010)." >&2
        rc=1
        continue
    fi

    if ! listed=$($LIST_CMD '^Fuzz' "$pkg" 2>&1); then
        echo "$WORKFLOW:$lineno: could not list fuzz targets in $pkg:" >&2
        printf '%s\n' "$listed" | sed 's/^/    /' >&2
        rc=1
        continue
    fi

    if ! printf '%s\n' "$listed" | grep -qxF -- "$target"; then
        echo "$WORKFLOW:$lineno: -fuzz names $target, and $pkg has no such target." >&2
        echo "  go test would print PASS and exit 0 having fuzzed nothing. Targets there:" >&2
        printf '%s\n' "$listed" | grep -E '^Fuzz' | sed 's/^/    /' >&2
        rc=1
        continue
    fi
    SMOKED["$target"]=1
done

# The other direction: a target nobody runs is a target that rots.
for t in "${TREE_TARGETS[@]}"; do
    if [ -z "${SMOKED[$t]:-}" ]; then
        echo "check-fuzz-budget: $t exists in the tree and $WORKFLOW never fuzzes it." >&2
        echo "  Its seed corpus runs under 'go test ./...', which is not the same thing:" >&2
        echo "  seeds cannot find an input nobody has generated yet." >&2
        rc=1
    fi
done

if [ "$rc" -eq 0 ]; then
    echo "check-fuzz-budget: ${#LINES[@]} fuzz invocation(s) use a bounded execution budget"
    echo "check-fuzz-budget: ${#TREE_TARGETS[@]} target(s) visible to Scorecard, all of them smoked"
fi
exit "$rc"
