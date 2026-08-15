#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every page that prints a cosign command must also state which cosign
# major it needs (#522).
#
# Why this is a gate and not a note. A downstream consumer running the
# documented `cosign verify-blob` with cosign v2 gets
#
#     Error: bundle does not contain cert for verification, please provide public key
#
# The message names the bundle, so it reads as "this release was signed
# wrong" — not "your cosign is too old". The person running it is by
# definition deciding whether to trust the artifact, and we handed them an
# error that implicates the artifact when the fault is their toolchain.
# It also quietly undoes #264/#265: a reader who concludes the signature is
# broken ends up where they would have been if it actually were.
#
# The required major has one source of truth: the COSIGN_MAJOR assignment
# in scripts/check-release-tooling.sh, which is what the runbook preflight
# enforces on the maintainer's box. This gate makes the user-facing pages
# follow it, so the next installer bump cannot leave them behind — which is
# exactly how the mismatch arrived in the first place.
#
# Usage:
#   bash scripts/check-cosign-docs.sh
#   DOCS_ROOT=path TOOLING_SCRIPT=path bash scripts/check-cosign-docs.sh
set -u

ROOT="${DOCS_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
TOOLING="${TOOLING_SCRIPT:-$ROOT/scripts/check-release-tooling.sh}"

if [ ! -f "$TOOLING" ]; then
    echo "cannot read $TOOLING — no source of truth for the cosign major" >&2
    exit 2
fi

MAJOR="$(sed -n 's/^COSIGN_MAJOR=\([0-9][0-9]*\).*/\1/p' "$TOOLING" | head -1)"
if [ -z "$MAJOR" ]; then
    echo "no COSIGN_MAJOR=<n> assignment in $TOOLING." >&2
    echo "That line is this gate's source of truth; restore it rather than" >&2
    echo "hardcoding the major here, or the two will drift apart again." >&2
    exit 2
fi

# The phrase the pages must carry. Markdown emphasis and code ticks are
# stripped before matching, so "**cosign v3 or newer**" counts.
WANT="cosign v${MAJOR} or newer"

# Pages in scope: any tracked Markdown file that prints a cosign
# verification command. Discovered, not listed — a new page that copies the
# snippet is in scope the moment it lands, which a hand-maintained list
# would miss.
mapfile -t PAGES < <(
    cd "$ROOT" && grep -rl --include='*.md' -E '^[[:space:]]*cosign verify' . \
        | sed 's|^\./||' | sort
)

if [ "${#PAGES[@]}" -eq 0 ]; then
    echo "no page prints a cosign verify command — nothing to check." >&2
    echo "That is almost certainly a broken search, not a real state." >&2
    exit 2
fi

fail=0
for page in "${PAGES[@]}"; do
    if tr -d '*`' < "$ROOT/$page" | grep -qiF "$WANT"; then
        echo "PASS  $page"
    else
        echo "FAIL  $page prints a cosign command but never says \"$WANT\""
        fail=1
    fi
done

echo
if [ "$fail" -ne 0 ]; then
    cat <<EOF
A reader who runs the documented command with an older cosign gets an error
that blames the release, not their toolchain. State the requirement on every
page that shows the command.
EOF
    exit 1
fi
echo "All ${#PAGES[@]} page(s) printing a cosign command state \"$WANT\"."
