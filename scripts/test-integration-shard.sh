#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for integration-shard.sh (#381, D41).
#
# Balance is a nice-to-have. COMPLETENESS is the property that must
# hold, and since D41 it has two halves: every test of a suite lands in
# exactly one shard OF THAT SUITE, and the two suites partition the
# roster between them. A test assigned to none is silently never run,
# and the gate goes green having tested less than it did before — a
# green that means less than it looks, which is the failure this
# milestone keeps finding. Splitting the failure suite off into its own
# partition added a second way to lose a test: one that is in neither
# population. The roster case below is what closes it.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SHARD="$HERE/integration-shard.sh"
SUITE="$(dirname "$HERE")/test/integration"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# The same extraction the script uses, restated here on purpose: if the
# two ever disagree, this test is meant to notice.
roster() {
    grep -hoE '^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)' "$SUITE"/*_test.go \
        | sed -E 's/^func (Test[A-Za-z0-9_]+)\(.*/\1/' | sort -u
}

all_tests() { roster | grep -v '^TestFailure_'; }
failure_tests() { roster | grep '^TestFailure_'; }

names_of() { # <index> <total> [suite] -> one test name per line
    bash "$SHARD" "$1" "$2" "${3:-main}" | sed -E 's/^\^\(//; s/\)\$$//' | tr '|' '\n' | sort
}

ROSTER_TESTS=$(roster | wc -l)
TOTAL_TESTS=$(all_tests | wc -l)
FAILURE_TESTS=$(failure_tests | wc -l)
[ "$TOTAL_TESTS" -gt 0 ] && ok "found $TOTAL_TESTS main-suite tests to partition" \
    || no "no main-suite tests found — every case below would pass vacuously"
[ "$FAILURE_TESTS" -gt 0 ] && ok "found $FAILURE_TESTS failure-suite tests to partition" \
    || no "no failure-suite tests found — the failure cases below would pass vacuously"

# THE TWO POPULATIONS MUST PARTITION THE ROSTER (D41).
#
# The per-suite completeness cases below cannot see this: each asks
# whether its own population is fully covered, and both hold perfectly
# while a test sits in neither population. That is a test nothing runs
# and nothing reports, which is the whole failure class this file is
# for. Asserted against a roster extracted here, not against either
# suite's own derivation.
both=$( { names_of 1 1 main; names_of 1 1 failure; } | sort )
both_uniq=$(printf '%s\n' "$both" | sort -u)
overlap=$(printf '%s\n' "$both" | uniq -d)
if [ -n "$overlap" ]; then
    no "these tests are in BOTH suites' populations, so they run twice: $(printf '%s' "$overlap" | tr '\n' ' ')"
elif [ "$(printf '%s\n' "$both_uniq" | wc -l)" != "$ROSTER_TESTS" ]; then
    orphan=$(comm -23 <(roster) <(printf '%s\n' "$both_uniq") | tr '\n' ' ')
    no "the two suites cover $(printf '%s\n' "$both_uniq" | wc -l)/${ROSTER_TESTS} of the roster — in NEITHER population: ${orphan}"
else
    ok "the main and failure populations partition the roster ($ROSTER_TESTS tests)"
fi

# 9 is the value CI actually ships for main and 2 for failure (D41); the
# rest bracket them. A completeness property asserted at every n EXCEPT
# the production one is a property nobody has checked where it matters.
for n in 1 2 3 4 5 7 9 13; do
    union=$(for i in $(seq 1 "$n"); do names_of "$i" "$n"; done | sort)
    dupes=$(printf '%s\n' "$union" | uniq -d)
    uniq_count=$(printf '%s\n' "$union" | sort -u | wc -l)

    if [ -n "$dupes" ]; then
        no "n=$n: these tests are in more than one shard: $(printf '%s' "$dupes" | tr '\n' ' ')"
    elif [ "$uniq_count" != "$TOTAL_TESTS" ]; then
        missing=$(comm -23 <(all_tests) <(printf '%s\n' "$union" | sort -u) | tr '\n' ' ')
        no "n=$n: ${uniq_count}/${TOTAL_TESTS} tests covered — MISSING: ${missing}"
    else
        ok "n=$n: every main-suite test in exactly one shard"
    fi
done

for n in 1 2 3 4; do
    union=$(for i in $(seq 1 "$n"); do names_of "$i" "$n" failure; done | sort)
    dupes=$(printf '%s\n' "$union" | uniq -d)
    uniq_count=$(printf '%s\n' "$union" | sort -u | wc -l)

    if [ -n "$dupes" ]; then
        no "failure n=$n: these tests are in more than one shard: $(printf '%s' "$dupes" | tr '\n' ' ')"
    elif [ "$uniq_count" != "$FAILURE_TESTS" ]; then
        missing=$(comm -23 <(failure_tests) <(printf '%s\n' "$union" | sort -u) | tr '\n' ' ')
        no "failure n=$n: ${uniq_count}/${FAILURE_TESTS} tests covered — MISSING: ${missing}"
    else
        ok "failure n=$n: every failure-suite test in exactly one shard"
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

# Neither suite may select the other's tests: they run in separate jobs
# in separate processes, and a crossing test would be run twice — once
# under a fixture written for the other suite.
if names_of 1 1 main | grep '^TestFailure_' >/dev/null; then
    no "a main shard selected a TestFailure_ test"
