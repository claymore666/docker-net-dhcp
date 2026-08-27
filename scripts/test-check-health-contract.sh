#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-health-contract.sh (#638).
#
# Fixtures are generated, never the repo's own reference.md, so the
# cases keep meaning after the next counter is added.
#
# The case that keeps the rest honest is the last positive one: a
# FIFTH counter, agreed everywhere — the Troubleshooting row included —
# must pass. Without it a gate hardcoded to today's four would satisfy
# every other case here while blocking the next real change.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-health-contract.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

# mkdoc <file> <count-word> <summary-list> <row-list> <yes-list> \
#       [<trouble-list>] [<trouble-word>] [<preamble-word>] \
#       [<row-word>] [<remedy-word>]
#   lists are space-separated counter names; <yes-list> gets `yes` in
#   the healthy-affecting column, and two `no` counters are always
#   present so the column is judged and not merely echoed.
#   <trouble-list> is the cause cell of the Troubleshooting row an
#   operator lands on after seeing `healthy: false`; it defaults to the
#   column so a case that is not about that row stays about its own
#   subject.
#   FIVE statements carry the count in words, not one, and each takes
#   its own word so a case can move exactly one of them: <count-word>
#   is the At-a-glance summary's, <trouble-word> the Troubleshooting
#   row's cause cell, <preamble-word> the preamble's, <row-word> the
#   `healthy` row's own restatement, <remedy-word> the Troubleshooting
#   row's remedy sentence. All but the first default to the first, so a
#   case that is not about them stays about its own subject.
#
#   The last two exist because their rows' NAME LISTS were guarded
#   while the sentence restating the length of those lists, in the same
#   two rows, was not.
mkdoc() {
    local f="$1" word="$2" summary="$3" row="$4" yes="$5" trouble="${6-$5}" n
    local tword="${7-$2}" pword="${8-$2}" rword="${9-$2}" mword="${10-$2}"
    {
        printf '# Reference\n\nThe claims made *about* those counters — which %s flip `healthy` —\nare gated.\n\n' "$(printf '%s' "$pword" | tr '[:upper:]' '[:lower:]')"
        printf '## At a glance\n\n'
        printf '**[Health counters](#pluginhealth)** — `/Plugin.Health` on the socket. %s flip `healthy` to `false`:' "$word"
        for n in $summary; do printf ' `%s`,' "$n"; done
        printf '\n\n## Counters\n\n| field | healthy-affecting | meaning |\n| --- | --- | --- |\n'
        printf '| `healthy` | — | `false` when'
        for n in $row; do printf ' `%s`,' "$n"; done
        printf ' is non-zero. Those %s, and only those, are the ones marked **yes** in this column. |\n' "$(printf '%s' "$rword" | tr '[:upper:]' '[:lower:]')"
        for n in $yes; do printf '| `%s` | yes | a fault. |\n' "$n"; done
        printf '| `leases_renewed` | no | not a fault. |\n'
        printf '| `pending_hints` | no | not a fault. |\n'
        printf '\n## Troubleshooting\n\n| symptom | likely cause | fix |\n| --- | --- | --- |\n'
        printf '| `healthy: false` on `/Plugin.Health` | Exactly %s counters flip it:' "$(printf '%s' "$tword" | tr '[:upper:]' '[:lower:]')"
        for n in $trouble; do printf ' `%s`,' "$n"; done
        printf ' | Read the %s in the field table above to see which one moved. |\n' "$(printf '%s' "$mword" | tr '[:upper:]' '[:lower:]')"
        printf '| `docker plugin disable` refuses | networks still reference it | remove them first. |\n'
    } > "$f"
}

# mkmetrics_phrased <file> <plain-list> <extra "name=help" pairs...>
#   Same shape as mkmetrics, but the caller controls the exact PHRASING of
#   the extra entries. mkmetrics only ever emits "Healthy-affecting." and
#   "not a fault.", so no fixture it builds can distinguish a gate keyed on
#   the property from one keyed on that literal spelling -- the natural
#   fixture selects the passing path, which is why #826 shipped.
mkmetrics_phrased() {
    local f="$1" list="$2"; shift 2
    local n pair
    {
        printf 'package plugin\n\nvar metricDefs = []metricDef{\n'
        for n in $list; do
            printf '\t{name: "%s", counter: true, help: "a fault. Healthy-affecting.", field: "%s"},\n' "$n" "$n"
        done
        for n in leases_renewed pending_hints; do
            printf '\t{name: "%s", counter: true, help: "not a fault.", field: "%s"},\n' "$n" "$n"
        done
        for pair in "$@"; do
            printf '\t{name: "%s", counter: true, help: "%s", field: "%s"},\n' \
                "${pair%%=*}" "${pair#*=}" "${pair%%=*}"
        done
        printf '}\n'
    } > "$f"
}

