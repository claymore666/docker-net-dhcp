#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-pool-facts.sh (#879), driven through the
# --root seam against synthetic git trees.
#
# The cases that matter most are the refusals. This gate's whole subject
# is numbers that decayed because nothing looked at them, so a gate that
# passes when it cannot see would reproduce the defect one level up: an
# empty domain, an unreadable constant, a workflow whose pool job it
# failed to find, all exit 2 and none of them exit 0.
#
# The derivation is driven rather than asserted: the last group changes
# the SUITE MATRIX and requires the previously-green marker to go red,
# which is what distinguishes a gate keyed on the derivation from one
# keyed on today's spelling.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-pool-facts.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# THE MARKER TOKEN IS BUILT, NEVER SPELLED. This file is tracked, so a
# literal `ci-pool: <fact>=<value>` in a fixture string would be read by
# the gate as a claim THIS REPOSITORY makes — a deliberately stale
# fixture would then redden the real tree. Assembling the token at
# runtime keeps the fixtures out of the gate's domain without the gate
# having to know this file exists.
MK="ci-pool"
# For the same reason the fixture PROSE is assembled rather than
# spelled: the backstop's own statement shapes would match this file's
# test data and report it as an unmarked claim.
P="pool"
J="jobs"

# check NAME WANT_EXIT ROOT [WANT_GREP]
check() {
    local name="$1" want_exit="$2" root="$3" want_grep="${4:-}"
    bash "$GATE" --root "$root" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && { [ -z "$want_grep" ] || grep -q -- "$want_grep" "$TMP/out"; }; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit${want_grep:+, wanted /$want_grep/})"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# A synthetic tree: a constant, an integration workflow with a 3-entry
# suite matrix on the pool label, a coverage workflow with one, and a
# prose file. Everything is committed, because the gate's file list is
# `git ls-files` -- an untracked file is deliberately outside its domain.
tree() { # <name> [runners] [matrix entries]
    local d="$TMP/$1" runners="${2:-16}" entries="${3:-3}"
    mkdir -p "$d/.github/workflows"
    cat > "$d/.github/ci-pool.json" <<JSON
{"x64_label": "dhcp-ci", "x64_runners": $runners,
 "arm64_label": "dhcp-ci-arm64", "arm64_runners": 1}
JSON
    {
        echo "name: integration"
        echo "on: [push]"
        echo "jobs:"
        echo "  gate:"
        echo "    runs-on: ubuntu-latest"
        echo "    steps: [{run: 'true'}]"
        echo "  suite:"
        echo "    runs-on: dhcp-ci"
        echo "    strategy:"
        echo "      matrix:"
        echo "        include:"
        local i
        for i in $(seq 1 "$entries"); do echo "          - {shard: $i}"; done
        echo "    steps: [{run: 'true'}]"
    } > "$d/.github/workflows/integration.yml"
    cat > "$d/.github/workflows/coverage.yml" <<'YML'
name: coverage
on: [push]
jobs:
  coverage:
    runs-on: dhcp-ci
    steps: [{run: 'true'}]
YML
    git -C "$d" init -q
    git -C "$d" add -A
    echo "$d"
}

prose() { # <tree> <lines...>
    local d="$1"; shift
    printf '%s\n' "$@" > "$d/NOTES.md"
    git -C "$d" add -A
}

# --- refusals: it cannot see -------------------------------------------

d=$(tree noconst); rm -f "$d/.github/ci-pool.json"
check "no pool constant is 'cannot see', not a pass" 2 "$d" "No pool constant"

d=$(tree badjson); echo 'not json' > "$d/.github/ci-pool.json"
check "an unparseable constant is 'cannot see'" 2 "$d" "Pool constant unreadable"

d=$(tree nolabel); printf '{"x64_runners": 16}\n' > "$d/.github/ci-pool.json"
check "a constant with no label is 'cannot see'" 2 "$d" "declares no x64_label"

d=$(tree zerorunners); printf '{"x64_label": "dhcp-ci", "x64_runners": 0}\n' > "$d/.github/ci-pool.json"
check "a constant with a non-positive count is 'cannot see'" 2 "$d" "want a positive integer"

