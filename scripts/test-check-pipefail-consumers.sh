#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-pipefail-consumers.sh.
#
# The racy construct is spelled through a variable everywhere below, so
# this file does not contain the literal pattern it is testing for. A
# self-test that trips the gate it tests is not a clever edge case; it
# is a gate that can never be green.
set -u

GATE="$(cd "$(dirname "$0")" && pwd)/check-pipefail-consumers.sh"
pass=0
fail=0
Q='q'
# Same reason as Q: the head fixture below must not make THIS file a
# finding of the gate it is testing.
H='head'

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

run_case() {
    local name="$1" want="$2"; shift 2
    local dir rc out
    dir=$(mktemp -d)
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        while [ "$#" -gt 0 ]; do
            local path="${1%%:::*}" body="${1#*:::}"
            mkdir -p "$(dirname "$path")"
            printf '%s\n' "$body" > "$path"
            shift
        done
        git add -A
        git commit -qm fixture
    ) >/dev/null 2>&1
    out=$(PIPE_ROOT="$dir" bash "$GATE" 2>&1)
    rc=$?
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then
        ok "$name"
    else
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
    fi
}

# The shape that shipped and lied in CI.
run_case "a pipe into grep -${Q} is reported" 1 \
    "scripts/x.sh:::set -uo pipefail
if git grep -n foo | grep -${Q}E 'bar'; then echo hi; fi"

# The fix, which must read as clean or nobody can satisfy the gate.
run_case "the redirect form is clean" 0 \
    "scripts/x.sh:::set -uo pipefail
if git grep -n foo | grep -E 'bar' >/dev/null; then echo hi; fi"

# Not a pipeline at all: there grep reads a file and its own status is
# the one that counts. Flagging it would fire on correct code.
run_case "a || before grep -${Q} is not a pipeline" 0 \
    "scripts/x.sh:::set -uo pipefail
[ -z \"\$want\" ] || grep -${Q} -- \"\$want\" out.txt"

# Prose describing the bug must not be the bug.
run_case "a commented example is not flagged" 0 \
    "scripts/x.sh:::set -uo pipefail
# never write: producer | grep -${Q}E 'pat'
echo fine"

# The adjacent construct. Harmless today only because every occurrence
# sits in a \$(...) whose status nobody reads; in a condition the status
# is exactly what is read, so that is where it is rejected.
run_case "a head pipeline in a condition is reported" 1 \
    "scripts/x.sh:::set -uo pipefail
if producer | ${H} -1; then echo hi; fi"

run_case "a head pipeline inside a substitution is clean" 0 \
    "scripts/x.sh:::set -uo pipefail
first=\$(producer | ${H} -1)
echo \"\$first\""

# A substitution INSIDE a condition is still a substitution: the status
# belongs to the test, not to the pipeline.
run_case "a substitution inside a condition is clean" 0 \
    "scripts/x.sh:::set -uo pipefail
if [ -n \"\$(producer | ${H} -1)\" ]; then echo hi; fi"

# --- rule three: a $(... | head) whose status is CONSUMED --------------
# The exemption for `$(... | head)` was correct and it had a condition:
# nothing reads the substitution status. These are the forms that read
# it. Each was found in the wild or is one operator away from one.

run_case "a || after the substitution is reported" 1 \
    "scripts/x.sh:::set -uo pipefail
row=\$(producer | ${H} -1) || echo boom"

run_case "an && after the substitution is reported" 1 \
    "scripts/x.sh:::set -uo pipefail
row=\$(producer | ${H} -1) && echo ok"

# THE SHAPE IT WAS ACTUALLY FOUND IN. The operator is on the next line,
# so a line-at-a-time reader cannot see it -- and would report the same
# clean tree, for the wrong reason.
run_case "a || on the CONTINUED line is reported" 1 \
    "scripts/x.sh:::set -uo pipefail
row=\$(producer | ${H} -1) \\
    || echo boom"

run_case "an assignment in an if condition is reported" 1 \
    "scripts/x.sh:::set -uo pipefail
if row=\$(producer | ${H} -1); then echo hi; fi"

# The exemption, said out loud. `|| true` reads the status in order to
# throw it away, which is exactly what the old blanket exemption assumed
# everyone was doing.
run_case "|| true inside the substitution is clean" 0 \
    "scripts/x.sh:::set -uo pipefail
row=\$(producer | ${H} -1 || true)"

run_case "|| true after the substitution is clean" 0 \
    "scripts/x.sh:::set -uo pipefail
row=\$(producer | ${H} -1) || true"

# THE EXCLUSION MUST NOT LEAK ALONG THE LINE. A line-level "does this
# line contain || true" test would exempt BOTH substitutions here, and
# an exemption that leaks always leaks in the direction that makes the
# gate silent. An exclusion tested only in its passing direction is a
# hole with a green light on it.
run_case "a || true elsewhere on the line does not exempt the real one" 1 \
    "scripts/x.sh:::set -uo pipefail
a=\$(producer | ${H} -1) || true; b=\$(producer | ${H} -1) || echo boom"

# The exemption that stays. Nothing reads this status, no script here
# sets -e, and the captured value is right.
run_case "a plain assignment is still clean" 0 \
    "scripts/x.sh:::set -uo pipefail
row=\$(producer | ${H} -1)
echo \"\$row\""

# Inspecting nothing is not a pass.
run_case "a repo with no shell scripts exits 2" 2 \
    "README.md:::nothing here"

# --- an unstaged file is still a file (#743) ----------------------------
# This gate runs in scripts/local-lane.sh, where the working tree is
# uncommitted BY DEFINITION — you run the lane on what you just wrote.
# Selecting subjects with `ls-files` alone meant the gate was blind for
# exactly as long as the author was still writing, and CI never showed
# it: a fresh checkout makes "tracked" and "present" the same set.
untracked_case() {
    local name="$1" want="$2"; shift 2
    local dir rc out
    dir=$(mktemp -d)
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        while [ "$#" -gt 0 ]; do
            local path="${1%%:::*}" body="${1#*:::}"
            mkdir -p "$(dirname "$path")"
            printf '%s\n' "$body" > "$path"
            shift
        done
        # deliberately NO `git add` — that is the whole case
    ) >/dev/null 2>&1
    out=$(PIPE_ROOT="$dir" bash "$GATE" 2>&1)
    rc=$?
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then
        ok "$name"
    else
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
    fi
}

untracked_case "an UNTRACKED script with the bad shape is reported" 1 \
    "scripts/x.sh:::set -uo pipefail
if git grep -n foo | grep -${Q}E 'bar'; then echo hi; fi"

untracked_case "an UNTRACKED clean script reads as clean, not as absent" 0 \
    "scripts/x.sh:::set -uo pipefail
if git grep -n foo | grep -E 'bar' >/dev/null; then echo hi; fi"

# ...and ignored files stay ignored: --exclude-standard is what keeps a
# vendored or build-output tree from becoming this gate's problem.
untracked_case "a .gitignore'd script is not inspected" 2 \
    ".gitignore:::vendor/
vendor/x.sh:::set -uo pipefail
if git grep -n foo | grep -${Q}E 'bar'; then echo hi; fi"

dir=$(mktemp -d)
if PIPE_ROOT="$dir" bash "$GATE" >/dev/null 2>&1; then
    no "a non-git directory should not report clean"
else
    [ "$?" = 2 ] && ok "a non-git directory exits 2" || no "a non-git directory should exit 2"
fi
rm -rf "$dir"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
