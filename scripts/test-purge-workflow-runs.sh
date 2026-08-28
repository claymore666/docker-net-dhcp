#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for scripts/purge-workflow-runs.sh.
#
# THE STUB IS `gh` ON PATH, NOT A FUNCTION SEAM INSIDE THE SCRIPT. That is
# deliberate and it is the whole reason this file is shaped the way it is.
# A seam one level up would leave the filtering, the jq expressions and the
# delete-call construction ungraded while these cases scored green against a
# stand-in -- the defect this repo found in check-attestation-parity (#827),
# where the block holding the gate's entire safety property had never once
# executed because every self-test invocation replaced it.
#
# So the stub answers HTTP-shaped requests with fixture JSON and runs the
# script's OWN `--jq` expressions through jq. Everything below `api()` in
# the shipped script runs for real.
#
# EVERY CASE CARRIES A WITNESS. A stub that is never invoked produces the
# same exit code as a stub that works, while the real `gh` quietly talks to
# GitHub. The call counter is what tells those apart, and without it three
# of these cases pass with the stub inert.

set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
GATE="$HERE/purge-workflow-runs.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
ok()  { echo "  ok   - $1"; pass=$((pass+1)); }
no()  { echo "  FAIL - $1"; fail=$((fail+1)); }

command -v jq >/dev/null || { echo "jq is required for this self-test"; exit 2; }

# ---------------------------------------------------------------- the stub
mkdir -p "$TMP/bin"
cat > "$TMP/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Fixture-backed `gh`. Records every call; applies the caller's own --jq.
echo "$*" >> "$FIXDIR/calls.log"
jqexpr=""; delete=0; path=""
while [ $# -gt 0 ]; do
    case "$1" in
        api) ;;
        --jq) shift; jqexpr="$1" ;;
        --paginate|--silent) ;;
        -X) shift; [ "$1" = "DELETE" ] && delete=1 ;;
        repos/*) path="$1" ;;
        *) ;;
    esac
    shift
done
if [ "$delete" = 1 ]; then
    echo "$path" >> "$FIXDIR/deleted.log"
    exit 0
fi
case "$path" in
    */actions/workflows)        src="$FIXDIR/workflows.json" ;;
    */actions/workflows/*/runs) src="$FIXDIR/wfruns.json" ;;
    */actions/runs)             src="$FIXDIR/runs.json" ;;
    */pulls*)                   src="$FIXDIR/pulls.json" ;;
    */commits*)                 src="$FIXDIR/commits.json" ;;
    *)                          echo "stub: unexpected path $path" >&2; exit 1 ;;
esac
[ -f "$src" ] || { echo "stub: missing fixture $src" >&2; exit 1; }
if [ -n "$jqexpr" ]; then jq -r "$jqexpr" < "$src"; else cat "$src"; fi
STUB
chmod +x "$TMP/bin/gh"

# ------------------------------------------------------------- the fixtures
# NOW is fixed so the window arithmetic is deterministic.
NOW=1788000000                      # a fixed instant
day() { date -u -d "@$(( NOW - $1 * 86400 ))" +%Y-%m-%dT%H:%M:%SZ; }

mkfix() {   # mkfix <dir>
    local d="$1"; mkdir -p "$d"
    cat > "$d/workflows.json" <<JSON
{"workflows":[
 {"id":1,"path":".github/workflows/release.yml","name":"Release"},
 {"id":2,"path":".github/workflows/test.yaml","name":"Test"}
]}
JSON
    # The provenance run carries the RAW PATH as its display name -- the
    # exact shape that defeated a name-keyed rule and put eight real
    # release runs in a delete set.
    cat > "$d/wfruns.json" <<JSON
{"workflow_runs":[{"id":9001},{"id":9002}]}
JSON
    # No open pull requests unless a case says otherwise. This is the
    # LEGITIMATE empty: a repository may genuinely have none, and it must
    # not be confused with the two illegitimate ones (a failed query, a
    # renamed field) that the cases below drive separately.
    echo '[]' > "$d/pulls.json"
    # Keep rule 5's scope, hermetic: the fixture's own file, never the
    # repository's. The default commits below are deliberately SHAs that
    # appear in no run, so rule 5 is armed and exercised in every case
    # while protecting nothing -- the cases that want it to protect
    # something overwrite commits.json with a SHA the run fixture uses.
    cat > "$d/scope.env" <<'SCOPE'
GATE_SCOPE_BRANCHES="dev main"
GATE_SCOPE_COMMITS=15
SCOPE
    cat > "$d/commits.json" <<JSON
[{"sha":"$(printf '1%.0s' $(seq 40))"},{"sha":"$(printf '2%.0s' $(seq 40))"}]
JSON
}

runs_json() { python3 - "$@" <<'PY'
import json,sys,time
now=int(sys.argv[1]); groups=int(sys.argv[2]); age0=int(sys.argv[3])
def ts(d): return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now-d*86400))
runs=[]
rid=1000
# `groups` code-event groups, each with 3 workflows, one per day going back
for g in range(groups):
    sha="%040x"%(0xabc000+g)
    for w in ("Test","Integration","CodeQL"):
        rid+=1
        runs.append({"id":rid,"head_sha":sha,"created_at":ts(age0+g),
                     "event":"push","status":"completed","name":w})
