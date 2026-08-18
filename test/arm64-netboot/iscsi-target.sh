#!/bin/bash
# Export one iSCSI LUN for the runner's /var/lib/docker.
#
# The runner is diskless, but Docker's overlay2 storage driver cannot run on
# NFS: it needs a real block device. A Pi 4 has no PCIe, so the block device is
# served from here rather than plugged into the Pi. The backing file stays on
# this server, which keeps the "the server owns the runner's state" property --
# resetting Docker's storage is an rm on this host, not a trip to the Pi.
set -euo pipefail

NETBOOT_DIR=${NETBOOT_DIR:-/srv/netboot}
LUN_FILE="${NETBOOT_DIR}/docker-lun.img"
LUN_SIZE=${DOCKER_LUN_SIZE:-24G}
TARGET_IQN=${TARGET_IQN:-iqn.2026-08.local.netboot:rpi-docker}

log() { printf '[iscsi] %s\n' "$*" >&2; }
die() { printf '[iscsi] ERROR: %s\n' "$*" >&2; exit 1; }

[ -n "${ISCSI_ALLOWED_CLIENTS:-}" ] || ISCSI_ALLOWED_CLIENTS="${SERVER_IP%.*}.0/24"

# Sparse: it consumes only what Docker actually writes, so a generous size
# costs nothing up front. Never recreate it if present -- that is the runner's
# image cache and recreating it silently would look like a mysteriously cold
# build rather than a wipe.
if [ ! -e "${LUN_FILE}" ]; then
    log "creating ${LUN_SIZE} sparse backing file ${LUN_FILE}"
    truncate -s "${LUN_SIZE}" "${LUN_FILE}"
else
    log "reusing existing backing file ${LUN_FILE} ($(du -h --apparent-size "${LUN_FILE}" | cut -f1) apparent, $(du -h "${LUN_FILE}" | cut -f1) actual)"
fi

log "starting tgtd"
tgtd || die "tgtd failed to start"
# tgtd forks; it is not ready to accept tgtadm calls immediately.
for _ in $(seq 1 20); do
    tgtadm --lld iscsi --op show --mode target >/dev/null 2>&1 && break
    sleep 0.5
done
tgtadm --lld iscsi --op show --mode target >/dev/null 2>&1 || die "tgtd did not become ready"

if ! tgtadm --lld iscsi --op show --mode target | grep -q "${TARGET_IQN}"; then
    log "creating target ${TARGET_IQN}"
    tgtadm --lld iscsi --op new --mode target --tid 1 -T "${TARGET_IQN}"
    tgtadm --lld iscsi --op new --mode logicalunit --tid 1 --lun 1 -b "${LUN_FILE}"
    tgtadm --lld iscsi --op bind --mode target --tid 1 -I "${ISCSI_ALLOWED_CLIENTS}"
    log "target bound for ${ISCSI_ALLOWED_CLIENTS}"
fi

tgtadm --lld iscsi --op show --mode target | grep -E "Target |LUN:|Backing store path|ACL" || true
