#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for engine-floor.sh (#670).
#
# THE VERDICT THIS GATE MUST NEVER PRODUCE is a clean pass over rows it
# could not read, or over rows that never arrived. Its first draft did
# the first: a row tagged 18.09 made the arithmetic fail on an invalid
# octal literal, bash printed a warning to stderr, the comparison ran
# against an empty string, and the gate finished with "PASS  declared
# floor 20.10 is the lowest passing row". Its second did the second: the
# reconciliation's domain was the rows that happened to arrive, so the
# one row whose job is to probe below the floor could die in its build
# step and leave a green matrix behind. Every case below that drives a
# malformed or missing input exists because of one of those, and each
# asserts a REFUSAL rather than a colour.
#
# Both directions, everywhere: a rule that only ever goes red on garbage
# is satisfied by refusing everything, so each red case has a green twin
# that differs in one field.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

GATE="$(cd "$(dirname "$0")" && pwd)/engine-floor.sh"

pass=0
fail=0

# floor_file <dir> <declaration-body> — writes a Go file carrying the
# declaration text given. The body is written verbatim so a case can
# drive a renamed or reformatted constant, which is the input the gate
# has to refuse rather than read as empty.
floor_file() {
    local dir="$1" body="$2"
    mkdir -p "$dir"
    {
        printf '// Copyright the docker-net-dhcp contributors.\n'
        printf '// SPDX-License-Identifier: GPL-3.0-only\n\n'
        printf 'package plugin\n\n'
        printf '%s\n' "$body"
    } > "$dir/engine_floor.go"
    printf '%s' "$dir/engine_floor.go"
}

# fixture <dir> <spec>... builds both halves of a case from one spec
# list, so a case cannot accidentally declare one set of rows and drive
# another. A spec is <tag>:<engine>:<result>, with two special results:
#
#   missing   the tag is declared in the row list and no row arrives.
#   undeclared  a row arrives for a tag the list does not declare.
fixture() {
    local dir="$1"; shift
    local spec tag engine result
    mkdir -p "$dir/rows"
    : > "$dir/rows.txt"
    for spec in "$@"; do
        tag="${spec%%:*}"
        engine="${spec#*:}"; engine="${engine%%:*}"
        result="${spec##*:}"
        [ "$result" = undeclared ] || printf '%s\n' "$tag" >> "$dir/rows.txt"
        [ "$result" = missing ] && continue
        [ "$result" = undeclared ] && result=pass
        printf 'ENGINE_MATRIX_ROW tag=%s engine=%s api=1.41 result=%s step=%s x\n' \
            "$tag" "$engine" "$result" \
            "$([ "$result" = pass ] && echo complete || echo plugin-create)" \
            > "$dir/rows/${tag//./-}.row"
    done
}

# check <name> <want_exit> <want_substring> <floor-body> <spec>...
# An empty <want_substring> asserts only the exit code.
check() {
    local name="$1" want_exit="$2" want_sub="$3" body="$4"; shift 4
    local tmp out got ff
    guarded_tmpdir tmp
    ff="$(floor_file "$tmp/src" "$body")"
    fixture "$tmp" "$@"

    out="$(ENGINE_FLOOR_FILE="$ff" ENGINE_ROWS_FILE="$tmp/rows.txt" \
        bash "$GATE" --reconcile "$tmp/rows" 2>&1)"
    got=$?

    if [ "$got" -ne "$want_exit" ]; then
        echo "FAIL  $name: exit $got, want $want_exit"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    elif [ -n "$want_sub" ] && ! printf '%s' "$out" | grep -F -- "$want_sub" >/dev/null; then
        echo "FAIL  $name: output does not mention '$want_sub'"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    else
        echo "ok    $name"
        pass=$((pass + 1))
    fi
    rm -rf "$tmp"
}

DECL='const MinEngineVersion = "20.10"'

