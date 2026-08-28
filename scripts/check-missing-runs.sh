#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Missing-run detector (#418).
#
# On 2026-08-01 three consecutive pushes to a PR branch created ZERO
# workflow runs. Actions was healthy — a manual dispatch started
# immediately, githubstatus reported all systems operational, nothing
# was queued — only push / pull_request event delivery was dropping.
#
# The failure mode is the dangerous shape: the absence of a run looks
# exactly like a run that has not finished yet, and after long enough it
# starts looking like a green branch. Nothing goes red. `gh pr checks`
# reports the PREVIOUS commit's checks against the new head without
# saying so, so a PR can read as passing for code no runner ever saw.
#
# This cannot be detected from inside a run — nothing executes to notice
# that nothing executed — so it runs on a schedule and reconciles open
# PR heads against the runs that exist.
#
# It reconciles two populations, and they need different questions.
#
# OPEN PR HEADS (#418, above): does a run exist at all?
#
# BRANCH HEADS (#515): did a run actually EXECUTE? On 2026-08-13 five
# squash merges landed in `dev` within about twenty seconds. GitHub
# keeps at most one running plus one pending run per concurrency group
# and cancels the rest, so two of the six push runs were cancelled
# before a single job started. `cancel-in-progress: false` protects the
# running run; it does not protect the pending ones.
#
# For those commits a run EXISTS — it is just cancelled and empty. So
# the #418 question ("total_count > 0") answers yes and reports them
# clean, which is why this needs the stronger predicate: a head is
# covered when it has a run that is still going, or one that reached a
# conclusion other than cancelled. Two commits are otherwise permanent
# points in `dev` history that no runner ever tested, and `git bisect`
# across that range lands on commits with no verdict.
#
# WHAT THIS GATE DEPENDS ON, discovered the hard way. It answers "was
# this head ever tested" by counting run RECORDS, so anything that
# deletes run records can manufacture a failure here. #837's retention
# purge did exactly that on 2026-08-27: it deleted the runs of an open
# draft PR head parked since June, and this gate went red on a schedule,
# on main, reporting a head as never tested and naming the wrong remedy.
#
# The fix belongs in the purge, not here -- it now carries a fourth keep
# rule that never deletes an open PR head's runs -- because there is no
# way to recover the answer afterwards. The Checks API does not do it:
# on that head the one surviving check-run belonged to
# `github-advanced-security` rather than `github-actions`, so filtering
# to Actions still answers zero.
#
# Deliberately NOT done here: an age exemption. Skipping heads older than
# the purge window would make today's red go away and would blind this
# gate to exactly the population it exists for -- a long-lived PR whose
# pushes silently produced nothing. A head with no surviving evidence IS
# unverified, and saying so is the gate working.
#
# Usage: check-missing-runs.sh [grace-minutes]
#   [grace-minutes]: how long a head may have no run before it counts as
#                    missing (default 20). Covers ordinary queueing and
#                    the concurrency group's serialization.
#
# Env: GATE_REPO=owner/repo (default: inferred)
#      GATE_BRANCHES="dev main"   branches to reconcile (empty = skip)
#      GATE_BRANCH_COMMITS=10     how far back on each branch
#      GATE_WORKFLOW=integration.yml  the workflow a branch head must have
#
# Exit: 0 every open PR head has a run and every branch head has an
#       executed one, 1 at least one does not, 2 cannot check.
#
# NOT fail-open. This exists because a silence was mistaken for health;
# a detector that goes quiet when it cannot read would reproduce the
# very bug it looks for.
set -uo pipefail

GRACE_MIN="${1:-20}"
BRANCHES="${GATE_BRANCHES-dev main}"
BRANCH_COMMITS="${GATE_BRANCH_COMMITS:-10}"
WORKFLOW="${GATE_WORKFLOW:-integration.yml}"

if ! command -v gh >/dev/null || ! command -v jq >/dev/null; then
    echo "check-missing-runs: needs gh and jq" >&2
    exit 2
fi

REPO="${GATE_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)}"
[ -n "$REPO" ] || { echo "check-missing-runs: cannot determine the repository (set GATE_REPO)" >&2; exit 2; }

prs=$(gh api "repos/${REPO}/pulls?state=open&per_page=100" \
        --jq '[.[] | {number, head: .head.sha, branch: .head.ref, updated: .updated_at, draft}]' 2>/dev/null) || {
    echo "check-missing-runs: could not list open PRs — cannot judge" >&2
    exit 2
}

now=$(date -u +%s)
missing=0
checked=0

n=$(printf '%s' "$prs" | jq 'length')
if [ "$n" = "0" ]; then
    echo "check-missing-runs: no open PRs"
    prs='[]'
fi