# two provenance runs, ancient, display name = raw path
for pid in (9001,9002):
    runs.append({"id":pid,"head_sha":"f"*40,"created_at":ts(400),
                 "event":"push","status":"completed",
                 "name":".github/workflows/release.yml"})
# one run still in flight, ancient
runs.append({"id":7777,"head_sha":"e"*40,"created_at":ts(300),
             "event":"push","status":"in_progress","name":"Test"})
print(json.dumps({"workflow_runs":runs}))
PY
}

drive() {  # drive <fixdir> <extra env...>  -> sets OUT, RC
    local d="$1"; shift
    : > "$d/calls.log"; : > "$d/deleted.log"
    OUT=$(FIXDIR="$d" PATH="$TMP/bin:$PATH" REPO=fixture/repo NOW_EPOCH="$NOW" \
          GATE_SCOPE_FILE="$d/scope.env" \
          "$@" bash "$GATE" 2>&1); RC=$?
    CALLS=$(wc -l < "$d/calls.log"); DELS=$(wc -l < "$d/deleted.log")
}

echo "== purge-workflow-runs =="

# --- 1. non-vacuity: an empty run listing must refuse, not report success
D="$TMP/empty"; mkfix "$D"; echo '{"workflow_runs":[]}' > "$D/runs.json"
drive "$D" env DRY_RUN=0
[ "$RC" = 2 ] && grep -q "Nothing to inspect" <<<"$OUT" \
  && ok "an empty run listing exits 2 rather than reporting a clean sweep" \
  || no "empty listing returned $RC: $OUT"
[ "$CALLS" -gt 0 ] && ok "witness: the stub was actually invoked ($CALLS calls)" \
  || no "witness: stub never called -- this case proves nothing"

# --- 2. the default is a dry run
D="$TMP/dry"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
drive "$D"
[ "$RC" = 0 ] && [ "$DELS" = 0 ] && grep -q "DRY RUN" <<<"$OUT" \
  && ok "DRY_RUN defaults to on and deletes nothing" \
  || no "default run deleted $DELS run(s), rc=$RC"

# --- 3. THE GROUP FLOOR AT A ZERO-DAY WINDOW ---------------------------
# Everything is older than the window, so the ONLY thing standing between
# the repo and total erasure is the group floor. 20 groups x 3 workflows,
# all ancient. Exactly the last 10 groups -- 30 runs -- must survive.
D="$TMP/floor"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=10 DRY_RUN=0
# 20 groups*3 = 60 runs; keep last 10 groups = 30; keep 2 provenance; keep 1 in-flight
[ "$DELS" = 30 ] \
  && ok "0-day window: 30 of 60 group runs deleted, the last 10 groups survive" \
  || no "0-day window deleted $DELS (expected 30) -- THE FLOOR DID NOT HOLD: $OUT"
# and the survivors must be COMPLETE groups, not a shredded mix
kept_shas=$(python3 - "$D" <<'PY'
import json,sys,os
d=sys.argv[1]
deleted={l.strip().rsplit("/",1)[1] for l in open(os.path.join(d,"deleted.log")) if l.strip()}
runs=json.load(open(os.path.join(d,"runs.json")))["workflow_runs"]
from collections import defaultdict
kept=defaultdict(int)
for r in runs:
    if r["status"]!="completed": continue
    if str(r["id"]) in deleted: continue
    if r["name"].startswith(".github/"): continue
    kept[r["head_sha"]]+=1
print(len(kept), sorted(set(kept.values())))
PY
)
[ "$kept_shas" = "10 [3]" ] \
  && ok "the survivors are 10 COMPLETE groups of 3 (not a shredded per-workflow mix)" \
  || no "survivor shape was '$kept_shas', expected '10 [3]'"

# --- 4. provenance is keyed on PATH, not on display name ----------------
# Both provenance runs carry ".github/workflows/release.yml" as their NAME.
# A name-keyed rule protects them by accident; a path-keyed one protects
# them on purpose. Drive it: they must never appear in deleted.log.
grep -qE '(^|/)900[12]$' "$D/deleted.log" \
  && no "a provenance run was deleted -- the path keying failed" \
  || ok "provenance runs survive a 0-day window (keyed on workflow path)"

# --- 5. a run still in flight is never deleted --------------------------
grep -qE '(^|/)7777$' "$D/deleted.log" \
  && no "an in_progress run was deleted" \
  || ok "an in_progress run is never deleted, however old"

# --- 6. the floor is group-shaped, not per-workflow ---------------------
# With KEEP_GROUPS=1 exactly one group (3 runs) survives.
D="$TMP/one"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$DELS" = 57 ] \
  && ok "KEEP_GROUPS=1 keeps one whole group of 3, deletes the other 57" \
  || no "KEEP_GROUPS=1 deleted $DELS (expected 57)"

