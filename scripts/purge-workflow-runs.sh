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
# THE FIVE KEEP RULES, and why each one is not negotiable:
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
#   5. GATE BRANCH COMMITS  every run whose head SHA is one of the last N
#      commits of a branch the missing-run detector reconciles.
#
#      Rule 4 closed the collision for OPEN PR HEADS. It left the second
#      population untouched, and check-missing-runs.sh reconciles both:
#      besides open PR heads it walks the last GATE_BRANCH_COMMITS commits
#      of each GATE_BRANCHES branch and demands a run that EXECUTED.
#
#      Nothing protected those. Measured 2026-08-28 against the live
#      listing: the keep-10 group set spanned 05:37:07Z to 05:58:32Z --
#      twenty-one MINUTES, because ten groups is ten pushes and this
#      repository pushes constantly. All 15 of `dev`'s reconciled commits
#      and 14 of `main`'s 15 were outside it. So a branch commit was held
#      only by rule 1's 7-day window, and `main` moves at releases: its
#      tip of 2026-08-23 crossed 7 days on 2026-08-30 with nothing else
#      holding it.
#
#      The reachability walk cannot save a branch TIP -- coverage
#      propagates from a tested descendant to its ancestors, and a tip has
#      no descendant. So the tip's runs age out, the detector finds no
#      executed run, and it goes red on a schedule, on main, recurring
#      every release cycle, for exactly the reason it went red on #221.
#
#      THE SCOPE IS READ, NOT RESTATED. Both scripts take the branches and
#      the depth from .github/gate-branch-scope.env. Setting the same
#      numbers in two workflow files would recreate the bug one edit later,
#      silently, in the direction that destroys data.
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
#   KEEP_BRANCH_COMMITS 0 = do not protect gate branch commits (default 1).
#                     The isolation seam for keep rule 5, same as above.
#   GATE_SCOPE_FILE   where the branch scope comes from
#                     (default .github/gate-branch-scope.env). Shared with
#                     check-missing-runs.sh so the population that gate
#                     READS and the one this SPARES cannot drift (#874).
#   GATE_BRANCHES / GATE_BRANCH_COMMITS  override the scope file; the
#                     self-test seams, not set in production.
#   DRY_RUN           1 = report only, do not delete (default 1)
#   NOW_EPOCH         override "now" -- the seam the self-test drives
# Exit:  0 ok, 1 one or more deletions failed, 2 refused to run

set -uo pipefail

RETENTION_DAYS="${RETENTION_DAYS:-7}"
KEEP_GROUPS="${KEEP_GROUPS:-10}"
DRY_RUN="${DRY_RUN:-1}"
PROVENANCE_PATHS="${PROVENANCE_PATHS:-.github/workflows/release.yml .github/workflows/runner-image.yml .github/workflows/netboot-image.yml}"
KEEP_OPEN_PR_HEADS="${KEEP_OPEN_PR_HEADS:-1}"
KEEP_BRANCH_COMMITS="${KEEP_BRANCH_COMMITS:-1}"

