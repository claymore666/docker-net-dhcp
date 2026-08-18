#!/bin/bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

# Serve one Raspberry Pi its boot firmware (TFTP) and root filesystem (NFS).
set -euo pipefail

NETBOOT_DIR=${NETBOOT_DIR:-/srv/netboot}
TFTP_DIR="${NETBOOT_DIR}/tftp"
NFSROOT_DIR="${NETBOOT_DIR}/nfsroot"
NFS_EXPORT=${NFS_EXPORT:-${NFSROOT_DIR}}

log() { printf '[netboot] %s\n' "$*" >&2; }
die() { printf '[netboot] ERROR: %s\n' "$*" >&2; exit 1; }

[ -n "${SERVER_IP:-}" ] || die "SERVER_IP is required (the address the Pi reaches this server on)"

# Exporting rw,no_root_squash to the world would hand any host on the segment a
# root-writable filesystem. Default to the server's own /24 rather than '*'.
if [ -z "${NFS_ALLOWED_CLIENTS:-}" ]; then
    NFS_ALLOWED_CLIENTS="${SERVER_IP%.*}.0/24"
    log "NFS_ALLOWED_CLIENTS defaulted to ${NFS_ALLOWED_CLIENTS}"
fi

/usr/local/bin/provision.sh

# ----------------------------------------------------------- server relocation
# A provisioned tree carries the server's address in two client-visible places:
# the kernel command line (nfsroot=) and the Pi's iSCSI node database inside
# the NFS root. When the data directory is copied to a new host, TFTP moves
# with the EEPROM's TFTP_IP but those two silently keep feeding the Pi from
# the OLD server -- the first migration ran exactly that way: TFTP from the
# new box, root filesystem and /var/lib/docker still from the old one.
# SERVER_IP is the single source of truth; rewrite both to match it.
CMDLINE="${TFTP_DIR}/cmdline.txt"
if [ -f "${CMDLINE}" ]; then
    old_ip=$(grep -o 'nfsroot=[0-9.]*' "${CMDLINE}" | cut -d= -f2 || true)
    if [ -n "${old_ip}" ] && [ "${old_ip}" != "${SERVER_IP}" ]; then
        log "relocating: ${old_ip} -> ${SERVER_IP} in cmdline.txt and the iSCSI node db"
        sed -i "s/${old_ip//./\\.}/${SERVER_IP}/g" "${CMDLINE}"
        ISCSI_DB="${NETBOOT_DIR}/nfsroot/var/lib/iscsi"
        if [ -d "${ISCSI_DB}" ]; then
            # children first, and only the basename: parent directory names
            # contain the address too and are renamed on their own turn
            find "${ISCSI_DB}" -depth -name "*${old_ip}*" | while read -r path; do
                dir=$(dirname "${path}"); base=$(basename "${path}")
                mv "${path}" "${dir}/${base//${old_ip}/${SERVER_IP}}"
            done
            grep -rl "${old_ip}" "${ISCSI_DB}" 2>/dev/null | while read -r f; do
                sed -i "s/${old_ip//./\\.}/${SERVER_IP}/g" "${f}"
            done
        fi
    fi
fi

# ------------------------------------------------------------------------ NFS
# no_root_squash is not optional: the Pi runs its entire root filesystem from
# this export and writes to it as uid 0 from the first second of boot.
log "exporting ${NFS_EXPORT} to ${NFS_ALLOWED_CLIENTS}"
cat > /etc/exports <<EOF
${NFS_EXPORT} ${NFS_ALLOWED_CLIENTS}(rw,sync,no_subtree_check,no_root_squash,insecure)
EOF

if ! mountpoint -q /proc/fs/nfsd; then
    modprobe nfsd 2>/dev/null || true
    mount -t nfsd nfsd /proc/fs/nfsd \
        || die "cannot mount nfsd; run with --privileged, and 'modprobe nfsd' on the host"
fi

log "starting rpcbind"
rpcbind -w
# NFSv4 needs a pseudo-root export and buys nothing here: the Pi's initramfs
# mounts with klibc nfsmount, which speaks v3 at most. NFSv2 is not disabled
# because current nfs-utils has removed it outright and rejects -N 2.
log "starting nfsd (v3 only)"
rpc.nfsd -N 4 -V 3 8
log "starting mountd on port 20048"
rpc.mountd -p 20048 -N 4 -V 3
exportfs -ra
exportfs -v

shutdown() {
    log "shutting down"
    exportfs -ua || true
    rpc.nfsd 0 || true
    exit 0
}
trap shutdown TERM INT

# ---------------------------------------------------------------------- iSCSI
# Serves the runner's /var/lib/docker as a real block device. Optional: a Pi
# that only needs to boot does not need it, and it is skipped if disabled.
if [ "${ENABLE_ISCSI:-1}" = "1" ]; then
    /usr/local/bin/iscsi-target.sh
else
    log "iSCSI disabled (ENABLE_ISCSI=0)"
fi

# ----------------------------------------------------------------------- TFTP
# port=0 disables DNS and no DHCP range is configured, so this dnsmasq answers
# TFTP and nothing else. The site DHCP server keeps its role untouched; the Pi
# is pointed here by TFTP_IP in its bootloader EEPROM.
log "serving TFTP from ${TFTP_DIR}"
# DNSMASQ_EXTRA_ARGS is an operator escape hatch and must word-split into
# separate flags, so it is intentionally unquoted.
# shellcheck disable=SC2086
exec dnsmasq \
    --keep-in-foreground \
    --port=0 \
    --enable-tftp \
    --tftp-root="${TFTP_DIR}" \
    --tftp-single-port \
    --log-facility=- \
    --user=root \
    ${DNSMASQ_EXTRA_ARGS:-}
