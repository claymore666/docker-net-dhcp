#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-run-expansions.sh (#737), on fixture workflow
# directories plus one assertion against the real .github/workflows.
#
# THE CASES THAT CARRY THE WEIGHT:
#
#   - `prefix` reproduces the four shapes that were actually in the tree:
#     `inputs.tag` expanded in the signing job, `inputs.ref` expanded in
#     the job that exists to VALIDATE it, `inputs.grace-minutes` with a
#     `||` default, and a secret piped into `docker login`. Run against
#     the real pre-fix workflows it reported all eight sites #737 names,
#     at the lines the issue cites; the fixture keeps that provable
#     without depending on git history.
#   - `safe` is the same values routed through `env:`. If this failed,
#     the check would be demanding the impossible and would be turned
#     off within a week.
#   - `enum` holds `github.event_name` and
#     `github.event.pull_request.number` in a run body. These must NOT
#     be findings: they cannot carry shell syntax, and a check that
#     flagged them would generate churn that trains people to ignore it.
#   - `commented` puts the unsafe form in a comment. This workflow tree
#     documents this very rule by quoting the bad shape, so a checker
#     that read comments could never go green.
#
# MUTANT COVERAGE: cases expecting exit 0, 1 and 2 are all present, so
# neither an always-exit-0 nor an always-exit-1 checker survives.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-run-expansions.sh"
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

mkdir -p "$TMP/prefix" "$TMP/safe" "$TMP/enum" "$TMP/commented" "$TMP/empty" \
         "$TMP/norun"

# --- the shapes that were actually in the tree ------------------------
cat > "$TMP/prefix/release.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      tag:
        required: true
jobs:
  release:
    permissions:
      id-token: write
    steps:
      - name: Resolve release tag
        run: |
          if [ -n "${{ inputs.tag }}" ]; then
            TAG="${{ inputs.tag }}"
          fi
      - name: Install released plugin
        run: |
          echo "${{ secrets.GITHUB_TOKEN }}" | docker login ghcr.io --password-stdin
YAML
cat > "$TMP/prefix/integration.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      ref:
        type: string
jobs:
  dispatch-ref:
    steps:
      - name: Reject a dispatch ref that is not ours
        run: bash scripts/check-dispatch-ref.sh "${{ inputs.ref }}"
YAML
cat > "$TMP/prefix/missing-runs.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      grace-minutes:
        type: string
jobs:
  reconcile:
    steps:
      - run: bash scripts/check-missing-runs.sh "${{ inputs.grace-minutes || '20' }}"
YAML
check "pre-#737: input expanded in the signing job" 1 "$TMP/prefix" \
      "release.yml:13"
check "pre-#737: the guard job expands its own input" 1 "$TMP/prefix" \
      "integration.yml:10"
check "pre-#737: an input with a || default" 1 "$TMP/prefix" \
      "missing-runs.yml:9"
check "pre-#737: a secret written into the step script" 1 "$TMP/prefix" \
      "Secret expanded into a shell"

# --- the same values, passed as data ----------------------------------
cat > "$TMP/safe/release.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      tag:
        required: true
jobs:
  release:
    steps:
      - name: Resolve release tag
        env:
          INPUT_TAG: ${{ inputs.tag }}
        run: |
          TAG="${INPUT_TAG:-$GITHUB_REF_NAME}"
          [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$ ]] || exit 1
      - name: Log in to GHCR
        uses: docker/login-action@dbcb813823bdd20940b903addbd779551569679f # v4
        with:
          registry: ghcr.io
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Use it
        run: docker pull "ghcr.io/x/y:${TAG}"
YAML
check "safe: value through env, secret through the action" 0 "$TMP/safe" \
      "none carrying an untrusted value or a secret"

# --- values that cannot carry shell syntax ----------------------------
cat > "$TMP/enum/integration.yml" <<'YAML'
on:
  pull_request:
jobs:
  gate:
    steps:
      - run: |
          case "${{ github.event_name }}" in
            pull_request)
              d=$(bash scripts/gate.sh pr "${{ github.event.pull_request.number }}")
              ;;
          esac
YAML
check "enums and integers are not findings" 0 "$TMP/enum" \
      "none carrying an untrusted value"

# --- prose that quotes the unsafe form --------------------------------
cat > "$TMP/commented/w.yml" <<'YAML'
on:
  workflow_dispatch:
    inputs:
      tag:
        required: true
jobs:
  release:
    steps:
      - name: Resolve release tag
        env:
          INPUT_TAG: ${{ inputs.tag }}
        run: |
          # This used to read TAG="${{ inputs.tag }}", which the shell
          # parsed before the check below ever ran. Do not reintroduce
          # it, and do not pipe "${{ secrets.GITHUB_TOKEN }}" anywhere.
          TAG="${INPUT_TAG}"
YAML
check "comments are not code" 0 "$TMP/commented" "none carrying an untrusted value"

