#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# No tracked file carries a merge-conflict marker (#818).
#
# WHY. A hand-resolved rebase can leave a conflict block behind in a
# file no compiler reads. RELEASE_NOTES.md carried two of them to a
# branch tip: `scripts/release-body.sh v2.2.0` emitted four marker
# lines, release.yml feeds that text to the GitHub release body, and
# the stale bullet the branch falsifies sat inside the shipped section
# beside its replacement. The whole lane was green: every gate in it
# reads Go, YAML, health fields, option tables or prose it knows the
# shape of, and a marker is none of those. A commit-by-commit `go vet`
# cannot see a markdown file either.
#
# DOMAIN: every tracked file, minus the ones git itself calls binary.
# A universal gate is satisfied by emptying its domain, so a domain of
# zero files is a REFUSAL here, not a pass.
#
# THE BOUNDARY, beside the rule. The middle marker is a line of exactly
# seven `=` and nothing else, which a markdown setext underline can
# also be. MEASURED 2026-09-17 with `git grep -nE '^={7,}$'`: no
# tracked file carries one, on this branch or on dev, so the strict
# form costs nothing today. If a page ever wants that underline, it
# gets an ATX heading instead. The outer two markers are matched with
# their trailing space, which is what git writes (`<<<<<<< HEAD`,
# `>>>>>>> 20ac707 (subject)`); a bare seven-character line is not a
# marker git produces and is not caught. The seven-pipe line that
# `merge.conflictStyle=diff3` writes between the two sides is not
# matched either: it never appears without the outer two, which are, so
# it escapes only if those are deleted by hand and it is left behind.
# The self-test pins that as a decision instead of an accident.
#
# The repository this runs in is the one it judges, which is how the
# self-test drives it: it builds a fixture repository and cds into it.
#
# Usage: check-conflict-markers.sh
# Exit:  0 clean, 1 a marker found, 2 cannot check.

set -uo pipefail

if ! root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
    echo "FAIL  not inside a git repository, so there is no tracked tree to read" >&2
    exit 2
fi
# The whole repository, not the subtree someone happened to run this
# from: a gate whose domain depends on the caller's cwd reports a pass
# it did not earn.
cd "$root" || { echo "FAIL  cannot enter $root" >&2; exit 2; }

if [ -z "$(git ls-files | head -n 1)" ]; then
    echo "FAIL  no tracked files: the domain is empty and a pass would mean nothing" >&2
    exit 2
fi

# Built from pieces so this file, which is in its own domain, carries no
# marker line of its own. The self-test composes its fixtures the same way.
open_m="$(printf '<%.0s' 1 2 3 4 5 6 7)"
mid_m="$(printf '=%.0s' 1 2 3 4 5 6 7)"
close_m="$(printf '>%.0s' 1 2 3 4 5 6 7)"
pattern="^($open_m |$mid_m\$|$close_m )"

hits="$(git grep -nIE "$pattern" -- . 2>/dev/null)"
rc=$?
if [ "$rc" -gt 1 ]; then
    echo "FAIL  git grep could not read the tree (exit $rc)" >&2
    exit 2
fi

if [ "$rc" -eq 0 ] && [ -n "$hits" ]; then
    while IFS= read -r hit; do
        rest="${hit#*:}"
        echo "FAIL  ${hit%%:*}:${rest%%:*}: unresolved merge-conflict marker -- ${rest#*:}"
    done <<< "$hits"
    echo "FAIL  $(printf '%s\n' "$hits" | wc -l) marker line(s); resolve the conflict, do not delete the markers alone" >&2
    exit 1
fi

echo "conflict-marker gate passed"
exit 0
