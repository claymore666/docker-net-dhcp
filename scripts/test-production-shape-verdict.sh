#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for production-shape-verdict.sh (#1014).
#
# THE VERDICT THIS GATE MUST NEVER PRODUCE is a pass over a log that
# says nothing. It stands in the release lane between a green build and
# a signed image on two registries, and every input it can be handed at
# 2am is one a cell produced while something was already going wrong: a
# log truncated before the row line, a row about the engine that ran
# last week, an `unavailable` from a Hub quota. Each case below drives
# one of those and asserts a REFUSAL, and each has a green twin that
# differs in one field, because a gate that only ever refuses is
# satisfied by refusing everything.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/production-shape-verdict.sh"

pass=0
fail=0

# check <name> <want_exit> <want_substring> <expected-tag> <log line>...
# A log is written verbatim, one argument per line, so a case can drive
# a log with no row line, two of them, or one wrapped in the noise a
# real cell prints around it.
check() {
    local name="$1" want_exit="$2" want_sub="$3" want_tag="$4"; shift 4
    local tmp out got l
    guarded_tmpdir tmp
    : > "$tmp/cell.log"
    for l in "$@"; do printf '%s\n' "$l" >> "$tmp/cell.log"; done

    out="$(bash "$GATE" "$tmp/cell.log" "$want_tag" 2>&1)"
    got=$?

    if [ "$got" -ne "$want_exit" ]; then
        echo "FAIL  $name: exit $got, want $want_exit"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    elif [ -n "$want_sub" ] && ! printf '%s' "$out" | grep -F -- "$want_sub" >/dev/null; then
        echo "FAIL  $name: output does not mention '$want_sub'"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    else
        echo "ok    $name"
        pass=$((pass + 1))
    fi
    rm -rf "$tmp"
}

ROW_PASS='ENGINE_MATRIX_ROW tag=26 engine=26.1.4 api=1.45 result=pass step=complete macvlan=10.0.0.5'
NOISE_A='== starting docker:26-dind'
NOISE_B='== macvlan: 10.0.0.5, ACKed by the DHCP server'

# --- the green direction -----------------------------------------------
check "a passing row for the expected engine releases" 0 "PASS" 26 \
    "$NOISE_A" "$NOISE_B" "$ROW_PASS"

# --- no verdict --------------------------------------------------------
check "a log with no row line refuses" 1 "carries no ENGINE_MATRIX_ROW line" 26 \
    "$NOISE_A" "$NOISE_B"

check "an empty log refuses" 1 "carries no ENGINE_MATRIX_ROW line" 26 \
    ""

check "a row line that is only quoted in prose does not count" 1 "carries no ENGINE_MATRIX_ROW line" 26 \
    "the cell prints ENGINE_MATRIX_ROW tag=26 engine=26.1.4 api=1.45 result=pass step=complete"

check "two verdicts refuse" 1 "One cell, one verdict" 26 \
    "$ROW_PASS" "$ROW_PASS"

# --- the wrong row -----------------------------------------------------
check "a passing row about another engine refuses" 1 "not the production row" 26 \
    'ENGINE_MATRIX_ROW tag=29 engine=29.7.2 api=1.52 result=pass step=complete x'

check "the same log passes when that engine IS the production row" 0 "PASS" 29 \
    'ENGINE_MATRIX_ROW tag=29 engine=29.7.2 api=1.52 result=pass step=complete x'

# --- the results that are not a pass -----------------------------------
check "unavailable refuses, and says it is not a verdict about the plugin" 1 "was not measured at all" 26 \
    'ENGINE_MATRIX_ROW tag=26 engine=unknown api=unknown result=unavailable step=engine-control x'

check "a failing row refuses and names the step" 1 "at step 'network-create-macvlan'" 26 \
    'ENGINE_MATRIX_ROW tag=26 engine=26.1.4 api=1.45 result=fail step=network-create-macvlan refused'

check "the synthesized no-verdict row refuses" 1 "reached no verdict" 26 \
    'ENGINE_MATRIX_ROW tag=26 engine=unknown api=unknown result=no-verdict step=unknown'

check "a result nobody has written yet refuses" 1 "reached no verdict" 26 \
    'ENGINE_MATRIX_ROW tag=26 engine=26.1.4 api=1.45 result=probably step=complete x'

check "a row line with no result field refuses" 1 "reached no verdict" 26 \
    'ENGINE_MATRIX_ROW tag=26 engine=26.1.4 api=1.45 step=complete x'

# --- cannot check ------------------------------------------------------
guarded_tmpdir tmp
out="$(bash "$GATE" "$tmp/no-such-log" 26 2>&1)"; got=$?
if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -F "is not readable" >/dev/null; then
    echo "ok    a missing log is 'cannot check', not a pass"
    pass=$((pass + 1))
else
    echo "FAIL  missing log: exit $got, output '$out'"
    fail=$((fail + 1))
fi
rm -rf "$tmp"

out="$(bash "$GATE" 2>&1)"; got=$?
if [ "$got" -eq 2 ]; then
    echo "ok    no arguments is 'cannot check', not a pass"
    pass=$((pass + 1))
else
    echo "FAIL  no arguments: exit $got, output '$out'"
    fail=$((fail + 1))
fi

guarded_tmpdir tmp
printf '%s\n' "$ROW_PASS" > "$tmp/cell.log"
out="$(bash "$GATE" "$tmp/cell.log" 2>&1)"; got=$?
if [ "$got" -eq 2 ]; then
    echo "ok    a log with no expected row named is 'cannot check', not a pass"
    pass=$((pass + 1))
else
    echo "FAIL  no expected row: exit $got, output '$out'"
    fail=$((fail + 1))
fi
rm -rf "$tmp"

echo
echo "production-shape-verdict self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
