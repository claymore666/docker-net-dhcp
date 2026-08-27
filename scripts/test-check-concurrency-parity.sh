#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Meta-test for check-concurrency-parity.sh (#742).
#
# The case that matters is the first one: it is the tree as it actually
# stood between 76d0b7f and this gate — two lanes on the #617 key and
# one on the pre-#617 key. Nothing in CI could see it, because a
# concurrency group is evaluated by GitHub and never by us.
set -uo pipefail

GATE="$(cd "$(dirname "$0")" && pwd)/check-concurrency-parity.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

NEW='selfhosted-privileged-${{ github.event_name == '"'"'push'"'"' && github.sha || github.ref }}'
OLD='selfhosted-privileged-${{ github.ref }}'

# THE LANE LIST IS READ FROM THE GATE, NEVER RETYPED. Every case below
# builds a fixture directory, and the gate exits 2 when a lane it
# declares is absent from that directory. So a self-test carrying its
# own copy of the list does not merely go stale when a lane is added --
# it turns EVERY case into "exit 2, a lane is missing", which is not the
# property any of them was written to measure. That is what a fourth
# privileged lane (#690) did to nine of these twelve cases.
#
# The parse is spelling-keyed, which is the one thing that can go wrong
# here: reformat the array literal in the gate and this reads nothing.
# So it refuses rather than continuing with an empty list -- an empty
# list would make `fill` a no-op and quietly restore the exact failure
# above.
mapfile -t LANE_FILES < <(sed -n 's/^LANES=(\(.*\))[[:space:]]*$/\1/p' "$GATE" | tr ' ' '\n' | grep .)
if [ "${#LANE_FILES[@]}" -lt 2 ]; then
    echo "FAIL  cannot read the LANES=(...) list out of $GATE; every case below would" >&2
    echo "      measure a missing lane rather than the property it names." >&2
    exit 2
fi

# Write a compliant lane for every declared lane the case did not name.
# A case says what it is VARYING; everything else is background, and
# background that is absent is a different test.
fill() {
    local d="$1" lane
    for lane in "${LANE_FILES[@]}"; do
        [ -e "$d/$lane" ] && continue
        printf 'name: %s\non:\n  workflow_dispatch:\nconcurrency:\n  group: %s\n  cancel-in-progress: false\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' \
            "$lane" "$NEW" > "$d/$lane"
    done
}

# $1 name, $2 want-rc, then `file:::group-line-or-empty` triples.
# test.yaml is written for every case unless a triple overrides it: it
# is in MUST_HAVE_GROUP but not in LANES, so a case that omits it is
# measuring a missing file rather than the property under test.
run_case() {
    local name="$1" want="$2"; shift 2
    local dir rc out
    dir=$(mktemp -d)
    printf 'name: test\non:\n  pull_request:\nconcurrency:\n  group: test-${{ github.ref }}\n' \
        > "$dir/test.yaml"
    while [ "$#" -gt 0 ]; do
        local f="${1%%:::*}" g="${1#*:::}"
        {
            printf 'name: %s\non:\n  workflow_dispatch:\n' "$f"
            if [ -n "$g" ]; then
                printf 'concurrency:\n  group: %s\n  cancel-in-progress: false\n' "$g"
            fi
            printf 'jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n'
        } > "$dir/$f"
        shift
    done
    fill "$dir"
    out=$(bash "$GATE" "$dir" 2>&1)
    rc=$?
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then
        ok "$name"
    else
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
    fi
}

# --- the shape that shipped --------------------------------------------
run_case "the pre-#617 key on one lane and the #617 key on two is reported" 1 \
    "integration.yml:::$NEW" \
    "coverage.yml:::$NEW" \
    "capture-fixtures.yml:::$OLD"

# --- the fix, which must read as clean or nobody can satisfy the gate ---
run_case "three identical keys are clean" 0 \
    "integration.yml:::$NEW" \
    "coverage.yml:::$NEW" \
    "capture-fixtures.yml:::$NEW"

# --- drift in either direction -----------------------------------------
# A single-direction case cannot tell "compares the set" from "compares
# against integration.yml", which would miss the drift landing there.
run_case "drift on integration.yml itself is reported too" 1 \
    "integration.yml:::$OLD" \
    "coverage.yml:::$NEW" \
    "capture-fixtures.yml:::$NEW"

# --- whitespace is not equivalence -------------------------------------
# Byte-identical is the deliberate bar: GitHub evaluates these, we do
# not, and "obviously the same" is the judgement that produced the
# original drift.
run_case "a key differing only in spacing is still drift" 1 \
    "integration.yml:::$NEW" \
    "coverage.yml:::$NEW" \
    "capture-fixtures.yml:::selfhosted-privileged-\${{ github.event_name=='push' && github.sha || github.ref }}"

