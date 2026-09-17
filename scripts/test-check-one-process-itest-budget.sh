#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-one-process-itest-budget.sh (#934).
#
# THE SUBJECT IS TODAY'S TREE, MUTATED. Every case below starts from the
# real .github/workflows and the real Makefile and removes exactly one
# property, because the claim the gate makes is about this repository's
# lanes and not about a fixture written to make it pass. A gate whose
# only evidence is a synthetic workflow is a gate that has never met its
# subject.
#
# So each of the gate's three properties is driven twice: absent, where
# the gate must go red, and present, where it must stay clean. A check
# that fires in both directions is one nobody can satisfy, and a check
# that fires in neither is decoration.
#
# Three cases exist for reasons that are not the properties themselves:
#
#   * THE DEFAULT IS READ. Change the Makefile's `?=` to 90m and the
#     steps that set nothing must suddenly cost 90 -- if the gate had a
#     hardcoded 20 it would stay green, and it would be a second copy of
#     the very fact it exists to reconcile.
#
#   * THE POPULATION EXCLUDES SHARDS. `make integration-test-shard` is
#     not a whole suite, and a workflow directory holding only shard
#     steps must REFUSE (exit 2), never report clean: a universal rule
#     over an empty domain is satisfied by emptying the domain.
#
#   * A CEILING IT CANNOT READ IS A REFUSAL. An expression-valued
#     ITEST_TIMEOUT exits 2 rather than being silently skipped, because
#     a skipped member looks exactly like a compliant one in the output.
#
# NO CASE SPELLS A CEILING OR A CAP. Mutating today's tree means every
# case needs an anchor in it, and an anchor that quotes the current value
# stops matching the day that value moves -- the edit then changes
# nothing, the case asserts the UNMUTATED tree's verdict, and it prints
# PASS. Nine cases here were exactly that, anchored on `45m` and
# `timeout-minutes: 70`, and all nine went quiet when the one-process
# ceiling was raised (#934, 2026-09-17). So the values are read out of
# the tree, every edit states how many occurrences it must change, and
# `mutated` refuses a fixture that came out identical to its source.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-one-process-itest-budget.sh"
REPO="$(cd "$HERE/.." && pwd)"

pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

guarded_tmpdir TMP

# fixture <name> -> prints the fixture root, a fresh copy of the tree
fixture() {
    local root="$TMP/$1"
    rm -rf "$root"
    mkdir -p "$root"
    cp -r "$REPO/.github/workflows" "$root/workflows"
    cp "$REPO/Makefile" "$root/Makefile"
    printf '%s\n' "$root"
}

# verdict <root> -> the gate's exit code
verdict() { bash "$GATE" "$1/workflows" "$1/Makefile" >/dev/null 2>&1; echo $?; }

# says <root> <pattern> -> 0 if the gate's output mentions the pattern.
# The output is captured before it is searched, deliberately: under
# `set -o pipefail` a pipeline inherits the GATE's exit code, so
# `gate | grep -q` would report "did not say it" for every finding the
# gate actually printed -- a control that always agrees with the case
# beside it is not a control.
says() {
    local out
    out="$(bash "$GATE" "$1/workflows" "$1/Makefile" 2>&1)"
    # `grep` without -q, output discarded: -q exits at the first match
    # and SIGPIPEs the producer, which check-pipefail-consumers.sh
    # refuses for exactly the reason it would bite here.
    printf '%s' "$out" | grep -- "$2" >/dev/null
}

# subst <file> <regex> <replacement> <count> -- a multiline regex
# substitution that must change exactly <count> occurrences, or the case
# aborts. An edit that matched nothing is an INERT CONTROL: it passes its
# own assertion having mutated nothing, and the gate then agrees with a
# tree nobody changed. Every `sed -i` here used to be that shape, anchored
# on the literal ceiling the lanes carried, and all nine of those cases
# went quiet the day the ceiling moved (#934, 2026-09-17).
subst() {
    python3 - "$@" <<'SUBST'
import re
import sys

path, pat, rep, want = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
text = open(path, encoding="utf-8").read()
out, n = re.subn(pat, rep, text, flags=re.M)
if n != want:
    sys.stderr.write(
        "MUTATION INERT: %s changed %d occurrence(s) of %r, wanted %d. "
        "The tree no longer has the shape this case mutates.\n" % (path, n, pat, want))
    sys.exit(1)
open(path, "w", encoding="utf-8").write(out)
SUBST
}

# mutated <root> <label> -- the fixture must DIFFER from the tree it was
# copied from. Without this a case whose anchor stopped matching asserts
# the unmutated tree's verdict and passes, which is indistinguishable in
# the output from a gate that works.
mutated() {
    if diff -r -q "$REPO/.github/workflows" "$1/workflows" >/dev/null 2>&1 &&
       cmp -s "$REPO/Makefile" "$1/Makefile"; then
        no "INERT: $2 left the fixture identical to the tree"
        return 1
    fi
    return 0
}

