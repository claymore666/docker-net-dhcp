#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The arm64 host's NFS-outage watchdog, checked from both ends. #745 merged
# the tree-side gate (#632) and the host-side gate (#654, #661, #677) into
# one script with one idiom; the split between them is real and stays:
#
#   --tree  reads the PROVISIONER, in the tree. Fails only with a commit.
#   --host  reads the RUNNING host. Can fail with nobody touching the tree.
#
# There is no default mode: a bare call is a usage error (exit 2), so a
# lane that forgets to name its side runs neither.
#
# ============ --tree: the wiring in the tree (#632) ============
#
#
# WHY THIS IS A GATE AND NOT A COMMENT
#
# nfs-watchdog only works if all of these hold at once:
#
#   1. systemd RELEASES /dev/watchdog (RuntimeWatchdogSec=0). Only one
#      process may hold it. Without the drop-in the service gets EBUSY,
#      its unit fails, and the host runs unprotected while every file
#      involved looks correctly installed.
#   2. The unit is INSTALLED into a .wants directory. Writing a unit
#      file that nothing pulls in means systemd has released the device
#      and nobody pets it — a healthy host then resets about a minute
#      after boot. That is the opposite failure and it is worse.
#   3. ExecStart points at the path the provisioner actually installs.
#   4. LimitMEMLOCK=infinity. nfs-watchdog calls mlockall and treats
#      failure as fatal, on purpose: an unpinned petter blocks on the
#      share it is watching. This one is insurance rather than currently
#      load-bearing — the unit runs as root and CAP_IPC_LOCK bypasses
#      RLIMIT_MEMLOCK, so mlockall succeeds without it today (measured).
#      It is checked so the guarantee cannot come to depend on running
#      as root without anyone noticing.
#   5. The netboot image builds the binary and ships it where the
#      provisioner installs it from.
#   6. The program stays stdlib-only. The image compiles it as a
#      throwaway module, because this build context is the netboot
#      directory rather than the repo root — an added dependency breaks
#      that build. The netboot-image workflow compiles it now, so this
#      is no longer the only thing between a dependency and a host that
#      cannot be reprovisioned — but that workflow is path-filtered and
#      this gate is not, and a check that names the rule beats a Go
#      compile error three layers inside a docker build.
#   7. The unit stays OUT of the shutdown ordering. systemd.special(7)
#      names Before=shutdown.target plus Conflicts=shutdown.target as the
#      idiom for a unit that should be stopped before shutdown proceeds;
#      both were once written here explicitly, under a comment claiming
#      the unit survives shutdown. It did not: every reboot stopped it at
#      the first instant, the daemon disarmed on SIGTERM, and a shutdown
#      that then blocked on the dead share hung with nothing armed — the
#      exact case the unit exists for. Measured on the host as a
#      14-minute hang, 2026-08-20.
#
# Points 1 and 2 are the pair worth the whole script: they fail in
# OPPOSITE directions, so a reader checking for one can be satisfied
# while the other is broken.
#
# The other half of point 7 is not greppable and is not checked here: the
# daemon must disarm only while the filesystem still answers, because
# shutdown sends the same SIGTERM an operator does. That one is pinned by
# TestRun_DisarmsOnlyWhileTheFilesystemAnswers in nfs-watchdog.
#
# ============ --host: the running host (#654, #677) ============
#
#
# WHY A SECOND MODE
#
# --tree reads the provisioner. It proves the tree
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
#
# Usage: check-pi-watchdog.sh --tree [<netboot-dir>]
#        check-pi-watchdog.sh --host
# Env (--host): HOST_WATCHDOG_SYSFS  default /sys/class/watchdog/watchdog0
#               HOST_WATCHDOG_KMSG   default /dev/kmsg
#               STRICT               1 = report "cannot check" as a failure
# Exit --tree: 0 wired, 1 drift, 2 cannot check.
# Exit --host: 0 armed, held by nfs-watchdog, on terms that fit the device
#              1 unarmed, held by something else, or on terms that cannot work
#              2 cannot check (no watchdog device, or the ring buffer wrapped)
set -uo pipefail
# shellcheck source=scripts/gatelib.sh
. "$(dirname "${BASH_SOURCE[0]}")/gatelib.sh" || exit 2

fail() { echo "FAIL  $*" >&2; }

