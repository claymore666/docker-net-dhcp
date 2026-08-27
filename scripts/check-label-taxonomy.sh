#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The label taxonomy is what .github/labels.yml says it is (#715).
#
# WHY THIS EXISTS
#
# The taxonomy was consolidated once and rotted twice, and both rots were
# found by a person reading the tracker rather than by anything going red.
# The second one, caught immediately before the v1.8.0 release PR: a
# duplicate type label `tests` alongside `testing` with no description; a
# `security` label created during a review with no description and no entry
# in the labeller's allow-list, on 13 issues; ten open issues carrying two
# type labels and seven carrying none.
#
# None of that was catchable, because until #715 no file said which labels
# may exist. `.github/issue-labeler.yml` and the `ALLOWED_LABELS` block are
# inputs to the labeller — a label missing from them is invisible to the
# automation, not forbidden. Two lists that agree with each other while both
# disagree with the tracker is exactly the state that shipped.
#
# THE SPLIT, AND WHY
#
#   --static  properties of the TREE: the declaration is well formed, and
#             the labeller's two lists are subsets of it. Cannot become
#             false without a commit, so it gates pull requests (test.yaml
#             and the local lane).
#
#   --live    properties of the TRACKER: its label set matches the
#             declaration in both directions, descriptions included, and no
#             open issue breaks the type rule or wears a Dependabot label.
#             Can become false with nobody touching the tree — somebody adds a label in the
#             web UI — so gating a pull request on it would charge that cost
#             to whoever pushed next, and would fail on an API blip. It runs
#             on a schedule instead, the same trade (and the same accepted
#             cost) as check-milestone-scope.sh and check-good-first-issues.sh
#             --live: a red schedule is easier to ignore than a red PR, and
#             that is affordable here because the risk window is long.
#
# WHAT IT DELIBERATELY DOES NOT CHECK
#
# `backlog` never coexisting with a milestone is the one taxonomy invariant
# that already has a gate — scripts/check-milestone-scope.sh, which also
# knows how to tell the two fixes apart using `in-dev`. Restating it here
# would mean two scripts going red for one defect and two places to update
# when the rule changes. Colour is not declared either: it carries no rule,
# and gating on it turns a cosmetic edit into a red run.
#
# ABSENT DATA IS NOT A ZERO. An API that answers with an empty label list or
# an empty issue list is reported as "cannot check" (exit 2), never as a
# clean pass. A gate whose data source died must not read as green — that is
# how a check stops meaning anything without anyone noticing.
#
# THE ISSUE LIST IS EVENTUALLY CONSISTENT, as check-milestone-scope.sh
# records in more detail: the label index lags the issue by up to a minute,
# so a red run that follows a fix by seconds may be reporting work already
# done. Confirm with `gh issue view <N> --json labels` and re-run rather than
# believing it. Not retried around here for the same reason it is not there:
# a retry loop trades a self-correcting false positive for a gate that hides
# a real one.
#
# Usage: check-label-taxonomy.sh --static [<labels>] [<map>] [<workflow>]
#        check-label-taxonomy.sh --live   [<labels>]
#   defaults: .github/labels.yml
#             .github/issue-labeler.yml
#             .github/workflows/issue-labeler.yml   (run from the repo root)
# Env:   REPO        owner/name for --live (default: ask `gh`)
#        LT_GH       the `gh` to run for --live (default: gh)
#
# THE SEAM IS THE COMMAND, NOT ITS RESULT (#840). It was the other way round
# until then: LABELS_TSV and ISSUES_TSV supplied the already-rendered result,
# so they sat ABOVE the three queries that fetch it and none of those three
# had ever executed under test — ten self-test cases drove the live rules
# through a data source that impersonated `gh` rather than through `gh`.
#
# Substituting the command instead is what check-good-first-issues.sh
# (GFI_GH) and check-milestone-scope.sh (MS_GH) already do, and it is why the
# queries below ask for --json ONLY. A --jq would put the rendering back
# inside `gh`, where no seam can reach it, and would leave the fixture and
# the query as two independent encodings of one format with nothing tying
# them together: extend the query and the gate reads a field no fixture
# supplies, while the suite stays green.
#
# Exit: 0 clean, 1 a rule is broken, 2 cannot check (bad usage/inputs/API).

set -u

MODE="${1:---static}"
case "$MODE" in
    --static|--live) shift ;;
    *)
        echo "usage: $0 --static|--live [<labels>] [<map>] [<workflow>]" >&2
        exit 2
        ;;
esac

LABELS="${1:-.github/labels.yml}"
MAP="${2:-.github/issue-labeler.yml}"
WORKFLOW="${3:-.github/workflows/issue-labeler.yml}"

if [ ! -f "$LABELS" ]; then
    echo "FAIL  missing declaration: $LABELS" >&2
    exit 2
