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
#   4. the CHECK column of the same table agrees with metricDefs'
#      `healthy:` / `warn:` declarations, in both directions
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
#   string ASSERTS the property in prose, and that sentence is served to
#   operators on /metrics. The assertion is read by meaning and not by
#   spelling (#826): case and trailing punctuation do not matter, and a
#   NEGATED mention counts as a denial rather than an assertion. The exposition golden is REGENERATED from
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

# --- 4b. the check classification column -------------------------------
# /Plugin.Health's `status` and `checks` are derived from metricDefs'
# `healthy` and `warn` declarations, and the reference table states the
# same classification in a column an operator reads. Same #638 shape as
# everything else in this file: one fact, two places.
#
# The fail half is redundant with the healthy-affecting column by
# construction and is checked anyway -- a table whose two columns
# disagree about the same counter is a table a reader cannot use, and
# the redundancy is only free while something reads it.
#
# Both directions. A counter marked `warn` in the doc and declared
# nowhere is a check an operator is told to expect and will never see;
# a counter declared `warn: true` and left off the table is a check that
# appears in the document with nothing explaining it.
doc_fail=$(grep -oE '^\| `[a-z0-9_]+` \| [^|]* \| fail \|' "$DOC" \
    | sed -E 's/^\| `([a-z0-9_]+)`.*/\1/' | sort -u)
doc_warn=$(grep -oE '^\| `[a-z0-9_]+` \| [^|]* \| warn \|' "$DOC" \
    | sed -E 's/^\| `([a-z0-9_]+)`.*/\1/' | sort -u)
if [ -z "$doc_warn" ]; then
    echo "check-health-contract: no rows classified 'warn' in $DOC" >&2
    echo "  Two readings, and this cannot tell them apart, so it refuses" >&2
    echo "  rather than reporting clean: either the check column was" >&2
    echo "  dropped or moved and this read nothing, or every warn" >&2
    echo "  classification was genuinely removed -- which is a change to" >&2
    echo "  the health document's shape and wants a human either way." >&2
    exit 2
fi
n_checks=$(printf '%s\n%s\n' "$doc_fail" "$doc_warn" | grep -c .)

# Declared on the metricDef, keyed on the FIELD (the json tag the
# document renders) rather than on the metric name, because that is what
# the check is keyed on in the document. The leading non-word character
# is load-bearing: `v4field:`/`v6field:` sit on the same line for the
# family-split metrics, and a bare `field:` collects the halves as
# though each were its own check.
code_class() {
    awk -v want="$1" '
        /field:[[:space:]]*"/ && $0 ~ want { print }
    ' "$METRICS" \
        | grep -oE '[^a-z0-9_]field:[[:space:]]*"[a-z0-9_]+"' \
        | sed -E 's/.*"([a-z0-9_]+)".*/\1/' | sort -u
}
code_fail=$(code_class 'healthy:[[:space:]]*true')
code_warn=$(code_class 'warn:[[:space:]]*true')
if [ -z "$code_warn" ]; then
    echo "check-health-contract: no metricDef in $METRICS declares warn: true" >&2
    echo "  Teach this check the new shape rather than letting it pass unread." >&2
    exit 2
fi

if [ "$doc_fail" != "$code_fail" ]; then
    note "the check column's 'fail' rows and the metricDef healthy declarations disagree:"
    diff <(printf '%s\n' "$doc_fail") <(printf '%s\n' "$code_fail") \
        | sed 's|^<|  classified fail in the doc, not declared healthy: true -- one of the two is wrong: |; s|^>|  declared healthy: true, not classified fail in the doc -- one of the two is wrong: |' >&2
fi
if [ "$doc_warn" != "$code_warn" ]; then
    note "the check column's 'warn' rows and the metricDef warn declarations disagree:"
    diff <(printf '%s\n' "$doc_warn") <(printf '%s\n' "$code_warn") \
        | sed 's|^<|  classified warn in the doc, not declared warn: true -- one of the two is wrong: |; s|^>|  declared warn: true, not classified warn in the doc -- one of the two is wrong: |' >&2
fi

