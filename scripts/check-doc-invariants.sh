#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every documentation invariant declared in .github/doc-invariants.txt
# must still be present in every file that declares it (#579).
#
# WHY THIS EXISTS
#
# The STATE_DIR install precondition — `sudo mkdir -p /var/lib/net-dhcp`
# before `docker plugin install` — appears in README.md, docs/index.md
# and docs/reference.md. It is not changelog prose: Docker will not
# create a missing bind source, so skipping it fails the install at
# start-up and leaves the plugin installed but disabled, with no error
# anywhere that names the directory.
#
# Nothing could see it. check-version-pins judges image pins,
# check-docs-drift judges counters, settings and driver-opts — the block
# is a standing operator instruction with no code fact to reconcile
# against, so it fell between both. And
# its text names a version, which makes it read as stale the moment a
# later one ships: a release documentation pass is the likeliest place
# for it to be deleted, in good faith, with nothing going red.
#
# WHY IT IS A MANIFEST AND NOT A GREP FOR THAT BLOCK
#
# A hard-coded grep solves one instance of a class and rots the same
# way — the next standing precondition gets no gate, because adding one
# means writing another script. A manifest makes the cost of protecting
# the next one a five-line entry, and forces a justification next to it
# aimed at whoever is holding the delete key.
#
# WHAT IT CANNOT DO
#
# It judges presence, not correctness. Prose whose underlying behaviour
# genuinely changed must leave this file in the same commit it leaves
# the docs; the gate going red is the prompt to make that a decision
# rather than a side effect. Said plainly here because a gate described
# as protecting documentation invites the belief that documentation is
# therefore correct.
#
# Three optional declarations widen an entry (#745; the cosign-version
# rule used to be a gate of its own):
#   scope: <ERE>      the entry's files are every tracked Markdown file
#                     with a line matching the regex, discovered rather
#                     than listed, so a new page that copies the snippet
#                     is judged the moment it lands. No match is exit 2.
#   var: NAME=<path>  NAME's value is read from the `NAME=<value>` line
#                     of <path>; `${NAME}` in a marker becomes that value.
#                     A missing file, a missing line or an unresolved
#                     `${NAME}` is exit 2, never an empty substitution.
#   match: text       markers match case-insensitively with Markdown
#                     emphasis and code ticks stripped. Default is raw.
#
# Usage: check-doc-invariants.sh [--root <dir>] [--manifest <file>]
# Exit:  0 all invariants present
#        1 an invariant is violated — a marker is gone, or a declared
#          file does not exist
#        2 the gate cannot see: no manifest, no root, a manifest that
#          declares nothing, or an entry whose scope or variable
#          resolves to nothing (1 wins when both happen)

set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
MANIFEST=""

while [ $# -gt 0 ]; do
    case "$1" in
        --root) ROOT="${2:-}"; shift 2 || exit 2 ;;
        --manifest) MANIFEST="${2:-}"; shift 2 || exit 2 ;;
        *) echo "usage: $0 [--root <dir>] [--manifest <file>]" >&2; exit 2 ;;
    esac
done
[ -n "$MANIFEST" ] || MANIFEST="$ROOT/.github/doc-invariants.txt"

if [ ! -d "$ROOT" ]; then
    echo "::error title=Doc-invariant root missing::$ROOT is not a directory" >&2
    exit 2
fi
if [ ! -f "$MANIFEST" ]; then
    echo "::error title=Doc-invariant manifest missing::$MANIFEST" >&2
    echo "      Without it this gate has nothing to check and would otherwise" >&2
    echo "      pass having examined no documentation at all." >&2
    exit 2
fi

fail=0
blind=0
entries=0
markers_checked=0

