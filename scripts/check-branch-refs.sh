#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every branch name written under `.github/` must resolve to a branch that
# EXISTS on the remote.
#
# WHY THIS EXISTS
#
# A `branches:` filter that admits no existing branch produces no check
# runs, and an absent check is not a red one. That is not a hypothetical:
# at the `2.x-beta` -> `2.0.0-alpha.1` rename three mutants survived the
# whole local lane on PR #907 — a filter restored to a dead branch name,
# the pattern widened to `2*`, and `.github/dependabot.yml`'s
# `target-branch` put back to `dev`. Nothing in the tree read any of them.
# `target-branch` is the loudest of the three because it is not an error
# either: Dependabot falls back to the default branch, so a rename that
# missed that file opens next Sunday's dependency PRs against `main`.
#
# One milestone later the same class arrived again (PR #912, the rename to
# `2.0.0`), which is what made it a gate rather than a note.
#
# WHAT IT WALKS
#
#   1. every `branches:` and `branches-ignore:` list in every workflow's
#      `on:` block, at any depth (push, pull_request, pull_request_target,
#      workflow_run, …), block or flow style;
#   2. every `target-branch` in `.github/dependabot.yml`;
#   3. every word of `GATE_SCOPE_BRANCHES` in
#      `.github/gate-branch-scope.env` — the branch scope
#      `check-missing-runs.sh` reconciles and `purge-workflow-runs.sh`
#      spares. A dead name there is silent in the direction that deletes
#      evidence.
#
# A LITERAL AND A PATTERN CARRY DIFFERENT OBLIGATIONS, and collapsing them
# is how a gate like this passes over nothing: a literal name must EXIST, a
# pattern must MATCH AT LEAST ONE existing branch. `2*` and `2.*` both
# match `2.0.0` today, so "the pattern is well formed" says nothing; what
# had to be checked is that the set it selects is not empty. The matching
# is `scripts/branch-glob.sh`, the same matcher the two gate-scope readers
# use, with GitHub's filter semantics (`*` stops at `/`, `**` does not).
#
# WHERE THE BRANCH LIST COMES FROM, AND WHY NOT THE LOCAL ONE
#
# `git ls-remote --heads`, never `git branch` or `git for-each-ref`. A
# clone carries whatever branches it happened to fetch — a workstation has
# the ones you worked on, a CI checkout has one — so the local list would
# make this gate answer differently on every machine, and both directions
# are wrong: a name that exists only here would pass, and a name that
# exists only on the remote would fail.
#
# AND AN UNREACHABLE REMOTE IS NOT A PASS. Name which state is the normal
# one: in CI and on a workstation the remote is reachable, and this gate is
# read as "every branch name resolves". A gate that exits 0 when it could
# not ask would render that sentence out of a measurement nobody took, so
# it REFUSES (exit 2) naming the remote instead.
#
# WHAT IT CANNOT DO
#
#   - `tags:` filters are outside the domain: a tag pattern that matches
#     nothing today is normal (the tag has not been cut yet).
#   - A branch name written anywhere else — in a script, a document, an
#     `if:` expression, a `ref:` input — is not walked. The three sources
#     above are the ones a rename has to edit, which is the failure class
#     this gate was built from.
#   - It judges existence, not intent. A filter naming a branch that exists
#     but is the wrong one passes here.
#
# Usage: check-branch-refs.sh
# Env:   BRANCH_REFS_WF_DIR      workflows to walk (default .github/workflows)
#        BRANCH_REFS_DEPENDABOT  dependabot file (default .github/dependabot.yml)
#        BRANCH_REFS_SCOPE       branch-scope env file
#                                (default .github/gate-branch-scope.env)
#        BRANCH_REFS_REMOTE      remote to ask (default origin)
#        BRANCH_REFS_HEADS_FILE  read `git ls-remote --heads` OUTPUT from
#                                this file instead of running it -- the
#                                transport seam the self-test drives, so
#                                the ref parsing below is the real one
# Exit:  0 every name resolves, 1 at least one does not, 2 cannot judge
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"

# shellcheck source=scripts/branch-glob.sh
. "$HERE/branch-glob.sh"

WF_DIR="${BRANCH_REFS_WF_DIR:-$ROOT/.github/workflows}"
DEPENDABOT="${BRANCH_REFS_DEPENDABOT:-$ROOT/.github/dependabot.yml}"
SCOPE="${BRANCH_REFS_SCOPE:-$ROOT/.github/gate-branch-scope.env}"
REMOTE="${BRANCH_REFS_REMOTE:-origin}"

refuse() {
    echo "::error title=Branch references cannot be judged::$*" >&2
    exit 2
}

command -v python3 >/dev/null 2>&1 || refuse \
    "python3 is required to parse the workflow triggers; a line scan sees one spelling of a branch filter out of several and every miss is in the direction that passes."