tree_side() {
    cd "$(dirname "$0")/.." || exit 2
    DIR="${1:-test/arm64-netboot}"

    PATCH="$DIR/patch-target.sh"
    DOCKERFILE="$DIR/Dockerfile"
    SRC="$DIR/nfs-watchdog/main.go"

    for f in "$PATCH" "$DOCKERFILE" "$SRC"; do
        [ -f "$f" ] || { echo "check-pi-watchdog --tree: $f does not exist" >&2; exit 2; }
    done

    tree_fail=0
    note() { fail "$@"; tree_fail=1; }

    # COMMENTS ARE NOT WIRING. Every match below reads the file with comment
    # lines removed, and anchors wherever the token's real position allows.
    #
    # Unanchored substring matches accepted a line that merely TALKED about
    # the setting. patch-target.sh:147 explains the systemd default in a
    # comment naming RuntimeWatchdogSec, one screen above the drop-in that
    # actually writes it; delete the drop-in and the prose alone kept this
    # green. Check 4 already got this right (`^LimitMEMLOCK=infinity`), so
    # the file carried its own correct form the whole time.
    uncommented() { grep -v '^[[:space:]]*#' "$1"; }

    # `grep -E ... >/dev/null`, never `grep -qE`, on the right-hand side of
    # these pipes: -q exits at the first match and SIGPIPEs `uncommented`,
    # so under `pipefail` the pipeline reports FAILURE exactly when the
    # wiring was found. Reading to EOF and discarding gives the real status.
    # scripts/check-pipefail-consumers.sh gates this repo-wide, and caught
    # this while the anchoring above was being written.

    # 1. systemd must be told to let go of the device.
    #    Anchored both ends: it is written at column 0 into a drop-in, so
    #    there is no reason to accept it anywhere else, and `=0` must be the
    #    whole value rather than the start of `=0m`.
    if uncommented "$PATCH" | grep -E '^RuntimeWatchdogSec=0[[:space:]]*$' >/dev/null; then
        echo "ok    systemd releases /dev/watchdog (RuntimeWatchdogSec=0)"
    else
        note "$PATCH does not write a RuntimeWatchdogSec=0 drop-in."
        echo "  systemd keeps /dev/watchdog0, nfs-watchdog gets EBUSY, its unit fails," >&2
        echo "  and the host is unprotected while every file here looks installed." >&2
    fi

    # 2. ...and something must actually pull the unit in.
    #    Not anchorable — the link path appears mid-command, indented — so
    #    comment-stripping is the whole guard here. The dots are escaped:
    #    unescaped they matched any character, which is loose for no gain.
    if uncommented "$PATCH" \
         | grep -E 'sysinit\.target\.wants/nfs-watchdog\.service|systemctl enable nfs-watchdog' \
           >/dev/null; then
        echo "ok    the unit is enabled (linked into a .wants directory)"
    else
        note "nfs-watchdog.service is written but never enabled."
        echo "  systemd has released the watchdog and nothing pets it, so a HEALTHY" >&2
        echo "  host resets about a minute after boot. This is the opposite failure" >&2
        echo "  to the one above, which is why both are checked." >&2
    fi

    # 3. ExecStart must match what gets installed.
    installed=$(grep -oE 'install -D -m [0-7]+ "\$\{TEMPLATES\}/nfs-watchdog" "\$\{NFSROOT_DIR\}[^"]*"' "$PATCH" \
        | grep -oE '\$\{NFSROOT_DIR\}[^"]*' | sed 's|${NFSROOT_DIR}||')
    execstart=$(grep -oE '^ExecStart=\S+' "$PATCH" | head -1 | cut -d= -f2-)
    if [ -z "$installed" ] || [ -z "$execstart" ]; then
        note "could not find both the install path and ExecStart in $PATCH (installed='$installed' ExecStart='$execstart')"
    elif [ "$installed" != "$execstart" ]; then
        note "ExecStart ($execstart) is not where the binary is installed ($installed)."
    else
        echo "ok    ExecStart matches the installed path ($execstart)"
    fi

    # 4. mlockall needs the limit raised, or the service dies at startup.
    if grep -q '^LimitMEMLOCK=infinity' "$PATCH"; then
        echo "ok    LimitMEMLOCK=infinity (so pinning does not depend on running as root)"
    else
        note "the unit does not set LimitMEMLOCK=infinity."
        echo "  nfs-watchdog calls mlockall and treats failure as FATAL on purpose." >&2
        echo "  As root this still works (CAP_IPC_LOCK bypasses the limit), so the" >&2
        echo "  line is insurance — but without it the guarantee silently depends on" >&2
        echo "  the unit staying root, and adding User= would stop the service dead." >&2
    fi

    # 5. the image must build and ship the binary the provisioner installs.
    #    Both halves read the Dockerfile without its comments: a commented-out
    #    COPY is exactly the state this check exists to catch, and it used to
    #    satisfy it.
    if uncommented "$DOCKERFILE" | grep 'GOARCH=arm64 go build' >/dev/null \
       && uncommented "$DOCKERFILE" | grep 'netboot-templates/nfs-watchdog' >/dev/null; then
        echo "ok    the netboot image builds nfs-watchdog and ships it to the templates dir"
    else
        note "$DOCKERFILE does not both build nfs-watchdog for arm64 and copy it into the templates directory."
        echo "  patch-target.sh installs it from \${TEMPLATES}/nfs-watchdog; without" >&2
        echo "  both halves, provisioning fails on a missing file." >&2
    fi

    # 6. stdlib-only, or the throwaway-module build in the image breaks.
    #    Read the import block rather than every line mentioning a quote.
    imports=$(awk '/^import \(/{f=1;next} f&&/^\)/{f=0} f' "$SRC" \
        | grep -oE '"[a-zA-Z0-9_/.-]+"' | tr -d '"' | grep '\.' || true)
    if [ -n "$imports" ]; then
        note "nfs-watchdog imports non-stdlib packages:"
        printf '  %s\n' $imports >&2
        echo "  The netboot image compiles it as a throwaway module (this build" >&2
        echo "  context is $DIR, not the repo root), so a dependency breaks that" >&2
        echo "  build — and that image is how the host is reprovisioned, so the" >&2
        echo "  cost of finding out later is a boot server that will not rebuild." >&2
    else
        echo "ok    nfs-watchdog is stdlib-only (the image builds it as its own module)"
    fi

    # 7. the unit must not be ordered against shutdown, or systemd stops it
    #    (and the daemon hands back the device) before shutdown proceeds.
    shutdown_deps=$(awk '/^\[Unit\]/{u=1;next} /^\[/{u=0} u' "$PATCH" \
        | grep -E '^(Before|Conflicts)=.*shutdown\.target' || true)
    if [ -n "$shutdown_deps" ]; then
        note "nfs-watchdog.service is ordered against shutdown.target:"
        printf '  %s\n' $shutdown_deps >&2
        echo "  systemd.special(7): that pair is the idiom for \"stop me before" >&2
        echo "  shutdown proceeds\". systemd stops the unit at the first instant of" >&2
        echo "  every reboot, the daemon closes the device, and a shutdown that then" >&2
        echo "  blocks on a dead NFS server hangs forever with NOTHING armed to end" >&2
        echo "  it — which is one of the two cases this watchdog exists for." >&2
        echo "  DefaultDependencies=no on its own keeps the unit out of that." >&2
    else
        echo "ok    the unit is not ordered against shutdown.target (it survives shutdown)"
    fi

    if [ "$tree_fail" -ne 0 ]; then
        exit 1
    fi
    echo "PASS  the Pi's NFS-outage watchdog is wired end to end"
}

