#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The release run must judge the notes BEFORE it publishes anything.
#
# WHY THIS EXISTS
#
# `scripts/release-body.sh` refuses a tag whose `RELEASE_NOTES.md` section
# cannot be identified. Refusing is right; WHERE it refused was not. The
# only caller was "Assemble release notes" in `github-release`, and that job
# `needs:` the four verify jobs, which need `release` and `release-arm64`.
# By the time the refusal is printed the tag is in both registries and
# `promote-latest` -- same `needs:`, no ordering between them -- is moving
# the floating tags. The run goes red having already published the images,
# with no release page and a `:latest` that may already point at the tag.
#
# A gate is only fail-closed if it fires while there is still something to
# close. "It refuses" and "it refuses in time" are two claims and the second
# is the one the operator lives with.
#
# WHAT IT CHECKS, in `release.yml`
#
#   1. Some job runs `release-body.sh`. (If none does, the extraction went
#      back into the YAML and there is nothing to order.)
#   2. Every PUBLISHING job has such a job among its transitive `needs:`.
#      Publishing means: the job logs in to a registry (`docker/login-action`
#      holds a push credential -- a job that has one is a job that can push,
#      and that is the conservative direction) or its shell runs a
#      registry-mutating command (`crane tag`/`copy`, `docker push`,
#      `docker plugin push`, `cosign sign`, a `--push` build, `make … push`).
#   3. The judging job is not itself a publishing job. A refusal that fires
#      in the job holding the credentials fires beside the push, not before
#      it, and `needs:` cannot order steps within one job the way this gate
#      reads them.
#   4. EVERY invocation of the script names a copy checked out at THIS
#      workflow's own ref -- a directory produced by an `actions/checkout`
#      in the same job that passes no `ref:`. "Our script, their data."
#      A bare `bash scripts/release-body.sh` inside a job whose checkout is
#      `ref: <the tag>` runs the TAG's copy of the extractor, which is the
#      tag deciding whether the tag is publishable -- and for every tag this
#      repository published before the script existed (v2.0.0-rc1, v1.9.0)
#      it is not a weaker judgement, it is exit 127 at the last step of a
#      run that has already re-pushed both registries. One fix does not
#      reach the copies: rule 4 is the reason there is no second copy to
#      forget.
#   5. The PRE-FLIGHT judge -- a judging job that is an ancestor of a
#      publishing job -- checks out no tree the trigger names: no
#      `actions/checkout` in it may carry a `ref:` that is a `${{ }}`
#      expression. The notes it judges are the tag's, so they must arrive
#      as DATA (fetch the ref, `git show` the blob to a file), never as a
#      working tree. A tree the trigger chose, materialised in the job that
#      gates every publisher, is what CodeQL's
#      `actions/cache-poisoning/poisonable-step` names; the shape is also
#      wrong on its own terms, since anything in that tree is one careless
#      later step away from being executed. The same rule covers the shell:
#      `git checkout`, `git switch`, `git worktree add` and `git restore`
#      put a tree on disk exactly as `actions/checkout` does, and a rule
#      that only reads `uses:` steps would be satisfied by rewriting the
#      defect as a `run:` line. `git fetch` and `git show` are the shape
#      that is kept -- they move objects, not a working tree.
#
# `needs:` is the ordering primitive on purpose. "The step is earlier in the
# file" orders nothing across jobs, and every publishing job in this workflow
# is a separate job.
#
# WHAT IT CANNOT DO
#
# It cannot tell that the judgement reads the right notes -- rule 4 pins
# which copy of the SCRIPT runs, not which file is handed to it; the
# self-test for `release-body.sh` covers what the script then decides. It
# also says nothing about steps INSIDE a publishing job: a job that logs in
# and pushes in step 2 is already too late for anything this gate could say,
# which is why rule 3 forbids putting the judgement there.
#
# Usage: bash scripts/check-release-refusal-order.sh [workflow-file]
# Exit:  0 every publishing job descends from the notes judgement, every
#          invocation runs our copy, and the pre-flight checks out no
#          trigger-named tree
#        1 at least one of those is false
#        2 the check could not run (no file, no publisher, no judgement, no
#          pre-flight judge)
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
WF="${1:-$ROOT/.github/workflows/release.yml}"

refuse() {
    echo "::error title=Release refusal order cannot be judged::$*" >&2
    exit 2
}

command -v python3 >/dev/null 2>&1 || refuse "python3 is required to read the job graph."
python3 -c 'import yaml' 2>/dev/null || refuse "python3 cannot import yaml. `needs:` is read from the parsed document -- a scalar and a sequence are the same edge and a line scan sees two different things."
[ -f "$WF" ] || refuse "${WF} does not exist. A gate whose subject vanished is not a gate that passed."

