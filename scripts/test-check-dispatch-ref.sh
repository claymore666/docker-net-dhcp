#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-dispatch-ref.sh (#593). Builds throwaway git repos
# through the DISPATCH_GIT_DIR seam, including the one fixture that
# matters most: a commit that exists in the object store but is
# reachable from no branch or tag, published under `refs/pull/<N>/head`
# exactly as GitHub publishes a fork's pull-request head in the BASE
# repository.
#
# The cases that carry the weight:
#
#   - THE RAW SHA. A fork PR head's commit can be checked out by hash,
#     so a denylist on `refs/pull/*` catches nothing. The test asserts
#     the bare SHA is rejected, and asserts it is rejected by the
#     REACHABILITY message rather than the shape message — otherwise
#     the two assertions could collapse into one and nobody would
#     notice when the shape check alone was left behind.
#   - THE SHAPE CHECK ALONE IS NOT THE GUARD. The same fixture is fed
#     to a grep for the pull-ref pattern to show it does not match:
#     that is the old guard run against the new broken input.
#   - the blindness guards: shallow clone and a non-repository must
#     exit 2, never 0. A check that cannot see must say so, because
#     `--contains` on a truncated graph reports "not reachable" for
#     commits that are perfectly reachable.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-dispatch-ref.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
n=0

# Signing is disabled explicitly: this machine signs commits and tags by
# default, and an annotated signed tag needs a message, so a fixture
# that did not say so failed for want of the host's key rather than for
# anything about the check.
git_q() {
    git -C "$1" -c user.email=t@example.invalid -c user.name=t \
        -c commit.gpgSign=false -c tag.gpgSign=false \
        -c tag.forceSignAnnotated=false "${@:2}"
}

# check NAME WANT_EXIT REPO_DIR REF GREP_PATTERN
check() {
    local name="$1" want_exit="$2" dir="$3" ref="$4" want_grep="$5"
    n=$((n + 1))
    env DISPATCH_GIT_DIR="$dir" bash "$CHECK" "$ref" > "$TMP/out" 2>&1
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

# --- the fixture: our own history, plus a fork's PR head --------------
#
# `main` and `v1.0.0` are ours. `outside` is built on top of main and
# then unreferenced, leaving its commit in the object store reachable
# from nothing — which is precisely the state of a fork PR head in the
# base repository. It is then published under refs/pull/7/head, as
# GitHub does.
D="$TMP/repo"
mkdir -p "$D"
git -C "$D" init -q -b main
git_q "$D" commit -q --allow-empty -m "base"
git_q "$D" tag v1.0.0
git_q "$D" commit -q --allow-empty -m "second"
MAIN_SHA="$(git_q "$D" rev-parse --verify HEAD)"
TAG_SHA="$(git_q "$D" rev-parse --verify "v1.0.0^{commit}")"

git_q "$D" checkout -q -b outside
git_q "$D" commit -q --allow-empty -m "an outside contributor's commit"
FORK_SHA="$(git_q "$D" rev-parse --verify HEAD)"
git_q "$D" checkout -q main
git_q "$D" branch -q -D outside
git_q "$D" update-ref refs/pull/7/head "$FORK_SHA"
# A remote-tracking branch, which is all a fresh CI clone has for any
# branch other than the one checked out.
git_q "$D" update-ref "refs/remotes/origin/dev" "$MAIN_SHA"

# --- accepted: our own history ----------------------------------------
check "a blank ref is the default branch and passes" 0 "$D" "" "blank"
check "a branch of ours passes"        0 "$D" main       "reachable from"
check "a tag of ours passes"           0 "$D" v1.0.0     "reachable from"
check "a raw SHA on a branch passes"   0 "$D" "$MAIN_SHA" "reachable from"
check "a SHA reachable only from a tag passes" 0 "$D" "$TAG_SHA" "reachable from"
check "a remote-tracking branch name passes" 0 "$D" dev  "reachable from"

# --- rejected: a fork's pull-request head -----------------------------
check "refs/pull/<N>/head is rejected by shape" 1 "$D" \
    "refs/pull/7/head" "is a pull-request ref"
check "a bare pull/<N>/merge is rejected by shape" 1 "$D" \
    "pull/7/merge" "is a pull-request ref"

# THE ONE THAT MATTERS. Same commit, named by hash: no pattern can see
# it, and checkout would fetch it happily.
check "the raw SHA of a fork PR head is rejected" 1 "$D" "$FORK_SHA" \
    "not reachable from any branch or tag"

# --- orthogonality: the shape check alone would have missed it --------
# The old guard, run against the new broken input (#593 is the second
# time a rule was correct and a new route walked around it).
n=$((n + 1))
if printf '%s' "$FORK_SHA" | grep -E '(^|/)pull/[0-9]+/' >/dev/null; then
    echo "FAIL: the fork-head SHA matches the pull-ref pattern, so the two" \
         "assertions are not independent and this fixture proves nothing"
    failures=$((failures + 1))
else
    echo "PASS: the fork-head SHA does not match the pull-ref pattern (the shape" \
         "check alone would have let it through)"
fi

# --- rejected: refs that are not ours at all --------------------------
check "a ref that does not resolve is rejected" 1 "$D" no-such-branch \
    "does not resolve"

# --- blindness guards: must exit 2, never 0 ---------------------------
SHALLOW="$TMP/shallow"
git clone -q --depth 1 "file://$D" "$SHALLOW" 2>/dev/null
check "a shallow clone refuses to answer" 2 "$SHALLOW" main "Shallow clone"

mkdir -p "$TMP/notarepo"
check "a directory that is not a repo exits 2" 2 "$TMP/notarepo" main "Not a repository"
check "a directory that does not exist exits 2" 2 "$TMP/nope" main "Not a repository"

# A shallow clone must not be talked past by a blank ref either: the
# blank case is checked BEFORE resolution, so this pins the order.
check "a shallow clone exits 2 even for a blank ref" 2 "$SHALLOW" "" "Shallow clone"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n check(s) FAILED"
    exit 1
fi
echo "all $n check(s) passed"
