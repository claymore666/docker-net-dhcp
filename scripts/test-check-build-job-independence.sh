#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Tests for check-build-job-independence.sh (#796).
#
# ONE CASE PER ACCEPTED SPELLING, EACH DERIVED FROM THE REAL FILE.
# The first version of this suite proved the gate's transitive closure,
# its vacuity refusals and its live positive — and every single case
# was written in flow form. So the depth it demonstrated rode on a
# spelling it never varied, and the gate underneath it reported OK on
#
#     needs:
#       - resolve
#       - release
#
# which is genuinely serialised and is what a person writes the moment
# a job gains a second dependency — the exact edit that reintroduces
# the bug. A frozen operand hides its own mutant.
#
# The serialised cases below therefore re-serialise the REAL
# release.yml, once per spelling the gate claims to accept, each guarded
# so a substitution that stops applying fails loudly instead of passing
# having reconstructed nothing.
#
# THE REFUSALS CARRY EQUAL WEIGHT. Two ways this gate can report a
# clean pass while knowing nothing: a `needs:` form it cannot parse,
# and a file whose publishing-job set is too small for the rule to say
# anything. Both are exit 2, and both are tested — including the
# multi-line flow sequence the parser deliberately does NOT handle,
# which must refuse rather than fall through to OK.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/scripts/check-build-job-independence.sh"
REAL="$ROOT/.github/workflows/release.yml"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
n=0