# `_` for updated_at, deliberately: the age that matters is the head
# COMMIT's, computed below, not the PR's. Naming it would suggest it
# was meant to be used.
while IFS=$'\t' read -r num head branch _ draft; do
    [ -z "$num" ] && continue

    # Age the HEAD COMMIT, not the PR. A PR opened last week whose head
    # was pushed a minute ago is inside the grace; the PR's own
    # updated_at would say otherwise.
    pushed=$(gh api "repos/${REPO}/commits/${head}" --jq '.commit.committer.date' 2>/dev/null) || {
        echo "  PR #${num}: could not resolve head ${head:0:8} — UNKNOWN, not clean" >&2
        missing=$((missing + 1))
        continue
    }
    pushed_s=$(date -u -d "$pushed" +%s 2>/dev/null) || {
        echo "  PR #${num}: unparseable commit date '${pushed}' — UNKNOWN, not clean" >&2
        missing=$((missing + 1))
        continue
    }
    age_min=$(( (now - pushed_s) / 60 ))
    [ "$age_min" -lt "$GRACE_MIN" ] && continue

    checked=$((checked + 1))
    runs=$(gh api "repos/${REPO}/actions/runs?head_sha=${head}&per_page=1" --jq '.total_count' 2>/dev/null) || {
        echo "  PR #${num}: could not query runs for ${head:0:8} — UNKNOWN, not clean" >&2
        missing=$((missing + 1))
        continue
    }
    if [ "$runs" = "0" ]; then
        d=""; [ "$draft" = "true" ] && d=" (draft)"
        echo "  PR #${num}${d} [${branch}] head ${head:0:8} pushed ${age_min}m ago has NO workflow run"
        missing=$((missing + 1))
    fi
done < <(printf '%s' "$prs" | jq -r '.[] | [.number, .head, .branch, .updated, .draft] | @tsv')

branch_checked=0

# Coverage bookkeeping for the reachability walk below (#740). A plain
# newline-delimited string rather than an associative array: this script
# targets the same shell everywhere the gate lane runs, and the set is
# at most BRANCH_COMMITS entries per branch.
nl=$'\n'
covered=""

is_covered() {
    case "$covered" in
        *"${nl}${1}${nl}"*) return 0 ;;
        *) return 1 ;;
    esac
}

# Merge commits have two parents and both are ancestors of a tested
# child, so both are covered. Splitting on commas is why the listing
# asks for the parent shas joined that way.
cover_parents() {
    local p
    for p in ${1//,/ }; do
        [ -n "$p" ] || continue
        is_covered "$p" || covered="${covered}${p}${nl}"
    done
}

# Branch heads (#515). A run that exists is not the question here — a
# cancelled, zero-job run exists too, and that is exactly what a burst
# of merges leaves behind.
#
# THE QUESTION IS REACHABILITY, NOT PER-COMMIT RUNS (#740). This walk
# used to demand a run keyed on every one of the last N commits. GitHub
# Actions creates one push-triggered run per push EVENT, at the tip
# commit only, so every non-tip commit of any multi-commit push — a
# rebase, a stacked branch, a fast-forward — was unsatisfiable by
# construction. The gate was red on 25 of its last 60 runs, in streaks
# of 8 to 11 hours that ended only when the commits aged out of the
# window: recovery by forgetting, not by fixing.
#
# That is not a harmless false alarm. This is the one detector built to
# catch CI lying about whether something ran, and it had been screaming
# into an empty room for two days. A detector nobody can act on is
# worse than no detector, because it teaches the reader to skip the one
# signal that was meant to be unskippable.
#
# What the maintainer actually needs to know is whether a commit was
# ever inside a tree that got tested. A commit reachable from a tested
# descendant was: the suite checked out that descendant, and it
# contains this commit. So coverage propagates from a tested commit to
# its ancestors, and only commits that no tested descendant reaches are
# genuinely untested — which still includes the case the gate exists
# for, a branch tip whose own run was cancelled before any job started.
#
# Parents come from the same listing, so this costs no extra API call,
# and a commit already covered is never queried at all.
for br in $BRANCHES; do
    commits=$(gh api "repos/${REPO}/commits?sha=${br}&per_page=${BRANCH_COMMITS}" \
                --jq '.[] | [.sha, .commit.committer.date, ([.parents[].sha] | join(","))] | @tsv' 2>/dev/null) || {
        echo "  branch ${br}: could not list commits — UNKNOWN, not clean" >&2
        missing=$((missing + 1))
        continue
    }
    [ -z "$commits" ] && continue

    # Newest first, which is the order the API returns and the order
    # coverage has to travel: a parent is always older than its child,
    # so a single pass carries every tested commit down to its
    # ancestors.
    covered="${nl}"
    while IFS=$'\t' read -r sha cdate parents; do
        [ -z "$sha" ] && continue

        cdate_s=$(date -u -d "$cdate" +%s 2>/dev/null) || {
            echo "  ${br} ${sha:0:8}: unparseable commit date '${cdate}' — UNKNOWN, not clean" >&2
            missing=$((missing + 1))
            continue
        }
        age_min=$(( (now - cdate_s) / 60 ))

        if is_covered "$sha"; then
            # A tested descendant already reached this commit: the tree
            # that ran contained it. No query, nothing to report.
            cover_parents "$parents"
            continue
        fi

        # status+conclusion per run, so "cancelled before any job
        # started" can be told apart from "ran and had an opinion".
        states=$(gh api "repos/${REPO}/actions/workflows/${WORKFLOW}/runs?head_sha=${sha}&per_page=20" \
                   --jq '.workflow_runs[] | "\(.status):\(.conclusion // "none")"' 2>/dev/null) || {
            echo "  ${br} ${sha:0:8}: could not query ${WORKFLOW} runs — UNKNOWN, not clean" >&2
            missing=$((missing + 1))
            continue
        }

        executed=""
        while IFS= read -r st; do
            [ -z "$st" ] && continue
            case "$st" in
                completed:cancelled|completed:skipped) continue ;;
                *) executed="$st"; break ;;
            esac
        done <<EOF
