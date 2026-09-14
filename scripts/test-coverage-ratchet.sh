#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for coverage-ratchet.sh (#127). Synthesizes
# `go tool covdata percent` outputs and asserts the ratchet's verdicts:
# hold/improve/within-epsilon pass, regression fails, and a package the
# baseline floors but the output does not carry is split three ways --
# still in the tree, or gone but still floored at head, both FAIL; gone
# and unfloored at head, DROPPED (the section at the bottom).
#
# The exit-2 cases at the bottom are #734: this suite asserted every
# verdict the ratchet renders and never that it renders one at all, so
# a `coverage` check that compared nothing and reported success was
# green here too.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

RATCHET="$(dirname "$0")/coverage-ratchet.sh"
guarded_tmpdir TMP

# THE HEAD BASELINE, FOR EVERY CASE WRITTEN BEFORE THE THIRD VERDICT
# EXISTED. A baselined package with no coverage is now judged against the
# baseline AS IT STANDS AT HEAD as well: gone from the tree and gone from
# that file is DROPPED, anything else is FAIL. The cases below were all
# written when the baseline handed in was the only baseline there was --
# which is what a push or a dispatch actually does (coverage.yml:362-373,
# "No pull_request context"), so that is the shape they keep, and the
# deliberate-deletion arm gets its own cases at the bottom with the two
# files differing. Without this every fake package in this file would be
# "deleted at head" AND unfloored at head, which is DROPPED: the vanished
# package case at line 71 would have gone green against a rule it says
# nothing about.
ratchet() { RATCHET_HEAD_BASELINE="${2-}" bash "$RATCHET" "$@"; }

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
        RATCHET_EPSILON="$eps" ratchet "$percent_file" "$BASELINE" > "$TMP/out" 2>&1
    else
        ratchet "$percent_file" "$BASELINE" > "$TMP/out" 2>&1
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

if ratchet "$TMP/hold.txt" > /dev/null 2>&1; [ $? -eq 2 ]; then
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
    ratchet "$TMP/hold.txt" "$baseline" > "$TMP/out" 2>&1
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
# Since the deleted-package arm it would be worse than a wrong
# diagnosis: a percent file that never arrived would read as a tree in
# which every unfloored-at-head package had been deliberately deleted,
# and some of those lines would be DROPPED rather than FAIL.
ratchet "$TMP/no-such-percent.txt" "$BASELINE" > "$TMP/out" 2>&1
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
        RATCHET_REPORT='' ratchet "$TMP/full.txt" "$bl" > "$TMP/out" 2>&1
    else
        RATCHET_REPORT="$rep" ratchet "$TMP/full.txt" "$bl" > "$TMP/out" 2>&1
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
ratchet "$TMP/full.txt" "$TMP/sidecar-baseline.txt" > "$TMP/out" 2>&1
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
    RATCHET_REPORT='' ratchet "$TMP/full.txt" "$TMP/floorless.txt" > "$TMP/out" 2>&1
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
RATCHET_REPORT='' ratchet "$TMP/under.txt" "$TMP/intfloor.txt" > "$TMP/out" 2>&1
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
RATCHET_REPORT='' ratchet "$TMP/under-a.txt" "$TMP/trailing.txt" > "$TMP/out" 2>&1
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
RATCHET_REPORT="$TMP/report-full" ratchet "$TMP/full-plus-c.txt" "$BASELINE" > "$TMP/out" 2>&1
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
RATCHET_REPORT="$TMP/report-full" ratchet "$TMP/full.txt" "$BASELINE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] && ! grep -F 'Measured but not floored' "$TMP/out" > /dev/null; then
    echo "PASS: and it stays silent when every measured package is floored"
else
    echo "FAIL: the warning fired on a run with nothing unfloored (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- two packages on one line (coverage run 34528649522) -----------------
