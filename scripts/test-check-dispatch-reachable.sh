#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-dispatch-reachable.sh. The failure it guards is a
# workflow_dispatch workflow that exists on the working branch but not on
# the default branch: GitHub will not expose it, `gh workflow run` answers
# 404, and any documentation naming it as a route is wrong until the next
# release. capture-fixtures.yml shipped in exactly that state (#665).
#
# Cases run against a real throwaway repository rather than a stub tree,
# because the property under test is "is this path present on another
# git ref" — a mock would test the mock.
set -uo pipefail

CHECK="$(cd "$(dirname "$0")" && pwd)/check-dispatch-reachable.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

REPO="$TMP/repo"
mkdir -p "$REPO/.github/workflows"
git -C "$REPO" init -q -b main
git -C "$REPO" config user.email t@example.com
git -C "$REPO" config user.name t
git -C "$REPO" config commit.gpgsign false

dispatchable() { printf 'name: %s\non:\n  workflow_dispatch:\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' "$1"; }
push_only()    { printf 'name: %s\non:\n  push:\n    branches: [main]\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' "$1"; }

# main carries one dispatchable workflow.
dispatchable released > "$REPO/.github/workflows/released.yml"
git -C "$REPO" add -A && git -C "$REPO" commit -qm base
git -C "$REPO" checkout -q -b work

verdict() {
    ( cd "$REPO" && BASE_REF=main bash "$CHECK" >"$TMP/out" 2>&1 ) \
        && echo pass || echo "rc$?"
}

# --- the baseline ------------------------------------------------------
check "a workflow present on the default branch passes" pass "$(verdict)"

# --- the defect --------------------------------------------------------
dispatchable newone > "$REPO/.github/workflows/newone.yml"
check "a dispatchable workflow absent from the default branch fails" rc1 "$(verdict)"

grep -F 'not on main' "$TMP/out" >/dev/null \
    && echo "PASS: the message names the default branch" \
    || { echo "FAIL: the message does not name the default branch"; fails=1; }

# --- declaring it is the release valve ---------------------------------
printf '# reason: lands in the next release\n.github/workflows/newone.yml\n' \
    > "$REPO/.github/dispatch-pending.txt"
check "declaring it passes" pass "$(verdict)"

# --- and the declaration has to stay true ------------------------------
git -C "$REPO" add -A && git -C "$REPO" commit -qm add-newone
git -C "$REPO" checkout -q main
git -C "$REPO" merge -q work
git -C "$REPO" checkout -q work
check "a declaration for a workflow now on the default branch fails" rc1 "$(verdict)"

grep -F 'stopped meaning anything' "$TMP/out" >/dev/null \
    && echo "PASS: the stale message says to remove it" \
    || { echo "FAIL: stale entry message missing"; fails=1; }

# --- scope: only dispatchable workflows are in scope -------------------
rm -f "$REPO/.github/dispatch-pending.txt"
push_only pushonly > "$REPO/.github/workflows/pushonly.yml"
check "a non-dispatchable workflow absent from the default branch is out of scope" pass "$(verdict)"

# --- an unreadable default branch is NOT a pass ------------------------
dispatchable another > "$REPO/.github/workflows/another.yml"
out=$( cd "$REPO" && BASE_REF=refs/heads/no-such-branch bash "$CHECK" 2>&1 )
rc=$?
check "an unreadable default branch exits 0" 0 "$rc"
printf '%s\n' "$out" | grep -F 'NOT INSPECTED' >/dev/null \
    && echo "PASS: and says NOT INSPECTED rather than passing silently" \
    || { echo "FAIL: silent pass on an unreadable default branch"; fails=1; }

# --- cannot-check is distinct from broken ------------------------------
out_rc=$( cd "$REPO" && BASE_REF=main bash "$CHECK" .github/nope >/dev/null 2>&1; echo $? )
check "a missing workflow directory is rc2, not rc1" 2 "$out_rc"

# --- allowlist parsing --------------------------------------------------
# Two workflows missing from the default branch, one declared and one
# not: the declared one must be accepted THROUGH the comments and blank
# lines, and the undeclared one must still be reported. A single-entry
# case cannot tell "parsed the file" from "ignored the file".
dispatchable undeclared > "$REPO/.github/workflows/undeclared.yml"
printf '\n# a comment\n\n.github/workflows/another.yml\n' > "$REPO/.github/dispatch-pending.txt"
check "an undeclared workflow alongside a declared one fails" rc1 "$(verdict)"
grep -F 'undeclared.yml' "$TMP/out" >/dev/null \
    && echo "PASS: and the undeclared one is the one reported" \
    || { echo "FAIL: undeclared workflow not reported"; fails=1; }
grep -F 'another.yml' "$TMP/out" >/dev/null \
    && { echo "FAIL: the declared entry was not honoured through comments"; fails=1; } \
    || echo "PASS: the declared entry was honoured through comments and blanks"

# --- the real repository ------------------------------------------------
# The shipped state must satisfy its own gate.
real=$( cd "$(dirname "$CHECK")/.." && bash "$CHECK" >/dev/null 2>&1 && echo pass || echo "rc$?" )
check "this repository passes its own dispatch-reachability gate" pass "$real"

exit "$fails"
