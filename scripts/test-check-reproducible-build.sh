#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-reproducible-build.sh (#456).
#
# The comparison is small, which is exactly why it needs pinning: the
# way a gate like this fails is by silently comparing nothing. An empty
# export directory, a renamed output path, or a build that produced no
# binary at all make a naive comparison trivially "equal". Those cases
# are the bulk of what is asserted below; the happy path is one line.
#
# THE FIXTURE CARRIES A TOOLCHAIN BINARY ON PURPOSE (2.0). It used to
# carry two of ours, net-dhcp and dhcp-handler, and the "different sets"
# case was built by deleting the second. cmd/dhcp-handler is deleted in
# 2.0 -- the library reports state changes on a channel, so there
# is no hook process -- and a one-file fixture would have made that
# case unbuildable and the happy path a comparison of one. So mk now
# plants the shape the real export actually has: one binary of ours
# under bin/, and the pinned Go toolchain's own bin/ beside it. That is
# also the shape the sweep's vacuity guard exists for.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-reproducible-build.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

# check NAME WANT_EXIT GREP DIR_A DIR_B
check() {
    local name="$1" want="$2" want_grep="$3" a="$4" b="$5"
    local out rc ok=1
    out=$(bash "$CHECK" "$a" "$b" 2>&1)
    rc=$?
    [ "$rc" -eq "$want" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -qF "$want_grep" <<<"$out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want / grep '$want_grep', got exit $rc)"
        sed 's/^/    /' <<<"$out"
        failures=$((failures + 1))
    fi
}

mk() {
    local dir="$TMP/$1" content="$2"
    mkdir -p "$dir/usr/local/src/docker-net-dhcp/bin" "$dir/usr/local/go/bin"
    printf '%s' "$content" > "$dir/usr/local/src/docker-net-dhcp/bin/net-dhcp"
    printf 'go' > "$dir/usr/local/go/bin/go"
    echo "$dir"
}

A=$(mk a 'identical bytes')
B=$(mk b 'identical bytes')
check "two identical builds pass" 0 "2 binaries identical" "$A" "$B"

C=$(mk c 'one byte differs.')
check "a differing binary fails" 1 "DIFFERS" "$A" "$C"
check "and names the file that differs" 1 "bin/net-dhcp" "$A" "$C"

# The failures that would otherwise read as success.
mkdir -p "$TMP/empty-a" "$TMP/empty-b"
check "two empty directories cannot be compared" 2 "Nothing to compare" \
    "$TMP/empty-a" "$TMP/empty-b"
check "one empty side cannot be compared" 2 "Nothing to compare" \
    "$A" "$TMP/empty-b"

# A binary present in one build and missing from the other is a
# difference in the build, not a smaller comparison. Driven on the
# toolchain file rather than ours: dropping net-dhcp would be caught
# one step earlier by the vacuity guard, and would therefore not
# exercise the set comparison at all.
D=$(mk d 'identical bytes')
rm "$D/usr/local/go/bin/go"
check "a missing binary on one side fails" 2 "different sets of binaries" "$A" "$D"

# Files outside bin/ are build detritus, not outputs; a difference there
# must not redden the gate.
E=$(mk e 'identical bytes')
mkdir -p "$E/usr/local/src/docker-net-dhcp/obj"
printf 'noise' > "$E/usr/local/src/docker-net-dhcp/obj/scratch"
check "files outside bin/ are ignored" 0 "2 binaries identical" "$A" "$E"

# The export is the whole builder filesystem, so the sweep also finds
# the Go toolchain's bin/. A non-empty comparison is therefore NOT
# evidence that our binaries were in it — the real CI run compares 12
# files, only two of which are ours. If the build output moved, ten
# identical toolchain files would otherwise carry the gate to green.
F="$TMP/toolchain-only-a"
G="$TMP/toolchain-only-b"
for d in "$F" "$G"; do
    mkdir -p "$d/usr/local/go/bin"
    printf 'go' > "$d/usr/local/go/bin/go"
    printf 'gofmt' > "$d/usr/local/go/bin/gofmt"
done
# The real CI run compares 12 files, only ONE of which is ours since
# cmd/dhcp-handler went; eleven identical toolchain files would carry
# the gate to green on their own.
check "our binaries missing entirely is a refusal, not a pass" 2 \
    "'net-dhcp' is not among the binaries" "$F" "$G"

H=$(mk h 'identical bytes')
mv "$H/usr/local/src/docker-net-dhcp/bin/net-dhcp" \
   "$H/usr/local/src/docker-net-dhcp/bin/net-dhcp-renamed"
I=$(mk i 'identical bytes')
mv "$I/usr/local/src/docker-net-dhcp/bin/net-dhcp" \
   "$I/usr/local/src/docker-net-dhcp/bin/net-dhcp-renamed"
check "a renamed output binary is a refusal on both sides" 2 \
    "is not among the binaries" "$H" "$I"

check "a missing directory is a usage error" 2 "not a directory" "$A" "$TMP/nope"
check "no arguments is a usage error" 2 "usage" "" ""

if [ "$failures" -ne 0 ]; then
    echo "$failures test(s) failed" >&2
    exit 1
fi
echo "All check-reproducible-build.sh tests passed."