# --- 7. the window alone keeps recent runs ------------------------------
D="$TMP/win"; mkfix "$D"; runs_json "$NOW" 20 0 > "$D/runs.json"
drive "$D" env RETENTION_DAYS=30 KEEP_GROUPS=0 DRY_RUN=0
[ "$DELS" = 0 ] \
  && ok "a 30-day window keeps 20 groups spread over 20 days" \
  || no "a 30-day window still deleted $DELS run(s)"

# --- 8. KEEP RULE 4: an open PR head survives, however old --------------
# THE REGRESSION THIS RULE EXISTS FOR. On 2026-08-27 this purge deleted the
# runs of PR #221's head -- a draft parked since June, so far outside both
# the window and the group floor -- and check-missing-runs.sh (#740) then
# reported that head as never tested, on a schedule, on main. Nothing
# recovers the answer afterwards: the one surviving check-run on that head
# belongs to `github-advanced-security`, so even the Checks API answers
# zero once it is filtered to Actions.
#
# So: the OLDEST group, with a 0-day window and a floor of one, which
# without this rule loses all three of its runs.
PRSHA=$(python3 -c 'print("%040x" % (0xabc000 + 19))')
D="$TMP/openpr"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
cat > "$D/pulls.json" <<JSON
[{"number":221,"head":{"sha":"$PRSHA","ref":"feature/218-stable-mac"}}]
JSON
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
# 60 group runs; 3 kept by the floor, 3 kept by this rule => 54 deleted.
[ "$DELS" = 54 ] \
  && ok "an open PR head survives a 0-day window and a floor of one (54 deleted, not 57)" \
  || no "open PR head not protected: deleted $DELS (expected 54) -- KEEP RULE 4 DID NOT HOLD"
kept_pr=$(python3 - "$D" "$PRSHA" <<'PYCASE8'
import json,sys,os
d,sha=sys.argv[1],sys.argv[2]
deleted={l.strip().rsplit("/",1)[1] for l in open(os.path.join(d,"deleted.log")) if l.strip()}
runs=json.load(open(os.path.join(d,"runs.json")))["workflow_runs"]
print(sum(1 for r in runs if r["head_sha"]==sha and str(r["id"]) not in deleted))
PYCASE8
)
[ "$kept_pr" = "3" ] \
  && ok "all 3 of that head's runs survive, not a shredded subset" \
  || no "only $kept_pr of the open PR head's 3 runs survived"
grep -q "pulls" "$D/calls.log" \
  && ok "witness: the open-PR query was actually made" \
  || no "witness: no pulls call -- case 8 proves nothing"

# --- 9. the rule can be turned off, and that isolates it ----------------
# The seam exists so this suite can prove the rule is what saved those
# three runs, rather than some other clause happening to spare them.
D="$TMP/off"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
cat > "$D/pulls.json" <<JSON
[{"number":221,"head":{"sha":"$PRSHA","ref":"feature/218-stable-mac"}}]
JSON
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0 KEEP_OPEN_PR_HEADS=0
[ "$DELS" = 57 ] \
  && ok "with the rule off the same fixture deletes 57 -- the 3 were saved BY the rule" \
  || no "rule disabled deleted $DELS (expected 57): the control does not isolate the rule"

# --- 10. a failed open-PR query refuses rather than deleting ------------
# The dangerous direction. An unanswerable query yields an empty keep set,
# which silently disarms the rule and deletes exactly what it protects --
# while printing "0 head(s) protected" as though that were good news.
D="$TMP/prfail"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
rm -f "$D/pulls.json"          # the stub fails on a missing fixture
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "Cannot list open pull requests" <<<"$OUT" \
  && ok "a failed open-PR query exits 2 and deletes nothing" \
  || no "failed open-PR query: rc=$RC, deleted=$DELS (want rc 2, 0 deleted)"

# --- 11. PRs listed but no head SHA is a shape change, not an empty set --
# The subtler half of the same failure: the query works, the field moved.
# Counting SHAs alone cannot tell that from "no open PRs", so the row count
# is what separates them.
D="$TMP/prshape"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
echo '[{"number":221,"head":{"ref":"feature/218-stable-mac"}}]' > "$D/pulls.json"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "shape changed" <<<"$OUT" \
  && ok "open PRs that yield no head SHA refuse, naming the shape change" \
  || no "shape change: rc=$RC, deleted=$DELS (want rc 2, 0 deleted)"

# --- 12. genuinely no open PRs is NOT a refusal -------------------------
# The legitimate empty has to keep working, or the two refusals above turn
# into a purge that can never run on a quiet repository.
D="$TMP/prnone"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 0 ] && [ "$DELS" = 57 ] && grep -q "0 head(s) protected" <<<"$OUT" \
  && ok "a repository with no open PRs purges normally (57 deleted, no refusal)" \
  || no "empty open-PR list: rc=$RC, deleted=$DELS (want rc 0, 57 deleted)"

