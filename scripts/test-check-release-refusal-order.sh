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

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
