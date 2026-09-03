#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The in-tree copy of the DHCP library (D21) is what the plugin is built
# from, and until now nothing checked it.
#
# WHY THIS EXISTS. Five gates were taken off internal/dhcp-golib/ on the
# ground that the directory is not this repository's source: editing it
# falsifies internal/dhcp-golib/SOURCE and the next sync reverts the
# edit. That argument has a hole in the middle of it — NOTHING COMPARED
# THE COPY TO ANYTHING. A hand-edit, a partial sync, or a sync run
# against the wrong tree shipped in the built plugin, past five gates
# that had been told not to look.
#
# WHAT IT CHECKS, AND WHAT IT CANNOT.
#
#   INTEGRITY (always, everywhere). internal/dhcp-golib/MANIFEST records
#   one line per tracked file: git's mode, the sha256 of the bytes, and
#   the path. This re-derives all three from the tree and fails on any
#   difference — content, exec bit, a file added, a file removed. The
#   manifest is written by this same script under --write, and
#   sync-dhcp-golib.sh calls it, so the two can never disagree about the
#   format.
#
#   PROVENANCE (only where the library checkout is). Re-deriving the
#   tree from the SHA in SOURCE needs the private repository, and this
#   branch holds no credential for it: CI cannot do it and says so
#   rather than passing quietly. Give a checkout in $1 or DHCP_GOLIB_SRC
#   and the second half runs too.
#
# A gate that could only ever report "I could not check" would be worse
# than none, which is why integrity is the half that always runs and why
# an absent or empty manifest is a REFUSAL rather than a pass.
#
# Usage: scripts/check-dhcp-golib-copy.sh [--write] [source-repo]
# Env:   DEST_DIR        the copy (default internal/dhcp-golib)
#        DHCP_GOLIB_SRC  library checkout, for the provenance half
# Exit:  0 clean, 1 a difference, 2 the check could not be made.

set -euo pipefail

DEST="${DEST_DIR:-internal/dhcp-golib}"
MANIFEST_NAME="MANIFEST"
SOURCE_NAME="SOURCE"

WRITE=0
if [ "${1:-}" = "--write" ]; then
    WRITE=1
    shift
fi
SRC="${1:-${DHCP_GOLIB_SRC:-}}"

die() { echo "::error title=$1::$2" >&2; exit "${3:-1}"; }

[ -d "$DEST" ] || die "No library copy" \
    "$DEST does not exist. This gate's whole subject is that directory; with it gone there is nothing to compare and nothing to build from." 2

# The population is git's, not find's. A manifest over the working tree
# would count build scratch and ignored files, and the thing that ships
# is what is tracked.
tracked=$(git ls-files -s -- "$DEST" | LC_ALL=C awk -v m="$DEST/$MANIFEST_NAME" '$4 != m {print $1 "\t" $4}' | LC_ALL=C sort -t$'\t' -k2,2)
if [ -z "$tracked" ]; then
    die "Nothing tracked under the library copy" \
        "git ls-files reports no tracked files under $DEST. Either the copy is untracked -- in which case no gate in this repository can see it -- or the path is wrong. Refusing rather than reporting a clean tree." 2
fi

# Derive the manifest from the tree: git's mode (reproducible across
# checkouts, unlike the umask-dependent filesystem mode) and the sha256
# of the bytes on disk (so an unstaged edit is caught too).
derived=$(
    printf '%s\n' "$tracked" | while IFS=$'\t' read -r mode path; do
        [ -n "$path" ] || continue
        [ -f "$path" ] || die "Tracked file missing from the tree" "$path is in the index and not on disk." 1
        sum=$(sha256sum "$path" | LC_ALL=C awk '{print $1}')
        printf '%s %s %s\n' "$mode" "$sum" "${path#"$DEST"/}"
    done
)

if [ "$WRITE" -eq 1 ]; then
    printf '%s\n' "$derived" >"$DEST/$MANIFEST_NAME"
    echo "check-dhcp-golib-copy: wrote $DEST/$MANIFEST_NAME ($(printf '%s\n' "$derived" | LC_ALL=C wc -l) file(s))"
    exit 0
fi

[ -r "$DEST/$MANIFEST_NAME" ] || die "No manifest" \
    "$DEST/$MANIFEST_NAME is missing. Without it this gate has nothing to compare the copy against, and the five gates that were switched off over this directory have nothing standing in for them. Regenerate it with: scripts/check-dhcp-golib-copy.sh --write" 2

recorded=$(cat "$DEST/$MANIFEST_NAME")
if [ -z "$recorded" ]; then
    die "Empty manifest" \
        "$DEST/$MANIFEST_NAME is empty, so every comparison below would succeed having compared nothing." 2
