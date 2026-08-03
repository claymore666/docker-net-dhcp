#!/usr/bin/env bash
# Issue state labels (#482). An issue whose work has already merged into
# `dev` is indistinguishable from an untouched one: both read as plain
# OPEN, and the milestone counts them the same. GitHub's "Development"
# sidebar link would say otherwise, but it is only created for PRs that
# target the *default* branch, and every PR here targets `dev` — so
# `Closes #N` degrades to a timeline cross-reference that is not visible
# in the issue list and not queryable.
#
# That is the branching model working as intended (an issue stays open
# until the release PR closes it on `main`, so "closed" means *shipped*,
# not *merged*). This script restores the missing signal without touching
# the model, by maintaining two labels on open issues:
#
#   in-dev   the work merged into `dev` — done, awaiting release
#   has-pr   a PR referencing it is open — someone is on it
#
# `in-dev` wins when both would apply: it is the stronger statement, and
# a follow-up PR should not make a finished issue look unstarted.
#
# WHERE THE REFERENCES COME FROM. Not from PR prose — that is written
# three different ways in this repo ("Closes #N", "Addresses #N (left
# open ...)", and nothing at all). The squash-commit subject convention
# is the reliable carrier and parses 32 of 32 commits on `dev` today:
#
#   test(integration): say which path preserved the address (#386) (#481)
#   ci: shard the main suite three ways ... (#468, #430) (#471)
#
# So: take the parenthesised groups at the END of the subject, walking
# right to left for as long as each group holds nothing but `#<digits>`
# separated by commas. Everything else in the subject is prose and is
# ignored — which is what keeps `#408` out of
#
#   fix(plugin): make the #408 restart wait observable (#422) (#437)
#
# KNOWN LIMIT, deliberately not worked around: a group with prose inside
# it stops the walk, so `... (#356, step 1 of 2) (#448)` yields only 448.
# Loosening the pattern to rescue that case is how a parser starts
# guessing; a subject that wants its issue counted can say so in the
# plain form.
#
# The same limit is why an issue whose subject named only the PR gets no
# label — #472 is the one case on `dev` at the time of writing (its
# subject ends `... (#473)`, the PR, and never names the issue). The
# labels are DERIVED, and every run reconciles: adding one by hand will
# be undone on the next tick. If an issue that is genuinely done shows no
# `in-dev`, the thing to fix is the commit subject, not the label.
#
# HOW A NUMBER IS CLASSIFIED. It isn't, and that is on purpose. Issues
# and PRs share one number sequence, so a ref could be either. Rather
# than spend an API call per ref asking which, the parsed refs are
# INTERSECTED with the list of open issues. A PR number is not in that
# list, and neither is an already-closed issue — both are exactly the
# things we must not label. The intersection is the classifier.
#
# SECURITY. PR titles are attacker-controlled on a public repo. They are
# never executed, never checked out, and are reduced to digits before
# use: the only thing that survives parsing is a bounded integer that
# must already appear in the repo's own open-issue list.
#
# Usage:
#   sync-issue-state-labels.sh              apply the plan
#   sync-issue-state-labels.sh --dry-run    print the plan, change nothing
#   sync-issue-state-labels.sh --parse      read subjects on stdin, print
#                                           the refs — offline, no gh
#   sync-issue-state-labels.sh --plan DIR   print the plan for a directory
#                                           of subjects.txt / issues.json /
#                                           prs.json — offline, no gh
#
# The two offline modes exist so the gate can exercise both halves that
# can actually be wrong: the reference parser, and the reconciliation
# (which label wins, and when a stale one is taken back off).
#
# Exit: 0 clean, 1 something failed, 2 cannot run (bad usage/inputs).
set -u

MODE="apply"
PLAN_DIR=""
case "${1:-}" in
    "") ;;
    --dry-run) MODE="dry-run" ;;
    --parse) MODE="parse" ;;
    --plan)
        MODE="plan"
        PLAN_DIR="${2:-}"
        if [ -z "$PLAN_DIR" ] || [ ! -d "$PLAN_DIR" ]; then
            echo "FAIL  --plan needs a directory holding subjects.txt, issues.json, prs.json" >&2
            exit 2
        fi
        ;;
    -h|--help)
        sed -n '/^# Usage:/,/^# Exit:/p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "usage: $0 [--dry-run|--parse]" >&2
        exit 2
        ;;
esac

if ! command -v python3 >/dev/null 2>&1; then
    echo "FAIL  python3 is required" >&2
    exit 2
fi

# The parser, shared by --parse and the full run. Kept in one place so
# the gate cannot end up testing a second copy that has drifted.
PARSER=$(cat <<'PY'
import re

# A trailing group is refs-only: '#' digits, comma-separated, nothing
# else. The digit cap keeps a pathological subject from producing a
# number no issue could ever have.
_GROUP = re.compile(r"\(\s*#\d{1,7}(?:\s*,\s*#\d{1,7})*\s*\)\s*$")
_NUM = re.compile(r"#(\d{1,7})")


def refs(subject):
    """Numbers from the trailing ref groups of a commit subject or PR
    title, left-to-right, deduplicated. Prose anywhere stops the walk."""
    rest = subject.rstrip()
    groups = []
    while True:
        m = _GROUP.search(rest)
        if not m:
            break
        groups.append(m.group(0))
        rest = rest[: m.start()].rstrip()
    out = []
    for group in reversed(groups):
        for raw in _NUM.findall(group):
            n = int(raw)
            if n and n not in out:
                out.append(n)
    return out
PY
)

