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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-pool-facts.sh"
guarded_tmpdir TMP

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

# --- --live: the declared pool against the registered runners (#886) ---
#
# `gh` is a stub on PATH that answers the Nth call with $STUB/N (first line
# its exit code, the rest its stdout), so the refusals are driven, not
# assumed. Pages are built by `runners`, never spelled, for the reason MK is.

mkdir -p "$TMP/bin"
cat > "$TMP/bin/gh" <<'GH'
#!/usr/bin/env bash
n=$(( $(cat "$STUB/count" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$STUB/count"
printf '%s\n' "$*" >> "$STUB/argv"
f="$STUB/$n"; [ -e "$f" ] || f="$STUB/last"
tail -n +2 "$f"
rc=$(head -n 1 "$f")
[ "$rc" -eq 0 ] || echo "gh: stub failure $rc" >&2
exit "$rc"
GH
chmod +x "$TMP/bin/gh"

# runners TOTAL NAME=LABEL[,LABEL] ... prints a runners API page
runners() {
    python3 - "$@" <<'PY'
import json, sys
total, rs = int(sys.argv[1]), []
for i, spec in enumerate(sys.argv[2:]):
    name, labels = spec.split("=")
    rs.append({"id": 100 + i, "name": name, "status": "online",
               "labels": [{"name": n} for n in ["self-hosted"] + labels.split(",")]})
print(json.dumps({"total_count": total, "runners": rs}))
PY
}
X3=(a=dhcp-ci b=dhcp-ci c=dhcp-ci p=dhcp-ci-arm64)

# live NAME WANT_EXIT ROOT WANT_GREP READS RESPONSE... (a response is
# "RC" followed by its stdout lines, written as one argument with \n)
live() {
    local name="$1" want_exit="$2" root="$3" want_grep="$4" reads="$5" i=0
    shift 5
    STUB="$TMP/stub-$((++stubs))"; mkdir -p "$STUB"
    for r in "$@"; do i=$((i + 1)); printf '%b\n' "$r" > "$STUB/$i"; cp "$STUB/$i" "$STUB/last"; done
    STUB="$STUB" PATH="$TMP/bin:$PATH" GATE_REPO=o/r CI_POOL_LIVE_GAP=0 \
        CI_POOL_LIVE_READS="$reads" bash "$GATE" --live --root "$root" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && grep -q -- "$want_grep" "$TMP/out"; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit, wanted /$want_grep/)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}
stubs=0
okpage="0\n$(runners 4 "${X3[@]}")"

d=$(tree live3 3); prose "$d" "<!-- ${MK}: pool-runners=3 -->"
live "live: the registered pool matches the file, and an arm64 label is not counted as dhcp-ci" \
     0 "$d" "3 carry 'dhcp-ci' over 1 read" 1 "$okpage"
if grep -q -- '^api repos/o/r/actions/runners?per_page=100 --paginate$' "$STUB/argv"; then
    echo "PASS: live: it asks the runners API of GATE_REPO, every page"
else
    echo "FAIL: live: the stub saw $(cat "$STUB/argv")"; failures=$((failures + 1))
fi

d=$(tree live4 4); prose "$d" "<!-- ${MK}: pool-runners=4 -->"
live "ABSENCE: a facts file one runner over the registered pool is red, and named" \
     1 "$d" "declares x64_runners=4; o/r has 3 runner name(s)" 4 "$okpage"
live "live: an arm64 runner missing from the API is red" \
     1 "$d" "declares arm64_runners=1" 1 "0\n$(runners 4 a=dhcp-ci b=dhcp-ci c=dhcp-ci d=dhcp-ci)"

d=$(tree live3b 3); prose "$d" "<!-- ${MK}: pool-runners=3 -->"
live "live: an API that cannot be read is a refusal, not a match" \
     2 "$d" "Live pool unreadable" 1 '1\n{"message":"Bad credentials","status":"401"}'
live "live: a later read that fails still refuses" \
     2 "$d" "Live pool unreadable" 3 "$okpage" '1\n{"message":"Bad credentials"}'
live "live: a gh that exits non-zero is a refusal even when its stdout is a complete runners page" \
     2 "$d" "could not be read" 1 "1\n$(runners 4 "${X3[@]}")"
live "live: an error object answered with exit 0 is a refusal, not a pool of zero" \
     2 "$d" "not a runners page" 1 '0\n{"message":"Not Found","status":"404"}'
live "live: an answer that is not JSON is a refusal" \
     2 "$d" "is not JSON" 1 '0\n<html>rate limited</html>'
live "live: an empty answer is a refusal" \
     2 "$d" "answered nothing" 1 '0\n'
live "live: a page short of total_count is a refusal" \
     2 "$d" "Live pool read incomplete" 1 "0\n$(runners 6 "${X3[@]}")"
live "live: a runner with no label list is a refusal" \
     2 "$d" "no readable name or label list" 1 '0\n{"total_count":1,"runners":[{"name":"a"}]}'
live "PRESERVATION: two pages, one total, read as one pool" \
     0 "$d" "3 carry 'dhcp-ci'" 1 "0\n$(runners 4 a=dhcp-ci b=dhcp-ci)\n$(runners 4 c=dhcp-ci p=dhcp-ci-arm64)"
live "PRESERVATION: a JIT gap in one read is filled by the next (union of names)" \
     0 "$d" "3 carry 'dhcp-ci' over 3 read" 3 \
     "0\n$(runners 3 a=dhcp-ci b=dhcp-ci p=dhcp-ci-arm64)" "$okpage" \
     "0\n$(runners 3 b=dhcp-ci c=dhcp-ci p=dhcp-ci-arm64)"
live "live: every read short by a different runner, none complete, still counts the union" \
     0 "$d" "3 carry 'dhcp-ci' over 3 read" 3 \
     "0\n$(runners 3 a=dhcp-ci b=dhcp-ci p=dhcp-ci-arm64)" \
     "0\n$(runners 3 b=dhcp-ci c=dhcp-ci p=dhcp-ci-arm64)" \
     "0\n$(runners 3 a=dhcp-ci c=dhcp-ci p=dhcp-ci-arm64)"
live "ABSENCE: more runners registered than the file declares is red (the pool grew)" \
     1 "$d" "declares x64_runners=3; o/r has 4 runner name(s)" 1 \
     "0\n$(runners 5 a=dhcp-ci b=dhcp-ci c=dhcp-ci d=dhcp-ci p=dhcp-ci-arm64)"
live "live: pages that disagree on total_count are a refusal, not read as the smaller" \
     2 "$d" "Live pool read incomplete" 1 \
     "0\n$(runners 4 a=dhcp-ci b=dhcp-ci)\n$(runners 3 c=dhcp-ci)"
live "live: an answer that is not UTF-8 is a refusal, not a wrong file" \
     2 "$d" "not UTF-8" 1 '0\n\xff\xfe{"total_count"'
live "ABSENCE: a name missing from every read is red, however many reads" \
     1 "$d" "has 2 runner name(s)" 3 "0\n$(runners 3 a=dhcp-ci b=dhcp-ci p=dhcp-ci-arm64)"
live "live: a non-numeric read count is a refusal" \
     2 "$d" "not a positive integer" x "$okpage"

d=$(tree livenoarm 3); prose "$d" "<!-- ${MK}: pool-runners=3 -->"
printf '{"x64_label": "dhcp-ci", "x64_runners": 3}\n' > "$d/.github/ci-pool.json"
git -C "$d" add -A
d0=$(tree livezeroarm 3); prose "$d0" "<!-- ${MK}: pool-runners=3 -->"
printf '{"x64_label": "dhcp-ci", "x64_runners": 3, "arm64_label": "dhcp-ci-arm64", "arm64_runners": 0}\n' > "$d0/.github/ci-pool.json"
git -C "$d0" add -A
live "live: a declared arm64 count of 0 is a refusal even when no arm64 runner is registered" \
     2 "$d0" "Pool constant incomplete" 1 "0\n$(runners 3 a=dhcp-ci b=dhcp-ci c=dhcp-ci)"
live "live: a file with no arm64 count is a refusal, not a skipped comparison" \
     2 "$d" "Pool constant incomplete" 1 "$okpage"

# The default read count and gap, and the gap itself, driven through a stub
# `sleep` on PATH that logs its argument and returns at once. The pool's JIT
# churn made a point read short in 8 of 72 reads and a dip lasted at most two
# reads 5 s apart (#886), so the defaults must be several reads, spaced more
# than the dip.
cat > "$TMP/bin/sleep" <<'SL'
#!/usr/bin/env bash
echo "$*" >> "$STUB/sleeps"
SL
chmod +x "$TMP/bin/sleep"

# liveenv NAME WANT_EXIT ROOT WANT_GREP RESPONSE ENV... (no READS/GAP of its own)
liveenv() {
    local name="$1" want_exit="$2" root="$3" want_grep="$4" resp="$5"
    shift 5
    STUB="$TMP/stub-$((++stubs))"; mkdir -p "$STUB"
    printf '%b\n' "$resp" > "$STUB/last"
    env STUB="$STUB" PATH="$TMP/bin:$PATH" GATE_REPO=o/r "$@" bash "$GATE" --live --root "$root" > "$TMP/out" 2>&1
    local got=$?
    if [ "$got" -eq "$want_exit" ] && grep -q -- "$want_grep" "$TMP/out"; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (exit $got, want $want_exit, wanted /$want_grep/)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

d=$(tree livedef 3); prose "$d" "<!-- ${MK}: pool-runners=3 -->"
liveenv "live: with no settings it reads four times, so a JIT dip is covered by the union" \
        0 "$d" "over 4 read" "$okpage"
if [ "$(cat "$STUB/count")" -eq 4 ]; then
    echo "PASS: live: the default is four calls to the runners API"
else
    echo "FAIL: live: the default made $(cat "$STUB/count") calls, want 4"; failures=$((failures + 1))
fi
if [ "$(wc -l < "$STUB/sleeps")" -eq 3 ] && ! grep -qvx '[0-9]*' "$STUB/sleeps" \
   && [ "$(sort -n "$STUB/sleeps" | head -n 1)" -ge 10 ]; then
    echo "PASS: live: the default waits between reads (three sleeps, each at least 10 s, past the longest measured dip)"
else
    echo "FAIL: live: the default sleeps were: $(tr '\n' ' ' < "$STUB/sleeps" 2>/dev/null)"; failures=$((failures + 1))
fi

liveenv "live: a configured gap is honoured between reads and not before the first" \
        0 "$d" "over 3 read" "$okpage" CI_POOL_LIVE_READS=3 CI_POOL_LIVE_GAP=7
if [ "$(tr '\n' ' ' < "$STUB/sleeps")" = "7 7 " ]; then
    echo "PASS: live: gap 7 over 3 reads sleeps 7 twice"
else
    echo "FAIL: live: gap 7 over 3 reads slept: $(tr '\n' ' ' < "$STUB/sleeps" 2>/dev/null)"; failures=$((failures + 1))
fi

liveenv "live: a non-numeric gap is a refusal before any read" \
        2 "$d" "not a whole number of seconds" "$okpage" CI_POOL_LIVE_GAP=5s
if [ ! -e "$STUB/count" ]; then
    echo "PASS: live: a bad gap made no API call"
else
    echo "FAIL: live: a bad gap still called the API"; failures=$((failures + 1))
fi

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

# The scheduled comparison (#886): parsed, because a `continue-on-error`,
# an `|| true` or the workflow token would each turn every verdict green
# or amber without touching the gate.
if python3 - "$REPO/.github/workflows/pool-live-count.yml" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
on = wf.get("on", wf.get(True)) or {}
assert isinstance(on, dict) and on.get("schedule"), "no schedule trigger"
steps = [(j, s) for j in wf["jobs"].values() for s in j.get("steps", [])
         if "check-pool-facts.sh" in str(s.get("run", ""))]
assert len(steps) == 1, f"{len(steps)} steps run the gate"
job, step = steps[0]
assert step["run"].strip() == "bash scripts/check-pool-facts.sh --live", step["run"]
assert not job.get("continue-on-error") and not step.get("continue-on-error"), "continue-on-error"
assert step.get("env", {}).get("GH_TOKEN") == "${{ secrets.SCORECARD_TOKEN }}", "token"
PY
then
    echo "PASS: pool-live-count.yml runs --live on a schedule with the admin-read token, exit code intact"
else
    echo "FAIL: pool-live-count.yml does not run the --live comparison as built (#886)"
    failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures failure(s)"
    exit 1
fi
echo "all passed"
