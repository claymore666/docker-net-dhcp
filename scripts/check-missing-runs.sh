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
#      GATE_BRANCHES             branches to reconcile (empty = skip);
#                                default from the scope file below
#      GATE_BRANCH_COMMITS       how far back on each branch; default from
#                                the scope file below
#      GATE_SCOPE_FILE           where those two defaults come from
#                                (default .github/gate-branch-scope.env).
#                                Shared with the retention purge so the
#                                population this gate READS and the one
#                                that purge SPARES cannot drift (#874).
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

# THE BRANCH SCOPE IS NOT DEFINED HERE (#874). The purge that deletes run
# records has to spare exactly the commits this gate reads, so both read
# one file rather than each carrying its own copy of the numbers. A second
# enumeration that must agree with the first is the defect, not the fix:
# it drifts silently, and the drift destroys evidence in the direction
# this gate then reports as an untested commit.
#
# Missing or incomplete, this REFUSES. Falling back to a built-in default
# would let the two gates disagree while both looked healthy, which is the
# whole failure being closed.
SCOPE_FILE="${GATE_SCOPE_FILE:-$(dirname "$0")/../.github/gate-branch-scope.env}"
# `-f` as well as `-r`: a DIRECTORY is readable, and a scope path naming one
# would otherwise pass here, fail to source, and be caught below under the
# wrong diagnosis. Exit 2 is not the product of this gate; the diagnosis is.
if [ ! -f "$SCOPE_FILE" ] || [ ! -r "$SCOPE_FILE" ]; then
    echo "check-missing-runs: cannot read the branch scope at ${SCOPE_FILE} as a regular file — cannot judge" >&2
    exit 2
fi
# A CARRIAGE RETURN IS WHITESPACE TO grep AND A CHARACTER TO THE API. A
# CRLF-saved scope file sources GATE_SCOPE_BRANCHES as `dev<CR>`: the guard
# below accepts it (CR is in [[:space:]], so the anchored `$` still matches)
# and the value goes onto the commit query as a branch that does not exist,
# so this gate reconciles nothing and says every commit is covered.
if grep -qF "$(printf '\r')" "$SCOPE_FILE"; then
    echo "check-missing-runs: ${SCOPE_FILE} has CRLF line endings, so its values would carry a" \
         "trailing carriage return onto the commit query — cannot judge" >&2
    exit 2
fi
# NOTHING IN THAT FILE BUT THE TWO KEYS. It is SOURCED, so every other
# line in it runs as this gate's own configuration. Measured 2026-08-28
# against the shipped code: a line reading `GATE_WORKFLOW=nonexistent.yml`
# silently redirected the branch phase at a workflow that does not exist,
# and the gate still exited 0 reporting every commit covered. The same
# seam in purge-workflow-runs.sh turns a dry run into real deletions. A
# pasted or typo'd line must REFUSE, not reconfigure the gate.
#
# Duplicated in purge-workflow-runs.sh on purpose. That duplication is
# safe in the way the SCOPE VALUES are not: if the copies drift, one
# script becomes stricter and refuses, and a refusal destroys nothing.
scope_foreign=$(grep -nE '[^[:space:]]' "$SCOPE_FILE" \
    | grep -vE '^[0-9]+:[[:space:]]*#' \
    | grep -vE '^[0-9]+:[[:space:]]*GATE_SCOPE_(BRANCHES|COMMITS)=("[A-Za-z0-9_./ -]*"|[A-Za-z0-9_./-]+)[[:space:]]*$')
if [ -n "$scope_foreign" ]; then
    echo "check-missing-runs: ${SCOPE_FILE} contains a line that is neither a comment nor a plain" \
         "GATE_SCOPE_BRANCHES/GATE_SCOPE_COMMITS assignment; the file is sourced, so such a line" \
         "reconfigures this gate — cannot judge. Offending line(s): ${scope_foreign}" >&2
    exit 2
fi
# A DUPLICATED KEY IS LAST-WINS AND EVERY LINE OF IT IS INDIVIDUALLY LEGAL,
# so the guard above passes it. Measured 2026-08-28 against the code as it
# stood: appending `GATE_SCOPE_COMMITS=1` to the shipped file narrowed both
# gates from 15 commits per branch to 1 with both self-test suites fully
# green. A second definition IS a second enumeration, which is the defect
# this file was created to remove -- arriving inside the file.
scope_dups=$(sed -nE 's/^[[:space:]]*(GATE_SCOPE_(BRANCHES|COMMITS))=.*/\1/p' "$SCOPE_FILE" \
    | sort | uniq -d | tr '\n' ' ')
