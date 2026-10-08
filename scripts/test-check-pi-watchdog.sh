#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-pi-watchdog.sh, both sides (#632, #677, #745).
#
# --tree claims that BOTH directions of the wiring are caught: systemd
# keeping the watchdog (nothing pets, unit dead) and systemd releasing it
# with nothing enabled to take over (a healthy host resets a minute after
# boot). Each is driven against the real tree copied and broken in one way.
#
# --host makes three claims that fail in different directions:
#
#   - an unarmed device (nobody pets)          -> 1
#   - an armed device held by the WRONG petter -> 1, and this one looks
#     healthy from every angle except the ring buffer
#   - terms that cannot work on this device    -> 1, the #661 shape
#
# and one direction where it must NOT reach a verdict at all: a ring buffer
# that wrapped past the boot. Its fixtures are captured from the live host,
# so the pristine case passes for the same reasons the real board does.
#
# Both sides share one harness below: one `check` on exit codes, one `says`
# on output, and a fixture per side that runs the script and returns its exit.
set -uo pipefail

# shellcheck source=scripts/tmpdir-guard.sh
. "$(cd "$(dirname "$0")" && pwd)/tmpdir-guard.sh"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
CHECK="$HERE/check-pi-watchdog.sh"
REAL="$REPO/test/arm64-netboot"

guarded_tmpdir TMP
fails=0

check() { # <desc> <want exit> <got exit>
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want exit '$want', got '$got'"
        fails=1
    fi
}

says() { # <log name> <pattern> <desc>
    if grep -qa -- "$2" "$TMP/$1.log"; then
        echo "PASS: $3"
    else
        echo "FAIL: $3 — '$2' not in the output"
        fails=1
    fi
}

# run <name> <args...>: the gate from the repo root, output to <name>.log.
run() {
    local name="$1"; shift
    ( cd "$REPO" && bash "$CHECK" "$@" ) >"$TMP/$name.log" 2>&1
    echo "$?"
}

# Tree fixture: the real tree, copied, then broken in one specific way.
# Copying the real files keeps the fixture honest: it starts out passing
# for the same reasons the repo does, so a failure is the single edit.
tree_fixture() { # <name> [break-fn]
    local name="$1"; shift
    local d="$TMP/$name"
    mkdir -p "$d/nfs-watchdog"
    cp "$REAL/patch-target.sh" "$REAL/Dockerfile" "$d/"
    cp "$REAL/nfs-watchdog/main.go" "$d/nfs-watchdog/"
    [ "$#" -gt 0 ] && "$@" "$d"
    run "$name" --tree "$d"
}

# Host fixture: a sysfs directory and a ring buffer, both plain files.
BOOT_LINE='6,0,0,-;Booting Linux on physical CPU 0x0000000000 [0x410fd083]'
WD_LINES='12,516,17673916,-;nfs-watchdog: device reports a 15s hardware timeout (configured 1m0s); using the device'"'"'s
12,517,17674010,-;nfs-watchdog: the default pet-interval, probe-interval, stale-after do not fit a 15s hardware timeout; scaled to 3s/3s/9s (pet/probe/stale) rather than refusing to run and leaving the board unwatched
12,518,17747729,-;nfs-watchdog: watching / via statfs every 3s; petting /dev/watchdog0 every 3s; stop petting after 9s without a successful probe'

host_fixture() { # <name> [state] [timeout] [kmsg-body]
    local name="$1" state="${2:-active}" timeout="${3:-15}" kmsg="${4:-$BOOT_LINE
$WD_LINES}"
    local d="$TMP/$name"
    mkdir -p "$d/sysfs"
    printf '%s\n' "$state" > "$d/sysfs/state"
    printf '%s\n' "$timeout" > "$d/sysfs/timeout"
    printf '%s\n' "Broadcom BCM2835 Watchdog timer" > "$d/sysfs/identity"
    printf '%s\n' "$kmsg" > "$d/kmsg"
    HOST_WATCHDOG_SYSFS="$d/sysfs" HOST_WATCHDOG_KMSG="$d/kmsg" run "$name" --host
}

