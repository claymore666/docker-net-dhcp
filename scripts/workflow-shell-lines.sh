#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# What a workflow ACTUALLY EXECUTES, as opposed to what it mentions (#871).
#
# SOURCED, not run: `. scripts/workflow-shell-lines.sh` then
# `workflow_shell_lines <dir>`.
#
# WHY THIS EXISTS. Two gates asked "does this workflow run X?" by
# grepping the workflow files, and a grep cannot tell a command from
# the prose around it. Both were satisfiable by text that executes
# nothing:
#
#   * check-lint-tag-coverage.sh counted `- name: Run staticcheck
#     (default view)` as a staticcheck invocation. The untagged run
#     could be DELETED from CI and the gate still reported full
#     coverage — measured, and it printed "4 invocations" over two.
#   * run-gate-selftests.sh accepted a delegated self-test's filename
#     appearing anywhere under .github/workflows/, including inside a
#     comment. The 14-assertion suite could be removed from CI with
#     nothing going red, because a comment about it remained.
#
# Both failed in the permissive direction, and both are the defect
# #871 is about wearing a different hat: a check reporting success
# over something nothing actually runs.
#
# WHAT IT EMITS. One line per line of shell a workflow executes:
#
#   * the scalar after an inline `run: <command>`
#   * every line of a `run: |` / `run: >` block body, which is every
#     line indented deeper than the `run:` key itself
#
# and nothing else. `- name:`, `if:`, `env:`, `uses:`, `with:` and
# YAML comments are not shell and are not emitted. Within the shell
# that is emitted, a full-line `#` comment is dropped and a trailing
# `#` comment is cut at the first unquoted `#` that follows
# whitespace, so a token named in a comment cannot satisfy a caller.
#
# THE BOUNDARY, stated here rather than in a pull request, because a
# guarantee whose limits live somewhere else is how the next reader
# gets it wrong:
#
#   * It answers "is this token in something the workflow runs", not
#     "is this token the command being run". `run: echo foo.sh` emits
#     a line containing foo.sh. Callers that need argv position must
#     say so themselves.
#   * It does not evaluate `if:`, job-level conditions, matrix
#     exclusions or `continue-on-error`. A step that can never execute
#     still contributes its shell.
#   * It does not expand `${{ }}`, so a command assembled from an
#     expression is emitted verbatim.
#   * Only `*.yml` / `*.yaml` files are read, which is all GitHub
#     loads from a workflow directory.
#   * Quote tracking is per line. A shell string opened on one line of
#     a block and closed on the next may have a trailing comment cut
#     from it. That direction loses text, so it can only make a caller
#     fail to find a token -- loud -- never silently find one.
#
# Exit: 0 emitted (possibly nothing), 1 the directory does not exist.

# _wsl_awk -- the extractor, kept in one place so both callers share it.
_wsl_awk() {
    awk '
BEGIN { SQ = sprintf("%c", 39); inblock = 0; blockind = 0 }

# Cut a trailing comment: the first `#` that is unquoted and preceded
# by whitespace. Quote state is tracked so `grep "#"` survives.
function decomment(s,   i, c, q, prev) {
    q = ""
    for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (q != "") { if (c == q) q = ""; continue }
        if (c == "\"" || c == SQ) { q = c; continue }
        if (c == "#") {
            prev = (i == 1) ? "" : substr(s, i - 1, 1)
            if (prev == "" || prev == " " || prev == "\t") return substr(s, 1, i - 1)
        }
    }
    return s
}

function emit(s) {
    sub(/^[ \t]+/, "", s)
    if (s ~ /^#/) return              # a whole-line comment executes nothing
    s = decomment(s)
    sub(/[ \t]+$/, "", s)
    if (s != "") print s
}

{
    line = $0
    if (inblock) {
        if (line ~ /^[ \t]*$/) next   # a blank line does not close a block
        ind = match(line, /[^ ]/) - 1
        if (ind > blockind) { emit(line); next }
        inblock = 0                   # dedent: fall through, this is a key
    }
    if (line ~ /^[ \t]*(-[ \t]+)?run:/) {
        p = index(line, "run:")
        blockind = p - 1
        rest = substr(line, p + 4)
        sub(/^[ \t]+/, "", rest)
        sub(/[ \t]+$/, "", rest)
        # `|`, `|-`, `>`, `>2` ... a block scalar: the body follows.
        if (rest == "" || rest ~ /^[|>]/) { inblock = 1; next }
        # A quoted scalar is shell with the quotes removed, not shell
        # that begins with a quote -- otherwise `run: "staticcheck ..."`
        # loses its word boundary and reads as no invocation at all.
        if (rest ~ /^".*"$/ || rest ~ /^'"'"'.*'"'"'$/) rest = substr(rest, 2, length(rest) - 2)
        emit(rest)
        next
    }
}
' "$1"
}

# workflow_shell_lines DIR -- every line of shell the workflows under
# DIR execute. Returns 1 if DIR is not a directory; the caller decides
# whether that is a refusal, because "no workflows" and "no matching
# shell" are different findings.
workflow_shell_lines() {
    local dir="$1" f
    [ -d "$dir" ] || return 1
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        _wsl_awk "$f"
    done < <(find "$dir" -type f \( -name '*.yml' -o -name '*.yaml' \) | LC_ALL=C sort)
    return 0
}
