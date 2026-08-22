#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Refuse to promote a floating tag backwards (#736).
#
# WHY THIS EXISTS. `promote-latest` runs `crane tag <repo>:$TAG $LATEST`.
# Nothing in that operation knows what `:latest` already points at, so
#
#     gh workflow run release.yml -f tag=v1.6.0
#
# republishes v1.6.0 and moves `:latest` back to it, on both registries,
# with no rollback: `docker plugin create` re-tars the rootfs
# non-reproducibly, so the only way to undo it is to overwrite it with a
# new digest, orphaning the previous signature.
#
# That dispatch is not hypothetical. docs/release-runbook.md offers it as
# the RECOVERY procedure for a failed release, in the same file that
# forbids moving `:latest` backwards. So the one operation nothing stops
# is documented as the remedy, reached by someone already having a bad
# day. This script is the executable end of that postmortem.
#
# WHAT IT ASSERTS. The tag being released is the newest tag IN ITS OWN
# CLASS, where the class is:
#
#   vX.Y.Z        — compared against every other bare vX.Y.Z tag
#   vX.Y.Z-rcN    — compared against the other rcs of the SAME vX.Y.Z
#
# Splitting by class is not fussiness, it is the only way to sort these
# without a trap. `git tag --sort=-v:refname` puts `v1.8.0-rc1` ABOVE
# `v1.8.0` unless `versionsort.suffix` is configured — measured, git
# 2.47.3 — so a single "newest tag overall" rule would REFUSE the real
# v1.8.0 release the moment an rc for it existed. Within a class every
# candidate has the identical shape, so `sort -V` over the filtered list
# is unambiguous and this depends on no git version-sort setting at all.
#
# REFUSE, NOT SKIP. A skip during an incident is not noticed, and an
# incident is exactly when this fires. Two consequences follow, both
# intended:
#
#   - re-running the CURRENT release after a transient failure still
#     works, which is what the runbook's recovery lines actually want;
#   - a genuine backport (v1.7.2 published after v1.8.0 exists) is
#     REFUSED. Moving `:latest` to it is then a deliberate manual
#     `crane tag`. A backport must not become `:latest` by accident.
#
# WHAT IT DOES NOT COVER. Re-dispatching an rc of an ALREADY-RELEASED
# version — v1.7.0-rc1 after v1.7.0 shipped — is not refused: it is the
# newest rc of its own base. That moves `latest-rc`, which no documented
# install command names, so it is left uncovered deliberately rather
# than by oversight.
#
# It needs the tags to actually be present. A shallow checkout hands you
# an empty list, and an empty list is an ERROR here, never a pass — the
# caller must fetch tags (`fetch-depth: 0`).
#
# Usage: bash scripts/assert-newest-release-tag.sh <tag>
# Exit:  0 <tag> is the newest in its class
#        1 REFUSED — a newer tag of the same class exists
#        2 cannot tell (no tags fetched, tag absent, unknown shape)

set -uo pipefail

TAG="${1-}"

if [ "$#" -ne 1 ]; then
    echo "::error title=assert-newest-release-tag::exactly one argument is" \
         "required, got $#." >&2
    echo "usage: bash scripts/assert-newest-release-tag.sh <tag>" >&2
    exit 2
fi

if [ -z "$TAG" ]; then
    echo "::error title=assert-newest-release-tag::empty tag. An empty tag" \
         "cannot be checked, and must not be treated as 'nothing newer'." >&2
    exit 2
fi

if [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    PATTERN='^v[0-9]+\.[0-9]+\.[0-9]+$'
    DESCRIPTION="release tag"
elif [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-rc[0-9]+$ ]]; then
    BASE="${TAG%-rc*}"
    PATTERN="^${BASE//./\\.}-rc[0-9]+\$"
    DESCRIPTION="pre-release of ${BASE}"
else
    echo "::error title=Unknown tag shape::'${TAG}' is neither vX.Y.Z nor" \
         "vX.Y.Z-rcN, so there is no class to compare it against." >&2
    echo "resolve-dispatch-ref.sh should have refused it before this point." >&2
    exit 2
fi

ALL="$(git tag --list 2>/dev/null)"
if [ -z "$ALL" ]; then
    echo "::error title=No tags::this repository has no tags visible." >&2
    echo >&2
    echo "That is almost always a shallow checkout: actions/checkout fetches" >&2
    echo "one commit by default and no tags, so the comparison below would" >&2
    echo "have nothing to compare against. An empty list is an error here," >&2
    echo "never a pass — a check that cannot see the newer tag must not" >&2
    echo "report that none exists. Use 'fetch-depth: 0'." >&2
    exit 2
fi

CANDIDATES="$(printf '%s\n' "$ALL" | grep -E "$PATTERN" | sort -r -V)"

if [ -z "$CANDIDATES" ]; then
    echo "::error title=No comparable tags::no tag matching ${PATTERN} exists," \
         "so '${TAG}' cannot be confirmed as the newest ${DESCRIPTION}." >&2
    echo "The tag being released should itself be in that list." >&2
    exit 2
fi

# grep -Fx, not -Fxq: under `pipefail` a consumer that exits on its
# first match kills the producer with SIGPIPE, and the pipeline then
# reports failure on SUCCESS. Reading to EOF and discarding the output
# gives the real status.
if ! printf '%s\n' "$CANDIDATES" | grep -Fx "$TAG" >/dev/null; then
    echo "::error title=Tag not found::'${TAG}' is not among the tags this" \
         "checkout can see, so nothing here can rank it." >&2
    echo "Tags of its class that ARE visible:" >&2
    printf '%s\n' "$CANDIDATES" | sed 's/^/  /' >&2
    exit 2
fi

NEWEST="$(printf '%s\n' "$CANDIDATES" | head -1)"

if [ "$NEWEST" = "$TAG" ]; then
    echo "OK: ${TAG} is the newest ${DESCRIPTION}; promoting the floating tag" \
         "moves it forward."
    exit 0
fi

echo "::error title=Refusing to move the floating tag backwards::'${TAG}' is" \
     "not the newest ${DESCRIPTION} — '${NEWEST}' is." >&2
echo >&2
echo "Promoting it would point the floating tag at an older release, on both" >&2
echo "registries, and this workflow cannot roll that back: rebuilding the" >&2
echo "plugin re-tars the rootfs non-reproducibly, so the only correction is" >&2
echo "another overwrite with a new digest that orphans the old signature." >&2
echo >&2
echo "If you are RE-RUNNING a failed release, dispatch the newest tag" >&2
echo "(${NEWEST}) — re-running the current release is allowed and is what" >&2
echo "the runbook's recovery step is reaching for." >&2
echo >&2
echo "If you are deliberately publishing a BACKPORT, that is correct and this" >&2
echo "refusal is doing its job: the backport is published by its own tag, and" >&2
echo "the floating tag stays on ${NEWEST}. Moving it is a deliberate manual" >&2
echo "'crane tag', never a side effect of a release run." >&2
exit 1
