#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every build-constraint term in the tree must be linted by something (#871).
#
# WHAT WENT WRONG. `.github/workflows/test.yaml` ran `staticcheck ./...`
# with no `-tags`. A Go build constraint is not a filter the linter
# applies afterwards — the tagged files are never compiled, so they are
# never parsed. Measured at 0b73c46 on dev: 196 tracked .go files, 64
# carrying `//go:build integration`. A third of the repository sat
# outside a REQUIRED status check that reported success over it.
#
# Both counts are of the tree at that commit and they move; the gap
# does not.
#
# IT FAILED IN BOTH DIRECTIONS AT ONCE, which is why it is worth a gate
# rather than a one-line fix. A defect inside those 64 files was
# invisible. And a defect that did not exist was reported: a symbol
# defined in an untagged file whose only caller is tagged reads as
# unused (U1000), because the linter sees the definition and not the
# use. That happened on a live PR. A gate manufacturing work is how a
# gate gets discharged.
#
# WHY THIS CHECKS COVERAGE RATHER THAN JUST ASSERTING `-tags integration`.
# On the tree this was written against, the tagged view is a strict
# superset of the untagged one, so a single `-tags integration` run
# would have covered everything. That is a property of THAT TREE, not
# of the gate: a file carrying `//go:build !integration` compiles in
# the untagged view and vanishes from the tagged one. There is no such
# file today — which is exactly why nothing would notice the day one
# arrives. So the rule is derived from the tree's constraints, and the
# untagged view is required to exist for the negated case that does not
# exist yet.
#
# THE RULES
#
#   1. at least one staticcheck invocation carries no `-tags` at all
#      (the default view: untagged files, and every negated term)
#   2. every POSITIVE term appearing in a build constraint is named in
#      the `-tags` of at least one invocation
#
# IT REFUSES RATHER THAN CLEARS. A compound constraint — `a && b`,
# `a || b`, parentheses — cannot be reduced to "which invocation
# compiles this" by anything this small, so the gate exits 2 and names
# the file instead of passing. A coverage gate that silently cannot
# judge is the failure being fixed here, wearing a different hat.
#
# NON-VACUITY. No .go files, or no staticcheck invocation found at all,
# is exit 2. A coverage rule over an empty population is satisfied by
# emptying the population, and it would read as green.
#
# WHAT IT DOES NOT SEE. Constraints are read from `//go:build` lines
# anywhere in a tracked .go file, not from the build-constraint
# position specifically; a `//go:build` inside a string or a comment
# block would be counted. That direction is toward reporting, never
# toward silence. Legacy `// +build` lines are read too — Go still
# honours them when no `//go:build` is present.
#
# Usage: check-lint-tag-coverage.sh
# Env:   LINT_TAG_ROOT       repo root (default: the parent of scripts/)
#        LINT_TAG_WORKFLOWS  workflow dir (default: $ROOT/.github/workflows)
# Exit:  0 covered, 1 a term nothing lints, 2 cannot judge.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${LINT_TAG_ROOT:-$(cd "$HERE/.." && pwd)}"
WORKFLOWS="${LINT_TAG_WORKFLOWS:-$ROOT/.github/workflows}"

[ -d "$ROOT" ] || { echo "FAIL  no such root: $ROOT" >&2; exit 2; }
[ -d "$WORKFLOWS" ] || {
    echo "FAIL  no workflow directory at $WORKFLOWS -- nothing to read invocations from" >&2
    exit 2
}

# TRACKED files only. An untracked scratch .go file is not part of what
# CI lints, and counting it would make this gate fail on a dirty tree.
#
# The repo check is separate from the empty check ON PURPOSE. `mapfile
# < <(git ...)` reports mapfile's status, never git's, so a git failure
# arrives here as an empty list and is indistinguishable from a tree
# with no Go in it. Both are exit 2, but they are different things to
# go and fix, and an error folded into a value has no direction.
if ! git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    echo "FAIL  $ROOT is not a git working tree, so the tracked file list cannot" \
         "be read. This is not the same as finding no Go files there." >&2
    exit 2
fi
mapfile -t gofiles < <(git -C "$ROOT" ls-files '*.go' 2>/dev/null)
if [ "${#gofiles[@]}" -eq 0 ]; then
    echo "FAIL  no tracked .go files under $ROOT. A coverage rule over an" \
         "empty population is satisfied by emptying the population." >&2
    exit 2
fi

