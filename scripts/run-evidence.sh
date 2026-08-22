#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Release-evidence assembler (#432).
#
# "Three consecutive green runs against an unchanged tree" is this
# project's bar for trusting a change of this class. For v1.4.0 those
# three passes were NOT run under matched conditions — pass 2 overlapped
# ~6 minutes with an Integration run on another ref — and that caveat
# only exists because a notification happened to go by. Nothing recorded
# it, so a future reader of the release PR cannot reconstruct it, and
# had one of the three failed, "was it contention?" would have been
# unanswerable after the fact.
#
# That matters more than bookkeeping. The suite's most expensive
# failures this cycle were all probabilistic and all first read as
# flakiness (#425 passed three times and failed twice on one commit;
# #296's serialization rested on two flakes, one later shown by #307 to
# be misdiagnosed). For that class, "what else was running" is not
# context — it is the variable.
#
# Usage: run-evidence.sh <tree-sha> [workflow]
#   <tree-sha>: the tree the release is cut from (git rev-parse HEAD^{tree})
#   [workflow]: workflow file to search (default integration.yml)
#
# Env: GATE_REPO=owner/repo (default: inferred from git remote)
#
# Exit: 0 report produced, 2 cannot check.
#
# ABSENT DATA IS REPORTED AS UNKNOWN, NEVER AS "ran alone". A blank
# taken for a clean condition is the entire failure mode here, and this
# project has made that exact mistake before — an aged-out log read as
# zero failures produced a confident wrong conclusion.
set -uo pipefail

TREE="${1:-}"
WORKFLOW="${2:-integration.yml}"

if [ -z "$TREE" ]; then
    echo "usage: $0 <tree-sha> [workflow]" >&2
    exit 2
fi

if ! command -v gh >/dev/null || ! command -v jq >/dev/null; then
    echo "run-evidence: needs gh and jq" >&2
    exit 2
fi

REPO="${GATE_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)}"
if [ -z "$REPO" ]; then
    echo "run-evidence: cannot determine the repository (set GATE_REPO)" >&2
    exit 2
fi

# All recent runs of the target workflow, plus every other privileged
# run, so overlap can be judged against what actually shares the pool.
runs=$(gh api "repos/${REPO}/actions/workflows/${WORKFLOW}/runs?per_page=60" \
        --jq '[.workflow_runs[] | {id, sha: .head_sha, branch: .head_branch, status, conclusion, started: .run_started_at, ended: .updated_at, attempt: .run_attempt}]' 2>/dev/null) || {
    echo "run-evidence: could not read runs of ${WORKFLOW}" >&2
    exit 2
}

others=$(gh api "repos/${REPO}/actions/runs?per_page=100" \
        --jq '[.workflow_runs[] | select(.name == "Integration" or .name == "Coverage")
               | {id, name, branch: .head_branch, started: .run_started_at, ended: .updated_at}]' 2>/dev/null) || others=""

if [ -z "$others" ]; then
    echo "run-evidence: WARNING — could not read the concurrent-run list." >&2
    echo "  Overlap will be reported as unknown, not as none (#432)." >&2
    others="[]"
    others_readable=false
else
    others_readable=true
fi

# How far back the concurrent-run list actually reaches.
#
# This is the load-bearing line, and the first version of this script
# did not have it. /actions/runs returns the most recent N runs across
# ALL workflows, so on a busy repo a 100-run page can cover well under
# an hour. Judging a run older than that against an empty slice yields
# "none — ran alone", which is the reassuring answer and an unfounded
# one.
#
# Found by running the first version against the v1.4.0 release tree: it
# reported all three verification passes as having run alone, on a page
# whose oldest entry was NEWER than every one of them. The claim was
# not wrong by luck — it was unsupported.
HORIZON=$(printf '%s' "$others" | jq -r '[.[].started] | map(select(. != null)) | min // empty')
if [ -z "$HORIZON" ]; then
    others_readable=false
fi

