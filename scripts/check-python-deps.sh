#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Python dependency-reachability gate (#535).
#
# `scripts/requirements.txt` declared two third-party distributions for
# `scripts/common.py`, the manifest-list helper inherited from upstream.
# Nothing installed the file and nothing imported the module: the
# multiarch targets they served were removed once #507 measured that
# `docker plugin install` cannot resolve a manifest list at all.
#
# Both files sat there for months looking like live build tooling. While
# investigating #507 someone read the scripts, found no install step
# anywhere, and concluded the dependencies were undeclared. They were
# declared. Nothing pointed at them, and nothing pointed out that
# nothing pointed at them.
#
# The resolution was to delete both, and this is what stops the shape
# from coming back. It checks two directions, because a declaration and
# its consumer can each go missing on their own:
#
#   A. every requirements.txt is named by a `pip install` line
#      somewhere, so a declaration nobody installs goes red;
#   B. every .py is reachable — named by basename from another
#      file, or imported as a module by another .py — so a consumer
#      nobody calls goes red.
#
# WHAT THIS DOES NOT COVER, stated so the next reader does not assume
# otherwise: it does not check that a requirements file declares the
# distributions its importers actually need. Module name and
# distribution name differ freely (`import dxf` comes from
# `python-dxf`), and there is no mechanical map between them, so that
# half needs a human. Nor does it judge pins or hashes — `docs/`
# installs with --require-hashes and the deleted file had neither,
# which is its own issue and not this one.
#
# `requirements.in` is deliberately out of scope for A: it is the
# pip-compile SOURCE for a requirements.txt, so nothing installs it and
# nothing should.
#
# Usage: check-python-deps.sh
# Env:   PYDEPS_ROOT  repository to inspect (default: the repo this
#                     script lives in) — the seam the self-test drives.
# Exit:  0 clean, 1 something is unreachable, 2 cannot check.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${PYDEPS_ROOT:-$(cd "$HERE/.." && pwd)}"

if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "::error title=Not a git repository::$ROOT — this gate discovers" \
         "files through the git index, so it cannot inspect anything here." >&2
    exit 2
fi

# Discovery through the index, not a filesystem walk. A walk descends
# into gitignored worktrees and judges another branch's files as if they
# were this one's — check-dockerfile-pins hit exactly that (#639).
# UNTRACKED FILES COUNT TOO (#743). A file written but not yet `git
# add`ed is invisible to `ls-files` alone, and that is precisely the
# state a source file is in for the whole time someone is writing it.
# This gate runs in scripts/local-lane.sh, where an uncommitted working
# tree is the NORMAL state — so the blind spot sat exactly where the
# gate is meant to be useful, and never showed in CI, where a fresh
# checkout makes tracked and present mean the same thing. Same argument
# and same fix as check-license-headers.sh:74.
mapfile -t REQS < <({
    git -C "$ROOT" ls-files -- '*requirements.txt'
    git -C "$ROOT" ls-files --others --exclude-standard -- '*requirements.txt'
} | sort -u)
mapfile -t PYS < <({
    git -C "$ROOT" ls-files -- '*.py'
    git -C "$ROOT" ls-files --others --exclude-standard -- '*.py'
} | sort -u)

# Inspecting nothing is not a pass. Every gate here that reported success
# over an empty input set was hiding something by the time anyone looked.
if [ "${#REQS[@]}" -eq 0 ] && [ "${#PYS[@]}" -eq 0 ]; then
    echo "::error title=Nothing to inspect::no requirements.txt and no .py" \
         "files in $ROOT. This gate would otherwise report a" \
         "clean pass having looked at nothing." >&2
    exit 2
fi

findings=0
report() {
    findings=$((findings + 1))
    printf '  %-34s %s\n' "$1" "$2" >&2
}

# --- A. a declaration nobody installs --------------------------------
#
# THE PATH HAS TO MATCH AT A BOUNDARY, NOT AS A SUBSTRING (#743). A
# fixed-string search for `requirements.txt` is satisfied by any line
# naming `ci/runner-image/requirements.txt`, so a NEW orphan at the repo
# root read as installed the moment any other requirements file was
# installed anywhere. Found by running the untracked-file probe from
# #743 against the fixed gate: it still passed, and the file selection
# was no longer the reason.
#
# The boundary only has to be checked on the left. What produces a false
# match is a longer path ending in this one, so rejecting a match
# preceded by a path character is exactly the fix; a match preceded by a
# space, a quote or the start of the line is a genuine reference.
#
# COMMENTS ARE NOT INSTALL LINES, and leaving them in made this gate
# satisfy itself: its own header at :23 reads "every requirements.txt is
# named by a `pip install` line", which contains the path at a word
# boundary and the words `pip install`. So the sentence describing the
# rule was accepted as evidence that the rule held. Sibling gates strip
# comments for the mirror-image reason — check-selftest-fixtures.sh so
# prose does not TRIP it; here so prose does not SATISFY it.
#
# THE EVIDENCE SET HAS TO MATCH THE SUBJECT SET (#743). Adding untracked
# files to the subjects above without adding them here made this gate
# WORSE, not better: a newly-written requirements.txt was now inspected,
# but the newly-written workflow that installs it was still invisible,
# so the local lane reported an orphan for every pair an author was in
# the middle of writing. Caught by the self-test case for the fix to the
# subject set — one half of a union is not a smaller version of the
# union, it is a different, wrong question. `--untracked` searches
# tracked plus untracked-not-ignored, which is exactly the subject set.
for req in "${REQS[@]}"; do
    req_re=$(printf '%s' "$req" | sed 's/[][\.*^$+?(){}|/]/\\&/g')
    if git -C "$ROOT" grep --untracked -nI -F -e "$req" -- ":!$req" 2>/dev/null \
         | grep -vE '^[^:]*:[0-9]+:[[:space:]]*#' \
         | grep -E 'pip[0-9.]*[[:space:]]+install|pip_install' \
         | grep -E "(^|[^[:alnum:]_./-])${req_re}([^[:alnum:]_.-]|\$)" >/dev/null; then
        continue
    fi
    report "$req" "declared but no 'pip install' line names it — nothing ever installs these."
done

# --- B. a consumer nobody calls --------------------------------------
for py in "${PYS[@]}"; do
    base="${py##*/}"
    mod="${base%.py}"

    # Named by path or basename from anywhere else: a workflow step, a
    # Makefile recipe, a runbook, a self-test.
    if git -C "$ROOT" grep --untracked -qI -F -e "$base" -- ":!$py" 2>/dev/null; then
        continue
    fi

    # Or imported as a module, where the filename never appears.
    if git -C "$ROOT" grep --untracked -qIE "^[[:space:]]*(import[[:space:]]+${mod}|from[[:space:]]+${mod}[[:space:]]+import)\b" \
         -- '*.py' ":!$py" 2>/dev/null; then
        continue
    fi

    report "$py" "nothing names it and nothing imports it — dead code that reads as live tooling."
done

if [ "$findings" -ne 0 ]; then
    echo >&2
    echo "::error title=Unreachable Python dependency::${findings} finding(s)." \
         "Wire it to a caller, or delete it — git remembers it either way." >&2
    exit 1
fi

echo "PASS  python deps reachable: ${#REQS[@]} requirements file(s) installed by something," \
     "${#PYS[@]} module(s) reachable"
