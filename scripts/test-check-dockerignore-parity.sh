#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-dockerignore-parity.sh. The failure mode it guards
# is a build-output directory ignored by git but shipped in the docker
# build context — /plugin-cover/ and /logs/ were both in that state.
set -euo pipefail

CHECK="$(dirname "$0")/check-dockerignore-parity.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}
run() { bash "$CHECK" "$TMP/gitignore" "$TMP/dockerignore" >/dev/null 2>&1; }
verdict() { run && echo pass || echo "rc$?"; }

# --- the real defect: an ignored build dir missing from the context ----
printf '/bin/*\n/plugin/\n/plugin-cover/\n' > "$TMP/gitignore"
printf '/bin/\n/plugin/\n'                  > "$TMP/dockerignore"
check "a gitignored build dir absent from .dockerignore fails" rc1 "$(verdict)"

printf '/bin/\n/plugin/\n/plugin-cover/\n' > "$TMP/dockerignore"
check "adding it passes" pass "$(verdict)"

# --- accepted spellings ------------------------------------------------
# Docker accepts the unanchored forms; the gate must not demand one
# spelling and fail a correct file.
for spelling in 'plugin-cover/' 'plugin-cover'; do
    printf '/bin/\n/plugin/\n%s\n' "$spelling" > "$TMP/dockerignore"
    check "spelling '$spelling' is accepted" pass "$(verdict)"
done

# A name that merely CONTAINS the entry must not count as excluding it.
printf '/bin/\n/plugin/\n/plugin-cover-extra/\n' > "$TMP/dockerignore"
check "a longer name does not satisfy the entry" rc1 "$(verdict)"

# --- scope: non-directory and non-anchored ignores are out of scope ----
printf '/bin/\n*.key\nCLAUDE.md\n.vscode/\n' > "$TMP/gitignore"
printf '/bin/\n'                             > "$TMP/dockerignore"
check "unanchored and non-directory ignores are not required" pass "$(verdict)"

# A negation must not be read as a directory to exclude.
printf '/bin/*\n!/bin/.gitkeep\n' > "$TMP/gitignore"
printf '/bin/\n'                  > "$TMP/dockerignore"
check "a negation line is not treated as an entry" pass "$(verdict)"

# A commented-out entry is not an entry.
printf '/bin/\n# /plugin/ is handled elsewhere\n' > "$TMP/gitignore"
printf '/bin/\n'                                 > "$TMP/dockerignore"
check "a commented entry is ignored" pass "$(verdict)"

# --- the empty-glob guard ----------------------------------------------
# A gitignore yielding nothing must go red, not green. A comparison of
# zero entries reports success having compared nothing at all — the
# failure this whole class of gate exists to prevent.
printf '*.key\nCLAUDE.md\n' > "$TMP/gitignore"
printf '/bin/\n'            > "$TMP/dockerignore"
check "matching no entries is an error, not a pass" rc2 "$(verdict)"

# --- usage errors ------------------------------------------------------
got=$(bash "$CHECK" "$TMP/nope" "$TMP/dockerignore" >/dev/null 2>&1 && echo pass || echo "rc$?")
check "an unreadable gitignore is a usage error" rc2 "$got"
got=$(bash "$CHECK" "$TMP/gitignore" "$TMP/nope" >/dev/null 2>&1 && echo pass || echo "rc$?")
check "an unreadable dockerignore is a usage error" rc2 "$got"

# --- the gate against the real repo ------------------------------------
# The point of the gate is this file pair, so assert it directly rather
# than only against fixtures.
REPO="$(cd "$(dirname "$0")/.." && pwd)"
got=$(bash "$CHECK" "$REPO/.gitignore" "$REPO/.dockerignore" >/dev/null 2>&1 && echo pass || echo "rc$?")
check "the repo's own .gitignore/.dockerignore agree" pass "$got"

if [ "$fails" -ne 0 ]; then
    echo "check-dockerignore-parity.sh self-tests FAILED" >&2
    exit 1
fi
echo "All check-dockerignore-parity.sh tests passed."
