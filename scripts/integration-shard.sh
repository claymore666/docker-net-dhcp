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
    done | sort -rn | awk -v total="$TOTAL" '
        BEGIN { for (i = 1; i <= total; i++) load[i] = 0 }
        {
            # Smallest-load bin wins; ties go to the lowest index so the
            # partition is deterministic across runs and machines.
            best = 1
            for (i = 2; i <= total; i++) if (load[i] < load[best]) best = i
            load[best] += $1
            print best "\t" $2
        }'
)

mine=$(printf '%s\n' "$assigned" | awk -F'\t' -v i="$IDX" '$1==i {print $2}' | sort)

if [ -z "$mine" ]; then
    echo "shard $IDX of $TOTAL is empty — more shards than tests?" >&2
    exit 2
fi

# Anchored alternation so TestFoo does not also select TestFooBar.
printf '^(%s)$\n' "$(printf '%s\n' "$mine" | paste -sd'|' -)"