# read_one <file> <regex with one group> -- the value, or a loud failure
# if the file does not have exactly one of them.
read_one() {
    python3 - "$1" "$2" <<'READ'
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
hits = re.findall(sys.argv[2], text, flags=re.M)
if len(hits) != 1:
    sys.stderr.write("expected one match for %r in %s, found %d\n"
                     % (sys.argv[2], sys.argv[1], len(hits)))
    sys.exit(1)
print(hits[0])
READ
}

WF="$REPO/.github/workflows"

# THE NUMBERS THESE CASES USE ARE READ OUT OF THE TREE, never restated. A
# case that spells today's ceiling stops being a case the moment the
# ceiling moves, which is the defect the gate itself exists to close.
#
# The hosted `full` job's two whole-suite ceilings are summed here by
# reading the file, not by asking the gate: a case that took its expected
# value from its own subject could not fail.
HOSTED_SUM="$(python3 - "$WF/integration-hosted.yml" <<'SUM'
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
vals = re.findall(r"^ *ITEST_TIMEOUT: (\d+)m$", text, flags=re.M)
assert len(vals) == 2, "the hosted lane no longer states exactly two ceilings"
print(sum(int(v) for v in vals))
SUM
)" || exit 1
COVERAGE_CAP="$(read_one "$WF/coverage.yml" '^    timeout-minutes: (\d+)$')" || exit 1

# The coverage lane's MAIN-suite ceiling, keyed on the step's own `run:`
# line rather than on its value, because the failure step's ceiling sits
# at the same indentation two steps below.
COVERAGE_MAIN_RE='^          ITEST_TIMEOUT: \S+$\n        run: make integration-test$'

# coverage_main_ceiling <value> -- that step rewritten with a new ceiling.
coverage_main_ceiling() {
    printf '          ITEST_TIMEOUT: %s\n        run: make integration-test' "$1"
}

# ---------------------------------------------------------------- control
root="$(fixture control)"
if [ "$(verdict "$root")" = "0" ]; then
    ok "today's tree passes: every one-process lane states and holds its budget"
else
    no "today's tree does not pass the gate; every case below measures against it"
    bash "$GATE" "$root/workflows" "$root/Makefile" >&2
fi

derivation="$(bash "$GATE" "$root/workflows" "$root/Makefile" 2>&1)"
if printf '%s' "$derivation" | grep -- "6 whole-suite step(s) in 3 lane(s)" >/dev/null; then
    ok "the population is the three one-process lanes' six whole-suite steps"
else
    no "the derived population is not the six steps in three lanes this tree has"
fi

# ------------------------------------------------- A: the ceiling is stated
root="$(fixture no_main_ceiling)"
subst "$root/workflows/coverage.yml" "$COVERAGE_MAIN_RE" \
      '        run: make integration-test' 1 || fail=$((fail + 1))
mutated "$root" "no_main_ceiling"
if [ "$(verdict "$root")" = "1" ]; then
    ok "a main-suite step with no ITEST_TIMEOUT is a finding (property A)"
else
    no "a main-suite step inheriting the shard default passed the gate"
fi
if says "$root" "inherits"; then
    ok "the finding says the step inherits the Makefile default rather than naming a number"
else
    no "the property A message does not name the inheritance it is about"
fi

# A, the other direction: the FAILURE suite may take the default, because
# there the default is the whole-suite budget. Remove it and stay clean.
root="$(fixture failure_takes_default)"
if ! python3 - "$root/workflows/integration-hosted.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = """        env:
          ITEST_TIMEOUT: 20m
        run: sudo env "PATH=$PATH" "INTEGRATION_PLUGIN_REF=$INTEGRATION_PLUGIN_REF" "ITEST_TIMEOUT=$ITEST_TIMEOUT" make integration-test-failure
