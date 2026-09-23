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

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/workflow-shell-lines.sh
. "$HERE/workflow-shell-lines.sh"

guarded_tmpdir TMP
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

# --- shell_command_words: command position (#883) ----------------------
# words <desc> <expected, space-joined> <shell line>...
words() {
    local desc="$1" want="$2" got; shift 2
    got="$(printf '%s\n' "$@" | shell_command_words | tr '\n' ' ')"
    got="${got% }"
    [ "$got" = "$want" ] && ok "$desc" || bad "$desc — want '$want', got '$got'"
}
words "a plain command" "scripts/a.sh" "scripts/a.sh --flag x.sh"
words "behind bash and its options" "scripts/a.sh" "bash -e scripts/a.sh"
words "an echo argument is not a command" "echo" 'echo "scripts/a.sh runs elsewhere"'
words "a separator inside quotes is not a separator" "echo" 'echo "x; bash scripts/a.sh | y"'
words "a single-quoted substitution is not run" "echo" "echo '\$(bash scripts/a.sh)'"
words "each side of && || | ;" "a b c d true" "a && b || c | d; true x"
words "an assignment is skipped, its substitution counts" "scripts/a.sh" 'x=$(bash scripts/a.sh arg) || x=run'
words "a substitution inside double quotes counts" "scripts/a.sh" 'row="$(bash scripts/a.sh --rows)"'
words "keywords are skipped" "[ scripts/a.sh" 'if [ -f x ]; then bash scripts/a.sh; fi'
words "loop keywords are skipped" "for scripts/a.sh" 'for f in x; do bash scripts/a.sh "$f"; done'
words "negation and grouping" "scripts/a.sh echo" '! bash scripts/a.sh || { echo no; }'
words "a redirect target is not a command" "scripts/a.sh" 'bash scripts/a.sh > out.sh 2>&1'
words "a quoted command word" "./scripts/a.sh" 'FOO=1 "./scripts/a.sh"'
words "a continuation joins the next line" "echo" 'echo "see" \' '  "scripts/a.sh describe"'
words "a continued command still counts" "scripts/a.sh" 'bash \' '  scripts/a.sh'
words "a heredoc body is not run" "cat scripts/b.sh" "cat <<'EOF'" "scripts/a.sh" "EOF" "scripts/b.sh"
words "an indented heredoc delimiter" "cat" "cat <<-EOF" "  scripts/a.sh" "  EOF"
words "a here-string is data" "grep" 'grep x <<< "scripts/a.sh"'
words "a double quote spanning lines is data" "echo" 'echo "gates:' 'scripts/a.sh"'
words "a single quote spanning lines is data" "gh" "gh pr comment 1 --body '" "scripts/a.sh" "'"
words "the line after a spanning quote is a command" "echo scripts/a.sh scripts/b.sh" 'echo "a' 'b" && scripts/a.sh' 'scripts/b.sh'
words "a # inside a spanning quote is data" "echo" 'echo "a # b' 'scripts/a.sh"'
words "an escaped quote does not close a spanning string" "echo" 'echo "a \" b' 'scripts/a.sh""' '"'
words "a trailing comment is not run" "true" 'true # bash scripts/a.sh'
words "a separator inside a comment is not a separator" "true" 'true # x; scripts/a.sh'
words "a quote inside a comment opens nothing" "true scripts/a.sh" "true # it's" 'scripts/a.sh'
words "a quoted or glued # is not a comment" "echo scripts/a.sh" 'echo "#x" a#b; scripts/a.sh'
words "a bash -c string runs its first word" "echo" 'bash -c "echo scripts/a.sh"'
words "a bash -c string, assignment skipped" "scripts/a.sh" 'bash -ec "FOO=1 scripts/a.sh x; y"'

# cmds <desc> <expected, lines joined by |> <shell line>...
cmds() {
    local desc="$1" want="$2" got; shift 2
    got="$(printf '%s\n' "$@" | shell_simple_commands | paste -sd'|')"
    [ "$got" = "$want" ] && ok "$desc" || bad "$desc — want '$want', got '$got'"
}
cmds "arguments follow the command word, quotes removed" "staticcheck -tags integration ./..." "staticcheck -tags 'integration' ./..."
cmds "redirects and fd numbers are dropped" "make a|tee x" 'make a 2>&1 | tee x'
cmds "an echo keeps its text as arguments" "echo staticcheck -tags x" 'echo staticcheck -tags x'
cmds "a nested substitution is its own command" "git rev-parse --short HEAD|make capture-fixtures C=" 'make capture-fixtures C="$(git rev-parse --short HEAD)"'
cmds "a bash -c string is the command" "staticcheck -tags integration ./..." "bash -c 'staticcheck -tags integration ./...'"

wf 'jobs:' '  t:' '    steps:' '      - run: bash scripts/a.sh'
got="$(workflow_shell_lines "$D/ci.yaml")"
[ "$got" = "bash scripts/a.sh" ] && ok "a single workflow file is read" \
    || bad "a single workflow file gave '$got'"

# --raw: a quote left open in one run: cannot swallow the next (#883).
wf 'jobs:' '  t:' '    steps:' '      - run: echo "open' '      - run: bash scripts/a.sh'
got="$(workflow_shell_lines --raw "$D" | shell_command_words | paste -sd' ')"
[ "$got" = "echo scripts/a.sh" ] && ok "--raw ends an open quote at the next run:" \
    || bad "--raw let a quote cross into the next step: '$got'"
wf 'jobs:' '  t:' '    steps:' '      - run: |' '          echo "a # b' '          bash scripts/a.sh"'
got="$(workflow_shell_lines --raw "$D" | shell_command_words | paste -sd' ')"
[ "$got" = "echo" ] && ok "--raw keeps a # inside a quote spanning lines" \
    || bad "--raw over a spanning quote gave '$got'"
wf 'jobs:' '  t:' '    steps:' '      - run: |' '          echo "start' '          x # y" && bash scripts/a.sh'
got="$(workflow_shell_lines --raw "$D" | shell_command_words | paste -sd' ')"
[ "$got" = "echo scripts/a.sh" ] && ok "--raw does not cut a # on a string's second line" \
    || bad "--raw cut inside a spanning string: '$got'"

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