# ====================================================== modes (#745)
# No default mode: a lane that forgets to name its side must run neither.
check "no mode is a usage error" 2 "$(run nomode)"
says nomode "a mode is required" "says that a mode is required"
check "an unknown mode is a usage error" 2 "$(run badmode --nonsense)"
check "--host takes no argument" 2 "$(run hostarg --host extra)"

# Each workflow names its side. A bare call would be exit 2 above, so a
# call site that lost its flag goes red there; this pins the pairing itself.
if grep -q 'check-pi-watchdog\.sh --tree' "$REPO/.github/workflows/test.yaml"; then
    echo "PASS: test.yaml runs the tree side"
else
    echo "FAIL: test.yaml does not run check-pi-watchdog.sh --tree"; fails=1
fi
if grep -q 'check-pi-watchdog\.sh --host' "$REPO/.github/workflows/integration-arm64.yml"; then
    echo "PASS: the arm64 lane runs the host side"
else
    echo "FAIL: integration-arm64.yml does not run check-pi-watchdog.sh --host"; fails=1
fi

# ============================================================ --tree
check "the real tree passes" 0 "$(tree_fixture pristine)"
says pristine "wired end to end" "reports the verdict line, not only the exit"

# --- direction 1: systemd never lets go
break_dropin() { sed -i 's/RuntimeWatchdogSec=0/RuntimeWatchdogSec=1m/' "$1/patch-target.sh"; }
check "a missing RuntimeWatchdogSec=0 drop-in fails" 1 "$(tree_fixture nodropin break_dropin)"
says nodropin "EBUSY" "says the service would get EBUSY"

# --- direction 2: systemd lets go and nothing takes over. The more
# dangerous one: it resets a healthy host rather than failing to protect.
break_enable() { sed -i '/sysinit.target.wants\/nfs-watchdog.service/d' "$1/patch-target.sh"; }
check "a unit that is never enabled fails" 1 "$(tree_fixture noenable break_enable)"
says noenable "HEALTHY" "says a healthy host would reset"

# --- ExecStart drifting from the install path
break_path() { sed -i 's|^ExecStart=/usr/local/sbin/nfs-watchdog|ExecStart=/usr/sbin/nfs-watchdog|' "$1/patch-target.sh"; }
check "ExecStart pointing somewhere else fails" 1 "$(tree_fixture pathdrift break_path)"

# --- the memlock limit
break_memlock() { sed -i '/^LimitMEMLOCK=infinity/d' "$1/patch-target.sh"; }
check "a missing LimitMEMLOCK=infinity fails" 1 "$(tree_fixture nomemlock break_memlock)"
# The line is anchored: a comment that still names the setting is not it (#745).
break_memlock_commented() { sed -i 's/^LimitMEMLOCK=infinity/# LimitMEMLOCK=infinity was removed/' "$1/patch-target.sh"; }
check "a commented-out LimitMEMLOCK=infinity fails" 1 "$(tree_fixture commentedmemlock break_memlock_commented)"

# --- the image not shipping the binary, or not building it for arm64
break_image() { sed -i '/netboot-templates\/nfs-watchdog/d' "$1/Dockerfile"; }
check "an image that does not ship the binary fails" 1 "$(tree_fixture noship break_image)"
break_build() { sed -i 's/GOARCH=arm64 go build/GOARCH=amd64 go build/' "$1/Dockerfile"; }
check "an image that builds the binary for another arch fails" 1 "$(tree_fixture nobuild break_build)"

# --- a comment is not wiring. The checks above deleted or altered the
# line; these COMMENT IT OUT, the state the gate was once blind to: the
# token is still in the file, so an unanchored match reported it wired.
comment_dropin() { sed -i 's|^RuntimeWatchdogSec=0$|# RuntimeWatchdogSec=0|' "$1/patch-target.sh"; }
check "a COMMENTED-OUT RuntimeWatchdogSec=0 fails" 1 "$(tree_fixture commented_dropin comment_dropin)"

