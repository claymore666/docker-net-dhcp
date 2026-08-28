#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Refuse a command substitution whose `||` fallback fires on an input the
# primary command already answered correctly (#827).
#
# THE PROPERTY, NOT THE SPELLING. `grep -c PATTERN FILE` prints the count
# AND exits nonzero when the count is zero. That exit status is not an
# error -- it is the answer. So
#
#     n=$(grep -c . "$f" 2>/dev/null || echo 0)
#
# runs BOTH halves on the empty case: grep prints `0`, exits 1, the
# fallback prints `0` again, and the substitution yields the two-line
# string "0\n0". Feed that to `-eq` and it does not compare, it errors
# with "integer expression expected"; feed it to `=` and it silently
# fails every comparison.
#
# The general shape this refuses: a substitution where the primary
# command's output is correct on the very status that triggers the
# fallback, and the fallback ALSO prints. Two printers, one value.
#
# WHICH INPUT TRIGGERS IT -- narrower than it first looks, and worth
# stating exactly, because the narrow case is the expensive one.
#
#   populated file  grep prints N, exits 0. Fallback never runs. Fine.
#   ABSENT file     grep prints NOTHING and exits 2. The fallback is the
#                   only writer, so the value is a clean `0`. Also fine.
#   EMPTY file      grep prints `0` AND exits 1. BOTH write. Broken.
#
# So it is the empty-but-present file alone -- which is invariably the
# case the count exists to detect. The damage is invisible in every
# populated fixture, absent from the missing-file path, and present in
# the one that matters.
#
# THE CONSEQUENCE VARIES; THE DEFECT DOES NOT. Do not reason from one
# symptom. In scripts/test-check-attestation-parity.sh the value reached
# an `-eq`, which errored and left a diagnostic as unreachable dead code
# while all 24 cases passed. In scripts/test-runner-register.sh the
# comparison was a string, so the VERDICT stayed correct and only the
# message broke -- reporting `got '0` on one line and `0'` on the next.
# The quieter of the two is the more common one, so this gate reports
# the two-valued substitution itself and does not predict what it
# breaks. Measured 2026-08-27: two instances, two authors.
#
# THE CORRECT FORM assigns first and overrides on the primary's own
# status, so exactly one printer runs:
#
#     n=$(grep -c . "$f" 2>/dev/null) || n=0
#
# WHAT THIS CATCHES, AND WHAT IT DOES NOT. The reach is stated here so
# it is not discovered later by somebody's outage.
#
#   CAUGHT
#     - `grep -c` and `grep --count`, including bundled flags (-rc, -ci)
#     - `$( ... )` and backtick substitution
#     - fallbacks spelled `echo` or `printf`
#
#   DELIBERATELY NOT FLAGGED (these are correct code, not gaps)
#     - `|| true` and `|| :` -- the fallback prints NOTHING, so the
#       substitution is the primary's own `0`. Correct, and there is a
#       live instance in scripts/test-runner-register.sh that must stay
#       green; a gate that flagged it would be teaching the wrong fix.
#     - `wc -l`, `wc -c` and friends -- they exit 0 on empty input, so
#       the fallback never fires and there is no second printer. The
#       hazard needs a command that FAILS while printing the answer.
#
#   KNOWN GAPS (real, and each one a deliberate decision)
#     - the `||` on a line other than the substitution's opening: this
#       reads one line at a time, so a continuation splits the pattern.
#       Widening to a shell parser is not worth it for a shape that is
#       written on one line every time it has been written here.
#     - a pipeline before the `||` (`$(grep -c . f | tr -d ' ' || echo 0)`)
#       -- harmless without `set -o pipefail`, hazardous with it, and the
#       status depends on a setting this scan cannot see from the line.
#     - the command held in a variable (`$($counter f || echo 0)`) -- no
#       regex can see through the expansion.
#     - anything outside the tracked tree; see the domain note below.
#
# THE DOMAIN IS DERIVED, NOT CURATED. The defects were found in
# `test-*.sh`, and the first probe for an observer searched `check-*.sh`
# only -- returning empty, which read as "no gate exists" when it was
# really "the question could not be asked here". So this scans every
# tracked shell file: `*.sh` plus any tracked file whose first line is a
# shell shebang. Not a list. A list is what let the defect sit in a
# directory nobody thought to name.
#
# THE EXCLUSION: EXACTLY TWO PATHS, NAMED IN FULL.
#
#     scripts/check-counting-fallback.sh        (this file)
#     scripts/test-check-counting-fallback.sh   (its self-test)
#
# Both carry the hazard shape as DATA -- the self-test hands the two
# motivating defects to the gate verbatim, plus four spelling variants,
# a comment case and the two unexaminable-member fixtures, and this file
# quotes the bad form to explain it. Measured 2026-08-28 on this branch
# over base 9ae67ca: 9 findings, all of them fixtures, none of them code.
#
# CITE A TREE THE SHIPPED HISTORY CAN REACH. This paragraph named a
# pre-rebase object that resolves on GitHub and that nobody who clones
# the merge product can `git show`. A citation whose only reader is the
# web UI is a citation with a silent boundary; the base merge is
# reachable from every descendant, so that is what the numbers hang off.
#
# BY EXACT PATH, NOT BY PATTERN. A pattern would quietly excuse the next
# file whose name happens to match -- and here that is not theoretical:
# BOTH defects this gate was built from live in `test-*.sh` files
# (scripts/test-check-attestation-parity.sh and
# scripts/test-runner-register.sh). An exclusion spelled `test-*.sh`
# would drop the gate's entire reason for existing and still look
# tidy. Same call, same rationale, as scripts/check-upstream-blocker-claims.sh.
#
# AN EXCLUSION IS A BLIND SPOT, SO IT IS OBSERVED, NOT TRUSTED. The
# self-test scans these two files deliberately through SCAN_ROOT and
# compares the findings against a DECLARED SET, keyed on path plus the
# matched text. Not a count: a count is defeated by a compensating
# change -- a live hazard added as real code while a fixture leaves in
# the same commit keeps the total identical, and these are precisely the
# two files whose fixtures churn by design. A set member nobody declared
# goes red whatever else moved. Path-plus-text rather than path-plus-line
# because the fixture text is copied verbatim from the defects and does
# not move when this prose is reflowed, which it will be.
#
# `--list-domain` prints the domain and exits, so the boundary is
# checkable at a glance instead of inferred from this comment.
#
# THE DOMAIN IS TRACKED FILES, SO VERIFY AFTER `git add`. A gate keyed on
# `git ls-files` cannot see the file the commit is about to add to its
# own domain: run it against an untracked new self-test and it reports
# clean over a population missing exactly the file under test. That is
# how this gate passed locally and went red on its first push. Stage the
# change before believing a green.
#
# CI COMPOUNDS IT: `pull_request` checks out the MERGE REF, so its
# domain is the union of the branch's and the base's, not the branch
# head's. Measured 2026-08-28 over base 9ae67ca the two agree at 184
# files, because the branch is rebased onto that base -- which is the
# point rather than a counter-example. They agree only while the base
# has not moved, and the day it moves is the day a green measured on the
# head stops describing what CI scanned.
#
# NOT REACHABLE, AND IT MATTERS: `.claude/` is gitignored, so its 14
# shell instruments -- ci-slot.sh, review-verdict.sh and the rest -- are
# invisible to any gate keyed on tracked files. They are real tooling
# with real defects (one was fixed on 2026-08-27) and nothing in the
# gate corpus can see them. Stated rather than silently excluded.
#
# NON-VACUITY HAS A PARTIAL CASE, AND IT IS THE QUIET ONE. The guard
# below closes the all-or-nothing shape: a domain of zero files must
# refuse rather than report clean. It did NOT close the shape where the
# domain is populated and the SCAN drops members out of it. Measured
# 2026-08-28: point SCAN_ROOT at a directory holding one readable file
# and one mode-000 file carrying the hazard, and this gate printed
# `counting-fallback: clean, 2 shell file(s) examined.` and exited 0 --
# bash reported the permission error on stderr, the loop body never ran,
# `hits` stayed 0, and the count came from the DOMAIN rather than from
# what was read. A clean verdict over a file nobody opened is the same
# defect this gate exists to refuse, one level up, and it is worse than
# the empty case because the number beside it looks like evidence.
#
# So the two places a member can leave the scan -- not a regular file,
# and not openable -- each report the file by name and make the run
# EXIT 2. The final line reports what was EXAMINED, never the domain
# size; the two agree only when nothing was dropped, which is the
# property worth printing.
#
# THE CONSTRUCTIONS THAT EXPRESS THIS DIFFER BY PRIVILEGE, so the
# self-test drives the property from both ends -- see the cases in
# scripts/test-check-counting-fallback.sh. `chmod 000` is vacuous under
# root, and this gate runs in jobs and on workstations that are root.
#
# Usage: bash scripts/check-counting-fallback.sh [--list-domain]
#        --list-domain  print the files that would be scanned, one per
#                       line, and exit 0. The seam the self-test uses to
#                       assert what is IN the domain, not only what the
#                       scan of it returned.
# Env:   SCAN_ROOT   scan this directory's *.sh instead of the tracked
#                    tree -- the seam the self-test drives.
# Exit:  0 clean, 1 one or more findings, 2 cannot measure: an empty
#        domain, an unusable SCAN_ROOT, or a domain member the scan
#        could not read.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

