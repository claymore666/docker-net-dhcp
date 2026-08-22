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
# THE WAIVER FILE STATES THREE RULES AND THE FIRST VERSION OF THIS GATE
# ENFORCED NONE OF THEM. That is this gate's own defect wearing this
# gate's own subject: a check that exists because prose decays, with its
# rules living in prose that nothing read. The parser took
# `${line%%[[:space:]]*}` and dropped everything after the first field;
# the waiver map was consulted only AFTER resolution failed, so a waiver
# for a symbol that resolves was never read; and `WAIVED` was a flat map
# with nowhere for a section to go. All three are now decided on rather
# than described, except the one that cannot be:
#
#   ENFORCED   an entry belongs to a reason paragraph, and that
#              paragraph opens with HISTORICAL or EXTERNAL.
#   ENFORCED   a waiver whose symbol resolves, or that the notes no
#              longer mention at all, is stale and fails.
#   ENFORCED   a symbol in the CURRENT release section cannot be waived.
#   NOT ENFORCED, AND SAID SO OUT LOUD: whether the reason is a REASON.
#              "renamed" is a word, not an argument. No parser can tell
#              a decision from a shrug, and a gate that appears to check
#              something it cannot is worse than one that admits the
#              gap -- it converts a human review into a green tick.
#              The category and the paragraph are mechanical; the
#              content is reviewed by a person, on purpose.
#
# A STALE WAIVER FAILS HERE WHERE THE VULN ALLOWLIST ONLY WARNS
# (govulncheck-gate.sh:59), and the divergence is deliberate. That
# allowlist goes stale when a REMOTE database changes, which can happen
# between two runs over an identical tree and is nobody's commit to fix;
# warning is right there. A symbol waiver goes stale only when someone
# edits THIS tree, in the pull request that is being checked, so the
# person who made it stale is present and the fix is one line.
#
# DO NOT HARMONISE THE TWO. They share the word "stale" and nothing
# else: what decides warn-versus-fail is whether the person seeing the
# red can act on it. A govulncheck red for a moved database is a false
# alarm charged to whoever pushed next; a stale waiver here is a note
# addressed to its own author. Parity would be parity of vocabulary,
# not of meaning -- and harmonising DOWNWARD, to a warning, costs
# nothing to do and everything to notice.
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
# An entry is a run of token lines introduced by a CATEGORY LINE -- a
# comment whose text begins `HISTORICAL` or `EXTERNAL`. Tokens in one
# run share it, which is how `LastIP` and `LastIPv6` are one decision
# and not two, and the category is consumed when the run opens so the
# next run needs its own.
#
# NOT "the paragraph immediately above", WHICH IS WHAT I WROTE FIRST AND
# THE FILE ITSELF REFUTED. A bare `#` in this file is a blank line
# INSIDE a reason as often as it is a separator between reasons: the
# LastIP entry has a second paragraph about why this check strips
# comments, so the run directly above the token opens with "Worth
# recording", and a rule keyed on adjacency called the file's most
# carefully written entry a bare token. The category, not the position,
# is what marks an entry -- and one space after the `#`, so the rules in
# the file header (indented seven) cannot pose as one.
declare -A WAIVED=()
declare -A WAIVED_CAT=()
bad_waivers=()
if [ -f "$WAIVERS" ]; then
    pending_cat=""
    group_cat=""
    in_group=0
    lineno=0
    while IFS= read -r line || [ -n "$line" ]; do
        lineno=$((lineno + 1))
        case "$line" in
            ''|'#'*)
                # Anything that is not a token ends the current run, so
                # a token appended below a finished entry cannot inherit
                # the reason that entry was given.
                in_group=0
                case "$line" in
                    '# HISTORICAL'*) pending_cat="HISTORICAL" ;;
                    '# EXTERNAL'*)   pending_cat="EXTERNAL" ;;
                esac
                continue
                ;;
        esac
        if [ "$in_group" -eq 0 ]; then
            group_cat="$pending_cat"
            pending_cat=""
            in_group=1
        fi
        sym="${line%%[[:space:]]*}"
        if [ -z "$group_cat" ]; then
            bad_waivers+=("${lineno}:${sym}")
            continue
        fi
        WAIVED["$sym"]=1
        WAIVED_CAT["$sym"]="$group_cat"
    done < "$WAIVERS"
fi

# ------------------------------------------------------- current section
# The notes are newest-first, so the section being written is the first
# `## vX.Y.Z` heading. A waiver cannot reach into it: if a symbol named
# there does not resolve, the prose is wrong and the remedy is to fix
# the prose. That is the one rule of the three that a flat waiver map
# could not express at all -- there was nowhere for a section to go.
CUR_START=$(awk '/^## v[0-9]/ {print NR; exit}' "$NOTES")
if [ -n "$CUR_START" ]; then
    CUR_END=$(awk -v a="$CUR_START" 'NR > a && /^## / {print NR; exit}' "$NOTES")
    [ -z "$CUR_END" ] && CUR_END=$(($(wc -l < "$NOTES") + 1))
else
    CUR_END=""
fi