python3 -c 'import yaml' 2>/dev/null || refuse \
    "python3 cannot import yaml. The branch filters are read from the PARSED document -- block and flow sequences, quoted and unquoted keys -- and there is no line-scan fallback here on purpose: the fallback is the defect."

[ -d "$WF_DIR" ] || refuse "${WF_DIR} is not a directory"
[ -f "$DEPENDABOT" ] || refuse \
    "${DEPENDABOT} does not exist. It is one of the three files a branch rename has to edit, and a gate that passes because its subject vanished is the failure this gate is about."
[ -f "$SCOPE" ] || refuse "${SCOPE} does not exist"

# --- the branches that exist ------------------------------------------
if [ -n "${BRANCH_REFS_HEADS_FILE:-}" ]; then
    [ -f "$BRANCH_REFS_HEADS_FILE" ] || refuse "BRANCH_REFS_HEADS_FILE=${BRANCH_REFS_HEADS_FILE} does not exist"
    ls_out=$(cat "$BRANCH_REFS_HEADS_FILE")
    ls_rc=$?
    origin_desc="${BRANCH_REFS_HEADS_FILE}"
else
    ls_out=$(git -C "$ROOT" ls-remote --heads "$REMOTE" 2>&1)
    ls_rc=$?
    origin_desc="${REMOTE}"
fi
if [ "$ls_rc" -ne 0 ]; then
    refuse "could not list the heads of '${origin_desc}' (git ls-remote exited ${ls_rc}). The normal state on this box and in CI is a reachable remote; an unreachable one is not a pass, because every name below would then 'match nothing' for a reason that has nothing to do with the tree. It said: $(printf '%s' "$ls_out" | tr '\n' ' ' | cut -c1-300)"
fi

# `refs/heads/` only, and the ref name is taken as everything after it, so
# a branch with a slash in it survives the parse.
HEADS=$(printf '%s\n' "$ls_out" | sed -n 's|^[0-9a-f]\{40\}[[:space:]]\{1,\}refs/heads/||p')
n_heads=$(printf '%s\n' "$HEADS" | grep -c .)
if [ "$n_heads" -eq 0 ]; then
    refuse "'${origin_desc}' answered with no refs/heads/ lines at all. Comparing every branch name against an empty set would report either everything broken or -- if the loop below were ever emptied too -- nothing checked."
fi

# --- the names that are written ---------------------------------------
#
# TSV: <where>\t<name>. `where` is a file and a path inside it, because a
# failure has to say which of forty branch filters is the dead one.
subjects=$(python3 - "$WF_DIR" "$DEPENDABOT" <<'PARSE'
import os
import sys

try:
    import yaml
except ImportError:                                  # pragma: no cover
    sys.stderr.write("PyYAML is not importable by this python3\n")
    sys.exit(3)

wf_dir, dependabot = sys.argv[1], sys.argv[2]
rows = []


def emit(where, value):
    # A filter entry is a scalar. Anything else (a mapping, a nested list)
    # is a shape this gate has never seen, and printing it as a branch name
    # would produce a diagnosis nobody can act on -- so it is reported as
    # its own kind of unreadable.
    if isinstance(value, str):
        rows.append((where, value))
    elif isinstance(value, (int, float, bool)) or value is None:
        rows.append((where, str(value)))
    else:
        rows.append(("UNREADABLE " + where, repr(value)))


def walk(node, where):
    """Every `branches:` / `branches-ignore:` under an `on:` block."""
    if isinstance(node, dict):
        for k, v in node.items():
            key = k if isinstance(k, str) else str(k)
            if key in ("branches", "branches-ignore"):
                if isinstance(v, list):
                    for i, item in enumerate(v):
                        emit("%s.%s[%d]" % (where, key, i), item)
                else:
                    # `branches: dev` is not what GitHub documents, but it
                    # parses, and a name written that way must still
                    # resolve.
                    emit("%s.%s" % (where, key), v)
            else:
                walk(v, "%s.%s" % (where, key))
    elif isinstance(node, list):
        for i, item in enumerate(node):
            walk(item, "%s[%d]" % (where, i))


for name in sorted(os.listdir(wf_dir)):
    if not name.endswith((".yml", ".yaml")):
        continue
    path = os.path.join(wf_dir, name)
    with open(path, encoding="utf-8") as fh:
        try:
            doc = yaml.safe_load(fh)
        except yaml.YAMLError as exc:
            sys.stderr.write("cannot parse %s: %s\n" % (path, exc))
            sys.exit(3)
    if not isinstance(doc, dict):
        continue
    # YAML 1.1 resolves an unquoted `on` to the boolean True, so the
    # trigger block's key is True and not the string "on" unless somebody
    # quoted it. Reading doc["on"] alone finds nothing in every real
    # workflow in this tree.
    for k, v in doc.items():
        if k is True or (isinstance(k, str) and k.lower() == "on"):
            walk(v, "%s:on" % name)

with open(dependabot, encoding="utf-8") as fh:
    try:
        dep = yaml.safe_load(fh)
    except yaml.YAMLError as exc:
        sys.stderr.write("cannot parse %s: %s\n" % (dependabot, exc))
        sys.exit(3)
updates = dep.get("updates") if isinstance(dep, dict) else None
if isinstance(updates, list):
    for i, upd in enumerate(updates):
        if isinstance(upd, dict) and "target-branch" in upd:
            emit("dependabot.yml:updates[%d].target-branch" % i, upd["target-branch"])

for where, value in rows:
    sys.stdout.write("%s\t%s\n" % (where, value))
PARSE
)
py_rc=$?
[ "$py_rc" -eq 0 ] || refuse "the workflow/dependabot parse failed (python3 exited ${py_rc})"

