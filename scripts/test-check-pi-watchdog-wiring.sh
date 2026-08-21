#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Meta-test for check-pi-watchdog-wiring.sh (#632).
#
# The gate's whole claim is that it catches BOTH directions: systemd
# keeping the watchdog (nothing pets, unit dead, host unprotected) and
# systemd releasing it with nothing enabled to take over (a healthy host
# resets a minute after boot). A gate that only caught one would read as
# covering the feature while half of it rotted, so each is driven here
# against a fixture with exactly that one thing wrong.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
CHECK="$HERE/check-pi-watchdog-wiring.sh"
REAL="$REPO/test/arm64-netboot"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
fails=0

check() {
    local desc="$1" want="$2" got="$3"
    if [ "$got" = "$want" ]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc — want '$want', got '$got'"
        fails=1
    fi
}

# A fixture is the real tree, copied, then broken in one specific way.
# Copying the real files rather than writing minimal ones keeps the
# fixture honest: it starts out passing for the same reasons the repo
# does, so a failure is attributable to the single edit.
fixture() {
    local name="$1"; shift
    local d="$TMP/$name"
    mkdir -p "$d/nfs-watchdog"
    cp "$REAL/patch-target.sh" "$REAL/Dockerfile" "$d/"
    cp "$REAL/nfs-watchdog/main.go" "$d/nfs-watchdog/"
    [ "$#" -gt 0 ] && "$@" "$d"
    ( cd "$REPO" && bash "$CHECK" "$d" >"$TMP/$name.log" 2>&1 ) && echo pass || echo fail
}

got=$(fixture pristine)
check "the real tree passes" pass "$got"

# --- direction 1: systemd never lets go --------------------------------
break_dropin() { sed -i 's/RuntimeWatchdogSec=0/RuntimeWatchdogSec=1m/' "$1/patch-target.sh"; }
got=$(fixture nodropin break_dropin)
check "a missing RuntimeWatchdogSec=0 drop-in fails" fail "$got"
grep -q "EBUSY" "$TMP/nodropin.log" \
    && echo "PASS: says the service would get EBUSY" \
    || { echo "FAIL: rejected without naming the consequence"; fails=1; }

# --- direction 2: systemd lets go and nothing takes over ---------------
# The opposite failure, and the more dangerous one: it resets a healthy
# host rather than failing to protect a broken one.
break_enable() { sed -i '/sysinit.target.wants\/nfs-watchdog.service/d' "$1/patch-target.sh"; }
got=$(fixture noenable break_enable)
check "a unit that is never enabled fails" fail "$got"
grep -q "HEALTHY" "$TMP/noenable.log" \
    && echo "PASS: says a healthy host would reset" \
    || { echo "FAIL: rejected without naming the opposite consequence"; fails=1; }

# --- ExecStart drifting from the install path --------------------------
break_path() { sed -i 's|^ExecStart=/usr/local/sbin/nfs-watchdog|ExecStart=/usr/sbin/nfs-watchdog|' "$1/patch-target.sh"; }
got=$(fixture pathdrift break_path)
check "ExecStart pointing somewhere else fails" fail "$got"

# --- the memlock limit -------------------------------------------------
break_memlock() { sed -i '/^LimitMEMLOCK=infinity/d' "$1/patch-target.sh"; }
got=$(fixture nomemlock break_memlock)
check "a missing LimitMEMLOCK=infinity fails" fail "$got"

# --- the image not shipping the binary ---------------------------------
break_image() { sed -i '/netboot-templates\/nfs-watchdog/d' "$1/Dockerfile"; }
got=$(fixture noship break_image)
check "an image that does not ship the binary fails" fail "$got"

# --- a non-stdlib import -----------------------------------------------
# This one is here because nothing in CI builds the netboot image, so the
# breakage would otherwise surface at the next reprovision of the host.
break_import() {
    sed -i 's|^\t"strconv"$|\t"strconv"\n\n\t"golang.org/x/sys/unix"|' "$1/nfs-watchdog/main.go"
}
got=$(fixture dep break_import)
check "a non-stdlib import fails" fail "$got"
grep -q "reprovision" "$TMP/dep.log" \
    && echo "PASS: says when the breakage would otherwise surface" \
    || { echo "FAIL: rejected without explaining why it matters here"; fails=1; }

# A stdlib import that merely contains a dot in a path segment must not
# be mistaken for a dependency. Guard against the pattern being too eager.
allow_stdlib() { sed -i 's|^\t"strconv"$|\t"strconv"\n\t"path/filepath"|' "$1/nfs-watchdog/main.go"; }
got=$(fixture stdlibdup allow_stdlib)
check "duplicate stdlib imports are not read as dependencies" pass "$got"

# --- ordered against shutdown ------------------------------------------
# The regression this check exists for: both lines were in the unit, under
# a comment saying the unit survives shutdown, and the board hung for 14
# minutes on a shutdown that blocked on its dead NFS root (2026-08-20).
# Each line is driven separately — either one alone is enough to have
# systemd stop the unit, so a gate that only looked for the pair would
# pass a tree that is still broken.
break_shutdown_conflicts() {
    sed -i 's|^After=sysinit.target$|After=sysinit.target\nConflicts=shutdown.target|' "$1/patch-target.sh"
}
got=$(fixture shutdownconflict break_shutdown_conflicts)
check "Conflicts=shutdown.target in the unit fails" fail "$got"
grep -q "NOTHING armed" "$TMP/shutdownconflict.log" \
    && echo "PASS: says what is left running the board during a hung shutdown" \
    || { echo "FAIL: rejected without naming the consequence"; fails=1; }

break_shutdown_before() {
    sed -i 's|^After=sysinit.target$|After=sysinit.target\nBefore=shutdown.target|' "$1/patch-target.sh"
}
got=$(fixture shutdownbefore break_shutdown_before)
check "Before=shutdown.target in the unit fails" fail "$got"

# ...and the check must read the [Unit] section only. A [Service] line
# mentioning the same target is not an ordering dependency, and flagging
# it would train the next reader to ignore this check.
allow_shutdown_elsewhere() {
    sed -i 's|^OOMScoreAdjust=-1000$|OOMScoreAdjust=-1000\nEnvironment=NOTE=Before=shutdown.target|' "$1/patch-target.sh"
}
got=$(fixture shutdownelsewhere allow_shutdown_elsewhere)
check "the same text outside [Unit] is not read as an ordering dep" pass "$got"

# --- cannot check is not a pass ----------------------------------------
got=$(cd "$REPO" && bash "$CHECK" "$TMP/does-not-exist" >/dev/null 2>&1; echo $?)
check "a missing directory exits 2, not 0" 2 "$got"

if [ "$fails" -ne 0 ]; then
    echo "pi watchdog wiring meta-test FAILED"
    exit 1
fi
echo "PASS  the wiring gate catches both directions and refuses to judge nothing"
