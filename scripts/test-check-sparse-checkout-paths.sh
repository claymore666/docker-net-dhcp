#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-sparse-checkout-paths.sh (#736), on fixture workflow
# directories plus two fixtures RECONSTRUCTED from the repository's own
# pages.yml and release.yml.
#
# THE RECONSTRUCTED PAIR IS THE POINT OF THIS SUITE. The bug it is
# written for actually shipped, on dev @ 9245f09, in both of those
# files. Rather than transcribe the pre-fix text — which would drift on
# the first unrelated edit and go on passing while testing nothing — the
# suite takes the real files as they stand today and mutates them back
# into the pre-fix shape. That shape is derived rather than copied, and
# it stays exact for as long as the files exist. Every mutation carries
# a guard that fails loudly when it stops applying, so the day someone
# restructures those checkouts this suite says so instead of quietly
# proving nothing.
#
# pages.yml needs one mutation: delete `path: .resolver`. release.yml
# has needed two since #796 split its resolver checkout into a job of
# its own, where a deleted `path:` collides with nothing. The day that
# happened, the one-line guard did NOT fire — the line it watches was
# still present and still deleted — so nothing announced that the
# reconstruction had stopped reconstructing. What failed was the
# assertion itself: the gate returned 0 where the case wanted 1, because
# the mutated file no longer contained a collision to find. That is the
# case working, but it is luck about which half broke; had the shape
# survived under a different job name the grep would have caught it
# instead, and had both held it would have passed over nothing. A guard
# belongs on each mutation, asserting that mutation applied, which is
# what they now do.
#
# THE OTHER CASES THAT CARRY THEIR WEIGHT:
#
#   - THE MIRROR IMAGE: full checkout first, sparse second, same path.
#     Same cause, different symptom — the workspace is left sparse for
#     every step after it. A rule phrased as "a sparse checkout followed
#     by a full one" passes this, which is why the rule is phrased
#     without a direction.
#   - SEPARATION. release.yml's pair is seventy lines and five unrelated
#     steps apart, so a check that looked at neighbouring steps would
#     call the more dangerous of the two instances clean.
#   - PATH SPELLING. `.`, `./`, absent, `sub/` and `"sub"` are the same
#     directory written five ways. If normalisation is dropped the gate
#     reports clean on a real collision.
#   - DIFFERENT JOBS. Steps in different jobs run on different runners
#     with different workspaces, so the same path in two jobs is not a
#     collision and must not be reported — a gate that flags working
#     workflows gets waived.
#   - NO SPARSE CHECKOUT INVOLVED. Two full checkouts at one path are
#     somebody else's problem, not this rule's.
#   - COMMENTS, BOTH WAYS ROUND. A commented-out `sparse-checkout:` is
#     not a sparse checkout, and — the direction that matters more — a
#     commented-out `path:` must NOT answer for a real one, or the
#     prose explaining this rule would switch it off.
#   - EXIT 2 WHEN NOTHING WAS DISCOVERED. A workflow directory in which
#     the parser recognises no checkout at all is a broken parser, not a
#     clean tree.
#   - THE REAL WORKFLOWS. A checker that only ever sees its own fixtures
#     proves nothing about this repository.
#
# MUTANT COVERAGE: cases expecting exit 0, 1 and 2 are all present, so
# none of always-exit-0, always-exit-1 and always-exit-2 survives.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-sparse-checkout-paths.sh"
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

fail_case() {
    n=$((n + 1))
    echo "FAIL: $1"
    failures=$((failures + 1))
}

mkdir -p "$TMP/collide" "$TMP/mirror" "$TMP/separated" "$TMP/dotslash" \
         "$TMP/trailing" "$TMP/quoted" "$TMP/fixed" "$TMP/twojobs" \
         "$TMP/nosparse" "$TMP/commented-sparse" "$TMP/commented-path" \
         "$TMP/lone" "$TMP/nocheckouts" "$TMP/empty" \
         "$TMP/real-pages" "$TMP/real-release"

