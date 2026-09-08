#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The runner pool's size and the number of jobs a run puts on it are
# stated in prose all over this tree, and until now nothing checked any
# of them (#879). Every one of them was correct when written; both
# operands move on their own schedule — the pool has been resized, the
# suite matrix has been resharded three times — so each statement decays
# silently and is trusted by exactly the readers who will not go and
# measure. Two of the stale sites were `advise()` output in
# ci-queue-watchdog.sh: the text an operator reads DURING an incident,
# on the STARVATION and POOL SHORT paths. A diagnostic that misdirects
# is worse than a comment that is merely stale.
#
# THE FACTS AND WHERE EACH IS DERIVED FROM
#
#   integration-pool-jobs   how many jobs an integration.yml run places
#                           on the pool. DERIVED by parsing the workflow
#                           and expanding the matrix of every job whose
#                           runs-on names the pool label.
#   coverage-pool-jobs      the same, for coverage.yml.
#   pool-runners            how many x64 runners are registered. Read
#                           from .github/ci-pool.json.
#
# WHY THE POOL SIZE IS A DECLARED CONSTANT AND NOT AN API READ.
# MEASURED, and already written down in ci-queue-watchdog.sh's
# classify_wait: listing self-hosted runners needs repo administration
# rights, and `administration` is not one of the workflow
# GITHUB_TOKEN's permission scopes -- so this is not a matter of adding
# a permission, an in-lane query cannot be made to work at all. The
# constant is therefore the weaker of the issue's two options, and its
# weakness is exact and worth stating: these numbers can still disagree
# with reality, but they can no longer disagree with EACH OTHER, and
# reality now has exactly one place to be corrected instead of six.
#
# `--live` closes the rest, off the lane: with a token that CAN read the
# runners API (a workstation's, or the local lane's) it compares the
# constant to the live count and refuses on a mismatch. It is opt-in
# because the lane cannot run it, and it REFUSES rather than passing
# when it is asked for and cannot reach the API -- an unreadable pool is
# not a matching pool.
#
# THE DOMAIN, AND WHAT IT CANNOT SEE. A statement is in the domain when
# it carries a marker on its own line:
#
#     ci-pool: <fact>=<value>
#
# and the gate requires <value> to equal the derived fact. That half is
# keyed on the derivation. The other half is a BACKSTOP and is keyed on
# SPELLING, which is a real limit and is stated here rather than
# discovered later: a small set of statement shapes -- ci-pool-exempt: these
# are the gate's own pattern examples, not claims -- ("a pool of 16",
# ci-pool-exempt: as above
# "puts 11 jobs", "16 runners", and the same with number-words) must
# carry a marker on the line or the line above, or say
# `ci-pool-exempt: <reason>` on the line. A sentence that states the
# pool size in a shape this pattern does not match is invisible to the
# gate. It is a ratchet against the next drift, not evidence that every
# sentence in the tree is true.
#
# It REFUSES rather than passing when it cannot see: no constant file,
# an unparseable one, no workflow, a workflow that yields zero pool jobs
# (a universal gate is satisfied by emptying its domain), or no marker
# anywhere in the tree.
#
# Usage: bash scripts/check-pool-facts.sh [--live] [--root <dir>]
# Exit:  0 every stated number matches its derivation
#        1 a stated number disagrees, or an unmarked statement was found
#        2 cannot see -- refuse rather than pass

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIVE=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --live) LIVE=1; shift ;;
        --root) ROOT="$2"; shift 2 ;;
        *) echo "usage: $0 [--live] [--root <dir>]" >&2; exit 2 ;;
    esac
done

CONST="$ROOT/.github/ci-pool.json"
WF="$ROOT/.github/workflows"

if [ ! -r "$CONST" ]; then
    echo "::error title=No pool constant::$CONST is missing or unreadable, so there is" \
         "nothing for the prose in this tree to be checked against (#879)." >&2
    exit 2
fi
if [ ! -d "$WF" ]; then
    echo "::error title=No workflow directory::$WF is not a directory, so the per-run job" \
         "count cannot be derived (#879)." >&2
    exit 2
fi

# The derivation, and the marker/backstop scan, in one python pass: the
# scan reads the same tracked file list the sweep was done over, and
# splitting it would let the two drift.
export CI_POOL_ROOT="$ROOT" CI_POOL_CONST="$CONST" CI_POOL_WF="$WF"
python3 - <<'PY'
import json, os, re, subprocess, sys

root = os.environ["CI_POOL_ROOT"]
const_path = os.environ["CI_POOL_CONST"]
wf = os.environ["CI_POOL_WF"]

