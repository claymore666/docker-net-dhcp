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
  *"actions/runs/"*)
     # The witness lookup. "GONE" makes the API answer fail, which is
     # the documented condition, not an error.
     [ "\${WITNESS:-GONE}" = "GONE" ] && exit 1
     printf '%s\n' "\${WITNESS}" ;;
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
[ -n "\${GHLOG:-}" ] && echo "\$args" >> "\$GHLOG"
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
[ -n "\${GHLOG:-}" ] && echo "\$args" >> "\$GHLOG"
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

# --- the recovered-heads record (#874) --------------------------------
#
# A head whose runs were DELETED answers zero exactly like a head that
# was never tested. The record separates them by naming the run that saw
# the head tested -- so these cases drive the SEPARATION, not the file:
# the recorded head is spared AND a different untested head still goes
# red in the same scan.

# A full 40-char sha, because the record refuses anything shorter and a
# 12-char fixture would exercise the refusal instead of the acceptance.
REC_SHA=41aa96b4a302bf0cae849c158e4356c1bd286024
OTHER_SHA=0123456789abcdef0123456789abcdef01234567
REC_PR="[{\"number\":7,\"head\":\"$REC_SHA\",\"branch\":\"feature/x\",\"updated\":\"2026-08-01T10:00:00Z\",\"draft\":false}]"
OTHER_PR="[{\"number\":9,\"head\":\"$OTHER_SHA\",\"branch\":\"feature/y\",\"updated\":\"2026-08-01T10:00:00Z\",\"draft\":false}]"

# run_rec <record-body> <pulls-json>
run_rec() {
    local dir; dir=$(mktemp -d)
    make_gh "$dir" "$2" "$OLD" "0"
    printf '%s\n' "$1" > "$dir/rec.tsv"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES='' \
        WITNESS="${WITNESS:-GONE}" \
        GATE_RECOVERED_FILE="$dir/rec.tsv" bash "$CHECK" 20 >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

GOOD_REC="$(printf '%s\t2026-08-27\t33036626425\trecords destroyed by the retention purge; no re-test is possible at this ref\n' "$REC_SHA")"

out=$(run_rec "$GOOD_REC" "$REC_PR"); rc=$?
if [ "$rc" = 0 ] && [ "${out#*33036626425}" != "$out" ]; then
    ok "a recorded head is spared AND its evidence run is named in the output"
else
    no "a recorded head was not spared, or was spared silently (exit $rc): $out"
fi

# THE DIRECTION THAT MATTERS. The record must not become a way to be
# quiet: a head with no entry and no runs still fails, in a scan where
# the record is present and loaded.
out=$(run_rec "$GOOD_REC" "$OTHER_PR"); rc=$?
[ "$rc" = 1 ] && ok "an unrecorded head with no runs still fails while a record is loaded" || \
  no "the record silenced a head it does not name (exit $rc): $out"

# Evidence is the whole point, so a line that carries none is refused --
# and refused loudly enough to stop the run, not skipped as noise.
out=$(run_rec "$(printf '%s\t2026-08-27\t33036626425\t\n' "$REC_SHA")" "$REC_PR"); rc=$?
[ "$rc" = 2 ] && ok "an entry with no reason refuses the whole run" || \
  no "an entry carrying no evidence was accepted (exit $rc): $out"

out=$(run_rec "$(printf '%s\t2026-08-27\t\ta reason\n' "$REC_SHA")" "$REC_PR"); rc=$?
[ "$rc" = 2 ] && ok "an entry naming no run id refuses" || \
  no "an entry with no witnessing run was accepted (exit $rc): $out"

# A short sha would match more than one commit, which is how an
# exemption written for one head quietly covers another.
out=$(run_rec "$(printf '41aa96b4\t2026-08-27\t33036626425\ta reason\n')" "$REC_PR"); rc=$?
[ "$rc" = 2 ] && ok "an abbreviated commit id refuses" || \
  no "an abbreviated commit id was accepted (exit $rc): $out"

out=$(run_rec "$(printf '%s\t27-08-2026\t33036626425\ta reason\n' "$REC_SHA")" "$REC_PR"); rc=$?
[ "$rc" = 2 ] && ok "a malformed observed date refuses" || \
  no "a malformed date was accepted (exit $rc): $out"

# Comments and blank lines are not entries.
out=$(run_rec "$(printf '# a comment\n\n%s' "$GOOD_REC")" "$REC_PR"); rc=$?
[ "$rc" = 0 ] && ok "comments and blank lines are not read as entries" || \
  no "a comment or blank line was parsed as an entry (exit $rc): $out"

# With no record at all the gate behaves exactly as it did before it
# existed -- the control that proves the feature is additive.
dir=$(mktemp -d); make_gh "$dir" "$REC_PR" "$OLD" "0"
out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES='' \
      GATE_RECOVERED_FILE="$dir/absent.tsv" bash "$CHECK" 20 2>&1); rc=$?
