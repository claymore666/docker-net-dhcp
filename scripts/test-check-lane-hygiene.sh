#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-lane-hygiene.sh (#742).
#
# The three invariants this gate carries share a property that makes
# them hard to test any other way: each failure is an ABSENCE. A step
# without `if: always()` is skipped, not failed; a lane without a
# teardown leaves state on a machine nothing inspects; a workflow
# without `edited` in its types simply never runs. So the cases below
# check that the gate reports the absence — and, in every section, that
# the corresponding presence still reads as clean, because a gate that
# fires on both is one nobody can satisfy.
#
# The last section is the one that caught a real defect in this gate: a
# step block that never ended swallowed the next job's matrix, turning
# every per-step question into a whole-file question and reporting two
# workflows this branch had just fixed.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-lane-hygiene.sh"
pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# verdict <dir> -> the exit code
verdict() { bash "$GATE" "$1" >/dev/null 2>&1; echo $?; }

# --- A. teardown for a lane that installs a plugin ----------------------

mk_install() {
    # $1 dir, $2 "teardown"|"none"
    local d="$1"
    {
        printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
        printf '    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - name: Build + install\n        run: |\n'
        printf '          docker plugin rm -f "$REF" 2>/dev/null || true\n'
        printf '          docker plugin create "$REF" plugin\n'
        printf '          docker plugin enable "$REF"\n'
        if [ "$2" = teardown ]; then
            printf '      - name: Tear down integration plugin\n        if: always()\n        run: |\n'
            printf '          docker plugin rm -f "$REF" 2>/dev/null || true\n'
        fi
    } > "$d/lane.yml"
}

d=$(mktemp -d); mk_install "$d" none
[ "$(verdict "$d")" = 1 ] \
    && ok "a lane that installs a plugin with no teardown is reported" \
    || no "a lane with no teardown should exit 1"
rm -rf "$d"

d=$(mktemp -d); mk_install "$d" teardown
[ "$(verdict "$d")" = 0 ] \
    && ok "the same lane with an if: always() teardown is clean" \
    || no "a lane with a teardown should be clean"
rm -rf "$d"

# The pre-install `docker plugin rm -f` inside the BUILD step is not a
# teardown — it runs before the plugin this job installs exists. That is
# exactly what integration-arm64.yml had, and the reason a naive
# whole-file grep for `plugin rm` would have called it clean.
d=$(mktemp -d)
{
    printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
    printf '    runs-on: ubuntu-latest\n    steps:\n'
    printf '      - name: Build + install\n        if: always()\n        run: |\n'
    printf '          docker plugin rm -f "$REF" 2>/dev/null || true\n'
    printf '          docker plugin create "$REF" plugin\n'
} > "$d/lane.yml"
[ "$(verdict "$d")" = 1 ] \
    && ok "a pre-install 'plugin rm' inside the build step is not a teardown" \
    || no "a pre-install rm was accepted as a teardown"
rm -rf "$d"

# --- B. the failure suite runs after a red main suite -------------------

mk_suite() {
    # $1 dir, $2 "always"|"none"
    local d="$1"
    {
        printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
        printf '    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - name: Run main suite\n        run: make integration-test\n'
        printf '      - name: Run failure suite\n'
        [ "$2" = always ] && printf '        if: always()\n'
        printf '        run: make integration-test-failure\n'
    } > "$d/lane.yml"
}

d=$(mktemp -d); mk_suite "$d" none
[ "$(verdict "$d")" = 1 ] \
    && ok "a failure suite without if: always() is reported" \
    || no "a failure suite without if: always() should exit 1"
rm -rf "$d"

d=$(mktemp -d); mk_suite "$d" always
[ "$(verdict "$d")" = 0 ] \
    && ok "the same lane with if: always() is clean" \
    || no "a failure suite with if: always() should be clean"
rm -rf "$d"

# A matrix DECLARATION of the target is not an invocation. integration.yml
# runs its failure suite as a separate matrix job where `fail-fast: false`
# keeps the suites independent, and a step-level `if:` there would be
# wrong rather than missing.
d=$(mktemp -d)
{
    printf 'name: lane\non:\n  workflow_dispatch:\njobs:\n  suite:\n'
    printf '    runs-on: ubuntu-latest\n'
    printf '    strategy:\n      fail-fast: false\n      matrix:\n        include:\n'
    printf '          - suite: main\n            target: integration-test\n'
    printf '          - suite: failure\n            target: integration-test-failure\n'
    printf '    steps:\n'
    printf '      - name: Run suite\n        run: make ${{ matrix.target }}\n'
} > "$d/lane.yml"
[ "$(verdict "$d")" = 0 ] \
    && ok "a matrix target named integration-test-failure is not a step invocation" \
    || no "a matrix declaration was misread as an un-guarded step"