# `go tool covdata percent` prints a package that contributes no
# statements as its name alone, with no percentage and no newline, so the
# next package's entry lands on the same line. The four lines below are
# the ones that run produced, bytes and tabs as they came off the tool:
# pkg/buildinfo has no statements and swallowed pkg/dhcp, which measured
# 90.5%. Read a line at a time, the ratchet called pkg/dhcp "absent from
# coverage output" and failed a required check over coverage that was
# there.
GLUED="$TMP/glued.txt"
{
    printf '\tgithub.com/claymore666/docker-net-dhcp/v2/cmd/net-dhcp\t\tcoverage: 83.3%% of statements\n'
    printf '\tgithub.com/claymore666/docker-net-dhcp/v2/pkg/buildinfo\t\t\tgithub.com/claymore666/docker-net-dhcp/v2/pkg/dhcp\t\tcoverage: 90.5%% of statements\n'
    printf '\tgithub.com/claymore666/docker-net-dhcp/v2/pkg/plugin\t\tcoverage: 90.1%% of statements\n'
    printf '\tgithub.com/claymore666/docker-net-dhcp/v2/pkg/util\t\tcoverage: 97.3%% of statements\n'
} > "$GLUED"

REAL_BASELINE="$TMP/baseline-2x.txt"
cat > "$REAL_BASELINE" <<'EOF'
github.com/claymore666/docker-net-dhcp/v2/pkg/util 95.0
github.com/claymore666/docker-net-dhcp/v2/pkg/plugin 86.8
github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp 89.9
github.com/claymore666/docker-net-dhcp/v2/cmd/net-dhcp 77.8
EOF

RATCHET_REPORT='' ratchet "$GLUED" "$REAL_BASELINE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] \
   && grep -F 'pkg/dhcp: 90.5% beats baseline 89.9%' "$TMP/out" > /dev/null; then
    echo "PASS: a swallowed package keeps its percentage"
else
    echo "FAIL: the swallowed package did not get its verdict (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# THE OTHER HALF, and the one that makes the fix a parse rather than a
# search: the package that did the swallowing has no percentage of its
# own on that line, so it must still read as absent. A lookup that took
# any number from a line carrying the name would credit pkg/buildinfo
# with pkg/dhcp's 90.5 and pass this baseline.
BUILDINFO_BASELINE="$TMP/baseline-buildinfo.txt"
cat > "$BUILDINFO_BASELINE" <<'EOF'
github.com/claymore666/docker-net-dhcp/v2/pkg/buildinfo 90.0
EOF
RATCHET_REPORT='' ratchet "$GLUED" "$BUILDINFO_BASELINE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 1 ] \
   && grep -F 'pkg/buildinfo: in baseline but absent from coverage output' "$TMP/out" > /dev/null; then
    echo "PASS: the swallowing package is not credited with the percentage it swallowed"
else
    echo "FAIL: pkg/buildinfo was given a number it does not have (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# THE VANISHED-PACKAGE RULE, DRIVEN AGAINST THE NEW PARSE. The absence is
# real here: pkg/dhcp is deleted from the output entirely, leaving
# pkg/buildinfo alone on its line exactly as the tool prints it when
# nothing follows. A field scan that fell back to "any number on any line
# mentioning nothing in particular" would find 90.1 or 97.3 and pass.
GONE="$TMP/glued-gone.txt"
{
    printf '\tgithub.com/claymore666/docker-net-dhcp/v2/cmd/net-dhcp\t\tcoverage: 83.3%% of statements\n'
    printf '\tgithub.com/claymore666/docker-net-dhcp/v2/pkg/buildinfo\t\t\tgithub.com/claymore666/docker-net-dhcp/v2/pkg/plugin\t\tcoverage: 90.1%% of statements\n'
    printf '\tgithub.com/claymore666/docker-net-dhcp/v2/pkg/util\t\tcoverage: 97.3%% of statements\n'
} > "$GONE"
RATCHET_REPORT='' ratchet "$GONE" "$REAL_BASELINE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 1 ] \
   && grep -F 'pkg/dhcp: in baseline but absent from coverage output' "$TMP/out" > /dev/null; then
    echo "PASS: a package genuinely absent from the output still fails"
else
    echo "FAIL: the vanished-package rule did not fire (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# The measured-but-not-floored warning reads the same file with the same