$states
EOF

        if [ -n "$executed" ]; then
            branch_checked=$((branch_checked + 1))
            covered="${covered}${sha}${nl}"
            cover_parents "$parents"
            continue
        fi

        # Untested, and no tested descendant reaches it. The grace
        # window is applied here rather than at the top of the loop
        # because a commit too young to flag can still be the tested
        # descendant that covers everything beneath it.
        [ "$age_min" -lt "$GRACE_MIN" ] && continue

        branch_checked=$((branch_checked + 1))
        if [ -z "$states" ]; then
            why="no ${WORKFLOW} run at all"
        else
            why="only cancelled/skipped ${WORKFLOW} run(s): $(printf '%s' "$states" | tr '\n' ' ')"
        fi
        echo "  ${br} ${sha:0:8} committed ${age_min}m ago has ${why}, and no tested descendant"
        missing=$((missing + 1))
    done <<EOF
$commits
EOF
done

if [ "$missing" -eq 0 ]; then
    echo "check-missing-runs: ${checked} open PR head(s) past the ${GRACE_MIN}m grace, all have runs"
    echo "check-missing-runs: ${branch_checked} branch commit(s) on [${BRANCHES:-none}], all have an executed ${WORKFLOW} run"
    exit 0
fi

cat >&2 <<EOF

${missing} head(s) above have no run that executed.

On a PR head there are now TWO causes, and they need different hands.

FIRST, dropped event delivery — observed 2026-08-01, when three
consecutive pushes produced zero runs while a manual dispatch worked
fine. The danger is that a head with no run is indistinguishable from
one still waiting, and \`gh pr checks\` reports the PREVIOUS commit's
checks against it without saying so. A PR can read as passing for code
no runner ever saw.

SECOND, and this is not hypothetical: THE RUNS WERE DELETED. #837's
retention purge keeps a 7-day window, the last N CI groups and the
provenance paths, and on 2026-08-27 it deleted the runs of a draft PR
head parked since June. This gate then reported that head as never
tested — truthfully, in the sense that no evidence survives, but with
the dropped-delivery remedy attached, which would spend a privileged CI
cycle repairing a bookkeeping artifact.

Tell them apart before acting. If the head is younger than the purge
window it cannot be the second cause. If it is older, check that the
purge still carries KEEP RULE 4 — never delete a run whose head SHA is
an open PR head (scripts/purge-workflow-runs.sh). With that rule intact
the second cause is IMPOSSIBLE for an open PR, so seeing it here again
means the rule has stopped working, and that is the thing to fix rather
than the head.

Note what does NOT recover the answer: the Checks API. On the 2026-08-27
head the one surviving check-run belonged to \`github-advanced-security\`,
not \`github-actions\`, so filtering to Actions still answers zero. Once
the runs are gone the head is genuinely unverified and the only honest
move is to run something on it.

On a branch head it was a merge burst (#515, #617) until the group was
keyed per commit for pushes: GitHub keeps at most one running plus one
pending run per concurrency group and cancels the rest, so several
merges landing within a few seconds left commits whose run was
cancelled before any job started. Those commits are permanent points in
the branch's history that nothing ever tested, and a bisect across them
lands on a commit with no verdict.

CHECK THIS FIRST, because every dispatch recipe below depends on it: a
dispatch uses the workflow file AS IT EXISTS AT THE TARGET REF, so it
works only if that ref's own integration.yml declares workflow_dispatch.
It was added in #419, and a head older than that carries a workflow with
push and pull_request triggers only — the dispatch is simply refused.
That is exactly the population this section is for, so verify before
spending the effort:

    git show <ref>:.github/workflows/integration.yml | grep workflow_dispatch

Measured 2026-08-28 on PR #221's head 41aa96b4: no workflow_dispatch at
that ref, so the tag recipe below cannot recover it and the empty commit
is the only remedy that works.

To recover a PR head: push an empty commit. That changes the head, which
is what clears the finding — the check asks about the CURRENT head, and
the new one gets a run. Dispatching on the PR branch works only subject
to the precondition above. Closing and reopening does NOT re-fire it.

To recover a branch commit: dispatching with \`-f ref=<sha>\` does NOT
clear it — GitHub records a dispatched run's head_sha as the tip of the
ref it was dispatched on, whatever the suite checks out. Give the commit
a ref of its own and dispatch on that, subject to the precondition above:

    git tag verify/<sha> <sha> && git push origin verify/<sha>
    gh workflow run integration.yml --ref verify/<sha>
    git push origin :verify/<sha>   # afterwards
EOF
exit 1
