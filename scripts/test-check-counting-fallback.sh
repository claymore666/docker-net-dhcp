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
#
# IT READS THE MESSAGE, NOT ONLY THE STATUS, AND THAT IS THE WHOLE CASE.
# This asserted `rc -eq 2` alone, and the sibling case one block up --
# which does read its message -- is what made the gap visible. Measured
# 2026-08-28 at the pre-fix head: mutate domain()'s `[ -d "$SCAN_ROOT" ]
# || return 1` to `return 0` and the suite stayed 18/18 green. The
# mutant is not harmless: with the refusal gone, domain() returns an
# empty list, the NON-VACUITY guard downstream refuses instead, rc is
# still 2 -- and the entire `cannot measure::SCAN_ROOT ... is not a
# directory` block becomes unreachable dead code. A DIFFERENT GUARD
# caught it, which is exactly the reading that makes a surviving mutant
# look like a covered one.
#
# So the assertion is keyed on WHICH refusal fired. `examined nothing`
# and `cannot measure::SCAN_ROOT` are different verdicts about different
# faults, and a status code cannot tell them apart.
n=$((n + 1))
out=$(SCAN_ROOT="$TMP/does-not-exist" bash "$GATE" 2>&1); rc=$?
if [ "$rc" -eq 2 ] \
   && grep -qF 'cannot measure::SCAN_ROOT' <<< "$out" \
   && grep -qF 'is not a directory' <<< "$out"; then
    echo "PASS: a missing scan root refuses AS a missing scan root (exit 2)"
else
    echo "FAIL: a missing scan root -- want exit 2 naming SCAN_ROOT, got $rc"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
fi

echo "--- a member the scan could not read is a refusal, not a clean count"

# THE PARTIAL VACUITY CASE. The guard above closes the all-or-nothing
# shape: zero files must refuse. It did not close the shape where the
# domain is populated and the SCAN drops members out of it. Measured
# 2026-08-28 at the pre-fix head: one readable file plus one mode-000
# file carrying the hazard produced `counting-fallback: clean, 2 shell
# file(s) examined.` and exit 0 -- the count came from the domain, the
# hazard was never read, and the number beside "clean" read as evidence.
#
# THE CONSTRUCTION DEPENDS ON PRIVILEGE, AND THAT IS THE TRAP. `chmod
# 000` denies nothing to root, and root is normal in two of the places
# this suite runs -- a workstation `local-lane.sh`, and the root
# containers of the self-hosted pool. A case that chmods and asserts is
# VACUOUS there. So the two arms below drive the property -- a counted
# member the scan could not read -- from both directions, and the
# privilege-independent one runs unconditionally.

# ARM 1: NOT A REGULAR FILE. Uid-independent, because no capability
# opens a path that is not there. `find | sort` is newline-delimited, so
# a member whose NAME carries a newline arrives as two paths that do not
# exist -- both counted into the domain, neither readable. The hazard in
# it must not be reported as absent.
n=$((n + 1))
split="$TMP/split"
mkdir -p "$split"
printf 'echo ok\n' > "$split/plain.sh"
printf '%s\n' 'n=$(grep -c . "$f" || echo 0)' > "$split/carries a
hazard.sh"
out=$(SCAN_ROOT="$split" bash "$GATE" 2>&1); rc=$?
if [ "$rc" -eq 2 ] \
   && grep -qF 'could not examine a domain member' <<< "$out" \
   && grep -qF "$split/carries a" <<< "$out" \
   && ! grep -qF 'clean,' <<< "$out"; then
    echo "PASS: a domain member that is not a regular file refuses, named (exit 2)"
else
    echo "FAIL: an unexaminable member -- want exit 2 naming the file, got $rc"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
fi

# ARM 2: COUNTED BUT UNOPENABLE -- the construction the defect was
# measured with, and the one the fix's explicit `exec 3<` exists for.
#
# THE FIXTURE IS PROBED, NOT ASSUMED. `chmod 000` is a statement about
# permission bits; what this case needs is a statement about THIS
# process. So it opens the fixture the way the gate does and asserts on
# the result. If the open succeeds -- a privileged runner -- the fixture
# cannot express the condition and the case says so and drives the
# unopenable property through the arm that does not need permission,
# rather than skipping. A skip is how the root case would ship untested,
# and a sibling PR is red in CI right now for exactly that.
n=$((n + 1))
noperm="$TMP/noperm"
mkdir -p "$noperm"
printf 'echo ok\n' > "$noperm/plain.sh"
printf '%s\n' 'n=$(grep -c . "$f" || echo 0)' > "$noperm/locked.sh"
chmod 000 "$noperm/locked.sh"
if { : < "$noperm/locked.sh"; } 2>/dev/null; then
    # Privileged: mode 000 denies this process nothing. Re-aim the same
    # assertion at a member no capability can open.
    arm="unopenable-by-anyone (privileged: mode 000 is vacuous here)"
    chmod 644 "$noperm/locked.sh"
    mv "$noperm/locked.sh" "$noperm/locked