# --- refusing to pass having examined nothing -------------------------
check "missing directory exits 2" 2 "$TMP/does-not-exist" "is not a directory"
check "no workflows exits 2" 2 "$TMP/empty" "matched no"

cat > "$TMP/norun/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ inputs.ref }}
YAML
# The sentinel counts run bodies PARSED, not expansions found in them.
# Keyed on expansions this rejected `safe` and `commented` — workflows
# that were simply correct — which is the check refusing to believe a
# clean tree. A directory with no `run:` step at all is the real
# "examined nothing" case.
check "no run: step at all exits 2" 2 "$TMP/norun" \
      "found no 'run:' step at all"

# --- a clean tree is not an unexamined one ----------------------------
# The regression the sentinel above caused: `safe` and `commented` both
# contain run bodies with no untrusted expansion, and an
# expansion-keyed sentinel called that "the parser is broken". Asserting
# the count explicitly keeps the two conditions apart.
check "a clean tree reports what it read" 0 "$TMP/safe" "run body/bodies across"

# --- a file it could not read is a REFUSAL, not an absence (#839) ------
# `examined` counted the files this check VISITED, and `run_bodies` is
# an AGGREGATE over the whole corpus, so neither number moves when ONE
# workflow among two dozen readable ones cannot be opened: awk says
# nothing, the loop reads nothing, and the findings in that file are
# gone. Measured 2026-08-28 with a real script-injection finding planted
# in a `run:` body -- `${{ github.event.pull_request.title }}`, control
# rc=1 -- and the same file at mode 000: rc=0 under mawk 1.3.4 AND under
# gawk 5.4.1. This gate guards the one workflow holding
# `id-token: write`, so a finding it drops in silence is the expensive
# kind.
#
# The fixture is a DIRECTORY named *.yml, not a file at mode 000,
# because mode 000 is readable as root: a case that quietly stops
# testing on a root runner reports exactly what a working case reports.
# The corpus around it is `safe`, which HAS run bodies -- so the
# aggregate "no run bodies parsed" sentinel is satisfied and cannot be
# what produces the refusal. Without that, this case would pass for the
# wrong reason.
mkdir -p "$TMP/unreadable"
cp "$TMP/safe"/*.yml "$TMP/unreadable/"
mkdir "$TMP/unreadable/isadir.yml"
check "a workflow that cannot be read is a refusal, not an absence" 2 \
      "$TMP/unreadable" "Cannot read workflow"

# Preservation for the guard above: the same corpus WITHOUT the
# unreadable member must still be clean. A guard that answered
# "cannot read" to everything would satisfy the case above by refusing
# workflows it can read perfectly well.
rmdir "$TMP/unreadable/isadir.yml"
check "the same corpus without the unreadable member is still clean" 0 \
      "$TMP/unreadable" "run body/bodies across"

# --- the OTHER half of the same refusal: awk failed while printing ----
# `-f` and `-r` are asked in the shell first, so no workflow fixture can
# hand awk a file awk cannot open -- which leaves the status half a
# branch no case can reach. That is the same defect one level down, and
# it is why RUN_EXPANSIONS_AWK is a declared seam. The stub prints a
# plausible PARTIAL scan and then fails: partial output is not empty
# output, and the aggregate `run_bodies` sentinel below cannot tell a
# short read from a short file.
seam_check() { # NAME WANT_EXIT DIR STUB GREP
    local name="$1" want="$2" dir="$3" stub="$4" want_grep="$5"
    n=$((n + 1))
    RUN_EXPANSIONS_AWK="$stub" bash "$CHECK" "$dir" > "$TMP/out" 2>&1
    local got=$? ok=1
    [ "$got" -eq "$want" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q -- "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then echo "PASS: $name"; else
        echo "FAIL: $name (want exit $want/grep '$want_grep', got exit $got)"
        sed 's/^/    /' "$TMP/out"; failures=$((failures + 1))
    fi
}

STUB="$TMP/awkstub"
cat > "$STUB" <<'STUBEOF'
#!/bin/sh
printf '0\t!run\n12\tgithub.event_name\n'
exit 2
STUBEOF
chmod +x "$STUB"
seam_check "an awk that prints a partial scan and then fails is a refusal" 2 \
      "$TMP/safe" "$STUB" "Cannot read workflow"

# Preservation for the seam itself: the SAME output with status 0 must
# be believed, or the case above would pass because the seam is broken
# rather than because the status is read. A case that cannot tell its
# subject from its harness is not evidence.
cat > "$STUB" <<'STUBEOF'
#!/bin/sh
printf '0\t!run\n12\tgithub.event_name\n'
STUBEOF
chmod +x "$STUB"
seam_check "the same partial output with status 0 is believed, not refused" 0 \
      "$TMP/safe" "$STUB" "run body/bodies across"

# --- the real workflows -----------------------------------------------
check "the real .github/workflows" 0 "$ROOT/.github/workflows" \
      "none carrying an untrusted value or a secret"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
