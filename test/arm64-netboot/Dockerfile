# Netboot server for the native arm64 CI runner lane (#531).
#
# Serves a Raspberry Pi its boot firmware over TFTP and its root filesystem
# over NFS, so the runner has no local OS media and cannot drift between
# boots. Deliberately carries no DHCP server: the site DHCP server keeps
# that role and the Pi is pointed here by TFTP_IP in its bootloader EEPROM.
FROM debian:trixie-slim

# dnsmasq   - TFTP only (DHCP and DNS are disabled in the config)
# nfs-*     - kernel NFS server; klibc nfsmount in the Pi initramfs is v2/v3
# tgt       - userspace iSCSI target; Docker's overlay2 cannot run on NFS
# mtools    - read the image's FAT boot partition without a loop mount
# xz/curl   - fetch and decompress the Raspberry Pi OS image
# rsync     - copy the root partition preserving xattrs and capabilities
RUN apt-get update && apt-get install -y --no-install-recommends \
        dnsmasq \
        tgt \
        nfs-kernel-server \
        rpcbind \
        mtools \
        xz-utils \
        curl \
        ca-certificates \
        rsync \
        kmod \
        util-linux \
        fdisk \
        e2fsprogs \
        dosfstools \
        iproute2 \
        procps \
        gettext-base \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY provision.sh  /usr/local/bin/provision.sh
COPY patch-target.sh /usr/local/bin/patch-target.sh
COPY iscsi-target.sh /usr/local/bin/iscsi-target.sh
COPY sync-boot.sh /usr/local/bin/sync-boot.sh
COPY templates/    /usr/local/share/netboot-templates/
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/provision.sh \
        /usr/local/bin/patch-target.sh /usr/local/bin/iscsi-target.sh \
        /usr/local/bin/sync-boot.sh

# cache/  downloaded .img.xz        tftp/  boot firmware served over TFTP
# nfsroot/ the Pi's root filesystem
VOLUME ["/srv/netboot"]

# 69/udp TFTP, 111 rpcbind, 2049 NFS, 20048 mountd (pinned for firewalling)
# 3260 iSCSI
EXPOSE 69/udp 111/tcp 111/udp 2049/tcp 2049/udp 20048/tcp 20048/udp 3260/tcp

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
