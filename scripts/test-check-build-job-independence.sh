#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
#
# Tests for check-build-job-independence.sh (#796).
#
# THE LIVE POSITIVE IS THE POINT. The shape this gate refuses is not
# hypothetical and is not transcribed: it is what release.yml looked
# like until #796, and the case reconstructs it by putting `needs:
# release` back on the real file as it stands today. If that edit ever
# stops changing the file, the case fails loudly rather than passing
# having reconstructed nothing.
#
# THE VACUITY CASES CARRY EQUAL WEIGHT. "No publishing job depends on
# another" is true for free of a file with one publishing job, and of a
# file with none. A gate that answers a question about an empty set and
# reports success is the failure this tree has hit repeatedly, so both
# are exit 2 and both are tested.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/scripts/check-build-job-independence.sh"
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

# --- THE LIVE POSITIVE, reconstructed from the real file --------------
REAL="$ROOT/.github/workflows/release.yml"
if [ ! -f "$REAL" ]; then
    fail_case "release.yml is missing — the live-positive case tests nothing"
else
    # REPLACE the arm64 job's `needs:`, do not prepend a second one.
    # A duplicate key is not the pre-#796 shape -- it is invalid YAML,
    # and the first attempt at this case wrote one, whereupon the gate
    # read the surviving `needs: resolve` and correctly reported no
    # collision. The case failed and said so, which is the only reason
    # this comment exists rather than a silently useless fixture.
    awk '
      /^  release-arm64:[ \t]*$/ { inarm = 1; print; next }
      inarm && /^    needs:/      { print "    needs: release"; inarm = 0; next }
      { print }
    ' "$REAL" > "$TMP/prefix.yml"
    if cmp -s "$REAL" "$TMP/prefix.yml"; then
        fail_case "release.yml has no 'release-arm64:' job with a needs: — the" \
                  "pre-#796 shape could not be reconstructed and this case proves nothing"
    elif ! grep -q '^    needs: release$' "$TMP/prefix.yml"; then
        fail_case "the reconstruction did not put 'needs: release' back —" \
                  "the anchor moved and this case reconstructs nothing (#796)"
    else
        check "the real release.yml with 'needs: release' put back is the pre-#796 shape" \
              1 "$TMP/prefix.yml" "release-arm64 reaches release"
    fi
    check "the real release.yml passes" 0 "$REAL" "none waiting on another"
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
    needs: a
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  b:
    needs: middle
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
    needs: resolve
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

# --- refusal, not a verdict, on nothing to read -----------------------
check "a missing file is exit 2" 2 "$TMP/nope.yml" "not a readable file"

echo
if [ "$failures" -eq 0 ]; then
    echo "check-build-job-independence: $n/$n cases passed"
else
    echo "check-build-job-independence: $failures of $n cases FAILED"
    exit 1
fi