# --- a lane with no group at all ---------------------------------------
# The worst version of this failure, and the one a set-comparison alone
# would call clean: two lanes agree, and the third excludes nothing.
run_case "a privileged lane with no concurrency group is reported" 1 \
    "integration.yml:::$NEW" \
    "coverage.yml:::$NEW" \
    "capture-fixtures.yml:::"

# --- a lane with no concurrency block at all (#742) ---------------------
# test.yaml is the case this covers: hosted, so it has no business in
# the privileged group, and the only CI-heavy workflow in the tree that
# had never had a group of any kind. A gate that only compared the
# privileged three would have called that clean forever.
priv3() {
    local d="$1"
    printf 'name: x\non:\n  workflow_dispatch:\nconcurrency:\n  group: %s\n' "$NEW" > "$d/integration.yml"
    printf 'name: x\non:\n  workflow_dispatch:\nconcurrency:\n  group: %s\n' "$NEW" > "$d/coverage.yml"
    printf 'name: x\non:\n  workflow_dispatch:\nconcurrency:\n  group: %s\n' "$NEW" > "$d/capture-fixtures.yml"
    fill "$d"
}

dir=$(mktemp -d)
priv3 "$dir"
printf 'name: test\non:\n  pull_request:\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: "true"}]\n' \
    > "$dir/test.yaml"
bash "$GATE" "$dir" >/dev/null 2>&1
rc=$?
rm -rf "$dir"
[ "$rc" = 1 ] \
    && ok "a declared lane with no concurrency block at all is reported" \
    || no "a lane with no concurrency block should exit 1 (got $rc)"

# ...and a commented-out block does not count as having one, for the
# same reason the key comparison strips comments.
dir=$(mktemp -d)
priv3 "$dir"
printf 'name: test\non:\n  pull_request:\n# concurrency:\n#   group: test-x\n' > "$dir/test.yaml"
bash "$GATE" "$dir" >/dev/null 2>&1
rc=$?
rm -rf "$dir"
[ "$rc" = 1 ] \
    && ok "a commented-out concurrency block does not count as having one" \
    || no "a commented-out block was accepted (got $rc)"

# A lane outside the privileged group needs A group, not THEIR group —
# otherwise the presence check would quietly become a demand that
# test.yaml join a self-hosted exclusion it has no business in.
dir=$(mktemp -d)
priv3 "$dir"
printf 'name: test\non:\n  pull_request:\nconcurrency:\n  group: test-${{ github.ref }}\n' > "$dir/test.yaml"
bash "$GATE" "$dir" >/dev/null 2>&1
rc=$?
rm -rf "$dir"
[ "$rc" = 0 ] \
    && ok "a non-privileged lane with its own distinct group is clean" \
    || no "a non-privileged lane with its own group should pass (got $rc)"

# --- prose cannot answer for the file ----------------------------------
# These workflows quote their own key at length in comments, on purpose.
# If the gate read comments, the drifted file would describe itself into
# compliance — the shape that made check-python-deps.sh satisfy itself
# from its own header (#743).
dir=$(mktemp -d)
printf 'name: x\non:\n  workflow_dispatch:\nconcurrency:\n  group: %s\n' "$NEW" > "$dir/integration.yml"
printf 'name: x\non:\n  workflow_dispatch:\nconcurrency:\n  group: %s\n' "$NEW" > "$dir/coverage.yml"
printf 'name: test\non:\n  pull_request:\nconcurrency:\n  group: test-${{ github.ref }}\n' > "$dir/test.yaml"
{
    printf 'name: x\non:\n  workflow_dispatch:\n'
    printf '# this lane shares  group: %s  with the others\n' "$NEW"
    printf 'concurrency:\n  group: %s\n' "$OLD"
} > "$dir/capture-fixtures.yml"
fill "$dir"
bash "$GATE" "$dir" >/dev/null 2>&1
rc=$?
rm -rf "$dir"
[ "$rc" = 1 ] \
    && ok "a comment quoting the correct key does not satisfy the gate" \
    || no "a comment quoting the correct key satisfied the gate (exit $rc, want 1)"

# --- cannot-check is distinct from broken -------------------------------
dir=$(mktemp -d)
printf 'name: x\n' > "$dir/integration.yml"
bash "$GATE" "$dir" >/dev/null 2>&1
rc=$?
rm -rf "$dir"
[ "$rc" = 2 ] \
    && ok "a declared lane missing from the directory is rc2, not rc1" \
    || no "a missing declared lane should exit 2 (got $rc)"

bash "$GATE" /nonexistent-workflow-dir >/dev/null 2>&1
rc=$?
[ "$rc" = 2 ] \
    && ok "a missing workflow directory is rc2" \
    || no "a missing workflow directory should exit 2 (got $rc)"

# --- the real repository ------------------------------------------------
real_rc=$( cd "$(dirname "$GATE")/.." && bash "$GATE" >/dev/null 2>&1; echo $? )
[ "$real_rc" = 0 ] \
    && ok "this repository passes its own concurrency-parity gate" \
    || no "this repository fails its own concurrency-parity gate (exit $real_rc)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
