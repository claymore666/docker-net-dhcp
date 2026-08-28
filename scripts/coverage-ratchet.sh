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
#
# AND NEITHER IS A LINE THAT LOST ITS FLOOR (#791). The cross-check above
# compares NAMES and COUNTS; a data line whose percentage is gone is
# present on both sides, so all three of resolver, cross-check and ratchet
# attest "5 of 5" while one package has no floor at all -- awk reads the
# empty field as 0 and every percentage beats it. The floor is therefore
# validated where it is read, and an unreadable one refuses the run.
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
floor_bad=0

while read -r pkg want; do
    [ -z "$pkg" ] && continue
    case "$pkg" in '#'*) continue ;; esac
    compared=$((compared + 1))
    compared_pkgs="${compared_pkgs}${pkg}
"

    # A FLOOR THAT LOST ITS NUMBER IS NOT A FLOOR (#791). `read -r pkg want`
    # leaves `want` EMPTY when a line carries only a package name, and awk
    # evaluates an empty string as numeric 0 -- so `got > want` is true for
    # every percentage and the package prints
    #
    #     PASS  .../pkg/plugin: 0.1% beats baseline % -- raise the floor
    #
    # while nothing is enforced. Two of the four causes named in the header
    # (a truncated blob, a partial fetch) damage a line rather than delete
    # it, and the completeness cross-check below cannot see this at all: the
    # resolver counts a floor-less line too, so resolver, cross-check and
    # ratchet ALL AGREE on "compared 5 of 5" with one of the five gone. An
    # attestation over a missing floor is worse than no attestation.
    #
    # This is the third zero-shape defect in this repo's gates. An unset or
    # unparseable numeric field is 0, never a sentinel -- and 0 on the
    # RIGHT-HAND side of the comparison is the fail-OPEN direction, which is
    # why this one had to be validated and the left-hand `got` did not: a
    # garbage `got` reads as 0% and fails the package closed.
    #
    # A case glob, not a subprocess, for the reason given at the count check
    # below. Accepts an integer or one-decimal-point floor; rejects empty,
    # a bare `.`, two dots, and anything carrying a non-digit.
    case "$want" in
        ''|.|*[!0-9.]*|*.*.*) want_bad=1 ;;
        *)                    want_bad=0 ;;
    esac

    # RECORD AND CONTINUE, refuse after the loop. A merge that damaged one
    # data line has usually damaged more than one, and a gate that names
    # the first and stops makes the next person fix them one round trip at
    # a time. Every damaged line is named in one run; the verdict is still
    # a refusal, and it still outranks a regression found further down.
    if [ "$want_bad" -eq 1 ]; then
        echo "::error title=Unreadable baseline floor::$BASELINE_FILE gives $pkg no readable floor" \
             "(got '${want:0:40}'). An empty or unparseable floor is numeric 0 to awk, so every" \
             "percentage beats it and the package reports PASS while no floor is enforced." \
             "The completeness cross-check cannot see this: it counts the line, which is present." >&2
        floor_bad=1
        continue
    fi

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

# A line that lost its floor is not a verdict either. Refused HERE and not
# at the point of detection so that one run names every damaged line, and
# ahead of every other post-loop check because "this file does not say what
# the floors are" outranks anything derived from those floors.
if [ "$floor_bad" -ne 0 ]; then
    echo "::error title=Unreadable baseline floor::$BASELINE_FILE holds data line(s) with no" \
         "readable floor, named above. The ratchet cannot render a verdict over a floor it" \
         "cannot read, and reading one as 0 would pass every package silently." >&2
    exit 2
fi

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

    # `paste -sd', '` was used here and it does NOT join with ", ":
    # -d takes a LIST of delimiters and cycles them, so four items come
    # out "a,b c,d". Driven rather than read. The names in an error
    # message are what a reader greps the baseline for, and a stray
    # space inside a comma-separated list makes two of them look like
    # one token.
    # Compared BY NAME, not by count alone. Two files can hold the same
    # number of packages and not the same packages -- a substitution
    # keeps the count and changes the verdict, which a count check reads
    # as complete.
    sed -n 's/^package //p' "$REPORT" | sort -u > "$BASELINE_FILE.want.$$"
    printf '%s\n' "$compared_pkgs" | grep . | sort -u > "$BASELINE_FILE.got.$$"
    missing=$(comm -23 "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$" | paste -sd, - | sed 's/,/, /g')
    extra=$(comm -13 "$BASELINE_FILE.want.$$" "$BASELINE_FILE.got.$$" | paste -sd, - | sed 's/,/, /g')
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

    # A SECOND OPINION THAT DOES NOT COME FROM THE BLOB.
    #
    # Everything above compares two parses of one object: the resolver
    # read the baseline blob, this script read the same bytes, and they
    # agree. A blob that was ALREADY short when it was resolved is
    # therefore agreed to by both sides -- which is exactly the failure
    # the resolver's header names (a rebase, a truncated blob, a partial
    # fetch, a merge that dropped lines) and exactly the one neither side
    # can see.
    #
    # $PERCENT_FILE is not derived from the blob. It names every package
    # the run actually measured and comes out of `go tool covdata`. So
    # "the run measured N packages and the baseline floors M of them" is
    # a question the cross-check above cannot ask.
    #
    # IT WARNS, IT DOES NOT REFUSE, and the reason is that the signal is
    # ambiguous by construction: a package measured but not floored is
    # what a truncated baseline looks like AND what a legitimately new
    # package looks like, and nothing here can tell those apart. A gate
    # that fails on both would fire on every new package until someone
    # updated the baseline, and a gate that cries wolf gets discharged.
    # Naming them costs a line and lets a human decide in one glance.
    measured=$(awk '$2 == "coverage:" { print $1 }' "$PERCENT_FILE" | sort -u)
    unfloored=$(comm -13 <(sed -n 's/^package //p' "$REPORT" | sort -u) \
                         <(printf '%s\n' "$measured" | grep .) | paste -sd, - | sed 's/,/, /g')
    if [ -n "$unfloored" ]; then
        echo "::warning title=Measured but not floored::the run measured package(s) the baseline" \
             "does not floor: ${unfloored}. If those are new packages, add floors. If they are" \
             "not new, the baseline blob was already short when it was resolved -- which the" \
             "cross-check above cannot see, because both of its sides parse that same blob."
    fi
fi

if [ "$fail" -ne 0 ]; then
    echo
    echo "Coverage ratchet failed. Add tests covering what this change touches;"
    echo "lowering a floor in $BASELINE_FILE requires a recorded decision in the PR."
fi
exit "$fail"
