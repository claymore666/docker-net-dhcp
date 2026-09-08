#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# THE SHARDS THE LANE SCHEDULES MUST COVER THE ROSTER EXACTLY ONCE.
#
# WHY THIS EXISTS. Everything about the partition was checked except the
# thing the lane actually does with it. scripts/integration-shard.sh is
# proven to partition the roster at every count -- test-integration-shard.sh
# drives that -- and the two workflows were proven to name the same list
# as each other. Neither says the list is COMPLETE. The count is a
# literal, repeated eleven times in .github/workflows/integration.yml
# and eleven more in .github/workflows/integration-hosted.yml, and a
# reshard edits it with one sed per file.
#
# MEASURED, in review, against the tree this gate is added to: changing
# `OF=9` to `OF=10` in integration.yml's nine main entries and `-of-9`
# to `-of-10` in integration-hosted.yml, WITHOUT adding a tenth entry --
# which is exactly what that sed does -- left nine main-suite tests
# (shard 10 of 10, among them TestLifecycleBridge_GoldenPath and all
# three TestConflictCheck_*) never scheduled by either lane, while
# test-integration-shard.sh reported "30 passed" and "the pool and
# hosted lanes name the same 11 shard(s)", check-pool-facts.sh reported
# eleven jobs, and check-durations-table.sh reported 95 rows naming
# exactly the 95 tests the partitioner places. A second edit -- delete
# the main-5 entry and duplicate main-4 in both files -- lost eight more
# tests, equally green. Both are single-edit mistakes, not sabotage.
#
# The reason every one of those gates stayed green is that each is true
# of a different operand. The partitioner is complete AT A COUNT; the
# two lanes agree WITH EACH OTHER; the job count is the number of matrix
# entries; the durations table names the tests the partitioner PLACES.
# None of them is the composition, which is the only thing that decides
# whether a test runs: the union, over the triples the matrices actually
# schedule, of what the partitioner selects for each.
#
# WHAT THIS CHECKS, in one line: for each lane, the multiset union of
# `integration-shard.sh <index> <total> <suite>` over the (suite, index,
# total) triples that lane schedules is the roster, each test exactly
# once. A MISSING test and a DUPLICATED test are both red, and both are
# named.
#
# THREE DERIVATIONS, AND WHY THREE. The roster is read twice here, from
# the sources and from the partitioner asked for one shard of one, and
# they are required to agree before any verdict is emitted. A gate whose
# input is wrong must not report a verdict at all: if those two disagree
# the partitioner has stopped seeing the sources, which is
# test-integration-shard.sh's finding to make, and this gate refuses
# rather than measuring the scheduled set against a roster it cannot
# trust. The third derivation is the scheduled set itself, and it is the
# only one the workflows contribute to.
#
# WHAT IT CANNOT SEE, stated as a bound rather than left to be found.
#   * It reads the triples a matrix NAMES. A shard skipped at run time
#     -- an `if:` on the job, a `continue-on-error`, a cancelled job --
#     schedules the shard and does not run it. That is the aggregate's
#     domain (`integration` refuses a rollup that is not all-success),
#     not this gate's.
#   * It knows the two lanes this repository has. A third workflow that
#     ran shards would not be seen until it is named in WORKFLOWS below,
#     and there is no way to discover "a job that runs a shard" that
#     does not amount to the same list.
#   * The "runs a test that is not in the roster" arm is UNREACHABLE
#     while the roster refusal above holds: every scheduled name comes
#     from the partitioner, and the partitioner's population is required
#     to equal the sources' before any verdict is emitted. It is kept
#     because that refusal is what makes it unreachable, and a future
#     loosening of the refusal would otherwise widen the scheduled set
#     in silence. The self-test drives the refusal, not the arm, and
#     says so in the case.
#   * It says nothing about ORDER or about which tests share a shard.
#     The partition is balanced by seconds, and any statement about
#     which tests land together is true of one durations table only.
#
# Exit: 0 covered, 1 not covered (missing or duplicated tests, named),
# 2 refused (the gate could not see its subject -- never silent).

