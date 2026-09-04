#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# No living document may say `dhcpcd` (external review row E-4).
#
# WHY THIS EXISTS
#
# The 2.0 branch performs the DHCP exchange in-process through the
# in-tree library. There is no dhcpcd in the image, none in the tree,
# and no helper the plugin execs. The documents said otherwise in 82
# places, which is not a typo class: an operator reading them is told to
# `pgrep dhcpcd`, to read a lease file that does not exist, and to set
# two plugin settings the manifest no longer declares. Every one of
# those is a wrong instruction, not a stale sentence.
#
# The sweep that removed them is a one-time act. This is the part that
# does not decay: a document reintroducing the word goes red on the next
# push rather than at the next release's docs review.
#
# WHAT IT CANNOT DO, said here rather than discovered later
#
#   * IT IS KEYED ON A SPELLING. A page can describe the deleted
#     mechanism perfectly without ever using the word -- "the hook
#     script", "the FIFO", "--noconfigure", "the client's private mount
#     namespace", "the EXPIRE event". This gate is blind to every one of
#     them. It is a backstop against the word coming back, not evidence
#     that the prose is true.
#   * SPLIT AND HYPHENATED FORMS ARE NOT COVERED. `dhcp cd`, `dhcp-cd`
#     and a URL-encoded form all pass. Case is covered (-i) and word
#     boundaries are (a `dhcpcdN` identifier does not trip it, and
#     `dhcpcd.conf` does).
#
# THE DOMAIN IS DERIVED, NOT LISTED
#
# Every tracked *.md outside internal/dhcp-golib/ (a vendored copy of
# another repository, whose history is not ours to rewrite). A universal
# gate is satisfied by emptying its domain, so two things guard against
# that: an EXPECTED list of core documents whose absence is a REFUSAL
# (exit 2, not a pass), and an announced count on every run.
#
# THE ALLOWLIST IS TWO ENTRIES AND ONE IS A REGION
#
#   RELEASE_NOTES.md   -- from the first 1.x release heading DOWNWARDS
#                         only. The word is true of those releases. It
#                         is NOT permitted above that line, which is
#                         where the 2.0 sections live.
#   docs/release-runbook.md -- whole file. It is the 1.x release
#                         runbook and describes a 1.x process.
#
# Each carries a dated reason below. Adding a third entry is a decision
# about a living document and should read like one.
#
# Usage: check-no-dhcpcd-word.sh [<repo root>]
# Exit:  0 clean, 1 the word appears in a living document,
#        2 refuses to judge (no work tree, empty domain, expected
#          document missing, allowlisted region not found).

set -uo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$ROOT" || { echo "check-no-dhcpcd-word: cannot cd to $ROOT" >&2; exit 2; }

WORD='dhcpcd'

# Documents that must exist. Their absence means the repository is not
# the one this gate was written for -- a rename, a bad checkout, a
# sparse clone -- and a gate that passes on that is worse than no gate.
EXPECTED=(
    README.md
    SECURITY.md
    RELEASE_NOTES.md
    docs/reference.md
    docs/internals.md
    docs/roadmap.md
)

# Allowlist. Each entry: path, mode, reason. Mode `file` exempts the
# whole document; mode `below` exempts everything from the first line
# matching REGION_RE downwards, and holds the lines above it to the
# rule.
ALLOW_FILE_PATHS=(
    "docs/release-runbook.md"
)
ALLOW_FILE_REASONS=(
    "2026-09-04: the 1.x release runbook, describing the 1.x release process, in which dhcpcd is a real component. Out of scope for the 2.0 docs sweep; it is rewritten when 2.0 gets its own runbook."
)
ALLOW_BELOW_PATH="RELEASE_NOTES.md"
ALLOW_BELOW_RE='^## v1\.'
ALLOW_BELOW_REASON="2026-09-04: history stays history. From the first 1.x release heading downwards these entries describe versions that really shipped dhcpcd; rewriting them would falsify the record. Everything ABOVE that line -- the 2.0 sections -- is held to the rule."

fail=0

git rev-parse --is-inside-work-tree >/dev/null 2>&1 || {
    echo "::error title=check-no-dhcpcd-word cannot run::not inside a git work tree, so the document set cannot be derived" >&2
    exit 2
}

for f in "${EXPECTED[@]}"; do
    [ -f "$f" ] && continue
    echo "::error title=check-no-dhcpcd-word refuses::expected document $f is missing." \
         "The gate will not report a pass over a document set it does not recognise." >&2
    exit 2
done

mapfile -t DOCS < <(git ls-files '*.md' | grep -v '^internal/dhcp-golib/' | sort)

if [ "${#DOCS[@]}" -eq 0 ]; then
    echo "::error title=check-no-dhcpcd-word refuses::the document set is empty." \
         "This step would otherwise pass having inspected nothing." >&2
    exit 2
fi

# Announced on every run, pass or fail: a count is the only thing that
# distinguishes "nothing said dhcpcd" from "nothing was read".
echo "check-no-dhcpcd-word: inspecting ${#DOCS[@]} document(s) for the word '$WORD'"

allow_file_index() {
    local p="$1" i
    for i in "${!ALLOW_FILE_PATHS[@]}"; do
        [ "${ALLOW_FILE_PATHS[$i]}" = "$p" ] && { echo "$i"; return 0; }
    done
    return 1
}

for f in "${DOCS[@]}"; do
    if idx=$(allow_file_index "$f"); then
        echo "ALLOWED $f (whole file) — ${ALLOW_FILE_REASONS[$idx]}"
        continue
    fi

    # The region case. Everything from the first heading matching
    # ALLOW_BELOW_RE downwards is history; above it is a living
    # document and is judged.
    if [ "$f" = "$ALLOW_BELOW_PATH" ]; then
        start=$(grep -n -E "$ALLOW_BELOW_RE" "$f" | head -1 | cut -d: -f1)
        if [ -z "$start" ]; then
            echo "::error title=check-no-dhcpcd-word refuses::$f has no line matching $ALLOW_BELOW_RE," \
                 "so the boundary between the living part and the history part cannot be found." >&2
            exit 2
        fi
        echo "ALLOWED $f (from line $start down) — $ALLOW_BELOW_REASON"
        hits=$(head -n "$((start - 1))" "$f" | grep -in -E "\\b${WORD}\\b" || true)
        if [ -n "$hits" ]; then
            echo "::error file=$f,title=dhcpcd in a living document::the word appears ABOVE the history boundary (line $start)" >&2
            printf '%s\n' "$hits" | sed "s|^|  $f:|" >&2
            fail=1
        else
            echo "PASS  $f (lines 1-$((start - 1)))"
        fi
        continue
    fi

    hits=$(grep -in -E "\\b${WORD}\\b" "$f" || true)
    if [ -n "$hits" ]; then
        echo "::error file=$f,title=dhcpcd in a living document::this branch has no dhcpcd; rewrite the sentence or delete it" >&2
        printf '%s\n' "$hits" | sed "s|^|  $f:|" >&2
        fail=1
    fi
done

if [ "$fail" -ne 0 ]; then
    echo "check-no-dhcpcd-word: FAILED" >&2
    exit 1
fi

echo "check-no-dhcpcd-word: clean across ${#DOCS[@]} document(s)"
