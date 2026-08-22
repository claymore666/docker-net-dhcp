#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# A release from the canonical repository must reach BOTH registries.
#
# WHY THIS EXISTS
#
# The Docker Hub push is conditional on DOCKERHUB_USERNAME and
# DOCKERHUB_TOKEN being set, and the absence of those secrets is handled
# by a `::warning::` and a skip. That leniency is deliberate and stays:
# a fork with no Hub account has to be able to cut a release, and
# failing it there would be hostile for no benefit.
#
# The defect is that the canonical repository inherits the fork's
# leniency. If its Hub secrets ever lapse -- rotated, expired, revoked,
# renamed org -- the release run stays GREEN, publishes to GHCR alone,
# and the only trace is one `::warning::` in a log nobody opens on a
# green run. A GHCR-only release and a both-registries release are then
# indistinguishable from the outside, and the users who install from
# Hub get the previous version until somebody notices.
#
# So the same absence means different things in the two places, and this
# says which is which: on the canonical repository it is a failure, and
# everywhere else it is the documented skip.
#
# WHAT IT DOES NOT DO
#
# It does not check that the push SUCCEEDED -- that is the push step's
# own failure -- and it does not check that credentials are VALID, only
# that they are present. An expired token that is still set reads as
# present here and fails at the push, which is the right place for it.
#
# Inputs, all via the environment so the workflow passes them explicitly
# rather than this reaching for the GitHub context:
#
#   REPO            github.repository, e.g. claymore666/docker-net-dhcp
#   HAS_HUB_CREDS   the literal string "true" or "false"
#   CANONICAL_REPO  optional override, for the self-test
#
# Exit: 0 both registries, or a fork skipping Hub as documented
#       1 the canonical repository is about to publish to GHCR alone
#       2 cannot judge (an input missing, or not the shape expected)
set -uo pipefail

CANONICAL_REPO="${CANONICAL_REPO:-claymore666/docker-net-dhcp}"

refuse() { echo "check-release-registries: $*" >&2; exit 2; }

REPO="${REPO:-}"
HAS_HUB_CREDS="${HAS_HUB_CREDS:-}"

[ -n "$REPO" ] || refuse "REPO is unset; without it this cannot tell the canonical repository from a fork, and guessing either way is worse than refusing"

# HAS_HUB_CREDS is produced by a GitHub expression that resolves to the
# literal "true" or "false". Anything else means the expression changed
# shape -- and an unrecognised value must not be read as "false", which
# would fail every fork, nor as "true", which would pass the case this
# exists to catch.
case "$HAS_HUB_CREDS" in
    true|false) ;;
    "") refuse "HAS_HUB_CREDS is unset; the credentials-presence expression did not reach this step" ;;
    *)  refuse "HAS_HUB_CREDS is '$HAS_HUB_CREDS', expected the literal 'true' or 'false' -- the expression that produces it has changed shape and this check can no longer read it" ;;
esac

if [ "$HAS_HUB_CREDS" = "true" ]; then
    echo "Docker Hub credentials present; the release publishes to both registries."
    exit 0
fi

if [ "$REPO" = "$CANONICAL_REPO" ]; then
    echo "::error::Docker Hub credentials are missing on ${REPO}. A release from this repository must reach BOTH registries: publishing to GHCR alone leaves every user who installs from Docker Hub on the previous version, and a green run is indistinguishable from a complete one. Set DOCKERHUB_USERNAME and DOCKERHUB_TOKEN, or -- if the Hub repository is being retired deliberately -- remove the Hub push and this check together, in one change that says so."
    exit 1
fi

echo "::warning::DOCKERHUB_USERNAME / DOCKERHUB_TOKEN not set on ${REPO} -- skipping the Docker Hub push. GHCR publishing proceeds normally. This is the documented behaviour for a fork; on ${CANONICAL_REPO} it is a hard failure."
exit 0