set -uo pipefail

# Pinned for the whole script, not per command (#554). Every comparison
# below -- sort, uniq, comm, and the regex classes -- has to agree about
# collation, and a per-command LC_ALL is one command away from being
# forgotten: the first draft of this gate pinned `sort` and not `comm`,
# and `comm` then warned "input is not sorted" on a German locale while
# still producing an answer. A gate that answers from an unsorted
# comparison is worse than one that refuses.
export LC_ALL=C

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
WF="${SHARD_COVERAGE_WORKFLOWS:-$ROOT/.github/workflows}"
SHARDER="$HERE/integration-shard.sh"
SUITE_DIR="$ROOT/test/integration"

# The two lanes that schedule shards, and how each spells a shard.
WORKFLOWS="integration.yml integration-hosted.yml"

refuse() {
    echo "::error title=$1::$2" >&2
    exit 2
}

[ -d "$WF" ] || refuse "No workflow directory" "$WF is not a directory, so no lane can be read."
[ -x "$SHARDER" ] || [ -f "$SHARDER" ] || refuse "No partitioner" "$SHARDER is missing."
[ -d "$SUITE_DIR" ] || refuse "No suite directory" "$SUITE_DIR is missing."

for wf in $WORKFLOWS; do
    [ -f "$WF/$wf" ] || refuse "A lane is missing" "$WF/$wf does not exist; this gate's domain would be smaller than the lane it checks."
done

# ---------------------------------------------------------------- roster
#
# From the sources, with the same anchored shape the partitioner uses:
# a top-level `func TestX(t *testing.T)` in test/integration/*_test.go.
# TestMain carries no `(t *testing.T)` and is excluded by the pattern
# rather than by a name.
roster_src=$(grep -hoE '^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)' "$SUITE_DIR"/*_test.go 2>/dev/null \
             | sed -E 's/^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)$/\1/' | sort)
[ -n "$roster_src" ] || refuse "No tests found" \
    "no top-level test function was read out of $SUITE_DIR, so the roster this gate would compare against is empty."

# From the partitioner, one shard of one per suite -- everything it can
# place. Any suite it refuses is a refusal here, not an empty set.
roster_part=""
for suite in main failure; do
    if ! rx=$(bash "$SHARDER" 1 1 "$suite" 2>&1); then
        refuse "The partitioner refused" "$SHARDER 1 1 $suite: $rx"
    fi
    names=$(printf '%s\n' "$rx" | sed -E 's/^\^\(//; s/\)\$$//' | tr '|' '\n' | grep -E '^Test[A-Za-z0-9_]+$')
    [ -n "$names" ] || refuse "The partitioner named nothing" "$SHARDER 1 1 $suite emitted no test names."
    roster_part=$(printf '%s\n%s' "$roster_part" "$names")
done
roster_part=$(printf '%s\n' "$roster_part" | grep -E '^Test[A-Za-z0-9_]+$' | sort)

if [ "$roster_src" != "$roster_part" ]; then
    refuse "The partitioner and the sources disagree about the roster" \
        "$(printf 'only in the sources: %s | only in the partitioner: %s -- scripts/test-integration-shard.sh owns this; a scheduled set cannot be judged against a roster that is in dispute.' \
            "$(comm -23 <(printf '%s\n' "$roster_src") <(printf '%s\n' "$roster_part") | tr '\n' ' ')" \
            "$(comm -13 <(printf '%s\n' "$roster_src") <(printf '%s\n' "$roster_part") | tr '\n' ' ')")"
fi
roster="$roster_src"
roster_n=$(printf '%s\n' "$roster" | wc -l)

# ------------------------------------------------- the scheduled triples
#
# integration.yml: the `target` of each matrix include entry, which is
# the make invocation the job actually runs. Reading the target rather
# than the entry's name is deliberate -- the name is a label, the target
# is what selects tests, and a mismatch between them is invisible to a
# check that reads the label.
#
# integration-hosted.yml: the shard ids in the matrix expression, in the
# `<suite>-<index>-of-<total>` spelling that lane's own "Run one shard"
# step parses back into SHARD/OF/SUITE. `full` is that lane's unsharded
# mode and is not a triple.
triples_of() {
    case "$1" in
    integration.yml)
        sed -n 's/.*integration-test-shard SHARD=\([0-9][0-9]*\) OF=\([0-9][0-9]*\) SUITE=\([a-z][a-z]*\).*/\3 \1 \2/p' "$WF/$1"
        ;;
    integration-hosted.yml)
        grep -oE '"[a-z]+-[0-9]+-of-[0-9]+"' "$WF/$1" | tr -d '"' \
            | sed -E 's/^([a-z]+)-([0-9]+)-of-([0-9]+)$/\1 \2 \3/'
        ;;
    *)
        refuse "Unknown lane" "$1 has no triple spelling declared in this gate."
        ;;
    esac
}