# mkmetrics <file> <counter-list> [<extra-tagged>]
#   metricDefs as pkg/plugin/metrics.go writes it: name and help on one
#   line, and the help of a healthy-affecting counter ending
#   "Healthy-affecting.". Two counters that are NOT healthy-affecting
#   are always emitted, so a check that merely echoed every name back
#   would fail here. <extra-tagged> gets the sentence without being in
#   the doc's column, which is the drift in the other direction.
mkmetrics() {
    local f="$1" list="$2" extra="${3-}" n
    {
        printf 'package plugin\n\nvar metricDefs = []metricDef{\n'
        for n in $list; do
            printf '\t{name: "%s", counter: true, help: "a fault. Healthy-affecting.", field: "%s"},\n' "$n" "$n"
        done
        for n in leases_renewed pending_hints; do
            printf '\t{name: "%s", counter: true, help: "not a fault.", field: "%s"},\n' "$n" "$n"
        done
        for n in $extra; do
            printf '\t{name: "%s", counter: true, help: "not a fault. Healthy-affecting.", field: "%s"},\n' "$n" "$n"
        done
        printf '}\n'
    } > "$f"
}

# mkfloor <file> <counter-list> [<nonfatal-list>]
#   test/integration/harness/healthfloor.go's floorCounters block.
#   <nonfatal-list> entries are present but fatal: false -- watched and
#   then waved through, which reads greener than being absent.
mkfloor() {
    local f="$1" list="$2" nonfatal="${3-}" n
    {
        printf 'package harness\n\nvar floorCounters = []floorCounter{\n'
        for n in $list; do
            printf '\t{\n\t\tname:  "%s",\n\t\tfatal: true,\n\t\twhy:   "a fault.",\n\t},\n' "$n"
        done
        for n in $nonfatal; do
            printf '\t{\n\t\tname:  "%s",\n\t\tfatal: false,\n\t\twhy:   "a fault.",\n\t},\n' "$n"
        done
        printf '}\n'
    } > "$f"
}

# mkgo <file> <n-terms>
mkgo() {
    local f="$1" n="$2" i terms=""
    for ((i = 0; i < n; i++)); do
        [ -n "$terms" ] && terms="$terms && "
        terms="${terms}c${i} == 0"
    done
    printf 'package plugin\n\nfunc f() {\n\t_ = HealthResponse{\n\t\tHealthy:           %s,\n\t}\n}\n' "$terms" > "$f"
}

FOUR="recovery_failed join_start_failures tombstone_write_failures address_conflicts"
FIVE_="$FOUR ledger_write_failures"

# The two code rails, agreeing with the doc, so every case below is
# about its own subject. A case that IS about a rail passes its own.
M4="$DIR/rail4.metrics.go"; F4="$DIR/rail4.floor.go"
M5="$DIR/rail5.metrics.go"; F5="$DIR/rail5.floor.go"
mkmetrics "$M4" "$FOUR"; mkfloor "$F4" "$FOUR"
mkmetrics "$M5" "$FIVE_"; mkfloor "$F5" "$FIVE_"

# --- agreement ---------------------------------------------------------
mkdoc "$DIR/ok.md" Four "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/ok.go" 4
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "four agreeing statements and a matching expression pass" \
               || no "agreement failed (rc=$rc: $out)"

# --- the #638 shape: the row went a counter stale ----------------------
THREE="recovery_failed join_start_failures tombstone_write_failures"
mkdoc "$DIR/stale.md" Four "$FOUR" "$THREE" "$FOUR"; mkgo "$DIR/stale.go" 4
out=$(bash "$CHECK" "$DIR/stale.md" "$DIR/stale.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a healthy row that lost a counter fails" \
               || no "the shipped bug's shape returned $rc (: $out)"
case "$out" in *address_conflicts*) ok "the failure names the missing counter" ;;
  *) no "the failure does not name the counter: $out" ;; esac