rm -rf "$dir"
[ "$rc" = 1 ] && ok "with no record file the gate flags the head as before" || \
  no "an absent record file changed the verdict (exit $rc): $out"

# The record shipped in the tree must itself be loadable -- a file that
# only the fixtures ever parse is not the file CI reads.
#
# PRESENCE IS ITS OWN CASE, and it is asserted OUTSIDE the guard. Measured
# 2026-08-28: with the case wrapped in `if [ -f ... ]`, deleting
# .github/recovered-heads.tsv made it silently VANISH -- 61 passed instead
# of 62, nothing red -- while the scope file's twin assertion a few hundred
# lines down goes red for the same deletion. A universal gate is satisfied
# by emptying its domain, and a case that disappears with its fixture is
# that failure in miniature.
[ -f "$HERE/../.github/recovered-heads.tsv" ] \
  && ok "the recovered-heads record exists at the path this gate defaults to" \
  || no "no record at .github/recovered-heads.tsv -- every head it excuses is flagged again with no run to find"
# AND THE PARSE CASE FAILS RATHER THAN VANISHING. The presence assertion
# above goes red on a deleted record, so the set is not vacuous -- but this
# case still disappeared with its fixture (74 cases became 73), which is
# the same shape one level in. A case that cannot run is a case that
# reports nothing, and the count is the only place that showed it.
if [ -f "$HERE/../.github/recovered-heads.tsv" ]; then
    dir=$(mktemp -d); make_gh "$dir" "$ONE_PR" "$NEW" "1"
    out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES='' bash "$CHECK" 20 2>&1); rc=$?
    rm -rf "$dir"
    [ "$rc" = 0 ] && ok "the record committed to the tree parses under the real default path" || \
      no "the in-tree record does not load (exit $rc): $out"
else
    no "the in-tree record is absent, so whether it PARSES under the real default path was never measured"
fi

# --- the witness is checked for truth, not presence (#874) -------------
#
# A numeric field nothing resolves is an id-shaped hole: any plausible
# number silences a head. These drive what the resolution actually
# asserts -- and, as importantly, what it must NOT assert.

WIT_OK=$(printf 'Missing runs\tsuccess')

out=$(WITNESS="$WIT_OK" run_rec "$GOOD_REC" "$REC_PR"); rc=$?
if [ "$rc" = 0 ] && [ "${out#*verified}" != "$out" ]; then
    ok "a resolvable witness is verified and said to be verified"
else
    no "a good witness was not verified (exit $rc): $out"
fi

# A witness from some other workflow proves nothing about open heads.
out=$(WITNESS="$(printf 'CodeQL\tsuccess')" run_rec "$GOOD_REC" "$REC_PR"); rc=$?
[ "$rc" = 1 ] && ok "a witness from a different workflow is refused" || \
  no "any workflow was accepted as a witness (exit $rc): $out"

# A DETECTOR RUN THAT FAILED IS ONE THAT FOUND A HEAD MISSING, so its
# word is not evidence that every head had runs. This is the case that
# separates "the run exists" from "the run supports the claim".
out=$(WITNESS="$(printf 'Missing runs\tfailure')" run_rec "$GOOD_REC" "$REC_PR"); rc=$?
[ "$rc" = 1 ] && ok "a witness run that FAILED is refused, not merely present" || \
  no "a failed detector run was accepted as a witness (exit $rc): $out"

