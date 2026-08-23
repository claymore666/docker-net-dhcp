#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-dockerignore-parity.sh. Two failure modes: a
# build-output directory ignored by git but shipped in the docker build
# context (/plugin-cover/ and /logs/ were both in that state), and a
# credential-shaped path ignored by git but shipped in the context —
# secrets/ was, because the gate's pattern was root-anchored and that
# block is not, so the lines that mattered most were the ones it could
# not see.
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
printf '/bin/*\n/plugin/\n/plugin-cover/\n*.key\n' > "$TMP/gitignore"
printf '/bin/\n/plugin/\n**/*.key\n'                > "$TMP/dockerignore"
check "a gitignored build dir absent from .dockerignore fails" rc1 "$(verdict)"

printf '/bin/\n/plugin/\n/plugin-cover/\n**/*.key\n' > "$TMP/dockerignore"
check "adding it passes" pass "$(verdict)"

# --- accepted spellings ------------------------------------------------
# Docker accepts the unanchored forms; the gate must not demand one
# spelling and fail a correct file.
for spelling in 'plugin-cover/' 'plugin-cover'; do
    printf '/bin/\n/plugin/\n**/*.key\n%s\n' "$spelling" > "$TMP/dockerignore"
    check "spelling '$spelling' is accepted" pass "$(verdict)"
done

# A name that merely CONTAINS the entry must not count as excluding it.
printf '/bin/\n/plugin/\n**/*.key\n/plugin-cover-extra/\n' > "$TMP/dockerignore"
check "a longer name does not satisfy the entry" rc1 "$(verdict)"

# --- unanchored directories are in scope -------------------------------
# This is the half the gate could not see. secrets/ is written without a
# leading slash, so the old root-anchored pattern skipped it and the
# private key inside rode into every build context.
printf '/bin/\n*.key\nsecrets/\n' > "$TMP/gitignore"
printf '/bin/\n**/*.key\n'         > "$TMP/dockerignore"
check "an unanchored ignored directory absent from .dockerignore fails" rc1 "$(verdict)"

printf '/bin/\n**/*.key\n**/secrets\n' > "$TMP/dockerignore"
check "excluding it passes" pass "$(verdict)"

# --- credential-shaped file ignores are in scope -----------------------
printf '/bin/\nsecrets/\n*.key\n' > "$TMP/gitignore"
printf '/bin/\n**/secrets\n'       > "$TMP/dockerignore"
check "a credential-shaped ignore absent from .dockerignore fails" rc1 "$(verdict)"

for spelling in '**/*.key' '*.key'; do
    printf '/bin/\n**/secrets\n%s\n' "$spelling" > "$TMP/dockerignore"
    check "credential spelling '$spelling' is accepted" pass "$(verdict)"
done

# The shape is what is matched, not a list of the lines present when this
# was written: a newly added extension must be caught by the gate rather
# than by whoever remembers to widen it.
printf '/bin/\nsecrets/\n*.key\n*.jks\n' > "$TMP/gitignore"
printf '/bin/\n**/secrets\n**/*.key\n'    > "$TMP/dockerignore"
check "a newly added credential extension is caught" rc1 "$(verdict)"

# --- scope: ordinary file ignores stay out of scope --------------------
printf '/bin/\nsecrets/\n*.key\nCLAUDE.md\ncode-review-report.md\n' > "$TMP/gitignore"
printf '/bin/\n**/secrets\n**/*.key\n'                                > "$TMP/dockerignore"
check "ordinary file ignores are not required" pass "$(verdict)"

# A multi-component path is a FILE ignore. Reading it as its leading
# directory would demand the whole test tree leave the build context.
printf '/bin/\nsecrets/\n*.key\ntest/arm64-netboot/nfs-watchdog/nfs-watchdog\n' > "$TMP/gitignore"
printf '/bin/\n**/secrets\n**/*.key\n'                                          > "$TMP/dockerignore"
check "a nested file path is not read as a directory entry" pass "$(verdict)"

# A negation must not be read as a directory to exclude.
printf '/bin/*\n!/bin/.gitkeep\n!secrets/\n*.key\n' > "$TMP/gitignore"
printf '/bin/\n**/*.key\n'                        > "$TMP/dockerignore"
check "a negation line is not treated as an entry" pass "$(verdict)"

# A commented-out entry is not an entry.
printf '/bin/\n*.key\n# /plugin/ is handled elsewhere\n# *.pem too\n' > "$TMP/gitignore"
printf '/bin/\n**/*.key\n'                                            > "$TMP/dockerignore"
check "a commented entry is ignored" pass "$(verdict)"

# --- the empty-glob guard ----------------------------------------------
# A gitignore yielding nothing must go red, not green. A comparison of
# zero entries reports success having compared nothing at all — the
# failure this whole class of gate exists to prevent.
printf '*.key\nCLAUDE.md\n' > "$TMP/gitignore"
printf '/bin/\n'            > "$TMP/dockerignore"
check "matching no directories is an error, not a pass" rc2 "$(verdict)"

# The same for the credential class: a .gitignore whose credential block
# has been deleted must go red rather than quietly compare nothing.
printf '/bin/\nCLAUDE.md\n' > "$TMP/gitignore"
printf '/bin/\n'             > "$TMP/dockerignore"
check "matching no credential patterns is an error, not a pass" rc2 "$(verdict)"

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
