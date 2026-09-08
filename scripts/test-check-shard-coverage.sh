#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-shard-coverage.sh.
#
# THE TWO CASES THAT EARNED THE GATE are the two edits a reshard makes,
# both measured against the real tree in review and both green under
# every gate that existed before this one:
#   * bump the count and forget the entry  (nine tests never scheduled)
#   * drop one entry and duplicate another (eight lost, nine doubled)
# Neither is sabotage; each is one sed away from a correct reshard.
#
# WHERE THE FIXTURES LIVE. Workflow-only cases get a copy of
# .github/workflows and reach the gate through SHARD_COVERAGE_WORKFLOWS.
# Cases that have to move the PARTITIONER get a copy of the whole
# tracked tree and run the COPY's gate, because the gate finds the
# partitioner beside itself. Nothing is edited in place: the gate corpus
# runs concurrently, so an in-place edit here reddens somebody else's
# gate (that happened once already, #879's round).
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
GATE="$HERE/check-shard-coverage.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
last=1
ok() { echo "ok   $1"; pass=$((pass + 1)); last=1; }
no() { echo "FAIL $1"; fail=$((fail + 1)); last=0; }

# run <label> <want-exit> <gate-path> <workflows-or-empty> [grep...]
run() {
    local label="$1" want="$2" gate="$3" wfdir="$4"; shift 4
    local out rc
    if [ -n "$wfdir" ]; then
        out=$(SHARD_COVERAGE_WORKFLOWS="$wfdir" bash "$gate" 2>&1); rc=$?
    else
        out=$(bash "$gate" 2>&1); rc=$?
    fi
    local good=1
    [ "$rc" -eq "$want" ] || good=0
    local pat
    for pat in "$@"; do
        printf '%s\n' "$out" | grep -- "$pat" > /dev/null || good=0
    done
    if [ "$good" -eq 1 ]; then
        ok "$label"
    else
        no "$label (want exit $want, got $rc)"
        printf '%s\n' "$out" | sed 's/^/     /'
    fi
}

wfcopy() {   # wfcopy <name> -> echoes the path to a fresh workflows copy
    local d="$TMP/$1"
    rm -rf "$d"; mkdir -p "$d"
    cp "$ROOT/.github/workflows/"*.yml "$ROOT/.github/workflows/"*.yaml "$d/" 2>/dev/null
    echo "$d"
}

treecopy() { # treecopy <name> -> echoes the path to a fresh tracked-tree copy
    local d="$TMP/$1"
    rm -rf "$d"; mkdir -p "$d"
    ( cd "$ROOT" && git ls-files -z | tar --null -T - -cf - ) | ( cd "$d" && tar -xf - )
    echo "$d"
}

# --- the control: the tree as it ships ------------------------------------
run "the shipping tree covers the roster from both lanes" 0 "$GATE" "" \
    "integration.yml schedules 11 shard(s) covering all 95 test(s) exactly once" \
    "integration-hosted.yml schedules 11 shard(s) covering all 95 test(s) exactly once"

# Every mutant below builds its input by EDITING this same tree, so a red control
# makes their verdicts describe the mutation on top of a tree that already fails.
# Measured 2026-09-08 driving mutant 1 through the lane: the control fired and
# took seven collateral cases down with it, and the one true line was ninth on
# screen. Nothing is suppressed -- the cases still run and still report -- but
# the reader is told which line to act on.
if [ "$last" -eq 0 ]; then
    echo "NOTE: the shipping-tree control above is RED. The mutant cases that follow edit"
    echo "      THIS tree, so their verdicts are collateral, not independent findings."
    echo "      Fix the schedule the control names; then read this corpus again."
fi

# --- MUTANT 1: the count moves, the entry does not ------------------------
d=$(wfcopy m1)
sed -i 's/OF=9 SUITE=main/OF=10 SUITE=main/g' "$d/integration.yml"
sed -i 's/-of-9"/-of-10"/g' "$d/integration-hosted.yml"
run "MUTANT: OF=9 -> OF=10 with no tenth entry is red on both lanes" 1 "$GATE" "$d" \
    "Tests no scheduled shard runs" \
    "TestLifecycleBridge_GoldenPath" \
    "integration.yml schedules 11 shard(s), and 9 of the 95" \
    "integration-hosted.yml schedules 11 shard(s), and 9 of the 95"

