#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-missing-runs.sh (#418).
#
# `gh` is stubbed via PATH. The cases that matter are the ones where the
# detector cannot see: this exists because a silence was mistaken for
# health, so every unreadable answer must be loud rather than clean.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-missing-runs.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

OLD=$(date -u -d '3 hours ago' +%Y-%m-%dT%H:%M:%SZ)
NEW=$(date -u -d '2 minutes ago' +%Y-%m-%dT%H:%M:%SZ)

# make_gh <dir> <pulls-json> <commit-date> <run-count>
# A run count of "ERR" makes the runs query fail.
make_gh() {
    local dir="$1" pulls="$2" cdate="$3" runs="$4"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/gh" <<EOF
#!/usr/bin/env bash
args="\$*"
case "\$args" in
  *"pulls?state=open"*) cat <<'J'
$pulls
J
  ;;
  *"/commits/"*)
     [ "$cdate" = "ERR" ] && exit 1
     echo "$cdate" ;;
  *"actions/runs?head_sha"*)
     [ "$runs" = "ERR" ] && exit 1
     echo "$runs" ;;
  *"repo view"*) echo "o/r" ;;
  *) echo "" ;;
esac
EOF
    chmod +x "$dir/bin/gh"
}

run_it() {
    local dir; dir=$(mktemp -d)
    make_gh "$dir" "$1" "$2" "$3"
    # Branch phase off: these cases are about open PR heads.
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES='' bash "$CHECK" 20 >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

# make_branch_gh <dir> <commits-tsv> <run-states>
# commits-tsv: "<sha>\t<date>" lines, or "ERR" to fail the commits query.
# run-states:  "<status>:<conclusion>" lines, "" for none, "ERR" to fail.
make_branch_gh() {
    local dir="$1" commits="$2" states="$3"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/gh" <<EOF
#!/usr/bin/env bash
args="\$*"
case "\$args" in
  *"pulls?state=open"*) echo '[]' ;;
  *"commits?sha="*)
     [ "$commits" = "ERR" ] && exit 1
     cat <<'J'
$commits
J
     ;;
  *"/runs?head_sha"*)
     [ "$states" = "ERR" ] && exit 1
     cat <<'J'
$states
J
     ;;
  *"repo view"*) echo "o/r" ;;
  *) echo "" ;;
esac
EOF
    chmod +x "$dir/bin/gh"
}

run_branch() {
    local dir; dir=$(mktemp -d)
    make_branch_gh "$dir" "$1" "$2"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES=dev \
        bash "$CHECK" 20 >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

ONE_PR='[{"number":7,"head":"abc123def456","branch":"feature/x","updated":"2026-08-01T10:00:00Z","draft":false}]'

# --- the happy path ---------------------------------------------------
out=$(run_it "$ONE_PR" "$OLD" "3"); rc=$?
[ "$rc" = 0 ] && ok "a head with runs passes" || no "a head with runs failed (exit $rc): $out"

# --- the incident shape ----------------------------------------------
out=$(run_it "$ONE_PR" "$OLD" "0"); rc=$?
if [ "$rc" = 1 ]; then ok "a head with zero runs fails"; else no "zero runs did not fail (exit $rc)"; fi
case "$out" in *"#7"*) ok "the failure names the PR" ;; *) no "PR not named: $out" ;; esac

# THE REMEDY IS PART OF THE VERDICT, so it is asserted like one. This
# gate's message named exactly one cause -- dropped event delivery --
# and told the reader to push an empty commit. After #837 added a
# retention purge that deletes run records, a zero here can also mean
# "the runs were deleted", and on 2026-08-27 it did: a draft PR head
# parked since June went red on a schedule, on main, with a remedy
# attached that would have spent a privileged CI cycle repairing a
# bookkeeping artifact.
#
# A message is prose and prose decays silently, so the two causes and
# the rule that makes the second one impossible are checked here. If
# someone trims this message back to one cause, this goes red.
# Keyed on the MECHANISM, not on the word "deleted": that word appears
# three more times in this message describing the keep rule, so a check
# for it is satisfied by prose that never mentions the second cause at
# all. Driving the mutant is what exposed that -- rewording the cause
# left the assertion green.
case "$out" in
  *"retention purge"*) ok "the failure names the retention purge as the second cause" ;;
  *) no "the failure names only dropped delivery; the purge cause is missing: $out" ;;
esac
case "$out" in
  *"KEEP RULE 4"*) ok "the failure names the keep rule that makes the second cause impossible" ;;
  *) no "the failure does not point at the purge's keep rule: $out" ;;
esac
case "$out" in
  *"github-advanced-security"*) ok "the failure warns that the Checks API does not recover the answer" ;;
  *) no "the failure does not say check-runs cannot answer this: $out" ;;
esac

