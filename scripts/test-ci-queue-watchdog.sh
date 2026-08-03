#!/usr/bin/env bash
# Tests for ci-queue-watchdog.sh (#392).
#
# The GitHub API is stubbed by putting a fake `curl` earlier on PATH,
# the same trick scripts/test-integration-run-gate.sh uses. The point is
# to exercise the real decision logic against real JSON shapes, not to
# assert that a mock was called.
#
# Since #477 the script does not only decide, it acts: it cancels the run
# it condemns. The stub therefore records cancel POSTs to a file, and the
# cases below assert in both directions — cancelled when stuck, never
# cancelled otherwise.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WATCHDOG="$HERE/ci-queue-watchdog.sh"
pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# stub_curl <dir> <json> [cancel-exit] makes every API read return
# <json>, and records any cancel POST in <dir>/cancelled so a test can
# assert the run was actually stopped rather than merely complained
# about. cancel-exit (default 0) is what the cancel POST returns, so the
# refused-cancel path is reachable.
#
# The stub distinguishes the two calls the way the real API does — by
# the URL — rather than by call order, so a test cannot pass because the
# script happened to issue its requests in the expected sequence.
stub_curl() {
    local dir="$1" json="$2" cancel_exit="${3:-0}"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/curl" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
    case "\$a" in
        */cancel)
            echo "\$a" >> "$CANCEL_LOG"
            exit $cancel_exit
            ;;
    esac
done
cat <<'JSON'
$json
JSON
EOF
    chmod +x "$dir/bin/curl"
}

# CANCEL_LOG is where the stub records cancel POSTs. It deliberately
# lives OUTSIDE the per-run temp dir and is set by the parent before the
# call: `out=$(run_watchdog ...)` runs in a subshell, so anything the
# function assigns to a variable is discarded when it returns.
#
# The first cut of this file did exactly that, and the cost was not a
# broken test but a silently passing one — `[ -z "$CANCELLED" ] && ok
# "a healthy run is never cancelled"` held for every input, including
# inputs that cancel. An assertion that cannot fail is worse than an
# absent one; it reports coverage that does not exist.
CANCEL_LOG=""

# cancels prints the cancel URLs issued by the most recent run_watchdog.
cancels() { cat "$CANCEL_LOG" 2>/dev/null; }

run_watchdog() {
    local json="$1" budget="$2" poll="$3" cancel_exit="${4:-0}"
    local dir
    dir=$(mktemp -d)
    stub_curl "$dir" "$json" "$cancel_exit"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GH_TOKEN=x \
        bash "$WATCHDOG" 12345 "$budget" "$poll" >"$dir/out" 2>&1
    local rc=$?
    cat "$dir/out"
    rm -rf "$dir"
    return $rc
}

# arm resets the cancel log so each case starts from "nothing cancelled".
# Called by the parent, never from inside a command substitution.
arm() {
    CANCEL_LOG=$(mktemp -u)/cancelled
    mkdir -p "$(dirname "$CANCEL_LOG")"
}
arm

# Guard the guard: if the stub ever stops recording, every "was
# cancelled" assertion below would fail loudly, but every "was NOT
# cancelled" assertion would pass for the wrong reason. Prove the
# recorder works before trusting either direction.
arm
out=$(run_watchdog '{"jobs":[{"name":"probe","status":"queued"}]}' 2 1)
if [ -n "$(cancels)" ]; then
    ok "the test stub records cancels (so a negative assertion means something)"
else
    no "the stub recorded no cancel on a stuck run — every not-cancelled assertion below is vacuous"
fi

# --- nothing queued --------------------------------------------------
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"in_progress"},{"name":"failure-suite","status":"in_progress"}]}' 2 1)
rc=$?
[ "$rc" = 0 ] && ok "running jobs are not stuck" || no "running jobs reported stuck (exit $rc)"

out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"completed"},{"name":"failure-suite","status":"completed"}]}' 2 1)
rc=$?
[ "$rc" = 0 ] && ok "a finished run is not stuck" || no "finished run reported stuck (exit $rc)"

# --- the real incident: one job assigned, one queued forever ---------
arm
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"in_progress"},{"name":"failure-suite","status":"queued"}]}' 2 1)
rc=$?
if [ "$rc" = 1 ]; then
    ok "a job queued past the budget fails the watchdog"
else
    no "a permanently queued job did not fail the watchdog (exit $rc)"
fi
case "$out" in
    *failure-suite*) ok "the failure names which job is stuck" ;;
    *) no "the failure does not name the stuck job" ;;
esac
case "$out" in
    *"concurrency group"*) ok "the failure explains the blast radius" ;;
    *) no "the failure does not explain why a stuck job matters" ;;
esac

# --- and it acts on it (#477) ----------------------------------------
# The whole point of the fix: before it, the watchdog printed the line
# above and the run went on holding the pool for another seven hours.
if [ -n "$(cancels)" ]; then
    ok "a stuck run is cancelled, not just reported"
