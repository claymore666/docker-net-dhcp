#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The `healthy` contract must say the same thing in every place that
# states it (#638).
#
# WHY THIS EXISTS
#
# `/Plugin.Health` returns one boolean an operator is expected to alert
# on, and which counters flip it is restated all over the reference: the
# counter table's healthy-affecting column, the prose in the `healthy`
# row itself, the At a glance summary, and the Troubleshooting row an
# operator lands on when they have already seen `healthy: false`. Three
# of those also carry the count in words.
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
# This gate therefore states no total of its own. It counts the
# statements it actually read and prints that in the PASS line — a
# hardcoded "all four" in here is the same rot one file over, and this
# header carried one until #724 added a fifth counter and two more
# count-word checks.
#
# WHAT IT CHECKS
#
#   1. each doc statement it knows about lists the same counters
#   2. each doc statement it knows about that carries the count in
#      words carries the count that list actually has
#   3. the code's Healthy expression -- the only one in the file -- has
#      that many terms
#
# WHAT IT CANNOT CHECK, stated because the PASS line looks total and is
# not: it reads statements it was TAUGHT, by pattern, in ONE file. A
# sixth statement added anywhere -- a new row, a new page, README,
# SECURITY.md, a release note -- is invisible to it and will read as
# covered, because the tally above will still print and still be
# right about what it read. The tally is a receipt, not a proof of
# completeness. Adding a statement about which counters flip `healthy`
# means adding it here too.
#
# (3) is deliberately weak: it counts terms rather than resolving the
# locals in `failed == 0 && joinFails == 0 && ...` back to json names,
# which would mean tracking an assignment chain a gate has no business
# parsing. Adding or removing a counter changes the count, which is the
# edit that needs a human to look. Rewriting the expression into a
# shape this cannot parse is an explicit exit 2, never a pass — a gate
# that cannot see must not report clean.
#
# TWO MORE CODE SURFACES RESTATE THE SAME SET, and neither was read.
#
#   pkg/plugin/metrics.go -- every healthy-affecting counter's help
#   string ends "Healthy-affecting.", and that sentence is served to
#   operators on /metrics. The exposition golden is REGENERATED from
#   these strings, so it agrees with whatever they say and proves
#   nothing about whether they are true; check-docs-drift.sh reconciles
#   the SET OF FIELD NAMES, not the sentence. #709 already shipped a
#   /metrics warning naming data it did not carry -- the failure mode
#   is not hypothetical, it is one release old.
#
#   test/integration/harness/healthfloor.go -- floorCounters is the
#   list the integration health floor fails a run over. Its own type
#   comment calls it "one healthy-affecting counter", so equality with
#   the doc column is the intent already written down. Nothing enforced
#   it, and a new healthy-affecting counter could ship with no
#   integration run watching it while every file above agreed.
#
# Both are the #638 shape one file over: a claim ABOUT the counters,
# restated somewhere nothing reads. They are checked here rather than
# in a new gate because this is the gate that owns the question.
#
# Usage: check-health-contract.sh [<reference-doc>] [<endpoints.go>]
#                                 [<metrics.go>] [<healthfloor.go>]
# Exit:  0 they all agree, 1 they disagree, 2 cannot check.
set -uo pipefail

DOC="${1:-docs/reference.md}"
SRC="${2:-pkg/plugin/endpoints.go}"
METRICS="${3:-pkg/plugin/metrics.go}"
FLOOR="${4:-test/integration/harness/healthfloor.go}"

for f in "$DOC" "$SRC" "$METRICS" "$FLOOR"; do
    [ -f "$f" ] || { echo "check-health-contract: $f does not exist" >&2; exit 2; }
done

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

# Tallies for the PASS line. A gate that hardcodes how much it covers is
# the same rot it exists to catch, one file over: #638 shipped because a
# comment said three copies and there were four. These count reads.
n_lists=0
n_words=0

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
n_lists=$((n_lists + 1))

# --- 2. the prose in the `healthy` row --------------------------------
healthy_row=$(grep -E '^\| `healthy` \|' "$DOC" | head -1)
[ -n "$healthy_row" ] || { echo "check-health-contract: no \`healthy\` row in $DOC" >&2; exit 2; }
n_lists=$((n_lists + 1))
row_set=$(counters_on "$healthy_row")