# --- the shipped shape: sparse first, full second, default path -------
cat > "$TMP/collide/w.yml" <<'YAML'
on:
  push:
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          sparse-checkout: scripts/resolve-dispatch-ref.sh
          sparse-checkout-cone-mode: false
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: refs/heads/dev
          fetch-depth: 0
      - run: pip install -r docs/requirements.txt
YAML
check "a sparse checkout and a full one share the default path" 1 "$TMP/collide" \
      "job deploy"

# --- the mirror image, which a direction-carrying rule would pass ------
cat > "$TMP/mirror/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          sparse-checkout: scripts/one.sh
          sparse-checkout-cone-mode: false
      - run: make check
YAML
check "MIRROR: full first, sparse second, same path is still a collision" 1 \
      "$TMP/mirror" "job build"

# --- the pair separated by unrelated steps (release.yml's shape) ------
cat > "$TMP/separated/w.yml" <<'YAML'
on:
  push:
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          sparse-checkout: scripts/resolve-dispatch-ref.sh
          sparse-checkout-cone-mode: false
      - name: Resolve ref
        id: ref
        run: echo "ref=refs/heads/dev" >> "$GITHUB_OUTPUT"
      - name: Set up Go
        uses: actions/setup-go@v6
      - name: Log in to GHCR
        run: echo logging in
      - name: Log in to Docker Hub
        run: echo logging in
      - name: Install crane
        run: echo installing
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: ${{ steps.ref.outputs.ref }}
          fetch-depth: 0
      - run: make push
YAML
check "the pair is found across five unrelated steps (no adjacency)" 1 \
      "$TMP/separated" "job release"

# --- path spelling: `./` and absent are the same directory ------------
cat > "$TMP/dotslash/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          path: ./
          sparse-checkout: scripts/one.sh
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
YAML
check "'path: ./' and no path at all are the same directory" 1 "$TMP/dotslash" \
      "path \."

# --- path spelling: a trailing slash is not a different directory -----
cat > "$TMP/trailing/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          path: helper/
          sparse-checkout: scripts/one.sh
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          path: helper
YAML
check "'helper/' and 'helper' are the same directory" 1 "$TMP/trailing" \
      "path helper"

# --- path spelling: quoting is not a different directory --------------
cat > "$TMP/quoted/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          path: "helper"
          sparse-checkout: scripts/one.sh
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          path: helper
YAML
check "a quoted path is the same directory as an unquoted one" 1 "$TMP/quoted" \
      "path helper"

# --- THE FIX passes ---------------------------------------------------
cat > "$TMP/fixed/w.yml" <<'YAML'
on:
  push:
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          path: .resolver
          sparse-checkout: scripts/resolve-dispatch-ref.sh
          sparse-checkout-cone-mode: false
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: refs/heads/dev
          fetch-depth: 0
      - run: pip install -r docs/requirements.txt
YAML
check "the fix — a separate path — passes" 0 "$TMP/fixed" "no sparse checkout shares"

# --- different jobs cannot collide ------------------------------------
cat > "$TMP/twojobs/w.yml" <<'YAML'
on:
  push:
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          sparse-checkout: scripts/one.sh
  b:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          fetch-depth: 0
YAML
check "the same path in two different jobs is not a collision" 0 "$TMP/twojobs" \
      "no sparse checkout shares"

# --- two full checkouts are not this rule's business ------------------
cat > "$TMP/nosparse/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: refs/heads/dev
YAML
check "two full checkouts at one path are not reported" 0 "$TMP/nosparse" \
      "no sparse checkout shares"

# --- a commented-out sparse-checkout is not a sparse checkout ---------
cat > "$TMP/commented-sparse/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          # sparse-checkout: scripts/one.sh
          fetch-depth: 0
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: refs/heads/dev
YAML
check "a commented-out sparse-checkout: does not count as one" 0 \
      "$TMP/commented-sparse" "no sparse checkout shares"

# --- ...and the direction that matters more: a commented-out `path:`
#     must not answer for a real one. The prose explaining this very
#     rule quotes `path: .resolver`; if a comment counted, documenting
#     the fix would be indistinguishable from applying it.
cat > "$TMP/commented-path/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          # path: .resolver
          sparse-checkout: scripts/one.sh
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          ref: refs/heads/dev
YAML
check "a commented-out path: does NOT separate the two trees" 1 \
      "$TMP/commented-path" "job build"

