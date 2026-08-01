#!/usr/bin/env bash
# Decide whether the integration suite actually needs to run for this
# event (#311, #312). Two skippable shapes:
#
#   pr <number>     docs-only PR diffs (#311): every changed path is
#                   *.md — nothing the suite executes. The allowlist is
#                   deliberately just *.md; comment-only code changes,
#                   workflow edits, scripts, Dockerfile etc. all run
#                   the full suite.
#   dispatch        never skippable (#419). A manual run is a request
#                   for fresh evidence against a tree that has NOT
#                   changed — three consecutive green runs is this
#                   project's bar for trusting a change of this class.
#                   Taking the duplicate-tree skip there would report
#                   success in ~13 seconds having executed nothing,
#                   which is precisely the green-that-is-not-evidence
#                   shape of #418.
#   push <treesha>  duplicate post-merge trees (#312): a squash merge
#                   whose tree is byte-identical to a tree that already
#                   passed integration (the PR's merge-ref run, when the
#                   base didn't move in between). A tree that differs —
#                   the semantic-conflict case dev-push runs exist for —
#                   always runs.
#
# Prints exactly one word on stdout: "run" or "skip" (reason on stderr).
# FAIL-OPEN by design: any API error, unexpected input, missing tool, or
# unknown mode prints "run" — the expensive path is always the safe one.
# This gate is a job step, NOT a workflow paths filter: `integration` is
# a required check, and a workflow that never triggers leaves PRs stuck
# on "Expected" forever.
#
# API access is curl + jq, NOT the gh CLI: the self-hosted runner image
# doesn't ship gh (learned live — the first post-merge gate silently
# failed open on `gh: command not found`). curl and jq are baked into
# the image and verified below; if either goes missing the gate says so
# on stderr and fails open.
#
# Env: GATE_REPO=owner/repo, GH_TOKEN for the API.
# Test hook: the meta-test (scripts/test-integration-run-gate.sh) stubs
# `curl` via PATH; `classify` mode exposes the pure path-classifier.
set -uo pipefail

MODE="${1:-}"

api_get() {
    curl -sf --max-time 15 \
        -H "Authorization: Bearer ${GH_TOKEN:-}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/$1"
}

have_deps() {
    command -v curl >/dev/null && command -v jq >/dev/null && return 0
    echo "gate: curl or jq missing on this runner — failing open" >&2
    return 1
}

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
        have_deps || { echo run; exit 0; }
        # Page through the changed-file list. A page shorter than 100
        # entries is the last one; a still-full page past the cap means
        # we can't see the whole diff — fail open rather than judge a
        # truncated view.
        files="" page=1
        while :; do
            chunk=$(api_get "repos/${GATE_REPO}/pulls/${PR}/files?per_page=100&page=${page}" \
                    | jq -r '.[].filename') || { echo run; exit 0; }
            files="${files}${chunk}"$'\n'
            n=$(printf '%s\n' "$chunk" | grep -c .) || n=0
            [ "$n" -lt 100 ] && break
            page=$((page + 1))
            [ "$page" -gt 10 ] && { echo run; exit 0; }
        done
        if printf '%s' "$files" | docs_only; then
            echo "docs-only diff (all *.md) — suite adds no signal (#311)" >&2
            echo skip
        else
            echo run
        fi
        ;;
    dispatch)
        # Deliberately before any dependency or API check: there is
        # nothing to look up. Somebody asked for a run; that is the
        # whole decision.
        echo "manual dispatch — duplicate-tree skip does not apply, running the full suite (#419)" >&2
        echo run
        ;;
    push)
        TREE="${2:-}"
        [ -n "$TREE" ] && [ -n "${GATE_REPO:-}" ] || { echo run; exit 0; }
        have_deps || { echo run; exit 0; }
        shas=$(api_get "repos/${GATE_REPO}/actions/workflows/integration.yml/runs?status=success&per_page=15" \
                | jq -r '.workflow_runs[].head_sha') || { echo run; exit 0; }
        for sha in $shas; do
            t=$(api_get "repos/${GATE_REPO}/commits/${sha}" | jq -r '.commit.tree.sha') || continue
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
