#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The declared engine floor, and the reconciliation of it against what
# the engine matrix measured (#670).
#
# ONE DECLARATION, READ TWICE. The floor is a Go constant, because the
# plugin refuses to start below it and a refusal cannot be driven from a
# shell file. Everything else that needs the number — this lane, the
# documentation gate, a person asking what we support — reads it back
# out of that one line rather than carrying a second copy. The number
# appears in README.md and docs/reference.md as prose, and
# scripts/check-docs-drift.sh is what holds those to this. The row list
# is the same shape: .github/engine-rows.txt declares it, the workflow
# builds its matrix from it, and this gate demands a row for every line.
#
# WHAT `--reconcile` ACTUALLY CHECKS, and it is not "the rows passed".
# A matrix of nine green rows says nothing on its own: the list could
# simply have started above the floor, or the rows that would have said
# otherwise could have died before uploading anything. The rules are
#
#   1. the list probes BELOW the declared floor. A list whose lowest row
#      is the floor itself has measured the list somebody typed.
#   2. every declared row is present, exactly once, and reached a
#      verdict. A row that produced no verdict line is a job that died,
#      and an absent measurement read as a pass is the failure this
#      whole issue is about. A row nobody declared is the same
#      disagreement seen from the other side.
#   3. the passing rows are UPWARD CLOSED. If 24 passes and 25 does not,
#      there is no floor to publish — there is a bug on 25, and calling
#      the lowest passing row the floor would bury it.
#   4. the lowest passing row IS the declared floor. Not "at or above":
#      a floor declared above what the plugin demonstrably works on
#      turns away installs that would have worked, and one declared
#      below it is a promise nothing backs.
#
# `unavailable` IS NOT `fail`, and keeping them apart is the whole
# reason the cell has a control step. A `fail` row is evidence about the
# plugin: that engine ran an ordinary container and then something of
# ours did not work. An `unavailable` row is evidence about the rig: the
# engine never started a container here at all, so nothing was asked of
# the plugin. Below the floor the difference decides what the floor IS,
# and this gate says which of the two it measured rather than leaving a
# reader to assume the stronger one.
#
# THE LIMIT OF RULE 4 ONCE THE REFUSAL SHIPS, stated here rather than
# discovered by someone trusting the lane too far. Below the declared
# floor the plugin refuses to start, so a row below the floor fails
# BECAUSE of that refusal as well as for whatever reason the floor was
# set. The lane can therefore still detect a floor that is too LOW (a
# row at the floor that stops passing), and cannot detect one that is
# too HIGH. Re-measuring means lowering the constant on a branch and
# letting this lane run: pkg/plugin/engine_floor.go is in
# engine-matrix.yml's `paths:` for exactly that reason.
#
# Usage:
#   scripts/engine-floor.sh --print
#   scripts/engine-floor.sh --rows-json
#   scripts/engine-floor.sh --reconcile <dir-of-row-files>
# Exit: 0 declared / reconciled, 1 a rule was broken, 2 cannot check.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

# The seams the self-test drives. They are paths, not values: the floor
# still has to be parsed out of a Go declaration and the list out of the
# file the workflow reads, so a test cannot hand this gate a number or a
# row list it never read.
FLOOR_FILE="${ENGINE_FLOOR_FILE:-pkg/plugin/engine_floor.go}"
ROWS_FILE="${ENGINE_ROWS_FILE:-.github/engine-rows.txt}"

# The anchored form of the declaration. It matches the constant and
# nothing else in the file: a comment quoting the name does not have the
# `=` and a string, and a renamed constant matches nothing at all, which
# is a refusal rather than an empty answer (see read_floor).
FLOOR_PATTERN='^const[[:space:]]+MinEngineVersion[[:space:]]*=[[:space:]]*"([0-9]+\.[0-9]+)"$'

