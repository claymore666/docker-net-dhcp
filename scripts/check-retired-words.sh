#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# No living document may say `dhcpcd`, and nothing on this branch may
# call the 2.0 line a beta (external review row E-4; #911).
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
# `beta` is the same class one branch later. The 2.x branch was named
# `2.x-beta` until 2026-09-05, and the name leaked into the product in
# 150 places -- a README note telling operators "this branch is the 2.0
# beta", a version column in the reference tables reading "2.0 beta"
# where every other row reads `v1.8.0`, and the text of the error an
# operator gets from `-o ipv6=true`. There is no beta. The line is 2.0,
# its first pre-release is `v2.0.0-rc1`, and IPv6 parity is tracked in
# #911.
#
# The sweeps that removed them are one-time acts. This is the part that
# does not decay: a document or a source file reintroducing either word
# goes red on the next push rather than at the next release's docs
# review.
#
# ONE MECHANISM, A LIST OF WORDS
#
# Each word carries its own DOMAIN and its own ALLOWANCES, because they
# are not the same question. `dhcpcd` is a claim about documents: the
# 1.x release runbook and the 1.x release-notes history are true when
# they say it. `beta` is a claim about this branch's whole surface,
# documents and Go sources alike -- the word reached error strings an
# operator reads and comments a maintainer acts on, and a gate that
# only read *.md would have watched the sweep undone in a comment.
#
# Everything below the boundary and allowance logic is shared. A second
# copy of it, keyed on the second word, is how the second word ends up
# with a boundary nobody drives.
#
# WHAT IT CANNOT DO, said here rather than discovered later
#
#   * IT IS KEYED ON A SPELLING. A page can describe the deleted
#     mechanism perfectly without ever using the word -- "the hook
#     script", "the FIFO", "--noconfigure", "the client's private mount
#     namespace", "the EXPIRE event". So can a page that calls 2.0 "the
#     preview" or "the pre-release line". This gate is blind to every
#     one of them. It is a backstop against the word coming back, not
#     evidence that the prose is true.
#   * SPLIT AND HYPHENATED FORMS ARE NOT COVERED. `dhcp cd`, `dhcp-cd`
#     and a URL-encoded form all pass. Case is covered (-i) and word
#     boundaries are (a `dhcpcdN` identifier does not trip it, and
#     `dhcpcd.conf` does; `TestBetaFoo` does not trip `beta`, and
#     `2.0-beta` does).
#   * THE GO DOMAIN IS EVERY TRACKED *.go OUTSIDE THE VENDORED COPY.
#     It reads comments, strings and identifiers alike, because all
#     three reach a reader. A test fixture that legitimately wants a
#     placeholder named `beta` therefore has to pick another word; the
#     alternative is an allowlist nobody rereads.
#
# THE DOMAIN IS DERIVED, NOT LISTED
#
# Every tracked file matching the word's globs, outside
# internal/dhcp-golib/ (a vendored copy of another repository, whose
# history is not ours to rewrite). A universal gate is satisfied by
# emptying its domain, so three things guard against that: an EXPECTED
# list of core documents whose absence is a REFUSAL (exit 2, not a
# pass), a REFUSAL when ANY GLOB of a word's domain comes back empty --
# checked per glob, not over the union, or a word covering `*.md *.go`
# would still pass with no Go file tracked at all -- and an announced
# count per word on every run.
#
# THE ALLOWANCES ARE KEYED ON (WORD, PATH)
#
#   dhcpcd  RELEASE_NOTES.md        -- from the first 1.x release
#                                      heading DOWNWARDS only.
#   dhcpcd  docs/release-runbook.md -- whole file.
#   beta    RELEASE_NOTES.md        -- from the same heading downwards.
#
# `beta` is deliberately NOT allowed in docs/release-runbook.md: the
# reason that exempts that file is about dhcpcd being a real component
# of the 1.x process, and it does not transfer. Each entry carries a
# dated reason below. Adding one is a decision about a living document
# and should read like one.
#
# Usage: check-retired-words.sh [<repo root>]
# Exit:  0 clean, 1 a word appears where it is not permitted,
#        2 refuses to judge (no work tree, an empty half of a domain,
#          expected document missing, allowlisted region not found).

set -uo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$ROOT" || { echo "check-retired-words: cannot cd to $ROOT" >&2; exit 2; }

# The word list. WORDS[i] is judged over the tracked files matching
# WORD_GLOBS[i] (space-separated), and WORD_WHAT[i] names that domain in
# the announced count.
WORDS=(
    'dhcpcd'
    'beta'
)
WORD_GLOBS=(
    '*.md'
    '*.md *.go'
)
WORD_WHAT=(
    'document(s)'
    'document(s) and Go source(s)'
)
# Said in the failure, so the person who trips it is told what to write
# instead rather than only what not to write.
WORD_REMEDY=(
    'this branch has no dhcpcd; rewrite the sentence or delete it'
    'there is no beta: the line is 2.0, its first pre-release is v2.0.0-rc1, and IPv6 parity is tracked in #911'
)

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

# Whole-file allowances, keyed on (word, path).
ALLOW_FILE_WORDS=(
    "dhcpcd"
)
ALLOW_FILE_PATHS=(
    "docs/release-runbook.md"
)
ALLOW_FILE_REASONS=(
    "2026-09-04: the 1.x release runbook, describing the 1.x release process, in which dhcpcd is a real component. Out of scope for the 2.0 docs sweep; it is rewritten when 2.0 gets its own runbook."
)