def refuse(title, msg):
    print(f"::error title={title}::{msg}", file=sys.stderr)
    sys.exit(2)

try:
    import yaml
except ImportError:
    refuse("PyYAML unavailable",
           "check-pool-facts.sh derives the per-run job count by PARSING the workflows. "
           "A line scan sees one runs-on spelling out of seven (#844), so this refuses "
           "rather than degrading to one.")

try:
    const = json.loads(open(const_path).read())
except Exception as e:
    refuse("Pool constant unreadable", f"{const_path} is not valid JSON: {e}")

label = const.get("x64_label")
runners = const.get("x64_runners")
if not isinstance(label, str) or not label:
    refuse("Pool constant incomplete", f"{const_path} declares no x64_label")
if not isinstance(runners, int) or runners <= 0:
    refuse("Pool constant incomplete",
           f"{const_path} declares x64_runners={runners!r}, want a positive integer")

def pool_jobs(path):
    """How many jobs this workflow places on the pool, matrix expanded."""
    try:
        doc = yaml.safe_load(open(path))
    except Exception as e:
        refuse("Workflow unparseable", f"{path}: {e}")
    if not isinstance(doc, dict) or not isinstance(doc.get("jobs"), dict):
        refuse("Workflow has no jobs", f"{path} parses but declares no jobs mapping")
    total = 0
    for name, job in doc["jobs"].items():
        if not isinstance(job, dict):
            continue
        ro = job.get("runs-on")
        labels = []
        if isinstance(ro, str):
            labels = [ro]
        elif isinstance(ro, list):
            labels = [x for x in ro if isinstance(x, str)]
        elif isinstance(ro, dict):
            g = ro.get("labels")
            labels = [g] if isinstance(g, str) else [x for x in (g or []) if isinstance(x, str)]
        if label not in labels:
            continue
        m = (job.get("strategy") or {}).get("matrix") if isinstance(job.get("strategy"), dict) else None
        n = 1
        if isinstance(m, dict):
            inc = m.get("include")
            axes = [v for k, v in m.items() if k not in ("include", "exclude") and isinstance(v, list)]
            if axes:
                n = 1
                for a in axes:
                    n *= len(a)
                if isinstance(inc, list):
                    # `include` entries that name no existing combination add jobs.
                    # This gate does not model that; a matrix with both shapes is
                    # refused rather than guessed at.
                    refuse("Matrix shape not modelled",
                           f"{path}: job '{name}' has both matrix axes and include; "
                           "the expanded count is ambiguous to this gate, so it refuses "
                           "rather than reporting a number it guessed.")
            elif isinstance(inc, list):
                n = len(inc)
            else:
                refuse("Matrix shape not modelled",
                       f"{path}: job '{name}' has a strategy.matrix this gate cannot count")
        total += n
    return total

facts = {
    "pool-runners": runners,
    "integration-pool-jobs": pool_jobs(os.path.join(wf, "integration.yml")),
    "coverage-pool-jobs": pool_jobs(os.path.join(wf, "coverage.yml")),
}

for k in ("integration-pool-jobs", "coverage-pool-jobs"):
    if facts[k] == 0:
        refuse("No pool job found",
               f"{k} derived as 0 -- no job in that workflow names the '{label}' label. "
               "Either the label moved or the parse is wrong; a gate whose domain is "
               "empty passes by saying nothing, so this refuses.")

try:
    tracked = subprocess.run(["git", "-C", root, "ls-files", "-z"],
                             capture_output=True, check=True).stdout.decode().split("\0")
except Exception as e:
    refuse("Cannot list tracked files", f"git ls-files failed in {root}: {e}")
tracked = [t for t in tracked if t]
if not tracked:
    refuse("No tracked files", f"git ls-files listed nothing in {root}")

MARKER = re.compile(r"ci-pool:\s*([a-z0-9-]+)\s*=\s*(\d+)")
EXEMPT = re.compile(r"ci-pool-exempt:")

WORDS = {"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
         "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
         "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18,
         "nineteen": 19, "twenty": 20}
NUM = r"(?:\d+|" + "|".join(WORDS) + r")"
# The backstop's statement shapes. Keyed on spelling and declared as
# such in the header; each was written from a real site in this tree.
SHAPES = [re.compile(p, re.I) for p in (
    rf"\bpool of (?:{NUM})\b",
    rf"\b{NUM}[- ]runner pool\b",
    rf"\b{NUM} (?:registered )?runners\b",
    rf"\bpool (?:is|was|of) (?:{NUM})\b",
    rf"\bputs (?:{NUM}) jobs\b",
    rf"\b{NUM} (?:privileged|suite|pool) jobs\b",
    rf"\b{NUM} jobs on the pool\b",
)]

