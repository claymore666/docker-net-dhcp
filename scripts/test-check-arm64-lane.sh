#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-arm64-lane.sh (#531).
#
# `gh` is stubbed via PATH, with the fixtures in files rather than inside
# the generated stub, so a fixture can never turn the stub into a syntax
# error and fail a case for a reason unrelated to what it tests.
#
# The cases that keep the rest honest are the ones where the gate must
# stay quiet: a suite that started and then FAILED still passes this gate
# (it judges the start, the suite owns the verdict), and a job absent
# from the first poll but present on the second passes too (a run needs a
# moment to materialise its jobs, and treating that as "no runner" would
# make the gate fire on every rc). Without those, a gate that answered
# "never started" unconditionally would still be green on the rest.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-arm64-lane.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# make_gh <dir>
# Answers from $FIXTURE_DIR: `jobs` holds what `gh api --jq` would print
# for the job's status (empty means the job is not in the run yet), or
# the literal ERR to fail the query. `jobs.<n>` overrides it on poll n,
# which is how the "appears on the second poll" case is expressed.
make_gh() {
    local dir="$1"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/gh" <<'STUB'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *"/jobs?per_page=100"*)
     if [ -f "$FIXTURE_DIR/step" ]; then
        n=$(cat "$FIXTURE_DIR/step")
        printf '%s' "$((n + 1))" > "$FIXTURE_DIR/step"
        if [ -f "$FIXTURE_DIR/jobs.$n" ]; then
           [ "$(cat "$FIXTURE_DIR/jobs.$n")" = "ERR" ] && exit 1
           cat "$FIXTURE_DIR/jobs.$n"; exit 0
        fi
     fi
     [ "$(cat "$FIXTURE_DIR/jobs")" = "ERR" ] && exit 1
     cat "$FIXTURE_DIR/jobs" ;;
  *"repo view"*) echo "o/r" ;;
  *) echo "" ;;
esac
STUB
    chmod +x "$dir/bin/gh"
}

# run_it <jobs-fixture> [wait-minutes] [extra-fixture-name=value ...]
run_it() {
    local dir; dir=$(mktemp -d)
    make_gh "$dir"
    printf '%s\n' "$1" > "$dir/jobs"
    local wait="${2:-0}"
    shift 2 2>/dev/null || shift $#
    local kv
    for kv in "$@"; do
        printf '%s\n' "${kv#*=}" > "$dir/${kv%%=*}"
    done
    FIXTURE_DIR="$dir" PATH="$dir/bin:$PATH" GATE_REPO=o/r \
        GATE_POLL_SECONDS=0 \
        bash "$CHECK" 12345 "$wait" >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

# --- the job started --------------------------------------------------

out=$(run_it "in_progress" 0); rc=$?
[ $rc -eq 0 ] && ok "a running arm64-suite passes" || no "a running arm64-suite passes (rc=$rc: $out)"

# The gate judges the start, not the verdict. A suite that ran and failed
# must reach the reader as a failing suite — if this gate also went red,
# a real arm64 failure would be reported as an infrastructure problem.
out=$(run_it "completed" 0); rc=$?
[ $rc -eq 0 ] && ok "a completed arm64-suite passes regardless of outcome" \
               || no "a completed arm64-suite passes regardless of outcome (rc=$rc: $out)"

# --- nobody launched a runner ----------------------------------------

out=$(run_it "queued" 0); rc=$?
[ $rc -eq 1 ] && ok "a job still queued at the deadline fails" || no "a job still queued at the deadline fails (rc=$rc: $out)"
case "$out" in
  *"dhcp-ci-arm64"*) ok "the failure names the label to look for" ;;
  *) no "the failure names the label to look for (got: $out)" ;;
esac

# A job absent from the list is the same "not yet" as queued: the run may
# still be creating it.
out=$(run_it "" 0); rc=$?
[ $rc -eq 1 ] && ok "a job absent at the deadline fails" || no "a job absent at the deadline fails (rc=$rc: $out)"

# --- patience before the deadline ------------------------------------

# Absent on the first poll, running on the second. This is the ordinary
# start of every rc; firing here would make the gate cry wolf every time.
out=$(run_it "in_progress" 1 "step=0" "jobs.0=" ); rc=$?
[ $rc -eq 0 ] && ok "a job that appears on a later poll passes" || no "a job that appears on a later poll passes (rc=$rc: $out)"

# --- the API is unreadable -------------------------------------------

# Not fail-open, and not a false accusation either: unknown is its own
# exit code, distinct from "no runner was online".
out=$(run_it "ERR" 0); rc=$?
[ $rc -eq 2 ] && ok "an unreadable API exits 2, not 0 or 1" || no "an unreadable API exits 2, not 0 or 1 (rc=$rc: $out)"

# A transient error that clears must not be held against the lane.
out=$(run_it "in_progress" 1 "step=0" "jobs.0=ERR"); rc=$?
[ $rc -eq 0 ] && ok "a transient API error that clears passes" || no "a transient API error that clears passes (rc=$rc: $out)"

# --- usage ------------------------------------------------------------

out=$(bash "$CHECK" 2>&1); rc=$?
[ $rc -eq 2 ] && ok "no run id exits 2" || no "no run id exits 2 (rc=$rc: $out)"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
