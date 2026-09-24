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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

CHECK="$(cd "$(dirname "$0")" && pwd)/check-latest-promotion.sh"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
guarded_tmpdir TMP

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
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$REF"
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
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-arm64:
    needs: [release, release-arm64]
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
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

# --- THE PRINTED REMEDY IS DERIVED, NOT TRANSCRIBED --------------------
#
# The remedy block used to carry a hand-written six-job `needs:` list.
# When the Hub alias added two more install proofs (#972), the list was
# not updated, so on exactly the failure above the gate printed the
# shape it had just rejected: a reader who pasted it got the same
# finding back. The remedy now prints the jobs THIS RUN derived.
#
# DRIVEN ON THE REAL WORKFLOW, not on the four-proof fixture above. The
# transcribed list happened to name the fixture's four proofs exactly,
# so a case built on it passed either way and measured nothing. This
# fixture is the shipping release.yml with promote-latest's `needs:`
# cut back to the two build jobs: the gate fails, and the remedy it
# prints has to name all eight proofs the workflow actually has.
sed 's|^\(    needs: \[release, release-arm64\), [^]]*\]|\1]|' \
    "$ROOT/.github/workflows/release.yml" > "$TMP/remedy.yml"
n=$((n + 1))
if ! grep -q '^    needs: \[release, release-arm64\]$' "$TMP/remedy.yml"; then
    echo "FAIL: the remedy fixture's needs: was not cut back; the case would measure nothing"
    failures=$((failures + 1))
elif [ "$(sed -n 's/^  \(verify-install[A-Za-z0-9_-]*\):$/\1/p' "$TMP/remedy.yml" | wc -l)" -lt 5 ]; then
    echo "FAIL: the remedy fixture has fewer install proofs than the transcribed list held,"
    echo "      so it cannot tell a derived remedy from a hand-written one"
    failures=$((failures + 1))
else
    echo "PASS: the remedy fixture fails with more install proofs than any transcribed list"
fi
n=$((n + 1))
remedy=$(bash "$CHECK" "$TMP/remedy.yml" 2>&1 || true)
remedy_shape=$(printf '%s\n' "$remedy" | sed -n '/The shape this expects/,/steps:/p')
missing=""
for g in $(sed -n 's/^  \(verify-install[A-Za-z0-9_-]*\):$/\1/p' "$TMP/remedy.yml"); do
    printf '%s\n' "$remedy_shape" | grep -F -- "$g" >/dev/null || missing="$missing $g"
done
if [ -n "$missing" ]; then
    echo "FAIL: the printed remedy omits install proof(s):$missing"
    echo "      A reader who copies it gets the finding the gate just reported."
    printf '%s\n' "$remedy_shape" | sed 's/^/    /'
    failures=$((failures + 1))
else
    echo "PASS: the printed remedy names every install proof the run derived"
fi

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
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
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
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
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

# --- the required gate list is DERIVED, not transcribed ---------------
# #972 adds a fifth and sixth install proof (the Hub alias). A checker
# carrying a hard-coded list of four job names passes this fixture,
# because the name it has never heard of is not on its list. The list is
# derived from the jobs that really install the published plugin, so an
# install proof the promotion does not wait on is a finding whatever it
# is called.
cat > "$TMP/newgate.yml" <<'YAML'
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
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
  verify-install-hub-alias:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install --grant-all-permissions "$ALIAS_REF"
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
check "a new install proof the promotion does not wait on is a finding" 1 \
      "$TMP/newgate.yml" "verify-install-hub-alias"

# The preservation control for the case above: the same fixture with the
# new proof wired into the promotion passes. Without it the case above
# only measures "hard", not "derived".
sed 's/^\( *needs: \[verify-install, .*\)\]$/\1, verify-install-hub-alias]/' \
    "$TMP/newgate.yml" > "$TMP/newgate-wired.yml"
if cmp -s "$TMP/newgate.yml" "$TMP/newgate-wired.yml"; then
    echo "FAIL: the wiring control did not change the fixture; it proves nothing"
    failures=$((failures + 1))
fi
check "the same new proof, wired in, passes" 0 "$TMP/newgate-wired.yml" \
      "all behind"

# --- an advertised install is not an install --------------------------
# A job that echoes the install command publishes nothing and proves
# nothing. If the derivation counted it, this fixture would be red for a
# job the promotion has no reason to wait on.
cat > "$TMP/echoedinstall.yml" <<'YAML'
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
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$REF"
  verify-install-hub:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
  verify-install-hub-arm64:
    needs: release
    runs-on: ubuntu-24.04-arm
    steps:
      - run: docker plugin install --grant-all-permissions "$HUB_REF"
  advertise-install:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - run: echo "docker plugin install --grant-all-permissions $ALIAS_REF"
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
check "an echoed install does not manufacture a required gate" 0 \
      "$TMP/echoedinstall.yml" "all behind"

