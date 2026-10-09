#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Self-test for integration-suites.sh, the one step every integration lane
# runs its suites through (#733). make, sudo and id are fakes on PATH: make
# records its arguments and the variables it can see, then exits with the
# status its suite was given; sudo resets the environment as env_reset does.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SUBJECT="$HERE/integration-suites.sh"
# shellcheck source=scripts/tmpdir-guard.sh
. "$HERE/tmpdir-guard.sh"

pass=0
fail=0
ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

guarded_tmpdir BIN
LOG="selftest-suites-$$"
MAINLOG="/tmp/itest-${LOG}-main.log"
FAILLOG="/tmp/itest-${LOG}-failure.log"
cleanup_logs() { rm -f "/tmp/itest-${LOG}-main.log" "/tmp/itest-${LOG}-failure.log"; }
trap cleanup_logs EXIT

# The fakes read their settings from files beside them, because the fake
# sudo drops every variable a test could have used to configure them.
cat > "$BIN/make" <<'FAKE'
#!/usr/bin/env bash
here="$(cd "$(dirname "$0")" && pwd)"
suite=main
for a in "$@"; do [ "$a" = integration-test-failure ] && suite=failure; done
printf 'suite=%s argc=%d args=%s IT=%s REF=%s HEAP=%s COMMIT=%s FOO=%s SUDO=%s\n' \
    "$suite" "$#" "$*" "${ITEST_TIMEOUT-<unset>}" "${INTEGRATION_PLUGIN_REF-<unset>}" \
    "${ITEST_HEAP_BOUND_MB-<unset>}" "${NET_DHCP_EXPECT_COMMIT-<unset>}" \
    "${FOO-<unset>}" "${SUDO_MARK-0}" >> "$here/record"
echo "fake $suite suite output"
exit "$(cat "$here/rc-$suite" 2>/dev/null || echo 0)"
FAKE
cat > "$BIN/sudo" <<'FAKE'
#!/usr/bin/env bash
exec env -i SUDO_MARK=1 "$@"
FAKE
cat > "$BIN/id" <<'FAKE'
#!/usr/bin/env bash
here="$(cd "$(dirname "$0")" && pwd)"
cat "$here/uid" 2>/dev/null || echo 0
FAKE
chmod +x "$BIN/make" "$BIN/sudo" "$BIN/id"

# run <uid> <main rc> <failure rc> [VAR=value ...] -- the subject's exit status
run() {
    local uid="$1" rcm="$2" rcf="$3"
    shift 3
    rm -f "$BIN/record"
    printf '%s\n' "$uid" > "$BIN/uid"
    printf '%s\n' "$rcm" > "$BIN/rc-main"
    printf '%s\n' "$rcf" > "$BIN/rc-failure"
    env -u ITEST_TIMEOUT PATH="$BIN:$PATH" INTEGRATION_PLUGIN_REF=ref:it SUITES_LOG="$LOG" \
        SUITES_MAIN=integration-test SUITES_FAILURE=integration-test-failure \
        SUITES_MAIN_TIMEOUT=165m SUITES_FAILURE_TIMEOUT= "$@" \
        bash "$SUBJECT" >/dev/null 2>&1
    echo $?
}
record() { cat "$BIN/record" 2>/dev/null; }
has() { record | grep -F -- "$1" >/dev/null; }

# --- the exit status is the lane's verdict --------------------------------
[ "$(run 0 0 0)" = 0 ] && ok "both suites green: exit 0" || no "both suites green should exit 0"

rc="$(run 0 3 0)"
[ "$rc" = 3 ] && ok "a red main suite is the step's status, through tee" \
    || no "a red main suite behind tee exited $rc, not 3"
has "suite=failure" && ok "the failure suite still runs after a red main suite" \
    || no "a red main suite skipped the failure suite"

rc="$(run 0 0 4)"
[ "$rc" = 4 ] && ok "a red failure suite alone is the step's status" \
    || no "a red failure suite after a green main exited $rc, not 4"

rc="$(run 0 3 4)"
[ "$rc" = 3 ] && ok "both red: the main suite's status is reported" \
    || no "both suites red exited $rc, not the main suite's 3"

rc="$(run 0 0 4 SUITES_FAILURE=)"
[ "$rc" = 0 ] && ! has "suite=failure" && ok "no failure input: the failure suite is not run" \
    || no "an empty failure input still ran or failed on the failure suite (exit $rc)"

# --- refusals run no suite ------------------------------------------------
refused() {
    local label="$1" rc
    shift
    rc="$(run 0 0 0 "$@")"
    if [ "$rc" = 2 ] && [ -z "$(record)" ]; then
        ok "$label is refused before any suite runs"
    else
        no "$label: exit $rc, record '$(record)'"
    fi
}
refused "an empty main input" SUITES_MAIN=
refused "a log name with a slash" SUITES_LOG=../x
refused "an empty log name" SUITES_LOG=
refused "an empty plugin ref" INTEGRATION_PLUGIN_REF=
refused "an ITEST_TIMEOUT in the environment" ITEST_TIMEOUT=150m
refused "an empty ITEST_TIMEOUT in the environment" ITEST_TIMEOUT=

# --- what make sees -------------------------------------------------------
run 0 0 0 >/dev/null
has "suite=main argc=1 args=integration-test IT=165m REF=ref:it" \
    && ok "the main suite gets its own ceiling and the plugin ref" \
    || no "the main suite did not see ITEST_TIMEOUT=165m and the ref: $(record)"
has "suite=failure argc=1 args=integration-test-failure IT=<unset>" \
    && ok "an empty ceiling is not exported, so the Makefile default holds" \
    || no "an empty failure-timeout reached make: $(record)"

run 0 0 0 SUITES_MAIN="integration-test-shard SHARD=2 OF=9 SUITE=main" SUITES_FAILURE= >/dev/null
has "argc=4 args=integration-test-shard SHARD=2 OF=9 SUITE=main" \
    && ok "the main input is split into make's arguments in order" \
    || no "a shard's arguments did not reach make as four words: $(record)"

run 1000 0 0 ITEST_HEAP_BOUND_MB=256 NET_DHCP_EXPECT_COMMIT=abc FOO=bar >/dev/null
has "suite=main argc=1 args=integration-test IT=165m REF=ref:it HEAP=256 COMMIT=abc FOO=<unset> SUDO=1" \
    && ok "unprivileged: make runs under sudo and every variable the suites read is handed over" \
    || no "the sudo path lost a variable or ran without sudo: $(record)"
has "suite=failure argc=1 args=integration-test-failure IT=<unset>" \
    && ok "unprivileged: an empty ceiling is not handed over either" \
    || no "the sudo path handed an empty ceiling to the failure suite: $(record)"

run 0 0 0 >/dev/null
has "SUDO=0" && ! has "SUDO=1" && ok "as root: no sudo" || no "a root runner went through sudo: $(record)"

# --- the logs are this run's ------------------------------------------------
echo "stale output from an earlier run" > "$FAILLOG"
run 0 0 0 SUITES_FAILURE= >/dev/null
[ ! -e "$FAILLOG" ] && ok "a failure log left by an earlier run is removed" \
    || no "a stale failure log survived a run that had no failure suite"
grep -F "fake main suite output" "$MAINLOG" >/dev/null \
    && ok "the main suite's output is tee'd to its log" \
    || no "the main log does not hold the suite's output"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