if [ -n "${scope_dups% }" ]; then
    echo "check-missing-runs: ${SCOPE_FILE} assigns ${scope_dups}more than once; the file is sourced," \
         "so the last assignment silently wins and the population narrows — cannot judge" >&2
    exit 2
fi
# UNSET BEFORE SOURCING, so the completeness check below is satisfied by the
# FILE and not by an exported value standing in for a file that failed to load.
unset GATE_SCOPE_BRANCHES GATE_SCOPE_COMMITS
# shellcheck source=../.github/gate-branch-scope.env disable=SC1091
if ! . "$SCOPE_FILE"; then
    echo "check-missing-runs: ${SCOPE_FILE} could not be sourced — cannot judge" >&2
    exit 2
fi
if [ -z "${GATE_SCOPE_BRANCHES+x}" ] || [ -z "${GATE_SCOPE_COMMITS:-}" ]; then
    echo "check-missing-runs: ${SCOPE_FILE} does not define GATE_SCOPE_BRANCHES and GATE_SCOPE_COMMITS — cannot judge" >&2
    exit 2
fi
# A BRANCH LIST WITH NO WORDS IS NOT A BRANCH LIST, AND `-n` CANNOT TELL.
# Measured 2026-08-28: GATE_SCOPE_BRANCHES="   " satisfies the foreign
# content guard (space is in its character class), satisfies every `-n`
# presence test in both self-test suites, and then makes `for br in
# $BRANCHES` iterate ZERO times -- this gate reports "0 branch commit(s) on
# [   ], all have an executed run" and exits 0, while the purge deletes the
# very commits it was reconciling. Presence is one character to the side of
# the property; count WORDS.
#
# Emptiness stays legal through the GATE_BRANCHES environment seam, which is
# where the self-tests need it. It is never legal in the file.
# shellcheck disable=SC2086
# WHAT THIS COUNT DEPENDS ON, because it depends on a neighbour: `set --`
# performs pathname expansion, and so does the `for br in $BRANCHES` that
# consumes the list further down. Neither can glob only because the
# foreign-content class above admits no `*` or `?`. If that class is ever
# widened, BOTH sites need `set -f` -- hardening one of the two would look
# like the problem was handled.
if [ "$(set -- $GATE_SCOPE_BRANCHES; echo $#)" -eq 0 ]; then
    echo "check-missing-runs: ${SCOPE_FILE} sets GATE_SCOPE_BRANCHES to a value with no words in it," \
         "which silently turns the branch phase off on this gate and on the purge — cannot judge" >&2
    exit 2
fi
# And the depth has to be a positive integer: it goes onto the query as
# per_page, where `0`, `abc` or a stray character reconciles nothing.
scope_depth_ok=1
case "$GATE_SCOPE_COMMITS" in
    ''|*[!0-9]*) scope_depth_ok=0 ;;
    *) [ "$GATE_SCOPE_COMMITS" -ge 1 ] || scope_depth_ok=0 ;;
esac
if [ "$scope_depth_ok" -eq 0 ]; then
    echo "check-missing-runs: ${SCOPE_FILE} sets GATE_SCOPE_COMMITS to '${GATE_SCOPE_COMMITS}', which is" \
         "not a positive integer; it goes onto the commit query as per_page — cannot judge" >&2
    exit 2
fi

# The environment still wins, because the self-tests drive these seams.
# `-` not `:-` on BRANCHES: an explicitly empty value means "skip the
# branch phase", which is different from "not set".
BRANCHES="${GATE_BRANCHES-$GATE_SCOPE_BRANCHES}"
BRANCH_COMMITS="${GATE_BRANCH_COMMITS:-$GATE_SCOPE_COMMITS}"
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

