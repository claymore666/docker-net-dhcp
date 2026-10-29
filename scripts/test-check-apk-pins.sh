#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Table-driven tests for check-apk-pins.sh (#169). Uses the APK_AVAIL_FILE
# seam to feed synthetic `apk policy` output (no docker/network), against a
# synthetic Dockerfile, and asserts the drift verdict. The first case is
# the regression guard: `apk policy` emits versions with a trailing colon
# (`10.0.6-r1:`), which the checker must strip so a current pin is NOT
# falsely reported as an upgrade candidate.
set -u

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

CHECK="$(dirname "$0")/check-apk-pins.sh"
guarded_tmpdir TMP

# Synthetic Dockerfile the checker parses pins + alpine ref from.
DF="$TMP/Dockerfile"
cat > "$DF" <<'EOF'
FROM golang:1.26-alpine AS builder
FROM alpine:3.20
RUN apk add --no-cache \
        dhcpcd=10.0.6-r1 \
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
$'dhcpcd\t10.0.6-r1:\niproute2\t6.9.0-r0:\n' \
"All apk pins current."

# A genuinely newer available version => drift, --strict exits 1.
check "behind pin flagged, --strict exits 1" 1 "--strict" \
$'dhcpcd\t10.0.6-r2:\niproute2\t6.9.0-r0:\n' \
"upgrade candidate"

# Same drift without --strict is report-only (exit 0).
check "behind pin is report-only without --strict" 0 "" \
$'dhcpcd\t10.0.6-r2:\niproute2\t6.9.0-r0:\n' \
"upgrade candidate"

# Empty available (pkg not in index / bad pin) => drift.
check "missing-from-index flagged, --strict exits 1" 1 "--strict" \
$'dhcpcd\t\niproute2\t6.9.0-r0:\n' \
"not found in index"

# --- #1398: the pin parser reads the apk add list and nothing else -----
# Runtime-stage shape of the real Dockerfile: the list's last pin carries
# a trailing `&&`, and the same RUN goes on with a shell assignment.
RT="$TMP/Dockerfile.runtime"
cat > "$RT" <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache \
        iproute2=7.0.0-r0 \
        zlib=1.3.2-r1 && \
    n=3 \
    for c in dhcpcd udhcpc; do \
        p="$(command -v "$c" 2>/dev/null || true)"; \
        [ -n "$p" ] || continue; \
    done && \
    if [ "$(readlink -f "$p")" = /bin/busybox ]; then \
        x=y; \
    fi
RUN apk add --no-cache musl=1.2.6-r3
ENV N=3
RUN apk add --no-cache \
        musl-utils=1.2.6-r3 \
        ARG=abc
RUN apkish=1.0-r0 sh
EOF
pins=$(DOCKERFILE="$RT" bash "$CHECK" --list-pins 2>&1)
want=$'iproute2=7.0.0-r0\nzlib=1.3.2-r1\nmusl=1.2.6-r3\nmusl-utils=1.2.6-r3'
if [ "$pins" = "$want" ]; then
    echo "PASS: pins are exactly the name=version tokens of the apk add lists"
else
    echo "FAIL: parsed pins differ (trailing && / shell assignment read as pins)"
    printf '    got:\n%s\n' "$pins" | sed 's/^/    /'
    failures=$((failures + 1))
fi

DF_SAVE="$DF"; DF="$RT"
check "trailing && and shell assignments do not drift a current list" 0 "--strict" \
$'iproute2\t7.0.0-r0:\t=\nzlib\t1.3.2-r1:\t=\nmusl\t1.2.6-r3:\t=\nmusl-utils\t1.2.6-r3:\t=\n' \
"All apk pins current."
DF="$DF_SAVE"

# apk version -t PINNED AVAILABLE: `>` means the pin is newer than anything
# the index offers; the image build could not install it, so it fails.
check "pin newer than the index is reported and --strict exits 1" 1 "--strict" \
$'dhcpcd\t10.0.5-r0:\t>\niproute2\t6.9.0-r0:\t=\n' \
"pin not in the index"

check "available newer than the pin still fails --strict" 1 "--strict" \
$'dhcpcd\t10.0.6-r2:\t<\niproute2\t6.9.0-r0:\t=\n' \
"upgrade candidate"

# --- a pin after a shell operator is not silent (#1398) ----------------
SUS="$TMP/Dockerfile.suspect"
cat > "$SUS" <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache \
        a=1.0-r0 && \
    b=2.0-r1
EOF
DOCKERFILE="$SUS" bash "$CHECK" --list-pins > "$TMP/out" 2>&1
if [ $? -eq 2 ] && grep -q "b=2.0-r1' looks like an apk pin" "$TMP/out"; then
    echo "PASS: a pin-shaped token after an operator fails loudly, exit 2"
else
    echo "FAIL: pin after && was dropped silently"; sed 's/^/    /' "$TMP/out"
    failures=$((failures + 1))
fi
SECOND="$TMP/Dockerfile.second"
cat > "$SECOND" <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache a=1.0-r0 && apk add --no-cache b=2.0-r1 -t c=3.0-r2
EOF
pins=$(DOCKERFILE="$SECOND" bash "$CHECK" --list-pins 2>&1)
if [ "$pins" = $'a=1.0-r0\nb=2.0-r1\nc=3.0-r2' ]; then
    echo "PASS: a second apk add after an operator contributes its pins"