else
    no "the watchdog reported a stuck run and left it running — #477, the harm it names is the harm it must end"
fi
case "$(cancels)" in
    *"/actions/runs/12345/cancel"*) ok "it cancels the run it was given" ;;
    *) no "cancel did not target run 12345, got: $(cancels)" ;;
esac

# The diagnosis must be complete before the axe falls: cancelling the
# run kills the job running this script, so anything printed after the
# POST may never reach the log.
if [ "${out%%Cancelling this run now*}" != "$out" ]; then
    case "${out##*Cancelling this run now}" in
        *"still queued: failure-suite"*)
            no "the stuck-job list is printed after the cancel is announced; it may be lost" ;;
        *) ok "the diagnosis is complete before the cancel is announced" ;;
    esac
else
    no "the output never says it is cancelling the run"
fi

# --- a partial pickup is cancelled too, deliberately ------------------
# The 2026-07-31 incident's shape: one runner assigned, the rest queued.
# The run cannot complete, so the assigned shard's result is not evidence
# anyone can act on — holding the pool for it is the worse trade. This
# asserts the decision rather than leaving it to whoever reads the code.
arm
out=$(run_watchdog '{"jobs":[{"name":"main-1-suite","status":"in_progress"},{"name":"main-2-suite","status":"queued"}]}' 2 1)
if [ -n "$(cancels)" ]; then
    ok "a partial pickup is cancelled too"
else
    no "a run with one assigned and one queued job was left holding the pool"
fi

# --- a cancel the API refuses must be loud ----------------------------
# 403 is the realistic case: the calling job lacking `actions: write`.
# Silence here would be the original bug wearing a fix's clothes — the
# pool still held, and now with a check that claims to have freed it.
arm
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"queued"}]}' 2 1 22)
rc=$?
[ "$rc" = 1 ] && ok "a refused cancel still fails the watchdog" || no "a refused cancel exited $rc, want 1"
case "$out" in
    *"cancel request FAILED"*) ok "a refused cancel says so" ;;
    *) no "a refused cancel was silent — the run is still holding the pool and nobody is told" ;;
esac
case "$out" in
    *"actions: write"*) ok "a refused cancel names the likely cause" ;;
    *) no "a refused cancel does not point at the missing permission" ;;
esac

# --- the escape hatch is opt-in and says so ---------------------------
arm
dir=$(mktemp -d)
stub_curl "$dir" '{"jobs":[{"name":"main-suite","status":"queued"}]}'
PATH="$dir/bin:$PATH" GATE_REPO=o/r GH_TOKEN=x WATCHDOG_NO_CANCEL=1 \
    bash "$WATCHDOG" 12345 2 1 >"$dir/out" 2>&1
rc=$?
nocancel=$(cancels)
out=$(cat "$dir/out")
rm -rf "$dir"
[ "$rc" = 1 ] && ok "WATCHDOG_NO_CANCEL still fails" || no "WATCHDOG_NO_CANCEL exited $rc, want 1"
[ -z "$nocancel" ] && ok "WATCHDOG_NO_CANCEL issues no cancel" || no "WATCHDOG_NO_CANCEL cancelled anyway"
case "$out" in
    *"diagnosed only"*) ok "WATCHDOG_NO_CANCEL says the run was left alone" ;;
    *) no "WATCHDOG_NO_CANCEL was silent about not cancelling" ;;
esac

# --- and a clean run is never cancelled -------------------------------
# The failure mode that would matter most: a watchdog that cancels runs
# which were fine.
arm
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"in_progress"},{"name":"failure-suite","status":"in_progress"}]}' 2 1)
[ -z "$(cancels)" ] && ok "a healthy run is never cancelled" || no "the watchdog cancelled a run with nothing queued"

arm
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"completed"}]}' 2 1)
[ -z "$(cancels)" ] && ok "a finished run is never cancelled" || no "the watchdog cancelled a finished run"

# --- it must not go quiet when it cannot see -------------------------
dir=$(mktemp -d)
mkdir -p "$dir/bin"
printf '#!/usr/bin/env bash\nexit 22\n' > "$dir/bin/curl"
chmod +x "$dir/bin/curl"
PATH="$dir/bin:$PATH" GATE_REPO=o/r GH_TOKEN=x bash "$WATCHDOG" 12345 2 1 >/dev/null 2>&1
rc=$?
rm -rf "$dir"
if [ "$rc" = 2 ]; then
    ok "an unreadable API is an error, not a clean bill of health"
else
    no "an unreadable API returned $rc; a watchdog that goes quiet when blind is the bug it exists to catch"
fi

# --- usage -----------------------------------------------------------
GATE_REPO="" bash "$WATCHDOG" >/dev/null 2>&1
[ "$?" = 2 ] && ok "missing arguments exit 2" || no "missing arguments did not exit 2"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
