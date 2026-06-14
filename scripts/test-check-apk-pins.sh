#!/usr/bin/env bash
# Table-driven tests for check-apk-pins.sh (#169). Uses the APK_AVAIL_FILE
# seam to feed synthetic `apk policy` output (no docker/network), against a
# synthetic Dockerfile, and asserts the drift verdict. The first case is
# the regression guard: `apk policy` emits versions with a trailing colon
# (`1.36.1-r31:`), which the checker must strip so a current pin is NOT
# falsely reported as an upgrade candidate.
set -u

CHECK="$(dirname "$0")/check-apk-pins.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Synthetic Dockerfile the checker parses pins + alpine ref from.
DF="$TMP/Dockerfile"
cat > "$DF" <<'EOF'
FROM golang:1.26-alpine AS builder
FROM alpine:3.20
RUN apk add --no-cache \
        busybox-extras=1.36.1-r31 \
        iproute2=6.9.0-r0
EOF

failures=0
# check NAME WANT_EXIT STRICT AVAIL_CONTENT GREP_PATTERN
check() {
    local name="$1" want_exit="$2" strict="$3" avail="$4" want_grep="$5"
    printf '%s' "$avail" > "$TMP/avail.txt"
    DOCKERFILE="$DF" APK_AVAIL_FILE="$TMP/avail.txt" \
        bash "$CHECK" $strict > "$TMP/out" 2>&1
    local got_exit=$?
    local ok=1
    [ "$got_exit" -eq "$want_exit" ] || ok=0
    if [ -n "$want_grep" ] && ! grep -q "$want_grep" "$TMP/out"; then ok=0; fi
    if [ "$ok" -eq 1 ]; then
        echo "PASS: $name"
    else
        echo "FAIL: $name (want exit $want_exit/grep '$want_grep', got exit $got_exit)"
        sed 's/^/    /' "$TMP/out"
        failures=$((failures + 1))
    fi
}

# Regression guard: trailing-colon versions equal to the pins => no drift.
check "current pins (trailing colon) report OK, exit 0" 0 "--strict" \
$'busybox-extras\t1.36.1-r31:\niproute2\t6.9.0-r0:\n' \
"All apk pins current."

# A genuinely newer available version => drift, --strict exits 1.
check "behind pin flagged, --strict exits 1" 1 "--strict" \
$'busybox-extras\t1.36.1-r32:\niproute2\t6.9.0-r0:\n' \
"upgrade candidate"

# Same drift without --strict is report-only (exit 0).
check "behind pin is report-only without --strict" 0 "" \
$'busybox-extras\t1.36.1-r32:\niproute2\t6.9.0-r0:\n' \
"upgrade candidate"

# Empty available (pkg not in index / bad pin) => drift.
check "missing-from-index flagged, --strict exits 1" 1 "--strict" \
$'busybox-extras\t\niproute2\t6.9.0-r0:\n' \
"not found in index"

# No FROM alpine: => usage error (exit 2).
cat > "$TMP/Dockerfile.noalpine" <<'EOF'
FROM golang:1.26-alpine AS builder
RUN echo nope
EOF
DOCKERFILE="$TMP/Dockerfile.noalpine" APK_AVAIL_FILE="$TMP/avail.txt" \
    bash "$CHECK" >/dev/null 2>&1
if [ $? -eq 2 ]; then echo "PASS: missing FROM alpine: exits 2"; else
    echo "FAIL: missing FROM alpine: should exit 2"; failures=$((failures + 1)); fi

if [ "$failures" -ne 0 ]; then
    echo "$failures apk-pin-check test(s) failed"
    exit 1
fi
echo "All check-apk-pins tests passed"
