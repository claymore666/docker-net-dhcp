#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Resolve the coverage baseline as it stands at the merge base (#735).
#
# Usage: coverage-baseline-at.sh <base-ref> <output-file>
#   <base-ref>:    the PR's base commit, e.g. github.event.pull_request.base.sha
#   <output-file>: where to write the baseline blob
#
# Exit: 0 written, 2 cannot resolve.
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

if [ "$#" -ne 2 ]; then
    echo "usage: $0 <base-ref> <output-file>" >&2
    exit 2
fi

BASE_REF="$1"
OUT="$2"
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

echo "Coverage floors read from $BASELINE_PATH at merge base $MERGE_BASE."