# Emitted when the current entry ends (next header, or end of file).
# Everything the entry declared is validated here so the manifest is
# read exactly once, top to bottom.
check_entry() {
    local id="$1"
    [ -n "$id" ] || return 0
    entries=$((entries + 1))

    local -a files=("${files[@]}") markers=("${markers[@]}")
    local v name vpath val i found
    case "$matchmode" in
        raw|text) ;;
        *)
            echo "BLIND $id: match: $matchmode is not raw or text"
            blind=1
            return 0
            ;;
    esac
    for v in "${vars[@]}"; do
        name="${v%%=*}"
        vpath="${v#*=}"
        if [[ ! "$name" =~ ^[A-Z][A-Z0-9_]*$ ]] || [ "$name" = "$v" ]; then
            echo "BLIND $id: var: $v is not NAME=<path>"
            blind=1
            return 0
        fi
        val=""
        if [ -f "$ROOT/$vpath" ]; then
            val="$(sed -n "s/^${name}=\([A-Za-z0-9._-][A-Za-z0-9._-]*\)\([[:space:]].*\)\{0,1\}\$/\1/p" "$ROOT/$vpath" | head -1)"
        fi
        if [ -z "$val" ]; then
            echo "BLIND $id: no ${name}=<value> line in $vpath, so the marker cannot be built."
            echo "      That line is this entry's source of truth; restore it rather than"
            echo "      hardcoding the value in the marker."
            blind=1
            return 0
        fi
        for i in "${!markers[@]}"; do
            markers[i]="${markers[i]//\$\{$name\}/$val}"
        done
    done
    if [ "${#vars[@]}" -gt 0 ]; then
        for i in "${!markers[@]}"; do
            if [[ "${markers[i]}" =~ \$\{[A-Z_][A-Z0-9_]*\} ]]; then
                echo "BLIND $id: marker still holds an unresolved variable: ${markers[i]}"
                blind=1
                return 0
            fi
        done
    fi

    if [ -n "$scope" ]; then
        local -a md=() hits=()
        gate_subjects md md "$ROOT"
        mapfile -t hits < <(grep -lE -- "$scope" "${md[@]}" 2>/dev/null | sed "s|^${ROOT%/}/||")
        if [ "${#hits[@]}" -eq 0 ]; then
            echo "BLIND $id: no tracked Markdown page matches scope: $scope"
            echo "      That is almost certainly a broken search, not a real state."
            blind=1
            return 0
        fi
        files+=("${hits[@]}")
    fi

    if [ "${#files[@]}" -eq 0 ]; then
        echo "FAIL  $id declares no file: — an invariant that names no file is checked against nothing"
        fail=1
        return 0
    fi
    if [ "${#markers[@]}" -eq 0 ]; then
        echo "FAIL  $id declares no marker: — an invariant with no marker passes vacuously"
        fail=1
        return 0
    fi
    if [ "$justified" -eq 0 ]; then
        echo "FAIL  $id has no justification — say what breaks for the operator if this text goes,"
        echo "      addressed to whoever is about to delete it. An entry with no reason becomes a"
        echo "      line somebody deletes along with the block it protects."
        fail=1
    fi

    local f m path
    for f in "${files[@]}"; do
        path="$ROOT/$f"
        if [ ! -f "$path" ]; then
            # A manifest pointing at nothing must go red rather than
            # quietly check zero files — that is the failure mode this
            # whole gate exists to prevent, turned on itself.
            echo "FAIL  $id declares $f, which does not exist"
            echo "      Either the file moved and this entry must follow it, or the invariant is"
            echo "      genuinely gone and the entry must be removed deliberately."
            fail=1
            continue
        fi
        for m in "${markers[@]}"; do
            markers_checked=$((markers_checked + 1))
            if [ "$matchmode" = text ]; then
                found="$(tr -d '*`' < "$path" | grep -iFc -- "$m")"
            else
                found="$(grep -Fc -- "$m" "$path")"
            fi
            if [ "${found:-0}" -eq 0 ]; then
                echo "FAIL  $id: $f no longer contains: $m"
                echo "      This is a standing operator instruction, not a description of the"
                echo "      current version. Restore it, or — if the behaviour it describes has"
                echo "      actually changed — remove the entry from $(basename "$MANIFEST") in the"
                echo "      same commit, so the deletion is a decision and not a side effect."
                fail=1
            fi
        done
    done
}

id=""
files=()
markers=()
vars=()
scope=""
matchmode=raw
justified=0

while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
        '#'*) continue ;;
        '') continue ;;
    esac

    if [[ "$line" =~ ^[^[:space:]] ]]; then
        check_entry "$id"
        id="${line%%[[:space:]]*}"
        files=()
        markers=()
        vars=()
        scope=""
        matchmode=raw
        justified=0
        continue
    fi

    # Indented: a declaration or a line of justification. Leading
    # whitespace is stripped; a marker's own leading/trailing spaces
    # would be invisible in the manifest and are not worth preserving.
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
        file:*)
            value="${trimmed#file:}"
            value="${value#"${value%%[![:space:]]*}"}"
            [ -n "$value" ] && files+=("$value")
            ;;
        marker:*)
            value="${trimmed#marker:}"
            value="${value#"${value%%[![:space:]]*}"}"
            [ -n "$value" ] && markers+=("$value")
            ;;
        scope:*)
            scope="${trimmed#scope:}"
            scope="${scope#"${scope%%[![:space:]]*}"}"
            ;;
        var:*)
            value="${trimmed#var:}"
            value="${value#"${value%%[![:space:]]*}"}"
            vars+=("$value")
            ;;
        match:*)
            value="${trimmed#match:}"
            value="${value#"${value%%[![:space:]]*}"}"
            matchmode="$value"
            ;;
        *)
            justified=1
            ;;
    esac
done < "$MANIFEST"
check_entry "$id"

# A manifest that declares nothing is not a clean tree. Every check
# above reduces to a loop over the entries, so an empty one reports
# success having read no documentation — the same self-defeat that
# run-gate-selftests.sh guards against with its empty-glob check.
if [ "$entries" -eq 0 ]; then
    echo "::error title=No doc invariants declared::$MANIFEST parsed to zero entries." >&2
    echo "      This gate would otherwise pass having checked nothing at all." >&2
    exit 2
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi
if [ "$blind" -ne 0 ]; then
    exit 2
fi

echo "doc invariants: OK — $entries invariant(s), $markers_checked marker/file check(s) passed."
echo "NOTE: this proves the text is still THERE, not that it is still true. Prose whose"
echo "      behaviour has changed must leave $(basename "$MANIFEST") in the same commit it"
echo "      leaves the docs (#579)."
