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
#   A. every tracked requirements.txt is named by a `pip install` line
#      somewhere, so a declaration nobody installs goes red;
#   B. every tracked .py is reachable — named by basename from another
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
mapfile -t REQS < <(git -C "$ROOT" ls-files -- '*requirements.txt' | sort)
mapfile -t PYS  < <(git -C "$ROOT" ls-files -- '*.py' | sort)

# Inspecting nothing is not a pass. Every gate here that reported success
# over an empty input set was hiding something by the time anyone looked.
if [ "${#REQS[@]}" -eq 0 ] && [ "${#PYS[@]}" -eq 0 ]; then
    echo "::error title=Nothing to inspect::no requirements.txt and no .py" \
         "files are tracked in $ROOT. This gate would otherwise report a" \
         "clean pass having looked at nothing." >&2
    exit 2
fi

findings=0
report() {
    findings=$((findings + 1))
    printf '  %-34s %s\n' "$1" "$2" >&2
}

# --- A. a declaration nobody installs --------------------------------
for req in "${REQS[@]}"; do
    if git -C "$ROOT" grep -nI -F -e "$req" -- ":!$req" 2>/dev/null \
         | grep -qE 'pip[0-9.]*[[:space:]]+install|pip_install'; then
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
    if git -C "$ROOT" grep -qI -F -e "$base" -- ":!$py" 2>/dev/null; then
        continue
    fi

    # Or imported as a module, where the filename never appears.
    if git -C "$ROOT" grep -qIE "^[[:space:]]*(import[[:space:]]+${mod}|from[[:space:]]+${mod}[[:space:]]+import)\b" \
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