# FAIL-OPEN, AND ONLY HERE. The premise of the whole record is that run
# records get deleted; a gate that demanded the witness still exist
# would rot into the dependency the record was built to survive.
out=$(WITNESS=GONE run_rec "$GOOD_REC" "$REC_PR"); rc=$?
if [ "$rc" = 0 ] && [ "${out#*could not be re-checked}" != "$out" ]; then
    ok "a witness whose own record is gone is accepted AND says so"
else
    no "a vanished witness was not handled as the documented case (exit $rc): $out"
fi

# The refusal must not become a way to be quiet either: an unrecorded
# head still fails while witness verification is switched on.
out=$(WITNESS="$WIT_OK" run_rec "$GOOD_REC" "$OTHER_PR"); rc=$?
[ "$rc" = 1 ] && ok "an unrecorded head still fails with witness checking on" || \
  no "witness checking silenced an unnamed head (exit $rc): $out"

# A spent entry is NAMED but must not fail, and must not be named while
# it is still live -- the two halves are a guard with a direction.
out=$(WITNESS="$WIT_OK" run_rec "$GOOD_REC" "$OTHER_PR"); rc=$?
case "$out" in
  *"is spent"*) ok "an entry for no open PR head is reported as spent" ;;
  *) no "a spent entry was not named: $out" ;;
esac

out=$(WITNESS="$WIT_OK" run_rec "$GOOD_REC" "$REC_PR"); rc=$?
if [ "$rc" = 0 ] && [ "${out#*is spent}" = "$out" ]; then
    ok "a live entry is NOT called spent, and reporting one does not fail the gate"
else
    no "a live entry was called spent, or the note changed the verdict (exit $rc): $out"
fi

# --- the branch-head arm names BOTH causes (#874) ----------------------
#
# The PR-head arm was taught the deletion cause when #837's purge deleted
# an open PR head's runs. The branch-head arm was not, and it reconciles a
# SECOND population the purge could also empty -- so a maintainer hitting
# that red found only the merge burst named, and no mention that deletion
# was possible at all.
#
# SCOPED TO THE BRANCH ARM, deliberately. The words "purge" and "deleted"
# appear all over the PR-head arm above, so an unscoped check for either
# is satisfied by a message whose branch arm still names one cause -- the
# exact defect the #874 review found by reading, and the same shape as the
# mechanism-keying lesson recorded further up this file. Cut the message
# at the branch arm first, then assert inside it.
out=$(run_branch "$OLD_COMMIT" ""); rc=$?
branch_arm=$(printf '%s\n' "$out" | sed -n '/On a branch head/,$p')
[ -n "$branch_arm" ] \
  && ok "the failure has a branch-head arm to assert on" \
  || no "no branch-head arm found in the message: $out"
case "$branch_arm" in
  *"THE RUNS WERE DELETED"*) ok "the branch-head arm names deletion as a second cause" ;;
  *) no "the branch-head arm names only the merge burst: $branch_arm" ;;
esac
case "$branch_arm" in
  *"Keep rule 5"*) ok "the branch-head arm names the keep rule that makes deletion impossible" ;;
  *) no "the branch-head arm does not name keep rule 5: $branch_arm" ;;
esac
case "$branch_arm" in
  *"gate-branch-scope.env"*) ok "the branch-head arm names the shared scope both gates read" ;;
  *) no "the branch-head arm does not name the shared scope: $branch_arm" ;;
esac
# The merge-burst cause must SURVIVE, not be replaced. A message that
# swapped one single cause for another single cause would pass every
# assertion above.
case "$branch_arm" in
  *"merge burst"*) ok "the merge-burst cause survives alongside the new one" ;;
  *) no "the branch-head arm lost the merge-burst cause: $branch_arm" ;;
esac

