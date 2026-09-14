#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Publish an already-signed manifest under a second Docker Hub name, and
# refuse anything that is not that exact manifest (#972).
#
# WHY THIS IS A SCRIPT AND NOT TWO `run:` BLOCKS
#
# It was two `run:` blocks, one per architecture, holding the same three
# decisions twice. Two things were wrong with that.
#
# ONE FIX DOES NOT REACH THE COPIES. The amd64 block and the arm64 block
# were the same logic transcribed, and the release path is the one place
# in this repository where a half-applied correction cannot be rolled
# back: by the time the alias step runs, GHCR and Docker Hub already
# hold `:vX.Y.Z` and the signature is made.
#
# THE REFUSAL BRANCH HAD NEVER EXECUTED. The digest-equality assertion
# is the whole point of the step, and inline in a workflow it can only
# be reached by a real registry serving a different manifest under the
# alias. So the branch that is supposed to stop a rebuild from shipping
# was, itself, never run — anywhere, ever. Here the three commands it
# drives are resolved from `PATH`, so the self-test stubs the transport
# and drives both outcomes on every lane run. That is the same bargain
# `scripts/check-release-registries.sh` made for the same reason (#638).
#
# WHAT IT DOES
#
#   1. copies SOURCE to ALIAS with `oras cp -r`. `-r` carries the OCI
#      REFERRERS, which is where a cosign signature lives; `cosign copy`
#      is deprecated and drops them, and `crane copy` never had them.
#   2. re-derives the alias digest FROM THE REGISTRY, through the alias
#      name, rather than trusting the copy's own report.
#   3. refuses unless that digest equals the digest that was signed. A
#      second `docker plugin create` re-tars the rootfs and yields a
#      different digest (#267), so "the alias is a rebuild" and "the
#      alias is the signed manifest" are distinguishable, and this is
#      where they are distinguished.
#   4. verifies the signature THROUGH THE ALIAS NAME. Equal digests say
#      the bytes match; this says a user who pulls the alias can verify
#      what they pulled, which is a different claim and the one the
#      referrer copy can lose on its own.
#
# Each of (2), (3) and (4) fails separately on purpose: a copy that
# never happened, a copy that landed on a rebuilt manifest and a copy
# that lost the referrer are three different faults.
#
# Usage:
#   scripts/publish-hub-alias.sh --expect-digest sha256:… SOURCE ALIAS
#
# ALIAS IS LAST, and that is not cosmetic. scripts/check-publish-verify-parity.sh
# derives the publish set from the DESTINATION reference at the end of a
# copy line, so the argument order is what makes the alias appear in
# that gate's published set. Reorder these and the alias silently stops
# being a published cell there.
#
# Env:
#   COSIGN_IDENTITY_REGEXP  override for the certificate identity the
#                           signature must carry. Defaults to this
#                           repository's release workflow; the self-test
#                           is the only other caller.
#   COSIGN_OIDC_ISSUER      override for the OIDC issuer the certificate
#                           must come from. Defaults to GitHub Actions.
#                           Both are verification parameters, so both
#                           are named here: an override that is read and
#                           not documented is a way to weaken the check
#                           that no reader of this header would see.
#
# Exit: 0 the alias is the signed manifest and verifies under its own name
#       1 the copy failed, the digests differ, or the signature does not
#         verify through the alias
#       2 cannot judge — an argument missing, or the registry returned
#         no digest for the alias
set -uo pipefail

IDENTITY_REGEXP="${COSIGN_IDENTITY_REGEXP:-^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@}"
OIDC_ISSUER="${COSIGN_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"

refuse() { echo "::error::publish-hub-alias: $*" >&2; exit 2; }
fail()   { echo "::error::$*" >&2; exit 1; }

EXPECT=""
while [ $# -gt 0 ]; do
    case "$1" in
        --expect-digest) EXPECT="${2-}"; shift 2 || refuse "--expect-digest needs a value" ;;
        --) shift; break ;;
        -*) refuse "unknown option '$1'" ;;
        *)  break ;;
    esac
done

SOURCE="${1-}"
ALIAS="${2-}"

[ -n "$EXPECT" ] || refuse "--expect-digest is unset. Without the digest that was signed there is nothing to compare the alias against, and copying without that comparison is the failure this exists to prevent."
[ -n "$SOURCE" ] || refuse "no SOURCE reference given."
[ -n "$ALIAS" ]  || refuse "no ALIAS reference given."
[ $# -le 2 ] || refuse "unexpected extra argument '${3}'; usage is --expect-digest DIGEST SOURCE ALIAS."

# `oras cp` and `docker buildx imagetools inspect` want the reference in
# different spellings: oras always needs the registry host, and
# `imagetools` takes the Docker Hub short form. The host prefix is
# stripped for the inspect and kept for the copy, in one place, so the
# two cannot drift.
inspect_ref() { printf '%s\n' "${1#docker.io/}"; }

oras cp -r "$SOURCE" "$ALIAS" || fail "oras cp -r ${SOURCE} -> ${ALIAS} failed. Nothing was published under the alias name; the release still holds the source name only."

ALIAS_DIGEST=$(docker buildx imagetools inspect "$(inspect_ref "$ALIAS")" --format '{{.Manifest.Digest}}')
[ -n "$ALIAS_DIGEST" ] || refuse "the registry returned no digest for ${ALIAS} after the copy reported success. This cannot say whether the alias is the signed manifest, and passing without that answer is what the check is for."

if [ "$ALIAS_DIGEST" != "$EXPECT" ]; then
    fail "${ALIAS} is ${ALIAS_DIGEST}, but ${SOURCE} was signed as ${EXPECT}. The alias must be the same manifest, so this is a rebuild and not a copy."
fi

# The repository part, without the tag. `%:*` strips the SHORTEST
# `:...` suffix, which is the tag; `%%:*` would strip from a registry
# port instead and hand cosign a hostname.
ALIAS_REPO="$(inspect_ref "$ALIAS")"
ALIAS_REPO="${ALIAS_REPO%:*}"

cosign verify \
    --certificate-identity-regexp "$IDENTITY_REGEXP" \
    --certificate-oidc-issuer "$OIDC_ISSUER" \
    "${ALIAS_REPO}@${ALIAS_DIGEST}" > /dev/null \
    || fail "${ALIAS} is the signed digest, but the signature does not verify through the alias name. The manifest copied and its referrers did not, so a user pulling the alias cannot verify what they pulled."

echo "Alias ${ALIAS} is ${ALIAS_DIGEST}, the digest signed for ${SOURCE}, and it verifies under its own name."