"""
new = """        run: sudo env "PATH=$PATH" "INTEGRATION_PLUGIN_REF=$INTEGRATION_PLUGIN_REF" make integration-test-failure
"""
assert s.count(old) == 1, "the hosted failure step is not the shape this case mutates"
open(p, "w").write(s.replace(old, new))
PY
then
    fail=$((fail + 1))
fi
mutated "$root" "failure_takes_default"
if [ "$(verdict "$root")" = "0" ]; then
    ok "a failure-suite step taking the Makefile default is not a finding"
else
    no "the gate demands a value identical to the default it would replace"
fi

# ------------------------------------------------ B: the value survives sudo
root="$(fixture sudo_not_forwarded)"
if ! python3 - "$root/workflows/integration-hosted.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = ' "ITEST_TIMEOUT=$ITEST_TIMEOUT" make integration-test\n'
assert s.count(old) == 1, "the hosted main step's forward is not the shape this case mutates"
open(p, "w").write(s.replace(old, " make integration-test\n"))
PY
then
    fail=$((fail + 1))
fi
mutated "$root" "sudo_not_forwarded"
if [ "$(verdict "$root")" = "1" ]; then
    ok "an env: value not forwarded through sudo is a finding (property B)"
else
    no "a budget that sudo discards passed the gate"
fi
if says "$root" "sudo resets the environment"; then
    ok "the finding names the mechanism, not just the missing text"
else
    no "the property B message does not say why the env: block bounds nothing"
fi

# B, the other direction, twice: a step that does not use sudo needs no
# forward (integration-arm64.yml's main step is exactly that, and the
# control already covers it), and `sudo -E` is a forward too.
root="$(fixture sudo_preserve_env)"
if ! python3 - "$root/workflows/integration-hosted.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = 'run: sudo env "PATH=$PATH" "INTEGRATION_PLUGIN_REF=$INTEGRATION_PLUGIN_REF" "ITEST_TIMEOUT=$ITEST_TIMEOUT" make integration-test\n'
new = 'run: sudo -E env "PATH=$PATH" "INTEGRATION_PLUGIN_REF=$INTEGRATION_PLUGIN_REF" make integration-test\n'
assert s.count(old) == 1
open(p, "w").write(s.replace(old, new))
PY
then
    fail=$((fail + 1))
fi
mutated "$root" "sudo_preserve_env"
if [ "$(verdict "$root")" = "0" ]; then
    ok "sudo -E is a forward: the gate reads the mechanism, not one spelling"
else
    no "the gate insists on one spelling of forwarding rather than on the property"
fi

# ------------------------------------------- C: the job cap holds the ceilings
root="$(fixture cap_below_ceilings)"
subst "$root/workflows/integration-hosted.yml" \
      '^    timeout-minutes: \d+$' "    timeout-minutes: $((HOSTED_SUM - 5))" 1 || fail=$((fail + 1))
mutated "$root" "cap_below_ceilings"
if [ "$(verdict "$root")" = "1" ]; then
    ok "a job cap under the sum of its step ceilings is a finding (property C)"
else
    no "a job cap that fires before its own step alarms passed the gate"
fi
if says "$root" "fires FIRST"; then
    ok "the finding says what goes wrong: the cap reports the wrong cause"
else
    no "the property C message does not name the misattribution it prevents"
fi

# C from the other side: raise a STEP ceiling under an unchanged cap.
root="$(fixture ceiling_above_cap)"
subst "$root/workflows/coverage.yml" "$COVERAGE_MAIN_RE" \
      "$(coverage_main_ceiling "$((COVERAGE_CAP + 1))m")" 1 || fail=$((fail + 1))
mutated "$root" "ceiling_above_cap"
if [ "$(verdict "$root")" = "1" ]; then
    ok "raising a step ceiling past its job cap is the same finding, driven from the other side"
else
    no "the gate only watches the cap, not the ceilings underneath it"
fi

# C's margin: exactly the sum is not enough, sum + 1 is.
root="$(fixture cap_equals_sum)"
subst "$root/workflows/integration-hosted.yml" \
      '^    timeout-minutes: \d+$' "    timeout-minutes: ${HOSTED_SUM}" 1 || fail=$((fail + 1))
mutated "$root" "cap_equals_sum"
if [ "$(verdict "$root")" = "1" ]; then
    ok "a cap set exactly equal to the sum leaves nothing for setup and is a finding"
else
    no "a cap with zero margin over its ceilings passed"
fi
root="$(fixture cap_sum_plus_one)"
subst "$root/workflows/integration-hosted.yml" \
      '^    timeout-minutes: \d+$' "    timeout-minutes: $((HOSTED_SUM + 1))" 1 || fail=$((fail + 1))
mutated "$root" "cap_sum_plus_one"
if [ "$(verdict "$root")" = "0" ]; then
    ok "the required margin is one minute, not an invented setup estimate"
else
    no "the gate demands more margin than it documents"
fi

# C: no cap at all.
root="$(fixture no_cap)"
subst "$root/workflows/integration-hosted.yml" '^    timeout-minutes: \d+\n' "" 1 || fail=$((fail + 1))
mutated "$root" "no_cap"
if [ "$(verdict "$root")" = "1" ]; then
    ok "a whole-suite job with no timeout-minutes is a finding"
else
    no "an unbounded job passed as a backstop"
fi

# ------------------------------------------------- the default is READ, not 20
root="$(fixture default_moves)"
subst "$root/Makefile" '^ITEST_TIMEOUT \?= \S+$' 'ITEST_TIMEOUT ?= 90m' 1 || fail=$((fail + 1))
mutated "$root" "default_moves"
if [ "$(verdict "$root")" = "1" ]; then
    ok "moving the Makefile default moves what an unset step costs (90m blows the caps)"
else
    no "the gate carries its own copy of the default instead of reading the Makefile's"
fi
if says "$root" "default of 90m"; then
    ok "the gate reports the default it actually read"
else
    no "the gate does not say which default it costed unset steps at"
fi

root="$(fixture default_missing)"
subst "$root/Makefile" '^ITEST_TIMEOUT \?= \S+\n' '' 1 || fail=$((fail + 1))
mutated "$root" "default_missing"
if [ "$(verdict "$root")" = "2" ]; then
    ok "a Makefile with no ITEST_TIMEOUT default is a refusal, not a pass"
else
    no "the gate invented a default when the Makefile had none"
fi

# --------------------------------------- population: shards are not whole suites
root="$(fixture shards_only)"
rm -f "$root"/workflows/*.yml "$root"/workflows/*.yaml
cat > "$root/workflows/shard-only.yml" <<'YML'
name: shards only
on: workflow_dispatch
jobs:
  itest:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - run: make integration-test-shard "SHARD=1" "OF=9" "SUITE=main"
YML
if [ "$(verdict "$root")" = "2" ]; then
    ok "a directory of shard steps has an EMPTY population and refuses (exit 2)"
else
    no "the gate reported clean over a population it never found"
fi
if says "$root" "population has rotted"; then
    ok "the refusal says the domain is empty rather than implying everything passed"
else
    no "the empty-population refusal does not explain itself"
fi

# The same file with a shard step beside a whole-suite step: the shard
# must not be charged to the job, or the cap arithmetic is wrong for
# every lane that runs both arms.
root="$(fixture shard_beside_whole)"
rm -f "$root"/workflows/*.yml "$root"/workflows/*.yaml
cat > "$root/workflows/mixed.yml" <<'YML'
name: mixed
on: workflow_dispatch
jobs:
  itest:
    runs-on: ubuntu-latest
    timeout-minutes: 46
    steps:
      - env:
          ITEST_TIMEOUT: 45m
        run: make integration-test
      - run: make integration-test-shard "SHARD=1" "OF=9" "SUITE=main"
YML
if [ "$(verdict "$root")" = "0" ]; then
    ok "a shard step beside a whole-suite step is not charged to the job cap"
else
    no "the gate charges shard steps to the cap, which would make every mixed lane red"
fi

# ------------------------------------- a ceiling the gate cannot read is a refusal
root="$(fixture expression_ceiling)"
# shellcheck disable=SC2016  # the literal ${{ }} is the case: an
# unexpanded workflow expression is exactly what the gate must refuse.
subst "$root/workflows/coverage.yml" "$COVERAGE_MAIN_RE" \
      "$(coverage_main_ceiling '${{ inputs.budget }}')" 1 || fail=$((fail + 1))
mutated "$root" "expression_ceiling"
if [ "$(verdict "$root")" = "2" ]; then
    ok "an expression-valued ceiling is a refusal, not a silently skipped member"
else
    no "a member the gate cannot judge was treated as if it complied"
fi

root="$(fixture nonsense_ceiling)"
subst "$root/workflows/coverage.yml" "$COVERAGE_MAIN_RE" \
      "$(coverage_main_ceiling 'soon')" 1 || fail=$((fail + 1))
mutated "$root" "nonsense_ceiling"
if [ "$(verdict "$root")" = "2" ]; then
    ok "a ceiling that is not a Go duration is a refusal"
else
    no "the gate accepted a value make cannot parse either"
fi

# --------------------------------------------- the inertness guard, driven
# The guard that makes every case above worth reading, checked the only
# way a guard can be: by feeding it an anchor that matches nothing. Nine
# cases here were `sed -i` on the ceiling the lanes happened to carry, and
# the day that ceiling moved each of them mutated nothing, asserted the
# unmutated tree's verdict and printed PASS.
root="$(fixture inert_guard)"
if subst "$root/workflows/coverage.yml" \
         '^          ITEST_TIMEOUT: no-ceiling-is-ever-spelled-this-way$' \
         '          ITEST_TIMEOUT: 60m' 1 2>/dev/null; then
    no "a mutation that matched nothing was accepted as an edit"
else
    ok "a mutation whose anchor no longer matches aborts the case instead of passing"
fi
if diff -r -q "$REPO/.github/workflows" "$root/workflows" >/dev/null 2>&1; then
    ok "and the fixture is provably identical to the tree, which is what it detected"
else
    no "the inertness case changed the fixture, so it proves nothing about inert edits"
fi

# ------------------------------------------------------------------ verdict
printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