# hazard: line-at-a-time it could not see a package whose entry shared a
# line, so the one thing it exists to say went unsaid about exactly the
# package this run measured and this baseline does not floor.
THREE_BASELINE="$TMP/baseline-three.txt"
cat > "$THREE_BASELINE" <<'EOF'
github.com/claymore666/docker-net-dhcp/v2/pkg/util 95.0
github.com/claymore666/docker-net-dhcp/v2/pkg/plugin 86.8
github.com/claymore666/docker-net-dhcp/v2/cmd/net-dhcp 77.8
EOF
{
    echo "count 3"
    echo "package github.com/claymore666/docker-net-dhcp/v2/pkg/util"
    echo "package github.com/claymore666/docker-net-dhcp/v2/pkg/plugin"
    echo "package github.com/claymore666/docker-net-dhcp/v2/cmd/net-dhcp"
} > "$TMP/report-three"
RATCHET_REPORT="$TMP/report-three" ratchet "$GLUED" "$THREE_BASELINE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] \
   && grep -F 'Measured but not floored' "$TMP/out" > /dev/null \
   && grep -F 'pkg/dhcp' "$TMP/out" > /dev/null; then
    echo "PASS: a swallowed package is named in the unfloored warning"
else
    echo "FAIL: the unfloored warning did not name the swallowed package (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- a deliberately deleted package (run 34541498417) --------------------
# Since #735 the floors come from the MERGE BASE, which is what stops a PR
# lowering its own. The same rule made a deliberate deletion unpassable:
# 2.0 deletes cmd/dhcp-handler and removes its row from the baseline, and
# the row's REMOVAL is invisible to a PR whose base still carries it. The
# base floor is read, no coverage is found, and the release PR's required
# `coverage` check fails over a package that is gone on purpose --
# measured on run 34541498417 (release PR #937).
#
# A baselined package with no coverage therefore has three verdicts,
# decided by two facts: does the import path still build AT HEAD, and does
# the HEAD baseline still floor it.
#
# THE PACKAGES BELOW ARE THIS REPOSITORY'S OWN, deliberately. The
# existence half is asked of `go list`, so a made-up import path answers
# "deleted" for the same reason a deleted one does and case (c) could not
# be written at all. cmd/dhcp-handler is deleted on 2.0.0; pkg/buildinfo
# is present and contributes no statements.
SELF=github.com/claymore666/docker-net-dhcp/v2
GONE_PKG=$SELF/cmd/dhcp-handler
HERE_PKG=$SELF/pkg/buildinfo

printf '\t%s/pkg/util\t\tcoverage: 97.3%% of statements\n' "$SELF" > "$TMP/drop-pct.txt"
printf '%s/pkg/util 95.0\n%s 74.0\n' "$SELF" "$GONE_PKG" > "$TMP/drop-base.txt"
printf '%s/pkg/util 95.0\n' "$SELF"                      > "$TMP/head-without.txt"
printf '%s/pkg/util 95.0\n%s 74.0\n' "$SELF" "$GONE_PKG" > "$TMP/head-with.txt"
printf '%s/pkg/util 95.0\n%s 90.0\n' "$SELF" "$HERE_PKG" > "$TMP/base-present.txt"

# THE PRE-FIX SCRIPT IS THE STRONGEST MUTANT, and it is built by putting
# the old arm back into the REAL script rather than by keeping a copy of
# the block: a copy stops being the subject the moment the script moves
# on, and the assertion that the two differ is what says the surgery
# found anything. Its verdict on all three cases was FAIL, so (a) must
# flip and (b) and (c) must not -- a case that passes against the pre-fix
# script is measuring something else.
python3 - "$RATCHET" "$TMP/prefix.sh" <<'SURGERY'
import sys
src = open(sys.argv[1]).read()
start = src.index('    if [ -z "$got" ]; then\n')
mark = '        continue\n    fi\n'
end = src.index(mark, start) + len(mark)
old = ('    if [ -z "$got" ]; then\n'
       '        echo "FAIL  $pkg: in baseline but absent from coverage output — '
       'deleted/renamed? Update $BASELINE_FILE deliberately."\n'
       '        fail=1\n'
       '        continue\n'
       '    fi\n')