# --- the branch scope is READ, not built in (#874) ---------------------
#
# check-missing-runs.sh READS the run records of a population and
# purge-workflow-runs.sh SPARES it. They can only agree if both take the
# population from one file, so this gate must REFUSE when it cannot read
# that file rather than falling back to a private default -- a default
# would let the two disagree while both looked healthy, which is the whole
# failure being closed.
run_scope() {   # run_scope <scope-file-path> [gh-call-log]
    local dir; dir=$(mktemp -d)
    make_branch_gh "$dir" "$OLD_COMMIT" "completed:success"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_SCOPE_FILE="$1" GHLOG="${2:-}" \
        bash "$CHECK" 20 >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

# ASSERT WHICH REFUSAL FIRED, not merely that one did. Measured
# 2026-08-28: replacing the `-r` test with `if false` -- so the gate
# sources an unreadable file and falls through -- SURVIVED this case,
# because the completeness check below then exits 2 with a different
# message. Exit 2 was preserved; the DIAGNOSIS was not, and the diagnosis
# is the whole product of a gate that fires on a schedule at 03:00.
# test-purge-workflow-runs.sh already asserted its twin this way; this
# side did not, and the two were written together.
out=$(run_scope "/nonexistent/gate-branch-scope.env"); rc=$?
[ "$rc" = 2 ] && grep -q "cannot read the branch scope" <<<"$out" \
  && ok "an unreadable scope file exits 2 rather than judging on a private default" \
  || no "missing scope returned $rc (want 2, naming the unreadable file): $out"

SCOPETMP=$(mktemp -d)
printf 'GATE_SCOPE_BRANCHES="dev main"\n' > "$SCOPETMP/half.env"
out=$(run_scope "$SCOPETMP/half.env"); rc=$?
[ "$rc" = 2 ] && ok "a scope file defining only half the scope exits 2" \
  || no "half scope returned $rc (want 2): $out"

# AN EXPORTED VALUE MAY NOT STAND IN FOR THE FILE. The file is sourced, so
# anything the environment already exports is still set when the
# completeness check runs, and a half scope file would be completed by it --
# this gate then judging a population the file never named. Nothing exports
# these today, which is precisely why it is asserted.
dir=$(mktemp -d); make_branch_gh "$dir" "$OLD_COMMIT" "completed:success"
out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_SCOPE_FILE="$SCOPETMP/half.env" \
      GATE_SCOPE_COMMITS=15 bash "$CHECK" 20 2>&1); rc=$?
rm -rf "$dir"
[ "$rc" = 2 ] && grep -q "does not define" <<<"$out" \
  && ok "an exported GATE_SCOPE_COMMITS does not complete a half scope file" \
  || no "the environment completed a half scope file (exit $rc): $out"

# EMPTINESS IS AN ENVIRONMENT SEAM, NEVER A FILE VALUE (#874). Both halves
# are asserted, because a guard fails in one direction.
#
# The seam still works: an empty GATE_BRANCHES in the ENVIRONMENT skips the
# branch phase, which is what the self-tests drive and what keeps this file
# from being the only way to isolate the PR phase.
dir=$(mktemp -d); make_branch_gh "$dir" "$OLD_COMMIT" "completed:success"
out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES='' bash "$CHECK" 20 2>&1); rc=$?
rm -rf "$dir"
case "$out" in
  *"branch commit(s) on [none]"*) ok "an empty GATE_BRANCHES in the ENVIRONMENT still skips the branch phase" ;;
  *) no "the environment seam no longer skips the branch phase (exit $rc): $out" ;;
esac

# And the FILE may not carry it. Measured 2026-08-28 on the code as it stood:
# an empty branch list in the scope file turned the branch phase off on this
# gate AND on the purge -- this gate reconciling `[none]`, the purge printing
# "rule DISABLED" and deleting the commits it was sparing -- with both suites
# fully green. A universal gate satisfied by emptying its domain.
printf 'GATE_SCOPE_BRANCHES=""\nGATE_SCOPE_COMMITS=15\n' > "$SCOPETMP/none.env"
out=$(run_scope "$SCOPETMP/none.env"); rc=$?
[ "$rc" = 2 ] && grep -q "no words in it" <<<"$out" \
  && ok "a scope FILE naming no branch exits 2 rather than judging nothing" \
  || no "an empty scope branch list in the file was accepted or refused elsewhere (exit $rc): $out"

# WHITESPACE IS THE SHAPE `-n` CANNOT SEE, and it is the one that got past
# the first version of this guard: "   " is non-empty to every presence test
# and yields zero words to the loop that consumes it.
printf 'GATE_SCOPE_BRANCHES="   "\nGATE_SCOPE_COMMITS=15\n' > "$SCOPETMP/blank.env"
out=$(run_scope "$SCOPETMP/blank.env"); rc=$?
[ "$rc" = 2 ] && grep -q "no words in it" <<<"$out" \
  && ok "a scope FILE whose branch list is only whitespace exits 2 (presence is not the property)" \
  || no "a whitespace-only scope branch list was accepted or refused elsewhere (exit $rc): $out"

