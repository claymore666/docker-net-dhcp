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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

RUNNER="$(dirname "$0")/run-gate-selftests.sh"
guarded_tmpdir TMP

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

# A WORKER THAT DIED WROTE NO EXIT STATUS (D41).
#
# Since the execution became a bounded worker pool, a subject's verdict
# reaches the parent through a file rather than through `$?`. That adds
# a state the serial version could not have: the file is ABSENT. It is
# absent exactly when the worker did not live to write it -- OOM-killed,
# SIGKILLed, the container reaped -- which is to say in the cases where
# something went most wrong. Read as a pass, those cases are a suite
# that silently did not run, which is the hole #542 exists to close,
# rebuilt one level down.
#
# Driven, not asserted: the fixture kills its own worker with SIGKILL,
# so no `.rc` is written for it and the runner must still call it a
# failure. The passing directory above is the preservation control --
# the same runner, subjects that DO write their status, exit 0 -- so
# this case cannot be satisfied by a runner that fails everything.
mkdir -p "$TMP/reaped"
mk "$TMP/reaped/test-a.sh" "exit 0"
mk "$TMP/reaped/test-b.sh" "kill -9 \$PPID"
check "a worker killed before it wrote its status is a failure, not a pass" \
    1 "$TMP/reaped" "1 gate self-test(s) failed"
check "and the reaped test is the one named" 1 "$TMP/reaped" "test-b.sh"

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

# #883: an executed line that only NAMES the file is not delegation.
wf_mention "      - run: echo scripts/test-b.sh runs elsewhere"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "an echo argument naming the file is not delegation" 1 "$TMP/deleg" "delegated to nowhere"

wf_mention "      - run: echo \"x; bash scripts/test-b.sh\""
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a separator inside a quoted string is not delegation" 1 "$TMP/deleg" "delegated to nowhere"

wf_mention "      - run: cat scripts/test-b.sh"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "reading the file is not delegation" 1 "$TMP/deleg" "delegated to nowhere"

wf_mention "      - run: out=\"\$(bash scripts/test-b.sh)\" || exit 1"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "a substitution that runs it IS delegation" 0 "$TMP/deleg" "test-b.sh -> somejob"

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
/^        case \$.\\n."\$workflow_cmds"\$.\\n. in$/ {
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
case "$mut_code" in *'"$workflow_cmds"'*) mut_built=0 ;; esac
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

# The version before #883, restored: any executed line containing the
# name counts. The echo fixture must read as delegation under it, or the
# #883 cases above are not measuring command position.
MUT883="$TMP/mut-883.sh"
sed -e "s#^\(    workflow_cmds=\"\$(workflow_shell_lines --raw \"\$WORKFLOWS\"\) | shell_command_words | sed 's|\.\*/||')\"#\1)\"#" \
    -e 's#^            \*\$.\\n."\$base"\$.\\n.\*) : ;;#            *"$base"*) : ;;#' "$RUNNER" > "$MUT883"
if ! cmp -s "$RUNNER" "$MUT883" && ! grep -q 'shell_command_words |' "$MUT883" \
        && grep -qF '*"$base"*) : ;;' "$MUT883" && bash -n "$MUT883"; then
    wf_mention "      - run: echo scripts/test-b.sh runs elsewhere"
    SELFTEST_DIR="$TMP/deleg" SELFTEST_WORKFLOWS="$TMP/wf" bash "$MUT883" > "$TMP/out" 2>&1
    mrc=$?
    if [ "$mrc" -eq 0 ] && grep -q "test-b.sh -> somejob" "$TMP/out"; then
        echo "PASS: with the pre-#883 match, an ECHO reads as delegation -- the cases are live"
    else
        echo "FAIL: with the pre-#883 match, the echo still failed (rc=$mrc)"
        failures=$((failures + 1))
    fi
else
    echo "FAIL: could not build the pre-#883 mutant"
    failures=$((failures + 1))
fi

# UNMUTATED CONTROL, after the mutation work.
wf_mention "      - run: bash scripts/test-b.sh"
SELFTEST_WORKFLOWS="$TMP/wf" \
    check "the unmutated runner still passes the control fixture" 0 "$TMP/deleg" "test-b.sh -> somejob"

# --- THE JOB ENVIRONMENT DOES NOT REACH A SUITE (#977) -------------------
#
# The runner hands every suite an environment with no GITHUB_ names in
# it. Two things were measured on this repository and are pinned here:
# a gate that reads GITHUB_EVENT_NAME decided a fixture's verdict from
# the job's own event, and three suites handed GITHUB_OUTPUT and
# GITHUB_STEP_SUMMARY to the tool under test, which appended to the
# real job's files.
#
# The planted suite reports BOTH: it writes to the two sinks if it can
# see them, and it fails if it can see GITHUB_ACTIONS.
mkdir -p "$TMP/env"
cat > "$TMP/env/test-env.sh" <<'PLANT'
#!/usr/bin/env bash
[ -z "${GITHUB_OUTPUT:-}" ] || echo "the suite reached GITHUB_OUTPUT" >> "$GITHUB_OUTPUT"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "the suite reached GITHUB_STEP_SUMMARY" >> "$GITHUB_STEP_SUMMARY"
if [ -n "${GITHUB_ACTIONS:-}" ]; then
    echo "GITHUB_ACTIONS reached the suite"
    exit 1
fi
echo "no job environment reached the suite"
PLANT

env_sinks() { printf '%s' "$(cat "$TMP/gho" "$TMP/ghs" 2>/dev/null)"; }

