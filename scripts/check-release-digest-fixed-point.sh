#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The release lane must have a reachable passing state (#910, review r2).
#
# WHAT WENT WRONG. Through 1.x this repository carried a per-release
# block of binary digests in docs/verifying-releases.md, and the release
# workflow compared it against the binaries it had just built. Its
# documented recovery, runbook step 10b, was: take the corrected block
# from the failed run, land it on the TAGGED commit, re-tag.
#
# From 2.0 the tag and the commit are compiled into the binary (O-4:
# -ldflags -X on pkg/buildinfo). So landing the block changes the
# commit, which changes the binary, which changes the digest the block
# states. THE RECOVERY CANNOT CONVERGE: no commit can record the digest
# of a binary built from itself. The gate therefore had no passing state
# to reach, and because it ran between the GHCR push and the Hub push,
# every attempt left a half-published release behind it.
#
# WHAT THIS CHECKS. Two claims, both of which have to hold for the lane
# to have a reachable passing state, and one measurement that decides
# which of them applies:
#
#   A (MEASURED, never skipped) — is the commit still part of the
#     binary's identity? Two builds whose ONLY difference is the commit,
#     with the link flags derived from the Makefile the release uses,
#     plus a repeat of the first as a determinism control. If the
#     control fails, this host cannot measure it and the check refuses
#     rather than guessing.
#
#   B — while A holds, no file in the tree may record a digest of a
#     binary this tree builds. That is the self-reference above, and it
#     is what would go red the moment someone pastes the block back.
#
#   C — the release workflow's signed checksum manifest must cover the
#     binary. This is where the digests went instead: produced by the
#     build that makes them true, signed with the tarball. Without it,
#     removing the block would mean no published digest at all, which is
#     a worse tree than the one this replaced.
#
# The four cells of A x B are all reachable and all meaningful: A true
# and B violated is the defect; A true and B clean is the shipped state;
# A false and B populated is a tree that could carry digests again; A
# false and B clean is fine. C is independent and is required in every
# cell, so the B-clean arm is not a universal satisfied by an empty
# domain -- there is always something left to fail.
#
# WHAT IT CANNOT SEE.
#   - It measures the HOST `go build`, not the container build. The
#     Dockerfile threads the same two build args into the same -ldflags,
#     but a divergence between the two build shapes is invisible here.
#     `Reproducible build` covers the container half.
#   - B is keyed on the SHAPE `<64 hex><spaces><path ending in a binary
#     name>` -- what `sha256sum` writes. A digest recorded some other
#     way (a prose sentence, a table cell, base64, a truncated hash) is
#     invisible to it.
#   - C reads the workflow's text. It asserts the manifest names the
#     binary; it cannot assert the run produced a correct digest, and no
#     tag will be cut to find out.
#   - It says nothing about whether a recorded digest is CORRECT. Under
#     A that question has no answer to give; without A it is a different
#     check.
#
# Usage: check-release-digest-fixed-point.sh [TREE] [WORKFLOW]
# Exit:  0 pass, 1 a claim is violated, 2 cannot run.
set -uo pipefail

TREE="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
WF="${2:-$TREE/.github/workflows/release.yml}"

die() { echo "check-release-digest-fixed-point: $*" >&2; exit 2; }
note() { echo "FAIL  $*" >&2; failed=1; }
failed=0

[ -d "$TREE" ] || die "$TREE is not a directory"
[ -f "$TREE/Makefile" ] || die "$TREE/Makefile does not exist — the link flags are derived from it"
[ -f "$WF" ] || die "$WF does not exist"
command -v go >/dev/null 2>&1 || die "go is not on PATH; half A is a measurement, not an assumption"
command -v make >/dev/null 2>&1 || die "make is not on PATH; the link flags are read from the Makefile"

# --- the binaries this tree builds --------------------------------------
# Derived, not listed. `cmd/dhcp-handler` was deleted for 2.0 and the
# release workflow still named it for two rounds; a transcribed list
# here would have inherited exactly that.
mapfile -t BINARIES < <(cd "$TREE/cmd" 2>/dev/null && for d in */; do [ -d "$d" ] && printf '%s\n' "${d%/}"; done)
[ "${#BINARIES[@]}" -gt 0 ] || die "$TREE/cmd holds no command directories — nothing to reason about"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# --- A. is the commit part of the binary's identity? --------------------
# The link flags come from the Makefile's own GO_LDFLAGS, evaluated with
# the COMMIT we want -- the release builds through that variable, so
# asking make is asking the subject. Building it ourselves into a temp
# directory keeps bin/ untouched: a developer's next `make debug` must
# not run a binary this check stamped.
ldflags_for() {
    make -C "$TREE" -s \
        --eval='fp-print-ldflags: ; @printf "%s\n" "$(GO_LDFLAGS)"' \
        fp-print-ldflags COMMIT="$1" VERSION=fixed-point-probe 2>/dev/null
}

build_hash() {
    local out="$2"
    ( cd "$TREE" && go build -ldflags "$1" -o "$out" "./cmd/$3" ) >/dev/null 2>&1 || return 1
    sha256sum "$out" | cut -d' ' -f1
}

A_COMMIT=1111111111111111111111111111111111111111
B_COMMIT=2222222222222222222222222222222222222222