by-name.sh"
    marker="$noperm/locked"
else
    arm="mode 000 (measured: this process cannot open it)"
    marker="$noperm/locked.sh"
fi
out=$(SCAN_ROOT="$noperm" bash "$GATE" 2>&1); rc=$?
if [ "$rc" -eq 2 ] \
   && grep -qF 'could not examine a domain member' <<< "$out" \
   && grep -qF "$marker" <<< "$out" \
   && ! grep -qF 'clean,' <<< "$out"; then
    echo "PASS: an unreadable domain member refuses, named -- $arm (exit 2)"
else
    echo "FAIL: an unreadable domain member -- want exit 2 naming the file, got $rc"
    echo "       arm: $arm"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
fi

# THE PRESERVATION CONTROL FOR BOTH ARMS. A refusal that fires on every
# domain is a gate that measures nothing, and the two arms above are a
# widening. This is the same directory shape with nothing wrong with it:
# it must still be examined and still report the examined count.
n=$((n + 1))
okdir="$TMP/allreadable"
mkdir -p "$okdir"
printf 'echo ok\n' > "$okdir/one.sh"
printf 'echo also ok\n' > "$okdir/two.sh"
out=$(SCAN_ROOT="$okdir" bash "$GATE" 2>&1); rc=$?
if [ "$rc" -eq 0 ] && grep -qF 'clean, 2 shell file(s) examined' <<< "$out"; then
    echo "PASS: two readable members are examined and counted as two (exit 0)"
else
    echo "FAIL: the readable control -- want exit 0 and 'clean, 2', got $rc"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
fi

# THE COUNT IS WHAT WAS EXAMINED, NOT THE DOMAIN SIZE. Without this the
# fix could report the domain size beside "clean" and stay green: the
# refusal above only fires when a member is dropped, and this pins the
# number in the case where none is. Three members, one of them the gate
# would have to drop, must never yield a clean three.
n=$((n + 1))
mixed="$TMP/mixed"
mkdir -p "$mixed"
printf 'echo ok\n' > "$mixed/one.sh"
printf 'echo ok\n' > "$mixed/two.sh"
printf 'echo ok\n' > "$mixed/three
split.sh"
out=$(SCAN_ROOT="$mixed" bash "$GATE" 2>&1); rc=$?
if [ "$rc" -eq 2 ] && grep -qF '(2 examined,' <<< "$out"; then
    echo "PASS: the report counts what was examined, not the domain (2 of 4)"
else
    echo "FAIL: the examined count -- want exit 2 reporting '2 examined', got $rc"
    printf '%s\n' "$out" | sed 's/^/       /'
    fail=$((fail + 1))
fi

echo "--- the real tree is a real population"

# NON-VACUITY ON THE LIVE RUN. The gate reports how many files it
# examined; a clean verdict over a suspiciously small domain is the
# failure this asserts against. The tracked shell corpus was 184 files
# measured 2026-08-28 over base 9ae67ca; anything under 50 means the
# domain derivation broke, and a green result would be meaningless.
#
# THE FLOOR IS THE LOAD-BEARING HALF, THE NUMBER IS NOT, and that is
# worth saying because it changes what a mutant here proves. Reverting
# this dated corpus figure alone leaves the suite green -- the assertion
# reads `>= 50`, so 177, 182 and 184 are the same fact to it. Deleting
# the floor is what reddens. The date is here so a reader can tell a
# corpus that grew from a domain derivation that broke, not so a check
# can compare against it.
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

echo "--- the exclusion's boundary, asserted rather than described"

# PRESERVATION CONTROL FOR THE EXCLUSION. The gate drops exactly two
# paths -- itself and this file -- and the one way that could go wrong
# quietly is spelling it as a pattern: BOTH defects the gate was built
# from live in `test-*.sh` files, so `test-*.sh` would excuse the gate's
# entire reason for existing while looking like tidier code. This asserts
# membership directly through the --list-domain seam, so the boundary is
# a check and not a paragraph.
n=$((n + 1))
dom=$(cd "$HERE/.." && bash "$GATE" --list-domain 2>&1); rc=$?
missing=""
for must in scripts/test-check-attestation-parity.sh scripts/test-runner-register.sh; do
    grep -qxF "$must" <<< "$dom" || missing="$missing $must"
done
present=""
for must_not in scripts/check-counting-fallback.sh scripts/test-check-counting-fallback.sh; do
    grep -qxF "$must_not" <<< "$dom" && present="$present $must_not"
