#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

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
# HOW A NUMBER IS CLASSIFIED. It isn't, and that is on purpose. Issues
# and PRs share one number sequence, so a ref could be either. Rather
# than spend an API call per ref asking which, the parsed refs are
# INTERSECTED with the list of open issues. A PR number is not in that
# list, and neither is an already-closed issue — both are exactly the
# things we must not label. The intersection is the classifier.
#
# THE ONE HOP (#487). A subject sometimes names only the PR:
#
#   test(harness): verify the lease kea granted ... (#473)
#
# #473 is the PR; the issue is #472 and is never mentioned. The first
# version of this script called that the contributor's problem and said
# to fix the commit subject. That is not an available action — the
# subject is GitHub's squash default, taken from the PR title *as it
# stood when the merge button was pressed*, and it is immutable
# afterwards. So the issue read as untouched forever, which is the exact
# condition these labels exist to remove.
#
# Hence: every ref that survives parsing but is NOT an open issue is a
# candidate PR number. Fetch those PRs' titles and run the same parser
# over them, one hop, no recursion. The intersection with the open-issue
# list still does the classifying, so nothing about the trust model
# changes — a title can only ever contribute a number the repo already
# lists as open.
#
# Not `closingIssuesReferences`, which would be the authoritative field:
# PR bodies here say "Closes nothing on dev" by convention, because the
# release PR does the closing on `main`. That field is empty for exactly
# the PRs this needs to resolve. The title is the carrier the repo
# actually uses.
#
# SECURITY. PR titles are attacker-controlled on a public repo. They are
# never executed, never checked out, and are reduced to digits before
# use: the only thing that survives parsing is a bounded integer that
# must already appear in the repo's own open-issue list.
#
# Usage:
#   sync-issue-state-labels.sh              apply the plan
#   sync-issue-state-labels.sh --dry-run    print the plan, change nothing
#   sync-issue-state-labels.sh --parse-title read PR titles on stdin, print
#                                           the refs the RECONCILER would
#                                           see — refs(), not the
#                                           merge-aware commit_refs()
#   sync-issue-state-labels.sh --parse      read subjects on stdin, print
#                                           the refs — offline, no gh
#   sync-issue-state-labels.sh --parse-body read PR prose on stdin, print
#                                           the closing-keyword refs —
#                                           offline, no gh
#   sync-issue-state-labels.sh --plan DIR   print the plan for a directory
#                                           of subjects.txt / issues.json /
#                                           prs.json, plus an optional
#                                           pr_titles.json — offline, no gh
#   sync-issue-state-labels.sh --unresolved DIR
#                                           print the refs in subjects.txt
#                                           that are not open issues, i.e.
#                                           the PRs to look up — offline
#
# The offline modes exist so the gate can exercise every half that can
# actually be wrong: the reference parser, the set of refs the hop will
# spend API calls on, and the reconciliation (which label wins, and when
# a stale one is taken back off).
#
# Exit: 0 clean, 1 something failed, 2 cannot run (bad usage/inputs).
set -u

MODE="apply"
PLAN_DIR=""
case "${1:-}" in
    "") ;;
    --dry-run) MODE="dry-run" ;;
    --parse) MODE="parse" ;;
    --parse-title) MODE="parse-title" ;;
    --parse-body) MODE="parse-body" ;;
    --plan|--unresolved)
        MODE="${1#--}"
        PLAN_DIR="${2:-}"
        if [ -z "$PLAN_DIR" ] || [ ! -d "$PLAN_DIR" ]; then
            echo "FAIL  $1 needs a directory holding subjects.txt, issues.json, prs.json" >&2
            exit 2
        fi
        ;;
    -h|--help)
        sed -n '/^# Usage:/,/^# Exit:/p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "usage: $0 [--dry-run|--parse|--parse-title|--parse-body|--plan DIR|--unresolved DIR]" >&2
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
# GitHub's default subject for the "Create a merge commit" button. It is
# the ONLY thing such a commit says, and it names the PR, never the
# issue — so it is a pure input to the one hop below (#718).
_MERGE_SUBJECT = re.compile(r"^Merge pull request #(\d{1,7}) from \S")
_NUM = re.compile(r"#(\d{1,7})")
_HTML_COMMENT = re.compile(r"<!--.*?-->", re.DOTALL)
_CLOSES = re.compile(
    r"(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s*(#[0-9]+(?:\s*(?:,|and)\s*#[0-9]+)*)"
)
_NUM_BOUNDED = re.compile(r"#([0-9]{1,7})(?![0-9])")


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


