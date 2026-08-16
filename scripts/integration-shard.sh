#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Partition the main integration suite across jobs (#381).
#
# After #375 the gate is max(main, failure) and the main suite is the
# critical path: ~540s against the failure suite's ~495s. Nothing else
# improves the gate until main comes down.
#
# The suite is serial by design and must stay so IN-PROCESS — there is
# no t.Parallel() anywhere, and TestRecovery_DaemonRestart_PreservesContainer
# restarts the daemon, dropping every Docker connection on the host
# including a neighbour's. Sharding across separate JOBS does not touch
# that: each shard is its own runner, own daemon, own fixture, exactly
# the isolation stage 1 already relies on.
#
# Usage: integration-shard.sh <index> <total>
#   prints a `go test -run` regex selecting this shard's tests.
#
# Exit: 0 with a regex on stdout, 2 on bad usage or an empty partition.
#
# THE PROPERTY THAT MATTERS is not balance, it is completeness: every
# test must land in exactly one shard. A test assigned to none is
# silently never run, and the suite goes green having tested less —
# which is the failure this whole milestone keeps finding in other
# shapes. scripts/test-integration-shard.sh asserts the union across all
# shards equals the full list, for several values of <total>.
set -uo pipefail

# THE PARTITION MUST BE A FUNCTION OF THE TREE, NOT OF WHO RUNS IT (#554).
#
# Without this the same commit produced a different partition depending
# on the maintainer's locale. Measured: shard 3 of 4 held three
# different tests under de_DE.UTF-8 than under C — one of them the test
# somebody was at that moment failing to reproduce a CI failure in.
#
# Three separate mechanisms, which is why the fix is a blanket export
# rather than a flag on one command:
#
#   1. awk PARSES the durations file with a locale decimal separator.
#      Under a comma-decimal locale `$2` of `12.5` is read as 12 — every
#      duration silently truncated to its integer part, so the packer
#      optimises against numbers that are simply wrong. The computed
#      mean differs outright: 6.48 under C, 6,21 under de_DE.
#   2. awk's printf "%.2f" then EMITS a comma, and `sort -rn` reads
#      "6,21" as 6, truncating a second time.
#   3. Ties (every test absent from the durations file gets the mean)
#      fall to sort's last-resort whole-line comparison, which is
#      locale-collated.
#
# Only the third is visible by reading the sort; the first two are the
# ones that actually moved tests between shards.
export LC_ALL=C

IDX="${1:-}"
TOTAL="${2:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
SUITE_DIR="$ROOT/test/integration"
DURATIONS="$SUITE_DIR/testdata/main-suite-durations.tsv"

case "$IDX$TOTAL" in
    *[!0-9]*|"") echo "usage: $0 <index> <total>   (index is 1-based)" >&2; exit 2 ;;
esac
[ "$IDX" -ge 1 ] && [ "$IDX" -le "$TOTAL" ] || { echo "index $IDX out of range 1..$TOTAL" >&2; exit 2; }

# Test names come from the sources, not `go test -list`: listing runs
# TestMain, which requires root, and a partitioner that needs root to
# decide what to run is useless in half the places it is wanted.
mapfile -t ALL < <(
    grep -hoE '^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)' "$SUITE_DIR"/*_test.go 2>/dev/null \
    | sed -E 's/^func (Test[A-Za-z0-9_]+)\(.*/\1/' \
    | grep -v '^TestFailure_' \
    | sort -u
)

if [ "${#ALL[@]}" -eq 0 ]; then
    echo "no main-suite tests found in $SUITE_DIR — refusing to emit a regex that would run nothing" >&2
    exit 2
fi

# Greedy longest-first bin packing against measured durations. A test
# absent from the durations file gets the mean, so a stale file makes
# shards less even and never makes one incomplete.
mean=$(awk -F'\t' '$1 !~ /^#/ && NF==2 {s+=$2; n++} END {if (n) printf "%.2f", s/n; else print "1"}' "$DURATIONS" 2>/dev/null || echo 1)

assigned=$(
    for t in "${ALL[@]}"; do
        d=$(awk -F'\t' -v k="$t" '$1==k {print $2; found=1; exit} END {if (!found) print ""}' "$DURATIONS" 2>/dev/null)
        [ -z "$d" ] && d="$mean"
        printf '%s\t%s\n' "$d" "$t"
    # Explicit keys: duration descending, then name ascending. Every
    # test absent from the durations file gets the mean and so TIES on
    # the numeric key; without a second key those ties fall to sort's
    # last-resort whole-line comparison. LC_ALL=C is exported at the top
    # and is what keeps that comparison stable (#554).
    done | sort -t"$(printf '\t')" -k1,1rn -k2,2 | awk -v total="$TOTAL" '
        BEGIN { for (i = 1; i <= total; i++) load[i] = 0 }
        {
            # Smallest-load bin wins; ties go to the lowest index. This
            # is deterministic only because the input order above is —
            # see the sort. Both halves are needed.
            best = 1
            for (i = 2; i <= total; i++) if (load[i] < load[best]) best = i
            load[best] += $1
            print best "\t" $2
        }'
)

# This sort decides the order of the alternation in the emitted regex,
# so it is covered by the same LC_ALL=C export: membership would survive
# a locale change but the emitted string would not, and "same tree, same
# shard, same bytes" is the property the self-test pins (#554).
mine=$(printf '%s\n' "$assigned" | awk -F'\t' -v i="$IDX" '$1==i {print $2}' | sort)

if [ -z "$mine" ]; then
    echo "shard $IDX of $TOTAL is empty — more shards than tests?" >&2
    exit 2
fi

# Anchored alternation so TestFoo does not also select TestFooBar.
printf '^(%s)$\n' "$(printf '%s\n' "$mine" | paste -sd'|' -)"
