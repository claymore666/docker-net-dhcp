#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for coverage-ratchet.sh (#127). Synthesizes
# `go tool covdata percent` outputs and asserts the ratchet's verdicts:
# hold/improve/within-epsilon pass, regression and vanished packages fail.
#
# The exit-2 cases at the bottom are #734: this suite asserted every
# verdict the ratchet renders and never that it renders one at all, so
# a `coverage` check that compared nothing and reported success was
# green here too.
set -u

RATCHET="$(dirname "$0")/coverage-ratchet.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BASELINE="$TMP/baseline.txt"
cat > "$BASELINE" <<'EOF'
# comment lines and blanks are ignored

example.com/mod/pkg/a 80.0
example.com/mod/pkg/b 50.0
EOF

failures=0
check() {
    local name="$1" want_exit="$2" percent_file="$3"
    local eps="${4:-}"
    local got_exit
    if [ -n "$eps" ]; then
        RATCHET_EPSILON="$eps" bash "$RATCHET" "$percent_file" "$BASELINE" > "$TMP/out" 2>&1
    else
        bash "$RATCHET" "$percent_file" "$BASELINE" > "$TMP/out" 2>&1
    fi
    got_exit=$?
    if [ "$got_exit" -eq "$want_exit" ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

percent() { # percent <file> <pct-a> [<pct-b>]
    local f="$1"
    printf '\texample.com/mod/pkg/a\t\tcoverage: %s%% of statements\n' "$2" > "$f"
    if [ "$#" -ge 3 ]; then
        printf '\texample.com/mod/pkg/b\t\tcoverage: %s%% of statements\n' "$3" >> "$f"
    fi
}

percent "$TMP/hold.txt" 80.0 50.0
check "exact hold passes" 0 "$TMP/hold.txt"

percent "$TMP/up.txt" 85.5 61.2
check "improvement passes" 0 "$TMP/up.txt"

percent "$TMP/noise.txt" 79.7 50.0
check "drop within epsilon passes" 0 "$TMP/noise.txt"

percent "$TMP/down.txt" 77.9 50.0
check "regression fails" 1 "$TMP/down.txt"

percent "$TMP/down-b.txt" 80.0 48.0
check "regression in second package fails" 1 "$TMP/down-b.txt"

percent "$TMP/gone.txt" 80.0
check "baselined package missing from output fails" 1 "$TMP/gone.txt"

percent "$TMP/eps.txt" 78.0 50.0
check "wider RATCHET_EPSILON tolerates the drop" 0 "$TMP/eps.txt" 2.5

if bash "$RATCHET" "$TMP/hold.txt" > /dev/null 2>&1; [ $? -eq 2 ]; then
    echo "PASS: usage error exits 2"
else
    echo "FAIL: usage error should exit 2"
    failures=$((failures + 1))
fi

# #734: a verdict over nothing. Each of these exited 0 — silently, in
# two of the three cases — before the refusal was added, which is a
# green required check on main enforcing no floor at all.
refuses() { # refuses <name> <baseline-file>
    local name="$1" baseline="$2" got_exit
    bash "$RATCHET" "$TMP/hold.txt" "$baseline" > "$TMP/out" 2>&1
    got_exit=$?
    if [ "$got_exit" -eq 2 ] && grep -q 'Nothing to inspect' "$TMP/out"; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit 2 + a refusal, got $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

refuses "missing baseline file refuses a verdict" "$TMP/does-not-exist.txt"

: > "$TMP/empty-baseline.txt"
refuses "empty baseline refuses a verdict" "$TMP/empty-baseline.txt"

printf '# a floor used to live here\n\n' > "$TMP/comments-baseline.txt"
refuses "comments-only baseline refuses a verdict" "$TMP/comments-baseline.txt"

# The percent file is the other half of the same hazard: the ratchet
# would report every baselined package "absent from coverage output"
# and exit 1, which reads as a coverage regression rather than as a
# harness fault. A wrong diagnosis costs the next person the afternoon.
bash "$RATCHET" "$TMP/no-such-percent.txt" "$BASELINE" > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q 'Nothing to inspect' "$TMP/out"; then
    echo "PASS: missing percent file refuses a verdict"
else
    echo "FAIL: missing percent file should refuse a verdict"
    sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures ratchet test(s) failed"
    exit 1
fi
echo "All ratchet tests passed"
