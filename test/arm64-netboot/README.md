# arm64 netboot server

Serves a Raspberry Pi its boot firmware over TFTP and its root filesystem over
NFS, so the native arm64 CI runner (#531) has no local OS media.

Two properties motivate the diskless shape. The root is reprovisioned from a
known image, so the runner **cannot drift** — the failure mode that once gave
the amd64 pool a per-job failure dice roll. And there is no SD card to wear out
under CI write load.

## What it does not do

It runs **no DHCP server**. The site DHCP server keeps that role untouched; the
Pi is pointed at this host by `TFTP_IP` in its bootloader EEPROM instead of by
DHCP option 66. That matters twice over: the site DHCP server is a consumer
router with no `next-server` support, and the suite this runner exists to run
deliberately abuses DHCP.

`dnsmasq` is used only as a TFTP daemon (`--port=0`, no DHCP range).

## Running it

Needs `--privileged` (loop-mounting the image, and the kernel NFS server),
`--network host` (TFTP and the RPC portmapper), a bind of the host `/dev` (a
privileged container gets its own minimal `/dev`, so the loop node `losetup`
creates on the host is otherwise invisible), and `/lib/modules` so it can load
`nfsd` itself rather than needing a `modprobe` on the host.

```sh
docker run -d --name rpi-netboot \
  --privileged --network host \
  -v /dev:/dev \
  -v /lib/modules:/lib/modules:ro \
  -v /srv/rpi-netboot:/srv/netboot \
  -e SERVER_IP=<this host's LAN address> \
  -e NFS_ALLOWED_CLIENTS=<the Pi's address> \
  -e ISCSI_ALLOWED_CLIENTS=<the Pi's address> \
  -e PI_SSH_AUTHORIZED_KEY="$(cat ~/.ssh/id_ed25519.pub)" \
  rpi-netboot:dev
```

The two `*_ALLOWED_CLIENTS` lines are not paranoia: without them the ACLs
default to the whole /24, which offers a root-writable filesystem and a raw
block device to every host on the subnet. The default exists so a first
bring-up works before the Pi has a stable address; once it does, narrow both
to it and do not rely on a firewall to do this file's job.

First start downloads the Raspberry Pi OS image, verifies its checksum and
unpacks it into the volume; that takes a few minutes. Later starts reuse it.
`FORCE_PROVISION=1` rebuilds from scratch.

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `SERVER_IP` | *required* | Address the Pi reaches this server on |
| `PI_SSH_AUTHORIZED_KEY` | *required* | Public key for the runner account; there is no password login |
| `PI_HOSTNAME` | `rpi-arm64-runner` | |
| `PI_USER` | `pi` | |
| `PI_IP_MODE` | `dhcp` | `dhcp` or `static` — see below |
| `PI_STATIC_IP`, `PI_GATEWAY`, `PI_NETMASK` | — | Required when `PI_IP_MODE=static` |
| `PI_SERIAL` | — | Adds a serial-named TFTP directory, for `TFTP_PREFIX=0` |
| `NFS_ALLOWED_CLIENTS` | `SERVER_IP`'s /24 | Export ACL. Narrow to the Pi's address in production; never widen to `*`: the export is `rw,no_root_squash` |
| `ISCSI_ALLOWED_CLIENTS` | `SERVER_IP`'s /24 | Initiator ACL for the `/var/lib/docker` LUN. Narrow to the Pi's address in production |
| `RPIOS_IMAGE_URL` / `RPIOS_IMAGE_SHA256` | — | Image to serve |

### Addressing: use `static` in production

`dhcp` is for first-boot bring-up. The kernel takes a lease before `/` is
mounted and **nothing ever renews it**, so the address silently expires on a
long-lived runner.

`static` is the production shape, and not only for that reason: it makes the
runner's own networking independent of the DHCP server that its test suite
spends the day abusing. Pair it with a reservation so nothing else is handed
the address.

Either way, NetworkManager is configured to leave the boot interface alone.
Re-running DHCP on it would drop the link that the root filesystem is mounted
over.

## Docker storage

Docker's `overlay2` driver cannot run on NFS — it needs a real block device —
and a Pi 4 has no PCIe to plug one into. So the block device is served from
here too, as an iSCSI LUN backed by a sparse file in the volume.

That keeps the runner genuinely diskless and keeps its state server-side:
resetting Docker's storage is an `rm` on this host, not a trip to the Pi. The
file is sparse, so a generous size costs nothing up front — a 24G LUN holding
one small image occupies well under a gigabyte.

Set up the Pi side once, from the Pi:

```sh
sudo env SERVER_IP=<server> bash setup-runner-storage.sh
```

It installs `open-iscsi`, logs in, formats the LUN if it is blank, mounts it at
`/var/lib/docker` by filesystem label, and orders `docker.service` after that
mount. All of it lives in the NFS root, so it survives reboots. It will not
reformat an existing filesystem — that is the runner's image cache, and wiping
it silently would present as an inexplicably cold build.

Disable the target entirely with `ENABLE_ISCSI=0` if a Pi only needs to boot.

## Preparing the Pi

**This is the one step the container cannot do for you**, and it must happen
before the first netboot.

The bootloader's default `BOOT_ORDER` does not include network boot, and the
setting lives in SPI EEPROM on the Pi — not on any bootable medium. So the Pi
has to run a Raspberry Pi OS once, from any medium, to set it:

```sh
sudo rpi-eeprom-config --edit
```

```ini
BOOT_ORDER=0xf12
TFTP_IP=<SERVER_IP>
TFTP_PREFIX=1
```

`BOOT_ORDER` digits are read **right to left**: `2` network, then `1` SD card,
then `f` restart. Network first with SD as the fallback has a useful
consequence — stop this container and the Pi boots whatever is on its SD card
instead.

`TFTP_IP` is what removes the dependency on DHCP option 66. `TFTP_PREFIX=1`
serves from the TFTP root directly rather than a directory named after the
board serial; set `PI_SERIAL` instead if you would rather keep the default.

Changing `BOOT_ORDER` does not touch the SD card, and is reversible from the
same command.

## Debugging a boot that does not come up

The Pi is headless and its root filesystem is the thing most likely to be
broken, so `enable_uart=1` is set and the serial console is the only channel
that survives every failure. Attach a USB-serial adapter to the header at
115200 baud.

`docker logs rpi-netboot` shows each TFTP request, which distinguishes "the
bootloader never asked" (EEPROM or link problem) from "it asked and then
stopped" (kernel or NFS problem).

## Status

Verified end to end on a Raspberry Pi 4B rev 1.4 (2 GB) on 2026-08-18:

- EEPROM configured over SSH from the SD-card OS; the SD was never modified
- TFTP boot chain served, kernel and initramfs loaded, NFS root mounted
- cloud-init completed, key-only SSH as the runner account
- bootloader updated in place 2024-04-15 → 2026-05-17 while netbooted
- `/var/lib/docker` on an iSCSI LUN, `overlay2` on `extfs` with `d_type`
- Docker 26.1.5 — the same version the amd64 lane runs, so a difference
  between the lanes is architecture rather than Docker version
- survives reboot: iSCSI session restored, mount and Docker ordered correctly,
  no failed units, image cache intact

Two things this tree cannot tell you, recorded because both cost real time:

**Updating the bootloader on a netbooted Pi needs the self-update path.** The
2711 ROM cannot load `recovery.bin` over the network, and `/boot/firmware` on a
netbooted root is an ordinary directory the bootloader never reads — so
`rpi-eeprom-update -a` there reports success and changes nothing. Stage it with
`sudo env RPI_EEPROM_SELF_UPDATE=1 BOOTFS=/tmp/stage rpi-eeprom-update -a`, copy
`pieeprom.upd` and `pieeprom.sig` into this server's TFTP root, reboot, confirm,
then remove them again. The same applies to a regenerated initramfs: only the
copy in the TFTP root is ever used.

**`PI_IP_MODE=dhcp` was observed moving the Pi's address across a reboot**, a
live demonstration of why `static` is the production shape.

## Still open

The runner is not enrolled as a GitHub Actions runner, and the `dhcp-ci-runner`
image is amd64-only. 2 GB of RAM is tight for Go builds over an NFS root.
