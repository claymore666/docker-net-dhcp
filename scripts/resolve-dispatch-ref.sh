#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Turn a dispatch input into a FULLY QUALIFIED ref, or refuse (#738).
#
# WHY THIS EXISTS SEPARATELY FROM check-dispatch-ref.sh. That script
# answers "is this commit ours?" and needs the repository to answer it —
# a full clone, every branch, every tag. It therefore has to run AFTER a
# checkout, which is fine when it runs as its own gate job and the
# consumer sits behind it.
#
# pages.yml cannot use that shape. Its dispatch input feeds the checkout
# of the job that publishes the site, and the job holds contents: write.
# By the time a validating step could run, `pip install` from the
# checked-out tree and `mkdocs` (which executes hooks from the
# checked-out mkdocs.yml) have already happened. So the value has to be
# constrained BEFORE anything is fetched, which means constraining it by
# shape alone.
#
# WHAT IT GUARANTEES. The output is `refs/heads/dev` or
# `refs/tags/vX.Y.Z[-rcN]`, and nothing else — ever. An arbitrary SHA,
# a `refs/pull/<N>/head`, a fork branch, a `--upload-pack=...` argument
# smuggled in as a ref: none of them can be produced by this script, so
# handing its output to `actions/checkout` makes them unreachable by
# construction rather than rejected by a matching rule. That is the
# difference between a denylist, which the next unexpected shape walks
# around, and an allowlist of two forms.
#
# It deliberately does NOT check that the ref exists. Existence is
# check-dispatch-ref.sh's question and needs a clone; this one runs
# before there is one. A non-existent tag fails at checkout, loudly, on
# the workflow's own terms — after this script has already ruled out
# everything that could check out something dangerous.
#
# Usage: bash scripts/resolve-dispatch-ref.sh <name>
# Output: the qualified ref on stdout
# Exit:  0 resolved
#        1 refused (unknown shape, or no argument)

set -uo pipefail

NAME="${1-}"

usage() {
    echo "usage: bash scripts/resolve-dispatch-ref.sh <name>" >&2
    echo "  <name> is 'dev' or a release tag vX.Y.Z / vX.Y.Z-rcN" >&2
}

if [ "$#" -ne 1 ]; then
    echo "::error title=resolve-dispatch-ref::exactly one argument is required," \
         "got $#." >&2
    usage
    exit 1
fi

# An empty argument is a refusal, not a default. The caller decides what
# happens when its input is blank — silently resolving to `dev` here
# would mean a workflow that meant to publish a tag publishes the
# development branch instead, and nothing would say so.
if [ -z "$NAME" ]; then
    echo "::error title=resolve-dispatch-ref::empty ref name. The caller must" \
         "decide its own default rather than have one invented here." >&2
    exit 1
fi

case "$NAME" in
    dev)
        echo "refs/heads/dev"
        exit 0
        ;;
esac

if [[ "$NAME" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$ ]]; then
    echo "refs/tags/${NAME}"
    exit 0
fi

echo "::error title=Dispatch ref refused::'${NAME}' is neither 'dev' nor a" \
     "release tag (vX.Y.Z or vX.Y.Z-rcN)." >&2
echo >&2
echo "Only those two shapes may be checked out by a job that publishes. A raw" >&2
echo "SHA, a pull-request ref, or a fork branch would put outside code into a" >&2
echo "context that has this repository's write token — so they are not" >&2
echo "rejected by a rule here, they are simply not expressible: this script" >&2
echo "can only ever emit refs/heads/dev or refs/tags/vX.Y.Z." >&2
exit 1