# A counter cannot be both. The Go side holds it too
# (TestHealthChecks_EveryCheckIsAnnotated); here because a doc row
# carrying two classifications would be read by whichever grep ran
# first and would otherwise pass silently.
both=$(comm -12 <(printf '%s\n' "$doc_fail") <(printf '%s\n' "$doc_warn"))
if [ -n "$both" ]; then
    note "classified both fail and warn in $DOC: $(printf '%s' "$both" | tr '\n' ' ')"
fi

# --- 4c. the column must be DERIVABLE from the rows ---------------------
# The check column is stated in docs/reference.md as a rule an operator
# can apply to any row. Section 4b reconciles the column against the
# code; it never asks whether the column reproduces the RULE, so the
# next counter's classification is a judgement nothing reads. This is
# the smallest thing that can be asked mechanically about that rule:
#
#   (a) every `warn` row carries an imperative about its own counter.
#       A row classified warn with nothing in it telling the operator to
#       act cannot be derived from the reference by a reader, which is
#       the property the rule paragraph promises.
#   (b) every `-` row that DOES carry such an imperative says why it is
#       not a check, in the row, with the marker below. Those are the
#       near misses -- the rule's second clause -- and they shipped
#       classified `-` with the reasoning living only in a handover.
#
# (a) IS THE POSITIVE CONTROL FOR (b). The vocabulary is a fixed list of
# phrasings, so a rewrite of the reference that stopped matching it
# would silently empty (b)'s domain -- a universal satisfied by having
# nothing to check. It cannot empty (a)'s: section 4b refuses when the
# warn set is empty, so there is always a non-empty population that must
# match, and a vocabulary that has gone stale fails there first.
#
# WHAT IT CANNOT SEE, and it is the same bound in both directions: the
# vocabulary is ENUMERATED. A row that tells the operator to act in a
# phrasing not on this list is invisible to (b) -- it will be neither
# required to carry the marker nor reported. Two spellings enumerated
# means a third exists; this list is the whole claim.
#
# Only COUNTER rows are judged. The document's non-counter fields
# (`status`, `checks`, `endpoints`, `healthy`, `version`, ...) carry `-`
# in the healthy-affecting column and describe structures rather than
# values, and `endpoints`'s own "read the phase against the mode and
# never alone" is an instruction about two fields of one entry, not
# about a counter that could be a check.
CHECK_IMPERATIVES='alert on|watch it|watch for|worth investigating|worth attention|actionable|read this \*\*before\*\*|never alone'
CHECK_NEARMISS='Not a check:'

n_warn_rows=0
n_nearmiss=0
while IFS= read -r row; do
    [ -n "$row" ] || continue
    name=$(printf '%s' "$row" | sed -E 's/^\| `([a-z0-9_]+)`.*/\1/')
    col=$(printf '%s' "$row" | awk -F'|' '{gsub(/[* ]/,"",$4); print $4}')
    low=$(printf '%s' "$row" | tr '[:upper:]' '[:lower:]')
    imperative=no
    printf '%s' "$low" | grep -E "$(printf '%s' "$CHECK_IMPERATIVES" | tr '[:upper:]' '[:lower:]')" >/dev/null && imperative=yes

    case "$col" in
        warn)
            n_warn_rows=$((n_warn_rows + 1))
            if [ "$imperative" = no ]; then
                note "\`$name\` is classified warn and its row tells the operator nothing to do about its own value."
                echo "  The check column is documented as derivable from the rows: a warn row" >&2
                echo "  states the imperative that puts it there. Add it, or reclassify the row." >&2
            fi
            ;;
        fail) ;;
        *)
            if [ "$imperative" = yes ]; then
                if printf '%s' "$row" | grep -F -- "$CHECK_NEARMISS" >/dev/null; then
                    n_nearmiss=$((n_nearmiss + 1))
                else
                    note "\`$name\` is classified '$col' and its row carries an imperative about the counter."
                    echo "  Under the stated rule that reads as a warn check, so the row has to say which" >&2
                    echo "  clause excluded it -- its own value carrying no verdict, or a normal reading" >&2
                    echo "  that is already non-zero. Write that in the row, starting '$CHECK_NEARMISS'." >&2
                fi
            fi
            ;;
    esac
