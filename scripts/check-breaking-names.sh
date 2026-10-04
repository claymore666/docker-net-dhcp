#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every operator-facing name that changed or disappeared since the previous
# release must be named in the unreleased section of RELEASE_NOTES.md (#856).
#
# Expires-when: the release notes are generated from the code's name tables
#   instead of written by hand, or the plugin stops exposing /metrics,
#   /Plugin.Health or the lease ledger (#856).
#
# WHY. The upgrade notes are what tells an operator which dashboards and
# alerts to fix. Reviewing one table against the code found a counter whose
# "_v4/_v6 name split" never existed on /metrics and a /Plugin.Health key
# rename the table did not mention, on a head green on every required check.
# check-release-notes-symbols.sh reads Go symbols; every name on these three
# surfaces is snake_case, and none of its candidates were one.
#
# SUBJECT. The difference between two trees, never the current tree: a removed
# name does not resolve at HEAD and its row is correct. Per surface the set is
# (names at the previous tag) minus (names at HEAD). A name only HEAD has is an
# addition and breaks no scraper, so it is never in the set.
#   metric names   `# TYPE <name>` in pkg/plugin/testdata/metrics_exposition.golden
#   health keys    json tags of `type HealthResponse struct` in pkg/plugin/endpoints.go
#   ledger kinds   the literal first argument of audit( and auditFrom( in Go sources
#
# PREVIOUS TAG. The highest vX.Y.Z tag with no suffix, so -rcN is never picked,
# in version order, and an ancestor of HEAD when the history is complete. A
# shallow clone (CI, `git fetch --tags --depth=1`) cannot say what is an
# ancestor and `git describe` finds nothing there, so the ancestor test is
# skipped when the repository is shallow. Names are read with `git show`.
#
# NOTES REGION. From the first `## v` heading down to the previous tag's
# heading: every section not yet released. A name is mentioned when its whole
# identifier is inside a backtick span or a fenced block there.
#   changed set empty                        pass, with or without a section
#   changed set non-empty, no section        fail: a section must be added
#   changed set non-empty, names unmentioned fail, naming them
#
# BOUNDS. Names only: a type, label set, unit or payload that changes under a
# kept name is not seen. Presence, not truth: the gate cannot read whether the
# sentence says removed or renamed. A name counts on any surface, and a generic
# word (bound, renew, config, stopped) inside any backtick span of the region
# satisfies the kind of that name. Labels, options, flags and log lines are
# not read. Refused (exit 2): no stable tag, the previous tag's heading is not
# in the notes, a source unreadable or parsing to zero names at either tree, an
# audit( call whose kind is not a literal, a HealthResponse field with no json
# tag.
#
# Usage: check-breaking-names.sh [<tree>]
# Exit:  0 clean, 1 a changed name is not in the notes, 2 cannot check.
set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

cd "$(dirname "$0")/.." || exit 2
cd "${1:-.}" || exit 2

command -v python3 >/dev/null 2>&1 || gate_refuse "python3 is required by check-breaking-names"
command -v git >/dev/null 2>&1 || gate_refuse "git is required by check-breaking-names"

gofiles=()
gate_subjects gofiles go-src

FILES="$(printf '%s\n' "${gofiles[@]}")" python3 - <<'PY'
import os
import re
import subprocess
import sys

GOLDEN = "pkg/plugin/testdata/metrics_exposition.golden"
ENDPOINTS = "pkg/plugin/endpoints.go"
NOTES = "RELEASE_NOTES.md"
STABLE = re.compile(r"^v([0-9]+)\.([0-9]+)\.([0-9]+)$")


def refuse(msg):
    print(f"::error title=check-breaking-names cannot judge::{msg}", file=sys.stderr)
    sys.exit(2)


def git(*args):
    p = subprocess.run(["git", *args], capture_output=True)
    return p.returncode, p.stdout.decode("utf-8", "replace"), p.stderr.decode("utf-8", "replace")


def previous_tag():
    rc, out, err = git("tag", "--list")
    if rc != 0:
        refuse(f"git tag --list failed: {err.strip()}")
    tags = []
    for t in out.split():
        m = STABLE.match(t)
        if m:
            tags.append((tuple(int(x) for x in m.groups()), t))
    tags.sort(reverse=True)
    if not tags:
        refuse("no vX.Y.Z release tag resolves here; a shallow clone needs `git fetch --tags --depth=1 origin`, and passing would have compared nothing")
    rc, out, _ = git("rev-parse", "--is-shallow-repository")
    shallow = rc == 0 and out.strip() == "true"
    for _, t in tags:
        if shallow or git("merge-base", "--is-ancestor", f"refs/tags/{t}", "HEAD")[0] == 0:
            return t
    refuse("no release tag is an ancestor of HEAD")


def tree_file(ref, path):
    if ref is None:
        try:
            with open(path, encoding="utf-8") as fh:
                return fh.read()
        except (OSError, UnicodeDecodeError) as exc:
            refuse(f"cannot read {path} at HEAD: {exc}")
    rc, out, err = git("show", f"refs/tags/{ref}:{path}")
    if rc != 0:
        refuse(f"cannot read {path} at {ref}: {err.strip()}")
    return out