read_floor() {
    local line
    [ -r "$FLOOR_FILE" ] || {
        echo "FAIL  $FLOOR_FILE is not readable — nothing declares the engine floor." >&2
        return 2
    }
    line="$(grep -E "$FLOOR_PATTERN" "$FLOOR_FILE")"
    if [ -z "$line" ]; then
        echo "FAIL  no MinEngineVersion declaration in $FLOOR_FILE." >&2
        echo "  Expected exactly: const MinEngineVersion = \"<major>.<minor>\"" >&2
        echo "  A renamed or reformatted constant is a refusal here, never an" >&2
        echo "  empty floor: an empty floor compares below every engine." >&2
        return 2
    fi
    if [ "$(printf '%s\n' "$line" | wc -l)" -ne 1 ]; then
        echo "FAIL  $FLOOR_FILE declares MinEngineVersion more than once." >&2
        return 2
    fi
    printf '%s\n' "$line" | sed -E "s/$FLOOR_PATTERN/\1/"
}

# read_rows prints the declared row tags, one per line. An empty list is
# a refusal: an empty expected set makes every rule below vacuous, which
# is the one verdict this gate must not be able to produce.
read_rows() {
    local out
    [ -r "$ROWS_FILE" ] || {
        echo "FAIL  $ROWS_FILE is not readable — nothing declares the matrix rows." >&2
        return 2
    }
    out="$(sed -e 's/#.*//' -e 's/[[:space:]]//g' "$ROWS_FILE" | grep -v '^$')"
    if [ -z "$out" ]; then
        echo "FAIL  $ROWS_FILE declares no engine rows." >&2
        return 2
    fi
    printf '%s\n' "$out"
}

# key turns a version into a sortable integer. Engine versions are not
# decimals — 20.10 is above 20.9 and below 23.0 — so a numeric
# comparison of the whole string would order them wrongly, and a string
# comparison orders "9" above "20". Anything it cannot read is -1, and
# every caller refuses on -1 rather than ordering against it.
#
# 10# is not decoration: a minor of `09` is an invalid octal literal and
# bash fails the whole arithmetic expression on it, which under `set -u`
# alone is a non-fatal error message and an EMPTY result. That is the
# shape that let a malformed row through this gate with a clean PASS.
key() {
    local v="$1" major minor
    major="${v%%.*}"
    if [ "$v" = "$major" ]; then
        minor=0
    else
        minor="${v#*.}"
        minor="${minor%%.*}"
    fi
    case "$major" in *[!0-9]*|'') echo "-1"; return ;; esac
    case "$minor" in *[!0-9]*|'') echo "-1"; return ;; esac
    echo $(( 10#$major * 1000 + 10#$minor ))
}

# row_key orders a row that did NOT pass. Such a row may never have
# reached a daemon, so it may have no reported version, and its tag is
# all there is to order it by.
#
# A PASSING row is not ordered through here, deliberately. A pass means
# a daemon answered every call the baseline makes, so its version parsed
# — and falling back to the tag there would order the floor by the
# string somebody typed into the matrix instead of by what ran. That is
# the hole the self-test's unorderable-version case pins.
row_key() {
    local engine="$1" tag="$2" k
    k="$(key "$engine")"
    [ "$k" != "-1" ] && { echo "$k"; return; }
    key "$tag"
}

case "${1:-}" in
    --print)
        read_floor || exit 2
        exit 0
        ;;
    --rows-json)
        # The matrix list as JSON, for `fromJSON` in the workflow. Built
        # here and not with a `jq` the runner may not have, and built
        # from read_rows so the workflow's matrix and this gate's
        # expected set cannot come from two different readings.
        rows="$(read_rows)" || exit 2
        printf '['
        sep=""
        while IFS= read -r tag; do
            printf '%s"%s"' "$sep" "$tag"
            sep=","
        done <<< "$rows"
        printf ']\n'
        exit 0
        ;;
    --reconcile) ;;
    *)
        echo "usage: $0 --print | --rows-json | --reconcile <dir>" >&2
        exit 2
        ;;
esac

ROWS_DIR="${2:-}"
[ -n "$ROWS_DIR" ] && [ -d "$ROWS_DIR" ] || {
    echo "FAIL  --reconcile needs a directory of row files; got '${ROWS_DIR:-}'." >&2
    exit 2
}

