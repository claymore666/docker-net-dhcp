#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Extract the release-notes section a tag publishes, or REFUSE.
#
# WHY THIS EXISTS
#
# `release.yml` used to do this inline, with
#
#     awk -v hdr="## ${TAG}" '$0==hdr{...}' RELEASE_NOTES.md > section.md || true
#     [ -s section.md ] || echo "Pre-release / dry-run build for ..." > section.md
#
# an EXACT string comparison with a silent fallback. The living heading in
# this repository is `## v2.0.0 (unreleased)`, which is not equal to
# `## v2.0.0`, so at the `v2.0.0` tag the extraction found nothing, the
# fallback wrote a one-line placeholder, and the step exited 0. The release
# page would have carried the placeholder instead of the notes the
# maintainer signed off, and nothing anywhere would have said so. MEASURED
# as mutant R2 in the 2.0.0 rename round: dropping or restoring the
# `(unreleased)` suffix changed no verdict in the whole local lane.
#
# So: never the placeholder. A tag whose notes cannot be identified is a
# release that must not publish a body at all, and the way to find that out
# is on the workstation before the tag, not on the release page after the
# images are pushed.
#
# IT IS A SCRIPT AND THE WORKFLOW CALLS IT. Keeping the awk in the YAML and
# testing a copy of it here would test the copy. The workflow now has no
# extraction of its own; this file is the only one, and the lane drives it.
#
# WHICH SECTION A TAG PUBLISHES
#
# `vX.Y.Z` publishes `## vX.Y.Z`. A PRE-RELEASE tag (`vX.Y.Z-rc2`, anything
# with a suffix) publishes its own section if the file has one, and
# otherwise the section of the version it is a candidate for — an rc is a
# dry run OF vX.Y.Z and this repository does not write a notes section per
# rc. That fallback is between two EXACT headings; it is not a loosening of
# the match, and it does not reach a decorated heading.
#
# WHAT IT REFUSES, ALL FIVE LOUD
#
#   - the version's heading carries anything after it (`## v2.0.0
#     (unreleased)`): the suffix is quoted back, because that is the live
#     failure and the fix is to remove it before tagging;
#   - no heading for the tag (nor for its release version): the tag is
#     named and the version headings that DO exist are listed;
#   - the section exists and is empty: an empty body is not a body;
#   - two headings for the same version: which one ships would be decided
#     by position;
#   - the tag is not vX.Y.Z-shaped: this is called with `${TAG}` from a
#     tag push, so an unparseable value means the caller changed.
#
# WHAT IT CANNOT DO
#
# It judges the FILE, not the release. It cannot tell that a section is the
# wrong text, out of date, or unsigned-off; the runbook's step 4 sign-off is
# what covers that. And its verdict is about `RELEASE_NOTES.md` as it stands
# in the tree it is run against — the tag's tree at release time, this
# branch's tree when the runbook runs it before tagging.
#
# Usage: release-body.sh <tag> [notes-file]
#        the section body goes to stdout, diagnostics to stderr
# Exit:  0 a body was extracted, 1 REFUSED (no body is produced),
#        2 cannot judge (bad usage, unreadable file)
set -uo pipefail

TAG="${1:-}"
NOTES="${2:-$(dirname "$0")/../RELEASE_NOTES.md}"

if [ -z "$TAG" ] || [ "$#" -gt 2 ]; then
    echo "usage: $0 <tag> [notes-file]" >&2
    exit 2
fi

case "$TAG" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) echo "release-body: '${TAG}' is not a vX.Y.Z tag, so there is no section to look for — cannot judge" >&2
       exit 2 ;;
esac

if [ ! -f "$NOTES" ] || [ ! -r "$NOTES" ]; then
    echo "release-body: cannot read ${NOTES} as a regular file — cannot judge" >&2
    exit 2
fi

refuse() {
    # `::error` so a refusal inside the release run is annotated on the run
    # page: this is the one gate whose failure arrives after the images are
    # already pushed, and the releaser needs the cause without opening logs.
    echo "::error title=Release body refused::$*" >&2
    echo "release-body: no body was produced. The release page must not fall back to a placeholder:" \
         "a generated one-liner where the signed-off notes belong is indistinguishable, on the page," \
         "from a release that had nothing to say." >&2
    exit 1
}