# Region allowances, keyed on (word, path): everything from the first
# line matching the regex downwards is exempt, and the lines above it
# are held to the rule.
ALLOW_BELOW_WORDS=(
    "dhcpcd"
    "beta"
)
ALLOW_BELOW_PATHS=(
    "RELEASE_NOTES.md"
    "RELEASE_NOTES.md"
)
ALLOW_BELOW_RES=(
    '^## v1\.'
    '^## v1\.'
)
ALLOW_BELOW_REASONS=(
    "2026-09-04: history stays history. From the first 1.x release heading downwards these entries describe versions that really shipped dhcpcd; rewriting them would falsify the record. Everything ABOVE that line -- the 2.0 sections -- is held to the rule."
    "2026-09-05: history stays history, for the same reason and at the same boundary. A 1.x entry may describe a beta of something that really was one. Everything ABOVE that line -- the 2.0 sections -- is held to the rule."
)

fail=0

git rev-parse --is-inside-work-tree >/dev/null 2>&1 || {
    echo "::error title=check-retired-words cannot run::not inside a git work tree, so the domain cannot be derived" >&2
    exit 2
}

for f in "${EXPECTED[@]}"; do
    [ -f "$f" ] && continue
    echo "::error title=check-retired-words refuses::expected document $f is missing." \
         "The gate will not report a pass over a document set it does not recognise." >&2
    exit 2
done

# Index lookups over the (word, path) allowance tables. Both return the
# index so the caller can print the reason: an allowance whose reason is
# invisible grows without anyone reading it.
allow_file_index() {
    local w="$1" p="$2" i
    for i in "${!ALLOW_FILE_PATHS[@]}"; do
        [ "${ALLOW_FILE_WORDS[$i]}" = "$w" ] && [ "${ALLOW_FILE_PATHS[$i]}" = "$p" ] && { echo "$i"; return 0; }
    done
    return 1
}

allow_below_index() {
    local w="$1" p="$2" i
    for i in "${!ALLOW_BELOW_PATHS[@]}"; do
        [ "${ALLOW_BELOW_WORDS[$i]}" = "$w" ] && [ "${ALLOW_BELOW_PATHS[$i]}" = "$p" ] && { echo "$i"; return 0; }
    done
    return 1
}

for w in "${!WORDS[@]}"; do
    WORD="${WORDS[$w]}"
    read -r -a globs <<< "${WORD_GLOBS[$w]}"

    # Each glob is checked SEPARATELY for emptiness before the union is
    # taken. A word whose domain is `*.md *.go` and whose *.go half
    # matches nothing still has a non-empty union, so a union-only
    # check would report a pass over a Go tree it never opened -- the
    # emptied-universal defeat, one level down.
    FILES=()
    for g in "${globs[@]}"; do
        mapfile -t part < <(git ls-files -- "$g" | grep -v '^internal/dhcp-golib/' | sort)
        if [ "${#part[@]}" -eq 0 ]; then
            echo "::error title=check-retired-words refuses::the '$g' half of the domain for '$WORD' is empty." \
                 "This step would otherwise pass having inspected nothing under it." >&2
            exit 2
        fi
        FILES+=("${part[@]}")
    done
    mapfile -t FILES < <(printf '%s\n' "${FILES[@]}" | sort -u)

    # Announced on every run, pass or fail: a count is the only thing
    # that distinguishes "nothing said it" from "nothing was read".
    echo "check-retired-words: inspecting ${#FILES[@]} ${WORD_WHAT[$w]} for the word '$WORD'"

    for f in "${FILES[@]}"; do
        if idx=$(allow_file_index "$WORD" "$f"); then
            echo "ALLOWED $f (whole file, '$WORD') — ${ALLOW_FILE_REASONS[$idx]}"
            continue
        fi

        # The region case. Everything from the first line matching the
        # regex downwards is history; above it is living and is judged.
        if idx=$(allow_below_index "$WORD" "$f"); then
            re="${ALLOW_BELOW_RES[$idx]}"
            start=$(grep -n -E "$re" "$f" | head -1 | cut -d: -f1)
            if [ -z "$start" ]; then
                echo "::error title=check-retired-words refuses::$f has no line matching $re," \
                     "so the boundary between the living part and the history part cannot be found." >&2
                exit 2
            fi
            echo "ALLOWED $f (from line $start down, '$WORD') — ${ALLOW_BELOW_REASONS[$idx]}"
            hits=$(head -n "$((start - 1))" "$f" | grep -in -E "\\b${WORD}\\b" || true)
            if [ -n "$hits" ]; then
                echo "::error file=$f,title=$WORD above the history boundary::the word appears ABOVE the history boundary (line $start)" >&2
                printf '%s\n' "$hits" | sed "s|^|  $f:|" >&2
                fail=1
            else
                echo "PASS  $f (lines 1-$((start - 1)), '$WORD')"
            fi
            continue
        fi

        hits=$(grep -in -E "\\b${WORD}\\b" "$f" || true)
        if [ -n "$hits" ]; then
            echo "::error file=$f,title=$WORD on this branch::${WORD_REMEDY[$w]}" >&2
            printf '%s\n' "$hits" | sed "s|^|  $f:|" >&2
            fail=1
        fi
    done
done

if [ "$fail" -ne 0 ]; then
    echo "check-retired-words: FAILED" >&2
    exit 1
fi

echo "check-retired-words: clean across ${#WORDS[@]} word(s)"
