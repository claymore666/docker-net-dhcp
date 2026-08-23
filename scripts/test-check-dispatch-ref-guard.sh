#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-dispatch-ref-guard.sh (#593, #738), on fixture
# workflow directories plus one assertion against the real
# .github/workflows.
#
# THE CASE THIS SUITE WAS REWRITTEN FOR is `othername`: a dispatch input
# called something other than `ref` reaching a checkout. The previous
# checker matched the literal string `inputs.ref`, so release.yml and
# pages.yml — both taking `inputs.tag` into a checkout, one of them into
# a job holding contents: write that then executes the checked-out tree
# — reported as "nothing to guard" and exited 0. Run against the real
# pre-fix workflows the rewritten checker reports both, plus the two
# jobs that inherit the tag transitively; the fixtures keep that
# provable without depending on git history.
#
# THE OTHER CASES THAT CARRY THEIR WEIGHT:
#
#   - A CONSUMER WITH NO `needs:` AT ALL. An early version of the
#     original checker silently passed this: the scanner emitted an
#     empty needs field, tab is an IFS whitespace character, bash `read`
#     collapsed the run of tabs, and every later field shifted left — so
#     the job read as "does not consume the input". It reported two
#     findings where there were three and looked like it was working.
#   - TRANSITIVE reach, because that is how the protection actually
#     works: a failed guard skips everything downstream of it, however
#     many hops away.
#   - EACH HOP the taint can take — a step output, a job output, and an
#     `env:` variable. A workflow-level `env:` is the laundering path
#     that lets a checkout consume an input without naming it.
#   - THE RESOLVER as the second accepted proof, for the job whose
#     checkout IS the thing being protected and which therefore has no
#     earlier point to gate.
#   - EXIT 2 WHEN NOTHING WAS DISCOVERED. #738's actual failure was a
#     green report over an empty examination, so that is now an error
#     rather than a pass.
#   - THE REAL WORKFLOWS. A checker that only ever sees its own fixtures
#     proves nothing about this repository.
#
# MUTANT COVERAGE: cases expecting exit 0, 1 and 2 are all present, so
# neither an always-exit-0 nor an always-exit-1 checker survives.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-dispatch-ref-guard.sh"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
n=0

# check NAME WANT_EXIT DIR GREP_PATTERN
check() {
    local name="$1" want_exit="$2" dir="$3" want_grep="$4"
    n=$((n + 1))
    bash "$CHECK" "$dir" > "$TMP/out" 2>&1
    local got=$?
    local ok=1
    [ "$got" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

mkdir -p "$TMP/unguarded" "$TMP/direct" "$TMP/transitive" "$TMP/guardonly" \
         "$TMP/commented" "$TMP/nodispatch" "$TMP/shapes" "$TMP/cycle" \
         "$TMP/empty" "$TMP/othername" "$TMP/stephop" "$TMP/jobhop" \
         "$TMP/envhop" "$TMP/resolver" "$TMP/nosink"

# --- THE #738 BUG: an input that is not called `ref` -------------------
cat > "$TMP/othername/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      tag:
        description: "Tag to release"
        required: true
jobs:
  release:
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.tag }}
      - run: mkdocs build
YAML
check "#738: an input not called 'ref' is still a consumer" 1 "$TMP/othername" \
      "the input is passed straight to checkout"

# --- a consumer with no needs: at all ---------------------------------
cat > "$TMP/unguarded/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  suite:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.ref }}
YAML
check "a consumer with no needs: at all" 1 "$TMP/unguarded" "job suite"

# --- direct reach ------------------------------------------------------
cat > "$TMP/direct/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  dispatch-ref:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
      - env:
          INPUT_REF: ${{ inputs.ref }}
        run: bash scripts/check-dispatch-ref.sh "$INPUT_REF"
  suite:
    needs: dispatch-ref
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.ref }}
YAML
check "direct needs: on the guard" 0 "$TMP/direct" "are constrained by"

# --- transitive reach --------------------------------------------------
sed 's/^  suite:/  middle:\n    needs: dispatch-ref\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hop\n  suite:/; s/^    needs: dispatch-ref$/    needs: middle/2' \
    "$TMP/direct/w.yml" > "$TMP/transitive/w.yml"
check "transitive reach through an intermediate job" 0 "$TMP/transitive" \
      "are constrained by"

# --- the guard job is itself allowed to name the input -----------------
cat > "$TMP/guardonly/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  dispatch-ref:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.ref }}
      - env:
          INPUT_REF: ${{ inputs.ref }}
        run: bash scripts/check-dispatch-ref.sh "$INPUT_REF"
