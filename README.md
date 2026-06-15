# docker-net-dhcp

[![Test](https://github.com/claymore666/docker-net-dhcp/actions/workflows/test.yaml/badge.svg)](https://github.com/claymore666/docker-net-dhcp/actions/workflows/test.yaml)
[![Integration](https://github.com/claymore666/docker-net-dhcp/actions/workflows/integration.yml/badge.svg)](https://github.com/claymore666/docker-net-dhcp/actions/workflows/integration.yml)
[![Dependencies](https://img.shields.io/badge/dependencies-Dependabot%20%2B%20govulncheck-brightgreen?logo=dependabot)](https://github.com/claymore666/docker-net-dhcp/network/updates)
[![Release](https://img.shields.io/github/v/release/claymore666/docker-net-dhcp?sort=semver)](https://github.com/claymore666/docker-net-dhcp/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/claymore666/docker-net-dhcp)](https://goreportcard.com/report/github.com/claymore666/docker-net-dhcp)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/claymore666/docker-net-dhcp/badge)](https://scorecard.dev/viewer/?uri=github.com/claymore666/docker-net-dhcp)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13229/badge)](https://www.bestpractices.dev/projects/13229)
[![Docs](https://img.shields.io/badge/docs-claymore666.github.io-blue?logo=materialformkdocs&logoColor=white)](https://claymore666.github.io/docker-net-dhcp/)

A Docker network plugin that allocates container IP addresses (IPv4 and
optionally IPv6) from an **existing DHCP server** — your router, a
Fritz!Box, dnsmasq, anything — instead of Docker's self-managed IPAM
pools. Containers come up on your LAN as first-class hosts, addressable
like any other machine. Bridge, macvlan, and ipvlan attachment modes.

> **This is a maintained fork** of [`devplayer0/docker-net-dhcp`][upstream]
> (quiet since 2021, no longer builds on current Docker). This fork
> modernises the toolchain (Go 1.26, docker SDK v28, current Alpine),
> adds **macvlan** and **ipvlan** modes, fixes the daemon-restart
> deadlock and a state data-race, and gates every PR on a live
> integration suite (all three modes + DHCPv6, recovery, failure
> injection) with a coverage ratchet and supply-chain gates on release.
> The maintained image lives at `ghcr.io/claymore666/docker-net-dhcp`.

[upstream]: https://github.com/devplayer0/docker-net-dhcp

## Quick start

Install the plugin:

```bash
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.1.1
```

It requests `host` networking, the host PID namespace, the Docker
socket, and `CAP_NET_ADMIN`/`CAP_SYS_ADMIN` — grant them to proceed.
(If you hit `invalid rootfs in image configuration`, upgrade Docker.)

Create a bridge-mode network and run a container on it (assumes you
already have a host bridge `my-bridge` on your LAN — see
[bridge mode](docs/bridge-mode.md) for that one-time setup):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.1.1 \
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

A versioned documentation site is published at
**<https://claymore666.github.io/docker-net-dhcp/>** (pick your plugin
version from the selector). The same content lives in `docs/` in the repo:

- **[Driver reference](docs/reference.md)** — every option, plugin
  setting, the `/Plugin.Health` observability endpoint, and
  troubleshooting.
- **[Bridge mode](docs/bridge-mode.md)** — host bridge setup +
  end-to-end walkthrough.
- **[macvlan / ipvlan modes](docs/parent-attached-modes.md)** — NIC
  attachment, DHCP identity (hostname / option 60 / 61), restart
  stability, recovery.
- **[How it works](docs/internals.md)** — the veth + DHCP-client
  mechanism and lease flow.
- **[Changelog](RELEASE_NOTES.md)** — per-release notes and credits.
- **[Release runbook](docs/release-runbook.md)** — maintainer-facing
  publish procedure.

## Project & community

- **Contributing:** see [below](#contributing).
- **Security policy / vulnerability reporting:** [SECURITY.md](SECURITY.md)
  (do not open public issues for vulnerabilities).
- **Bug reports & feature requests:** the
  [issue forms](https://github.com/claymore666/docker-net-dhcp/issues/new/choose).
- **Pull requests:** the
  [PR template](.github/PULL_REQUEST_TEMPLATE.md) — target the `dev` branch.
- **Governance & code of conduct:** [GOVERNANCE.md](GOVERNANCE.md),
  [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

This fork publishes semver-tagged images on GHCR
(`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`, `linux/amd64`; ARM via
the build pipeline on request). See the
[Releases page](https://github.com/claymore666/docker-net-dhcp/releases).

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

## Contributing

Contributions are welcome.

- **Questions, bugs, and feature requests:** open a [GitHub issue](https://github.com/claymore666/docker-net-dhcp/issues).
  For bugs, please include the plugin version, your Docker version, the network
  mode (`bridge`, `macvlan`, or `ipvlan`), and the relevant output from
  `docker plugin logs <plugin>`.
- **Code changes:** open a pull request against the `dev` branch (not `main`).
  Requirements for an acceptable contribution:
  - **Coding standard:** Go code must be formatted with `gofmt` and pass
    `go vet` and [`staticcheck`](https://staticcheck.dev/); shell and workflow
    files must pass `shellcheck`/`actionlint`. These are enforced in CI.
  - **Tests:** new functionality is expected to ship with tests; a coverage
    ratchet enforces this at release time.
  - **Green CI:** every PR must pass the required checks — unit tests,
    `staticcheck`, the live integration suite, `govulncheck`, and `actionlint` —
    before it can be merged.
  - **Hosted cross-check:** a separate, *non-required* workflow runs the
    integration suite on a stock GitHub-hosted runner on a weekly schedule
    (and on demand) to validate the plugin against a vanilla distro's Docker.
    It is a portability probe, not a PR gate — a red there flags the hosted
    environment, not your change.
- **Security vulnerabilities:** do **not** open a public issue — follow the
  private process described in [SECURITY.md](SECURITY.md).

This is an actively maintained fork. It is solo-maintained, so please allow a
few days for a response.

## License

MIT — see [LICENSE.md](LICENSE.md).
