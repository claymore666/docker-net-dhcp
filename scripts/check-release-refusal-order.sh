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
#
# `needs:` is the ordering primitive on purpose. "The step is earlier in the
# file" orders nothing across jobs, and every publishing job in this workflow
# is a separate job.
#
# WHAT IT CANNOT DO
#
# It cannot tell that the judging job judges the right tree. That is the
# workflow's own business (`.resolver` is our ref, `.notes` is the tag's),
# and the self-test for `release-body.sh` covers what the script decides.
# It also says nothing about steps INSIDE a publishing job: a job that logs
# in and pushes in step 2 is already too late for anything this gate could
# say, which is why rule 3 forbids putting the judgement there.
#
# Usage: bash scripts/check-release-refusal-order.sh [workflow-file]
# Exit:  0 every publishing job descends from the notes judgement
#        1 at least one does not
#        2 the check could not run (no file, no publisher, no judgement)
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

JUDGE_CMD = re.compile(r'release-body\.sh\b')


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


publishers = {}   # name -> why
judges = []
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
            if JUDGE_CMD.search(run):
                judges.append(name)
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

if fail:
    sys.exit(1)

print("PASS  %d publishing job(s) in %s all descend from the release-body judgement in %s"
      % (len(publishers), path, ', '.join(judges)))
PY
