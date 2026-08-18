#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Compare the reference digests printed in docs/verifying-releases.md
# against the binaries this release actually publishes (#502).
#
# Why this failure mode is worse than an omission:
#
# The "Rebuilding the binaries yourself" section ends with the expected
# sha256 of the two published binaries, labelled with the version they
# belong to. The digests cannot be written before the tag builds, so
# refreshing them is inherently a post-tag step — runbook step 10b — and
# nothing enforced it. A reader who rebuilds the current tag and compares
# against the PREVIOUS release's digests gets a mismatch, and the honest
# conclusion to draw from a mismatch is "the published binaries do not
# match the source". A verification procedure that produces a false
# accusation against our own release is worse than having none.
#
# It is also invisible to every pattern-matching gate we have: the stale
# text is perfectly well-formed, just wrong — the same shape as the
# check-version-pins blind spot.
#
# The version label and the digests are checked TOGETHER. A correct
# digest under the wrong version heading is still wrong, and would send
# the same reader to the same false conclusion.
#
# Usage:
#   check-reference-digests.sh <tag> <net-dhcp path> <dhcp-handler path> [arch]
#
# arch defaults to amd64. Since #507 the doc carries one digest block
# per architecture, labelled "For **vX.Y.Z** (`linux/<arch>`) they are:".
# The unlabelled legacy form is still accepted as the amd64 block, so a
# doc written before the arm64 tags existed keeps verifying.
#
# Env seams (for the self-test):
#   DIGEST_DOCS  path to the doc to parse (default docs/verifying-releases.md)
#
# Exit: 0 match, 1 stale/mismatched, 2 cannot see (usage, missing file,
#       unparseable doc).

set -uo pipefail

DOCS="${DIGEST_DOCS:-docs/verifying-releases.md}"

TAG="${1:-}"
BIN_MAIN="${2:-}"
BIN_HANDLER="${3:-}"
ARCH="${4:-amd64}"

if [ -z "$TAG" ] || [ -z "$BIN_MAIN" ] || [ -z "$BIN_HANDLER" ]; then
    echo "usage: $0 <tag> <net-dhcp path> <dhcp-handler path> [arch]" >&2
    exit 2
fi
if [ ! -f "$DOCS" ]; then
    echo "check-reference-digests: $DOCS does not exist" >&2
    exit 2
fi
for f in "$BIN_MAIN" "$BIN_HANDLER"; do
    if [ ! -f "$f" ]; then
        echo "check-reference-digests: $f does not exist — nothing to compare against" >&2
        exit 2
    fi
done

# A pre-release tag builds from the same source as the release it
# rehearses, so its binaries carry the digests the real tag will publish.
# Compare the label against the BASE version: that makes an rc a true
# dry run of this check rather than a guaranteed failure, which is the
# whole reason the rc exists.
BASE_TAG="${TAG%%-*}"

# The label line, e.g. "... For **v1.5.0** (`linux/amd64`) they are:".
# Matched as a superset — any bolded vN.N.N on a line that introduces
# the digests for this arch — then judged, rather than requiring the
# exact sentence. The unlabelled legacy form counts as amd64, so docs
# from before the per-arch tags keep verifying.
arch_label_line() {
    grep -n "For \*\*v[0-9][0-9.]*\*\* (\`linux/${ARCH}\`) they are" "$DOCS" | head -1
}
LABEL_LINE=$(arch_label_line)
if [ -z "$LABEL_LINE" ] && [ "$ARCH" = "amd64" ]; then
    LABEL_LINE=$(grep -n "For \*\*v[0-9][0-9.]*\*\* they are" "$DOCS" | head -1)
fi
if [ -z "$LABEL_LINE" ]; then
    echo "check-reference-digests: no ${ARCH} version label found in $DOCS." >&2
    echo "  Expected a line of the form: For **vX.Y.Z** (\`linux/${ARCH}\`) they are:" >&2
    echo "  Either the digest section was reshaped or it is gone; this check" >&2
    echo "  is now watching nothing, which is why it fails instead of passing." >&2
    exit 2
fi
LABEL_LINENO=${LABEL_LINE%%:*}
DOC_VERSION=$(printf '%s\n' "$LABEL_LINE" | sed -n 's/.*For \*\*\(v[0-9][0-9.]*\)\*\*.*/\1/p')

# The two digest lines: 64 hex chars, two spaces, the binary name.
# Scoped to THIS arch\'s block: from its label line to the next label
# line (or EOF). With two arch blocks in one doc, a whole-file grep
# would silently read the first block for both arches.
doc_digest() {
    awk -v start="$LABEL_LINENO" \
        'NR > start && /For \*\*v[0-9]/ { exit } NR >= start' "$DOCS" \
      | sed -n "s/^\([0-9a-f]\{64\}\)  *$1\$/\1/p" | head -1
}
DOC_MAIN=$(doc_digest "net-dhcp")
DOC_HANDLER=$(doc_digest "dhcp-handler")

if [ -z "$DOC_MAIN" ] || [ -z "$DOC_HANDLER" ]; then
    echo "check-reference-digests: could not read both reference digests from $DOCS" >&2
    echo "  net-dhcp:     ${DOC_MAIN:-<missing>}" >&2
    echo "  dhcp-handler: ${DOC_HANDLER:-<missing>}" >&2
    exit 2
fi

GOT_MAIN=$(sha256sum "$BIN_MAIN" | cut -d' ' -f1)
GOT_HANDLER=$(sha256sum "$BIN_HANDLER" | cut -d' ' -f1)

rc=0
if [ "$DOC_VERSION" != "$BASE_TAG" ]; then
    echo "::error::$DOCS documents reference digests for $DOC_VERSION, but this release is $BASE_TAG." >&2
    echo "  A reader rebuilding $BASE_TAG and comparing against $DOC_VERSION's digests gets a" >&2
    echo "  mismatch, and concludes the published binaries do not match the source." >&2
    rc=1
fi
if [ "$DOC_MAIN" != "$GOT_MAIN" ]; then
    echo "::error::net-dhcp digest in $DOCS is stale." >&2
    echo "  documented: $DOC_MAIN" >&2
    echo "  published:  $GOT_MAIN" >&2
    rc=1
fi
if [ "$DOC_HANDLER" != "$GOT_HANDLER" ]; then
    echo "::error::dhcp-handler digest in $DOCS is stale." >&2
    echo "  documented: $DOC_HANDLER" >&2
    echo "  published:  $GOT_HANDLER" >&2
    rc=1
fi

if [ "$rc" -ne 0 ]; then
    echo >&2
    echo "Update the block in $DOCS (runbook step 10b) to:" >&2
    echo >&2
    echo "The two pairs of digests must match. For **$BASE_TAG** (\`linux/${ARCH}\`) they are:" >&2
    echo >&2
    echo '```' >&2
    echo "$GOT_MAIN  net-dhcp" >&2
    echo "$GOT_HANDLER  dhcp-handler" >&2
    echo '```' >&2
    exit 1
fi

echo "check-reference-digests: $DOCS matches the published $BASE_TAG ${ARCH} binaries"
echo "  net-dhcp     $GOT_MAIN"
echo "  dhcp-handler $GOT_HANDLER"
