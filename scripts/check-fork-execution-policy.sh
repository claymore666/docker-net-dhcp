#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The three settings that stand between a fork PR and root on the
# self-hosted pool (#830).
#
# WHAT IS ACTUALLY AT STAKE. TWO workflows trigger on `pull_request`
# AND place jobs on self-hosted runners: `coverage.yml` and
# `integration.yml`. Neither carries a fork guard -- no `if:
# github.event.pull_request.head.repo.full_name == github.repository`
# anywhere in the tree. The integration lane runs as root. So the
# approval policy below is not defence in depth; on the day it is
# relaxed it is the ONLY thing that was stopping fork-authored code from
# executing there, and nothing in the repository goes red.
#
# THE NUMBER WAS FIVE HERE UNTIL IT WAS MEASURED. This header claimed
# coverage-presence, release-backmerge and test as well; all three run
# every job on `ubuntu-latest`. That is not a nit in a security file. A
# reader acting on the old sentence adds three `if:` conditions that
# guard nothing and concludes the window is shut -- a false remedy
# printed beside a real risk, overstating exposure two-and-a-half-fold
# in the direction that flatters this gate. The population is now
# DERIVED below and compared against the two named above, so the next
# time it moves this refuses instead of misinforming.
#
# #593 was this class -- an untrusted ref reaching credentialed,
# root, self-hosted jobs -- and the answer then was a gate.
#
# WHY NOT JUST ADD THE FORK GUARD, now that it is two `if:` lines rather
# than five. Because the guard and the policy do different things: the
# approval policy is a maintainer-gated YES -- an outside contributor's
# integration run can be approved and will then execute -- while a fork
# guard is a permanent NO. It would mean an approved fork PR can never
# run integration, ever, with no way for the maintainer to grant it.
# That is a real capability to give up, and it is the reason this is a
# gate and not two lines, even though two lines would be cheaper and
# would prevent rather than notice.
#
# STATED PLAINLY: THIS IS PROPHYLACTIC. No incident sits behind it.
# Precautionary gates are a small minority of the corpus -- see
# `check-apk-pins.sh` for the other shape -- and that is recorded here,
# in the header, because the finding of the CI review is that gates get
# added faster than the reason for them gets recorded, so the person
# later deciding whether this can go needs to see "no incident yet" from
# inside the file rather than from an issue.
#
# The count that used to sit in that sentence is gone on purpose. It
# measured 60 when written, 62 a day later, and the header never named
# the population it was counting, so no reader could check it and no
# author could maintain it. A named example survives; a number rots.
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

# THE ENUMERATION NEEDS A WATCHER, OR IT IS A RULE ENFORCED BY READING.
# The header's whole argument rests on WHICH workflows expose the pool.
# The settings are watched; the population whose exposure is the reason
# to watch them was not, so nothing went red the day a third
# `pull_request` workflow gained a self-hosted job. This gate's own
# thesis is that a rule nothing executes is not enforced -- and until
# now this enumeration was one.
#
# So derive it and refuse on divergence. `check-dispatch-reachable.sh`
# is the precedent: derive the subjects, and refuse at zero rather than
# pass over nothing.
#
# THE `on:` BLOCK, NOT THE WHOLE FILE. A file-wide grep for
# `pull_request` matches `github.event.pull_request...` in an `if:`, and
# matches prose in a comment -- including the comment above. Both would
# admit workflows that do not trigger on it at all. The scan below reads
# the top-level `on:` mapping and nothing else.
EXPOSED_DECLARED="coverage.yml integration.yml"
WF_DIR="${WF_DIR:-.github/workflows}"

derive_exposed() {
    local f
    shopt -s nullglob
    for f in "$WF_DIR"/*.yml "$WF_DIR"/*.yaml; do
        awk -v name="$(basename "$f")" '
            # Track the top-level `on:` mapping. Any other column-0 key
            # ends it, so `jobs:` closes the block.
            /^[A-Za-z_"'"'"'-]+:/ { in_on = ($0 ~ /^(on|"on"|'"'"'on'"'"'):/) }
            in_on && /(^|[[:space:],[])pull_request([:,\]]|$)/ { pr = 1 }
            /runs-on:.*self-hosted/ { sh = 1 }
            END { if (pr && sh) print name }
        ' "$f"
    done
    shopt -u nullglob
}

exposed_now=$(derive_exposed | sort | tr '\n' ' ')
exposed_now="${exposed_now% }"
exposed_want=$(printf '%s\n' $EXPOSED_DECLARED | sort | tr '\n' ' ')
exposed_want="${exposed_want% }"

if [ -z "$exposed_now" ]; then
    refuse "derived ZERO workflows that trigger on pull_request AND place a job on a self-hosted runner, while this gate's entire justification is that '$exposed_want' do. Either the pool is no longer reachable from a pull request -- in which case this gate's reason is gone and it should be reconsidered, not left passing -- or, far more likely, the scan over $WF_DIR stopped matching."
fi

if [ "$exposed_now" != "$exposed_want" ]; then
    refuse "the set of workflows exposing the self-hosted pool to pull requests has CHANGED. This header documents '$exposed_want'; the tree now has '$exposed_now'. That enumeration is the reason this gate exists and the thing a reader acts on, so it is corrected here before any setting is judged. If the new set is right, update EXPOSED_DECLARED and the header together."
fi

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
