#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# ONE branch-pattern matcher, for every reader of a branch pattern.
#
# WHY THIS IS A FILE AND NOT THREE COPIES OF A `case`
#
# Three places in this repository decide whether a branch name matches a
# pattern: `check-branch-refs.sh` (does every pattern under `.github/`
# match something that exists), `check-missing-runs.sh` (whose commits must
# be evidenced) and `purge-workflow-runs.sh` (whose run records are spared).
# The last two are the pair that `.github/gate-branch-scope.env` exists to
# keep from disagreeing: if the purge's idea of the population is smaller
# than the detector's, the purge deletes evidence the detector then demands
# and the detector goes red naming a cause that never happened. A second
# enumeration that must agree with the first is the defect, not the fix —
# and a matcher copied into two scripts is exactly that.
#
# WHY NOT THE SHELL'S OWN GLOB
#
# `case "$b" in 2.*)` is one character away and wrong in two directions.
# The shell's `*` matches `/`, so `2.*` would also match `2.0/wip`, which
# GitHub's `branches: ['2.*']` filter does not; and the shell expands an
# unquoted pattern against the CURRENT DIRECTORY, so a word list carrying
# `2.*` in a loop silently becomes a list of filenames. Both scripts'
# headers already record that second failure, measured.
#
# THE SEMANTICS, which are GitHub's filter-pattern semantics for the subset
# this repository uses:
#
#   **   any sequence, including `/`
#   *    any sequence NOT containing `/`
#   ?    exactly one character, not `/`
#   anything else is literal, `.` included
#
# AND EVERYTHING ELSE REFUSES. GitHub also gives `+`, `!`, `[` and `\`
# meanings in a filter pattern. They are not implemented here, and a
# pattern using one is REFUSED (rc 2) rather than matched approximately:
# an approximate match is a population nobody wrote down, and every caller
# of this file is deciding what gets tested, demanded or deleted. The
# repository uses `2.*` and literal names today, so the price of refusing
# is a diagnosis the day somebody writes something cleverer.
#
# Sourced, not executed: `. "$(dirname "$0")/branch-glob.sh"`.

# Is this word a pattern at all, or a literal branch name?
# A literal is required to EXIST; a pattern is required to MATCH something.
# The two are different obligations, so the callers have to be able to tell
# them apart before asking anything else.
branch_glob_is_pattern() { # <word>
    case "$1" in
        *'*'*|*'?'*|*'['*|*'+'*|*'!'*|*'\'*) return 0 ;;
        *) return 1 ;;
    esac
}

# Does this word LIST contain a pattern at all?
#
# The two gate-scope readers fetch the repository's branch listing only when
# there is something to expand, so a literal-only scope costs no API call and
# behaves exactly as it did before patterns existed. That "is there anything
# to expand" test has to be the SAME predicate as branch_glob_is_pattern, or
# the two disagree in the gap: `2.0.0+` carries no `*` and no `?`, so a
# grep for those two characters calls it a literal and queries a branch by
# that name, while branch_glob_match refuses it as unimplemented syntax. One
# word, two answers, and the looser one decides.
#
# `set -f` is saved and restored here for the same reason it is in
# branch_glob_expand_list: the words being split may themselves be patterns,
# and an unquoted expansion would judge the working directory.
branch_glob_list_has_pattern() { # <words>
    local w reset='' found=1
    case "$-" in *f*) ;; *) reset=1; set -f ;; esac
    for w in $1; do
        [ -n "$w" ] || continue
        if branch_glob_is_pattern "$w"; then found=0; break; fi
    done
    [ -n "$reset" ] && set +f
    return "$found"
}

