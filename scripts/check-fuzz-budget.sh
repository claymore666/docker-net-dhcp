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
# So every name a fuzz step names is RESOLVED against the package beside
# it (`go test -list`), every target in the tree is required to appear
# in every file that fuzzes, and the tree is required to hold at least
# one target that Scorecard's own pattern can see.
#
# EVERY FILE THAT FUZZES, because there are two. `test.yaml` is what CI
# runs and `scripts/local-lane.sh` is what `make check` runs and what
# docs/internals.md tells a contributor gives the same answer. The first
# version of this gate read only the workflow, so the same dead-name
# failure it was built for stayed live in the copy a developer actually
# runs before pushing: renaming one target in the lane alone left all
# three gates green. One fix does not reach the copies unless the gate's
# domain is the copies.
#
# Usage: bash scripts/check-fuzz-budget.sh
# Exit:  0 ok, 1 a budget is wall-clock or unbounded, or a name resolves
#        to no target, or a target is not smoked; 2 cannot see.

set -uo pipefail

WORKFLOW="${FUZZ_WORKFLOW:-.github/workflows/test.yaml}"
LANE="${FUZZ_LANE:-scripts/local-lane.sh}"
# Seams, so the self-test drives this script rather than a copy of its
# logic: the tree it enumerates and the command that resolves a name.
TREE_ROOT="${FUZZ_TREE_ROOT:-.}"
LIST_CMD="${FUZZ_LIST_CMD:-go test -list}"

SOURCES=("$WORKFLOW" "$LANE")

for src in "${SOURCES[@]}"; do
    if [ ! -f "$src" ]; then
        echo "check-fuzz-budget: $src does not exist" >&2
        exit 2
    fi
done

# shellcheck source=scripts/workflow-shell-lines.sh
. "$(cd "$(dirname "$0")" && pwd)/workflow-shell-lines.sh"

# Deliberately a SUPERSET of budgets: every `go test` that carries a
# -fuzztime is judged, well-formed or not, because a pattern that only
# recognised valid budgets would be blind to the spelling that breaks.
fuzz_cmds() { awk '$1 == "go" && $2 == "test" && / -fuzztime([= ]|$)/'; }

# Only a command counts, never a mention (#883): an echo, a comment or a
# name: once smoked a target here. The workflow is read as the shell its
# steps run; the lane as the command field of each LANE entry, unescaped
# the way bash reads a double-quoted string, since that is what it runs.
lane_cmds() {
    awk '
        /^LANE=\(/ { inl = 1; next }
        inl && /^\)/ { inl = 0; next }
        inl && /^[ \t]*"/ {
            s = $0; sub(/^[ \t]*"/, "", s); out = ""; closed = 0
            for (i = 1; i <= length(s); i++) {
                c = substr(s, i, 1); d = substr(s, i + 1, 1)
                if (c == "\"") { closed = 1; break }
                if (c == "\\" && d != "" && index("$\"\\`", d)) { out = out d; i++ } else out = out c
            }
            if (!closed) next
            n = index(out, "|"); if (!n) next; out = substr(out, n + 1)
            n = index(out, "|"); if (!n) next
            printf "%d\t%s\n", FNR, substr(out, n + 1)
        }' "$1"
}

# workflow_fuzz FILE: lineno:command for each fuzz command its steps run.
# A line is tied to a command only if the joined block ran that command,
# so a line inside a quoted string cannot stand in for one; a command
# split over lines keeps no line number (#883).
workflow_fuzz() {
    local src="$1" ln text c
    local -A avail=()
    while IFS= read -r c; do avail["$c"]=$(( ${avail["$c"]:-0} + 1 )); done \
        < <(workflow_shell_lines --raw "$src" | shell_simple_commands | fuzz_cmds)
    while IFS=: read -r ln text; do
        text="$(printf '%s\n' "$text" | sed -E 's/^[[:space:]]*(-[[:space:]]+)?run:[[:space:]]*//')"
        while IFS= read -r c; do
            [ "${avail["$c"]:-0}" -gt 0 ] || continue
            avail["$c"]=$(( avail["$c"] - 1 ))
            printf '%s:%s\n' "$ln" "$c"
        done < <(printf '%s\n' "$text" | shell_simple_commands | fuzz_cmds)
    done < <(grep -nE -- '-fuzztime' "$src" | grep -vE '^[0-9]+:[[:space:]]*#')
    for c in "${!avail[@]}"; do
        for ((ln = 0; ln < avail["$c"]; ln++)); do printf '%s:%s\n' "-" "$c"; done
    done
}