# --- the summary drifting the other way --------------------------------
mkdoc "$DIR/sum.md" Four "$THREE" "$FOUR" "$FOUR"; mkgo "$DIR/sum.go" 4
out=$(bash "$CHECK" "$DIR/sum.md" "$DIR/sum.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "an At-a-glance summary missing a counter fails" \
               || no "a stale summary returned $rc (: $out)"

# --- the count word left behind ----------------------------------------
mkdoc "$DIR/word.md" Three "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/word.go" 4
out=$(bash "$CHECK" "$DIR/word.md" "$DIR/word.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word fails even when the list is right" \
               || no "the count word is not judged (rc=$rc: $out)"

# The count word is carried by THREE statements, and the check started
# on one. A fifth counter added correctly to every name-list still
# shipped a Troubleshooting row and a preamble saying "four" — the row
# being the one an operator reaches after they have already seen
# `healthy: false`, which is the exact reader #638 was about (#724).
mkdoc "$DIR/tword.md" Four "$FOUR" "$FOUR" "$FOUR" "$FOUR" Three; mkgo "$DIR/tword.go" 4
out=$(bash "$CHECK" "$DIR/tword.md" "$DIR/tword.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word in the troubleshooting row fails" \
               || no "the troubleshooting row's count word is not judged (rc=$rc: $out)"
case "$out" in *Troubleshooting*three*) ok "the failure names the row and the word it found" ;;
  *) no "the failure does not identify the troubleshooting word: $out" ;; esac

mkdoc "$DIR/pword.md" Four "$FOUR" "$FOUR" "$FOUR" "$FOUR" Four Three; mkgo "$DIR/pword.go" 4
out=$(bash "$CHECK" "$DIR/pword.md" "$DIR/pword.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word in the preamble fails" \
               || no "the preamble's count word is not judged (rc=$rc: $out)"
case "$out" in *preamble*three*) ok "the failure names the preamble and the word it found" ;;
  *) no "the failure does not identify the preamble word: $out" ;; esac

# The two rows whose LISTS were already guarded each restate the count
# a second time. A row that names five counters and then says "those
# four, and only those" contradicts itself inside one cell, and the
# list check passes it because the list is right.
mkdoc "$DIR/rword.md" Four "$FOUR" "$FOUR" "$FOUR" "$FOUR" Four Four Three; mkgo "$DIR/rword.go" 4
out=$(bash "$CHECK" "$DIR/rword.md" "$DIR/rword.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word inside the healthy row fails" \
               || no "the healthy row's own count word is not judged (rc=$rc: $out)"
case "$out" in *"\`healthy\` row"*three*) ok "the failure names the healthy row and the word it found" ;;
  *) no "the failure does not identify the healthy row's word: $out" ;; esac

# And the remedy sentence, which is the one that changes what an
# operator DOES: "read the four in the field table above" over a list
# of five stops them after the fourth.
mkdoc "$DIR/mword.md" Four "$FOUR" "$FOUR" "$FOUR" "$FOUR" Four Four Four Three; mkgo "$DIR/mword.go" 4
out=$(bash "$CHECK" "$DIR/mword.md" "$DIR/mword.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word in the troubleshooting remedy sentence fails" \
               || no "the remedy sentence's count word is not judged (rc=$rc: $out)"
case "$out" in *remedy*three*) ok "the failure names the remedy sentence and the word it found" ;;
  *) no "the failure does not identify the remedy word: $out" ;; esac

# Every count word moving TOGETHER, to a real fifth counter agreed
# everywhere, must pass -- otherwise these five checks have pinned the
# doc to today's four and the next counter cannot be added at all.
mkdoc "$DIR/five5.md" Five "$FIVE_" "$FIVE_" "$FIVE_" "$FIVE_" Five Five Five Five; mkgo "$DIR/five5.go" 5
out=$(bash "$CHECK" "$DIR/five5.md" "$DIR/five5.go" "$M5" "$F5" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "five counters with all five count words moved pass" \
               || no "a correct fifth counter is blocked (rc=$rc: $out)"
case "$out" in *"5 doc count-word(s)"*) ok "the PASS line reports how many count words it read" ;;
  *) no "the PASS line does not tally the count words: $out" ;; esac

# --- the /metrics help strings -----------------------------------------
# Operator-facing text served on the wire, and nothing read it. The
# exposition golden is regenerated FROM these strings, so it agrees
# with whatever they say; check-docs-drift.sh reconciles the set of
# field NAMES, not the sentence (#724).
mkmetrics "$DIR/m1.metrics.go" "recovery_failed join_start_failures tombstone_write_failures"
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$DIR/m1.metrics.go" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a counter whose help string forgot \"Healthy-affecting.\" fails" \
               || no "the help strings are not judged (rc=$rc: $out)"
