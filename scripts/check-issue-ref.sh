#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Will this pull request be reachable from an issue after it merges (#718)?
#
# WHY THIS EXISTS
#
# `scripts/sync-issue-state-labels.sh` puts `in-dev` on an issue whose work
# has landed on `dev`. That label is not decoration: an issue here stays open
# until the release PR closes it on `main`, so `in-dev` is the only thing
# distinguishing "merged, awaiting release" from "nobody has started". The
# release PR's `Closes` list is built from milestone membership, and `in-dev`
# is what the maintainer reads to decide the milestone is telling the truth.
#
# The reconciler can only derive that from what a PR leaves behind. When a PR
# leaves nothing — no reference in any commit subject, none in the title, none
# in the body — the issue reads as untouched forever, and NOTHING GOES RED.
# An absent label is indistinguishable from a legitimate one, which is the
# same failure shape the reconciler itself was written to fix.
#
# WHAT IT IS NOT
#
# This gate does not reproduce #718. That defect was the reconciler
# discarding merge-commit subjects, and the check that goes red for it lives
# in `test-sync-issue-state-labels.sh` — including a negative control that
# pins the broken state. This gate closes the hole that remains once the
# reconciler is fixed: a PR that names an issue nowhere at all.
#
# WHAT IT CANNOT DO
#
# A parsed reference is NECESSARY, NOT SUFFICIENT. Whether the number is an
# open issue in this repository is settled later by the reconciler, against
# live data this gate deliberately does not fetch — a PR check that called the
# API would fail on a blip and would fail a contributor for somebody else's
# issue being closed. So this catches "you referenced nothing", not "you
# referenced the wrong thing".
#
# It also cannot see the squash subject GitHub synthesises at merge time,
# because that does not exist yet. That is fine: the synthesised subject only
# ever carries the PR's own number, which is a route to the title and body
# this gate has already read.
#
# THE PARSER IS NOT COPIED. Both halves come from
# `sync-issue-state-labels.sh --parse` and `--parse-body`. A gate carrying its
# own regex would be free to disagree with the script that actually assigns
# the labels, and would then pass a PR the reconciler cannot read — which is
# precisely the class of bug this is here to stop.
#
# Usage: check-issue-ref.sh <commit-range> [pr-title-file] [pr-body-file]
#   <commit-range>: any git range, e.g. origin/dev..HEAD
#
# Waiver: a body line `No issue: <reason>` passes the check. Some pull
# requests genuinely have no issue — a typo fix, a revert — and the honest
# way to say so is to say so, in the artifact a reviewer reads, rather than
# to leave the reader unable to tell that case from an oversight.
#
# Exit: 0 reachable, 1 nothing references an issue, 2 cannot check.

set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
SYNC="$HERE/sync-issue-state-labels.sh"

RANGE="${1:-}"
TITLE_FILE="${2:-}"
BODY_FILE="${3:-}"

if [ -z "$RANGE" ]; then
    echo "usage: $0 <commit-range> [pr-title-file] [pr-body-file]" >&2
    exit 2
fi

if [ ! -x "$SYNC" ] && [ ! -f "$SYNC" ]; then
    echo "FAIL  cannot find sync-issue-state-labels.sh next to this script" >&2
    exit 2
fi

if ! git rev-parse --verify --quiet "${RANGE%%..*}" >/dev/null 2>&1; then
    echo "FAIL  cannot resolve commit range '$RANGE'" >&2
    exit 2
fi

SUBJECTS="$(git log "$RANGE" --format='%s' 2>/dev/null)" || {
    echo "FAIL  cannot resolve commit range '$RANGE'" >&2
    exit 2
}

if [ -z "$SUBJECTS" ]; then
    # An empty range is not a clean pass: it means the caller handed us the
    # wrong base, and reporting "reachable" would be a green check over
    # nothing at all.
    echo "FAIL  no commits in range '$RANGE' — cannot check" >&2
    exit 2
fi

found=""
note=""

refs_from_subjects="$(printf '%s\n' "$SUBJECTS" | bash "$SYNC" --parse)"
if [ -n "$refs_from_subjects" ]; then
    found="$refs_from_subjects"
    note="a commit subject"
fi

if [ -z "$found" ] && [ -n "$TITLE_FILE" ] && [ -f "$TITLE_FILE" ]; then
    refs_from_title="$(bash "$SYNC" --parse < "$TITLE_FILE")"
    if [ -n "$refs_from_title" ]; then
        found="$refs_from_title"
        note="the PR title"
    fi
fi

if [ -z "$found" ] && [ -n "$BODY_FILE" ] && [ -f "$BODY_FILE" ]; then
    refs_from_body="$(bash "$SYNC" --parse-body < "$BODY_FILE")"
    if [ -n "$refs_from_body" ]; then
        found="$refs_from_body"
        note="the PR body"
    fi
fi

if [ -n "$found" ]; then
    echo "Issue reference OK — $(printf '%s' "$found" | tr '\n' ' ' | sed 's/ *$//') found in $note."
    exit 0
fi

# The waiver, checked only once the check has otherwise failed, so a PR that
# carries both a reference and a waiver line is reported on its reference.
if [ -n "$BODY_FILE" ] && [ -f "$BODY_FILE" ] &&
   command grep -qiE '^[[:space:]]*No issue:[[:space:]]*\S' "$BODY_FILE"; then
    reason="$(command grep -iE '^[[:space:]]*No issue:[[:space:]]*\S' "$BODY_FILE" | head -1 | sed 's/^[[:space:]]*//')"
    echo "Issue reference waived — $reason"
    exit 0
fi

cat >&2 <<'MSG'
FAIL  nothing in this pull request references an issue.

  After it merges, scripts/sync-issue-state-labels.sh has no way to reach an
  issue from it, so no `in-dev` label appears — and an issue with the work
  merged then looks exactly like one nobody has started. The release PR's
  `Closes` list is built from the milestone, and `in-dev` is what says the
  milestone is telling the truth.

  Any ONE of these fixes it:

    - a commit subject ending in a ref group:  fix(plugin): a thing (#123)
    - the same group at the end of the PR title
    - a closing keyword in the PR body:        Closes #123

  If this pull request genuinely has no issue, say so in the body:

    No issue: <reason>

MSG
exit 1
