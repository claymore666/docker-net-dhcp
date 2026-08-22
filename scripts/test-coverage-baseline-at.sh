#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for coverage-baseline-at.sh (#735).
#
# The script it covers runs in one place only — coverage.yml, on pull
# requests into main — so without this suite its first execution would
# be the release PR. That is the entire reason the resolution was pulled
# out of the workflow's `run:` block.
#
# The load-bearing case is the third: the merge base must win over BOTH
# the branch's copy and the base branch's tip, because a floor lowered on
# this branch is exactly what the merge-base read exists to defeat.
set -u

GATE="$(cd "$(dirname "$0")" && pwd)/coverage-baseline-at.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# A repo shaped like a real PR: a base branch that has moved on since the
# fork point, and a feature branch that lowered a floor.
build_repo() {
    local dir="$1"
    (
        cd "$dir" || exit 2
        git init -q -b dev .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        mkdir -p .github
        printf 'pkg/a 80.0\npkg/b 50.0\n' > .github/coverage-baseline.txt
        git add -A; git commit -qm "fork point"

        git checkout -q -b feature
        printf 'pkg/a 70.0\npkg/b 50.0\n' > .github/coverage-baseline.txt
        git add -A; git commit -qm "lower a floor"

        # dev moves on after the fork point, so merge-base != base tip.
        git checkout -q dev
        printf 'pkg/a 80.0\npkg/b 55.0\n' > .github/coverage-baseline.txt
        git add -A; git commit -qm "raise b on dev"

        git checkout -q feature
    ) >/dev/null 2>&1
}

dir=$(mktemp -d)
build_repo "$dir"

got=$(cd "$dir" && bash "$GATE" dev "$dir/out.txt" >/dev/null 2>&1; echo $?)
if [ "$got" = "0" ]; then ok "resolves and writes"; else no "resolves and writes (exit $got)"; fi

# The fork point had pkg/a 80.0. The branch says 70.0 and dev's tip says
# 80.0/55.0 — only the merge base has 80.0 AND 50.0 together.
if grep -q '^pkg/a 80.0$' "$dir/out.txt" 2>/dev/null; then
    ok "takes the floor from the merge base, not from the branch"
else
    no "takes the floor from the merge base, not from the branch"
    sed 's/^/    /' "$dir/out.txt" 2>/dev/null
fi

if grep -q '^pkg/b 50.0$' "$dir/out.txt" 2>/dev/null; then
    ok "takes the merge base, not the base branch's tip"
else
    no "takes the merge base, not the base branch's tip"
    sed 's/^/    /' "$dir/out.txt" 2>/dev/null
fi
rm -rf "$dir"

# An unresolvable base ref must refuse, never fall back to the working
# copy — a fallback restores the defect silently, which is worse than red.
dir=$(mktemp -d)
build_repo "$dir"
got=$(cd "$dir" && bash "$GATE" no-such-ref "$dir/out.txt" >/dev/null 2>&1; echo $?)
if [ "$got" = "2" ]; then ok "an unresolvable base ref refuses"; else no "an unresolvable base ref refuses (exit $got)"; fi
if [ ! -s "$dir/out.txt" ]; then ok "and leaves no half-written baseline"; else no "and leaves no half-written baseline"; fi
rm -rf "$dir"

# A merge base that carries no baseline at all: the file was added later.
dir=$(mktemp -d)
(
    cd "$dir" || exit 2
    git init -q -b dev .
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    printf 'x\n' > README.md
    git add -A; git commit -qm "no baseline yet"
    git checkout -q -b feature
    mkdir -p .github
    printf 'pkg/a 80.0\n' > .github/coverage-baseline.txt
    git add -A; git commit -qm "introduce the baseline"
) >/dev/null 2>&1
got=$(cd "$dir" && bash "$GATE" dev "$dir/out.txt" >/dev/null 2>&1; echo $?)
if [ "$got" = "2" ]; then ok "a merge base without a baseline refuses"; else no "a merge base without a baseline refuses (exit $got)"; fi
rm -rf "$dir"

if bash "$GATE" >/dev/null 2>&1; [ $? -eq 2 ]; then
    ok "usage error exits 2"
else
    no "usage error should exit 2"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
