#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# No integration test may name a lane's plugin build directory itself
# (#583). The one place that knows them is harness/build.go, and every
# test that needs the built rootfs asks harness.BuiltPluginDir.
#
# WHY THIS EXISTS
#
# The two lanes build into different directories: `make plugin` ->
# plugin/ on every PR, `make plugin-cover` -> plugin-cover/ on release
# PRs only. A test that spells one of them is green in the lane that
# builds it and red in the other — and the other runs once per release,
# so the red arrives mid-release with a tag waiting, which is exactly
# when the temptation to weaken the test is highest. #541 was born that
# way and stayed green for a whole cycle. #582 fixed the instance; the
# accessor fixes the class; this gate keeps the class fixed.
#
# WHAT IT LOOKS FOR
#
# The literal directory names, as a Go string or a path segment:
#   "plugin-cover"      the coverage lane's directory
#   "plugin/rootfs"     the PR lane's rootfs path
#   Join(<root>, "plugin") the PR lane's directory assembled from the
#                       repo root
# in every .go file under test/integration EXCEPT the accessor and its
# unit test, which is where they are allowed to live. Comments are not
# exempt — a comment that names the directory is a comment the next
# author copies from.
#
# Usage: check-build-dir-refs.sh [--root <dir>]
# Exit:  0 nothing outside the accessor names a build directory
#        1 a test file names one; each offending line is printed
#        2 the gate cannot see: no test directory, or the accessor
#          file that is supposed to carry the names is missing them
#          (which would mean the names moved and this gate is watching
#          the wrong thing)

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
while [ $# -gt 0 ]; do
    case "$1" in
        --root) ROOT="${2:-}"; shift 2 || exit 2 ;;
        *) echo "usage: $0 [--root <dir>]" >&2; exit 2 ;;
    esac
done

TESTDIR="$ROOT/test/integration"
ACCESSOR="$TESTDIR/harness/build.go"
if [ ! -d "$TESTDIR" ]; then
    echo "::error title=build-dir gate cannot see::$TESTDIR is not a directory" >&2
    exit 2
fi
if [ ! -f "$ACCESSOR" ] || ! grep -q '"plugin-cover"' "$ACCESSOR"; then
    echo "::error title=build-dir gate cannot see::$ACCESSOR does not name the build directories;" \
         "the accessor moved and this gate is watching the wrong file" >&2
    exit 2
fi

# The accessor and its own unit test are the two files allowed to spell
# the names.
ALLOWED_RE="^($ACCESSOR|${ACCESSOR%.go}_test.go):"
PATTERN='"plugin-cover"|plugin-cover/|plugin/rootfs|Join\([^)]*[Rr]oot[^)]*"plugin"\)'
offenders=$(grep -rnE --include='*.go' -- "$PATTERN" "$TESTDIR" \
    | grep -vE -- "$ALLOWED_RE" || true)

if [ -n "$offenders" ]; then
    echo "::error title=A test names a plugin build directory (#583)::the lanes build into" \
         "different directories, so a test that spells one is broken in the other lane." \
         "Ask harness.BuiltPluginDir() instead." >&2
    echo "$offenders" >&2
    exit 1
fi
echo "build-dir gate: no test outside $(basename "$ACCESSOR") names a plugin build directory"
exit 0
