#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-fuzz-budget.sh (#324). Feeds synthetic
# workflow files through the FUZZ_WORKFLOW seam and asserts the verdict.
#
# The cases that matter most are the two blindness guards: a fuzz step
# that has vanished must exit 2 (not 0), and a malformed budget must be
# judged rather than skipped. A gate that silently sees nothing is the
# failure mode this repo has shipped twice.
set -u

CHECK="$(dirname "$0")/check-fuzz-budget.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
# check NAME WANT_EXIT WORKFLOW_BODY GREP_PATTERN
check() {
    local name="$1" want_exit="$2" body="$3" want_grep="$4"
    printf '%s\n' "$body" > "$TMP/wf.yaml"
    FUZZ_WORKFLOW="$TMP/wf.yaml" bash "$CHECK" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

check "execution budget with a timeout passes" 0 \
"          go test ./pkg/dhcp/ -fuzz X -fuzztime 200000x -timeout 5m" \
"bounded execution budget"

check "wall-clock budget is rejected" 1 \
"          go test ./pkg/dhcp/ -fuzz X -fuzztime 20s -timeout 5m" \
"wall-clock budget"

check "wall-clock in minutes is rejected too" 1 \
"          go test ./pkg/dhcp/ -fuzz X -fuzztime 2m -timeout 5m" \
"wall-clock budget"

check "execution budget without a -timeout is rejected" 1 \
"          go test ./pkg/dhcp/ -fuzz X -fuzztime 200000x" \
"no -timeout"

check "a non-numeric count is judged, not waved through" 1 \
"          go test ./pkg/dhcp/ -fuzz X -fuzztime abcx -timeout 5m" \
"not a valid execution count"

check "-fuzztime= spelling is understood" 0 \
"          go test ./pkg/dhcp/ -fuzz X -fuzztime=200000x -timeout=5m" \
"bounded execution budget"

# Blindness guard 1: the step is gone entirely.
check "no fuzz invocation at all exits 2, loudly" 2 \
"          go test ./... -race" \
"watching nothing"

# Blindness guard 2: prose must not satisfy or trip the gate.
check "a comment mentioning -fuzztime neither passes nor fails the gate" 2 \
"          # never use a wall-clock -fuzztime 20s here" \
"watching nothing"

check "a comment alongside a real invocation does not double-report" 0 \
"          # -fuzztime 20s used to be the shape here
          go test ./pkg/dhcp/ -fuzz X -fuzztime 200000x -timeout 5m" \
"1 fuzz invocation"

# Several invocations: every one is judged, not just the first.
check "a second, bad invocation is caught behind a good one" 1 \
"          go test ./pkg/dhcp/ -fuzz A -fuzztime 200000x -timeout 5m
          go test ./pkg/dhcp/ -fuzz B -fuzztime 30s -timeout 5m" \
"wall-clock budget"

FUZZ_WORKFLOW="$TMP/does-not-exist.yaml" bash "$CHECK" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "does not exist" "$TMP/out"; then
    echo "PASS: a missing workflow file exits 2"
else
    echo "FAIL: a missing workflow file exits 2"
    failures=$((failures + 1))
fi

# The real workflow must satisfy its own gate.
if (cd "$(dirname "$0")/.." && bash scripts/check-fuzz-budget.sh > "$TMP/real" 2>&1); then
    echo "PASS: the committed workflow passes the gate"
else
    echo "FAIL: the committed workflow does not pass the gate"
    sed 's/^/    /' "$TMP/real"
    failures=$((failures + 1))
fi

total=$((failures == 0 ? 0 : failures))
if [ "$total" -eq 0 ]; then
    echo "all check-fuzz-budget tests passed"
    exit 0
fi
echo "$failures failed"
exit 1