python3 - "$WF" <<'PY'
import re, sys
import yaml

path = sys.argv[1]
with open(path, encoding='utf-8') as fh:
    doc = yaml.safe_load(fh)

jobs = (doc or {}).get('jobs')
if not isinstance(jobs, dict) or not jobs:
    print("::error title=Release refusal order cannot be judged::%s declares no jobs." % path, file=sys.stderr)
    sys.exit(2)

PUBLISH_CMD = re.compile(
    r'\bcrane\s+(tag|copy|cp)\b'
    r'|\bdocker\s+(plugin\s+)?push\b'
    r'|\bcosign\s+sign\b'
    r'|\bdocker\s+buildx\b[^\n]*--push\b'
    r'|\bmake\b[^\n]*\bpush\b')

# COMMAND POSITION. `release.yml`'s assembly step carries a paragraph ABOUT
# the script inside its own `run:` scalar; a substring search reads that
# prose as a second invocation, from the wrong directory, and the gate fails
# on a comment. A comment line's `#` is not a command delimiter, so
# requiring command position is what excludes it -- there is no separate
# comment stripper to keep in step with the shell's actual grammar.
INVOCATION = re.compile(
    r'(?:^|[;&|(]|\|\||&&)\s*(?:!\s*)?(?:(?:bash|sh)\s+)?'
    r'((?:[A-Za-z0-9._-]+/)*)scripts/release-body\.sh(?=[\s;&|)]|$)', re.M)
EXPRESSION = re.compile(r'\$\{\{')
# Tree-materialising git, in command position (see INVOCATION above).
CHECKOUT_CMD = re.compile(
    r'(?:^|[;&|(]|\|\||&&)\s*(?:!\s*)?git\b[^\n]*?'
    r'\s(checkout|switch|restore|worktree)\b', re.M)


def steps_of(job):
    s = job.get('steps')
    return s if isinstance(s, list) else []


def needs_of(job):
    n = job.get('needs')
    if n is None:
        return []
    if isinstance(n, str):
        return [n]
    return list(n)


def checkouts_of(job):
    """(path, ref, step name) for every actions/checkout step in the job."""
    out = []
    for st in steps_of(job):
        if not isinstance(st, dict):
            continue
        uses = st.get('uses') or ''
        if not uses.startswith('actions/checkout@'):
            continue
        with_ = st.get('with')
        if not isinstance(with_, dict):
            with_ = {}
        path = str(with_.get('path') or '.').rstrip('/') or '.'
        ref = with_.get('ref')
        out.append((path, None if ref is None else str(ref), st.get('name') or uses))
    return out


publishers = {}     # name -> why
judges = []
invocations = []    # (job, step name, directory, full text)
for name, job in jobs.items():
    if not isinstance(job, dict):
        continue
    why = []
    for st in steps_of(job):
        if not isinstance(st, dict):
            continue
        uses = st.get('uses') or ''
        if 'docker/login-action' in uses:
            why.append("logs in to a registry (%s)" % (st.get('name') or 'unnamed step'))
        run = st.get('run')
        if isinstance(run, str):
            m = PUBLISH_CMD.search(run)
            if m:
                why.append("runs `%s` (%s)" % (m.group(0).strip(), st.get('name') or 'unnamed step'))
            for hit in INVOCATION.finditer(run):
                judges.append(name)
                invocations.append((name, st.get('name') or 'unnamed step',
                                    hit.group(1).rstrip('/') or '.',
                                    '%sscripts/release-body.sh' % hit.group(1)))
    if why:
        publishers[name] = why

judges = sorted(set(judges))

if not judges:
    print("::error title=Release refusal order cannot be judged::no job in %s runs release-body.sh. The release "
          "body is either assembled inline again -- the defect that shipped a placeholder as v2.0.0-rc1's notes -- "
          "or the script was renamed and this gate now orders nothing. Either way it is not a pass." % path,
          file=sys.stderr)
    sys.exit(2)

if not publishers:
    print("::error title=Release refusal order cannot be judged::no job in %s logs in to a registry or runs a "
          "publishing command. This workflow's whole purpose is to publish; a domain of zero here means the "
          "detector stopped matching, not that nothing is pushed." % path, file=sys.stderr)
    sys.exit(2)


def ancestors(name, seen=None):
    if seen is None:
        seen = set()
    for n in needs_of(jobs.get(name) or {}):
        if n in seen:
            continue
        seen.add(n)
        ancestors(n, seen)
    return seen