case "$out" in *address_conflicts*) ok "the failure names the counter whose help string is short" ;;
  *) no "the failure does not name the counter: $out" ;; esac

# And the other direction, which is the #709 shape: prose promising a
# property the counter does not have.
mkmetrics "$DIR/m2.metrics.go" "$FOUR" "leases_renewed"
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$DIR/m2.metrics.go" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a help string claiming \"Healthy-affecting.\" on a counter the doc does not mark fails" \
               || no "the reverse drift is not judged (rc=$rc: $out)"

# --- #826: the assertion is read by MEANING, not by spelling ------------
# Both arms below fail against the pre-fix gate, which matched the fixed
# string "Healthy-affecting." and so was wrong in both directions at once.

# ARM 1 -- a DENIAL must not be collected as an assertion.
# "Healthy-affecting." is a substring of "Not Healthy-affecting.", so the
# old matcher collected a counter whose help string explicitly denies the
# property. The remedy it then printed was "mark this yes in the doc",
# i.e. document a non-affecting counter as healthy-affecting.
# leases_renewed is NOT in the doc's yes column, so a gate that collects
# it reports drift that does not exist.
mkmetrics_phrased "$DIR/m826a.metrics.go" "$FOUR" \
    "leases_renewed=expected, not a fault. Not Healthy-affecting."
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$DIR/m826a.metrics.go" "$F4" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a help string DENYING the property is not collected as asserting it (#826)" \
               || no "a negated help string was read as an assertion (rc=$rc: $out)"

# ARM 2 -- a genuine assertion phrased differently must be SEEN.
# The worse arm: the old matcher missed lowercase, a colon, or a missing
# period, and the remedy it printed was "remove the yes from the doc" --
# deleting a true statement about a counter that really does affect
# health. Here the counter is NOT in the doc column, so the gate must
# fail AND name it; a gate that cannot see the sentence reports clean.
mkmetrics_phrased "$DIR/m826b.metrics.go" "$FOUR" \
    "leases_renewed=a fault. healthy-affecting: an operator should look"
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$DIR/m826b.metrics.go" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "an assertion phrased lowercase with a colon is still seen (#826)" \
               || no "an off-spelling assertion was invisible (rc=$rc: $out)"
case "$out" in *leases_renewed*) ok "the failure names the counter whose assertion was off-spelling" ;;
  *) no "the failure does not name leases_renewed: $out" ;; esac

# ARM 3 -- every negator in the pattern is load-bearing.
# The matcher accepts `not`, `non` and `never`. Arms 1 and 2 exercise only
# `not`, so narrowing the pattern to `(not)` alone leaves the suite green:
# an untested alternative is capability nobody is measuring, and it reads
# as coverage. One counter per negator, each denying the property, none of
# them in the doc's yes column -- so the gate must stay clean, and does not
# if any one of the three stops being recognised.
mkmetrics_phrased "$DIR/m826c.metrics.go" "$FOUR" \
    "leases_renewed=expected. non-healthy-affecting, no operator action" \
    "pending_hints=expected. never Healthy-affecting."
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$DIR/m826c.metrics.go" "$F4" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "\"non-\" and \"never\" are recognised as denials, not just \"not\" (#826)" \
               || no "a negator other than \"not\" was read as an assertion (rc=$rc: $out)"

# A metrics file this gate cannot read is exit 2, never a pass. If
# metricDefs stops putting name and help on one line, the set comes
# back empty and an empty set must not compare clean against anything.
mkmetrics "$DIR/m3.metrics.go" ""
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$DIR/m3.metrics.go" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a metrics file with no \"Healthy-affecting.\" at all exits 2, not clean" \
               || no "an unreadable metrics file returned $rc (: $out)"

# --- the integration health floor --------------------------------------
# floorCounters is what an integration run FAILS over. client2 found it
# is a subset with nothing saying so: a new healthy-affecting counter
# can ship with no run watching it while every other file agrees.
mkfloor "$DIR/f1.floor.go" "recovery_failed join_start_failures tombstone_write_failures"
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$M4" "$DIR/f1.floor.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a healthy-affecting counter missing from the integration floor fails" \
               || no "the floor is not judged (rc=$rc: $out)"
case "$out" in *"no integration run watches it"*) ok "the failure says what the absence costs" ;;
  *) no "the failure does not say what is lost: $out" ;; esac

