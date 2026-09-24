#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-coverage-run.sh (#504).
#
# `gh` is stubbed via PATH; the fixtures live in files rather than inside
# the generated stub, so a multi-line fixture cannot turn the stub into a
# syntax error and fail the cases for a reason unrelated to what they
# test.
#
# The two cases that keep the rest honest are the negatives: a coverage
# run that FAILED must pass this gate (it judges presence, not the
# verdict — the ratchet owns the numbers), and a run cancelled AFTER a
# job started must pass too (it produced a check run, so the PR shows it;
# the #504 harm is specifically the run that produced none). Without
# those, a gate that answered "evicted" unconditionally would still go
# green on every remaining case.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-coverage-run.sh"
pass=0; fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# make_gh <dir>
# Reads its answers from $FIXTURE_DIR: `runs` holds the TSV the real
# `gh api --jq` would print (id, status, conclusion), `jobs` maps a run
# id to its job count. Either may be the literal ERR to fail the query.
make_gh() {
    local dir="$1"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/gh" <<'STUB'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *"/actions/workflows/"*"/runs?"*)
     if [ -f "$FIXTURE_DIR/step" ]; then
        n=$(cat "$FIXTURE_DIR/step")
        printf '%s' "$((n + 1))" > "$FIXTURE_DIR/step"
        [ -f "$FIXTURE_DIR/runs.$n" ] && { cat "$FIXTURE_DIR/runs.$n"; exit 0; }
     fi
     if [ -f "$FIXTURE_DIR/late_at" ] && [ "$(cat "$FIXTURE_DIR/clock")" -ge "$(cat "$FIXTURE_DIR/late_at")" ]; then
        cat "$FIXTURE_DIR/runs.late"; exit 0
     fi
     [ "$(cat "$FIXTURE_DIR/runs")" = "ERR" ] && exit 1
     cat "$FIXTURE_DIR/runs" ;;
  *"/jobs?per_page=1"*)
     id=$(printf '%s' "$args" | sed -n 's#.*/actions/runs/\([0-9]*\)/jobs.*#\1#p')
     v=$(awk -v i="$id" '$1 == i { print $2 }' "$FIXTURE_DIR/jobs")
     [ "$v" = "ERR" ] && exit 1
     printf '%s\n' "$v" ;;
  *"repo view"*) echo "o/r" ;;
  *) echo "" ;;
esac
STUB
    chmod +x "$dir/bin/gh"
}

# run_it <runs-tsv> <jobs-map> [wait-minutes] [poll-seconds]
run_it() {
    local dir; guarded_tmpdir dir
    make_gh "$dir"
    printf '%s\n' "$1" > "$dir/runs"
    printf '%s\n' "$2" > "$dir/jobs"
    FIXTURE_DIR="$dir" PATH="$dir/bin:$PATH" GATE_REPO=o/r \
        GATE_POLL_SECONDS="${4:-0}" \
        bash "$CHECK" abc123def456 "${3:-1}" >"$dir/o" 2>&1
    local rc=$?; cat "$dir/o"; rm -rf "$dir"; return $rc
}

# --- the happy path ---------------------------------------------------
out=$(run_it "$(printf '11\tcompleted\tsuccess')" "11 1"); rc=$?
[ "$rc" = 0 ] && ok "a completed coverage run passes" || no "completed run failed (exit $rc): $out"

# --- presence, not verdict --------------------------------------------
# The `coverage` context reports the ratchet's answer itself. If this
# gate also failed on a red run it would double-report, and the release
# manager would have two reds for one cause.
out=$(run_it "$(printf '11\tcompleted\tfailure')" "11 1"); rc=$?
[ "$rc" = 0 ] && ok "a FAILED coverage run still passes — presence is what is judged" \
    || no "a failed coverage run was flagged as missing (exit $rc): $out"

# --- the #504 shape ---------------------------------------------------
# Cancelled while pending: no job was ever assigned, so no check run
# exists and the required context never appears.
out=$(run_it "$(printf '11\tcompleted\tcancelled')" "11 0"); rc=$?
if [ "$rc" = 1 ]; then ok "a run cancelled with zero jobs is reported as evicted"; else
  no "an evicted run returned $rc — that is the bug this gate exists for"; fi
case "$out" in *"gh run rerun 11"*) ok "the failure carries the recovery command" ;;
  *) no "no rerun command in the message: $out" ;; esac
case "$out" in *EVICTED*) ok "an evicted run is diagnosed as evicted" ;;
  *) no "an evicted run lacks the eviction diagnosis: $out" ;; esac

out=$(run_it "$(printf '11\tcompleted\tstartup_failure')" "11 0"); rc=$?
if [ "$rc" = 1 ]; then ok "a run that completed with zero jobs and no cancel fails"; else
  no "a zero-job startup_failure run returned $rc, want 1: $out"; fi