# --- terms in the tree ------------------------------------------------
declare -A positive=() negated=()
compound=()
for f in "${gofiles[@]}"; do
    while IFS= read -r line; do
        # The trailing space is load-bearing: `//go:build*` also matches
        # `//go:buildfoo`, which is not a constraint and would invent a
        # term out of the rest of the line.
        case "$line" in
            "//go:build "*)  expr="${line#//go:build }" ;;
            "// +build "*)   expr="${line#// +build }" ;;
            *) continue ;;
        esac
        # A comma in the legacy syntax is AND, and a space there is OR.
        # Both are compound, and so are the modern operators.
        if [[ "$expr" == *"&&"* || "$expr" == *"||"* || "$expr" == *"("* || "$expr" == *","* ]]; then
            compound+=("$f: $(printf '%s' "$line" | sed 's/^[[:space:]]*//')")
            continue
        fi
        # read -ra rather than words=($expr): the latter globs, and a
        # //go:build line quoted inside a string could then expand
        # against the working directory.
        read -ra words <<< "$expr"
        if [ "${#words[@]}" -ne 1 ]; then
            compound+=("$f: $(printf '%s' "$line" | sed 's/^[[:space:]]*//')")
            continue
        fi
        t="${words[0]}"
        if [[ "$t" == !* ]]; then negated["${t#!}"]=1; else positive["$t"]=1; fi
    done < "$ROOT/$f"
done

if [ "${#compound[@]}" -ne 0 ]; then
    echo "FAIL  cannot judge lint coverage: ${#compound[@]} compound build constraint(s)." >&2
    printf '  %s\n' "${compound[@]}" >&2
    echo >&2
    echo "  This gate maps ONE term to one invocation. A compound expression needs" >&2
    echo "  a real constraint solver to answer 'which -tags compiles this', and" >&2
    echo "  guessing would clear a file nothing lints. Extend the gate, or split" >&2
    echo "  the constraint." >&2
    exit 2
fi

# --- invocations in the workflows -------------------------------------
# `go install .../cmd/staticcheck@vX` is not an invocation: staticcheck
# there is preceded by a slash, so requiring a word boundary in front
# excludes it without naming the install line.
mapfile -t invocations < <(
    grep -rhE '(^|[[:space:]|;&(])staticcheck[[:space:]]' "$WORKFLOWS" 2>/dev/null |
        sed 's/^[[:space:]]*//' | grep -v '^#'
)

if [ "${#invocations[@]}" -eq 0 ]; then
    echo "FAIL  no staticcheck invocation found under $WORKFLOWS. This gate would" \
         "otherwise report full coverage having found nothing that lints anything." >&2
    exit 2
fi

untagged=0
declare -A covered=()
for inv in "${invocations[@]}"; do
    tags=$(printf '%s' "$inv" | sed -n 's/.*-tags[ =]\{1,\}\([A-Za-z0-9_,]*\).*/\1/p')
    if [ -z "$tags" ]; then
        untagged=1
        continue
    fi
    IFS=',' read -r -a parts <<< "$tags"
    for p in "${parts[@]}"; do [ -n "$p" ] && covered["$p"]=1; done
done

# --- verdict ----------------------------------------------------------
problems=()

if [ "$untagged" -eq 0 ]; then
    problems+=("no staticcheck invocation runs WITHOUT -tags. Every file carrying a
    negated constraint (//go:build !x) compiles only in that view, and so does
    every untagged file the tagged view happens to exclude. There is no negated
    constraint in the tree today, which is precisely why its disappearance from
    CI would go unnoticed.")
fi

for t in "${!positive[@]}"; do
    if [ -z "${covered[$t]:-}" ]; then
        n=$(grep -lE "^//go:build[[:space:]]+$t\$|^// \+build[[:space:]]+$t\$" \
            "${gofiles[@]/#/$ROOT/}" 2>/dev/null | wc -l)
        problems+=("build tag '$t' is carried by $n tracked .go file(s) and named by no
    staticcheck -tags. Those files are not compiled by any invocation, so they are
    not linted by any of them, and the check reports success over them.")
    fi
done

if [ "${#problems[@]}" -ne 0 ]; then
    echo "lint tag coverage: ${#problems[@]} gap(s)." >&2
    for p in "${problems[@]}"; do echo "  - $p" >&2; done
    echo >&2
    echo "  Add the missing invocation to a workflow. Two runs rather than one" >&2
    echo "  widened flag: see the header." >&2
    exit 1
fi

echo "lint tag coverage: clean (${#gofiles[@]} tracked .go file(s), ${#positive[@]} positive" \
     "term(s) [${!positive[*]}], ${#negated[@]} negated, ${#invocations[@]} staticcheck invocation(s))"
exit 0
