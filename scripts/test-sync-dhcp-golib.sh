#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for scripts/sync-dhcp-golib.sh, and above all for ITS SWEEP.
#
# The sweep is the only thing standing between a private repository and
# a public branch, and until now it was the one script in this directory
# with no test beside it — while its own header called DEST_DIR "the seam
# the self-test drives". This is that self-test.
#
# The deny list used here is INVENTED ("zzsecretzz", "zzhostzz"). The
# real list is deliberately outside the tracked tree, and a test that
# quoted it would publish exactly what the script exists to keep out.
# What is tested is the MECHANISM: content, filenames, dotfiles, case,
# and every refusal that must fire before anything is copied.
#
# The other half of the discipline is that a refusal must not destroy
# what is already there. Every refusing case below asserts the existing
# copy survived, because a sweep that emptied the destination and then
# refused would take the branch down with it.

set -uo pipefail

SYNC="$(cd "$(dirname "$0")" && pwd)/sync-dhcp-golib.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

pass=0; fail=0
SHA=""

DENY="$ROOT/deny.txt"
printf '# invented, not the real list\nzzsecretzz\nzzhostzz\n\n' >"$DENY"

# new_src builds a source repository whose tree is customised by the
# named setup function, commits it, and echoes "<path> <sha>".
new_src() {
    local setup="$1" d sha
    d=$(mktemp -d "$ROOT/srcXXXXXX")
    mkdir -p "$d/pkg"
    printf 'package lib\n' >"$d/pkg/lib.go"
    printf 'module example.com/lib\n\ngo 1.22\n' >"$d/go.mod"
    "$setup" "$d"
    git -C "$d" init -q
    git -C "$d" config user.email t@t; git -C "$d" config user.name t
    # commit.gpgsign false explicitly: an inherited `true` makes the
    # commit below BLOCK on a signing prompt rather than fail, and this
    # suite would hang on the developer's machine instead of failing on
    # it. check-selftest-fixtures.sh enforces this.
    git -C "$d" config commit.gpgsign false
    git -C "$d" add -A >/dev/null 2>&1
    git -C "$d" commit -qm one >/dev/null 2>&1
    sha=$(git -C "$d" rev-parse HEAD)
    printf '%s %s' "$d" "$sha"
}

# new_dest builds a destination workspace that ALREADY holds a copy, so
# that every refusal can be checked for having left it alone.
new_dest() {
    local d
    d=$(mktemp -d "$ROOT/dstXXXXXX")
    mkdir -p "$d/internal/dhcp-golib"
    printf 'the copy that must survive a refusal\n' >"$d/internal/dhcp-golib/PRIOR"
    printf '%s' "$d"
}

check() {
    local name="$1" want_rc="$2" want_text="$3" out="$4" rc="$5"
    if [ "$rc" != "$want_rc" ]; then
        echo "FAIL: $name — exit $rc, want $want_rc"; echo "$out" | sed 's/^/    /'
        fail=$((fail + 1)); return
    fi
    if [ -n "$want_text" ] && ! printf '%s' "$out" | LC_ALL=C grep -qF -- "$want_text"; then
        echo "FAIL: $name — exit $rc as wanted, but the output does not mention: $want_text"
        echo "$out" | sed 's/^/    /'
        fail=$((fail + 1)); return
    fi
    echo "ok: $name"; pass=$((pass + 1))
}

