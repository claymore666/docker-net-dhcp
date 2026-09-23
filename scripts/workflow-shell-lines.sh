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
#     a line containing foo.sh. Callers that need argv position pipe
#     the lines through `shell_command_words` (#883).
#   * It does not evaluate `if:`, job-level conditions, matrix
#     exclusions or `continue-on-error`. A step that can never execute
#     still contributes its shell.
#   * It does not expand `${{ }}`, so a command assembled from an
#     expression is emitted verbatim.
#   * Only `*.yml` / `*.yaml` files are read, which is all GitHub
#     loads from a workflow directory.
#   * Quote tracking here is per line, so a string spanning lines of a
#     block is emitted as separate lines. For a word search a line of
#     prose then reads as shell. `--raw` emits the block unchanged with
#     a separator before each `run:`, and the command readers below
#     join the open quote and cut comments themselves (#883).
#
# Exit: 0 emitted (possibly nothing), 1 the directory does not exist.

# _wsl_awk -- the extractor, kept in one place so both callers share it.
_wsl_awk() {
    awk -v raw="${2:-0}" '
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
    if (raw) { print s; return }
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
        if (raw) printf "%c\n", 1
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

# workflow_shell_lines [--raw] DIR|FILE -- every line of shell the workflows
# under DIR, or the one workflow FILE, execute. Returns 1 if DIR is not a directory; the caller decides
# whether that is a refusal, because "no workflows" and "no matching
# shell" are different findings.
workflow_shell_lines() {
    local dir="$1" f
    local raw=0
    if [ "$dir" = --raw ]; then raw=1; dir="$2"; fi
    if [ -f "$dir" ]; then _wsl_awk "$dir" "$raw"; return 0; fi
    [ -d "$dir" ] || return 1
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        _wsl_awk "$f" "$raw"
    done < <(find "$dir" -type f \( -name '*.yml' -o -name '*.yaml' \) | LC_ALL=C sort)
    return 0
}

# shell_command_words -- reads shell lines on stdin, prints the word in
# command position of every simple command, behind `bash`/`sh` and their
# options, and the first word of a `bash -c` string (#883). Feed it
# `workflow_shell_lines --raw` so quotes spanning lines are joined. Out
# of reach: case patterns, and later commands inside a `bash -c` string.
shell_command_words() { _wsl_commands 0; }

# shell_simple_commands -- the same commands, one per line, command word
# first, then its arguments with quotes removed and redirects dropped.
shell_simple_commands() { _wsl_commands 1; }

_wsl_commands() {
    awk -v full="$1" '
BEGIN { SQ = sprintf("%c", 39); SEP = sprintf("%c", 1); KW = "^(if|then|else|elif|fi|do|done|while|until|esac|!|time|\\{|\\}|exec|command|nohup)$" }
function reset() { atstart = 1; collecting = 0; buf = ""; wrapper = 0; cstr = 0; skipword = 0; sp = 0; q = ""; incmd = 0; cur = "" }
function endcmd() { if (incmd) print cur; incmd = 0; cur = "" }
function finish(   n, k, w, cw) {
    if (!collecting) return
    collecting = 0
    if (skipword) { skipword = 0; buf = ""; return }
    if (hdwant) { hdend = buf; hdwant = 0; buf = ""; return }
    if (cstr) {
        cstr = 0; wrapper = 0; atstart = 0; w = ""
        n = split(buf, cw, /[ \t;&|()]+/)
        for (k = 1; k <= n; k++) {
            if (cw[k] == "" || cw[k] ~ /^[A-Za-z_][A-Za-z0-9_]*\+?=/ || cw[k] ~ KW) continue
            w = cw[k]; break
        }
        buf = ""
        if (w == "") return
        if (full) { incmd = 1; cur = w; for (k++; k <= n; k++) if (cw[k] != "") cur = cur " " cw[k] } else print w
        return
    }
    if (!atstart) { if (incmd) cur = cur " " buf; buf = ""; return }
    if (buf ~ /^[A-Za-z_][A-Za-z0-9_]*\+?=/ && !wrapper) { buf = ""; return }
    if (buf ~ KW && !wrapper) { buf = ""; return }
    if (buf == "bash" || buf == "sh") { if (!wrapper) { wrapper = 1; buf = ""; return } }
    if (wrapper && buf ~ /^-/) { if (buf ~ /^-[A-Za-z]*c[A-Za-z]*$/) cstr = 1; buf = ""; return }
    if (full) { incmd = 1; cur = buf } else print buf
    atstart = 0; wrapper = 0; buf = ""
}
function opencmd() { finish(); endcmd(); atstart = 1; wrapper = 0 }
function push(tag) { sp++; stk[sp] = tag; scur[sp] = cur; sinc[sp] = incmd; incmd = 0; cur = ""; collecting = 0; buf = ""; atstart = 1; wrapper = 0 }
function pop() {
    finish(); endcmd()
    q = stk[sp]; cur = scur[sp]; incmd = sinc[sp]; sp--
    if (q == "=assign") { q = ""; atstart = 1; collecting = 1; buf = "x=" }
    else atstart = 0
}
function tokenize(s,   i, c, n) {
    reset()
    for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1); n = substr(s, i + 1, 1)
        if (q == SQ) { if (c == SQ) q = ""; else if (collecting) buf = buf c; continue }
        if (q == "\"") {
            if (c == "\\") { if (collecting) buf = buf n; i++; continue }
            if (c == "\"") { q = ""; continue }
            if (c == "$" && n == "(") { if (collecting && !atstart) finish(); push(q); q = ""; i++; continue }
            if (collecting) buf = buf c
            continue
        }
        if (c == " " || c == "\t") { finish(); continue }
        if (c == "#" && !collecting) break
        if (c == "\\") { if (!collecting) { collecting = 1; buf = "" } buf = buf n; i++; continue }
        if (c == SQ || c == "\"") { q = c; if (!collecting) { collecting = 1; buf = "" } continue }
        if (c == "$" && n == "(") {
            if (collecting && atstart && buf ~ /^[A-Za-z_][A-Za-z0-9_]*\+?=/) push("=assign")
            else { finish(); push("") }
            i++; continue
        }
        if (c == ")" && sp > 0) { pop(); continue }
        if (c == "<" && n == "<") {
            finish()
            if (substr(s, i + 2, 1) == "<") { i += 2; skipword = 1; continue }
            i++; if (substr(s, i + 1, 1) == "-") i++
            hdwant = 1; continue
        }
        if (c == "<" || c == ">") {
            if (collecting && buf ~ /^[0-9]+$/) { collecting = 0; buf = "" }
            finish()
            if (n == "&" || n == ">" || n == "|") i++
            skipword = 1; continue
        }
        if (c == ";" || c == "&" || c == "|" || c == "(" || c == ")" || c == "`") { opencmd(); continue }
        if (!collecting) { collecting = 1; buf = "" }
        buf = buf c
    }
    finish(); endcmd()
    while (sp > 0) { endcmd(); cur = scur[sp]; incmd = sinc[sp]; sp--; endcmd() }
}
function openq(s,   i, c, qq, prev) {
    qq = ""; prev = " "
    for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (qq == SQ) { if (c == SQ) qq = ""; prev = c; continue }
        if (qq == "\"") { if (c == "\\") i++; else if (c == "\"") qq = ""; prev = c; continue }
        if (c == "\\") { i++; prev = "x"; continue }
        if (c == "#" && prev ~ /[ \t;&|(]/) break
        if (c == SQ || c == "\"") qq = c
        prev = c
    }
    return qq != ""
}
{
    line = $0
    if (line == SEP) { if (pending != "") tokenize(pending); pending = ""; hdend = ""; next }
    if (hdend != "") { t = line; gsub(/^[ \t]+|[ \t]+$/, "", t); if (t == hdend) hdend = ""; next }
    if (pending != "") { line = pending line; pending = "" }
    if (line ~ /\\$/ && line !~ /\\\\$/) { pending = substr(line, 1, length(line) - 1) " "; next }
    if (openq(line)) { pending = line " "; next }
    hdwant = 0
    tokenize(line)
}
END { if (pending != "") tokenize(pending) }
'
}