# The same edit in ONE file only. The two lanes now disagree, which the
# lane-agreement case in test-integration-shard.sh sees -- but this gate
# is not that check, and it must answer per lane: red for the file that
# was edited, green for the one that was not.
d=$(wfcopy m1a)
sed -i 's/OF=9 SUITE=main/OF=10 SUITE=main/g' "$d/integration.yml"
run "MUTANT: the same edit in one file only is red for that lane and green for the other" 1 "$GATE" "$d" \
    "Tests no scheduled shard runs::integration.yml" \
    "integration-hosted.yml schedules 11 shard(s) covering all 95 test(s) exactly once"

# --- MUTANT 2: one entry dropped, another duplicated ----------------------
d=$(wfcopy m2)
python3 - "$d" <<'PY'
import sys
d = sys.argv[1]
p = d + "/integration.yml"
s = open(p).read()
a = "          - suite: main-5\n            target: integration-test-shard SHARD=5 OF=9 SUITE=main\n"
b = "          - suite: main-4b\n            target: integration-test-shard SHARD=4 OF=9 SUITE=main\n"
assert s.count(a) == 1
open(p, "w").write(s.replace(a, b))
p = d + "/integration-hosted.yml"
s = open(p).read()
assert s.count('"main-5-of-9"') == 1
open(p, "w").write(s.replace('"main-5-of-9"', '"main-4-of-9"'))
PY
run "MUTANT: main-5 dropped and main-4 duplicated is red BOTH ways" 1 "$GATE" "$d" \
    "Tests no scheduled shard runs" \
    "TestV6Fixture_RefusesASegmentInAnotherModesShape" \
    "Tests more than one scheduled shard runs" \
    "9 test(s) twice or more"

# The duplicate arm alone, with nothing missing: eleven entries plus a
# twelfth that repeats main-4. Coverage is complete, so only the
# duplicate arm may fire -- which is what separates "this shard runs
# twice" from "that shard does not run".
d=$(wfcopy dup)
python3 - "$d" <<'PY'
import sys
d = sys.argv[1]
p = d + "/integration.yml"
s = open(p).read()
a = "          - suite: main-4\n            target: integration-test-shard SHARD=4 OF=9 SUITE=main\n"
assert s.count(a) == 1
open(p, "w").write(s.replace(a, a + a.replace("main-4", "main-4b")))
PY
run "a duplicated entry with nothing missing fires the duplicate arm alone" 1 "$GATE" "$d" \
    "Tests more than one scheduled shard runs" \
    "integration-hosted.yml schedules 11 shard(s) covering all 95 test(s) exactly once"
d_out=$(SHARD_COVERAGE_WORKFLOWS="$d" bash "$GATE" 2>&1)
if printf '%s\n' "$d_out" | grep "Tests no scheduled shard runs" > /dev/null; then
    no "and it does not also claim tests are missing"
else
    ok "and it does not also claim tests are missing"
fi

# --- a scheduled shard the partitioner cannot serve -----------------------
d=$(wfcopy oor)
sed -i 's/SHARD=9 OF=9 SUITE=main/SHARD=12 OF=9 SUITE=main/' "$d/integration.yml"
run "a scheduled shard outside the partition is named, not skipped" 1 "$GATE" "$d" \
    "A scheduled shard does not partition" \
    "schedules main shard 12 of 9"

