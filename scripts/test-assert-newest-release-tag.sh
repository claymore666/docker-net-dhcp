#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for assert-newest-release-tag.sh (#736).
#
# The cases below are built as real git repositories with real tags,
# not as a mocked tag list, because the thing that actually goes wrong
# here is version ordering: git's own `--sort=-v:refname` ranks
# `v1.8.0-rc1` above `v1.8.0` unless `versionsort.suffix` is set. A test
# that handed the script a pre-sorted array would agree with any
# implementation and prove nothing about the property that matters —
# that the real v1.8.0 release is NOT refused because an rc for it
# exists.
#
# Three exits are distinguished on purpose. 1 is "refused, a newer tag
# of this class exists"; 2 is "could not tell". Collapsing them would
# let a shallow checkout — which sees no tags at all — read as a
# refusal, or worse, as a pass.
set -u

SCRIPT="$(cd "$(dirname "$0")" && pwd)/assert-newest-release-tag.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The fixtures must not inherit whoever runs this. A developer with
# `tag.gpgsign = true` in ~/.gitconfig gets `git tag` demanding a
# message, every fixture repo silently ends up with zero tags, and the
# suite reports the shallow-checkout error for cases that have nothing
# to do with it — measured on a maintainer machine. A test whose result
# depends on the host config is testing the host.
#
# Both mechanisms are here on purpose. The per-invocation `-c` flags
# below are what scripts/check-selftest-fixtures.sh requires and what a
# reader sees at the call site; the two variables cover what a flag
# cannot, because a global setting that changes what `git tag` MEANS
# (tag.forceSignAnnotated, an alias, a template) is not undone by
# setting the key it happens to name.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

# Applied to every fixture invocation. A signing key here does not fail
# the suite, it HANGS it: the touch prompt never comes on a runner.
FIXTURE_CFG=(-c commit.gpgsign=false -c tag.gpgsign=false
             -c user.email=t@t -c user.name=t)

failures=0
n=0

# mkrepo TAG... — a fresh repo carrying exactly these tags.
#
# The directory comes from mktemp, not from a counter. A counter
# incremented in here would be incremented in a SUBSHELL — mkrepo is
# always called inside a command substitution — so every case would
# reuse one directory, accumulate every previous case's tags, and assert
# against a fixture nobody wrote. Found exactly that way.
mkrepo() {
    local d t
    d="$(mktemp -d "$TMP/repoXXXXXX")"
    git -C "$d" init -q
    git -C "$d" "${FIXTURE_CFG[@]}" commit -q --allow-empty -m x
    for t in "$@"; do
        git -C "$d" "${FIXTURE_CFG[@]}" tag "$t"
    done
    echo "$d"
}

# expect DESC WANT_EXIT TAG REPO_TAG...
expect() {
    local desc="$1" want="$2" tag="$3"
    shift 3
    n=$((n + 1))
    local d rc
    d="$(mkrepo "$@")"
    ( cd "$d" && bash "$SCRIPT" "$tag" ) >"$TMP/out" 2>"$TMP/err"
    rc=$?
    if [ "$rc" -eq "$want" ]; then
        echo "PASS: $desc (exit $rc)"
    else
        echo "FAIL: $desc — want exit $want, got $rc"
        sed 's/^/    /' "$TMP/out" "$TMP/err"
        failures=$((failures + 1))
    fi
}

# --- the release that is happening now --------------------------------
expect "the newest release tag promotes" 0 \
    v1.8.0 v1.7.0 v1.7.1 v1.8.0
expect "the first release ever promotes" 0 \
    v1.0.0 v1.0.0

# THE CASE THE CLASS SPLIT EXISTS FOR. The default version sort puts
# v1.8.0-rc2 above v1.8.0, so a single "newest tag overall" rule would
# refuse the real release on tag day — the worst possible moment to
# discover it.
expect "a real release is NOT refused because its own rcs exist" 0 \
    v1.8.0 v1.7.1 v1.8.0-rc1 v1.8.0-rc2 v1.8.0

# Nor because an rc for the NEXT version was already cut.
expect "a real release is NOT refused because a later rc exists" 0 \
    v1.8.0 v1.7.1 v1.8.0 v1.9.0-rc1

# Ordering is numeric, not lexicographic.
expect "v1.10.0 is newer than v1.9.0" 0 \
    v1.10.0 v1.9.0 v1.10.0
