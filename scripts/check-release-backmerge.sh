#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Assert every commit on `main` is reachable from `dev` (#598) — that
# the post-release back-merge in release-runbook step 11 actually ran.
#
# Why this is a gate and not a comment:
#
# The runbook records step 11 being forgotten outright once, after
# v0.9.0, and points at release.yml's header comment, which carries the
# same checklist. Two prose reminders, in two files, for a step that
# nothing verifies.
#
# Nothing can observe the omission from inside a run. It happens AFTER
# the last CI-visible event of a release: the release PR merges green,
# the tag builds green, and then a step that no run watches does not
# happen. There is no run to fail. Same shape as #418 — nothing executes
# to notice that nothing executed — so, same answer: a schedule.
#
# The window is also narrower than it looks. After v1.6.0 the
# fast-forward was still pending, three hours old and entirely normal,
# when three dependency PRs were merged into `dev`. That foreclosed it:
# once `dev` carries commits of its own the prescribed `--ff-only` can
# no longer run, and it takes a back-merge PR instead (#597). Nothing
# warned, because nothing was looking.
#
# What makes it survive is that the divergence carries NO CONTENT. Every
# one of those merge commits has a `dev` commit as its second parent, so
# `git diff main dev` is clean, every file reads correct, and every test
# passes. Only the graph is wrong, and nobody diffs a graph.
#
# DIRECTION. This guard fails in one direction only: `main` ahead of
# `dev`. The opposite — `dev` ahead of `main` — is the normal state of
# the branch model and is deliberately unchecked; there is nothing to
# cover there.
#
# GRACE, so a release in flight is not a false red. Between a release PR
# merging and the back-merge there is a legitimate window, and an rc
# chain reopens it once per rc (v1.6.0 took four). The check stays green
# while the OLDEST main-only commit is younger than the grace window. A
# slow release that trips it anyway is not an alarm worth suppressing:
# the remedy for the red — back-merge now — is what the runbook wants
# after every release-PR merge, not only the last one.
#
# Usage: bash scripts/check-release-backmerge.sh
# Env:   BACKMERGE_GIT_DIR      repo to inspect (default: this checkout)
#        BACKMERGE_BASE         ref that must be contained (default origin/main)
#        BACKMERGE_HEAD         ref that must contain it  (default origin/dev)
#        BACKMERGE_GRACE_HOURS  window before divergence is an error (default 24)
#        BACKMERGE_NOW          epoch seconds, overrides "now" for tests
# Exit:  0 contained, or within grace
#        1 main-only commits older than the grace window
#        2 cannot see — refs missing, shallow clone, not a repo

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
DIR="${BACKMERGE_GIT_DIR:-$(cd "$HERE/.." && pwd)}"
BASE="${BACKMERGE_BASE:-origin/main}"
HEAD_REF="${BACKMERGE_HEAD:-origin/dev}"
GRACE_HOURS="${BACKMERGE_GRACE_HOURS:-24}"

if ! [[ "$GRACE_HOURS" =~ ^[0-9]+$ ]]; then
    echo "::error title=Bad grace window::BACKMERGE_GRACE_HOURS must be a whole" \
         "number of hours, got '$GRACE_HOURS'." >&2
    exit 2
fi

cd "$DIR" 2>/dev/null || {
    echo "::error title=Cannot see the repository::$DIR is not a directory." >&2
    exit 2
}

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "::error title=Cannot see the repository::$DIR is not a git repository." >&2
    exit 2
fi

# A shallow clone answers rev-list confidently and WRONGLY: commits
# beyond the graft look absent, so a healthy repo reports divergence and
# a diverged one can report none. actions/checkout defaults to depth 1,
# which makes this the single likeliest way for this gate to start
# lying. Refuse to answer instead.
if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
    echo "::error title=Shallow clone::reachability cannot be computed from a" \
         "shallow clone — this gate needs fetch-depth 0. Refusing to report a" \
         "verdict it cannot support." >&2
    exit 2
fi

for ref in "$BASE" "$HEAD_REF"; do
    if ! git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
        echo "::error title=Ref not found::'$ref' does not resolve in $DIR." \
             "A renamed or unfetched branch must go red here, not quietly green." >&2
        exit 2
    fi
done

# Commits reachable from BASE but not from HEAD_REF. Oldest first.
mapfile -t only < <(git rev-list --reverse "${HEAD_REF}..${BASE}")

if [ "${#only[@]}" -eq 0 ]; then
    echo "check-release-backmerge: ${BASE} is fully contained in ${HEAD_REF}" \
         "($(git rev-parse --short "$BASE"))."
    exit 0
fi

# Age of the OLDEST straggler, not the newest: the question is how long
# the branch has been left disconnected, and a fresh commit on top of an
# old omission must not reset the clock.
now="${BACKMERGE_NOW:-$(date +%s)}"
oldest_ct=""
for c in "${only[@]}"; do
    ct="$(git show -s --format=%ct "$c")"
    if [ -z "$oldest_ct" ] || [ "$ct" -lt "$oldest_ct" ]; then
        oldest_ct="$ct"
    fi
done

age_h=$(( (now - oldest_ct) / 3600 ))

echo "${#only[@]} commit(s) on ${BASE} are not reachable from ${HEAD_REF}:"
for c in "${only[@]}"; do
    echo "  $(git show -s --format='%h %ad %s' --date=short "$c")"
done

if [ "$age_h" -lt "$GRACE_HOURS" ]; then
    echo "::notice title=Back-merge pending::oldest is ${age_h}h old, within the" \
         "${GRACE_HOURS}h grace window. A release is presumably still in flight."
    exit 0
fi

echo >&2
echo "::error title=Post-release back-merge was skipped::the oldest commit on" \
     "${BASE} that ${HEAD_REF} does not contain is ${age_h}h old, past the" \
     "${GRACE_HOURS}h grace window. Release-runbook step 11 did not run." >&2
echo >&2
echo "Fix, while it is still a fast-forward:" >&2
echo "    git checkout dev && git merge --ff-only main && git push origin dev" >&2
echo >&2
echo "If ${HEAD_REF} has already moved on, the fast-forward is gone and it" \
     "takes a back-merge PR instead (see #597)." >&2
exit 1