# --- emptying the domain is a refusal, not a pass ---------------------
# Every requirement this check makes is quantified over the derived
# gates, so a file with none of them satisfies all of them. That is a
# universal gate satisfied by emptying its domain, and it exits 2.
sed 's/docker plugin install --grant-all-permissions/docker plugin ls #/' \
    "$TMP/transitive.yml" > "$TMP/noinstall.yml"
if grep -q 'docker plugin install' "$TMP/noinstall.yml"; then
    echo "FAIL: noinstall fixture still contains an install; the mutation did not apply"
    failures=$((failures + 1))
fi
check "zero derived install proofs exits 2" 2 "$TMP/noinstall.yml" \
      "No install proofs found"

# --- a mention is not an invocation (#883) ----------------------------
# Each decoy is the real release.yml with a command turned into text that
# names it; its control deletes the same lines. Both give one verdict.
REAL="$ROOT/.github/workflows/release.yml"
real_mutant() { # OUT SED-SCRIPT: the mutation has to have applied
    sed -E "$2" "$REAL" > "$TMP/$1"
    if cmp -s "$REAL" "$TMP/$1"; then
        echo "FAIL: $1 left release.yml byte-identical; re-anchor it"
        failures=$((failures + 1))
    fi
}
real_mutant crane-echoed.yml 's/^( +)crane tag /\1echo crane tag /'
check "an echoed crane tag is not a promotion" 2 "$TMP/crane-echoed.yml" \
      "No floating-tag promotion found"
real_mutant crane-deleted.yml '/^ +crane tag /d'
check "control: the crane tag lines deleted" 2 "$TMP/crane-deleted.yml" \
      "No floating-tag promotion found"
real_mutant recency-echoed.yml 's|run: bash (scripts/assert-newest-release-tag\.sh)|run: echo bash \1|'
check "an echoed recency call does not count" 1 "$TMP/recency-echoed.yml" \
      "assert-newest-release-tag.sh"
real_mutant recency-deleted.yml '/run: bash scripts\/assert-newest-release-tag\.sh/d'
check "control: the recency call deleted" 1 "$TMP/recency-deleted.yml" \
      "assert-newest-release-tag.sh"
real_mutant install-echoed.yml 's/^( +)docker plugin install --grant/\1echo docker plugin install --grant/'
check "a bare echoed install is not an install proof" 2 "$TMP/install-echoed.yml" \
      "No install proofs found"
real_mutant install-deleted.yml '/^ +docker plugin install --grant/d'
check "control: the installs deleted" 2 "$TMP/install-deleted.yml" \
      "No install proofs found"

# Shapes the real file does not hold: a separator inside quotes, a
# keyword that is an argument, a name: line. And the forms that do run.
sed 's|^          crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"$|          echo "done; crane tag ${GHCR_NAME}:${TAG} ${LATEST}"\n          echo then crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"|; s|- name: Promote the GHCR floating tags|- name: crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"|' \
    "$TMP/fixed.yml" > "$TMP/crane-text.yml"
check "a crane tag in a string, an argument or a name is not a promotion" 2 \
      "$TMP/crane-text.yml" "No floating-tag promotion found"
sed 's|^          crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"$|          true \&\& crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"|; s|run: bash scripts/assert-newest-release-tag.sh|run: set -e; sh ./scripts/assert-newest-release-tag.sh|' \
    "$TMP/fixed.yml" > "$TMP/crane-chained.yml"
check "a promotion after a separator still counts" 0 \
      "$TMP/crane-chained.yml" "all exercised by an rc"
sed 's|^          crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"$|          if ! crane tag "${GHCR_NAME}:${TAG}" "${LATEST}"; then exit 1; fi|' \
    "$TMP/fixed.yml" > "$TMP/crane-keyword.yml"
check "a promotion after if and ! still counts" 0 \
      "$TMP/crane-keyword.yml" "all exercised by an rc"

# --- the real workflow -------------------------------------------------
check "the real release.yml" 0 "$ROOT/.github/workflows/release.yml" \
      "all exercised by an rc"

echo
if [ "$failures" -ne 0 ]; then
    echo "$failures of $n case(s) FAILED"
    exit 1
fi
echo "all $n case(s) passed"