assert src[start:end] != old, "the pre-fix arm is still in the script: this control is inert"
open(sys.argv[2], "w").write(src[:start] + old + src[end:])
SURGERY

arm() { # arm <name> <want-exit> <baseline> <head-baseline> <needle> <pre-fix-want-exit>
    local name="$1" want="$2" bl="$3" hb="$4" needle="$5" pwant="$6" got
    RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$hb" bash "$RATCHET" \
        "$TMP/drop-pct.txt" "$bl" > "$TMP/out" 2>&1
    got=$?
    if [ "$got" -ne "$want" ] || ! grep -F "$needle" "$TMP/out" > /dev/null; then
        echo "FAIL: $name (want exit $want and '$needle', got $got)"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1)); return
    fi
    echo "PASS: $name"
    RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$hb" bash "$TMP/prefix.sh" \
        "$TMP/drop-pct.txt" "$bl" > "$TMP/pre" 2>&1
    got=$?
    if [ "$got" -eq "$pwant" ]; then
        echo "PASS: ...and the pre-fix script exits $pwant on the same input"
    else
        echo "FAIL: $name — pre-fix script wanted exit $pwant, got $got"
        sed 's/^/    /' "$TMP/pre"; failures=$((failures + 1))
    fi
}

# (a) gone from the tree AND gone from the head baseline. The deletion is
# deliberate, and the run is not held to a floor for a package nobody can
# cover. This is the arm that flips: the pre-fix script fails it.
arm "a deleted package unfloored at head is DROPPED, not failed" \
    0 "$TMP/drop-base.txt" "$TMP/head-without.txt" "DROPPED  $GONE_PKG: deleted at head" 1
if grep -F '(base floor was 74.0)' "$TMP/out" > /dev/null; then
    echo "PASS: and the DROPPED line names the floor the base was holding it to"
else
    echo "FAIL: the DROPPED line does not name the base floor"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# (b) gone from the tree, still floored at head. The deletion did not
# reach the baseline; the row is the thing to remove, and until it is this
# is a red gate rather than a silent pass.
arm "a deleted package still floored at head fails" \
    1 "$TMP/drop-base.txt" "$TMP/head-with.txt" "deleted at head but still floored in" 1

# (c) STILL IN THE TREE, and the head baseline does not floor it either,
# so the existence probe is the only thing between this and a DROPPED
# line. This is the case that makes (a) a rule about deletion rather than
# a rule about any absence at all.
arm "a package that still builds at head fails though head does not floor it" \
    1 "$TMP/base-present.txt" "$TMP/head-without.txt" \
    "$HERE_PKG: in baseline but absent from coverage output" 1
if grep -F 'DROPPED' "$TMP/out" > /dev/null; then
    echo "FAIL: a package still in the tree was reported as DROPPED"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
else
    echo "PASS: and it is not reported as DROPPED"
fi

# THE CROSS-CHECK STILL SEES IT. DROPPED is a verdict, not a skip: the
# package is counted as compared and named in compared_pkgs, or the
# completeness check refuses "compared 1 of 2" and the fix trades one red
# required check for another.
{ echo "count 2"; echo "package $SELF/pkg/util"; echo "package $GONE_PKG"; } > "$TMP/report-drop"
RATCHET_REPORT="$TMP/report-drop" RATCHET_HEAD_BASELINE="$TMP/head-without.txt" \
    bash "$RATCHET" "$TMP/drop-pct.txt" "$TMP/drop-base.txt" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] && grep -F 'Cross-checked: compared 2 of 2' "$TMP/out" > /dev/null; then
    echo "PASS: a DROPPED package is counted as compared by the cross-check"
else
    echo "FAIL: the cross-check did not count the DROPPED package (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# THE HEAD BASELINE IS FOUND WITH NO WIRING, and from the script's own
