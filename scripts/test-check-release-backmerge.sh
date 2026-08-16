#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-release-backmerge.sh (#598). Builds throwaway git
# repos through the BACKMERGE_GIT_DIR seam and pins "now" with
# BACKMERGE_NOW so the grace window is exact rather than approximate.
#
# The cases that carry the weight:
#
#   - the v1.6.0 SHAPE: a merge commit on main whose second parent is
#     the dev tip, leaving the two trees byte-identical. That is what
#     actually shipped, and it is invisible to every content-based
#     check in the repo. The test asserts the trees are equal AND that
#     the gate still goes red.
#   - DIRECTION: dev ahead of main is the normal state of the branch
#     model and must stay green. A guard that fires on both directions
#     would be red permanently and therefore ignored.
#   - the blindness guards: shallow clone and unresolvable ref must
#     exit 2, never 0. A gate that cannot see must say so.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-release-backmerge.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Fixed clock. Every commit date below is expressed as an offset from
# this, and the gate is told the same value as "now".
NOW=1755000000
H=3600

failures=0
n=0

git_q() { git -C "$1" -c user.email=t@example.invalid -c user.name=t "${@:2}"; }

# newrepo DIR — a repo with main at one commit and dev equal to it.
newrepo() {
    local d="$TMP/$1"
    mkdir -p "$d"
    git -C "$d" init -q -b main
    commit "$1" "base" $((NOW - 200 * H))
    git_q "$d" branch -f dev main
    echo "$d"
}

# commit REPO MSG EPOCH [BRANCH]
commit() {
    local d="$TMP/$1"
    GIT_AUTHOR_DATE="@$3 +0000" GIT_COMMITTER_DATE="@$3 +0000" \
        git_q "$d" commit -q --allow-empty -m "$2"
}

