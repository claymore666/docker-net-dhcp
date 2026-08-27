#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-host-watchdog.sh (#677).
#
# The gate makes three claims that fail in different directions, and a
# version that caught only the loudest of them would still read as
# covering the feature:
#
#   - an unarmed device (nobody pets)          -> 1
#   - an armed device held by the WRONG petter -> 1, and this one looks
#     healthy from every angle except the ring buffer
#   - terms that cannot work on this device    -> 1, the #661 shape
#
# and one direction where it must NOT reach a verdict at all: a ring
# buffer that wrapped past the boot. Silence there is missing evidence,
# not a pass and not a failure, and the gate is only trustworthy if it
# can tell that apart from real absence. Both are driven here.
#
# The fixtures are captured from the live host rather than invented, so
# the pristine case passes for the same reasons the real board does.
set -uo pipefail

# THE SUBJECT READS `STRICT`, SO THIS FILE HAS TO OWN IT. The gate turns
# "cannot check" (2) into a failure (1) when STRICT=1 is in its
# environment, and the two cases below that assert on the 2 do not set
# it -- so they inherited whatever the caller had. `scripts/local-lane.sh`
# documents `STRICT=1` as its own supported mode ("a skipped step is a
# failure"), a different meaning under the same name, and passes it to
# every step: `STRICT=1 bash scripts/local-lane.sh` -- the invocation the
# lane's own header tells automation to use -- made three cases here fail
# for a reason that has nothing to do with the gate.
#
# Cleared rather than saved and restored: a self-test's verdict must not
# depend on the environment it was launched from, and the two cases that
# WANT strict mode set it per-invocation already.
unset STRICT

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-host-watchdog.sh"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want exit '$want', got '$got'"
        fails=1
    fi
}

says() {
    local name="$1" pattern="$2" desc="$3"
    if grep -qa -- "$pattern" "$TMP/$name.log"; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — '$pattern' not in the output"
        fails=1
    fi
}

# A real boot's ring buffer, trimmed to the lines the gate reads: the
# kernel's first line (its unwrapped-buffer marker) and nfs-watchdog's
# startup announcement, verbatim from the arm64 host.
BOOT_LINE='6,0,0,-;Booting Linux on physical CPU 0x0000000000 [0x410fd083]'
WD_LINES='12,516,17673916,-;nfs-watchdog: device reports a 15s hardware timeout (configured 1m0s); using the device'"'"'s
12,517,17674010,-;nfs-watchdog: the default pet-interval, probe-interval, stale-after do not fit a 15s hardware timeout; scaled to 3s/3s/9s (pet/probe/stale) rather than refusing to run and leaving the board unwatched
12,518,17747729,-;nfs-watchdog: watching / via statfs every 3s; petting /dev/watchdog0 every 3s; stop petting after 9s without a successful probe'

# fixture <name> [state] [timeout] [kmsg-body]
fixture() {
    local name="$1" state="${2:-active}" timeout="${3:-15}" kmsg="${4:-$BOOT_LINE
$WD_LINES}"
    local d="$TMP/$name"
    mkdir -p "$d/sysfs"
    printf '%s\n' "$state" > "$d/sysfs/state"
    printf '%s\n' "$timeout" > "$d/sysfs/timeout"
    printf '%s\n' "Broadcom BCM2835 Watchdog timer" > "$d/sysfs/identity"
    printf '%s\n' "$kmsg" > "$d/kmsg"
    HOST_WATCHDOG_SYSFS="$d/sysfs" HOST_WATCHDOG_KMSG="$d/kmsg" \
        bash "$CHECK" >"$TMP/$name.log" 2>&1
    echo "$?"
}

# --- the real thing passes ---------------------------------------------

got=$(fixture pristine)
check "a real armed host with scaled timings passes" 0 "$got"
says pristine "probe 3s, pet 3s, stale 9s" "reports the effective timings it verified"

# --- direction 1: nobody holds the device ------------------------------
# The #661 regression as the kernel sees it: the daemon refused to start,
# so the device was never opened.

got=$(fixture unarmed inactive)
check "an inactive watchdog fails" 1 "$got"
says unarmed "UNWATCHED" "names the consequence rather than just the state"
says unarmed "#661" "points at the image regression that causes it"

# --- direction 2: the wrong petter holds it ----------------------------
# Everything looks healthy: the device is armed and being petted. Only
# the absence of the daemon's own lines, in a buffer proven to reach back
# to the start of the boot, gives it away.

got=$(fixture wrongpetter active 15 "$BOOT_LINE")
check "an armed device with no nfs-watchdog announcement fails" 1 "$got"
says wrongpetter "systemd" "names systemd's petter as the thing that took the device"
says wrongpetter "pets straight through the" "explains why that is worse than unarmed"

# --- the direction that must NOT be a verdict --------------------------
# Same absence, but the buffer no longer reaches the start of the boot.
# Missing evidence must never be scored as either outcome.

