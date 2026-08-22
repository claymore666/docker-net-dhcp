#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Tests for check-latest-promotion.sh (#736), on fixture workflows plus
# one assertion against the real .github/workflows/release.yml.
#
# THE CASE THAT CARRIES THE WEIGHT is `prefix`: a reduction of the real
# pre-#736 release.yml, with `crane tag ... latest` inside the `release`
# job, guarded by `if: prerelease != 'true'`. That is the shape that
# shipped, and it is the shape this check exists to make impossible. It
# was also run against the actual pre-fix file — all four retag lines
# (192, 203, 518, 526, the ones #736 cites) were reported — but a
# fixture is kept here so the test does not depend on git history being
# present or unshallow.
#
# THE GUARD HAS A DIRECTION, so both directions are tested:
#   - `skipped` — promotion conditioned on prerelease (an rc that
#     exercises nothing). Must fail.
#   - `noassert` — promotion NOT conditioned, but nothing asserts the rc
#     left `:latest` alone. Must also fail. Without this case, deleting
#     rule (4) from the checker would go unnoticed.
#   - `norecency` — promotion correctly ordered, unconditional, aimed at
#     a computed tag and asserted for rc-immutability, but with nothing
#     stopping a dispatch of an OLDER tag. Rules (1)-(4) all pass on it.
#     Must fail on rule (5) alone.
#
# MUTANT COVERAGE: the suite contains cases expecting exit 0, exit 1 and
# exit 2, so a checker mutated to `exit 0` unconditionally fails the
# fixtures below, and one mutated to `exit 1` fails `fixed` and the real
# workflow. Neither mutant survives.
set -u

CHECK="$(cd "$(dirname "$0")" && pwd)/check-latest-promotion.sh"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
n=0

