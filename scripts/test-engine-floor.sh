#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Self-test for engine-floor.sh (#670).
#
# THE VERDICT THIS GATE MUST NEVER PRODUCE is a clean pass over rows it
# could not read. Its first draft did exactly that: a row tagged 18.09
# made the arithmetic fail on an invalid octal literal, bash printed a
# warning to stderr, the comparison ran against an empty string, and the
# gate finished with "PASS  declared floor 20.10 is the lowest passing
# row". Every case below that drives a malformed input exists because of
# that run, and each asserts a REFUSAL rather than a colour.
#
# Both directions, everywhere: a rule that only ever goes red on garbage
# is satisfied by refusing everything, so each red case has a green twin
# that differs in one field.
set -uo pipefail

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

# rows <dir> <tag>:<engine>:<result> ...
rows() {
    local dir="$1"; shift
    local spec tag engine result
    mkdir -p "$dir"
    for spec in "$@"; do
        tag="${spec%%:*}"
        engine="${spec#*:}"; engine="${engine%%:*}"
        result="${spec##*:}"
        printf 'ENGINE_MATRIX_ROW tag=%s engine=%s api=1.41 result=%s step=%s x\n' \
            "$tag" "$engine" "$result" \
            "$([ "$result" = pass ] && echo complete || echo plugin-install)" \
            > "$dir/${tag//./-}.row"
    done
}

# check <name> <want_exit> <want_substring> <floor-body> <row-spec>...
# An empty <want_substring> asserts only the exit code.
check() {
    local name="$1" want_exit="$2" want_sub="$3" body="$4"; shift 4
    local tmp out got ff
    tmp="$(mktemp -d)"
    ff="$(floor_file "$tmp/src" "$body")"
    rows "$tmp/rows" "$@"

    out="$(ENGINE_FLOOR_FILE="$ff" bash "$GATE" --reconcile "$tmp/rows" 2>&1)"
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

check "a renamed constant is refused, not read as empty" 2 "no MinEngineVersion declaration" \
    'const EngineFloor = "20.10"' \
    20.10:20.10.24:pass 23:23.0.6:pass

check "a declaration in a comment does not count" 2 "no MinEngineVersion declaration" \
    '// const MinEngineVersion = "20.10"' \
    20.10:20.10.24:pass

check "two declarations are refused" 2 "more than once" \
    "$(printf 'const MinEngineVersion = "20.10"\nconst MinEngineVersion = "23.0"')" \
    20.10:20.10.24:pass

check "the same file with one declaration passes" 0 "PASS" \
    "$DECL" \
    20.10:20.10.24:pass 23:23.0.6:pass

# --- the rows ----------------------------------------------------------

check "the declared floor is the lowest passing row" 0 "declared floor 20.10" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:pass 23:23.0.6:pass 29:29.8.0:pass

check "a floor declared above the lowest passing row fails" 1 "declared floor 23.0, measured floor 20.10" \
    'const MinEngineVersion = "23.0"' \
    20.10:20.10.24:pass 23:23.0.6:pass

check "a floor declared below the lowest passing row fails" 1 "declared floor 19.03" \
    'const MinEngineVersion = "19.03"' \
    19.03:19.03.15:fail 20.10:20.10.24:pass

check "a hole above the floor is not a floor" 1 "not upward closed" \
    "$DECL" \
    20.10:20.10.24:pass 23:23.0.6:fail 29:29.8.0:pass

check "a row with no verdict is a failure, not an absence" 1 "reached no verdict" \
    "$DECL" \
    20.10:20.10.24:pass 23:23.0.6:no-verdict

check "no passing row at all is a failure" 1 "no row passed" \
    "$DECL" \
    19.03:19.03.15:fail 20.10:20.10.24:fail

# THE OCTAL TRAP, both directions. `09` and `08` are invalid octal
# literals; every other minor in the matrix is not. The green case is
# what proves the gate READS such a row rather than merely refusing it.
check "a minor with a leading zero is ordered, not silently dropped" 0 "declared floor 18.09" \
    'const MinEngineVersion = "18.09"' \
    18.09:18.09.9:pass 20.10:20.10.24:pass

check "a leading-zero minor is still held to upward closure" 1 "not upward closed" \
    'const MinEngineVersion = "18.09"' \
    18.09:18.09.9:pass 19.03:19.03.15:fail 20.10:20.10.24:pass

check "a row reporting an unorderable version is refused" 1 "cannot order" \
    "$DECL" \
    20.10:OCI-runtime-exec-failed:pass

# --- vacuity -----------------------------------------------------------

tmp="$(mktemp -d)"
ff="$(floor_file "$tmp/src" "$DECL")"
mkdir -p "$tmp/empty"
out="$(ENGINE_FLOOR_FILE="$ff" bash "$GATE" --reconcile "$tmp/empty" 2>&1)"; got=$?
if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -F "carries no ENGINE_MATRIX_ROW line" >/dev/null; then
    echo "ok    an empty row directory is refused, not passed"
    pass=$((pass + 1))
else
    echo "FAIL  empty row directory: exit $got, output '$out'"
    fail=$((fail + 1))
fi
rm -rf "$tmp"

tmp="$(mktemp -d)"
ff="$(floor_file "$tmp/src" "$DECL")"
out="$(ENGINE_FLOOR_FILE="$ff" bash "$GATE" --reconcile "$tmp/does-not-exist" 2>&1)"; got=$?
if [ "$got" -eq 2 ]; then
    echo "ok    a missing row directory is refused"
    pass=$((pass + 1))
else
    echo "FAIL  missing row directory: exit $got, output '$out'"
    fail=$((fail + 1))
fi
rm -rf "$tmp"

echo
echo "engine-floor self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
