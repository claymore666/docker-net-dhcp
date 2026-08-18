#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Reject a workflow_dispatch `ref` input that is not this repository's
# own history (#593).
#
# THE FAILURE THIS PREVENTS. `integration.yml` takes a free-text `ref`
# input and hands it to actions/checkout. A fork's pull-request head is
# fetchable from the BASE repository — no `repository:` argument needed:
#
#     git ls-remote https://github.com/<owner>/<repo> 'refs/pull/*'
#     <sha>	refs/pull/<N>/head
#
# so `-f ref=refs/pull/<N>/head` checks outside code out into a
# workflow_dispatch context, where repository secrets ARE present
# (unlike a fork's `pull_request`), and runs it as root on the
# self-hosted pool with the registry credential in the job (#562).
# The same input also let the gate job execute an attacker-chosen
# script with the default branch's Actions cache scope — the open
# CodeQL `actions/cache-poisoning/poisonable-step` alert.
#
# WHY THIS IS NOT "JUST DON'T DO THAT". integration.yml already carried
# a SECURITY comment forbidding exactly this outcome:
#
#     "checking out a PR head in a job that has the credential ...
#      must not be done here"
#
# The rule was written down correctly and a second route walked around
# it, because the note anticipated `pull_request_target` and not
# `workflow_dispatch`. Prose decays silently; a check fails loudly.
#
# WHAT IT ASSERTS. Two things, in this order:
#
#   1. The ref is not a pull-request ref by shape. A `refs/pull/<N>/*`
#      ref is never a legitimate input — the documented purpose of
#      `inputs.ref` (#419) is re-running one of OUR branches, tags or
#      SHAs against a tree that has not changed. Checked first purely
#      so the error names the reason instead of "does not resolve".
#
#   2. The commit it names is reachable from a branch or tag of this
#      repository. This is the assertion that actually holds, and it is
#      deliberately not a denylist: a raw SHA walks straight past any
#      `refs/pull/*` pattern, because a fork PR head's commit lives in
#      the base repository's object store and checkout will happily
#      fetch it by hash. Reachability is the honest predicate — an
#      outside contributor's commit is by construction not reachable
#      from any of our branches, and the moment it is merged it becomes
#      reachable and legitimate on its own.
#
# WHAT IT DOES NOT CLAIM. It says nothing about whether the ref is a
# GOOD thing to test, only whether it is ours. And it is worth exactly
# its wiring: a job that consumes `inputs.ref` without depending on a
# job that runs this script is unprotected, which is why
# check-dispatch-ref-guard.sh exists as a separate, static check.
#
# Usage: bash scripts/check-dispatch-ref.sh "<ref>"
# Env:   DISPATCH_GIT_DIR  repository to answer in (default: this repo)
#        DISPATCH_REMOTE   remote-tracking namespace (default: origin)
# Exit:  0 accepted (or empty — the default branch)
#        1 rejected
#        2 the check could not run (not a repository, shallow clone)

set -uo pipefail

REF="${1-}"

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
GIT_DIR_ARG="${DISPATCH_GIT_DIR:-$ROOT}"
REMOTE="${DISPATCH_REMOTE:-origin}"

git_in() { git -C "$GIT_DIR_ARG" "$@"; }

if ! git_in rev-parse --git-dir >/dev/null 2>&1; then
    echo "::error title=Not a repository::$GIT_DIR_ARG is not a git repository," \
         "so no reachability question can be answered here. Refusing to report a" \
         "verdict this check cannot support." >&2
    exit 2
fi

# The same blindness guard the back-merge gate carries (#598), for the
# same reason: `--contains` on a truncated graph answers "not reachable"
# for commits that are perfectly reachable, which would reject every
# legitimate dispatch. The workflow must check out with fetch-depth 0.
if [ "$(git_in rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
    echo "::error title=Shallow clone::reachability cannot be computed from a" \
         "shallow clone — this check needs fetch-depth 0. Refusing to report a" \
         "verdict it cannot support." >&2
    exit 2
fi

# Empty is the documented default: "Blank = the default branch". Every
# event other than workflow_dispatch also arrives here empty, which is
# what lets the guard job run unconditionally instead of being skipped
# — a skipped guard would skip everything that depends on it.
if [ -z "$REF" ]; then
    echo "dispatch ref: (blank) — the workflow's own ref, nothing to check."
    exit 0
fi

# 1. Pull-request refs, by shape. `refs/pull/<N>/head`, the bare
#    `pull/<N>/merge`, and anything else carrying that path segment.
if printf '%s' "$REF" | grep -Eq '(^|/)pull/[0-9]+/'; then
    echo "::error title=Pull-request ref rejected::'$REF' is a pull-request ref." \
         "A fork's PR head is fetchable from this repository, so dispatching it" \
         "would check outside code out into a context that HAS the repository" \
         "secrets and run it as root on the self-hosted pool (#593). The 'ref'" \
         "input exists to re-run one of our own branches, tags or SHAs (#419)." >&2
    exit 1
fi

# 2. Resolve to a commit. rev-parse does not DWIM remote-tracking
#    branches the way checkout does, so the candidates are tried
#    explicitly — otherwise dispatching `dev` from a fresh clone, where
#    only refs/remotes/origin/dev exists, would be rejected as
#    unresolvable.
sha=""
for cand in "refs/heads/$REF" "refs/tags/$REF" "refs/remotes/$REMOTE/$REF" "$REF"; do
    if sha="$(git_in rev-parse --verify --quiet "${cand}^{commit}" 2>/dev/null)" \
       && [ -n "$sha" ]; then
        break
    fi
    sha=""
done

if [ -z "$sha" ]; then
    echo "::error title=Dispatch ref does not resolve::'$REF' does not name a" \
         "commit in this repository. Dispatch a branch, tag or SHA that exists" \
         "here (#593)." >&2
    exit 1
fi

# 3. Reachability — the assertion that holds. A ref that resolves is not
#    enough: `refs/pull/<N>/head` resolves, and so does its raw SHA.
containing="$(git_in for-each-ref --contains "$sha" --count=1 \
                  --format='%(refname)' \
                  refs/heads refs/tags "refs/remotes/$REMOTE" 2>/dev/null)"

if [ -z "$containing" ]; then
    echo "::error title=Dispatch ref is not ours::'$REF' ($(printf '%.12s' "$sha"))" \
         "is not reachable from any branch or tag of this repository. That is" \
         "exactly the shape of a fork's pull-request head, which lives in this" \
         "repository's object store and can be checked out by hash — into a" \
         "context that has the secrets, as root, on the self-hosted pool (#593)." \
         "If this commit is legitimate, push it to a branch here first." >&2
    exit 1
fi

echo "dispatch ref: '$REF' -> $(printf '%.12s' "$sha"), reachable from $containing."
exit 0