# check NAME WANT_EXIT FILE GREP_PATTERN
check() {
    local name="$1" want_exit="$2" file="$3" want_grep="$4"
    n=$((n + 1))
    bash "$CHECK" "$file" > "$TMP/out" 2>&1
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

# --- the pre-#736 shape: retag inside the publishing job --------------
cat > "$TMP/prefix.yml" <<'YAML'
name: Release
on:
  push:
    tags:
      - "v*"
jobs:
  release:
    runs-on: ubuntu-latest
    outputs:
      tag: ${{ steps.tag.outputs.tag }}
    steps:
      - name: Push to GHCR
        run: make push
      - name: Tag :latest to the published GHCR digest
        if: steps.tag.outputs.prerelease != 'true'
        run: crane tag "${GHCR_NAME}:${{ steps.tag.outputs.tag }}" latest
      - name: Sign published images (cosign keyless)
        run: cosign sign --yes "${GHCR_NAME}@${DIGEST}"
  verify-install:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install "$REF"
YAML
check "pre-#736: retag inside the publishing job" 1 "$TMP/prefix.yml" \
      "without depending on 'verify-install'"
check "pre-#736: an rc would skip the promote path" 1 "$TMP/prefix.yml" \
      "conditioned on prerelease"
check "pre-#736: literal destination tag" 1 "$TMP/prefix.yml" \
      "promotes to the literal 'latest'"

# --- correct shape: promotion last, rc-exercisable, with the assertion -
cat > "$TMP/fixed.yml" <<'YAML'
name: Release
on:
  push:
    tags:
      - "v*"
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: make push
  release-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: make push
  verify-install:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install "$REF"
  verify-install-arm64:
    needs: [release, release-arm64]
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install "$HUB_REF"
  promote-latest:
    needs: [release, release-arm64, verify-install, verify-install-arm64, verify-install-hub, verify-install-hub-arm64]
    runs-on: ubuntu-latest
    steps:
      - name: Refuse to promote a floating tag backwards
        run: bash scripts/assert-newest-release-tag.sh "${TAG}"
      - name: Promote the GHCR floating tags
        run: |
          crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"
      - name: Assert a pre-release did not move :latest
        if: needs.release.outputs.prerelease == 'true'
        run: |
          crane digest "${GHCR_NAME}:latest"
YAML
check "fixed: promotion behind all four install proofs" 0 "$TMP/fixed.yml" \
      "all exercised by an rc"

# --- direction A: promotion skipped on an rc --------------------------
sed 's|      - name: Promote the GHCR floating tags|      - name: Promote the GHCR floating tags\n        if: needs.release.outputs.prerelease != '"'"'true'"'"'|' \
    "$TMP/fixed.yml" > "$TMP/skipped.yml"
check "skipped: an rc exercises nothing" 1 "$TMP/skipped.yml" \
      "conditioned on prerelease"

# --- direction B: nothing asserts the rc left :latest alone -----------
# Same file with the immutability assertion removed. The promotion is
# correctly ordered and correctly unconditional, so rules (1)-(3) pass
# and only rule (4) can catch this.
sed '/Assert a pre-release did not move/,$d' "$TMP/fixed.yml" > "$TMP/noassert.yml"
check "noassert: nothing proves an rc left :latest untouched" 1 "$TMP/noassert.yml" \
      "no prerelease-conditional step"

# --- direction C: nothing stops a dispatch of an OLDER tag ------------
# The promotion is ordered last, unconditional, aimed at a computed tag
# and asserted for rc-immutability — every earlier rule passes. It is
# simply pointed at the wrong release, which is what
# `gh workflow run release.yml -f tag=v1.6.0` does, and what the
# runbook offers as its recovery step for a failed release.
sed '/Refuse to promote a floating tag backwards/,+1d' "$TMP/fixed.yml" \
    > "$TMP/norecency.yml"
check "norecency: an older tag could be promoted over a newer one" 1 \
      "$TMP/norecency.yml" "assert-newest-release-tag.sh"

# The recency call must be a STEP, not prose about one. Same trap as the
# `commented` case below: this file explains rule (5) in comments that
# name the script.
sed 's|      - name: Refuse to promote a floating tag backwards|      # - name: Refuse to promote a floating tag backwards|; s|        run: bash scripts/assert-newest-release-tag.sh|        # run: bash scripts/assert-newest-release-tag.sh|' \
    "$TMP/fixed.yml" > "$TMP/recency-commented.yml"
check "a commented-out recency call does not count" 1 \
      "$TMP/recency-commented.yml" "assert-newest-release-tag.sh"

# --- direction D: the Hub proofs run, but nothing waits for them ------
# The shape #776 could have shipped: both Hub install jobs present and
# green, and `promote-latest` behind only the GHCR pair. Rules (2)-(5)
# all pass, and so does rule (1) for two of its four gates -- so the run
# is green while `:latest` moves without the Docker Hub half of the
# deliverable ever having been proven installable. Without this case,
# deleting the two Hub entries from REQUIRED_GATES would go unnoticed.
sed 's|, verify-install-hub, verify-install-hub-arm64\]|]|' \
    "$TMP/fixed.yml" > "$TMP/nohub.yml"
# The mutation has to have applied, and it has to have removed only the
# `needs:` entries. A fixture that lost the jobs themselves would fail
# rule (1) for a different reason and prove nothing about REQUIRED_GATES.
if ! grep -q '^  verify-install-hub:' "$TMP/nohub.yml"; then
    echo "FAIL: nohub fixture lost the Hub jobs themselves; it would fail for the wrong reason"
    failures=$((failures + 1))
fi
if grep -q 'needs:.*verify-install-hub' "$TMP/nohub.yml"; then
    echo "FAIL: nohub fixture still lists a Hub gate in needs; the mutation did not apply"
    failures=$((failures + 1))
fi
check "nohub: :latest moves without the Docker Hub install proof" 1 \
      "$TMP/nohub.yml" "verify-install-hub"

# --- transitive reach counts ------------------------------------------
# promote-latest needs a job that needs verify-install*. A failed gate
# skips everything downstream of it however many hops away, so this must
# pass — a checker that only looked one hop would reject it.
cat > "$TMP/transitive.yml" <<'YAML'
name: Release
on:
  push:
    tags:
      - "v*"
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: make push
  verify-install:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install "$HUB_REF"
  collect:
    needs: [verify-install, verify-install-arm64, verify-install-hub, verify-install-hub-arm64]
    runs-on: ubuntu-latest
    steps:
      - run: echo collected
  promote-latest:
    needs: collect
    runs-on: ubuntu-latest
    steps:
      - name: Refuse to promote a floating tag backwards
        run: bash scripts/assert-newest-release-tag.sh "${TAG}"
      - run: crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"
      - name: Assert a pre-release did not move :latest
        if: needs.release.outputs.prerelease == 'true'
        run: crane digest "${GHCR_NAME}:latest"
YAML
check "transitive: reach through an intermediate job" 0 "$TMP/transitive.yml" \
      "all behind"

# --- comments must not count as behaviour -----------------------------
# release.yml explains this rule in prose that names both `crane tag`
# and `prerelease`. A checker that read comments would fire on its own
# documentation.
cat > "$TMP/commented.yml" <<'YAML'
name: Release
on:
  push:
    tags:
      - "v*"
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      # crane tag "${GHCR_NAME}:${TAG}" latest used to run here, before
      # signing, and was skipped when prerelease was true.
      - run: make push
  verify-install:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install "$HUB_REF"
  promote-latest:
    needs: [verify-install, verify-install-arm64, verify-install-hub, verify-install-hub-arm64]
    runs-on: ubuntu-latest
    steps:
      - name: Refuse to promote a floating tag backwards
        run: bash scripts/assert-newest-release-tag.sh "${TAG}"
      - run: crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"
      - name: Assert a pre-release did not move :latest
        if: needs.release.outputs.prerelease == 'true'
        run: crane digest "${GHCR_NAME}:latest"
YAML
check "comments are not steps" 0 "$TMP/commented.yml" "all behind"

# --- refusing to pass having examined nothing -------------------------
check "missing file exits 2" 2 "$TMP/does-not-exist.yml" "is not a file"

cat > "$TMP/nopromo.yml" <<'YAML'
name: Release
on:
  push:
    tags:
      - "v*"
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: make push
YAML
check "no promotion at all exits 2" 2 "$TMP/nopromo.yml" \
      "No floating-tag promotion found"

cat > "$TMP/nojobs.yml" <<'YAML'
name: Release
on:
  push:
    tags:
      - "v*"
YAML
check "no jobs exits 2" 2 "$TMP/nojobs.yml" "yielded no jobs"

# --- the real workflow -------------------------------------------------
check "the real release.yml" 0 "$ROOT/.github/workflows/release.yml" \
      "all exercised by an rc"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
