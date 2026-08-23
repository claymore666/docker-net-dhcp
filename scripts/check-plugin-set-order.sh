#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# `docker plugin set` requires the plugin DISABLED. Every snippet that
# sets a value must disable first and enable after.
#
# WHY THIS EXISTS
#
# The daemon refuses the call outright:
#
#   $ docker plugin set ghcr.io/claymore666/docker-net-dhcp:v0.4.0 LOG_LEVEL=trace
#   Error response from daemon: cannot set on an active plugin, disable plugin before setting
#
# So a snippet written as "set, then disable && enable" does not do the
# wrong thing in a subtle way — it fails on its FIRST line, and the
# reader is left holding an error message that does not name the
# instruction that misled them. Every copy of that snippet in this
# repository had the order backwards, in four places across two files,
# while the repository states the rule correctly in its own words at
# test/integration/README.md and obeys it in the Makefile at three call
# sites. Prose drifted; the executable copies never did.
#
# WHAT IT CHECKS
#
#   1. In a fenced block containing `docker plugin set`, a
#      `docker plugin disable` appears on an EARLIER line, and an
#      `enable` appears after the last `set`. A block is a recipe: it
#      is meant to be pasted, so it must work as written.
#   2. In a prose paragraph that gives an INVOCATION — a
#      `docker plugin set` carrying an assignment — and also mentions
#      disabling, the disable comes first. That is the sentence a
#      reader turns into a command. The paragraph is joined and judged
#      by character offset, never line by line: prose wraps wherever
#      the margin falls, and the first draft of this gate got that
#      wrong in both directions — it missed a backwards instruction
#      whose "disable and\nenable" straddled a line break, and it
#      failed a correct paragraph that opened with "Disable the
#      plugin," and closed by explaining why. Both were found by
#      scripts/test-check-plugin-set-order.sh, which is the argument
#      for a gate having one.
#
# WHAT IT DOES NOT CHECK, deliberately, in two places:
#
#   - a prose mention with no disable anywhere near it. Naming the
#     command — "settings are changed with `docker plugin set`",
#     pointing at the section that explains it — is not a recipe, and
#     demanding the full cycle at every mention would push the gate
#     into rewriting prose rather than judging instructions.
#   - a prose mention with no assignment after it. The first draft of
#     this gate failed test/integration/README.md, which says
#     "`docker plugin set` requires the plugin to be disabled, so
#     changing one means `docker plugin disable`, `set`, `enable`" —
#     the rule stated CORRECTLY, and it necessarily names the command
#     before it names the disable. Reading that as a backwards
#     instruction was the gate being wrong, not the prose, and the fix
#     was to judge invocations rather than mentions.
#
# A gate must say where it stops looking. This is where.
#
# Usage: check-plugin-set-order.sh [<file>...]   (default: tracked *.md)
# Exit:  0 every snippet is in the right order, 1 one is not.
set -uo pipefail

if [ "$#" -gt 0 ]; then
    files=("$@")
else
    mapfile -t files < <(git ls-files '*.md')
fi
[ "${#files[@]}" -gt 0 ] || { echo "check-plugin-set-order: no files to check" >&2; exit 2; }

fail=0

SET_RE='docker plugin set'
# Inside a fenced block only the command counts -- a block is pasted,
# and a comment saying "disable first" does not disable anything.
BLOCK_DIS_RE='docker plugin disable'
# In prose it is written every way English allows: the command, the
# shorthands the reference uses ("a disable/enable cycle"), and the
# bare verb ("Disable the plugin, then ..."). Matching the verb is
# deliberately broad, and it is safe BECAUSE this rule only fires on a
# paragraph that already carries an invocation: the question is never
# "is disabling mentioned here", only "is it mentioned before the
# command the reader is being told to run".
PROSE_DIS_RE='[Dd]isabl'

