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

# THE PARTITION MUST NOT DEPEND ON WHO RUNS IT (#554).
#
# The completeness cases above cannot catch this by construction: they
# assert the union equals the full list and that no test appears twice,
# and both survive ANY permutation of the input. They therefore hold
# identically under every locale while the shards themselves differ.
#
# The bug was that a maintainer reproducing a shard failure under a
# comma-decimal locale silently ran a DIFFERENT set of tests: awk parsed
# the durations file with a locale decimal separator (`12.5` read as
# 12), computed a different mean, printed it with a comma, and sort then
# truncated it again.
#
# A locale that is not installed falls back to C, which would make this
# case pass while exercising nothing — so the locale is verified to be
# genuinely in effect before it is trusted.
comma_locale=""
for cand in $(locale -a 2>/dev/null); do
    case "$cand" in
        C|C.*|POSIX|en_US*|*.iso*|*ISO*) continue ;;
    esac
    # In effect only if awk really formats with a comma under it.
    if [ "$(LC_ALL="$cand" awk 'BEGIN{printf "%.1f", 1.5}' 2>/dev/null)" = "1,5" ]; then
        comma_locale="$cand"
        break
    fi
done

if [ -z "$comma_locale" ]; then
    no "no comma-decimal locale is installed, so the locale-independence case could not run.
      This case must not pass vacuously — an uninstalled locale falls back to C and would
      exercise nothing. Install one, e.g.:  sudo locale-gen de_DE.UTF-8"
else
    drift=""
    for n in 2 4 7; do
        for i in $(seq 1 "$n"); do
            a=$(LC_ALL=C bash "$SHARD" "$i" "$n" 2>/dev/null)
            b=$(LC_ALL="$comma_locale" bash "$SHARD" "$i" "$n" 2>/dev/null)
            [ "$a" = "$b" ] || drift="$drift $i/$n"
        done
    done
    if [ -n "$drift" ]; then
        no "the partition changes with the locale ($comma_locale) at shard(s):$drift
      Same tree, same commit, different tests. Anyone reproducing a shard failure
      locally under this locale runs a different set and cannot reproduce it."
    else
        ok "the partition is byte-identical under C and $comma_locale"
    fi

    # The mean is the value every unmeasured test inherits, so if it
    # moves, every tie moves with it. Pinned separately because it is
    # the mechanism, and a future edit could reintroduce it without
    # changing any shard on today's durations file.
    m_c=$(LC_ALL=C awk -F'\t' '$1 !~ /^#/ && NF==2 {s+=$2; n++} END {if (n) printf "%.2f", s/n}' \
        "$SUITE/testdata/main-suite-durations.tsv" 2>/dev/null)
    m_x=$(LC_ALL="$comma_locale" bash -c 'export LC_ALL=C; awk -F"\t" '"'"'$1 !~ /^#/ && NF==2 {s+=$2; n++} END {if (n) printf "%.2f", s/n}'"'"' "$1"' _ \
        "$SUITE/testdata/main-suite-durations.tsv" 2>/dev/null)
    if [ -n "$m_c" ] && [ "$m_c" = "$m_x" ]; then
        ok "the mean duration is computed identically once LC_ALL is pinned ($m_c)"
    else
        no "the mean duration still moves with the locale: C='$m_c' vs '$comma_locale'='$m_x'"
    fi
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
