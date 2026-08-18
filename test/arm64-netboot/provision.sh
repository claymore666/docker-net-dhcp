#!/bin/bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Build the TFTP tree and the NFS root from a stock Raspberry Pi OS image.
#
# Runs once per volume; re-run with FORCE_PROVISION=1 to rebuild from scratch.
# Everything here is done on the x86 host with plain file edits, so no arm64
# emulation is involved and nothing in the target rootfs is ever executed.
set -euo pipefail

NETBOOT_DIR=${NETBOOT_DIR:-/srv/netboot}
CACHE_DIR="${NETBOOT_DIR}/cache"
TFTP_DIR="${NETBOOT_DIR}/tftp"
NFSROOT_DIR="${NETBOOT_DIR}/nfsroot"
STAMP="${NETBOOT_DIR}/.provisioned"

log() { printf '[provision] %s\n' "$*" >&2; }
die() { printf '[provision] ERROR: %s\n' "$*" >&2; exit 1; }

if [ -e "${STAMP}" ] && [ "${FORCE_PROVISION:-0}" != "1" ]; then
    log "already provisioned ($(cat "${STAMP}")); set FORCE_PROVISION=1 to rebuild"
    exit 0
fi

[ -n "${SERVER_IP:-}" ] || die "SERVER_IP is required (the address the Pi reaches this server on)"

mkdir -p "${CACHE_DIR}" "${TFTP_DIR}" "${NFSROOT_DIR}"

# ---------------------------------------------------------------- fetch image
IMAGE_XZ="${CACHE_DIR}/$(basename "${RPIOS_IMAGE_URL}")"
IMAGE_RAW="${IMAGE_XZ%.xz}"

if [ ! -e "${IMAGE_RAW}" ]; then
    if [ ! -e "${IMAGE_XZ}" ]; then
        log "downloading ${RPIOS_IMAGE_URL}"
        curl -fSL --retry 3 -o "${IMAGE_XZ}" "${RPIOS_IMAGE_URL}"
    else
        log "using cached ${IMAGE_XZ}"
    fi

    if [ -n "${RPIOS_IMAGE_SHA256:-}" ]; then
        log "verifying sha256"
        echo "${RPIOS_IMAGE_SHA256}  ${IMAGE_XZ}" | sha256sum -c - \
            || die "checksum mismatch on ${IMAGE_XZ}"
    else
        log "WARNING: RPIOS_IMAGE_SHA256 unset, image not verified"
    fi

    log "decompressing (this takes a minute)"
    xz -dk -T0 "${IMAGE_XZ}"
fi

# ------------------------------------------------------- unpack the partitions
# losetup -P exposes p1 (FAT boot) and p2 (ext4 root). rsync -aHAX on the root
# preserves file capabilities, which a debugfs dump would silently drop.
LOOP=$(losetup -Pf --show "${IMAGE_RAW}") || die "losetup failed; run the container with --privileged"
cleanup() {
    if mountpoint -q /mnt/rpi-boot; then umount /mnt/rpi-boot || true; fi
    if mountpoint -q /mnt/rpi-root; then umount /mnt/rpi-root || true; fi
    if [ -n "${LOOP:-}" ]; then losetup -d "${LOOP}" || true; fi
}
trap cleanup EXIT
log "image attached at ${LOOP}"

mkdir -p /mnt/rpi-boot /mnt/rpi-root
mount -o ro "${LOOP}p1" /mnt/rpi-boot || die "cannot mount boot partition"
mount -o ro "${LOOP}p2" /mnt/rpi-root || die "cannot mount root partition"

log "copying boot firmware -> ${TFTP_DIR}"
rsync -a --delete /mnt/rpi-boot/ "${TFTP_DIR}/"

log "copying root filesystem -> ${NFSROOT_DIR} (a few minutes)"
rsync -aHAX --delete --numeric-ids /mnt/rpi-root/ "${NFSROOT_DIR}/"

umount /mnt/rpi-boot
umount /mnt/rpi-root
losetup -d "${LOOP}"
LOOP=""
trap - EXIT

/usr/local/bin/patch-target.sh
date -u +%Y-%m-%dT%H:%M:%SZ > "${STAMP}"
log "provisioning complete"