# Present but non-fatal is worse than absent, because it reads greener.
mkfloor "$DIR/f2.floor.go" "recovery_failed join_start_failures tombstone_write_failures" "address_conflicts"
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$M4" "$DIR/f2.floor.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a floor entry that is present but not fatal fails" \
               || no "a non-fatal floor entry passed (rc=$rc: $out)"

# An unrecognised floorCounters block reads nothing, and nothing must
# not compare clean.
printf 'package harness\n\nvar somethingElse = []floorCounter{}\n' > "$DIR/f3.floor.go"
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" "$M4" "$DIR/f3.floor.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a floor file whose var block is not found exits 2, not clean" \
               || no "an unreadable floor file returned $rc (: $out)"

# The positive that keeps all five rails honest: a fifth counter agreed
# in the doc, the code, the help strings AND the floor must pass, or
# these checks have pinned the contract to today's four.
out=$(bash "$CHECK" "$DIR/five5.md" "$DIR/five5.go" "$M5" "$F5" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a fifth counter agreed across all five surfaces passes" \
               || no "a correct fifth counter is blocked by the code rails (rc=$rc: $out)"
case "$out" in *"/metrics help string(s)"*"integration floor entr(ies)"*)
    ok "the PASS line tallies the two code rails too" ;;
  *) no "the PASS line does not report the code rails: $out" ;; esac

