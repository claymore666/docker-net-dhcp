#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Delete old workflow runs, keeping a recent window, the last N complete CI
# groups, and every run that is build provenance for a published artifact.
#
# WHY THIS EXISTS. The repository accumulates roughly 325 runs a day. A
# one-off purge buys about ten days, so the cleanup has to be scheduled or
# it is not a fix. This is that scheduled job's body.
#
# THE FOUR KEEP RULES, and why each one is not negotiable:
#
#   1. RETENTION_DAYS   everything newer than the window.
#
#   2. KEEP_GROUPS      every run belonging to the last N *groups*, where a
#      group is one code event and all the workflows it triggered, keyed on
#      head SHA. NOT "the newest N runs per workflow" -- that returns a
#      shredded picture after an idle stretch: ten Test runs from unrelated
#      commits, with no Integration run beside any of them. The point of the
#      floor is to come back from two weeks away and find ten COMPLETE CI
#      invocations, so the floor has to be group-shaped.
#
#   3. PROVENANCE       every run of a workflow that publishes an artifact,
#      forever. These are the only record of what produced an image someone
#      may have pinned. Deleting them is not a tidy-up, it is destroying the
#      audit trail for software already in someone else's hands.
#
#   4. OPEN PR HEADS    every run whose head SHA is the head of an OPEN pull
#      request. Added after this script broke check-missing-runs.sh on its
#      first real outing (#740's detector, #837's purge).
#
#      That detector answers "was this head ever tested" by counting run
#      records, because a head that no runner ever saw is indistinguishable
#      from one still queueing and `gh pr checks` will report the PREVIOUS
#      commit's checks against it. Deleting an open PR head's runs destroys
#      the only evidence it was tested, and the detector then reports the
#      head as never run -- with a remedy attached ("push an empty commit")
#      that spends a privileged CI cycle repairing a bookkeeping artifact.
#
#      Measured 2026-08-27: PR #221's head had runs at 03:31 and none at
#      14:35, and the scheduled detector went red on a draft that had been
#      parked since June. Nothing recovers the answer afterwards -- the one
#      check-run that survived on that head belongs to `github-advanced-
#      security`, not `github-actions`, so filtering the Checks API for
#      Actions returns zero as well. The evidence is simply gone.
#
#      The rule is cheap and self-limiting: open PRs are a handful, and a
#      head stops being protected the moment its PR closes or merges.
#
# PROVENANCE IS KEYED ON WORKFLOW PATH, NEVER ON DISPLAY NAME. This is not
# a style choice, it is a bug that was already made and caught: a name-keyed
# rule was written first, and eight release runs from the first day of
# Docker Hub dual-publish did not carry the display name "Release" -- they
# predate the workflow's `name:` and are recorded under the raw path. The
# name-keyed rule put them in the delete set while the sanity check reported
# "0 release runs to delete", because the check keyed on the same wrong
# field it was checking. The path is the workflow's identity; the name is a
# label that changed once and can change again.
#
# NON-VACUITY. A purge that finds nothing to purge and a purge that cannot
# see anything look identical from the outside: both delete zero runs and
# exit 0. This refuses to proceed on an empty listing (exit 2), because the
# failure it is guarding is "the API shape changed and we are now flying
# blind", and that must be loud.
#
# Usage: bash scripts/purge-workflow-runs.sh
# Env:
#   REPO              owner/name (default: gh's current repo)
#   RETENTION_DAYS    keep everything newer than this (default 7)
#   KEEP_GROUPS       keep this many most-recent CI groups (default 10)
#   PROVENANCE_PATHS  space-separated workflow paths never deleted
#   KEEP_OPEN_PR_HEADS 0 = do not protect open PR heads (default 1). The
#                     seam the self-test drives; there is no reason to
#                     turn it off in production.
#   DRY_RUN           1 = report only, do not delete (default 1)
#   NOW_EPOCH         override "now" -- the seam the self-test drives
# Exit:  0 ok, 1 one or more deletions failed, 2 refused to run

set -uo pipefail