# A TAB IS NOT A SPACE, and driving it rather than assuming showed that it
# never reaches the word count at all: a literal tab is outside the
# foreign-content character class, so THAT guard catches it one step earlier.
# Recorded as the guard that actually fires, because a shape credited to the
# wrong check is a kill scored for a test that never ran.
printf 'GATE_SCOPE_BRANCHES="\t"\nGATE_SCOPE_COMMITS=15\n' > "$SCOPETMP/tab.env"
out=$(run_scope "$SCOPETMP/tab.env"); rc=$?
[ "$rc" = 2 ] && grep -q "neither a comment nor a plain" <<<"$out" \
  && ok "a scope FILE whose branch list is a single tab exits 2 (as foreign content, one guard earlier)" \
  || no "a tab-only scope branch list was accepted or refused elsewhere (exit $rc): $out"

# THE OTHER DIRECTION: one branch is still one branch. A word count that
# refused a legal single-branch scope would be the refusal-only guard this
# rule exists to prevent.
printf 'GATE_SCOPE_BRANCHES="dev"\nGATE_SCOPE_COMMITS=15\n' > "$SCOPETMP/one.env"
out=$(run_scope "$SCOPETMP/one.env"); rc=$?
[ "$rc" != 2 ] && ok "a scope naming exactly one branch is still ACCEPTED" \
  || no "the word count refused a legal single-branch scope: $out"

# --- the depth is a COUNT, not a string (#874) -------------------------
# It goes onto the query as per_page. `0` reconciles nothing, `abc` is not a
# number, and `15` written with a CRLF ending carries a character the API
# does not want -- all three read as configuration and none is one.
printf 'GATE_SCOPE_BRANCHES="dev"\nGATE_SCOPE_COMMITS=0\n' > "$SCOPETMP/zero.env"
out=$(run_scope "$SCOPETMP/zero.env"); rc=$?
[ "$rc" = 2 ] && grep -q "not a positive integer" <<<"$out" \
  && ok "a scope depth of 0 exits 2 (a page of nothing is not a scope)" \
  || no "a zero depth was accepted or refused elsewhere (exit $rc): $out"
printf 'GATE_SCOPE_BRANCHES="dev"\nGATE_SCOPE_COMMITS=abc\n' > "$SCOPETMP/abc.env"
out=$(run_scope "$SCOPETMP/abc.env"); rc=$?
[ "$rc" = 2 ] && grep -q "not a positive integer" <<<"$out" \
  && ok "a non-numeric scope depth exits 2" \
  || no "a non-numeric depth was accepted or refused elsewhere (exit $rc): $out"
printf 'GATE_SCOPE_BRANCHES="dev"\r\nGATE_SCOPE_COMMITS=15\r\n' > "$SCOPETMP/crlf.env"
out=$(run_scope "$SCOPETMP/crlf.env"); rc=$?
[ "$rc" = 2 ] && grep -q "CRLF line endings" <<<"$out" \
  && ok "a CRLF scope file exits 2 rather than querying a branch named dev<CR>" \
  || no "a CRLF scope file was accepted or refused elsewhere (exit $rc): $out"

# --- a duplicated key is a second enumeration inside the file (#874) ----
# Every line below is individually legal, so the foreign-content guard
# passes it and the shell takes the last one. Measured 2026-08-28 against
# the code as it stood: this narrowed both gates from 15 commits to 1 with
# both suites green. Asserted by EXIT CODE.
printf 'GATE_SCOPE_BRANCHES="dev"\nGATE_SCOPE_COMMITS=15\nGATE_SCOPE_COMMITS=1\n' > "$SCOPETMP/dupc.env"
out=$(run_scope "$SCOPETMP/dupc.env"); rc=$?
[ "$rc" = 2 ] && grep -q "more than once" <<<"$out" \
  && ok "a scope file assigning GATE_SCOPE_COMMITS twice exits 2" \
  || no "a duplicated depth key was accepted or refused elsewhere (exit $rc): $out"
