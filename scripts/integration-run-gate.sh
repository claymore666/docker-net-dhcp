#!/usr/bin/env bash
# Decide whether the integration suite actually needs to run for this
# event (#311, #312). Two skippable shapes:
#
#   pr <number>     docs-only PR diffs (#311): every changed path is
#                   *.md — nothing the suite executes. The allowlist is
#                   deliberately just *.md; comment-only code changes,
#                   workflow edits, scripts, Dockerfile etc. all run
#                   the full suite.
#   push <treesha>  duplicate post-merge trees (#312): a squash merge
#                   whose tree is byte-identical to a tree that already
#                   passed integration (the PR's merge-ref run, when the
#                   base didn't move in between). A tree that differs —
#                   the semantic-conflict case dev-push runs exist for —
#                   always runs.
#
# Prints exactly one word on stdout: "run" or "skip" (reason on stderr).
# FAIL-OPEN by design: any API error, unexpected input, or unknown mode
# prints "run" — the expensive path is always the safe one. This gate is
# a job step, NOT a workflow paths filter: `integration` is a required
# check, and a workflow that never triggers leaves PRs stuck on
# "Expected" forever.
#
# Env: GATE_REPO=owner/repo, GH_TOKEN for `gh api`.
# Test hook: the meta-test (scripts/test-integration-run-gate.sh) stubs
# `gh` via PATH; `classify` mode exposes the pure path-classifier.
set -uo pipefail

MODE="${1:-}"

# docs_only: stdin = newline-separated changed paths.
# Exit 0 only when there is at least one path and every path is *.md.
docs_only() {
    local any=0 f
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        any=1
        case "$f" in
            *.md) ;;
            *) return 1 ;;
        esac
    done
    [ "$any" -eq 1 ]
}

case "$MODE" in
    classify)
        # Pure classifier for the meta-test: exit 0 = docs-only.
        docs_only
        ;;
    pr)
        PR="${2:-}"
        [ -n "$PR" ] && [ -n "${GATE_REPO:-}" ] || { echo run; exit 0; }
        files=$(gh api "repos/${GATE_REPO}/pulls/${PR}/files" --paginate -q '.[].filename' 2>/dev/null) \
            || { echo run; exit 0; }
        if printf '%s\n' "$files" | docs_only; then
            echo "docs-only diff (all *.md) — suite adds no signal (#311)" >&2
            echo skip
        else
            echo run
        fi
        ;;
    push)
        TREE="${2:-}"
        [ -n "$TREE" ] && [ -n "${GATE_REPO:-}" ] || { echo run; exit 0; }
        shas=$(gh api "repos/${GATE_REPO}/actions/workflows/integration.yml/runs?status=success&per_page=15" \
                -q '.workflow_runs[].head_sha' 2>/dev/null) \
            || { echo run; exit 0; }
        for sha in $shas; do
            t=$(gh api "repos/${GATE_REPO}/commits/${sha}" -q '.commit.tree.sha' 2>/dev/null) || continue
            if [ "$t" = "$TREE" ]; then
                echo "tree ${TREE} already passed integration at ${sha} — duplicate skipped (#312)" >&2
                echo skip
                exit 0
            fi
        done
        echo run
        ;;
    *)
        echo run
        ;;
esac
