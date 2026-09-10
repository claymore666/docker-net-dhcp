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
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-one-process-itest-budget.sh"
REPO="$(cd "$HERE/.." && pwd)"

pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

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
python3 - "$root/workflows/coverage.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = """        env:
          INTEGRATION_PLUGIN_REF: ${{ env.COVER_PLUGIN_REF }}
          ITEST_TIMEOUT: 45m
        run: make integration-test
"""
new = """        env:
          INTEGRATION_PLUGIN_REF: ${{ env.COVER_PLUGIN_REF }}
        run: make integration-test
"""
assert s.count(old) == 1, "the coverage main step is not the shape this case mutates"
open(p, "w").write(s.replace(old, new))
PY
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
python3 - "$root/workflows/integration-hosted.yml" <<'PY'
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
if [ "$(verdict "$root")" = "0" ]; then
    ok "a failure-suite step taking the Makefile default is not a finding"
else
    no "the gate demands a value identical to the default it would replace"
fi

# ------------------------------------------------ B: the value survives sudo
root="$(fixture sudo_not_forwarded)"
python3 - "$root/workflows/integration-hosted.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = ' "ITEST_TIMEOUT=$ITEST_TIMEOUT" make integration-test\n'
assert s.count(old) == 1, "the hosted main step's forward is not the shape this case mutates"
open(p, "w").write(s.replace(old, " make integration-test\n"))
PY
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
python3 - "$root/workflows/integration-hosted.yml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = 'run: sudo env "PATH=$PATH" "INTEGRATION_PLUGIN_REF=$INTEGRATION_PLUGIN_REF" "ITEST_TIMEOUT=$ITEST_TIMEOUT" make integration-test\n'
new = 'run: sudo -E env "PATH=$PATH" "INTEGRATION_PLUGIN_REF=$INTEGRATION_PLUGIN_REF" make integration-test\n'
assert s.count(old) == 1
open(p, "w").write(s.replace(old, new))
PY
if [ "$(verdict "$root")" = "0" ]; then
    ok "sudo -E is a forward: the gate reads the mechanism, not one spelling"
else
    no "the gate insists on one spelling of forwarding rather than on the property"
fi

# ------------------------------------------- C: the job cap holds the ceilings
root="$(fixture cap_below_ceilings)"
sed -i 's/^    timeout-minutes: 70$/    timeout-minutes: 60/' "$root/workflows/integration-hosted.yml"
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
sed -i 's/^          ITEST_TIMEOUT: 45m$/          ITEST_TIMEOUT: 60m/' "$root/workflows/coverage.yml"
if [ "$(verdict "$root")" = "1" ]; then
    ok "raising a step ceiling past its job cap is the same finding, driven from the other side"
else
    no "the gate only watches the cap, not the ceilings underneath it"
fi

# C's margin: exactly the sum is not enough, sum + 1 is.
root="$(fixture cap_equals_sum)"
sed -i 's/^    timeout-minutes: 70$/    timeout-minutes: 65/' "$root/workflows/integration-hosted.yml"
if [ "$(verdict "$root")" = "1" ]; then
    ok "a cap set exactly equal to the sum leaves nothing for setup and is a finding"
else
    no "a cap with zero margin over its ceilings passed"
fi
root="$(fixture cap_sum_plus_one)"
sed -i 's/^    timeout-minutes: 70$/    timeout-minutes: 66/' "$root/workflows/integration-hosted.yml"
if [ "$(verdict "$root")" = "0" ]; then
    ok "the required margin is one minute, not an invented setup estimate"
else
    no "the gate demands more margin than it documents"
fi

# C: no cap at all.
root="$(fixture no_cap)"
sed -i '/^    timeout-minutes: 70$/d' "$root/workflows/integration-hosted.yml"
if [ "$(verdict "$root")" = "1" ]; then
    ok "a whole-suite job with no timeout-minutes is a finding"
else
    no "an unbounded job passed as a backstop"
fi

# ------------------------------------------------- the default is READ, not 20
root="$(fixture default_moves)"
sed -i 's/^ITEST_TIMEOUT ?= 20m$/ITEST_TIMEOUT ?= 90m/' "$root/Makefile"
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
sed -i '/^ITEST_TIMEOUT ?= /d' "$root/Makefile"
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
sed -i 's/^          ITEST_TIMEOUT: 45m$/          ITEST_TIMEOUT: ${{ inputs.budget }}/' "$root/workflows/coverage.yml"
if [ "$(verdict "$root")" = "2" ]; then
    ok "an expression-valued ceiling is a refusal, not a silently skipped member"
else
    no "a member the gate cannot judge was treated as if it complied"
fi

root="$(fixture nonsense_ceiling)"
sed -i 's/^          ITEST_TIMEOUT: 45m$/          ITEST_TIMEOUT: soon/' "$root/workflows/coverage.yml"
if [ "$(verdict "$root")" = "2" ]; then
    ok "a ceiling that is not a Go duration is a refusal"
else
    no "the gate accepted a value make cannot parse either"
fi

# ------------------------------------------------------------------ verdict
printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