# --- 13. KEEP RULE 5: a gate branch commit survives, however old --------
# THE COLLISION RULE 4 DID NOT CLOSE (#874). check-missing-runs.sh
# reconciles TWO populations. Rule 4 covers the first (open PR heads); the
# second is the last N commits of each gate branch, and nothing protected
# those.
#
# Measured 2026-08-28 against the live listing: the keep-10 group set
# spanned twenty-one MINUTES, so all 15 of `dev`'s reconciled commits and
# 14 of `main`'s were outside it, leaving the 7-day window as the only
# thing holding a branch commit. `main` moves at releases; its tip of
# 2026-08-23 crossed 7 days on 2026-08-30 with nothing else holding it,
# and the reachability walk cannot save a TIP because a tip has no tested
# descendant.
#
# So: the OLDEST group, a 0-day window and a floor of one -- which without
# this rule loses all three of its runs, exactly as the open PR head did.
BRSHA=$(python3 -c 'print("%040x" % (0xabc000 + 19))')
D="$TMP/branch"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
echo "[{\"sha\":\"$BRSHA\"}]" > "$D/commits.json"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$DELS" = 54 ] \
  && ok "a gate branch commit survives a 0-day window and a floor of one (54 deleted, not 57)" \
  || no "gate branch commit not protected: deleted $DELS (expected 54) -- KEEP RULE 5 DID NOT HOLD"
kept_br=$(python3 - "$D" "$BRSHA" <<'PYCASE13'
import json,sys,os
d,sha=sys.argv[1],sys.argv[2]
deleted={l.strip().rsplit("/",1)[1] for l in open(os.path.join(d,"deleted.log")) if l.strip()}
runs=json.load(open(os.path.join(d,"runs.json")))["workflow_runs"]
print(sum(1 for r in runs if r["head_sha"]==sha and str(r["id"]) not in deleted))
PYCASE13
)
[ "$kept_br" = "3" ] \
  && ok "all 3 of that commit's runs survive, not a shredded subset" \
  || no "only $kept_br of the gate branch commit's 3 runs survived"
grep -q "commits" "$D/calls.log" \
  && ok "witness: the branch commits query was actually made" \
  || no "witness: no commits call -- case 13 proves nothing"

# --- 14. the rule can be turned off, and that isolates it ---------------
# The control. Without it case 13 only shows those three runs survived,
# not that KEEP RULE 5 is what saved them -- rule 4 and the group floor
# are both in the same fixture.
D="$TMP/branchoff"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
echo "[{\"sha\":\"$BRSHA\"}]" > "$D/commits.json"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0 KEEP_BRANCH_COMMITS=0
[ "$DELS" = 57 ] \
  && ok "with rule 5 off the same fixture deletes 57 -- the 3 were saved BY the rule" \
  || no "rule 5 disabled deleted $DELS (expected 57): the control does not isolate the rule"

# --- 15. a failed branch commits query refuses rather than deleting -----
# Same dangerous direction as case 10. An unanswerable query yields an
# empty keep set, which disarms the rule and deletes exactly what it
# protects, while printing a count that reads like success.
D="$TMP/brfail"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
rm -f "$D/commits.json"        # the stub fails on a missing fixture
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "Cannot list branch commits" <<<"$OUT" \
  && ok "a failed branch commits query exits 2 and deletes nothing" \
  || no "failed commits query: rc=$RC, deleted=$DELS (want rc 2, 0 deleted)"

# --- 16. commits listed but no SHA is a shape change --------------------
# The subtler half, as with case 11: the query works, the field moved. A
# gate branch always has commits, so an empty answer is never legitimate
# here -- unlike the open-PR list, which may honestly be empty.
D="$TMP/brshape"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
echo '[{"commit":{"message":"no sha field"}}]' > "$D/commits.json"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "shape changed" <<<"$OUT" \
  && ok "branch commits that yield no SHA refuse, naming the shape change" \
  || no "commits shape change: rc=$RC, deleted=$DELS (want rc 2, 0 deleted)"

# --- 17. an unreadable scope file refuses -------------------------------
# THE ANTI-DRIFT PROPERTY, driven. If the scope cannot be read the purge
# must not fall back to a built-in default: a default could be NARROWER
# than what check-missing-runs.sh reconciles, and then the purge deletes
# evidence that gate demands while both look healthy. That silent
# disagreement is the entire bug being closed, so it has to be loud.
D="$TMP/noscope"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
rm -f "$D/scope.env"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "No branch scope" <<<"$OUT" \
  && ok "an unreadable scope file exits 2 rather than purging on a private default" \
  || no "missing scope: rc=$RC, deleted=$DELS (want rc 2, 0 deleted)"

# --- 18. a scope file missing a key refuses -----------------------------
# Half a scope is not a scope. A file that defines the branches but not
# the depth would otherwise leave BRANCH_COMMITS empty and the query would
# ask for a page size of nothing.
D="$TMP/halfscope"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
echo 'GATE_SCOPE_BRANCHES="dev main"' > "$D/scope.env"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "Branch scope incomplete" <<<"$OUT" \
  && ok "a scope file defining only half the scope refuses" \
  || no "half scope: rc=$RC, deleted=$DELS (want rc 2, 0 deleted)"