fi

if ! command -v python3 >/dev/null 2>&1; then
    echo "FAIL  python3 is required" >&2
    exit 2
fi

# ---------------------------------------------------------------- static
if [ "$MODE" = "--static" ]; then
    for f in "$MAP" "$WORKFLOW"; do
        if [ ! -f "$f" ]; then
            echo "FAIL  missing: $f" >&2
            exit 2
        fi
    done

    LABELS="$LABELS" MAP="$MAP" WORKFLOW="$WORKFLOW" python3 - <<'PY'
import os
import re
import sys

ROLES = {"type", "area", "status", "dependabot"}
failures = []


def parse_labels(path):
    """Read the declaration.

    A small hand parser rather than PyYAML, for the reason
    check-issue-label-map.sh gives: the CI image carries no YAML
    dependency, and a parser that accepts less than YAML rejects a file
    that has drifted into a shape a real YAML reader would take
    differently.
    """
    entries = []
    cur = None
    for lineno, raw in enumerate(open(path, encoding="utf-8"), 1):
        line = raw.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        m = re.match(r"^- name:\s*(.+)$", line)
        if m:
            cur = {"line": lineno, "name": unquote(m.group(1))}
            entries.append(cur)
            continue
        m = re.match(r"^  (role|description):\s*(.+)$", line)
        if m:
            if cur is None:
                failures.append(f"{path}:{lineno}: field before any '- name:'")
                continue
            key = m.group(1)
            if key in cur:
                failures.append(f"{path}:{lineno}: duplicate '{key}' for {cur['name']!r}")
            cur[key] = unquote(m.group(2))
            continue
        failures.append(f"{path}:{lineno}: cannot parse {line!r}")
    return entries


def unquote(value):
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
        return value[1:-1]
    return value


labels_path = os.environ["LABELS"]
entries = parse_labels(labels_path)

if not entries:
    print(f"FAIL  {labels_path}: no labels declared", file=sys.stderr)
    sys.exit(1)

declared = {}
for e in entries:
    name = e.get("name", "")
    if not name:
        failures.append(f"{labels_path}:{e['line']}: empty name")
        continue
    if name in declared:
        failures.append(
            f"{labels_path}:{e['line']}: {name!r} declared twice "
            f"(first at line {declared[name]['line']})"
        )
        continue
    role = e.get("role", "")
    desc = e.get("description", "")
    if not role:
        failures.append(f"{labels_path}:{e['line']}: {name!r} has no role")
    elif role not in ROLES:
        failures.append(
            f"{labels_path}:{e['line']}: {name!r} has role {role!r}, "
            f"not one of {', '.join(sorted(ROLES))}"
        )
    if not desc:
        # An undescribed label is how `security` shipped: usable, used, and
        # unexplained to anyone reading the label list in the web UI.
        failures.append(f"{labels_path}:{e['line']}: {name!r} has no description")
    declared[name] = e

# The empty-set guard. A declaration with no type label would let every
# issue pass the "exactly one type" rule vacuously.
types = [n for n, e in declared.items() if e.get("role") == "type"]
if not types:
    failures.append(f"{labels_path}: no label has role 'type'")

# The same guard for the other universal. The live half reports any issue
# wearing a `dependabot`-role label; a declaration with none would make that
# rule pass on every issue in the tracker, silently.
if not [n for n, e in declared.items() if e.get("role") == "dependabot"]:
    failures.append(f"{labels_path}: no label has role 'dependabot'")

# The labeller may only ever apply a type or an area. Letting it reach a
# status or a Dependabot label would have it fabricate workflow state from an
# issue title.
APPLICABLE = {"type", "area"}


def map_labels(path):
    names = []
    for lineno, raw in enumerate(open(path, encoding="utf-8"), 1):
        line = raw.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if not line.startswith((" ", "\t", "-")) and line.rstrip().endswith(":"):
            names.append((lineno, line.rstrip()[:-1].strip()))
    return names


for lineno, name in map_labels(os.environ["MAP"]):
    if name not in declared:
        failures.append(
            f"{os.environ['MAP']}:{lineno}: rule label {name!r} is not declared "
            f"in {labels_path}"
        )
    elif declared[name].get("role") not in APPLICABLE:
        failures.append(
            f"{os.environ['MAP']}:{lineno}: rule label {name!r} has role "
            f"{declared[name].get('role')!r}; the labeller may only apply "
            f"{' or '.join(sorted(APPLICABLE))}"
        )

