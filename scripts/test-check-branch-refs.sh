#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-branch-refs.sh, driven in both directions.
#
# Every case builds a fixture `.github/` and hands the gate a fixture
# `git ls-remote --heads` OUTPUT rather than a branch list, so the ref
# parsing that runs in CI is the parsing under test here and no case
# touches the network.
#
# The first case is the control: a fixture where every name resolves must
# PASS, or every refusal below is satisfied by a gate that refuses
# everything.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/check-branch-refs.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

HEADS_DEFAULT='1111111111111111111111111111111111111111	refs/heads/main
2222222222222222222222222222222222222222	refs/heads/dev
3333333333333333333333333333333333333333	refs/heads/2.0.0
4444444444444444444444444444444444444444	refs/heads/feature/218-stable-mac'

# <name> <want-exit> <workflow body> <dependabot body> <scope value> [<expect>] [<heads>]
run_case() {
    local name="$1" want="$2" wf="$3" dep="$4" scope="$5" expect="${6:-}" heads="${7:-$HEADS_DEFAULT}"
    local dir out rc
    dir=$(mktemp -d)
    mkdir -p "$dir/wf"
    printf '%s\n' "$wf" > "$dir/wf/probe.yml"
    printf '%s\n' "$dep" > "$dir/dependabot.yml"
    printf 'GATE_SCOPE_BRANCHES="%s"\nGATE_SCOPE_COMMITS=15\n' "$scope" > "$dir/scope.env"
    printf '%s\n' "$heads" > "$dir/heads"
    out=$(BRANCH_REFS_WF_DIR="$dir/wf" \
          BRANCH_REFS_DEPENDABOT="$dir/dependabot.yml" \
          BRANCH_REFS_SCOPE="$dir/scope.env" \
          BRANCH_REFS_HEADS_FILE="$dir/heads" \
          bash "$GATE" 2>&1)
    rc=$?
    rm -rf "$dir"
    if [ "$rc" != "$want" ]; then
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
        return
    fi
    if [ -n "$expect" ] && ! printf '%s' "$out" | grep -F -- "$expect" >/dev/null; then
        no "$name (exit $rc as wanted, but the output does not name '$expect')"
        printf '      %s\n' "$out" >&2
        return
    fi
    ok "$name"
}

WF_OK='name: probe
on:
  push:
    branches:
      - main
      - dev
      - "2.*"
  pull_request:
    branches: [dev]
jobs:
  x:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
'
DEP_OK='version: 2
updates:
  - package-ecosystem: gomod
    directory: "/"
    schedule:
      interval: weekly
    target-branch: "2.0.0"
'

# --- the control -------------------------------------------------------
run_case "every name resolving PASSES" 0 "$WF_OK" "$DEP_OK" "dev main" \
    "branch reference(s) under .github/ resolve"

# --- literals ----------------------------------------------------------
#
# #907's own shape: a filter left pointing at the branch that was renamed
# away. This is the mutant that survived the whole local lane.
run_case "a workflow filter naming a branch that no longer exists FAILS" 1 \
    "${WF_OK/      - dev/      - 2.x-beta}" "$DEP_OK" "dev main" \
    "'2.x-beta' is not a branch"

# The same defect in flow style. Two spellings of a list, and a gate that
# reads only one of them passes over the other silently.
run_case "a dead name in a FLOW-style branches list FAILS" 1 \
    'name: probe
on:
  pull_request:
    branches: [main, 2.x-beta]
jobs:
  x:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
' "$DEP_OK" "dev main" "'2.x-beta' is not a branch"

# `branches-ignore` is the same obligation: a name that resolves to
# nothing excludes nothing.
run_case "a dead name in branches-ignore FAILS" 1 \
    'name: probe
on:
  push:
    branches-ignore:
      - gone-away
jobs:
  x:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
' "$DEP_OK" "dev main" "'gone-away' is not a branch"

# A quoted `on:` key parses to the string, an unquoted one to the boolean
# True. A reader that knows only one of those finds no triggers at all --
# and reports every file as clean.
run_case "a quoted 'on:' key is still walked" 1 \
    'name: probe
"on":
  push:
    branches:
      - gone-away
jobs:
  x:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
' "$DEP_OK" "dev main" "'gone-away' is not a branch"

# --- patterns ----------------------------------------------------------
run_case "a glob matching no existing branch FAILS" 1 \
    "${WF_OK/\"2.*\"/\"3.*\"}" "$DEP_OK" "dev main" \
    "matches no branch"

# The other direction: the same pattern shape, matching something, passes.
# Without this the case above is satisfied by a gate that fails every
# pattern.
run_case "a glob matching an existing branch PASSES" 0 "$WF_OK" "$DEP_OK" "dev main" \
    "2.* -> 2.0.0"

# GitHub's `*` stops at `/`, and so does this matcher. `feature/*` matches
# the fixture's `feature/218-stable-mac`; `feature*` alone would too under
# the shell's own globbing, which is why the matcher is not the shell's.
run_case "a slash-crossing pattern is matched GitHub's way" 0 \
    "${WF_OK/\"2.*\"/\"feature/*\"}" "$DEP_OK" "dev main" \
    "feature/* -> feature/218-stable-mac"
