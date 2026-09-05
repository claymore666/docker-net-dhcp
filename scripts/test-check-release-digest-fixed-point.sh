#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for check-release-digest-fixed-point.sh (#910, reviews r2/r3).
#
# The gate reasons about a release lane that nothing in this repository
# can execute: no tag will be cut to test it. So every cell is driven
# against a GENERATED TREE that has the shape the gate reads -- a
# Makefile whose GO_LDFLAGS may or may not carry the commit, a
# Dockerfile that may or may not spell the same flags, a real Go command
# it builds, a doc that may or may not record a digest and may or may
# not describe the manifest, and a workflow whose checksum manifest may
# or may not cover the binary at the path the tarball extracts to.
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
# tree) are all four driven, and the two directions of the manifest,
# link-flag and document claims with them. Without the passing cells
# this would be a check that only knows how to refuse.
#
# Two of the failing cells are the REVIEWER'S OWN EDITS rather than
# invented ones: the `cd plugin &&` dropped from the manifest line
# (review r2, finding 2) and GO_LDFLAGS losing Commit= while the
# Dockerfile keeps it (finding 3). Both were measured to leave the gate
# green before this round.
set -uo pipefail

CHECK="$(cd "$(dirname "$0")" && pwd)/check-release-digest-fixed-point.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok() { echo "PASS  $*"; pass=$((pass + 1)); }
no() { echo "FAIL  $*"; fail=$((fail + 1)); }

command -v go >/dev/null 2>&1 || { echo "go is not on PATH; this self-test builds"; exit 2; }

