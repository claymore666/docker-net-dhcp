#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The arm64 host's NFS-outage watchdog, checked on the RUNNING host
# rather than in the tree that provisions it (#654, #661).
#
# WHY A SECOND WATCHDOG GATE
#
# check-pi-watchdog-wiring.sh reads the provisioner. It proves the tree
# would install a working watchdog. It cannot prove the host BOOTED one,
# and those came apart in exactly the way that costs a board:
#
# The netbooted root is an image. A revert to a baseline snapshot taken
# before #661 restores a watchdog binary that REFUSES to start on this
# hardware — the BCM2835 device caps at 15s, the pre-#661 defaults need
# a 60s device, and it exited rather than scaling. Every file the source
# gate reads is still correct in that image; the tree is not what
# regressed. The host boots clean, every unit but one is happy, and the
# board is unwatched until the next NFS outage turns it into a wedge
# that needs a physical visit.
#
# So this asserts on OUTSIDE evidence, in the order of how much it can
# be faked:
#
#   1. The kernel's own view — /sys/class/watchdog/watchdog0/state.
#      `active` means some process holds the device open. Nothing else
#      needs to be believed: if the holder had stopped petting, the board
#      would have reset within `timeout` seconds and we would not be
#      running. A daemon that refused to start leaves this `inactive`,
#      which is the regression above, seen from outside.
#   2. WHO holds it, and on what terms — nfs-watchdog announces its
#      effective pet, probe and staleness intervals to /dev/kmsg at
#      startup. `state=active` alone cannot tell the daemon apart from
#      systemd's own petter, and systemd petting is the failure the
#      whole design exists to remove: PID 1 never touches the share, so
#      it pets straight through the outage.
#   3. Whether those terms can work on THIS device — re-derived here
#      from the timeout the kernel reports, not from what the daemon
#      believed it had. The pre-#661 defaults do not fit a 15s device;
#      that is the shape of the regression this exists to name.
#
# WHAT IT DELIBERATELY DOES NOT DO
#
# It never opens /dev/watchdog0. Opening it ARMS it, and a reader that
# armed a device nobody then pets would reset the board it was sent to
# check. Every read here is from sysfs or the kernel ring buffer.
#
# ABSENT EVIDENCE IS NOT A PASS
#
# The ring buffer wraps. If the daemon's startup lines have aged out,
# this reports "cannot check" (2) rather than treating silence as
# either verdict. It tells the two apart from the ring itself: the
# kernel's own first line of the boot is still present in an unwrapped
# buffer, so if THAT is there and the daemon's lines are not, the
# absence is real and it is a failure.
#
# WHAT A CALLER SHOULD DO WITH 2
#
# Note which claim survives it. The arming check runs FIRST and reads
# only sysfs, so a wrapped buffer never costs that verdict — the board
# is proven watched, and what is unproven is by WHOM. That is a real
# gap, and it is also a normal consequence of a long-running host, so
# the arm64 lane annotates it rather than reddening a release candidate
# over it. Anywhere a green exit is read as coverage rather than by a
# human who can see the annotation, set STRICT=1 and 2 becomes 1.
#
# Usage: check-host-watchdog.sh
# Env:   HOST_WATCHDOG_SYSFS  default /sys/class/watchdog/watchdog0
#        HOST_WATCHDOG_KMSG   default /dev/kmsg
#        STRICT               1 = report "cannot check" as a failure
# Exit:  0 armed, held by nfs-watchdog, on terms that fit the device
#        1 unarmed, held by something else, or on terms that cannot work
#        2 cannot check (no watchdog device, or the ring buffer wrapped)
set -uo pipefail

SYSFS="${HOST_WATCHDOG_SYSFS:-/sys/class/watchdog/watchdog0}"
KMSG="${HOST_WATCHDOG_KMSG:-/dev/kmsg}"

# STRICT=1 turns missing evidence into a failure. The default is 2 so a
# caller can tell "this host is broken" from "this run could not tell",
# which is the distinction the whole gate is built around.
skip() {
    echo "check-host-watchdog: cannot check — $*" >&2
    [ "${STRICT:-0}" = "1" ] && { echo "  STRICT=1: reporting that as a failure." >&2; exit 1; }
    exit 2
}
fail() { echo "FAIL  $*" >&2; }

# --- 1. the kernel's view ----------------------------------------------

[ -d "$SYSFS" ] || skip "no watchdog device at $SYSFS (expected on hosts without one)"

state=$(cat "$SYSFS/state" 2>/dev/null)
timeout=$(cat "$SYSFS/timeout" 2>/dev/null)
# fallback-safe: `cat` on an unreadable path prints nothing to stdout, so
# the fallback replaces rather than appends.
identity=$(cat "$SYSFS/identity" 2>/dev/null || echo "unknown")

[ -n "$state" ] || skip "$SYSFS/state is unreadable"
case "$timeout" in
    ''|*[!0-9]*) skip "$SYSFS/timeout is not a number: '$timeout'" ;;
