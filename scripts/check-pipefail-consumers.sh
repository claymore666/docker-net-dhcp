#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# pipefail + early-exit-consumer gate.
#
# Under `set -o pipefail` a pipeline reports the failure of ANY stage.
# `grep -q` exits the moment it matches, which closes the pipe while the
# producer is still writing: the producer dies of SIGPIPE with status
# 141, and the pipeline reports FAILURE even though the match succeeded.
#
# It is timing-dependent, which is the worst part. If the producer
# finishes writing before grep exits — anything small, anything warm in
# page cache — the pipeline is correct, so the bug does not show up
# locally and does not show up on a re-run. check-python-deps.sh shipped
# with `git grep ... | grep -qE 'pip install'` and passed every local
# run; on a cold CI checkout git grep was slow enough to still be
# writing, and the gate reported that nothing installs a file that two
# workflow lines install. The gate was correct and its plumbing lied.
#
# The repo has been here before: #297 was the same class through `tee`.
#
# THE FIX IS ALWAYS THE SAME. Drop the -q and redirect instead:
#
#     producer | grep -qE 'pat'          # racy under pipefail
#     producer | grep -E 'pat' >/dev/null # reads to EOF, no SIGPIPE
#
# grep without -q consumes all of its input, so the producer always
# finishes and the status is the one the author meant.
#
# WHAT THIS DOES NOT COVER, and why:
#
#   `| head -N` closes the pipe the same way. Every such pipeline in
#   this tree sits inside a `$(...)` whose status nothing reads, and no
#   script here sets `-e`, so a 141 is discarded and the captured value
#   is still right — measured, not assumed. The second rule below keeps
#   it that way by rejecting a bare `head` pipeline in a condition,
#   which is where the status would start to matter. A `$(...)` inside a
#   condition is not flagged: there the status belongs to the test, not
#   to the pipeline.
#
# Usage: check-pipefail-consumers.sh
# Env:   PIPE_ROOT  repository to inspect (default: the repo this script
#                   lives in) — the seam the self-test drives.
# Exit:  0 clean, 1 a racy pipeline found, 2 cannot check.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${PIPE_ROOT:-$(cd "$HERE/.." && pwd)}"

if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "::error title=Not a git repository::$ROOT — this gate discovers" \
         "files through the git index, so it cannot inspect anything here." >&2
    exit 2
fi

mapfile -t FILES < <(git -C "$ROOT" ls-files -- '*.sh' | sort)

if [ "${#FILES[@]}" -eq 0 ]; then
    echo "::error title=Nothing to inspect::no tracked *.sh files in $ROOT." \
         "This gate would otherwise report a clean pass having read nothing." >&2
    exit 2
fi

# Built from pieces so this gate's own source, and its self-test's
# fixtures, do not match the thing they describe.
GREP_Q="[|][[:space:]]*grep[[:space:]]\+-[A-Za-z-]*"'q'
HEAD_COND="^[[:space:]]*\(if\|elif\|while\|until\)[[:space:]].*[|][[:space:]]*head[[:space:]]"

findings=0
for f in "${FILES[@]}"; do
    # `[^|]` before the pipe so `||` is not read as a pipeline: there the
    # grep is a command in its own right and reads a file, not a pipe.
    while IFS=: read -r n line; do
        [ -n "$n" ] || continue
        findings=$((findings + 1))
        printf '  %s:%s\n      %s\n' "$f" "$n" "$(printf '%s' "$line" | sed 's/^[[:space:]]*//')" >&2
    done < <(
        {
            grep -n -e "[^|]${GREP_Q}" "$ROOT/$f" || true
            # A $(...) in a condition is a substitution, not the
            # condition's own pipeline: its status belongs to the test.
            grep -n -e "$HEAD_COND" "$ROOT/$f" | grep -v '[$](' || true
        } | grep -v '^[0-9]*:[[:space:]]*#' | sort -t: -k1,1n -u || true
    )
done

if [ "$findings" -ne 0 ]; then
    echo >&2
    echo "::error title=Racy pipeline under pipefail::${findings} pipeline(s)." \
         "A consumer that exits early kills the producer with SIGPIPE and the" \
         "pipeline reports failure on success. Drop the -q and redirect to" \
         "/dev/null instead — it reads to EOF, so the status is the real one." >&2
    exit 1
fi

echo "PASS  no early-exit pipeline consumers under pipefail: ${#FILES[@]} script(s) inspected"