# mktree DIR STAMPED DIGESTLINE MANIFESTCOVERS
#   STAMPED        yes -> GO_LDFLAGS *and* the Dockerfile carry $(COMMIT)
#   DIGESTLINE     yes -> the doc records a sha256sum line for the binary
#   MANIFESTCOVERS yes -> the workflow's manifest names the binary
#
# Env seams, each defaulting to the shape that passes, so a case moves
# exactly one thing:
#   WF_BINLINE   the manifest line that records the binary
#   WF_TAR       the packaging line
#   DF_XFLAGS    the Dockerfile's -ldflags contents
#   DOC_COUNT    the entry count the doc states (a word; "" omits it)
#   DOC_TAR      yes -> the doc's check block unpacks the tarball first
#   DOC_WARN_N   the number in the worked example's WARNING line
#   DOC_MISSING  the names the worked example shows as unreadable
mktree() {
    local d="$1" stamped="$2" digest="$3" covers="$4" df
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
        df='-X main.Commit=${COMMIT}'
        printf 'FROM scratch\nRUN go build -ldflags "%s" -o bin/ ./cmd/...\n' \
            "${DF_XFLAGS:-$df}" > "$d/Dockerfile"
    else
        # The flags are non-empty -- an empty GO_LDFLAGS would be
        # refused for a different reason and would not drive this cell.
        cat > "$d/Makefile" <<'EOF'
COMMIT ?= unknown
GO_LDFLAGS = -s
EOF
        df='-s'
        printf 'FROM scratch\nRUN go build -ldflags "%s" -o bin/ ./cmd/...\n' \
            "${DF_XFLAGS:-$df}" > "$d/Dockerfile"
    fi

    {
        echo "# Verifying"
        echo
        if [ -n "${DOC_COUNT-three}" ]; then
            echo "\`checksums.txt\` covers **${DOC_COUNT-three}** files."
            echo
        fi
        echo '```sh'
        [ "${DOC_TAR:-yes}" = yes ] && echo "tar -xzf art.tar.gz"
        echo "sha256sum --ignore-missing -c checksums.txt"
        echo '```'
        echo
        echo '```'
        echo "art.tar.gz: OK"
        for n in ${DOC_MISSING-sbom.json rootfs/usr/sbin/toy}; do
            echo "sha256sum: $n: No such file or directory"
            echo "$n: FAILED open or read"
        done
        echo "sha256sum: WARNING: ${DOC_WARN_N:-2} listed files could not be read"
        echo '```'
        echo
        if [ "$digest" = yes ]; then
            echo '```'
            echo "0000000000000000000000000000000000000000000000000000000000000000  toy"
            echo '```'
        else
            echo "Nothing here records a digest."
        fi
    } > "$d/docs/verifying-releases.md"

    {
        echo "name: Release"
        echo "jobs:"
        echo "  release:"
        echo "    steps:"
        echo "      - run: |"
        echo "          ${WF_TAR:-tar -czf \"\$ART\" -C plugin .}"
        echo '          sha256sum "$ART" sbom.json > checksums.txt'
        if [ "$covers" = yes ]; then
            echo "          ${WF_BINLINE:-( cd plugin && sha256sum rootfs/usr/sbin/toy ) >> checksums.txt}"
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

# --- the manifest entry's PATH (review r2, finding 2) -------------------
# THE REVIEWER'S EDIT, verbatim in shape: drop the `cd` and name the
# binary from the workflow's own working directory. The digest is
# correct and the entry is worthless -- no operator can produce that
# path from a published asset, and --ignore-missing skips it forever.
WF_BINLINE='sha256sum plugin/rootfs/usr/sbin/toy >> checksums.txt' \
    mktree "$TMP/m" yes no yes
case_ "a manifest entry under a path the tarball never extracts to is refused" "$TMP/m" 1 \
    "after extraction that file is at 'rootfs/usr/sbin/toy'"

# The preservation control: the same line WITH the cd, which is the only
# difference, passes -- so the case above is not satisfied by a gate that
# refuses every path.
WF_BINLINE='( cd plugin && sha256sum rootfs/usr/sbin/toy ) >> checksums.txt' \
    mktree "$TMP/n" yes no yes
case_ "the same entry written relative to the tarball root passes" "$TMP/n" 0 "extracted paths"

# A binary that is not inside the packaged directory at all.
WF_BINLINE='( cd build && sha256sum rootfs/usr/sbin/toy ) >> checksums.txt' \
    mktree "$TMP/o" yes no yes
case_ "a manifest entry from outside the packaged directory is refused" "$TMP/o" 1 \
    "does not contain"

# The packaging is what the extracted path is derived FROM, so a
# workflow that packages nothing cannot be judged, and two different
# packaging roots make the association a guess.
WF_TAR='echo no packaging here' mktree "$TMP/p" yes no yes
case_ "a workflow that packages no tarball is a refusal" "$TMP/p" 2 "packages no tarball"

WF_TAR='tar -czf "$ART" -C plugin . && tar -czf "$ART2" -C other .' \
    mktree "$TMP/q" yes no yes
case_ "two packaging roots is a refusal, not a guess" "$TMP/q" 2 "ambiguous"

# --- the link flags across both builds (review r2, finding 3) -----------
# THE REVIEWER'S SCENARIO. Half A reads the Makefile; the release binary
# comes from the Dockerfile. With GO_LDFLAGS losing Commit= and the
# Dockerfile keeping it, the gate used to flip to commit-in-binary=no
# and exit 0, and the next commit re-adding a digest block passed.
DF_XFLAGS='-X main.Commit=${COMMIT}' mktree "$TMP/r" no no yes
case_ "the Makefile dropping a key the Dockerfile keeps is refused" "$TMP/r" 1 \
    "do not link the same -X keys"

# The other direction: the Dockerfile drops it and the Makefile keeps
# it, so half A measures a dependence the released binary does not have.
DF_XFLAGS='-s' mktree "$TMP/s" yes no yes
case_ "the Dockerfile dropping a key the Makefile keeps is refused" "$TMP/s" 1 \
    "do not link the same -X keys"

# Same key set, different binding: both spell Commit, and the Dockerfile
# stamps the version into it. A set comparison alone would pass this.
DF_XFLAGS='-X main.Commit=${VERSION}' mktree "$TMP/t" yes no yes
case_ "the same key bound to a different build argument is refused" "$TMP/t" 1 \
    "is bound to \$COMMIT by the Makefile"

# The preservation control for all three: agreeing spellings pass, in
# BOTH worlds -- a stamped pair and an unstamped pair.
DF_XFLAGS='-X main.Commit=${COMMIT}' mktree "$TMP/u" yes no yes
case_ "agreeing link flags pass" "$TMP/u" 0 "link flags reconciled"
DF_XFLAGS='-s' mktree "$TMP/v" no no yes
case_ "agreeing absence of link flags passes" "$TMP/v" 0 "commit-in-binary=no"

mktree "$TMP/w" yes no yes
rm "$TMP/w/Dockerfile"
case_ "a tree with no Dockerfile is a refusal" "$TMP/w" 2 "claim D cannot reconcile"

# --- the document (review r2, finding 1) --------------------------------
# THE DEFECT. The workflow writes three entries and the document says
# two: its exhaustive path exits 1 on a good release and its lenient
# path skips an entry in silence.
DOC_COUNT=two mktree "$TMP/x" yes no yes
case_ "a document stating the wrong entry count is refused" "$TMP/x" 1 \
    "covers two files; release.yml writes 3 entries"

DOC_COUNT='' mktree "$TMP/y" yes no yes
case_ "a document that never states the entry count is refused" "$TMP/y" 1 \
    "never says how many files"

# The precondition. An entry that exists only after extraction has to be
# extracted in the same block that checks it, or the reader follows the
# page into a failure on a good release.
DOC_TAR=no mktree "$TMP/z" yes no yes
case_ "a check block that never unpacks the tarball is refused" "$TMP/z" 1 \
    "never unpacks the tarball first"

# The worked example has to tally with the manifest it is an example of,
# in both of its numbers.
DOC_WARN_N=1 mktree "$TMP/aa" yes no yes
case_ "a worked example whose count disagrees with its own lines is refused" "$TMP/aa" 1 \
    "says 1 listed files could not be read and shows 2"

DOC_MISSING='sbom.json other.json' mktree "$TMP/ab" yes no yes
case_ "a worked example that omits an entry the manifest carries is refused" "$TMP/ab" 1 \
    "does not tally with any manifest"

# The preservation control for the three above: the same document with
# the count, the extraction and the example all in step passes.
mktree "$TMP/ac" yes no yes
case_ "a document in step with the workflow passes" "$TMP/ac" 0 "in step with"

# The document's own claims are what claim E judges, so a document that
# checks nothing, or shows no partial check, has emptied E's domain and
# is a refusal rather than a clean pass.
mktree "$TMP/ad" yes no yes
sed -i '/sha256sum --ignore-missing -c checksums.txt/d' "$TMP/ad/docs/verifying-releases.md"
case_ "a document that never checks a manifest is a refusal" "$TMP/ad" 2 \
    "no block that checks a manifest"

mktree "$TMP/ae" yes no yes
sed -i '/listed files could not be read/d' "$TMP/ae/docs/verifying-releases.md"
case_ "a document with no worked example of a partial check is a refusal" "$TMP/ae" 2 \
    "no worked example"

mktree "$TMP/af" yes no yes
rm "$TMP/af/docs/verifying-releases.md"
case_ "a tree with no verification document is a refusal" "$TMP/af" 2 \
    "claim E has lost its subject"

# --- the record's shape ------------------------------------------------
# A digest of something this tree does NOT build is not a self-
# reference. cmd/dhcp-handler was deleted for 2.0 and release.yml went
# on naming it for two rounds; a gate keyed on a transcribed name list
# would still be refusing its digest today.
mktree "$TMP/h" yes no yes
printf '0000000000000000000000000000000000000000000000000000000000000000  dhcp-handler\n' \
    >> "$TMP/h/docs/verifying-releases.md"
case_ "a digest of a binary this tree no longer builds is not a self-reference" "$TMP/h" 0

# A path in front of the name is still the same record: the release
# manifest writes rootfs/usr/sbin/<name>, and a doc block may too.
mktree "$TMP/i" yes no yes
printf '0000000000000000000000000000000000000000000000000000000000000000  rootfs/usr/sbin/toy\n' \
    >> "$TMP/i/docs/verifying-releases.md"
case_ "a recorded digest with a path in front of the name is caught" "$TMP/i" 1 \
    "rootfs/usr/sbin/toy"

# --- refusals: the gate must not report a verdict it cannot reach ------
mktree "$TMP/j" yes no yes
rm "$TMP/j/Makefile"
case_ "a tree with no Makefile is a refusal" "$TMP/j" 2 "does not exist"

mktree "$TMP/k" yes no yes
rm -rf "$TMP/k/cmd"
case_ "a tree that builds no commands is a refusal" "$TMP/k" 2 "no command directories"

# The workflow writing no manifest at all: the third claim has lost its
# subject, and reporting a clean pass over that is how a check goes
# quiet instead of red.
mktree "$TMP/l" yes no yes
printf 'name: Release\njobs:\n  release:\n    steps:\n      - run: echo nothing\n' \
    > "$TMP/l/.github/workflows/release.yml"
case_ "a workflow writing no checksum manifest is a refusal" "$TMP/l" 2 \
    "writes no checksums manifest"

# --- the real tree -----------------------------------------------------
# The gate has to pass over what actually ships, or every case above is
# a claim about fixtures.
out=$(bash "$CHECK" 2>&1); rc=$?
[ "$rc" -eq 0 ] && ok "the repository as it stands passes" \
                || no "the real tree returned $rc (: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
