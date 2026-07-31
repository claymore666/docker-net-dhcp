#!/usr/bin/env bash
# Issue-labeller rule-map gate (#393). The rule pass in
# .github/workflows/issue-labeler.yml is the cheap half of automatic
# labelling: every title it classifies is an issue that never reaches
# the model pass. A silently broken pattern therefore doesn't fail
# anything at runtime — it just quietly moves cost onto the model and
# mislabels nothing visibly. This gate makes that failure loud.
#
# Three things are checked:
#   1. Every regex in the map compiles.
#   2. Every label the map can apply is one the model pass also knows
#      about (the ALLOWED_LABELS block in the workflow). Two lists that
#      disagree is how a rule starts applying a label nothing else in
#      the system recognises.
#   3. The map classifies a fixture of real issue titles exactly as
#      recorded, negatives included.
#
# Usage: check-issue-label-map.sh [<map>] [<workflow>] [<fixture>]
#   defaults: .github/issue-labeler.yml
#             .github/workflows/issue-labeler.yml
#             scripts/testdata/issue-titles.tsv   (run from the repo root)
#
# Exit: 0 clean, 1 a check failed, 2 cannot check (bad usage/inputs).
#
# Note on regex dialect: the action matches with JavaScript's RegExp and
# this gate uses Python's re. The patterns in the map are deliberately
# kept to the common subset (anchors, groups, character classes,
# quantifiers, the /.../i flag form) so the two agree. Anything needing
# a JS-only or Python-only construct does not belong in the map.
set -u

MAP="${1:-.github/issue-labeler.yml}"
WORKFLOW="${2:-.github/workflows/issue-labeler.yml}"
FIXTURE="${3:-scripts/testdata/issue-titles.tsv}"

for f in "$MAP" "$WORKFLOW" "$FIXTURE"; do
    if [ ! -f "$f" ]; then
        echo "usage: $0 [<map>] [<workflow>] [<fixture>]" >&2
        echo "FAIL  missing: $f" >&2
        exit 2
    fi
done

if ! command -v python3 >/dev/null 2>&1; then
    echo "FAIL  python3 is required" >&2
    exit 2
fi

MAP="$MAP" WORKFLOW="$WORKFLOW" FIXTURE="$FIXTURE" python3 - <<'PY'
import os
import re
import sys

map_path = os.environ["MAP"]
workflow_path = os.environ["WORKFLOW"]
fixture_path = os.environ["FIXTURE"]

failures = []


def parse_map(path):
    """Read the label -> [pattern] map.

    Deliberately a small hand parser rather than PyYAML: the CI image
    carries no YAML dependency and this file's shape is fixed (a label
    key, then one or more '- pattern' lines). A parser that accepts less
    than YAML is a feature here — it rejects a map that has drifted into
    a shape the action would read differently.
    """
    rules = {}
    label = None
    for lineno, raw in enumerate(open(path, encoding="utf-8"), 1):
        line = raw.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if not line.startswith((" ", "\t", "-")) and line.rstrip().endswith(":"):
            label = line.rstrip()[:-1].strip()
            rules.setdefault(label, [])
            continue
        stripped = line.strip()
        if stripped.startswith("- "):
            if label is None:
                failures.append(f"{path}:{lineno}: pattern before any label")
                continue
            value = stripped[2:].strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
                value = value[1:-1]
            rules[label].append((lineno, value))
            continue
        failures.append(f"{path}:{lineno}: cannot parse {line!r}")
    return rules


def compile_pattern(pattern):
    """Mirror the action's /pattern/flags handling; only 'i' is honoured."""
    m = re.match(r"^/(.+)/([a-z]*)$", pattern)
    flags = 0
    body = pattern
    if m:
        body, raw_flags = m.group(1), m.group(2)
        for flag in raw_flags:
            if flag == "i":
                flags |= re.IGNORECASE
            elif flag in ("g", "m", "s", "u", "y"):
                # 'g' is meaningless for a single .match(); the rest would
                # change semantics, so refuse rather than approximate.
                if flag != "g":
                    raise ValueError(f"flag '{flag}' is not supported by this gate")
    return re.compile(body, flags)


def allowed_labels(path):
    """Pull the ALLOWED_LABELS block out of the workflow."""
    text = open(path, encoding="utf-8").read()
    m = re.search(r"^\s*ALLOWED_LABELS:\s*\|\s*\n((?:\s+\S.*\n)+)", text, re.MULTILINE)
    if not m:
        failures.append(f"{path}: no ALLOWED_LABELS block found")
        return set()
    return {ln.strip() for ln in m.group(1).splitlines() if ln.strip()}


rules = parse_map(map_path)
if not rules:
    failures.append(f"{map_path}: no rules found")

# 1. every pattern compiles
compiled = {}
for label, patterns in rules.items():
    if not patterns:
        failures.append(f"{map_path}: label '{label}' has no patterns")
    for lineno, pattern in patterns:
        try:
            compiled.setdefault(label, []).append(compile_pattern(pattern))
        except (re.error, ValueError) as exc:
            failures.append(f"{map_path}:{lineno}: bad regex {pattern!r}: {exc}")

# 2. rule labels are a subset of what the model pass knows
allowed = allowed_labels(workflow_path)
if allowed:
    for label in sorted(rules):
        if label not in allowed:
            failures.append(
                f"{map_path}: label '{label}' is not in ALLOWED_LABELS "
                f"in {workflow_path}"
            )


def classify(title):
    """Reproduce the action: target is the title plus a blank line, and
    several patterns under one label are ANDed."""
    target = f"{title}\n\n"
    hit = []
    for label in sorted(compiled):
        if all(p.search(target) for p in compiled[label]):
            hit.append(label)
    return hit


# 3. the fixture classifies exactly as recorded
checked = 0
for lineno, raw in enumerate(open(fixture_path, encoding="utf-8"), 1):
    line = raw.rstrip("\n")
    if not line.strip() or line.lstrip().startswith("#"):
        continue
    parts = line.split("\t")
    if len(parts) != 2:
        failures.append(f"{fixture_path}:{lineno}: want '<title>\\t<labels>'")
        continue
    title, expected_raw = parts[0], parts[1].strip()
    expected = [] if expected_raw == "-" else sorted(
        p.strip() for p in expected_raw.split(",") if p.strip()
    )
    got = classify(title)
    checked += 1
    if got != expected:
        failures.append(
            f"{fixture_path}:{lineno}: {title!r}\n"
            f"    want [{', '.join(expected) or '-'}]\n"
            f"    got  [{', '.join(got) or '-'}]"
        )

if failures:
    for f in failures:
        print(f"FAIL  {f}", file=sys.stderr)
    sys.exit(1)

print(f"Issue label map OK — {len(rules)} labels, {checked} fixture titles.")
PY
