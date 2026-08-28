#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for coverage-ratchet.sh (#127). Synthesizes
# `go tool covdata percent` outputs and asserts the ratchet's verdicts:
# hold/improve/within-epsilon pass, regression and vanished packages fail.
#
# The exit-2 cases at the bottom are #734: this suite asserted every
# verdict the ratchet renders and never that it renders one at all, so
# a `coverage` check that compared nothing and reported success was
# green here too.
set -u

RATCHET="$(dirname "$0")/coverage-ratchet.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BASELINE="$TMP/baseline.txt"
cat > "$BASELINE" <<'EOF'
# comment lines and blanks are ignored

example.com/mod/pkg/a 80.0
example.com/mod/pkg/b 50.0
EOF

failures=0
check() {
    local name="$1" want_exit="$2" percent_file="$3"
    local eps="${4:-}"
    local got_exit
    if [ -n "$eps" ]; then
        RATCHET_EPSILON="$eps" bash "$RATCHET" "$percent_file" "$BASELINE" > "$TMP/out" 2>&1
    else
        bash "$RATCHET" "$percent_file" "$BASELINE" > "$TMP/out" 2>&1
    fi
    got_exit=$?
    if [ "$got_exit" -eq "$want_exit" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

percent() { # percent <file> <pct-a> [<pct-b>]
    local f="$1"
    printf '\texample.com/mod/pkg/a\t\tcoverage: %s%% of statements\n' "$2" > "$f"
    if [ "$#" -ge 3 ]; then
        printf '\texample.com/mod/pkg/b\t\tcoverage: %s%% of statements\n' "$3" >> "$f"
    fi
}

percent "$TMP/hold.txt" 80.0 50.0
check "exact hold passes" 0 "$TMP/hold.txt"

percent "$TMP/up.txt" 85.5 61.2
check "improvement passes" 0 "$TMP/up.txt"

percent "$TMP/noise.txt" 79.7 50.0
check "drop within epsilon passes" 0 "$TMP/noise.txt"

percent "$TMP/down.txt" 77.9 50.0
check "regression fails" 1 "$TMP/down.txt"

percent "$TMP/down-b.txt" 80.0 48.0
check "regression in second package fails" 1 "$TMP/down-b.txt"

percent "$TMP/gone.txt" 80.0
check "baselined package missing from output fails" 1 "$TMP/gone.txt"

percent "$TMP/eps.txt" 78.0 50.0
check "wider RATCHET_EPSILON tolerates the drop" 0 "$TMP/eps.txt" 2.5

if bash "$RATCHET" "$TMP/hold.txt" > /dev/null 2>&1; [ $? -eq 2 ]; then
    echo "PASS: usage error exits 2"
else
    echo "FAIL: usage error should exit 2"
    failures=$((failures + 1))
fi

# #734: a verdict over nothing. Each of these exited 0 — silently, in
# two of the three cases — before the refusal was added, which is a
# green required check on main enforcing no floor at all.
refuses() { # refuses <name> <baseline-file>
    local name="$1" baseline="$2" got_exit
    bash "$RATCHET" "$TMP/hold.txt" "$baseline" > "$TMP/out" 2>&1
    got_exit=$?
    if [ "$got_exit" -eq 2 ] && grep -q 'Nothing to inspect' "$TMP/out"; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit 2 + a refusal, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

refuses "missing baseline file refuses a verdict" "$TMP/does-not-exist.txt"

: > "$TMP/empty-baseline.txt"
refuses "empty baseline refuses a verdict" "$TMP/empty-baseline.txt"

printf '# a floor used to live here\n\n' > "$TMP/comments-baseline.txt"
refuses "comments-only baseline refuses a verdict" "$TMP/comments-baseline.txt"

# The percent file is the other half of the same hazard: the ratchet
# would report every baselined package "absent from coverage output"
# and exit 1, which reads as a coverage regression rather than as a
# harness fault. A wrong diagnosis costs the next person the afternoon.
bash "$RATCHET" "$TMP/no-such-percent.txt" "$BASELINE" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q 'Nothing to inspect' "$TMP/out"; then
    echo "PASS: missing percent file refuses a verdict"
else
    echo "FAIL: missing percent file should refuse a verdict"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

# --- the completeness cross-check (#791) --------------------------------
# THE DEFECT: `compared` counts data lines of the baseline the ratchet was
# HANDED, and the loop iterates over that same file, so the non-vacuity
# guard can only ever fire on zero. A baseline arriving with two of its
# five packages compares two, prints two PASS lines and exits 0.
#
# Every case below hands the ratchet a COMPLETE percent file and varies
# only the baseline and the report, so nothing here can pass or fail for
# a coverage reason.
percent "$TMP/full.txt" 85.0 55.0

xcheck() { # xcheck <name> <want-exit> <baseline> <report|-> [grep-for]
    local name="$1" want="$2" bl="$3" rep="$4" needle="${5:-}"
    local got
    if [ "$rep" = "-" ]; then
        RATCHET_REPORT='' bash "$RATCHET" "$TMP/full.txt" "$bl" > "$TMP/out" 2>&1
    else
        RATCHET_REPORT="$rep" bash "$RATCHET" "$TMP/full.txt" "$bl" > "$TMP/out" 2>&1
    fi
    got=$?
    if [ "$got" -ne "$want" ]; then
        echo "FAIL: $name (want exit $want, got $got)"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    if [ -n "$needle" ] && ! grep -F "$needle" "$TMP/out" > /dev/null; then
        echo "FAIL: $name (output does not mention '$needle')"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name"
}

# The resolver's report for the COMPLETE baseline: two packages.
cat > "$TMP/report-full" <<'EOF'
merge_base 1111111111111111111111111111111111111111
blob 2222222222222222222222222222222222222222
count 2
package example.com/mod/pkg/a
package example.com/mod/pkg/b
EOF
xcheck "a baseline matching its report is cross-checked" 0 "$BASELINE" "$TMP/report-full" "Cross-checked"

# THE CASE THIS EXISTS FOR. The baseline lost a line; the report still
# says two. Pre-#791 this compared one package, printed PASS, exited 0.
cat > "$TMP/truncated.txt" <<'EOF'
# comment lines and blanks are ignored

example.com/mod/pkg/a 80.0
EOF
xcheck "a truncated baseline is refused, not passed" 2 "$TMP/truncated.txt" "$TMP/report-full" "The baseline is incomplete"
grep -F 'example.com/mod/pkg/b' "$TMP/out" > /dev/null \
    && echo "PASS: the refusal NAMES the package that was not compared" \
    || { echo "FAIL: the refusal does not name the missing package"; sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); }

# A SUBSTITUTION KEEPS THE COUNT. Two packages in, two compared, and one
# of them is not the one the resolver handed over — a count-only check
# reads that as complete, which is why the comparison is by name.
cat > "$TMP/swapped.txt" <<'EOF'
example.com/mod/pkg/a 80.0
example.com/mod/pkg/z 50.0
EOF
xcheck "a substituted package is caught though the count matches" 2 "$TMP/swapped.txt" "$TMP/report-full" "COMPARED BUT NOT RESOLVED"

# The absence of a cross-check must be announced. A silent exemption here
# rebuilds the hole: a release log reading "PASS ... PASS" with no further
# comment is exactly what the incomplete baseline produced.
xcheck "no report says NOT CROSS-CHECKED and still renders its verdict" 0 "$BASELINE" "-" "NOT CROSS-CHECKED"

# A report that cannot be read is not a licence to skip the check.
#
# THE PACKAGE LINES ARE CORRECT HERE ON PURPOSE. The first version of
# this case carried no package lines either, so the name comparison found
# two packages compared and none resolved and refused on THAT — and a
# mutant that deleted the unreadable-count refusal entirely still exited
# 2, still printed the message, and passed. The verdict was being carried
# by a different mechanism than the one under test.
#
# With the names agreeing, the count guard is the only thing left that
# can produce a refusal.
{ printf 'merge_base abc\nblob def\n'
  printf 'package example.com/mod/pkg/a\npackage example.com/mod/pkg/b\n'; } > "$TMP/report-nocount"
xcheck "a report with no count line refuses" 2 "$BASELINE" "$TMP/report-nocount" "Unreadable resolver report"

# The default is the sidecar beside the baseline, so the release path is
# cross-checked without a second wiring step someone could forget.
cp "$BASELINE" "$TMP/sidecar-baseline.txt"
cp "$TMP/report-full" "$TMP/sidecar-baseline.txt.report"
bash "$RATCHET" "$TMP/full.txt" "$TMP/sidecar-baseline.txt" > "$TMP/out" 2>&1
if [ $? -eq 0 ] && grep -F 'Cross-checked' "$TMP/out" > /dev/null; then
    echo "PASS: the report is found beside the baseline with no wiring"
else
    echo "FAIL: the default sidecar report was not picked up"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- a floor that lost its number (#791) ---------------------------------
# `read -r pkg want` leaves `want` EMPTY when a data line carries only a
# package name, and awk evaluates an empty string as numeric 0 -- so every
# percentage "beats" it and the package prints PASS while nothing is
# enforced. Driven on the real baseline with only pkg/plugin's number
# deleted (the LINE survives, the NUMBER does not), the ratchet printed
#
#     PASS  .../pkg/plugin: 0.1% beats baseline % -- raise the floor
#     Cross-checked: compared 5 of 5 resolved package floor(s)
#
# and exited 0. The completeness cross-check is no help and cannot be: it
# compares names and counts, and the damaged line is present on both
# sides, so resolver, cross-check and ratchet all agree while one of the
# five floors is gone. The attestation is what makes it look verified.
#
# The percent file below is COMPLETE and every package is at or above its
# real floor except the one whose floor was deleted, so nothing here can
# pass or fail for a coverage reason.
floorless() { # floorless <name> <baseline-body> [needle]
    local name="$1" body="$2" needle="${3:-Unreadable baseline floor}" got
    printf '%s' "$body" > "$TMP/floorless.txt"
    RATCHET_REPORT='' bash "$RATCHET" "$TMP/full.txt" "$TMP/floorless.txt" > "$TMP/out" 2>&1
    got=$?
    if [ "$got" -ne 2 ]; then
        echo "FAIL: $name (want exit 2, got $got)"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    if ! grep -F "$needle" "$TMP/out" > /dev/null; then
        echo "FAIL: $name (output does not mention '$needle')"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name"
}

floorless "a baseline line that lost its percentage refuses a verdict" \
    'example.com/mod/pkg/a 80.0
example.com/mod/pkg/b
'
# The refusal must NAME the line, and the fail-open verdict must be gone.
# Asserting exit 2 alone would be satisfied by any other exit-2 arm.
#
# Grepped on the REFUSAL LINE, not on the whole output: the fail-open
# verdict this replaces also mentions the package, so a bare name grep
# passes against the very defect it is here to observe -- measured, it
# survived the mutant that deletes the validation outright.
if grep -F 'no readable floor' "$TMP/out" | grep -F 'example.com/mod/pkg/b' > /dev/null; then
    echo "PASS: the refusal names the package whose floor is missing"
else
    echo "FAIL: the refusal does not name the floorless package"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi
# The fail-open signature is a PASS line for the floorless package itself
# -- `0.1% beats baseline %`, with the floor rendering as nothing at all.
# pkg/a's own PASS line is expected and is not what this looks for.
if grep -E '^PASS .*example\.com/mod/pkg/b' "$TMP/out" > /dev/null; then
    echo "FAIL: the floorless package still had a verdict rendered over it"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
else
    echo "PASS: and no verdict is rendered over the package with no floor"
fi

# The same hazard with a non-empty but unparseable floor: awk reads it as
# 0 just the same. `n/a` is what a hand-edited baseline produces.
floorless "an unparseable floor refuses a verdict" \
    'example.com/mod/pkg/a 80.0
example.com/mod/pkg/b n/a
'

# PRESERVATION CONTROL for the validator. A refusal is only worth having
# if it refuses the damaged shape and nothing else, and the cheapest way
# to break this gate is to make the pattern too strict. An integer floor
# carries no decimal point and is a perfectly good floor: it must still be
# READ and still be ENFORCED, so this drives a regression against it and
# demands exit 1 -- not merely "not 2", which a silent pass would satisfy.
printf 'example.com/mod/pkg/a 80\n' > "$TMP/intfloor.txt"
percent "$TMP/under.txt" 70.0
RATCHET_REPORT='' bash "$RATCHET" "$TMP/under.txt" "$TMP/intfloor.txt" > "$TMP/out" 2>&1
if [ $? -eq 1 ] && grep -F 'is below baseline 80%' "$TMP/out" > /dev/null; then
    echo "PASS: an integer floor is still read and still enforced"
else
    echo "FAIL: an integer floor was not enforced as a floor"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# EVERY damaged line in ONE run. The refusal is raised after the loop, not
# at the point of detection, because a merge that damaged one data line has
# usually damaged more than one and a gate that names the first and stops
# makes the next person fix them a round trip at a time. Two floors deleted
# here; both names must appear.
floorless "two damaged lines are both named in one run" \
    'example.com/mod/pkg/a
example.com/mod/pkg/b
'
if grep -F 'no readable floor' "$TMP/out" | grep -F 'example.com/mod/pkg/a' > /dev/null \
   && grep -F 'no readable floor' "$TMP/out" | grep -F 'example.com/mod/pkg/b' > /dev/null; then
    echo "PASS: and the run did not stop at the first one"
else
    echo "FAIL: the second damaged line was not named"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# THE NARROWING THIS INTRODUCES, PINNED SO IT IS A DECISION AND NOT AN
# ACCIDENT. `read -r pkg want` puts EVERYTHING after the package name into
# `want`, so a data line carrying a third field arrives as "80.0 junk".
# Before the validation that was handed to awk, which read the numeric
# prefix 80.0 and rendered "is below baseline 80.0 junk%" -- a verdict off
# a value nobody meant. It now refuses.
#
# Refusing is the intended direction: the documented data line is
# "<package> <min-percent>", a trailing field is not a supported comment
# form, and extra content after the floor is one of the shapes a merge that
# went wrong actually produces. The real baseline has ZERO data lines with
# other than two fields, measured, so nothing in the tree depends on the
# old behaviour. This case exists so a future reader finds the decision
# rather than rediscovering it from a red release check.
printf 'example.com/mod/pkg/a 80.0 junk\n' > "$TMP/trailing.txt"
percent "$TMP/under-a.txt" 70.0
RATCHET_REPORT='' bash "$RATCHET" "$TMP/under-a.txt" "$TMP/trailing.txt" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 2 ] && grep -F "got '80.0 junk'" "$TMP/out" > /dev/null; then
    echo "PASS: a data line with a trailing field refuses and quotes what it read"
else
    echo "FAIL: a trailing field was not refused (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- the count term of the completeness comparison (#791) ----------------
# The refusal at the cross-check is three terms OR'd together -- count,
# missing, extra -- and the two name terms were the only ones with a case.
# Dropping just `[ "$compared" -ne "$want" ]` left both suites green while
# the ratchet printed "compared 6 of 5" and exited 0.
#
# A DUPLICATED data line is the shape that isolates it: `sort -u` on both
# sides makes the NAME sets identical, so missing and extra are empty and
# the count term is the only thing that can produce a refusal.
cat > "$TMP/duplicated.txt" <<'EOF'
example.com/mod/pkg/a 80.0
example.com/mod/pkg/b 50.0
example.com/mod/pkg/a 80.0
EOF
xcheck "a duplicated baseline line is caught by the count, not by the names" \
    2 "$TMP/duplicated.txt" "$TMP/report-full" "handed over 2 package floor(s) and this run compared 3."
# AIMED, not coincidental: if either name term had fired, the kill would
# not have come from the count comparison under test.
if grep -F 'NOT COMPARED' "$TMP/out" > /dev/null || grep -F 'COMPARED BUT NOT RESOLVED' "$TMP/out" > /dev/null; then
    echo "FAIL: the refusal came from a name term, so the count term is still unobserved"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
else
    echo "PASS: and the name terms stayed silent, so only the count could have refused"
fi

# --- the measured-but-not-floored warning (#791) -------------------------
# The one check in this gate that does NOT compare two parses of the same
# blob -- it asks the percent file, which comes from `go tool covdata` --
# and it shipped with no observer at all: deleting the whole block left
# both suites at 0 failures.
#
# It WARNS rather than refuses, so exit 0 is correct here and the output
# is the entire assertion.
printf '\texample.com/mod/pkg/c\t\tcoverage: 42.0%% of statements\n' > "$TMP/third.txt"
cat "$TMP/full.txt" "$TMP/third.txt" > "$TMP/full-plus-c.txt"
RATCHET_REPORT="$TMP/report-full" bash "$RATCHET" "$TMP/full-plus-c.txt" "$BASELINE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] \
   && grep -F 'Measured but not floored' "$TMP/out" > /dev/null \
   && grep -F 'example.com/mod/pkg/c' "$TMP/out" > /dev/null; then
    echo "PASS: a measured package with no floor is named in a warning"
else
    echo "FAIL: the measured-but-not-floored warning did not fire (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# PRESERVATION CONTROL. The warning is ambiguous by construction -- a
# truncated baseline and a legitimately new package look identical -- so
# it is only tolerable if it stays silent on a healthy run. A widening
# that fires on the healthy tree is a gate nobody keeps.
RATCHET_REPORT="$TMP/report-full" bash "$RATCHET" "$TMP/full.txt" "$BASELINE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] && ! grep -F 'Measured but not floored' "$TMP/out" > /dev/null; then
    echo "PASS: and it stays silent when every measured package is floored"
else
    echo "FAIL: the warning fired on a run with nothing unfloored (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures ratchet test(s) failed"
    exit 1
fi
echo "All ratchet tests passed"