declared="$(read_floor)" || exit 2
declared_key="$(key "$declared")"
[ "$declared_key" != "-1" ] || {
    echo "FAIL  the declared floor '$declared' cannot be ordered." >&2
    exit 2
}

expected_rows="$(read_rows)" || exit 2
mapfile -t EXPECTED <<< "$expected_rows"

# Rule 1: the list probes below the floor. Checked before any row is
# read, because it is a property of the list alone and a reader who
# reaches the table below should already know the list could have said
# no.
lowest_expected_key=""
for tag in "${EXPECTED[@]}"; do
    k="$(key "$tag")"
    if [ "$k" = "-1" ]; then
        echo "FAIL  $ROWS_FILE declares row '$tag', which is not an orderable engine line." >&2
        exit 2
    fi
    if [ -z "$lowest_expected_key" ] || [ "$k" -lt "$lowest_expected_key" ]; then
        lowest_expected_key="$k"
    fi
done
if [ "$lowest_expected_key" -ge "$declared_key" ]; then
    echo "FAIL  no declared row is below the floor $declared." >&2
    echo "  A matrix whose lowest row is the floor has measured the list, not" >&2
    echo "  the floor. Add the engine line below $declared to $ROWS_FILE." >&2
    exit 1
fi

mapfile -t ROW_LINES < <(find "$ROWS_DIR" -type f -name '*.row' -exec cat {} + 2>/dev/null | grep '^ENGINE_MATRIX_ROW ' | sort -u)

# NON-VACUITY. A reconciliation over zero rows would otherwise print a
# clean pass having read nothing, which is the one verdict this lane
# must never be able to produce (#743's shape). Rule 2 below subsumes
# this, and it stays because an empty directory has a cause worth
# naming separately (the download step, not a cell).
if [ "${#ROW_LINES[@]}" -eq 0 ]; then
    echo "::error title=No rows::$ROWS_DIR carries no ENGINE_MATRIX_ROW line." \
         "This is not a verdict about any engine." >&2
    exit 2
fi

fail=0
declare -a PASSED=() FAILED=() UNAVAILABLE=() SEEN_TAGS=()
# entries are "<tag> <engine>", so the ordering below keys on the
# version the daemon reported rather than on the tag that asked for it.
echo "| engine tag | engine | API | result | first failing step |"
echo "|---|---|---|---|---|"
for line in "${ROW_LINES[@]}"; do
    tag=""; engine=""; api=""; result=""; step=""
    # shellcheck disable=SC2086
    for field in $line; do
        case "$field" in
            tag=*)    tag="${field#tag=}" ;;
            engine=*) engine="${field#engine=}" ;;
            api=*)    api="${field#api=}" ;;
            result=*) result="${field#result=}" ;;
            step=*)   step="${field#step=}" ;;
        esac
    done
    echo "| $tag | $engine | $api | $result | $step |"
    SEEN_TAGS+=("$tag")
    case "$result" in
        pass)        PASSED+=("$tag $engine") ;;
        fail)        FAILED+=("$tag $engine") ;;
        unavailable) UNAVAILABLE+=("$tag $engine") ;;
        *)
            echo "FAIL  row $tag reached no verdict (result='$result')." >&2
            fail=1
            ;;
    esac
done

# Rule 2: the declared rows and the rows that arrived are the same set.
# The domain of this reconciliation is the LIST, not whatever happened
# to be uploaded: a cell that dies before it records its row would
# otherwise shrink the matrix silently, and the row most likely to
# matter is the lowest one.
for tag in "${EXPECTED[@]}"; do
    n=0
    for seen in "${SEEN_TAGS[@]}"; do
        [ "$seen" = "$tag" ] && n=$((n + 1))
    done
    if [ "$n" -eq 0 ]; then
        echo "FAIL  $ROWS_FILE declares engine $tag and no row for it arrived." >&2
        echo "  A missing row is a cell that died, not a smaller matrix." >&2
        fail=1
    elif [ "$n" -gt 1 ]; then
        echo "FAIL  engine $tag reported $n different rows." >&2
        fail=1
    fi
