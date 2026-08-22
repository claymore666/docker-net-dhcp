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
#   RE-RUNS IT — AND THE FIRST DRAFT OF THIS PARAGRAPH PROVED ITS OWN
#   POINT. It listed three consumers and said of the first, `set -e`,
#   "genuinely absent here". That was false when written:
#
#     verify-bridge-recipes.sh:1              set -e
#     setup-runner-storage.sh:1               set -e
#
#   The true statement is narrower — no `set -e` script has a `$(... |
#   head)` in ITS OWN shell — and a truer exemption is still an
#   exemption with a timestamp. So it is not written here as a claim.
#   It is an arm below, with fixtures, and it fails the build.
#
#   THE SPEC IS THE PROPERTY, NOT THE LIST: anything that reads the
#   substitution's status BEFORE THE NEXT COMMAND RUNS. The list below
#   is examples. It has been one member short twice — three consumers,
#   then four when `$?` was added — and each fix appended a member and
#   left the form of the claim intact. A list will keep being one short;
#   the property will not.
#
#     `set -e`                                — arm four.
#     `x=$(... | head -1) || handler`         — reads it explicitly.
#     `if x=$(... | head -1); then`, and `&&` — the same.
#     `$?`                                    — reads it afterwards, and
#                                               a comment or a blank
#                                               line does not reset it.
#
#   `$?` has no instance in the tree today. Neither did the two-dot
#   receiver in check-release-notes-symbols.sh, which is exactly why it
#   is here: the cheapest time to close a member is while writing the
#   arm next to it.
#
#   So the rule is not "a producer that can SIGPIPE". It is `$(... |
#   head)` WHOSE EXIT STATUS IS CONSUMED BY ANYTHING — by an operator,
#   by `$?`, or by `-e` on the script's own behalf. A `$(...)` used for
#   its VALUE inside a condition — `if [ -n "$(p | head -1)" ]` — stays
#   clean: there the status belongs to the test.
#
#   WHAT ARM FOUR DELIBERATELY DOES NOT SEE, both documented rather than
#   parsed, because an undocumented limit is the next stale exemption:
#
#     - a pipeline inside a nested shell string, `sh -c "a | head -1"`
#       or `sh -c 'a | head -1'`: it runs in another shell, so this
#       script's `-e` does not reach it, and whether THAT shell sets
#       pipefail is not visible here. Detected by an odd count of
#       EITHER quote character before the pipe — counting only one made
#       the arm flag the other spelling of the same safe construct.
#     - a nested substitution, `$( ... $(...) | head)`. The occurrence
#       scanner stops at the first `)`, so the outer form is not
#       matched at all.
#     - the PRICE of counting both quote characters: an apostrophe
#       inside a double-quoted string, `x=$(grep "it's" f | head -1)`,
#       leaves an odd `'` count and reads as a nested shell. A false
#       negative, narrow and in the quiet direction. It is written down
#       rather than parsed because the parse is a shell lexer, and this
#       gate is not going to become one — the previous sentence in this
#       header that was a claim instead of an arm is the reason the
#       whole file exists.
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
    # Arm four needs to know whether the script errexits. Computed here
    # rather than inside awk so the pattern is a shell regex like every
    # other one in this gate: `set -e`, `set -eu`, `set -euo pipefail`.
    if grep -qE '^[[:space:]]*set[[:space:]]+-[A-Za-z]*e' "$ROOT/$f"; then
        sete=1
    else
        sete=0
    fi
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
            awk -v sete="$sete" '
              function consumed(after) {
                  if (after ~ /^[ \t]*\|\|[ \t]*true([ \t;)]|$)/) return 0
                  return (after ~ /^[ \t]*(\|\||&&)/)
              }
              # Is the pipe that feeds head at the substitution as
              # written, or inside a string being handed to ANOTHER
              # shell? An odd count of either quote character before it
              # means the latter, and this script\47s -e does not reach
              # there.
              #
              # BOTH characters, because `sh -c "a | head"` and
              # `sh -c \47a | head\47` are the same construct and the
              # same measured non-risk. Counting only one of them made
              # the arm exempt the double-quoted nesting and FLAG the
              # single-quoted one -- a false positive, so it failed in
              # the safe direction, but the red named a remedy for a
              # pipeline that was never at risk. Comment lines never
              # reach here, so an unbalanced apostrophe in prose cannot
              # reach this count.
              #
              # match() writes RSTART and RLENGTH, which the caller\47s
              # occurrence loop is still using, so they are saved and
              # put back. Forgetting that turns the loop into an
              # infinite one, silently.
              function toplevel(t,   before, saveS, saveL, r) {
                  saveS = RSTART; saveL = RLENGTH; r = 1
                  if (match(t, /[|][ \t]*head/) > 0) {
                      before = substr(t, 1, RSTART - 1)
                      if (gsub(/"/, "\"", before) % 2) r = 0
                      if (gsub(/\47/, "\47", before) % 2) r = 0
                  }
                  RSTART = saveS; RLENGTH = saveL
                  return r
              }
              # A COMMENT IS NOT A COMMAND, AND NEITHER IS A BLANK LINE.
              # Neither resets `$?`, so the `$?` arm has to see past
              # them: the pending substitution survives, and the next
              # line that actually runs is the one that decides.
              #
              # `pend` is the opposite -- a backslash continuation
              # cannot span a comment or a blank -- so it is dropped
              # here rather than carried.
              #
              # This is the same defect as the continued-line case one
              # commit earlier: a reader counting LINES where the shell
              # counts COMMANDS, and the two differ over exactly these
              # two kinds of line.
              /^[ \t]*#/ { pend = 0; next }
              /^[ \t]*$/ { pend = 0; next }
              {
                if (pend && NR == pend + 1 && $0 ~ /^[ \t]*(\|\||&&)/ \
                    && $0 !~ /^[ \t]*\|\|[ \t]*true([ \t;)]|$)/) {
                    printf "%d:%s\n", pend, pendline
                }
                # Consumer four, on the next line that RUNS. No NR
                # arithmetic: the comment and blank rules above already
                # skipped everything that is not a command, and qpend
                # is cleared below, so only that one line is examined.
                if (qpend && $0 ~ /[$][?]/) {
                    printf "%d:%s\n", qpend, qpendline
                }
                pend = 0; qpend = 0
                rest = $0
                while (match(rest, /[$]\([^)]*\|[ \t]*head[^)]*\)/)) {
                    rs = RSTART; rl = RLENGTH
                    subst = substr(rest, rs, rl)
                    after = substr(rest, rs + rl)
                    if (consumed(after)) { printf "%d:%s\n", NR, $0; break }
                    # Consumer four, same line: `x=$(p | head -1); rc=$?`
                    if (after ~ /[$][?]/) { printf "%d:%s\n", NR, $0; break }
                    # Consumer one. A `|| true` INSIDE the substitution
                    # already throws the status away before -e can see
                    # it, which is the exemption stated out loud.
                    if (sete == 1 && toplevel(subst) \
                        && subst !~ /\|\|[ \t]*true/) {
                        printf "%d:%s\n", NR, $0
                        break
                    }
                    # Nothing but a line continuation after the
                    # substitution: the operator, if any, is on the next
                    # line. `after` still holds that backslash, so
                    # testing it for whitespace ALONE never matches --
                    # which is how the first version silently stopped
                    # covering the one form the bug was found in.
                    if (after ~ /^[ \t]*\\[ \t]*$/) {
                        pend = NR; pendline = $0
                    }
                    # Same for `$?`: an assignment with nothing after it
                    # may still be read by the next line.
                    if (after ~ /^[ \t]*(\\[ \t]*)?$/) {
                        qpend = NR; qpendline = $0
                    }
                    if (rs + rl > length(rest)) break
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
