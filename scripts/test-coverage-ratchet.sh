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

if [ "$failures" -ne 0 ]; then
    echo "$failures ratchet test(s) failed"
    exit 1
fi
echo "All ratchet tests passed"
