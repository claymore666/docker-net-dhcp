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
# -e, NOT -s. `-s` is false for a file that EXISTS AND IS EMPTY, which is
# precisely what a failing `> "$OUT"` leaves behind -- so the one shape
# this is here to catch was the one shape it could not see.
#
# This case reaches an EARLIER guard than the redirect, so `$OUT` was
# never created and this passes for a reason unrelated to any cleanup.
# It is kept as the control, and labelled so nobody reads it as the
# observer; the arm itself is driven below.
if [ ! -e "$dir/out.txt" ]; then ok "an unresolvable ref never creates the output at all"; else no "an unresolvable ref left $(wc -c < "$dir/out.txt") byte(s) at out.txt"; fi
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
# THE ARM THAT NOTHING OBSERVED (#789). This is the only case that
# reaches
#
#     if ! git show "$MERGE_BASE:$BASELINE_PATH" > "$OUT" 2>/dev/null; then
#         ... ; rm -f "$OUT" ; exit 2
#
# and it asserted the exit status alone. `> "$OUT"` CREATES THE FILE
# BEFORE the command can fail, so on this arm `rm -f` is the only thing
# standing between a refusal and an empty baseline left on disk. Delete
# it and the whole suite stayed green -- the production path was right
# and unobserved.
#
# An empty baseline left behind is not cosmetic: coverage.yml hands that
# path straight to coverage-ratchet.sh, whose own non-vacuity guard is
# the next thing that has to catch it (#791). Two guards deep is not
# where this should be caught.
if [ ! -e "$dir/out.txt" ]; then
    ok "and removes the output the failing redirect had already created"
else
    no "a refusal left $(wc -c < "$dir/out.txt") byte(s) at out.txt — the cleanup beside the failing redirect is gone"
fi
rm -rf "$dir"

if bash "$GATE" >/dev/null 2>&1; then
    no "usage error should exit 2"
else
    [ $? -eq 2 ] && ok "usage error exits 2" || no "usage error should exit 2"
fi

# --- the report the ratchet cross-checks against (#791) ------------------
# coverage-ratchet.sh cannot tell a complete baseline from a truncated one:
# its `compared` count is derived from the file it was handed, so the
# count has to come from whoever resolved the blob. That is this script.
dir=$(mktemp -d)
build_repo "$dir"
(cd "$dir" && bash "$GATE" dev "$dir/out.txt" >/dev/null 2>&1)

if [ -f "$dir/out.txt.report" ]; then
    ok "the report defaults to <output-file>.report, so no caller has to wire it"
else
    no "no report was written beside the output"
fi
if grep -qx 'count 2' "$dir/out.txt.report" 2>/dev/null; then
    ok "the report counts the data lines it resolved"
else
    no "report count wrong: $(sed -n 's/^count //p' "$dir/out.txt.report" 2>/dev/null)"
fi
# NAMES, not just a count: "2 of 5" sends someone to read a 258-line file.
if grep -qx 'package pkg/a' "$dir/out.txt.report" && grep -qx 'package pkg/b' "$dir/out.txt.report"; then
    ok "the report names the packages, not only how many"
else
    no "the report does not name both packages"; sed 's/^/    /' "$dir/out.txt.report"
fi
# The blob, so the report identifies an OBJECT and not a path. A hash of
# the copy would still agree with itself after the copy was truncated.
if grep -qE '^blob [0-9a-f]{40}$' "$dir/out.txt.report"; then
    ok "the report records the blob it read, not just the path"
else
    no "the report carries no blob id"; sed 's/^/    /' "$dir/out.txt.report"
fi

# An explicit third argument overrides the default.
(cd "$dir" && bash "$GATE" dev "$dir/out2.txt" "$dir/elsewhere.report" >/dev/null 2>&1)
[ -f "$dir/elsewhere.report" ] \
    && ok "an explicit report path is honoured" \
    || no "the third argument was ignored"
rm -rf "$dir"

# --- non-vacuity AT THE SOURCE ------------------------------------------
# A merge-base baseline of pure commentary is the extreme of the
# truncation the report exists to catch. Handing it on would produce the
# ratchet's refusal one step later, naming a temp file instead of the
# merge base that actually produced it — and, before #791, the ratchet's
# guard was the ONLY thing that would have caught it at all.
dir=$(mktemp -d)
(
    cd "$dir" || exit 2
    git init -q -b dev .
    git config user.email t@t; git config user.name t
    git config commit.gpgsign false
    mkdir -p .github
    printf '# floors live here\n\n' > .github/coverage-baseline.txt
    git add -A; git commit -qm "commentary only"
    git checkout -q -b feature
    git commit -q --allow-empty -m "work"
) >/dev/null 2>&1
(cd "$dir" && bash "$GATE" dev "$dir/out.txt" >/dev/null 2>&1); rc=$?
[ "$rc" -eq 2 ] \
    && ok "a merge-base baseline holding no data lines refuses" \
    || no "a commentary-only baseline was handed on (exit $rc)"
[ ! -e "$dir/out.txt" ] \
    && ok "and leaves no output behind for the next step to read" \
    || no "a refusal left $(wc -c < "$dir/out.txt") byte(s) at out.txt"
rm -rf "$dir"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