fi

if [ "$recorded" != "$derived" ]; then
    echo "The library copy does not match its manifest:" >&2
    diff <(printf '%s\n' "$recorded") <(printf '%s\n' "$derived") >&2 || true
    die "Library copy differs from its manifest" \
        "$DEST no longer matches $DEST/$MANIFEST_NAME. A line only present on the right is a file added or changed; only on the left, a file removed. This directory is a verbatim copy of an external repository (D21) -- it is not edited here. Re-run scripts/sync-dhcp-golib.sh at the SHA in $DEST/$SOURCE_NAME."
fi

files=$(printf '%s\n' "$derived" | LC_ALL=C wc -l)

# An untracked file under the copy ships in a local build and is in no
# manifest, so the comparison above cannot see it.
untracked=$(git ls-files --others --exclude-standard -- "$DEST")
if [ -n "$untracked" ]; then
    echo "$untracked" >&2
    die "Untracked files under the library copy" \
        "The files above are inside $DEST, are not tracked, and are therefore in no manifest. They would be built into a local image while being invisible to every check here."
fi

[ -r "$DEST/$SOURCE_NAME" ] || die "No SOURCE pin" \
    "$DEST/$SOURCE_NAME is missing, so nothing records which commit this copy came from." 2
sha=$(LC_ALL=C tr -d ' \t\n' <"$DEST/$SOURCE_NAME")
case "$sha" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *) die "SOURCE is not a full commit SHA" "$DEST/$SOURCE_NAME reads '$sha'. A short or malformed SHA is not a pin." ;;
esac

if [ -z "$SRC" ]; then
    echo "check-dhcp-golib-copy: integrity OK — $files tracked file(s) match $DEST/$MANIFEST_NAME, pinned at $sha"
    echo "check-dhcp-golib-copy: provenance NOT CHECKED — no library checkout given (\$1 or DHCP_GOLIB_SRC). This branch holds no credential for the private repository, so CI cannot re-derive the tree; run this locally with a checkout to close the other half."
    exit 0
fi

git -C "$SRC" rev-parse --git-dir >/dev/null 2>&1 \
    || die "Source is not a git repository" "$SRC has no git directory." 2
git -C "$SRC" cat-file -e "${sha}^{commit}" 2>/dev/null \
    || die "Commit not found" "$SRC does not contain $sha, so provenance cannot be checked from it." 2

TMP="$(mktemp -d)"
# shellcheck disable=SC2064  # expand TMP now, not at trap time
trap "rm -rf '$TMP'" EXIT
mkdir -p "$TMP/tree"
git -C "$SRC" archive --format=tar "$sha" | tar -x -C "$TMP/tree"
rm -rf "$TMP/tree/.claude" "$TMP/tree/.git"

# The upstream side, in the manifest's own shape: git's mode for the
# blob at that commit, the sha256 of the extracted bytes, the path.
upstream=$(
    git -C "$SRC" ls-tree -r "$sha" | LC_ALL=C awk '{print $1 "\t" $4}' | LC_ALL=C sort -t$'\t' -k2,2 |
    while IFS=$'\t' read -r mode path; do
        [ -n "$path" ] || continue
        case "$path" in .claude/*|.git/*) continue ;; esac
        sum=$(sha256sum "$TMP/tree/$path" | LC_ALL=C awk '{print $1}')
        printf '%s %s %s\n' "$mode" "$sum" "$path"
    done
)

# SOURCE and MANIFEST are this repository's, written after the copy, and
# have no counterpart upstream.
mine=$(printf '%s\n' "$derived" | LC_ALL=C grep -v -e "^[0-9]* [0-9a-f]* $SOURCE_NAME\$" -e "^[0-9]* [0-9a-f]* $MANIFEST_NAME\$" || true)

if [ "$mine" != "$upstream" ]; then
    echo "The library copy is not what $sha produces:" >&2
    diff <(printf '%s\n' "$upstream") <(printf '%s\n' "$mine") >&2 || true
    die "Library copy differs from its recorded commit" \
        "$DEST does not match github.com/claymore666/dhcp-golib @ $sha. Re-run scripts/sync-dhcp-golib.sh $sha."
fi

echo "check-dhcp-golib-copy: integrity OK — $files tracked file(s) match $DEST/$MANIFEST_NAME"
echo "check-dhcp-golib-copy: provenance OK — the copy is byte-for-byte $sha ($(printf '%s\n' "$upstream" | LC_ALL=C wc -l) file(s) compared)"
