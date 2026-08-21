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

## NFS-outage watchdog

The Pi's root filesystem and its Docker volume both come from this
server. When the server goes away the host does not fail, it hangs: the
kernel stays up, it answers ping, TCP connects to 22, and sshd never
prints a banner because it cannot re-exec its own binary off the share.
Only a power cycle clears it.

The SoC watchdog does not save you here, and it is worth being precise
about why, because it is already switched on. Raspberry Pi OS ships
`/usr/lib/systemd/system.conf.d/40-rpi-enable-watchdog.conf` with
`RuntimeWatchdogSec=1m`, and PID 1 holds `/dev/watchdog0`. Note that it
does *not* get the minute it asks for: the BCM2835 watchdog tops out at
**15s**, and the kernel clamps the request down to that silently — read
the real figure from `/sys/class/watchdog/watchdog0/timeout` rather than
from the unit file. It was armed through the outage that motivated this
and the board still needed hands anyway, because systemd is resident in
memory and its event loop never touches the root filesystem — it keeps
petting for as long as the outage lasts.

So provisioning hands the device to `nfs-watchdog` (`nfs-watchdog/`),
which pets it only while `statfs` on `/` still reaches the server:

- `statfs`, not a file read — the page cache answers a read from RAM long
  after the server is gone, which would feed the watchdog through the
  very outage it is meant to catch.
- the probe runs in its own goroutine and publishes a timestamp; the
  petting loop never calls into the filesystem. On a hard mount a probe
  does not fail, it blocks forever, so a stuck probe simply stops
  refreshing the timestamp and is treated identically to a failed one.
  The staleness deadline *is* the timeout.
- `mlockall`, and it refuses to start without it: clean file-backed pages
  stay evictable during an outage, and faulting them back in blocks on
  the dead share. A petter that gets paged out stops petting for the
  wrong reason.
- it logs to `/dev/kmsg`, because journald can block a writer and its
  storage is on the share.
