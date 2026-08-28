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
#
# The second is the DELEGATION check, hardened in #872: a delegated
# self-test must be RUN by a workflow, not merely named in one. See the
# block near the end — every mention-but-do-not-run shape is driven,
# each execution shape is driven back as a preservation control, and the
# pre-#872 matcher is restored as a mutant so the narrowing is measured
# rather than asserted.
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

# DELEGATED IS RUN BY, NOT MENTIONED IN (#872).
#
# This check shipped as `grep -rq -- "$base"` over the workflow
# directory, so any text naming the file satisfied it. Measured on the
# real tree: deleting `run: bash scripts/test-staticcheck-tag-views.sh`
# from test.yaml, while the comment block above it still named the file,
# left this runner at exit 0 with the test printed as delegated — a
# 14-assertion suite removable from CI with nothing going red.
#
# Every fixture below is the passing one with the EXECUTION removed and
# a different kind of mention left behind. Each must be a finding.
wf_mention() { printf '%s\n' "jobs:" "  somejob:" "    steps:" "$@" > "$TMP/wf/ci.yaml"; }

wf_mention "      # - run: bash scripts/test-b.sh"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a YAML comment naming the file is not delegation" 1 "$TMP/deleg" "delegated to nowhere"

wf_mention "      - name: bash scripts/test-b.sh" "        run: echo nothing"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a step NAME containing the file is not delegation" 1 "$TMP/deleg" "delegated to nowhere"

wf_mention "      - run: echo hi # bash scripts/test-b.sh"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a trailing shell comment is not delegation" 1 "$TMP/deleg" "delegated to nowhere"

wf_mention "      - run: |" "          # bash scripts/test-b.sh" "          echo hi"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a shell comment inside a run block is not delegation" 1 "$TMP/deleg" "delegated to nowhere"

# THE PRESERVATION CONTROLS. Narrowing what counts is only safe if the
# forms that DO execute still count; a check that rejected everything
# would pass all four cases above.
wf_mention "      - name: Prove one invocation is not enough" "        run: bash scripts/test-b.sh"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a named step that runs it IS delegation" 0 "$TMP/deleg" "test-b.sh -> somejob"

wf_mention "      - run: |" "          set -e" "          bash scripts/test-b.sh"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a run: | block that runs it IS delegation" 0 "$TMP/deleg" "test-b.sh -> somejob"

wf_mention "      - run: bash scripts/test-b.sh # the real thing"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "an executed line with a trailing comment IS delegation" 0 "$TMP/deleg" "test-b.sh -> somejob"

# MUTATE BACK TO THE REJECTED SHAPE. Restore the line-wide grep and the
# comment fixture must go from a finding back to a pass. If it does not,
# the four cases above are not measuring the narrowing.
MUT="$TMP/mut-grepall.sh"
# The helper is copied alongside: the runner SOURCES it, and a mutant
# that cannot find its sibling refuses for want of a file rather than
# for want of the rule. A mutant refused by a different guard is not
# evidence about this one.
cp "$(dirname "$0")/workflow-shell-lines.sh" "$TMP/workflow-shell-lines.sh"
awk '
/^        case "\$workflow_shell" in$/ {
    print "        if [ ! -d \"$WORKFLOWS\" ] || ! grep -rq -- \"$base\" \"$WORKFLOWS\" 2>/dev/null; then"
    print "            undelegated+=(\"$base\")"
    print "        fi"
    skip = 1
    next
}
skip && /^        esac$/ { skip = 0; next }
skip { next }
{ print }
' "$RUNNER" > "$MUT"

# PROVE THE MUTATION LANDED, and prove it on CODE. An earlier version of
# this block asserted `grep -q 'grep -rq --' "$MUT"` and passed against
# a mutant that had not been built at all — because the runner's own
# header quotes the string `grep -rq -- "$base"` while explaining why it
# was removed. That is the defect this whole file is about, committed by
# the file itself: a marker satisfied by prose. So the checks below
# compare against the unmutated runner and against the code the mutant
# must no longer contain.
mut_code="$(grep -v '^[[:space:]]*#' "$MUT")"
mut_built=1
cmp -s "$RUNNER" "$MUT" && mut_built=0
case "$mut_code" in *'case "$workflow_shell" in'*) mut_built=0 ;; esac
case "$mut_code" in *'grep -rq -- "$base"'*) : ;; *) mut_built=0 ;; esac
if [ "$mut_built" -eq 1 ] && bash -n "$MUT" 2>/dev/null; then
    echo "PASS: mutant built, differs from the runner, and restores the line-wide grep"
    wf_mention "      # - run: bash scripts/test-b.sh"
    SELFTEST_DIR="$TMP/deleg" SELFTEST_WORKFLOWS="$TMP/wf" bash "$MUT" > "$TMP/out" 2>&1
    mrc=$?
    if [ "$mrc" -eq 0 ] && grep -q "test-b.sh -> somejob" "$TMP/out"; then
        echo "PASS: with it, a COMMENT reads as delegation -- the cases are live"
    else
        echo "FAIL: with it, a comment still failed (rc=$mrc); the narrowing is unmeasured"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
else
    echo "FAIL: could not build the line-wide-grep mutant; the narrowing is unverified"
    failures=$((failures + 1))
fi

# UNMUTATED CONTROL, after the mutation work.
wf_mention "      - run: bash scripts/test-b.sh"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "the unmutated runner still passes the control fixture" 0 "$TMP/deleg" "test-b.sh -> somejob"

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