# Keep rule 5's population, read from the file check-missing-runs.sh reads.
# A missing or incomplete scope REFUSES rather than falling back: a default
# here could be narrower than the detector's scope, and the whole point of
# the rule is that the two cannot disagree. Refusing to purge is cheap;
# deleting the evidence a scheduled gate then demands is not.
if [ "$KEEP_BRANCH_COMMITS" != "0" ]; then
    SCOPE_FILE="${GATE_SCOPE_FILE:-$(dirname "$0")/../.github/gate-branch-scope.env}"
    if [ ! -f "$SCOPE_FILE" ] || [ ! -r "$SCOPE_FILE" ]; then
        # `-f` as well as `-r`, because a DIRECTORY is readable. Without it a
        # scope path that names a directory sails past this test, fails to
        # source, and is then caught by the completeness check below under the
        # wrong diagnosis -- "incomplete" for a file that was never a file.
        # Exit 2 was preserved; the diagnosis is the product of a gate that
        # fires unattended at 03:00.
        echo "::error title=No branch scope::cannot read ${SCOPE_FILE} as a regular file, so the branch" \
             "commits that check-missing-runs.sh reconciles cannot be determined. Refusing rather than" \
             "deleting the run records that gate reads (#874)." >&2
        exit 2
    fi
    # A CARRIAGE RETURN IS WHITESPACE TO grep AND A CHARACTER TO THE API.
    # A CRLF-saved scope file sources GATE_SCOPE_BRANCHES as `dev<CR>`: the
    # guard below accepts it (CR is in [[:space:]], so the anchored `$` still
    # matches) and the value then goes onto the commit query as a branch that
    # does not exist -- rule 5 protecting nothing while printing a count.
    if grep -qF "$(printf '\r')" "$SCOPE_FILE"; then
        echo "::error title=Branch scope has carriage returns::${SCOPE_FILE} has CRLF line endings, so" \
             "its values would carry a trailing carriage return onto the commit query and protect" \
             "nothing. Refusing (#874)." >&2
        exit 2
    fi
    # NOTHING IN THAT FILE BUT THE TWO KEYS. It is SOURCED, so every other
    # line in it runs as this script's own configuration -- and this script
    # deletes data. Measured 2026-08-28 against the shipped code: a line
    # reading `DRY_RUN=0` turned a dry run into three real deletions, and
    # `KEEP_OPEN_PR_HEADS=0` disarmed keep rule 4 and deleted the open PR
    # head's runs that rule exists to protect, both silently and both
    # exiting 0. A pasted or typo'd line must REFUSE, not reconfigure the
    # gate. So every non-comment line has to be an assignment to one of the
    # two keys, with a value drawn from a character set that cannot expand.
    #
    # This check is duplicated in check-missing-runs.sh, and the duplication
    # is safe in the way the SCOPE VALUES are not. If the two copies drift,
    # one script becomes stricter and refuses -- and a refusal destroys
    # nothing. Drift in the VALUES is what deletes evidence the other gate
    # then demands, which is why those live in one file rather than two.
    scope_foreign=$(grep -nE '[^[:space:]]' "$SCOPE_FILE" \
        | grep -vE '^[0-9]+:[[:space:]]*#' \
        | grep -vE '^[0-9]+:[[:space:]]*GATE_SCOPE_(BRANCHES|COMMITS)=("[A-Za-z0-9_./ -]*"|[A-Za-z0-9_./-]+)[[:space:]]*$')
    if [ -n "$scope_foreign" ]; then
        echo "::error title=Branch scope has foreign content::${SCOPE_FILE} contains a line that is" \
             "neither a comment nor a plain GATE_SCOPE_BRANCHES/GATE_SCOPE_COMMITS assignment. The" \
             "file is sourced, so such a line reconfigures this purge -- DRY_RUN or a keep rule --" \
             "and it deletes run records. Refusing (#874). Offending line(s): ${scope_foreign}" >&2
        exit 2
    fi
    # A DUPLICATED KEY IS LAST-WINS, AND EVERY LINE OF IT IS INDIVIDUALLY
    # LEGAL, so the guard above passes it. Measured 2026-08-28 against the
    # code as it stood: appending `GATE_SCOPE_COMMITS=1` to the shipped file
    # narrowed both gates from 15 commits per branch to 1 with both self-test
    # suites fully green, and appending a second `GATE_SCOPE_BRANCHES=""`
    # printed "rule DISABLED" and deleted the branch commits keep rule 5
    # exists to spare. A second definition IS a second enumeration, which is
    # the defect this file was created to remove -- arriving inside the file.
    scope_dups=$(sed -nE 's/^[[:space:]]*(GATE_SCOPE_(BRANCHES|COMMITS))=.*/\1/p' "$SCOPE_FILE" \
        | sort | uniq -d | tr '\n' ' ')
    if [ -n "${scope_dups% }" ]; then
        echo "::error title=Branch scope defines a key twice::${SCOPE_FILE} assigns ${scope_dups}more" \
             "than once. The file is sourced, so the last assignment silently wins and the population" \
             "this purge spares narrows below the one check-missing-runs.sh reconciles. Refusing (#874)." >&2
        exit 2
    fi
    # UNSET BEFORE SOURCING. The completeness check below has to be satisfied
    # by the FILE and by nothing else; an exported GATE_SCOPE_BRANCHES in the
    # environment would otherwise stand in for a file that failed to load, and
    # this script deletes run records.
    unset GATE_SCOPE_BRANCHES GATE_SCOPE_COMMITS
    # shellcheck source=../.github/gate-branch-scope.env disable=SC1091
    if ! . "$SCOPE_FILE"; then
        echo "::error title=No branch scope::${SCOPE_FILE} could not be sourced, so the branch commits" \
             "check-missing-runs.sh reconciles cannot be determined. Refusing (#874)." >&2
        exit 2
    fi
    if [ -z "${GATE_SCOPE_BRANCHES+x}" ] || [ -z "${GATE_SCOPE_COMMITS:-}" ]; then
        echo "::error title=Branch scope incomplete::${SCOPE_FILE} does not define both" \
             "GATE_SCOPE_BRANCHES and GATE_SCOPE_COMMITS. Refusing rather than protecting a" \
             "narrower population than check-missing-runs.sh reconciles (#874)." >&2
        exit 2
    fi
    # A BRANCH LIST WITH NO WORDS IS NOT A BRANCH LIST, AND `-n` CANNOT TELL.
    # Measured 2026-08-28: GATE_SCOPE_BRANCHES="   " satisfies the foreign
    # content guard (space is in its character class), satisfies every `-n`
    # presence test in both self-test suites, and then makes `for br in
    # $BRANCHES` below iterate ZERO times -- so the shape refusal inside that
    # loop never runs, this purge deletes the branch commits keep rule 5
    # exists to spare, and it exits 0 printing "0 commit(s) protected across
    # [   ]" as though that were a count. Presence is one character to the
    # side of the property; count WORDS.
    #
    # Emptiness stays legal through the GATE_BRANCHES environment seam, which
    # is where the self-tests need it. It is never legal in the file.
    # shellcheck disable=SC2086
    # WHAT THIS COUNT DEPENDS ON, because it depends on a neighbour: `set --`
    # performs pathname expansion, and so does the `for br in $BRANCHES` that
    # consumes the list further down. Neither can glob only because the
    # foreign-content class above admits no `*` or `?`. If that class is ever
    # widened, BOTH sites need `set -f` -- hardening one of the two would look
    # like the problem was handled.
    if [ "$(set -- $GATE_SCOPE_BRANCHES; echo $#)" -eq 0 ]; then
        echo "::error title=Branch scope names no branch::${SCOPE_FILE} sets GATE_SCOPE_BRANCHES to a" \
             "value with no words in it, which disarms keep rule 5 while it reports a count of zero as" \
             "success. Refusing rather than deleting the run records check-missing-runs.sh reads (#874)." >&2
        exit 2
    fi
    # And the depth has to be a positive integer, for the same reason one
    # level down: it goes onto the query as per_page, where `0`, `abc` or a
    # value carrying a stray character protects a population of nothing.
    scope_depth_ok=1
    case "$GATE_SCOPE_COMMITS" in
        ''|*[!0-9]*) scope_depth_ok=0 ;;
        *) [ "$GATE_SCOPE_COMMITS" -ge 1 ] || scope_depth_ok=0 ;;
    esac
    if [ "$scope_depth_ok" -eq 0 ]; then
        echo "::error title=Branch scope depth is not a count::${SCOPE_FILE} sets GATE_SCOPE_COMMITS to" \
             "'${GATE_SCOPE_COMMITS}', which is not a positive integer; it goes onto the commit query as" \
             "per_page and would protect nothing. Refusing (#874)." >&2
        exit 2
    fi
    BRANCHES="${GATE_BRANCHES-$GATE_SCOPE_BRANCHES}"
    BRANCH_COMMITS="${GATE_BRANCH_COMMITS:-$GATE_SCOPE_COMMITS}"