RETENTION_DAYS="${RETENTION_DAYS:-7}"
KEEP_GROUPS="${KEEP_GROUPS:-10}"
DRY_RUN="${DRY_RUN:-1}"
PROVENANCE_PATHS="${PROVENANCE_PATHS:-.github/workflows/release.yml .github/workflows/runner-image.yml .github/workflows/netboot-image.yml}"
KEEP_OPEN_PR_HEADS="${KEEP_OPEN_PR_HEADS:-1}"

# The seam is HERE, at the transport, and deliberately not one level up.
# A seam above the filtering would leave the filtering untested while the
# self-test graded a stand-in -- which is exactly the defect this repo
# found in check-attestation-parity (#827). Stub `gh` on PATH to drive
# everything below this line for real.
api() { gh api "$@"; }

REPO="${REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)}"
if [ -z "$REPO" ]; then
    echo "::error title=No repository::set REPO=owner/name" >&2
    exit 2
fi

now="${NOW_EPOCH:-$(date -u +%s)}"
cutoff_epoch=$(( now - RETENTION_DAYS * 86400 ))
cutoff=$(date -u -d "@${cutoff_epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || {
    echo "::error title=Cannot compute cutoff::date(1) rejected @${cutoff_epoch}" >&2
    exit 2
}
echo "Repository:  $REPO"
echo "Cutoff:      $cutoff  (${RETENTION_DAYS}d)"
echo "Group floor: last ${KEEP_GROUPS} CI groups"

RUNS=$(mktemp) || exit 2
PROV=$(mktemp) || exit 2
OPENPR=$(mktemp) || exit 2
trap 'rm -f "$RUNS" "$PROV" "$OPENPR"' EXIT

# --- the provenance run ids, resolved from PATHS not names ---------------
for p in $PROVENANCE_PATHS; do
    wid=$(api "repos/$REPO/actions/workflows" --paginate \
        --jq ".workflows[] | select(.path == \"$p\") | .id" 2>/dev/null | head -1)
    if [ -z "$wid" ]; then
        # A provenance workflow that no longer exists is not an error -- but
        # a typo in PROVENANCE_PATHS silently protects nothing, so say so.
        echo "::warning title=Provenance workflow not found::$p matched no workflow; nothing is being protected under that path"
        continue
    fi
    api "repos/$REPO/actions/workflows/$wid/runs" --paginate \
        --jq '.workflow_runs[].id' 2>/dev/null >> "$PROV"
done
sort -u -o "$PROV" "$PROV"
echo "Provenance:  $(wc -l < "$PROV") run(s) protected across $(wc -w <<<"$PROVENANCE_PATHS") workflow path(s)"

# --- the open pull requests' head SHAs (keep rule 4) ----------------------
#
# THREE OUTCOMES, NOT TWO, and the middle one is the whole point. "no open
# pull requests" is a legitimate state of a repository; "I could not ask"
# and "the field I read has been renamed" are not, and all three produce an
# empty list. An empty list here silently disarms the rule and the next run
# deletes exactly the evidence this exists to protect -- with no error, and
# a summary line reporting 0 protected that reads like good news.
#
# So the transport failure refuses, and a non-empty PR list that yields no
# SHAs refuses separately, naming the shape change. Only a genuinely empty
# repository passes through with nothing protected.
if [ "$KEEP_OPEN_PR_HEADS" != "0" ]; then
    PRRAW=$(mktemp) || exit 2
    trap 'rm -f "$RUNS" "$PROV" "$OPENPR" "$PRRAW"' EXIT
    if ! api "repos/$REPO/pulls?state=open&per_page=100" --paginate \
            --jq '.[] | [.number, .head.sha] | @tsv' > "$PRRAW" 2>/dev/null; then
        echo "::error title=Cannot list open pull requests::the open-PR query failed for $REPO," \
             "so the heads that must never be purged cannot be determined." \
             "Refusing rather than deleting an open PR's only evidence that it was ever tested (#740)." >&2
        exit 2
    fi
    pr_rows=$(grep -c . "$PRRAW")
    awk -F'\t' 'NF >= 2 && $2 != "" { print $2 }' "$PRRAW" | sort -u > "$OPENPR"
    pr_shas=$(grep -c . "$OPENPR")
    if [ "$pr_rows" -gt 0 ] && [ "$pr_shas" -eq 0 ]; then
        echo "::error title=Open-PR head shape changed::$pr_rows open pull request(s) were listed" \
             "and not one yielded a head SHA. The .head.sha field this rule reads has moved or been" \
             "renamed, and the rule is now protecting nothing while reporting success." >&2
        exit 2
    fi
    echo "Open PRs:    $pr_shas head(s) protected across $pr_rows open pull request(s)"
