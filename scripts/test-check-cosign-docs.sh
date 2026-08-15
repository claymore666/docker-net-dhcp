#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-cosign-docs.sh (#522). Drives the gate
# through its DOCS_ROOT / TOOLING_SCRIPT seams against synthetic doc trees,
# so its red path is exercised rather than assumed.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-cosign-docs.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
# check NAME WANT_EXIT TOOLING_LINE PAGE_BODY GREP_PATTERN
check() {
    local name="$1" want_exit="$2" tooling="$3" body="$4" want_grep="$5"
    local root
    root="$(mktemp -d -p "$TMP")"
    mkdir -p "$root/scripts" "$root/docs"
    printf '%s\n' "$tooling" > "$root/scripts/check-release-tooling.sh"
    printf '%s\n' "$body" > "$root/docs/verifying-releases.md"

    DOCS_ROOT="$root" TOOLING_SCRIPT="$root/scripts/check-release-tooling.sh" \
        bash "$CHECK" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -qF "$want_grep" "$TMP/out"; then ok=0; fi

    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

DOC_WITH_VERSION=$'# Verify\n\nYou need **cosign v3 or newer**.\n\n```sh\ncosign verify-blob --bundle b.json checksums.txt\n```'
DOC_WITHOUT=$'# Verify\n\n```sh\ncosign verify-blob --bundle b.json checksums.txt\n```'
DOC_WRONG_MAJOR=$'# Verify\n\nYou need `cosign v2 or newer`.\n\n```sh\ncosign verify-blob --bundle b.json checksums.txt\n```'

check "page states the required major => green" 0 \
    'COSIGN_MAJOR=3' "$DOC_WITH_VERSION" "PASS  docs/verifying-releases.md"

check "page prints the command but states nothing => red" 1 \
    'COSIGN_MAJOR=3' "$DOC_WITHOUT" "FAIL  docs/verifying-releases.md"

# The drift case this gate exists for: the tooling preflight moves to a new
# major and the user-facing page still names the old one.
check "tooling bumped, page left behind => red" 1 \
    'COSIGN_MAJOR=4' "$DOC_WITH_VERSION" "cosign v4 or newer"

check "page names an older major than the tooling => red" 1 \
    'COSIGN_MAJOR=3' "$DOC_WRONG_MAJOR" "FAIL  docs/verifying-releases.md"

# Markdown emphasis and code ticks must not defeat the match.
check "backticked phrase still counts => green" 0 \
    'COSIGN_MAJOR=3' \
    $'# Verify\n\nNeeds `cosign v3 or newer`.\n\n```sh\ncosign verify x\n```' \
    "PASS  docs/verifying-releases.md"

# Source-of-truth failures must be loud (exit 2), not a quiet green.
check "no COSIGN_MAJOR assignment => exit 2" 2 \
    '# nothing here' "$DOC_WITH_VERSION" "no COSIGN_MAJOR="

# A doc tree with no cosign command at all is a broken search, not a pass:
# the real repo always has at least one such page.
check "no page prints a cosign command => exit 2" 2 \
    'COSIGN_MAJOR=3' $'# Nothing to see\n' "nothing to check"

if [ "$failures" -gt 0 ]; then
    echo "$failures test(s) failed" >&2
    exit 1
fi
echo "All check-cosign-docs.sh tests passed."
