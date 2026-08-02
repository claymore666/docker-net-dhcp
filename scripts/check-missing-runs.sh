#!/usr/bin/env bash
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
# Usage: check-missing-runs.sh [grace-minutes]
#   [grace-minutes]: how long a head may have no run before it counts as
#                    missing (default 20). Covers ordinary queueing and
#                    the concurrency group's serialization.
#
# Env: GATE_REPO=owner/repo (default: inferred)
#
# Exit: 0 every open PR head has a run, 1 at least one does not,
#       2 cannot check.
#
# NOT fail-open. This exists because a silence was mistaken for health;
# a detector that goes quiet when it cannot read would reproduce the
# very bug it looks for.
set -uo pipefail

GRACE_MIN="${1:-20}"

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

n=$(printf '%s' "$prs" | jq 'length')
if [ "$n" = "0" ]; then
    echo "check-missing-runs: no open PRs"
    exit 0
fi

now=$(date -u +%s)
missing=0
checked=0

while IFS=$'\t' read -r num head branch updated draft; do
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

if [ "$missing" -eq 0 ]; then
    echo "check-missing-runs: ${checked} open PR head(s) past the ${GRACE_MIN}m grace, all have runs"
    exit 0
fi

cat >&2 <<EOF

${missing} open PR head(s) have no workflow run.

Actions can silently drop push / pull_request event delivery while
otherwise healthy — observed 2026-08-01, when three consecutive pushes
produced zero runs and a manual dispatch worked fine. The danger is that
a head with no run is indistinguishable from one still waiting, and
\`gh pr checks\` reports the PREVIOUS commit's checks against it without
saying so. A PR can read as passing for code no runner ever saw.

To recover: push an empty commit, or re-run the workflow by dispatch
against that ref. Closing and reopening the PR does NOT re-fire it.
EOF
exit 1