# --- recovered heads: evidence that was destroyed, recorded once ---------
#
# A head whose runs were DELETED is indistinguishable from one that was
# never tested: both answer zero. Keep rule 4 in purge-workflow-runs.sh
# stops this repository creating new ones, but it cannot resurrect the
# records already gone, and no query recovers them.
#
# So an entry here says "a named run observed this head as tested, and
# something later deleted the record". It is a claim about EVIDENCE, not
# a permission to be quiet, and the difference is enforced: every field
# is required and a malformed line REFUSES the whole run rather than
# being skipped. A head with no entry and no runs still fails.
#
# Each entry ends by itself, because it names a commit id rather than a
# rule: it stops applying the moment that commit stops being the head it
# was recorded for. Nothing has to remember to remove it.
# The workflow name a witness run must carry. Named once: the check
# below compares against it, and a rename that missed this line would
# turn every entry into a refusal rather than a silent pass.
GATE_NAME="${GATE_WITNESS_NAME:-Missing runs}"
RECOVERED_FILE="${GATE_RECOVERED_FILE:-$(dirname "$0")/../.github/recovered-heads.tsv}"
recovered=""
rec_nl=$'\n'
if [ -f "$RECOVERED_FILE" ]; then
    rec_line=0
    while IFS= read -r rline || [ -n "$rline" ]; do
        rec_line=$((rec_line + 1))
        case "${rline#"${rline%%[![:space:]]*}"}" in ''|'#'*) continue ;; esac
        rsha=$(printf '%s' "$rline" | cut -f1)
        robs=$(printf '%s' "$rline" | cut -f2)
        rrun=$(printf '%s' "$rline" | cut -f3)
        rwhy=$(printf '%s' "$rline" | cut -f4-)
        rbad=""
        case "$rsha" in
            ''|*[!0-9a-f]*) rbad="the commit id is not lowercase hex" ;;
        esac
        if [ -z "$rbad" ] && [ "${#rsha}" -ne 40 ]; then
            rbad="the commit id is ${#rsha} characters, not a full 40-character sha"
        fi
        if [ -z "$rbad" ]; then
            case "$robs" in
                [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
                *) rbad="the observed date '${robs}' is not YYYY-MM-DD" ;;
            esac
        fi
        if [ -z "$rbad" ]; then
            case "$rrun" in
                ''|*[!0-9]*) rbad="the run id '${rrun}' is not numeric — name the run that SAW this head tested" ;;
            esac
        fi
        if [ -z "$rbad" ] && [ -z "$(printf '%s' "$rwhy" | tr -d '[:space:]')" ]; then
            rbad="no reason is given for why the head cannot simply be re-tested"
        fi
        if [ -n "$rbad" ]; then
            echo "::error title=Recovered-heads record is not usable::${RECOVERED_FILE}:${rec_line}: ${rbad}." \
                 "Every entry must carry a full sha, an observed date, the run id that saw the head tested," \
                 "and a reason. Refusing the whole run rather than skipping the line: a record that can be" \
                 "written badly and ignored is a place to dump commit ids, which is exactly what this must" \
                 "not become." >&2
            exit 2
        fi
        recovered="${recovered}${rsha} ${robs} ${rrun}${rec_nl}"
    done < "$RECOVERED_FILE"