LINES=()
for src in "${SOURCES[@]}"; do
    if [ "$src" = "$LANE" ]; then
        mapfile -t found < <(lane_cmds "$src" | while IFS=$'\t' read -r ln c; do
            printf '%s\n' "$c" | shell_simple_commands | fuzz_cmds | sed "s/^/$ln:/"
        done)
    else
        mapfile -t found < <(workflow_fuzz "$src")
    fi
    if [ "${#found[@]}" -eq 0 ]; then
        echo "check-fuzz-budget: no go test with -fuzztime runs in $src." >&2
        echo "Either the fuzz step was removed (say so deliberately, and delete this gate)" >&2
        echo "or it was renamed/reshaped and this check is now watching nothing." >&2
        exit 2
    fi
    for f in "${found[@]}"; do
        LINES+=("$src:$f")
    done
done

rc=0
for entry in "${LINES[@]}"; do
    src="${entry%%:*}"
    rest="${entry#*:}"
    lineno="${rest%%:*}"
    line="${rest#*:}"

    budget=$(printf '%s\n' "$line" | grep -oE -- '-fuzztime[= ]+[^ "'"'"']+' | sed -E 's/^-fuzztime[= ]+//')
    if [ -z "$budget" ]; then
        echo "$src:$lineno: -fuzztime with no value: $(printf '%s' "$line" | sed 's/^ *//')" >&2
        rc=1
        continue
    fi

    for b in $budget; do
        case "$b" in
            *x)
                case "${b%x}" in
                    ''|*[!0-9]*)
                        echo "$src:$lineno: -fuzztime $b is not a valid execution count" >&2
                        rc=1
                        ;;
                esac
                ;;
            *)
                echo "$src:$lineno: -fuzztime $b is a wall-clock budget (#324)." >&2
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
        echo "$src:$lineno: fuzz invocation has no -timeout." >&2
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
    src="${entry%%:*}"
    rest="${entry#*:}"
    lineno="${rest%%:*}"
    line="${rest#*:}"

    pkg=$(printf '%s\n' "$line" | grep -oE -- '\./[^ "'"'"']*' | head -1)
    target=$(printf '%s\n' "$line" | grep -oE -- "-fuzz[= ]+['\"]?\^?Fuzz[_[:alnum:]]*" |
        sed -E "s/^-fuzz[= ]+['\"]?\^?//")

    if [ -z "$pkg" ] || [ -z "$target" ]; then
        echo "$src:$lineno: cannot read a package and a literal -fuzz target from this line." >&2
        echo "  Write one invocation per target, with both spelled out, e.g." >&2
        echo "    go test ./pkg/dhcp/ -run '^\$' -fuzz '^FuzzX\$' -fuzztime 200000x -timeout 5m" >&2
        echo "  A name assembled at run time is a name this gate cannot resolve, and an" >&2
        echo "  unresolvable name is how a step comes to fuzz nothing (#1010)." >&2
        rc=1
        continue
    fi

    if ! listed=$($LIST_CMD '^Fuzz' "$pkg" 2>&1); then
        echo "$src:$lineno: could not list fuzz targets in $pkg:" >&2
        printf '%s\n' "$listed" | sed 's/^/    /' >&2
        rc=1
        continue
    fi

    # No -q: a consumer that exits early takes the producer down with
    # SIGPIPE and the pipeline reports failure on success under pipefail
    # (scripts/check-pipefail-consumers.sh). Reading to EOF and dropping
    # the output is the same test with an honest status.
    if ! printf '%s\n' "$listed" | grep -xF -- "$target" > /dev/null; then
        echo "$src:$lineno: -fuzz names $target, and $pkg has no such target." >&2
        echo "  go test would print PASS and exit 0 having fuzzed nothing. Targets there:" >&2
        printf '%s\n' "$listed" | grep -E '^Fuzz' | sed 's/^/    /' >&2
        rc=1
        continue
    fi
    SMOKED["$src|$target"]=1
done

# The other direction, asked of EVERY file that fuzzes rather than of
# their union: a target nobody runs is a target that rots, and a target
# the workflow runs while the lane does not is a local run that reports
# green having covered less than CI.
for src in "${SOURCES[@]}"; do
    for t in "${TREE_TARGETS[@]}"; do
        if [ -z "${SMOKED[$src|$t]:-}" ]; then
            echo "check-fuzz-budget: $t exists in the tree and $src never fuzzes it." >&2
            echo "  Its seed corpus runs under 'go test ./...', which is not the same thing:" >&2
            echo "  seeds cannot find an input nobody has generated yet." >&2
            rc=1
        fi
    done
done

if [ "$rc" -eq 0 ]; then
    echo "check-fuzz-budget: ${#LINES[@]} fuzz invocation(s) across ${#SOURCES[@]} file(s) use a bounded execution budget"
    echo "check-fuzz-budget: ${#TREE_TARGETS[@]} target(s) visible to Scorecard, each smoked by every file that fuzzes"
fi
exit "$rc"