printf 'GATE_SCOPE_BRANCHES="dev"\nGATE_SCOPE_BRANCHES=""\nGATE_SCOPE_COMMITS=15\n' > "$SCOPETMP/dupb.env"
out=$(run_scope "$SCOPETMP/dupb.env"); rc=$?
[ "$rc" = 2 ] && grep -q "more than once" <<<"$out" \
  && ok "a scope file assigning GATE_SCOPE_BRANCHES twice exits 2" \
  || no "a duplicated branch key was accepted or refused elsewhere (exit $rc): $out"

# --- THE SEAM, where the first version of this fix stopped (#874) ------
# Every case above judges the FILE. What reaches `for br in $BRANCHES` is
# the value after the GATE_BRANCHES override, and it was tested with `-n` --
# the presence test this whole change replaces, left standing one seam over.
# MEASURED at 83c27b1: GATE_BRANCHES="   " exited 0 reporting
# "0 branch commit(s) on [   ], all have an executed integration.yml run".
#
# Emptiness through the seam is the documented disable, so a blank list is
# normalised onto that same path rather than refused. Both directions are
# asserted: the blank list disables, and a real list still reconciles.
seam_branch() {   # seam_branch <label> <gate-branches-value>
    local dir; dir=$(mktemp -d)
    make_branch_gh "$dir" "$OLD_COMMIT" "completed:success"
    local out rc
    out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES="$2" bash "$CHECK" 20 2>&1); rc=$?
    rm -rf "$dir"
    [ "$rc" = 0 ] && grep -qF "branch commit(s) on [none]" <<<"$out" \
      && ok "GATE_BRANCHES $1 takes the documented disable path ([none])" \
      || no "GATE_BRANCHES $1 did not take the disable path (exit $rc): $out"
}
SEAM_NL=$'\n'; SEAM_TAB=$'\t'
seam_branch "set to spaces only" "   "
seam_branch "set to a tab only" "$SEAM_TAB"
seam_branch "set to a newline only" "$SEAM_NL"
seam_branch "set to a space, a tab and a newline" " $SEAM_TAB$SEAM_NL"
# THE OTHER DIRECTION: a real list through the seam still reconciles, or the
# normalisation is a disable-only guard that switched the branch phase off.
dir=$(mktemp -d); make_branch_gh "$dir" "$OLD_COMMIT" "completed:success"
out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES="dev" bash "$CHECK" 20 2>&1); rc=$?
rm -rf "$dir"
[ "$rc" = 0 ] && grep -qF "branch commit(s) on [dev]" <<<"$out" \
  && ok "a real branch list through the GATE_BRANCHES seam still reconciles" \
  || no "the seam normalisation disarmed a legitimate override (exit $rc): $out"

# THE DEPTH SEAM. The file-side message said a bad depth "would protect
# nothing"; MEASURED 2026-08-28 against the live API that reason was wrong
# in the direction that matters -- GitHub CLAMPS an unusable per_page to its
# own default instead of erroring (0, abc and -1 each returned 30 commits;
# 999 returned 100). A degenerate depth therefore reconciles a DIFFERENT
# population, silently, and if only one of the two scripts carries the
# override they stop reading one list.
seam_depth() {   # seam_depth <label> <gate-branch-commits-value>
    local dir; dir=$(mktemp -d)
    make_branch_gh "$dir" "$OLD_COMMIT" "completed:success"
    local out rc
    out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES=dev GATE_BRANCH_COMMITS="$2" \
          bash "$CHECK" 20 2>&1); rc=$?
    rm -rf "$dir"
    [ "$rc" = 2 ] && grep -qF "the effective GATE_BRANCH_COMMITS is" <<<"$out" \
      && ok "a GATE_BRANCH_COMMITS override that $1 exits 2" \
      || no "a GATE_BRANCH_COMMITS override that $1 was accepted or refused elsewhere (exit $rc): $out"
}
seam_depth "is zero" "0"
seam_depth "is not a number" "abc"
seam_depth "is negative" "-1"
seam_depth "is blank" " "
seam_depth "carries a stray character" "15x"
# THE OTHER DIRECTION, asserted ON THE WIRE: a legal override is honoured and
# is the number actually queried. Exit code alone would pass against a gate
# that accepted the value and then ignored it.
dir=$(mktemp -d); make_branch_gh "$dir" "$OLD_COMMIT" "completed:success"
PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES=dev GATE_BRANCH_COMMITS=7 \
    GHLOG="$dir/calls" bash "$CHECK" 20 >/dev/null 2>&1; rc=$?