status=0
for wf in $WORKFLOWS; do
    triples=$(triples_of "$wf")
    if [ -z "$triples" ]; then
        refuse "A lane schedules no shard" \
            "no (suite, index, total) triple could be read out of $WF/$wf. An empty scheduled set covers nothing, and a gate whose domain is empty passes by saying nothing."
    fi

    # The multiset union: every test the lane's triples select, with
    # multiplicity. A duplicated matrix entry shows up as a count of two
    # and is as red as a missing test -- it means some other shard's
    # tests are not scheduled at all, which is how the second mutant
    # hides.
    scheduled=""
    n_triples=0
    while read -r suite idx total; do
        [ -n "${suite:-}" ] || continue
        n_triples=$((n_triples + 1))
        if ! rx=$(bash "$SHARDER" "$idx" "$total" "$suite" 2>&1); then
            echo "::error title=A scheduled shard does not partition::${wf} schedules ${suite} shard ${idx} of ${total}, and ${SHARDER} refuses it: ${rx}" >&2
            status=1
            continue
        fi
        names=$(printf '%s\n' "$rx" | sed -E 's/^\^\(//; s/\)\$$//' | tr '|' '\n' | grep -E '^Test[A-Za-z0-9_]+$')
        scheduled=$(printf '%s\n%s' "$scheduled" "$names")
    done <<EOF
$triples
EOF

    scheduled=$(printf '%s\n' "$scheduled" | grep -E '^Test[A-Za-z0-9_]+$' | sort)
    uniq_sched=$(printf '%s\n' "$scheduled" | uniq)

    missing=$(comm -23 <(printf '%s\n' "$roster") <(printf '%s\n' "$uniq_sched"))
    extra=$(comm -13 <(printf '%s\n' "$roster") <(printf '%s\n' "$uniq_sched"))
    dupes=$(printf '%s\n' "$scheduled" | uniq -d)

    if [ -n "$missing" ]; then
        echo "::error title=Tests no scheduled shard runs::${wf} schedules ${n_triples} shard(s), and $(printf '%s\n' "$missing" | wc -l) of the ${roster_n} test(s) in the roster are in none of them: $(printf '%s\n' "$missing" | tr '\n' ' ')-- the partition is complete, the SCHEDULE is not." >&2
        status=1
    fi
    if [ -n "$extra" ]; then
        echo "::error title=A scheduled shard runs a test that is not in the roster::${wf}: $(printf '%s\n' "$extra" | tr '\n' ' ')" >&2
        status=1
    fi
    if [ -n "$dupes" ]; then
        echo "::error title=Tests more than one scheduled shard runs::${wf} schedules $(printf '%s\n' "$dupes" | wc -l) test(s) twice or more: $(printf '%s\n' "$dupes" | tr '\n' ' ')-- a duplicated matrix entry costs whichever shard it replaced." >&2
        status=1
    fi
    if [ -z "$missing" ] && [ -z "$extra" ] && [ -z "$dupes" ]; then
        echo "shard-coverage gate: ${wf} schedules ${n_triples} shard(s) covering all ${roster_n} test(s) exactly once"
    fi
done

exit "$status"
