#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-build-context.sh (#530). Drives the gate
# through its CONTEXT_DIR seam against synthetic contexts and asserts the
# verdict, so the gate's own red path is exercised rather than assumed.
#
# The last case is the one that matters most in practice: the gate plants
# a mode-000 directory in the context it checks, and has to remove it
# again. A gate that leaves that behind would itself become the defect it
# guards against.
#
# Requires: docker (the gate builds a FROM scratch context walk).
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-build-context.sh"
TMP="$(mktemp -d)"
cleanup() {
    find "$TMP" -type d ! -perm -u+rwx -exec chmod 755 {} + 2>/dev/null
    rm -rf "$TMP"
}
trap cleanup EXIT

if ! command -v docker >/dev/null 2>&1; then
    echo "SKIP: docker not available" >&2
    exit 0
fi

failures=0
# check NAME WANT_EXIT DOCKERIGNORE_CONTENT GREP_PATTERN
check() {
    local name="$1" want_exit="$2" ignore="$3" want_grep="$4"
    local ctx
    ctx="$(mktemp -d -p "$TMP")"
    : > "$ctx/go.mod"
    if [ -n "$ignore" ]; then printf '%s\n' "$ignore" > "$ctx/.dockerignore"; fi

    CONTEXT_DIR="$ctx" bash "$CHECK" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q "$want_grep" "$TMP/out"; then ok=0; fi

    # Whatever the verdict, the gate must not leave its fixture behind.
    if [ -e "$ctx/.claude/check-build-context-fixture" ]; then
        ok=0
        echo "    (fixture left behind in the checked context)"
    fi

    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

check "ignore rule present => green" 0 \
    '.claude/' \
    "OK: .claude/ is excluded"

check "ignore rule absent => red" 1 \
    $'/bin/\n.git/\ndocs/' \
    "FAIL: an unreadable path under .claude/"

check "no .dockerignore at all => red" 1 \
    '' \
    "FAIL: an unreadable path under .claude/"

# A rule that only matches the string, not the property: ignoring a
# sibling path leaves .claude/ in the context. Guards against someone
# "fixing" the gate by ignoring the wrong thing.
check "unrelated ignore rule => red" 1 \
    '.claude-backup/' \
    "FAIL: an unreadable path under .claude/"

if [ "$failures" -gt 0 ]; then
    echo "$failures test(s) failed" >&2
    exit 1
fi
echo "All check-build-context.sh tests passed."
