#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-python-deps.sh (#535).
#
# Every signal is exercised in BOTH directions. A reachability gate is
# especially easy to get one-directional: one that never fires is
# useless, and one that fires on everything gets waived by reflex and is
# useless in the same way a week later.
set -u

GATE="$(cd "$(dirname "$0")" && pwd)/check-python-deps.sh"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# Each case builds a throwaway repo from a list of "path<TAB>content"
# entries and commits it. A real git index, because the gate discovers
# through `git ls-files` and `git grep` — a mocked listing would test
# the mock.
run_case() {
    local name="$1" want="$2"; shift 2
    local dir rc out
    dir=$(mktemp -d)
    (
        cd "$dir" || exit 2
        git init -q .
        git config user.email t@t; git config user.name t
        git config commit.gpgsign false
        while [ "$#" -gt 0 ]; do
            local path="${1%%:::*}" body="${1#*:::}"
            mkdir -p "$(dirname "$path")"
            printf '%s\n' "$body" > "$path"
            shift
        done
        git add -A
        git commit -qm fixture
    ) >/dev/null 2>&1
    out=$(PYDEPS_ROOT="$dir" bash "$GATE" 2>&1)
    rc=$?
    rm -rf "$dir"
    if [ "$rc" = "$want" ]; then
        ok "$name"
    else
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" >&2
    fi
}

INSTALLER='name: ci
jobs:
  x:
    steps:
      - run: pip install -r scripts/requirements.txt'

# --- A. a declaration nobody installs --------------------------------

# The exact shape that was in the tree: a requirements file and the
# module it served, with no installer and no importer anywhere.
run_case "the shipped orphan pair is reported" 1 \
    "scripts/requirements.txt:::python-dxf>7, <8" \
    "scripts/common.py:::import dxf"

run_case "a requirements.txt with an installer is clean" 0 \
    "scripts/requirements.txt:::python-dxf>7, <8" \
    ".github/workflows/ci.yml:::$INSTALLER" \
    "scripts/common.py:::import dxf" \
    "scripts/use.py:::import common" \
    "scripts/run.sh:::python3 scripts/use.py"

# The near miss, and the reason the gate greps for the install verb
# rather than the path alone. A path mentioned in prose reads as wired
# up to a search and is not: this is how the file survived review.
run_case "a requirements.txt only MENTIONED in prose is still reported" 1 \
    "scripts/requirements.txt:::python-dxf>7, <8" \
    "README.md:::The deps live in scripts/requirements.txt for reference." \
    "scripts/common.py:::import dxf" \
    "scripts/use.py:::import common" \
    "scripts/run.sh:::python3 scripts/use.py"

# pip-compile's source file is not installable and must not be demanded.
run_case "a requirements.in is not required to have an installer" 0 \
    "docs/requirements.in:::mkdocs-material" \
    "docs/requirements.txt:::mkdocs-material==9.0.0" \
    ".github/workflows/pages.yml:::run: pip install --require-hashes -r docs/requirements.txt"

# --- B. a consumer nobody calls --------------------------------------

run_case "a .py nothing names and nothing imports is reported" 1 \
    "scripts/orphan.py:::import json" \
    "README.md:::Nothing here points at it."

run_case "a .py named by basename elsewhere is clean" 0 \
    "scripts/tool.py:::import json" \
    "scripts/test-tool.sh:::exec python3 \"\$(dirname \"\$0\")/tool.py\""

# The case a basename grep alone would miss entirely: an imported module
# is reachable, and its FILENAME appears nowhere. Without this branch
# the gate would demand a caller name a file that is never named by one.
run_case "a .py imported as a module is clean" 0 \
    "scripts/common.py:::CONST = 1" \
    "scripts/main.py:::import common" \
    "scripts/run.sh:::python3 main.py"

run_case "a from-import also counts as reachable" 0 \
    "scripts/common.py:::CONST = 1" \
    "scripts/main.py:::from common import CONST" \
    "scripts/run.sh:::python3 main.py"

# --- refusing to judge -----------------------------------------------

# A repo with neither kind of file must not report a clean pass: the
# same empty-input blind spot that #569, #564 and #487 each turned out
# to be. Inspecting nothing is exit 2.
run_case "a repo with no python and no requirements exits 2" 2 \
    "README.md:::nothing to see"

dir=$(mktemp -d)
if PYDEPS_ROOT="$dir" bash "$GATE" >/dev/null 2>&1; then
    no "a non-git directory should not report clean"
else
    [ "$?" = 2 ] && ok "a non-git directory exits 2" || no "a non-git directory should exit 2"
fi
rm -rf "$dir"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
