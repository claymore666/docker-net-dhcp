#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for integration-shard.sh (#381).
#
# Balance is a nice-to-have. COMPLETENESS is the property that must
# hold: every main-suite test lands in exactly one shard. A test
# assigned to none is silently never run, and the gate goes green having
# tested less than it did before — a green that means less than it
# looks, which is the failure this milestone keeps finding.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SHARD="$HERE/integration-shard.sh"
SUITE="$(dirname "$HERE")/test/integration"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# The same extraction the script uses, restated here on purpose: if the
# two ever disagree, this test is meant to notice.
all_tests() {
    grep -hoE '^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)' "$SUITE"/*_test.go \
        | sed -E 's/^func (Test[A-Za-z0-9_]+)\(.*/\1/' \
        | grep -v '^TestFailure_' | sort -u
}

names_of() { # <index> <total> -> one test name per line
    bash "$SHARD" "$1" "$2" | sed -E 's/^\^\(//; s/\)\$$//' | tr '|' '\n' | sort
}

TOTAL_TESTS=$(all_tests | wc -l)
[ "$TOTAL_TESTS" -gt 0 ] && ok "found $TOTAL_TESTS main-suite tests to partition" \
    || no "no tests found — every case below would pass vacuously"

for n in 1 2 3 4 7 13; do
    union=$(for i in $(seq 1 "$n"); do names_of "$i" "$n"; done | sort)
    dupes=$(printf '%s\n' "$union" | uniq -d)
    uniq_count=$(printf '%s\n' "$union" | sort -u | wc -l)

    if [ -n "$dupes" ]; then
        no "n=$n: these tests are in more than one shard: $(printf '%s' "$dupes" | tr '\n' ' ')"
    elif [ "$uniq_count" != "$TOTAL_TESTS" ]; then
        missing=$(comm -23 <(all_tests) <(printf '%s\n' "$union" | sort -u) | tr '\n' ' ')
        no "n=$n: ${uniq_count}/${TOTAL_TESTS} tests covered — MISSING: ${missing}"
    else
        ok "n=$n: every test in exactly one shard"
    fi
done

# Determinism: the same shard must select the same tests every time, or
# a re-run silently tests something different from what went green.
a=$(names_of 2 3); b=$(names_of 2 3)
[ "$a" = "$b" ] && ok "partitioning is deterministic across invocations" \
    || no "shard 2/3 differed between two runs"

# The regex must be anchored, or TestFoo also selects TestFooBar and a
# shard runs a neighbour's test twice while its owner runs it too.
case "$(bash "$SHARD" 1 2)" in
    '^('*')$') ok "the emitted regex is anchored" ;;
    *) no "regex is not anchored — prefix names would cross-select" ;;
esac

# No shard may select a failure-suite test: those run in their own job
# with their own fixture, and pulling one in here would run it twice.
if names_of 1 1 | grep -q '^TestFailure_'; then
    no "a shard selected a TestFailure_ test"
else
    ok "no shard selects failure-suite tests"
fi

# Usage errors must be errors, not an empty regex that silently runs
# every test or none.
for bad in "" "0 2" "3 2" "x y"; do
    # shellcheck disable=SC2086
    if bash "$SHARD" $bad >/dev/null 2>&1; then
        no "bad usage '$bad' was accepted"
    else
        ok "bad usage '$bad' rejected"
    fi
done

# The Makefile shard target must also run the harness package. Three of
# its files are integration-tagged and today run only via the unsharded
# main target's ./... glob; a -run regex naming suite tests matches none
# of them. Without this the shard job would drop a whole package —
# including the guards that stop a hand-rolled counter read (#405) and a
# bare HostConfig literal (#367) coming back — and stay green.
MK="$(dirname "$HERE")/Makefile"
if grep -qE 'go test .*-tags integration.*\./test/integration/harness/' "$MK"; then
    ok "the shard target also runs the harness package"
else
    no "the shard target does not run ./test/integration/harness/ — its integration-tagged guards would never execute"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