host_side() {
    SYSFS="${HOST_WATCHDOG_SYSFS:-/sys/class/watchdog/watchdog0}"
    KMSG="${HOST_WATCHDOG_KMSG:-/dev/kmsg}"

    # STRICT=1 turns missing evidence into a failure. The default is 2 so a
    # caller can tell "this host is broken" from "this run could not tell",
    # which is the distinction the whole gate is built around.
    skip() {
        echo "check-pi-watchdog --host: cannot check — $*" >&2
        [ "${STRICT:-0}" = "1" ] && { echo "  STRICT=1: reporting that as a failure." >&2; exit 1; }
        exit 2
    }

    # --- 1. the kernel's view ----------------------------------------------

    [ -d "$SYSFS" ] || skip "no watchdog device at $SYSFS (expected on hosts without one)"

    state=$(cat "$SYSFS/state" 2>/dev/null)
    timeout=$(cat "$SYSFS/timeout" 2>/dev/null)
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
    term() { fail "$@"; fail_terms=1; }

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
    echo "check-pi-watchdog --host: the host is watched."
}

case "${1:-}" in
    --tree) shift; tree_side "$@" ;;
    --host) [ "$#" -eq 1 ] || gate_refuse "usage: check-pi-watchdog.sh --host takes no argument"; host_side ;;
    *) gate_refuse "usage: check-pi-watchdog.sh --tree [<netboot-dir>] | --host (a mode is required)" ;;
esac