def block_scalar(text, key, path, failures):
    r"""Read a `key: |` block by INDENTATION, not by regex.

    The obvious regex — `key:\s*\|\s*\n((?:\s+\S.*\n)+)` — is wrong,
    and wrong in the direction that hides itself: `\s` matches a newline,
    so `\s+` walks straight through the blank line that ends the block and
    keeps going to the end of the file. On this repository's own workflow it
    returned 108 entries where 8 were meant (#715).

    That defect survived because the only consumer asked "is every rule
    label IN this set?" — a subset test, which a polluted superset can only
    make pass more easily. A guard fails in one direction; this one was
    never asked the question that would have exposed it.
    """
    lines = text.splitlines()
    want = key + ":"
    for i, ln in enumerate(lines):
        stripped = ln.strip()
        if stripped not in (want + " |", want + " |-", want + " |+"):
            continue
        indent = len(ln) - len(ln.lstrip())
        out = []
        for rest in lines[i + 1:]:
            if not rest.strip():
                out.append("")
                continue
            if len(rest) - len(rest.lstrip()) <= indent:
                break
            out.append(rest.strip())
        return [x for x in out if x]
    failures.append(f"{path}: no {key} block found")
    return None


wf_path = os.environ["WORKFLOW"]
allowed = block_scalar(
    open(wf_path, encoding="utf-8").read(), "ALLOWED_LABELS", wf_path, failures
)
if allowed is not None:
    for name in allowed:
        if name not in declared:
            failures.append(
                f"{wf_path}: ALLOWED_LABELS names {name!r}, which is not declared "
                f"in {labels_path}"
            )
        elif declared[name].get("role") not in APPLICABLE:
            failures.append(
                f"{wf_path}: ALLOWED_LABELS names {name!r}, whose role is "
                f"{declared[name].get('role')!r}; the labeller may only apply "
                f"{' or '.join(sorted(APPLICABLE))}"
            )

if failures:
    for f in failures:
        print(f"FAIL  {f}", file=sys.stderr)
    sys.exit(1)

by_role = {}
for name, e in declared.items():
    by_role.setdefault(e["role"], []).append(name)
summary = ", ".join(f"{len(v)} {k}" for k, v in sorted(by_role.items()))
print(f"Label taxonomy OK (static) — {len(declared)} labels: {summary}.")
PY
    exit $?
fi

# ------------------------------------------------------------------ live
GH="${LT_GH:-gh}"

command -v "$GH" >/dev/null 2>&1 || {
    echo "FAIL  $GH is required for --live" >&2
    exit 2
}

GH_ERR="$(mktemp)"
trap 'rm -f "$GH_ERR"' EXIT

# ASK, THEN JUDGE THE OUTCOMES SEPARATELY (#840).
#
# All three queries were `2>/dev/null` with only the emptiness of stdout
# examined, so a revoked token, a rate limit, an org rename, a network
# failure and a genuinely empty tracker collapsed into one line: "the
# tracker returned no labels — cannot check". Failing closed is right and
# deliberate — absent data is not a zero — but the diagnostic was destroyed,
# and whoever reads a red scheduled run has nothing to act on.
#
# Three outcomes, told apart: the query failed; it succeeded and said
# nothing at all; it succeeded and returned a body. Only the third is
# judged, and the first two now say which one happened.
ask() { # <what> <gh argv...>  -> body on stdout, rc 2 if it cannot be trusted
    local what="$1"; shift
    local out rc
    : > "$GH_ERR"
    out="$("$GH" "$@" 2>"$GH_ERR")"
    rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "FAIL  the $what query failed (exit $rc): $GH $*" >&2
        if [ -s "$GH_ERR" ]; then
            sed 's/^/      /' "$GH_ERR" >&2
        else
            # A non-zero exit with both streams empty carries no reason at
            # all. Say so, rather than printing a bare number and letting it
            # read like a diagnostic.
            echo "      it printed nothing on stderr" >&2
        fi
        return 2
    fi
    if [ -z "$out" ]; then
        echo "FAIL  the $what query succeeded but returned nothing — cannot check" >&2
        return 2
    fi
    printf '%s' "$out"
}

if [ -z "${REPO:-}" ]; then
    REPO_JSON="$(ask "repository" repo view --json nameWithOwner)" || exit 2
    REPO="$(printf '%s' "$REPO_JSON" | python3 -c \
        'import json,sys;print(json.load(sys.stdin).get("nameWithOwner","") or "")' \
        2>/dev/null)"
fi
if [ -z "${REPO:-}" ]; then
    echo "FAIL  cannot determine the repository; set REPO=owner/name" >&2
    exit 2
fi

LIVE_LABELS="$(ask "label list" label list --repo "$REPO" --limit 200 \
    --json name,description)" || exit 2
LIVE_ISSUES="$(ask "open issue list" issue list --repo "$REPO" --state open \
    --limit 200 --json number,labels)" || exit 2

LABELS="$LABELS" LIVE_LABELS="$LIVE_LABELS" LIVE_ISSUES="$LIVE_ISSUES" REPO="$REPO" python3 - <<'PY'
import json
import os
import re
import sys