# --- 3. the At a glance summary ---------------------------------------
glance=$(grep -E 'flip `healthy` to `false`' "$DOC" | head -1)
[ -n "$glance" ] || { echo "check-health-contract: no At-a-glance healthy summary in $DOC" >&2; exit 2; }
n_lists=$((n_lists + 1))
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
n_lists=$((n_lists + 1))
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
#
# FIVE STATEMENTS CARRY THE NUMBER, NOT ONE. The check started on the
# At-a-glance line only, and that left the same bug it was written to
# stop: the Troubleshooting row opens "Exactly four counters flip it"
# and the preamble says "which four flip `healthy`", and NEITHER was
# read. A fifth counter added correctly to all four name-lists still
# shipped two rows saying "four" over a list of five — including the row
# an operator reaches after they have already seen `healthy: false`,
# which is the exact reader #638 was about (#724).
#
# Counting the statements is itself the trap this gate is about, so it
# was done by reading the file rather than from memory: `healthy` row
# ("Those five, and only those") and the Troubleshooting row's second
# sentence ("Read the five in the field table above") each carry the
# number a second time, in the same two rows whose NAME LISTS were
# already guarded. Guarding a row's list and not its count is the same
# sampling error as guarding three copies of a claim that exists in
# four — the row still ships a sentence that contradicts the list
# directly above it.
#
# THE COST, stated so nobody has to rediscover it: five pinned prose
# patterns are five ways for an honest rewording to exit 2. That is the
# intended trade. A reword of a sentence stating how many counters flip
# the one boolean operators alert on is exactly the edit that should
# stop and be looked at, and the remedy is one line in the list below,
# not a waiver.
word_to_n() {
    case "$1" in
        one) echo 1 ;; two) echo 2 ;; three) echo 3 ;; four) echo 4 ;;
        five) echo 5 ;; six) echo 6 ;; seven) echo 7 ;; eight) echo 8 ;;
        nine) echo 9 ;;
        *) echo "" ;;
    esac
}

# Each entry: a label, the line, and the regex that pulls the count word
# out of it. A line that matches nothing is exit 2, never a pass — a
# gate that cannot see must not report clean.
check_word() {
    label=$1; line=$2; pattern=$3
    w=$(printf '%s' "$line" | grep -oE "$pattern" | head -1 | grep -oE '^[A-Za-z]+' | tr '[:upper:]' '[:lower:]')
    n=$(word_to_n "$w")
    if [ -z "$n" ]; then
        echo "check-health-contract: cannot read the count word in the $label: $line" >&2
        exit 2
    fi
    n_words=$((n_words + 1))
    [ "$n" = "$n_doc" ] || note "the $label says '$w' but $n_doc counters are marked healthy-affecting"
}

check_word "At a glance summary" "$glance" '[A-Za-z]+ flip `healthy`'

# The Troubleshooting row: "Exactly four counters flip it".
check_word "Troubleshooting \`healthy: false\` row" \
    "$(printf '%s' "$trouble" | sed -E 's/.*[Ee]xactly //')" '[A-Za-z]+ counters flip'

# The preamble: "which four flip `healthy`".
preamble=$(grep -E 'which [a-z]+ flip `healthy`' "$DOC" | head -1)
[ -n "$preamble" ] || { echo "check-health-contract: no 'which N flip \`healthy\`' preamble in $DOC" >&2; exit 2; }
check_word "preamble" "$(printf '%s' "$preamble" | sed -E 's/.*which //')" '[A-Za-z]+ flip `healthy`'

# The `healthy` row's own count: "Those four, and only those, are the
# ones marked yes in this column." Its NAME LIST is checked above; the
# sentence restating how long that list is was not, so the row could
# name five counters and then tell the reader there are four, one
# sentence later, in the row #638 exists because of.
check_word "\`healthy\` row" "$(printf '%s' "$healthy_row" | sed -E 's/.*[Tt]hose //')" '[A-Za-z]+, and only those'

# And the Troubleshooting row's second count: "Read the four in the
# field table above". The remedy sentence, not the cause sentence — an
# operator who reads "read the four" over a list of five stops looking
# after the fourth, which is a wrong remedy rather than a cosmetic
# disagreement.
check_word "Troubleshooting remedy sentence" \
    "$(printf '%s' "$trouble" | sed -E 's/.*Read the //')" '[A-Za-z]+ in the field table'

