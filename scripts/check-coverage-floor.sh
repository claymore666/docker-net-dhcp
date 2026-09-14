#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Coverage-floor gate (#735): a PR may not lower a floor in
# .github/coverage-baseline.txt without saying so.
#
# Usage: check-coverage-floor.sh <commit-range> [pr-body-file]
#   <commit-range>: any git range, e.g. origin/dev..HEAD
#   [pr-body-file]: optional file holding the PR description
#
# Exit: 0 no floor lowered, or lowered and waived; 1 an unwaived
#       decrease; 2 cannot check.
#
# WHY THE RATCHET ALONE DOES NOT COVER THIS. coverage-ratchet.sh reads
# the baseline from the branch under test, so a PR that lowers a floor
# is measured against the number it just lowered — the comparison is
# with itself and it always passes. The only control was prose in that
# script's header ("lowering a floor requires a recorded decision in
# the PR"), enforced by nothing, and a paragraph is not enforcement.
#
# The second half of the gap is scheduling: coverage.yml runs only on
# PRs into main, so a floor lowered on the way into dev was measured by
# no coverage run at all, and the release PR then ratcheted against the
# already-lowered number. This gate is deliberately pure text over two
# git blobs — no counters, no instrumented plugin, no self-hosted
# runner — so it can run on EVERY pull request, including the ones into
# dev where the lowering would otherwise land unseen.
#
# THE WAIVER IS DELIBERATE AND CHEAP, BUT IT MUST BE DELIBERATE.
# Floors have moved down exactly once in fourteen commits (b7a720c,
# argued in-file, since repaid), so the answer here is not to forbid it.
# It is to make it a sentence someone wrote on purpose:
#
#     Coverage-floor: #123
#
# at column 0 in the PR body or a commit message in the range. Same
# shape as the `Test-weakening:` trailer and for the same reason: almost
# every commit here cites an issue, so a waiver that accepts any issue
# reference fires always and prevents nothing. This line is a statement
# about lowering a floor; citing the issue you are working on is not.
set -u

BASELINE_PATH=".github/coverage-baseline.txt"

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    echo "usage: $0 <commit-range> [pr-body-file]" >&2
    exit 2
fi

RANGE="$1"
BODY="${2-}"

case "$RANGE" in
    *..*) ;;
    *)
        echo "::error title=Cannot check coverage floors::'$RANGE' is not a <base>..<head> range." >&2
        exit 2
        ;;
esac

BASE_REF="${RANGE%%..*}"
HEAD_REF="${RANGE##*..}"
[ -n "$HEAD_REF" ] || HEAD_REF="HEAD"

if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null; then
    echo "::error title=Cannot check coverage floors::base ref '$BASE_REF' does not resolve." \
         "A gate that cannot see the base would otherwise pass having compared nothing." >&2
    exit 2
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# `git show` on a path that does not exist at that ref is an error, not
# an empty file, so the two cases are told apart rather than collapsed.
if ! git show "$BASE_REF:$BASELINE_PATH" > "$TMP/base.txt" 2>/dev/null; then
    echo "coverage-floor gate: $BASELINE_PATH does not exist at $BASE_REF — nothing to compare against."
    exit 0
fi

if ! git show "$HEAD_REF:$BASELINE_PATH" > "$TMP/head.txt" 2>/dev/null; then
    echo "::error title=Coverage baseline deleted::$BASELINE_PATH exists at $BASE_REF and not at $HEAD_REF." \
         "Deleting the baseline disables the ratchet outright." >&2
    exit 1
fi

# Same refusal the ratchet itself now makes (#734), applied here: a base
# baseline that parses to nothing means every floor reads as "not
# lowered" and the gate reports a clean pass having compared nothing.
floors() { # floors <file> <out>
    awk '
        /^[[:space:]]*#/ { next }
        NF == 2          { print $1, $2 }
    ' "$1" | sort > "$2"
}

