#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The `healthy` contract must say the same thing in all four places
# that state it (#638).
#
# WHY THIS EXISTS
#
# `/Plugin.Health` returns one boolean an operator is expected to alert
# on, and which counters flip it is stated four times: the counter
# table's healthy-affecting column, the prose in the `healthy` row
# itself, the At a glance summary, and the Troubleshooting row an
# operator lands on when they have already seen `healthy: false`.
# v1.6.0 added a fourth counter (`address_conflicts`) to the code and to
# two of those statements. The `healthy` row kept listing three for two
# releases — and it is the one an operator reads first, because it is
# the row for the field they are asking about.
#
# THE FOURTH COPY. The gate originally guarded three of them, and the
# Troubleshooting row — the one reached by the operator who is already
# in trouble — named two of the four counters and sent every reader to
# check disk space, which is the remedy for exactly one of them. Same
# bug, one table further down the same file: a claim ABOUT the counters,
# stated in a place nothing read. Guarding three copies of a fact that
# exists in four is not a gate, it is a sample.
#
# Nothing could see it. check-docs-drift.sh reconciles the *set of
# fields* against the code's json tags, so every counter involved was
# present and documented; what drifted was a claim ABOUT them, which no
# gate reads. That gate's own header records the same class biting
# before (#345, a duplicated table that "told operators the wrong
# healthy contract") — the fix then was to delete the duplicate, which
# left three copies inside one file unguarded.
#
# WHAT IT CHECKS
#
#   1. the four doc statements name exactly the same counters
#   2. the number word in the summary matches how many that is
#   3. the code's Healthy expression has that many terms
#
# (3) is deliberately weak: it counts terms rather than resolving the
# locals in `failed == 0 && joinFails == 0 && ...` back to json names,
# which would mean tracking an assignment chain a gate has no business
# parsing. Adding or removing a counter changes the count, which is the
# edit that needs a human to look. Rewriting the expression into a
# shape this cannot parse is an explicit exit 2, never a pass — a gate
# that cannot see must not report clean.
#
# Usage: check-health-contract.sh [<reference-doc>] [<go-file>]
# Exit:  0 the four agree, 1 they disagree, 2 cannot check.
set -uo pipefail

DOC="${1:-docs/reference.md}"
SRC="${2:-pkg/plugin/endpoints.go}"

for f in "$DOC" "$SRC"; do
    [ -f "$f" ] || { echo "check-health-contract: $f does not exist" >&2; exit 2; }
done

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

# Every counter the table documents: the superset the three statements
# are allowed to draw from. Matched first so a claim can be judged
# against real counter names rather than against every backticked word.
all_counters=$(grep -oE '^\| `[a-z0-9_]+` \|' "$DOC" | tr -d '|` ' | sort -u)
if [ -z "$all_counters" ]; then
    echo "check-health-contract: no counter table rows found in $DOC" >&2
    exit 2
fi

# Backticked names on a line, kept only if they are counters.
counters_on() {
    printf '%s' "$1" | grep -oE '`[a-z0-9_]+`' | tr -d '`' \
        | grep -Fxv healthy | sort -u | comm -12 - <(printf '%s\n' "$all_counters")
}

# --- 1. the healthy-affecting column ----------------------------------
column_set=$(grep -oE '^\| `[a-z0-9_]+` \| \*{0,2}yes\*{0,2} \|' "$DOC" \
    | sed -E 's/^\| `([a-z0-9_]+)`.*/\1/' | sort -u)
[ -n "$column_set" ] || { echo "check-health-contract: no rows marked healthy-affecting in $DOC" >&2; exit 2; }

# --- 2. the prose in the `healthy` row --------------------------------
healthy_row=$(grep -E '^\| `healthy` \|' "$DOC" | head -1)
[ -n "$healthy_row" ] || { echo "check-health-contract: no \`healthy\` row in $DOC" >&2; exit 2; }
row_set=$(counters_on "$healthy_row")

# --- 3. the At a glance summary ---------------------------------------
glance=$(grep -E 'flip `healthy` to `false`' "$DOC" | head -1)
[ -n "$glance" ] || { echo "check-health-contract: no At-a-glance healthy summary in $DOC" >&2; exit 2; }
glance_set=$(counters_on "$glance")

