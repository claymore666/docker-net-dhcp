#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

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
# RUNS_JSON is what the stub returns for the "other runs in flight"
# query the classifier makes (#513). Set per-case by the parent; the
# default is "nothing else running", which is the POOL SHORT class.
RUNS_JSON='{"workflow_runs":[]}'

# The runs fixture goes to a FILE rather than into the generated script.
# Interpolating multi-line JSON into a `[ "$x" = ERR ]` test inside the
# stub turns it into a syntax error, and the stub then fails for a
# reason that has nothing to do with the case under test — which is
# exactly how the first cut of these cases produced four confident
# "POOL UNKNOWN" passes-as-failures.
stub_curl() {
    local dir="$1" json="$2" cancel_fail_n="${3:-0}" cancel_fail_code="${4:-403}"
    mkdir -p "$dir/bin"
    if [ "$RUNS_JSON" = "ERR" ]; then
        : > "$dir/runs.err"
    else
        printf '%s\n' "$RUNS_JSON" > "$dir/runs.json"
    fi
    cat > "$dir/bin/curl" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
    case "\$a" in
        */cancel)
            echo "\$a" >> "$CANCEL_LOG"
            # Real curl now prints %{http_code} and exits 0 even on an
            # HTTP error (-f is gone), so the stub must answer with a
            # status, not an exit code. Failing the first N attempts is
            # what makes the retry observable: a stub that always
            # refuses cannot tell "retried" from "gave up".
            n=\$(wc -l < "$CANCEL_LOG")
            if [ "\$n" -le "$cancel_fail_n" ]; then
                printf '%s' "$cancel_fail_code"
            else
                printf '202'
            fi
            exit 0
            ;;
        *"actions/runs?status=in_progress"*)
            [ -f "$dir/runs.err" ] && exit 1
            cat "$dir/runs.json"
            exit 0
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
    local json="$1" budget="$2" poll="$3" cancel_fail_n="${4:-0}" cancel_fail_code="${5:-403}"
    local dir
    dir=$(mktemp -d)
    stub_curl "$dir" "$json" "$cancel_fail_n" "$cancel_fail_code"
    # Backoff 0: these cases assert the retry COUNT, and sleeping
    # through the real one would add ~9s per case for nothing.
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GH_TOKEN=x \
        WATCHDOG_CANCEL_ATTEMPTS=3 WATCHDOG_CANCEL_BACKOFF=0 \
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
# Silence here would be the original bug wearing a fix's clothes — the
# pool still held, and now with a check that claims to have freed it.
# 999 refusals: every attempt fails, so this is the give-up path.
arm
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"queued"}]}' 2 1 999 403)
rc=$?
[ "$rc" = 1 ] && ok "a refused cancel still fails the watchdog" || no "a refused cancel exited $rc, want 1"
case "$out" in
    *"cancel request FAILED"*) ok "a refused cancel says so" ;;
    *) no "a refused cancel was silent — the run is still holding the pool and nobody is told" ;;
esac

# #611. The old message asserted the job was missing `actions: write`.
# It was not — the permission was present in the one real incident, and
# that guess sent the investigation down the wrong path. The status the
# API actually returned is the thing to report.
case "$out" in
    *"HTTP 403"*) ok "a refused cancel reports the status the API returned" ;;
    *) no "a refused cancel does not report its HTTP status, so the next one is undiagnosable too" ;;
esac
case "$out" in
    *"check that this job has"*) no "the message still asserts a cause it has not established (#611)" ;;
    *) ok "a refused cancel no longer guesses at the cause" ;;
esac
# The case above can only fail when a failure message is printed at all,
# so on a script that reported nothing it would pass having proved
# nothing — exactly the vacuous pass this file warns about elsewhere.
# Asserted against the source too, where it can always fail.
if grep -q "without it the API refuses with 403" "$WATCHDOG"; then
    no "the guessed 403 cause is still in the script (#611)"
else
    ok "the guessed 403 cause is gone from the script"
fi

# The retry itself. Asserted as a COUNT of POSTs, because "it retried"
# and "it gave up once" produce identical output otherwise.
n=$(cancels | wc -l)
[ "$n" -eq 3 ] && ok "a refused cancel is retried to the attempt limit (3 POSTs)" || \
    no "want 3 cancel POSTs at the limit, got $n — a single attempt is #611 unfixed"

# --- a cancel that fails then succeeds must NOT fail the watchdog -----
# The whole point of #611: the 2026-08-17 refusals were transient, so a
# later attempt landing is the expected shape, not an edge case. If this
# still exited 1 the retry would be decoration.
arm
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"queued"}]}' 2 1 1 403)
rc=$?
[ "$rc" = 1 ] && ok "a run cancelled on retry still fails the watchdog (the run WAS stuck)" || \
    no "a cancelled-on-retry run exited $rc, want 1 — the verdict is about the queue, not the cancel"
case "$out" in
    *"cancel accepted (HTTP 202) on attempt 2"*) ok "a cancel that lands on the second attempt is reported as accepted" ;;
    *) no "a cancel that succeeded on retry was not reported as accepted" ;;
esac
case "$out" in
    *"cancel request FAILED"*) no "a cancel that eventually landed still claimed to have failed" ;;
    *) ok "a cancel that eventually landed does not claim failure" ;;
esac
n=$(cancels | wc -l)
[ "$n" -eq 2 ] && ok "a cancel that lands on attempt 2 stops there" || \
    no "want 2 cancel POSTs, got $n — the loop does not stop on success"

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