for f in "${files[@]}"; do
    [ -f "$f" ] || continue
    grep -qE "$SET_RE" "$f" || continue

    awk -v file="$f" -v setre="$SET_RE" -v bdisre="$BLOCK_DIS_RE" -v disre="$PROSE_DIS_RE" '
    function flush_block(   i) {
        if (bset == 0) { return }
        if (bdis == 0 || bdis > bset) {
            printf "FAIL  %s:%d a fenced block runs `docker plugin set` with no `docker plugin disable` before it — the daemon refuses the call on an enabled plugin, so this block fails on that line\n", file, bsetline > "/dev/stderr"
            bad = 1
        } else if (benable == 0 || benable < blastset) {
            printf "FAIL  %s:%d a fenced block disables and sets but never enables after the last set — it leaves the plugin down\n", file, bsetline > "/dev/stderr"
            bad = 1
        }
    }
    # THE PARAGRAPH IS JUDGED AS ONE STRING, not line by line.
    #
    # Line-by-line was wrong twice, and the self-test found both. A
    # paragraph wraps wherever the margin falls: "then disable and\nenable
    # the plugin" put the phrase across two lines and no disable was seen
    # at all, so a backwards instruction PASSED. And a correct paragraph
    # that opens "Disable the plugin, run `docker plugin set ...`" and
    # closes by explaining that a disable/enable cycle is required had
    # its LAST mention compared against the set, so correct prose FAILED.
    #
    # Offsets into the joined text answer the only question worth asking:
    # does the reader meet the disable before the command they are being
    # told to run. Where the line breaks fall is not part of that.
    function flush_para(   ip, id) {
        if (para == "") { pset = 0; return }
        ip = match(para, setre)
        if (ip == 0) { para = ""; pset = 0; return }
        # An INVOCATION, not a mention: a set carrying an assignment.
        if (substr(para, ip + RLENGTH) !~ /=/) { para = ""; pset = 0; return }
        id = match(para, disre)
        if (id > 0 && id > ip) {
            printf "FAIL  %s:%d a paragraph tells the reader to `docker plugin set` and THEN disable/enable; the set is refused on an enabled plugin, so the order has to be disable, set, enable\n", file, psetline > "/dev/stderr"
            bad = 1
        }
        para = ""; pset = 0
    }
    {
        if ($0 ~ /^[[:space:]]*(```|~~~)/) {
            if (infence) { flush_block(); infence = 0; bset = bdis = benable = blastset = bsetline = 0 }
            else { flush_para(); infence = 1 }
            next
        }
        if (infence) {
            if ($0 ~ setre)            { if (bset == 0) { bset = NR; bsetline = NR } blastset = NR }
            if ($0 ~ bdisre) { if (bdis == 0) bdis = NR }
            if ($0 ~ /docker plugin enable|plugin enable/) benable = NR
            next
        }
        if ($0 ~ /^[[:space:]]*$/) { flush_para(); next }
        # Accumulate the paragraph. Only an INVOCATION is judged, and an
        # invocation is a set carrying an assignment:
        # `docker plugin set <plugin> NAME=v`. A sentence that merely
        # names the command in order to state its constraint —
        # "`docker plugin set` requires the plugin to be disabled" — is
        # the rule being stated CORRECTLY, and it legitimately names the
        # command before it names the disable. test/integration/README.md
        # says exactly that, and reading it as a backwards instruction is
        # the gate being wrong, not the prose.
        para = (para == "" ? $0 : para " " $0)
        if (pset == 0 && $0 ~ setre) { pset = NR; psetline = NR }
    }
    END { flush_para(); if (infence) flush_block(); exit bad }
    ' "$f" || fail=1
done

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "\`docker plugin set\` is refused while the plugin is enabled:" >&2
    echo "  Error response from daemon: cannot set on an active plugin, disable plugin before setting" >&2
    echo "The order is always: disable, set, enable. The repository states" >&2
    echo "this correctly in test/integration/README.md and obeys it in the" >&2
    echo "Makefile; the documentation is what drifts." >&2
    exit 1
fi

echo "PASS  every \`docker plugin set\` snippet disables first and enables after (${#files[@]} file(s) read)"
exit 0
