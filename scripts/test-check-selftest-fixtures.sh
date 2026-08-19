#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-selftest-fixtures.sh.
#
# Both directions, as everywhere else: a fixture that inherits the
# developer's config must be reported, and one that pins it must not.
# A gate that fires on every fixture would be waived on the first run.
set -u

GATE="$(cd "$(dirname "$0")" && pwd)/check-selftest-fixtures.sh"
pass=0
fail=0

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
    out=$(FIXTURE_ROOT="$dir" bash "$GATE" 2>&1)
    rc=$?
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then
        ok "$name"
    else
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
    fi
}

FULL='git init -q .
git config user.email t@t
git config user.name t
git config commit.gpgsign false
git commit -qm fixture'

# The exact shape that hung: identity pinned, signing inherited. The two
# settings a fixture obviously needs were there, and the one whose
# absence hangs rather than fails was not.
run_case "identity without gpgsign is reported" 1 \
    "scripts/test-x.sh:::git init -q .
git config user.email t@t
git config user.name t
git commit -qm fixture"

run_case "all three settings is clean" 0 \
    "scripts/test-x.sh:::$FULL"

run_case "a missing identity is reported too" 1 \
    "scripts/test-x.sh:::git init -q .
git config commit.gpgsign false
git commit -qm fixture"

# Scope control. A script that never stands up a repository has nothing
# to pin, and demanding it would make the gate fire on most of scripts/.
run_case "a script that commits nothing is not judged" 0 \
    "scripts/test-x.sh:::$FULL" \
    "scripts/plain.sh:::echo hello"

# A fixture that only needs an index to read commits nothing, so it
# inherits nothing and needs no config. Demanding it there fires on a
# script with nothing to fix — check-test-dockerfile-pins is exactly
# this shape, and the init-based version of this gate flagged it.
run_case "an init-only fixture is not judged" 0 \
    "scripts/test-x.sh:::$FULL" \
    "scripts/test-index.sh:::git -C \"\$dir\" init -q
git -C \"\$dir\" add -A"

# The wrapper form, which is how two of the three real cases were
# written. A gate matching the literal string \`git commit\` sees none of
# them, and the one it does see is the one that was already fine.
run_case "a git_q wrapper commit is discovered" 1 \
    "scripts/test-x.sh:::git -C \"\$d\" init -q
git_q() { git -C \"\$1\" -c user.email=t@t -c user.name=t \"\${@:2}\"; }
git_q \"\$d\" commit -q --allow-empty -m base"

# git config keys are case-insensitive and one real fixture spells it
# gpgSign. Reporting that as missing is a false red, and a gate with a
# false red gets waived.
run_case "commit.gpgSign spelled with a capital S counts" 0 \
    "scripts/test-x.sh:::git init -q .
git config user.email t@t
git config user.name t
git config commit.gpgSign false
git commit -qm fixture"

# An annotated tag is signed under a global tag.gpgsign and blocks the
# same way; a lightweight one is not signed at all. The demand follows
# what the fixture actually does, in both directions.
run_case "an annotated tag also requires tag.gpgsign" 1 \
    "scripts/test-x.sh:::$FULL
git tag -a v1.0.0 -m release"

run_case "a lightweight tag does not require tag.gpgsign" 0 \
    "scripts/test-x.sh:::$FULL
git tag v1.0.0"

# One bad fixture beside a good one must still be caught: reporting the
# first and stopping, or passing because most were fine, are both ways
# this quietly stops working.
run_case "a bad fixture beside a good one is still caught" 1 \
    "scripts/test-good.sh:::$FULL" \
    "scripts/test-bad.sh:::git init -q .
git config user.email t@t
git config user.name t
git commit -qm fixture"

# Inspecting nothing is not a pass.
run_case "a repo where nothing commits at all exits 2" 2 \
    "README.md:::nothing here"

dir=$(mktemp -d)
if FIXTURE_ROOT="$dir" bash "$GATE" >/dev/null 2>&1; then
    no "a non-git directory should not report clean"
else
    [ "$?" = 2 ] && ok "a non-git directory exits 2" || no "a non-git directory should exit 2"
fi
rm -rf "$dir"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