fi

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
        # A recorded head is REPORTED, never silent. The whole failure
        # this file exists for is a zero that nobody saw, and a record
        # that made heads disappear from the output would reproduce it
        # one level up -- with the added charm of being invisible in the
        # green run where it matters.
        rec=$(printf '%s' "$recovered" | awk -v s="$head" '$1 == s { print; exit }')
        if [ -n "$rec" ]; then
            set -- $rec
            # THE WITNESS IS CHECKED FOR TRUTH, NOT PRESENCE. A numeric
            # field that nothing resolves is an id-shaped hole: any
            # plausible number silences a head, which is the failure
            # mode of every allowlist whose reason is never read.
            #
            # But note WHAT is verified, because the obvious check is
            # wrong. The witness is a scheduled run of THIS detector
            # whose log observed the head; it is NOT a run on that head
            # -- run 33036626425 carries head_sha 56a72b66 (main).
            # Asserting run.head_sha == the entry's sha would reject
            # every valid entry. What must hold is that the run is this
            # gate and that it CONCLUDED SUCCESS: a detector run that
            # failed is one that found a head missing, so its word is
            # not evidence that every head had runs.
            #
            # Fail-open on a run that no longer exists, and only there.
            # The entire premise is that run records get deleted, so a
            # gate demanding the witness still exist would rot into
            # exactly the dependency this record was built to survive.
            wname=$(gh api "repos/${REPO}/actions/runs/$3" \
                    --jq '[.name, (.conclusion // "-")] | @tsv' 2>/dev/null) || wname=""
            if [ -n "$wname" ]; then
                wn=$(printf '%s' "$wname" | cut -f1)
                wc=$(printf '%s' "$wname" | cut -f2)
                if [ "$wn" != "$GATE_NAME" ] || [ "$wc" != "success" ]; then
                    echo "::error title=Recovered-heads witness does not support its claim::" \
                         "${RECOVERED_FILE}: run $3 is '${wn}' concluding '${wc}', not a successful" \
                         "'${GATE_NAME}' run. Only a successful run of this detector establishes" \
                         "that every open head had runs; anything else cannot witness ${head:0:8}." >&2
                    missing=$((missing + 1))
                    continue
                fi
                wverdict="verified"
            else
                # Not an error: this is the documented condition.
                wverdict="the witness run's own record is gone too, so it could not be re-checked"
            fi
            echo "  PR #${num}${d} [${branch}] head ${head:0:8}: no surviving run, but run $3 observed it" \
                 "tested on $2 before the record was deleted (${wverdict}; ${RECOVERED_FILE})"
            continue
        fi
        echo "  PR #${num}${d} [${branch}] head ${head:0:8} pushed ${age_min}m ago has NO workflow run"
        missing=$((missing + 1))
    fi
done < <(printf '%s' "$prs" | jq -r '.[] | [.number, .head, .branch, .updated, .draft] | @tsv')

# SPENT ENTRIES ARE NAMED, NOT ENFORCED, AND THE DISTINCTION IS THE
# DESIGN. An entry is keyed on a commit id, so once that commit stops
# being a head the entry matches nothing and can silence nothing -- it
# has already expired, which is the property that lets the record exist
# without a review date. Making a spent entry FAIL would take that back:
# it converts "nothing has to remember to remove it" into a cleanup
# obligation enforced by a red on the scheduled main lane, for a line
# that is provably inert. So this reports and does not refuse.
#
# It is still worth saying out loud, because the one thing the record
# genuinely cannot do is shrink by itself, and an accumulating file
# nobody reads is how a record of evidence turns into a list of excuses.
if [ -n "$recovered" ]; then
    while IFS= read -r rline; do
        [ -n "$rline" ] || continue
        rsha=${rline%% *}
        if ! printf '%s' "$prs" | jq -e --arg s "$rsha" 'any(.[]; .head == $s)' >/dev/null 2>&1; then
            echo "  note: recovered-heads entry ${rsha:0:8} is spent — it is no longer any open" \
                 "PR's head, so it can no longer spare anything. Delete the line when convenient."
        fi
    done <<EOF
$recovered
EOF
fi

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

On a branch head there are TWO causes as well, and the same rule of
thumb separates them: a cancelled run leaves a record, a purge leaves
none.

FIRST, a merge burst (#515, #617), until the group was keyed per commit
for pushes: GitHub keeps at most one running plus one pending run per
concurrency group and cancels the rest, so several merges landing
within a few seconds left commits whose run was cancelled before any
job started. Those commits are permanent points in the branch's history
that nothing ever tested, and a bisect across them lands on a commit
with no verdict. This cause prints "only cancelled/skipped run(s)"
above, because the records are still there.

SECOND, THE RUNS WERE DELETED — the same #837 purge, reaching the same
gate through the other population. It prints "no run at all", and it is
the likelier reading for a commit on \`main\`, which moves only at
releases: the group floor is ten PUSHES wide, which on this repository
is about twenty minutes of wall clock, so between releases a branch tip
is held by the 7-day window and nothing else. Once past it the tip's
runs are deletable, and the reachability walk cannot save a TIP —
coverage travels from a tested descendant to its ancestors, and a tip
has no descendant.

Keep rule 5 in scripts/purge-workflow-runs.sh is what makes this
impossible: it spares every run whose head is one of the last N commits
of a gate branch, reading .github/gate-branch-scope.env — the same file
this gate reads, so the population spared and the population demanded
are one list. Seeing this cause on a branch head means that rule has
stopped working, or the two scripts have stopped reading the same
scope, and THAT is the thing to fix rather than the commit.

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