done <<EOF
$(awk -F'|' '
    NF >= 6 && $2 ~ /^ `[a-z0-9_]+` $/ {
        ha = $3; gsub(/[*[:space:]]/, "", ha)
        if (ha == "yes" || ha == "no") print
    }' "$DOC")
EOF

if [ "$n_warn_rows" -eq 0 ]; then
    echo "check-health-contract: section 4c read no warn-classified counter rows in $DOC." >&2
    echo "  Section 4b has already refused an empty warn set, so reaching here means this" >&2
    echo "  section's row parser no longer matches the table -- it would report clean over" >&2
    echo "  a document it cannot read." >&2
    exit 2
fi

# --- 5. the /metrics help strings --------------------------------------
# Operator-facing text, served on the wire. Name and help sit on one
# line in metricDefs, so this reads that line. If the shape ever
# changes the set comes back empty, which is exit 2 -- a gate reading a
# file it no longer understands must not print PASS over it.
#
# KEYED ON THE PROPERTY, NOT THE SPELLING (#826). This used to be
# `grep -F 'Healthy-affecting.'`, which was wrong in BOTH directions and
# was correct only by an accident of phrasing:
#
#   - `Healthy-affecting.` is a substring of `not Healthy-affecting.`, so
#     a help string DENYING the property was collected as ASSERTING it.
#   - a genuine assertion phrased any other way -- lowercase, a colon
#     instead of the period, no trailing punctuation -- was invisible.
#
# #826 replaced it with a negation stripper: lowercase the line, delete
# every negated occurrence, ask whether one survives. That read the
# sentence better and was wrong one axis over, because the negator had
# to sit flush against the term:
#
#     Not healthy-affecting: informational.       correctly denied
#     Not a healthy-affecting counter.            read as ASSERTING
#     No longer healthy-affecting.                read as ASSERTING
#     Never a healthy-affecting counter.          read as ASSERTING
#
# THE PROPERTY IS NO LONGER PROSE. Two heuristics over English were wrong
# in the same place a month apart, each fix parsing the sentence better
# and leaving the next phrasing open. `metricDef` now declares
# `healthy: true`, and this reads the declaration.
#
# The operator-facing sentence stays in `help`, where it belongs, and it
# is pinned to the declaration by TestMetricHelpMatchesHealthyField in
# pkg/plugin -- which also constrains the negative form to one spelling,
# so the ambiguity that produced both defects cannot be written again.
# That check is in Go rather than here because it compares two fields of
# one struct, which is a job for the language that has the struct.
metrics_set=$(awk '
    /name:[[:space:]]*"/ && /healthy:[[:space:]]*true/ { print }
' "$METRICS" \
    | grep -oE 'name:[[:space:]]*"[a-z0-9_]+"' \
    | sed -E 's/.*"([a-z0-9_]+)".*/\1/' | sort -u)
if [ -z "$metrics_set" ]; then
    echo "check-health-contract: no metricDef in $METRICS declares healthy: true" >&2
    echo "  Either the field was dropped from every counter, or metricDefs no" >&2
    echo "  longer puts name and healthy on one line and this check has gone" >&2
    echo "  blind. Teach it the new shape rather than letting it pass unread." >&2
    exit 2
fi
if [ "$column_set" != "$metrics_set" ]; then
    # THESE TWO LINES NAME A DISAGREEMENT AND STOP THERE, DELIBERATELY.
    # They used to read "the doc does not mark it", which presupposes the
    # code is right and instructs the reader to edit the doc. When the
    # classifier was wrong -- and it was, twice -- that instruction wrote
    # a false entry into an operator-facing table, after which the gate
    # went green over it. A gate that cannot tell which side is wrong
    # must not phrase its output as though it could.
    note "the /metrics healthy declarations and the healthy-affecting column disagree:"
    diff <(printf '%s\n' "$column_set") <(printf '%s\n' "$metrics_set") \
        | sed 's|^<|  marked yes in the doc, not declared healthy: true in the code -- one of the two is wrong: |; s|^>|  declared healthy: true in the code, not marked yes in the doc -- one of the two is wrong: |' >&2
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

echo "PASS  healthy contract agrees in ${n_lists} doc counter-list(s), ${n_words} doc count-word(s), ${n_code} code term(s), ${n_metrics} /metrics healthy declaration(s), ${n_floor} integration floor entr(ies) and ${n_checks} check classification(s): $(printf '%s' "$column_set" | tr '\n' ' ')"
exit 0
