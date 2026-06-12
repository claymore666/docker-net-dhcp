#!/usr/bin/env bash
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
set -u

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <covdata-percent-output> <baseline-file>" >&2
    exit 2
fi

PERCENT_FILE="$1"
BASELINE_FILE="$2"
EPSILON="${RATCHET_EPSILON:-0.5}"
fail=0

while read -r pkg want; do
    [ -z "$pkg" ] && continue
    case "$pkg" in '#'*) continue ;; esac

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

if [ "$fail" -ne 0 ]; then
    echo
    echo "Coverage ratchet failed. Add tests covering what this change touches;"
    echo "lowering a floor in $BASELINE_FILE requires a recorded decision in the PR."
fi
exit "$fail"
