#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every documented option has a line in the engine matrix (#1015): the
# pull-request half of scripts/engine-baseline.sh --print-option-steps,
# which runs on its own lane. The At a glance tables are read here with
# this file's own parser and compared with the detailed tables the cell
# derives from, so a table the derivation stops seeing changes one set
# and not the other.
# Env seams (the self-test's): ENGINE_OPTIONS_DOC, the document;
# ENGINE_OPTIONS_STEPS_CMD, prints `<option>|<kind>|<observer>` lines.
# Exit: 0 every documented option has a valid line, 1 one has not, 2 cannot check.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

DOC="${ENGINE_OPTIONS_DOC:-docs/reference.md}"
STEPS_CMD="${ENGINE_OPTIONS_STEPS_CMD:-bash scripts/engine-baseline.sh --print-option-steps}"

[ -f "$DOC" ] || {
    echo "FAIL  $DOC does not exist; there is nothing to derive the matrix options from." >&2
    exit 2
}

documented="$(ENGINE_SHAPES_DOC="$DOC" bash scripts/engine-baseline.sh --print-documented-options)" || {
    echo "FAIL  the engine matrix cannot read the documented options from $DOC; the message above names the source." >&2
    exit 1
}
steps="$(ENGINE_SHAPES_DOC="$DOC" $STEPS_CMD)" || {
    echo "FAIL  the option steps could not be printed from $DOC." >&2
    exit 1
}

fail=0

# The second population: the At a glance tables and the container-level
# flags sentence, read by this file and not by the derivation (#1015).
glance="$(awk '
    /^## / { g = ($0 == "## At a glance"); fl = 0; t = 0; next }
    !g { next }
    /^\| option \| (modes \| )?default \|/ { t = 1; next }
    !/^\|/ { t = 0 }
    t && /^\| `/ { split($0, f, "|"); n = f[2]; gsub(/^[ \t]*`|`[ \t]*$/, "", n); print n; next }
    /^\*\*\[Container-level flags\]/ { fl = 1 }
    fl && /^[ \t]*$/ { fl = 0 }
    fl { s = $0; while (match(s, /`--[a-z0-9-]+`/)) { print substr(s, RSTART + 1, RLENGTH - 2); s = substr(s, RSTART + RLENGTH) } }
' "$DOC" | sort -u)"
detailed="$(printf '%s\n' "$documented" | sort -u)"

if [ -z "$glance" ]; then
    echo "FAIL  $DOC has no option under \"## At a glance\"; this gate has nothing to compare." >&2
    exit 1
fi
while read -r o; do
    echo "FAIL  $DOC: \`$o\` is in the At a glance tables and not in the detailed option tables the cell derives from." >&2
    fail=1
done < <(comm -23 <(printf '%s\n' "$glance") <(printf '%s\n' "$detailed"))
while read -r o; do
    echo "FAIL  $DOC: \`$o\` is in the detailed option tables and not in the At a glance tables." >&2
    fail=1
done < <(comm -13 <(printf '%s\n' "$glance") <(printf '%s\n' "$detailed"))

notdriven=0
while IFS='|' read -r opt kind obs; do
    [ -n "$opt" ] || continue
    if ! grep -qxF -- "$opt" <<< "$detailed"; then
        echo "FAIL  the option catalogue has a line for \`$opt\`, which $DOC does not document." >&2
        fail=1
        continue
    fi
    case "$kind" in
        '') echo "FAIL  $DOC documents \`$opt\` and the engine matrix has no line for it." >&2
            fail=1; continue ;;
        shape|step|measure|not-driven) ;;
        *) echo "FAIL  \`$opt\` has the unknown kind '$kind'; the kinds are shape, step, measure, not-driven." >&2
           fail=1; continue ;;
    esac
    if [ -z "${obs//[[:space:]]/}" ]; then
        echo "FAIL  \`$opt\` ($kind) names no observer or reason." >&2
        fail=1
        continue
    fi
    case "$kind" in
        shape)
            case "$opt" in
                mode|bridge|parent) ;;
                *) echo "FAIL  \`$opt\` is called a shape and the shape steps drive only mode, bridge and parent." >&2
                   fail=1 ;;
            esac ;;
        not-driven)
            # The reason has to say which device the cell lacks (#1015).
            if ! grep -qiw 'device' <<< "$obs"; then
                echo "FAIL  \`$opt\` is not driven and its reason names no missing device: $obs" >&2
                fail=1
            else
                echo "::notice title=engine matrix: option not driven::$opt: $obs"
                notdriven=$((notdriven + 1))
            fi ;;
    esac
done <<< "$steps"

while read -r o; do
    [ -n "$o" ] || continue
    grep -q "^$(printf '%s' "$o" | sed 's/[.[\*^$]/\\&/g')|" <<< "$steps" || {
        echo "FAIL  $DOC documents \`$o\` and the option steps print no line for it." >&2
        fail=1
    }
done <<< "$detailed"

[ "$fail" -eq 0 ] || exit 1

echo "OK    $DOC documents $(grep -c . <<< "$detailed") options; every one has a matrix line ($notdriven not driven)."
exit 0