case $'\n'"$out"$'\n' in *$'\n    gh run rerun 11\n'*) ok "the never-started failure carries the exact recovery command" ;;
  *) no "no exact 'gh run rerun 11' line in the never-started message: $out" ;; esac
case "$out" in *EVICTED*) no "a startup_failure run was misreported as eviction: $out" ;;
  *"without running a job"*) ok "a startup_failure run is reported as never started" ;;
  *) no "a startup_failure run lacks the never-started diagnosis: $out" ;; esac

# --- ordinary cancellation is NOT eviction ----------------------------
# A run cancelled after a job started produced a check run, so the PR
# shows a cancelled `coverage` and the merge is blocked visibly. That is
# not the silence this gate looks for.
out=$(run_it "$(printf '11\tcompleted\tcancelled')" "11 3"); rc=$?
[ "$rc" = 0 ] && ok "a run cancelled after jobs started is not called evicted" \
    || no "a cancelled-with-jobs run was called evicted (exit $rc): $out"

# --- recovery ---------------------------------------------------------
# After `gh run rerun`, an evicted run and a good one share the head. The
# gate must go green, or the recovery it prescribes never clears it.
out=$(run_it "$(printf '12\tcompleted\tcancelled\n11\tcompleted\tsuccess')" "$(printf '12 0\n11 1')"); rc=$?
[ "$rc" = 0 ] && ok "an evicted run alongside a completed one passes" \
    || no "recovery state still failed (exit $rc): $out"

# --- no run at all ----------------------------------------------------
out=$(run_it "" ""); rc=$?
if [ "$rc" = 1 ]; then ok "a head with no coverage run at all fails"; else
  no "no coverage run returned $rc — absence is the failure mode"; fi
case "$out" in *EVICTED*) no "absence was misreported as eviction: $out" ;;
  *) ok "absence is not misreported as eviction" ;; esac

# --- still running at the deadline ------------------------------------
out=$(run_it "$(printf '11\tin_progress\tnull')" "11 1"); rc=$?
if [ "$rc" = 1 ]; then ok "a run that never finishes inside the wait fails"; else
  no "an unfinished run returned $rc"; fi
case "$out" in *EVICTED*) no "an unfinished run was misreported as eviction" ;;
  *) ok "an unfinished run is not misreported as eviction" ;; esac

# --- the wait is real -------------------------------------------------
# Without this, the poll loop could judge once and the wait would be
# decorative: a run still queued on the first look would fail forever.
guarded_tmpdir dir
make_gh "$dir"
printf '0' > "$dir/step"
printf '11\tqueued\tnull\n' > "$dir/runs.0"
printf '11\tcompleted\tsuccess\n' > "$dir/runs.1"
printf '11\tcompleted\tsuccess\n' > "$dir/runs"
printf '11 1\n' > "$dir/jobs"
FIXTURE_DIR="$dir" PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_POLL_SECONDS=1 \
    bash "$CHECK" abc123def456 1 >"$dir/o" 2>&1
rc=$?
[ "$rc" = 0 ] && ok "a queued run that completes on a later poll passes" \
    || no "the gate judged once instead of waiting (exit $rc): $(cat "$dir/o")"
rm -rf "$dir"

# run_clocked <runs-tsv> <late-runs-tsv> <late-at-minute> <wait-min> <live-min> [jobs-map]
# Stubs `date` and `sleep` so minutes pass instantly. The stub gh answers
# <late-runs-tsv> from <late-at-minute> on; the clock at the verdict, in
# minutes, is written to $AT_FILE (#1042).
clockdir=""
guarded_tmpdir clockdir
AT_FILE="$clockdir/at"
run_clocked() {
    local dir; guarded_tmpdir dir
    make_gh "$dir"
    printf '%s\n' "$1" > "$dir/runs"
    printf '%s\n' "$2" > "$dir/runs.late"
    printf '%s' "$(( $3 * 60 ))" > "$dir/late_at"
    printf '%s\n' "${6:-11 1}" > "$dir/jobs"
    printf '0' > "$dir/clock"
    cat > "$dir/bin/date" <<'STUB'
#!/usr/bin/env bash
cat "$FIXTURE_DIR/clock"
STUB
    cat > "$dir/bin/sleep" <<'STUB'
#!/usr/bin/env bash
printf '%s' "$(( $(cat "$FIXTURE_DIR/clock") + $1 ))" > "$FIXTURE_DIR/clock"
STUB
    chmod +x "$dir/bin/date" "$dir/bin/sleep"
    FIXTURE_DIR="$dir" PATH="$dir/bin:$PATH" GATE_REPO=o/r GATE_POLL_SECONDS=60 \
        timeout 60 bash "$CHECK" abc123def456 "$4" "$5" >"$dir/o" 2>&1
    local rc=$?
    printf '%s' "$(( $(cat "$dir/clock") / 60 ))" > "$AT_FILE"
    cat "$dir/o"; rm -rf "$dir"; return $rc
}

