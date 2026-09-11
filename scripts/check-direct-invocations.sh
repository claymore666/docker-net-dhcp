#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# A script this repository tells someone to RUN must be runnable, and a
# refusal printed in the middle of a documented block must stop the block.
#
# WHY THIS EXISTS
#
# `scripts/release-body.sh` shipped mode 100644. Every caller in CI writes
# `bash scripts/release-body.sh …`, so every observer it had ran it through
# an interpreter and none of them could see the missing bit. The runbook
# writes it without `bash`, the way the file is meant to be run:
#
#     scripts/release-body.sh vX.Y.Z RELEASE_NOTES.md >/dev/null
#
# which exits 126, "Permission denied", having never opened RELEASE_NOTES.md.
# And because the block's lines were newline-separated, with no `&&` and no
# `set -e`, `git tag -s` and `git push origin` ran next regardless. The check
# whose entire purpose is to be met on a workstation before the tag could
# not run, and could not stop anything when it did not.
#
# Both halves are classes, not instances. 173 of this tree's 196
# `scripts/*.sh` carry the bit; the ones that do not are the ones only ever
# reached through `bash`, and nothing said which of those a document also
# invokes directly. Likewise, "the refusal stops what follows" was true of
# every documented block except the one where it mattered.
#
# WHAT IT CHECKS
#
#   1. EXECUTABLE. Every `scripts/*.sh` invoked in COMMAND POSITION without
#      an interpreter prefix -- in a markdown code block, a workflow `run:`
#      line, the Makefile, or another tracked shell script -- is mode
#      100755 in the git INDEX. The index, not the filesystem: a bit that
#      is set on this box and not committed is not a bit that ships, and
#      that distinction is the whole finding.
#
#   2. PROPAGATING. In a markdown code block, a direct invocation that is
#      followed by another command in the same block must be chained to it
#      with a trailing `&&`, or the block must set `set -e` before it.
#      A reader pastes a block; a check in the middle of one is there to
#      stop the commands under it.
#
# The scope of (2) is documentation, deliberately. Workflow `run:` blocks
# and shell scripts have their own propagation rules -- `set -euo pipefail`,
# which other gates in this tree already enforce -- and a markdown block has
# nothing between it and a human's terminal.
#
# WHAT IT CANNOT DO
#
# It judges command position, not intent. `scripts/x.sh` inside a string, in
# prose, or as an argument is not an invocation and is not read as one; a
# document that instructs "run scripts/x.sh" in a sentence is invisible here.
# It also cannot judge a name it cannot resolve: a block that invokes
# `scripts/test-thing.sh` inside a temporary sandbox names a file this repo
# does not track, and those are counted and reported rather than guessed at.
# And (2) is about the block as printed -- a reader who runs the lines one
# at a time reads each refusal themselves, which is the case this cannot and
# need not cover.
#
# Usage: bash scripts/check-direct-invocations.sh
# Env:   DIRECT_INV_ROOT  tree to judge (default: this script's repository).
#                         The self-test copies the tree, edits the copy and
#                         points this at it, so the parsing under test is
#                         this file's and not a transcription of it.
# Exit:  0 every direct invocation is executable and propagates
#        1 at least one does not
#        2 the check could not run (no git, no domain to judge)
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${DIRECT_INV_ROOT:-$(dirname "$HERE")}"

refuse() {
    echo "::error title=Direct invocations cannot be judged::$*" >&2
    exit 2
}

command -v git >/dev/null 2>&1 || refuse "git is required: the executable bit this gate reads is the one in the index, and only git can say what that is."
command -v python3 >/dev/null 2>&1 || refuse "python3 is required to find command position in fenced blocks and YAML."
[ -d "$ROOT" ] || refuse "${ROOT} is not a directory"
git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1 || refuse "${ROOT} is not a git repository"

python3 - "$ROOT" <<'PY'
import os, re, subprocess, sys

try:
    import yaml
except ImportError:
    print("::error title=Direct invocations cannot be judged::python3 cannot import yaml. Workflow shell is read "
          "from the parsed document, not by line scan: a line scan counts a `sparse-checkout:` path as an "
          "invocation and misses one written in a flow mapping, and both misses look like a pass.", file=sys.stderr)
    sys.exit(2)

root = sys.argv[1]
os.chdir(root)

out = subprocess.run(['git', 'ls-files', '-s'], capture_output=True, text=True)
if out.returncode != 0:
    print("::error title=Direct invocations cannot be judged::git ls-files failed in %s" % root, file=sys.stderr)
    sys.exit(2)

modes = {}
for line in out.stdout.splitlines():
    meta, path = line.split('\t', 1)
    modes[path] = meta.split(' ', 1)[0]

# Command position: start of line, or after a separator that ENDS a command.
# `bash x.sh`, `sh x.sh`, `. x.sh` and `source x.sh` are interpreter calls and
# are not this gate's subject -- they are exactly the callers that hid the
# defect, and they are legitimate.
CMD = re.compile(r'(?:^|[;&|(]|\|\||&&|\bthen\b|\bdo\b|\belse\b|\bif\b|\buntil\b|\bwhile\b)\s*(?:!\s*)?(scripts/[A-Za-z0-9._-]+\.sh)(?=[\s;&|)]|$)')
# A trailing comment, so `cmd &&   # why` reads as a chained line.
TRAILING_COMMENT = re.compile(r'\s#(?![{#!]).*$')

SHELLISH = ('', 'sh', 'bash', 'shell', 'console')


def commands_in(line):
    s = re.sub(r'^\s*\$\s+', '', line.strip())   # a pasted `$ ` prompt
    if s.startswith('#'):
        return []
    return [m.group(1) for m in CMD.finditer(s)]


