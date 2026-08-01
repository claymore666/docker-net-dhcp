#!/usr/bin/env bash
# Tests for ci-queue-watchdog.sh (#392).
#
# The GitHub API is stubbed by putting a fake `curl` earlier on PATH,
# the same trick scripts/test-integration-run-gate.sh uses. The point is
# to exercise the real decision logic against real JSON shapes, not to
# assert that a mock was called.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WATCHDOG="$HERE/ci-queue-watchdog.sh"
pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

# stub_curl <json> makes every API read return <json>.
stub_curl() {
    local dir="$1" json="$2"
    mkdir -p "$dir/bin"
    cat > "$dir/bin/curl" <<EOF
#!/usr/bin/env bash
cat <<'JSON'
$json
JSON
EOF
    chmod +x "$dir/bin/curl"
}

run_watchdog() {
    local json="$1" budget="$2" poll="$3"
    local dir
    dir=$(mktemp -d)
    stub_curl "$dir" "$json"
    PATH="$dir/bin:$PATH" GATE_REPO=o/r GH_TOKEN=x \
        bash "$WATCHDOG" 12345 "$budget" "$poll" >"$dir/out" 2>&1
    local rc=$?
    cat "$dir/out"
    rm -rf "$dir"
    return $rc
}

# --- nothing queued --------------------------------------------------
out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"in_progress"},{"name":"failure-suite","status":"in_progress"}]}' 2 1)
rc=$?
[ "$rc" = 0 ] && ok "running jobs are not stuck" || no "running jobs reported stuck (exit $rc)"

out=$(run_watchdog '{"jobs":[{"name":"main-suite","status":"completed"},{"name":"failure-suite","status":"completed"}]}' 2 1)
rc=$?
[ "$rc" = 0 ] && ok "a finished run is not stuck" || no "finished run reported stuck (exit $rc)"

# --- the real incident: one job assigned, one queued forever ---------
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
