#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for scripts/check-dhcp-golib-copy.sh.
#
# Every case builds a throwaway git repository with its own small
# "library copy" and points DEST_DIR at it, so nothing here depends on
# the real internal/dhcp-golib/ or on the private library repository.
# A case that shared a fixture with the one before it would be testing
# the residue of the previous case, which is a defect this repository
# has already paid for once.
#
# The gate has THREE verdicts and all three are driven: 0 clean, 1 a
# difference, 2 the check could not be made. A gate proven in one
# direction only is the shape that ships a check which can never fail.

set -uo pipefail

CHECK="$(cd "$(dirname "$0")" && pwd)/check-dhcp-golib-copy.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

pass=0; fail=0

# new_repo builds a repository holding a two-file library copy with a
# manifest, and echoes its path.
new_repo() {
    local d
    d=$(mktemp -d "$ROOT/caseXXXXXX")
    mkdir -p "$d/lib/sub"
    printf 'package a\n' >"$d/lib/a.go"
    printf 'package b\n' >"$d/lib/sub/b.go"
    printf 'tool\n' >"$d/lib/run.sh"
    chmod +x "$d/lib/run.sh"
    printf '%s\n' "0123456789abcdef0123456789abcdef01234567" >"$d/lib/SOURCE"
    git -C "$d" init -q
    git -C "$d" config user.email t@t; git -C "$d" config user.name t
    git -C "$d" add -A >/dev/null 2>&1
    ( cd "$d" && DEST_DIR=lib bash "$CHECK" --write >/dev/null 2>&1 )
    git -C "$d" add lib/MANIFEST >/dev/null 2>&1
    printf '%s' "$d"
}

run_in() { ( cd "$1" && DEST_DIR="${2:-lib}" bash "$CHECK" 2>&1 ); }

check() {
    local name="$1" want_rc="$2" want_text="$3" out rc
    out="$4"; rc="$5"
    if [ "$rc" != "$want_rc" ]; then
        echo "FAIL: $name — exit $rc, want $want_rc"
        echo "$out" | sed 's/^/    /'
        fail=$((fail + 1)); return
    fi
    if [ -n "$want_text" ] && ! printf '%s' "$out" | LC_ALL=C grep -qF -- "$want_text"; then
        echo "FAIL: $name — exit $rc as wanted, but the output does not mention: $want_text"
        echo "$out" | sed 's/^/    /'
        fail=$((fail + 1)); return
    fi
    echo "ok: $name"
    pass=$((pass + 1))
}

one() { # name want_rc want_text setup_fn
    local name="$1" want_rc="$2" want_text="$3" setup="$4" d out rc
    d=$(new_repo)
    "$setup" "$d"
    out=$(run_in "$d"); rc=$?
    check "$name" "$want_rc" "$want_text" "$out" "$rc"
}

noop() { :; }
edit_byte() { printf 'package a // edited\n' >"$1/lib/a.go"; }
flip_exec() { chmod -x "$1/lib/run.sh"; git -C "$1" add -A >/dev/null 2>&1; }
delete_file() { rm "$1/lib/sub/b.go"; git -C "$1" add -A >/dev/null 2>&1; }
add_tracked() { printf 'package c\n' >"$1/lib/c.go"; git -C "$1" add -A >/dev/null 2>&1; }
add_untracked() { printf 'scratch\n' >"$1/lib/scratch.go"; }
no_manifest() { rm "$1/lib/MANIFEST"; git -C "$1" add -A >/dev/null 2>&1; }
empty_manifest() { : >"$1/lib/MANIFEST"; }
no_source() { rm "$1/lib/SOURCE"; git -C "$1" add -A >/dev/null 2>&1; ( cd "$1" && DEST_DIR=lib bash "$CHECK" --write >/dev/null 2>&1 ); git -C "$1" add -A >/dev/null 2>&1; }
short_source() { printf '%s\n' "0123456" >"$1/lib/SOURCE"; git -C "$1" add -A >/dev/null 2>&1; ( cd "$1" && DEST_DIR=lib bash "$CHECK" --write >/dev/null 2>&1 ); git -C "$1" add -A >/dev/null 2>&1; }
untrack_all() { git -C "$1" rm -r --cached lib >/dev/null 2>&1; }

echo "== the two verdicts that must both be reachable =="
one "a clean copy passes"                       0 "integrity OK"                        noop
one "a changed byte fails"                      1 "differs from its manifest"           edit_byte

echo "== every kind of difference the manifest must see =="
one "the exec bit being cleared fails"          1 "differs from its manifest"           flip_exec
one "a deleted file fails"                      1 "differs from its manifest"           delete_file
one "an added tracked file fails"               1 "differs from its manifest"           add_tracked
one "an untracked file under the copy fails"    1 "Untracked files"                     add_untracked

echo "== the check refusing rather than passing =="
one "no manifest refuses"                       2 "No manifest"                         no_manifest
one "an empty manifest refuses"                 2 "Empty manifest"                      empty_manifest
one "no SOURCE pin refuses"                     2 "No SOURCE pin"                       no_source
one "nothing tracked under the copy refuses"    2 "Nothing tracked"                     untrack_all
one "a short SOURCE sha fails"                  1 "not a full commit SHA"               short_source

# DEST_DIR pointing at nothing is its own case: no fixture can create it.
d=$(new_repo)
out=$( cd "$d" && DEST_DIR=absent bash "$CHECK" 2>&1 ); rc=$?
check "a missing copy directory refuses" 2 "No library copy" "$out" "$rc"

echo "== --write is what makes a legitimate change checkable =="
d=$(new_repo)
edit_byte "$d"
out=$(run_in "$d"); rc=$?
check "the edit is caught before --write" 1 "differs from its manifest" "$out" "$rc"
( cd "$d" && DEST_DIR=lib bash "$CHECK" --write >/dev/null 2>&1 )
git -C "$d" add -A >/dev/null 2>&1
out=$(run_in "$d"); rc=$?
check "and accepted after it" 0 "integrity OK" "$out" "$rc"

echo "== provenance: the half that needs the library checkout =="
d=$(new_repo)
out=$(run_in "$d"); rc=$?
check "with no source repository, provenance is declared UNCHECKED, not OK" \
    0 "provenance NOT CHECKED" "$out" "$rc"

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