if [ "$MODE" = "parse" ]; then
    # python3 -c, not a heredoc: `python3 - <<PY` makes the heredoc the
    # process's stdin, so the subjects being piped in would never be read
    # and this mode would print nothing while exiting 0.
    PARSER="$PARSER" python3 -c '
import os
import sys

exec(os.environ["PARSER"])  # noqa: S102 - defines refs()

for line in sys.stdin:
    for n in refs(line.rstrip("\n")):
        print(f"#{n}")
'
    exit $?
fi

# The reconciliation, likewise in one place. Reads three files out of a
# directory and emits the plan as ADD/REMOVE rows plus one SUMMARY row.
PLANNER=$(cat <<'PY'
import json
import os

exec(os.environ["PARSER"])  # noqa: S102 - defines refs()

tmp = os.environ["TMP"]

issues = json.load(open(f"{tmp}/issues.json", encoding="utf-8"))
open_numbers = {i["number"] for i in issues}
current = {i["number"]: {lbl["name"] for lbl in i["labels"]} for i in issues}

with open(f"{tmp}/subjects.txt", encoding="utf-8") as fh:
    in_dev = {n for line in fh for n in refs(line.rstrip("\n"))} & open_numbers

prs = json.load(open(f"{tmp}/prs.json", encoding="utf-8"))
has_pr = {n for pr in prs for n in refs(pr["title"])} & open_numbers

# in-dev is the stronger claim; a later PR must not un-finish an issue.
has_pr -= in_dev

for number in sorted(open_numbers):
    want = set()
    if number in in_dev:
        want.add("in-dev")
    if number in has_pr:
        want.add("has-pr")
    have = current[number] & {"in-dev", "has-pr"}
    for label in sorted(want - have):
        print(f"ADD\t{number}\t{label}")
    for label in sorted(have - want):
        print(f"REMOVE\t{number}\t{label}")

print(f"SUMMARY\t{len(open_numbers)}\t{len(in_dev)}\t{len(has_pr)}")
PY
)

if [ "$MODE" = "plan" ]; then
    for f in subjects.txt issues.json prs.json; do
        [ -f "$PLAN_DIR/$f" ] || {
            echo "FAIL  missing: $PLAN_DIR/$f" >&2
            exit 2
        }
    done
    PARSER="$PARSER" TMP="$PLAN_DIR" python3 -c "$PLANNER"
    exit $?
fi

for tool in gh git; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FAIL  $tool is required" >&2
        exit 2
    }
done

REPO="${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)}"
if [ -z "$REPO" ]; then
    echo "FAIL  cannot determine the repository" >&2
    exit 2
fi

BASE_BRANCH="${STATE_LABELS_BASE:-main}"
DEV_BRANCH="${STATE_LABELS_DEV:-dev}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# What has merged into dev but not yet shipped on main. If the range is
# not resolvable the run is wrong rather than empty — an empty answer
# here would silently strip in-dev off every issue.
if ! git rev-parse --verify --quiet "origin/$BASE_BRANCH" >/dev/null ||
    ! git rev-parse --verify --quiet "origin/$DEV_BRANCH" >/dev/null; then
    echo "FAIL  need both origin/$BASE_BRANCH and origin/$DEV_BRANCH — fetch them first" >&2
    exit 2
fi
git log "origin/$BASE_BRANCH..origin/$DEV_BRANCH" --format='%s' --no-merges > "$TMP/subjects.txt"

gh issue list --repo "$REPO" --state open --limit 1000 \
    --json number,labels > "$TMP/issues.json" || exit 1
gh pr list --repo "$REPO" --state open --base "$DEV_BRANCH" --limit 200 \
    --json number,title > "$TMP/prs.json" || exit 1

if ! PARSER="$PARSER" TMP="$TMP" python3 -c "$PLANNER" > "$TMP/plan"; then
    echo "FAIL  could not build the plan" >&2
    exit 1
fi

summary=$(grep '^SUMMARY' "$TMP/plan" | head -1)
changes=$(grep -cE '^(ADD|REMOVE)' "$TMP/plan" || true)
echo "issue state labels: $(echo "$summary" | cut -f2) open issues, $(echo "$summary" | cut -f3) in-dev, $(echo "$summary" | cut -f4) has-pr, $changes change(s)"

if [ "$changes" -eq 0 ]; then
    echo "nothing to do."
    exit 0
fi

if [ "$MODE" = "dry-run" ]; then
    grep -E '^(ADD|REMOVE)' "$TMP/plan" | sed 's/^/  /'
    exit 0
fi

# Create the labels on first use so there is no out-of-band setup step
# to forget. Already-exists is not an error.
gh label create in-dev --repo "$REPO" --color 0E8A16 \
    --description "Work merged into dev — done, awaiting release" >/dev/null 2>&1 || true
gh label create has-pr --repo "$REPO" --color FBCA04 \
    --description "An open PR references this issue" >/dev/null 2>&1 || true

failed=0
while IFS=$'\t' read -r action number label; do
    case "$action" in
        ADD) flag="--add-label" ;;
        REMOVE) flag="--remove-label" ;;
        *) continue ;;
    esac
    if gh issue edit "$number" --repo "$REPO" "$flag" "$label" >/dev/null; then
        echo "  $action #$number $label"
    else
        echo "FAIL  $action #$number $label" >&2
        failed=$((failed + 1))
    fi
done < <(grep -E '^(ADD|REMOVE)' "$TMP/plan")

if [ "$failed" -ne 0 ]; then
    echo "$failed label edit(s) failed" >&2
    exit 1
fi
