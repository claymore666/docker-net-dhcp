#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-release-digest-fixed-point.sh (#910, review r2).
#
# The gate reasons about a release lane that nothing in this repository
# can execute: no tag will be cut to test it. So every cell is driven
# against a GENERATED TREE that has the shape the gate reads -- a
# Makefile whose GO_LDFLAGS may or may not carry the commit, a real Go
# command it builds, a doc that may or may not record a digest, and a
# workflow whose checksum manifest may or may not cover the binary.
#
# HALF A IS REALLY BUILT, not stubbed. The `stamped` fixture links the
# commit into the binary through the Makefile the same way this
# repository does; the `unstamped` one does not. The gate runs `go
# build` against both and reaches its two answers by measurement. A
# fixture that only claimed one or the other would leave the gate's
# central measurement unexercised, which is the shape a check of a
# check fails in.
#
# The four cells of (commit in the binary) x (a digest recorded in the
# tree) are all four driven, and the two directions of the manifest
# claim with them. Without the passing cells this would be a check that
# only knows how to refuse.
set -uo pipefail

CHECK="$(cd "$(dirname "$0")" && pwd)/check-release-digest-fixed-point.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok() { echo "PASS  $*"; pass=$((pass + 1)); }
no() { echo "FAIL  $*"; fail=$((fail + 1)); }

command -v go >/dev/null 2>&1 || { echo "go is not on PATH; this self-test builds"; exit 2; }

# mktree DIR STAMPED DIGESTLINE MANIFESTCOVERS
#   STAMPED       yes -> GO_LDFLAGS carries $(COMMIT); no -> it does not
#   DIGESTLINE    yes -> the doc records a sha256sum line for the binary
#   MANIFESTCOVERS yes -> the workflow's manifest names the binary
mktree() {
    local d="$1" stamped="$2" digest="$3" covers="$4"
    rm -rf "$d"; mkdir -p "$d/cmd/toy" "$d/docs" "$d/.github/workflows"

    cat > "$d/go.mod" <<'EOF'
module example.test/fixedpoint

go 1.24
EOF
    cat > "$d/cmd/toy/main.go" <<'EOF'
package main

var Commit = "unknown"

func main() { println(Commit) }
EOF

    if [ "$stamped" = yes ]; then
        cat > "$d/Makefile" <<'EOF'
COMMIT ?= unknown
GO_LDFLAGS = -X main.Commit=$(COMMIT)
EOF
    else
        # The flags are non-empty -- an empty GO_LDFLAGS would be
        # refused for a different reason and would not drive this cell.
        cat > "$d/Makefile" <<'EOF'
COMMIT ?= unknown
GO_LDFLAGS = -s
EOF
    fi

    {
        echo "# Verifying"
        echo
        if [ "$digest" = yes ]; then
            echo '```'
            echo "0000000000000000000000000000000000000000000000000000000000000000  toy"
            echo '```'
        else
            echo "Nothing here records a digest."
        fi
    } > "$d/docs/verifying.md"

    {
        echo "name: Release"
        echo "jobs:"
        echo "  release:"
        echo "    steps:"
        echo "      - run: |"
        echo '          sha256sum "$ART" > checksums.txt'
        if [ "$covers" = yes ]; then
            echo '          ( cd plugin && sha256sum rootfs/usr/sbin/toy ) >> checksums.txt'
        fi
    } > "$d/.github/workflows/release.yml"
}

# case NAME DIR WANT [NEEDLE]
case_() {
    local name="$1" d="$2" want="$3" needle="${4:-}" out rc
    out=$(bash "$CHECK" "$d" "$d/.github/workflows/release.yml" 2>&1); rc=$?
    if [ "$rc" -ne "$want" ]; then
        no "$name — want exit $want, got $rc"
        printf '%s\n' "$out" | sed 's/^/      /'
        return
    fi
    if [ -n "$needle" ] && ! printf '%s\n' "$out" | grep -F -- "$needle" >/dev/null; then
        no "$name — exit $rc as expected, but the output never says '$needle'"
        printf '%s\n' "$out" | sed 's/^/      /'
        return
    fi
    ok "$name"
}

# --- the four cells of A x B -------------------------------------------

# THE DEFECT. This is the tree the release lane shipped in: the commit
# is in the binary and a commit records that binary's digest.
mktree "$TMP/a" yes yes yes
case_ "a recorded digest under a commit-stamped binary is refused" "$TMP/a" 1 \
    "record the digest of a binary this tree builds"