# --- 19. the two gates read ONE scope, and it is the shipped one --------
# The populations can only agree if both scripts read the same file, so
# the file has to exist where both defaults point. A test that only ever
# drives GATE_SCOPE_FILE would pass with the shipped file absent or
# defining different keys -- which is the drift this closes, one edit
# later. Assert the real artifact, not the fixture.
SHIPPED="$HERE/../.github/gate-branch-scope.env"
[ -r "$SHIPPED" ] \
  && ok "the shipped scope file exists at the path both scripts default to" \
  || no "no scope file at $SHIPPED -- both gates would refuse in production"
if [ -r "$SHIPPED" ]; then
    ( set -u
      # shellcheck disable=SC1090
      . "$SHIPPED"
      [ -n "${GATE_SCOPE_BRANCHES+x}" ] && [ -n "${GATE_SCOPE_COMMITS:-}" ] ) \
      && ok "the shipped scope defines both GATE_SCOPE_BRANCHES and GATE_SCOPE_COMMITS" \
      || no "the shipped scope is incomplete -- both gates would refuse in production"
    # AND THE BRANCH LIST IS NOT EMPTY. Presence is not the property, and
    # the difference is not academic: measured 2026-08-28 on the shipped
    # code, `GATE_SCOPE_BRANCHES=""` in this file disables the branch phase
    # on BOTH gates -- the purge prints "rule DISABLED", the detector
    # reconciles `[none]` -- and BOTH self-test suites stayed fully green.
    # That is a universal gate satisfied by emptying its domain, arriving
    # through the one file this design made load-bearing. An empty value is
    # a legitimate SELF-TEST seam, driven through the environment; it is
    # never a legitimate shipped configuration.
    # COUNT WORDS, DO NOT TEST PRESENCE. `-n` is one character to the side of
    # the property and it is the side that fails silently: measured
    # 2026-08-28, `GATE_SCOPE_BRANCHES="   "` in this file satisfies the
    # foreign-content guard, satisfies `-n` in BOTH suites, makes `for br in
    # $BRANCHES` iterate zero times so the shape refusal never runs, and the
    # purge then deletes the branch commits keep rule 5 exists to spare --
    # exiting 0, printing "0 commit(s) protected across [   ]" as though a
    # count of zero were a result. Both suites stayed 38/0 and 62/0 while it
    # did. A presence test cannot see a whitespace-only list; a word count can.
    ( set -u
      # shellcheck disable=SC1090
      . "$SHIPPED"
      # shellcheck disable=SC2086
      [ "$(set -- ${GATE_SCOPE_BRANCHES:-}; echo $#)" -ge 1 ] ) \
      && ok "the shipped scope names at least one branch, counted as WORDS (an empty or blank list silently disarms both gates)" \
      || no "the shipped GATE_SCOPE_BRANCHES has no words in it -- the branch phase is off on both gates and nothing else says so"
fi
# And no workflow may restate the scope: a copy in a workflow file is the
# second enumeration this design exists to remove.
#
# TWO SPELLINGS ENUMERATED MEANS A THIRD EXISTS. The first version of this
# scan matched only the YAML `env:` KEY spelling, and measured 2026-08-28 the
# same restatement written INLINE on the `run:` line --
#   run: GATE_BRANCH_COMMITS=10 bash scripts/check-missing-runs.sh 20
# -- survived both suites: a live second enumeration overriding the scope
# file, which is exactly the collision being closed. So the separator is now
# `[:=]`, covering the env key and the inline assignment, and the key set
# includes GATE_SCOPE_FILE -- pointing either gate at a different scope file
# restates the scope wholesale without naming a branch or a number.
#
# WHAT THIS SCAN CANNOT SEE, stated rather than claimed away. Its domain is
# `.github/workflows/` only, so a caller outside it -- a Makefile target, a
# composite action, a local script -- is invisible to it; today there is no
# `.github/actions/` and `missing-runs.yml` is the only workflow invoking
# either gate. It is a text scan, so a value assembled at runtime
# (`GATE_BRANCH""ES=dev`, or a name built from `${{ }}` fragments) passes it.
# And it judges the checked-out tree, so a scope restated in repository or
# environment VARIABLES in the GitHub settings is out of reach entirely.
# The bound is: no literal restatement of these keys in a workflow file.
#
# `scan_restates` is a FUNCTION so this suite can drive the shapes it claims
# to catch. A scan whose only subject is one real directory that is expected
# to be clean has one possible verdict, which is not a check.
scan_restates() {   # scan_restates <dir>
    grep -rnE '^[^#]*GATE_(BRANCHES|BRANCH_COMMITS|SCOPE_FILE|SCOPE_BRANCHES|SCOPE_COMMITS)[[:space:]]*[:=]' \
         "$1" 2>/dev/null
}
wf_restates=$(scan_restates "$HERE/../.github/workflows/") || wf_restates=""
if [ -n "$wf_restates" ]; then
    no "a workflow restates the branch scope -- a second enumeration that must agree with the scope file: $wf_restates"
else
    ok "no workflow restates the branch scope; the scope file is the only definition"