done
for seen in "${SEEN_TAGS[@]}"; do
    found=0
    for tag in "${EXPECTED[@]}"; do
        [ "$seen" = "$tag" ] && found=1
    done
    if [ "$found" -eq 0 ]; then
        echo "FAIL  a row arrived for engine $seen, which $ROWS_FILE does not declare." >&2
        fail=1
    fi
done

[ "$fail" -eq 0 ] || exit 1

if [ "${#PASSED[@]}" -eq 0 ]; then
    echo "FAIL  no row passed. There is no floor to declare." >&2
    exit 1
fi

# Rule 3: upward closure. Anything failing above the lowest passing row
# is a defect on that engine, not a floor.
lowest_pass=""
lowest_key=""
for entry in "${PASSED[@]}"; do
    k="$(key "${entry#* }")"
    if [ "$k" = "-1" ]; then
        echo "FAIL  row '${entry%% *}' reports a version this gate cannot order ('${entry#* }')." >&2
        fail=1
        continue
    fi
    if [ -z "$lowest_key" ] || [ "$k" -lt "$lowest_key" ]; then
        lowest_key="$k"; lowest_pass="${entry#* }"
    fi
done

[ "$fail" -eq 0 ] && [ -n "$lowest_key" ] || exit 1

for entry in "${FAILED[@]+"${FAILED[@]}"}"; do
    k="$(row_key "${entry#* }" "${entry%% *}")"
    [ "$k" = "-1" ] && continue
    if [ "$k" -gt "$lowest_key" ]; then
        echo "FAIL  engine ${entry%% *} failed while $lowest_pass passed below it." >&2
        echo "  The passing rows are not upward closed, so the lowest passing row" >&2
        echo "  is not a floor. Fix ${entry%% *} rather than publishing $lowest_pass." >&2
        fail=1
    fi
done

# An unmeasured row ABOVE the floor is a hole in the claim, not a
# defect on that engine. The published sentence is that every engine
# from the floor up was driven through the baseline, and a row the rig
# could not run leaves that sentence with nothing behind it.
for entry in "${UNAVAILABLE[@]+"${UNAVAILABLE[@]}"}"; do
    k="$(row_key "${entry#* }" "${entry%% *}")"
    [ "$k" = "-1" ] && continue
    if [ "$k" -gt "$lowest_key" ]; then
        echo "FAIL  engine ${entry%% *} was not measured at all while $lowest_pass passed below it." >&2
        echo "  Nothing backs the claim that every engine above the floor works." >&2
        fail=1
    fi
done

[ "$fail" -eq 0 ] || exit 1

# Rule 4: the declared floor is the measured one.
if [ "$declared_key" -ne "$lowest_key" ]; then
    echo "FAIL  declared floor $declared, measured floor $lowest_pass." >&2
    echo "  $FLOOR_FILE and this matrix disagree. One of them is wrong and the" >&2
    echo "  documentation is derived from the first." >&2
    exit 1
fi

# WHICH KIND OF FLOOR THIS IS. The sentence the documentation may use
# is decided here, by what the rows below the floor actually did.
demonstrated=0
for entry in "${FAILED[@]+"${FAILED[@]}"}"; do
    k="$(row_key "${entry#* }" "${entry%% *}")"
    [ "$k" = "-1" ] && continue
    [ "$k" -lt "$lowest_key" ] && demonstrated=1
done

echo
echo "PASS  declared floor $declared is the lowest passing row (${#PASSED[@]} passing," \
     "${#FAILED[@]} failing, ${#UNAVAILABLE[@]} unmeasured)."
if [ "$demonstrated" -eq 1 ]; then
    echo "      A row below the floor ran a container and then failed a plugin step:"
    echo "      the floor is a measured boundary."
else
    echo "      No row below the floor was measurable on this host, so the floor is the"
    echo "      lowest engine MEASURED to work. Nothing here says the plugin fails below it."
fi