comment_enable() {
    sed -i 's|^\(.*sysinit\.target\.wants/nfs-watchdog\.service.*\)$|#\1|' "$1/patch-target.sh"
}
check "a COMMENTED-OUT enable link fails" 1 "$(tree_fixture commented_enable comment_enable)"

comment_ship() {
    sed -i 's|^COPY --from=watchdog-builder|# COPY --from=watchdog-builder|' "$1/Dockerfile"
}
check "a COMMENTED-OUT COPY of the binary fails" 1 "$(tree_fixture commented_ship comment_ship)"

comment_build() { sed -i 's|^\(.*GOARCH=arm64 go build.*\)$|#\1|' "$1/Dockerfile"; }
check "a COMMENTED-OUT arm64 build fails" 1 "$(tree_fixture commented_build comment_build)"

# The other direction, so anchoring cannot be "fixed" by making the tokens
# unmatchable: an indented real line must still count.
indent_enable() {
    sed -i 's|^\(  *\)\(.*sysinit\.target\.wants/nfs-watchdog\.service.*\)$|\1  \2|' \
        "$1/patch-target.sh"
}
check "a further-indented enable link still counts" 0 "$(tree_fixture indented_enable indent_enable)"

# --- a non-stdlib import. The netboot-image workflow compiles it too, but
# only on changes under that directory and as a Go compile error; this
# runs on every push and names the rule.
break_import() {
    sed -i 's|^\t"strconv"$|\t"strconv"\n\n\t"golang.org/x/sys/unix"|' "$1/nfs-watchdog/main.go"
}
check "a non-stdlib import fails" 1 "$(tree_fixture dep break_import)"
says dep "reprovision" "says when the breakage would otherwise surface"

# A stdlib import with a dot in a path segment is not a dependency.
allow_stdlib() { sed -i 's|^\t"strconv"$|\t"strconv"\n\t"path/filepath"|' "$1/nfs-watchdog/main.go"; }
check "duplicate stdlib imports are not read as dependencies" 0 "$(tree_fixture stdlibdup allow_stdlib)"

# --- ordered against shutdown (the 14-minute hang, 2026-08-20). Each line
# is driven separately: either alone has systemd stop the unit.
break_shutdown_conflicts() {
    sed -i 's|^After=sysinit.target$|After=sysinit.target\nConflicts=shutdown.target|' "$1/patch-target.sh"
}
check "Conflicts=shutdown.target in the unit fails" 1 "$(tree_fixture shutdownconflict break_shutdown_conflicts)"
says shutdownconflict "NOTHING armed" "says what is left running the board during a hung shutdown"

break_shutdown_before() {
    sed -i 's|^After=sysinit.target$|After=sysinit.target\nBefore=shutdown.target|' "$1/patch-target.sh"
}
check "Before=shutdown.target in the unit fails" 1 "$(tree_fixture shutdownbefore break_shutdown_before)"

# ...and only the [Unit] section counts: a [Service] line naming the same
# target is not an ordering dependency.
allow_shutdown_elsewhere() {
    sed -i 's|^OOMScoreAdjust=-1000$|OOMScoreAdjust=-1000\nBefore=shutdown.target\nConflicts=shutdown.target|' "$1/patch-target.sh"
}
check "the same text outside [Unit] is not read as an ordering dep" 0 "$(tree_fixture shutdownelsewhere allow_shutdown_elsewhere)"

# --- cannot check is not a pass
check "a missing directory exits 2, not 0" 2 "$(cd "$REPO" && bash "$CHECK" --tree "$TMP/does-not-exist" >/dev/null 2>&1; echo $?)"

# ============================================================ --host
check "a real armed host with scaled timings passes" 0 "$(host_fixture hostok)"
says hostok "probe 3s, pet 3s, stale 9s" "reports the effective timings it verified"

# --- direction 1: nobody holds the device (the #661 regression as the
# kernel sees it: the daemon refused to start, so it never opened it)
check "an inactive watchdog fails" 1 "$(host_fixture unarmed inactive)"
says unarmed "UNWATCHED" "names the consequence rather than just the state"
says unarmed "#661" "points at the image regression that causes it"

