#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every root-anchored directory .gitignore excludes must also be excluded
# by .dockerignore.
#
# The defect this guards: .dockerignore listed /plugin/ but not
# /plugin-cover/. Both are build output written by `sudo make create...`,
# so both end up root-owned; once one is in the build context, every
# unprivileged `docker build` from the repo root dies in the context walk
# with "permission denied" — in a place unrelated to whatever the
# developer changed. /logs/ (teed by the root-only integration targets)
# was missing for the same reason.
#
# check-build-context.sh already asserts the property for .claude/, but
# it can only see the defect on a tree that HAPPENS to have root-owned
# residue sitting there. On a clean checkout — every CI checkout — it
# passes no matter what .dockerignore says. That is a guard that fires by
# luck. This one is deterministic: it compares the two files and needs no
# residue to exist.
#
# The rule is deliberately narrow. A root-anchored ignored DIRECTORY is
# build output living at the context root; nothing in the image is ever
# built from it, and shipping it is at best wasted context and at worst a
# failed build. Non-directory ignores (*.key, CLAUDE.md) are out of scope
# — .dockerignore covers those its own way.
#
# Matching is on the superset, then judged: every /name/ line is
# collected first and each is required to be either excluded or listed in
# ALLOWED_ABSENT with a reason. A gate that only matched the entries it
# already knew about would go quiet exactly when a new one appeared,
# which is the #487 shape.
#
# Usage:
#   scripts/check-dockerignore-parity.sh [.gitignore] [.dockerignore]
# Exit: 0 pass, 1 drift, 2 usage error.

set -uo pipefail

GITIGNORE="${1:-.gitignore}"
DOCKERIGNORE="${2:-.dockerignore}"

for f in "$GITIGNORE" "$DOCKERIGNORE"; do
    if [ ! -r "$f" ]; then
        echo "cannot read $f" >&2
        exit 2
    fi
done

# Directories that are deliberately NOT in .dockerignore, each with the
# reason. Empty today: every known root-anchored build-output directory
# belongs out of the context. Add an entry rather than deleting a check.
declare -A ALLOWED_ABSENT=()

# Collect root-anchored directory entries: /name/ or /name/* , skipping
# negations (!/bin/.gitkeep) and comments.
mapfile -t dirs < <(
    sed -e 's/#.*//' -e 's/[[:space:]]*$//' "$GITIGNORE" |
        grep -E '^/[^/]+/' |
        sed -E 's#^(/[^/]+/).*#\1#' |
        sort -u
)

if [ "${#dirs[@]}" -eq 0 ]; then
    echo "::error title=No directories matched::$GITIGNORE yielded no root-anchored" \
         "directory entries. This gate would otherwise pass having compared nothing." >&2
    exit 2
fi

# excluded NAME -> 0 if .dockerignore excludes it in any accepted spelling.
excluded() {
    local bare="${1#/}"; bare="${bare%/}"
    grep -qE "^/?${bare}/?[[:space:]]*$" "$DOCKERIGNORE"
}

fails=0
for d in "${dirs[@]}"; do
    if excluded "$d"; then
        echo "ok    $d excluded from the build context"
    elif [ -n "${ALLOWED_ABSENT[$d]:-}" ]; then
        echo "ok    $d deliberately in context — ${ALLOWED_ABSENT[$d]}"
    else
        echo "FAIL  $d is ignored by $GITIGNORE but not by $DOCKERIGNORE."
        echo "      It is build output at the context root. If it is ever written by a"
        echo "      root-only target, an unprivileged \`docker build\` from the repo root"
        echo "      will die walking it. Add it to $DOCKERIGNORE, or to ALLOWED_ABSENT"
        echo "      in this script with the reason."
        fails=1
    fi
done

if [ "$fails" -ne 0 ]; then
    exit 1
fi
echo "OK: every root-anchored ignored directory is out of the build context."
