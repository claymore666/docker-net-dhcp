#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Resolve the coverage baseline as it stands at the merge base (#735).
#
# Usage: coverage-baseline-at.sh <base-ref> <output-file> [report-file]
#   <base-ref>:    the PR's base commit, e.g. github.event.pull_request.base.sha
#   <output-file>: where to write the baseline blob
#   [report-file]: where to write the machine-readable report
#                  (default: <output-file>.report)
#
# Exit: 0 written, 2 cannot resolve.
#
# THE REPORT IS HALF OF A CROSS-CHECK (#791). coverage-ratchet.sh guards
# non-vacuity by asking whether it compared ZERO packages, and its loop
# iterates over the baseline it was handed -- so `compared` is by
# construction the number of data lines in that file and the guard can
# only ever catch the empty case. A baseline that arrives holding two of
# its five packages compares two, prints two PASS lines and exits 0, and
# the release reads that as a clean ratchet.
#
# The count cannot come from the ratchet, because the ratchet has no
# second opinion about what a complete baseline is. It comes from here:
# this script resolved the blob, so it is the one place that knows what
# it handed over. It writes the count, the blob's identity and the
# package NAMES; the ratchet asserts it compared exactly those.
#
# WHAT THIS CROSS-CHECK DOES NOT COVER, stated because the paragraph
# above reads wider than the mechanism is. The count and names below are
# parsed out of $OUT -- the same bytes this script just extracted with
# `git show` and is about to hand to the ratchet -- by, deliberately, the
# same rule the ratchet parses with. So both sides of the cross-check are
# two parses of ONE object. A blob that was already damaged when it was
# resolved is agreed to by both sides.
#
# Concretely, and driven at fbc1ed2 with the two scripts unmodified: a
# merge base holding a two-package baseline where the run measured five
# produces `count 2`, two PASS lines, exit 0 -- and now a "Cross-checked:
# compared 2 of 2" line attesting to it. The guard's contribution in that
# scenario is a sentence that makes the clean verdict look verified.
#
# So the four causes named in coverage-ratchet.sh -- a rebase, a
# truncated blob, a partial fetch, a merge that dropped lines -- all
# damage the blob AT SOURCE and none of them is caught here. What IS
# caught is damage BETWEEN the resolver and the ratchet: the wrong file
# handed over, a file truncated after it was written, a by-name
# substitution, the report missing or unreadable. That is real and it is
# what the NOT CROSS-CHECKED line protects.
#
# The completeness question needs a source outside the blob. The percent
# file is one -- it names every package the run actually measured and is
# produced by `go tool covdata`, not by `git show`. The ratchet holds it
# already, and it now warns (see coverage-ratchet.sh) rather than
# refusing, because a package measured but not floored is also what a
# legitimately new package looks like. Turning that into a refusal needs
# a rule for telling those apart and is deliberately not attempted here.
#
# NAMES AND NOT ONLY A COUNT, because "2 of 5" sends someone to read a
# 258-line file of which 253 lines are commentary, and
# "pkg/dhcp, cmd/net-dhcp" sends them to two lines.
#
# WHY THIS IS A SCRIPT AND NOT THREE LINES OF YAML. It runs in exactly
# one place — coverage.yml, which triggers only on pull requests into
# main — so the first time it would ever execute is the release PR, and
# a defect there fails a required check with the release already in
# flight. Inline in a `run:` block that is untestable; here it has a
# self-test that runs on every PR. The rule this repo already states for
# release.yml applies to anything else on the release path.
#
# NO FALLBACK TO THE WORKING COPY. If the merge base or the blob will
# not resolve, this refuses. Falling back would silently restore the
# defect the merge-base read exists to close: a branch that lowered a
# floor being measured against the number it had just lowered.
set -u

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
    echo "usage: $0 <base-ref> <output-file> [report-file]" >&2
    exit 2
fi

BASE_REF="$1"
OUT="$2"
REPORT="${3:-$2.report}"
BASELINE_PATH=".github/coverage-baseline.txt"

if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null; then
    echo "::error title=Cannot resolve the coverage baseline::base ref '$BASE_REF' does not resolve." \
         "A shallow checkout is the usual cause — coverage.yml needs fetch-depth: 0." >&2
    exit 2
fi

if ! MERGE_BASE=$(git merge-base "$BASE_REF" HEAD 2>/dev/null) || [ -z "$MERGE_BASE" ]; then
    echo "::error title=Cannot resolve the coverage baseline::no merge base between '$BASE_REF' and HEAD." >&2
    exit 2
fi

if ! git show "$MERGE_BASE:$BASELINE_PATH" > "$OUT" 2>/dev/null; then
    echo "::error title=Cannot resolve the coverage baseline::$BASELINE_PATH does not exist at merge base $MERGE_BASE." >&2
    rm -f "$OUT"
    exit 2
fi

# The blob's identity, so the report names an object rather than a path.
# `git rev-parse` on the tree entry, not a hash of $OUT: the point is to
# record WHICH object was read, and a hash of the copy would still agree
# with itself after the copy was truncated.
BLOB=$(git rev-parse --verify --quiet "$MERGE_BASE:$BASELINE_PATH" 2>/dev/null) || BLOB="unknown"

# Data lines, by the SAME rule the ratchet parses with -- a leading `#`
# or a blank line is commentary. Two different rules here would make the
# cross-check fail on files that are perfectly fine.
PKGS=$(awk '
    { sub(/^[[:space:]]+/, "") }
    /^#/ { next }
    NF == 0 { next }
    { print $1 }
' "$OUT")
COUNT=$(printf '%s\n' "$PKGS" | grep -c .)

# NON-VACUITY AT THE SOURCE. A baseline blob that resolves to no data
# lines is the extreme of the truncation this report exists to catch, and
# handing it to the ratchet would produce the ratchet's own "Nothing to
# inspect" refusal one step later, naming the temp file instead of the
# merge base that actually produced it.
if [ "$COUNT" -eq 0 ]; then
    echo "::error title=Cannot resolve the coverage baseline::$BASELINE_PATH at merge base $MERGE_BASE (blob $BLOB)" \
         "holds no <package> <percent> data lines. The ratchet would compare nothing and report a clean pass." >&2
    rm -f "$OUT"
    exit 2
fi

{
    echo "merge_base $MERGE_BASE"
    echo "blob $BLOB"
    echo "count $COUNT"
    printf '%s\n' "$PKGS" | grep . | sed 's/^/package /'
} > "$REPORT" || {
    echo "::error title=Cannot resolve the coverage baseline::could not write the report to $REPORT." >&2
    rm -f "$OUT"
    exit 2
}

echo "Coverage floors read from $BASELINE_PATH at merge base $MERGE_BASE."
echo "Resolved $COUNT package floor(s) from blob $BLOB; report written to $REPORT."
