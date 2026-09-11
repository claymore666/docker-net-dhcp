#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every grant in config.json has exactly one sentence in SECURITY.md,
# and every sentence names a grant config.json still asks for (E-2,
# #725's second follow-up).
#
# WHY THIS EXISTS
#
# SECURITY.md's job is to let an operator decide whether to approve what
# `docker plugin install` prompts them with. That decision is made
# against a document, and the document is not the manifest. Before this
# gate, the manifest could gain or lose a privilege and the document
# would keep describing the old set -- silently, and in the direction
# that matters most: a grant REMOVED from config.json and left in the
# prose reads as a privilege the plugin still holds, which is a document
# telling an operator to be more worried than the facts warrant, and a
# grant ADDED with no sentence is the opposite and worse.
#
# BOTH DIRECTIONS ARE CHECKED, AND THE SELF-TEST DRIVES BOTH. A gate
# that only asks "does every grant have a sentence" is satisfied by a
# document that has a sentence for everything that ever was granted.
#
# THE GRANT SET IS DERIVED, NOT LISTED. jq reads config.json and emits
# one canonical token per grant:
#
#   network:<type>            from .network.type
#   ipchost                   when .ipchost is true
#   pidhost                   when .pidhost is true
#   mount:<source>:<options>  one per .mounts[] entry, keyed on the HOST
#                             path AND the mount options, because that
#                             is what an operator is being asked to
#                             expose and on what terms
#   device:<path>             one per .linux.devices[]
#   allowalldevices           when .linux.allowAllDevices is true
#   CAP_*                     one per .linux.capabilities[]
#
# THE SET IS MOBY'S, NOT THIS MANIFEST'S. The fields are the ones
# `computePrivileges` walks when the daemon builds the install prompt
# (moby, daemon/pkg/plugin/backend_linux.go: network.type, IpcHost,
# PidHost, mounts[].Source, linux.devices[].Path, linux.AllowAllDevices,
# linux.capabilities). Deriving the list from what this manifest happens
# to carry is how `ipchost: true` and `allowAllDevices: true` came to be
# invisible to this gate: both are prompted, neither was projected, and
# either could have been added with the whole lane green.
#
# MOUNT OPTIONS ARE PART OF THE TOKEN AND THE DAEMON DOES NOT PROMPT ON
# THEM. `computePrivileges` takes the source path alone, so flipping
# /var/run/docker from `ro` to `rw` re-uses the operator's existing
# approval and asks nobody anything. "Read-only" is the word the
# sentence for that mount rests on, and a reviewer flipped it on both
# manifests with the lane green (privilege review, 2026-09-05). The
# option set is in the token so the row has to be rewritten when the
# terms change.
#
# The document must use those tokens verbatim. That is the answer to
# "two spellings enumerated means a third exists": there is one spelling
# and it comes from the manifest, so `NET_ADMIN`, "host PID namespace"
# and "the Docker socket" cannot silently satisfy a row.
#
# WHAT IT CANNOT DO, said here rather than discovered later
#
#   * IT CANNOT READ. A row whose sentence is true of a different
#     grant, or is simply wrong, passes. What is checked is that the row
#     exists, is not empty, and cites at least one file that is really
#     in the tree -- a citation to a file nobody wrote is the failure
#     mode a presence check CAN see.
#   * IT IS BLIND TO THE EFFECTIVE SET. config.json is the REQUESTED
#     set; Docker composes it additively over the OCI defaults, and the
#     fourteen defaults have no manifest line to check against. That is
#     stated in SECURITY.md's prose, which this gate does not read.
#   * config-cover.json IS NOT COMPARED HERE. check-manifest-parity.sh
#     already fails when the two manifests' privilege fields differ, so
#     checking the same thing twice would give one fact two derivations.
#
# Usage: check-privilege-sentences.sh [<repo root>]
# Exit:  0 the sets agree, 1 they differ, 2 refuses to judge (missing
#        file, missing block markers, empty set on either side).

set -uo pipefail

ROOT="${1:-.}"
MANIFEST="$ROOT/config.json"
DOC="$ROOT/SECURITY.md"
BEGIN='<!-- privilege-sentences: begin -->'
END='<!-- privilege-sentences: end -->'

fail() { echo "::error title=Privilege sentences::$*" >&2; }

for f in "$MANIFEST" "$DOC"; do
    if [ ! -f "$f" ]; then
        fail "$f is missing; the two sets cannot be compared"
        exit 2
    fi
done

if ! command -v jq >/dev/null 2>&1; then
    fail "jq is not installed; the grant set is derived from $MANIFEST and cannot be guessed"
    exit 2
fi