d=$(tree nowf); rm -rf "$d/.github/workflows"
check "no workflow directory is 'cannot see'" 2 "$d" "No workflow directory"

d=$(tree badwf); echo 'jobs: [' > "$d/.github/workflows/integration.yml"
check "an unparseable workflow is 'cannot see'" 2 "$d" "Workflow unparseable"

d=$(tree nojobs); printf 'name: x\non: [push]\n' > "$d/.github/workflows/integration.yml"
check "a workflow with no jobs mapping is 'cannot see'" 2 "$d" "declares no jobs"

# THE UNIVERSAL GATE, EMPTIED. Take the pool label off the suite job and
# the derivation is 0 -- which would compare equal to nothing and pass by
# saying nothing. It must refuse instead.
d=$(tree unlabelled)
sed -i 's/^    runs-on: dhcp-ci$/    runs-on: ubuntu-latest/' "$d/.github/workflows/integration.yml"
check "a workflow that puts nothing on the pool is 'cannot see'" 2 "$d" "No pool job found"

d=$(tree bothshapes)
python3 - "$d/.github/workflows/integration.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read().replace("      matrix:\n        include:\n",
                           "      matrix:\n        os: [a, b]\n        include:\n")
open(p, "w").write(s)
PY
check "a matrix with both axes and include is refused, not guessed at" 2 "$d" "Matrix shape not modelled"

d=$(tree nomarker)
check "no marker anywhere is 'cannot see' — it would pass having checked nothing" 2 "$d" \
      "No pool fact is stated anywhere"

# --- red: a stated number disagrees ------------------------------------

d=$(tree stale); prose "$d" "the $P is 16 ${P}s" "<!-- ${MK}: pool-runners=8 -->"
check "a stale pool size is red, and is named" 1 "$d" "pool-runners=8, derived 16"

d=$(tree stalejobs); prose "$d" "<!-- ${MK}: integration-pool-jobs=6 -->"
check "a stale job count is red, and is named" 1 "$d" "integration-pool-jobs=6, derived 3"

d=$(tree unknownfact); prose "$d" "<!-- ${MK}: pool-colour=16 -->"
check "a marker naming a fact nothing derives is red" 1 "$d" "Unknown pool fact"

# --- red: an unmarked statement (the spelling-keyed backstop) ----------

d=$(tree unmarked); prose "$d" "<!-- ${MK}: pool-runners=16 -->" "" "It runs against a $P of 16."
check "an unmarked 'pool of N' is red" 1 "$d" "An unmarked pool statement"

d=$(tree unmarkedword); prose "$d" "<!-- ${MK}: pool-runners=16 -->" "" "a $P of sixteen"
check "the backstop reads number-words too" 1 "$d" "An unmarked pool statement"

d=$(tree unmarkedjobs); prose "$d" "<!-- ${MK}: pool-runners=16 -->" "" "each run puts 11 ${J} on the $P"
check "an unmarked 'N jobs on the pool' is red" 1 "$d" "An unmarked pool statement"

# --- green: both directions --------------------------------------------

d=$(tree okmarks); prose "$d" "<!-- ${MK}: pool-runners=16 -->" \
    "<!-- ${MK}: integration-pool-jobs=3 -->" "<!-- ${MK}: coverage-pool-jobs=1 -->"
check "correct markers pass" 0 "$d" "3 marker(s) agree"

d=$(tree inlinemark); prose "$d" "It runs against a $P of 16. <!-- ${MK}: pool-runners=16 -->"
check "a marker on the statement's own line satisfies the backstop" 0 "$d" "1 marker(s) agree"

d=$(tree abovemark); prose "$d" "<!-- ${MK}: pool-runners=16 -->" "It runs against a $P of 16."
check "a marker on the line above satisfies the backstop" 0 "$d" "1 marker(s) agree"

d=$(tree exempted); prose "$d" "<!-- ${MK}: pool-runners=16 -->" \
    "A $P of three addresses. <!-- ${MK}-exempt: a DHCP pool -->"
check "an exempt line is not a claim about this pool" 0 "$d" "1 marker(s) agree"

