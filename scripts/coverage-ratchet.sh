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
#
# THAT GUARD CATCHES ZERO, NOT INCOMPLETENESS (#791). `compared` is
# incremented once per data line and the loop iterates OVER the baseline,
# so `compared` is by construction the number of data lines in the file
# it was handed. The guard can therefore only ever fire on the empty
# case. A baseline arriving with two of its five packages -- a bad
# rebase, a truncated blob, a partial fetch, a merge that dropped lines
# -- compares two, prints two PASS lines and exits 0, and the release
# reads that as a clean ratchet. The 253 commentary lines are what make
# it invisible: the file still looks populated.
#
# The count cannot come from inside this script. Nothing here has a
# second opinion about what a complete baseline is, and deriving the
# expected number from the same file is a measurement backstopping
# itself. It comes from coverage-baseline-at.sh, which resolved the blob
# and is the one place that knows what it handed over. Set
# RATCHET_REPORT to that report and this asserts it compared exactly the
# packages named in it -- by NAME, because "2 of 5" sends someone to read
# a 258-line file and a list of names sends them to two lines.
#
# A run with no report is NOT silently exempt. It says so, loudly, in
# the one line a reader of a release log would otherwise take as a clean
# verdict -- because an unannounced absence of a cross-check is exactly
# the shape this gate keeps being caught by.
set -u

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <covdata-percent-output> <baseline-file>" >&2
    exit 2
fi

PERCENT_FILE="$1"
BASELINE_FILE="$2"
EPSILON="${RATCHET_EPSILON:-0.5}"
# Default to the sidecar coverage-baseline-at.sh writes beside the blob,
# so the release path is cross-checked without a second wiring step that
# could be forgotten. An explicit RATCHET_REPORT overrides; RATCHET_REPORT=
# (empty) is how a caller says "there is no resolver here", and that
# still prints the NOT CROSS-CHECKED line rather than passing quietly.
REPORT="${RATCHET_REPORT-$2.report}"
fail=0

for f in "$PERCENT_FILE" "$BASELINE_FILE"; do
    if [ ! -f "$f" ] || [ ! -r "$f" ]; then
        echo "::error title=Nothing to inspect::$f is not a readable file." \
             "The ratchet would otherwise report a clean pass having compared nothing." >&2
        exit 2
    fi
done

compared=0
compared_pkgs=""

while read -r pkg want; do
    [ -z "$pkg" ] && continue
    case "$pkg" in '#'*) continue ;; esac
    compared=$((compared + 1))
    compared_pkgs="${compared_pkgs}${pkg}
"

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

# --- the completeness cross-check (#791) ---------------------------------
if [ -z "$REPORT" ] || [ ! -f "$REPORT" ]; then
    echo
    echo "NOT CROSS-CHECKED: no resolver report${REPORT:+ at $REPORT}. This run compared" \
         "${compared} package(s) and CANNOT TELL whether that is all of them —" \
         "the count comes from the baseline it was handed, so a truncated baseline" \
         "agrees with itself. On the release path coverage-baseline-at.sh writes the" \
         "report beside the blob; if you are seeing this there, that step did not run."
else
    want=$(sed -n 's/^count //p' "$REPORT" | head -1)
    # A case glob, not `printf | grep -q`: a piped `grep -q` exits at its
    # first match and SIGPIPEs the producer, so under pipefail the
    # pipeline can report failure on success (scripts/check-pipefail-
    # consumers.sh). No subprocess is needed to ask whether a string is
    # digits.
    case "$want" in
        ''|*[!0-9]*) want_bad=1 ;;
        *)           want_bad=0 ;;
    esac
    if [ "$want_bad" -eq 1 ]; then
        echo "::error title=Unreadable resolver report::$REPORT carries no 'count <n>' line" \
             "(got '${want:0:40}'). The cross-check cannot be made, and a run that cannot" \
             "verify its own completeness must not report a clean ratchet." >&2
        exit 2
    fi

    # Compared BY NAME, not by count alone. Two files can hold the same
    # number of packages and not the same packages -- a substitution
    # keeps the count and changes the verdict, which a count check reads
    # as complete.
    sed -n 's/^package //p' "$REPORT" | sort -u > "$BASELINE_FILE.want.$$"
    printf '%s\n' "$compared_pkgs" | grep . | sort -u > "$BASELINE_FILE.got.$$"
    missing=$(comm -23 "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$" | paste -sd', ' -)
    extra=$(comm -13 "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$" | paste -sd', ' -)
    rm -f "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$"

    if [ "$compared" -ne "$want" ] || [ -n "$missing" ] || [ -n "$extra" ]; then
        echo "::error title=The baseline is incomplete::the resolver handed over ${want} package floor(s)" \
             "and this run compared ${compared}.${missing:+ NOT COMPARED: ${missing}.}${extra:+ COMPARED BUT NOT RESOLVED: ${extra}.}" \
             "A baseline that lost lines between resolution and comparison still compares what survived and" \
             "reports every one of them as holding, which is a clean ratchet over a floor that is no longer there." >&2
        exit 2
    fi
    echo
    echo "Cross-checked: compared ${compared} of ${want} resolved package floor(s)$(sed -n 's/^blob /, blob /p' "$REPORT" | head -1)."
fi

if [ "$fail" -ne 0 ]; then
    echo
    echo "Coverage ratchet failed. Add tests covering what this change touches;"
    echo "lowering a floor in $BASELINE_FILE requires a recorded decision in the PR."
fi
exit "$fail"
