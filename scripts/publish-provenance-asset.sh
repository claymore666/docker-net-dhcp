#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Attach the build provenance to the release page, and verify the file
# that is being attached (#1011).
#
# WHY THE ATTESTATION IS NOT ENOUGH ON ITS OWN
#
# `actions/attest-build-provenance` stores the attestation in GitHub's
# attestation store, keyed by the artifact digest. That is discoverable
# only by someone who already holds the artifact and knows to ask. A
# mirror, an offline copy, and OpenSSF Scorecard's Signed-Releases check
# all read the release PAGE, where until now there were ten assets and
# no provenance. Scorecard counts provenance by asset NAME and matches
# exactly one suffix, `.intoto.jsonl`
# (ossf/scorecard v5.5.0, probes/releasesHaveProvenance/impl.go:43,70),
# which is why the name is checked here and not left to a caller.
#
# Nothing is re-signed. The bundle the attest step wrote is a Sigstore
# bundle carrying its own certificate chain and transparency-log entry,
# already signed by this run's keyless identity, so publishing it needs
# no second signing identity and no new permission.
#
# WHY THIS IS A SCRIPT AND NOT TWO `run:` BLOCKS
#
# The same reasons as scripts/publish-hub-alias.sh, and the first one
# applies harder here: amd64 and arm64 would hold the same four refusals
# twice, and a half-applied correction on the release path cannot be
# rolled back. The second reason is the refusals themselves. A bundle
# that attests nothing, or that does not name the artifact, cannot be
# produced by a real release on demand, so inline in the workflow those
# branches would never execute anywhere. Here `gh` is resolved from
# PATH, so the self-test stubs the transport and drives every branch on
# every lane run.
#
# WHAT IT DOES
#
#   1. refuses an ASSET name Scorecard would not count. Publishing a
#      bundle under a name the check ignores looks exactly like success.
#   2. copies BUNDLE to ASSET.
#   3. reads the subject list out of the file it is about to publish,
#      not out of the caller's glob. A `subject-path` that stops
#      matching leaves a bundle attesting the SBOMs alone, and a verify
#      loop transcribed from the same glob passes over it happily.
#   4. refuses unless ARTIFACT is one of those subjects. This is the
#      direction (3) cannot fail in: a loop over the bundle's own
#      subjects is satisfied by whatever the bundle happens to contain.
#   5. verifies every subject against the PUBLISHED file with
#      `gh attestation verify --bundle`, which is the command
#      docs/verifying-releases.md gives users. Publishing an attestation
#      nobody checked is the shape this repository keeps finding.
#
# (1), (3), (4) and (5) fail separately on purpose: a name nothing
# counts, an unreadable bundle, a bundle about the wrong artifact and a
# bundle that does not verify are four different faults.
#
# Usage:
#   scripts/publish-provenance-asset.sh BUNDLE ASSET ARTIFACT
#
# Env:
#   REPO   the repository the certificate must name, passed to
#          `gh attestation verify --repo`. Defaults to
#          GITHUB_REPOSITORY. It is a verification parameter, so it is
#          named here: an override that is read and not documented is a
#          way to weaken the check that no reader of this header would
#          see.
#
# Exit: 0 the asset is published and every subject verifies against it
#       1 the bundle does not name ARTIFACT, or does not verify
#       2 cannot judge — an argument missing, no bundle to copy, or a
#         bundle nothing can read
set -uo pipefail

# ossf/scorecard v5.5.0 probes/releasesHaveProvenance/impl.go:43.
SCORECARD_PROVENANCE_SUFFIX='.intoto.jsonl'

refuse() { echo "::error::publish-provenance-asset: $*" >&2; exit 2; }
fail()   { echo "::error::$*" >&2; exit 1; }

BUNDLE="${1-}"
ASSET="${2-}"
ARTIFACT="${3-}"

[ -n "$BUNDLE" ]   || refuse "no BUNDLE given. Without the attest step's bundle-path output there is nothing to publish, and an empty path would copy nothing and report success."
[ -n "$ASSET" ]    || refuse "no ASSET name given."
[ -n "$ARTIFACT" ] || refuse "no ARTIFACT given. The subject assertion is the only check here that a loop over the bundle cannot make, so running without it is running without the check."
[ $# -le 3 ] || refuse "unexpected extra argument '${4}'; usage is BUNDLE ASSET ARTIFACT."

REPO="${REPO:-${GITHUB_REPOSITORY:-}}"
[ -n "$REPO" ] || refuse "neither REPO nor GITHUB_REPOSITORY is set; \`gh attestation verify\` would then accept a certificate naming any repository."

case "$ASSET" in
    *"$SCORECARD_PROVENANCE_SUFFIX") ;;
    *) refuse "asset name '${ASSET}' does not end in '${SCORECARD_PROVENANCE_SUFFIX}'. Scorecard's Signed-Releases check counts provenance by asset name and matches that suffix and no other, so the file would be published and counted by nothing." ;;
esac

[ -f "$BUNDLE" ] || refuse "no file at '${BUNDLE}'."
cp "$BUNDLE" "$ASSET" || refuse "could not copy '${BUNDLE}' to '${ASSET}'."

# jq in a command substitution, not a process substitution: the exit
# status of a process substitution reaches nothing, so an unreadable
# bundle would arrive below as an empty subject list and be reported as
# the wrong fault.
if ! NAMES=$(jq -r '.dsseEnvelope.payload | @base64d | fromjson | .subject[].name' "$ASSET"); then
    refuse "'${ASSET}' is not a readable attestation bundle."
fi
mapfile -t SUBJECTS < <(printf '%s' "$NAMES")
[ "${#SUBJECTS[@]}" -gt 0 ] || fail "${ASSET} attests nothing: its statement carries no subject, so it would be published as provenance for no file at all."

if ! printf '%s\n' "${SUBJECTS[@]}" | grep -Fx -- "$ARTIFACT" >/dev/null; then
    fail "${ARTIFACT} is not a subject of ${ASSET}. The bundle attests: ${SUBJECTS[*]}"
fi

for f in "${SUBJECTS[@]}"; do
    gh attestation verify "$f" --bundle "$ASSET" --repo "$REPO" \
        || fail "${f} does not verify against the published ${ASSET}."
done

echo "${ASSET}: ${#SUBJECTS[@]} subjects verified against the published bundle"
