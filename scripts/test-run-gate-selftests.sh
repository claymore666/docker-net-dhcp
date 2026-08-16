#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for run-gate-selftests.sh (#542), driven through
# the SELFTEST_DIR seam against synthetic directories.
#
# No recursion risk: the runner under test is pointed at a temp
# directory, never at scripts/, so it never discovers this file.
#
# The case that matters most is the empty directory. A discovery loop
# that matches nothing and exits 0 is worse than the hand-maintained
# list it replaced — it reports success for running nothing. That case
# must be exit 2.
set -u

RUNNER="$(dirname "$0")/run-gate-selftests.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
# check NAME WANT_EXIT DIR GREP
check() {
    local name="$1" want_exit="$2" dir="$3" want_grep="$4"
    SELFTEST_DIR="$dir" SELFTEST_WORKFLOWS="${SELFTEST_WORKFLOWS:-$TMP/wf-default}" \
        bash "$RUNNER" > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

mk() { printf '%s\n' "#!/usr/bin/env bash" "$2" > "$1"; }

# THE case: discovery that finds nothing must not pass.
mkdir -p "$TMP/empty"
check "an empty directory exits 2, not 0" 2 "$TMP/empty" "matched nothing"

mkdir -p "$TMP/nodir"
rmdir "$TMP/nodir"
check "a missing directory exits 2" 2 "$TMP/nodir" "not a directory"

# A directory with non-matching files is still empty for our purposes.
mkdir -p "$TMP/decoys"
mk "$TMP/decoys/check-something.sh" "exit 0"
mk "$TMP/decoys/helper.sh" "exit 0"
check "files that are not test-*.sh do not count as coverage" 2 "$TMP/decoys" "matched nothing"

mkdir -p "$TMP/ok"
mk "$TMP/ok/test-a.sh" "exit 0"
mk "$TMP/ok/test-b.sh" "exit 0"
check "all passing exits 0 and reports the count" 0 "$TMP/ok" "All 2 gate self-test(s) run here passed"

mkdir -p "$TMP/onebad"
mk "$TMP/onebad/test-a.sh" "exit 0"
mk "$TMP/onebad/test-b.sh" "exit 1"
check "one failure exits 1" 1 "$TMP/onebad" "1 gate self-test(s) failed"
check "and names the test that failed" 1 "$TMP/onebad" "test-b.sh"

# The old list was fail-fast; a commit breaking three gates took three
# CI rounds to diagnose. Every test must run and all failures report.
mkdir -p "$TMP/manybad"
mk "$TMP/manybad/test-a.sh" "exit 1"
mk "$TMP/manybad/test-b.sh" "exit 1"
mk "$TMP/manybad/test-c.sh" "exit 0"
check "a failure does not stop the rest running" 1 "$TMP/manybad" "2 gate self-test(s) failed"
SELFTEST_DIR="$TMP/manybad" bash "$RUNNER" > "$TMP/out" 2>&1
if grep -q "test-c.sh" "$TMP/out"; then
    echo "PASS: a test after a failing one still runs"
else
    echo "FAIL: a test after a failing one still runs"
    failures=$((failures + 1))
fi

# Delegation: a test that declares an owning job is skipped here, but
# only if a workflow actually names it. "Delegated to nowhere" is the
# way this mechanism would rebuild the hole it exists to close.
mkdir -p "$TMP/deleg" "$TMP/wf"
mk "$TMP/deleg/test-a.sh" "exit 0"
printf '%s\n' "#!/usr/bin/env bash" "# gate-selftest-runs-in: somejob" "exit 1" > "$TMP/deleg/test-b.sh"
printf '%s\n' "jobs:" "  somejob:" "    run: bash scripts/test-b.sh" > "$TMP/wf/ci.yaml"
SELFTEST_WORKFLOWS="$TMP/wf" check "a declared test is skipped, not run" 0 "$TMP/deleg" "test-b.sh -> somejob"

printf '%s\n' "jobs:" "  somejob:" "    run: echo nothing" > "$TMP/wf/ci.yaml"
SELFTEST_WORKFLOWS="$TMP/wf" check "a test delegated to nowhere fails" 1 "$TMP/deleg" "delegated to nowhere"

# The real directory must be discoverable and non-trivial. This is the
# guard against the runner being wired to a path that happens to be
# empty in CI — the exact way this class of check goes quietly green.
real_count=$(ls "$(dirname "$0")"/test-*.sh 2>/dev/null | wc -l)
if [ "$real_count" -ge 10 ]; then
    echo "PASS: the committed scripts/ directory holds $real_count self-tests"
else
    echo "FAIL: only $real_count self-tests discovered in the committed scripts/ directory"
    failures=$((failures + 1))
fi

if [ "$failures" -eq 0 ]; then
    echo "all run-gate-selftests tests passed"
    exit 0
fi
echo "$failures failed"
exit 1