# Does <branch> match <pattern>?
# rc 0 match, rc 1 no match, rc 2 the pattern uses syntax this does not
# implement (the caller must refuse, never fall back to a looser test).
branch_glob_match() { # <pattern> <branch>
    local p="$1" b="$2" re='' i=0 c

    [ -n "$p" ] || return 2
    case "$p" in
        *'['*|*']'*|*'+'*|*'!'*|*'\'*) return 2 ;;
    esac

    while [ "$i" -lt "${#p}" ]; do
        c="${p:i:1}"
        case "$c" in
            '*')
                if [ "${p:i:2}" = '**' ]; then
                    re+='.*'; i=$((i + 2)); continue
                fi
                re+='[^/]*'; i=$((i + 1)) ;;
            '?')
                re+='[^/]'; i=$((i + 1)) ;;
            # Escaped one at a time rather than through a class, because a
            # character class here would itself need escaping and the list
            # is short. `/` and `-` are literal in an ERE and are left
            # alone deliberately.
            '.'|'^'|'$'|'('|')'|'|'|'{'|'}')
                re+="\\${c}"; i=$((i + 1)) ;;
            *)
                re+="$c"; i=$((i + 1)) ;;
        esac
    done

    [[ "$b" =~ ^${re}$ ]]
}

# A WHOLE WORD LIST, resolved to branch names. This is the function the two
# gate-scope readers call, and it is one function rather than a loop in each
# of them for the reason `.github/gate-branch-scope.env` exists at all: the
# detector's population and the purge's must be the same one, and a rule
# written twice is a rule that will be corrected once.
#
# On stdout: the matched branch names, space separated, duplicates removed,
# in the order the words were written. rc 1 with a diagnosis on stderr when
# ANY word resolves to nothing — a word that matches nothing silently
# narrows the population, which is the direction that deletes evidence.
#
# Literals are passed through UNCHECKED. That is deliberate and it is the
# division of labour with `check-branch-refs.sh`, which is the gate that
# asks whether every name written under `.github/` still exists. Here a dead
# literal keeps its old behaviour — the commit query fails and the caller
# reports UNKNOWN, never clean.
branch_glob_expand_list() { # <words> <heads, newline separated>
    local words="$1" heads="$2" w hits out='' bad='' reset=''
    case "$-" in *f*) ;; *) reset=1; set -f ;; esac
    for w in $words; do
        [ -n "$w" ] || continue
        if ! branch_glob_is_pattern "$w"; then
            out="${out}${w} "
            continue
        fi
        hits=$(branch_glob_expand "$w" "$heads")
        case $? in
            0) out="${out}$(printf '%s' "$hits" | tr '\n' ' ') " ;;
            2) bad="${bad}'${w}' uses filter syntax branch-glob.sh does not implement (+, !, [ ] or a backslash escape). " ;;
            *) bad="${bad}'${w}' matches none of the branches that exist. " ;;
        esac
    done
    [ -n "$reset" ] && set +f
    if [ -n "$bad" ]; then
        echo "branch scope cannot be resolved: ${bad}Branches that exist: $(printf '%s' "$heads" | tr '\n' ' ')" >&2
        return 1
    fi
    # Duplicates: `dev 2.*` and `dev` name the same branch twice the day a
    # branch called `dev` also matches a pattern. Harmless to a reader,
    # doubled work and a doubled count for a caller.
    printf '%s' "$out" | tr ' ' '\n' | grep -v '^$' | awk '!seen[$0]++' | tr '\n' ' ' | sed 's/ $//'
    return 0
}

# Every branch in <heads> that matches <pattern>, one per line. Empty
# output means NOTHING matched, which is the answer every caller has to
# fail on rather than treat as an empty population.
# rc 2 propagates an unsupported pattern.
branch_glob_expand() { # <pattern> <heads, newline separated>
    local p="$1" heads="$2" b rc found=0
    while IFS= read -r b; do
        [ -n "$b" ] || continue
        branch_glob_match "$p" "$b"; rc=$?
        [ "$rc" -eq 2 ] && return 2
        [ "$rc" -eq 0 ] && { printf '%s\n' "$b"; found=1; }
    done <<EOF
$heads
EOF
    [ "$found" -eq 1 ]
}
