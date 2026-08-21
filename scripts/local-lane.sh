#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The fast CI lane, runnable locally (#636).
#
# WHY THIS EXISTS
#
# `.github/workflows/test.yaml` runs its gates as hand-listed YAML steps.
# There was no way to run that lane on a workstation, so the only way to
# learn whether a branch was green was to push it and wait — which is how
# a merge on an incomplete check set becomes possible at all.
# `make integration-local` already covers the privileged lane; this is its
# counterpart for the lane that needs no root and catches most of what
# actually fails.
#
# THIS FILE IS THE LANE'S SINGLE SOURCE OF TRUTH.
#
# `scripts/check-local-lane.sh` reconciles it against test.yaml, so a gate
# added to the workflow without an entry here fails CI rather than quietly
# making this run test less than CI does. That is the #542 lesson applied
# one level up: a hand-listed local target would rebuild the same hole.
#
# The two list modes exist for that gate — it asks this file what it
# covers instead of parsing it, so the answer cannot drift from the code
# that produces it.
#
# WHAT IT DELIBERATELY DOES NOT DO
#
# No privileges, no host mutation. Anything needing root, a Docker daemon,
# the network, or a pull-request context is declared in OUT_OF_LANE below
# with its reason — "out of lane" must never quietly mean "runs nowhere".
#
# A step whose tool is missing is SKIPPED LOUDLY and named in the summary.
# STRICT=1 turns a skip into a failure (use it in any automation that
# treats a green exit as coverage). A silent skip is the failure mode this
# whole file exists to remove, so it is never the default and never quiet.
#
# Usage:
#   scripts/local-lane.sh              run the lane
#   scripts/local-lane.sh --list       print the scripts the lane runs
#   scripts/local-lane.sh --list-exempt   print "<script>\t<reason>"
# Env:
#   STRICT=1   a skipped step is a failure
# Exit: 0 all ran and passed, 1 something failed, 2 cannot run (empty lane).
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

# Find tools installed the way this project tells people to install them.
# The release runbook's tooling table is all `go install`, which puts
# binaries in $(go env GOPATH)/bin — a directory that is NOT on PATH by
# default. Without this, a developer who followed our own instructions
# gets `staticcheck` and `actionlint` reported as missing and silently
# unchecked, which is the exact failure this lane exists to remove.
if command -v go >/dev/null 2>&1; then
    gobin="$(go env GOBIN 2>/dev/null)"
    [ -n "$gobin" ] || gobin="$(go env GOPATH 2>/dev/null)/bin"
    [ -d "$gobin" ] && case ":$PATH:" in
        *":$gobin:"*) ;;
        *) PATH="$PATH:$gobin"; export PATH ;;
    esac
fi

