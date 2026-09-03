#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Copy the DHCP library's tracked tree, at one pinned commit, into
# internal/dhcp-golib/ (D21).
#
# The library is a separate, private repository. This branch is public and
# has no credential for it, so the source travels as a directory rather than
# as a module dependency, with a `replace` directive in go.mod pointing at
# it. The directory is dropped at publication (M9), when the module is
# published under its own path.
#
# THE SWEEP IS THE REASON THIS IS A SCRIPT AND NOT A `cp -r`. Copying a
# private tree onto a public branch publishes whatever is in it. The deny
# list is itself private — a public script that spelled the names out would
# publish exactly what it exists to keep out — so it is read from a file
# outside the repository's tracked tree, and its ABSENCE REFUSES THE COPY.
# Fail-closed, because the alternative ("no list, nothing to check, carry
# on") is a gate that reports success having checked nothing.
#
# Usage: scripts/sync-dhcp-golib.sh <40-hex-sha> [source-repo]
# Env:   DHCP_GOLIB_SRC   source repository (used when no second argument)
#        DHCP_GOLIB_DENY  deny-list file (default .claude/internal/dhcp-golib-deny.txt),
#                         one case-insensitive fixed string per line, blank
#                         lines and #-comments ignored
#        DEST_DIR         destination (default internal/dhcp-golib) — the
#                         seam the self-test drives (scripts/test-sync-dhcp-golib.sh)
# Exit:  0 copied, 1 refused, 2 usage or environment error.

set -euo pipefail

DEST="${DEST_DIR:-internal/dhcp-golib}"
DENY="${DHCP_GOLIB_DENY:-.claude/internal/dhcp-golib-deny.txt}"

die() { echo "::error title=$1::$2" >&2; exit "${3:-1}"; }

SHA="${1:-}"
SRC="${2:-${DHCP_GOLIB_SRC:-}}"

case "$SHA" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *) die "Usage" "sync-dhcp-golib.sh needs a full 40-character lowercase commit SHA; got '${SHA}'. A short SHA is not a pin: it can become ambiguous as the repository grows." 2 ;;
esac

[ -n "$SRC" ] || die "No source repository" \
    "Pass the library checkout as the second argument or set DHCP_GOLIB_SRC. This branch holds no credential for the private repository, so the copy is made locally and committed (D21)." 2

git -C "$SRC" rev-parse --git-dir >/dev/null 2>&1 \
    || die "Source is not a git repository" "$SRC has no git directory." 2

git -C "$SRC" cat-file -e "${SHA}^{commit}" 2>/dev/null \
    || die "Commit not found" "$SRC does not contain commit $SHA. Fetch it before syncing." 2

# Fail-closed on the deny list. See the header.
[ -r "$DENY" ] || die "Deny list missing" \
    "$DENY is not readable. The sweep for internal names cannot run, so the copy is refused. Set DHCP_GOLIB_DENY to the list, or create it." 2

mapfile -t PATTERNS < <(sed -e 's/#.*//' -e 's/[[:space:]]*$//' "$DENY" | grep -v '^$' || true)
[ "${#PATTERNS[@]}" -gt 0 ] || die "Deny list empty" \
    "$DENY contains no patterns. An empty list would sweep for nothing and pass everything." 2

TMP="$(mktemp -d)"
# shellcheck disable=SC2064  # expand TMP now, not at trap time
trap "rm -rf '$TMP'" EXIT

# git archive emits the TRACKED tree at that commit and nothing else: no
# .git, no untracked scratch, no ignored build output. The .claude removal
# below is therefore belt-and-braces against the library ever tracking one,
# and the assertion after it is what says so out loud.
mkdir -p "$TMP/tree"
git -C "$SRC" archive --format=tar "$SHA" | tar -x -C "$TMP/tree"

rm -rf "$TMP/tree/.claude" "$TMP/tree/.git"
# Captured rather than piped into a consumer. A `find | grep -q` exits
# at the first match, the SIGPIPE makes the pipeline status non-zero,
# and under pipefail this `if` would then read a HIT as no hit -- the
# one direction a refusal must never fail in.
survivors="$(find "$TMP/tree" \( -name .claude -o -name .git \) -print)"
if [ -n "$survivors" ]; then
    die "Excluded directory survived" "A .claude or .git directory is still present after removal."
fi

# The sweep. Case-insensitive fixed strings over every copied file,
# filenames included: a path can carry a name as easily as a line can.
HITS="$TMP/hits"
: >"$HITS"
for pat in "${PATTERNS[@]}"; do
    if grep -rniF -e "$pat" "$TMP/tree" >>"$HITS" 2>/dev/null; then :; fi
    find "$TMP/tree" -iname "*${pat}*" -printf '%p: filename\n' >>"$HITS" 2>/dev/null || true
done
if [ -s "$HITS" ]; then
    # The hits are counted, not printed: printing them here would put the
    # names into a CI log, which is the same publication this refuses.
    die "Internal names found" \
        "The sweep found $(wc -l <"$HITS") hit(s) in the library tree at $SHA. The copy is refused. Inspect the source tree locally; the matches are deliberately not echoed."
fi

# No third-party modules: the library has none, and a `replace`d directory
# that pulled some in would drag them into this module's graph without ever
# appearing in a dependency review.
if [ -f "$TMP/tree/go.mod" ] && grep -qE '^\s*(require|replace)\b' "$TMP/tree/go.mod"; then
    die "Library has module dependencies" \
        "$SHA's go.mod carries a require or replace directive. The seam assumes the library vendors nothing; a dependency arriving this way would bypass dependency review."
fi

rm -rf "${DEST:?}"
mkdir -p "$(dirname "$DEST")"
mv "$TMP/tree" "$DEST"
printf '%s\n' "$SHA" >"$DEST/SOURCE"

echo "Synced github.com/claymore666/dhcp-golib @ $SHA into $DEST"
echo "Files: $(find "$DEST" -type f | wc -l)"

# The manifest is written HERE, by the gate that reads it, so the two
# cannot disagree about its format. Without it the copy ships with
# nothing to check it against -- which is the state this seam was in
# until r2, and the reason five gates could be switched off over this
# directory on an argument nothing enforced.
#
# git ls-files is the manifest's population, so a freshly synced tree
# must be staged before the manifest means anything. Said out loud
# rather than assumed: a manifest generated before `git add` would
# record the OLD file set and pass against the new one.
echo "Stage the copy, then write its manifest:"
echo "  git add -A $DEST && DEST_DIR=$DEST scripts/check-dhcp-golib-copy.sh --write && git add $DEST/MANIFEST"