expect "v1.9.0 is refused once v1.10.0 exists" 1 \
    v1.9.0 v1.9.0 v1.10.0

# --- the operation this exists to stop --------------------------------
# `gh workflow run release.yml -f tag=v1.6.0` after v1.8.0 shipped —
# the dispatch the runbook offers as its recovery step.
expect "re-dispatching an old release is REFUSED" 1 \
    v1.6.0 v1.6.0 v1.7.0 v1.7.1 v1.8.0

# A backport published after a newer minor. Refusing is the intended
# behaviour, not a limitation: the backport ships under its own tag and
# the floating tag stays where it is.
expect "a backport is REFUSED" 1 \
    v1.7.2 v1.7.0 v1.7.1 v1.7.2 v1.8.0

# --- pre-releases ------------------------------------------------------
expect "the newest rc of its base promotes" 0 \
    v1.8.0-rc2 v1.7.1 v1.8.0-rc1 v1.8.0-rc2
expect "an older rc of the same base is REFUSED" 1 \
    v1.8.0-rc1 v1.7.1 v1.8.0-rc1 v1.8.0-rc2
expect "rc10 outranks rc9" 0 \
    v1.8.0-rc10 v1.8.0-rc9 v1.8.0-rc10
expect "rc9 is refused once rc10 exists" 1 \
    v1.8.0-rc9 v1.8.0-rc9 v1.8.0-rc10

# An rc is compared only against its OWN base. A newer base's rcs are a
# different class and must not refuse it; that is what lets an rc for
# v1.8.0 run while v1.9.0-rc1 exists.
expect "an rc is not refused by another base's rc" 0 \
    v1.8.0-rc1 v1.8.0-rc1 v1.9.0-rc1
expect "an rc is not refused by a newer release tag" 0 \
    v1.8.0-rc1 v1.8.0-rc1 v1.9.0

# Documented as NOT covered: re-dispatching an rc whose release already
# shipped. It is the newest rc of its own base, so it passes. Pinned
# here so that changing it later is a deliberate act rather than a
# silent drift away from what the header promises.
expect "an rc of an already-released version passes (documented gap)" 0 \
    v1.8.0-rc1 v1.8.0-rc1 v1.8.0

# --- cannot tell (exit 2), never a pass -------------------------------
# The shallow-checkout case: the one that must not read as "nothing
# newer exists".
n=$((n + 1))
d="$(mkrepo)"
( cd "$d" && bash "$SCRIPT" v1.8.0 ) >"$TMP/out" 2>"$TMP/err"
rc=$?
if [ "$rc" -eq 2 ]; then
    echo "PASS: a repo with no tags is exit 2, not a pass"
else
    echo "FAIL: no tags should be exit 2 (cannot tell), got $rc"
    sed 's/^/    /' "$TMP/out" "$TMP/err"
    failures=$((failures + 1))
fi

expect "a tag absent from the checkout is exit 2" 2 \
    v1.8.0 v1.7.0 v1.7.1
expect "no rc of this base at all is exit 2" 2 \
    v1.8.0-rc1 v1.7.1 v1.8.0

# Shapes resolve-dispatch-ref.sh already refuses. Reaching here at all
# means something upstream changed, so it is "cannot tell", not a pass.
expect "a branch name is exit 2" 2 \
    dev v1.8.0
expect "an unanchored near-miss is exit 2" 2 \
    v1.8.0junk v1.8.0
expect "a beta suffix is exit 2" 2 \
    v1.8.0-beta1 v1.8.0-beta1 v1.8.0

n=$((n + 1))
d="$(mkrepo v1.8.0)"
( cd "$d" && bash "$SCRIPT" "" ) >/dev/null 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then
    echo "PASS: an empty tag is exit 2"
else
    echo "FAIL: an empty tag should be exit 2, got $rc"
    failures=$((failures + 1))
fi

n=$((n + 1))
d="$(mkrepo v1.8.0)"
( cd "$d" && bash "$SCRIPT" ) >/dev/null 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then
    echo "PASS: no argument is exit 2"
else
    echo "FAIL: no argument should be exit 2, got $rc"
    failures=$((failures + 1))
fi

n=$((n + 1))
d="$(mkrepo v1.8.0)"
( cd "$d" && bash "$SCRIPT" v1.8.0 extra ) >/dev/null 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then
    echo "PASS: two arguments is exit 2"
else
    echo "FAIL: two arguments should be exit 2, got $rc"
    failures=$((failures + 1))
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