else
    : > "$OPENPR"
    echo "Open PRs:    rule DISABLED by KEEP_OPEN_PR_HEADS=0"
fi

# --- every run ------------------------------------------------------------
api "repos/$REPO/actions/runs" --paginate \
    --jq '.workflow_runs[] | [.id, .head_sha, .created_at, .event, .status] | @tsv' \
    2>/dev/null > "$RUNS"

if [ ! -s "$RUNS" ]; then
    echo "::error title=Nothing to inspect::the run listing came back empty for $REPO." \
         "This job would otherwise report a successful cleanup having examined no runs at all." >&2
    exit 2
fi
echo "Runs seen:   $(wc -l < "$RUNS")"

# --- partition ------------------------------------------------------------
# A group is one head SHA from a CODE event. Issue- and schedule-triggered
# runs carry the default branch's SHA, so counting them as groups would let
# a week of label churn evict every real CI invocation from the floor.
mapfile -t KEEPSHA < <(
    awk -F'\t' '$4=="push"||$4=="pull_request"||$4=="workflow_dispatch"||$4=="merge_group" {
        if (!($2 in m) || $3 > m[$2]) m[$2]=$3
    } END { for (s in m) print m[s]"\t"s }' "$RUNS" \
    | sort -r | head -n "$KEEP_GROUPS" | cut -f2
)
printf '%s\n' "${KEEPSHA[@]}" | grep -v '^$' | sort -u > "$RUNS.shas"

DEL=$(mktemp) || exit 2
trap 'rm -f "$RUNS" "$PROV" "$OPENPR" "${PRRAW:-}" "$RUNS.shas" "$DEL"' EXIT

# ARGV ORDER IS LOAD-BEARING: the three keep sets are read as records and
# the run listing last. An empty keep file contributes no records, which is
# why `prsha` may legitimately stay empty -- and why the emptiness has to be
# adjudicated above, where the three causes are still distinguishable.
awk -F'\t' -v cut="$cutoff" '
    FILENAME==ARGV[1] { prov[$1]=1; next }
    FILENAME==ARGV[2] { ksha[$1]=1; next }
    FILENAME==ARGV[3] { prsha[$1]=1; next }
    $5 != "completed" { next }          # never touch a run still in flight
    $3 >= cut         { next }
    ($2 in ksha)      { next }
    ($2 in prsha)     { next }          # keep rule 4: head of an open PR
    ($1 in prov)      { next }
    { print $1 }
' "$PROV" "$RUNS.shas" "$OPENPR" "$RUNS" > "$DEL"

echo "To delete:   $(wc -l < "$DEL")"

if [ ! -s "$DEL" ]; then
    echo "Nothing older than the window that is not protected. Done."
    exit 0
fi

if [ "$DRY_RUN" != "0" ]; then
    echo "DRY RUN -- nothing deleted. Set DRY_RUN=0 to apply."
    exit 0
fi

ok=0; fail=0
while read -r id; do
    if api -X DELETE "repos/$REPO/actions/runs/$id" --silent 2>/dev/null; then
        ok=$((ok+1))
    else
        fail=$((fail+1))
    fi
done < "$DEL"

echo "Deleted $ok run(s), $fail failure(s)."
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
        echo "### Workflow run retention"
        echo
        echo "- Window: **${RETENTION_DAYS} days** (cutoff \`${cutoff}\`)"
        echo "- Group floor: last **${KEEP_GROUPS}** CI groups"
        echo "- Provenance protected: **$(wc -l < "$PROV")** run(s)"
        echo "- Open PR heads protected: **$(grep -c . "$OPENPR")**"
        echo "- Deleted: **${ok}**, failed: **${fail}**"
    } >> "$GITHUB_STEP_SUMMARY"
fi
[ "$fail" -eq 0 ] || exit 1