echo "Evidence for tree ${TREE}  (workflow: ${WORKFLOW}, repo: ${REPO})"
echo

matched=0
# The unit separator, not a tab. `read` collapses runs of IFS
# WHITESPACE, so with IFS=$'\t' an empty field cannot be represented
# and every column after it shifts left by one. `.conclusion` is null
# for every run that has not finished, which is most of them while a
# release is being assembled:
#
#   printf 'a\tb\t\td\n' | IFS=$'\t' read -r p q r s  ->  p=[a] q=[b] r=[d] s=[]
#   printf 'a|b||d\n'     | IFS='|'    read -r p q r s  ->  p=[a] q=[b] r=[]  s=[d]
#
# The collapse is worse than a blank, because it feeds the timing
# guard below plausible non-empty wrong values -- run_attempt landing
# in `ended` -- so the guard passes and the overlap query silently
# searches a window that does not exist.
while IFS=$'\x1f' read -r id sha branch status conclusion started ended attempt; do
    [ -z "$id" ] && continue
    # Resolve this run's head commit to its tree. A commit we cannot
    # resolve is skipped loudly rather than assumed to be a mismatch.
    t=$(gh api "repos/${REPO}/commits/${sha}" --jq '.commit.tree.sha' 2>/dev/null) || {
        echo "  run ${id}: could not resolve ${sha} to a tree — SKIPPED (unknown, not excluded)" >&2
        continue
    }
    [ "$t" != "$TREE" ] && continue
    matched=$((matched + 1))

    overlap="unknown"
    if [ "$others_readable" = true ]; then
        if [ -z "$started" ] || [ "$started" = "null" ] || [ -z "$ended" ] || [ "$ended" = "null" ]; then
            overlap="unknown (this run has no timing)"
        elif [[ "$started" < "$HORIZON" ]]; then
            # The run predates everything the concurrent list can see,
            # so "no overlap found" would mean "did not look".
            overlap="unknown — the concurrent-run list only reaches back to ${HORIZON}"
        else
            names=$(printf '%s' "$others" | jq -r --arg id "$id" --arg s "$started" --arg e "$ended" '
                [ .[] | select((.id|tostring) != $id)
                      | select(.started != null and .ended != null)
                      | select(.started < $e and .ended > $s)
                      | "\(.name)[\(.branch)]" ] | unique | join(", ")')
            # A run that has not completed has an open window: `ended`
            # is updated_at, which is when it was last touched, not when
            # it finished. Anything that starts after this instant will
            # still have shared the pool with it. So an empty result here
            # means "nothing has overlapped YET", which is not the same
            # claim as "ran alone" -- and this script exists to keep
            # those two apart.
            #
            # Presence is not affected. An overlap the query CAN see
            # already happened, and stays a fact whether or not the run
            # is finished; only its absence is provisional.
            if [ -z "$names" ]; then
                if [ "$status" != "completed" ]; then
                    overlap="unknown — this run has not completed (status: ${status}), so its window is still open"
                else
                    overlap="none — ran alone"
                fi
            elif [ "$status" != "completed" ]; then
                overlap="$names (so far — this run has not completed)"
            else
                overlap="$names"
            fi
        fi
    fi

    printf '  run %s  attempt %s  %s/%s  branch %s\n' "$id" "$attempt" "$status" "${conclusion:-<none>}" "$branch"
    printf '      window   %s .. %s\n' "$started" "$ended"
    printf '      overlap  %s\n\n' "$overlap"
done < <(printf '%s' "$runs" | jq -r '.[] | [.id, .sha, .branch, .status, .conclusion, .started, .ended, .attempt]
               | map(if . == null then "" else tostring end) | join("\u001f")')

if [ "$matched" -eq 0 ]; then
    echo "  no runs found for this tree."
    echo "  That is 'no evidence', not 'no failures' — the tree may never have been tested,"
    echo "  or its runs may have aged out of the API window (#432)."
fi
