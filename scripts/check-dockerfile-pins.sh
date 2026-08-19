#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Dockerfile base-image pin gate (#633).
#
# Every `FROM` in the tree must name its image by digest. Scorecard
# reported `test/arm64-netboot/Dockerfile` as unpinned (alert 95) while
# the other three Dockerfiles pinned by digest — including the sibling
# that pins the SAME base image. So the convention was already
# universal, and nothing enforced it: one file was added without a
# digest and only an advisory external scan noticed.
#
# It matters most where it was missing. That image serves the arm64
# runner its root filesystem, so an unpinned base means the machine
# running the integration suite can change between two builds, and a
# suite that passes on one and fails on the next says nothing about the
# plugin.
#
# MATCHES THE SUPERSET ON PURPOSE. It looks at every FROM and then
# judges, rather than matching only well-formed pins. A gate written the
# other way round cannot by construction see the reference that is
# malformed — which is exactly how a wrong-namespace image reference
# stayed invisible for months in #487.
#
# Exempt, and only these:
#   - a scratch base, which has no digest to name
#   - a stage reference (`FROM builder`, `FROM x AS y` reusing a stage
#     defined earlier in the same file): naming a digest there is
#     impossible, and the stage is already pinned at its own FROM.
#
# Usage: check-dockerfile-pins.sh [root]
# Env:   PIN_GATE_FILES  newline-separated file list, overriding
#                        discovery (the seam the self-test drives).
# Exit:  0 every FROM is pinned, 1 one or more are not, 2 cannot check.
set -uo pipefail

ROOT="${1:-.}"

if [ ! -d "$ROOT" ]; then
    echo "check-dockerfile-pins: $ROOT is not a directory" >&2
    exit 2
fi

# An Actions annotation puts the verdict on the checks page, so a reader
# does not have to open a log to see which file is unpinned.
annotate() {
    [ -n "${GITHUB_ACTIONS:-}" ] && printf '::error file=%s,line=%s::%s\n' "$1" "$2" "$3"
    return 0
}

if [ -n "${PIN_GATE_FILES:-}" ]; then
    files="$PIN_GATE_FILES"
else
    files=$(find "$ROOT" \
                -path '*/.git' -prune -o \
                -type f \( -name 'Dockerfile' -o -name 'Dockerfile.*' -o -name '*.Dockerfile' \) \
                -print 2>/dev/null | sort)
fi

if [ -z "$files" ]; then
    # Zero files is not a pass. This repo has Dockerfiles; finding none
    # means the discovery broke, and a gate that reports clean having
    # inspected nothing is worse than no gate.
    echo "::error title=No Dockerfiles found::check-dockerfile-pins inspected nothing under ${ROOT}." >&2
    exit 2
fi

bad=0
checked=0
stages=""

while IFS= read -r f; do
    [ -z "$f" ] && continue
    if [ ! -r "$f" ]; then
        echo "check-dockerfile-pins: cannot read $f" >&2
        exit 2
    fi
    # Stage names are per-file: a `FROM x AS builder` in one Dockerfile
    # does not license a bare `FROM builder` in another.
    stages=""
    lineno=0
    while IFS= read -r line; do
        lineno=$((lineno + 1))
        case "$line" in
            [Ff][Rr][Oo][Mm][[:space:]]*) ;;
            *) continue ;;
        esac

        # Drop flags such as --platform=$BUILDPLATFORM, then take the
        # image reference and any `AS <stage>` that follows it.
        rest=$(printf '%s' "$line" | sed -E 's/^[Ff][Rr][Oo][Mm][[:space:]]+//')
        while :; do
            case "$rest" in
                --*) rest=$(printf '%s' "$rest" | sed -E 's/^--[^[:space:]]+[[:space:]]+//') ;;
                *) break ;;
            esac
        done
        image=$(printf '%s' "$rest" | awk '{print $1}')
        alias=$(printf '%s' "$rest" | awk 'tolower($2) == "as" { print $3 }')

        checked=$((checked + 1))

        skip=""
        [ "$image" = "scratch" ] && skip=1
        # A reference to a stage declared earlier in this same file.
        case " $stages " in
            *" $image "*) skip=1 ;;
        esac

        if [ -z "$skip" ]; then
            case "$image" in
                *@sha256:*) : ;;
                *)
                    annotate "$f" "$lineno" "FROM ${image} is not pinned by digest; use image@sha256:<digest>"
                    printf 'check-dockerfile-pins: %s:%s FROM %s is not pinned by digest\n' "$f" "$lineno" "$image" >&2
                    bad=$((bad + 1))
                    ;;
            esac
        fi

        [ -n "$alias" ] && stages="${stages}${stages:+ }${alias}"
    done < "$f"
done <<< "$files"

if [ "$checked" -eq 0 ]; then
    echo "::error title=No FROM lines found::check-dockerfile-pins inspected files but found no FROM to judge." >&2
    exit 2
fi

if [ "$bad" -gt 0 ]; then
    echo "check-dockerfile-pins: ${bad} unpinned FROM line(s) of ${checked} checked" >&2
    exit 1
fi

echo "check-dockerfile-pins: OK — ${checked} FROM line(s) pinned by digest."
