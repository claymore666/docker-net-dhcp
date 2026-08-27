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

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