# --- direction 2: the wrong petter holds it. The device is armed and
# petted; only the daemon's missing lines, in a buffer proven to reach back
# to the start of the boot, give it away.
check "an armed device with no nfs-watchdog announcement fails" 1 "$(host_fixture wrongpetter active 15 "$BOOT_LINE")"
says wrongpetter "systemd" "names systemd's petter as the thing that took the device"
says wrongpetter "pets straight through the" "explains why that is worse than unarmed"

# --- the direction that must NOT be a verdict: same absence, but the
# buffer no longer reaches the start of the boot.
check "a wrapped ring buffer is 'cannot check', not a pass or a failure" 2 "$(host_fixture wrapped active 15 "12,900,99999999,-;some later message")"
says wrapped "silence is not a verdict" "says why it declined to judge"

# Identical missing lines, opposite handling: assert they really diverge.
if [ "$(host_fixture wrongpetter2 active 15 "$BOOT_LINE")" = "$(host_fixture wrapped2 active 15 "12,900,99999999,-;later")" ]; then
    echo "FAIL: real absence and a wrapped buffer now return the same exit code"
    fails=1
else
    echo "PASS: real absence and a wrapped buffer are told apart"
fi

# --- direction 3: terms that cannot work on this device. The daemon is up,
# holds the device and announced itself, with the pre-#661 defaults that
# need a 60s device.
DEFAULTS='12,518,17747729,-;nfs-watchdog: watching / via statfs every 12s; petting /dev/watchdog0 every 12s; stop petting after 36s without a successful probe'
check "tuned-for-60s defaults on a 15s device fail" 1 "$(host_fixture defaults active 15 "$BOOT_LINE
$DEFAULTS")"
says defaults "staleness tolerance 36s is not under the 15s hardware timeout" "names which invariant broke, with the numbers"

# The same numbers on the device they were tuned for must PASS, or the gate
# rejects a constant instead of comparing against the hardware.
check "the same timings on a 60s device pass" 0 "$(host_fixture defaults60 active 60 "$BOOT_LINE
$DEFAULTS")"

# One invariant at a time, so a gate that collapsed the three into one
# test cannot pass this file.
PETTOOSLOW='12,518,1,-;nfs-watchdog: watching / via statfs every 3s; petting /dev/watchdog0 every 8s; stop petting after 9s without a successful probe'
check "a pet interval over half the timeout fails on its own" 1 "$(host_fixture petslow active 15 "$BOOT_LINE
$PETTOOSLOW")"
says petslow "one missed tick" "explains the pet-interval invariant"

PROBETOOSLOW='12,518,1,-;nfs-watchdog: watching / via statfs every 10s; petting /dev/watchdog0 every 3s; stop petting after 9s without a successful probe'
check "a probe interval over the staleness tolerance fails on its own" 1 "$(host_fixture probeslow active 15 "$BOOT_LINE
$PROBETOOSLOW")"
says probeslow "goes stale between probes" "explains the probe-interval invariant"

# The bounds are strict: a timing equal to its limit fails (#745). Each case
# sits exactly on one bound and clears the other two.
ann() { # <probe> <pet> <stale>
    printf '12,518,1,-;nfs-watchdog: watching / via statfs every %ss; petting /dev/watchdog0 every %ss; stop petting after %ss without a successful probe' "$1" "$2" "$3"
}
check "a staleness tolerance equal to the hardware timeout fails" 1 "$(host_fixture staleeq active 15 "$BOOT_LINE
$(ann 3 3 15)")"
says staleeq "staleness tolerance 15s is not under the 15s hardware timeout" "names the staleness bound"
check "a pet interval of exactly half the timeout fails" 1 "$(host_fixture peteq active 16 "$BOOT_LINE
$(ann 3 8 9)")"
says peteq "pet interval 8s is not under half the 16s" "names the pet bound"
check "a probe interval equal to the staleness tolerance fails" 1 "$(host_fixture probeeq active 15 "$BOOT_LINE
$(ann 9 3 9)")"
says probeeq "probe interval 9s is not under the 9s staleness" "names the probe bound"