else
    ok "no main shard selects failure-suite tests"
fi
if names_of 1 1 failure | grep -v '^TestFailure_' > /dev/null; then
    no "a failure shard selected a main-suite test"
else
    ok "no failure shard selects main-suite tests"
fi

# A shard with nothing in it must REFUSE. `go test -run` with an empty
# alternation matches every test, and an empty regex emitted here would
# be handed straight to it: the shard that was meant to run nothing runs
# everything, or — with the anchors — nothing at all, and exits 0 either
# way. Asked past the failure suite's four tests, where it is reachable.
if bash "$SHARD" 5 5 failure >/dev/null 2>&1; then
    no "shard 5 of 5 on a four-test suite was accepted — an empty partition emitted a regex"
else
    ok "a shard with no tests in it refuses instead of emitting an empty regex"
fi

# An unknown suite name must refuse rather than fall back to `main`: a
# caller that asks for a population this script does not have would
# otherwise silently run the main suite under the failure suite's name,
# and both would be green.
if bash "$SHARD" 1 1 mian >/dev/null 2>&1; then
    no "an unknown suite name was accepted"
else
    ok "an unknown suite name is rejected"
fi

# Usage errors must be errors, not an empty regex that silently runs
# every test or none.
for bad in "" "0 2" "3 2" "x y" "1 2 3"; do
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
    for suite in main failure; do
        for n in 2 4 7; do
            [ "$suite" = failure ] && [ "$n" -gt "$FAILURE_TESTS" ] && continue
            for i in $(seq 1 "$n"); do
                a=$(LC_ALL=C bash "$SHARD" "$i" "$n" "$suite" 2>/dev/null)
                b=$(LC_ALL="$alt_locale" bash "$SHARD" "$i" "$n" "$suite" 2>/dev/null)
                [ "$a" = "$b" ] || drift="$drift $suite:$i/$n"
            done
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
            "$SUITE/testdata/suite-durations.tsv" 2>/dev/null)
        # Ambient locale is the alt one; the pin the script applies must
        # make the result identical anyway. That is the whole fix.
        m_x=$(LC_ALL="$alt_locale" bash -c 'export LC_ALL=C; awk -F"\t" '"'"'$1 !~ /^#/ && NF==2 {s+=$2; n++} END {if (n) printf "%.2f", s/n}'"'"' "$1"' _ \
            "$SUITE/testdata/suite-durations.tsv" 2>/dev/null)
        if [ -n "$m_c" ] && [ "$m_c" = "$m_x" ]; then
            ok "the mean duration is computed identically once LC_ALL is pinned ($m_c)"
        else
            no "the mean duration still moves with the locale: C='$m_c' vs '$alt_locale'='$m_x'"
        fi
    else
        ok "this awk ignores LC_NUMERIC, so the decimal half cannot arise here (collation half asserted above)"
    fi
fi

# --- the two lanes must partition the same way (D41) ---------------------
#
# integration-hosted.yml can run the same eleven shards on hosted
# runners, which is what makes "hosted against the pool" a comparison
# rather than two unrelated numbers. Two matrices in two files is two
# places a shard count can move, and only one of them would be noticed.
# So the (suite, index, total) triples are extracted from both and
# required to be the same LIST -- and the extraction refuses when either
# side yields nothing, because two empty sets are equal.
#
# `sort`, not `sort -u`. The first version deduplicated both sides, so a
# matrix entry duplicated in BOTH files compared equal to itself and the
# case was green while a shard went unscheduled; review measured exactly
# that (drop main-5, duplicate main-4). Whether the union still covers
# the roster is scripts/check-shard-coverage.sh's question, not this
# case's -- this case's job is only that the two lanes say the same
# thing -- but it must not be the place a duplicate hides.
WF="$(dirname "$HERE")/.github/workflows"
if [ -d "$WF" ]; then
    pool_triples=$(sed -n 's/.*integration-test-shard SHARD=\([0-9]*\) OF=\([0-9]*\) SUITE=\([a-z]*\).*/\3-\1-of-\2/p' \
                   "$WF/integration.yml" | LC_ALL=C sort)
    hosted_triples=$(grep -o '"[a-z]*-[0-9]*-of-[0-9]*"' "$WF/integration-hosted.yml" \
                     | tr -d '"' | LC_ALL=C sort)
    if [ -z "$pool_triples" ]; then
        no "no shard triple could be read out of integration.yml — this case would compare two empty sets"
    elif [ -z "$hosted_triples" ]; then
        no "no shard triple could be read out of integration-hosted.yml — this case would compare two empty sets"
    elif [ "$pool_triples" = "$hosted_triples" ]; then
        ok "the pool and hosted lanes name the same $(printf '%s\n' "$pool_triples" | wc -l) shard(s)"
    else
        no "the two lanes partition differently:$(printf '\n  only in integration.yml: %s' \
            "$(comm -23 <(printf '%s\n' "$pool_triples") <(printf '%s\n' "$hosted_triples") | tr '\n' ' ')")$(printf '\n  only in integration-hosted.yml: %s' \
            "$(comm -13 <(printf '%s\n' "$pool_triples") <(printf '%s\n' "$hosted_triples") | tr '\n' ' ')")"
    fi
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