seam_wire=$(grep -cF 'per_page=7' "$dir/calls" 2>/dev/null || true)
rm -rf "$dir"
[ "$rc" = 0 ] && [ "${seam_wire:-0}" -ge 1 ] \
  && ok "a legal GATE_BRANCH_COMMITS override is accepted and is the depth actually queried" \
  || no "a legal depth override did not reach the wire (exit $rc, per_page=7 seen ${seam_wire:-0} time(s))"

# THE FOURTH SEAM, enumerated and MEASURED rather than guarded. Two
# spellings enumerated means a third exists, and a third means a fourth:
# GATE_WORKFLOW also overrides from the environment and passes through no
# check. It is NOT the same defect and it is deliberately left alone.
# MEASURED 2026-08-28: pointed at a workflow with no runs, this gate FLAGS
# the commit and exits 1 -- loud, not quiet -- and it is detector-only, so
# it cannot make the two gates read different populations, which is the
# property the scope file exists to hold. Recorded as a case so the
# direction is asserted rather than asserted about.
dir=$(mktemp -d); make_branch_gh "$dir" "$OLD_COMMIT" ""
out=$(PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_BRANCHES=dev GATE_WORKFLOW=nonexistent.yml \
      bash "$CHECK" 20 2>&1); rc=$?
rm -rf "$dir"
[ "$rc" = 1 ] && grep -qF "no nonexistent.yml run at all" <<<"$out" \
  && ok "GATE_WORKFLOW through the environment seam fails LOUD (the commit is flagged), so it is not the branch-list defect" \
  || no "the GATE_WORKFLOW seam changed direction (exit $rc): $out"

# --- the scope path being a DIRECTORY names the right refusal ----------
# `-r` alone is true of a directory. Exit 2 was already preserved by the
# completeness check below it; the DIAGNOSIS was not, and the diagnosis is
# the entire product of a gate that runs unattended.
out=$(run_scope "$SCOPETMP"); rc=$?
[ "$rc" = 2 ] && grep -q "cannot read the branch scope" <<<"$out" \
  && ok "a scope path that is a directory refuses as unreadable, not as incomplete" \
  || no "a directory scope path gave the wrong diagnosis (exit $rc): $out"
# THE DEPTH IN THE FILE MUST BE THE DEPTH ON THE WIRE. Existence and
# completeness are not the property. A gate that read the file, ignored it
# and used a built-in default would pass every assertion above while
# reconciling a different population from the one the purge spares --
# which is the silent disagreement this whole design removes. So drive an
# unusual depth and read it back off the wire.
# test-purge-workflow-runs.sh asserts the same property on the other gate;
# together they are what makes the two populations one population.
printf 'GATE_SCOPE_BRANCHES="dev"\nGATE_SCOPE_COMMITS=3\n' > "$SCOPETMP/depth.env"
: > "$SCOPETMP/calls.log"
out=$(run_scope "$SCOPETMP/depth.env" "$SCOPETMP/calls.log"); rc=$?
depth_hits=$(grep -c 'commits?sha=dev&per_page=3' "$SCOPETMP/calls.log") || depth_hits=0
[ "$depth_hits" -ge 1 ] \
  && ok "the gate queries the depth the scope file names (per_page=3, not a built-in default)" \
  || no "the gate ignored the scope file's depth; commits calls were: $(grep commits "$SCOPETMP/calls.log")"

# The other half: the branch LIST, not just the depth. A branch this gate
# does not walk is a branch it does not reconcile, and the purge's keep
# rule 5 would then spare commits nothing asks about while the branch that
# matters goes unwatched. Named so nothing could guess them.
printf 'GATE_SCOPE_BRANCHES="alpha beta"\nGATE_SCOPE_COMMITS=15\n' > "$SCOPETMP/names.env"
: > "$SCOPETMP/names.log"
out=$(run_scope "$SCOPETMP/names.env" "$SCOPETMP/names.log"); rc=$?
n_alpha=$(grep -c 'commits?sha=alpha&' "$SCOPETMP/names.log") || n_alpha=0
n_beta=$(grep -c 'commits?sha=beta&' "$SCOPETMP/names.log")   || n_beta=0
[ "$n_alpha" -ge 1 ] && [ "$n_beta" -ge 1 ] \
  && ok "every branch the scope file names is reconciled (alpha and beta both on the wire)" \
  || no "the gate did not walk the scope's branch list (alpha=$n_alpha beta=$n_beta): $(grep commits "$SCOPETMP/names.log")"
