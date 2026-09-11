#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# The lanes that run a WHOLE integration suite in one `go test` binary
# must each state a ceiling that holds it, and their job caps must hold
# the ceilings (#934).
#
# WHY THIS EXISTS. Three lanes now run the main suite unsharded --
# integration-arm64.yml, coverage.yml and integration-hosted.yml -- and
# each of them arrived at the same 45m by its own derivation, in its own
# comment, reconciled against the others by nothing at all. Before this
# gate, `grep -rn ITEST_TIMEOUT scripts/` returned empty: no check read
# the value, none asked which lanes are one-process, and none compared a
# job cap against the step ceilings underneath it. The cross-references
# were prose.
#
# That is the shape #542 already killed one door along, where 28
# hand-listed self-test lines were reconciled by nothing until
# run-gate-selftests.sh was made to DISCOVER instead. So this gate
# derives its population rather than listing it: any workflow step whose
# `run:` invokes `make integration-test` or `make integration-test-failure`
# is running a whole suite in one process, and a fourth such lane is in
# scope the moment it is written, with nobody having to remember to add
# it here.
#
# HOW THE DEFECT ACTUALLY SURFACES, twice in one release: `Makefile:171`
# sets `ITEST_TIMEOUT ?= 20m`, sized for one of the amd64 lane's nine
# shards. A one-process lane that sets nothing inherits it and dies
# mid-suite -- coverage.yml did that on the v2.0.0 release PR (run
# 34523956617, 56 of 93 tests in), and integration-hosted.yml would have
# done it on whichever week nobody was watching the cron. The failure is
# a timeout months later on a lane nobody dispatched, which is the worst
# possible place to learn it.
#
# THE THREE PROPERTIES, one per way the budget can be absent:
#
#   A. THE MAIN SUITE'S CEILING IS STATED. A step running the whole main
#      suite (`make integration-test`) must have ITEST_TIMEOUT set where
#      the step can see it -- its own `env:`, its job's, or the
#      workflow's. Inheriting the Makefile default is the defect: that
#      default is a SHARD's budget and states nothing about a lane that
#      runs all 93 tests at once. The failure suite is not held to this,
#      because there the default IS the whole-suite budget (the four
#      TestFailure_ tests are never sharded by time), and a gate that
#      demands a value identical to the default it replaces buys
#      nothing.
#
#   B. THE VALUE SURVIVES `sudo`. `sudo` resets the environment, so a
#      step that sets ITEST_TIMEOUT in `env:` and then runs make through
#      `sudo` without forwarding it has written a budget that does not
#      reach the process it is supposed to bound: make takes the default
#      and the `env:` block reads, to every future editor, as if the
#      lane were covered. integration-hosted.yml is the lane where this
#      matters, and the forward is asserted by name.
#
#   C. THE JOB CAP HOLDS THE STEP CEILINGS. `timeout-minutes` on the job
#      must exceed the sum of the ceilings of its whole-suite steps, so
#      that a suite hitting its alarm reports as a suite timing out
#      rather than as the job being killed with budget still on the
#      clock. integration-hosted.yml carried `timeout-minutes: 40` over
#      65 minutes of ceilings; the two suites alone cost 34.6 minutes on
#      the 2.0 tree, so the cap sat under the WORK, never mind the
#      ceilings. The margin required here is one minute: enough to prove
#      the cap was not simply set equal to the sum, and not a pretence
#      that this gate can derive any lane's setup time offline. Each
#      lane's real setup allowance is measured and stated in its own
#      comment beside the number.
#
# The sum is taken over every whole-suite step in the job, including
# steps that cannot run together (an `if:` picking one arm). That
# over-counts in the SAFE direction -- it can only ask for a larger cap
# than the lane needs -- and the alternative is this gate evaluating
# GitHub expressions, which it will not do.
#
# THE DEFAULT IS READ, NOT ASSUMED. The Makefile's `?=` value is parsed
# out of the Makefile on every run, so moving it moves what this gate
# costs an unset step; a hardcoded 20 here would be a second copy of the
# fact the gate exists to reconcile.
#
# THE EMPTY POPULATION IS A REFUSAL, not a pass. A universal claim over
# an empty domain is true and worthless: if no step matches, the lanes
# have been renamed or the pattern has rotted, and either way this gate
# no longer observes anything. It exits 2 rather than reporting clean.
#
# Usage: check-one-process-itest-budget.sh [workflow-dir] [makefile]
# Exit:  0 every one-process lane states and holds its budget
#        1 a property above is broken
#        2 cannot check (no workflows, no python3/PyYAML, no default to
#          read, a value this gate cannot judge, or an empty population)