rm -rf "$d"

# --- C. `edited` where a gate reads the PR body -------------------------

mk_body_gate() {
    # $1 dir, $2 types-line-or-empty
    local d="$1"
    {
        printf 'name: t\non:\n  pull_request:\n'
        [ -n "$2" ] && printf '    types: %s\n' "$2"
        printf 'jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n'
        printf '      - name: Weakening\n        run: bash scripts/check-test-weakening.sh "$R" "$B"\n'
    } > "$d/t.yaml"
}

d=$(mktemp -d); mk_body_gate "$d" ""
[ "$(verdict "$d")" = 1 ] \
    && ok "a body-reading gate with the default pull_request types is reported" \
    || no "default types with a body-reading gate should exit 1"
rm -rf "$d"

# The default set spelled out explicitly is still the default set. This
# is the case that separates "checks for edited" from "checks that types
# is present at all" — and the latter would have called the tree clean.
d=$(mktemp -d); mk_body_gate "$d" "[opened, synchronize, reopened]"
[ "$(verdict "$d")" = 1 ] \
    && ok "types listed without 'edited' is still reported" \
    || no "an explicit types list missing 'edited' should exit 1"
rm -rf "$d"

d=$(mktemp -d); mk_body_gate "$d" "[opened, synchronize, reopened, edited]"
[ "$(verdict "$d")" = 0 ] \
    && ok "types including 'edited' is clean" \
    || no "types including 'edited' should be clean"
rm -rf "$d"

# A workflow that runs no body-reading gate is out of scope: demanding
# `edited` everywhere would fire runs on every typo in every PR body.
d=$(mktemp -d)
{
    printf 'name: t\non:\n  pull_request:\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n'
    printf '      - name: Unit\n        run: go test ./...\n'
} > "$d/t.yaml"
[ "$(verdict "$d")" = 0 ] \
    && ok "a workflow with no body-reading gate does not need 'edited'" \
    || no "a workflow with no body-reading gate was required to have 'edited'"
rm -rf "$d"

# --- prose cannot trip or satisfy any of it -----------------------------
# These workflows document their own invariants at length, quoting the
# very strings this gate matches on.
d=$(mktemp -d)
{
    printf 'name: lane\non:\n  workflow_dispatch:\n'
    printf '# this lane runs docker plugin create and tears it down with if: always()\n'
    printf '# and its failure suite runs make integration-test-failure\n'
    printf 'jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n'
    printf '      - name: Unit\n        run: go test ./...\n'
} > "$d/lane.yml"
[ "$(verdict "$d")" = 0 ] \
    && ok "comments naming the matched strings neither trip nor satisfy the gate" \
    || no "a comment tripped the gate"
rm -rf "$d"

# --- inspecting nothing is not a pass -----------------------------------
d=$(mktemp -d)
[ "$(verdict "$d")" = 2 ] \
    && ok "an empty workflow directory is rc2, not a pass" \
    || no "an empty directory should exit 2"
rm -rf "$d"

[ "$(verdict /nonexistent-workflow-dir)" = 2 ] \
    && ok "a missing workflow directory is rc2" \
    || no "a missing workflow directory should exit 2"

# A workflow whose indentation this gate cannot parse must REFUSE. The
# alternative is the failure mode every gate here was audited for in
# #743: a clean pass rendered over an input set that came out empty.
d=$(mktemp -d)
printf 'name: x\non:\n  workflow_dispatch:\njobs:\n  a:\n    steps:\n      "not a step"\n' > "$d/x.yml"
[ "$(verdict "$d")" = 2 ] \
    && ok "a 'steps:' block this gate cannot parse is rc2, not a clean pass" \
    || no "an unparseable steps block should exit 2"
rm -rf "$d"

# --- the real repository ------------------------------------------------
real=$( cd "$(dirname "$GATE")/.." && bash "$GATE" >/dev/null 2>&1; echo $? )
[ "$real" = 0 ] \
    && ok "this repository passes its own lane-hygiene gate" \
    || no "this repository fails its own lane-hygiene gate (exit $real)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
