#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Guards the tee added to the integration-suite targets (#378).
#
# Teeing a test run is not a cosmetic change: get it wrong and the
# suite reports tee's exit 0 instead of go test's failure, and the gate
# goes green over a red run. That is not hypothetical — it is exactly
# how a broken feature shipped behind a green CI gate for a month
# (#297), caught only by the one lane that did not tee.
#
# Two halves, and both are needed:
#
#   A. the mechanism — prove pipefail is what makes the difference,
#      so the reason for the wrapper is demonstrated, not asserted;
#   B. the real Makefile — prove THIS repo's recipes actually use it.
#      (A) alone would pass happily against a Makefile that had lost
#      the wrapper, which is the trap of testing a construct in
#      isolation rather than the invocation path that ships.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; failures=$((failures + 1)); }

# ---------------------------------------------------------------
# A. The mechanism
# ---------------------------------------------------------------

# The naive form: sh has no pipefail, so a failing left-hand side is
# masked by tee's success. This asserts the BUG exists in the form we
# are avoiding — if this ever starts failing, /bin/sh grew pipefail
# and the rationale below needs revisiting.
sh -c 'false 2>&1 | tee '"$TMP/a.log" >/dev/null 2>&1
if [ $? -eq 0 ]; then
    pass "naive 'cmd | tee' under sh masks failure (the #297 mechanism)"
else
    fail "naive 'cmd | tee' under sh did NOT mask failure — rationale needs review"
fi

# The form the Makefile uses: the pipeline's status is the failing
# left-hand side, so make sees a non-zero exit and stops.
bash -o pipefail -c 'false 2>&1 | tee '"$TMP/b.log" >/dev/null 2>&1
if [ $? -ne 0 ]; then
    pass "'bash -o pipefail -c' propagates the failure"
else
    fail "'bash -o pipefail -c' swallowed the failure"
fi

# ...and still writes the log. A wrapper that propagated failure but
# dropped the output would defeat the point of the change.
bash -o pipefail -c 'printf "hello\n"; false' > "$TMP/c.log" 2>&1
if grep -q hello "$TMP/c.log"; then
    pass "output is still captured on a failing run"
else
    fail "output was lost on a failing run"
fi

# ---------------------------------------------------------------
# B. The real Makefile
# ---------------------------------------------------------------
# `make -n` expands variables and prints the recipe without running
# it, so this checks the shipped recipe rather than a copy of it.

for target in integration-test integration-test-failure; do
    if ! out="$(make -C "$ROOT" -n "$target" 2>&1)"; then
        fail "$target: make -n failed
$(echo "$out" | sed 's/^/    /')"
        continue
    fi

    if grep -q 'bash -o pipefail -c' <<<"$out"; then
        pass "$target: uses the pipefail wrapper"
    else
        fail "$target: NO pipefail wrapper — a failing suite would report green (#297)"
    fi

    if grep -qE 'tee [^|]*\.log' <<<"$out"; then
        pass "$target: tees to a log file"
    else
        fail "$target: does not tee to a log file (#378)"
    fi

    # The path must be announced before the run, not after: make stops
    # at the failing line, so a trailing echo never prints on exactly
    # the outcome where the log matters most.
    echo_line="$(grep -n '==> test output:' <<<"$out" | head -1 | cut -d: -f1)"
    test_line="$(grep -n 'go test' <<<"$out" | head -1 | cut -d: -f1)"
    if [ -n "$echo_line" ] && [ -n "$test_line" ] && [ "$echo_line" -lt "$test_line" ]; then
        pass "$target: log path is printed before the run"
    else
        fail "$target: log path is not printed before the run (echo=${echo_line:-none} test=${test_line:-none})"
    fi
done

echo
if [ "$failures" -eq 0 ]; then
    echo "all checks passed"
    exit 0
fi
echo "$failures check(s) failed"
exit 1
