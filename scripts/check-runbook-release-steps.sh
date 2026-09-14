#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The runbook's release walkthrough must describe the workflow that
# exists (#972).
#
# WHAT WENT WRONG. docs/release-runbook.md walks the release run step by
# step: which jobs run, in which order, with which steps, and how many
# jobs the promotion waits on. Adding the Docker Hub alias added three
# steps to the `release` job, one to `promote-latest`, two install-proof
# jobs, and moved two `needs:` counts from six to eight -- and the
# walkthrough said none of it. Nothing compared the two files, so the
# page a releaser follows at 2am described a workflow that had not
# existed for a release and a half.
#
# That is the failure this repository keeps finding in other shapes: a
# transcription with no observer. A releaser reading the old text would
# have watched for six green jobs, seen eight, and had no way to tell
# "two extra" from "two that should not be there".
#
# WHAT IT CHECKS
#
#   1. THE WALKTHROUGH DECLARES WHICH JOBS IT WALKS, in an HTML comment
#      in the page itself. Rules 2 and 3 range over that declaration, so
#      it is refused when it is missing, empty, or names a job the
#      workflow does not have. A gate that guessed which jobs the page
#      had "taken on" would drop a job silently the day someone stopped
#      describing it, which is precisely the direction being closed.
#
#   2. EVERY STEP OF A DECLARED JOB IS NAMED IN THE PAGE. Whitespace is
#      collapsed and markdown emphasis stripped on both sides first: the
#      page wraps step names across lines and bolds half of them, and a
#      gate that could not read its own documentation's formatting would
#      be answered by unwrapping the prose.
#
#   3. THE INSTALL PROOFS MATCH, BOTH WAYS. Every `verify-install*` job
#      in the workflow is named on the page, and every `verify-install*`
#      name on the page is a job in the workflow. The second direction
#      is the one that catches a removed job still being watched for.
#
#   4. THE COUNTS ARE THE DERIVED COUNTS. The page tells a releaser how
#      many jobs `promote-latest` and `github-release` wait on. Those
#      sentences are read out of the page and compared with the length
#      of each job's own `needs:` list. "All six of the above are green"
#      survived two added proofs, and a releaser has no way to notice.
#
# Usage: bash scripts/check-runbook-release-steps.sh [runbook] [workflow]
# Exit:  0 the walkthrough matches the workflow
#        1 a step, a job or a count disagrees
#        2 CANNOT JUDGE -- a file is unreadable, or the declaration is
#          missing or empty, which would make every rule vacuous
set -uo pipefail

RUNBOOK="${1:-docs/release-runbook.md}"
WORKFLOW="${2:-.github/workflows/release.yml}"

refuse() {
    echo "::error title=The release walkthrough cannot be judged::$*" >&2
    exit 2
}

[ -f "$RUNBOOK" ]  || refuse "no runbook at '$RUNBOOK'."
[ -f "$WORKFLOW" ] || refuse "no workflow at '$WORKFLOW'."