set -uo pipefail

WF_DIR="${1:-.github/workflows}"
MAKEFILE="${2:-Makefile}"

refuse() {
    echo "::error title=One-process budget gate cannot check::$1" >&2
    exit 2
}

[ -d "$WF_DIR" ] || refuse "no workflow directory '$WF_DIR'."
[ -f "$MAKEFILE" ] || refuse "no makefile at '$MAKEFILE'; the default ceiling is read from it, never assumed."

command -v python3 >/dev/null 2>&1 || refuse \
    "python3 is required. The subject is a step inside a job inside a workflow, and a line scan cannot tell which job a step belongs to -- the job boundary is the whole of property C."

python3 - "$WF_DIR" "$MAKEFILE" <<'PY'
import glob
import os
import re
import sys

try:
    import yaml
except ImportError:
    sys.stderr.write(
        "::error title=One-process budget gate cannot check::python3 cannot import yaml. "
        "There is no line-scan fallback on purpose: the fallback is the defect.\n")
    sys.exit(2)

wf_dir, makefile = sys.argv[1], sys.argv[2]

VAR = "ITEST_TIMEOUT"

# The whole-suite make targets. `integration-test-shard` is deliberately
# absent: a shard is what the 20m default was sized for.
WHOLE_SUITE = ("integration-test", "integration-test-failure")
MAIN_SUITE = "integration-test"

# `make [flags/vars] <target>`, with the target's word boundary explicit
# so `integration-test-shard` cannot be read as `integration-test`.
MAKE_RE = re.compile(r"\bmake\b[^\n]*?\b(integration-test(?:-failure)?)(?![\w-])")

DURATION_RE = re.compile(r"^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$")

MARGIN_MINUTES = 1


def refuse(msg):
    sys.stderr.write("::error title=One-process budget gate cannot check::%s\n" % msg)
    sys.exit(2)


def minutes(value, where):
    """A Go duration as minutes. Refuses rather than guessing."""
    if not isinstance(value, str):
        refuse("%s is %r, which is not a duration this gate can judge." % (where, value))
    text = value.strip()
    if "${{" in text:
        refuse("%s is the expression %r. This gate does not evaluate workflow "
               "expressions, and a ceiling it cannot read is a ceiling nobody checks." % (where, text))
    m = DURATION_RE.match(text)
    if not m or text == "":
        refuse("%s is %r, which is not a Go duration (1h30m, 45m, 90s)." % (where, text))
    h, mi, s = (int(g) if g else 0 for g in m.groups())
    return h * 60 + mi + s / 60.0


# The default, read out of the Makefile rather than restated here.
default_text = None
with open(makefile, encoding="utf-8") as fh:
    for line in fh:
        m = re.match(r"^\s*%s\s*\?=\s*(\S+)" % VAR, line)
        if m:
            default_text = m.group(1)
            break
if default_text is None:
    refuse("no `%s ?=` assignment in %s. The gate costs an unset step at the "
           "Makefile's own default and will not invent one." % (VAR, makefile))
default_minutes = minutes(default_text, "%s's %s default" % (makefile, VAR))


def env_of(node):
    env = (node or {}).get("env") or {}
    return env if isinstance(env, dict) else {}


files = sorted(glob.glob(os.path.join(wf_dir, "*.yml")) + glob.glob(os.path.join(wf_dir, "*.yaml")))
if not files:
    refuse("no *.yml or *.yaml in %s." % wf_dir)

findings = []
members = 0
jobs_checked = 0
lanes = set()

