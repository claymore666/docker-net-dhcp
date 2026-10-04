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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/scripts/check-build-job-independence.sh"
REAL="$ROOT/.github/workflows/release.yml"
guarded_tmpdir TMP

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

# --- THE COMMAND POSITION, the last silent exit ------------------------
#
# "Undecidable -> refuse" was applied to the TARGET position while the
# COMMAND position still answered "none", which removes the job from
# the population -- the same defect as the target position, one slot to
# the left. Both of these hold a `make` token in a form this classifier
# cannot read, and both used to report a clean pass on a genuinely
# serialised three-publisher file.
cat > "$TMP/indirect.yml" <<'YAML'
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
  c:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=z push
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: ${MAKE} PLUGIN_TAG=y push
YAML
check "'\${MAKE} push' is refused, not silently dropped from the population" \
      2 "$TMP/indirect.yml" "not a literal \`make\` invocation"

cat > "$TMP/shwrapped.yml" <<'YAML'
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
  c:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=z push
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: sh -c "make PLUGIN_TAG=y push"
YAML
check "a make wrapped in 'sh -c' is refused, not silently dropped" 2 \
      "$TMP/shwrapped.yml" "not a literal \`make\` invocation"

# ...and the word test must stay bounded, or every mention of a
# Makefile becomes a refusal and the gate gets waived.
cat > "$TMP/makefileword.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    env:
      MAKEFLAGS: -j2
    steps:
      - run: cat Makefile
      - run: make PLUGIN_TAG=x push
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "'Makefile' and 'MAKEFLAGS' are not make invocations" 0 \
      "$TMP/makefileword.yml" "none waiting on another"

