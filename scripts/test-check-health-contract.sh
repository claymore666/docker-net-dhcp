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
#       [<trouble-list>] [<trouble-word>] [<preamble-word>]
#   lists are space-separated counter names; <yes-list> gets `yes` in
#   the healthy-affecting column, and two `no` counters are always
#   present so the column is judged and not merely echoed.
#   <trouble-list> is the cause cell of the Troubleshooting row an
#   operator lands on after seeing `healthy: false`; it defaults to the
#   column so a case that is not about that row stays about its own
#   subject.
#   THREE statements carry the count in words, not one, and each takes
#   its own word so a case can move exactly one of them: <count-word>
#   is the At-a-glance summary's, <trouble-word> the Troubleshooting
#   row's, <preamble-word> the preamble's, and the last two default to
#   the first so a case that is not about them stays about its subject.
mkdoc() {
    local f="$1" word="$2" summary="$3" row="$4" yes="$5" trouble="${6-$5}" n
    local tword="${7-$2}" pword="${8-$2}"
    {
        printf '# Reference\n\nThe claims made *about* those counters — which %s flip `healthy` —\nare gated.\n\n' "$(printf '%s' "$pword" | tr '[:upper:]' '[:lower:]')"
        printf '## At a glance\n\n'
        printf '**[Health counters](#pluginhealth)** — `/Plugin.Health` on the socket. %s flip `healthy` to `false`:' "$word"
        for n in $summary; do printf ' `%s`,' "$n"; done
        printf '\n\n## Counters\n\n| field | healthy-affecting | meaning |\n| --- | --- | --- |\n'
        printf '| `healthy` | — | `false` when'
        for n in $row; do printf ' `%s`,' "$n"; done
        printf ' is non-zero. |\n'
        for n in $yes; do printf '| `%s` | yes | a fault. |\n' "$n"; done
        printf '| `leases_renewed` | no | not a fault. |\n'
        printf '| `pending_hints` | no | not a fault. |\n'
        printf '\n## Troubleshooting\n\n| symptom | likely cause | fix |\n| --- | --- | --- |\n'
        printf '| `healthy: false` on `/Plugin.Health` | Exactly %s counters flip it:' "$(printf '%s' "$tword" | tr '[:upper:]' '[:lower:]')"
        for n in $trouble; do printf ' `%s`,' "$n"; done
        printf ' | read the counter that is non-zero and follow its row. |\n'
        printf '| `docker plugin disable` refuses | networks still reference it | remove them first. |\n'
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

# --- agreement ---------------------------------------------------------
mkdoc "$DIR/ok.md" Four "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/ok.go" 4
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "four agreeing statements and a matching expression pass" \
               || no "agreement failed (rc=$rc: $out)"

# --- the #638 shape: the row went a counter stale ----------------------
THREE="recovery_failed join_start_failures tombstone_write_failures"
mkdoc "$DIR/stale.md" Four "$FOUR" "$THREE" "$FOUR"; mkgo "$DIR/stale.go" 4
out=$(bash "$CHECK" "$DIR/stale.md" "$DIR/stale.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a healthy row that lost a counter fails" \
               || no "the shipped bug's shape returned $rc (: $out)"
case "$out" in *address_conflicts*) ok "the failure names the missing counter" ;;
  *) no "the failure does not name the counter: $out" ;; esac

# --- the summary drifting the other way --------------------------------
mkdoc "$DIR/sum.md" Four "$THREE" "$FOUR" "$FOUR"; mkgo "$DIR/sum.go" 4
out=$(bash "$CHECK" "$DIR/sum.md" "$DIR/sum.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "an At-a-glance summary missing a counter fails" \
               || no "a stale summary returned $rc (: $out)"

# --- the count word left behind ----------------------------------------
mkdoc "$DIR/word.md" Three "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/word.go" 4
out=$(bash "$CHECK" "$DIR/word.md" "$DIR/word.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word fails even when the list is right" \
               || no "the count word is not judged (rc=$rc: $out)"

# The count word is carried by THREE statements, and the check started
# on one. A fifth counter added correctly to every name-list still
# shipped a Troubleshooting row and a preamble saying "four" — the row
# being the one an operator reaches after they have already seen
# `healthy: false`, which is the exact reader #638 was about (#724).
mkdoc "$DIR/tword.md" Four "$FOUR" "$FOUR" "$FOUR" "$FOUR" Three; mkgo "$DIR/tword.go" 4
out=$(bash "$CHECK" "$DIR/tword.md" "$DIR/tword.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word in the troubleshooting row fails" \
               || no "the troubleshooting row's count word is not judged (rc=$rc: $out)"
case "$out" in *Troubleshooting*three*) ok "the failure names the row and the word it found" ;;
  *) no "the failure does not identify the troubleshooting word: $out" ;; esac

mkdoc "$DIR/pword.md" Four "$FOUR" "$FOUR" "$FOUR" "$FOUR" Four Three; mkgo "$DIR/pword.go" 4
out=$(bash "$CHECK" "$DIR/pword.md" "$DIR/pword.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a stale count word in the preamble fails" \
               || no "the preamble's count word is not judged (rc=$rc: $out)"
case "$out" in *preamble*three*) ok "the failure names the preamble and the word it found" ;;
  *) no "the failure does not identify the preamble word: $out" ;; esac

# All three words move together on a real edit: that must pass, or the
# gate blocks the next counter instead of guarding it.
mkdoc "$DIR/allwords.md" Five "$FIVE_" "$FIVE_" "$FIVE_" "$FIVE_" Five Five; mkgo "$DIR/allwords.go" 5
out=$(bash "$CHECK" "$DIR/allwords.md" "$DIR/allwords.go" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a fifth counter with all three count words moved passes" \
               || no "the gate blocks a correct five-counter edit (rc=$rc: $out)"

# A statement that carries no readable count is "cannot see", not a
# pass: the gate must never judge a line it failed to parse as clean.
mkdoc "$DIR/nopre.md" Four "$FOUR" "$FOUR" "$FOUR"
grep -v 'flip `healthy` —' "$DIR/nopre.md" > "$DIR/nopre2.md"
out=$(bash "$CHECK" "$DIR/nopre2.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing preamble exits 2" \
               || no "a missing preamble returned $rc (: $out)"

mkdoc "$DIR/notword.md" Four "$FOUR" "$FOUR" "$FOUR"
sed -i 's/Exactly four counters flip it:/several counters flip it:/' "$DIR/notword.md"
out=$(bash "$CHECK" "$DIR/notword.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a troubleshooting row with no readable count exits 2" \
               || no "an unreadable count word returned $rc (: $out)"

# --- the code moving without the docs ----------------------------------
mkdoc "$DIR/code.md" Four "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/code.go" 5
out=$(bash "$CHECK" "$DIR/code.md" "$DIR/code.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a fifth term in the code with no doc change fails" \
               || no "the code side is not judged (rc=$rc: $out)"

# --- the fourth copy: the Troubleshooting row ---------------------------
# The row an operator reaches AFTER seeing `healthy: false`. It shipped
# naming two of the four counters, and the gate that guarded the other
# three copies could not see it.
TWO="recovery_failed tombstone_write_failures"
mkdoc "$DIR/trouble.md" Four "$FOUR" "$FOUR" "$FOUR" "$TWO"; mkgo "$DIR/trouble.go" 4
out=$(bash "$CHECK" "$DIR/trouble.md" "$DIR/trouble.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a troubleshooting row naming 2 of 4 counters fails" \
               || no "the troubleshooting row is not judged (rc=$rc: $out)"
case "$out" in *join_start_failures*) ok "the failure names a counter the row omits" ;;
  *) no "the failure does not name the omitted counter: $out" ;; esac

# The shipped shape exactly: prose that names no counter at all.
mkdoc "$DIR/trouble0.md" Four "$FOUR" "$FOUR" "$FOUR" ""; mkgo "$DIR/trouble0.go" 4
out=$(bash "$CHECK" "$DIR/trouble0.md" "$DIR/trouble0.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a troubleshooting row naming no counter fails" \
               || no "an unbackticked cause cell passed (rc=$rc: $out)"

# A row that names a name which is not a counter must not count as
# naming one — `STATE_DIR` is a setting, and the shipped row cited it.
mkdoc "$DIR/troubleset.md" Four "$FOUR" "$FOUR" "$FOUR" ""; mkgo "$DIR/troubleset.go" 4
sed -i 's/counters flip it: |/counters flip it: check `STATE_DIR` |/' "$DIR/troubleset.md"
out=$(bash "$CHECK" "$DIR/troubleset.md" "$DIR/troubleset.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a cause cell citing a setting rather than a counter still fails" \
               || no "a non-counter backtick satisfied the row (rc=$rc: $out)"

# --- growth must be possible -------------------------------------------
# The gate must not encode today's four. Add a fifth everywhere: pass.
mkdoc "$DIR/five.md" Five "$FIVE_" "$FIVE_" "$FIVE_"; mkgo "$DIR/five.go" 5
out=$(bash "$CHECK" "$DIR/five.md" "$DIR/five.go" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a fifth counter agreed everywhere passes" \
               || no "the gate blocks a legitimate new counter (rc=$rc: $out)"

# A missing troubleshooting row is "cannot see", not "nothing to check".
mkdoc "$DIR/notrouble.md" Four "$FOUR" "$FOUR" "$FOUR"
grep -v '^| `healthy: false`' "$DIR/notrouble.md" > "$DIR/notrouble2.md"
out=$(bash "$CHECK" "$DIR/notrouble2.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing troubleshooting row exits 2" \
               || no "a missing troubleshooting row returned $rc (: $out)"

# --- cannot see: every one of these must be loud -----------------------
mkdoc "$DIR/shape.md" Four "$FOUR" "$FOUR" "$FOUR"
printf 'package plugin\n\n\t\tHealthy:           a == 0 || b != 3,\n' > "$DIR/shape.go"
out=$(bash "$CHECK" "$DIR/shape.md" "$DIR/shape.go" 2>&1); rc=$?
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
out=$(bash "$CHECK" "$DIR/two.md" "$DIR/twohealth.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "two Healthy assignments exit 2 rather than judging the first" \
               || no "a second Healthy assignment was read as the only one (rc=$rc: $out)"

printf 'package plugin\n' > "$DIR/nohealth.go"
out=$(bash "$CHECK" "$DIR/shape.md" "$DIR/nohealth.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "no Healthy assignment at all exits 2" || no "a missing Healthy returned $rc"

printf '# nothing here\n' > "$DIR/empty.md"
out=$(bash "$CHECK" "$DIR/empty.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a doc with no counter table exits 2" || no "an empty doc returned $rc"

mkdoc "$DIR/norow.md" Four "$FOUR" "$FOUR" "$FOUR"
grep -v '^| `healthy` |' "$DIR/norow.md" > "$DIR/norow2.md"
out=$(bash "$CHECK" "$DIR/norow2.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing healthy row exits 2" || no "a missing healthy row returned $rc"

out=$(bash "$CHECK" "$DIR/does-not-exist.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "a missing doc exits 2" || no "a missing doc returned $rc"

# --- the repository itself ---------------------------------------------
out=$(bash "$CHECK" "$HERE/../docs/reference.md" "$HERE/../pkg/plugin/endpoints.go" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "the repository's own healthy contract agrees" \
               || no "the repo's healthy contract disagrees (rc=$rc: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