YAML
check "the guard job counts as guarded" 0 "$TMP/guardonly" "are constrained by"

# --- the hops the taint can take ---------------------------------------
cat > "$TMP/stephop/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      tag:
        required: true
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - id: ref
        env:
          INPUT_TAG: ${{ inputs.tag }}
        run: echo "ref=${INPUT_TAG}" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ steps.ref.outputs.ref }}
YAML
check "hop through a step output" 1 "$TMP/stephop" "through step 'ref'"

cat > "$TMP/jobhop/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      tag:
        required: true
jobs:
  resolve:
    runs-on: ubuntu-latest
    outputs:
      tag: ${{ steps.t.outputs.tag }}
    steps:
      - id: t
        env:
          INPUT_TAG: ${{ inputs.tag }}
        run: echo "tag=${INPUT_TAG}" >> "$GITHUB_OUTPUT"
  build:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ needs.resolve.outputs.tag }}
YAML
check "hop through a job output" 1 "$TMP/jobhop" "through job 'resolve'"

# A workflow-level `env:` carries the input into a job that never names
# it. Scanning job bodies alone reports "nothing to guard" on this file.
cat > "$TMP/envhop/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
env:
  TARGET_REF: ${{ inputs.ref }}
jobs:
  suite:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ env.TARGET_REF }}
YAML
check "hop through a workflow-level env" 1 "$TMP/envhop" "through env 'TARGET_REF'"

# --- the resolver is the second accepted proof -------------------------
cat > "$TMP/resolver/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      tag:
        required: true
jobs:
  deploy:
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          sparse-checkout: scripts/resolve-dispatch-ref.sh
          sparse-checkout-cone-mode: false
      - id: ref
        env:
          INPUT_TAG: ${{ inputs.tag }}
        run: |
          echo "ref=$(bash scripts/resolve-dispatch-ref.sh "${INPUT_TAG}")" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ steps.ref.outputs.ref }}
YAML
check "the resolver constrains without a gate job" 0 "$TMP/resolver" \
      "are constrained by"

# --- needs: shapes -----------------------------------------------------
cat > "$TMP/shapes/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  dispatch-ref:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/check-dispatch-ref.sh "$INPUT_REF"
  a:
    needs: [dispatch-ref, other]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.ref }}
  other:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  b:
    needs:
      - dispatch-ref
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.ref }}
YAML
check "inline-list and block-list needs: both parse" 0 "$TMP/shapes" \
      "are constrained by"

# --- a cycle in needs: must not hang -----------------------------------
# GitHub rejects this, but the parser has to survive reading it.
cat > "$TMP/cycle/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  a:
    needs: b
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.ref }}
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YAML
check "a cycle in needs: terminates" 1 "$TMP/cycle" "job a"

# --- comments never carry behaviour ------------------------------------
# These workflows explain this rule in prose that names both scripts and
# quotes the unsafe shape.
cat > "$TMP/commented/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  suite:
    runs-on: ubuntu-latest
    steps:
      # A checkout here must never take `ref: ${{ inputs.ref }}` without
      # depending on the job that runs scripts/check-dispatch-ref.sh.
      - run: echo hi
YAML
check "comments are not consumers" 0 "$TMP/commented" \
      "none of them reaches an actions/checkout ref"

# --- a dispatch input that never reaches a checkout --------------------
cat > "$TMP/nosink/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      grace-minutes:
        type: string
jobs:
  reconcile:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - env:
          GRACE: ${{ inputs.grace-minutes }}
        run: bash scripts/check-missing-runs.sh "$GRACE"
YAML
check "an input that never reaches a checkout is not a finding" 0 "$TMP/nosink" \
      "none of them reaches an actions/checkout ref"

# --- refusing to pass having examined nothing --------------------------
check "missing directory exits 2" 2 "$TMP/does-not-exist" "is not a directory"
check "no workflows exits 2" 2 "$TMP/empty" "matched no"

cat > "$TMP/nodispatch/w.yml" <<'YAML'
on:
  push:
    branches: [dev]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - run: go test ./...
YAML
check "no dispatch input anywhere exits 2" 2 "$TMP/nodispatch" \
      "found no workflow_dispatch input in any of them"

# --- the real workflows -------------------------------------------------
check "the real .github/workflows" 0 "$ROOT/.github/workflows" "are constrained by"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