# THE REMEDY MUST BE EXECUTABLE FOR THE HEADS IT IS PRINTED FOR, and for
# two months it was not. The tag-and-dispatch recipe uses the workflow
# file at the TARGET ref, so it needs that ref's own integration.yml to
# declare workflow_dispatch -- added in #419. Every head older than that
# carries push and pull_request triggers only and the dispatch is simply
# refused, which is precisely the population this remedy addresses: old
# heads whose evidence is gone. Measured 2026-08-28 on PR #221's head
# 41aa96b4, the head that is red right now: no workflow_dispatch at that
# ref, so the recipe printed beneath the failure could not have worked.
#
# Keyed on the MECHANISM and not on the token "workflow_dispatch", which
# now appears three times in this message -- including inside the
# verification command -- so a check for the bare token stays green over
# a message that never states WHY the dispatch fails. Same mistake this
# file already made once with the word "deleted".
case "$out" in
  *"AS IT EXISTS AT THE TARGET REF"*) ok "the remedy states which workflow file a dispatch uses" ;;
  *) no "the remedy does not say a dispatch reads the TARGET ref's workflow: $out" ;;
esac
case "$out" in
  *"git show <ref>:.github/workflows/integration.yml"*)
      ok "the remedy hands the reader the command that checks the precondition" ;;
  *) no "the remedy names a precondition with no way to test it: $out" ;;
esac

# --- inside the grace -------------------------------------------------
# A push a moment ago has legitimately not been picked up yet. Flagging
# it would make the detector cry wolf on every push and get muted.
out=$(run_it "$ONE_PR" "$NEW" "0"); rc=$?
[ "$rc" = 0 ] && ok "a head inside the grace window is not flagged" || no "a fresh push was flagged (exit $rc)"

# --- cannot see: every one of these must be loud ---------------------
out=$(run_it "$ONE_PR" "ERR" "3"); rc=$?
if [ "$rc" = 1 ]; then ok "an unresolvable head commit is reported, not skipped"; else
  no "unresolvable head returned $rc — silence is what this tool exists to catch"; fi

out=$(run_it "$ONE_PR" "$OLD" "ERR"); rc=$?
if [ "$rc" = 1 ]; then ok "an unreadable runs query is reported, not skipped"; else
  no "unreadable runs query returned $rc"; fi

# --- no PRs -----------------------------------------------------------
out=$(run_it '[]' "$OLD" "3"); rc=$?
[ "$rc" = 0 ] && ok "no open PRs is clean" || no "no open PRs returned $rc"

# --- missing tooling --------------------------------------------------
BASH_BIN=$(command -v bash)
PATH="" "$BASH_BIN" "$CHECK" >/dev/null 2>&1
[ "$?" = 2 ] && ok "missing gh/jq exits 2 rather than reporting clean" || no "missing tooling did not exit 2"

# --- branch heads (#515) --------------------------------------------
#
# The shape that has to go red is NOT "no run". It is a run that exists,
# is cancelled, and never started a job — which is what a burst of
# merges leaves behind, and what the #418 question answered "fine" to.

OLD_COMMIT="deadbeef1111	$OLD"
NEW_COMMIT="deadbeef2222	$NEW"

out=$(run_branch "$OLD_COMMIT" "completed:success"); rc=$?
[ "$rc" = 0 ] && ok "a branch head with an executed run passes" || no "executed run failed (exit $rc): $out"

out=$(run_branch "$OLD_COMMIT" "completed:cancelled"); rc=$?
if [ "$rc" = 1 ]; then ok "a branch head whose only run was CANCELLED fails (the #515 shape)"; else
  no "a cancelled-only head returned $rc — this is the exact commit a merge burst leaves untested: $out"; fi
case "$out" in
  *deadbeef*) ok "the failure names the commit" ;;
  *) no "commit not named: $out" ;;
esac
case "$out" in
  *cancelled*) ok "the failure says the run was cancelled, not merely absent" ;;
  *) no "cause not stated: $out" ;;
esac

out=$(run_branch "$OLD_COMMIT" "completed:failure"); rc=$?
[ "$rc" = 0 ] && ok "a run that executed and FAILED still counts as covered" || \
  no "a red run was reported as missing (exit $rc) — this gate is about coverage, not results"

out=$(run_branch "$OLD_COMMIT" "in_progress:none"); rc=$?
[ "$rc" = 0 ] && ok "a run still in progress counts as covered" || no "in-progress run flagged (exit $rc)"

