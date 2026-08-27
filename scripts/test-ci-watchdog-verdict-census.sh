#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for ci-watchdog-verdict-census.sh (#848).
#
# THE SEAM IS THE TRANSPORT, NOT THE VERDICT. `$GH` is stubbed with a
# fake `gh` that serves real JSON from a fixture tree and then runs the
# REAL jq expression the script asked for. Stubbing at the classified
# result -- handing back "POOL SHORT" directly -- would leave the
# annotation-title matching, the attempt walk and the clustering
# unexecuted, which is exactly the defect #827 found in
# check-attestation-parity: a seam placed above the logic under test.
#
# THE CASE THAT CARRIES THE WEIGHT is `verdict-survives-a-rerun`. The
# run's CURRENT conclusion is success and only attempt 1 carries the
# annotation. That is the precise shape that made #848 read as refuted:
# a sweep keyed on current conclusion excluded the one run holding the
# answer. If this suite ever passes with the attempt walk removed, the
# census has the same blind spot the investigation did.
set -uo pipefail

CENSUS="$(cd "$(dirname "$0")" && pwd)/ci-watchdog-verdict-census.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
command -v jq >/dev/null || { echo "FAIL: jq is required for this suite"; exit 1; }

pass=0; fail=0

# The fake gh: FIXTURES/<path with / -> __>.json is the body; the jq the
# caller passed is applied for real.
cat > "$TMP/gh" <<'STUB'
#!/usr/bin/env bash
# usage: gh api <path> --jq <expr>
[ "$1" = api ] || { echo "unexpected gh subcommand: $1" >&2; exit 9; }
path="$2"; shift 2
expr='.'
while [ $# -gt 0 ]; do case "$1" in --jq) expr="$2"; shift 2 ;; *) shift ;; esac; done
key=$(printf '%s' "$path" | tr '/?=' '___')
f="$FIXTURES/$key.json"
[ -f "$f" ] || { echo "no fixture for $path (looked for $key.json)" >&2; exit 8; }
jq -r "$expr" < "$f"
STUB
chmod +x "$TMP/gh"

# --- fixture builders ---------------------------------------------------
fx() { # fx DIR ; then wf/run/job helpers write into it
    FX="$1"; rm -rf "$FX"; mkdir -p "$FX"
}
put() { printf '%s' "$2" > "$FX/$(printf '%s' "$1" | tr '/?=' '___').json"; }

wf_runs() { # wf_runs FILE id...
    local f="$1"; shift
    local ids="" i
    for i in "$@"; do ids="$ids{\"id\":$i},"; done
    put "repos/o/r/actions/workflows/$f/runs?per_page=5" "{\"workflow_runs\":[${ids%,}]}"
}
run_meta() { # run_meta ID ATTEMPTS CREATED BRANCH
    put "repos/o/r/actions/runs/$1" \
        "{\"run_attempt\":$2,\"created_at\":\"$3\",\"head_branch\":\"$4\"}"
}
attempt_jobs() { # attempt_jobs RUN ATTEMPT JOBID|none
    if [ "$3" = none ]; then
        put "repos/o/r/actions/runs/$1/attempts/$2/jobs" '{"jobs":[{"name":"gate","id":1}]}'
    else
        put "repos/o/r/actions/runs/$1/attempts/$2/jobs" \
            "{\"jobs\":[{\"name\":\"gate\",\"id\":1},{\"name\":\"watchdog\",\"id\":$3}]}"
    fi
}
annotations() { # annotations JOBID TITLE|-
    if [ "$2" = - ]; then
        put "repos/o/r/check-runs/$1/annotations" '[{"title":"","message":"x"}]'
    else
        put "repos/o/r/check-runs/$1/annotations" \
            "[{\"title\":\"\",\"message\":\"noise\"},{\"title\":\"$2\",\"message\":\"m\"}]"
    fi
}

run() { # run NAME WANT_RC [SUBSTR...]
    local name="$1" want="$2"; shift 2
    local out got
    out=$(FIXTURES="$FX" REPO="o/r" GH="$TMP/gh" WINDOW_RUNS=5 \
          WATCHDOG_POOL_WORKFLOWS=".github/workflows/integration.yml" \
          bash "$CENSUS" 2>&1); got=$?
    if [ "$got" -ne "$want" ]; then
        echo "FAIL: $name — want exit $want, got $got"
        printf '%s\n' "$out" | sed 's/^/      /'; fail=$((fail + 1)); return
    fi
    local missing=""
    for s in "$@"; do printf '%s\n' "$out" | grep -F -- "$s" >/dev/null || missing="$missing '$s'"; done
    if [ -n "$missing" ]; then
        echo "FAIL: $name — exit $got as wanted, output lacks:$missing"
        printf '%s\n' "$out" | sed 's/^/      /'; fail=$((fail + 1)); return
    fi
    echo "ok: $name"; pass=$((pass + 1))
}

SHORT="CI POOL SHORT: this run never got a runner"
STARVE="CI STARVATION: this run never got a runner"

# --- a clean window -----------------------------------------------------
fx "$TMP/clean"
wf_runs integration.yml 100
run_meta 100 1 "2026-08-01T10:00:00Z" main
attempt_jobs 100 1 900
annotations 900 -
run "a window with no POOL SHORT passes" 0 "POOL SHORT 0"

# --- the case that carries the weight ----------------------------------
# One verdict, on attempt 1 only, of a run whose CURRENT conclusion is
# success. A sweep keyed on the run rather than its attempts sees
# nothing here — which is exactly how #848 came to be refuted.
fx "$TMP/rerun"
wf_runs integration.yml 100
run_meta 100 2 "2026-08-27T12:36:59Z" tests/827
attempt_jobs 100 1 900
attempt_jobs 100 2 901
annotations 900 "$SHORT"
annotations 901 -
run "a verdict on attempt 1 survives the re-run" 0 "POOL SHORT 1" "attempt 1"

# --- verdicts are not incidents ----------------------------------------
# 45 seconds apart: two runs caught by ONE pool event. Counting raw
# would report a recurrence that has not happened.
fx "$TMP/cluster"
wf_runs integration.yml 100 200
run_meta 100 1 "2026-08-27T12:36:59Z" tests/827
run_meta 200 1 "2026-08-27T12:37:44Z" ci/831
attempt_jobs 100 1 900; annotations 900 "$SHORT"
attempt_jobs 200 1 901; annotations 901 "$SHORT"
run "two verdicts 45s apart are one incident" 0 "POOL SHORT 2 (in 1 incident(s))"

# --- and the real recurrence -------------------------------------------
fx "$TMP/recur"
wf_runs integration.yml 100 200
run_meta 100 1 "2026-08-27T12:36:59Z" tests/827
run_meta 200 1 "2026-08-29T09:00:00Z" ci/900
attempt_jobs 100 1 900; annotations 900 "$SHORT"
attempt_jobs 200 1 901; annotations 901 "$SHORT"
run "two verdicts two days apart are two incidents" 1 \
    "in 2 incident(s)" "second incident" "nothing else held the pool"

# --- the other branch is counted, and does not trip the threshold ------
# Without this, a census that matched any watchdog annotation at all
# would pass every case above for the wrong reason.
fx "$TMP/starve"
wf_runs integration.yml 100 200
run_meta 100 1 "2026-08-27T12:36:59Z" a
run_meta 200 1 "2026-08-29T09:00:00Z" b
attempt_jobs 100 1 900; annotations 900 "$STARVE"
attempt_jobs 200 1 901; annotations 901 "$STARVE"
run "contention verdicts are counted separately and do not fire" 0 \
    "STARVATION 2" "POOL SHORT 0"

# --- non-vacuity --------------------------------------------------------
# "No POOL SHORT verdicts" is true of a window holding no watchdog job,
# and that is the strongest possible pass produced by measuring nothing.
fx "$TMP/nowd"
wf_runs integration.yml 100
run_meta 100 1 "2026-08-01T10:00:00Z" main
attempt_jobs 100 1 none
run "a window with no watchdog job at all refuses" 2 "NO watchdog job"

# --- absent is not zero -------------------------------------------------
fx "$TMP/dark"
# no fixture for the workflow listing at all: the transport fails
run "an unreadable run listing refuses rather than counting zero" 2 "could not list runs"

# --- unusable inputs ----------------------------------------------------
fx "$TMP/clean2"
wf_runs integration.yml 100
run_meta 100 1 "2026-08-01T10:00:00Z" main
attempt_jobs 100 1 900; annotations 900 -
out=$(FIXTURES="$FX" REPO="o/r" GH="$TMP/gh" WINDOW_RUNS=5 THRESHOLD=two \
      WATCHDOG_POOL_WORKFLOWS=".github/workflows/integration.yml" \
      bash "$CENSUS" 2>&1); got=$?
if [ "$got" -eq 2 ] && printf '%s\n' "$out" | grep -F "not a number" >/dev/null; then
    echo "ok: a non-numeric threshold refuses"; pass=$((pass + 1))
else
    echo "FAIL: a non-numeric threshold — want exit 2 naming it, got $got"; fail=$((fail + 1))
fi

# --- A PARTLY-READ WINDOW IS NOT A CLEAN ONE ---------------------------
# The census only fails UPWARD: it goes red when POOL SHORT recurs, so a
# verdict lost to a failed read makes a recurrence less likely to be
# reported. Each of the three reads is driven, because they fail
# independently and only one of them was ever the suspect.
#
# THE FIRST CASE IS ALSO THE MUTATE-BACK. `jobs_seen` used to be
# incremented above the annotations read. With it there, this fixture
# gives jobs_seen 1, the non-vacuity guard is satisfied by a job whose
# annotations were never read, and the script prints "POOL SHORT 0" and
# exits 0 -- a clean bill of health over an unread job.
fx "$TMP/unread-ann"
wf_runs integration.yml 100
run_meta 100 1 "2026-08-01T10:00:00Z" main
attempt_jobs 100 1 900
# no annotations fixture for job 900: that read fails
run "an unreadable annotations read refuses rather than counting it clean" 2 \
    "read(s) failed" "job 900: annotations" "only partly examined"

fx "$TMP/unread-meta"
wf_runs integration.yml 100
# no run_meta fixture: the metadata read fails
run "an unreadable run metadata refuses" 2 "read(s) failed" "run 100: metadata"

fx "$TMP/unread-jobs"
wf_runs integration.yml 100
run_meta 100 1 "2026-08-01T10:00:00Z" main
# no attempt_jobs fixture: the job-list read fails
run "an unreadable attempt job list refuses" 2 "read(s) failed" "attempt 1: job list"

# THE UNDERCOUNT MADE VISIBLE. One run's verdict IS read; a second run's
# annotations are not. Without the refusal this window reports
# "POOL SHORT 1" -- a real number, one short, with nothing saying so.
fx "$TMP/unread-partial"
wf_runs integration.yml 100 200
run_meta 100 1 "2026-08-27T12:36:59Z" a
run_meta 200 1 "2026-08-29T09:00:00Z" b
attempt_jobs 100 1 900; annotations 900 "$SHORT"
attempt_jobs 200 1 901
# no annotations fixture for 901
run "a window read only in part refuses even when it holds real verdicts" 2 \
    "read(s) failed" "job 901: annotations"

# --- a timestamp that will not parse is a refusal, not a raw count ------
fx "$TMP/badtime"
wf_runs integration.yml 100
run_meta 100 1 "not-a-timestamp" main
attempt_jobs 100 1 900; annotations 900 "$SHORT"
run "an unparseable timestamp refuses rather than counting raw" 2 "could not be grouped"

echo
echo "passed: $pass  failed: $fail"
[ "$fail" -eq 0 ]