# location rather than from the caller's working directory: coverage.yml
# passes no such argument, and a derivation that needed one would be the
# step someone forgets on the release path (#791's lesson). Driven from a
# directory that is not the repository, with RATCHET_HEAD_BASELINE unset
# -- this branch's real .github/coverage-baseline.txt carries no
# cmd/dhcp-handler row, so the verdict must still be DROPPED.
ABS_RATCHET=$(cd "$(dirname "$RATCHET")" && pwd)/coverage-ratchet.sh
( cd "$TMP" && RATCHET_REPORT='' bash "$ABS_RATCHET" "$TMP/drop-pct.txt" "$TMP/drop-base.txt" ) \
    > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] && grep -F "DROPPED  $GONE_PKG" "$TMP/out" > /dev/null; then
    echo "PASS: the head baseline is found from the script's own path, from any directory"
else
    echo "FAIL: the default head baseline was not found from another directory (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# THE PROBE IS ROOTED AT THE REPOSITORY, NOT AT THE CALLER. The case
# above cannot see that: a deleted package is unresolvable from anywhere,
# so it reads DROPPED with or without the rooting, and the mutant that
# drops it SURVIVED that case (measured). The rooting only decides for a
# package that DOES exist -- from another directory an unrooted probe
# calls pkg/buildinfo deleted and DROPS a package the tree still carries.
( cd "$TMP" && RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$TMP/head-without.txt" \
    bash "$ABS_RATCHET" "$TMP/drop-pct.txt" "$TMP/base-present.txt" ) > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 1 ] && grep -F "$HERE_PKG: in baseline but absent from coverage output" "$TMP/out" > /dev/null; then
    echo "PASS: a package that exists is found from another directory too"
else
    echo "FAIL: the existence probe followed the caller's directory (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# AND AN UNREADABLE ONE REFUSES. This is the fail-open direction of the
# whole arm: a head baseline resolved to a path that does not exist floors
# nothing, so every vanished package reads as deliberately deleted, on a
# run where nothing else looks wrong.
RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$TMP/no-such-head.txt" \
    bash "$RATCHET" "$TMP/drop-pct.txt" "$TMP/drop-base.txt" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 2 ] && grep -F 'No baseline at head' "$TMP/out" > /dev/null; then
    echo "PASS: an unreadable head baseline refuses a verdict"
else
    echo "FAIL: an unreadable head baseline did not refuse (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# THE HEAD-BASELINE LOOKUP IS A FIELD, NOT A SUBSTRING. A row for a
# LONGER import path that contains the dropped one -- the shape a rename
# to <pkg>-v2 produces -- must not read as "head still floors it". The
# real baseline cannot observe this: it carries the dropped path zero
# times, in a data row or a comment, so a substring lookup passes every
# other case in this file (measured: `$1 == p` mutated to `$0 ~ p`
# survived the whole suite before this case existed).
printf '%s/pkg/util 95.0\n%s-v2 74.0\n' "$SELF" "$GONE_PKG" > "$TMP/head-longer.txt"
RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$TMP/head-longer.txt" \
    bash "$RATCHET" "$TMP/drop-pct.txt" "$TMP/drop-base.txt" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] && grep -F "DROPPED  $GONE_PKG" "$TMP/out" > /dev/null; then
    echo "PASS: a longer path containing the dropped one does not count as its row"
else
    echo "FAIL: the head-baseline lookup matched a longer path (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# A FAILED `go list` IS NOT ALWAYS "GONE". A toolchain that cannot run
# fails for a package that is right there, and folded into "absent" that
# DROPS a floor for a package the release still ships whenever the head
# baseline has dropped the row too. The probe's failure is controlled
# against `go list ./...` through the same toolchain, and a red control
# is a refusal rather than a verdict.
#
# Driven with a `go` on PATH that fails for everything -- the shape an
# unusable module cache or a missing toolchain produces, without needing
# either.
mkdir -p "$TMP/fakebin"
printf '#!/bin/sh\nexit 1\n' > "$TMP/fakebin/go"
chmod +x "$TMP/fakebin/go"
PATH="$TMP/fakebin:$PATH" RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$TMP/head-without.txt" \
    bash "$RATCHET" "$TMP/drop-pct.txt" "$TMP/drop-base.txt" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 2 ] && grep -F "fails in this checkout too" "$TMP/out" > /dev/null; then
    echo "PASS: a toolchain that answers nothing refuses instead of DROPPING the package"
else
    echo "FAIL: a broken toolchain was read as a deleted package (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# PRESERVATION CONTROL for the refusal above, or it measures "any go list
# failure refuses" and the DROPPED arm is unreachable. Same fake `go`,
# except that the control pattern resolves: the toolchain works, this one
# package does not, which is exactly what a deletion looks like.
printf '#!/bin/sh\nfor a in "$@"; do [ "$a" = "./..." ] && exit 0; done\nexit 1\n' \
    > "$TMP/fakebin/go"
chmod +x "$TMP/fakebin/go"
PATH="$TMP/fakebin:$PATH" RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$TMP/head-without.txt" \
    bash "$RATCHET" "$TMP/drop-pct.txt" "$TMP/drop-base.txt" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] && grep -F "DROPPED  $GONE_PKG" "$TMP/out" > /dev/null; then
    echo "PASS: and a working toolchain that cannot resolve one package still DROPS it"
else
    echo "FAIL: the control probe swallowed a genuine deletion (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi
/bin/rm -f "$TMP/fakebin/go"

# --- THE RELEASE PR AFTER A MAJOR-VERSION RENAME (#979) ------------------
#
# The dev->main release PR carrying a module rename is the shape that
# turns this gate off. The merge-base blob is MAIN's baseline, spelled
# without the major; the run's covdata output and the head baseline both
# carry it. Read a row at a time under its own name, every base row is
# "gone at head, and gone from the head baseline too": the DROPPED arm,
# counted as compared, cross-check agreeing, exit 0 over packages nobody
# compared to any floor. Measured on this repository's own rename before
# the fix: four DROPPED, "compared 4 of 4", packages at 10.0% against
# floors of 82.8 to 96.8.
#
# BOTH SPELLINGS ARE WRITTEN OUT HERE, deliberately. The script derives
# the old one by stripping its own major suffix; a test that derived it
# the same way could not tell a wrong derivation from a right one.
SELF_OLD=github.com/claymore666/docker-net-dhcp
SELF_NEW=github.com/claymore666/docker-net-dhcp/v2

RENAME_BASE="$TMP/rename-base.txt"      # main's baseline: the OLD spelling
RENAME_HEAD="$TMP/rename-head.txt"      # this branch's: the NEW spelling
cat > "$RENAME_BASE" <<EOF
$SELF_OLD/pkg/util 96.8
$SELF_OLD/pkg/plugin 89.6
EOF
cat > "$RENAME_HEAD" <<EOF
$SELF_NEW/pkg/util 96.8
$SELF_NEW/pkg/plugin 89.6
EOF

RENAME_LOW="$TMP/rename-low.txt"        # the release measured almost nothing
RENAME_OK="$TMP/rename-ok.txt"          # the release held its floors
printf '\t%s/pkg/util\t\tcoverage: 10.0%% of statements\n\t%s/pkg/plugin\t\tcoverage: 10.0%% of statements\n' \
    "$SELF_NEW" "$SELF_NEW" > "$RENAME_LOW"
printf '\t%s/pkg/util\t\tcoverage: 97.3%% of statements\n\t%s/pkg/plugin\t\tcoverage: 90.1%% of statements\n' \
    "$SELF_NEW" "$SELF_NEW" > "$RENAME_OK"

# rename_run <script> <percent-file>
rename_run() {
    RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$RENAME_HEAD" \
        bash "$1" "$2" "$RENAME_BASE" > "$TMP/out" 2>&1
}

rename_run "$RATCHET" "$RENAME_LOW"; got=$?
if [ "$got" -eq 1 ] \
   && grep -F "FAIL  $SELF_NEW/pkg/util: 10.0% is below baseline 96.8%" "$TMP/out" > /dev/null \
   && grep -F "FAIL  $SELF_NEW/pkg/plugin: 10.0% is below baseline 89.6%" "$TMP/out" > /dev/null; then
    echo "PASS: a renamed module's floors are compared under the new names"
else
    echo "FAIL: the release PR's floors were not compared after the rename (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# The other direction, so the arm is not simply "fail a renamed release".
rename_run "$RATCHET" "$RENAME_OK"; got=$?
if [ "$got" -eq 0 ] \
   && grep -F "PASS  $SELF_NEW/pkg/util: 97.3% beats baseline 96.8%" "$TMP/out" > /dev/null; then
    echo "PASS: a renamed module that held its floors still passes"
else
    echo "FAIL: a renamed release that held its floors did not pass (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# The pairing line is what a reader of the log matches an old row to a
# new one by, and a summary count is true of any two rows.
if grep -F "RENAMED  $SELF_OLD/pkg/util is $SELF_NEW/pkg/util at head" "$TMP/out" > /dev/null; then
    echo "PASS: the log names which row moved to which path"
else
    echo "FAIL: the rename pairing was not named in the log"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# A package the rename does not reach is still classified as before:
# already under the head module, so nothing is re-spelled, and it is
# genuinely absent. Driven beside the two above so the retry cannot have
# become "match everything".
DROP_BASE="$TMP/rename-drop-base.txt"
DROP_HEAD="$TMP/rename-drop-head.txt"
cat > "$DROP_BASE" <<EOF
$SELF_NEW/pkg/util 96.8
$SELF_NEW/cmd/dhcp-handler 74.0
EOF
printf '%s/pkg/util 96.8\n' "$SELF_NEW" > "$DROP_HEAD"
RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$DROP_HEAD" \
    bash "$RATCHET" "$RENAME_OK" "$DROP_BASE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] \
   && grep -F "DROPPED  $SELF_NEW/cmd/dhcp-handler" "$TMP/out" > /dev/null \
   && ! grep -F "RENAMED  $SELF_NEW/cmd/dhcp-handler" "$TMP/out" > /dev/null; then
    echo "PASS: a row already under the head module is not re-spelled"
