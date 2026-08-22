#!/usr/bin/env bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# The arm64 host's NFS-outage watchdog is wired across four files, and
# every way of getting it wrong is silent (#632).
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
# Usage: check-pi-watchdog-wiring.sh [<netboot-dir>]
# Exit: 0 wired, 1 drift, 2 cannot check.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2
DIR="${1:-test/arm64-netboot}"

PATCH="$DIR/patch-target.sh"
DOCKERFILE="$DIR/Dockerfile"
SRC="$DIR/nfs-watchdog/main.go"

for f in "$PATCH" "$DOCKERFILE" "$SRC"; do
    [ -f "$f" ] || { echo "check-pi-watchdog-wiring: $f does not exist" >&2; exit 2; }
done

fail=0
note() { echo "FAIL  $*" >&2; fail=1; }

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

if [ "$fail" -ne 0 ]; then
    exit 1
fi
echo "PASS  the Pi's NFS-outage watchdog is wired end to end"
