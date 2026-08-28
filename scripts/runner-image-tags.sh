#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Decide which tags a Runner image build publishes (#812).
#
# WHY THIS IS A SCRIPT AND NOT AN EXPRESSION. `:latest` on
# ghcr.io/claymore666/dhcp-ci-runner is what the runner orchestrator
# launches. Whatever it points at is the image every self-hosted job in
# this repo runs on. Before #812 the `manifest` job moved it
# unconditionally, and `workflow_dispatch` carries no branch filter, so a
# dispatch from any branch repointed the whole pool at an unmerged
# Dockerfile — silently, with nothing in the run naming the ref.
#
# The obvious fix is an `if:` on the job and a gate that greps the YAML
# for it. That gate is worth very little: it asserts the shape of a
# string, goes green the moment someone rewrites the expression into an
# equivalent-looking one, and never once executes the decision it claims
# to protect. So the decision lives here, `manifest` calls this, and
# scripts/test-runner-image-tags.sh drives THIS FILE over every case.
# The workflow and the test exercise the same code.
#
# WHY THE HATCH STAYS OPEN. Closing dispatch-promotion outright would be
# wrong. A Go toolchain bump cannot go green until a runner image
# carrying the new Go exists, and the image source lives in the same PR
# that bumps go.mod — #805 sat on exactly that dependency on 2026-08-27.
# So promotion from a branch stays possible; it just has to be asked for,
# where "asked for" is a recorded workflow input rather than a side
# effect of dispatching at all.
#
# ORDERING. `latest` is emitted LAST. Same reason release.yml moves the
# product tag last (#736, scripts/check-latest-promotion.sh): a floating
# tag must not move before the thing it will point at is fully published.
#
# Usage: bash scripts/runner-image-tags.sh
# Env:   EVENT_NAME      github.event_name
#        REF             github.ref
#        PROMOTE_LATEST  the workflow_dispatch input (may be unset)
#        SHA7            short SHA, the immutable tag
# Out:   one tag per line, on stdout
# Exit:  0 normally, 1 if SHA7 is missing — a build with no immutable
#        tag is not a build worth publishing.

set -euo pipefail

EVENT_NAME="${EVENT_NAME:-}"
REF="${REF:-}"
PROMOTE_LATEST="${PROMOTE_LATEST:-}"
SHA7="${SHA7:-}"

if [ -z "$SHA7" ]; then
    echo "::error title=No immutable tag::SHA7 is empty; refusing to publish." >&2
    exit 1
fi

# The two arms are deliberately separate rather than one boolean. A push
# can only reach this workflow from dev (the `on:` filter), but that
# filter is in a different file and someone widening it must not silently
# widen promotion too. Assert the branch here as well.
promote=no
case "$EVENT_NAME" in
    push)
        [ "$REF" = "refs/heads/dev" ] && promote=yes
        ;;
    workflow_dispatch)
        # Exact equality. GitHub renders an unchecked boolean input as
        # "false" and an absent one as empty; anything that is not
        # literally "true" is not a request to move the pool's tag.
        [ "$PROMOTE_LATEST" = "true" ] && promote=yes
        ;;
esac

echo "$SHA7"
if [ "$promote" = yes ]; then
    echo "latest"
else
    echo "Not moving :latest — event=$EVENT_NAME ref=$REF promote_latest=${PROMOTE_LATEST:-<unset>}" >&2
fi
