#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-dispatch-ref-guard.sh (#593), on fixture workflow
# directories plus one assertion against the real .github/workflows.
#
# The cases that carry the weight:
#
#   - A CONSUMER WITH NO `needs:` AT ALL. This is the shape of the very
#     first job in integration.yml, and an early version of the checker
#     silently passed it: the scanner emitted an empty needs field, tab
#     is an IFS whitespace character, bash `read` collapsed the run of
#     tabs, and every later field shifted left — so the job read as
#     "does not consume inputs.ref". The check reported two findings
#     where there were three and looked like it was working.
#   - TRANSITIVE reach, because that is how the protection actually
#     works: a failed guard skips everything downstream of it, however
#     many hops away.
#   - THE REAL WORKFLOWS. A checker that only ever sees its own
#     fixtures proves nothing about this repository.
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
         "$TMP/commented" "$TMP/none" "$TMP/shapes" "$TMP/cycle" "$TMP/empty"

# --- a consumer with no needs: at all ---------------------------------
cat > "$TMP/unguarded/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
YAML
check "a consumer with no needs: is reported" 1 "$TMP/unguarded" "job gate"

# --- direct dependency on the guard -----------------------------------
cat > "$TMP/direct/w.yml" <<'YAML'
jobs:
  dispatch-ref:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/check-dispatch-ref.sh "${{ inputs.ref }}"
  suite:
    needs: dispatch-ref
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
YAML
check "a consumer needing the guard directly passes" 0 "$TMP/direct" "are behind"

# --- transitive dependency --------------------------------------------
cat > "$TMP/transitive/w.yml" <<'YAML'
jobs:
  dispatch-ref:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/check-dispatch-ref.sh "${{ inputs.ref }}"
  gate:
    needs: dispatch-ref
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
  suite:
    needs: [gate]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.ref }}
YAML
check "a consumer two hops from the guard passes" 0 "$TMP/transitive" "are behind"

# --- the guard job itself is not a finding ----------------------------
cat > "$TMP/guardonly/w.yml" <<'YAML'
jobs:
  dispatch-ref:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/check-dispatch-ref.sh "${{ inputs.ref }}"
YAML
check "the guard job does not report itself" 0 "$TMP/guardonly" "are behind"

# --- a comment mentioning inputs.ref is not a consumer ----------------
cat > "$TMP/commented/w.yml" <<'YAML'
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      # Deliberately does NOT pass inputs.ref to checkout — see #593.
      - uses: actions/checkout@v7
YAML
check "a comment naming inputs.ref is not a consumer" 0 "$TMP/commented" "nothing to guard"

# --- no consumers at all ----------------------------------------------
cat > "$TMP/none/w.yml" <<'YAML'
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
YAML
check "a workflow with no dispatch ref passes" 0 "$TMP/none" "nothing to guard"

# --- all three needs: shapes parse ------------------------------------
cat > "$TMP/shapes/w.yml" <<'YAML'
jobs:
  dispatch-ref:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/check-dispatch-ref.sh "${{ inputs.ref }}"
  a:
    needs: dispatch-ref
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.ref }}
  b:
    needs: [dispatch-ref, a]
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.ref }}
  c:
    needs:
      - dispatch-ref
      - b
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.ref }}
YAML
check "scalar, inline-array and block needs: all parse" 0 "$TMP/shapes" "4 job(s)"

# --- a cycle must terminate, not hang ---------------------------------
# GitHub rejects this, but a checker that hangs on malformed input is
# worse than one that reports it.
cat > "$TMP/cycle/w.yml" <<'YAML'
jobs:
  a:
    needs: b
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ inputs.ref }}
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
YAML
check "a needs: cycle terminates and is reported" 1 "$TMP/cycle" "job a"

# --- blindness guards --------------------------------------------------
check "a directory with no workflows exits 2" 2 "$TMP/empty" "No workflows found"
check "a missing directory exits 2" 2 "$TMP/nope" "Workflow directory missing"

# --- the real workflows ------------------------------------------------
# The assertion that this is actually adopted rather than merely
# implemented. If a future job takes inputs.ref without the guard, this
# is the line that goes red.
check "this repository's own workflows are guarded" 0 "$ROOT/.github/workflows" ""

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n check(s) FAILED"
    exit 1
fi
echo "all $n check(s) passed"
