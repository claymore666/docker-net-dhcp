# docker-net-dhcp

[![Test](https://github.com/claymore666/docker-net-dhcp/actions/workflows/test.yaml/badge.svg)](https://github.com/claymore666/docker-net-dhcp/actions/workflows/test.yaml)
[![Integration](https://github.com/claymore666/docker-net-dhcp/actions/workflows/integration.yml/badge.svg)](https://github.com/claymore666/docker-net-dhcp/actions/workflows/integration.yml)
[![Dependencies](https://img.shields.io/badge/dependencies-Dependabot%20%2B%20govulncheck-brightgreen?logo=dependabot)](https://github.com/claymore666/docker-net-dhcp/network/updates)
[![Release](https://img.shields.io/github/v/release/claymore666/docker-net-dhcp?sort=semver)](https://github.com/claymore666/docker-net-dhcp/releases)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/claymore666/docker-net-dhcp/badge)](https://scorecard.dev/viewer/?uri=github.com/claymore666/docker-net-dhcp)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13229/badge)](https://www.bestpractices.dev/projects/13229)
[![Docs](https://img.shields.io/badge/docs-claymore666.github.io-blue?logo=materialformkdocs&logoColor=white)](https://claymore666.github.io/docker-net-dhcp/)

A Docker network plugin that gives every container an address from the
DHCP server your LAN already runs (your router, a Fritz!Box, dnsmasq)
instead of from Docker's own IPAM, over `bridge`, `macvlan` or `ipvlan`,
for IPv4 and IPv6. The DHCP exchange runs inside the plugin on the
project's own engine, the [dhcp-golib][dhcp-golib] library: there is no
external DHCP client to install and no client process per container.

This branch is the 2.0 line and every page on it describes that build.
The snippets below install the current release.

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
  runs on, **29.7.2** today, read from that run's `Fixture engine drift`
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
  [the reference](docs/reference.md#install-upgrade-uninstall).
- **Architecture.** `linux/amd64` on the bare tag, `linux/arm64` on the
  `-arm64` tag. A Docker plugin cannot be installed from a
  multi-architecture manifest list, so the tag is how the architecture is
  chosen, in **every** snippet that names the image and not only the
  install line. Why, in full:
  [Install, upgrade, uninstall](docs/reference.md#install-upgrade-uninstall).
- **Privileges.** The manifest asks for `host` networking, the host PID
  namespace, the Docker socket, a bind mount of the state directory, a
  read-only bind mount of `/var/run/docker`, and `CAP_NET_ADMIN`,
  `CAP_NET_RAW`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE`. `docker plugin
  install` prompts for the set; what each is for is in
  [SECURITY.md](SECURITY.md#scope--what-this-plugin-is).
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
  [`/Plugin.Health`](docs/reference.md#pluginhealth) therefore needs
  `sudo`. Without it `curl -s` prints nothing and exits 7, which is what
  an absent socket also gives, so a permission problem looks like a
  stopped plugin.
- **Mode constraints.** `bridge` expects a host bridge you maintain;
  `macvlan` and `ipvlan` attach to a host NIC and change nothing on the
  host, at the cost of the kernel rule that a child cannot reach its own
  host's address. Both in
  [macvlan / ipvlan modes](docs/parent-attached-modes.md).

## Quick start

```bash
# Once per host, before the install. See Requirements above.
sudo mkdir -p /var/lib/net-dhcp

# amd64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v2.0.0
# arm64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v2.0.0-arm64
```

One network, created once. `macvlan` needs only a host NIC; `bridge`
wants a bridge you bring yourself ([bridge mode](docs/bridge-mode.md)):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.0.0 \
  --ipam-driver null -o mode=macvlan -o parent=eth0 lan-dhcp

docker run --rm -ti --network lan-dhcp alpine ip address show
```

`--ipam-driver null` stops Docker handing out addresses that would
collide with the real LAN. From v2.1.0 there is a second supported
shape: name the plugin again in place of `null` and the leased address
goes into Docker's own address management, which makes `--ip` and
Compose's `ipv4_address` work.

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.0.0 \
  --ipam-driver ghcr.io/claymore666/docker-net-dhcp:v2.0.0 \
  -o mode=macvlan -o parent=eth0 lan-dhcp
```

One of the two is required. On arm64 the `-arm64` tag goes in these
lines too, because a network records the tagged reference as its driver.
Add `-o ipv6=true` for a DHCPv6 lease beside the v4 one. The two shapes
are set out in
[the driver reference](docs/reference.md#address-allocation).

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
  on [the health endpoint](docs/reference.md#pluginhealth). There is no
  external DHCP client to install, supervise or reap.
- **IPv6 is the same one line.** `-o ipv6=true` adds a DHCPv6 lease with
  its own timers, its own counters and a DUID that survives a restart.
- **A restart keeps the address.** In `bridge` and `macvlan` the MAC is
  carried across `docker restart`, so a server-side reservation still
  matches and the old address is re-requested; a plugin restart or upgrade
  re-adopts running containers, so their leases do not lapse
  ([how](docs/reference.md#restart-stability-mac-and-ip)).
- **No host plumbing per container.** `macvlan` and `ipvlan` attach to a
  NIC that is already there: no bridge to build, no route to add, nothing
  on the host to undo afterwards.

What is planned, and what this project has decided not to do, is on the
[roadmap](docs/roadmap.md).

## Origin and licence

This began as a fork of [`devplayer0/docker-net-dhcp`][fork-parent]
(quiet since 2021); since 2.0 it is its own product, with its own DHCP
engine.

GPL-3.0. See [LICENSE.md](LICENSE.md). The upstream project is GPL-3.0
and this derivative stays under the same licence.

[fork-parent]: https://github.com/devplayer0/docker-net-dhcp
[dhcp-golib]: https://github.com/claymore666/dhcp-golib

## Documentation

Published at **<https://claymore666.github.io/docker-net-dhcp/>**, one
version per release; the same pages live in [`docs/`](docs).

- **[Driver reference](docs/reference.md)** is the manual: every option,
  setting and counter, install and upgrade, lease behaviour,
  observability, Compose usage, troubleshooting.
- **[Bridge mode](docs/bridge-mode.md)** is the one-time host bridge setup.
- **[macvlan / ipvlan modes](docs/parent-attached-modes.md)** covers
  choosing between them, and their constraints.
- **[Verifying releases](docs/verifying-releases.md)** covers signatures,
  SLSA provenance, SBOMs, and rebuilding the binaries yourself.
- **[How it works](docs/internals.md)** is the mechanism, for contributors.
- **[Roadmap](docs/roadmap.md)** is where this is going, and what it will
  not do.
- **[Contributing](docs/contributing.md)** is what an acceptable pull
  request looks like.
- **[Changelog](RELEASE_NOTES.md)** · **[Release runbook](docs/release-runbook.md)**

Images go to GHCR (`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`, primary)
and are mirrored to Docker Hub (`claymore666/net-dhcp:vX.Y.Z`).

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
[Verifying releases](docs/verifying-releases.md).

## Project & community

- **Security policy / vulnerability reporting:** [SECURITY.md](SECURITY.md).
  Do **not** open a public issue for a vulnerability.
- **Bug reports & feature requests:** the
  [issue forms](https://github.com/claymore666/docker-net-dhcp/issues/new/choose).
  Include the plugin version, your Docker version, the mode, and the
  [plugin log](docs/reference.md#plugin-log). Docker has no
  `plugin logs` subcommand.
- **Questions:** ask in
  [Discussions](https://github.com/claymore666/docker-net-dhcp/discussions/new?category=q-a);
  each issue form stamps a type label, so a question has no honest home on
  the tracker.
- **Governance & code of conduct:** [GOVERNANCE.md](GOVERNANCE.md),
  [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Contributing

Contributions are welcome. Open a pull request against the `dev` branch;
[Contributing](docs/contributing.md) is the whole of what is asked, and
`make check` runs the fast CI lane locally in about a minute. In short:

- Go code is `gofmt`-formatted and passes `go vet` and
  [`staticcheck`](https://staticcheck.dev/); shell and workflow files pass
  `shellcheck` and `actionlint`.
- New functionality is expected to ship with tests, and a per-package
  coverage ratchet enforces that at release time.
- Commits and pull request descriptions carry no AI-assistant
  attribution and the commit author is a person. The `attribution`
  check reads every commit and the description.
- Every required check must be green; branch protection holds the list.

<!-- starter-task-claim: begin -->
- **Looking for somewhere to start?** There are no starter tasks open at the
  moment. The ones that were seeded were closed as the work they described
  landed, and this section says so instead of sending you to an empty list.
  If you would like a first task,
  [ask in Discussions](https://github.com/claymore666/docker-net-dhcp/discussions/new?category=q-a)
  and say what interests you: a subsystem, a bug you hit, a piece of the
  documentation you found thin. One will be scoped against it. When
  starter tasks exist again they carry the `good first issue` label and this
  section links to them.
<!-- starter-task-claim: end -->

This is solo-maintained, so please allow a few days for a response.