else
    echo "FAIL: second apk add pins not read"; printf '%s\n' "$pins" | sed 's/^/    /'
    failures=$((failures + 1))
fi

# --- the in-image query reads the repository, not the installed line ----
# Real `apk policy` output (apk-tools 3.0.8, alpine 3.24, measured
# 2026-10-29): the INSTALLED version is listed first. A base whose
# installed musl is older than the index is the case the first-line read
# could not see (#1398). The apk shim answers from $TMP/policy and orders
# versions with sort -V, enough for these fixtures.
NEWEST="$(cd "$(dirname "$0")" && pwd)/apk-newest-available.sh"
mkdir -p "$TMP/shim" "$TMP/policy"
cat > "$TMP/shim/apk" <<'EOF'
#!/bin/sh
case "$1" in
    update) exit 0 ;;
    policy) cat "$POLICY_DIR/$2" 2>/dev/null; exit 0 ;;
    version)
        a="$3"; b="$4"
        if [ "$a" = "$b" ]; then echo '='; exit 0; fi
        first=$(printf '%s\n%s\n' "$a" "$b" | sort -V | head -n1)
        if [ "$first" = "$a" ]; then echo '<'; else echo '>'; fi ;;
esac
EOF
chmod +x "$TMP/shim/apk"
cat > "$TMP/policy/musl" <<'EOF'
musl policy:
  1.2.6-r2:
    lib/apk/db/installed
  1.2.6-r3:
    https://dl-cdn.alpinelinux.org/alpine/v3.24/main
EOF
cat > "$TMP/policy/zlib" <<'EOF'
zlib policy:
  1.3.2-r1:
    lib/apk/db/installed
    https://dl-cdn.alpinelinux.org/alpine/v3.24/main
EOF
cat > "$TMP/policy/libx" <<'EOF'
libx policy:
  1.0-r3:
    https://dl-cdn.alpinelinux.org/alpine/v3.24/main
  1.0-r4:
    lib/apk/db/installed
EOF
: > "$TMP/policy/ghost"
got=$(PATH="$TMP/shim:$PATH" POLICY_DIR="$TMP/policy" sh "$NEWEST" musl=1.2.6-r2 zlib=1.3.2-r1 libx=1.0-r3 ghost=1-r0)
want=$'musl\t1.2.6-r3\t<\nzlib\t1.3.2-r1\t=\nlibx\t1.0-r3\t=\nghost\t\t'
if [ "$got" = "$want" ]; then
    echo "PASS: newest offered version is read from the repository, not the installed line"
else
    echo "FAIL: apk-newest-available.sh output differs"; printf '%s\n' "$got" | sed 's/^/    /'
    failures=$((failures + 1))
fi
# End to end: the pins equal the installed versions, the index is newer.
DF_SAVE="$DF"; DF="$TMP/Dockerfile.installed"
cat > "$DF" <<'EOF'
FROM alpine:3.24.2
RUN apk add --no-cache \
        musl=1.2.6-r2 \
        zlib=1.3.2-r1
EOF
check "pin at the installed version with a newer index is an upgrade candidate" 1 "--strict" \
"$got"$'\n' "upgrade candidate"
DF="$DF_SAVE"

# No FROM alpine: => usage error (exit 2).
cat > "$TMP/Dockerfile.noalpine" <<'EOF'
FROM golang:1.26-alpine AS builder
RUN echo nope
EOF
DOCKERFILE="$TMP/Dockerfile.noalpine" APK_AVAIL_FILE="$TMP/avail.txt" \
    bash "$CHECK" >/dev/null 2>&1
if [ $? -eq 2 ]; then echo "PASS: missing FROM alpine: exits 2"; else
    echo "FAIL: missing FROM alpine: should exit 2"; failures=$((failures + 1)); fi

# --- the CRON CALLER must pass --strict (#742) -------------------------
# The report-only mode above is correct for a human at a terminal and
# WRONG for the scheduled workflow, which was invoking it bare. GitHub
# sends no notification for a successful scheduled run, so drift printed
# its report into a log nobody had a reason to open: seventeen of
# seventeen runs green while the pin being watched is dhcpcd, the
# component performing the whole DHCP exchange against an untrusted LAN.
#
# This is asserted as text because there is nothing else to assert. The
# failure has no runtime symptom — a report-only run and a no-drift run
# are byte-for-byte the same green tick — so the only place it is
# visible is the edit that removes the flag.
WF="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/apk-pin-check.yml"
if [ ! -r "$WF" ]; then
    echo "FAIL: cannot read $WF — the cron caller this gate reports for is missing"
    failures=$((failures + 1))
elif grep -vE '^[[:space:]]*#' "$WF" \
       | grep -E 'check-apk-pins\.sh[[:space:]]+--strict' >/dev/null; then
    echo "PASS: the scheduled workflow invokes the script with --strict"
else
    echo "FAIL: apk-pin-check.yml does not pass --strict; drift would exit 0 and"
    echo "      GitHub notifies on FAILED scheduled runs only, so nothing would"
    echo "      reach anyone. Report-only is the terminal mode, not the cron mode."
    failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
    echo "$failures apk-pin-check test(s) failed"
    exit 1
fi
echo "All check-apk-pins tests passed"
