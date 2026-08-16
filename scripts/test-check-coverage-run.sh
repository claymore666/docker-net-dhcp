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
    local dir; dir=$(mktemp -d)
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
dir=$(mktemp -d)
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