# check NAME WANT_EXIT REPO_DIR GREP_PATTERN [extra env assignments...]
check() {
    local name="$1" want_exit="$2" dir="$3" want_grep="$4"; shift 4
    n=$((n + 1))
    env BACKMERGE_GIT_DIR="$dir" BACKMERGE_NOW="$NOW" \
        BACKMERGE_BASE=main BACKMERGE_HEAD=dev "$@" \
        bash "$CHECK" > "$TMP/out" 2>&1
    local got=$?
    local ok=1
    [ "$got" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# --- main contained in dev: the healthy state -------------------------
d="$(newrepo contained)"
git_q "$d" checkout -q dev
commit contained "work on dev" $((NOW - 100 * H))
check "main fully contained in dev passes" 0 "$d" "fully contained"

# --- DIRECTION: dev ahead of main is normal, not a finding ------------
# Same repo as above — dev is 1 commit ahead. Asserting it explicitly so
# the one-directional intent is pinned by a test and not only by prose.
check "dev ahead of main is not reported" 0 "$d" "fully contained"

# --- main ahead, past the grace window --------------------------------
d="$(newrepo stale)"
git_q "$d" checkout -q main
commit stale "release merge" $((NOW - 48 * H))
check "main-only commit older than grace fails" 1 "$d" "back-merge was skipped"

# --- main ahead, still inside the grace window ------------------------
d="$(newrepo inflight)"
git_q "$d" checkout -q main
commit inflight "release merge" $((NOW - 2 * H))
check "main-only commit inside grace passes with a notice" 0 "$d" "within the"

# --- the age is the OLDEST straggler's, not the newest ----------------
# A fresh commit landing on top of an old omission must not reset the
# clock and hide it for another day.
d="$(newrepo oldest)"
git_q "$d" checkout -q main
commit oldest "old release merge" $((NOW - 48 * H))
commit oldest "fresh commit on top" $((NOW - 1 * H))
check "a fresh commit does not reset the clock" 1 "$d" "back-merge was skipped"

# --- grace is honoured as configured ----------------------------------
check "a wider grace window accepts the same divergence" 0 "$d" "within the" \
    BACKMERGE_GRACE_HOURS=72

# --- THE v1.6.0 SHAPE: identical trees, divergent graph ---------------
# main gets a merge commit whose second parent IS the dev tip, exactly
# as a release PR produces. No file differs afterwards.
d="$(newrepo releaseshape)"
git_q "$d" checkout -q dev
echo "shipped" > "$d/RELEASE"
git_q "$d" add RELEASE
commit releaseshape "prepare release" $((NOW - 60 * H))
git_q "$d" checkout -q main
GIT_AUTHOR_DATE="@$((NOW - 50 * H)) +0000" GIT_COMMITTER_DATE="@$((NOW - 50 * H)) +0000" \
    git_q "$d" merge -q --no-ff -m "Merge pull request #1 from dev" dev
# Precondition: this is the invisible case. If the trees ever differ,
# this test has stopped covering what it claims to.
if [ "$(git -C "$d" rev-parse main^{tree})" != "$(git -C "$d" rev-parse dev^{tree})" ]; then
    echo "FAIL: release-shape fixture is not content-identical — test is invalid"
    failures=$((failures + 1))
fi
n=$((n + 1))
if git -C "$d" diff --quiet main dev; then
    echo "PASS: release-shape fixture leaves no content difference at all"
else
    echo "FAIL: release-shape fixture has a content diff; wrong fixture"
    failures=$((failures + 1))
fi
check "a content-identical release merge is still caught" 1 "$d" "back-merge was skipped"

# --- advisory mode ----------------------------------------------------
# The case this mode exists for: a divergence WELL INSIDE the grace
# window, which the scheduled check deliberately stays quiet about, and
# which merging into dev anyway is what turns permanent. If a shared
# grace ever creeps back in, this goes red.
d="$(newrepo advisoryfresh)"
git_q "$d" checkout -q main
commit advisoryfresh "release merge, one hour ago" $((NOW - 1 * H))
check "advisory warns inside the grace window (the case it exists for)" 0 "$d" \
    "::warning" BACKMERGE_ADVISORY=1
check "the same repo is silent in scheduled mode" 0 "$d" "within the"

# Advisory never blocks, however old the divergence is.
d="$(newrepo advisorystale)"
git_q "$d" checkout -q main
commit advisorystale "release merge, long ago" $((NOW - 200 * H))
check "advisory warns rather than failing on a stale divergence" 0 "$d" \
    "::warning" BACKMERGE_ADVISORY=1
check "the same repo DOES fail in scheduled mode" 1 "$d" "back-merge was skipped"

# Nothing to say when there is nothing wrong — an advisory that warns on
# every PR is one nobody reads.
d="$(newrepo advisoryclean)"
n=$((n + 1))
if env BACKMERGE_GIT_DIR="$d" BACKMERGE_NOW="$NOW" BACKMERGE_ADVISORY=1 \
       BACKMERGE_BASE=main BACKMERGE_HEAD=dev bash "$CHECK" > "$TMP/out" 2>&1 \
   && ! grep -q "::warning" "$TMP/out"; then
    echo "PASS: advisory is silent when main is contained"
else
    echo "FAIL: advisory warned with nothing to warn about"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

# Advisory must NOT swallow a blind gate. Exit 2 means the check cannot
# see; downgrading that to a green PR turns the advisory into decoration.
d="$(newrepo advisoryshallow)"
git -C "$d" rev-parse main > "$d/.git/shallow"
check "advisory still exits 2 on a shallow clone" 2 "$d" "fetch-depth 0" \
    BACKMERGE_ADVISORY=1
d="$(newrepo advisorymissing)"
check "advisory still exits 2 on an unresolvable ref" 2 "$d" "must go red here" \
    BACKMERGE_ADVISORY=1 BACKMERGE_BASE=refs/heads/renamed-away

# --- defaults resolve origin/main and origin/dev ----------------------
# Every case above overrides both refs. This one does not, so the
# documented defaults are exercised rather than assumed.
d="$(newrepo defaults)"
git_q "$d" update-ref refs/remotes/origin/main main
git_q "$d" update-ref refs/remotes/origin/dev dev
n=$((n + 1))
if env BACKMERGE_GIT_DIR="$d" BACKMERGE_NOW="$NOW" bash "$CHECK" > "$TMP/out" 2>&1 \
   && grep -q "origin/main is fully contained in origin/dev" "$TMP/out"; then
    echo "PASS: default refs are origin/main and origin/dev"
else
    echo "FAIL: default refs did not resolve"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

# --- blindness guard: unresolvable ref --------------------------------
d="$(newrepo missingref)"
check "a ref that does not resolve exits 2, loudly" 2 "$d" "must go red here" \
    BACKMERGE_BASE=refs/heads/renamed-away

# --- blindness guard: shallow clone -----------------------------------
# actions/checkout defaults to depth 1, which is the likeliest way for
# this gate to start answering confidently and wrongly. The marker file
# is the same one git itself reads for --is-shallow-repository.
d="$(newrepo shallow)"
git -C "$d" rev-parse main > "$d/.git/shallow"
check "a shallow clone refuses to answer" 2 "$d" "fetch-depth 0"

# --- blindness guard: not a repository at all -------------------------
mkdir -p "$TMP/plain"
check "a directory that is not a repo exits 2" 2 "$TMP/plain" "not a git repository"
check "a directory that does not exist exits 2" 2 "$TMP/nope" "is not a directory"

# --- input validation --------------------------------------------------
d="$(newrepo badgrace)"
check "a non-numeric grace window is rejected" 2 "$d" "whole" \
    BACKMERGE_GRACE_HOURS=soon

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n check(s) failed" >&2
    exit 1
fi
echo "all $n check(s) passed"