# The daemon can restart inside one boot with new timings; the ring then
# holds both announcements and the LAST one is what is running (#745).
check "a restart onto bad timings is judged on the new ones" 1 "$(host_fixture restartbad active 15 "$BOOT_LINE
$WD_LINES
$DEFAULTS")"
says restartbad "staleness tolerance 36s" "names the timings of the last announcement"
check "a restart onto good timings is not judged on the old ones" 0 "$(host_fixture restartgood active 15 "$BOOT_LINE
$DEFAULTS
$WD_LINES")"

# --- evidence that cannot be read is "cannot check", never a verdict
check "a non-numeric hardware timeout is 'cannot check'" 2 "$(host_fixture badtimeout active fifteen)"
says badtimeout "is not a number" "says which sysfs file was unreadable"
check "an announcement with unparseable timings is 'cannot check'" 2 "$(host_fixture garbled active 15 "$BOOT_LINE
12,518,1,-;nfs-watchdog: watching / via statfs every soon; petting /dev/watchdog0 every 3s; stop petting after 9s without a successful probe")"
says garbled "could not parse" "says the timings could not be parsed"
mkdir -p "$TMP/emptyring"; : > "$TMP/emptyring/kmsg"
check "an empty ring buffer is 'cannot check'" 2 \
    "$(HOST_WATCHDOG_SYSFS="$TMP/hostok/sysfs" HOST_WATCHDOG_KMSG="$TMP/emptyring/kmsg" run emptyring --host)"
says emptyring "produced nothing" "says the ring buffer was empty"
rm -f "$TMP/nostate/sysfs/state"; mkdir -p "$TMP/nostate/sysfs"; printf '15\n' > "$TMP/nostate/sysfs/timeout"
check "an unreadable state file is 'cannot check'" 2 \
    "$(HOST_WATCHDOG_SYSFS="$TMP/nostate/sysfs" HOST_WATCHDOG_KMSG="$TMP/hostok/kmsg" run nostate --host)"
says nostate "state is unreadable" "says the state file was unreadable"

# --- hosts that have no watchdog at all. The amd64 pool runs this same
# lane step; a missing device is not a failure there.
rm -rf "$TMP/nodev"
check "a host with no watchdog device is 'cannot check', not a failure" 2 \
    "$(HOST_WATCHDOG_SYSFS="$TMP/nodev/sysfs" HOST_WATCHDOG_KMSG="$TMP/nodev/kmsg" run nodev --host)"

# --- STRICT=1 collapses "cannot check" into a failure, for callers that
# read the exit code by machine. The escape hatch is checked to close.
d="$TMP/wrapped"
check "STRICT=1 turns a wrapped buffer into a failure" 1 \
    "$(STRICT=1 HOST_WATCHDOG_SYSFS="$d/sysfs" HOST_WATCHDOG_KMSG="$d/kmsg" run strict --host)"
says strict "STRICT=1" "says the exit code was escalated rather than silently changed"

d="$TMP/hostok"
check "STRICT=1 leaves a real pass alone" 0 \
    "$(STRICT=1 HOST_WATCHDOG_SYSFS="$d/sysfs" HOST_WATCHDOG_KMSG="$d/kmsg" run strictok --host)"

# --- the gate must never arm the device. Opening /dev/watchdog0 starts
# it, and a checker that armed a device nobody pets resets the board it
# was sent to inspect.
if grep -nE '(cat|dd|head|tail|exec|<).*"?/dev/watchdog[0-9]' "$CHECK" >/dev/null; then
    echo "FAIL: the gate reads /dev/watchdog0 — opening it ARMS the device"
    fails=1
else
    echo "PASS: the gate never opens /dev/watchdog0"
fi

if [ "$fails" -ne 0 ]; then
    echo "test-check-pi-watchdog: FAILURES"
    exit 1
fi
echo "test-check-pi-watchdog: all assertions passed"