LIST_ONLY=0
case "${1:-}" in
    --list-domain) LIST_ONLY=1 ;;
    "") ;;
    *) echo "usage: $0 [--list-domain]" >&2; exit 2 ;;
esac

# The hazard, as one extended regex.
#
#   \$\(|`      a command substitution opens
#   [^)]*       ... anything but its close, so the match stays inside it
#   grep        the primary
#   [^|]*       its arguments, up to the ||
#   (-[A-Za-z]*c|--count)   -c, bundled (-rc, -ci) or spelled long
#   [^|]*\|\|   the fallback separator
#   [[:space:]]*(echo|printf)   a fallback that ALSO prints
#
# `echo`/`printf` is the discriminator that keeps `|| true` green: the
# defect needs two printers, and `true` is not one.
# shellcheck disable=SC2016  # a regex, not a string to expand
HAZARD='(\$\(|`)[^)`]*grep([[:space:]]+-[A-Za-z]*c|[[:space:]]+--count)[^|]*\|\|[[:space:]]*(echo|printf)'

# Domain: tracked shell, or the self-test's directory.
domain() {
    if [ -n "${SCAN_ROOT:-}" ]; then
        [ -d "$SCAN_ROOT" ] || return 1
        find "$SCAN_ROOT" -type f -name '*.sh' 2>/dev/null | sort
        return 0
    fi
    git ls-files -z '*.sh' 2>/dev/null | tr '\0' '\n' | sed '/^$/d'
    # Property-keyed, not extension-keyed: a tracked shell script that
    # does not end in .sh is still shell. Today this adds nothing, which
    # is the point -- it will not need editing on the day one appears.
    local f
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        case "$f" in *.sh) continue ;; esac
        [ -f "$f" ] || continue
        # A here-string, not a pipeline: `grep -q` exits at the first
        # match and would SIGPIPE its producer under pipefail, which
        # scripts/check-pipefail-consumers.sh refuses. Nothing produces
        # into a here-string, so there is no pipe to break.
        first=$(head -n 1 -- "$f" 2>/dev/null)
        grep -qE '^#!.*[ /](ba)?sh\b' <<< "$first" && printf '%s\n' "$f"
    done < <(git ls-files 2>/dev/null)
}