for path in files:
    with open(path, encoding="utf-8") as fh:
        try:
            doc = yaml.safe_load(fh)
        except yaml.YAMLError as exc:
            refuse("%s does not parse as YAML: %s" % (path, exc))
    if not isinstance(doc, dict):
        continue
    wf_env = env_of(doc)
    jobs = doc.get("jobs") or {}
    if not isinstance(jobs, dict):
        continue

    for job_id, job in jobs.items():
        if not isinstance(job, dict):
            continue
        job_env = env_of(job)
        steps = job.get("steps") or []
        if not isinstance(steps, list):
            continue

        ceilings = []
        for step in steps:
            if not isinstance(step, dict):
                continue
            run = step.get("run")
            if not isinstance(run, str):
                continue
            targets = MAKE_RE.findall(run)
            if not targets:
                continue

            name = step.get("name") or run.strip().splitlines()[0][:40]
            where = "%s: job %s: step '%s'" % (path, job_id, name)
            members += 1
            lanes.add(path)

            step_env = env_of(step)
            if VAR in step_env:
                value, source = step_env[VAR], "the step's env"
            elif VAR in job_env:
                value, source = job_env[VAR], "the job's env"
            elif VAR in wf_env:
                value, source = wf_env[VAR], "the workflow's env"
            else:
                value, source = None, None

            ceiling = (minutes(value, "%s: %s" % (where, VAR))
                       if value is not None else default_minutes)
            ceilings.append(ceiling)

            # A. the main suite's ceiling is stated.
            if MAIN_SUITE in targets and value is None:
                findings.append(
                    "%s runs the whole main suite and sets no %s, so it inherits "
                    "%s's %s -- a SHARD's budget applied to every test at once. "
                    "State the lane's own ceiling beside its derivation."
                    % (where, VAR, makefile, default_text))

            # B. the value survives sudo.
            if value is not None:
                for line in run.splitlines():
                    if not MAKE_RE.search(line):
                        continue
                    head = line[:MAKE_RE.search(line).start()]
                    if not re.search(r"\bsudo\b", head):
                        continue
                    forwarded = (
                        re.search(r"\b%s=" % VAR, head)
                        or re.search(r"\bsudo\s+(-E|--preserve-env)\b", head)
                        or re.search(r"--preserve-env=[^\s]*\b%s\b" % VAR, head))
                    if not forwarded:
                        findings.append(
                            "%s sets %s in %s and then runs make through sudo without "
                            "forwarding it. sudo resets the environment, so make takes "
                            "%s's %s and the env: block bounds nothing. Forward it by "
                            "name in the sudo env list, as the step already does for its "
                            "other variables." % (where, VAR, source, makefile, default_text))

        if not ceilings:
            continue
        jobs_checked += 1

        cap = job.get("timeout-minutes")
        job_where = "%s: job %s" % (path, job_id)
        required = sum(ceilings) + MARGIN_MINUTES
        if cap is None:
            findings.append(
                "%s runs %d whole-suite step(s) totalling %.0f minutes of ceilings and "
                "carries no timeout-minutes. An unbounded job cannot backstop a suite "
                "alarm, and GitHub's 360-minute default is nobody's derivation."
                % (job_where, len(ceilings), sum(ceilings)))
        else:
            if isinstance(cap, str) and "${{" in cap:
                refuse("%s's timeout-minutes is an expression this gate cannot evaluate." % job_where)
            try:
                cap_minutes = float(cap)
            except (TypeError, ValueError):
                refuse("%s's timeout-minutes is %r, which is not a number." % (job_where, cap))
            if cap_minutes < required:
                findings.append(
                    "%s caps the job at %g minutes over %.0f minutes of step ceilings. "
                    "The cap fires FIRST, killing the job while a suite still holds its "
                    "budget and reporting the wrong cause. It must be at least %.0f."
                    % (job_where, cap_minutes, sum(ceilings), required))

if members == 0:
    refuse("no step in %s invokes `make %s`. Either the lanes were renamed or this "
           "gate's population has rotted -- and a rule with nothing to check reports "
           "clean while observing nothing." % (wf_dir, "` or `make ".join(WHOLE_SUITE)))

# The derivation prints on EVERY outcome, red included. What the gate
# found and what it costed unset steps at is the first thing a reader
# needs, and printing it only on success means the one run where someone
# is reading closely is the run that does not say.
print("one-process budget gate: %d whole-suite step(s) in %d lane(s), %d job cap(s) "
      "examined; unset steps costed at %s's %s default of %s"
      % (members, len(lanes), jobs_checked, makefile, VAR, default_text))

for f in findings:
    sys.stderr.write("::error title=One-process lane budget::%s\n" % f)

if findings:
    sys.stderr.write("%d finding(s) over %d whole-suite step(s) in %d lane(s).\n"
                     % (len(findings), members, len(lanes)))
    sys.exit(1)

print("every one-process lane states its ceiling and every job cap holds the ceilings under it")
PY