# --- 4. the Troubleshooting row ---------------------------------------
# Keyed on the symptom cell, because that is what the row IS: the entry
# an operator finds by searching for what they just saw. Its cause cell
# has to name every counter that can produce that symptom — a cause list
# that names two of four sends three readers in four to the wrong
# remedy, and the reader who arrives here has already stopped reading
# the field table.
trouble=$(grep -E '^\|[[:space:]]*`healthy: false`' "$DOC" | head -1)
[ -n "$trouble" ] || { echo "check-health-contract: no \`healthy: false\` row in the Troubleshooting table of $DOC" >&2; exit 2; }
trouble_set=$(counters_on "$trouble")

if [ "$column_set" != "$row_set" ]; then
    note "the \`healthy\` row and the healthy-affecting column disagree:"
    diff <(printf '%s\n' "$column_set") <(printf '%s\n' "$row_set") \
        | sed 's/^</  only marked yes in the column: /; s/^>/  only named in the healthy row: /' >&2
fi
if [ "$column_set" != "$glance_set" ]; then
    note "the At a glance summary and the healthy-affecting column disagree:"
    diff <(printf '%s\n' "$column_set") <(printf '%s\n' "$glance_set") \
        | sed 's/^</  only marked yes in the column: /; s/^>/  only named in the summary: /' >&2
fi
if [ -z "$trouble_set" ]; then
    note "the Troubleshooting \`healthy: false\` row names no health counter at all — it must name every counter marked healthy-affecting:"
    echo "  $trouble" >&2
elif [ "$column_set" != "$trouble_set" ]; then
    note "the Troubleshooting \`healthy: false\` row and the healthy-affecting column disagree:"
    diff <(printf '%s\n' "$column_set") <(printf '%s\n' "$trouble_set") \
        | sed 's/^</  only marked yes in the column: /; s/^>/  only named in the troubleshooting row: /' >&2
fi

n_doc=$(printf '%s\n' "$column_set" | grep -c .)

# The summary opens with the count in words, which is exactly the part
# that survives an edit that adds a counter to the list below it.
word=$(printf '%s' "$glance" | grep -oE '[A-Za-z]+ flip `healthy`' | awk '{print tolower($1)}')
case "$word" in
    one) n_word=1 ;; two) n_word=2 ;; three) n_word=3 ;; four) n_word=4 ;;
    five) n_word=5 ;; six) n_word=6 ;; seven) n_word=7 ;; eight) n_word=8 ;;
    nine) n_word=9 ;;
    *) echo "check-health-contract: cannot read the count word in: $glance" >&2; exit 2 ;;
esac
[ "$n_word" = "$n_doc" ] && : || note "the summary says '$word' but $n_doc counters are marked healthy-affecting"

# --- 4. the code -------------------------------------------------------
expr=$(grep -hE '^[[:space:]]*Healthy:' "$SRC" | head -1)
[ -n "$expr" ] || { echo "check-health-contract: no Healthy: field assignment in $SRC" >&2; exit 2; }
rest=$(printf '%s' "$expr" | sed -E 's/^[[:space:]]*Healthy:[[:space:]]*//; s/,[[:space:]]*$//')
n_code=$(printf '%s' "$rest" | grep -oE '==[[:space:]]*0' | wc -l | tr -d ' ')
shape=$(printf '%s' "$rest" \
    | sed -E 's/[A-Za-z_][A-Za-z0-9_.()]*[[:space:]]*==[[:space:]]*0//g; s/&&//g; s/[[:space:]]//g')
if [ -n "$shape" ]; then
    echo "check-health-contract: unrecognised Healthy expression — cannot judge it:" >&2
    echo "  $rest" >&2
    echo "  Expected a conjunction of '<counter> == 0' terms. Teach this gate the" >&2
    echo "  new shape rather than letting it pass unread." >&2
    exit 2
fi
if [ "$n_code" != "$n_doc" ]; then
    note "the code's Healthy expression has $n_code term(s), the docs mark $n_doc counter(s):"
    echo "  $rest" >&2
fi

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "\`healthy\` is the one boolean operators alert on. All four doc" >&2
    echo "statements and the code must name the same counters — see the" >&2
    echo "header of this script for why the row is the one that rots." >&2
    exit 1
fi

echo "PASS  healthy contract agrees in 4 doc statements and ${n_code} code term(s): $(printf '%s' "$column_set" | tr '\n' ' ')"
exit 0