floors "$TMP/base.txt" "$TMP/base.floors"
floors "$TMP/head.txt" "$TMP/head.floors"

if [ ! -s "$TMP/base.floors" ]; then
    echo "::error title=Nothing to inspect::$BASELINE_PATH at $BASE_REF holds no <package> <percent> lines." \
         "This gate would otherwise report a clean pass having compared nothing." >&2
    exit 2
fi

findings=0
report() {
    echo "FAIL  $1"
    findings=$((findings + 1))
}

# --- a floor removed WITH its package (2.0 M8b) ----------------------
#
# A removed floor is strictly weaker than a lowered one -- the ratchet
# stops judging the package altogether -- so the check above is right to
# treat it as a decrease. It has one blind spot, and 2.0 walks straight
# into it: `cmd/dhcp-handler` is DELETED, not merely unmeasured. A floor
# for a package that no longer exists cannot be honoured by anything,
# and leaving the row in is the worse of the two errors: the coverage
# job then either fails on a package it cannot find or, if it skips
# what it cannot find, carries a floor nothing will ever test again --
# a ratchet tooth with no gear behind it.
#
# So the question is asked of the TREE, not of a list of blessed
# packages: is there any .go file left under the package's directory at
# the head of the range? Derived, so it cannot go stale, and it answers
# for a package this gate has never heard of.
#
# THREE OUTCOMES, and the third is the one that keeps this from being
# an escape hatch. Gone -> announced and not a finding. Still present
# -> the original finding, unchanged. CANNOT TELL -> also a finding,
# said in those words. A gate that guesses "probably deleted" when it
# cannot resolve the module path would be switched off by a malformed
# go.mod, which is wrong in the direction of silence.
MODULE=$(git show "$HEAD_REF:go.mod" 2>/dev/null | awk '$1 == "module" { print $2; exit }')

package_state() { # <import path> -> prints gone | present | unknown
    local path="$1" dir
    if [ -z "$MODULE" ]; then echo unknown; return; fi
    case "$path" in
        "$MODULE")   dir="." ;;
        "$MODULE"/*) dir="${path#"$MODULE"/}" ;;
        # A floor on some other module's package is not something this
        # gate can resolve against this tree, and saying so is the
        # honest answer.
        *) echo unknown; return ;;
    esac
    # Read to EOF rather than `grep -q`: a short-circuiting consumer
    # SIGPIPEs its producer, and this repo has already shipped one
    # refusal that inverted that way (scripts/check-pipefail-consumers.sh).
    if git ls-tree -r --name-only "$HEAD_REF" -- "$dir" 2>/dev/null |
        grep -E '\.go$' >/dev/null; then
        echo present
    else
        echo gone
    fi
}

# --- A MAJOR-VERSION RENAME MOVES EVERY ROW ONE SEGMENT (#979) -------
#
# From v2 onward Go requires the major version in the module path, so
# the bump rewrites `module M` to `module M/vN` and every import with
# it. The baseline keys on import paths, so all of its rows move at
# once -- and each one then matches neither $MODULE nor $MODULE/*,
# lands in the "cannot tell" arm above, and the gate reports a finding
# per floor for a change in which no floor moved at all. Measured on
# the v2 rename: four findings, four floors, none of them lowered.
#
# THE RETRY IS NOT A BYPASS, and the shape is what keeps it from
# becoming one. It only re-SPELLS the row; the floor comparison that
# follows is the same one every other row gets, so a floor lowered
# under the new name still fails (self-test case 2). It fires only when
# the two go.mod files name the same module at different majors --
# both sides read, both stripped of a trailing /vN, and required to be
# equal -- so a row from some other module is untouched, and so is one
# from a tree that did not rename.
#
# The base module is read for the same reason the head one is: the row
# carries no marker saying which module it came from, and DERIVING the
# old prefix by stripping whatever the row happens to start with would
# match paths this tree never published.
BASE_MODULE=$(git show "$BASE_REF:go.mod" 2>/dev/null | awk '$1 == "module" { print $2; exit }')

unversioned() { # <module path> -> the path with a trailing /vN removed
    local last rest
    last="${1##*/}"
    case "$last" in
        v[0-9]*)
            # DIGITS ONLY. `v2beta` and `vendor` are ordinary last
            # segments; treating them as a major suffix would strip a
            # real directory off the module path.
            rest="${last#v}"
            case "$rest" in
                *[!0-9]*) printf '%s\n' "$1" ;;
                *)        printf '%s\n' "${1%/*}" ;;
            esac
            ;;
        *) printf '%s\n' "$1" ;;
    esac
}