def commit_refs(subject):
    """Refs from a COMMIT subject: the trailing-group rule, plus the
    merge-commit form.

    Kept separate from refs() rather than folded into it because the two
    have different trust properties. refs() is also run over PR titles,
    which are attacker-controlled and intersected DIRECTLY with the open
    issues; teaching it the merge form would let a PR titled "Merge pull
    request #500 from x" mark issue #500. A commit subject on `dev` has
    already passed review, and the number it yields is only ever a
    candidate for the hop.
    """
    m = _MERGE_SUBJECT.match(subject.strip())
    if m:
        return [int(m.group(1))]
    return refs(subject)


def body_refs(body):
    """Numbers referenced with closing keywords in PR prose (e.g.
    'Closes #123', 'Fixes #45, #67'), deduplicated. HTML comments are
    stripped first so template examples like '<!-- e.g. Closes #123 -->'
    do not create false references."""
    if not body:
        return []
    clean_body = _HTML_COMMENT.sub("", body)
    out = []
    for m in _CLOSES.finditer(clean_body):
        for raw in _NUM_BOUNDED.findall(m.group(1)):
            n = int(raw)
            if n and n not in out:
                out.append(n)
    return out


def pr_refs(pr):
    """Candidate issue refs from an open PR's title and body."""
    out = refs(pr.get("title", "") or "")
    for n in body_refs(pr.get("body", "") or ""):
        if n not in out:
            out.append(n)
    return out
PY
)

if [ "$MODE" = "parse-title" ]; then
    # THE TITLE HALF, BOUND TO refs() AND NOT commit_refs() (#742).
    # scripts/check-issue-ref.sh used to run --parse over the PR title,
    # which is commit_refs() — the merge-aware variant. commit_refs()'s
    # docstring above says in as many words why the two must not be the
    # same function over a title: a title is attacker-controlled, and the
    # merge form would let "Merge pull request #500 from x" name issue
    # #500.
    #
    # The consequence was the mirror of that and just as bad. The
    # reconciler reads titles with refs() (:307), so a merge-form title
    # satisfied the GATE while leaving the reconciler nothing to read —
    # the exact false green the gate exists to prevent. Measured:
    #   printf 'Merge pull request #500 from evil/branch' | ... --parse
    #     -> #500          (gate: PASS)
    #   refs('Merge pull request #500 from evil/branch')
    #     -> []            (reconciler: nothing)
    #
    # The gate is right not to carry its own regex — that is how two
    # copies drift. It needed the other half exposed, which is this.
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

if [ "$MODE" = "parse" ]; then
    # python3 -c, not a heredoc: `python3 - <<PY` makes the heredoc the
    # process's stdin, so the subjects being piped in would never be read
    # and this mode would print nothing while exiting 0.
    PARSER="$PARSER" python3 -c '
import os
import sys

exec(os.environ["PARSER"])  # noqa: S102 - defines refs()

for line in sys.stdin:
    for n in commit_refs(line.rstrip("\n")):
        print(f"#{n}")
'
    exit $?
fi

# The body half of the same parser, exposed for scripts/check-issue-ref.sh
# (#718). A gate that re-implemented body_refs would be a second copy free
# to drift from the one that actually decides the labels, which is the
# failure this whole file exists to prevent one level down.
if [ "$MODE" = "parse-body" ]; then
    PARSER="$PARSER" python3 -c '
import os
import sys

exec(os.environ["PARSER"])  # noqa: S102 - defines body_refs()

for n in body_refs(sys.stdin.read()):
    print(f"#{n}")
'
    exit $?
fi