# ORTHOGONALITY FIRST. Run the planted suite with nothing between it and
# the environment. If it cannot fail and cannot write here, the two
# assertions below are satisfied by a suite that does nothing.
rm -f "$TMP/gho" "$TMP/ghs"
( GITHUB_ACTIONS=true GITHUB_OUTPUT="$TMP/gho" GITHUB_STEP_SUMMARY="$TMP/ghs" \
    bash "$TMP/env/test-env.sh" >/dev/null 2>&1 )
plain_rc=$?
if [ "$plain_rc" -eq 1 ] && [ -n "$(env_sinks)" ]; then
    echo "PASS: run directly, the planted suite fails and writes to both sinks (control)"
else
    echo "FAIL: the planted suite cannot fail or cannot write (rc=$plain_rc), so the"
    echo "      two cases below measure nothing"
    failures=$((failures + 1))
fi

# THE VERDICT AND THE WRITE, both gone, in one run.
rm -f "$TMP/gho" "$TMP/ghs"
( SELFTEST_DIR="$TMP/env" SELFTEST_WORKFLOWS="$TMP/wf-default" \
  GITHUB_ACTIONS=true GITHUB_OUTPUT="$TMP/gho" GITHUB_STEP_SUMMARY="$TMP/ghs" \
    bash "$RUNNER" > "$TMP/envout" 2>&1 )
env_rc=$?
if [ "$env_rc" -eq 0 ] && grep -q "no job environment reached the suite" "$TMP/envout"; then
    echo "PASS: through the runner the same suite sees no GITHUB_ACTIONS and passes"
else
    echo "FAIL: the scrubbed suite did not pass (rc=$env_rc)"
    sed 's/^/    /' "$TMP/envout"
    failures=$((failures + 1))
fi
if [ -z "$(env_sinks)" ]; then
    echo "PASS: and it wrote to neither GITHUB_OUTPUT nor GITHUB_STEP_SUMMARY"
else
    echo "FAIL: the suite still wrote into the job's sinks: $(env_sinks)"
    failures=$((failures + 1))
fi

# THE ALLOWLIST IS LIVE, driven the other way. Its shipped value is
# empty, so without this the keep branch never executes and a scrub that
# ignored the list entirely would pass every case above.
rm -f "$TMP/gho" "$TMP/ghs"
( SELFTEST_DIR="$TMP/env" SELFTEST_WORKFLOWS="$TMP/wf-default" \
  SELFTEST_ENV_ALLOW=GITHUB_ACTIONS \
  GITHUB_ACTIONS=true GITHUB_OUTPUT="$TMP/gho" GITHUB_STEP_SUMMARY="$TMP/ghs" \
    bash "$RUNNER" > "$TMP/allowout" 2>&1 )
allow_rc=$?
if [ "$allow_rc" -eq 1 ] && grep -q "GITHUB_ACTIONS reached the suite" "$TMP/allowout"; then
    echo "PASS: an allowed name reaches the suite, so the list decides and not the run"
else
    echo "FAIL: allowing GITHUB_ACTIONS changed nothing (rc=$allow_rc); the list is dead code"
    sed 's/^/    /' "$TMP/allowout"
    failures=$((failures + 1))
fi
if [ -z "$(env_sinks)" ]; then
    echo "PASS: and the two sinks stay scrubbed, so the list admits one name and not all"
else
    echo "FAIL: allowing one name admitted the sinks too: $(env_sinks)"
    failures=$((failures + 1))
fi

# --- THE GO PROBLEM MATCHER DOES NOT READ SUITE PROSE (#977) -------------
#
# actions/setup-go registers a matcher for `<path>.go: <message>`, and a
# suite that names a Go path in a result line had that line turned into
# a failure annotation on a job that passed. The runner removes the
# matcher for its own output. Driven in both directions, because a line
# printed unconditionally would be noise in every local run.
if grep -q "::remove-matcher owner=go::" "$TMP/envout"; then
    echo "PASS: with GITHUB_ACTIONS set the runner removes the Go problem matcher"
else
    echo "FAIL: the Go problem matcher was not removed, so suite prose is still read"
    echo "      as compiler output"
    failures=$((failures + 1))
fi
# WHERE the line is printed decides whether it does anything. A matcher
# applies to output as it streams, so a removal printed after the group
# replay leaves every annotation in place while every case above still
# passes.
matcher_line="$(grep -n '::remove-matcher owner=go::' "$TMP/envout" | head -1 | cut -d: -f1)"
first_group="$(grep -n '::group::' "$TMP/envout" | head -1 | cut -d: -f1)"
if [ -n "$matcher_line" ] && [ -n "$first_group" ] && [ "$matcher_line" -lt "$first_group" ]; then
    echo "PASS: and it removes the matcher before the first suite line is replayed"
else
    echo "FAIL: the removal is printed at line ${matcher_line:-none} and the first"
    echo "      replayed suite output at line ${first_group:-none}, so the matcher"
    echo "      already read the suite prose"
    failures=$((failures + 1))
fi
( SELFTEST_DIR="$TMP/env" SELFTEST_WORKFLOWS="$TMP/wf-default" \
    env -u GITHUB_ACTIONS bash "$RUNNER" > "$TMP/localout" 2>&1 )
if grep -q "::remove-matcher" "$TMP/localout"; then
    echo "FAIL: the runner emits a workflow command outside a job"
    failures=$((failures + 1))
else
    echo "PASS: and outside a job it emits no workflow command at all"
fi

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