report=$(python3 - "$RUNBOOK" "$WORKFLOW" <<'PARSE'
import re, sys

runbook_path, workflow_path = sys.argv[1], sys.argv[2]
rb = open(runbook_path, encoding="utf-8").read()
wf_lines = open(workflow_path, encoding="utf-8").read().split("\n")

JOB   = re.compile(r"^  ([A-Za-z0-9_-]+):\s*$")
STEP  = re.compile(r"^      - name:\s*(.+?)\s*$")
NEEDS = re.compile(r"^    needs:\s*\[(.*)\]\s*$")
MARK  = re.compile(r"^\s*#\s*runbook-walkthrough:")
STALE = re.compile(r"<!--\s*release-walkthrough:")

# THE WALKED SET IS DECLARED IN THE WORKFLOW, NOT IN THE PAGE.
#
# It used to be a `<!-- release-walkthrough: ... -->` comment in the
# runbook, and that made the subject of the comparison the thing being
# compared. Measured: narrow the declaration from `release,
# promote-latest` to `release` and delete the alias promotion from the
# prose, and this gate printed "walks release step for step" and exited
# 0. Rules 3 and 4 are unconditional, so the proofs and the counts
# stayed covered; the step lists did not. A page that stops describing
# a job stopped being judged on it, which is the direction the gate
# exists to close.
jobs, steps, needs, walked, cur = [], {}, {}, [], None
for line in wf_lines:
    m = JOB.match(line)
    if m:
        cur = m.group(1)
        jobs.append(cur)
        steps[cur] = []
        continue
    if cur is None:
        continue
    if line.lstrip().startswith("#"):
        if MARK.match(line) and cur not in walked:
            walked.append(cur)
        continue
    m = STEP.match(line)
    if m:
        steps[cur].append(m.group(1))
        continue
    m = NEEDS.match(line)
    if m:
        needs[cur] = [n.strip() for n in m.group(1).split(",") if n.strip()]

if not jobs:
    print("REFUSE\tderived no jobs from %s; every rule below ranges over jobs, "
          "so this would pass having measured nothing." % workflow_path)
    raise SystemExit(0)

def flat(text):
    return re.sub(r"\s+", " ", re.sub(r"[*`_]", "", text))

rb_flat = flat(rb)

if STALE.search(rb):
    print("REFUSE\t%s still carries a `<!-- release-walkthrough: ... -->` "
          "declaration. The walked set now lives in %s, on the jobs "
          "themselves, and two declarations of one set would disagree the "
          "day one of them was edited." % (runbook_path, workflow_path))
    raise SystemExit(0)

if not walked:
    print("REFUSE\tno job in %s carries a `# runbook-walkthrough:` marker. "
          "Rule 2 ranges over the jobs that carry it, so without one this "
          "gate reports a clean walkthrough having compared no step lists "
          "at all." % workflow_path)
    raise SystemExit(0)

findings = []

# `walked` is built from the jobs of this file as they are walked, so
# every name in it is a key of `steps` by construction. The version
# that read the set out of the page needed a "declares a job that does
# not exist" finding here; this one cannot express that state, and a
# branch with one possible verdict is worse than no branch -- it reads
# as coverage and measures nothing.
for job in walked:
    if not steps[job]:
        findings.append("job '%s' is declared walked but has no named steps in "
                        "%s, so rule 2 would pass over nothing for it."
                        % (job, workflow_path))
        continue
    for s in steps[job]:
        if flat(s) not in rb_flat:
            findings.append("job '%s' runs a step named '%s' that %s never "
                            "mentions. A releaser following the page would "
                            "watch a run with a step in it nobody described."
                            % (job, s, runbook_path))

# 3. the install proofs, both directions
proofs = [j for j in jobs if j.startswith("verify-install")]
for j in proofs:
    if j not in rb_flat:
        findings.append("%s runs the install proof '%s' and %s never names it. "
                        "The page is what a releaser watches, so an unnamed "
                        "proof is one nobody is waiting for."
                        % (workflow_path, j, runbook_path))
for j in sorted(set(re.findall(r"verify-install[a-z0-9-]*", rb_flat))):
    if j not in jobs:
        findings.append("%s tells a releaser to watch '%s', which is not a job "
                        "in %s." % (runbook_path, j, workflow_path))

# 4. the counts the page states about each job's own needs:
WORDS = {"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
         "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
         "twelve": 12}

def stated(pattern):
    out = []
    for m in re.finditer(pattern, rb_flat):
        w = m.group(1).lower()
        out.append(WORDS.get(w, w if not w.isdigit() else int(w)))
    return out

COUNTS = [
    ("promote-latest", r"only after all ([a-z]+|\d+) of the above are green"),
    ("github-release", r"needs the same ([a-z]+|\d+) jobs"),
]
for job, pattern in COUNTS:
    want = len(needs.get(job, []))
    got = stated(pattern)
    if want == 0:
        findings.append("job '%s' has no `needs:` list this gate could read, so "
                        "the count the page states about it cannot be checked."
                        % job)
        continue
    if not got:
        findings.append("%s states no count for the jobs '%s' waits on. That "
                        "sentence is how a releaser knows when the run is "
                        "complete; it was 'six' for two releases after the "
                        "answer became eight." % (runbook_path, job))
        continue
    for g in got:
        if g != want:
            findings.append("%s says '%s' waits on %s jobs; its `needs:` list "
                            "in %s has %d." % (runbook_path, job, g,
                                               workflow_path, want))

print("WALKED\t" + " ".join(walked))
for f in findings:
    print("FAIL\t" + f)
PARSE
)
rc=$?
[ "$rc" -eq 0 ] || refuse "could not parse the pair (python3 exit $rc)."

if printf '%s\n' "$report" | grep '^REFUSE' >/dev/null; then
    refuse "$(printf '%s\n' "$report" | sed -n 's/^REFUSE\t//p')"
fi

fails=$(printf '%s\n' "$report" | sed -n 's/^FAIL\t//p')
if [ -n "$fails" ]; then
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        echo "::error title=The release walkthrough does not match the workflow::$f" >&2
        echo "FAIL: $f" >&2
    done <<< "$fails"
    exit 1
fi

walked=$(printf '%s\n' "$report" | sed -n 's/^WALKED\t//p')
echo "OK: $RUNBOOK walks $walked step for step, names every install proof," \
     "and states the counts $WORKFLOW derives."