marks = []      # (path, lineno, fact, value)
unmarked = []   # (path, lineno, text)
unknown = []    # (path, lineno, fact)

# NO EXEMPTION. Every tracked file in this repository is read. The DHCP
# library is a module dependency, not a directory of this tree.

for rel in tracked:
    p = os.path.join(root, rel)
    if not os.path.isfile(p) or os.path.islink(p):
        continue
    try:
        with open(p, "rb") as fh:
            head = fh.read(8192)
        if b"\0" in head:
            continue
        lines = open(p, encoding="utf-8", errors="replace").read().splitlines()
    except OSError:
        continue
    for i, line in enumerate(lines, 1):
        for m in MARKER.finditer(line):
            fact, val = m.group(1), int(m.group(2))
            if fact not in facts:
                unknown.append((rel, i, fact))
            else:
                marks.append((rel, i, fact, val))
        if any(s.search(line) for s in SHAPES):
            if MARKER.search(line) or EXEMPT.search(line):
                continue
            prev = lines[i - 2] if i >= 2 else ""
            if MARKER.search(prev) or EXEMPT.search(prev):
                continue
            unmarked.append((rel, i, line.strip()))

# `unknown` counts as a marker being present: a tree whose only markers
# name facts nothing derives is not a tree with no markers, and reporting
# it as "nothing is stated" would hide the naming error behind a refusal.
if not marks and not unknown:
    refuse("No pool fact is stated anywhere",
           "no tracked line carries a `ci-pool: <fact>=<value>` marker, so this gate "
           "has nothing to check and would pass having checked nothing.")

rc = 0
wrong = [(p, i, f, v) for (p, i, f, v) in marks if v != facts[f]]
if wrong:
    print("::error title=A stated pool fact disagrees with its derivation (#879)::these lines "
          "state a number that is no longer true. Correct the line, or the thing it "
          "describes.", file=sys.stderr)
    for p, i, f, v in wrong:
        print(f"  {p}:{i}: {f}={v}, derived {facts[f]}", file=sys.stderr)
    rc = 1
if unknown:
    print("::error title=Unknown pool fact (#879)::these markers name a fact this gate does "
          "not derive, so nothing checks them and they read as checked.", file=sys.stderr)
    for p, i, f in unknown:
        print(f"  {p}:{i}: {f} (known: {', '.join(sorted(facts))})", file=sys.stderr)
    rc = 1
if unmarked:
    print("::error title=An unmarked pool statement (#879)::these lines state the pool size "
          "or a per-run job count and carry no `ci-pool: <fact>=<value>` marker, so nothing "
          "would notice them going stale. Add the marker, or `ci-pool-exempt: <reason>` if "
          "the line is not a claim about this pool.", file=sys.stderr)
    for p, i, t in unmarked:
        print(f"  {p}:{i}: {t[:120]}", file=sys.stderr)
    rc = 1

if rc:
    sys.exit(1)

print("pool-facts gate: %d marker(s) agree with the derivation "
      "(pool-runners=%d, integration-pool-jobs=%d, coverage-pool-jobs=%d)"
      % (len(marks), facts["pool-runners"], facts["integration-pool-jobs"],
         facts["coverage-pool-jobs"]))
PY
rc=$?
[ "$rc" -eq 0 ] || exit "$rc"

if [ "$LIVE" -eq 1 ]; then
    label=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["x64_label"])' "$CONST")
    want=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["x64_runners"])' "$CONST")
    repo="${GATE_REPO:-claymore666/docker-net-dhcp}"
    if ! live=$(gh api "repos/${repo}/actions/runners" --paginate \
                --jq "[.runners[]|select(.labels|map(.name)|index(\"${label}\"))]|length" 2>/dev/null); then
        echo "::error title=Live pool unreadable::--live was asked for and the runners API for" \
             "${repo} could not be read. An unreadable pool is not a matching pool, so this" \
             "refuses (#879)." >&2
        exit 2
    fi
    # `gh --jq` over --paginate prints one number per page.
    live=$(printf '%s\n' "$live" | awk '{s+=$1} END {print s+0}')
    if [ "$live" -ne "$want" ]; then
        echo "::error title=The declared pool is not the live pool (#879)::.github/ci-pool.json" \
             "declares x64_runners=${want}; ${repo} has ${live} runner(s) carrying '${label}'." \
             "Correct the constant and re-run the sweep." >&2
        exit 1
    fi
    echo "pool-facts --live: ${live} runner(s) carry '${label}', matching the declared constant"
fi
