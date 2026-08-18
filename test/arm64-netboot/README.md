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
  -e PI_SSH_AUTHORIZED_KEY="$(cat ~/.ssh/id_ed25519.pub)" \
  rpi-netboot:dev
```

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
| `NFS_ALLOWED_CLIENTS` | `SERVER_IP`'s /24 | Export ACL. Never widen this to `*`: the export is `rw,no_root_squash` |
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

Verified on the server side: image fetch and checksum, extraction, all
patching, TFTP transfer of a 10 MB kernel byte-identical, and an NFS v3/tcp
mount with root write access.

**Not yet exercised: an actual Pi boot.** Also not yet built: local block
storage for `/var/lib/docker`. Docker's `overlay2` does not run on NFS, so the
runner needs a real block device — iSCSI from this same server is the intended
answer and is not implemented here.
