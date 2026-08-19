#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-health-contract.sh (#638).
#
# Fixtures are generated, never the repo's own reference.md, so the
# cases keep meaning after the next counter is added.
#
# The case that keeps the rest honest is the last positive one: a
# FIFTH counter, agreed everywhere, must pass. Without it a gate
# hardcoded to today's four would satisfy every other case here while
# blocking the next real change.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-health-contract.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

# mkdoc <file> <count-word> <summary-list> <row-list> <yes-list>
#   lists are space-separated counter names; <yes-list> gets `yes` in
#   the healthy-affecting column, and two `no` counters are always
#   present so the column is judged and not merely echoed.
mkdoc() {
    local f="$1" word="$2" summary="$3" row="$4" yes="$5" n
    {
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

# --- agreement ---------------------------------------------------------
mkdoc "$DIR/ok.md" Four "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/ok.go" 4
out=$(bash "$CHECK" "$DIR/ok.md" "$DIR/ok.go" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "three agreeing statements and a matching expression pass" \
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

# --- the code moving without the docs ----------------------------------
mkdoc "$DIR/code.md" Four "$FOUR" "$FOUR" "$FOUR"; mkgo "$DIR/code.go" 5
out=$(bash "$CHECK" "$DIR/code.md" "$DIR/code.go" 2>&1); rc=$?
[ $rc -eq 1 ] && ok "a fifth term in the code with no doc change fails" \
               || no "the code side is not judged (rc=$rc: $out)"

# --- growth must be possible -------------------------------------------
# The gate must not encode today's four. Add a fifth everywhere: pass.
FIVE="$FOUR ledger_write_failures"
mkdoc "$DIR/five.md" Five "$FIVE" "$FIVE" "$FIVE"; mkgo "$DIR/five.go" 5
out=$(bash "$CHECK" "$DIR/five.md" "$DIR/five.go" 2>&1); rc=$?
[ $rc -eq 0 ] && ok "a fifth counter agreed everywhere passes" \
               || no "the gate blocks a legitimate new counter (rc=$rc: $out)"

# --- cannot see: every one of these must be loud -----------------------
mkdoc "$DIR/shape.md" Four "$FOUR" "$FOUR" "$FOUR"
printf 'package plugin\n\n\t\tHealthy:           a == 0 || b != 3,\n' > "$DIR/shape.go"
out=$(bash "$CHECK" "$DIR/shape.md" "$DIR/shape.go" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "an expression shape it cannot parse exits 2, not clean" \
               || no "an unparseable Healthy expression returned $rc (: $out)"

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
