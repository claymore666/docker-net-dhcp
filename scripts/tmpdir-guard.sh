#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# ONE guarded temp directory for every gate self-test (#963).
#
# SOURCED, not run:
#
#     HERE="$(cd "$(dirname "$0")" && pwd)"
#     # shellcheck source=scripts/tmpdir-guard.sh
#     . "$HERE/tmpdir-guard.sh"
#     guarded_tmpdir dir                    # dir=/tmp/tmp.AbCdEf
#     guarded_tmpdir case "$dir/caseXXXXXX" # any mktemp -d arguments
#
# WHY THIS IS A FILE AND NOT THIRTY COPIES OF `dir=$(mktemp -d)`
#
# A gate self-test builds its subject in a throwaway repository and
# then runs the gate from inside it. The shape that shipped, in
# fifteen files:
#
#     dir=$(mktemp -d)
#     (
#         cd "$dir" || exit 2
#         git init -q .
#         git config user.email t@t; git config user.name t
#         ...
#     )
#
# That `|| exit 2` reads like a guard and is not one. `mktemp -d`
# writes nothing to stdout when it fails, so `dir` is the empty
# string, and bash's `cd ""` RETURNS 0 AND STAYS WHERE IT IS. The
# fixture is then built in whatever directory the self-test was
# started from, which is the checkout. Measured 2026-09-11: /tmp on
# the session box reached 100 percent inodes with 3161 leaked
# `tmp.*` directories, `mktemp -d` began returning nothing, and a
# self-test ran `git init`, an identity change, a go.mod replacement
# and a file deletion inside a working clone. Two commits authored
# `t <t@t>` reached a pull request before anyone noticed.
#
# So the two halves are one file:
#
#   REFUSE. An empty path, a path that is not a directory, a path
#   that is the repository root, inside it, above it, or `/` is a
#   refusal with exit 2, not a value to carry. This fails in one
#   direction on purpose: it never rewrites a bad path into a good
#   one, because a self-test that quietly moved its fixture
#   somewhere else is the defect wearing the fix's clothes. A
#   refused path is also never removed. The whole point of the
#   refusal is that the path is not one this file trusts, and the
#   worst case it refuses is the checkout.
#
#   REMOVE. Every directory handed out is removed by an EXIT trap,
#   so a suite that dies on its third case leaves nothing behind.
#   The explicit `rm -rf "$dir"` at the end of a case is still
#   correct and still there; it just is not the only thing standing
#   between a failing run and a full filesystem.
#
# THE REFUSAL HAS TO REACH THE TOP-LEVEL SHELL. Self-tests build
# fixtures inside command substitutions (`check "$(mkws)"`), where
# `exit` ends the subshell and the caller carries on with an empty
# string -- which is the original defect one door along. A refusal
# raised in a subshell therefore signals the script's own process
# as well, and the handler for that signal exits 2 rather than
# letting the shell die by it: a script killed by a signal is
# reported as "Terminated" by whatever waited on it, and a refusal
# should read as a refusal.
#
# THE REGISTRY IS A FILE, NOT AN ARRAY, FOR THE SAME REASON. A
# fixture built inside `$(mkws)` is created by a SUBSHELL, and a
# subshell's variables die with it while its directory does not.
# Bash also does not run an inherited EXIT trap when a subshell
# ends, so an array-backed sweep either loses those directories or,
# armed inside the subshell, deletes them the instant the command
# substitution returns. Both were measured here. Every handed-out
# path is therefore appended to one file named after the top-level
# shell's `$$`, which every subshell shares, and only the top-level
# shell sweeps it.
#
# WHAT IT DOES NOT COVER. It judges the path, not what the caller
# does with it: a self-test that ignores the variable and cds
# somewhere else is invisible here. It also owns the EXIT trap. An
# EXIT trap already installed when this file is sourced is
# preserved and still runs; one installed afterwards is re-composed
# at the next call and lost if there is no next call.
#
# Env: TMPDIR_GUARD_ROOT  the tree a temp path must stay out of
#                         (default: the repository this file lives
#                         in) -- the seam the self-test drives.

# One registry per top-level shell. `$$` is the top-level shell's pid in
# every subshell it forks, which is exactly the scope the sweep needs.
_tmpdir_guard_registry="${TMPDIR:-/tmp}/.tmpdir-guard.$$"
: > "$_tmpdir_guard_registry" 2>/dev/null || _tmpdir_guard_registry=