# --- one sparse checkout on its own is fine ---------------------------
cat > "$TMP/lone/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          sparse-checkout: scripts/one.sh
      - run: bash scripts/one.sh
YAML
check "a lone sparse checkout is fine" 0 "$TMP/lone" "1 of them sparse"

# --- nothing recognised is an error, not a pass -----------------------
cat > "$TMP/nocheckouts/w.yml" <<'YAML'
on:
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo no checkout here
YAML
check "a tree with no checkout at all is exit 2, not a green pass" 2 \
      "$TMP/nocheckouts" "pass having examined nothing"

check "an empty workflow directory is exit 2" 2 "$TMP/empty" "matched no"
check "a missing workflow directory is exit 2" 2 "$TMP/nope" "not a directory"

# --- THE LIVE POSITIVES, reconstructed from the real files ------------
#
# Delete the one line that fixes each file and the pre-fix shape is
# back, exactly, without anything being transcribed. If the deletion
# stops changing the file the case fails rather than passing quietly.
#
# pages.yml still needs exactly that one deletion. release.yml needs TWO
# mutations since #796, and the reason is worth stating because it is
# the shape this suite exists to notice.
#
# THE COLLISION NEEDS TWO CHECKOUTS IN ONE JOB. Deleting `path:` only
# reproduces the bug where a second checkout shares the job. #796 moved
# release.yml's resolver checkout into its own `resolve` job, where it
# is now the ONLY checkout — so the deletion alone leaves a lone
# non-colliding step and the gate correctly stays green. That is a real
# improvement in release.yml and a real hole in this case: the premise
# it was built on stopped holding, and the `cmp -s` guard could not see
# it, because the line it watches is still there and still gets deleted.
# The guard sat on the other case.
#
# So the release.yml mutant restores BOTH halves of the pre-#796 shape:
# a second, default-path checkout in the same job, and no `path:` on the
# sparse one. Both are derived from the file as it stands; nothing is
# transcribed, and each mutation carries a guard that fails loudly if it
# stops applying.
for pair in "real-pages:pages.yml:deploy" "real-release:release.yml:resolve"; do
    IFS=: read -r dir wf job <<<"$pair"
    src="$ROOT/.github/workflows/$wf"
    dst="$TMP/$dir/$wf"
    if [ ! -f "$src" ]; then
        fail_case "$wf is missing — the live-positive case tests nothing"
        continue
    fi
    grep -v '^ *path: \.resolver$' "$src" > "$dst"
    if cmp -s "$src" "$dst"; then
        fail_case "$wf no longer carries 'path: .resolver' — this case would" \
                  "otherwise pass having reconstructed nothing (#736)"
        continue
    fi
    if [ "$wf" = "release.yml" ]; then
        # Put a second checkout back into the resolver's job, at the
        # default path, immediately after the sparse step it must
        # collide with. Anchored on the sparse step's own last line so
        # it lands in that job and no other.
        awk '
          !done && /^ *sparse-checkout-cone-mode: false$/ {
              print
              print ""
              print "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1"
              print "        with:"
              print "          fetch-depth: 0"
              done = 1
              next
          }
          { print }
        ' "$dst" > "$dst.inj" && mv "$dst.inj" "$dst"
        if [ "$(grep -c 'actions/checkout@' "$dst")" -le "$(grep -c 'actions/checkout@' "$src")" ]; then
            fail_case "release.yml: the second checkout was not injected —" \
                      "the anchor moved and this case reconstructs nothing (#796)"
            continue
        fi
    fi
    check "the real $wf reconstructed into the pre-fix shape is the shipped bug" \
          1 "$TMP/$dir" "job $job"
done

# --- the real tree ----------------------------------------------------
check "the real .github/workflows passes" 0 "$ROOT/.github/workflows" \
      "no sparse checkout shares"

echo
if [ "$failures" -eq 0 ]; then
    echo "check-sparse-checkout-paths: $n/$n cases passed"
else
    echo "check-sparse-checkout-paths: $failures of $n cases FAILED"
fi
exit $(( failures > 0 ? 1 : 0 ))
