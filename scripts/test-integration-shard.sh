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
if names_of 1 1 | grep '^TestFailure_' >/dev/null; then
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
# different locale silently ran a DIFFERENT set of tests. Measured:
# shard 3 of 4 held three different tests under de_DE.UTF-8 than under
# C, one of them the test being investigated at the time.
#
# WHAT THIS CASE REQUIRES, AND WHY IT IS NOT "A COMMA-DECIMAL LOCALE".
# The property is that the partition is a function of the tree alone, so
# all that is needed to test it is SOME locale other than C to compare
# against. An earlier version of this case demanded a locale under which
# awk formats 1.5 as "1,5", and failed on the hosted runner — whose awk
# ignores LC_NUMERIC entirely, so no installed locale could ever satisfy
# it. That is an environment in which half the bug cannot exist, not an
# environment where the check may be skipped: the collation half still
# applies everywhere, and it is asserted unconditionally below.
alt_locale=""
for cand in $(locale -a 2>/dev/null); do
    case "$cand" in
        C|C.*|POSIX|*.iso*|*ISO*) continue ;;
    esac
    alt_locale="$cand"
    # Prefer one whose collation actually differs from C, since that is
    # the half every awk exhibits.
    break
done

if [ -z "$alt_locale" ]; then
    no "no locale other than C is installed, so locale-independence could not be tested.
      This case must not pass having compared C against itself. Install one, e.g.:
      sudo locale-gen de_DE.UTF-8"
else
    drift=""
    for n in 2 4 7; do
        for i in $(seq 1 "$n"); do
            a=$(LC_ALL=C bash "$SHARD" "$i" "$n" 2>/dev/null)
            b=$(LC_ALL="$alt_locale" bash "$SHARD" "$i" "$n" 2>/dev/null)
            [ "$a" = "$b" ] || drift="$drift $i/$n"
        done
    done
    if [ -n "$drift" ]; then
        no "the partition changes with the locale ($alt_locale) at shard(s):$drift
      Same tree, same commit, different tests. Anyone reproducing a shard failure
      locally under this locale runs a different set and cannot reproduce it."
    else
        ok "the partition is byte-identical under C and $alt_locale"
    fi

    # The decimal half, asserted only where the environment can express
    # it. awk implementations differ: some honour LC_NUMERIC for input
    # fields and printf, some ignore it entirely. Where it is ignored the
    # mean cannot move, so demanding that it moves would fail a correct
    # tree; where it is honoured, this is the mechanism that actually
    # reshuffled the shards and it is pinned.
    if [ "$(LC_ALL="$alt_locale" awk 'BEGIN{printf "%.1f", 1.5}' 2>/dev/null)" = "1,5" ]; then
        m_c=$(LC_ALL=C awk -F'\t' '$1 !~ /^#/ && NF==2 {s+=$2; n++} END {if (n) printf "%.2f", s/n}' \
            "$SUITE/testdata/main-suite-durations.tsv" 2>/dev/null)
        # Ambient locale is the alt one; the pin the script applies must
        # make the result identical anyway. That is the whole fix.
        m_x=$(LC_ALL="$alt_locale" bash -c 'export LC_ALL=C; awk -F"\t" '"'"'$1 !~ /^#/ && NF==2 {s+=$2; n++} END {if (n) printf "%.2f", s/n}'"'"' "$1"' _ \
            "$SUITE/testdata/main-suite-durations.tsv" 2>/dev/null)
        if [ -n "$m_c" ] && [ "$m_c" = "$m_x" ]; then
            ok "the mean duration is computed identically once LC_ALL is pinned ($m_c)"
        else
            no "the mean duration still moves with the locale: C='$m_c' vs '$alt_locale'='$m_x'"
        fi
    else
        ok "this awk ignores LC_NUMERIC, so the decimal half cannot arise here (collation half asserted above)"
    fi
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