# The row set every green case starts from: a floor row, rows above it,
# and one row BELOW it. The below-floor row is not decoration — a case
# without it is refused, which is its own case further down.
BASE=(19.03:19.03.15:fail 20.10:20.10.24:pass 23:23.0.6:pass 29:29.8.0:pass)

# --- the declaration ---------------------------------------------------

# --print answers from the real tree, which is also what the workflow's
# documentation gate reads. A tree whose constant has moved fails here
# before any row is considered.
out="$(bash "$GATE" --print 2>&1)"; got=$?
if [ "$got" -eq 0 ] && printf '%s' "$out" | grep -E '^[0-9]+\.[0-9]+$' >/dev/null; then
    echo "ok    --print answers a major.minor from the tree ($out)"
    pass=$((pass + 1))
else
    echo "FAIL  --print: exit $got, output '$out'"
    fail=$((fail + 1))
fi

# --rows-json is what the workflow's matrix is built from. A list this
# cannot render is a matrix that does not run, so it is checked against
# the real file rather than a fixture.
out="$(bash "$GATE" --rows-json 2>&1)"; got=$?
if [ "$got" -eq 0 ] && printf '%s' "$out" | grep -E '^\["[0-9][^"]*"(,"[0-9][^"]*")*\]$' >/dev/null; then
    echo "ok    --rows-json answers a JSON array from the tree ($out)"
    pass=$((pass + 1))
else
    echo "FAIL  --rows-json: exit $got, output '$out'"
    fail=$((fail + 1))
fi

check "a renamed constant is refused, not read as empty" 2 "no MinEngineVersion declaration" \
    'const EngineFloor = "20.10"' \
    "${BASE[@]}"

check "a declaration in a comment does not count" 2 "no MinEngineVersion declaration" \
    '// const MinEngineVersion = "20.10"' \
    "${BASE[@]}"

check "two declarations are refused" 2 "more than once" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst MinEngineVersion = "23.0"')" \
    "${BASE[@]}"

check "the same file with one declaration passes" 0 "PASS" \
    "$DECL" \
    "${BASE[@]}"

# --- the list ----------------------------------------------------------

# THE LIST IS THE DOMAIN. Both cases below are green under a gate that
# reconciles whatever arrived.
check "a list that stops at the floor is refused" 1 "no declared row is below the floor" \
    "$DECL" \
    20.10:20.10.24:pass 23:23.0.6:pass

check "a declared row that never arrived is refused" 1 "and no row for it arrived" \
    "$DECL" \
    19.03:19.03.15:missing 20.10:20.10.24:pass 23:23.0.6:pass

check "a row nobody declared is refused" 1 "does not declare" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:pass 18.09:18.09.9:undeclared

# --- the rows ----------------------------------------------------------

check "the declared floor is the lowest passing row" 0 "declared floor 20.10" \
    "$DECL" \
    "${BASE[@]}"

check "a floor declared above the lowest passing row fails" 1 "declared floor 23.0, measured floor 20.10" \
    'const MinEngineVersion = "23.0"' \
    19.03:19.03.15:fail 20.10:20.10.24:pass 23:23.0.6:pass

check "a floor declared below the lowest passing row fails" 1 "declared floor 19.03" \
    'const MinEngineVersion = "19.03"' \
    18.09:18.09.9:fail 19.03:19.03.15:fail 20.10:20.10.24:pass

check "a hole above the floor is not a floor" 1 "not upward closed" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:pass 23:23.0.6:fail 29:29.8.0:pass

check "a row with no verdict is a failure, not an absence" 1 "reached no verdict" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:pass 23:23.0.6:no-verdict

check "no passing row at all is a failure" 1 "no row passed" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:fail

# --- measured, unmeasured, and the sentence the docs may use -----------

# `unavailable` says the rig never ran a container on that engine, so
# the row is evidence about the host and not about the plugin. Below the
# floor that changes which sentence the documentation is allowed to
# make; above it, it is a hole in the claim.
check "an unmeasured row below the floor still reconciles" 0 "lowest engine MEASURED to work" \
    "$DECL" \
    19.03:19.03.15:unavailable 20.10:20.10.24:pass 23:23.0.6:pass