# check NAME WANT_EXIT FILE GREP_PATTERN
check() {
    local name="$1" want_exit="$2" file="$3" want_grep="$4"
    n=$((n + 1))
    bash "$CHECK" "$file" > "$TMP/out" 2>&1
    local got=$? ok=1
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

fail_case() {
    n=$((n + 1))
    echo "FAIL: $1"
    failures=$((failures + 1))
}

# --- THE LIVE POSITIVES: every accepted spelling of a serialised graph -
#
# `awk` rewrites the arm64 job's own `needs:` -- REPLACING it, never
# prepending a second one. A duplicate key is not the pre-#796 shape,
# it is invalid YAML, and the first attempt at this suite wrote one,
# whereupon the gate read the surviving `needs: resolve` and correctly
# reported no collision.
# The arm64 job's own `needs:` region: everything between its header
# and its `runs-on:`, HEADER EXCLUDED. The exclusion is the whole point
# and is asserted below -- `  release-arm64:` contains the string
# `release`, so a region that keeps it makes the guard match
# unconditionally and gives it exactly one possible verdict.
needs_region() {
    awk '/^  release-arm64:[ \t]*$/ { inarm = 1; next }
         inarm && /^    runs-on:/    { exit }
         inarm { print }' "$1"
}

reserialise() {   # reserialise <out-file> <replacement-block>
    awk -v repl="$2" '
      /^  release-arm64:[ \t]*$/ { inarm = 1; print; next }
      inarm && /^    needs:/     { printf "%s\n", repl; inarm = 0; next }
      { print }
    ' "$REAL" > "$1"
}

if [ ! -f "$REAL" ]; then
    fail_case "release.yml is missing — every live-positive case tests nothing"
else
    # name|the text that replaces the arm64 job's `needs:` line
    while IFS='|' read -r label repl; do
        [ -n "$label" ] || continue
        out="$TMP/ser-$label.yml"
        reserialise "$out" "$repl"
        if cmp -s "$REAL" "$out"; then
            fail_case "release.yml has no 'release-arm64:' job with a needs: —" \
                      "the '$label' spelling reconstructs nothing (#796)"
            continue
        fi
        # Guard the MUTATION, not the file. `grep release` over the
        # whole workflow matches the amd64 job's own name and would
        # pass on a fixture that changed nothing relevant; the region
        # between the arm64 job header and its `runs-on:` is the only
        # place the new dependency can be.
        #
        # `next` ON THE HEADER RULE IS LOAD-BEARING. Without it the
        # header line `  release-arm64:` falls through to the printing
        # rule and becomes the region's first line -- and it contains
        # the string `release`, so the guard below matched
        # unconditionally and had exactly one possible verdict. The
        # paragraph above had the right reasoning and stopped one line
        # short of its own target.
        region="$(needs_region "$out")"
        if ! printf '%s\n' "$region" | grep 'release' >/dev/null; then
            fail_case "$label: the arm64 job's needs: does not name 'release'" \
                      "— this case reconstructs nothing (#796)"
            continue
        fi
        if printf '%s\n' "$region" | grep '\\n' >/dev/null; then
            fail_case "$label: the replacement landed as a literal backslash-n," \
                      "so the fixture is one unparsed line and not this spelling"
            continue
        fi
        check "serialised, spelled '$label', is caught" 1 "$out" \
              "release-arm64 reaches release"
    done <<'SPELLINGS'
flow-scalar|    needs: release
flow-seq|    needs: [resolve, release]
flow-seq-dquoted|    needs: [resolve, "release"]
flow-seq-squoted|    needs: ['resolve', 'release']
flow-seq-trailing-comment|    needs: [resolve, release]  # arm64 waits
flow-seq-trailing-comma|    needs: [resolve, release,]
block-seq|    needs:\n      - resolve\n      - release
block-seq-quoted|    needs:\n      - resolve\n      - "release"
block-seq-comment-inside|    needs:\n      # why we wait\n      - resolve\n      - release
SPELLINGS

    # THE GUARD'S OWN GUARD. On the real file the arm64 job depends on
    # `resolve`, so its needs: region must NOT mention `release`. If it
    # does, the region is carrying the job header and every spelling
    # case above is guarded by a check with one possible verdict.
    n=$((n + 1))
    if needs_region "$REAL" | grep 'release' >/dev/null; then
        echo "FAIL: the needs: region includes the job header, so the" \
             "spelling guards can never fail"
        needs_region "$REAL" | sed 's/^/    /'
        failures=$((failures + 1))
    else
        echo "PASS: the needs: region excludes the job header (the guards can fail)"
    fi

    check "the real release.yml passes" 0 "$REAL" "none waiting on another"

    # A form the parser does NOT handle must refuse, not fall through
    # to OK. This is the whole point of the exit-2 path: an unknown
    # spelling is an unknown meaning.
    reserialise "$TMP/multiline.yml" '    needs: [\n      resolve,\n      release ]'
    check "a multi-line flow sequence is refused, not silently OK" 2 \
          "$TMP/multiline.yml" "unparseable needs: value"
fi

# --- TRANSITIVE, not merely direct ------------------------------------
# b -> middle -> a. A check that only read each job's own `needs:` list
# calls this clean, and it serialises exactly as badly.
cat > "$TMP/transitive.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push
  middle:
    needs:
      - a
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  b:
    needs: [middle]
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a publishing job reaching another THROUGH a third job is caught" \
      1 "$TMP/transitive.yml" "b reaches a"

# --- THE TRUE NEGATIVE: a gate that flags working files gets waived ---
cat > "$TMP/parallel.yml" <<'YAML'
jobs:
  resolve:
    runs-on: ubuntu-latest
    steps:
      - run: echo tag
  a:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push
  b:
    needs:
      - resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
  after:
    needs: [a, b]
    runs-on: ubuntu-latest
    steps:
      - run: echo done
YAML
check "two publishers on a common prerequisite are not a collision" \
      0 "$TMP/parallel.yml" "none waiting on another"

# An empty flow sequence is a real YAML value meaning no dependencies.
# Reading it as unparseable would refuse a verdict on a valid file.
cat > "$TMP/emptyneeds.yml" <<'YAML'
jobs:
  a:
    needs: []
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "'needs: []' means no dependencies, not an unknown form" 0 \
      "$TMP/emptyneeds.yml" "none waiting on another"

# --- REFUSALS: a form whose meaning is unknown ------------------------
cat > "$TMP/garbage.yml" <<'YAML'
jobs:
  a:
    needs: {job: release}
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a mapping where a name or list belongs is refused" 2 "$TMP/garbage.yml" \
      "unparseable needs: value"

# --- VACUITY, both ways round -----------------------------------------
cat > "$TMP/one.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: echo not a publisher
YAML
check "one publishing job is exit 2, not a green pass" 2 "$TMP/one.yml" \
      "needs at least two"

cat > "$TMP/none.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
YAML
check "no publishing job at all is exit 2, not a green pass" 2 "$TMP/none.yml" \
      "needs at least two"

# --- a COMMENT naming the step does not make a job a publisher --------
# Getting this wrong turns the vacuity guard off: the file would look
# like it holds publishers it does not, and the rule would pass over
# jobs that never push anything.
cat > "$TMP/commented.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      # this used to run make PLUGIN_TAG=x push
      - run: echo nothing
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      # and so did this: make PLUGIN_TAG=y push
      - run: echo nothing
YAML
check "a commented-out 'make ... push' is not a publishing job" 2 \
      "$TMP/commented.yml" "needs at least two"

# --- prose that quotes `needs: release` is prose ----------------------
# release.yml documents this very rule and names the shape it forbids.
# A parser that read commented `needs:` lines would fire on the file's
# own explanation of itself.
cat > "$TMP/prose.yml" <<'YAML'
jobs:
  resolve:
    runs-on: ubuntu-latest
    steps:
      - run: echo tag
  a:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push
  b:
    # This job must NOT carry `needs: a` -- see #796. It used to read
    #     needs:
    #       - a
    # and that is the serialised shape this gate refuses.
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a commented-out 'needs:' is documentation, not a dependency" 0 \
      "$TMP/prose.yml" "none waiting on another"

# --- THE PUBLISHER DETECTOR, which used to answer "no" when it meant
# --- "I could not tell" ------------------------------------------------
#
# A missed publisher does not merely go unchecked: it LEAVES THE
# POPULATION, vanishing from the serialisation check and from the count
# the non-vacuity refusal is computed against, in one stroke. The
# defence that npub < 2 backstops the detector is unsound because the
# refusal's own domain comes from the detector -- and on a
# two-publisher file it happens to look sound, which is why the
# three-publisher case below is the one that matters. #796's whole
# premise is that a third architecture is plausible.
cat > "$TMP/threearch.yml" <<'YAML'
jobs:
  resolve:
    runs-on: ubuntu-latest
    steps:
      - run: echo tag
  release:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=amd64 push
  release-riscv64:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=riscv push
  release-arm64:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: make push PLUGIN_TAG=arm64
YAML
check "a third arch cannot hide a serialised one behind a spelling" 1 \
      "$TMP/threearch.yml" "release-arm64 reaches release"

# Argument order is not meaning. `make push VAR=x` and `make VAR=x push`
# are the same command; the regex this replaced saw only the second.
cat > "$TMP/argorder.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make push PLUGIN_NAME=x PLUGIN_TAG=y
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=z push
YAML
check "'make push VAR=x' is a publishing job whatever the argument order" \
      1 "$TMP/argorder.yml" "b reaches a"

# A continuation is one command. A line-oriented reader sees neither half.
cat > "$TMP/continuation.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: |
          make PLUGIN_NAME=x \
            PLUGIN_TAG=y \
            push
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=z push
YAML
check "a 'make ... push' split over continuations is one invocation" 1 \
      "$TMP/continuation.yml" "b reaches a"

# "I could not tell" must be exit 2, never "not a publisher". A target
# that is a variable expansion, and a make with no target at all (the
# answer lives in the Makefile's default goal), are both undecidable.
cat > "$TMP/varTarget.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x "${MAKE_TARGET}"
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a make whose target is a variable is refused, not called a non-publisher" \
      2 "$TMP/varTarget.yml" "cannot tell whether this \`make\` publishes"

cat > "$TMP/defaultgoal.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a bare 'make' is refused: the default goal lives in the Makefile" \
      2 "$TMP/defaultgoal.yml" "cannot tell whether this \`make\` publishes"

# ...and the true negative for the same code path: a make that plainly
# does NOT publish must stay a non-publisher, or every workflow that
# builds without pushing starts refusing.
cat > "$TMP/othertarget.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make plugin
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "'make plugin' is decidably not a publisher, not a refusal" 2 \
      "$TMP/othertarget.yml" "needs at least two"

# ...including when its ASSIGNMENTS carry variable expansions. Treating
# `VAR="${X}"` as a target makes it look undecidable, and the gate would
# refuse on every workflow that builds without pushing -- a gate that
# cries wolf gets waived, which is the same end as no gate.
cat > "$TMP/varassign.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_NAME="${GHCR_NAME}" PLUGIN_TAG="${TAG}" plugin
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a non-publishing make with variable ASSIGNMENTS is still decidable" 2 \
      "$TMP/varassign.yml" "needs at least two"

# --- refusal, not a verdict, on nothing to read -----------------------
check "a missing file is exit 2" 2 "$TMP/nope.yml" "not a readable file"

echo
if [ "$failures" -eq 0 ]; then
    echo "check-build-job-independence: $n/$n cases passed"
else
    echo "check-build-job-independence: $failures of $n cases FAILED"
    exit 1
fi
