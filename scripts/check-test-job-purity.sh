#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The `test` job must contain only tests of this program (#829).
#
# WHY THIS EXISTS
#
# `test` is a REQUIRED status check on dev and main. For a long time it
# ran the Go suite and then ~48 further steps that are not tests of this
# program at all -- release-promotion ordering, cosign documentation,
# watchdog wiring, lane hygiene, the gate self-test corpus. One name
# carried all of it: red said nothing about which of some fifty subjects
# had failed, and green said "the tests pass" about a job that was
# mostly not tests. #829 split it into `test` and `policy-gates`.
#
# The split is true by CONSTRUCTION on the day it lands, and after that
# it is protected by nothing but the reader. The next person with a gate
# to wire up appends it where gate steps have always gone, the job grows
# a policy check back, and #829 reopens itself in silence. That is the
# shape this repository keeps paying for: prose decays quietly, a check
# fails loudly. So the boundary gets an observer rather than a paragraph.
#
# WHAT IT CHECKS, and each arm's direction
#
#   A. REFUSE unless both `test` and `policy-gates` exist and both have steps.
#      A missing half is the absent-check-reads-as-green failure, and it
#      must send a human to look rather than render a clean verdict.
#   B. The steps of `test` are EXACTLY the set this gate knows, in both
#      directions -- identified by name, or by `uses:` with the version
#      stripped so a pin bump is not a finding. An unknown step is a
#      finding, and so is a KNOWN step that has gone missing. One
#      direction alone is not enough: checking only for strangers lets
#      the job be emptied one deletion at a time, which is the same
#      vacuity D exists to stop, arriving through the other door.
#   C. No non-comment line of any `run:` body in `test` MENTIONS a
#      `scripts/*.sh` gate. This is the drift that actually happens: on
#      the tree this gate was written against, 48 of the 50 steps in
#      `policy-gates` invoke a script and only the two environment-setup steps
#      do not, so a script invocation appearing in `test` is a gate that
#      moved.
#   D. `test` must actually run `go test`, IN COMMAND POSITION.
#      Without this arm the gate is satisfied by EMPTYING the job -- a
#      `test` check that runs nothing passes A and C perfectly and means
#      less than before the split.
#   E. `policy-gates` must INVOKE at least one `scripts/*.sh`, in command
#      position. Same direction, other half: gutting `policy-gates` must
#      not read as clean.
#   F. Neither job may lose the ability to report a red UNDER ITS OWN
#      NAME. No `if:` on either job or on any of their steps, no
#      `continue-on-error`, no `|| true`, `|| :` or `set +e` on a live
#      run line, and no `strategy:` -- a matrix renames the check to
#      `test (x)` and `test` itself stops existing, so the context
#      branch protection requires goes permanently absent. D and E ask
#      whether the work is written down; F asks whether its failure can
#      still reach a human. A required check that is green because it
#      skipped, or because the one step that matters was allowed to
#      fail, is the same absent-check-reads-as-green failure this split
#      was filed about, wearing a repository setting instead of a job
#      name.
#
# THE SAME SPELLING IS MATCHED TWO DIFFERENT WAYS, AND THAT IS THE POINT
#
# C and E both look for `scripts/*.sh`, and they must not share a
# predicate, because a match means opposite things to them. For C a
# match is a FINDING, so the broad substring is right: over-matching
# costs a false red, and red sends a human to look. For E a match is a
# PASS, so the broad substring is exactly wrong -- `echo "scripts/
# check-lock-discipline.sh runs elsewhere"` satisfied it, and the gate
# certified an emptied corpus (measured, #829 review round 1). An arm
# whose match is a pass must be anchored in COMMAND POSITION, the same
# boundary scripts/workflow-shell-lines.sh names in as many words: it
# answers "is this token in something the workflow runs", not "is this
# token the command being run". D was defeated identically, by
# `echo "we no longer go test in CI"`.
#
# Comment lines are excluded from C deliberately. This workflow's own
# prose names scripts -- the `Fuzz (short)` step's comment names
# check-fuzz-budget.sh -- and prose satisfies a substring search. A gate
# that counted comments would report the file's explanation of itself.
#
# WHAT IT CANNOT SEE, said plainly rather than left to be discovered.
# This is a BOUND, not a list of everything: each entry below is a shape
# measured to pass, and the escapes nobody has thought of yet outnumber
# them.
#
#   * A gate written INLINE in the `test` job as raw shell, invoking no
#     script, is invisible to C. Arm B is what stands between that and
#     nothing, and B is keyed on step NAMES: rename the step and B goes
#     red, which is the intended friction, but B cannot judge what a
#     step whose name it already knows has come to contain.
#   * INDIRECTION defeats C, D and E alike, because each reads one line
#     of shell and none follows a call. `make check-all-policy-gates`
#     inside an ALLOWED step runs the corpus from `test` and C says
#     nothing; a `go test` reached only through a Makefile target or a
#     wrapper script leaves D red rather than quiet, which is the safe
#     direction, but a `scripts/*.sh` reached the same way from
#     `policy-gates` leaves E red for a job that is in fact carrying its
#     corpus. Neither is closed here: following the call would mean
#     evaluating the shell.
#   * DEAD CODE in command position satisfies D and E. `if false; then
#     go test ./...; fi` is a live line, anchored, and never executes.
#     Reachability is not decidable from a line scan, and F closes the
#     spellings of this that a repository setting can express -- `if:`
#     and `continue-on-error` -- not the ones the shell can.
#   * A HERE DOCUMENT body is indistinguishable from shell to this gate,
#     so a script name at the start of a heredoc line satisfies E.
#   * F does not enumerate every way to discard an exit status. `|| echo
#     "..."`, `|| exit 0`, a trailing `; true`, and a pipeline whose
#     last stage always succeeds all pass. The three it does name are
#     the ones with no legitimate use in these two jobs; a wider net
#     would red-flag ordinary shell in a future gate step, and a gate
#     that cries wolf gets discharged.
#   * E asks for AT LEAST ONE invocation, so a `policy-gates` reduced
#     from 48 gates to 1 still passes. That bound is deliberate -- the
#     corpus grows every week and a count here would be a stale number
#     with no observer -- and it is backstopped from the other side by
#     scripts/check-local-lane.sh, which fails when a `check-*.sh` in
#     the tree is run by nothing.
#   * F reads the workflow as written, and a job can also stop reporting
#     for reasons no line of it expresses: the file being DELETED (this
#     gate is invoked from the very workflow it judges, so it does not
#     run at all), a `runs-on` label with no runner behind it, or the
#     `PURITY_WORKFLOW` seam being pointed at some other file. The first
#     two are absent required checks, which block; the third would need
#     a committed decoy workflow and a changed invocation, and
#     scripts/test-check-test-job-purity.sh asserts the invocation is in
#     command position in the real file.
#   * It cannot see branch protection. Whether `test` and `policy-gates` are
#     REQUIRED is a repository setting, not a fact in this tree, and no
#     gate running inside CI can read it without a token. If `policy-gates` is
#     not a required check then everything it guards -- including this
#     file -- is advisory, and this gate will still exit 0.
#   * D is keyed on the literal `go test`, which is a spelling. There is
#     no other way to run this program's suite today; if that changes,
#     this arm goes red rather than quiet, which is the right direction.
#
# Usage: check-test-job-purity.sh [<workflow>]
# Env:   PURITY_WORKFLOW  workflow to inspect -- the seam the self-test
#                         drives. Default .github/workflows/test.yaml.
# Exit:  0 the split holds (the NORMAL outcome)
#        1 the split has drifted -- each finding is named
#        2 CANNOT JUDGE -- refusal, never a pass

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

