#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Coverage ratchet (#127): compare per-package coverage against the
# committed baseline. Fails when any baselined package drops more than
# RATCHET_EPSILON points below its floor. The baseline only moves up:
# when a package beats its floor, raise the number in the baseline
# file in the same PR that earned it.
#
# Usage: coverage-ratchet.sh <covdata-percent-output> <baseline-file>
#   <covdata-percent-output>: file containing `go tool covdata percent`
#       output, lines like:
#         github.com/.../pkg/plugin   coverage: 82.4% of statements
#   <baseline-file>: lines of "<package> <min-percent>", '#' comments ok.
#
# RATCHET_EPSILON (default 0.5): tolerated drop in percentage points,
# absorbing run-to-run noise from timing-dependent integration paths.
#
# Exit: 0 every baselined package holds, 1 a package regressed or
# vanished, 2 the ratchet cannot render a verdict.
#
# EXIT 2 IS WHY THIS GATE IS TRUSTWORTHY AT ALL (#734). `coverage` is a
# required context on main and is the one thing between a coverage
# regression and a release, and it used to `exit 0` whenever it compared
# nothing: the loop was `done < "$BASELINE_FILE"` with no `set -e` and no
# post-loop assertion, so a missing baseline file printed a shell error
# to stderr and still reported success. The baseline is 258 lines of
# which 253 are commentary, so a rebase dropping the five data lines,
# a rename, or a relative path resolved from the wrong directory all
# leave a file that still LOOKS populated while the gate enforces
# nothing. Refusing a verdict over empty input is the shape 46 of the
# other 47 gates already use.
set -u

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <covdata-percent-output> <baseline-file>" >&2
    exit 2
fi

PERCENT_FILE="$1"
BASELINE_FILE="$2"
EPSILON="${RATCHET_EPSILON:-0.5}"
fail=0

for f in "$PERCENT_FILE" "$BASELINE_FILE"; do
    if [ ! -f "$f" ] || [ ! -r "$f" ]; then
        echo "::error title=Nothing to inspect::$f is not a readable file." \
             "The ratchet would otherwise report a clean pass having compared nothing." >&2
        exit 2
    fi
done

compared=0

while read -r pkg want; do
    [ -z "$pkg" ] && continue
    case "$pkg" in '#'*) continue ;; esac
    compared=$((compared + 1))

    got=$(awk -v p="$pkg" '$1 == p && $2 == "coverage:" { gsub(/%/, "", $3); print $3; exit }' "$PERCENT_FILE")
    if [ -z "$got" ]; then
        echo "FAIL  $pkg: in baseline but absent from coverage output — deleted/renamed? Update $BASELINE_FILE deliberately."
        fail=1
        continue
    fi

    verdict=$(awk -v got="$got" -v want="$want" -v eps="$EPSILON" 'BEGIN {
        if (got + eps < want)      print "regressed"
        else if (got > want)       print "improved"
        else                       print "held"
    }')
    case "$verdict" in
        regressed)
            echo "FAIL  $pkg: ${got}% is below baseline ${want}% (epsilon ${EPSILON})"
            fail=1
            ;;
        improved)
            echo "PASS  $pkg: ${got}% beats baseline ${want}% — raise the floor in $BASELINE_FILE"
            ;;
        held)
            echo "PASS  $pkg: ${got}% holds baseline ${want}%"
            ;;
    esac
done < "$BASELINE_FILE"

# A baseline that parsed to no comparisons is not a pass. It is the
# gate having read a file and learned nothing from it — see the header.
if [ "$compared" -eq 0 ]; then
    echo "::error title=Nothing to inspect::$BASELINE_FILE holds no <package> <percent> lines." \
         "The ratchet would otherwise report a clean pass having compared nothing." >&2
    exit 2
fi

if [ "$fail" -ne 0 ]; then
    echo
    echo "Coverage ratchet failed. Add tests covering what this change touches;"
    echo "lowering a floor in $BASELINE_FILE requires a recorded decision in the PR."
fi
exit "$fail"
