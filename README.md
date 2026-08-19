# docker-net-dhcp

[![Test](https://github.com/claymore666/docker-net-dhcp/actions/workflows/test.yaml/badge.svg)](https://github.com/claymore666/docker-net-dhcp/actions/workflows/test.yaml)
[![Integration](https://github.com/claymore666/docker-net-dhcp/actions/workflows/integration.yml/badge.svg)](https://github.com/claymore666/docker-net-dhcp/actions/workflows/integration.yml)
[![Dependencies](https://img.shields.io/badge/dependencies-Dependabot%20%2B%20govulncheck-brightgreen?logo=dependabot)](https://github.com/claymore666/docker-net-dhcp/network/updates)
[![Release](https://img.shields.io/github/v/release/claymore666/docker-net-dhcp?sort=semver)](https://github.com/claymore666/docker-net-dhcp/releases)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/claymore666/docker-net-dhcp/badge)](https://scorecard.dev/viewer/?uri=github.com/claymore666/docker-net-dhcp)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13229/badge)](https://www.bestpractices.dev/projects/13229)
[![OpenSSF silver](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fwww.bestpractices.dev%2Fprojects%2F13229.json&query=%24.badge_percentage_1&label=OpenSSF%20silver&suffix=%25&color=9e9e9e)](https://www.bestpractices.dev/projects/13229/silver)
[![OpenSSF gold](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fwww.bestpractices.dev%2Fprojects%2F13229.json&query=%24.badge_percentage_2&label=OpenSSF%20gold&suffix=%25&color=b8860b)](https://www.bestpractices.dev/projects/13229/gold)
[![Docs](https://img.shields.io/badge/docs-claymore666.github.io-blue?logo=materialformkdocs&logoColor=white)](https://claymore666.github.io/docker-net-dhcp/)

A Docker network plugin that allocates container IP addresses (IPv4 and
optionally IPv6) from an **existing DHCP server** — your router, a
Fritz!Box, dnsmasq, anything — instead of Docker's self-managed IPAM
pools. Containers come up on your LAN as first-class hosts, addressable
like any other machine. Bridge, macvlan, and ipvlan attachment modes.

> **This is a maintained fork** of [`devplayer0/docker-net-dhcp`][fork-parent]
> (quiet since 2021, no longer builds on current Docker). This fork
> modernises the toolchain (Go 1.26, docker SDK v28, current Alpine),
> adds **macvlan** and **ipvlan** modes, fixes the daemon-restart
> deadlock and a state data-race, and gates every PR on a live
> integration suite (all three modes + DHCPv6, recovery, failure
> injection) with a coverage ratchet and supply-chain gates on release.
> The maintained image lives at `ghcr.io/claymore666/docker-net-dhcp`.

[fork-parent]: https://github.com/devplayer0/docker-net-dhcp

> [!WARNING]
> **⚠️ BREAKING CHANGE IN v1.5.0 — DO THIS FIRST ⚠️**
>
> ```bash
> sudo mkdir -p /var/lib/net-dhcp
> ```
>
> v1.5.0 is the first release that **bind-mounts its state directory
> from the host** (so leases survive an upgrade), and **Docker will not
> create a missing bind source.** Run the line above before
> `docker plugin install`, on every host, new install or upgrade.
>
> **If you skip it**, `docker plugin install` fails at start-up and
> leaves the plugin **installed but disabled** — and re-running the
> exact same install command then answers only
> `plugin ... already exists`, which says nothing about the cause.
> Recover with:
>
> ```bash
> sudo mkdir -p /var/lib/net-dhcp
> docker plugin enable ghcr.io/claymore666/docker-net-dhcp:v1.7.0
> ```
>
> On arm64, enable the `-arm64` plugin instead — that is the one that
> was installed, and the bare reference names nothing on that host.
>
> Nothing is lost or corrupted. Full detail:
> [the reference](docs/reference.md#install-upgrade-uninstall).

## Quick start

Install the plugin:

```bash
# One-time, and REQUIRED — see the warning above. Docker will not
# create this directory for you, and `plugin install` fails at
# start-up without it.
sudo mkdir -p /var/lib/net-dhcp

# amd64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.7.0

# arm64 (v1.7.0 onward) — the architecture is in the tag, see below
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.7.0-arm64
```

It requests `host` networking, the host PID namespace, the Docker
socket, a bind mount of the state directory above, a read-only bind
mount of `/var/run/docker` (v1.6.0+), and
`CAP_NET_ADMIN`/`CAP_SYS_ADMIN`/`CAP_SYS_PTRACE` — grant them to
proceed.
(If you hit `invalid rootfs in image configuration`, upgrade Docker.)

Create a bridge-mode network and run a container on it (assumes you
already have a host bridge `my-bridge` on your LAN — see
[bridge mode](docs/bridge-mode.md) for that one-time setup):

```bash
# On arm64 use the -arm64 tag here too — a network stores this exact
# reference as its driver, so it must name the plugin you installed.
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

A versioned documentation site is published at
**<https://claymore666.github.io/docker-net-dhcp/>** (pick your plugin
version from the selector). The same content lives in `docs/` in the repo:

- **[Driver reference](docs/reference.md)** — **the manual.** Every
  option, setting, and counter, plus lease behaviour, observability,
  Compose usage, and troubleshooting. Its
  [At a glance](docs/reference.md#at-a-glance) section lists everything
  you can set on one screen.
- **[Bridge mode](docs/bridge-mode.md)** — host bridge setup +
  end-to-end walkthrough.
- **[macvlan / ipvlan modes](docs/parent-attached-modes.md)** — choosing
  between the two, quick start, and the mode-specific constraints.
- **[How it works](docs/internals.md)** — the mechanism, for
  contributors: the veth + DHCP-client flow, and how state survives a
  restart.
- **[Roadmap](docs/roadmap.md)** — where the project is going over the
  next year, and what it deliberately will not do.
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

This fork publishes semver-tagged images to two registries — GHCR is
primary (`ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z`), mirrored to
Docker Hub as `claymore666/net-dhcp:vX.Y.Z`. Every snippet here uses the
GHCR reference. See the
[Releases page](https://github.com/claymore666/docker-net-dhcp/releases).

Published builds: **`linux/amd64`** on the bare tag (`:vX.Y.Z`,
`:latest`) and **`linux/arm64`** on an arch-suffixed tag
(`:vX.Y.Z-arm64`, `:latest-arm64`; v1.7.0 onward). The architecture is
in the tag rather than in a manifest because a Docker *plugin* cannot be
installed from a multi-architecture manifest list at all: the daemon
reads a plugin's privileges before pulling it and its manifest handler
matches single manifests only, so an index fails with `did not find
plugin config for specified reference` — for every architecture,
including the one you are on — and `docker plugin install` has no
`--platform` to steer it. The `-arm64` tag replaces the bare one in
**every** snippet that names the image, not only the install line: a
network stores the tagged reference as its driver, so a bare tag in
`docker network create -d` would name a plugin the host does not have.
Both architectures are signed and attested identically
(see below). 32-bit ARM is not built.

## Verifying releases

Every release (v1.1.0 onward) is signed and attested via Sigstore. The
published plugin image is signed with cosign (keyless), carries SLSA
build provenance, and ships an SBOM; the release-artifact `checksums.txt`
manifest is cosign-signed so one signature covers every attached file.
The full, copy-pasteable procedure lives in
**[Verifying releases](docs/verifying-releases.md)** (also on the
[docs site](https://claymore666.github.io/docker-net-dhcp/verifying-releases/)),
and every [GitHub Release](https://github.com/claymore666/docker-net-dhcp/releases)
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

## Contributing

Contributions are welcome.

- **Looking for somewhere to start?** Issues tagged
  [`good first issue`](https://github.com/claymore666/docker-net-dhcp/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
  are self-contained and need no deep project context — each one states the
  gap, where to look, and how to check that you fixed it. They are real work
  that would otherwise sit undone, not busywork.
- **Questions, bugs, and feature requests:** open a [GitHub issue](https://github.com/claymore666/docker-net-dhcp/issues).
  For bugs, please include the plugin version, your Docker version, the network
  mode (`bridge`, `macvlan`, or `ipvlan`), and the relevant plugin log.
  Docker has no `plugin logs` subcommand — the log lives in two places
  and [Plugin log](docs/reference.md#plugin-log) shows how to read
  either: `sudo journalctl -u docker | grep net-dhcp` on a systemd host
  (the copy that survives an upgrade), or the file inside the plugin
  rootfs.
- **Code changes:** open a pull request against the `dev` branch (not `main`).
  Requirements for an acceptable contribution:
  - **Coding standard:** Go code must be formatted with `gofmt` and pass
    `go vet` and [`staticcheck`](https://staticcheck.dev/); shell and workflow
    files must pass `shellcheck`/`actionlint`. These are enforced in CI.
  - **Tests:** new functionality is expected to ship with tests; a coverage
    ratchet enforces this at release time. Run them with `go test ./...`
    for the fast loop and `sudo make integration-local` for the live
    suites — see
    [Running the tests](docs/internals.md#running-the-tests). Use that
    target rather than `make integration-test` directly: the latter does
    not rebuild, so it silently tests whatever plugin is already
    installed.
  - **Authorship:** commits and pull request descriptions must not carry
    AI-assistant attribution — no `Co-authored-by:` trailer naming an
    assistant or an assistant's no-reply address, no "Generated with …"
    line, no assistant session trailer or link — and the commit author must
    be a person. Using an assistant to help write a change is fine and needs
    no disclosure; what the project asks is that you sign the work as its
    author and stand behind it. This is enforced by the `attribution` check,
    which reads every commit in your PR — message *and* author identity,
    since a rebase preserves authorship — plus the PR description. In the
    description, code blocks and inline code are stripped before scanning,
    so you can quote a trailer to discuss one, as here; commit messages are
    scanned in full and have no such escape.
  - **Green CI:** every PR must pass the repository's required checks
    before it can be merged. Branch protection holds the authoritative
    list and your PR's checks panel shows it applied to your branch — at
    the time of writing it is unit tests, `staticcheck`, the live
    integration suite, `govulncheck`, `actionlint`, CodeQL (`Analyze
    (go)` and `Analyze (actions)`), and `attribution`. (Docs-only PRs — diffs touching nothing but
    `*.md` — satisfy the integration check via a fast in-job skip; any code,
    script, or workflow change runs the full suite.)
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

GPL-3.0 — see [LICENSE.md](LICENSE.md). This is a fork of
[`devplayer0/docker-net-dhcp`][fork-parent], which is GPL-3.0; as a
derivative work it stays under the same license.