# All three words move together on a real edit: that must pass, or the
# gate blocks the next counter instead of guarding it.
mkdoc "$DIR/allwords.md" Five "$FIVE_" "$FIVE_" "$FIVE_" "$FIVE_" Five Five; mkgo "$DIR/allwords.go" 5
out=$(bash "$CHECK" "$DIR/allwords.md" "$DIR/allwords.go" "$M5" "$F5" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a fifth counter with all three count words moved passes" \
               || no "the gate blocks a correct five-counter edit (rc=$rc: $out)"

# A statement that carries no readable count is "cannot see", not a
# pass: the gate must never judge a line it failed to parse as clean.
mkdoc "$DIR/nopre.md" Four "$FOUR" "$FOUR" "$FOUR"
grep -v 'flip `healthy` —' "$DIR/nopre.md" > "$DIR/nopre2.md"
out=$(bash "$CHECK" "$DIR/nopre2.md" "$DIR/ok.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing preamble exits 2" \
               || no "a missing preamble returned $rc (: $out)"

mkdoc "$DIR/notword.md" Four "$FOUR" "$FOUR" "$FOUR"
sed -i 's/Exactly four counters flip it:/several counters flip it:/' "$DIR/notword.md"
out=$(bash "$CHECK" "$DIR/notword.md" "$DIR/ok.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a troubleshooting row with no readable count exits 2" \
               || no "an unreadable count word returned $rc (: $out)"

# --- the code moving without the docs ----------------------------------
mkdoc "$DIR/code.md" Four "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/code.go" 5
out=$(bash "$CHECK" "$DIR/code.md" "$DIR/code.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a fifth term in the code with no doc change fails" \
               || no "the code side is not judged (rc=$rc: $out)"

# --- the fourth copy: the Troubleshooting row ---------------------------
# The row an operator reaches AFTER seeing `healthy: false`. It shipped
# naming two of the four counters, and the gate that guarded the other
# three copies could not see it.
TWO="recovery_failed tombstone_write_failures"
mkdoc "$DIR/trouble.md" Four "$FOUR" "$FOUR" "$FOUR" "$TWO"; mkgo "$DIR/trouble.go" 4
out=$(bash "$CHECK" "$DIR/trouble.md" "$DIR/trouble.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a troubleshooting row naming 2 of 4 counters fails" \
               || no "the troubleshooting row is not judged (rc=$rc: $out)"
case "$out" in *join_start_failures*) ok "the failure names a counter the row omits" ;;
  *) no "the failure does not name the omitted counter: $out" ;; esac

# The shipped shape exactly: prose that names no counter at all.
mkdoc "$DIR/trouble0.md" Four "$FOUR" "$FOUR" "$FOUR" ""; mkgo "$DIR/trouble0.go" 4
out=$(bash "$CHECK" "$DIR/trouble0.md" "$DIR/trouble0.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a troubleshooting row naming no counter fails" \
               || no "an unbackticked cause cell passed (rc=$rc: $out)"

# A row that names a name which is not a counter must not count as
# naming one — `STATE_DIR` is a setting, and the shipped row cited it.
mkdoc "$DIR/troubleset.md" Four "$FOUR" "$FOUR" "$FOUR" ""; mkgo "$DIR/troubleset.go" 4
sed -i 's/counters flip it: |/counters flip it: check `STATE_DIR` |/' "$DIR/troubleset.md"
out=$(bash "$CHECK" "$DIR/troubleset.md" "$DIR/troubleset.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a cause cell citing a setting rather than a counter still fails" \
               || no "a non-counter backtick satisfied the row (rc=$rc: $out)"

# --- growth must be possible -------------------------------------------
# The gate must not encode today's four. Add a fifth everywhere: pass.
mkdoc "$DIR/five.md" Five "$FIVE_" "$FIVE_" "$FIVE_"; mkgo "$DIR/five.go" 5
out=$(bash "$CHECK" "$DIR/five.md" "$DIR/five.go" "$M5" "$F5" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a fifth counter agreed everywhere passes" \
               || no "the gate blocks a legitimate new counter (rc=$rc: $out)"

# A missing troubleshooting row is "cannot see", not "nothing to check".
mkdoc "$DIR/notrouble.md" Four "$FOUR" "$FOUR" "$FOUR"
grep -v '^| `healthy: false`' "$DIR/notrouble.md" > "$DIR/notrouble2.md"
out=$(bash "$CHECK" "$DIR/notrouble2.md" "$DIR/ok.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing troubleshooting row exits 2" \
               || no "a missing troubleshooting row returned $rc (: $out)"

# --- cannot see: every one of these must be loud -----------------------
mkdoc "$DIR/shape.md" Four "$FOUR" "$FOUR" "$FOUR"
printf 'package plugin\n\n\t\tHealthy:           a == 0 || b != 3,\n' > "$DIR/shape.go"
out=$(bash "$CHECK" "$DIR/shape.md" "$DIR/shape.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "an expression shape it cannot parse exits 2, not clean" \
               || no "an unparseable Healthy expression returned $rc (: $out)"

# TWO Healthy assignments must be "cannot see", not "read the first".
# The gate used `head -1`, so a second literal added ABOVE the real one
# — a degraded-mode response, a test helper that migrates in — would
# have become the thing judged while the shipping expression went
# unread. The decoy below carries the RIGHT number of terms, so a gate
# that stops at the first match passes and never reaches the one under
# it: the case has to be built so that only ordering can save it.
mkdoc "$DIR/two.md" Four "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/two.go" 4
{
    printf 'package plugin\n\nfunc degraded() {\n\t_ = HealthResponse{\n'
    printf '\t\tHealthy:           a == 0 && b == 0 && c == 0 && d == 0,\n\t}\n}\n'
    grep -v '^package plugin$' "$DIR/two.go"
} > "$DIR/twohealth.go"
out=$(bash "$CHECK" "$DIR/two.md" "$DIR/twohealth.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "two Healthy assignments exit 2 rather than judging the first" \
               || no "a second Healthy assignment was read as the only one (rc=$rc: $out)"

printf 'package plugin\n' > "$DIR/nohealth.go"
out=$(bash "$CHECK" "$DIR/shape.md" "$DIR/nohealth.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "no Healthy assignment at all exits 2" || no "a missing Healthy returned $rc"

printf '# nothing here\n' > "$DIR/empty.md"
out=$(bash "$CHECK" "$DIR/empty.md" "$DIR/ok.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a doc with no counter table exits 2" || no "an empty doc returned $rc"

mkdoc "$DIR/norow.md" Four "$FOUR" "$FOUR" "$FOUR"
grep -v '^| `healthy` |' "$DIR/norow.md" > "$DIR/norow2.md"
out=$(bash "$CHECK" "$DIR/norow2.md" "$DIR/ok.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing healthy row exits 2" || no "a missing healthy row returned $rc"

out=$(bash "$CHECK" "$DIR/does-not-exist.md" "$DIR/ok.go" "$M4" "$F4" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing doc exits 2" || no "a missing doc returned $rc"

# --- the repository itself ---------------------------------------------
out=$(bash "$CHECK" "$HERE/../docs/reference.md" "$HERE/../pkg/plugin/endpoints.go" \
    "$HERE/../pkg/plugin/metrics.go" "$HERE/../test/integration/harness/healthfloor.go" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the repository's own healthy contract agrees" \
               || no "the repo's healthy contract disagrees (rc=$rc: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