def runs_in(node):
    """Every `run:` scalar anywhere in a parsed workflow."""
    if isinstance(node, dict):
        for k, v in node.items():
            if k == 'run' and isinstance(v, str):
                yield v
            else:
                for r in runs_in(v):
                    yield r
    elif isinstance(node, list):
        for v in node:
            for r in runs_in(v):
                yield r


def fenced_blocks(text):
    """(info, [(lineno, text), ...]) for every fenced block."""
    lines = text.splitlines()
    i = 0
    while i < len(lines):
        m = re.match(r'^\s*(`{3,}|~{3,})(.*)$', lines[i])
        if not m:
            i += 1
            continue
        fence, info = m.group(1), m.group(2).strip()
        closer = re.compile(r'^\s*' + re.escape(fence[0]) + '{' + str(len(fence)) + r',}\s*$')
        body, j = [], i + 1
        while j < len(lines) and not closer.match(lines[j]):
            body.append((j + 1, lines[j]))
            j += 1
        yield info, body
        i = j + 1


fail = 0
n_exec = 0        # direct invocations judged for the bit
n_unresolved = 0  # named a path this repo does not track
n_blocks = 0      # doc blocks judged for propagation
unresolved_names = set()


def judge_bit(where, script):
    global fail, n_exec, n_unresolved
    mode = modes.get(script)
    if mode is None:
        n_unresolved += 1
        unresolved_names.add(script)
        return
    n_exec += 1
    if mode != '100755':
        fail = 1
        print("FAIL  %s: `%s` is invoked directly but is mode %s in the index." % (where, script, mode), file=sys.stderr)
        print("      Run directly it exits 126 without reading its arguments, so a check written this way", file=sys.stderr)
        print("      never runs and never says so. Fix: git update-index --chmod=+x %s" % script, file=sys.stderr)


files = subprocess.run(['git', 'ls-files'], capture_output=True, text=True).stdout.split('\n')
for f in sorted(x for x in files if x):
    if not os.path.isfile(f):
        continue
    try:
        text = open(f, encoding='utf-8', errors='replace').read()
    except OSError:
        continue

    if f.endswith('.md'):
        for info, body in fenced_blocks(text):
            if info.lower() not in SHELLISH:
                continue
            # The block's command lines, in order, with what each invokes.
            cmds = []
            sets_e = False
            for ln, raw in body:
                stripped = re.sub(r'^\s*\$\s+', '', raw.strip())
                if not stripped or stripped.startswith('#'):
                    continue
                if re.match(r'^set\s+-[a-z]*e', stripped):
                    sets_e = True
                cmds.append((ln, stripped, commands_in(raw)))
            if not any(c[2] for c in cmds):
                continue
            n_blocks += 1
            for idx, (ln, stripped, scripts) in enumerate(cmds):
                for s in scripts:
                    judge_bit("%s:%d" % (f, ln), s)
                if not scripts:
                    continue
                if idx == len(cmds) - 1:
                    continue        # nothing follows it in this block
                if sets_e:
                    continue
                chained = TRAILING_COMMENT.sub('', stripped).rstrip()
                if chained.endswith('&&') or chained.endswith('\\'):
                    continue
                fail = 1
                nxt = cmds[idx + 1][1]
                print("FAIL  %s:%d: `%s` is followed in the same block by `%s`, with nothing carrying its"
                      % (f, ln, stripped, nxt[:60]), file=sys.stderr)
                print("      failure across the newline. A refusal here prints and the next line runs anyway.", file=sys.stderr)
                print("      Fix: end the line with `&&`, or open the block with `set -e`.", file=sys.stderr)
    elif f.startswith('.github/workflows/') and f.endswith(('.yml', '.yaml')):
        # The PARSED document, and only `run:` scalars. A line scan reads
        # the sparse-checkout list in release.yml as four invocations and
        # a `path:` as one; a workflow's shell is exactly its `run:` keys.
        try:
            doc = yaml.safe_load(text)
        except yaml.YAMLError as e:
            print("::error title=Direct invocations cannot be judged::%s does not parse as YAML (%s)" % (f, e), file=sys.stderr)
            sys.exit(2)
        for script in runs_in(doc):
            for i, raw in enumerate(script.splitlines(), 1):
                for s in commands_in(raw):
                    judge_bit("%s (a run: block, line %d)" % (f, i), s)
    elif f == 'Makefile' or f.endswith(('.sh', '.mk', '.bash')):
        for i, raw in enumerate(text.splitlines(), 1):
            for s in commands_in(raw):
                judge_bit("%s:%d" % (f, i), s)

if n_exec == 0:
    print("::error title=Direct invocations cannot be judged::this tree holds no direct `scripts/*.sh` invocation "
          "resolving to a tracked file, so rule 1 would pass by having nothing to judge. The normal state of this "
          "repository is at least four (the release runbook alone prints four). Either the detector stopped "
          "matching command position or the domain moved; both are failures, neither is a pass.", file=sys.stderr)
    sys.exit(2)
if n_blocks == 0:
    print("::error title=Direct invocations cannot be judged::no markdown code block in this tree invokes a "
          "`scripts/*.sh` directly, so rule 2 has no domain. See above: an empty domain is not a pass.", file=sys.stderr)
    sys.exit(2)

if fail:
    sys.exit(1)

extra = ''
if n_unresolved:
    extra = "; %d name(s) in sandboxed blocks resolve to no tracked file and were not judged (%s)" % (
        n_unresolved, ' '.join(sorted(unresolved_names)))
print("PASS  %d direct scripts/*.sh invocation(s) are executable in the index, and %d documentation block(s) "
      "carrying one propagate its failure%s" % (n_exec, n_blocks, extra))
PY