fi

# DRIVE THE ABSENCE: the scan has to go off on each shape, or the clean
# verdict above proves only that the pattern matches nothing.
RESTATE=$(mktemp -d)
restate_case() {   # restate_case <label> <file-content>
    printf '%s' "$2" > "$RESTATE/wf.yml"
    if [ -n "$(scan_restates "$RESTATE")" ]; then
        ok "the restatement scan sees $1"
    else
        no "the restatement scan is BLIND to $1 -- a live second enumeration would ship"
    fi
}
restate_case "the YAML env-key spelling" 'jobs:
  x:
    steps:
      - env:
          GATE_BRANCH_COMMITS: "10"
        run: bash scripts/check-missing-runs.sh 20
'
restate_case "the inline run-line spelling" 'jobs:
  x:
    steps:
      - run: GATE_BRANCH_COMMITS=10 GATE_BRANCHES="dev" bash scripts/check-missing-runs.sh 20
'
restate_case "a redirected GATE_SCOPE_FILE" 'jobs:
  x:
    steps:
      - env:
          GATE_SCOPE_FILE: .github/other-scope.env
        run: bash scripts/purge-workflow-runs.sh
'
restate_case "an exported assignment inside a run block" 'jobs:
  x:
    steps:
      - run: |
          export GATE_BRANCHES=dev
          bash scripts/check-missing-runs.sh 20
'
# THE OTHER DIRECTION: prose about the scope is not a restatement of it, and
# missing-runs.yml carries exactly that prose today. A scan that refused it
# would be unusable, and the clean verdict above would be meaningless.
printf '%s' '# GATE_BRANCHES: not set here, it lives in the scope file
jobs:
  x:
    steps:
      # the scope file defines GATE_BRANCHES and GATE_BRANCH_COMMITS
      - run: bash scripts/check-missing-runs.sh 20
' > "$RESTATE/wf.yml"
[ -z "$(scan_restates "$RESTATE")" ] \
  && ok "the restatement scan does NOT fire on commented prose naming the variables" \
  || no "the restatement scan fires on a comment -- missing-runs.yml's own prose would trip it"
rm -rf "$RESTATE"

# --- 18b. an exported value may not stand in for the file ---------------
# The file is SOURCED, so whatever the environment already exports is still
# set when the completeness check runs. A scope file defining only half the
# scope would then be completed by an exported GATE_SCOPE_COMMITS and this
# script would purge on a population the file never named. Nothing exports
# those today, which is exactly why it has to be asserted: "unreadable must
# refuse, not default" is only true while nobody sets them.
D="$TMP/exported"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
echo 'GATE_SCOPE_BRANCHES="dev main"' > "$D/scope.env"
drive "$D" env GATE_SCOPE_COMMITS=15 RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "Branch scope incomplete" <<<"$OUT" \
  && ok "an exported GATE_SCOPE_COMMITS does not complete a half scope file" \
  || no "the environment completed a half scope file: rc=$RC deleted=$DELS: $OUT"

# --- 19a. THE SHIPPED SCOPE MUST BE ACCEPTED BY THIS SCRIPT -------------
# Reading the shipped file's contents is not the same as running the purge
# against it, and the difference is measurable: with `GATE_SCOPE_COMMITS=1`
# appended to the shipped file -- a duplicated key, last-wins -- every
# assertion above still passed, because every other case in this suite
# supplies its own fixture scope. The detector's suite went red; this one
# did not. So drive the REAL artifact through the REAL script, and let any
# degenerate shipped value (blank list, duplicate key, CRLF, bad depth)
# turn this suite red on its own.
D="$TMP/shippedscope"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
drive "$D" env GATE_SCOPE_FILE="$SHIPPED" RETENTION_DAYS=0 KEEP_GROUPS=1
# shellcheck disable=SC1090
shipped_brs=$( . "$SHIPPED"; echo "${GATE_SCOPE_BRANCHES:-}" )
[ "$RC" != 2 ] && grep -qF "protected across [${shipped_brs}]" <<<"$OUT" \
  && ok "the purge runs against the SHIPPED scope file and arms keep rule 5 on its branches" \
  || no "the shipped scope file is not usable by this script: rc=$RC: $OUT"