# refuses runs the sync and asserts both the verdict and that the prior
# copy is still there.
refuses() {
    local name="$1" want_rc="$2" want_text="$3" setup="$4" dst src pair out rc
    pair=$(new_src "$setup"); src=${pair% *}; SHA=${pair#* }
    dst=$(new_dest)
    out=$( cd "$dst" && DHCP_GOLIB_DENY="$DENY" bash "$SYNC" "$SHA" "$src" 2>&1 ); rc=$?
    check "$name" "$want_rc" "$want_text" "$out" "$rc"
    if [ ! -f "$dst/internal/dhcp-golib/PRIOR" ]; then
        echo "FAIL: $name — the refusal destroyed the existing copy"
        fail=$((fail + 1))
    fi
}

clean()      { :; }
in_content() { printf 'const k = "zzsecretzz"\n' >"$1/pkg/leak.go"; }
in_name()    { printf 'package lib\n' >"$1/pkg/zzhostzz_notes.go"; }
in_dotfile() { printf 'zzsecretzz\n' >"$1/.hidden"; }
upper_case() { printf 'const k = "ZZSecretZZ"\n' >"$1/pkg/leak.go"; }
with_req()   { printf 'module example.com/lib\n\ngo 1.22\n\nrequire other v1.0.0\n' >"$1/go.mod"; }

echo "== the sweep =="
refuses "a denied name in file content is refused"     1 "Internal names found" in_content
refuses "a denied name in a FILENAME is refused"       1 "Internal names found" in_name
refuses "a denied name in a DOTFILE is refused"        1 "Internal names found" in_dotfile
refuses "the match is case-insensitive"                1 "Internal names found" upper_case

echo "== the refusals that must fire before any copying =="
refuses "a library with module dependencies is refused" 1 "module dependencies" with_req

pair=$(new_src clean); src=${pair% *}; SHA=${pair#* }
dst=$(new_dest)
out=$( cd "$dst" && DHCP_GOLIB_DENY="$ROOT/absent.txt" bash "$SYNC" "$SHA" "$src" 2>&1 ); rc=$?
check "a missing deny list refuses rather than sweeping for nothing" 2 "Deny list missing" "$out" "$rc"
[ -f "$dst/internal/dhcp-golib/PRIOR" ] || { echo "FAIL: the refusal destroyed the existing copy"; fail=$((fail+1)); }

printf '# only comments\n\n' >"$ROOT/empty-deny.txt"
dst=$(new_dest)
out=$( cd "$dst" && DHCP_GOLIB_DENY="$ROOT/empty-deny.txt" bash "$SYNC" "$SHA" "$src" 2>&1 ); rc=$?
check "a comments-only deny list refuses" 2 "Deny list empty" "$out" "$rc"

dst=$(new_dest)
out=$( cd "$dst" && DHCP_GOLIB_DENY="$DENY" bash "$SYNC" "${SHA:0:12}" "$src" 2>&1 ); rc=$?
check "a short SHA is not a pin" 2 "Usage" "$out" "$rc"

dst=$(new_dest)
out=$( cd "$dst" && DHCP_GOLIB_DENY="$DENY" bash "$SYNC" "$SHA" 2>&1 ); rc=$?
check "no source repository refuses" 2 "No source repository" "$out" "$rc"

dst=$(new_dest)
out=$( cd "$dst" && DHCP_GOLIB_DENY="$DENY" bash "$SYNC" "$SHA" "$ROOT" 2>&1 ); rc=$?
check "a source that is not a git repository refuses" 2 "not a git repository" "$out" "$rc"

dst=$(new_dest)
out=$( cd "$dst" && DHCP_GOLIB_DENY="$DENY" bash "$SYNC" "ffffffffffffffffffffffffffffffffffffffff" "$src" 2>&1 ); rc=$?
check "a commit the source does not hold refuses" 2 "Commit not found" "$out" "$rc"

echo "== and the case that must SUCCEED, or the refusals above prove nothing =="
dst=$(new_dest)
out=$( cd "$dst" && DHCP_GOLIB_DENY="$DENY" bash "$SYNC" "$SHA" "$src" 2>&1 ); rc=$?
check "a clean library tree is copied" 0 "Synced" "$out" "$rc"
if [ ! -f "$dst/internal/dhcp-golib/pkg/lib.go" ]; then
    echo "FAIL: the copy did not land"; fail=$((fail + 1))
else
    echo "ok: the copied tree is in place"; pass=$((pass + 1))
fi
if [ "$(cat "$dst/internal/dhcp-golib/SOURCE" 2>/dev/null)" != "$SHA" ]; then
    echo "FAIL: SOURCE does not record the synced SHA"; fail=$((fail + 1))
else
    echo "ok: SOURCE records the synced SHA"; pass=$((pass + 1))
fi
if [ -f "$dst/internal/dhcp-golib/PRIOR" ]; then
    echo "FAIL: the previous copy survived a SUCCESSFUL sync; the destination is not replaced"
    fail=$((fail + 1))
else
    echo "ok: a successful sync replaces the destination rather than merging into it"
    pass=$((pass + 1))
fi

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