run_case "'*' does not cross a slash" 1 \
    "${WF_OK/\"2.*\"/\"feature*stable-mac\"}" "$DEP_OK" "dev main" \
    "matches no branch"
run_case "'**' does cross a slash" 0 \
    "${WF_OK/\"2.*\"/\"feature**stable-mac\"}" "$DEP_OK" "dev main" \
    "-> feature/218-stable-mac"

# Filter syntax this matcher does not implement is REFUSED, not matched
# approximately: an approximate match invents a population.
run_case "a pattern using unimplemented filter syntax FAILS" 1 \
    "${WF_OK/\"2.*\"/\"2.0.0+\"}" "$DEP_OK" "dev main" \
    "does not implement"

# --- dependabot --------------------------------------------------------
#
# The one that is not even an error at GitHub: a target-branch naming a
# branch that does not exist falls back to the DEFAULT branch, so the
# weekly PRs open somewhere nobody expects.
run_case "a dependabot target-branch that does not resolve FAILS" 1 \
    "$WF_OK" "${DEP_OK/\"2.0.0\"/\"2.0.0-alpha.1\"}" "dev main" \
    "updates[0].target-branch: '2.0.0-alpha.1' is not a branch"

# --- the gate scope ----------------------------------------------------
run_case "a dead name in GATE_SCOPE_BRANCHES FAILS" 1 "$WF_OK" "$DEP_OK" "dev 2.x-beta" \
    "GATE_SCOPE_BRANCHES: '2.x-beta' is not a branch"
run_case "a glob in GATE_SCOPE_BRANCHES matching nothing FAILS" 1 "$WF_OK" "$DEP_OK" "dev 9.*" \
    "matches no branch"
run_case "a glob in GATE_SCOPE_BRANCHES matching a branch PASSES" 0 "$WF_OK" "$DEP_OK" "dev main 2.*" \
    "GATE_SCOPE_BRANCHES"

# --- refusals ----------------------------------------------------------
#
# NAME WHICH STATE IS THE NORMAL ONE. An unreachable remote is not a pass:
# every name would "match nothing" for a reason that has nothing to do with
# the tree.
run_case "a heads listing with no refs/heads/ lines REFUSES" 2 "$WF_OK" "$DEP_OK" "dev main" \
    "no refs/heads/ lines" "$(printf 'ref: refs/heads/main\tHEAD\n')"

out=$(BRANCH_REFS_HEADS_FILE=/nonexistent/heads bash "$GATE" 2>&1); rc=$?
if [ "$rc" = "2" ]; then ok "an unreadable heads source REFUSES"
else no "an unreadable heads source gave exit $rc, want 2: $out"; fi

# A gate that inspects nothing must not report success.
run_case "a tree with no branch reference at all REFUSES" 2 \
    'name: probe
on:
  workflow_dispatch:
jobs:
  x:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
' 'version: 2
updates: []
' "" "no branch name was extracted"

dir=$(mktemp -d); mkdir -p "$dir/wf"; printf '%s\n' "$WF_OK" > "$dir/wf/probe.yml"
printf '%s\n' "$HEADS_DEFAULT" > "$dir/heads"
printf 'GATE_SCOPE_BRANCHES="dev"\n' > "$dir/scope.env"
out=$(BRANCH_REFS_WF_DIR="$dir/wf" BRANCH_REFS_DEPENDABOT="$dir/none.yml" \
      BRANCH_REFS_SCOPE="$dir/scope.env" BRANCH_REFS_HEADS_FILE="$dir/heads" \
      bash "$GATE" 2>&1); rc=$?
rm -rf "$dir"
if [ "$rc" = "2" ]; then ok "a missing dependabot.yml REFUSES rather than passing over it"
else no "a missing dependabot.yml gave exit $rc, want 2: $out"; fi

# --- the preservation control, against the real .github/ ---------------
#
# The fixture cases prove the rules; this proves the shipped tree obeys
# them. The head set is written out rather than fetched, so the case is
# offline and deterministic -- and so that adding a filter for a branch
# this repository does not maintain has to be a deliberate edit HERE as
# well.
real_heads=$(printf '%s\n' \
    '1111111111111111111111111111111111111111	refs/heads/main' \
    '2222222222222222222222222222222222222222	refs/heads/dev' \
    '3333333333333333333333333333333333333333	refs/heads/2.0.0' \
    '4444444444444444444444444444444444444444	refs/heads/gh-pages')
f=$(mktemp); printf '%s\n' "$real_heads" > "$f"
out=$(BRANCH_REFS_HEADS_FILE="$f" bash "$GATE" 2>&1); rc=$?
rm -f "$f"
if [ "$rc" = "0" ]; then
    ok "the shipped .github/ resolves against main, dev, 2.0.0 and gh-pages"
else
    no "the shipped .github/ did not resolve (exit $rc)"
    printf '      %s\n' "$out" >&2
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