renamed_path() { # <base import path> -> its head spelling, or nothing
    local tail
    [ -n "$MODULE" ] && [ -n "$BASE_MODULE" ] || return 0
    [ "$MODULE" != "$BASE_MODULE" ] || return 0
    [ "$(unversioned "$MODULE")" = "$(unversioned "$BASE_MODULE")" ] || return 0
    case "$1" in
        "$BASE_MODULE")   printf '%s\n' "$MODULE" ;;
        "$BASE_MODULE"/*) tail="${1#"$BASE_MODULE"/}"; printf '%s\n' "$MODULE/$tail" ;;
    esac
}

gone_count=0
renamed_count=0

while read -r pkg was; do
    now=$(awk -v p="$pkg" '$1 == p { print $2; exit }' "$TMP/head.floors")
    # The row may be present under the renamed module rather than
    # absent. Asked only after the row missed under its own name, so a
    # tree that did not rename runs exactly the code it ran before.
    renamed=""
    if [ -z "$now" ]; then
        renamed=$(renamed_path "$pkg")
        if [ -n "$renamed" ]; then
            now=$(awk -v p="$renamed" '$1 == p { print $2; exit }' "$TMP/head.floors")
            if [ -n "$now" ]; then
                renamed_count=$((renamed_count + 1))
                echo "note: $pkg is floored at $renamed after the module's major-version rename." \
                     "The floor is compared under the new name."
            else
                # A row that moved AND lost its floor is not something
                # this re-spelling can speak to: it falls through to
                # the arms below under its ORIGINAL name, where an
                # unresolvable path is reported rather than assumed.
                renamed=""
            fi
        fi
    fi
    if [ -z "$now" ]; then
        case "$(package_state "$pkg")" in
            gone)
                gone_count=$((gone_count + 1))
                echo "note: $pkg: floor ${was}% removed, and no .go file remains under that" \
                     "package at $HEAD_REF. The package is gone, so the row had nothing left" \
                     "to judge; keeping it would be a floor nothing can ever test."
                ;;
            unknown)
                report "$pkg: floor ${was}% removed, and this gate cannot tell whether the package still exists at $HEAD_REF (its module path did not resolve). Reported rather than assumed."
                ;;
            *)
                report "$pkg: floor ${was}% removed from $BASELINE_PATH — the ratchet no longer judges this package."
                ;;
        esac
        continue
    fi
    lowered=$(awk -v a="$was" -v b="$now" 'BEGIN { print (b + 0 < a + 0) ? "yes" : "no" }')
    if [ "$lowered" = "yes" ]; then
        if [ -n "$renamed" ]; then
            report "$renamed (was $pkg before the module's major-version rename): floor lowered ${was}% → ${now}%"
        else
            report "$pkg: floor lowered ${was}% → ${now}%"
        fi
    fi
done < "$TMP/base.floors"

# The emptied-domain case, which the note above would otherwise hide.
# If EVERY floor in the baseline was dropped because its package went
# away, this gate has just approved a tree it no longer measures
# anywhere -- and it would say "no floor lowered or removed", which
# reads as a clean ratchet rather than an absent one.
base_rows=$(wc -l < "$TMP/base.floors")
if [ "$gone_count" -ne 0 ] && [ "$gone_count" -eq "${base_rows:-0}" ]; then
    echo "::error title=Nothing left to ratchet::all $gone_count floor(s) were dropped" \
         "because their packages no longer exist at $HEAD_REF. There is no coverage floor" \
         "left in $BASELINE_PATH to judge anything by, and reporting that as 'no floor" \
         "lowered' would be a pass over a ratchet that is gone. This is a refusal." >&2
    exit 2
fi

if [ "$findings" -eq 0 ]; then
    # The counts are printed because a clean pass over rows that all
    # MOVED and a clean pass over rows that never moved are different
    # runs, and one sentence for both would make the re-spelling
    # invisible in the only output anyone reads when the gate is green.
    detail=""
    [ "$gone_count" -ne 0 ] && detail="$gone_count floor(s) dropped with their package"
    if [ "$renamed_count" -ne 0 ]; then
        [ -n "$detail" ] && detail="$detail; "
        detail="$detail$renamed_count floor(s) compared under the module's renamed path"
    fi
    if [ -n "$detail" ]; then
        echo "coverage-floor gate: no floor lowered or removed against $BASE_REF ($detail)"
        exit 0
    fi
    echo "coverage-floor gate: no floor lowered or removed against $BASE_REF"
    exit 0
fi

# ANCHORED AT COLUMN 0, WHICH IS NOT A DETAIL. Found by running this
# gate against its own branch: the commit body explaining the trailer
# quoted it as an indented example, and the gate read that as a waiver
# and passed a real lowered floor. A gate that any commit describing it
# can switch off is not a gate. A trailer is a trailer — column 0, its
# own line — so an indented or quoted mention is deliberately inert and
# the machinery can be written about without being invoked.
#
# `grep -E ... >/dev/null` rather than `grep -qE`: -q exits at the first
# match and SIGPIPEs `git log`, so under pipefail the pipeline reports
# failure exactly when the waiver was found. scripts/check-pipefail-consumers.sh
# gates this repo-wide.
TRAILER='^[Cc]overage-floor:[[:space:]]*#[0-9]+'
waiver=""
if [ -n "$BODY" ] && [ -f "$BODY" ] && grep -qE "$TRAILER" "$BODY"; then
    waiver="the PR body"
elif git log --format='%B' "$RANGE" 2>/dev/null | grep -E "$TRAILER" >/dev/null; then
    waiver="a commit message"
fi

if [ -n "$waiver" ]; then
    echo
    echo "coverage-floor gate: $findings finding(s), WAIVED by a Coverage-floor trailer in $waiver."
    exit 0
fi

cat <<EOF

::error title=Coverage floor lowered::$findings floor(s) in $BASELINE_PATH went down against $BASE_REF.

A floor is a claim that this package's coverage will not go backwards.
Lowering one is sometimes right — it happened once, deliberately, and was
repaid — but it is a decision, and the ratchet cannot see it because it
reads the baseline from this branch: it compares the new number against
the new number.

If the decrease is intended, file an issue saying why and add this line
to the PR body or a commit message in the range, at the start of its own
line:

Coverage-floor: #<issue>

`#<issue>` is a PLACEHOLDER, and it must stay one. This message is
printed at column 0 and gets pasted into pull request bodies, so a
worked example with a real number here would satisfy the very waiver
this paragraph is explaining — the column-0 anchor below would not
help, because the anchor is what a pasted column-0 line satisfies. The
sibling gate check-issue-ref.sh spelled its example out with a real
number and was switched off by its own failure text exactly that way.
This one has been safe only because `#<issue>` does not match
`#[0-9]+`, which was luck rather than design until someone wrote it
down. Do not "improve" it into a real number.

It has to be that line rather than any mention of an issue, because
almost every commit here cites one — a waiver that loose fires on
everything, and a gate that always waives itself prevents nothing. It
has to be at column 0 for the same reason one step further out: this
gate was first caught waiving itself on the commit that introduced it,
which quoted the trailer as an indented example.
EOF
exit 1
