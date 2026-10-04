#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Every gate states when it should stop existing (#749, part of #733).
#
# A gate header says why the gate exists; nothing said when that reason
# would be gone, so gates were only ever added. Each gate now carries
#
#   # Expires-when: <condition, citing the issue or PR it derives from>
#   #   <optional continuation lines, indented two or more spaces>
#
# in its leading comment block, the lines before the first line of code.
# A gate that guards a standing invariant says "never" and why.
#
# POPULATION: gatelib's `gates` class (check-*.sh) and its `named-gates`
# class (*-gate.sh, which holds govulncheck-gate.sh and
# integration-run-gate.sh: both decide a CI verdict, both rest on a
# premise from their issue). The rule is the file name, so a verdict
# script named otherwise (coverage-ratchet.sh, engine-floor.sh) is
# outside it; renaming a gate out of both patterns removes it silently.
#
# FINDINGS, per gate: the key is missing, or appears only below the
# header; a near-miss spelling of the key; more than one key line; the
# text is empty, a placeholder (TODO, TBD, n/a, a bare "never"), shorter
# than six words, cites no #N, or runs past four lines; two gates carry
# the same text, which is boilerplate, not a condition.
#
# WHAT IT CANNOT SEE: whether a condition is true or already met. That
# is a reader's judgement; --list prints every line for that reader and
# never turns red because a condition might hold.
#
# Expires-when: never: a gate added without a stated end is the growth
#   #749 measured (gates 9 to 47 in twenty days, none retired); this
#   ends only if gates stop being added as scripts at all.
#
# Usage: check-gate-expiry.sh [--list] [<scripts dir>]
#   --list  print every gate and its Expires-when as a markdown table,
#           to stdout and to $GITHUB_STEP_SUMMARY when that is set
# Exit:  0 every gate states one, 1 a finding, 2 cannot check.

set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

LIST=0
if [ "${1:-}" = "--list" ]; then
    LIST=1
    shift
fi
[ "$#" -le 1 ] || gate_refuse "usage: check-gate-expiry.sh [--list] [<scripts dir>]"
DIR="${1:-$(dirname "${BASH_SOURCE[0]}")}"
[ -d "$DIR" ] || gate_refuse "$DIR is not a directory"

gate_subjects --shallow checks gates "$DIR"
gate_subjects --shallow --may-be-empty named named-gates "$DIR"
population=("${checks[@]}" ${named[@]+"${named[@]}"})

# extract FILE: one record per key line, near miss and late key, tab-separated.
#   KEY <line> <n lines> <text>   NEAR <line> <raw>   LATE <line>
extract() {
    awk '
        function flush() { if (key) { printf "KEY\t%d\t%d\t%s\n", kl, kn, kt; key = 0 } }
        NR == 1 { next }
        hdr && !/^#/ && !/^[[:space:]]*$/ { flush(); hdr = 0 }
        hdr && key && /^#[[:space:]][[:space:]]+[^[:space:]]/ {
            t = $0; sub(/^#[[:space:]]+/, "", t); kt = kt " " t; kn++; next
        }
        hdr { flush() }
        hdr && /^# Expires-when:/ {
            key = 1; kl = NR; kn = 1; kt = $0; sub(/^# Expires-when:[[:space:]]*/, "", kt); next
        }
        hdr && tolower($0) ~ /^#[[:space:]]*expires?[-_ ]*when/ { printf "NEAR\t%d\t%s\n", NR, $0; next }
        !hdr && /^[[:space:]]*#[[:space:]]*Expires-when:/ { printf "LATE\t%d\n", NR }
        END { flush() }
    ' hdr=1 "$1"
}

declare -A text_of=() owner_of=()
findings=0
finding() {
    echo "FAIL  $1: $2"
    findings=$((findings + 1))
}

for f in "${population[@]}"; do
    [ -r "$f" ] || gate_refuse "cannot read $f"
    name="${f##*/}"
    records="$(extract "$f")" || gate_refuse "awk failed on $f"
    keys=0 late=""
    text="" lines=0 line=0
    while IFS=$'\t' read -r kind l a b; do
        case "$kind" in
            KEY) keys=$((keys + 1)); line="$l"; lines="$a"; text="$b" ;;
            NEAR) finding "$name:$l" "near-miss spelling of the key, want '# Expires-when:' at column 0: $a" ;;
            LATE) late="$l" ;;
        esac
    done <<< "$records"
    if [ "$keys" -eq 0 ]; then
        if [ -n "$late" ]; then
            finding "$name:$late" "Expires-when is below the header; it belongs in the leading comment block"
        else
            finding "$name" "no '# Expires-when:' line in the header"
        fi
        continue
    fi
    if [ "$keys" -gt 1 ]; then
        finding "$name" "$keys Expires-when lines; a gate has one end condition"
        continue
    fi
    text="$(printf '%s' "$text" | tr -s '[:space:]' ' ' | sed 's/^ //; s/ $//')"
    if [ -z "$text" ]; then
        finding "$name:$line" "Expires-when is empty"
        continue
    fi
    bare="$(printf '%s' "$text" | sed -E 's/\(?#[0-9]+\)?//g' | tr '[:upper:]' '[:lower:]' | tr -d '[:punct:]' | tr -s ' ' | sed 's/^ //; s/ $//')"
    words="$(printf '%s' "$bare" | wc -w)"
    if [[ $bare =~ ^(todo|tbd|fixme|xxx|n\ ?a|none|unknown|never|later)$ ]] \
        || [[ $bare =~ ^(todo|tbd|fixme|xxx)(\ |$) ]]; then
        finding "$name:$line" "Expires-when is a placeholder, not a condition: $text"
        continue
    fi
    if [ "$words" -lt 6 ]; then
        finding "$name:$line" "Expires-when has $words words; name the condition and its reason: $text"
    fi
    if ! [[ $text =~ \#[0-9]+ ]]; then
        finding "$name:$line" "Expires-when cites no issue or PR (#N) it derives from"
    fi
    if [ "$lines" -gt 4 ]; then
        finding "$name:$line" "Expires-when runs $lines lines; at most four, the reasons live above it"
    fi
    norm="$(printf '%s' "$text" | tr '[:upper:]' '[:lower:]')"
    if [ -n "${owner_of[$norm]:-}" ]; then
        finding "$name:$line" "same Expires-when as ${owner_of[$norm]}; boilerplate is not a condition"
    else
        owner_of[$norm]="$name"
    fi
    text_of[$name]="$text"
done

if [ "$LIST" -eq 1 ]; then
    table="$(
        echo "## Gate expiry: ${#population[@]} gates"
        echo
        echo "Re-read each condition; a gate whose condition holds is a candidate for removal (#749)."
        echo
        echo "| Gate | Expires when |"
        echo "|---|---|"
        for f in "${population[@]}"; do
            name="${f##*/}"
            t="${text_of[$name]:-(missing or rejected, see the findings)}"
            printf '| `%s` | %s |\n' "$name" "${t//|/\\|}"
        done
    )"
    printf '%s\n' "$table"
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
        printf '%s\n' "$table" >> "$GITHUB_STEP_SUMMARY" || gate_refuse "cannot write the job summary"
    fi
fi

if [ "$findings" -gt 0 ]; then
    echo "FAIL  $findings finding(s) over ${#population[@]} gates; every gate states when it expires (#749)" >&2
    exit 1
fi
echo "gate-expiry check passed: ${#population[@]} gates state when they expire"
exit 0