else
    echo "FAIL: the retry reached a row it must not (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- A RUN IN WHICH EVERY ROW DROPPED IS NOT A PASS (#979) ---------------
#
# DROPPED counts as compared, so "compared N of N" is true whether the
# floors were held or discharged. One deliberate deletion must not fail a
# release; a whole baseline going at once is the gate having lost its
# subject.
ALLGONE_BASE="$TMP/allgone-base.txt"
ALLGONE_HEAD="$TMP/allgone-head.txt"
cat > "$ALLGONE_BASE" <<EOF
$SELF_NEW/cmd/dhcp-handler 74.0
$SELF_NEW/pkg/gone-as-well 61.0
EOF
printf '# every row went\n' > "$ALLGONE_HEAD"
RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$ALLGONE_HEAD" \
    bash "$RATCHET" "$RENAME_OK" "$ALLGONE_BASE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 2 ] && grep -F 'Nothing left to ratchet' "$TMP/out" > /dev/null; then
    echo "PASS: a run in which every row DROPPED is refused"
else
    echo "FAIL: an all-dropped run was not refused (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# AT THE THRESHOLD, and the escape named in the script's comment: one row
# surviving is one comparison, and that run still passes. Driven so the
# refusal cannot quietly widen into "any deletion fails a release", which
# is the state the DROPPED arm exists to replace.
NEARLY_BASE="$TMP/nearly-base.txt"
NEARLY_HEAD="$TMP/nearly-head.txt"
cat > "$NEARLY_BASE" <<EOF
$SELF_NEW/cmd/dhcp-handler 74.0
$SELF_NEW/pkg/util 96.8
EOF
printf '%s/pkg/util 96.8\n' "$SELF_NEW" > "$NEARLY_HEAD"
RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$NEARLY_HEAD" \
    bash "$RATCHET" "$RENAME_OK" "$NEARLY_BASE" > "$TMP/out" 2>&1