done
if [ "$rc" -eq 0 ] && [ -z "$missing" ] && [ -z "$present" ]; then
    echo "PASS: the excluded pair is out and both defect files are still in"
else
    echo "FAIL: the exclusion's boundary moved -- rc=$rc"
    [ -n "$missing" ] && echo "       dropped from the domain, must NOT be:$missing"
    [ -n "$present" ] && echo "       still in the domain, must NOT be:$present"
    fail=$((fail + 1))
fi

echo "--- the excluded pair is observed, not trusted"

# THE OBSERVER OVER THE EXCLUSION. An exclusion is a blind spot, and this
# project has already paid for one. So the two excluded files are scanned
# DELIBERATELY, through the same SCAN_ROOT seam, and every finding is
# matched against a declared set.
#
# A SET, NOT A COUNT. A count only holds at the threshold: add a live
# hazard as real code and delete a fixture in the same commit and the
# total is unchanged while the gate's own script now carries the defect
# it exists to catch. These are exactly the two files whose fixtures
# churn, because they are what you edit to extend the gate. A member
# nobody declared goes red whatever else moved.
#
# KEYED ON PATH PLUS A DIGEST OF THE MATCHED TEXT. Path-plus-line would
# break every time this prose is reflowed. The text itself cannot be
# written here literally -- quoting the seven fixtures in this file would
# create seven more of them, and the set would chase its own tail -- so
# it is pinned by digest with the fixture named beside it in words. To
# re-derive after a deliberate fixture change, run the gate over the pair
# and read the sha256 of each reported line.
n=$((n + 1))
pair="$TMP/selfpair"
mkdir -p "$pair"
cp "$GATE" "$HERE/$(basename "$0")" "$pair/"
got=$(SCAN_ROOT="$pair" bash "$GATE" 2>&1 \
      | sed -n 's|^::error file=\([^,]*\),.*::[^`]*`\(.*\)` -- the primary.*|\1\t\2|p' \
      | while IFS= read -r rec; do
            printf '%s  %s\n' "$(basename "${rec%%$'\t'*}")" \
                "$(printf '%s' "${rec#*$'\t'}" | sha256sum | cut -c1-12)"
        done | sort)
want=$(sort <<'WANT'
test-check-counting-fallback.sh  26d09ee9d08b
test-check-counting-fallback.sh  98e7b9a4e161
test-check-counting-fallback.sh  a3e9afed947d
test-check-counting-fallback.sh  13e20ff8cf3e
test-check-counting-fallback.sh  7e1f40b36b53
test-check-counting-fallback.sh  ca31b9a6b5db
test-check-counting-fallback.sh  ea26377731e1
test-check-counting-fallback.sh  2e1f1e448525
test-check-counting-fallback.sh  ff789c8d27d0
WANT
)
# 26d09ee  the attestation-parity defect, verbatim
# 98e7b9a  the runner-register defect, verbatim
# a3e9afe  the backtick spelling
# 13e20ff  --count spelled long
# 7e1f40b  bundled flags (-rc)
# ca31b9a  printf as the fallback
# ea26377  the comment quoting the bad form
# 2e1f1e4  the newline-named fixture (arm 1 of the unexaminable case)
# ff789c8  the mode-000 fixture (arm 2 of the unexaminable case)
#
# The last two were added when the unexaminable-member cases landed, and
# the set going red on them is the mechanism working: a fixture carrying
# the hazard into an EXCLUDED file has to be declared by a person, not
# absorbed by a count.
#
# check-counting-fallback.sh contributes NOTHING to this set and that is
# asserted by its absence: every line in it that carries the shape is a
# comment, and comment-only lines are skipped. A finding attributed to
# the gate script is a live hazard in the gate itself.
undeclared=$(comm -23 <(printf '%s\n' "$got") <(printf '%s\n' "$want"))
absent=$(comm -13 <(printf '%s\n' "$got") <(printf '%s\n' "$want"))
if [ -z "$undeclared" ] && [ -z "$absent" ]; then
    echo "PASS: the excluded pair holds exactly the declared fixtures"
else
    echo "FAIL: the excluded pair no longer matches its declared set"
    [ -n "$undeclared" ] && {
        echo "       UNDECLARED -- a hazard nobody registered, in a file the"
        echo "       gate does not scan. Treat as live until shown to be a fixture:"
        printf '%s\n' "$undeclared" | sed 's/^/         /'
    }
    [ -n "$absent" ] && {
        echo "       DECLARED BUT GONE -- a fixture left; re-derive the digest"
        echo "       if that was deliberate:"
        printf '%s\n' "$absent" | sed 's/^/         /'
    }
    fail=$((fail + 1))
fi

echo
if [ "$fail" -gt 0 ]; then
    echo "$fail of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