la="$(ldflags_for "$A_COMMIT")"
lb="$(ldflags_for "$B_COMMIT")"
# GO_LDFLAGS not varying with COMMIT is not a refusal: it is the OTHER
# world, the one where a recorded digest could be a fixed point again.
# Refusing here would make the whole A-false half of this check
# unreachable, and B would then be a rule nothing could ever satisfy in
# the direction that matters.
commit_dependent=no
if [ "$la" = "$lb" ]; then
    echo "note: the Makefile's link flags do not vary with COMMIT, so the binary's" >&2
    echo "  identity does not depend on the commit and a recorded digest CAN be a fixed" >&2
    echo "  point. If that is deliberate, docs/verifying-releases.md and runbook step" >&2
    echo "  10b explain the absence of a digest list by the opposite fact and are now" >&2
    echo "  stale." >&2
    BINARIES_TO_BUILD=()
else
    BINARIES_TO_BUILD=("${BINARIES[@]}")
fi

for b in ${BINARIES_TO_BUILD[@]+"${BINARIES_TO_BUILD[@]}"}; do
    h1="$(build_hash "$la" "$TMP/$b.a1" "$b")" || die "could not build ./cmd/$b with the Makefile's link flags"
    h2="$(build_hash "$la" "$TMP/$b.a2" "$b")" || die "could not rebuild ./cmd/$b"
    if [ "$h1" != "$h2" ]; then
        echo "check-release-digest-fixed-point: two builds of ./cmd/$b from identical inputs differ." >&2
        echo "  This host cannot measure whether the COMMIT matters, because nothing here is" >&2
        echo "  stable enough to compare. Refusing rather than reporting the difference as a" >&2
        echo "  commit dependence it has not shown." >&2
        exit 2
    fi
    h3="$(build_hash "$lb" "$TMP/$b.b1" "$b")" || die "could not build ./cmd/$b with the second commit"
    [ "$h1" != "$h3" ] && commit_dependent=yes
done

# --- B. the tree must not record a digest of a binary it builds ---------
# `sha256sum`'s own output shape, with an optional path in front of the
# name, over the tracked files.
if git -C "$TREE" rev-parse --git-dir >/dev/null 2>&1; then
    mapfile -t FILES < <(git -C "$TREE" ls-files)
else
    mapfile -t FILES < <(cd "$TREE" && find . -type f -not -path './.git/*' | sed 's|^\./||')
fi
[ "${#FILES[@]}" -gt 0 ] || die "no files to scan under $TREE"

names="$(printf '%s|' "${BINARIES[@]}")"; names="${names%|}"
DIGEST_RE="^[0-9a-f]{64}[[:space:]]+[^[:space:]]*(${names})[[:space:]]*$"

records=""
for f in "${FILES[@]}"; do
    [ -f "$TREE/$f" ] || continue
    hits="$(grep -nE "$DIGEST_RE" "$TREE/$f" 2>/dev/null)" || continue
    while IFS= read -r hit; do
        [ -n "$hit" ] && records="${records}${f}:${hit}"$'\n'
    done <<< "$hits"
done

n_records=$(printf '%s' "$records" | grep -c . || true)

if [ "$n_records" -gt 0 ] && [ "$commit_dependent" = yes ]; then
    note "$n_records file line(s) record the digest of a binary this tree builds, and the commit is part of that binary."
    printf '%s' "$records" | sed 's/^/        /' >&2
    {
        echo "  A commit that states the digest of a binary built from itself changes that"
        echo "  binary by being made, so no such record can ever be correct on the commit"
        echo "  that carries it, and any release gate comparing the two has no passing"
        echo "  state to reach. Measured just now: two builds differing only in COMMIT"
        echo "  hash differently."
        echo "  The digests belong in the release build's signed checksums manifest, which"
        echo "  is produced by the build that makes them true. See"
        echo "  docs/verifying-releases.md and runbook step 10b."
    } >&2
fi

# --- C. the signed manifest covers the binary ---------------------------
# Derived from the workflow: every checksums*.txt it writes, and whether
# the sha256sum invocations feeding it name the binary.
mapfile -t MANIFESTS < <(grep -oE 'checksums[A-Za-z0-9_.-]*\.txt' "$WF" | grep -v '\.sigstore' | sort -u)
if [ "${#MANIFESTS[@]}" -eq 0 ]; then
    die "$WF writes no checksums manifest — this check's third claim has lost its subject, which is a bigger change than it can judge"
fi

for m in "${MANIFESTS[@]}"; do
    feeding="$(grep -E "sha256sum .*>>? *\"?${m}\"?" "$WF" || true)"
    if [ -z "$feeding" ]; then
        note "$WF names $m but no sha256sum invocation writes it."
        continue
    fi
    covered=no
    for b in "${BINARIES[@]}"; do
        printf '%s\n' "$feeding" | grep -E "[^[:space:]\"]*/${b}([[:space:]\"]|$)" >/dev/null && covered=yes
    done
    if [ "$covered" = no ]; then
        note "$m covers no binary: nothing feeding it names $(printf '%s ' "${BINARIES[@]}")under a path."
        {
            echo "  This manifest is the only signed statement of what the published binary"
            echo "  hashes to, now that no commit can carry one. Record the binary under the"
            echo "  path it has inside the release tarball, so an operator who extracts it"
            echo "  can run 'sha256sum --ignore-missing -c $m'."
        } >&2
    fi
done

[ "$failed" -eq 0 ] || exit 1

echo "PASS  release digests are a fixed point: commit-in-binary=${commit_dependent}, ${n_records} in-tree digest record(s) of ${#BINARIES[@]} built binar(ies), ${#MANIFESTS[@]} signed manifest(s) covering them"