fi

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
BRSHA=$(mktemp) || exit 2
trap 'rm -f "$RUNS" "$PROV" "$OPENPR" "$BRSHA"' EXIT

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
    trap 'rm -f "$RUNS" "$PROV" "$OPENPR" "$BRSHA" "$PRRAW"' EXIT
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

# --- the gate branches' recent commits (keep rule 5) ----------------------
#
# Same three outcomes as rule 4, and for the same reason: a branch that
# yields no commits is not a state this repository can be in, so an empty
# answer here means the query failed or the field moved, and either way the
# rule would silently protect nothing while printing a count that reads
# like success.
#
# NOT `--paginate`. The detector asks for exactly per_page=N commits and
# stops; paginating would walk the entire history and protect all of it.
if [ "$KEEP_BRANCH_COMMITS" != "0" ] && [ -n "${BRANCHES:-}" ]; then
    BRRAW=$(mktemp) || exit 2
    trap 'rm -f "$RUNS" "$PROV" "$OPENPR" "$BRSHA" "${PRRAW:-}" "$BRRAW"' EXIT
    for br in $BRANCHES; do
        # `select`, not a bare `.sha`: on an object without the field jq
        # emits the literal string "null", which is non-empty and would
        # sail through the count below as a protected SHA that protects
        # nothing. The self-test drives exactly that shape.
        if ! api "repos/$REPO/commits?sha=${br}&per_page=${BRANCH_COMMITS}" \
                --jq '.[] | select(.sha != null and .sha != "") | .sha' > "$BRRAW" 2>/dev/null; then
            echo "::error title=Cannot list branch commits::the commit query for '$br' failed for $REPO," \
                 "so the branch commits check-missing-runs.sh reconciles cannot be determined." \
                 "Refusing rather than deleting the run records that gate reads (#874)." >&2
            exit 2
        fi
        br_n=$(grep -c . "$BRRAW")
        if [ "$br_n" -eq 0 ]; then
            echo "::error title=Branch commit shape changed::listing '$br' yielded no commit SHAs." \
                 "A gate branch always has commits, so the .sha field this rule reads has moved or" \
                 "been renamed, and the rule is now protecting nothing while reporting success." >&2
            exit 2
        fi
        cat "$BRRAW" >> "$BRSHA"
    done
    sort -u -o "$BRSHA" "$BRSHA"
    echo "Gate branches: $(grep -c . "$BRSHA") commit(s) protected across [${BRANCHES}] (last ${BRANCH_COMMITS} each)"
