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
# Usage: check-issue-ref.sh <commit-range> [pr-title-file] [pr-body-file] [pr-author]
#   <commit-range>: any git range, e.g. origin/dev..HEAD
#   <pr-author>:    github.event.pull_request.user.login, for the bot exemption
#
# Waiver: a body line `No issue: <reason>` passes the check. Some pull
# requests genuinely have no issue — a typo fix, a revert — and the honest
# way to say so is to say so, in the artifact a reviewer reads, rather than
# to leave the reader unable to tell that case from an oversight.
#
# THE WAIVER IS ANCHORED AT COLUMN 0, and its own FAIL text below shows the
# line INDENTED. Both halves are deliberate, and together they are the fix
# for this gate waiving itself.
#
# Unanchored, the waiver matched leading whitespace. The gate's failure
# message teaches the waiver string indented by four spaces, and the most
# natural reaction to a failing required check is to paste its output into
# the PR body to talk about it. That paste matched — `<reason>` is enough to
# satisfy the trailing `\S` — so a REQUIRED check reported green and printed
# its own help text back as the waiver reason. The single most obvious
# response to the failure switched the failure off.
#
# `check-coverage-floor.sh` and `check-test-weakening.sh` already anchor at
# column 0 for exactly this reason: #735 caught one of them waiving itself
# and both were fixed. This one was not, so the fix did not reach the third
# copy. Keeping the FAIL text indented as well means neither mechanism is
# load-bearing alone.
#
# THE SAME MESSAGE MUST NOT CARRY A WORKED REFERENCE EITHER. Anchoring the
# waiver is only half of it: the FAIL text also demonstrated the carriers,
# and `Closes #123` in a pasted failure satisfied the BODY parser, so the
# gate passed on the reference path instead of the waiver path — same
# action, same silent green, different route. The parser is not the thing
# to fix there: GitHub honours closing keywords anywhere in a body, indent
# and all, and `sync-issue-state-labels.sh` mirrors GitHub on purpose. So
# the message uses `#<n>` placeholders, exactly as `check-coverage-floor.sh`
# writes `Coverage-floor: #<issue>`.
#
# THE WAIVER IS UNREACHABLE FOR A BOT, WHICH MADE THIS GATE UNSATISFIABLE.
# Dependabot composes its own title, body and commit subjects and offers no
# hook to add a line to any of them. It has no issue to reference and cannot
# write `No issue:` to say so, so every dependency bump failed a REQUIRED
# check with no action its author could take. That is not a strict gate, it
# is a closed door: #805, #806 and #807 all sat red on this, and the batch
# before them (#594-#596) merged only because it predates this gate.
#
# So the fourth argument is the PR author, and `dependabot[bot]` is exempt.
#
# THE EXEMPTION IS EXACT STRING EQUALITY, not a prefix, suffix or substring
# match. A login is the one field here GitHub sets rather than the author,
# and `[bot]` is reserved for Apps, so equality is sound where a substring
# is not: `mydependabot[bot]` and `dependabot[bot]-x` are ordinary names
# somebody could hold, and a `case`/`*dependabot*` test would hand them a
# permanent waiver. The same shape — a name treated as an authenticator —
# is what #785 fixed in the plugin's setns path.
#
# AN ABSENT AUTHOR EXEMPTS NOBODY. Called with three arguments, as every
# caller did before this, the parameter is empty and the gate behaves
# exactly as it did. The failure direction is the safe one: if the workflow
# ever stops passing the author, dependency bumps go red again and somebody
# notices, rather than every PR silently acquiring a waiver.
#
# Exit: 0 reachable, 1 nothing references an issue, 2 cannot check.

set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
SYNC="$HERE/sync-issue-state-labels.sh"

RANGE="${1:-}"
TITLE_FILE="${2:-}"
BODY_FILE="${3:-}"
AUTHOR="${4:-}"

if [ -z "$RANGE" ]; then
    echo "usage: $0 <commit-range> [pr-title-file] [pr-body-file] [pr-author]" >&2
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

# --parse-title, NOT --parse (#742). `--parse` is commit_refs(), the
# merge-aware variant; the reconciler reads PR titles with refs(). A
# title of the form "Merge pull request #500 from x" therefore satisfied
# this gate and gave the reconciler nothing — a green check for a PR
# whose reference no downstream consumer can see, which is precisely the
# false green this gate exists to prevent. Deliberately still not a regex
# of our own: the point is to ask the reconciler the question the
# reconciler will answer.
if [ -z "$found" ] && [ -n "$TITLE_FILE" ] && [ -f "$TITLE_FILE" ]; then
    refs_from_title="$(bash "$SYNC" --parse-title < "$TITLE_FILE")"
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
#
# Anchored at column 0 — see the header. An indented copy is deliberately
# inert, so the gate can be quoted, explained, or have its own failure output
# pasted into a body without thereby being switched off.
WAIVER='^No issue:[[:space:]]*\S'
if [ -n "$BODY_FILE" ] && [ -f "$BODY_FILE" ] &&
   command grep -qiE "$WAIVER" "$BODY_FILE"; then
    reason="$(command grep -iE "$WAIVER" "$BODY_FILE" | head -1)"
    echo "Issue reference waived — $reason"
    exit 0
fi

# Checked after the reference and the waiver, so a bump that DOES carry a
# reference is still reported on it. Exact equality — see the header.
BOT_AUTHOR='dependabot[bot]'
if [ "$AUTHOR" = "$BOT_AUTHOR" ]; then
    echo "Issue reference exempt — authored by $BOT_AUTHOR, which cannot write a waiver."
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

    - a commit subject ending in a ref group:  fix(plugin): a thing (#<n>)
    - the same group at the end of the PR title
    - a closing keyword in the PR body:        Closes #<n>

  `#<n>` stands for the number. It is written that way rather than as a
  real one because this message gets pasted into pull request bodies, and
  GitHub's closing keywords — which the shared parser deliberately mirrors —
  are recognised anywhere in a body, indent and all. A worked example here
  would satisfy the very check it is explaining.

  If this pull request genuinely has no issue, say so in the body, as a line
  of its own starting at column 0:

    No issue: <why there is no issue>

  It is shown indented here ON PURPOSE. This message is most often read by
  pasting it into the pull request to discuss it, and an indented copy must
  not waive the gate it is quoting — which is what an unanchored waiver used
  to do, printing this very text back as the reason it passed.

MSG
exit 1
