#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Shared gate library (#744): one collation, one refusal, one subject
# discovery. Sourced as the first statement after `set` in every
# scripts/check-*.sh; scripts/test-gatelib.sh holds the contract.
#
#   gate_refuse <reason...>         exit 2 with "::error title=<gate> cannot judge::"
#   gate_subjects [--shallow] [--may-be-empty] <array> <class> [<dir>]
#   gate_classes                    print the class table

# The issue's measured pair (#744): check-go-pins.sh sorts before
# check-good-first-issues.sh in C and after it in de_DE.UTF-8, and comm
# reads the order its own locale produces. CI runs C.UTF-8, whose order
# is C's for these names, so C here is CI's answer on every machine.
gate_collation() {
    export LC_ALL=C
    unset LANGUAGE
}
gate_collation

GATE_NAME="${0##*/}"
GATE_NAME="${GATE_NAME%.sh}"

# GATE_TITLE keeps a gate's established annotation title where its
# self-test or a reader already matches on it (#744).
gate_refuse() {
    printf '::error title=%s::%s\n' "${GATE_TITLE:-$GATE_NAME cannot judge}" "$*" >&2
    exit 2
}

# One class per line: name, then git pathspecs. Exceptions are the
# exclude specs on the same line (#744); testdata holds fixtures, not
# subjects, except for the class that names it.
GATE_CLASSES='
go          :(glob)**/*.go :(exclude,glob)**/testdata/**
go-src      :(glob)**/*.go :(exclude,glob)**/*_test.go :(exclude,glob)**/testdata/**
go-test     :(glob)**/*_test.go :(exclude,glob)**/testdata/**
md          :(glob)**/*.md :(exclude,glob)**/testdata/**
docs        :(glob)README.md :(glob)docs/*.md
sh          :(glob)**/*.sh
gates       :(glob)check-*.sh
workflows   :(glob)*.yml :(glob)*.yaml
dockerfile  :(glob)**/Dockerfile :(glob)**/Dockerfile.* :(glob)**/*.Dockerfile
manifest    :(glob)*/manifest.json
'

gate_classes() { printf '%s\n' "$GATE_CLASSES" | sed '/^$/d'; }

# Git's view of the work tree, tracked plus untracked-not-ignored, is the
# one discovery (#454, #743): a fresh CI checkout and a working clone
# answer alike, and ignored build output or nested clones are never read.
# Paths come back as "<dir>/<path>" when <dir> is given, as find printed.
# The array is filled in the caller's shell, so a refusal here ends the
# gate; a $(...) or <(...) wrapper would lose it.
gate_subjects() {
    local _gs_shallow=0 _gs_empty_ok=0
    while :; do
        case "${1:-}" in
            --shallow) _gs_shallow=1; shift ;;
            --may-be-empty) _gs_empty_ok=1; shift ;;
            *) break ;;
        esac
    done
    if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
        gate_refuse "gate_subjects: want <array> <class> [<dir>], got $*"
    fi
    local -n _gs_out="$1"
    local _gs_class="$2" _gs_dir="${3:-}" _gs_line _gs_p _gs_pre=""
    local -a _gs_spec=() _gs_raw=()
    _gs_line="$(gate_classes | awk -v c="$_gs_class" '$1 == c')"
    [ -n "$_gs_line" ] || gate_refuse "unknown subject class '$_gs_class'"
    read -r -a _gs_spec <<< "${_gs_line#* }"
    if [ -n "$_gs_dir" ]; then
        [ -d "$_gs_dir" ] || gate_refuse "$_gs_dir is not a directory"
        _gs_pre="${_gs_dir%/}/"
    fi
    git -C "${_gs_dir:-.}" rev-parse --is-inside-work-tree >/dev/null 2>&1 \
        || gate_refuse "${_gs_dir:-$PWD} is not in a git work tree; subjects are discovered through git"
    mapfile -d '' -t _gs_raw < <(git -C "${_gs_dir:-.}" ls-files -z --cached --others --exclude-standard -- "${_gs_spec[@]}"; printf 'rc=%s\0' "$?")
    [ "${_gs_raw[-1]}" = rc=0 ] || gate_refuse "git ls-files failed in ${_gs_dir:-$PWD} (${_gs_raw[-1]})"
    unset '_gs_raw[-1]'
    _gs_out=()
    for _gs_p in ${_gs_raw[@]+"${_gs_raw[@]}"}; do
        [ "$_gs_shallow" -eq 0 ] || [ "${_gs_p#*/}" = "$_gs_p" ] || continue
        [ -e "$_gs_pre$_gs_p" ] || continue
        _gs_out+=("$_gs_pre$_gs_p")
    done
    if [ "${#_gs_out[@]}" -gt 1 ]; then
        mapfile -t _gs_out < <(printf '%s\n' "${_gs_out[@]}" | sort -u)
    fi
    [ "${#_gs_out[@]}" -gt 0 ] || [ "$_gs_empty_ok" -eq 1 ] \
        || gate_refuse "no '$_gs_class' file under ${_gs_dir:-$PWD}; a pass here would have read nothing"
}
