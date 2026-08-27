#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for scripts/check-counting-fallback.sh (#827).
#
# It drives the REAL gate through its SCAN_ROOT seam -- never a copy of
# its regex. A self-test that re-implements the pattern grades its own
# transcription, and would stay green through any edit to the gate.
#
# THE TWO CASES THAT MATTER MOST are the ones taken verbatim from the
# defects that motivated the gate: the line from
# test-check-attestation-parity.sh and the one from
# test-runner-register.sh, copied character for character. A gate built
# from a defect must be shown failing on that exact defect, not on a
# tidied paraphrase of it -- a fixture written from scratch can be red
# for a reason the original never had.
#
# The preservation cases are equally load-bearing. `|| true` is CORRECT
# code with a live instance in the tree, and a gate that reddened it
# would push somebody toward the wrong fix. A widening needs a control
# proving what it did NOT widen onto.
#
# Usage: bash scripts/test-check-counting-fallback.sh
# Exit:  0 all passed, 1 one or more failed.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-counting-fallback.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

n=0
fail=0

# Run the gate over a directory holding one fixture file.
# $1 label  $2 expected exit  $3 file content
case_is() {
    local label="$1" want="$2" body="$3"
    local d="$TMP/case$n"
    mkdir -p "$d"
    printf '%s\n' "$body" > "$d/fixture.sh"
    n=$((n + 1))
    local out rc
    out=$(SCAN_ROOT="$d" bash "$GATE" 2>&1); rc=$?
    if [ "$rc" -eq "$want" ]; then
        echo "PASS: $label (exit $rc)"
    else
        echo "FAIL: $label -- want exit $want, got $rc"
        printf '%s\n' "$out" | sed 's/^/       /'
        fail=$((fail + 1))
    fi
}

echo "--- the two motivating defects, verbatim"

# From scripts/test-check-attestation-parity.sh before the fix. This is
# the line whose "0\n0" made an -eq error out and left the zero-call
# diagnostic unreachable.
case_is "the attestation-parity defect, as written" 1 \
'    c=$(grep -c . "$1" 2>/dev/null || echo 0)'

# From scripts/test-runner-register.sh:107 before the fix.
case_is "the runner-register defect, as written" 1 \
'got=$(grep -c . "$TMP/first/state/.credentials_rsaparams" 2>/dev/null || echo 0)'

echo "--- the shape, in spellings the defect did not happen to use"

case_is "backtick substitution" 1 \
'n=`grep -c . "$f" || echo 0`'

case_is "--count spelled long" 1 \
'n=$(grep --count . "$f" || echo 0)'

case_is "bundled flags (-rc)" 1 \
'n=$(grep -rc pattern "$d" || echo 0)'

case_is "printf as the fallback" 1 \
'n=$(grep -c . "$f" || printf 0)'

echo "--- preservation: correct code that must stay green"

# The live instance in scripts/test-runner-register.sh. `true` prints
# nothing, so grep's own `0` is the whole substitution. Correct.
case_is "|| true is correct and stays green" 0 \
'got=$(grep -c TOKENSECRET "$TMP/first/log" || true)'

case_is "|| : is correct and stays green" 0 \
'got=$(grep -c X "$f" || :)'

# wc exits 0 on empty input, so the fallback never fires: one printer.
case_is "wc -l with a fallback is not the hazard" 0 \
'n=$(wc -l < "$f" || echo 0)'

case_is "the corrected form itself" 0 \
'n=$(grep -c . "$f" 2>/dev/null) || n=0'

# WITHOUT -c THERE IS NO SECOND PRINTER. A plain grep prints NOTHING
# when it matches nothing, so a printing fallback is the only output and
# supplying a default is exactly right. This is the case that pins the
# `-c` half of the pattern: drop that requirement from the gate and this
# line reddens, which is how the suite can tell a counting grep from an
# ordinary one.
case_is "plain grep with a printing fallback is correct" 0 \
'v=$(grep -o pattern "$f" || echo none)'

case_is "plain grep, backticks, printing fallback" 0 \
'v=`grep -m1 x "$f" || echo fallback`'

# Both fixed files carry a comment quoting the bad form directly above
# the fixed line. If those reddened, the fix could not land.
case_is "a comment quoting the bad form" 0 \
'# the obvious $(grep -c . f || echo 0) fires BOTH halves'

echo "--- the gate cannot pass having examined nothing"

# THE VACUITY CASE. A scan of an empty domain must REFUSE, not report a
# clean tree. This is the gate's own version of the defect it hunts:
# a green that means "nothing was asked".
mkdir -p "$TMP/empty"
n=$((n + 1))
out=$(SCAN_ROOT="$TMP/empty" bash "$GATE" 2>&1); rc=$?
if [ "$rc" -eq 2 ] && grep -q 'examined nothing' <<< "$out"; then
    echo "PASS: an empty domain refuses rather than reporting clean (exit 2)"
else
    echo "FAIL: an empty domain -- want exit 2 naming the vacuity, got $rc"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
fi

# An unreadable seam must refuse too, for the same reason.
n=$((n + 1))
out=$(SCAN_ROOT="$TMP/does-not-exist" bash "$GATE" 2>&1); rc=$?
if [ "$rc" -eq 2 ]; then
    echo "PASS: a missing scan root refuses (exit 2)"
else
    echo "FAIL: a missing scan root -- want exit 2, got $rc"
    fail=$((fail + 1))
fi

echo "--- the real tree is a real population"

# NON-VACUITY ON THE LIVE RUN. The gate reports how many files it
# examined; a clean verdict over a suspiciously small domain is the
# failure this asserts against. The tracked shell corpus is ~171 files;
# anything under 50 means the domain derivation broke, and a green
# result would be meaningless.
n=$((n + 1))
out=$(cd "$HERE/.." && bash "$GATE" 2>&1); rc=$?
seen=$(sed -n 's/.*clean, \([0-9]*\) shell file.*/\1/p' <<< "$out")
if [ "$rc" -eq 0 ] && [ -n "$seen" ] && [ "$seen" -ge 50 ]; then
    echo "PASS: the live tree is clean over a real domain ($seen files)"
else
    echo "FAIL: the live tree -- rc=$rc, files examined='${seen:-none}' (want clean over >=50)"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
fi

echo
if [ "$fail" -gt 0 ]; then
    echo "$fail of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