WF="${1:-${PURITY_WORKFLOW:-.github/workflows/test.yaml}}"

refuse() {
    echo "::error title=test/policy-gates split cannot be judged::$*" >&2
    exit 2
}

[ -f "$WF" ] || refuse "$WF does not exist, so the boundary between the required check named \`test\` and the gate corpus could not be read."
[ -r "$WF" ] || refuse "$WF is not readable, so nothing below would be a measurement."

command -v python3 >/dev/null 2>&1 || refuse \
    "python3 is required to PARSE the workflow. This gate does not fall back to a line scan: a line scan cannot tell which job a step belongs to, and the job boundary is the entire subject."

err="$(mktemp)"
trap 'rm -f "$err"' EXIT

out=$(python3 - "$WF" 2>"$err" <<'PARSE'
import re
import sys

try:
    import yaml
except ImportError:
    sys.stderr.write("PyYAML is not importable by this python3\n")
    sys.exit(3)

# The steps `test` is allowed to contain. This is an enumeration, and an
# enumeration without a watcher is an unrun checklist -- so it is
# compared against the parsed workflow on every run rather than read.
# A `uses:` step is identified with its version stripped, so a pin bump
# is not a finding; a `run:` step is identified by its name.
ALLOWED = {
    "uses:actions/checkout",
    "uses:actions/setup-go",
    "go.mod tidy check",
    "Build",
    "Vet",
    "Format check",
    "Test (with race detector)",
    "Fuzz (short)",
}

# Two predicates for one spelling, because a match means opposite
# things to C and to E. See the header: broad where a match is a
# FINDING, anchored in command position where a match is a PASS.
SCRIPT_MENTION = re.compile(r"scripts/[A-Za-z0-9_.-]+\.sh")
SCRIPT_INVOKE = re.compile(
    r"^\s*(?:(?:bash|sh)\s+)?(?:\./)?(scripts/[A-Za-z0-9_.-]+\.sh)(?:\s|$)"
)
# The same anchor for arm D. Every `go test` in this workflow today is
# either the whole command or the body of a loop, so leading whitespace
# is allowed and nothing else is.
GO_TEST_INVOKE = re.compile(r"^\s*go\s+test(?:\s|$)")

# Arm F: the ways a run line can discard the exit status it was supposed
# to report. Bounded on purpose -- see the header for what is NOT here
# and why widening it would cost more than it buys.
SWALLOW = (
    (re.compile(r"\|\|\s*true(?:\s|;|$)"), "`|| true`"),
    (re.compile(r"\|\|\s*:(?:\s|;|$)"), "`|| :`"),
    (re.compile(r"(?:^|;|\s)set\s+\+[A-Za-z]*e"), "`set +e`"),
)

path = sys.argv[1]
try:
    with open(path) as fh:
        doc = yaml.safe_load(fh)
except Exception as exc:                      # noqa: BLE001 - any parse failure refuses
    sys.stderr.write("unparsable: %s\n" % exc)
    sys.exit(3)

if not isinstance(doc, dict):
    sys.stderr.write("the workflow is not a mapping\n")
    sys.exit(3)
jobs = doc.get("jobs")
if not isinstance(jobs, dict):
    sys.stderr.write("the workflow has no `jobs` mapping\n")
    sys.exit(3)

# The two job names are the load-bearing literals in this file: they are
# the strings branch protection is configured with. `policy-gates` is
# NOT spelled `policy-gates` because integration.yml already has a job named
# `gate`, and a one-character slip in a hand-typed required-context list
# requires the wrong job while leaving this one advisory.
TEST_JOB = "test"
GATES_JOB = "policy-gates"

for want in (TEST_JOB, GATES_JOB):
    job = jobs.get(want)
    if not isinstance(job, dict):
        sys.stderr.write("job `%s` is absent from %s\n" % (want, path))
        sys.exit(3)
    if not isinstance(job.get("steps"), list) or not job["steps"]:
        sys.stderr.write("job `%s` has no steps\n" % want)
        sys.exit(3)


def identity(step):
    # A step that is not a mapping -- a stray `-` leaving a null entry, a
    # bare string -- used to reach the caller as a REFUSAL, because
    # `"uses" in None` raises and the shell maps a crash to exit 2. That
    # fails closed, so nothing got through, but it is the wrong verdict
    # and it is the same class already fixed for `run: true` below: a
    # step like that is still a step in `test`, and the honest answer is
    # a finding naming it.
    if not isinstance(step, dict):
        return "<non-mapping step %r>" % (step,)
    if "uses" in step:
        return "uses:" + str(step["uses"]).split("@")[0]
    return step.get("name") or "<unnamed run step>"


def live_lines(step):
    # `run: true` is a BOOLEAN to a YAML parser, not the string "true",
    # and `run: 42` is an int. Coercing rather than assuming str is not
    # defensive noise: the first draft of this gate crashed on exactly
    # that input, and a crash here reaches the caller as a refusal --
    # the safe direction, but the wrong verdict. The right one is a
    # finding, because a step like that is still a step in `test`.
    if not isinstance(step, dict):
        return []
    body = step.get("run")
    if body is None:
        return []
    if not isinstance(body, str):
        body = str(body)
    return [ln for ln in body.splitlines() if not ln.strip().startswith("#")]


findings = []

# B -- the steps of `test` are EXACTLY the set this gate knows.
present = [identity(step) for step in jobs[TEST_JOB]["steps"]]
for ident in present:
    if ident not in ALLOWED:
        findings.append(
            "test: unrecognised step %r. Either it is not a test of this "
            "program and belongs in `%s`, or it is and this gate's ALLOWED "
            "set must be widened in the same commit -- deliberately, because "
            "widening it is widening what a required check named `test` is "
            "allowed to mean." % (ident, GATES_JOB)
        )
for ident in sorted(ALLOWED - set(present)):
    findings.append(
        "test: known step %r is GONE. `test` is a required check, and a "
        "required check shrinks silently: no name disappears from the "
        "status list when the job stops doing something. If the step is "
        "genuinely obsolete, remove it from this gate's ALLOWED set in "
        "the same commit -- narrowing what `test` means is a decision, "
        "not a deletion." % ident
    )

# C -- no gate script is MENTIONED on a live line of `test`.
for step in jobs[TEST_JOB]["steps"]:
    for ln in live_lines(step):
        for hit in SCRIPT_MENTION.findall(ln):
            findings.append(
                "test: step %r invokes %s. Gate scripts belong in the policy job; "
                "that separation is the whole of #829." % (identity(step), hit)
            )

# D -- `test` is not vacuous: `go test` in COMMAND POSITION.
if not any(
    GO_TEST_INVOKE.search(ln)
    for step in jobs[TEST_JOB]["steps"]
    for ln in live_lines(step)
):
    findings.append(
        "test: no step RUNS `go test` -- naming it is not running it. A "
        "required check named `test` that runs no suite is worse than the "
        "job this split replaced."
    )

# E -- `policy-gates` is not vacuous: a script in COMMAND POSITION.
gate_scripts = set()
for step in jobs[GATES_JOB]["steps"]:
    for ln in live_lines(step):
        hit = SCRIPT_INVOKE.search(ln)
        if hit:
            gate_scripts.add(hit.group(1))
if not gate_scripts:
    findings.append(
        "policy-gates: no step RUNS a scripts/*.sh gate -- naming one is not "
        "running it. The corpus this job exists to carry has been emptied."
    )

# F -- neither job may swallow its own result.
for jname in (TEST_JOB, GATES_JOB):
    job = jobs[jname]
    if "if" in job:
        findings.append(
            "%s: the job carries an `if:`. A required check that can be "
            "SKIPPED is reported green without running, which is the "
            "absent-check-reads-as-green failure #829 exists to remove."
            % jname
        )
    if job.get("continue-on-error"):
        findings.append(
            "%s: the job sets `continue-on-error`. Its failure would no "
            "longer be able to turn the check red." % jname
        )
    if "strategy" in job:
        findings.append(
            "%s: the job has a `strategy:`. A matrix renames the check to "
            "`%s (...)`, so the context branch protection requires goes "
            "permanently ABSENT -- which blocks rather than passes, but "
            "stalls every open pull request until someone edits a "
            "repository setting. If the matrix is wanted, the required "
            "contexts must be changed in the same breath." % (jname, jname)
        )
    for step in job["steps"]:
        ident = identity(step)
        if isinstance(step, dict) and "if" in step:
            findings.append(
                "%s: step %r carries an `if:`. The check stays green when "
                "the step does not run." % (jname, ident)
            )
        if isinstance(step, dict) and step.get("continue-on-error"):
            findings.append(
                "%s: step %r sets `continue-on-error`, so its failure "
                "cannot turn the check red." % (jname, ident)
            )
        for ln in live_lines(step):
            for rx, label in SWALLOW:
                if rx.search(ln):
                    findings.append(
                        "%s: step %r discards an exit status with %s. The "
                        "step still appears in the job and still runs; only "
                        "its verdict is gone." % (jname, ident, label)
                    )

for f in findings:
    print("FINDING\t%s" % f)
print(
    "COUNTED\t%d step(s) in test, %d in policy-gates, %d distinct gate script(s)"
    % (len(jobs[TEST_JOB]["steps"]), len(jobs[GATES_JOB]["steps"]), len(gate_scripts))
)
PARSE
)
rc=$?

if [ "$rc" -eq 3 ]; then
    refuse "$WF could not be parsed into two jobs with steps, so the boundary was not judged. The parser said: $(tr '\n' ' ' < "$err"). PyYAML must be importable and both \`test\` and \`policy-gates\` must exist with steps."
fi
if [ "$rc" -ne 0 ]; then
    refuse "the parse of $WF exited $rc, which is neither a verdict nor a documented refusal. The parser said: $(tr '\n' ' ' < "$err")"
fi

findings=$(printf '%s\n' "$out" | grep '^FINDING	' | sed 's/^FINDING	//')
counted=$(printf '%s\n' "$out" | grep '^COUNTED	' | sed 's/^COUNTED	//')

if [ -z "$counted" ]; then
    refuse "the parse produced no tally line, so its silence cannot be read as a clean result."
fi

if [ -n "$findings" ]; then
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        echo "::error title=The test/policy-gates split has drifted::$f" >&2
    done <<<"$findings"
    echo "FAIL  the required check named \`test\` no longer means only tests ($counted)" >&2
    exit 1
fi

echo "PASS  \`test\` runs only the Go suite and \`policy-gates\` carries the corpus ($counted)"
exit 0