rm -rf "$SCOPETMP"

# The shipped scope has to exist where the default points, or this gate
# refuses in production. Asserted against the real artifact, because every
# case above supplies its own file and would pass with the shipped one
# absent.
SHIPPED_SCOPE="$HERE/../.github/gate-branch-scope.env"
[ -r "$SHIPPED_SCOPE" ] \
  && ok "the shipped scope file exists at the path this gate defaults to" \
  || no "no scope file at $SHIPPED_SCOPE -- this gate would exit 2 in production"

# AND IT NAMES AT LEAST ONE BRANCH. The case above proves an empty list
# reaches the branch phase, which is the seam working; this proves the
# SHIPPED file is not using it. Measured 2026-08-28: with
# `GATE_SCOPE_BRANCHES=""` committed to that file, this gate reconciles
# `[none]`, the purge prints "rule DISABLED", and both suites stayed
# fully green -- the branch half of this design silently absent with
# nothing red anywhere. Existence was never the property.
if [ -r "$SHIPPED_SCOPE" ]; then
    # COUNT WORDS, do not test presence. `-n` is one character to the side of
    # the property and passes on "   ", which yields zero branches to the
    # loop that consumes it -- measured 2026-08-28, with both suites green.
    ( set -u
      # shellcheck disable=SC1090
      . "$SHIPPED_SCOPE"
      # shellcheck disable=SC2086
      [ "$(set -- ${GATE_SCOPE_BRANCHES:-}; echo $#)" -ge 1 ] \
        && [ -n "${GATE_SCOPE_COMMITS:-}" ] ) \
      && ok "the shipped scope names at least one branch (counted as words) and a depth" \
      || no "the shipped scope names no branch or no depth -- the branch phase is silently off"
fi

# --- the scope file cannot smuggle this gate's configuration -----------
# The file is SOURCED, so any other line in it runs as this gate's own
# configuration. Measured 2026-08-28 against the code as it stood: a line
# reading `GATE_WORKFLOW=nonexistent.yml` redirected the branch phase at a
# workflow that does not exist and the gate still exited 0 reporting every
# commit covered -- a detector reporting health having asked about
# nothing. The same seam in purge-workflow-runs.sh turns a dry run into
# real deletions.
#
# Driven by EXIT CODE on each shape alone, and paired with the opposite
# direction below, because a guard fails in one direction.
SMUG=$(mktemp -d)
smuggle_scope() {   # smuggle_scope <label> <content>
    printf '%s' "$2" > "$SMUG/s.env"
    run_scope "$SMUG/s.env" >/dev/null 2>&1
    [ "$?" = 2 ] \
      && ok "a scope file that $1 exits 2" \
      || no "a scope file that $1 was accepted"
}
smuggle_scope "redirects GATE_WORKFLOW" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
GATE_WORKFLOW=nonexistent.yml
'
smuggle_scope "runs a bare command" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
echo smuggled
'
smuggle_scope "hides a second assignment behind a semicolon" 'GATE_SCOPE_BRANCHES="dev"; GATE_WORKFLOW=x.yml
GATE_SCOPE_COMMITS=15
'
smuggle_scope "substitutes a command into the value" 'GATE_SCOPE_BRANCHES="$(echo dev)"
GATE_SCOPE_COMMITS=15
'
printf '# a comment, then a blank line\n\n  GATE_SCOPE_BRANCHES="dev release/v1.9"\nGATE_SCOPE_COMMITS=15\n' > "$SMUG/legal.env"
out=$(run_scope "$SMUG/legal.env"); rc=$?
[ "$rc" != 2 ] \
  && ok "a legal scope with comments, blanks, indentation and a slashed branch is ACCEPTED" \
  || no "the foreign-content guard refused a legal scope file: $out"
rm -rf "$SMUG"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