# --- the two shapes #798 brought into the subject ---------------------
# A publisher that never calls make is recognised, and a make target
# that names push without being `push` refuses.
cat > "$TMP/nonmakepublisher.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: docker push ghcr.io/x:amd64
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a 'docker push' publisher is in the population (#798)" 1 \
      "$TMP/nonmakepublisher.yml" "b reaches a"

cat > "$TMP/otherpushname.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push-arm64
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "'make push-arm64' is refused, not called a non-publisher (#798)" 2 \
      "$TMP/otherpushname.yml" "cannot tell whether this \`make\` publishes"

# --- ONLY A `run:` VALUE IS SHELL, AND ONLY ITS OWN BODY --------------
#
# The command-position refusal was added reading every line of every
# job, which made prose into a refusal: a step named
# `Make gh-pages available to mike` is real text in pages.yml, and a
# gate that goes red over the name of a step is a gate that gets
# waived before it ever catches the edit it exists for. These four
# cases pin the boundary of what counts as a command.

cat > "$TMP/prosename.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - name: Make gh-pages available to mike
        if: make-believe
        env:
          NOTE: run make push by hand if this fails
        run: make PLUGIN_TAG=x push
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a step name, an if: and an env: value are not commands" 0 \
      "$TMP/prosename.yml" "none waiting on another"

# `echo` and `printf` do not execute their arguments. test.yaml:834 is
# this exact shape -- a gate quoting `make create` in its own failure
# message -- and it was reported as an unreadable make token.
cat > "$TMP/echodata.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo "make create created the bind source (#517)."
      - run: printf '%s\n' "run make push to publish"
      - run: make PLUGIN_TAG=x push
  b:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a make quoted inside echo/printf is data, not a command" 0 \
      "$TMP/echodata.yml" "none waiting on another"

# ...but a command substitution inside those arguments DOES execute,
# so the data rule must not become a way to hide a make.
cat > "$TMP/echosubst.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=x push
  c:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=z push
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: echo "$(make PLUGIN_TAG=y push)"
YAML
check "a make inside a command substitution in echo is still refused" 2 \
      "$TMP/echosubst.yml" "not a literal \`make\` invocation"

# A BLOCK SCALAR MUST END, AND THE STEPS AFTER IT MUST STILL BE READ.
# Restricting the scan to `run:` bodies introduces a second way to go
# quiet: a block that never closes swallows the rest of the job, and
# every publisher after it arrives at the classifier still carrying a
# `run: ` prefix. That is what the real release.yml did while the
# `runind` sentinel was unset -- an unset awk variable is 0, so the
# reader believed it was inside a block from line one.
cat > "$TMP/blockthenstep.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: |
          echo one
          echo two
        env:
          NOTE: x
      - run: make PLUGIN_TAG=x push
  b:
    runs-on: ubuntu-latest
    steps:
      - run: |
          echo three
      - run: make PLUGIN_TAG=y push
YAML
check "a '- run: |' block does not swallow the steps after it" 0 \
      "$TMP/blockthenstep.yml" "none waiting on another"

# ...and the body of that block is still read: a publisher hidden
# inside one must be found, or restricting the scan would have bought
# quiet by going blind.
cat > "$TMP/blockbody.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: |
          echo preparing
          make PLUGIN_TAG=x push
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: |
          make PLUGIN_TAG=y push
YAML
check "a publisher inside a block scalar is still found" 1 \
      "$TMP/blockbody.yml" "waits on"

# A SIBLING KEY AFTER THE BLOCK IS WHERE THE TWO COLUMNS DIFFER.
# A step is a mapping, so `name:`, `env:`, `if:`, `shell:` and
# `working-directory:` sit at the column of `run:` and may be written
# after it. Ending the block at the DASH column instead keeps them
# inside it and hands them to the classifier as shell.
#
# This case exists because the header once claimed the two columns
# were indistinguishable by any legal document. They are not, and a
# paragraph is the wrong thing to have been trusting: mutate
# `ind = index($0, "run:") - 1` to `ind = match($0, /[^[:space:]]/) - 1`
# and this case goes from exit 1 to exit 2, refusing on a step name.
cat > "$TMP/siblingkey.yml" <<'YAML'
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: |
          make PLUGIN_TAG=x push
        name: publish via ${MAKE} on the builder
  b:
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=y push
YAML
check "a sibling key after a block scalar is read as YAML, not shell" 1 \
      "$TMP/siblingkey.yml" "b reaches a"

# On a mixed file two visible publishers keep the count at two, so only
# the classifier can stop a serialised third from passing (#798).
cat > "$TMP/mixedbound.yml" <<'YAML'
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
  release-arm64:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=arm64 push
  release-riscv64:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=riscv64 push-riscv64
YAML
check "a serialised 'make push-riscv64' third publisher is refused (#798)" 2 \
      "$TMP/mixedbound.yml" "cannot tell whether this \`make\` publishes"

# --- #798: every shape on a three-publisher file ----------------------
# Two `make push` builds on `resolve` keep the count at two whatever the
# third job is, so each verdict below comes from the classifier alone.
threepub() {   # threepub <out-file> <yaml of the third job's steps>
    cat > "$1" <<YAML
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
  release-arm64:
    needs: resolve
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=arm64 push
  release-riscv64:
    needs: release
    runs-on: ubuntu-latest
    steps:
$2
YAML
}

# label|want exit|grep|the third job's run: value, serialised on release (#798)
while IFS='|' read -r label want pat cmd; do
    [ -n "$label" ] || continue
    threepub "$TMP/third-$label.yml" "      - run: $cmd"
    if ! grep -qF -- "$cmd" "$TMP/third-$label.yml"; then
        fail_case "$label: the fixture does not carry its command"
        continue
    fi
    check "third publisher '$cmd' -> exit $want" "$want" "$TMP/third-$label.yml" "$pat"
done <<'THIRD'
docker-push|1|release-riscv64 reaches release|docker push ghcr.io/x:riscv64
docker-image-push|1|release-riscv64 reaches release|docker image push ghcr.io/x:riscv64
docker-plugin-push|1|release-riscv64 reaches release|docker plugin push ghcr.io/x:riscv64
docker-compose-push|1|release-riscv64 reaches release|docker compose push
docker-buildx-push|1|release-riscv64 reaches release|docker buildx build --push -t ghcr.io/x:riscv64 .
docker-build-registry|1|release-riscv64 reaches release|docker build --output type=registry -t ghcr.io/x:riscv64 .
docker-bake-pushtrue|1|release-riscv64 reaches release|docker buildx bake --set *.output=type=image,push=true
crane-push|1|release-riscv64 reaches release|crane push img.tar ghcr.io/x:riscv64
crane-mutate|1|release-riscv64 reaches release|crane mutate --label a=b ghcr.io/x:riscv64
oras-push|1|release-riscv64 reaches release|oras push ghcr.io/x:riscv64 f.tar
oras-blob-push|1|release-riscv64 reaches release|oras blob push ghcr.io/x@sha256:0 f.tar
oras-manifest-push|1|release-riscv64 reaches release|oras manifest push ghcr.io/x:riscv64 m.json
cosign-upload|1|release-riscv64 reaches release|cosign upload blob -f f ghcr.io/x:riscv64
docker-by-path|1|release-riscv64 reaches release|/usr/bin/docker push ghcr.io/x:riscv64
docker-in-subst|1|release-riscv64 reaches release|d=$(docker push ghcr.io/x:riscv64)
docker-in-if|1|release-riscv64 reaches release|if docker push ghcr.io/x:riscv64; then echo ok; fi
make-push-other|2|cannot tell whether this `make` publishes|make PLUGIN_TAG=r push-riscv64
make-publish|2|cannot tell whether this `make` publishes|make publish
sudo-docker|2|not at command position|sudo docker push ghcr.io/x:riscv64
sh-c-docker|2|not at command position|sh -c "docker push ghcr.io/x:riscv64"
var-docker|2|named by a variable|"$DOCKER" push ghcr.io/x:riscv64
docker-global-flag|2|a flag before the `docker` verb|docker --config d push ghcr.io/x:riscv64
compose-flag|2|a flag before the `docker compose` verb|docker compose -f c.yml push
docker-verb-var|2|verb that is a variable|docker plugin "$VERB" ghcr.io/x:riscv64
oras-verb-var|2|verb that is a variable|oras manifest "$VERB" ghcr.io/x:riscv64
docker-cli-plugin|2|a `docker` verb this gate has no class for|docker pushrm ghcr.io/x
buildx-unknown|2|a `docker buildx` verb this gate has no class for|docker buildx frobnicate
compose-unknown|2|a `docker compose` verb this gate has no class for|docker compose frobnicate
crane-unknown|2|a `crane` verb this gate has no class for|crane frobnicate ghcr.io/x
oras-unknown|2|a `oras` verb this gate has no class for|oras frobnicate ghcr.io/x
cosign-unknown|2|a `cosign` verb this gate has no class for|cosign frobnicate ghcr.io/x
build-output-var|2|a build output that is a variable|docker buildx build --output "$OUT" .
bake-no-flag|2|a bake whose outputs live in its definition file|docker buildx bake release
podman|2|`podman` is a registry tool with no verb table here|podman push ghcr.io/x:riscv64
skopeo|2|`skopeo` is a registry tool with no verb table here|skopeo copy oci:x docker://ghcr.io/x
regctl|2|`regctl` is a registry tool with no verb table here|regctl image copy a b
oras-cp-layout|1|release-riscv64 reaches release|oras cp --from-oci-layout ./out:riscv64 ghcr.io/x:riscv64
oras-copy-layout-path|1|release-riscv64 reaches release|oras copy --from-oci-layout-path ./out out:riscv64 ghcr.io/x:riscv64
builder-build-push|1|release-riscv64 reaches release|docker builder build --push -t ghcr.io/x:riscv64 .
bytes-inside-ref|1|release-riscv64 reaches release|cosign sign --yes "$(crane push img.tar ghcr.io/x:riscv64)"
after-quoted-pipe|1|release-riscv64 reaches release|echo "a|b"; docker push ghcr.io/x:riscv64
escaped-quotes|1|release-riscv64 reaches release|echo \"; docker push ghcr.io/x:riscv64; echo \"
and-and|1|release-riscv64 reaches release|true&&docker push ghcr.io/x:riscv64
var-cli|2|a command named by a variable|"$CLI" push ghcr.io/x:riscv64
var-braced|2|a command named by a variable|${REGISTRY_TOOL} push ghcr.io/x:riscv64
subst-command|2|a command named by a variable|$(command -v docker) push ghcr.io/x:riscv64
backtick-command|2|a command named by a variable|true && `which docker` push ghcr.io/x:riscv64
build-o-var|2|a build output that is a variable|docker buildx build -o "$OUT" .
sh-c-later-word|2|not at command position|sh -c "set -e; docker push ghcr.io/x:riscv64"
THIRD

threepub "$TMP/third-action.yml" "      - uses: docker/build-push-action@v6
        with:
          push: true"
check "a publishing action outside the table is refused (#798)" 2 \
      "$TMP/third-action.yml" "an action this gate has no class for -> docker/build-push-action"

threepub "$TMP/third-shell.yml" "      - shell: python
        run: print(1)"
check "a non-POSIX shell: is refused, its body is not shell (#798)" 2 \
      "$TMP/third-shell.yml" "a shell whose body this does not read as commands -> python"

threepub "$TMP/third-defaults.yml" "      - run: echo x"
sed -i '1i defaults:\n  run:\n    shell: pwsh' "$TMP/third-defaults.yml"
check "a workflow-level defaults shell: is refused too (#798)" 2 \
      "$TMP/third-defaults.yml" "a shell whose body this does not read as commands -> pwsh"

threepub "$TMP/third-reusable.yml" "      - run: echo x"
cat >> "$TMP/third-reusable.yml" <<'YAML'
  release-reusable:
    needs: release
    uses: org/repo/.github/workflows/build.yml@v1
YAML
check "a job-level reusable workflow is refused, its steps are unread (#798)" 2 \
      "$TMP/third-reusable.yml" "a job-level reusable workflow"

# --- #798: reference writes and reads stay out of the population ------
# A promote job that waits on both builds is the contract (#796), so each
# command here must leave it out: exit 0, and it is named as reference-only
# when it writes one.
promote() {   # promote <out-file> <yaml of the promote job's steps>
    cat > "$1" <<YAML
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=amd64 push
  release-arm64:
    runs-on: ubuntu-latest
    steps:
      - run: make PLUGIN_TAG=arm64 push
  promote:
    needs: [release, release-arm64]
    runs-on: ubuntu-latest
    steps:
$2
YAML
}

# label|reference write (1) or read/local (0)|promote's run: value (#798)
while IFS='|' read -r label refw cmd; do
    [ -n "$label" ] || continue
    promote "$TMP/promote-$label.yml" "      - run: $cmd"
    if [ "$refw" = 1 ]; then
        check "promote job running '$cmd' is a reference write, not a publisher" 0 \
              "$TMP/promote-$label.yml" "free to wait on publishers: promote"
    else
        check "promote job running '$cmd' is not a publisher" 0 \
              "$TMP/promote-$label.yml" "free to wait on publishers: \$"
    fi
done <<'PROMOTE'
crane-tag|1|crane tag "${NAME}:${TAG}" latest
crane-copy|1|crane copy ghcr.io/x:1 docker.io/x:1
imagetools-create|1|docker buildx imagetools create -t ghcr.io/x:latest ghcr.io/x:1 ghcr.io/x:1-arm64
manifest-push|1|docker manifest push ghcr.io/x:latest
oras-cp|1|oras cp ghcr.io/x:1 docker.io/x:1
oras-attach|1|oras attach --artifact-type a ghcr.io/x:1 f
cosign-sign|1|cosign sign --yes "${NAME}@${DIGEST}"
cosign-attest|1|cosign attest --yes --predicate p "${NAME}@${DIGEST}"
crane-manifest-var|0|crane manifest "${NAME}:${TAG}"
oras-manifest-fetch|0|oras manifest fetch "${NAME}:${TAG}"
crane-digest|0|g=$(crane digest "${NAME}:latest" 2>/dev/null) || g="absent"
imagetools-inspect|0|D=$(docker buildx imagetools inspect "${NAME}:${TAG}" --format '{{.Manifest.Digest}}')
plugin-install|0|docker plugin install --grant-all-permissions "$REF"
build-load|0|docker buildx build --load -t x .
go-install-oras|0|go install oras.land/oras/cmd/oras@v1.3.4
string-naming-cosign|0|what="cosign bundle signing \`checksums.txt\`"
echo-docker-push|0|echo "run docker push by hand"
echo-escaped-backtick|0|echo "run \`docker push\` by hand"
quoted-assignment|0|missing="$missing $tool"
pipe-in-echo|0|echo "| \`${f}\` | ${what} |"
oras-cp-to-layout|1|oras cp --to-oci-layout ghcr.io/x:1 ./out:1
PROMOTE

promote "$TMP/promote-action.yml" "      - uses: actions/attest-build-provenance@v4
        with:
          push-to-registry: true"
check "a reference-writing action keeps the promote job out (#798)" 0 \
      "$TMP/promote-action.yml" "free to wait on publishers: promote"

# A job that signs AND uploads is a publisher (#798): a reference write must
# not outrank its bytes, or release/release-arm64 would leave the population.
threepub "$TMP/third-signandpush.yml" "      - run: cosign sign --yes x@sha256:0
      - run: docker push ghcr.io/x:riscv64
      - run: cosign sign --yes y@sha256:0"
check "bytes outrank a reference write in the same job (#798)" 1 \
      "$TMP/third-signandpush.yml" "release-riscv64 reaches release"

# A string opened on the line above leaves this line's quotes unbalanced;
# it is split as if unquoted, so the command after the quote is read (#798).
threepub "$TMP/third-openquote.yml" '      - run: |
          echo "start
            done"; docker push ghcr.io/x:riscv64'
check "a command after a string closed from the line above is read (#798)" 1 \
      "$TMP/third-openquote.yml" "release-riscv64 reaches release"

# One line closes a string and opens the next; the push between is a command.
threepub "$TMP/third-closeopen.yml" '      - run: |
          echo "start
          end" ; docker push ghcr.io/x:riscv64 ; echo "more
          tail"'
check "a command between two multi-line strings is read (#798)" 1 \
      "$TMP/third-closeopen.yml" "release-riscv64 reaches release"

# The quote state is per `run:`: an unclosed quote does not swallow the next.
threepub "$TMP/third-runreset.yml" '      - run: echo "unclosed
      - run: echo x; docker push ghcr.io/x:riscv64'
check "each run: starts outside a string (#798)" 1 \
      "$TMP/third-runreset.yml" "release-riscv64 reaches release"

# --- #798: release.yml as it ships keeps its downstream contract ------
# promote-latest re-tags with `crane tag` and github-release cuts the
# release; both name both builds in needs:, and the file must stay exit 0.
if [ -f "$REAL" ]; then
    for j in promote-latest github-release; do
        n=$((n + 1))
        line="$(awk -v j="  $j:" '$0 == j { f = 1; next } f && /^    needs:/ { print; exit }' "$REAL")"
        case "$line" in
            *release,*release-arm64*) echo "PASS: $j needs both builds in release.yml" ;;
            *) echo "FAIL: $j does not need both builds in release.yml: '$line'"
               failures=$((failures + 1)) ;;
        esac
    done
    check "release.yml as it ships: both builds publish, promote-latest only re-tags" 0 \
          "$REAL" "free to wait on publishers: promote-latest"
    check "release.yml as it ships: the population is exactly the two builds" 0 \
          "$REAL" "none waiting on another: release release-arm64\$"
fi

# --- refusal, not a verdict, on nothing to read -----------------------
check "a missing file is exit 2" 2 "$TMP/nope.yml" "not a readable file"

echo
if [ "$failures" -eq 0 ]; then
    echo "check-build-job-independence: $n/$n cases passed"
else
    echo "check-build-job-independence: $failures of $n cases FAILED"
    exit 1
fi