# The fix, and the positive control for the case above: the same tree
# with the record removed passes. Without this the case above is
# satisfied by a gate that refuses every tree.
mktree "$TMP/b" yes no yes
case_ "the same tree with no recorded digest passes" "$TMP/b" 0 \
    "commit-in-binary=yes"

# THE OTHER WORLD. The commit is not in the binary, so a recorded digest
# CAN be a fixed point and is not a defect. A gate that keyed on the
# record alone -- the obvious spelling -- would refuse here, and would
# be refusing a correct tree.
mktree "$TMP/c" no yes yes
case_ "a recorded digest without a commit-stamped binary is allowed" "$TMP/c" 0 \
    "commit-in-binary=no"

# ... and it says so rather than passing silently, because the tree's
# prose explains the absence of a digest list by the opposite fact.
mktree "$TMP/d" no no yes
case_ "an unstamped binary and no record passes, loudly" "$TMP/d" 0 \
    "do not vary with COMMIT"

# --- the manifest claim, both directions -------------------------------
mktree "$TMP/e" yes no no
case_ "a manifest that covers no binary is refused" "$TMP/e" 1 \
    "covers no binary"

# The preservation control for it: the ONLY difference from the case
# above is the line that records the binary.
mktree "$TMP/f" yes no yes
case_ "the same manifest covering the binary passes" "$TMP/f" 0 "signed manifest(s)"

# It is required in the passing arm of B too -- otherwise "no record in
# the tree" would be a universal satisfied by a tree that publishes no
# digest anywhere, which is worse than the one this replaced.
mktree "$TMP/g" no yes no
case_ "an unstamped tree still has to publish the digest somewhere" "$TMP/g" 1 \
    "covers no binary"

# --- the record's shape ------------------------------------------------
# A digest of something this tree does NOT build is not a self-
# reference. cmd/dhcp-handler was deleted for 2.0 and release.yml went
# on naming it for two rounds; a gate keyed on a transcribed name list
# would still be refusing its digest today.
mktree "$TMP/h" yes no yes
printf '0000000000000000000000000000000000000000000000000000000000000000  dhcp-handler\n' \
    >> "$TMP/h/docs/verifying.md"
case_ "a digest of a binary this tree no longer builds is not a self-reference" "$TMP/h" 0

# A path in front of the name is still the same record: the release
# manifest writes rootfs/usr/sbin/<name>, and a doc block may too.
mktree "$TMP/i" yes no yes
printf '0000000000000000000000000000000000000000000000000000000000000000  rootfs/usr/sbin/toy\n' \
    >> "$TMP/i/docs/verifying.md"
case_ "a recorded digest with a path in front of the name is caught" "$TMP/i" 1 \
    "rootfs/usr/sbin/toy"

# --- refusals: the gate must not report a verdict it cannot reach ------
mktree "$TMP/j" yes no yes
rm "$TMP/j/Makefile"
out=$(bash "$CHECK" "$TMP/j" "$TMP/j/.github/workflows/release.yml" 2>&1); rc=$?
[ "$rc" -eq 2 ] && ok "a tree with no Makefile is a refusal" \
                || no "a tree with no Makefile returned $rc (: $out)"

mktree "$TMP/k" yes no yes
rm -rf "$TMP/k/cmd"
out=$(bash "$CHECK" "$TMP/k" "$TMP/k/.github/workflows/release.yml" 2>&1); rc=$?
[ "$rc" -eq 2 ] && ok "a tree that builds no commands is a refusal" \
                || no "a tree with no cmd/ returned $rc (: $out)"

# The workflow writing no manifest at all: the third claim has lost its
# subject, and reporting a clean pass over that is how a check goes
# quiet instead of red.
mktree "$TMP/l" yes no yes
printf 'name: Release\njobs:\n  release:\n    steps:\n      - run: echo nothing\n' \
    > "$TMP/l/.github/workflows/release.yml"
out=$(bash "$CHECK" "$TMP/l" "$TMP/l/.github/workflows/release.yml" 2>&1); rc=$?
[ "$rc" -eq 2 ] && ok "a workflow writing no checksum manifest is a refusal" \
                || no "a workflow with no manifest returned $rc (: $out)"

# --- the real tree -----------------------------------------------------
# The gate has to pass over what actually ships, or every case above is
# a claim about fixtures.
out=$(bash "$CHECK" 2>&1); rc=$?
[ "$rc" -eq 0 ] && ok "the repository as it stands passes" \
                || no "the real tree returned $rc (: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