def go_sources(ref):
    if ref is None:
        for path in os.environ["FILES"].splitlines():
            if path:
                yield path, tree_file(None, path)
        return
    rc, out, err = git("ls-tree", "-r", "--name-only", f"refs/tags/{ref}")
    if rc != 0:
        refuse(f"cannot list {ref}: {err.strip()}")
    for path in out.splitlines():
        parts = path.split("/")
        if path.endswith(".go") and not path.endswith("_test.go") and "testdata" not in parts:
            yield path, tree_file(ref, path)


def metric_names(ref):
    return set(re.findall(r"^# TYPE (\S+) ", tree_file(ref, GOLDEN), re.M))


def health_keys(ref):
    body, inside = [], False
    for line in tree_file(ref, ENDPOINTS).split("\n"):
        if line.startswith("type HealthResponse struct {"):
            inside = True
            continue
        if inside and line.startswith("}"):
            break
        if inside:
            body.append(line)
    keys = set()
    for line in body:
        s = line.strip()
        if not s or s.startswith("//"):
            continue
        m = re.search(r'json:"([^",]*)', s)
        if not m:
            refuse(f"a HealthResponse field has no json tag at {ref or 'HEAD'}: {s}")
        if m.group(1) not in ("", "-"):
            keys.add(m.group(1))
    return keys


DEFN = re.compile(r"^func \([^)]*\)\s+audit(?:From)?\(")
CALL = re.compile(r"\baudit(?:From)?\(")
LITERAL = re.compile(r'\baudit(?:From)?\(\s*"([A-Za-z0-9_]+)"\s*,')


def ledger_kinds(ref):
    kinds = set()
    for path, text in go_sources(ref):
        wrapper = False
        for n, line in enumerate(text.split("\n"), 1):
            s = line.strip()
            if line.startswith("func "):
                wrapper = bool(DEFN.search(line))
                continue
            if line.startswith("}"):
                wrapper = False
            if wrapper or s.startswith("//") or not CALL.search(s):
                continue
            lits = LITERAL.findall(s)
            if len(lits) != len(CALL.findall(s)):
                refuse(f"{path}:{n} calls audit with a kind that is not a plain literal; the kind set cannot be read at {ref or 'HEAD'}: {s}")
            kinds.update(lits)
    return kinds


SURFACES = (
    ("metric name", metric_names),
    ("health key", health_keys),
    ("ledger kind", ledger_kinds),
)


def mentioned(region):
    prose, tokens, fence = [], set(), False
    for line in region:
        if line.lstrip().startswith("```"):
            fence = not fence
            continue
        if fence:
            tokens.update(re.findall(r"[A-Za-z0-9_]+", line))
        else:
            prose.append(line)
    for span in re.findall(r"`([^`]+)`", "\n".join(prose)):
        tokens.update(re.findall(r"[A-Za-z0-9_]+", span))
    return tokens


prev = previous_tag()
rc, sha, _ = git("rev-parse", "--short", f"refs/tags/{prev}^{{commit}}")
print(f"check-breaking-names: previous release {prev} ({sha.strip()}), notes {NOTES}")

changed = []
for label, read in SURFACES:
    before, after = read(prev), read(None)
    if not before:
        refuse(f"{label}s parse to zero names at {prev}; the source moved or the parser is stale, and an empty set here would read as clean")
    if not after:
        refuse(f"{label}s parse to zero names at HEAD; the source moved or the parser is stale, and an empty set here would read as clean")
    gone = sorted(before - after)
    print(f"  {label}s: {len(before)} at {prev}, {len(after)} at HEAD, {len(gone)} changed")
    changed += [(label, n) for n in gone]
print(f"changed: {len(changed)}")

if not os.path.isfile(NOTES):
    refuse(f"{NOTES} is not a file in this tree")
with open(NOTES, encoding="utf-8") as fh:
    lines = fh.read().split("\n")
first = next((i for i, l in enumerate(lines) if re.match(r"^## v[0-9]", l)), None)
heading = re.compile(r"^## " + re.escape(prev) + r"(?:\s|$)")
at = next((i for i, l in enumerate(lines) if heading.match(l)), None)
if first is None or at is None:
    refuse(f"{NOTES} has no `## {prev}` heading; without it the unreleased region cannot be told from the released ones")
region = lines[first:at]

if not changed:
    print(f"OK: no name changed since {prev}.")
    sys.exit(0)

if not region:
    print(f"FAIL  {len(changed)} name(s) changed since {prev} and {NOTES} has no section above `## {prev}`.", file=sys.stderr)
    for label, n in changed:
        print(f"  {label} {n}", file=sys.stderr)
    print("Add the section for the next release and name each one (#856).", file=sys.stderr)
    sys.exit(1)

seen = mentioned(region)
missing = [(l, n) for l, n in changed if n not in seen]
if missing:
    print(f"FAIL  {len(missing)} of {len(changed)} changed name(s) are not in the unreleased section of {NOTES}:", file=sys.stderr)
    for label, n in missing:
        print(f"  {label} {n}", file=sys.stderr)
    print("Name each in backticks in the section above the previous release (#856).", file=sys.stderr)
    sys.exit(1)
print(f"OK: all {len(changed)} name(s) changed since {prev} are named in the unreleased section.")
PY