# --- the lane ----------------------------------------------------------
# "label|required-tool|command".  required-tool is checked with
# `command -v`; "-" means the step needs nothing beyond a shell.
#
# Ordered cheapest-first so a fast failure arrives fast: the compiler
# before the test suite, the gates before the fuzzers.
LANE=(
  "go.mod tidy|go|go mod tidy -diff"
  "build|go|go build ./..."
  "vet|go|go vet ./..."
  "format|gofmt|test -z \"\$(gofmt -l .)\" || { echo 'gofmt -l found unformatted files:'; gofmt -l .; false; }"
  "staticcheck|staticcheck|staticcheck ./..."
  "shellcheck (scripts+runner+netboot)|shellcheck|shellcheck -S warning scripts/*.sh ci/runner-image/*.sh test/arm64-netboot/*.sh"
  "actionlint|actionlint|actionlint"
  "option-docs drift|-|bash scripts/check-option-docs.sh"
  "starter-task links|-|bash scripts/check-good-first-issues.sh --static"
  "docs drift|-|bash scripts/check-docs-drift.sh"
  "health contract|-|bash scripts/check-health-contract.sh"
  "version pins|-|bash scripts/check-version-pins.sh"
  "go pins|-|bash scripts/check-go-pins.sh"
  "manifest parity|-|bash scripts/check-manifest-parity.sh"
  "issue label map|-|bash scripts/check-issue-label-map.sh"
  "dockerfile pins|-|bash scripts/check-dockerfile-pins.sh"
  "python deps|-|bash scripts/check-python-deps.sh"
  "fixture hygiene|-|bash scripts/check-selftest-fixtures.sh"
  "pipefail consumers|-|bash scripts/check-pipefail-consumers.sh"
  "plugin bind sources|-|bash scripts/check-plugin-bind-sources.sh"
  "license headers|-|bash scripts/check-license-headers.sh"
  "lock discipline|-|bash scripts/check-lock-discipline.sh"
  "proc-path discipline|-|bash scripts/check-proc-path-discipline.sh"
  "manager registration|-|bash scripts/check-manager-registration.sh"
  "pi watchdog wiring|-|bash scripts/check-pi-watchdog-wiring.sh"
  "parent-gate accounting|-|bash scripts/check-parent-gate-accounting.sh"
  "doc invariants|-|bash scripts/check-doc-invariants.sh"
  "build-dir refs|-|bash scripts/check-build-dir-refs.sh"
  "build context|-|bash scripts/check-build-context.sh"
  "dockerignore parity|-|bash scripts/check-dockerignore-parity.sh"
  "registry login|-|bash scripts/check-registry-login.sh"
  "cosign docs|-|bash scripts/check-cosign-docs.sh"
  "dispatch-ref guard|-|bash scripts/check-dispatch-ref-guard.sh"
  "capture lane|-|bash scripts/check-capture-lane.sh"
  "dispatch reachable|-|bash scripts/check-dispatch-reachable.sh"
  # The lane checks itself: if test.yaml gains a gate this file does
  # not list, a local run says so instead of quietly covering less.
  "local-lane coverage|-|bash scripts/check-local-lane.sh"
  "gate self-tests|-|bash scripts/run-gate-selftests.sh"
  "unit tests (race)|go|go test -race -count=1 ./..."
  "fuzz (short)|go|for t in FuzzBuildEvent FuzzEventUnmarshal; do go test ./pkg/dhcp/ -run '^\$' -fuzz \"^\${t}\\\$\" -fuzztime 200000x -parallel 2 -timeout 5m || exit 1; done"
)

# --- declared out of lane ---------------------------------------------
# "script|reason".  Every reason is aimed at the person wondering why
# their change was not checked before they pushed it.
OUT_OF_LANE=(
  "scripts/check-no-ai-attribution.sh|judges a commit range against a pull-request body; there is no PR locally, and the range depends on the base the PR is opened against"
  "scripts/check-test-weakening.sh|same pull-request range and body, and its findings are waived by an issue reference in that body"
  "scripts/govulncheck-gate.sh|needs network access and a pinned govulncheck install; a local run would report a different vulnerability database than CI"
)

lane_scripts() {
    local e cmd
    for e in "${LANE[@]}"; do
        cmd="${e#*|}"; cmd="${cmd#*|}"
        grep -oE 'scripts/[A-Za-z0-9_.-]+\.sh' <<< "$cmd" || true
    done | sort -u
}

case "${1:-}" in
    --list) lane_scripts; exit 0 ;;
    --list-exempt)
        for e in "${OUT_OF_LANE[@]}"; do printf '%s\t%s\n' "${e%%|*}" "${e#*|}"; done
        exit 0 ;;
    "") ;;
    *) echo "usage: $0 [--list|--list-exempt]" >&2; exit 2 ;;
esac

if [ "${#LANE[@]}" -eq 0 ]; then
    echo "local-lane: the lane is empty — nothing was checked." >&2
    echo "A runner that inspects nothing must not report success." >&2
    exit 2
fi