# The version a pre-release tag is a candidate FOR. `v2.0.0-rc2` -> `v2.0.0`;
# `v2.0.0` -> itself. Parameter expansion, not a regex: the version carries
# dots and a regex here would match versions it was not asked about.
BASE="${TAG%%-*}"

# Every `## ` heading in the file, as "<line>\t<text>". `## ` with the space
# excludes `###` subsections, which are INSIDE a version's section and must
# not end it. Read once: every question below is asked of this census.
HEADINGS=$(awk '/^## /{printf "%d\t%s\n", NR, $0}' "$NOTES")
if [ -z "$HEADINGS" ]; then
    echo "release-body: ${NOTES} contains no '## ' heading at all, so this gate would be judging" \
         "an empty census — cannot judge" >&2
    exit 2
fi

# Version headings only, for the diagnosis when nothing matches. A reader
# who is told "no section for v2.0.1" needs to see what the file does have.
version_headings() {
    printf '%s\n' "$HEADINGS" | cut -f2- | grep -E '^## v[0-9]' | head -8 | tr '\n' ' '
}

# Exact heading lines for one version. String equality, never a pattern:
# `## v1.3.4` must not answer for `## v1.3.4x` and a version's dots are not
# wildcards.
exact_lines() { # <version> -> line numbers, one per line
    printf '%s\n' "$HEADINGS" | awk -v want="## $1" '
        { n = $0; sub(/^[0-9]+\t/, "", n); if (n == want) { l = $0; sub(/\t.*$/, "", l); print l } }'
}

# Headings that START with the version and then carry something else, with
# whitespace between. The whitespace is what keeps `## v2.0.0-rc1` from
# reading as a decorated `## v2.0.0`: those are two different versions, and
# a prefix test alone would confuse them.
decorated_text() { # <version> -> the decorated heading lines, one per line
    printf '%s\n' "$HEADINGS" | awk -v pfx="## $1" '
        {
            n = $0; sub(/^[0-9]+\t/, "", n)
            if (substr(n, 1, length(pfx)) == pfx) {
                rest = substr(n, length(pfx) + 1)
                if (rest ~ /^[ \t]/) print n
            }
        }'
}

# The lines under a heading, up to the next `## ` heading or end of file.
section_at() { # <line number> -> the section body
    awk -v start="$1" 'NR > start { if ($0 ~ /^## /) exit; print }' "$NOTES"
}

# Each candidate in turn, and a refusal from any of them is FINAL rather
# than a reason to try the next one. A decorated or duplicated heading for
# the release version is a defect in the file; falling through to another
# candidate would answer a question nobody asked and hide it.
for cand in "$TAG" "$BASE"; do
    lines=$(exact_lines "$cand")
    n=$(printf '%s' "$lines" | grep -c .)

    if [ "${n:-0}" -gt 1 ]; then
        refuse "${NOTES} carries ${n} '## ${cand}' headings (lines $(printf '%s' "$lines" | tr '\n' ' ')). Which one ships would be decided by position in the file. Merge them into one section."
    fi

    if [ "${n:-0}" -eq 1 ]; then
        body=$(section_at "$lines")
        if [ -z "$(printf '%s' "$body" | tr -d '[:space:]')" ]; then
            refuse "${NOTES}'s '## ${cand}' section (line ${lines}) is empty. An empty release body is not a body; write the section, or the release publishes a page with nothing on it."
        fi
        [ "$cand" = "$TAG" ] || echo "release-body: ${TAG} has no section of its own; publishing '## ${cand}', the version it is a pre-release of." >&2
        printf '%s\n' "$body"
        exit 0
    fi

    dec=$(decorated_text "$cand")
    if [ -n "$dec" ]; then
        refuse "${NOTES} has no '## ${cand}' heading, but it does have $(printf '%s' "$dec" | tr '\n' '/') — the version heading carries something after it, so an exact match finds nothing and the release would publish a placeholder. Remove what follows the version before tagging (docs/release-runbook.md step 4)."
    fi

    # A tag with no suffix has one candidate, so do not report the same
    # miss twice.
    [ "$TAG" = "$BASE" ] && break
done

if [ "$TAG" = "$BASE" ]; then
    refuse "${NOTES} has no '## ${TAG}' section. Version headings present: $(version_headings)"
fi
refuse "${NOTES} has no '## ${TAG}' section and none for '## ${BASE}', the version it is a pre-release of. Version headings present: $(version_headings)"