# A re-run after the cancellation is the documented recovery, so the
# head must go clean again once one exists.
out=$(run_branch "$OLD_COMMIT" "completed:cancelled
completed:success"); rc=$?
[ "$rc" = 0 ] && ok "a cancelled run plus a later executed one is covered" || \
  no "the re-run recovery path still reports missing (exit $rc): $out"

out=$(run_branch "$OLD_COMMIT" ""); rc=$?
[ "$rc" = 1 ] && ok "a branch head with no run at all fails" || no "no-run head returned $rc"

out=$(run_branch "$NEW_COMMIT" ""); rc=$?
[ "$rc" = 0 ] && ok "a branch head inside the grace window is not flagged" || \
  no "a fresh commit was flagged (exit $rc) — this would fire on every merge and get muted"

# --- branch phase cannot see: loud, never clean ----------------------
out=$(run_branch "ERR" "completed:success"); rc=$?
[ "$rc" = 1 ] && ok "an unlistable branch is reported, not skipped" || \
  no "unlistable branch returned $rc — silence is what this tool exists to catch"

out=$(run_branch "$OLD_COMMIT" "ERR"); rc=$?
[ "$rc" = 1 ] && ok "an unreadable branch runs query is reported, not skipped" || \
  no "unreadable branch runs query returned $rc"

# --- the phase can be turned off ------------------------------------
dir=$(mktemp -d); make_branch_gh "$dir" "$OLD_COMMIT" ""
PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES='' bash "$CHECK" 20 >/dev/null 2>&1
[ "$?" = 0 ] && ok "GATE_BRANCHES empty skips the branch phase" || no "empty GATE_BRANCHES still reconciled branches"
rm -rf "$dir"

# --- the multi-commit push (#740) ------------------------------------
#
# GitHub creates one push-triggered run per push EVENT, at the tip only.
# The gate used to demand a run keyed on every commit, so every rebase
# and every stacked push made it unsatisfiable: 25 red runs out of 60,
# in streaks that ended by ageing out rather than by anything being
# fixed. These cases pin both directions of the replacement rule.
#
# The stub answers per-sha, because the whole point is that different
# commits in one window have different run states.
make_chain_gh() { # make_chain_gh <dir> <commits-tsv> <sha:states pairs...>
    local dir="$1" commits="$2"; shift 2
    local cases=""
    local pair
    for pair in "$@"; do
        cases="${cases}  *\"head_sha=${pair%%:*}\"*) printf '%s\\n' '${pair#*:}' ;;
"
    done
    mkdir -p "$dir/bin"
    cat > "$dir/bin/gh" <<EOF
#!/usr/bin/env bash
args="\$*"
case "\$args" in
  *"pulls?state=open"*) echo '[]' ;;
  *"commits?sha="*)
     cat <<'J'
$commits
J
     ;;
${cases}  *"/runs?head_sha"*) : ;;
  *"repo view"*) echo "o/r" ;;
  *) echo "" ;;
esac
EOF
    chmod +x "$dir/bin/gh"
}

run_chain() {
    local dir; dir=$(mktemp -d)
    make_chain_gh "$dir" "$@"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES=dev \
        bash "$CHECK" 20 >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

# tip <- mid <- base, all old enough to flag. Only the tip has a run,
# which is exactly what one `git push` of three commits produces.
CHAIN="$(printf 'ccc333\t%s\tbbb222\nbbb222\t%s\taaa111\naaa111\t%s\t\n' \
    "$OLD" "$OLD" "$OLD")"

out=$(run_chain "$CHAIN" "ccc333:completed:success"); rc=$?
[ "$rc" = 0 ] && ok "a 3-commit push with a run only on the tip is clean" || \
  no "the non-tip commits of a single push were flagged (exit $rc): $out"

# The negative control, and the reason this cannot simply be "only ever
# check the tip": a tip whose own run was cancelled before any job
# started is the #515 incident this gate was built for, and it must
# still fail — with its two ancestors unflagged, since nothing tested
# reaches them either but the tip is where the actionable report is.
out=$(run_chain "$CHAIN" "ccc333:completed:cancelled"); rc=$?
[ "$rc" = 1 ] && ok "a tip whose only run was cancelled still fails" || \
  no "a cancelled tip went clean (exit $rc) — that is the incident this gate exists for: $out"

# Coverage must reach past the window's youngest commit: a tip too new
# to flag still covers what it contains, otherwise every merge would
# flag its own parents for the length of the grace window.
FRESH_CHAIN="$(printf 'ccc333\t%s\tbbb222\nbbb222\t%s\taaa111\naaa111\t%s\t\n' \
    "$NEW" "$OLD" "$OLD")"
out=$(run_chain "$FRESH_CHAIN" "ccc333:completed:success"); rc=$?
[ "$rc" = 0 ] && ok "a tip inside the grace window still covers its ancestors" || \
  no "ancestors of a fresh tested tip were flagged (exit $rc): $out"

# A merge commit has two parents and a tested merge covers both sides.
MERGE_CHAIN="$(printf 'mmm999\t%s\tccc333,ddd444\nccc333\t%s\t\nddd444\t%s\t\n' \
    "$OLD" "$OLD" "$OLD")"
out=$(run_chain "$MERGE_CHAIN" "mmm999:completed:success"); rc=$?
[ "$rc" = 0 ] && ok "a tested merge covers both of its parents" || \
  no "one side of a tested merge was flagged (exit $rc): $out"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