_tmpdir_guard_refuse() {
    printf '::error title=Unsafe temp directory::%s: %s\n' \
        "$(basename -- "${0:-tmpdir-guard}")" "$1" >&2
    # A subshell's `exit` is invisible to the caller that is about to
    # `cd` into the empty string, so end the whole script.
    if [ "${BASHPID:-$$}" != "$$" ]; then
        kill -s TERM "$$" 2>/dev/null
    fi
    exit 2
}

# The signal a subshell refusal raises. Exiting from the handler keeps
# the status at 2 and keeps the EXIT sweep running.
_tmpdir_guard_terminated() {
    exit 2
}

_tmpdir_guard_sweep() {
    # Only the shell that owns the registry sweeps it. A subshell that
    # re-armed the trap must not delete its parent's fixtures.
    [ "${BASHPID:-$$}" = "$$" ] || return 0
    [ -n "$_tmpdir_guard_registry" ] || return 0
    [ -f "$_tmpdir_guard_registry" ] || return 0
    local d
    while IFS= read -r d; do
        [ -n "$d" ] && rm -rf -- "$d"
    done < "$_tmpdir_guard_registry"
    rm -f -- "$_tmpdir_guard_registry"
}

# Install the EXIT trap, keeping whatever the caller had. Re-checked on
# every call so a trap installed after the first directory is composed
# rather than silently dropped.
_tmpdir_guard_arm() {
    local prev cmd
    local -a parts
    prev="$(trap -p EXIT)"
    case "$prev" in
        *_tmpdir_guard_sweep*) return 0 ;;
    esac
    cmd=
    if [ -n "$prev" ]; then
        # `trap -p EXIT` prints exactly: trap -- 'CMD' EXIT
        eval "parts=($prev)"
        cmd="${parts[2]:-}"
    fi
    # shellcheck disable=SC2064
    trap "_tmpdir_guard_sweep${cmd:+; $cmd}" EXIT
}

_tmpdir_guard_root() {
    local root="${TMPDIR_GUARD_ROOT:-}"
    if [ -z "$root" ]; then
        root="$(dirname -- "${BASH_SOURCE[0]}")/.."
    fi
    (cd "$root" 2>/dev/null && pwd -P)
}

_tmpdir_guard_check() {
    local d="$1" abs root
    [ -n "$d" ] || _tmpdir_guard_refuse \
        "mktemp -d produced an empty path. bash's \`cd \"\"\` succeeds and stays put, so the fixture would have been built in $(pwd -P)."
    [ -d "$d" ] || _tmpdir_guard_refuse \
        "mktemp -d produced '$d', which is not a directory."
    abs="$(cd "$d" 2>/dev/null && pwd -P)" || abs=
    [ -n "$abs" ] || _tmpdir_guard_refuse \
        "cannot resolve the temp path '$d'."
    [ "$abs" != "/" ] || _tmpdir_guard_refuse \
        "the temp path resolves to /."
    case "$abs" in
        *$'\n'*)
            _tmpdir_guard_refuse "the temp path contains a newline." ;;
    esac
    root="$(_tmpdir_guard_root)"
    [ -n "$root" ] || return 0
    case "$abs" in
        "$root")
            _tmpdir_guard_refuse \
                "the temp path resolves to the repository root ($root). A fixture built there rewrites the checkout." ;;
        "$root"/*)
            _tmpdir_guard_refuse \
                "the temp path '$abs' is inside the repository ($root)." ;;
    esac
    case "$root" in
        "$abs"/*)
            _tmpdir_guard_refuse \
                "the temp path '$abs' contains the repository ($root)." ;;
    esac
}

# guarded_tmpdir VAR [mktemp -d arguments...]
guarded_tmpdir() {
    local __tg_var="${1:-}"
    [ -n "$__tg_var" ] || _tmpdir_guard_refuse \
        "guarded_tmpdir needs the name of a variable to assign."
    shift
    local __tg_dir
    __tg_dir="$(mktemp -d "$@" 2>/dev/null)" || __tg_dir=
    _tmpdir_guard_check "$__tg_dir"
    _tmpdir_guard_arm
    if [ -n "$_tmpdir_guard_registry" ]; then
        printf '%s\n' "$__tg_dir" >> "$_tmpdir_guard_registry"
    fi
    printf -v "$__tg_var" '%s' "$__tg_dir"
}

# Armed here, at source time, so the traps belong to the top-level shell
# and not to whichever subshell happens to ask for the first directory.
_tmpdir_guard_arm
[ -n "$(trap -p TERM)" ] || trap '_tmpdir_guard_terminated' TERM