failures = []


def unquote(value):
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
        return value[1:-1]
    return value


def parse_labels(path):
    entries = {}
    cur = None
    for raw in open(path, encoding="utf-8"):
        line = raw.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        m = re.match(r"^- name:\s*(.+)$", line)
        if m:
            cur = unquote(m.group(1))
            entries[cur] = {}
            continue
        m = re.match(r"^  (role|description):\s*(.+)$", line)
        if m and cur is not None:
            entries[cur][m.group(1)] = unquote(m.group(2))
    return entries


labels_path = os.environ["LABELS"]
declared = parse_labels(labels_path)
if not declared:
    print(f"FAIL  {labels_path}: no labels declared", file=sys.stderr)
    sys.exit(1)

def load(raw, what):
    """The body of a --json query, or exit 2 saying why it cannot be read.

    A 4xx from the API is the mode that matters here: `gh` can exit 0 with
    the error document on stdout and stderr empty, and a body that is merely
    "not what was expected" is what makes a gate report drift and send
    someone to change a thing it never read. It has to be an error, not a
    verdict (#840).
    """
    try:
        value = json.loads(raw)
    except ValueError as e:
        print(f"FAIL  the {what} query did not return JSON: {e}", file=sys.stderr)
        print(f"      it began: {raw[:200]!r}", file=sys.stderr)
        sys.exit(2)
    if not isinstance(value, list):
        print(
            f"FAIL  the {what} query returned a JSON {type(value).__name__}, "
            f"not the expected list",
            file=sys.stderr,
        )
        print(f"      it began: {raw[:200]!r}", file=sys.stderr)
        sys.exit(2)
    if not value:
        # Absent data is not a zero, and neither is a repository with no
        # labels at all: both mean this gate cannot answer.
        print(f"FAIL  the {what} query returned an empty list — cannot check",
              file=sys.stderr)
        sys.exit(2)
    return value


# `description` is null, not absent, for a label created without one — which
# is exactly the state #715 was written about — so `or ""` and not a default.
live = {
    e.get("name", ""): (e.get("description") or "")
    for e in load(os.environ["LIVE_LABELS"], "label list")
}

# Both directions. An undeclared label on the tracker is how `tests` got in;
# a declared label the tracker does not have means the declaration is
# describing a repository that no longer exists.
for name in sorted(set(live) - set(declared)):
    failures.append(
        f"{name!r} exists on {os.environ['REPO']} but is not declared in "
        f"{labels_path} — declare it, or delete it"
    )
for name in sorted(set(declared) - set(live)):
    failures.append(
        f"{name!r} is declared in {labels_path} but does not exist on "
        f"{os.environ['REPO']} — create it, or drop the declaration"
    )

for name in sorted(set(declared) & set(live)):
    want = declared[name].get("description", "")
    got = live[name]
    if want != got:
        failures.append(
            f"{name!r} description differs\n"
            f"    declared: {want!r}\n"
            f"    tracker:  {got!r}"
        )

types = {n for n, e in declared.items() if e.get("role") == "type"}
dependabot = {n for n, e in declared.items() if e.get("role") == "dependabot"}

checked = 0
for issue in load(os.environ["LIVE_ISSUES"], "open issue list"):
    num = issue.get("number")
    on = {lab.get("name", "") for lab in issue.get("labels") or []}
    checked += 1

    have = sorted(on & types)
    if len(have) == 0:
        failures.append(f"#{num} carries no type label")
    elif len(have) > 1:
        failures.append(f"#{num} carries {len(have)} type labels: {', '.join(have)}")

    # `gh issue list` returns issues only, so anything here is an issue and a
    # Dependabot label on it was applied by hand — which the declaration has
    # forbidden in prose since #715 and nothing has ever checked. 21 issues
    # had picked one up, 4 of them open; open is the population this rule
    # judges, so 4 is what it would have caught. `github_actions` had been
    # borrowed as a "CI work" marker and `go` as a "Go code" one, and `ci` is
    # what both are for.
    borrowed = sorted(on & dependabot)
    if borrowed:
        failures.append(
            f"#{num} carries the Dependabot label {', '.join(borrowed)} — "
            f"those are applied by Dependabot to its own pull requests and "
            f"never belong on an issue"
        )

if failures:
    for f in failures:
        print(f"FAIL  {f}", file=sys.stderr)
    print(
        "\nA red run here is a prompt to look, not proof: the tracker's label "
        "index lags\nthe issue by up to a minute. Confirm with "
        "`gh issue view <N> --json labels`.",
        file=sys.stderr,
    )
    sys.exit(1)

print(
    f"Label taxonomy OK (live) — {len(declared)} labels match "
    f"{os.environ['REPO']}, {checked} open issues conform."
)
PY
