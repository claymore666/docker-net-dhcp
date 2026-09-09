# docker-net-dhcp

A Docker network plugin that gives every container an address from the
DHCP server your LAN already runs (your router, a Fritz!Box, dnsmasq)
instead of from Docker's own IPAM, over `bridge`, `macvlan` or `ipvlan`,
for IPv4 and IPv6. The DHCP exchange runs inside the plugin on the
project's own engine, the
[dhcp-golib](https://github.com/claymore666/dhcp-golib) library: there is
no external DHCP client to install and no client process per container.

!!! note "This documentation is the 2.0 line's"
    These pages describe the 2.0 build. Its first pre-release is
    `v2.0.0-rc1`; substitute that tag for the version in the snippets
    below to install it. Pick a v1.x version from the selector for the
    1.x manual.

## Requirements

- **Docker Engine.** Every change is tested against the engine the
  integration suite runs on, **29.7.2** today, read from that run's
  `Fixture engine drift` step. It is the version this build is measured
  on. It is not a floor: the minimum has never been measured (#670), so
  the measured number is the honest one to publish.
- **Plugin interface `docker.networkdriver/1.0`**, which is what the
  plugin manifest declares. The plugin negotiates the Docker API version
  with the daemon, so no API floor is claimed here either.
- **One directory, created once per host, before `docker plugin install`**
  (the line is in the quick start below). Docker will not create a missing
  bind source, so without it the install fails at start-up and leaves the
  plugin **installed but disabled**, after which the identical command
  answers only `plugin ... already exists` and names nothing. Recovery:
  [the reference](reference.md#install-upgrade-uninstall).
- **Architecture.** `linux/amd64` on the bare tag, `linux/arm64` on the
  `-arm64` tag. A Docker plugin cannot be installed from a
  multi-architecture manifest list, so the tag is how the architecture is
  chosen, in **every** snippet that names the image and not only the
  install line. Why, in full:
  [Install, upgrade, uninstall](reference.md#install-upgrade-uninstall).
- **Privileges.** The manifest asks for `host` networking, the host PID
  namespace, the Docker socket, a bind mount of the state directory, a
  read-only bind mount of `/var/run/docker`, and `CAP_NET_ADMIN`,
  `CAP_NET_RAW`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE`. `docker plugin
  install` prompts for the set; what each is for is in
  [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md#scope--what-this-plugin-is).
- **Kernel link types.** Bridge mode needs `veth` and `bridge`; `macvlan`
  and `ipvlan` each need the kernel module of the same name. A stock
  distribution kernel loads one the first time that link type is asked
  for, so there is normally nothing to do: measured on Linux 6.12,
  `ipvlan` was absent from `lsmod` before the first
  `ip link add ... type ipvlan` and present after, with no `modprobe`. A
  kernel built without the type, or a host where module loading is
  turned off, fails `docker network create` for that mode.
- **Root on the host.** The plugin runs as root and its socket lives
  under `/run/docker/plugins`, which only root can read. Reading
  [`/Plugin.Health`](reference.md#pluginhealth) therefore needs `sudo`.
  Without it `curl -s` prints nothing and exits 7, which is what an
  absent socket also gives, so a permission problem looks like a stopped
  plugin.
- **Mode constraints.** `bridge` expects a host bridge you maintain;
  `macvlan` and `ipvlan` attach to a host NIC and change nothing on the
  host, at the cost of the kernel rule that a child cannot reach its own
  host's address. Both in
  [macvlan / ipvlan modes](parent-attached-modes.md).

## Quick start

```bash
# Once per host, before the install. See Requirements above.
sudo mkdir -p /var/lib/net-dhcp

# amd64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.9.0
# arm64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.9.0-arm64
```

One network, created once. `macvlan` needs only a host NIC; `bridge`
wants a bridge you bring yourself ([Bridge mode](bridge-mode.md)):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.9.0 \
  --ipam-driver null -o mode=macvlan -o parent=eth0 lan-dhcp

docker run --rm -ti --network lan-dhcp alpine ip address show
```

`--ipam-driver null` is **mandatory**: it stops Docker handing out
addresses that would collide with the real LAN. On arm64 the `-arm64`
tag goes in this line too, because a network records the tagged reference
as its driver. Add `-o ipv6=true` for a DHCPv6 lease beside the v4 one.

After that, plain Compose. No static addresses, no sidecar, nothing per
container:

```yaml
services:
  app:
    image: nginx
    networks: [lan-dhcp]

networks:
  lan-dhcp:
    external: true
```

## Why this one

- **The address comes from the LAN's own server**, so the router's lease
  table, its MAC reservations and, with `-o register_dns=true`, its DNS
  all see the container as one more host on the network. The alternative
  is a hand-assigned address in every Compose file.
- **The lease is held for as long as the container runs.** Renewal, rebind, NAK and expiry
  run in the plugin, one client per endpoint, and the lifecycle is visible
  on [the health endpoint](reference.md#pluginhealth). There is no
  external DHCP client to install, supervise or reap.
- **IPv6 is the same one line.** `-o ipv6=true` adds a DHCPv6 lease with
  its own timers, its own counters and a DUID that survives a restart.
- **A restart keeps the address.** In `bridge` and `macvlan` the MAC is
  carried across `docker restart`, so a server-side reservation still
  matches and the old address is re-requested; a plugin restart or upgrade
  re-adopts running containers, so their leases do not lapse
  ([how](reference.md#restart-stability-mac-and-ip)).
- **No host plumbing per container.** `macvlan` and `ipvlan` attach to a
  NIC that is already there: no bridge to build, no route to add, nothing
  on the host to undo afterwards.

What is planned, and what this project has decided not to do, is on the
[roadmap](roadmap.md).

## Origin and licence

This began as a fork of
[`devplayer0/docker-net-dhcp`](https://github.com/devplayer0/docker-net-dhcp)
(quiet since 2021); since 2.0 it is its own product, with its own DHCP
engine.

GPL-3.0. See
[LICENSE.md](https://github.com/claymore666/docker-net-dhcp/blob/main/LICENSE.md).
The upstream project is GPL-3.0 and this derivative stays under the same
licence.

## Documentation

- **[Driver reference](reference.md)** is the manual: every option,
  setting and counter, install and upgrade, lease behaviour,
  observability, Compose usage, troubleshooting.
- **[Bridge mode](bridge-mode.md)** is the one-time host bridge setup.
- **[macvlan / ipvlan modes](parent-attached-modes.md)** covers choosing
  between them, and their constraints.
- **[Verifying releases](verifying-releases.md)** covers signatures, SLSA
  provenance, SBOMs, and rebuilding the binaries yourself.
- **[How it works](internals.md)** is the mechanism, for contributors.
- **[Roadmap](roadmap.md)** is where this is going, and what it will not do.
- **[Contributing](contributing.md)** is what an acceptable pull request
  looks like.
- **[Release runbook](release-runbook.md)** is the maintainer-facing
  publish procedure.

These pages are **versioned**: use the selector in the header to read the
documentation matching the plugin version you have installed.

## Images and releases

Images go to GHCR (`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`, primary)
and are mirrored to Docker Hub (`claymore666/net-dhcp:vX.Y.Z`). Pin a
version for reproducibility.

Published builds are **`linux/amd64`** on the bare tag and
**`linux/arm64`** as `:vX.Y.Z-arm64` / `:latest-arm64` (v1.7.0 onward).
Those two are the whole set: **32-bit ARM is not built**, so there is no
`armv7` or `armhf` tag to install.
The architecture lives in the tag because a Docker *plugin* cannot be
installed from a multi-architecture manifest list at all: the daemon
reads a plugin's privileges before pulling it, its manifest handler
matches single manifests only, and an index therefore fails with `did not
find plugin config for specified reference` on every architecture, with
no `--platform` to steer it. The `-arm64` tag replaces the bare one in
**every** snippet that names the image, including
`docker network create -d`: a network records the tagged reference as its
driver, so a bare tag there names a plugin the host does not have.

- [GHCR package](https://github.com/claymore666/docker-net-dhcp/pkgs/container/docker-net-dhcp)
- [GitHub Releases](https://github.com/claymore666/docker-net-dhcp/releases)
  carries per-release notes, credits and signed artifacts.

## Verifying releases

Every release from v1.1.0 is cosign-signed (keyless) on both registries
and ships an SBOM; **SLSA build provenance is attested for the GHCR image
only**, so verify provenance against the `ghcr.io` reference. Both
commands need **cosign v3 or newer**. v2 cannot read the bundle format
the release signs with, and it fails in a way that looks like a broken
signature. Replace `VERSION`:

```bash
cosign verify ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

gh attestation verify oci://ghcr.io/claymore666/docker-net-dhcp:VERSION --repo claymore666/docker-net-dhcp
```

The whole procedure, including rebuilding the binaries yourself, is in
[Verifying releases](verifying-releases.md).

## Project & community

- **Contributing:** open a pull request against the `dev` branch. See
  [Contributing](contributing.md).
- **Security policy / vulnerability reporting:**
  [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md).
  Do **not** open a public issue for a vulnerability.
- **Bug reports & feature requests:** the
  [issue forms](https://github.com/claymore666/docker-net-dhcp/issues/new/choose).
