# Release notes

This is a maintained fork of [`devplayer0/docker-net-dhcp`][upstream]. The
upstream repository has not been updated in several years and does not
build on current Docker hosts; the goals of this fork are (1) keep the
plugin building and running on modern Docker, (2) add a macvlan
attachment mode so containers can pick up DHCP leases from the LAN
without requiring the operator to maintain a host bridge, and (3)
incorporate sensible improvements from open upstream PRs and other
forks that have been waiting on review.

[upstream]: https://github.com/devplayer0/docker-net-dhcp

## v0.4.1

- **Critical fix:** added `sync.Mutex` to the `Plugin` struct guarding
  the `joinHints` and `persistentDHCP` maps, which were being mutated
  from concurrent `CreateEndpoint` / `Join` / `Leave` HTTP handlers
  without synchronisation. The race detector reproduced a concurrent
  map read+write that crashes the plugin under realistic load
  (multi-service compose-up, daemon-restart restoration sweep). This
  is the C-1 finding from the internal code review; inherited from
  upstream and present in every fork in the survey.
- **Race-time fix:** `Join` now registers the `dhcpManager` *before*
  spawning the goroutine that calls `Start`, so a fast `Leave` doesn't
  silently lose the lease-renewal client. `dhcpManager.Stop` blocks
  until `Start` has finished and short-circuits if `Start` failed.
- **Test suite:** added ~750 LOC of tests across `pkg/plugin` and
  `pkg/util`. CI on push/PR runs `go build`, `go vet`, `gofmt -l`,
  `staticcheck`, and `go test -race`.
- **Lint sweep:** all static-analysis findings from the internal code
  review (`go vet`, `staticcheck`, `gofmt`, the actionable subset of
  `errcheck`) cleared.
- Atomic write for persisted network options (temp + rename) instead
  of best-effort `os.WriteFile`.
- `JSONResponse` encodes to a buffer first so encoding failures
  produce a clean HTTP 500 instead of a half-flushed body and a
  no-op second `WriteHeader`.
- Dropped the broken upstream `.github/workflows/build.yaml` and
  `release.yaml`; replaced with a minimal `test.yaml` that runs the
  test suite on push/PR. The plugin image continues to be built and
  pushed manually via `make push`.
- Renamed `pkg/plugin/macvlan.go` to `pkg/plugin/parent_attached.go`
  to reflect that the file owns both macvlan and ipvlan paths.

## v0.4.0

- New `mode=ipvlan` attachment mode (L2 submode), as a third value
  for the existing `mode` driver option. Useful when the upstream
  switch or hypervisor refuses to bridge multiple MACs from one port
  (sticky-MAC port security, hostile vSwitches, some Wi-Fi APs).
  ipvlan children share the parent's MAC and differentiate by IP.
- ipvlan rejects custom MACs (kernel design); macvlan continues to
  accept `--mac-address`.
- `docs/macvlan.md` renamed to `docs/parent-attached-modes.md` since
  it now covers both modes.
- Internal: macvlan-specific helper names rebranded as
  `parent-attached` to reflect the shared lifecycle.

## v0.3.0

- Persist per-network options to disk so per-endpoint handlers don't
  call back into the docker API on the hot path. Fixes the upstream
  daemon-restart deadlock. **Configurable** via `STATE_DIR` env var
  (default `/var/lib/net-dhcp`).
- New `gateway` driver option to override the IPv4 gateway returned
  by DHCP (useful for VPN-egress / split-horizon LANs).
- 2-second timeout on all docker client requests as a safety net for
  any path that still talks to the docker socket.
- `driverRegexp` now matches any registry namespace, so the
  bridge-conflict scan keeps working under forks published under a
  name other than `ghcr.io/devplayer0`.

## v0.2.0

- New `mode=macvlan` attachment mode (see below).
- Modernized toolchain and dependency tree (see below).

## Changes vs. upstream

### Macvlan attachment mode (new)

A new driver option `mode` selects between the existing bridge attachment
and a new macvlan attachment:

```bash
docker network create \
    --driver=<this-plugin> \
    --ipam-driver=null \
    -o mode=macvlan \
    -o parent=ens18 \
    lan-dhcp
```

In `mode=macvlan` the plugin creates a macvlan child on the named
parent NIC (submode `bridge`, so children on the same parent can talk to
each other), runs `udhcpc` on it to acquire a lease from the LAN's DHCP
server, and hands the link to libnetwork. Docker moves the link into the
container's network namespace; a persistent `udhcpc` keeps the lease
alive for the life of the endpoint and sends `DHCPRELEASE` on container
stop so the upstream server doesn't accumulate stale leases.

The host's NIC configuration is never modified. There is no host bridge
requirement, no per-container compose plumbing, no sidecar, no `cap_add`.
Adding a container to the network is `networks: [<name>]` and nothing
else.

See [`docs/parent-attached-modes.md`](docs/parent-attached-modes.md) for the full how-to.

### Bridge mode

Bridge mode is unchanged. Networks created without `-o mode` (or with
`-o mode=bridge`) behave exactly as they did upstream.

### Toolchain and dependency modernization

The upstream plugin pinned Go 1.16, Docker SDK v20.10.7, and Alpine 3.14
— old enough that the build no longer works on current hosts and recent
Docker daemons hung at startup with the plugin enabled. This fork bumps:

- Go 1.16 → 1.25
- Alpine 3.14 → current (`alpine` / `golang:1.25-alpine`)
- `github.com/docker/docker` v20.10.7 → v28.4.0
- `github.com/vishvananda/netlink` → v1.3.0
- `github.com/vishvananda/netns` → v0.0.4
- `github.com/sirupsen/logrus` → v1.9.3
- `github.com/gorilla/handlers` → v1.5.2
- `github.com/mitchellh/mapstructure` → v1.5.0
- `golang.org/x/sys` → v0.42.0

Code changes for v28's package split:

- `api/types.NetworkListOptions` / `NetworkInspectOptions` →
  `api/types/network.ListOptions` / `InspectOptions`
- `api/types.ContainerJSON` → `api/types/container.InspectResponse`
- `client.NewClient(host, version, http, headers)` (removed) →
  `client.NewClientWithOpts(WithHost(...), WithAPIVersionNegotiation())`

The `iproute2` package is now installed in the runtime rootfs so the
plugin image has working `ip` for diagnostic shells.

## Installation

Build and push the plugin image to a registry you control:

```bash
make PLUGIN_NAME=<your-registry>/docker-net-dhcp PLUGIN_TAG=latest push
```

Then on each host:

```bash
docker plugin install <your-registry>/docker-net-dhcp:latest
```

The plugin requests the following privileges (same as upstream):

- network: `host`
- host pid namespace: `true`
- mount: `/var/run/docker.sock`
- capabilities: `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE`

## Backward compatibility

- Networks created by upstream `devplayer0/docker-net-dhcp` continue to
  work — bridge mode is the default and the option schema is a strict
  superset of upstream's.
- The driver name (`net-dhcp`) and Docker plugin manifest are unchanged.
- The bridge-conflict scan recognizes plugin instances by image name
  (`*/docker-net-dhcp:*`), so it works regardless of which registry
  namespace the plugin was published under — including the upstream
  `ghcr.io/devplayer0` and any fork. Upstream's regex was pinned to
  the upstream namespace; this fork loosened it.

## Credits

This fork stands on the shoulders of work that originated elsewhere.
With thanks to:

- **[@devplayer0](https://github.com/devplayer0)** — author of the
  original plugin. Everything in `bridge` mode is their design.
- **[@aczwink](https://github.com/aczwink)** — independently
  diagnosed the daemon-restart deadlock and shipped the
  persist-options-to-disk fix in
  [aczwink/docker-net-dhcp@c060b9c9](https://github.com/aczwink/docker-net-dhcp/commit/c060b9c9).
  This fork's persistence implementation is inspired by that approach,
  with state moved to a dedicated state directory and a graceful
  fallback to the docker API for networks that pre-date the change.
- **[@asheliahut](https://github.com/asheliahut)** — proposed the
  Docker client request timeout in upstream PR
  [#34](https://github.com/devplayer0/docker-net-dhcp/pull/34).
- **[@Vigilans](https://github.com/Vigilans)** — proposed the
  `gateway` override option in upstream PR
  [#32](https://github.com/devplayer0/docker-net-dhcp/pull/32).
- **[@relet](https://github.com/relet)** — proposed the
  package-bump-and-API-version-removal modernization in upstream PR
  [#43](https://github.com/devplayer0/docker-net-dhcp/pull/43); the
  spirit of that PR is reflected in this fork's Phase A modernization.
- **[@LANCommander](https://github.com/LANCommander)** — independently
  built both macvlan and ipvlan support side-by-side in
  [LANCommander/docker-net-dhcp](https://github.com/LANCommander/docker-net-dhcp).
  This fork's ipvlan addition (v0.4.0) is inspired by their approach;
  the macvlan implementation here predates and differs in its UX
  (separate `parent` option) and link rediscovery (MAC-based) but
  arrives at the same place semantically.
- The dependabot bumps that have been waiting on review in upstream
  (#35–#38) — superseded by the broader Phase A bump here.

## Security advisory assessment

`govulncheck` reports two vulnerabilities in `github.com/docker/docker`:

| ID | Description | Status |
|---|---|---|
| [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) | Moby AuthZ plugin bypass on oversized request bodies | **Not applicable** |
| [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883) | Moby off-by-one in plugin privilege validation | **Not applicable** |

Both vulnerabilities live in **Moby daemon** (server-side authz/privilege)
code, not in the client SDK we consume. Our usage of
`github.com/docker/docker` is exclusively the `client` package
(`NewClientWithOpts`, `NetworkInspect`, `NetworkList`,
`ContainerInspect`); the vulnerable code paths are in `daemon.*`, which
this codebase neither imports nor links. `govulncheck` flags any module
with a vuln conservatively without distinguishing client vs. daemon.

If you point `govulncheck` at a future build of this plugin and see
these IDs, the assessment above still holds unless the call graph
changes to include `daemon.*`. New vulnerabilities reported in
`docker/docker/client` should be re-evaluated.

## Known limitations

- The DHCP exchange uses BusyBox `udhcpc` / `udhcpc6`. Anything that
  requires a fuller DHCP client (vendor extensions beyond `-V`,
  RFC3315 reconfigure, etc.) is not supported.
- One DHCP-served network per container. Joining additional bridges
  works but may interact in surprising ways with the routing rules
  installed by the persistent client.
- The persistent client cannot currently handle the DHCP server handing
  out a different IP at renewal time. The lease must be sticky enough
  to survive a renewal cycle. This is a pre-existing upstream limitation
  documented in the source.
