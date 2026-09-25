# docker-net-dhcp

A Docker network plugin that leases every container's address from the
DHCP server your LAN already runs, over `bridge`, `macvlan` or `ipvlan`,
for IPv4 and IPv6. One `docker network create`, then `networks: [lan-dhcp]`
in any Compose file. No static addresses, no sidecars, no plumbing per
container.

This is the successor of `devplayer0/docker-net-dhcp`, not a patched copy:
2.x has its own DHCP engine and its own lifecycle code.

!!! note "This documentation is the 2.x line's"
    These pages describe the 2.x build. The snippets install the current
    release. Pick a v1.x version from the selector for the 1.x manual.

```bash
sudo mkdir -p /var/lib/net-dhcp                      # once per host
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v2.2.3   # -arm64 on arm64
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.2.3 \
  --ipam-driver null -o mode=macvlan -o parent=eth0 lan-dhcp
docker run --rm -ti --network lan-dhcp alpine ip address show
```

It is a privileged plugin: it runs with host networking, the Docker socket
and `CAP_NET_ADMIN`. The full list is under [Requirements](#requirements),
and what each item is for is in [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md#scope--what-this-plugin-is).

## Feature list

- **The address is the LAN's.** The router's lease table, its MAC
  reservations and, with `-o register_dns=true`, its DNS see the container
  as one more host.
- **IPv6 in the same shape as IPv4.** `-o ipv6_mode=` picks DHCPv6, SLAAC,
  or whatever the router advertisement says, per network, with the same
  identity rules and the same counters.
- **One identity per container, kept across restarts.** In `bridge` and
  `macvlan` the plugin keeps the MAC, and with it the DHCP client id and
  the DHCPv6 DUID, across `docker restart`, a daemon restart and a plugin
  upgrade, and asks for the old address again. A MAC reservation on the
  router keeps matching.
- **ipvlan gets its own client id.** ipvlan containers share the parent's
  MAC, so each one gets its own DHCP client id (option 61) and its own
  DUID ([#895]). That identity is new on every restart, so an ipvlan
  container does not keep its address across `docker restart` ([#219]).
- **VLANs, sub-modes, MTU.** `-o vlan=` puts a macvlan or ipvlan network
  on a tagged VLAN off the parent ([#902]); `-o macvlan_mode=` and
  `-o ipvlan_mode=` pick the kernel mode of each link ([#905]); `-o mtu=`
  sets the link MTU ([#1037]).
- **Bridge mode from a spare NIC.** With `-o parent=` the plugin makes the
  bridge, enslaves the NIC and removes both with the network ([#903]);
  `-o force_create=true` overrides the firewall check on that bridge.
  `-o link_local_fallback=true`
  starts a container on a 169.254 address while the DHCP server is down
  and moves it to the lease when one arrives ([#904]).
- **Leases follow Docker's lifecycle.** Renewal, rebind, NAK and expiry run
  in the plugin, one client per endpoint. `-o release_lease=` hands the
  address back at stop or at remove. Every Docker hook has a defined
  outcome, including a container that leaves before its lease binds, a
  network removed together with its bridge, and an endpoint Docker forgot.
- **Measured on Docker Engine 20.10 through 29.** The engine matrix builds
  the plugin, creates networks in every documented shape, confirms a lease
  in the DHCP server's own log and keeps an address across
  `docker restart`, on every engine line, weekly and on every change to
  the measurement. Below 20.10 the plugin refuses to start and names the
  minimum.

## Two shapes

| You want | Use |
| --- | --- |
| The container takes its address from the LAN, Docker keeps no pool | `--ipam-driver null` (the default shape, all three modes) |
| Docker knows the address: `docker run --ip`, Compose `ipv4_address`, `docker inspect` | the plugin as IPAM driver as well (`--ipam-driver <plugin>`), `bridge` and `macvlan`, IPv4 and IPv6; `--ip6` and `ipv6_address` are not served ([#960]) |
| `ipvlan` | `--ipam-driver null` |

| | IPv4 | IPv6 | as IPAM driver |
| --- | --- | --- | --- |
| bridge | yes | yes | yes |
| macvlan | yes | yes | yes |
| ipvlan | yes | yes | no, needs a Docker change ([#949]) |

## Unsupported

- `ipvlan` with this plugin as IPAM driver. Docker assigns a MAC to
  the ipvlan interface, and ipvlan interfaces cannot take one. Needs a
  change in Docker, not planned ([#949]). `ipvlan` networks use
  `--ipam-driver null`.
- Docker Engine 19.03. Unmeasured, not planned.

The plugin refuses these with a message that names the reason.

## Planned

v2.4.0: DHCPv6 Rapid Commit ([#926]), temporary addresses ([#927]) and
prefix delegation ([#214]); DHCPv4 Rapid Commit ([#1031]) and IPv6-only
preferred ([#1027]); stable-privacy SLAAC addresses ([#1032]); one
multi-architecture image per tag ([#1035]). The full list, with what this
project will not do, is on the [roadmap](roadmap.md).

## How to check any of this

- The DHCP exchange runs inside the plugin on the project's own library,
  [dhcp-golib](https://github.com/claymore666/dhcp-golib). No external
  DHCP client, no client process per container.
- Tests assert on the wire and on the server: packet captures and the
  DHCP server's lease log, not the plugin's own counters
  ([how this plugin is tested](testing.md)).
- Every pull request carries a public review verdict, and every release
  ships signatures, SLSA provenance and an SBOM
  ([verifying releases](verifying-releases.md)).

[#219]: https://github.com/claymore666/docker-net-dhcp/issues/219
[#895]: https://github.com/claymore666/docker-net-dhcp/issues/895
[#902]: https://github.com/claymore666/docker-net-dhcp/issues/902
[#905]: https://github.com/claymore666/docker-net-dhcp/issues/905
[#1037]: https://github.com/claymore666/docker-net-dhcp/issues/1037
[#903]: https://github.com/claymore666/docker-net-dhcp/issues/903
[#904]: https://github.com/claymore666/docker-net-dhcp/issues/904
[#949]: https://github.com/claymore666/docker-net-dhcp/issues/949
[#926]: https://github.com/claymore666/docker-net-dhcp/issues/926
[#927]: https://github.com/claymore666/docker-net-dhcp/issues/927
[#214]: https://github.com/claymore666/docker-net-dhcp/issues/214
[#1031]: https://github.com/claymore666/docker-net-dhcp/issues/1031
[#1027]: https://github.com/claymore666/docker-net-dhcp/issues/1027
[#1032]: https://github.com/claymore666/docker-net-dhcp/issues/1032
[#1035]: https://github.com/claymore666/docker-net-dhcp/issues/1035

## Requirements

- **Docker Engine 20.10 or newer.** 20.10 is the lowest version this
  plugin is measured on. The engine matrix drives the whole baseline on
  every engine line from 20.10 to the current release: the plugin
  created from a local build and enabled, `docker network create` in
  bridge, macvlan and ipvlan mode, a lease confirmed in the DHCP
  server's own log, and an endpoint that keeps its address across
  `docker restart`. Pulling the published plugin from a registry is not
  part of that measurement. It runs weekly and on every change to the
  measurement. Below 20.10 the plugin refuses to start, and the refusal
  names the minimum and the engine it saw. 19.03 is `unsupported`
  because it is unmeasured: on a cgroup v2 host it cannot start a
  container at all, so nothing there tests this plugin.
  Every change is also tested against the engine the integration suite
  runs on, **29.8.1** today, read from that run's `Fixture engine drift`
  step.
- **Plugin interface `docker.networkdriver/1.0`**, which is what the
  plugin manifest declares. The plugin negotiates the Docker API version
  with the daemon. It publishes both numbers on `/Plugin.Health` as
  `engine_version` and `api_version`. The minimum above is a version of
  the engine, not of the API, and nothing is refused on the API version.
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
- **Mode constraints.** `bridge` expects a host bridge you maintain,
  or makes one from a spare NIC that stays up with no address (v2.3.0);
  `macvlan` and `ipvlan` attach to a host NIC and change nothing on it,
  at the cost of the kernel rule that a child cannot reach its own host's
  address. With `-o vlan=<id>` (v2.3.0) they attach to a VLAN
  sub-interface of that NIC, which the plugin creates when it is missing
  and removes with the last network on it. Both in
  [macvlan / ipvlan modes](parent-attached-modes.md).

## Quick start

```bash
# Once per host, before the install. See Requirements above.
sudo mkdir -p /var/lib/net-dhcp

# amd64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v2.3.0
# arm64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v2.3.0-arm64
```

One network, created once. `macvlan` needs only a host NIC; `bridge`
wants a bridge you bring yourself ([Bridge mode](bridge-mode.md)):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.3.0 \
  --ipam-driver null -o mode=macvlan -o parent=eth0 lan-dhcp

docker run --rm -ti --network lan-dhcp alpine ip address show
```

`--ipam-driver null` stops Docker handing out addresses that would
collide with the real LAN. From v2.1.0 there is a second supported
shape: name the plugin again in place of `null` and the leased address
goes into Docker's own address management, which makes `--ip` and
Compose's `ipv4_address` work.

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.3.0 \
  --ipam-driver ghcr.io/claymore666/docker-net-dhcp:v2.3.0 \
  -o mode=macvlan -o parent=eth0 lan-dhcp
```

One of the two is required. On arm64 the `-arm64` tag goes in these
lines too, because a network records the tagged reference as its driver.
Add `-o ipv6_mode=dhcp` for a DHCPv6 lease beside the v4 one, or
`-o ipv6_mode=slaac` to take the address from the router's
advertisement; `auto` reads the advertisement and does what it says,
DHCPv6 where it asks for DHCPv6 and the prefix where it does not. `-o
ipv6=true` is the short spelling of `dhcp`. They work on both lines.
On the IPAM line leave out Docker's `--ipv6`: it is refused there,
because the plugin allocates no IPv6 pool ([#960]). The modes are set out in
[the driver reference](reference.md#driver-options-network-level), and
the two shapes in [the same page](reference.md#address-allocation).

[#960]: https://github.com/claymore666/docker-net-dhcp/issues/960

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
- **[How this plugin is tested](testing.md)** is what is tested, where, and
  what each result proves.
- **[Roadmap](roadmap.md)** is where this is going, and what it will not do.
- **[Contributing](contributing.md)** is what an acceptable pull request
  looks like.
- **[Release runbook](release-runbook.md)** is the maintainer-facing
  publish procedure.

These pages are **versioned**: use the selector in the header to read the
documentation matching the plugin version you have installed.

## Images and releases

Images go to GHCR (`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`, primary)
and are mirrored to Docker Hub under two names,
`claymore666/net-dhcp:vX.Y.Z` and
`claymore666/docker-net-dhcp:vX.Y.Z`. The two Hub names are the same
image at the same digest; install from either. Pin a version for
reproducibility.

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

## Origin and licence

This began as a fork of
[`devplayer0/docker-net-dhcp`](https://github.com/devplayer0/docker-net-dhcp)
(quiet since 2021); since 2.0 it is its own product, with its own DHCP
engine.

GPL-3.0. See
[LICENSE.md](https://github.com/claymore666/docker-net-dhcp/blob/main/LICENSE.md).
The upstream project is GPL-3.0 and this derivative stays under the same
licence.

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
