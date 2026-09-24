#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every workflow `run:` step that pipes into tee must run under pipefail.
# GitHub's implicit shell is `bash -e` without pipefail, so tee's exit 0
# wins and the step reports success over a failed command: #297 shipped
# that way, and the engine-matrix floor step did it again (#1015).
# Satisfied by `shell: bash` (which runs `bash -eo pipefail`), a shell
# naming pipefail, or `set ... -o pipefail` above the tee in the block.
# Workflow- or job-level `defaults:` are not read: such a step is red.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2
DIR="${WORKFLOW_DIR:-.github/workflows}"

shopt -s nullglob
files=("$DIR"/*.yml "$DIR"/*.yaml)
if [ "${#files[@]}" -eq 0 ]; then
    echo "FAIL  $DIR holds no workflow files; nothing was checked." >&2
    exit 2
fi

out="$(awk '
function indent(s) { match(s, /^ */); return RLENGTH }
function close_step() {
    if (tee_line != "" && !(shell == "bash" || shell ~ /pipefail/ || pf_before_tee))
        printf "%s:%s: pipes into tee without pipefail (shell: %s)\n", cur, tee_line, (shell == "" ? "implicit bash -e" : shell)
    in_step = 0; shell = ""; tee_line = ""; pf_seen = 0; pf_before_tee = 0; in_block = 0
}
FNR == 1 { close_step(); cur = FILENAME }
{
    line = $0
    ind = indent(line)
    blank = (line ~ /^[ \t]*$/)
    if (in_block) {
        if (blank || ind > run_ind) {
            body = line
            if (body ~ /^[ \t]*#/) next
            if (body ~ /set[ \t]+(-[a-zA-Z]*o[ \t]+pipefail|.*-o[ \t]+pipefail)/) pf_seen = 1
            if (tee_line == "" && body ~ /(^|[^|])\|[ \t]*(sudo[ \t]+)?tee([ \t]|$)/) { tee_line = FNR; pf_before_tee = pf_seen }
            next
        }
        in_block = 0
    }
    if (blank || line ~ /^[ \t]*#/) next
    if (in_step && ind <= step_ind) close_step()
    if (line ~ /^ *- /) { close_step(); in_step = 1; step_ind = ind }
    if (!in_step) next
    key = line; sub(/^ *(- )?/, "", key)
    kind = (line ~ /^ *- /) ? ind + 2 : ind
    if (key ~ /^shell:/) { shell = key; sub(/^shell:[ \t]*/, "", shell); sub(/[ \t]+#.*$/, "", shell); gsub(/["\047]/, "", shell) }
    if (key ~ /^run:[ \t]*[|>][-+]?[ \t]*(#.*)?$/) { in_block = 1; run_ind = kind; next }
    if (key ~ /^run:/) {
        cmd = key; sub(/^run:[ \t]*/, "", cmd)
        if (cmd ~ /(^|[^|])\|[ \t]*(sudo[ \t]+)?tee([ \t]|$)/ && tee_line == "") tee_line = FNR
    }
}
END { close_step() }
' "${files[@]}")"

if [ -n "$out" ]; then
    while IFS= read -r l; do
        echo "FAIL  $l" >&2
        echo "::error file=${l%%:*},line=$(printf '%s' "$l" | cut -d: -f2),title=tee without pipefail::add shell: bash to the step (#1015)"
    done <<< "$out"
    exit 1
fi
echo "OK    every workflow step that pipes into tee runs under pipefail (${#files[@]} files)."