got=$?
if [ "$got" -eq 0 ] && grep -F "PASS  $SELF_NEW/pkg/util: 97.3%" "$TMP/out" > /dev/null; then
    echo "PASS: one surviving comparison is still a ratchet"
else
    echo "FAIL: the all-dropped refusal fired on a single deletion (exit $got)"
    sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
fi

# --- THE PRE-FIX SCRIPTS, CUT OUT OF THE REAL ONE ------------------------
#
# Two surgeries, one per arm, so each is shown to be load-bearing on its
# own rather than as a pair. Built from $RATCHET itself; a kept copy of
# the block stops being the subject the moment the script moves on, and
# each surgery asserts it removed something.
#
# THEY LIVE BESIDE THE REAL SCRIPT, not in $TMP, and that is forced.
# coverage-ratchet.sh derives REPO_ROOT from its own path, and both arms
# below reach `go list` in that root; run from a temp directory the
# control probe refuses every classification and the cases measure the
# refusal instead of the arm. Dot-prefixed so the `test-*.sh` glob in
# run-gate-selftests.sh cannot pick them up, PID-suffixed so two runs do
# not collide, and removed by absolute path below.
SCRIPTS_DIR="$(cd "$(dirname "$RATCHET")" && pwd)"
PRE_ALL="$SCRIPTS_DIR/.prefix-all-$$.sh"        # neither arm: the script as it stood
PRE_NOREF="$SCRIPTS_DIR/.prefix-norefusal-$$.sh" # the retry, without the refusal
python3 - "$RATCHET" "$PRE_ALL" "$PRE_NOREF" <<'SURGERY'
import sys
src = open(sys.argv[1]).read()

