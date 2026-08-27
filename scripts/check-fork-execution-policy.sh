#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The three settings that stand between a fork PR and root on the
# self-hosted pool (#830).
#
# WHAT IS ACTUALLY AT STAKE. Five workflows trigger on `pull_request`
# AND place jobs on self-hosted runners: coverage-presence, coverage,
# integration, release-backmerge and test. None of them carries a fork
# guard -- no `if: github.event.pull_request.head.repo.full_name ==
# github.repository` anywhere in the tree. The integration lane runs as
# root. So the approval policy below is not defence in depth; on the day
# it is relaxed it is the ONLY thing that was stopping fork-authored
# code from executing there, and nothing in the repository goes red.
#
# #593 was this class -- an untrusted ref reaching credentialed,
# root, self-hosted jobs -- and the answer then was a gate.
#
# STATED PLAINLY: THIS IS PROPHYLACTIC. No incident sits behind it. The
# corpus ran at 1 precautionary gate in 60 before this one and #831.
# That is recorded here, in the header, because the finding of the CI
# review is that gates get added faster than the reason for them gets
# recorded -- so the person later deciding whether this can go needs to
# see "no incident yet" from inside the file, not from an issue.
#
# THE SETTINGS LIVE IN THE WEB UI, NOT IN THE TREE. That is exactly why
# they need a gate: `git log` cannot show the change, a diff cannot show
# the change, and review cannot catch it. It is also why this gate is
# the only kind that must reach the network to do its job.
#
# WHY THIS NEEDS AN ELEVATED TOKEN, AND WHICH ONE. The default
# GITHUB_TOKEN cannot read repository administration settings -- the
# tree already records this at scorecard.yml, where SCORECARD_TOKEN, a
# read-only single-repo fine-grained PAT with Administration: read,
# exists for the same reason. This gate reuses it rather than adding a
# second credential. A consequence follows and is not negotiable: this
# must NOT run in a lane that fork pull requests can reach. Secrets are
# withheld from fork PRs, so there it would refuse on every run and be
# discharged as noise within a month.
#
# THREE VERDICTS. "I could not ask" and "the answer is wrong" are
# different, and collapsing them sends the reader at 02:00 to the wrong
# place. A drifted setting is a failure; an unreadable endpoint is a
# refusal that names which side went dark.
#
# Inputs (environment):
#   REPO       owner/name to query (default: the canonical repository)
#   GH_TOKEN   passed through to `gh`; needs Administration: read
#
# Exit: 0 all three settings are as documented
#       1 at least one has drifted -- each is named with want vs got
#       2 CANNOT JUDGE -- a query failed or answered in a shape that is
#         none of the documented values

set -uo pipefail

REPO="${REPO:-claymore666/docker-net-dhcp}"

refuse() {
    echo "::error title=Fork-execution policy cannot be judged::$*" >&2
    exit 2
}

# ask <endpoint> <jq> -> prints `value:<v>` | `error:<text>`
#
# `gh api --jq` PRINTS A 4xx ERROR BODY ON STDOUT, so a value read here
# is guarded on SHAPE by its caller and never on emptiness -- an
# unguarded read would take the JSON error object as an answer. That is
# not hypothetical: it is the defect #827 found in check-attestation-
# parity, in a block no test had ever executed. There is deliberately no
# seam above this: the self-test stubs `gh` on PATH so that every line
# below runs exactly as it runs in CI.
ask() {
    local ep="$1" jqexpr="$2" out err rc detail
    err="$(mktemp)"
    out="$(gh api "repos/$REPO/$ep" --jq "$jqexpr" 2>"$err")"
    rc=$?
    if [ "$rc" -eq 0 ] && [ -n "$out" ]; then
        printf 'value:%s' "$out"
    else
        detail="$(tr '\n' ' ' < "$err" | cut -c1-200)"
        if [ -z "${detail// /}" ]; then
            detail="$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
        fi
        if [ -z "${detail// /}" ]; then
            detail="gh exited $rc and printed nothing on either stream"
        fi
        printf 'error:%s' "$detail"
    fi
    rm -f "$err"
}

# expect <label> <endpoint> <jq> <valid-regex> <wanted> <consequence>
#
# THE VALID-REGEX IS NOT DECORATION, AND IT IS THE HALF THAT IS EASY TO
# LEAVE OUT. Without it, `gh` printing a JSON error object on stdout
# yields a non-empty string that is simply != the wanted value, and this
# gate reports DRIFT -- "approval_policy is '{"message":"Not Found"}',
# documented as 'all_external_contributors'" -- sending whoever reads it
# to the settings page to fix a setting that was never measured. An
# unreadable answer must refuse, never disagree.
drift=0
expect() {
    local label="$1" ep="$2" jqexpr="$3" valid="$4" want="$5" why="$6" answer got
    answer="$(ask "$ep" "$jqexpr")"
    case "$answer" in
        value:*) got="${answer#value:}" ;;
        error:*) refuse "could not read $label from $ep: ${answer#error:}. Nothing was measured, so this is not a pass." ;;
        *)       refuse "reading $label returned '${answer:0:60}', which is neither value:<v> nor error:<text>." ;;
    esac

    if ! [[ "$got" =~ $valid ]]; then
        refuse "$label came back as '${got:0:80}', which is not one of the values this setting can hold. The endpoint answered with something that is not this setting -- an error body, a changed schema, or the wrong field. Nothing was measured."
    fi

    if [ "$got" = "$want" ]; then
        echo "  ok   $label = $got"
        return
    fi
    echo "::error title=Fork-execution policy drifted::$label is '$got', documented as '$want'. $why" >&2
    drift=$((drift + 1))
}

expect "approval_policy" \
       "actions/permissions/fork-pr-contributor-approval" \
       ".approval_policy" \
       "^(all_external_contributors|first_time_contributors|first_time_contributors_new_to_github|none)$" \
       "all_external_contributors" \
       "This is the only barrier between a fork pull request and execution on the self-hosted pool, where the integration lane runs as root, because no workflow carries a fork guard. Relaxing it means fork-authored code runs there on the next PR."

expect "default_workflow_permissions" \
       "actions/permissions/workflow" \
       ".default_workflow_permissions" \
       "^(read|write)$" \
       "read" \
       "A write-by-default GITHUB_TOKEN hands every job in every workflow push access, including jobs that only needed to read."

expect "can_approve_pull_request_reviews" \
       "actions/permissions/workflow" \
       ".can_approve_pull_request_reviews" \
       "^(true|false)$" \
       "false" \
       "Letting Actions approve pull requests lets a workflow satisfy the review requirement that branch protection exists to impose."

if [ "$drift" -gt 0 ]; then
    echo "::error title=Fork-execution policy drifted::$drift of 3 settings differ from what is documented. Each is named above with what it is and what it should be." >&2
    exit 1
fi

echo "fork-execution policy: all 3 settings are as documented for $REPO."
exit 0