# --- starvation must not read as a suite failure (#513) --------------
#
# The expensive failure mode is a red that carries no information about
# the change. On 2026-08-13 the third concurrent run reported six failed
# checks having executed none of its own diff, and "the debian bump
# broke the suite" is the natural misreading. The watchdog knows which
# it is; these cases pin that it SAYS so, on the annotation surface,
# before it cancels.

STUCK='{"jobs":[{"name":"main-1-suite","status":"queued"},{"name":"failure-suite","status":"queued"}]}'
OTHERS='{"workflow_runs":[
  {"id":999,"run_number":41,"name":"Integration","path":".github/workflows/integration.yml"},
  {"id":998,"run_number":40,"name":"Integration","path":".github/workflows/integration.yml"}
]}'

arm
RUNS_JSON="$OTHERS"
out=$(run_watchdog "$STUCK" 2 1); rc=$?
[ "$rc" = 1 ] && ok "starvation still fails the watchdog" || no "starvation exited $rc, want 1"
case "$out" in
    *"CLASS: STARVATION"*) ok "a run starved by other runs is classified as STARVATION" ;;
    *) no "starvation was not classified; the reader cannot tell it from a suite failure: $out" ;;
esac
case "$out" in
    *"::error title=CI STARVATION"*) ok "the class reaches the annotation surface" ;;
    *) no "no ::error annotation — the checks page still shows an unexplained red" ;;
esac
case "$out" in
    *"says NOTHING about the change under test"*) ok "it says the red carries no verdict on the diff" ;;
    *) no "it does not tell the reader the result is uninformative about their change" ;;
esac
case "$out" in
    *"#41"*|*"#40"*) ok "it names the runs that are holding the pool" ;;
    *) no "the competing runs are not named, so the claim cannot be checked: $out" ;;
esac
[ -n "$(cancels)" ] && ok "a starved run is still cancelled (#477 unchanged)" || \
    no "classification came at the cost of the cancel — the pool stays held"

# --- and an empty pool must NOT be called starvation ------------------
# Without this the classifier could return STARVATION unconditionally
# and every case above would pass.
arm
RUNS_JSON='{"workflow_runs":[]}'
out=$(run_watchdog "$STUCK" 2 1); rc=$?
case "$out" in
    *"CLASS: POOL SHORT"*) ok "nothing else running is classified POOL SHORT, not starvation" ;;
    *) no "an idle pool was misclassified — the two need opposite responses: $out" ;;
esac
case "$out" in
    *"CLASS: STARVATION"*) no "an idle pool was called starvation" ;;
    *) ok "STARVATION is not the unconditional answer" ;;
esac

# A run of a workflow that does NOT use the self-hosted pool is not why
# nobody took this job.
arm
RUNS_JSON='{"workflow_runs":[{"id":997,"run_number":39,"name":"Docs site","path":".github/workflows/pages.yml"}]}'
out=$(run_watchdog "$STUCK" 2 1)
case "$out" in
    *"CLASS: POOL SHORT"*) ok "a hosted-only run in flight does not count as competition" ;;
    *) no "a workflow that never touches the pool was blamed for the wait: $out" ;;
esac

# The run's own id must be excluded, or every stuck run blames itself.
arm
RUNS_JSON='{"workflow_runs":[{"id":12345,"run_number":42,"name":"Integration","path":".github/workflows/integration.yml"}]}'
out=$(run_watchdog "$STUCK" 2 1)
case "$out" in
    *"CLASS: POOL SHORT"*) ok "the stuck run does not count itself as competition" ;;
    *) no "the run blamed itself for holding the pool: $out" ;;
esac

# --- cannot see: unknown, never a guess -------------------------------
arm
RUNS_JSON="ERR"
out=$(run_watchdog "$STUCK" 2 1); rc=$?
[ "$rc" = 1 ] && ok "an unreadable run list still fails the watchdog" || no "unreadable run list exited $rc"
case "$out" in
    *"CLASS: POOL UNKNOWN"*) ok "an unreadable pool is reported as unknown" ;;
    *) no "an unreadable pool was resolved into a confident class: $out" ;;
esac
[ -n "$(cancels)" ] && ok "an unknown cause is still cancelled" || \
    no "an unreadable classification suppressed the cancel"
RUNS_JSON='{"workflow_runs":[]}'

# --- the machine-readable outputs -------------------------------------
arm
dir=$(mktemp -d)
RUNS_JSON="$OTHERS"
stub_curl "$dir" "$STUCK"
PATH="$dir/bin:$PATH" GATE_REPO=o/r GH_TOKEN=x \
    GITHUB_OUTPUT="$dir/gho" GITHUB_STEP_SUMMARY="$dir/ghs" \
    bash "$WATCHDOG" 12345 2 1 >/dev/null 2>&1
gho=$(cat "$dir/gho" 2>/dev/null); ghs=$(cat "$dir/ghs" 2>/dev/null)
rm -rf "$dir"
RUNS_JSON='{"workflow_runs":[]}'
case "$gho" in
    *"starved=true"*) ok "starvation sets starved=true for a triage query to filter on" ;;
    *) no "no starved= output; reds still cannot be told apart in bulk: $gho" ;;
esac
case "$gho" in
    *"wait_class=STARVATION"*) ok "the class is exported too" ;;
    *) no "wait_class not exported: $gho" ;;
esac
case "$ghs" in
    *"STARVATION"*) ok "the step summary states the class" ;;
    *) no "the step summary does not state the class: $ghs" ;;
esac
case "$ghs" in
    *"no verdict on the change"*) ok "the step summary says the run carries no verdict" ;;
    *) no "the step summary does not say the result is uninformative" ;;
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
