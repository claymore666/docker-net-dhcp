#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every Go-shaped symbol RELEASE_NOTES.md names must exist in the tree.
#
# RELEASE_NOTES.md merges last and describes code that is still moving.
# The v1.8.0 PID-mismatch paragraph named `notePIDMismatch` and
# `TestNotePIDMismatch_CountsTheEffect`; the only file in the repository
# containing either string was the release notes claiming they ran. The
# code they described was real -- it had been renamed on its way to dev,
# and nothing re-read the prose afterwards.
#
# Every derivation already in that file re-derives a COUNT or a SET.
# None re-derives a symbol NAME, and that is exactly where the fiction
# accumulated. This closes that axis and nothing else.
#
# COMMENTS AND STRING LITERALS ARE STRIPPED BEFORE RESOLUTION, AND THAT
# IS THE POINT. A plain grep resolves a symbol against prose ABOUT the
# symbol. Measured: `LastIP` and `lastIP` are the same field, unexported
# since 9c46e87, and `LastIP` matched a tracked .go file anyway -- on a
# single code comment at dhcp_manager.go:1090. A gate that reads
# comments scores a symbol as existing because somebody mentioned it.
# It still cannot tell a declaration from a use, and does not try to:
# the claim being checked is "this symbol is in the tree", not "this
# symbol is declared here".
#
# THE RESOLVED SHA IS PRINTED ON EVERY LINE, PASS OR FAIL. A symbol
# claim is a function of the tree. Four people once produced four
# correct and mutually incompatible answers about this very paragraph
# inside an hour, because none of them wrote down which tree they had
# asked. A verdict with no SHA has the defect it exists to prevent, and
# a green with no SHA is unfalsifiable reassurance.
#
# Usage: check-release-notes-symbols.sh [notes-file]
# Env:   SYMBOL_WAIVERS  waiver file (default .github/release-notes-symbol-waivers.txt)
#        SYMBOL_SOURCES  space-separated globs to resolve against
#                        (default: the tracked *.go files) -- the seam
#                        the self-test drives.
# Exit:  0 every symbol resolves or is waived, 1 one or more do not,
#        2 cannot check.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
NOTES="${1:-$ROOT/RELEASE_NOTES.md}"
WAIVERS="${SYMBOL_WAIVERS:-$ROOT/.github/release-notes-symbol-waivers.txt}"

if [ ! -f "$NOTES" ]; then
    echo "::error title=Release notes missing::$NOTES is not a file" >&2
    exit 2
fi

# The tree this verdict is about. A detached or unborn HEAD is not a
# reason to skip -- it is a reason to say so and still resolve.
SHA=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null) || SHA=""
[ -z "$SHA" ] && SHA="unknown-tree"

# ---------------------------------------------------------------- corpus
# Resolve against tracked Go files, with comments and string literals
# blanked. `git ls-files` rather than a find, so an untracked scratch
# copy of a renamed file cannot satisfy the gate.
if [ -n "${SYMBOL_SOURCES:-}" ]; then
    # shellcheck disable=SC2086
    mapfile -t GOFILES < <(ls -1 $SYMBOL_SOURCES 2>/dev/null)
else
    mapfile -t GOFILES < <(git -C "$ROOT" ls-files '*.go' 2>/dev/null | sed "s|^|$ROOT/|")
fi

if [ "${#GOFILES[@]}" -eq 0 ]; then
    echo "::error title=No Go sources to resolve against::the symbol corpus is empty at ${SHA}." \
         "This check would otherwise pass every symbol by having nothing to contradict it." >&2
    exit 2
fi

CORPUS=$(mktemp) || exit 2
trap 'rm -f "$CORPUS"' EXIT

# Blank comments and literals, then keep identifier-shaped words.
#
# ONE left-to-right alternation, not a sequence of passes: whichever
# construct opens first consumes its own closer, so a `//` inside a
# string stays part of the string and a quote inside a comment stays
# part of the comment. Two passes get that wrong in both directions --
# stripping `//` first turns "http://x" into an unterminated literal.
#
# AND ONE FILE AT A TIME. Slurping every .go file into a single perl
# -0777 string lets an unterminated construct in one file swallow code
# out of the next. The first version of this did that and called 49 of
# 97 symbols unresolvable, `propagateDNS` and `ipMu` among them -- a
# corpus that quietly lost half the tree looks exactly like prose that
# quietly went stale.
STRIP='
    s{
        \x27 (?: \\. | [^\x27\\] )* \x27
      | " (?: \\. | [^"\\\n] )* "
      | ` [^`]* `
      | // [^\n]*
      | /\* .*? \*/
    }{ }gsx;
'
for f in "${GOFILES[@]}"; do
    [ -f "$f" ] || continue
    perl -0777 -pe "$STRIP" "$f"
done | grep -oE '[A-Za-z_][A-Za-z0-9_]*' | sort -u > "$CORPUS"

if [ ! -s "$CORPUS" ]; then
    echo "::error title=Empty symbol corpus::stripping produced no identifiers at ${SHA}." \
         "Something is wrong with this check, not with the notes." >&2
    exit 2
fi

# --------------------------------------------------------------- waivers
declare -A WAIVED=()
if [ -f "$WAIVERS" ]; then
    while IFS= read -r line; do
        case "$line" in ''|'#'*) continue ;; esac
        WAIVED["${line%%[[:space:]]*}"]=1
    done < "$WAIVERS"
fi

# --------------------------------------------------------------- candidates
# Backticked tokens that look like Go identifiers: an inner lower->upper
# transition, which is what distinguishes `notePIDMismatch` from `bridge`
# or `renew`. Trailing () and a leading receiver are trimmed.
mapfile -t CANDIDATES < <(
    grep -oE '`[A-Za-z_][A-Za-z0-9_.]*(\(\))?`' "$NOTES" |
        tr -d '`' | sed -e 's/()$//' -e 's/^.*\.//' |
        grep -E '^[A-Za-z_][A-Za-z0-9_]*$' |
        grep -E '[a-z][A-Z]' | sort -u
)

echo "Release-notes symbols at ${SHA}: ${#CANDIDATES[@]} candidate(s) against ${#GOFILES[@]} Go file(s)."

missing=()
waived_used=()
for sym in "${CANDIDATES[@]}"; do
    if grep -qxF "$sym" "$CORPUS"; then
        continue
    fi
    if [ -n "${WAIVED[$sym]:-}" ]; then
        waived_used+=("$sym")
        continue
    fi
    missing+=("$sym")
done

if [ "${#waived_used[@]}" -ne 0 ]; then
    echo "Waived at ${SHA} (see $(basename "$WAIVERS")):"
    printf '  %s\n' "${waived_used[@]}"
fi

if [ "${#missing[@]}" -ne 0 ]; then
    echo >&2
    for sym in "${missing[@]}"; do
        line=$(grep -n -m1 -F "\`${sym}\`" "$NOTES" | cut -d: -f1)
        echo "::error file=$(basename "$NOTES"),line=${line:-1},title=Release notes name a symbol the tree does not define::" \
             "\`${sym}\` does not resolve in any tracked Go file at ${SHA}." \
             "Either the prose is stale, or the symbol was renamed and the notes were not re-read." \
             "If it is deliberately historical or external, add it to $(basename "$WAIVERS") WITH a reason." >&2
    done
    echo >&2
    echo "${#missing[@]} unresolved symbol(s) at ${SHA}: ${missing[*]}" >&2
    exit 1
fi

echo "All ${#CANDIDATES[@]} release-notes symbol(s) resolve at ${SHA} (${#waived_used[@]} waived)."
