# docker-net-dhcp

A Docker network plugin that allocates container IP addresses (IPv4 and
optionally IPv6) from an **existing DHCP server** — your router, a
Fritz!Box, dnsmasq, anything — instead of Docker's self-managed IPAM
pools. Containers come up on your LAN as first-class hosts, addressable
like any other machine. Bridge, macvlan, and ipvlan attachment modes.

!!! info "This is a maintained fork"
    A maintained fork of
    [`devplayer0/docker-net-dhcp`](https://github.com/devplayer0/docker-net-dhcp)
    (quiet since 2021, no longer builds on current Docker). This fork
    modernises the toolchain (Go 1.26, docker SDK v28, current Alpine),
    adds **macvlan** and **ipvlan** modes, fixes the daemon-restart
    deadlock and a state data-race, and gates every PR on a live
    integration suite (all three modes + DHCPv6, recovery, failure
    injection) with a coverage ratchet and supply-chain gates on release.
    The maintained image lives at `ghcr.io/claymore666/docker-net-dhcp`.

!!! danger "⚠️ BREAKING CHANGE IN v1.5.0 — DO THIS FIRST ⚠️"

    ```bash
    sudo mkdir -p /var/lib/net-dhcp
    ```

    v1.5.0 is the first release that **bind-mounts its state directory
    from the host** (so leases survive an upgrade), and **Docker will
    not create a missing bind source.** Run the line above before
    `docker plugin install`, on every host, new install or upgrade.

    **If you skip it**, `docker plugin install` fails at start-up and
    leaves the plugin **installed but disabled** — and re-running the
    exact same install command then answers only
    `plugin ... already exists`, which says nothing about the cause.
    Recover with:

    ```bash
    sudo mkdir -p /var/lib/net-dhcp
    docker plugin enable ghcr.io/claymore666/docker-net-dhcp:v1.7.0
    ```

    Nothing is lost or corrupted. Full detail:
    [the reference](reference.md#install-upgrade-uninstall).

## Quick start

Install the plugin:

```bash
# One-time, and REQUIRED — see the warning above. Docker will not
# create this directory for you, and `plugin install` fails at
# start-up without it.
sudo mkdir -p /var/lib/net-dhcp
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.7.0
```

It requests `host` networking, the host PID namespace, the Docker
socket, a bind mount of the state directory above, a read-only bind
mount of `/var/run/docker` (v1.6.0+), and
`CAP_NET_ADMIN`/`CAP_SYS_ADMIN`/`CAP_SYS_PTRACE` — grant them to
proceed.
(If you hit `invalid rootfs in image configuration`, upgrade Docker.)

Create a bridge-mode network and run a container on it (assumes you
already have a host bridge `my-bridge` on your LAN — see
[Bridge mode](bridge-mode.md) for that one-time setup):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.7.0 \
  --ipam-driver null -o bridge=my-bridge my-dhcp-net

docker run --rm -ti --network my-dhcp-net alpine ip address show
```

The `null` IPAM driver is **mandatory** — it stops Docker handing out
addresses that would collide with the real LAN.

## Attachment modes

Selected by the `mode` driver option:

| mode | parent | host changes required |
| ---- | ------ | --------------------- |
| `bridge` (default) | a Linux bridge you maintain (`-o bridge=<name>`) | yes — you bring the bridge |
| `macvlan` | a host NIC (`-o parent=<iface>`) | none |
| `ipvlan` (L2) | a host NIC (`-o parent=<iface>`) | none |

macvlan/ipvlan attach directly to a host NIC without a bridge — the
right pick when you don't want to reconfigure the host's networking.

## Documentation

- **[Driver reference](reference.md)** — **the manual.** Every option,
  setting, and counter, plus lease behaviour, observability, Compose
  usage, and troubleshooting. Start at
  [At a glance](reference.md#at-a-glance) for the one-screen list of
  everything you can set.
- **[Bridge mode](bridge-mode.md)** — host bridge setup + end-to-end
  walkthrough.
- **[macvlan / ipvlan modes](parent-attached-modes.md)** — choosing
  between the two, quick start, and the mode-specific constraints.
- **[How it works](internals.md)** — the mechanism, for contributors:
  the veth + DHCP-client flow, and how state survives a restart.
- **[Roadmap](roadmap.md)** — where the project is going over the next
  year, and what it deliberately will not do.
- **[Release runbook](release-runbook.md)** — maintainer-facing publish
  procedure.

## Images & releases

This fork publishes semver-tagged plugin images on GHCR
(`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`) and mirrors them to
Docker Hub (`claymore666/net-dhcp`). Pin a version (`:vX.Y.Z`) for
reproducibility, or track `:latest`.

Published builds: **`linux/amd64`** on the bare tag and
**`linux/arm64`** as `:vX.Y.Z-arm64` / `:latest-arm64` (v1.7.0 onward).
The architecture is in the tag because a Docker *plugin* cannot be
installed from a multi-architecture manifest list at all: the daemon
reads a plugin's privileges before pulling it, its manifest handler
matches single manifests only, and an index therefore fails with `did
not find plugin config for specified reference` on every architecture —
with no `--platform` to steer it. On an arm64 host the `-arm64` tag
replaces the bare one in **every** snippet that names the image. That
includes `docker network create -d <image>`: a network records the
tagged plugin reference as its driver, so leaving the bare tag there
names a plugin the host does not have.

- [GHCR package](https://github.com/claymore666/docker-net-dhcp/pkgs/container/docker-net-dhcp)
- [GitHub Releases](https://github.com/claymore666/docker-net-dhcp/releases)
  — per-release notes, credits, and signed artifacts.

This documentation is **versioned**: use the selector in the header to
read the docs matching the plugin version you have installed.

## Verifying releases

Every release (v1.1.0 onward) is signed and attested via Sigstore. The
published plugin image is signed with cosign (keyless), carries SLSA
build provenance, and ships an SBOM; the release-artifact `checksums.txt`
manifest is cosign-signed so one signature covers every attached file.
The full, copy-pasteable procedure lives in
**[Verifying releases](verifying-releases.md)**, and every
[GitHub Release](https://github.com/claymore666/docker-net-dhcp/releases)
links to it. Both commands need **cosign v3 or newer** — v2 cannot read the
Sigstore bundle format the release signs with, and fails in a way that looks
like a broken signature. In brief (replace `VERSION`):

```bash
# image signature
cosign verify ghcr.io/claymore666/docker-net-dhcp:VERSION \
  --certificate-identity-regexp '^https://github.com/claymore666/docker-net-dhcp/.github/workflows/release.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# SLSA build provenance (image + release artifacts)
gh attestation verify oci://ghcr.io/claymore666/docker-net-dhcp:VERSION --repo claymore666/docker-net-dhcp
```

## Project & community

- **Contributing:** open a pull request against the `dev` branch — see
  the [Contributing section in the README](https://github.com/claymore666/docker-net-dhcp#contributing).
- **Security policy / vulnerability reporting:**
  [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/dev/SECURITY.md)
  (do not open public issues for vulnerabilities).
- **Bug reports & feature requests:** the
  [issue forms](https://github.com/claymore666/docker-net-dhcp/issues/new/choose).

## License

GPL-3.0 — see
[LICENSE.md](https://github.com/claymore666/docker-net-dhcp/blob/dev/LICENSE.md).
This is a fork of
[`devplayer0/docker-net-dhcp`](https://github.com/devplayer0/docker-net-dhcp),
which is GPL-3.0; as a derivative work it stays under the same license.