# Everything both the planner and the unresolved-ref listing need: the
# open issues, and the refs the dev-window subjects carry. Kept as one
# fragment so the two modes cannot disagree about what a ref is.
LOADER=$(cat <<'PY'
import json
import os

exec(os.environ["PARSER"])  # noqa: S102 - defines refs()

tmp = os.environ["TMP"]

issues = json.load(open(f"{tmp}/issues.json", encoding="utf-8"))
open_numbers = {i["number"] for i in issues}
current = {i["number"]: {lbl["name"] for lbl in i["labels"]} for i in issues}

with open(f"{tmp}/subjects.txt", encoding="utf-8") as fh:
    subject_refs = {n for line in fh for n in commit_refs(line.rstrip("\n"))}

# A ref that is not an open issue is either a PR or an issue already
# closed. Both are things we must not label, and the first is the thing
# worth one lookup — see THE ONE HOP above.
unresolved = sorted(subject_refs - open_numbers)
PY
)

# The reconciliation. Reads the loader's inputs plus an optional
# pr_titles.json ({"473": "...title..."}) carrying the hop's answers, and
# emits the plan as ADD/REMOVE rows plus one SUMMARY row.
PLANNER=$(cat <<'PY'
in_dev = subject_refs & open_numbers

# The hop. A PR title contributes only through the same parser and the
# same intersection, so it can never introduce a number the repo does
# not already list as open. Absent file = no hop, which is what keeps
# the offline fixtures that predate this honest.
titles = {}
titles_path = f"{tmp}/pr_titles.json"
if os.path.exists(titles_path):
    titles = {int(k): v for k, v in json.load(open(titles_path, encoding="utf-8")).items()}
# Bodies are optional and in their own file, so every fixture written
# before this stays valid: absent means no body contribution, never an
# error. That also keeps the pre-existing fixtures as a negative control
# — they must still produce exactly what they did before.
bodies = {}
bodies_path = f"{tmp}/pr_bodies.json"
if os.path.exists(bodies_path):
    bodies = {int(k): v for k, v in json.load(open(bodies_path, encoding="utf-8")).items()}
for number in unresolved:
    title = titles.get(number)
    if title:
        in_dev |= set(refs(title)) & open_numbers
    # The body, with body_refs' closing-keyword rule rather than refs'
    # trailing-"(#N)" rule. A merged PR that names its issue only in the
    # body is the ordinary case here, not an edge one: the squash
    # subject carries the PR's own number and nothing else, so the issue
    # is reachable through the body or not at all.
    body = bodies.get(number)
    if body:
        in_dev |= set(body_refs(body)) & open_numbers

prs = json.load(open(f"{tmp}/prs.json", encoding="utf-8"))
has_pr = {n for pr in prs for n in pr_refs(pr)} & open_numbers

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

UNRESOLVED=$(cat <<'PY'
for number in unresolved:
    print(number)
PY
)

if [ "$MODE" = "plan" ] || [ "$MODE" = "unresolved" ]; then
    for f in subjects.txt issues.json prs.json; do
        [ -f "$PLAN_DIR/$f" ] || {
            echo "FAIL  missing: $PLAN_DIR/$f" >&2
            exit 2
        }
    done
    if [ "$MODE" = "unresolved" ]; then
        PARSER="$PARSER" TMP="$PLAN_DIR" python3 -c "$LOADER
$UNRESOLVED"
    else
        PARSER="$PARSER" TMP="$PLAN_DIR" python3 -c "$LOADER
$PLANNER"
    fi
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
# Merges INCLUDED (#718). `--no-merges` was here, and it meant a branch
# whose own commits never named their issue was invisible: the merge
# commit is the only place the PR number survives, and it was being
# thrown away before the parser ever saw it. Eleven fully-implemented
# issues read as untouched for exactly this reason. Ordinary merge
# subjects ("Merge branch 'main' into dev") parse to nothing and cost
# only a line.
git log "origin/$BASE_BRANCH..origin/$DEV_BRANCH" --format='%s' > "$TMP/subjects.txt"

gh issue list --repo "$REPO" --state open --limit 1000 \
    --json number,labels > "$TMP/issues.json" || exit 1
gh pr list --repo "$REPO" --state open --base "$DEV_BRANCH" --limit 200 \
    --json number,title,body > "$TMP/prs.json" || exit 1

# THE ONE HOP. Ask about exactly the refs that did not land on an open
# issue — no paged listing to truncate, so a ref can never go unlooked-at
# because it fell outside a fetch window. Most are PRs; the rest 404 as
# closed issues, which is not an error, just an answer.
if ! PARSER="$PARSER" TMP="$TMP" python3 -c "$LOADER
$UNRESOLVED" > "$TMP/unresolved"; then
    echo "FAIL  could not determine which refs need looking up" >&2
    exit 1
fi

echo "{}" > "$TMP/pr_titles.json"
echo "{}" > "$TMP/pr_bodies.json"
looked_up=0
resolved=0
while read -r number; do
    [ -n "$number" ] || continue
    looked_up=$((looked_up + 1))
    # One request, both fields. The body is where a PR that does not name
    # its issue in the title says "Closes #N", and asking for it costs
    # nothing extra: it is the same lookup that was already being made.
    #
    # Written as JSON rather than two delimited values because a body is
    # multi-line arbitrary text — any hand-rolled framing here would be a
    # parser bug waiting to happen.
    if ! gh api "repos/$REPO/pulls/$number" \
            --jq '{title: .title, body: (.body // "")}' > "$TMP/pr.$number" 2>/dev/null ||
       [ ! -s "$TMP/pr.$number" ]; then
        # A 404 IS AN ANSWER; EVERY OTHER FAILURE IS NOT (#739).
        #
        # `|| continue` treated them alike, and the two are opposites
        # here. A 404 means the ref was a closed issue rather than a PR,
        # so it legitimately contributes nothing. A 403 secondary rate
        # limit, a 5xx or a dropped connection means we did not find
        # out — and because the planner recomputes desired state from
        # scratch, a ref that contributes nothing becomes REMOVE in-dev
        # on issues that are in dev. This lane runs 40-67 times a day
        # and this repo has hit GitHub's secondary rate limiting before
        # (see missing-runs.yml), so it is not a theoretical branch.
        #
        # Two places in this same file already reason about exactly this
        # hazard and refuse; this was the third and it failed open.
        #
        # -i for the classification rather than for the whole call: the
        # status line is only needed when something went wrong, and the
        # happy path keeps its single --jq request.
        status=$(gh api "repos/$REPO/pulls/$number" -i 2>/dev/null | awk 'NR == 1 { print $2 }')
        if [ "$status" = "404" ]; then
            continue
        fi
        echo "FAIL  could not read repos/$REPO/pulls/$number (HTTP ${status:-unknown})." >&2
        echo "      A ref that cannot be read is not a ref that resolves to nothing." >&2
        echo "      Continuing would plan REMOVE in-dev for issues that ARE in dev." >&2
        exit 2
    fi
    resolved=$((resolved + 1))
done < "$TMP/unresolved"

if [ "$resolved" -ne 0 ]; then
    # Titles AND bodies are attacker-controlled text on a public repo;
    # hand them to python as files rather than through a shell-expanded
    # string. The trust model is unchanged by adding the body: it is
    # reduced to digits by the same parser, and the intersection with the
    # repo's own open-issue list still does all the classifying, so a
    # body can only ever contribute a number that is already an open
    # issue here.
    if ! TMP="$TMP" python3 -c '
import json
import os

tmp = os.environ["TMP"]
titles = {}
bodies = {}
for name in os.listdir(tmp):
    if not name.startswith("pr."):
        continue
    number = name[len("pr."):]
    with open(f"{tmp}/{name}", encoding="utf-8", errors="replace") as fh:
        try:
            pr = json.load(fh)
        except ValueError:
            continue
    title = (pr.get("title") or "").rstrip("\n")
    if title:
        titles[number] = title
    body = pr.get("body") or ""
    if body:
        bodies[number] = body
with open(f"{tmp}/pr_titles.json", "w", encoding="utf-8") as fh:
    json.dump(titles, fh)
with open(f"{tmp}/pr_bodies.json", "w", encoding="utf-8") as fh:
    json.dump(bodies, fh)
'; then
        echo "FAIL  could not collect the PR titles and bodies" >&2
        exit 1
    fi
fi
echo "one-hop lookups: $looked_up ref(s) not an open issue, $resolved resolved to a PR"

if ! PARSER="$PARSER" TMP="$TMP" python3 -c "$LOADER
$PLANNER" > "$TMP/plan"; then
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
