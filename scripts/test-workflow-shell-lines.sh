#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for workflow-shell-lines.sh (#871, hardened in #872).
#
# TWO GATES NOW REST ON THIS ONE FUNCTION — check-lint-tag-coverage.sh
# decides whether a staticcheck invocation exists, and
# run-gate-selftests.sh decides whether a delegated self-test is run by
# anything. Both previously grepped the raw file and were satisfied by
# prose. Concentrating the answer in one place is only an improvement if
# that place is driven, so every recognition rule is asserted here in
# BOTH directions: the shape that must be emitted, and the shape that
# must not.
#
# The false-negative direction matters as much as the false-positive
# one. Over-stripping loses a real command, and the callers then report
# a gap that is not there — a gate that cries wolf gets discharged, and
# a discharged gate is how the hole comes back.
#
# Usage: bash scripts/test-workflow-shell-lines.sh

set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/workflow-shell-lines.sh
. "$HERE/workflow-shell-lines.sh"

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok   $1"; }
bad() { fail=$((fail+1)); echo "  FAIL $1"; }

# wf <yaml lines...> -- a single-workflow directory built from stdin-ish
# arguments, one line each.
wf() { D="$TMP/wf"; rm -rf "$D"; mkdir -p "$D"; printf '%s\n' "$@" > "$D/ci.yaml"; }
out() { workflow_shell_lines "$D"; }

# has <desc> <line>    -- the extractor emits exactly this line.
# hasnt <desc> <text>  -- no emitted line contains this text.
#
# Both match against a CAPTURED string with a case glob, never a
# pipeline into grep: the producer here is the extractor itself, and a
# `grep -q` consumer that exits on the first match kills it with
# SIGPIPE, so under pipefail the assertion would report the producer's
# death rather than the match.
has() {
    local o; o="$(out)"
    case $'\n'"$o"$'\n' in
        *$'\n'"$2"$'\n'*) ok "$1" ;;
        *) bad "$1 — '$2' not emitted. Got:"$'\n'"$o" ;;
    esac
}
hasnt() {
    local o; o="$(out)"
    case "$o" in
        *"$2"*) bad "$1 — '$2' was emitted and should not be. Got:"$'\n'"$o" ;;
        *) ok "$1" ;;
    esac
}

echo "1..N workflow-shell-lines"

# --- what IS shell ----------------------------------------------------
wf "jobs:" "  j:" "    steps:" "      - run: bash scripts/test-b.sh"
has "an inline run: is shell" "bash scripts/test-b.sh"

wf "jobs:" "  j:" "    steps:" "      - name: Do the thing" "        run: bash scripts/test-b.sh"
has "a named step's run: is shell" "bash scripts/test-b.sh"

wf "jobs:" "  j:" "    steps:" "      - run: |" "          set -e" "          bash scripts/test-b.sh"
has "a block scalar body is shell (first line)" "set -e"
has "a block scalar body is shell (second line)" "bash scripts/test-b.sh"

wf "jobs:" "  j:" "    steps:" "      - run: >" "          bash scripts/test-b.sh"
has "a folded scalar body is shell" "bash scripts/test-b.sh"

wf "jobs:" "  j:" "    steps:" "      - run: |" "          echo one" "" "          echo two"
has "a blank line does not close a block" "echo two"

wf "jobs:" "  j:" "    steps:" '      - run: "staticcheck ./..."'
has "a double-quoted scalar loses its quotes" "staticcheck ./..."

wf "jobs:" "  j:" "    steps:" "      - run: 'staticcheck ./...'"
has "a single-quoted scalar loses its quotes" "staticcheck ./..."

# --- what is NOT shell ------------------------------------------------
wf "jobs:" "  j:" "    steps:" "      - name: bash scripts/test-b.sh" "        run: echo hi"
hasnt "a step NAME is not shell" "test-b.sh"

wf "jobs:" "  j:" "    steps:" "      - uses: ./scripts/test-b.sh" "      - if: contains(x, 'test-b.sh')"
hasnt "uses: and if: are not shell" "test-b.sh"

wf "jobs:" "  j:" "    steps:" "      # - run: bash scripts/test-b.sh"
hasnt "a YAML comment is not shell" "test-b.sh"

wf "jobs:" "  j:" "    steps:" "      - run: |" "          # bash scripts/test-b.sh" "          echo hi"
hasnt "a shell comment inside a block is not shell" "test-b.sh"
has "and the real command beside it still is" "echo hi"

wf "jobs:" "  j:" "    steps:" "      - run: echo hi # bash scripts/test-b.sh"
hasnt "a trailing comment is cut" "test-b.sh"
has "and what precedes it survives" "echo hi"

wf "jobs:" "  j:" "    steps:" "      - run: |" "          echo one" "      - name: next" "        run: echo two"
has "a dedent closes the block and the next run: is read" "echo two"
hasnt "and the dedented key itself is not shell" "name: next"

# --- the over-stripping direction ------------------------------------
# Cutting at every `#` would silently drop a real command, and the
# callers would then report a gap that does not exist.
wf "jobs:" "  j:" "    steps:" "      - run: grep '#' file"
has "a hash inside single quotes is not a comment" "grep '#' file"

wf "jobs:" "  j:" "    steps:" '      - run: grep "a#b" file'
has "a hash inside double quotes is not a comment" 'grep "a#b" file'

wf "jobs:" "  j:" "    steps:" "      - run: echo a#b"
has "a hash with no space before it is not a comment" "echo a#b"

# --- file selection and refusal --------------------------------------
D="$TMP/mixed"; rm -rf "$D"; mkdir -p "$D"
printf '%s\n' "jobs:" "  j:" "    steps:" "      - run: echo yaml" > "$D/a.yaml"
printf '%s\n' "jobs:" "  j:" "    steps:" "      - run: echo yml" > "$D/b.yml"
printf '%s\n' "jobs:" "  j:" "    steps:" "      - run: echo readme" > "$D/README.md"
has "a .yaml file is read" "echo yaml"
has "a .yml file is read" "echo yml"
hasnt "a non-workflow file is not read" "echo readme"

D="$TMP/empty"; rm -rf "$D"; mkdir -p "$D"
if out >/dev/null 2>&1 && [ -z "$(out)" ]; then
    ok "an empty directory emits nothing and returns 0"
else
    bad "an empty directory did not emit nothing at rc=0"
fi

if workflow_shell_lines "$TMP/nosuchdir" >/dev/null 2>&1; then
    bad "a missing directory returned 0 — the caller cannot tell it apart from empty"
else
    ok "a missing directory returns non-zero, distinct from finding no shell"
fi

# --- NON-VACUITY against the real tree --------------------------------
# Every case above is synthetic. If the extractor stopped reading this
# repository's own workflows it would still pass all of them, and both
# callers would go red for a reason that has nothing to do with them.
REAL="$HERE/../.github/workflows"
if [ -d "$REAL" ]; then
    n=$(workflow_shell_lines "$REAL" | wc -l)
    if [ "$n" -ge 100 ]; then
        ok "the repository's own workflows yield $n shell line(s)"
    else
        bad "only $n shell line(s) extracted from $REAL — the extractor has gone blind"
    fi
    real_out="$(workflow_shell_lines "$REAL")"
    case "$real_out" in
        *"staticcheck -tags integration"*)
            ok "and the integration-view invocation is among them" ;;
        *)  bad "the integration-view staticcheck invocation was not extracted" ;;
    esac
else
    bad "no $REAL to check against — this suite would be entirely synthetic"
fi

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
