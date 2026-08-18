#!/bin/bash
# Turn a stock Raspberry Pi OS tree into a netbootable one.
#
# Split out of provision.sh so it can be re-run on its own after changing
# SERVER_IP, the hostname or the SSH key, without re-extracting the image.
set -euo pipefail

NETBOOT_DIR=${NETBOOT_DIR:-/srv/netboot}
TFTP_DIR="${NETBOOT_DIR}/tftp"
NFSROOT_DIR="${NETBOOT_DIR}/nfsroot"
TEMPLATES=/usr/local/share/netboot-templates

PI_HOSTNAME=${PI_HOSTNAME:-rpi-arm64-runner}
PI_USER=${PI_USER:-pi}
NFS_EXPORT=${NFS_EXPORT:-${NFSROOT_DIR}}

log() { printf '[patch] %s\n' "$*" >&2; }
die() { printf '[patch] ERROR: %s\n' "$*" >&2; exit 1; }

[ -n "${SERVER_IP:-}" ]              || die "SERVER_IP is required"
[ -d "${NFSROOT_DIR}/etc" ]          || die "${NFSROOT_DIR} does not look like a root filesystem"
[ -n "${PI_SSH_AUTHORIZED_KEY:-}" ]  || die "PI_SSH_AUTHORIZED_KEY is required (no password login is configured)"

# ------------------------------------------------------------------ addressing
# The runner's own address must not depend on the site DHCP server, because
# the suite this machine exists to run deliberately abuses DHCP (#531). Static
# is therefore the production shape. dhcp is for first-boot bring-up only: the
# kernel takes a lease before / is mounted and nothing ever renews it, so the
# address silently expires on a long-lived runner.
PI_IP_MODE=${PI_IP_MODE:-dhcp}
case "${PI_IP_MODE}" in
    dhcp)
        IP_ARG="ip=dhcp"
        log "addressing: DHCP (bring-up mode; the kernel lease is never renewed)"
        ;;
    static)
        for v in PI_STATIC_IP PI_GATEWAY PI_NETMASK; do
            [ -n "${!v:-}" ] || die "PI_IP_MODE=static requires ${v}"
        done
        # ip=<client>:<server>:<gateway>:<netmask>:<hostname>:<device>:<autoconf>
        IP_ARG="ip=${PI_STATIC_IP}::${PI_GATEWAY}:${PI_NETMASK}:${PI_HOSTNAME}:eth0:off"
        log "addressing: static ${PI_STATIC_IP} via ${PI_GATEWAY}"
        ;;
    *)
        die "PI_IP_MODE must be 'dhcp' or 'static', got '${PI_IP_MODE}'"
        ;;
esac

# ------------------------------------------------------------------ cmdline.txt
# root=/dev/nfs is what makes the stock initramfs select its NFS boot script.
# The mount is performed by klibc nfsmount, which speaks NFSv2/v3 only, so the
# options are klibc syntax (v3,tcp) rather than the kernel's vers=3.
# cgroup_enable/cgroup_memory are required for Docker's memory accounting on
# Raspberry Pi kernels; without them the runner silently loses memory limits.
log "writing cmdline.txt (nfsroot=${SERVER_IP}:${NFS_EXPORT})"
cat > "${TFTP_DIR}/cmdline.txt" <<EOF
console=serial0,115200 console=tty1 root=/dev/nfs nfsroot=${SERVER_IP}:${NFS_EXPORT},v3,tcp rw ${IP_ARG} rootwait cgroup_enable=memory cgroup_memory=1 net.ifnames=0
EOF

# ------------------------------------------------------------------- config.txt
# A netbooted headless Pi has no other way to tell you why it stopped.
if ! grep -q '^enable_uart=1' "${TFTP_DIR}/config.txt"; then
    log "enabling UART console in config.txt"
    printf '\n# Added for netboot: serial console is the only debug channel\nenable_uart=1\n' \
        >> "${TFTP_DIR}/config.txt"
fi

# ---------------------------------------------------------------------- fstab
# The PARTUUID entries refer to the SD card this image was built for. Left in
# place they fail the boot outright, because neither partition exists here.
log "rewriting /etc/fstab for a diskless root"
cat > "${NFSROOT_DIR}/etc/fstab" <<'EOF'
# Netbooted root: / arrives over NFS from the kernel command line and
# /boot/firmware over TFTP, so neither is mounted here.
proc            /proc           proc    defaults          0       0
EOF

# ------------------------------------------------------------------ cloud-init
# The image seeds cloud-init from file:///boot/firmware (see
# /etc/cloud/cloud.cfg.d/99_raspberry-pi.cfg). On a netbooted root that path is
# an ordinary directory in the NFS export, so dropping the seed files there is
# the supported way to create the user rather than editing /etc/shadow by hand.
log "writing cloud-init seed for user '${PI_USER}' on host '${PI_HOSTNAME}'"
mkdir -p "${NFSROOT_DIR}/boot/firmware"

export PI_HOSTNAME PI_USER PI_SSH_AUTHORIZED_KEY
envsubst < "${TEMPLATES}/user-data.tmpl" > "${NFSROOT_DIR}/boot/firmware/user-data"
cp "${TEMPLATES}/meta-data"       "${NFSROOT_DIR}/boot/firmware/meta-data"
cp "${TEMPLATES}/network-config"  "${NFSROOT_DIR}/boot/firmware/network-config"

# --------------------------------------------------------------------- systemd
# userconfig.service is the first-boot account prompt. It expects a console
# operator; on a headless netbooted runner it would block the boot forever,
# and cloud-init has already created the account.
log "masking userconfig.service"
ln -sf /dev/null "${NFSROOT_DIR}/etc/systemd/system/userconfig.service"
rm -f "${NFSROOT_DIR}/etc/systemd/system/multi-user.target.wants/userconfig.service"

# ssh.service ships disabled. sshswitch.service enables it when /boot/firmware/ssh
# exists; both are done here so access does not depend on either one alone.
log "enabling ssh"
mkdir -p "${NFSROOT_DIR}/etc/systemd/system/multi-user.target.wants"
ln -sf /lib/systemd/system/ssh.service \
    "${NFSROOT_DIR}/etc/systemd/system/multi-user.target.wants/ssh.service"
touch "${NFSROOT_DIR}/boot/firmware/ssh"

# wpa_supplicant has no radio to manage on a wired netbooted runner and only
# adds a failed unit to every boot.
rm -f "${NFSROOT_DIR}/etc/systemd/system/multi-user.target.wants/wpa_supplicant.service"

# -------------------------------------------------------------- NetworkManager
# The boot interface carries the NFS root. NetworkManager would re-run DHCP on
# it and drop the link mid-flight; the kernel already configured it from the
# ip= argument before / existed, so it is marked unmanaged here.
log "marking eth0 unmanaged by NetworkManager"
mkdir -p "${NFSROOT_DIR}/etc/NetworkManager/conf.d"
cat > "${NFSROOT_DIR}/etc/NetworkManager/conf.d/00-netboot-unmanaged.conf" <<'EOF'
[keyfile]
unmanaged-devices=interface-name:eth0
EOF

# ------------------------------------------------------------ TFTP serial alias
# The Pi bootloader looks for files under a directory named after the last 8
# hex digits of its serial unless TFTP_PREFIX=1 flattens that. Supporting both
# means the EEPROM can be left at its default prefix behaviour.
if [ -n "${PI_SERIAL:-}" ]; then
    log "adding TFTP serial prefix directory ${PI_SERIAL}"
    ln -sfn . "${TFTP_DIR}/${PI_SERIAL}"
fi

log "target patched"