FILES=$(domain) || {
    echo "::error title=Counting-fallback scan cannot measure::SCAN_ROOT" \
         "'${SCAN_ROOT:-}' is not a directory. Refusing rather than" \
         "reporting a clean scan of nothing." >&2
    exit 2
}

# The two paths above, removed by exact string match and nothing else.
# Not applied under SCAN_ROOT: that seam exists so the self-test can aim
# the gate AT these files, and excluding them there would make the
# observer unable to observe.
SELF="scripts/$(basename "$0")"
SELF_TEST="scripts/test-$(basename "$0")"
if [ -z "${SCAN_ROOT:-}" ]; then
    kept=""
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        case "$f" in "$SELF"|"$SELF_TEST") continue ;; esac
        kept="$kept$f
"
    done <<EOF
$FILES
EOF
    FILES="$kept"
fi

# THE NON-VACUITY GUARD. A scan of zero files reports success having
# examined nothing, which is the exact failure this gate exists to
# refuse -- one level up. It must go red, not green.
count=$(printf '%s\n' "$FILES" | sed '/^$/d' | wc -l)
if [ "$count" -eq 0 ]; then
    echo "::error title=Counting-fallback scan examined nothing::the domain is" \
         "empty, so a clean result would mean 'nothing was checked' rather than" \
         "'nothing is wrong'. Refusing. If this is a deliberate scope change," \
         "the guard is what has to be changed, deliberately." >&2
    exit 2
fi

if [ "$LIST_ONLY" -eq 1 ]; then
    printf '%s\n' "$FILES" | sed '/^$/d'
    exit 0
