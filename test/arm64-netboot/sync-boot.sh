#!/bin/bash
# Publish the runner's boot files from the NFS root into the TFTP root.
#
# Run this on the SERVER after anything on the Pi writes to /boot/firmware --
# most importantly a kernel upgrade, but also an initramfs rebuild.
#
# Why it is needed at all: on a netbooted Pi the bootloader reads kernel,
# initramfs and device trees over TFTP, while the OS installs them into
# /boot/firmware inside the NFS root. Those are two different directories.
# Everything the OS writes there is inert until it is copied here, and the
# failure is nasty rather than obvious -- apt upgrades the kernel to 6.18.39
# and installs matching modules, the Pi keeps booting 6.18.34 from TFTP, and
# module loads start failing on a machine that looks perfectly healthy.
#
# After running this, REBOOT the Pi so the kernel and its modules match again.
set -euo pipefail

NETBOOT_DIR=${NETBOOT_DIR:-/srv/netboot}
TFTP_DIR="${NETBOOT_DIR}/tftp"
FW_DIR="${NETBOOT_DIR}/nfsroot/boot/firmware"

log() { printf '[sync-boot] %s\n' "$*" >&2; }
die() { printf '[sync-boot] ERROR: %s\n' "$*" >&2; exit 1; }

[ -d "${FW_DIR}" ]   || die "${FW_DIR} not found"
[ -d "${TFTP_DIR}" ] || die "${TFTP_DIR} not found; provision first"

# cmdline.txt and config.txt are OURS, not the distribution's: they carry the
# nfsroot= arguments and the UART setting. Copying the OS's copies over them
# would point the Pi back at a PARTUUID that does not exist here and strand it.
log "publishing boot files (kernel, initramfs, dtb, overlays, firmware blobs)"
rsync -a --delete \
    --exclude 'cmdline.txt' \
    --exclude 'config.txt' \
    --exclude 'user-data' \
    --exclude 'meta-data' \
    --exclude 'network-config' \
    --exclude 'ssh' \
    --exclude 'pieeprom.*' \
    --exclude 'recovery.*' \
    "${FW_DIR}/" "${TFTP_DIR}/"

log "kernels now published:"
for k in "${TFTP_DIR}"/kernel*.img "${TFTP_DIR}"/initramfs*; do
    [ -e "${k}" ] || continue
    printf '  %-20s %s\n' "$(basename "${k}")" "$(date -r "${k}" -u +%Y-%m-%dT%H:%MZ)" >&2
done

log "our cmdline.txt is intact:"
sed 's/^/  /' "${TFTP_DIR}/cmdline.txt" >&2

log "done. Reboot the Pi so its kernel and modules match."