esac

if [ "$state" != "active" ]; then
    fail "the hardware watchdog is '$state', not 'active' — nothing holds /dev/watchdog0."
    echo "  The board is running UNWATCHED: an NFS outage will wedge it (kernel alive," >&2
    echo "  root gone, sshd unable to re-exec) and only a power cycle clears that." >&2
    echo "  The usual cause is a root image older than #661, whose nfs-watchdog refuses" >&2
    echo "  to start on a ${timeout}s device instead of scaling to it. Reprovision the" >&2
    echo "  host from a current image; a baseline snapshot can silently restore the old one." >&2
    exit 1
fi
echo "ok    /dev/watchdog0 is held and being petted (device: $identity, timeout ${timeout}s)"

# --- 2. who holds it ---------------------------------------------------

# iflag=nonblock so this returns at the end of the buffer instead of
# blocking for messages that have not happened yet. /dev/kmsg yields one
# record per read, so short reads are normal and not an error here.
ring=$(dd if="$KMSG" iflag=nonblock bs=1024 2>/dev/null)

if [ -z "$ring" ]; then
    skip "$KMSG produced nothing (not readable from here?)"
fi

# nfs-watchdog's startup announcement, which carries the effective
# timings it chose for this device.
line=$(printf '%s\n' "$ring" | grep -a 'nfs-watchdog: watching .* petting /dev/watchdog0 every' | tail -1)

if [ -z "$line" ]; then
    # Real absence, or aged out? The kernel's first line of the boot is
    # the discriminator — it cannot be present in a buffer that wrapped.
    # No -q: under pipefail a consumer that exits on its first match
    # kills printf with SIGPIPE, and the pipeline then reports failure on
    # success. Reading to EOF and discarding makes the status the real one.
    if printf '%s\n' "$ring" | grep -a 'Booting Linux on physical CPU' >/dev/null; then
        fail "nfs-watchdog never announced itself, but the device is armed."
        echo "  The ring buffer still holds the start of this boot, so the daemon's" >&2
        echo "  startup lines are genuinely absent rather than aged out — something" >&2
        echo "  ELSE is petting /dev/watchdog0, and on this host that means systemd's" >&2
        echo "  own petter (RuntimeWatchdogSec) took the device back." >&2
        echo "  That is worse than an unarmed board, because it looks protected: PID 1" >&2
        echo "  is resident and never touches the share, so it pets straight through the" >&2
        echo "  outage the watchdog exists to end. Check the RuntimeWatchdogSec=0 drop-in." >&2
        exit 1
    fi
    skip "the ring buffer has wrapped past this boot — nfs-watchdog's startup lines are gone, and silence is not a verdict"
fi

echo "ok    nfs-watchdog is the holder, by its own startup announcement"

# --- 3. on terms that fit THIS device ----------------------------------

# "watching / via statfs every 3s; petting /dev/watchdog0 every 3s; stop
#  petting after 9s without a successful probe"
probe=$(printf '%s\n' "$line" | sed -n 's/.*via statfs every \([0-9]*\)s.*/\1/p')
pet=$(printf '%s\n' "$line" | sed -n 's/.*petting \/dev\/watchdog0 every \([0-9]*\)s.*/\1/p')
stale=$(printf '%s\n' "$line" | sed -n 's/.*stop petting after \([0-9]*\)s.*/\1/p')

for v in "$probe" "$pet" "$stale"; do
    case "$v" in
        ''|*[!0-9]*) skip "could not parse the effective timings from: $line" ;;
    esac
done

fail_terms=0
term() { echo "FAIL  $*" >&2; fail_terms=1; }

# The same three invariants the daemon validates, re-derived from the
# timeout the KERNEL reports rather than the one the daemon read. A
# daemon watching the wrong device would satisfy its own check and fail
# this one.
if [ "$stale" -ge "$timeout" ]; then
    term "staleness tolerance ${stale}s is not under the ${timeout}s hardware timeout — the board resets before the daemon decides anything."
fi
if [ "$probe" -ge "$stale" ]; then
    term "probe interval ${probe}s is not under the ${stale}s staleness tolerance — a healthy host goes stale between probes."
fi
if [ $((pet * 2)) -ge "$timeout" ]; then
    term "pet interval ${pet}s is not under half the ${timeout}s hardware timeout — one missed tick resets the board."
fi

if [ "$fail_terms" -ne 0 ]; then
    echo "  The daemon is running on terms that cannot work on this device. On a host" >&2
    echo "  whose watchdog caps below the tuned defaults, the pre-#661 build refused to" >&2
    echo "  start; a build that neither scales nor refuses is worse than either." >&2
    exit 1
fi

echo "ok    effective timings fit the device: probe ${probe}s, pet ${pet}s, stale ${stale}s vs a ${timeout}s timeout"
echo "check-host-watchdog: the host is watched."