# The refused shard ALONE, with coverage otherwise complete. An extra
# entry the partitioner cannot serve leaves no test missing and none
# duplicated, so nothing but this arm can make it red -- and a job that
# is guaranteed to fail at run time must not be scheduled green. Written
# after mutation: dropping `status=1` from that arm survived the case
# above, because there the out-of-range shard also took its tests with
# it and the missing arm carried the verdict.
d=$(wfcopy extra)
python3 - "$d" <<'PYX'
import sys
p = sys.argv[1] + "/integration.yml"
s = open(p).read()
a = "          - suite: main-9\n            target: integration-test-shard SHARD=9 OF=9 SUITE=main\n"
assert s.count(a) == 1
open(p, "w").write(s.replace(a, a + "          - suite: main-99\n            target: integration-test-shard SHARD=99 OF=9 SUITE=main\n"))
PYX
run "an extra shard the partitioner cannot serve is red on its own" 1 "$GATE" "$d" \
    "A scheduled shard does not partition" \
    "schedules main shard 99 of 9"
d_out=$(SHARD_COVERAGE_WORKFLOWS="$d" bash "$GATE" 2>&1)
if printf '%s\n' "$d_out" | grep -E "Tests (no scheduled shard runs|more than one scheduled shard runs)" > /dev/null; then
    no "and nothing else is claimed about it (coverage is complete)"
else
    ok "and nothing else is claimed about it (coverage is complete)"
fi

# --- REFUSALS: the gate must never be silent about not seeing -------------
run "a missing workflow directory refuses" 2 "$GATE" "$TMP/does-not-exist" \
    "No workflow directory"

d=$(wfcopy gone)
rm -f "$d/integration-hosted.yml"
run "a lane whose file is gone refuses rather than checking one lane" 2 "$GATE" "$d" \
    "A lane is missing"

d=$(wfcopy notriples)
python3 - "$d" <<'PY'
import sys, re
p = sys.argv[1] + "/integration.yml"
s = open(p).read()
s = re.sub(r"integration-test-shard SHARD=\d+ OF=\d+ SUITE=[a-z]+", "integration-test", s)
open(p, "w").write(s)
PY
run "a lane that schedules no shard refuses; an empty domain is not coverage" 2 "$GATE" "$d" \
    "A lane schedules no shard"

# --- the roster refusal, and what it is load-bearing for ------------------
#
# This is the case that keeps the "runs a test that is not in the
# roster" arm unreachable. Make the PARTITIONER stop seeing one test
# that the sources still declare: the gate must REFUSE (exit 2) and hand
# the finding to test-integration-shard.sh, not report a coverage red
# against a roster it cannot trust. Driven on a copy of the whole
# tracked tree, because the gate reads the partitioner beside itself.
d=$(treecopy roster)
python3 - "$d" <<'PY'
import sys
p = sys.argv[1] + "/scripts/integration-shard.sh"
s = open(p).read()
a = "        *)             [ \"$SUITE\" = main ]    && ALL+=(\"$t\") ;;\n"
b = "        TestLifecycleBridge_GoldenPath) : ;;\n" + a
assert s.count(a) == 1, s.count(a)
open(p, "w").write(s.replace(a, b))
PY
run "the partitioner losing a test the sources declare is a REFUSAL, not a coverage red" 2 \
    "$d/scripts/check-shard-coverage.sh" "" \
    "The partitioner and the sources disagree about the roster" \
    "TestLifecycleBridge_GoldenPath"

# --- preservation: an edit that changes no triple changes no verdict ------
d=$(wfcopy inert)
printf '\n# an added comment mentioning integration-test-shard and main-5-of-9\n' >> "$d/integration.yml"
printf '\n# an added comment\n' >> "$d/integration-hosted.yml"
run "PRESERVATION: a comment naming a shard does not schedule one" 0 "$GATE" "$d" \
    "integration.yml schedules 11 shard(s) covering all 95 test(s) exactly once"

# --- wiring: the gate is actually run by the lane and by local-lane.sh ----
if grep -q "check-shard-coverage.sh" "$ROOT/.github/workflows/test.yaml"; then
    ok "the lane runs this gate"
else
    no "the lane runs this gate"
fi
if grep -q "check-shard-coverage.sh" "$ROOT/scripts/local-lane.sh"; then
    ok "local-lane.sh runs this gate"
else
    no "local-lane.sh runs this gate"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