check "a failing row below the floor makes it a measured boundary" 0 "measured boundary" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:pass 23:23.0.6:pass

check "an unmeasured row above the floor is refused" 1 "was not measured at all" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:pass 23:23.0.6:unavailable 29:29.8.0:pass

# THE OCTAL TRAP, both directions. `09` and `08` are invalid octal
# literals; every other minor in the matrix is not. The green case is
# what proves the gate READS such a row rather than merely refusing it.
check "a minor with a leading zero is ordered, not silently dropped" 0 "declared floor 18.09" \
    'const MinEngineVersion = "18.09"' \
    17.12:17.12.1:fail 18.09:18.09.9:pass 20.10:20.10.24:pass

check "a leading-zero minor is still held to upward closure" 1 "not upward closed" \
    'const MinEngineVersion = "18.09"' \
    17.12:17.12.1:fail 18.09:18.09.9:pass 19.03:19.03.15:fail 20.10:20.10.24:pass

check "a row reporting an unorderable version is refused" 1 "cannot order" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:OCI-runtime-exec-failed:pass

# --- the deployed engine, and which row measures it (#1014) -----------

# check_prod <name> <want_exit> <want_substring> <floor-body> <row tag>...
# The rows here are a LIST only: --production-row reads the declaration
# and the list, never a verdict, so a spec carrying results would say
# this mode sees more than it does.
check_prod() {
    local name="$1" want_exit="$2" want_sub="$3" body="$4"; shift 4
    local tmp out got ff tag
    guarded_tmpdir tmp
    ff="$(floor_file "$tmp/src" "$body")"
    : > "$tmp/rows.txt"
    for tag in "$@"; do printf '%s\n' "$tag" >> "$tmp/rows.txt"; done

    out="$(ENGINE_FLOOR_FILE="$ff" ENGINE_ROWS_FILE="$tmp/rows.txt" \
        bash "$GATE" --production-row 2>&1)"
    got=$?

    if [ "$got" -ne "$want_exit" ]; then
        echo "FAIL  $name: exit $got, want $want_exit"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    elif [ -n "$want_sub" ] && ! printf '%s' "$out" | grep -F -- "$want_sub" >/dev/null; then
        echo "FAIL  $name: output does not mention '$want_sub'"
        printf '%s\n' "$out" | sed 's/^/      /'
        fail=$((fail + 1))
    else
        echo "ok    $name"
        pass=$((pass + 1))
    fi
    rm -rf "$tmp"
}

BOTH="$(printf 'const MinEngineVersion = "20.10"\nconst ProductionEngineVersion = "26.1.5"')"
LIST=(29 28 27 26 25 24 23 20.10 19.03)

# --production-row is what the release lane asks before it publishes, so
# it is checked against the real tree as well as against fixtures.
out="$(bash "$GATE" --production-row 2>&1)"; got=$?
if [ "$got" -eq 0 ] && printf '%s' "$out" | grep -E '^[0-9]+(\.[0-9]+)?$' >/dev/null; then
    echo "ok    --production-row answers a declared row from the tree ($out)"
    pass=$((pass + 1))
else
    echo "FAIL  --production-row: exit $got, output '$out'"
    fail=$((fail + 1))
fi

check_prod "the covering row is the row on the deployed engine's line" 0 "26" \
    "$BOTH" "${LIST[@]}"

check_prod "a major.minor row covers a build of that minor" 0 "20.10" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst ProductionEngineVersion = "20.10.24"')" \
    "${LIST[@]}"

check_prod "a renamed production constant is refused, not read as empty" 2 "no ProductionEngineVersion declaration" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst ProdEngine = "26.1.5"')" \
    "${LIST[@]}"