d=$(tree untracked); prose "$d" "<!-- ${MK}: pool-runners=16 -->"
echo "a $P of 99" > "$d/UNTRACKED.md"
check "an untracked file is outside the domain, and says so by staying green" 0 "$d" "1 marker(s) agree"

# NO PATH IS OUTSIDE THE DOMAIN. Every tracked file is read, however
# deep, so an unmarked pool sentence is a finding wherever it is written.
d=$(tree anydepth); prose "$d" "<!-- ${MK}: pool-runners=16 -->"
mkdir -p "$d/internal/other"
echo "// A $P of three addresses" > "$d/internal/other/x_test.go"
git -C "$d" add -A
check "an unmarked pool sentence deep in the tree is red" 1 "$d" "internal/other/x_test.go"

# --- the derivation, driven ---------------------------------------------
#
# A gate keyed on today's spelling reproduces its own silence. So the
# subject moves: the same marker that was green at three matrix entries
# must go red at four, without the marker or the gate changing.

d=$(tree derived 16 3); prose "$d" "<!-- ${MK}: integration-pool-jobs=3 -->"
check "PRESERVATION: three matrix entries, marker says 3, green" 0 "$d" "integration-pool-jobs=3"
python3 - "$d/.github/workflows/integration.yml" <<'PY'
import sys
p = sys.argv[1]
open(p, "a")
s = open(p).read().replace("          - {shard: 3}\n", "          - {shard: 3}\n          - {shard: 4}\n")
open(p, "w").write(s)
PY
git -C "$d" add -A
check "MUTANT: a fourth matrix entry turns the same marker red" 1 "$d" \
      "integration-pool-jobs=3, derived 4"

# --- the tree that actually ships ---------------------------------------

REPO="$(cd "$HERE/.." && pwd)"
check "the real tree passes" 0 "$REPO" "marker(s) agree with the derivation"

# DRIVE THE ABSENCE against the shipped tree: delete a marker's subject
# and the shipped prose must go red.
#
# ON A COPY, NOT IN PLACE, and that is not fastidiousness. The gate
# corpus runs its self-tests CONCURRENTLY since D41, so a self-test that
# edits the working tree -- even for a moment, even restoring it after --
# can redden an unrelated gate reading the same file. MEASURED: this case
# did exactly that on its first run in the corpus.
SHIPPED="$TMP/shipped"
mkdir -p "$SHIPPED"
git -C "$REPO" ls-files -z | tar -C "$REPO" --null -T - -cf - | tar -C "$SHIPPED" -xf -
git -C "$SHIPPED" init -q
git -C "$SHIPPED" add -A
check "PRESERVATION: a copy of the shipped tree passes" 0 "$SHIPPED" "marker(s) agree"

wf="$SHIPPED/.github/workflows/integration.yml"
cp "$wf" "$TMP/integration.yml.bak"
python3 - "$wf" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
# Remove one matrix entry from the suite job.
s = re.sub(r"\n *- suite: [^\n]*\n *target: [^\n]*", "", s, count=1)
open(p, "w").write(s)
PY
if cmp -s "$wf" "$TMP/integration.yml.bak"; then
    echo "FAIL: the absence drive did not change the workflow — the matrix shape moved and this case is now vacuous"
    failures=$((failures + 1))
else
    check "ABSENCE: dropping a shard from the matrix reddens the shipped prose" 1 "$SHIPPED" \
          "integration-pool-jobs=11, derived 10"
fi
cp "$TMP/integration.yml.bak" "$wf"
check "RESTORED: green again" 0 "$SHIPPED" "marker(s) agree with the derivation"

# --- wiring --------------------------------------------------------------

if grep -q 'check-pool-facts.sh' "$REPO/.github/workflows/test.yaml"; then
    echo "PASS: gate is wired into .github/workflows/test.yaml"
else
    echo "FAIL: gate is not run by .github/workflows/test.yaml"
    failures=$((failures + 1))
fi
if grep -q 'check-pool-facts.sh' "$REPO/scripts/local-lane.sh"; then
    echo "PASS: gate is wired into scripts/local-lane.sh"
else
    echo "FAIL: gate is not run by scripts/local-lane.sh"
    failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures failure(s)"
    exit 1
fi
echo "all passed"
