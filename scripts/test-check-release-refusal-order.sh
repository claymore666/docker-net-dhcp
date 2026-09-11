#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-release-refusal-order.sh.
#
# The first five cases are the REAL release.yml, mutated: the control, then
# the shape the tree shipped at r1 (the only judgement in `github-release`,
# downstream of both registry pushes), then each way the gate could be
# emptied. Mutating the real file rather than a fixture is deliberate --
# a fixture proves the parser reads a fixture.
#
# The last cases are synthetic, for the two graph shapes release.yml does
# not currently contain: a judgement reached through an intermediate job,
# and `needs:` written as a scalar.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
GATE="$HERE/check-release-refusal-order.sh"
REAL="$ROOT/.github/workflows/release.yml"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
no() { printf 'FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# <name> <want-exit> <file> [<expect>]
gate_case() {
    local name="$1" want="$2" file="$3" expect="${4:-}" out rc
    out=$(bash "$GATE" "$file" 2>&1)
    rc=$?
    if [ "$rc" != "$want" ]; then
        no "$name (exit $rc, want $want)"
        printf '      %s\n' "$out" | head -4 >&2
        return
    fi
    if [ -n "$expect" ] && ! printf '%s' "$out" | grep -F -- "$expect" >/dev/null; then
        no "$name (exit $rc as wanted, but the output does not name '$expect')"
        printf '      %s\n' "$out" | head -4 >&2
        return
    fi
    ok "$name"
}

# Rewrite the real workflow through PyYAML. The mutants are edits to the
# PARSED graph, so they cannot accidentally depend on where a line sits.
mutate() { # <outfile> <python-body-operating-on-`d`>
    python3 - "$REAL" "$1" "$2" <<'PY'
import sys, yaml
src, dst, body = sys.argv[1], sys.argv[2], sys.argv[3]
d = yaml.safe_load(open(src, encoding='utf-8'))
exec(body)
yaml.safe_dump(d, open(dst, 'w', encoding='utf-8'))
PY
}

# 1. THE CONTROL.
gate_case "the shipped release.yml passes" 0 "$REAL" "PASS  "

# 2. THE DEFECT AS IT SHIPPED AT r1: the notes are judged only where the
#    body is assembled, which is downstream of both registry pushes.
mutate "$WORK/only-late.yml" '
d["jobs"]["resolve"]["steps"] = [s for s in d["jobs"]["resolve"]["steps"]
                                 if "release-body" not in (s.get("run") or "")]
'
if [ -s "$WORK/only-late.yml" ]; then
    gate_case "judging the notes only in github-release is caught" 1 "$WORK/only-late.yml" "arrives after the images exist"
else
    no "the only-late mutant could not be built"
fi

# 3. No judgement anywhere: the extraction went back inline, or the script
#    was renamed. Neither is a pass.
mutate "$WORK/none.yml" '
for j in d["jobs"].values():
    j["steps"] = [s for s in j.get("steps", []) if "release-body" not in (s.get("run") or "")]
'
if [ -s "$WORK/none.yml" ]; then
    gate_case "no release-body judgement at all refuses" 2 "$WORK/none.yml" "orders nothing"
else
    no "the no-judgement mutant could not be built"
fi

# 4. The judgement moved into the job that pushes. `needs:` orders jobs, not
#    steps, so this reads as ordered while it is not.
mutate "$WORK/inside.yml" '
d["jobs"]["resolve"]["steps"] = [s for s in d["jobs"]["resolve"]["steps"]
                                 if "release-body" not in (s.get("run") or "")]
d["jobs"]["release"]["steps"].insert(0, {"name": "Refuse", "run": "bash scripts/release-body.sh v1.0.0 RELEASE_NOTES.md"})
'
if [ -s "$WORK/inside.yml" ]; then
    gate_case "a judgement inside a publishing job is caught" 1 "$WORK/inside.yml" "both judges the notes and publishes"
else
    no "the judgement-inside mutant could not be built"
fi

# 5. AN EMPTIED DOMAIN IS NOT A PASS. Strip every login and every publishing
#    command and the rule quantifies over nothing.
mutate "$WORK/nopub.yml" '
import re
for j in d["jobs"].values():
    keep = []
    for s in j.get("steps", []):
        if "docker/login-action" in (s.get("uses") or ""):
            continue
        r = s.get("run")
        if isinstance(r, str) and re.search(r"crane |docker push|plugin push|cosign sign|make .*push", r):
            continue
        keep.append(s)
    j["steps"] = keep
'
if [ -s "$WORK/nopub.yml" ]; then
    gate_case "a workflow that appears to publish nothing refuses" 2 "$WORK/nopub.yml" "detector stopped matching"
else
    no "the no-publisher mutant could not be built"
fi

# 6. A judgement reached THROUGH an intermediate job, with `needs:` as a
#    scalar on one edge and a list on the other. release.yml has neither
#    shape today; the gate's answer must not depend on that.
cat > "$WORK/chain.yml" <<'YAML'
name: chain
on: push
jobs:
  judge:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - run: bash scripts/release-body.sh v1.0.0 RELEASE_NOTES.md
  middle:
    needs: judge
    runs-on: ubuntu-latest
    steps:
      - run: echo nothing
  publish:
    needs: [middle]
    runs-on: ubuntu-latest
    steps:
      - uses: docker/login-action@v3
      - run: crane tag a b
YAML
gate_case "a judgement two needs: edges upstream still counts" 0 "$WORK/chain.yml" "judged first by: judge"

# 7. The same graph with the edge broken: the judgement is present, and on
#    no path to the publisher.
cat > "$WORK/parallel.yml" <<'YAML'
name: parallel
on: push
jobs:
  judge:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - run: bash scripts/release-body.sh v1.0.0 RELEASE_NOTES.md
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/login-action@v3
      - run: crane tag a b
YAML
gate_case "a judgement running in parallel with the push is caught" 1 "$WORK/parallel.yml" "no job running release-body.sh is among its needs"

# 8. A subject that is not there is not a subject that passed.
gate_case "a missing workflow refuses" 2 "$WORK/absent.yml" "does not exist"

# --- rule 4: every invocation runs THIS workflow's copy -------------------
#
# 9. THE PREVIOUS VERSION IS THE STRONGEST MUTANT. Point the assembly back
#    at the tag's tree, which is what it read until this round: the
#    pre-flight in `resolve` still passes, the images still publish, and the
#    run dies 127 at the last step for any tag older than the script.
mutate "$WORK/tagcopy.yml" '
for s in d["jobs"]["github-release"]["steps"]:
    r = s.get("run")
    if isinstance(r, str) and ".resolver/scripts/release-body.sh" in r:
        s["run"] = r.replace(".resolver/scripts/release-body.sh", "scripts/release-body.sh")
'
if [ -s "$WORK/tagcopy.yml" ]; then
    gate_case "the assembly reading the tag's copy of the extractor is caught" 1 "$WORK/tagcopy.yml" "checked out at \`ref: "
else
    no "the tag-copy mutant could not be built"
fi

# 10. The other arm of rule 4: the invocation names a directory nothing
#     checks out. A path typo, or the resolver checkout deleted while the
#     call site keeps its spelling -- 127 at run time, silent here.
mutate "$WORK/nocheckout.yml" '
d["jobs"]["github-release"]["steps"] = [
    s for s in d["jobs"]["github-release"]["steps"]
    if (s.get("with") or {}).get("path") != ".resolver"]
'
if [ -s "$WORK/nocheckout.yml" ]; then
    gate_case "an invocation from a directory no checkout produces is caught" 1 "$WORK/nocheckout.yml" "which no actions/checkout in that job produces"
else
    no "the missing-checkout mutant could not be built"
fi

# 11. A COMMENT IS NOT AN INVOCATION. The assembly step's own `run:` scalar
#     explains the script in prose; a substring search reads that as a call
#     from the wrong directory. Plant one more, in the other job, and the
#     verdict must not move.
mutate "$WORK/comment.yml" '
for s in d["jobs"]["resolve"]["steps"]:
    r = s.get("run")
    if isinstance(r, str) and "release-body.sh" in r:
        s["run"] = "# bash scripts/release-body.sh vX.Y.Z RELEASE_NOTES.md -- the shape we do NOT use\n" + r
'
if [ -s "$WORK/comment.yml" ]; then
    gate_case "a commented-out invocation does not change the verdict" 0 "$WORK/comment.yml" "PASS  "
else
    no "the comment mutant could not be built"
fi

# --- rule 5: the pre-flight checks out no tree the trigger names ----------
#
# 12. The shape that raised code-scanning alert 119 (poisonable-step), put
#     back: a second checkout in `resolve` on `steps.tag.outputs.ref`.
mutate "$WORK/poison.yml" '
d["jobs"]["resolve"]["steps"].append({
    "uses": "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
    "with": {"ref": "${{ steps.tag.outputs.ref }}", "path": ".notes",
             "sparse-checkout": "RELEASE_NOTES.md",
             "sparse-checkout-cone-mode": False}})
'
if [ -s "$WORK/poison.yml" ]; then
    gate_case "a trigger-named checkout in the pre-flight judge is caught" 1 "$WORK/poison.yml" "poisonable-step"
else
    no "the poisonable-checkout mutant could not be built"
fi

# 13. DRIVE THE SAFE SHAPE TOO. The same second checkout with a ref that is
#     not an expression is not this defect, and rule 5 must not read as
#     "`resolve` may hold only one checkout" -- a red that fires on any
#     second checkout measures difficulty, not the property.
mutate "$WORK/literalref.yml" '
d["jobs"]["resolve"]["steps"].append({
    "uses": "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
    "with": {"ref": "2.0.0", "path": ".other"}})
'
if [ -s "$WORK/literalref.yml" ]; then
    gate_case "a second checkout on a literal ref is not the defect" 0 "$WORK/literalref.yml" "PASS  "
else
    no "the literal-ref control could not be built"
fi

# 14. The same expression checkout, in the job that ASSEMBLES rather than
#     the one that gates. `github-release` must check the tag's tree out --
#     that tree is the release. Rule 5 is about the pre-flight only, and a
#     rule that cannot tell the two apart would forbid the workflow.
mutate "$WORK/assembly-tree.yml" '
d["jobs"]["github-release"]["steps"].append({
    "uses": "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
    "with": {"ref": "${{ needs.release.outputs.ref }}", "path": ".extra"}})
'
if [ -s "$WORK/assembly-tree.yml" ]; then
    gate_case "a trigger-named checkout outside the pre-flight is allowed" 0 "$WORK/assembly-tree.yml" "PASS  "
else
    no "the assembly-tree control could not be built"
fi

# 15. THE DEFECT REWRITTEN AS A SHELL LINE. `actions/checkout` is one way to
#     put the tag's tree on disk; `git checkout` in the same job is the
#     other, and a rule that reads only `uses:` steps would pass it.
mutate "$WORK/shellcheckout.yml" '
for s in d["jobs"]["resolve"]["steps"]:
    r = s.get("run")
    if isinstance(r, str) and "release-body.sh" in r:
        s["run"] = r.replace("git -C .resolver show",
                             "git -C .resolver checkout \"$REF\" && git -C .resolver show")
'
if [ -s "$WORK/shellcheckout.yml" ]; then
    gate_case "the pre-flight materialising the tag's tree in its shell is caught" 1 "$WORK/shellcheckout.yml" "A working tree is a working tree however it is created"
else
    no "the shell-checkout mutant could not be built"
fi

# 16. AND THE SHAPE THAT IS KEPT. Reading objects -- fetch, show, log, cat-file
#     -- is not materialising a tree, and rule 5 must not read as "the
#     pre-flight may not use git".
mutate "$WORK/gitread.yml" '
for s in d["jobs"]["resolve"]["steps"]:
    r = s.get("run")
    if isinstance(r, str) and "release-body.sh" in r:
        s["run"] = r + "\ngit -C .resolver log -1 --format=%H refs/notes-under-judgement\ngit -C .resolver cat-file -t refs/notes-under-judgement\n"
'
if [ -s "$WORK/gitread.yml" ]; then
    gate_case "reading objects in the pre-flight is not materialising a tree" 0 "$WORK/gitread.yml" "PASS  "
else
    no "the git-read control could not be built"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