got=$(fixture wrapped active 15 "12,900,99999999,-;some later message")
check "a wrapped ring buffer is 'cannot check', not a pass or a failure" 2 "$got"
says wrapped "silence is not a verdict" "says why it declined to judge"

# The pair above is the whole point: identical missing lines, opposite
# handling, decided only by whether the buffer still proves it saw the
# boot. Assert they really did diverge, so a future edit cannot collapse
# them into one answer.
if [ "$(fixture wrongpetter active 15 "$BOOT_LINE")" = "$(fixture wrapped active 15 "12,900,99999999,-;later")" ]; then
    echo "FAIL: real absence and a wrapped buffer now return the same exit code"
    fails=1
else
    echo "PASS: real absence and a wrapped buffer are told apart"
fi

# --- direction 3: terms that cannot work on this device ----------------
# A build that neither scales nor refuses. The daemon is up, it holds the
# device, it announced itself — and its numbers are the pre-#661 defaults,
# which need a 60s device. This is invisible to every other check.

DEFAULTS='12,518,17747729,-;nfs-watchdog: watching / via statfs every 12s; petting /dev/watchdog0 every 12s; stop petting after 36s without a successful probe'
got=$(fixture defaults active 15 "$BOOT_LINE
$DEFAULTS")
check "tuned-for-60s defaults on a 15s device fail" 1 "$got"
says defaults "staleness tolerance 36s is not under the 15s hardware timeout" "names which invariant broke, with the numbers"

# The same numbers on the device they were tuned for must PASS, or the
# gate is just rejecting a constant rather than comparing against the
# hardware.
got=$(fixture defaults60 active 60 "$BOOT_LINE
$DEFAULTS")
check "the same timings on a 60s device pass" 0 "$got"

# One invariant at a time, so a gate that collapsed the three into one
# test cannot pass this file.
PETTOOSLOW='12,518,1,-;nfs-watchdog: watching / via statfs every 3s; petting /dev/watchdog0 every 8s; stop petting after 9s without a successful probe'
got=$(fixture petslow active 15 "$BOOT_LINE
$PETTOOSLOW")
check "a pet interval over half the timeout fails on its own" 1 "$got"
says petslow "one missed tick" "explains the pet-interval invariant"

PROBETOOSLOW='12,518,1,-;nfs-watchdog: watching / via statfs every 10s; petting /dev/watchdog0 every 3s; stop petting after 9s without a successful probe'
got=$(fixture probeslow active 15 "$BOOT_LINE
$PROBETOOSLOW")
check "a probe interval over the staleness tolerance fails on its own" 1 "$got"
says probeslow "goes stale between probes" "explains the probe-interval invariant"

# --- hosts that have no watchdog at all --------------------------------
# The amd64 pool runs this same lane step. A missing device is not a
# failure there, and must not be dressed as one.

rm -rf "$TMP/nodev"
HOST_WATCHDOG_SYSFS="$TMP/nodev/sysfs" HOST_WATCHDOG_KMSG="$TMP/nodev/kmsg" \
    bash "$CHECK" >"$TMP/nodev.log" 2>&1
check "a host with no watchdog device is 'cannot check', not a failure" 2 "$?"

# --- STRICT=1 collapses "cannot check" into a failure ------------------
# The lane runs without it, because a wrapped buffer is a normal
# consequence of uptime rather than a defect. Anywhere the exit code is
# read by a machine instead of a human, missing evidence must not be
# survivable — so the escape hatch is checked to actually close.

d="$TMP/wrapped"
STRICT=1 HOST_WATCHDOG_SYSFS="$d/sysfs" HOST_WATCHDOG_KMSG="$d/kmsg" \
    bash "$CHECK" >"$TMP/strict.log" 2>&1
check "STRICT=1 turns a wrapped buffer into a failure" 1 "$?"
says strict "STRICT=1" "says the exit code was escalated rather than silently changed"

# ...and must not change a genuine pass into anything else.
d="$TMP/pristine"
STRICT=1 HOST_WATCHDOG_SYSFS="$d/sysfs" HOST_WATCHDOG_KMSG="$d/kmsg" \
    bash "$CHECK" >/dev/null 2>&1
check "STRICT=1 leaves a real pass alone" 0 "$?"

# --- the gate must never arm the device --------------------------------
# Opening /dev/watchdog0 starts it. A checker that armed a device nobody
# then pets would reset the board it was sent to inspect, which is the
# one bug this file cannot afford to let through.
if grep -nE '(cat|dd|head|tail|exec|<).*"?/dev/watchdog[0-9]' "$CHECK" >/dev/null; then
    echo "FAIL: the gate reads /dev/watchdog0 — opening it ARMS the device"
    fails=1
else
    echo "PASS: the gate never opens /dev/watchdog0"
fi

if [ "$fails" -ne 0 ]; then
    echo "test-check-host-watchdog: FAILURES"
    exit 1
fi
echo "test-check-host-watchdog: all assertions passed"