# run-gate-selftests.sh includes a case that asserts the shard partition
# is identical under a comma-decimal locale, and it FAILS rather than
# skips when that locale is absent (#554) — deliberately, because a skip
# there is indistinguishable from a pass. Say so up front so the failure
# reads as a missing locale rather than a broken gate.
locale_note=""
if ! locale -a 2>/dev/null | grep -i '^de_DE' >/dev/null; then
    locale_note="the de_DE.UTF-8 locale is absent — 'gate self-tests' will fail on the shard locale case (#554). Fix: sudo locale-gen de_DE.UTF-8"
    printf '\033[33mNOTE\033[0m  %s\n\n' "$locale_note"
fi

pass=0; fail=0; skip=0
failed=(); skipped=()
started=$SECONDS

for entry in "${LANE[@]}"; do
    label="${entry%%|*}"
    rest="${entry#*|}"
    tool="${rest%%|*}"
    cmd="${rest#*|}"

    if [ "$tool" != "-" ] && ! command -v "$tool" >/dev/null 2>&1; then
        printf '\033[33mSKIP\033[0m  %-26s (%s not installed)\n' "$label" "$tool"
        skipped+=("$label ($tool)")
        skip=$((skip + 1))
        continue
    fi

    step_start=$SECONDS
    if out=$(bash -c "$cmd" 2>&1); then
        printf '\033[32mPASS\033[0m  %-26s %ss\n' "$label" "$((SECONDS - step_start))"
        pass=$((pass + 1))
    else
        printf '\033[31mFAIL\033[0m  %-26s %ss\n' "$label" "$((SECONDS - step_start))"
        printf '%s\n' "$out" | sed 's/^/      /'
        failed+=("$label")
        fail=$((fail + 1))
    fi
done

echo
printf 'local lane: %d passed, %d failed, %d skipped in %ss\n' \
    "$pass" "$fail" "$skip" "$((SECONDS - started))"

if [ "$skip" -gt 0 ]; then
    echo "skipped (NOT checked — install the tool or run CI):"
    printf '  %s\n' "${skipped[@]}"
fi
if [ "$fail" -gt 0 ]; then
    echo "failed:"
    printf '  %s\n' "${failed[@]}"
    [ -n "$locale_note" ] && echo "note: $locale_note"
fi

# Several gates discover their input through `git ls-files` rather than a
# filesystem walk, because a walk descends into gitignored worktrees and
# judges another branch's files (#639). The cost of that choice is that a
# file you have just written is invisible to them until it is staged —
# and "invisible" reads here as a clean pass.
#
# This is not hypothetical: check-pipefail-consumers.sh was written,
# run green by this lane, pushed, and went red in CI on its own
# self-test, because the self-test was still untracked when the lane
# ran. Twice in one afternoon, and it is the same shape as #569.
#
# So the lane says what it could not see. It does not stage anything —
# guessing at a half-written tree is its own trap — and it does not
# fail: this is a refusal to claim full coverage, not a verdict.
mapfile -t untracked < <(git ls-files --others --exclude-standard 2>/dev/null | head -20)
if [ "${#untracked[@]}" -ne 0 ]; then
    echo
    echo "NOT INSPECTED — ${#untracked[@]} untracked file(s). Gates that discover through"
    echo "  the git index cannot see these; \`git add\` them and re-run for a full verdict:"
    printf '    %s\n' "${untracked[@]}"
fi

echo
echo "Not run here, by declaration (scripts/local-lane.sh --list-exempt):"
for e in "${OUT_OF_LANE[@]}"; do printf '  %s — %s\n' "${e%%|*}" "${e#*|}"; done

[ "$fail" -gt 0 ] && exit 1
if [ "$skip" -gt 0 ] && [ -n "${STRICT:-}" ]; then
    echo
    echo "STRICT=1 and ${skip} step(s) were skipped — treating as failure." >&2
    exit 1
fi
exit 0