fi

hits=0
examined=0
unexaminable=0

# A member that leaves the scan is REPORTED, not dropped. Both arms below
# used to be a bare `continue`, which is how a clean verdict came to cover
# files nobody opened.
drop() {
    unexaminable=$((unexaminable + 1))
    echo "::error file=$1,title=Counting-fallback scan could not examine a domain" \
         "member::$1 is in the domain but $2, so it was NOT read. Refusing:" \
         "a clean result here would report on a file this scan never" \
         "examined, which is the failure the gate exists to refuse." >&2
}

while IFS= read -r f; do
    [ -n "$f" ] || continue
    # Not a regular file. Under SCAN_ROOT `find -type f` cannot produce
    # one directly -- but it emits NEWLINE-SEPARATED paths, so a name
    # carrying a newline arrives as two members that do not exist, and no
    # capability overrides that. Under the tracked domain, `git ls-files`
    # lists a file deleted from the working tree.
    if [ ! -f "$f" ]; then
        drop "$f" "is not a regular file"
        continue
    fi
    # OPEN IT EXPLICITLY so a failure is a verdict rather than a stderr
    # line. `done < "$f"` on an unopenable file leaves the loop body
    # unexecuted: bash prints "Permission denied", `hits` stays 0, and the
    # scan carries on. The brace group takes the stderr redirection BEFORE
    # the open is attempted -- `exec 3< "$f" 2>/dev/null` would not,
    # because redirections apply left to right and the open comes first.
    if ! { exec 3< "$f"; } 2>/dev/null; then
        drop "$f" "could not be opened for reading"
        continue
    fi
    examined=$((examined + 1))
    # A comment-only line is documentation OF the hazard, not the hazard.
    # Both known instances carry a comment quoting the bad form directly
    # above the fixed line; flagging those would make the fix unlandable.
    # An inline trailing comment is NOT stripped -- deciding where a `#`
    # ends a command needs a parser, and over-reporting is the cheap
    # direction here.
    while IFS= read -r line <&3; do
        case "${line#"${line%%[![:space:]]*}"}" in '#'*) continue ;; esac
        grep -qE "$HAZARD" <<< "$line" || continue
        n=${line:0:200}
        hits=$((hits + 1))
        echo "::error file=$f,title=Counting fallback fires on the answer::$f:" \
             "\`$n\` -- the primary already prints the correct value on the" \
             "status that triggers the fallback, so both print and the" \
             "substitution holds two values. Assign first and override on" \
             "its own status: \`n=\$(grep -c . f) || n=0\`." >&2
    done
    exec 3<&-
done <<EOF
$FILES
EOF

# AN INCOMPLETE SCAN CANNOT BE DISCHARGED BY ITS OWN PARTIAL FINDINGS, so
# this outranks the findings exit: 2 says "the question was not fully
# asked", which is a different thing from "the answer is no".
if [ "$unexaminable" -gt 0 ]; then
    echo "counting-fallback: cannot measure -- $unexaminable of $count domain" \
         "member(s) could not be examined ($examined examined, $hits finding(s))." \
         "Refusing rather than reporting on a population this scan did not read." >&2
    exit 2
fi

if [ "$hits" -gt 0 ]; then
    echo "counting-fallback: $hits finding(s) across $examined file(s)." >&2
    exit 1
fi

# THE EXAMINED COUNT, NEVER THE DOMAIN SIZE. The number beside a clean
# verdict has to be what was read, or it is decoration that reads as
# evidence.
#
# AND IT IS A NO-OP TODAY, WHICH IS THE POINT AND IS SAID RATHER THAN
# LEFT FOR A REVIEWER TO REDISCOVER. Every domain member either raises
# `examined` or raises `unexaminable`, and a nonzero `unexaminable`
# exits above, so `examined` and `count` are provably equal on the only
# path that reaches this line. Swapping `$examined` for `$count` here
# survives the whole suite, and no case can kill it: the two cannot
# differ where they are printed. The mutant is a NO-OP, not a missing
# test, and adding a case that asserts they agree would assert a
# tautology. It stays as `$examined` because the invariant is what the
# arithmetic above is FOR -- the day somebody adds a third way out of
# the loop without a `drop`, this line keeps telling the truth and
# `$count` starts lying. What IS observed is the counter itself: the
# self-test drives a mixed domain and reads the split back.
echo "counting-fallback: clean, $examined shell file(s) examined."
