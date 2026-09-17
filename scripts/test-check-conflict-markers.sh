#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-conflict-markers.sh (#818).
#
# It drives all three verdicts, and it drives the three marker
# spellings SEPARATELY. A gate built from a list of patterns fails in
# one particular way: the first pattern works, the alternation is
# misspelled for the second, and nothing reports anything -- so each
# marker is planted on its own, with the other two absent.
#
# It also drives the two edges that decide whether the gate cries wolf:
# what is NOT a marker (eight equals, a bare seven-character line with
# no trailing text) and what is NOT in the domain (an untracked file, a
# file git calls binary).
#
# The fixtures COMPOSE their marker lines instead of typing them,
# because this file is inside the gate's own domain. A heredoc holding
# a literal marker would make the gate red on this repository, which is
# the self-referential shape a tree gate has and a Go gate does not.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

CHECK="$HERE/check-conflict-markers.sh"
guarded_tmpdir TMP

OPEN_M="$(printf '<%.0s' 1 2 3 4 5 6 7)"
MID_M="$(printf '=%.0s' 1 2 3 4 5 6 7)"
CLOSE_M="$(printf '>%.0s' 1 2 3 4 5 6 7)"
BASE_M="$(printf '|%.0s' 1 2 3 4 5 6 7)"

fails=0

# A repository with one clean tracked document and one clean tracked
# source, so the domain is never exactly the file a case plants into.
build_repo() {
    local d="$1"
    rm -rf "$d"; mkdir -p "$d/docs" "$d/.github/workflows"
    git init -q "$d"
    printf 'A clean release note.\n' > "$d/RELEASE_NOTES.md"
    printf 'package p\n\nfunc f() {}\n' > "$d/docs/thing.go"
    printf 'name: w\non: push\n' > "$d/.github/workflows/w.yaml"
    git -C "$d" add -A
}

# run <case> <expected-rc> [grep-for]
run() {
    local what="$1" want="$2" needle="${3-}" out rc
    out="$(cd "$TMP/repo" && bash "$CHECK" 2>&1)"; rc=$?
    if [ "$rc" -ne "$want" ]; then
        echo "FAIL  $what: exit $rc, want $want"
        printf '%s\n' "$out" | sed 's/^/      /'
        fails=$((fails + 1))
        return
    fi
    if [ -n "$needle" ] && ! printf '%s\n' "$out" | grep -F "$needle" >/dev/null; then
        echo "FAIL  $what: exit $want as expected, but the report does not name '$needle'"
        printf '%s\n' "$out" | sed 's/^/      /'
        fails=$((fails + 1))
        return
    fi
    echo "ok    $what"
}

# --- a clean tree passes ---------------------------------------------
build_repo "$TMP/repo"
run "a clean tracked tree" 0

# --- each marker spelling, alone -------------------------------------
for spec in "opening|$OPEN_M HEAD" "middle|$MID_M" "closing|$CLOSE_M 20ac707 (a subject)"; do
    build_repo "$TMP/repo"
    printf 'before\n%s\nafter\n' "${spec#*|}" >> "$TMP/repo/RELEASE_NOTES.md"
    git -C "$TMP/repo" add -A
    run "a planted ${spec%%|*} marker is caught, with its file:line" 1 "RELEASE_NOTES.md:3:"
done

# --- what is NOT a marker --------------------------------------------
# Eight equals is a setext underline, not the seven git writes; a bare
# seven-character line with nothing after it is not a marker git
# produces. Both stay green, and that is the gate's stated escape.
build_repo "$TMP/repo"
{ printf 'A heading\n========\n'; printf '%s\n' "$OPEN_M"; printf '%s\n' "$CLOSE_M"; } >> "$TMP/repo/RELEASE_NOTES.md"
git -C "$TMP/repo" add -A
run "an eight-equals underline and bare marker characters are not markers" 0

# --- the domain is every tracked file, and that is DRIVEN -------------
# The gate's whole reason for existing is that the marker can be in any
# file, so the cases above are not enough on their own: every one of
# them plants into RELEASE_NOTES.md, and a gate narrowed to that one
# path passes all of them. These two plant where nothing else does.
for target in docs/thing.go .github/workflows/w.yaml; do
    build_repo "$TMP/repo"
    printf 'before\n%s HEAD\nafter\n' "$OPEN_M" >> "$TMP/repo/$target"
    git -C "$TMP/repo" add -A
    run "a marker in $target is caught, so the domain is not one path" 1 "$target:"
done

# --- the diff3 base marker, stated as a bound and pinned here ---------
# `merge.conflictStyle=diff3` writes a seven-pipe line between the two
# sides. It is NOT matched: it never appears without the outer two,
# which are, so catching it buys nothing and a table row of empty cells
# would pay for it. This case pins that as a decision, not an accident.
build_repo "$TMP/repo"
printf 'before\n%s base\nafter\n' "$BASE_M" >> "$TMP/repo/RELEASE_NOTES.md"
git -C "$TMP/repo" add -A
run "a lone diff3 base marker is not matched, as the header states" 0

# --- the domain does not depend on the caller's cwd -------------------
# Run from a subdirectory with the marker one level up. A gate that
# greps `.` without going to the top of the tree passes here and is
# useless to anyone who runs it from `scripts/`.
build_repo "$TMP/repo"
printf 'before\n%s HEAD\nafter\n' "$OPEN_M" >> "$TMP/repo/RELEASE_NOTES.md"
git -C "$TMP/repo" add -A
out="$(cd "$TMP/repo/docs" && bash "$CHECK" 2>&1)"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s\n' "$out" | grep -F "RELEASE_NOTES.md:3:" >/dev/null; then
    echo "ok    a marker outside the caller's subdirectory is still caught"
else
    echo "FAIL  run from a subdirectory: exit $rc, want 1 naming RELEASE_NOTES.md:3:"
    printf '%s\n' "$out" | sed 's/^/      /'
    fails=$((fails + 1))
fi

# --- what is NOT in the domain ---------------------------------------
build_repo "$TMP/repo"
printf '%s HEAD\n' "$OPEN_M" > "$TMP/repo/docs/untracked.md"
run "an untracked file with a marker is out of the domain" 0

build_repo "$TMP/repo"
printf 'binary\000payload\n%s HEAD\n' "$OPEN_M" > "$TMP/repo/docs/blob.bin"
git -C "$TMP/repo" add -A
run "a tracked binary file is excluded, as git classifies it" 0

# --- the refusals ----------------------------------------------------
# A universal gate is satisfied by emptying its domain: an initialised
# repository with nothing tracked must REFUSE, not pass.
rm -rf "$TMP/repo"; git init -q "$TMP/repo"
run "an empty domain refuses instead of passing" 2 "domain is empty"

rm -rf "$TMP/repo"; mkdir -p "$TMP/repo"
run "a directory that is no repository refuses" 2 "not inside a git repository"

if [ "$fails" -eq 0 ]; then
    echo "check-conflict-markers.sh self-test passed"
    exit 0
fi
echo "check-conflict-markers.sh self-test: $fails case(s) failed" >&2
exit 1
