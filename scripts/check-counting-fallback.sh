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
# NOT REACHABLE, AND IT MATTERS: `.claude/` is gitignored, so its 14
# shell instruments -- ci-slot.sh, review-verdict.sh and the rest -- are
# invisible to any gate keyed on tracked files. They are real tooling
# with real defects (one was fixed on 2026-08-27) and nothing in the
# gate corpus can see them. Stated rather than silently excluded.
#
# Usage: bash scripts/check-counting-fallback.sh
# Env:   SCAN_ROOT   scan this directory's *.sh instead of the tracked
#                    tree -- the seam the self-test drives.
# Exit:  0 clean, 1 one or more findings, 2 cannot measure / empty domain.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

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

hits=0
while IFS= read -r f; do
    [ -n "$f" ] || continue
    [ -f "$f" ] || continue
    # A comment-only line is documentation OF the hazard, not the hazard.
    # Both known instances carry a comment quoting the bad form directly
    # above the fixed line; flagging those would make the fix unlandable.
    # An inline trailing comment is NOT stripped -- deciding where a `#`
    # ends a command needs a parser, and over-reporting is the cheap
    # direction here.
    while IFS= read -r line; do
        case "${line#"${line%%[![:space:]]*}"}" in '#'*) continue ;; esac
        grep -qE "$HAZARD" <<< "$line" || continue
        n=${line:0:200}
        hits=$((hits + 1))
        echo "::error file=$f,title=Counting fallback fires on the answer::$f:" \
             "\`$n\` -- the primary already prints the correct value on the" \
             "status that triggers the fallback, so both print and the" \
             "substitution holds two values. Assign first and override on" \
             "its own status: \`n=\$(grep -c . f) || n=0\`." >&2
    done < "$f"
done <<EOF
$FILES
EOF

if [ "$hits" -gt 0 ]; then
    echo "counting-fallback: $hits finding(s) across $count file(s)." >&2
    exit 1
fi

echo "counting-fallback: clean, $count shell file(s) examined."
