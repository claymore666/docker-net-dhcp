#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for resolve-dispatch-ref.sh (#738).
#
# The point of this script is that the dangerous shapes are not
# REJECTED, they are INEXPRESSIBLE: it can emit only `refs/heads/dev` or
# `refs/tags/vX.Y.Z[-rcN]`. So the cases below are not a denylist of
# things to refuse — they are a demonstration that nothing outside those
# two forms comes out, whatever goes in. A denylist is what the next
# unexpected shape walks around; that is how the guard this replaces
# came to match one literal input name and miss two workflows.
#
# The raw-SHA case is the one that mattered: pages.yml's deploy job
# holds contents: write, and its checkout used to take the dispatch
# input as-is, so any commit in the object store — including a fork's
# pull-request head — could be checked out and then executed by
# `pip install` and mkdocs hooks.
set -u

SCRIPT="$(cd "$(dirname "$0")" && pwd)/resolve-dispatch-ref.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
n=0

# resolves NAME EXPECTED
resolves() {
    local name="$1" want="$2"
    n=$((n + 1))
    local got
    got="$(bash "$SCRIPT" "$name" 2>"$TMP/err")"
    local rc=$?
    if [ "$rc" -eq 0 ] && [ "$got" = "$want" ]; then
        echo "PASS: '$name' -> $want"
    else
        echo "FAIL: '$name' -> want '$want' (exit 0), got '$got' (exit $rc)"
        sed 's/^/    /' "$TMP/err"
        failures=$((failures + 1))
    fi
}

# refuses NAME
refuses() {
    local name="$1"
    n=$((n + 1))
    local got
    got="$(bash "$SCRIPT" "$name" 2>"$TMP/err")"
    local rc=$?
    if [ "$rc" -ne 0 ] && [ -z "$got" ]; then
        echo "PASS: '$name' refused"
    else
        echo "FAIL: '$name' should be refused, got '$got' (exit $rc)"
        failures=$((failures + 1))
    fi
}

# --- the two forms that exist -----------------------------------------
resolves "dev" "refs/heads/dev"
resolves "v1.8.0" "refs/tags/v1.8.0"
resolves "v1.8.0-rc1" "refs/tags/v1.8.0-rc1"
resolves "v10.20.30-rc11" "refs/tags/v10.20.30-rc11"

# --- everything else ---------------------------------------------------
# A raw SHA is the shape that made pages.yml exploitable: it resolves,
# it is in the object store, and it is not on any branch of ours.
refuses "0279233bd2f6b7a3c1e4d5f60718293a4b5c6d7e"
refuses "refs/pull/42/head"
refuses "pull/42/merge"
refuses "main"
refuses "gh-pages"
refuses "attacker/branch"

# Anchoring, both ends. The guard this replaces used an unanchored
# pattern elsewhere in the tree and let `v1.2.3junk` through as a
# "pre-release".
refuses "v1.8.0junk"
refuses "v1.8.0.4"
refuses "xv1.8.0"
refuses "v1.8.0-rc"
refuses "v1.8.0-beta1"
refuses "v1.8"

# A ref that would be an argument rather than a ref if it reached a
# command line. Not expressible here whatever the consumer does with it.
refuses "--upload-pack=touch /tmp/pwned"
refuses "dev; touch /tmp/pwned"
refuses "\$(id)"

# --- the caller decides its own default -------------------------------
# An empty input must not silently become `dev`: a workflow that meant
# to publish a tag would publish the development branch and say nothing.
refuses ""

n=$((n + 1))
if bash "$SCRIPT" >/dev/null 2>&1; then
    echo "FAIL: no argument at all should be a usage error"
    failures=$((failures + 1))
else
    echo "PASS: no argument is a usage error"
fi

n=$((n + 1))
if bash "$SCRIPT" dev extra >/dev/null 2>&1; then
    echo "FAIL: two arguments should be a usage error"
    failures=$((failures + 1))
else
    echo "PASS: two arguments is a usage error"
fi

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
