# Build stage for nfs-watchdog, the resident petter that resets the Pi
# when its NFS root stops answering (#632). Built here rather than on the
# target because the target has no toolchain and its root filesystem is
# the very thing the binary exists to distrust. Statically linked so it
# depends on no shared library it would have to fault in off that share
# during an outage. Same digest pin as the repo's own builder.
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS watchdog-builder
# The program is stdlib-only on purpose, so it compiles here from a
# throwaway module without the repo's go.mod being in this build context
# — the context is this directory, and widening it to the repo root to
# fetch two files would pull the whole tree in. The same source is part
# of the root module, so `go build ./...`, `go vet` and `go test` in CI
# cover exactly this file.
WORKDIR /src
COPY nfs-watchdog/ ./
RUN printf 'module nfs-watchdog\ngo 1.26\n' > go.mod \
    && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
        -o /out/nfs-watchdog .

# Netboot server for the native arm64 CI runner lane (#531).
#
# Serves a Raspberry Pi its boot firmware over TFTP and its root filesystem
# over NFS, so the runner has no local OS media and cannot drift between
# boots. Deliberately carries no DHCP server: the site DHCP server keeps
# that role and the Pi is pointed here by TFTP_IP in its bootloader EEPROM.
# Pinned by digest like every other base image here (#633): this image
# serves the runner its ROOT FILESYSTEM, so an unpinned base lets the
# machine under the integration suite change between two builds.
FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

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
# Installed into the Pi's root filesystem by patch-target.sh.
COPY --from=watchdog-builder /out/nfs-watchdog /usr/local/share/netboot-templates/nfs-watchdog
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