check_prod "a production declaration in a comment does not count" 2 "no ProductionEngineVersion declaration" \
    "$(printf 'const MinEngineVersion = "20.10"\n// const ProductionEngineVersion = "26.1.5"')" \
    "${LIST[@]}"

# The floor names a line and this names a build; a two-field spelling
# here would claim the row measures the whole line, which is the claim
# the third field exists to avoid making.
check_prod "a two-field production version is refused" 2 "no ProductionEngineVersion declaration" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst ProductionEngineVersion = "26.1"')" \
    "${LIST[@]}"

check_prod "two production declarations are refused" 2 "more than once" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst ProductionEngineVersion = "26.1.5"\nconst ProductionEngineVersion = "28.0.1"')" \
    "${LIST[@]}"

check_prod "a deployed engine no row measures is refused" 1 "no declared row measures" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst ProductionEngineVersion = "30.0.1"')" \
    "${LIST[@]}"

check_prod "a deployed engine below the floor is refused" 1 "below the declared floor" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst ProductionEngineVersion = "19.03.15"')" \
    "${LIST[@]}"

# The floor rule is checked first, so this case declares a floor below
# the deployed engine: what it pins is the MINOR comparison, not the
# ordering. Its first spelling declared 20.9.1 against a floor of 20.10
# and was refused for being below the floor, which would have left the
# minor comparison unmeasured behind a red case that looked right.
check_prod "a minor row that is not the deployed minor does not cover it" 1 "no declared row measures" \
    "$(printf 'const MinEngineVersion = "19.03"\nconst ProductionEngineVersion = "20.9.1"')" \
    20.10 19.03

check_prod "two rows on the deployed engine's line are refused as ambiguous" 1 "both measure" \
    "$BOTH" 29 26 26.1 20.10 19.03

check_prod "an empty row list is refused, not read as covered" 2 "declares no engine rows" \
    "$BOTH"

# --- vacuity -----------------------------------------------------------

guarded_tmpdir tmp
ff="$(floor_file "$tmp/src" "$DECL")"
fixture "$tmp" "${BASE[@]}"
rm -rf "$tmp/rows"
mkdir -p "$tmp/rows"
out="$(ENGINE_FLOOR_FILE="$ff" ENGINE_ROWS_FILE="$tmp/rows.txt" bash "$GATE" --reconcile "$tmp/rows" 2>&1)"; got=$?
if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -F "carries no ENGINE_MATRIX_ROW line" >/dev/null; then
    echo "ok    an empty row directory is refused, not passed"
    pass=$((pass + 1))
else
    echo "FAIL  empty row directory: exit $got, output '$out'"
    fail=$((fail + 1))
fi
rm -rf "$tmp"

guarded_tmpdir tmp
ff="$(floor_file "$tmp/src" "$DECL")"
fixture "$tmp" "${BASE[@]}"
out="$(ENGINE_FLOOR_FILE="$ff" ENGINE_ROWS_FILE="$tmp/rows.txt" bash "$GATE" --reconcile "$tmp/does-not-exist" 2>&1)"; got=$?
if [ "$got" -eq 2 ]; then
    echo "ok    a missing row directory is refused"
    pass=$((pass + 1))
else
    echo "FAIL  missing row directory: exit $got, output '$out'"
    fail=$((fail + 1))
fi
rm -rf "$tmp"

guarded_tmpdir tmp
ff="$(floor_file "$tmp/src" "$DECL")"
fixture "$tmp" "${BASE[@]}"
: > "$tmp/rows.txt"
out="$(ENGINE_FLOOR_FILE="$ff" ENGINE_ROWS_FILE="$tmp/rows.txt" bash "$GATE" --reconcile "$tmp/rows" 2>&1)"; got=$?
if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -F "declares no engine rows" >/dev/null; then
    echo "ok    an empty row list is refused, not read as every rule satisfied"
    pass=$((pass + 1))
else
    echo "FAIL  empty row list: exit $got, output '$out'"
    fail=$((fail + 1))
fi
rm -rf "$tmp"

echo
echo "engine-floor self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
