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
#   `| head -N` closes the pipe the same way, and a `$(... | head)` in a
#   plain assignment is still exempt: nothing reads the substitution's
#   status, no script here sets `-e`, so a 141 is discarded and the
#   captured value is right. That was measured rather than assumed.
#
#   BUT AN EXEMPTION IS A MEASUREMENT WITH A TIMESTAMP, AND NOTHING
#   RE-RUNS IT. The one above was true over the tree as it stood, and it
#   depends entirely on nothing reading the status. Three things read
#   it, and the original wording weighed only the first:
#
#     1. `set -e`                       — genuinely absent here.
#     2. `x=$(... | head -1) || handler`  — reads it explicitly.
#     3. `if x=$(... | head -1); then`, and `&&` — the same.
#
#   So the rule is not "a producer that can SIGPIPE". It is `$(... |
#   head)` WHOSE EXIT STATUS IS CONSUMED BY ANYTHING, which needs no
#   reasoning about `-e` and is decidable at the call site. Rule two
#   still rejects a bare `head` pipeline in a condition; rule three
#   rejects the status-consuming substitution. A `$(...)` used for its
#   VALUE inside a condition — `if [ -n "$(p | head -1)" ]` — stays
#   clean: there the status belongs to the test.
#
#   `|| true` is excluded. It reads the status in order to throw it
#   away, which is the exemption stated out loud, and the tree's single
#   instance is exactly that.
#
#   Found because the failure is the expensive kind. SIGPIPE gives 141,
#   the handler fires, and a refusal path reports the SUBJECT as broken
#   when the instrument is. A tool that accuses its subject costs more
#   than one that crashes.
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

# UNTRACKED FILES COUNT TOO (#743). A file written but not yet `git
# add`ed is invisible to `ls-files` alone, and that is precisely the
# state a source file is in for the whole time someone is writing it.
# This gate runs in scripts/local-lane.sh, where an uncommitted working
# tree is the NORMAL state — so the blind spot sat exactly where the
# gate is meant to be useful, and never showed in CI, where a fresh
# checkout makes tracked and present mean the same thing. Same argument
# and same fix as check-license-headers.sh:74.
mapfile -t FILES < <({
    git -C "$ROOT" ls-files -- '*.sh'
    git -C "$ROOT" ls-files --others --exclude-standard -- '*.sh'
} | sort -u)

if [ "${#FILES[@]}" -eq 0 ]; then
    echo "::error title=Nothing to inspect::no *.sh files in $ROOT." \
         "This gate would otherwise report a clean pass having read nothing." >&2
    exit 2
fi

# Built from pieces so this gate's own source, and its self-test's
# fixtures, do not match the thing they describe.
GREP_Q="[|][[:space:]]*grep[[:space:]]\+-[A-Za-z-]*"'q'
Q_='q'   # same reason as GREP_Q: not the literal, in this file
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
            # Rule three: a `$(... | head)` whose status is CONSUMED.
            #
            # One awk pass, deciding per OCCURRENCE rather than per line.
            # A line-level exclusion would exempt every substitution on a
            # line that carries a `|| true` anywhere -- `x=$(a | head -1)
            # || true; y=$(b | head -1) || handler` would go quiet on
            # both -- and an exemption that leaks always leaks in the
            # direction that makes the gate silent.
            #
            # The continued form (`\` at the end, the `||` on the next
            # line) is the shape this was actually found in, so it is not
            # optional: without it the sweep returns the same clean tree
            # for the wrong reason.
            awk '
              function consumed(after) {
                  if (after ~ /^[ \t]*\|\|[ \t]*true([ \t;)]|$)/) return 0
                  return (after ~ /^[ \t]*(\|\||&&)/)
              }
              /^[ \t]*#/ { pend = 0; next }
              {
                if (pend && NR == pend + 1 && $0 ~ /^[ \t]*(\|\||&&)/ \
                    && $0 !~ /^[ \t]*\|\|[ \t]*true([ \t;)]|$)/) {
                    printf "%d:%s\n", pend, pendline
                }
                pend = 0
                rest = $0
                while (match(rest, /[$]\([^)]*\|[ \t]*head[^)]*\)/)) {
                    after = substr(rest, RSTART + RLENGTH)
                    if (consumed(after)) { printf "%d:%s\n", NR, $0; break }
                    # Nothing but a line continuation after the
                    # substitution: the operator, if any, is on the next
                    # line. `after` still holds that backslash, so
                    # testing it for whitespace ALONE never matches --
                    # which is how the first version silently stopped
                    # covering the one form the bug was found in.
                    if (after ~ /^[ \t]*\\[ \t]*$/) {
                        pend = NR; pendline = $0
                    }
                    if (RSTART + RLENGTH > length(rest)) break
                    rest = after
                }
                # `if x=$(... | head -1); then` -- there the reader is
                # the compound test itself rather than a following
                # operator, so it needs its own arm. No apostrophe in
                # this block: it lives inside a single-quoted awk
                # program and one would end the string.
                #
                # (That is not a hypothetical. The first draft of this
                # comment said "the compound command\47s own test" and
                # the gate died with a shell syntax error at this line.)
                if ($0 ~ /^[ \t]*(if|elif|while|until)[ \t]+[A-Za-z_][A-Za-z0-9_]*=[$]\([^)]*\|[ \t]*head/ \
                    && $0 !~ /\|\|[ \t]*true/) {
                    printf "%d:%s\n", NR, $0
                }
              }' "$ROOT/$f" || true
        } | grep -v '^[0-9]*:[[:space:]]*#' | sort -t: -k1,1n -u || true
    )
done

if [ "$findings" -ne 0 ]; then
    echo >&2
    # The remedy has to name the shape that was found. A message telling
    # someone to "drop the -q" when what they wrote was `x=$(p | head -1)
    # || handler` sends them looking for a `-q` that is not there, and a
    # red that names the wrong remedy costs more than the defect.
    echo "::error title=Racy pipeline under pipefail::${findings} pipeline(s)." \
         "A consumer that exits early kills the producer with SIGPIPE, so the" \
         "pipeline reports failure on success." \
         "For a piped 'grep -${Q_}': drop the -${Q_} and redirect to /dev/null" \
         "instead — it reads to EOF, so the status is the real one." \
         "For a 'head' pipeline whose status something reads: capture the whole" \
         "stream (out=\$(producer)) and take the first line with a %% parameter" \
         "expansion — nothing closes the pipe, so there is no SIGPIPE to read." \
         "If you meant to discard the status, say so with '|| true' and this" \
         "gate will leave it alone." >&2
    exit 1
fi

echo "PASS  no early-exit pipeline consumers under pipefail: ${#FILES[@]} script(s) inspected"