# --- 4. the code -------------------------------------------------------
# EXACTLY ONE, never `head -1`. The pattern is not guaranteed unique:
# a second HealthResponse literal added above the real one -- a
# degraded-mode response, a test helper that migrates in -- would
# silently become the thing judged, and the expression that actually
# ships would stop being read while this still printed PASS. Same rule
# as everywhere else in here: a gate that cannot tell which line it is
# looking at must exit 2, not pick one (#724).
n_expr=$(grep -hcE '^[[:space:]]*Healthy:' "$SRC")
if [ "$n_expr" != "1" ]; then
    echo "check-health-contract: expected exactly one 'Healthy:' assignment in $SRC, found $n_expr" >&2
    grep -hnE '^[[:space:]]*Healthy:' "$SRC" >&2
    echo "  With more than one, this gate cannot know which expression ships." >&2
    echo "  With none, there is nothing to judge. Teach it which is which" >&2
    echo "  rather than letting it read the first and report clean." >&2
    exit 2
fi
expr=$(grep -hE '^[[:space:]]*Healthy:' "$SRC")
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

# --- 5. the /metrics help strings --------------------------------------
# Operator-facing text, served on the wire. Name and help sit on one
# line in metricDefs, so this reads that line. If the shape ever
# changes the set comes back empty, which is exit 2 -- a gate reading a
# file it no longer understands must not print PASS over it.
metrics_set=$(grep -F 'Healthy-affecting.' "$METRICS" \
    | grep -oE 'name:[[:space:]]*"[a-z0-9_]+"' \
    | sed -E 's/.*"([a-z0-9_]+)".*/\1/' | sort -u)
if [ -z "$metrics_set" ]; then
    echo "check-health-contract: no metric help string in $METRICS ends 'Healthy-affecting.'" >&2
    echo "  Either the sentence was dropped from every counter, or metricDefs no" >&2
    echo "  longer puts name and help on one line and this check has gone blind." >&2
    echo "  Teach it the new shape rather than letting it pass unread." >&2
    exit 2
fi
if [ "$column_set" != "$metrics_set" ]; then
    note "the /metrics help strings and the healthy-affecting column disagree:"
    diff <(printf '%s\n' "$column_set") <(printf '%s\n' "$metrics_set") \
        | sed 's/^</  marked yes in the doc, but its help string does not say "Healthy-affecting.": /; s/^>/  its help string says "Healthy-affecting.", but the doc does not mark it: /' >&2
fi
n_metrics=$(printf '%s\n' "$metrics_set" | grep -c .)

# --- 6. the integration health floor -----------------------------------
# floorCounters is what an integration run FAILS over. A healthy-
# affecting counter missing from it ships with nothing watching it, and
# the file reads as complete either way.
floor_set=$(awk '
    /^var floorCounters = \[\]floorCounter\{/ { inblock = 1; next }
    inblock && /^\}/                          { inblock = 0 }
    inblock && /name:[[:space:]]*"/ {
        line = $0
        sub(/.*name:[[:space:]]*"/, "", line)
        sub(/".*/, "", line)
        print line
    }
' "$FLOOR" | sort -u)
if [ -z "$floor_set" ]; then
    echo "check-health-contract: no floorCounters entries found in $FLOOR" >&2
    echo "  The var block was not recognised, so this check read nothing." >&2
    exit 2
fi
if [ "$column_set" != "$floor_set" ]; then
    note "the integration health floor and the healthy-affecting column disagree:"
    diff <(printf '%s\n' "$column_set") <(printf '%s\n' "$floor_set") \
        | sed 's/^</  marked yes in the doc, but no integration run watches it: /; s/^>/  in floorCounters, but not marked healthy-affecting: /' >&2
fi

# Every entry fatal, which is what the floor MEANS. A healthy-affecting
# counter present but non-fatal is watched and then waved through, which
# reads greener than being absent.
n_floor=$(printf '%s\n' "$floor_set" | grep -c .)
n_fatal=$(awk '
    /^var floorCounters = \[\]floorCounter\{/ { inblock = 1; next }
    inblock && /^\}/                          { inblock = 0 }
    inblock && /fatal:[[:space:]]*true/       { n++ }
    END { print n + 0 }
' "$FLOOR")
if [ "$n_fatal" != "$n_floor" ]; then
    note "floorCounters has $n_floor entr(ies) but only $n_fatal marked fatal — a healthy-affecting counter the floor does not fail on is watched and waved through"
fi

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "\`healthy\` is the one boolean operators alert on. Every doc" >&2
    echo "statement and the code must name the same counters, and every" >&2
    echo "statement that carries the count must carry the right one — see" >&2
    echo "the header of this script for why the row is the one that rots." >&2
    exit 1
fi

echo "PASS  healthy contract agrees in ${n_lists} doc counter-list(s), ${n_words} doc count-word(s), ${n_code} code term(s), ${n_metrics} /metrics help string(s) and ${n_floor} integration floor entr(ies): $(printf '%s' "$column_set" | tr '\n' ' ')"
exit 0