retry_start = src.index('    # Missing under its own name is not the same as missing.')
retry_end = src.index("    # The row's identity at head is the renamed one where a rename", retry_start)
no_retry = src[:retry_start] + src[retry_end:]
assert no_retry != src, "the retry block is gone from the script: this control is inert"
no_retry, n = no_retry.replace('    at_head="${renamed:-$pkg}"\n',
                               '    at_head="$pkg"\n'), None
assert '${renamed:-$pkg}' not in no_retry, "the renamed identity survived the cut"

ref_start = src.index('if [ "$dropped" -ne 0 ] && [ "$dropped" -eq "$compared" ]; then')
ref_end = src.index('\nfi\n', ref_start) + len('\nfi\n')
refusal = src[ref_start:ref_end]
assert 'Nothing left to ratchet' in refusal, "the surgery cut the wrong block"

open(sys.argv[2], "w").write(no_retry.replace(refusal, ''))
open(sys.argv[3], "w").write(src.replace(refusal, ''))
SURGERY
prefix_built=$?

if [ "$prefix_built" -ne 0 ]; then
    echo "FAIL: the pre-fix controls could not be built from the real script"
    failures=$((failures + 1))
else
    rename_run "$PRE_ALL" "$RENAME_LOW"; got=$?
    if [ "$got" -eq 0 ] && grep -F "DROPPED  $SELF_OLD/pkg/util" "$TMP/out" > /dev/null; then
        echo "PASS: ...and the pre-fix script EXITS 0 on the same release PR (the defect)"
    else
        echo "FAIL: the pre-fix control did not reproduce the defect (exit $got)"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
    fi

    RATCHET_REPORT='' RATCHET_HEAD_BASELINE="$ALLGONE_HEAD" \
        bash "$PRE_NOREF" "$RENAME_OK" "$ALLGONE_BASE" > "$TMP/out" 2>&1
    got=$?
    if [ "$got" -eq 0 ]; then
        echo "PASS: ...and without the refusal an all-dropped run exits 0"
    else
        echo "FAIL: the all-dropped pre-fix control did not exit 0 (exit $got)"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
    fi
fi
/bin/rm -f "$PRE_ALL" "$PRE_NOREF"

if [ "$failures" -ne 0 ]; then
    echo "$failures ratchet test(s) failed"
    exit 1
fi
echo "All ratchet tests passed"
