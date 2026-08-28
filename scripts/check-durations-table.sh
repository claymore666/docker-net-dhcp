#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The shard-balance table must name exactly the tests it balances (#877).
#
# WHY THIS EXISTS
#
# scripts/integration-shard.sh partitions the main integration suite by
# measured duration, read from
# test/integration/testdata/main-suite-durations.tsv. A test with no row
# is NOT dropped — it is costed at the mean — so the file going stale
# never makes a shard incomplete. It makes the shards UNEVEN, and the
# gate is max() over the shards, so uneven is the whole cost.
#
# Nothing noticed. Measured 2026-08-28 at the time #877 was written, the
# file was one run from 2026-08-02 and had drifted both ways at once:
#
#   - 18 of the 70 main-suite tests had no row and ran on a mean guess.
#     The three shards had drifted to 321.9/318.2/239.1s of test time and the
#     longest job to 8m31, against a ~5-minute design.
#   - 38 rows named tests that live in test/integration/harness/ rather
#     than in the suite. They came from an old unsharded log that ran
#     both packages. They are never selected, so they look harmless —
#     but the mean is the average of the ROWS, they were nearly all
#     0.00, and they therefore pulled the number the 18 absent tests
#     were costed at DOWN. Two independent drifts, compounding.
#
# So the check is two-sided on purpose. A missing row is the failure the
# issue names; a stray row is the one that made it worse and would have
# passed a one-sided check.
#
# WHAT IT CHECKS
#
#   1. the table names every test the partitioner will place
#   2. the table names nothing else
#   3. every row is well formed: one name, one non-negative number
#   4. no name appears twice
#
# THE POPULATION IS ASKED OF THE PARTITIONER, NOT RE-DERIVED HERE.
# integration-shard.sh <n> <n> emits exactly the tests it will place, so
# this gate runs it rather than re-implementing its `grep` over the
# sources. A copy of that derivation would be one more thing to keep in
# step, and the failure mode of a stale copy is that the gate goes green
# over a population nobody partitions.
#
# WHAT IT DOES NOT COVER. TestFailure_* runs in its own unsharded job
# and TestMain is not selectable, so neither is balanced and neither
# belongs in the table — a row for one is caught by rule 2. Completeness
# of the partition itself (every test in exactly one shard) is
# scripts/test-integration-shard.sh's job, and is asserted independently
# of this table.
#
# A NOTE ON WHAT GOING GREEN MEANS. Once rule 1 holds, no test is ever
# costed at the mean, so the mean stops being reachable at all. Rule 2
# still earns its place: the window where the mean IS used is exactly
# the window before this gate has run — a test added locally, a branch
# mid-review — and that is when a poisoned mean does its damage.
#
# Usage: check-durations-table.sh [--root <dir>]
# Exit:  0 the table names exactly the partitioned set
#        1 a test has no row, a row names no partitioned test, or a row
#          is malformed or duplicated
#        2 the gate cannot see: no suite directory, no partitioner, a
#          partitioner that refuses, or a table that is missing,
#          unreadable, or holds no rows at all. A table with nothing in
#          it must not read as a table with nothing wrong with it.

set -uo pipefail

# Same reason as integration-shard.sh (#554): the sort and the numeric
# parse below must not depend on who runs this.
export LC_ALL=C

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
while [ $# -gt 0 ]; do
    case "$1" in
        --root) ROOT="${2:-}"; shift 2 || exit 2 ;;
        *) echo "usage: $0 [--root <dir>]" >&2; exit 2 ;;
    esac
done

SUITE_DIR="$ROOT/test/integration"
TABLE="$SUITE_DIR/testdata/main-suite-durations.tsv"
SHARDER="$ROOT/scripts/integration-shard.sh"

cannot_see() {
    echo "::error title=durations-table gate cannot see::$*" >&2
    exit 2
}

[ -d "$SUITE_DIR" ] || cannot_see "$SUITE_DIR is not a directory"
[ -f "$SHARDER" ]   || cannot_see "$SHARDER does not exist, so the partitioned set cannot be asked for"
{ [ -f "$TABLE" ] && [ -r "$TABLE" ]; } || cannot_see "$TABLE is missing, unreadable, or not a regular file"

# The partitioner is the authority on which tests get balanced. 1-of-1
# is the whole set; it exits 2 rather than emitting an empty regex when
# it finds no tests, which is the vacuity this gate would otherwise
# inherit.
if ! regex=$(bash "$SHARDER" 1 1 2>&1); then
    cannot_see "$SHARDER 1 1 refused: $regex"
fi
population=$(printf '%s\n' "$regex" | sed -E 's/^\^\(//; s/\)\$$//' | tr '|' '\n' | grep -E '^Test[A-Za-z0-9_]+$' | sort -u)
[ -n "$population" ] || cannot_see "$SHARDER 1 1 named no tests"

# Rule 3, before anything is compared: a row this loop cannot read is a
# row the partitioner's awk would read as a zero, silently.
malformed=$(awk -F'\t' '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    NF != 2                       { printf "%d: %d field(s), want 2: %s\n", NR, NF, $0; next }
    $1 !~ /^Test[A-Za-z0-9_]+$/   { printf "%d: %s is not a test name\n", NR, $1; next }
    $2 !~ /^[0-9]+(\.[0-9]+)?$/   { printf "%d: %s has duration %s, want a non-negative number\n", NR, $1, $2 }
' "$TABLE")
if [ -n "$malformed" ]; then
    echo "::error title=Malformed row in the shard-balance table (#877)::a row the partitioner" \
         "cannot parse is costed as zero, silently." >&2
    printf '%s\n' "$malformed" | sed "s|^|  $TABLE:|" >&2
    exit 1
fi

rows=$(awk -F'\t' '$1 !~ /^[[:space:]]*#/ && NF == 2 { print $1 }' "$TABLE")
[ -n "$rows" ] || cannot_see "$TABLE holds no rows — it would balance nothing and this gate would compare nothing"

dupes=$(printf '%s\n' "$rows" | sort | uniq -d)
if [ -n "$dupes" ]; then
    echo "::error title=Duplicate row in the shard-balance table (#877)::the partitioner takes the" \
         "FIRST match and the mean counts BOTH, so a duplicate weights the corpus without weighting the test." >&2
    printf '  %s\n' $dupes >&2
    exit 1
fi
rows=$(printf '%s\n' "$rows" | sort -u)

missing=$(comm -23 <(printf '%s\n' "$population") <(printf '%s\n' "$rows"))
stray=$(comm -13 <(printf '%s\n' "$population") <(printf '%s\n' "$rows"))

rc=0
if [ -n "$missing" ]; then
    echo "::error title=A main-suite test has no measured duration (#877)::these tests are" \
         "partitioned but not costed, so each is charged the mean and the shards drift apart." \
         "Refresh $TABLE — the header says how." >&2
    printf '  %s\n' $missing >&2
    rc=1
fi
if [ -n "$stray" ]; then
    echo "::error title=The shard-balance table names a test it does not balance (#877)::these rows" \
         "name nothing the partitioner will place — a renamed or deleted test, or a test from another" \
         "package. They are never selected, but the mean is the average of the ROWS, so they mis-cost" \
         "every test that IS missing a row." >&2
    printf '  %s\n' $stray >&2
    rc=1
fi
[ "$rc" -eq 0 ] || exit 1

echo "durations-table gate: $(printf '%s\n' "$rows" | wc -l) row(s) name exactly the $(printf '%s\n' "$population" | wc -l) test(s) the partitioner places"
exit 0