manifest_grants=$(jq -r '
    ("network:" + (.network.type // "none")),
    (if .ipchost == true then "ipchost" else empty end),
    (if .pidhost == true then "pidhost" else empty end),
    (.mounts[]? | "mount:" + (.source // "")
        + (if ((.options // []) | length) > 0 then ":" + ((.options | sort) | join(",")) else "" end)),
    (.linux.devices[]? | "device:" + ((.path // .name) // "")),
    (if ((.linux.allowAllDevices // .linux.allowalldevices) // false) == true then "allowalldevices" else empty end),
    (.linux.capabilities[]? )
' "$MANIFEST" | sort -u)

if [ -z "$manifest_grants" ]; then
    fail "$MANIFEST yielded no grants at all; a comparison against an empty set passes vacuously"
    exit 2
fi

# The block is extracted by its markers rather than by heading text: a
# heading is prose and gets rewritten, a marker is a contract.
if ! grep -qF "$BEGIN" "$DOC" || ! grep -qF "$END" "$DOC"; then
    fail "$DOC has no '$BEGIN' / '$END' block; there is nothing to compare $MANIFEST against"
    exit 2
fi

block=$(awk -v b="$BEGIN" -v e="$END" '
    index($0, b) { inb = 1; next }
    index($0, e) { inb = 0; next }
    inb { print }
' "$DOC")

# Rows only: a markdown table row that starts with a backticked token in
# its first cell. The header and the |---| separator have no backticks
# and are skipped by that alone.
rows=$(printf '%s\n' "$block" | grep -E '^\| *`[^`]+` *\|' || true)

if [ -z "$rows" ]; then
    fail "the privilege-sentences block in $DOC has no rows; an empty table would otherwise agree with nothing"
    exit 2
fi

doc_grants=$(printf '%s\n' "$rows" | sed -E 's/^\| *`([^`]+)`.*/\1/' | sort -u)

status=0

missing_sentence=$(comm -23 <(printf '%s\n' "$manifest_grants") <(printf '%s\n' "$doc_grants"))
missing_grant=$(comm -13 <(printf '%s\n' "$manifest_grants") <(printf '%s\n' "$doc_grants"))

if [ -n "$missing_sentence" ]; then
    status=1
    while IFS= read -r g; do
        [ -n "$g" ] || continue
        fail "$MANIFEST asks for '$g' and $DOC has no row for it — an operator approving this grant is told nothing about what it is for"
    done <<< "$missing_sentence"
fi

if [ -n "$missing_grant" ]; then
    status=1
    while IFS= read -r g; do
        [ -n "$g" ] || continue
        fail "$DOC has a row for '$g' and $MANIFEST does not ask for it — the document describes a privilege this plugin no longer requests"
    done <<< "$missing_grant"
fi

# Per-row substance. Checked for every row rather than only the matched
# ones, so a row that is about to be deleted is still held to it.
while IFS= read -r row; do
    [ -n "$row" ] || continue
    token=$(printf '%s' "$row" | sed -E 's/^\| *`([^`]+)`.*/\1/')
    sentence=$(printf '%s' "$row" | awk -F'|' '{print $3}' | sed -E 's/^ +| +$//g')
    consumers=$(printf '%s' "$row" | awk -F'|' '{print $4}')

    if [ "${#sentence}" -lt 30 ]; then
        status=1
        fail "the row for '$token' in $DOC says $(printf '%q' "$sentence"), which is not a sentence about what the grant is for"
    fi

    # A citation is checked for EXISTENCE, which is the half a presence
    # check can actually see. Whether the file is the right consumer is
    # not checkable here and is not claimed.
    cited=$(printf '%s' "$consumers" | grep -oE '`[^`]+`' | tr -d '`' || true)
    if [ -z "$cited" ]; then
        status=1
        fail "the row for '$token' in $DOC cites no consumer file; a grant with no named consumer is a grant nobody has to justify"
        continue
    fi
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        if [ ! -e "$ROOT/$f" ]; then
            status=1
            fail "the row for '$token' in $DOC cites $f, which is not in the tree"
        fi
    done <<< "$cited"
done <<< "$rows"

echo "manifest grants ($(printf '%s\n' "$manifest_grants" | wc -l)):"
printf '%s\n' "$manifest_grants" | sed 's/^/  /'
echo "documented grants ($(printf '%s\n' "$doc_grants" | wc -l)):"
printf '%s\n' "$doc_grants" | sed 's/^/  /'

if [ "$status" -ne 0 ]; then
    echo "privilege-sentence check FAILED" >&2
    exit 1
fi
echo "PASS  every grant in config.json has one sentence in SECURITY.md, and no sentence outlives its grant"
