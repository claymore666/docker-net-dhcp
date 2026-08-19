#!/bin/bash
# Copyright the docker-net-dhcp contributors.
# SPDX-License-Identifier: GPL-3.0-only

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

# ------------------------------------------------------------------- AppArmor
# Raspberry Pi OS ships AppArmor compiled in but disabled; Debian and Ubuntu
# hosts have it on. Leaving it off would make this runner quietly more
# permissive than the amd64 lane, so a confinement bug could pass here and fail
# there -- the opposite of what a second architecture is for.
#
# Nothing in the default profile set confines sshd, init or dhcpcd, so turning
# it on cannot lock the runner out.
if [ "${PI_APPARMOR:-1}" = "1" ]; then
    APPARMOR_ARG=" apparmor=1 security=apparmor"
    log "AppArmor: enabled (matches the amd64 hosts)"
else
    APPARMOR_ARG=""
    log "AppArmor: left disabled (PI_APPARMOR=0)"
fi

# ------------------------------------------------------------------ cmdline.txt
# root=/dev/nfs is what makes the stock initramfs select its NFS boot script.
# The mount is performed by klibc nfsmount, which speaks NFSv2/v3 only, so the
# options are klibc syntax (v3,tcp) rather than the kernel's vers=3.
# cgroup_enable/cgroup_memory are required for Docker's memory accounting on
# Raspberry Pi kernels; without them the runner silently loses memory limits.
log "writing cmdline.txt (nfsroot=${SERVER_IP}:${NFS_EXPORT})"
cat > "${TFTP_DIR}/cmdline.txt" <<EOF
console=serial0,115200 console=tty1 root=/dev/nfs nfsroot=${SERVER_IP}:${NFS_EXPORT},v3,tcp rw ${IP_ARG} rootwait cgroup_enable=memory cgroup_memory=1 net.ifnames=0${APPARMOR_ARG}
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

# Two units cannot succeed on a diskless root and fail on every boot otherwise:
# growfs-root tries to grow a filesystem that is an NFS export, and
# networkd-wait-online waits for a networkd that does not manage the boot
# interface (it is deliberately unmanaged, below).
log "masking units that cannot succeed on a diskless root"
ln -sf /dev/null "${NFSROOT_DIR}/etc/systemd/system/systemd-growfs-root.service"
ln -sf /dev/null "${NFSROOT_DIR}/etc/systemd/system/systemd-networkd-wait-online.service"

# ------------------------------------------------------------- nfs-watchdog
# The host's root filesystem is an NFS export. When the server goes away
# the kernel stays up and every process blocks: the board answers ping,
# accepts TCP on 22, and never produces an ssh banner, because sshd
# cannot re-exec its own binary off the share. Only a power cycle clears
# it, and this machine is not next to anyone.
#
# The SoC watchdog alone does NOT fix that, and it is already enabled —
# the image ships /usr/lib/systemd/system.conf.d/40-rpi-enable-watchdog.conf
# with RuntimeWatchdogSec=1m and PID 1 holds the device with a 60s
# timeout. It was armed through the outage that motivated this and the
# board still needed hands. systemd is resident in memory and its event
# loop never touches the root filesystem, so it keeps petting while
# everything that does I/O is stuck: the board looks healthy to the
# watchdog for as long as the outage lasts.
#
# So the device is handed to nfs-watchdog, which pets it only while
# statfs on / still reaches the server. Both halves are required and each
# fails silently on its own:
#   - without the drop-in, systemd keeps the device, nfs-watchdog gets
#     EBUSY, its unit fails, and the host is unprotected while looking
#     configured
#   - without the unit, nothing pets at all and a healthy host resets a
#     minute after boot
# scripts/check-pi-watchdog-wiring.sh exists because that pair is exactly
# the kind of thing prose cannot hold.
log "handing /dev/watchdog from systemd to nfs-watchdog"
mkdir -p "${NFSROOT_DIR}/etc/systemd/system.conf.d"
cat > "${NFSROOT_DIR}/etc/systemd/system.conf.d/50-nfs-watchdog.conf" <<'EOF'
# Overrides 40-rpi-enable-watchdog.conf. Only one process may hold
# /dev/watchdog0, and systemd's petting is unconditional: it keeps the
# board alive through an NFS outage because PID 1 never touches the
# filesystem. nfs-watchdog.service takes the device instead and pets it
# only while the root filesystem answers (#632).
[Manager]
RuntimeWatchdogSec=0
EOF

install -D -m 0755 "${TEMPLATES}/nfs-watchdog" "${NFSROOT_DIR}/usr/local/sbin/nfs-watchdog"

cat > "${NFSROOT_DIR}/etc/systemd/system/nfs-watchdog.service" <<'EOF'
[Unit]
Description=Reset this host when its NFS root stops answering (#632)
Documentation=https://github.com/claymore666/docker-net-dhcp/issues/632
# Starts before anything that could block on the share, and is not
# stopped during shutdown -- a shutdown that hangs on a dead NFS server
# is one of the cases this exists for.
DefaultDependencies=no
After=sysinit.target
Before=shutdown.target
Conflicts=shutdown.target

[Service]
Type=simple
ExecStart=/usr/local/sbin/nfs-watchdog
# Insurance, not load-bearing today, and said plainly because the
# opposite is easy to assume: this unit runs as root, and CAP_IPC_LOCK
# bypasses RLIMIT_MEMLOCK, so mlockall already succeeds without it
# (measured on the host: default limit 8192K, mlockall fine as root, and
# it fails immediately as an ordinary user). The line exists so the
# guarantee does not silently depend on running as root — the day anyone
# adds User= or trims CapabilityBoundingSet, nfs-watchdog would refuse
# to start rather than run unpinned, which is the correct but expensive
# way to discover this.
LimitMEMLOCK=infinity
# Being OOM-killed during a job would silently remove the protection.
OOMScoreAdjust=-1000
Restart=always
RestartSec=10s
# It logs to /dev/kmsg itself: journald can block a writer when its
# buffers fill, and its storage is on the share this distrusts.
StandardOutput=null
StandardError=null

[Install]
WantedBy=sysinit.target
EOF

mkdir -p "${NFSROOT_DIR}/etc/systemd/system/sysinit.target.wants"
ln -sf /etc/systemd/system/nfs-watchdog.service \
    "${NFSROOT_DIR}/etc/systemd/system/sysinit.target.wants/nfs-watchdog.service"

# ------------------------------------------------------------ initramfs-tools
# mkinitramfs cannot resolve a device for / when the root is an NFS export, and
# its postinst then fails. That breaks *every* later apt operation on the
# runner, not just the package that triggered it -- installing open-iscsi was
# enough to wedge dpkg. BOOT=nfs tells it not to look for a block device.
log "configuring initramfs-tools for an NFS root"
mkdir -p "${NFSROOT_DIR}/etc/initramfs-tools/conf.d"
cat > "${NFSROOT_DIR}/etc/initramfs-tools/conf.d/netboot.conf" <<'EOF'
# The root filesystem is an NFS export, not a block device. Without BOOT=nfs,
# mkinitramfs fails to determine a device for / and any package triggering an
# initramfs rebuild fails its postinst, wedging dpkg.
#
# Note: a regenerated initramfs lands in /boot/firmware here, which on a
# netbooted root the bootloader never reads. Only the copy in the server's TFTP
# root is used at boot; copy it there if a rebuild ever needs to take effect.
BOOT=nfs
MODULES=most
EOF

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