out=$(run_clocked "$(printf '11\tin_progress\tnull')" "$(printf '11\tcompleted\tsuccess')" 90 75 240); rc=$?
if [ "$rc" = 0 ] && [ "$(cat "$AT_FILE")" = 90 ]; then ok "a run still in progress at the 75-minute deadline is waited for"; else
  no "a run in progress past the absence deadline returned $rc at minute $(cat "$AT_FILE"): $out"; fi

out=$(run_clocked "$(printf '11\tqueued\tnull')" "$(printf '11\tqueued\tnull')" 0 75 240); rc=$?
if [ "$rc" = 1 ] && [ "$(cat "$AT_FILE")" = 240 ]; then ok "a run queued until the live ceiling fails there"; else
  no "a never-started run returned $rc at minute $(cat "$AT_FILE"), want 1 at 240: $out"; fi
case "$out" in *"did not finish within 240m"*) ok "a never-started run is reported as unfinished, not absent" ;;
  *) no "a never-started run was misreported: $out" ;; esac

out=$(run_clocked "" "" 9999 75 240); rc=$?
if [ "$rc" = 1 ] && [ "$(cat "$AT_FILE")" = 75 ]; then ok "a run that never appears fails at the absence deadline"; else
  no "a missing run returned $rc at minute $(cat "$AT_FILE"), want 1 at 75: $out"; fi

out=$(run_clocked "$(printf '11\tin_progress\tnull')" "" 10 75 240); rc=$?
if [ "$rc" = 1 ] && [ "$(cat "$AT_FILE")" = 75 ]; then ok "a run that leaves the list falls back to the absence deadline"; else
  no "a vanished run returned $rc at minute $(cat "$AT_FILE"), want 1 at 75: $out"; fi

out=$(run_clocked "$(printf '11\tcompleted\tstartup_failure')" "" 9999 75 240 "11 0"); rc=$?
if [ "$rc" = 1 ] && [ "$(cat "$AT_FILE")" = 0 ]; then ok "a run that ended with zero jobs fails at once, not at the deadline"; else
  no "a zero-job run returned $rc at minute $(cat "$AT_FILE"), want 1 at 0: $out"; fi

out=$(run_clocked "$(printf '12\tqueued\tnull\n11\tcompleted\tstartup_failure')" "$(printf '12\tqueued\tnull')" 10 75 240 "11 0"); rc=$?
case "$rc:$(cat "$AT_FILE"):$out" in 1:240:*"did not finish within 240m"*) ok "a zero-job run that leaves the list does not colour the later verdict" ;;
  *) no "stale zero-job state leaked: rc $rc at minute $(cat "$AT_FILE"): $out" ;; esac

wf="$HERE/../.github/workflows"
live=$(sed -n 's/^LIVE_MIN="\${3:-\([0-9]*\)}"$/\1/p' "$CHECK")
job=$(sed -n 's/^    timeout-minutes: \([0-9]*\)$/\1/p' "$wf/coverage-presence.yml")
cov=$(sed -n 's/^ *timeout-minutes: \([0-9]*\)$/\1/p' "$wf/coverage.yml" | LC_ALL=C sort -n | tail -1)
if [ -n "$live" ] && [ -n "$job" ] && [ "$job" -gt "$live" ]; then ok "the presence job timeout ($job) exceeds the live wait ($live)"; else
  no "presence job timeout '$job' does not exceed the live wait '$live'"; fi
if [ -n "$live" ] && [ -n "$cov" ] && [ "$live" -ge $(( 2 * cov )) ]; then ok "the live wait ($live) covers two coverage job timeouts ($cov)"; else
  no "live wait '$live' is under two coverage job timeouts '$cov'"; fi

# --- cannot see: every one of these must be loud ----------------------
out=$(run_it "ERR" ""); rc=$?
if [ "$rc" = 2 ]; then ok "an unreadable runs query exits 2, not clean"; else
  no "unreadable runs query returned $rc — silence is what this tool exists to catch"; fi

out=$(run_it "$(printf '11\tcompleted\tcancelled')" "11 ERR"); rc=$?
if [ "$rc" = 2 ]; then ok "an uncountable job list exits 2, not clean"; else
  no "uncountable jobs returned $rc"; fi

# --- usage ------------------------------------------------------------
bash "$CHECK" >/dev/null 2>&1
[ "$?" = 2 ] && ok "no head sha exits 2" || no "missing head sha did not exit 2"

BASH_BIN=$(command -v bash)
PATH="" "$BASH_BIN" "$CHECK" abc123 >/dev/null 2>&1
[ "$?" = 2 ] && ok "missing gh/jq exits 2 rather than reporting clean" || no "missing tooling did not exit 2"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
