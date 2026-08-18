#!/bin/bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Give the netbooted runner a real block device for /var/lib/docker.
#
# Run this ON the Pi, once. Everything it changes lives in the NFS root, so it
# survives reboots; re-running it is safe and will not reformat an existing
# filesystem.
#
# Why this exists: the runner is diskless by design (#531), but Docker's
# overlay2 storage driver does not support NFS. It needs a block device, and a
# Pi 4 has no PCIe to plug one into -- so the block device is an iSCSI LUN
# served by the same host that serves the root filesystem.
set -euo pipefail

SERVER_IP=${SERVER_IP:?SERVER_IP is required}
TARGET_IQN=${TARGET_IQN:-iqn.2026-08.local.netboot:rpi-docker}
FS_LABEL=${FS_LABEL:-dockerstore}
MOUNTPOINT=/var/lib/docker

log() { printf '[storage] %s\n' "$*" >&2; }
die() { printf '[storage] ERROR: %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root"

log "installing open-iscsi"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq open-iscsi >/dev/null

log "discovering ${TARGET_IQN} on ${SERVER_IP}"
iscsiadm -m discovery -t sendtargets -p "${SERVER_IP}" >/dev/null

# node.startup=automatic is what makes iscsi.service log this session back in on
# every boot; without it the mount succeeds once and never again.
iscsiadm -m node -T "${TARGET_IQN}" -p "${SERVER_IP}" --op update -n node.startup -v automatic
iscsiadm -m node -T "${TARGET_IQN}" -p "${SERVER_IP}" --login 2>/dev/null || log "already logged in"

# Wait for the SCSI device to appear, then find it by its iSCSI session rather
# than assuming /dev/sda -- any USB stick plugged in later would take that name.
log "waiting for the LUN to appear"
DEV=""
for _ in $(seq 1 30); do
    DEV=$(find /sys/class/iscsi_session/*/device/target*/*/block/ -mindepth 1 -maxdepth 1 2>/dev/null | head -1 || true)
    [ -n "${DEV}" ] && { DEV="/dev/$(basename "${DEV}")"; break; }
    sleep 1
done
[ -n "${DEV}" ] || die "LUN did not appear; check the target ACL on the server"
log "LUN is ${DEV} ($(blockdev --getsize64 "${DEV}" | numfmt --to=iec))"

# Never reformat a filesystem that already exists: it holds the runner's image
# cache, and silently wiping it would present as an inexplicably cold build.
if blkid "${DEV}" >/dev/null 2>&1; then
    log "${DEV} already has a filesystem ($(blkid -s TYPE -o value "${DEV}")), leaving it alone"
else
    log "formatting ${DEV} ext4, label ${FS_LABEL}"
    mkfs.ext4 -q -L "${FS_LABEL}" "${DEV}"
fi

# LABEL, not the device node: the kernel name depends on probe order.
log "wiring fstab"
mkdir -p "${MOUNTPOINT}"
sed -i "\#${MOUNTPOINT}#d" /etc/fstab
cat >> /etc/fstab <<EOF
# Docker's overlay2 cannot run on the NFS root; this is an iSCSI LUN served by
# the netboot server. _netdev and the iscsi dependency keep it ordered after
# the session is logged in.
LABEL=${FS_LABEL} ${MOUNTPOINT} ext4 _netdev,noatime,x-systemd.requires=iscsi.service 0 0
EOF

systemctl daemon-reload
mountpoint -q "${MOUNTPOINT}" || mount "${MOUNTPOINT}"
log "mounted: $(findmnt -no SOURCE,FSTYPE,SIZE "${MOUNTPOINT}")"

# Docker must not start before its storage is there, or it initialises a fresh
# graph on the NFS root and then hides the real one once the mount lands.
log "ordering docker after the mount"
mkdir -p /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/10-wait-for-storage.conf <<EOF
[Unit]
RequiresMountsFor=${MOUNTPOINT}
EOF
systemctl daemon-reload

log "done. ${MOUNTPOINT} is a real block device."
