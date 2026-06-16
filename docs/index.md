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

## Quick start

Install the plugin:

```bash
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.2.0
```

It requests `host` networking, the host PID namespace, the Docker
socket, and `CAP_NET_ADMIN`/`CAP_SYS_ADMIN` — grant them to proceed.
(If you hit `invalid rootfs in image configuration`, upgrade Docker.)

Create a bridge-mode network and run a container on it (assumes you
already have a host bridge `my-bridge` on your LAN — see
[Bridge mode](bridge-mode.md) for that one-time setup):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.2.0 \
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

- **[Driver reference](reference.md)** — every option, plugin setting,
  the `/Plugin.Health` observability endpoint, and troubleshooting.
- **[Bridge mode](bridge-mode.md)** — host bridge setup + end-to-end
  walkthrough.
- **[macvlan / ipvlan modes](parent-attached-modes.md)** — NIC
  attachment, DHCP identity (hostname / option 60 / 61), restart
  stability, recovery.
- **[How it works](internals.md)** — the veth + DHCP-client mechanism
  and lease flow.
- **[Release runbook](release-runbook.md)** — maintainer-facing publish
  procedure.

## Images & releases

This fork publishes semver-tagged plugin images on GHCR
(`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`, `linux/amd64`; ARM via
the build pipeline on request) and mirrors them to Docker Hub
(`claymore666/net-dhcp`). Pin a version (`:vX.Y.Z`) for reproducibility,
or track `:latest`.

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
The exact, copy-pasteable commands — pinned to that release's tag — are
appended to each [GitHub Release](https://github.com/claymore666/docker-net-dhcp/releases)
under **Verifying the signed artifacts**. In brief (replace `VERSION`):

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