else
    : > "$BRSHA"
    echo "Gate branches: rule DISABLED"
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
trap 'rm -f "$RUNS" "$PROV" "$OPENPR" "$BRSHA" "${PRRAW:-}" "${BRRAW:-}" "$RUNS.shas" "$DEL"' EXIT

# ARGV ORDER IS LOAD-BEARING: the four keep sets are read as records and
# the run listing last. An empty keep file contributes no records, which is
# why `prsha` may legitimately stay empty -- and why the emptiness has to be
# adjudicated above, where the three causes are still distinguishable.
awk -F'\t' -v cut="$cutoff" '
    FILENAME==ARGV[1] { prov[$1]=1; next }
    FILENAME==ARGV[2] { ksha[$1]=1; next }
    FILENAME==ARGV[3] { prsha[$1]=1; next }
    FILENAME==ARGV[4] { brsha[$1]=1; next }
    $5 != "completed" { next }          # never touch a run still in flight
    $3 >= cut         { next }
    ($2 in ksha)      { next }
    ($2 in prsha)     { next }          # keep rule 4: head of an open PR
    ($2 in brsha)     { next }          # keep rule 5: a gate branch commit
    ($1 in prov)      { next }
    { print $1 }
' "$PROV" "$RUNS.shas" "$OPENPR" "$BRSHA" "$RUNS" > "$DEL"

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
        echo "- Gate branch commits protected: **$(grep -c . "$BRSHA")**"
        echo "- Deleted: **${ok}**, failed: **${fail}**"
    } >> "$GITHUB_STEP_SUMMARY"
fi
[ "$fail" -eq 0 ] || exit 1