fail = 0
for name in sorted(publishers):
    anc = ancestors(name)
    covering = [j for j in judges if j in anc]
    if covering:
        print("      %-24s judged first by: %s" % (name, ', '.join(covering)))
        continue
    fail = 1
    print("FAIL  job `%s` publishes (%s) but no job running release-body.sh is among its needs:." % (name, '; '.join(publishers[name])), file=sys.stderr)
    print("      Its ancestors are: %s. Judging jobs: %s." % (', '.join(sorted(anc)) or '(none)', ', '.join(judges)), file=sys.stderr)
    print("      A refusal that arrives after this job has run arrives after the images exist.", file=sys.stderr)

for j in judges:
    if j in publishers:
        fail = 1
        print("FAIL  job `%s` both judges the notes and publishes (%s)." % (j, '; '.join(publishers[j])), file=sys.stderr)
        print("      `needs:` orders jobs, not steps; a judgement sharing a job with the push is not ordered before it.", file=sys.stderr)

# Rule 4 -- every invocation runs OUR copy. Its domain is `judges`, already
# refused above when empty.
for job_name, step_name, where, text in invocations:
    cos = checkouts_of(jobs.get(job_name) or {})
    match = [c for c in cos if c[0] == where]
    if not match:
        fail = 1
        print("FAIL  job `%s` runs `%s` (%s) from `%s`, which no actions/checkout in that job produces." % (job_name, text, step_name, where), file=sys.stderr)
        print("      Checkout paths in this job: %s." % (', '.join(sorted(c[0] for c in cos)) or '(no checkout at all)'), file=sys.stderr)
        continue
    bad = [c for c in match if c[1] is not None]
    if bad:
        fail = 1
        print("FAIL  job `%s` runs `%s` (%s) from `%s`, checked out at `ref: %s`." % (job_name, text, step_name, where, bad[0][1]), file=sys.stderr)
        print("      That is not our copy of the extractor, it is whatever that ref carries -- and for every tag", file=sys.stderr)
        print("      published before the script existed it carries nothing, so the run dies after publishing.", file=sys.stderr)
        print("      Add a checkout with no `ref:` at its own `path:` and invoke the script through that path.", file=sys.stderr)
        continue
    print("      %-24s runs %-40s from `%s` (this workflow's own ref)" % (job_name, text, where))

# Rule 5 -- the pre-flight judge checks out no tree the trigger names.
preflight = sorted(j for j in judges if any(j in ancestors(p_) for p_ in publishers))
if not preflight:
    # Rule 2 has already said why, in terms an operator can act on; do not
    # overwrite a judged failure with "could not judge".
    if fail:
        sys.exit(1)
    print("::error title=Release refusal order cannot be judged::no judging job in %s is an ancestor of a "
          "publishing job, so there is no pre-flight to hold to rule 5. Rule 2 decides that case; a vacuous "
          "rule 5 must not read as a pass." % path, file=sys.stderr)
    sys.exit(2)

for j in preflight:
    for cpath, cref, cname in checkouts_of(jobs.get(j) or {}):
        if cref is not None and EXPRESSION.search(cref):
            fail = 1
            print("FAIL  pre-flight judge `%s` checks out `ref: %s` at path `%s` (%s)." % (j, cref, cpath, cname), file=sys.stderr)
            print("      This job gates every publisher. A tree named by the trigger, materialised here, is the", file=sys.stderr)
            print("      shape CodeQL flags as actions/cache-poisoning/poisonable-step -- and it puts files the", file=sys.stderr)
            print("      tag controls one careless step away from execution in the job that decides the release.", file=sys.stderr)
            print("      Read the tag's data instead: fetch the ref, `git show` the blob to a file.", file=sys.stderr)
    for st in steps_of(jobs.get(j) or {}):
        if not isinstance(st, dict):
            continue
        run = st.get('run')
        if not isinstance(run, str):
            continue
        m = CHECKOUT_CMD.search(run)
        if m:
            fail = 1
            print("FAIL  pre-flight judge `%s` runs `git %s` in its shell (%s)." % (j, m.group(1), st.get('name') or 'unnamed step'), file=sys.stderr)
            print("      A working tree is a working tree however it is created; rewriting the checkout as a", file=sys.stderr)
            print("      `run:` line moves the defect out of `uses:` and nowhere else. Read objects instead:", file=sys.stderr)
            print("      `git fetch` the ref to a local ref name, then `git show <ref>:<path>` to a file.", file=sys.stderr)
    print("      %-24s pre-flight judge, checks out no trigger-named tree, materialises none in its shell" % j)

if fail:
    sys.exit(1)

print("PASS  %d publishing job(s) in %s all descend from the release-body judgement in %s; "
      "%d invocation(s) run this workflow's own copy; pre-flight judge(s) %s check out no trigger-named tree"
      % (len(publishers), path, ', '.join(judges), len(invocations), ', '.join(preflight)))
PY