# --- 19b. the degenerate scope values, each driven ALONE (#874) ---------
# Every shape below is individually legal to the foreign-content guard and
# each one silently narrows or disarms keep rule 5 -- the direction that
# DELETES the run records check-missing-runs.sh then demands. Asserted by
# EXIT CODE and by zero deletions, never by message text.
#
# ASSERT WHICH REFUSAL FIRED, not merely that one did. A shape caught by a
# guard other than the one it was written for scores a kill for the wrong
# check -- and one of these is exactly that: a literal TAB is not in the
# foreign-content character class, so it never reaches the word count. That
# is recorded below as the guard that actually catches it, not smuggled in
# as evidence for the new one.
degen() {   # degen <label> <expected-error-title> <scope-content>
    local d="$TMP/degen$$_$RANDOM"; mkfix "$d"; runs_json "$NOW" 20 30 > "$d/runs.json"
    printf '%s' "$3" > "$d/scope.env"
    drive "$d" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
    [ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -qF "title=$2::" <<<"$OUT" \
      && ok "a scope file that $1 refuses as '$2' (exit 2, 0 deleted)" \
      || no "a scope file that $1: rc=$RC deleted=$DELS, wanted title '$2': $OUT"
}
# The one that ran: 54 deletions with a valid scope, 57 with this one, exit 0.
degen "names no branch at all" "Branch scope names no branch" 'GATE_SCOPE_BRANCHES=""
GATE_SCOPE_COMMITS=15
'
degen "names only whitespace as its branch list" "Branch scope names no branch" 'GATE_SCOPE_BRANCHES="   "
GATE_SCOPE_COMMITS=15
'
degen "names only a tab as its branch list" "Branch scope has foreign content" "$(printf 'GATE_SCOPE_BRANCHES="\t"\nGATE_SCOPE_COMMITS=15\n')"
degen "sets a depth of zero" "Branch scope depth is not a count" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=0
'
degen "sets a non-numeric depth" "Branch scope depth is not a count" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=abc
'
degen "is saved with CRLF endings" "Branch scope has carriage returns" "$(printf 'GATE_SCOPE_BRANCHES="dev"\r\nGATE_SCOPE_COMMITS=15\r\n')"
degen "assigns GATE_SCOPE_COMMITS twice" "Branch scope defines a key twice" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
GATE_SCOPE_COMMITS=1
'
degen "assigns GATE_SCOPE_BRANCHES twice" "Branch scope defines a key twice" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_BRANCHES=""
GATE_SCOPE_COMMITS=15
'

# THE OPPOSITE DIRECTION for the word count specifically: one branch is one
# branch. A count that refused a legal single-branch scope would be the
# refusal-only guard the four-move rule exists to catch.
D="$TMP/onebranch"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
printf 'GATE_SCOPE_BRANCHES="dev"\nGATE_SCOPE_COMMITS=15\n' > "$D/scope.env"
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1
[ "$RC" = 0 ] \
  && ok "a scope naming exactly one branch is still ACCEPTED (the word count is not a refusal-only guard)" \
  || no "the word count refused a legal single-branch scope: rc=$RC: $OUT"

# --- 19c. the scope path being a DIRECTORY names the right refusal ------
# `-r` alone is true of a directory, so the path fell through to the
# completeness check and refused as "incomplete" -- exit 2 preserved, the
# DIAGNOSIS wrong, on a gate that runs unattended and deletes data.
D="$TMP/dirscope"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
mkdir -p "$D/scopedir"
: > "$D/calls.log"; : > "$D/deleted.log"
OUT=$(FIXDIR="$D" PATH="$TMP/bin:$PATH" REPO=fixture/repo NOW_EPOCH="$NOW" \
      GATE_SCOPE_FILE="$D/scopedir" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0 \
      bash "$GATE" 2>&1); RC=$?
DELS=$(wc -l < "$D/deleted.log")
# ASSERT THE WORDING THAT ONLY THIS GUARD EMITS. Measured 2026-08-28:
# keying on the "No branch scope" TITLE let the mutant that reverts `-f`
# survive, because the source-failure refusal below carries the same title
# -- a directory then refuses for the right exit code under the wrong
# reason, and the assertion could not tell the two apart.
[ "$RC" = 2 ] && [ "$DELS" = 0 ] && grep -q "as a regular file" <<<"$OUT" \
  && ok "a scope path that is a directory refuses as unreadable, not as incomplete" \
  || no "a directory scope path: rc=$RC deleted=$DELS: $OUT"

# --- 20. the scope file's DEPTH is the depth actually queried -----------
# Existence and completeness are not the property. The property is that
# the number in the file is the number this script asks GitHub for -- and
# a script that read the file, ignored it, and used a built-in default
# would pass every assertion above while protecting a different population
# from the one check-missing-runs.sh reconciles. That silent disagreement
# IS the bug. So drive an unusual depth and read it back off the wire.
# test-check-missing-runs.sh asserts the same property on the other gate;
# together they are what makes the two populations one population.
D="$TMP/depth"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
cat > "$D/scope.env" <<'SCOPE'
GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=3
SCOPE
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1
depth_call=$(grep -c 'commits?sha=dev&per_page=3' "$D/calls.log") || depth_call=0
[ "$depth_call" -ge 1 ] \
  && ok "the purge queries the depth the scope file names (per_page=3, not a built-in default)" \
  || no "the purge ignored the scope file's depth; commits calls were: $(grep commits "$D/calls.log")"

# --- 21. and the scope file's BRANCH LIST is the list actually queried ---
# The other half of the same property, and it was a real gap: a mutant
# that replaced the scope's branch list with a built-in "dev" SURVIVED the
# depth case above, because that case names one branch and the fixture
# answers every branch identically. A branch the purge does not walk is a
# branch whose commits it does not protect while the detector still
# reconciles them -- the collision again, one branch at a time. So name
# branches nothing could guess and require BOTH on the wire.
D="$TMP/brnames"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
cat > "$D/scope.env" <<'SCOPE'
GATE_SCOPE_BRANCHES="alpha beta"
GATE_SCOPE_COMMITS=15
SCOPE
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1
got_alpha=$(grep -c 'commits?sha=alpha&' "$D/calls.log") || got_alpha=0
got_beta=$(grep -c 'commits?sha=beta&' "$D/calls.log")   || got_beta=0
[ "$got_alpha" -ge 1 ] && [ "$got_beta" -ge 1 ] \
  && ok "every branch the scope file names is queried (alpha and beta both on the wire)" \
  || no "the purge did not walk the scope's branch list (alpha=$got_alpha beta=$got_beta): $(grep commits "$D/calls.log")"

# --- 22. the branch-commits query does NOT paginate ---------------------
# The script's own comment calls this out -- "NOT --paginate ... the
# detector asks for exactly per_page=N commits and stops; paginating would
# walk the entire history and protect all of it" -- and nothing asserted
# it. Measured 2026-08-28: adding `--paginate` to that one call SURVIVED
# every case above, because the stub, like every stub, ignores the flag.
# The wire record does not: the flag is in the recorded argv, so assert
# there. An enumeration beside the code is an unrun checklist.
#
# What the mutant costs in production is not a false green, it is the
# opposite failure -- a keep set that grows without bound, so rule 5
# protects every commit a gate branch ever had and the purge stops
# purging. This gate exists because run records accumulate at ~325/day.
D="$TMP/nopaginate"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
cat > "$D/scope.env" <<'SCOPE'
GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
SCOPE
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1
paginated=$(grep -c 'commits?sha=.*--paginate\|--paginate.*commits?sha=' "$D/calls.log") || paginated=0
[ "$paginated" -eq 0 ] \
  && ok "the branch-commits query is bounded -- no --paginate on the wire" \
  || no "the branch-commits query carries --paginate; rule 5 would protect the whole branch history: $(grep commits "$D/calls.log")"

# --- 23. the scope file cannot smuggle configuration --------------------
# THE FILE IS SOURCED, AND THIS SCRIPT DELETES DATA. Measured 2026-08-28
# against the code as it stood: a line reading `DRY_RUN=0` in the scope
# file turned a dry run into three real deletions, exiting 0; a line
# reading `KEEP_OPEN_PR_HEADS=0` disarmed keep rule 4 -- the rule #740 and
# #837 were paid for -- and deleted the open PR head's runs it exists to
# protect. Neither said anything a reader would notice.
#
# So a scope file may contain comments and the two assignments and
# nothing else. Each shape below is driven ALONE, and each is asserted by
# EXIT CODE and by zero deletions, never by message text.
smuggle() {   # smuggle <label> <scope-content>
    local d="$TMP/smug$$_$RANDOM"; mkfix "$d"; runs_json "$NOW" 20 30 > "$d/runs.json"
    printf '%s' "$2" > "$d/scope.env"
    drive "$d" env RETENTION_DAYS=0 KEEP_GROUPS=1 DRY_RUN=0
    [ "$RC" = 2 ] && [ "$DELS" = 0 ] \
      && ok "a scope file that $1 refuses (exit 2, 0 deleted)" \
      || no "a scope file that $1 was accepted: rc=$RC deleted=$DELS"
}
smuggle "turns off DRY_RUN" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
DRY_RUN=0
'
smuggle "disarms keep rule 4" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
KEEP_OPEN_PR_HEADS=0
'
smuggle "disarms keep rule 5" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
KEEP_BRANCH_COMMITS=0
'
smuggle "runs a bare command" 'GATE_SCOPE_BRANCHES="dev"
GATE_SCOPE_COMMITS=15
echo smuggled
'
smuggle "hides a second assignment behind a semicolon" 'GATE_SCOPE_BRANCHES="dev"; DRY_RUN=0
GATE_SCOPE_COMMITS=15
'
smuggle "substitutes a command into the value" 'GATE_SCOPE_BRANCHES="$(echo dev)"
GATE_SCOPE_COMMITS=15
'

# THE OPPOSITE DIRECTION, because a guard fails in ONE direction and the
# refusal above is worthless if it also refuses a legal file. Comments,
# blank lines, leading whitespace and a slashed branch name are all legal,
# and the shipped file itself is exercised by case 19 above.
D="$TMP/legalscope"; mkfix "$D"; runs_json "$NOW" 20 30 > "$D/runs.json"
cat > "$D/scope.env" <<'SCOPE'
# a comment, and a blank line follows

  GATE_SCOPE_BRANCHES="dev release/v1.9 main"
GATE_SCOPE_COMMITS=15
SCOPE
drive "$D" env RETENTION_DAYS=0 KEEP_GROUPS=1
[ "$RC" = 0 ] && grep -q 'release/v1.9' <<<"$OUT" \
  && ok "a legal scope with comments, blanks, indentation and a slashed branch is ACCEPTED" \
  || no "the foreign-content guard refused a legal scope file: rc=$RC: $OUT"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
