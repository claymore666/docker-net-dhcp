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
# mostly not tests. #829 split it into `test` and `gates`.
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
#   A. REFUSE unless both `test` and `gates` exist and both have steps.
#      A missing half is the absent-check-reads-as-green failure, and it
#      must send a human to look rather than render a clean verdict.
#   B. Every step in `test` is one this gate KNOWS, identified by name
#      (or by `uses:` with the version stripped, so a pin bump is not a
#      finding). An unknown step is a finding, not an allowance.
#   C. No non-comment line of any `run:` body in `test` invokes a
#      `scripts/*.sh` gate. This is the drift that actually happens: on
#      the tree this gate was written against, 48 of the 50 steps in
#      `gates` invoke a script and only the two environment-setup steps
#      do not, so a script invocation appearing in `test` is a gate that
#      moved.
#   D. `test` must actually run `go test`. Without this the gate is
#      satisfied by EMPTYING the job -- a `test` check that runs nothing
#      passes A, B and C perfectly and means less than before the split.
#   E. `gates` must invoke at least one `scripts/*.sh`. Same direction,
#      other half: gutting `gates` must not read as clean.
#
# Comment lines are excluded from C deliberately. This workflow's own
# prose names scripts -- the `Fuzz (short)` step's comment names
# check-fuzz-budget.sh -- and prose satisfies a substring search. A gate
# that counted comments would report the file's explanation of itself.
#
# WHAT IT CANNOT SEE, said plainly rather than left to be discovered
#
#   * A gate written INLINE in the `test` job as raw shell, invoking no
#     script, is invisible to C. Arm B is what stands between that and
#     nothing, and B is keyed on step NAMES: rename the step and B goes
#     red, which is the intended friction, but B cannot judge what a
#     step whose name it already knows has come to contain.
#   * It cannot see branch protection. Whether `test` and `gates` are
#     REQUIRED is a repository setting, not a fact in this tree, and no
#     gate running inside CI can read it without a token. If `gates` is
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
    echo "::error title=test/gates split cannot be judged::$*" >&2
    exit 2
}

[ -f "$WF" ] || refuse "$WF does not exist, so the boundary between the required check named \`test\` and the gate corpus could not be read."
[ -r "$WF" ] || refuse "$WF is not readable, so nothing below would be a measurement."

command -v python3 >/dev/null 2>&1 || refuse \
    "python3 is required to PARSE the workflow. This gate does not fall back to a line scan: a line scan cannot tell which job a step belongs to, and the job boundary is the entire subject."

out=$(python3 - "$WF" <<'PARSE'
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

SCRIPT_RE = re.compile(r"scripts/[A-Za-z0-9_.-]+\.sh")

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

for want in ("test", "gates"):
    job = jobs.get(want)
    if not isinstance(job, dict):
        sys.stderr.write("job `%s` is absent from %s\n" % (want, path))
        sys.exit(3)
    if not isinstance(job.get("steps"), list) or not job["steps"]:
        sys.stderr.write("job `%s` has no steps\n" % want)
        sys.exit(3)


def identity(step):
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
    body = step.get("run")
    if body is None:
        return []
    if not isinstance(body, str):
        body = str(body)
    return [ln for ln in body.splitlines() if not ln.strip().startswith("#")]


findings = []

# B -- every step in `test` is one this gate knows.
for step in jobs["test"]["steps"]:
    ident = identity(step)
    if ident not in ALLOWED:
        findings.append(
            "test: unrecognised step %r. Either it is not a test of this "
            "program and belongs in `gates`, or it is and this gate's "
            "ALLOWED set must be widened in the same commit -- deliberately, "
            "because widening it is widening what a required check named "
            "`test` is allowed to mean." % ident
        )

# C -- no gate script is invoked from `test`.
for step in jobs["test"]["steps"]:
    for ln in live_lines(step):
        for hit in SCRIPT_RE.findall(ln):
            findings.append(
                "test: step %r invokes %s. Gate scripts belong in `gates`; "
                "that separation is the whole of #829." % (identity(step), hit)
            )

# D -- `test` is not vacuous.
if not any(
    "go test" in ln
    for step in jobs["test"]["steps"]
    for ln in live_lines(step)
):
    findings.append(
        "test: no step runs `go test`. A required check named `test` that "
        "runs no suite is worse than the job this split replaced."
    )

# E -- `gates` is not vacuous.
gate_scripts = {
    hit
    for step in jobs["gates"]["steps"]
    for ln in live_lines(step)
    for hit in SCRIPT_RE.findall(ln)
}
if not gate_scripts:
    findings.append(
        "gates: no step invokes a scripts/*.sh gate. The corpus this job "
        "exists to carry has been emptied."
    )

for f in findings:
    print("FINDING\t%s" % f)
print(
    "COUNTED\t%d step(s) in test, %d in gates, %d distinct gate script(s)"
    % (len(jobs["test"]["steps"]), len(jobs["gates"]["steps"]), len(gate_scripts))
)
PARSE
)
rc=$?

if [ "$rc" -eq 3 ]; then
    refuse "$WF could not be parsed into two jobs with steps. PyYAML must be importable and both \`test\` and \`gates\` must exist: $(printf '%s' "$out" | tr '\n' ' ')"
fi
if [ "$rc" -ne 0 ]; then
    refuse "the parse of $WF exited $rc, which is neither a verdict nor a documented refusal."
fi

findings=$(printf '%s\n' "$out" | grep '^FINDING	' | sed 's/^FINDING	//')
counted=$(printf '%s\n' "$out" | grep '^COUNTED	' | sed 's/^COUNTED	//')

if [ -z "$counted" ]; then
    refuse "the parse produced no tally line, so its silence cannot be read as a clean result."
fi

if [ -n "$findings" ]; then
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        echo "::error title=The test/gates split has drifted::$f" >&2
    done <<<"$findings"
    echo "FAIL  the required check named \`test\` no longer means only tests ($counted)" >&2
    exit 1
fi

echo "PASS  \`test\` runs only the Go suite and \`gates\` carries the corpus ($counted)"
exit 0