- its timings are derived from the device, not assumed. The tuned
  defaults suit a 60s watchdog; on hardware that caps lower they would be
  refused as invalid, and a watchdog daemon that exits leaves the board
  *less* protected than one that never started. So it reads the real
  hardware timeout and scales the pet, probe and staleness intervals by
  ratio, reporting what it changed. Timings you set explicitly are never
  overridden — a contradiction you asked for is still an error (#632).

Recovery is then automatic, and it is worth naming *why* rather than
trusting it: the board resets **after** the server is already gone, so its
bootloader finds no TFTP server at all — and that is the case
`BOOT_ORDER=0xf2` loops on until this server answers again. Measured on
this hardware the reset lands 9-24s after the share stops answering (a 15s
device timeout, scaled to 3s/3s/9s pet/probe/stale), which is long after
anything this server was sending has stopped. A server that dies *during* a
transfer is a different case with a different outcome — see "Four states,
and only one of them needs hands" below.

It also has to survive the host's own shutdown, and that is a second
thing the unit gets wrong easily. A shutdown blocking on a dead NFS
server is one of the two wedges this exists to end — and it is not a
remote case here, because the pre-shutdown hook's whole job is to *reboot
this board*. The unit therefore sets `DefaultDependencies=no` and adds
nothing back: `Before=shutdown.target` with `Conflicts=shutdown.target` is
systemd's documented idiom for "stop me before shutdown proceeds", so
writing them out — as this unit did until #632 was reopened — makes
systemd hand the timer back at the first instant of every reboot. The
daemon completes the pair: shutdown sends the same `SIGTERM` an operator
does, so it disarms only while `statfs` still answers, and otherwise
closes the device *without* the magic byte, which the kernel reads as
"closed unexpectedly" and leaves running. An operator stopping the
service to debug it still gets a board that does not reset underneath
them; a shutdown that hangs on the dead share still gets ended by the
hardware.

Every one of these fails silently on its own — without the
`RuntimeWatchdogSec=0` drop-in the service gets `EBUSY` and the host runs
unprotected; without the unit enabled, nothing pets and a *healthy* host
resets as soon as the hardware timeout expires — 15s on this board, not
the minute the unit file names; ordered against `shutdown.target` it looks
perfect and covers one case fewer than it claims.
`scripts/check-pi-watchdog-wiring.sh` gates all three,
`scripts/test-check-pi-watchdog-wiring.sh` proves it catches each
direction, and the daemon's half is pinned by
`TestRun_DisarmsOnlyWhileTheFilesystemAnswers`.

None of that reaches a board that is already running: the root is an
image, so a host provisioned before this fix keeps the old unit until it
is reprovisioned. `scripts/check-host-watchdog.sh` cannot see the
difference — it reads sysfs and `/dev/kmsg` from inside a container and
never sees systemd's view of the unit — so this one is checked at
provisioning time, not on the live host.

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

### Four states, and only one of them needs hands (#654)

"No ICMP" is not a diagnosis. The board is silent on ICMP while it is
booting, while it is looping, and while it is hung, and those three want
completely different reactions. The discriminator is this server's TFTP
log, never ping:

| State | Signature | What it needs |
|---|---|---|
| **BOOTING** | no ICMP, TFTP requests arriving from the board | wait — a cold netboot is a couple of minutes |
| **BOOTLOOP** | no ICMP, this server down | nothing; the board boots about a minute after the server returns |
| **HUNG** | no ICMP, this server **up and serving**, and **zero** TFTP requests for minutes | power-cycle the board; nothing else clears it |
| **WEDGED** | TCP/22 opens but never sends a banner | half-booted with its root gone; the watchdog above resets it within ~24s |

**HUNG is the bootloader itself declining to retry**, so nothing
installable on the board can reach it. It has one specific cause: this
server disappearing *while a transfer is in flight*. Measured on
2026-08-19, the server's last line before it stopped was `failed sending
kernel8.img`; the board then answered ARP 5/5 while issuing **zero** TFTP
requests across 10.5 minutes with the server fully back up. A looping board
would have asked roughly eight times in that window. It stayed that way
until power was removed, and netbooted immediately once it was.

That contradicts the documented behaviour, which is exactly why it is
written down here rather than assumed: with `NET_BOOT_MAX_RETRIES=0` a
failed netboot falls through to the next nibble — `f`, restart — and loops
at roughly 45-75s per cycle (`DHCP_TIMEOUT` 45s, `TFTP_FILE_TIMEOUT` 30s).
It does loop as documented when it finds *no server*. It does not when the
server dies mid-download. Upstream: [rpi-eeprom#687][hung-687], and
[#417][hung-417] for the same class.

[hung-687]: https://github.com/raspberrypi/rpi-eeprom/issues/687
[hung-417]: https://github.com/raspberrypi/rpi-eeprom/issues/417

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

Both of the things this section used to name are done, and are recorded
here rather than deleted because the gap between them is the useful part:

- **The board is enrolled.** It runs as a standing self-hosted runner
  (`register` mode, #632) carrying the `dhcp-ci-arm64` label, and it
  reconnects on its own after a reboot with no token and no hands.
- **The image is multi-arch.** `runner-image.yml` builds `amd64` and
  `arm64` natively — no emulation — and each job asserts its own
  architecture rather than trusting the runner it landed on.

What genuinely remains:

- **2 GB of RAM is tight for Go builds over an NFS root.** Unchanged, and
  the reason the image cache on the iSCSI volume is worth keeping across
  jobs rather than running this host ephemerally.
- **The board reads `offline` whenever this server is down**, because its
  root filesystem is the share. That is expected rather than an outage, and
  it is also the normal state between release candidates: this runner is a
  standing registration (#632), so it reads `offline` most of the time by
  design, and `idle` rather than `busy` when it is up — unlike the
  ephemeral runners, which only exist while they hold a job. Neither
  reading is an alert. Pool monitoring counts `rpi-arm64-*` separately for
  this reason.
- **It recovers unattended from every routine way this server stops**, by
  two different mechanisms rather than one (#654). A graceful stop is
  caught by the pre-shutdown hook, which reboots the board while the export
  is still up and only then drops the netboot service, so the board returns
  to no server rather than to one vanishing underneath it. An abrupt stop —
  the host killed, crashed, or losing power — is caught by the watchdog
  above, which resets the board 9-24s later, by which time the server is
  already gone. Both land in BOOTLOOP, which is the harmless state.
- **What is left is a coincidence, and it is accepted rather than solved.**
  A hard stop of this server that lands inside the 20-60s a board spends
  netbooting for some unrelated reason produces HUNG, and that needs a
  physical power cycle. A hard stop cannot be hooked by definition, and the
  component that is stuck is the bootloader, so nothing installable on the
  board reaches it; closing it properly needs switchable power, which this
  project does not own. What is guaranteed instead is that it can never
  pass silently — `scripts/check-arm64-lane.sh` turns an arm64 runner that
  never appeared into a red check on the rc within 25 minutes, so the worst
  case is a release candidate waiting for someone to press a power button,
  never a lane that quietly stopped verifying.
- **The pre-shutdown hook lives on this server, not in this tree**, so
  nothing here can test or fix it. It is not a tidiness measure: without
  it, every planned stop of this server *is* the mid-transfer case above.
