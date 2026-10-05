#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only
# The capability matrix's cell list and its reconciliation against the
# table in docs/reference.md (#690).
#
# Usage: capability-matrix.sh --cells-json
#        capability-matrix.sh --columns
#        capability-matrix.sh --strip <capability> <config.json>
#        capability-matrix.sh --reconcile [--strict] <rows-dir>
# Exit:  0 agrees, 1 disagrees, 2 cannot check.
set -euo pipefail

ROOT="${CAP_MATRIX_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
CONFIG="${CAP_MATRIX_CONFIG:-$ROOT/config.json}"
DOC="${CAP_MATRIX_DOC:-$ROOT/docs/reference.md}"
# One declaration of the columns: capability-cell.sh prints these keys and
# the docs table carries them as its header, in this order (#690).
COLUMNS=(enables capeff mount bridge macvlan dns user dns_user renew renew_user restart)
SCENARIOS=(bridge macvlan dns user dns_user renew renew_user restart)

die() { echo "::error title=capability matrix::$*" >&2; exit 2; }

capabilities() {
    local caps
    caps="$(jq -r '.linux.capabilities[]?' "$CONFIG")" || die "cannot read $CONFIG"
    [ -n "$caps" ] || die "$CONFIG declares no capabilities"
    printf '%s\n' "$caps"
}

cells() { echo none; capabilities; }

allowed() {
    case "$1" in
        enables) [[ "$2" =~ ^(yes|no)$ ]] ;;
        capeff) [[ "$2" =~ ^(n/a|held|dropped)$ ]] ;;
        mount) [[ "$2" =~ ^[a-z,]+$ ]] ;;
        *) [[ "$2" =~ ^(pass|fail)$ ]] ;;
    esac
}

# table_rows prints "removed|col1|col2|..." per table row, backticks and
# padding stripped; the header line is checked against COLUMNS (#690).
table_rows() {
    local block header want
    block="$(sed -n '/<!-- capability-matrix: begin -->/,/<!-- capability-matrix: end -->/p' "$DOC")"
    [ -n "$block" ] || die "$DOC carries no capability-matrix block"
    block="$(printf '%s\n' "$block" | grep '^|' | tr -d '` ' | sed 's/^|//; s/|$//')"
    header="$(printf '%s\n' "$block" | sed -n 1p)"
    want="removed$(printf '|%s' "${COLUMNS[@]}")"
    [ "$header" = "$want" ] || die "table header '$header' is not '$want'"
    printf '%s\n' "$block" | sed -n '3,$p'
}

row_value() { printf '%s\n' "$1" | tr ' ' '\n' | sed -n "s/^$2=//p"; }

reconcile() {
    local strict=0 dir rc=0 cell table measured line tv mv i n
    if [ "${1:-}" = --strict ]; then strict=1; shift; fi
    dir="${1:-}"
    [ -d "$dir" ] || die "rows directory '$dir' does not exist"
    table="$(table_rows)"
    mapfile -t all_cells < <(cells)
    printf '| removed |%s\n|---|' "$(printf ' %s |' "${COLUMNS[@]}")"
    printf -- '---|%.0s' "${COLUMNS[@]}"; echo
    for cell in "${all_cells[@]}"; do
        measured="$(find "$dir" -name '*.row' -exec cat {} + 2>/dev/null | grep -E "^CAP_MATRIX_ROW removed=$cell( |$)" || true)"
        n="$(printf '%s' "$measured" | grep -c . || true)"
        if [ "$n" -ne 1 ]; then
            echo "::error title=capability matrix::cell $cell produced $n rows, want 1"
            rc=1; continue
        fi
        if [ "$(row_value "$measured" result)" != ok ]; then
            echo "::error title=capability matrix::cell $cell result=$(row_value "$measured" result): no measurement"
            rc=1; continue
        fi
        line="$(printf '%s\n' "$table" | awk -F'|' -v c="$cell" '$1 == c')"
        if [ "$(printf '%s' "$line" | grep -c . || true)" -ne 1 ]; then
            echo "::error title=capability matrix::docs table has no single row for $cell"
            rc=1
        fi
        printf '| %s |' "$cell"
        i=2
        for col in "${COLUMNS[@]}"; do
            mv="$(row_value "$measured" "$col")"
            tv="$(printf '%s' "$line" | cut -d'|' -f"$i")"
            i=$((i + 1))
            printf ' %s |' "$mv"
            if ! allowed "$col" "$mv"; then
                echo "::error title=capability matrix::cell $cell $col='$mv' is not a measured value" >&2
                rc=1
            elif [ "$tv" = '?' ]; then
                if [ "$strict" -eq 1 ]; then
                    echo "::error title=capability matrix::cell $cell $col is unmeasured in the docs table, measured $mv" >&2
                    rc=1
                else
                    echo "::warning title=capability matrix::cell $cell $col is unmeasured in the docs table, measured $mv" >&2
                fi
            elif [ -n "$line" ] && [ "$tv" != "$mv" ]; then
                echo "::error title=capability matrix::cell $cell $col measured $mv, docs table says $tv" >&2
                rc=1
            fi
        done
        echo
        if [ "$cell" = none ]; then
            [ "$(row_value "$measured" enables)" = yes ] || { echo "::error title=capability matrix::the full set did not enable" >&2; rc=1; }
            for col in "${SCENARIOS[@]}"; do
                [ "$(row_value "$measured" "$col")" = pass ] \
                    || { echo "::error title=capability matrix::the full set failed $col" >&2; rc=1; }
            done
        fi
    done
    while IFS='|' read -r cell _; do
        [ -z "$cell" ] && continue
        printf '%s\n' "${all_cells[@]}" | grep -qxF "$cell" \
            || { echo "::error title=capability matrix::docs table row $cell names no cell of config.json" >&2; rc=1; }
    done <<<"$table"
    return "$rc"
}

case "${1:-}" in
    --cells-json) cells | jq -R . | jq -cs . ;;
    --columns) printf '%s\n' "${COLUMNS[@]}" ;;
    --strip)
        [ $# -eq 3 ] || die "--strip <capability> <config.json>"
        capabilities | grep -qxF "$2" || die "$2 is not requested by $CONFIG"
        jq --arg c "$2" '.linux.capabilities -= [$c]' "$3" ;;
    --reconcile) shift; reconcile "$@" ;;
    *) die "usage: --cells-json | --columns | --strip <capability> <config.json> | --reconcile [--strict] <rows-dir>" ;;
esac