# The scope file's word list, read as text rather than sourced: this gate
# has no business running that file, and both scripts that DO source it
# already refuse anything but the two assignments.
# `tail -1`, because the file is SOURCED by its two readers and a second
# assignment would win there. Both of them refuse a duplicate outright, so
# this cannot arise in a file they accept; it is written the same way round
# anyway, so that if it ever does, this gate judges the value that would be
# used rather than one that would not.
scope_words=$(sed -n 's/^[[:space:]]*GATE_SCOPE_BRANCHES=//p' "$SCOPE" | tr -d '"' | tail -1)
# PATHNAME EXPANSION OFF FOR THE SPLIT. This value's character class admits
# `*` and `?` -- a scope word may be a filter pattern, `2.*` -- and an
# unquoted expansion globs against the CURRENT DIRECTORY before it splits.
# MEASURED: from a directory holding a file named `2.zzz-not-a-branch`, the
# word `2.*` was replaced by that filename and this gate failed on a branch
# name written nowhere; from a directory where the pattern matched something
# real, the pattern was never tested as a pattern and the
# must-match-a-branch obligation was dropped. Either way the verdict was a
# function of the working directory.
#
# This is one of the sites that split this value; the population is not a
# number written here, it is what scripts/test-scope-splitting-sites.sh
# discovers and drives -- an earlier count in prose said four and was wrong
# about this very line.
set -f
for w in $scope_words; do
    subjects="${subjects}
gate-branch-scope.env:GATE_SCOPE_BRANCHES	${w}"
done
set +f

n_subj=$(printf '%s\n' "$subjects" | grep -c .)
if [ "$n_subj" -eq 0 ]; then
    refuse "no branch name was extracted from ${WF_DIR}, ${DEPENDABOT} or ${SCOPE}. This tree has dozens; an empty extraction means the walk broke, and it would report success having compared nothing."
fi

# --- judge -------------------------------------------------------------
fail=0
literals=0
patterns=0
while IFS=$'\t' read -r where name; do
    [ -n "$where" ] || continue
    case "$where" in
        UNREADABLE\ *)
            echo "FAIL  ${where#UNREADABLE }: the entry is not a name this gate can read: ${name}" >&2
            fail=1
            continue ;;
    esac

    if branch_glob_is_pattern "$name"; then
        patterns=$((patterns + 1))
        matched=$(branch_glob_expand "$name" "$HEADS")
        case $? in
            0) ;;
            2) echo "FAIL  ${where}: the pattern '${name}' uses filter syntax scripts/branch-glob.sh does not implement (+, !, [ ] or a backslash escape). It is not matched approximately: refusing is the only answer that does not invent a population." >&2
               fail=1
               continue ;;
            *) echo "FAIL  ${where}: the pattern '${name}' matches no branch on ${origin_desc}." >&2
               echo "      A filter that admits nothing produces no check runs, and an absent check is not a red one." >&2
               echo "      Branches that exist: $(printf '%s\n' "$HEADS" | tr '\n' ' ')" >&2
               fail=1
               continue ;;
        esac
        printf '      %-52s %s -> %s\n' "$where" "$name" "$(printf '%s' "$matched" | tr '\n' ' ')"
    else
        literals=$((literals + 1))
        if printf '%s\n' "$HEADS" | grep -xF -- "$name" >/dev/null; then
            printf '      %-52s %s\n' "$where" "$name"
        else
            echo "FAIL  ${where}: '${name}' is not a branch on ${origin_desc}." >&2
            echo "      Branches that exist: $(printf '%s\n' "$HEADS" | tr '\n' ' ')" >&2
            fail=1
        fi
    fi
done <<EOF
$subjects
EOF

if [ "$fail" -ne 0 ]; then
    exit 1
fi

echo "PASS  ${n_subj} branch reference(s) under .github/ resolve on ${origin_desc}: ${literals} literal, ${patterns} pattern(s), against ${n_heads} existing branch(es)"
exit 0