# sym_lines prints the line numbers where the notes name a symbol in a
# backticked span, matching the same shapes the candidate extraction
# accepts: a bare name, a trailing (), and a receiver prefix.
sym_lines() {
    awk -v s="$1" '
        BEGIN { re = "`([A-Za-z_][A-Za-z0-9_]*\\.)?" s "(\\(\\))?`" }
        $0 ~ re { print NR }
    ' "$NOTES"
}

in_current_section() {
    local l
    [ -n "$CUR_START" ] || return 1
    for l in $(sym_lines "$1"); do
        if [ "$l" -ge "$CUR_START" ] && [ "$l" -lt "$CUR_END" ]; then
            return 0
        fi
    done
    return 1
}

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
stale_resolving=()
current_waived=()
for sym in "${CANDIDATES[@]}"; do
    if grep -qxF "$sym" "$CORPUS"; then
        # RESOLVES. The old gate stopped here, which is why a waiver
        # for a resolving symbol was never read: the map was consulted
        # only on the failure path, so the one entry that can never
        # fire is the one nothing could see.
        if [ -n "${WAIVED[$sym]:-}" ]; then
            stale_resolving+=("$sym")
        fi
        continue
    fi
    if [ -n "${WAIVED[$sym]:-}" ]; then
        if in_current_section "$sym"; then
            current_waived+=("$sym")
        else
            waived_used+=("$sym")
        fi
        continue
    fi
    missing+=("$sym")
done

# A waiver for a symbol the notes no longer mention is the other half of
# stale, and it is the quieter one: nothing resolves it, nothing fails,
# and it sits there covering a sentence that was deleted.
stale_absent=()
for sym in "${!WAIVED[@]}"; do
    found=0
    for cand in "${CANDIDATES[@]}"; do
        if [ "$cand" = "$sym" ]; then found=1; break; fi
    done
    [ "$found" -eq 0 ] && stale_absent+=("$sym")
done

if [ "${#waived_used[@]}" -ne 0 ]; then
    echo "Waived at ${SHA} (see $(basename "$WAIVERS")):"
    for sym in "${waived_used[@]}"; do
        echo "  ${sym} (${WAIVED_CAT[$sym]})"
    done
fi

fail=0

if [ "${#bad_waivers[@]}" -ne 0 ]; then
    echo >&2
    for entry in "${bad_waivers[@]}"; do
        echo "::error file=$(basename "$WAIVERS"),line=${entry%%:*},title=Waiver with no reason::" \
             "\`${entry#*:}\` is a bare token. An entry belongs to a comment paragraph" \
             "opening with HISTORICAL or EXTERNAL. The gate checks that the paragraph is" \
             "there and which category it claims; whether the reason is a REASON is" \
             "reviewed by a person, and a bare token gives them nothing to review." >&2
    done
    fail=1
fi

if [ "${#current_waived[@]}" -ne 0 ]; then
    echo >&2
    for sym in "${current_waived[@]}"; do
        line=$(sym_lines "$sym" | awk 'NR == 1 { print }')
        echo "::error file=$(basename "$NOTES"),line=${line:-1},title=A current release section cannot be waived::" \
             "\`${sym}\` is named in the section being written and does not resolve at ${SHA}," \
             "and a waiver does not reach it. A frozen section describes the tree as it was;" \
             "this one describes the tree as it is, so the prose is simply wrong." >&2
    done
    fail=1
fi

if [ "${#stale_resolving[@]}" -ne 0 ] || [ "${#stale_absent[@]}" -ne 0 ]; then
    echo >&2
    for sym in "${stale_resolving[@]}"; do
        echo "::error file=$(basename "$WAIVERS"),title=Stale waiver::" \
             "\`${sym}\` resolves in the tree at ${SHA}, so its waiver never fires." \
             "Remove it. A waiver that cannot fire is a hole: the day the symbol goes" \
             "away again, this gate stays silent and nobody chose that." >&2
    done
    for sym in "${stale_absent[@]}"; do
        echo "::error file=$(basename "$WAIVERS"),title=Stale waiver::" \
             "\`${sym}\` is not named by $(basename "$NOTES") at ${SHA}. The sentence it" \
             "covered is gone; the waiver outlived it. Remove it." >&2
    done
    fail=1
fi

if [ "${#missing[@]}" -ne 0 ]; then
    echo >&2
    for sym in "${missing[@]}"; do
        line=$(sym_lines "$sym" | awk 'NR == 1 { print }')
        echo "::error file=$(basename "$NOTES"),line=${line:-1},title=Release notes name a symbol the tree does not define::" \
             "\`${sym}\` does not resolve in any tracked Go file at ${SHA}." \
             "Either the prose is stale, or the symbol was renamed and the notes were not re-read." \
             "If it is deliberately historical or external, add it to $(basename "$WAIVERS") WITH a reason." >&2
    done
    echo >&2
    echo "${#missing[@]} unresolved symbol(s) at ${SHA}: ${missing[*]}" >&2
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi

echo "All ${#CANDIDATES[@]} release-notes symbol(s) resolve at ${SHA} (${#waived_used[@]} waived)."
