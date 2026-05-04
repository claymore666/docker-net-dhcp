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

## v0.5.3

Hotfix for a CPU-burning busy loop and a process-leak in the
persistent-DHCP path. Operators on v0.5.0 – v0.5.2 should upgrade.

### Closed udhcpc event channel turned the consumer into a CPU spinner

The persistent-DHCP consumer goroutine in `dhcpManager.setupClient`
selected on `<-events` without checking `ok`. When udhcpc exited on
its own (server NAK on a renew, parent NIC vanished, container netns
torn down), the scanner goroutine in `udhcpc.Start` closed `events`,
and from that point every iteration of the consumer's `select` took
the now-always-ready `<-events` branch and got a zero-value
`Event{}`. The switch matched no case, the loop iterated, and the
goroutine pegged a CPU thread forever — silently, with no log
output. Observed in the field as ~70 % of one host core sustained
for 1 d 14 h with seven hot Go runtime threads.

The consumer now uses the comma-ok form, logs the unexpected close,
reaps the udhcpc child via `client.Wait` (see below), and posts to
`errChan` so a concurrent `Stop` doesn't deadlock waiting on a
goroutine that's already gone.

### Zombie udhcpc child when the process exited unexpectedly

`cmd.Wait` was only ever called from `Finish`, which assumed `Stop`
would drive teardown. When udhcpc died on its own, nobody called
`Wait`, so the kernel kept the child as a zombie until plugin
shutdown. `udhcpc.Finish` is now split into a signal phase plus a
new `Wait(ctx)` method, and the consumer calls `Wait` from the
events-closed branch above to reap.

### `Await*` helper goroutines leaked on context cancel

`util.AwaitCondition`, `AwaitNetNS`, `AwaitLinkByIndex`, and
`AwaitContainerInspect` ran their poll in a side goroutine that
didn't observe `ctx`: when the outer `select` returned via
`<-ctx.Done()`, the poller kept calling its expensive operation
(Docker `NetworkInspect`, `netns.GetFromPath`, `LinkByIndex`,
`ContainerInspect`) every 100 ms forever, and blocked permanently on
the unbuffered result channel. Each leaked poller meant ~10
syscalls/s, accumulating across plugin restarts and per-endpoint
recovery attempts. All four helpers are now synchronous loops with
`select` on `ctx.Done()` between iterations.

## v0.5.2

Quick-wins cleanup pass on warning-level findings from the v0.5.0
code review. No new features; ten issues closed at low risk.

### Lease release on plugin shutdown (W-10)

`Plugin.Close` now stops every persistent DHCP client before
returning, in parallel with a 5-second total ceiling. This is what
v0.5.0's "send DHCPRELEASE on stop" contract was supposed to deliver
at the per-endpoint level — but plugin upgrade / `docker plugin
disable` previously bypassed it entirely, killing udhcpc children
with no chance to release. Result was orphaned leases on the
upstream DHCP server after every upgrade.

### Other fixes

- `parseExplicitV4` and `parseDriverOptIP` now reject `0.0.0.0` /
  unspecified IPv4 addresses — `udhcpc -r 0.0.0.0` is a malformed
  REQUEST hint (W-8).
- `Leave` refreshes the endpoint fingerprint from `manager.LastIP*`
  *unconditionally*, so a wedged-udhcpc shutdown still produces a
  tombstone with the latest known lease instead of stale
  initial-DISCOVER values (W-4).
- `dhcpManager.Stop`'s deferred `nsHandle.Close` / `netHandle.Close`
  now guard against zero values, so a Start that failed before
  AwaitNetNS no longer emits noisy EBADF on Stop (W-7).
- `consumeTombstone` drops *all* matching tombstones when the match
  is ambiguous, so the next consume isn't poisoned by the same
  ambiguity for the rest of the TTL window (W-3).
- `udhcpc.GetIP` no longer mutates the caller's `opts.Once` (I-7).

### Hygiene

- Makefile `PLUGIN_NAME` defaults to this fork's registry instead of
  the upstream one this fork can't push to (N-12).
- `cmd/net-dhcp/main.go` `AWAIT_TIMEOUT` default changed from 5s to
  10s to match `config.json` (N-4).
- `.dockerignore` excludes `.git/`, `.github/`, `docs/`, `scripts/`,
  `*.md` — saves ~8MB of context per build (N-5).

### Tests

- `TestParseExplicitV4` / `TestParseDriverOptIP` cover unspecified
  addresses (`0.0.0.0`, `0.0.0.0/0`, `0.0.0.0/24`).
- `TestTombstones_AmbiguousMatchesDropped` pins the W-3 fix.

Phase D smoke on gpu1 walked through D2 (LAN IP), plugin
disable/enable (lease persisted across the bounce), teardown.

## v0.5.1

Critical-bug cleanup pass driven by a full code-review of the v0.5.0
codebase. No new features; six classes of latent bug closed.

### Identity swap during sequential `compose restart` (C-5)

Tombstones in v0.5.0 were keyed only by NetworkID. A `docker compose
restart` of N containers on the same network could let container B
inherit container A's MAC during the brief 10s TTL window where A's
tombstone was fresh and B's was not yet written.

Fixed by extending the tombstone with the container's hostname (which
survives `docker restart`) and narrowing `consumeTombstone` to match
on NetworkID + Hostname when both sides know it. v0.5.0 tombstones
without a hostname still match — the new rule is "when both sides
know the hostname they must agree." Verified live on gpu1 with a
two-container sequential restart: each container kept its own MAC,
no swap.

### Recovery failures are now visible to operators (C-4)

`/Plugin.Health` gained two counters: `recovered_ok` and
`recovery_failed`. `healthy` flips to `false` when at least one
plugin-restart recovery fails — previously the only signal was a
single warn-level log line that scrolled away. The failure mode
mattered: a recovery failure means the container kept running but
without a lease-renewal client, so its IP would silently disappear
at lease expiry.

### nil-pointer panic in udhcpc-handler on malformed IPv6 (N-1)

`cmd/udhcpc-handler/main.go` would log a `net.ParseCIDR` error and
then dereference the (nil) result on the next line, panicking. A
handler panic means the corresponding `bound`/`renew` event is never
delivered to the persistent client; the lease silently ages out.
Fixed with an early return on parse error and an empty-string guard.

### Goroutine and udhcpc child leaks on lifecycle edges (C-1, C-2, W-9)

Three buffer fixes that together close three goroutine/process leak
classes:

- `udhcpc.Start` now writes events to a buffered channel (cap 16)
  with non-blocking send, so a final event line emitted by udhcpc
  between SIGTERM and exit can no longer deadlock the scanner
  goroutine.
- `udhcpc.Finish`'s `cmd.Wait` channel is buffered (cap 1), so
  context-cancel doesn't leave the Wait goroutine blocked on a send.
- `dhcpManager.setupClient`'s errChan is buffered (cap 1), so a
  partial Start (v4 OK, v6 fails) doesn't leave the v4 goroutine
  blocked on the final Finish-error send.

### Defensive ID truncation (C-3)

A `shortID(id)` helper replaces ~15 sites that did `id[:12]` for
log fields. A malformed Docker response with an empty/short ID
would have crashed the plugin during recovery, taking down lease
renewal for every healthy endpoint too. Two interface-name
construction sites still slice (they rely on Docker's 64-char
EndpointID contract for IFNAMSIZ fitting).

### Tests

Two new tests pinning the C-5 fix:

- `TestTombstones_HostnameNarrowsMatch` — two tombstones, two
  hostnames; each consume returns only its own.
- `TestTombstones_EmptyHostnameMatchesAny` — v0.5.0 tombstone
  without hostname is still consumable by a v0.5.1 binary.

Phase D walkthrough re-run on gpu1: D2, restart-stability, C-5
sequential-restart, D6 distinct-leases, C-4 health counters, D9
plugin disable/enable recovery, D7 release-on-stop — all green.

## v0.5.0

This release focuses on lifecycle correctness — keeping the DHCP
identity (MAC, lease, hostname) of a container stable across the
events that previously broke it: container restart, plugin restart,
and the initial DISCOVER timing window.

### Restart stability via tombstones

Docker 26.x reacts to `docker restart` by destroying the endpoint
and creating a fresh one with a new EndpointID, so any state keyed
on the endpoint can't bridge the two halves. The new mechanism:

- `DeleteEndpoint` writes a tombstone `{NetworkID, MAC, IPv4,
  deletedAt}` to `<stateDir>/tombstones.json`.
- The next `CreateEndpoint` on the same NetworkID within
  `tombstoneTTL` (10s) inherits the MAC and passes the IPv4 to
  `udhcpc` as `-r ADDR` on the initial DISCOVER, iff exactly one
  fresh tombstone matches.
- Concurrent restarts of multiple containers on the same network
  within the TTL fall back to fresh MACs — the "exactly one" rule
  prevents accidentally swapping identities between containers.

The IP carried in the tombstone is the **most recent** lease the
persistent client saw, not the initial-DISCOVER one. `dhcpManager`
now updates `LastIP`/`LastIPv6` on every `bound` and `renew` event
(previously only logged), and `Leave` refreshes the endpoint
fingerprint from `manager.LastIP` after `Stop` drains the event
goroutine. With a server that honors option 50 (Requested IP),
this makes IPs stable across restart. Servers that don't honor it
(notably Fritz.Box without a UI-side reservation) still rotate IPs
from the pool, but the **MAC** stays stable — which is the
prerequisite for setting a static reservation that does pin the IP.

Why this matters: consumer DHCP servers like Fritz.Box key
reservations on MAC. A fresh MAC every restart pollutes the lease
table and fragments the address pool. With a stable MAC, one-time
UI-side reservation pins the IP for good.

### Plugin-restart lease recovery

`docker plugin disable && enable`, plugin upgrade, or a plugin
crash previously left containers running without a renewal client,
so when the lease expired the IP went away. The plugin now walks
Docker's networks at startup, finds existing endpoints on
plugin-served networks, and rebuilds an in-memory `dhcpManager`
for each — passing `udhcpc -r LAST_IP` so the upstream ACKs the
lease the container is already using.

### Container-restart path fix

For Docker versions that issue Leave→Join on the same EndpointID
(older flows), `Join` now detects the missing `joinHint` and
synthesises an equivalent `CreateEndpoint` to rebuild the link.
On Docker 26.x the daemon takes a different path
(Delete→Create with new ID), where the tombstone mechanism above
takes over.

### Hostname + DHCP option 61 client-id

The initial DISCOVER now carries the container's hostname (option
12) when libnetwork has bound the container to the endpoint by the
time we look (best-effort, 2s poll; the persistent renewal client
fills it in regardless). The persistent client always carries the
hostname.

A stable client-id (option 61) derived from the first 8 bytes of
the EndpointID is also sent. This lets ipvlan deployments — where
all children share the parent MAC — be distinguished on the
upstream DHCP server, and lets reservations key on client-id
instead of MAC where the operator prefers.

### `/Plugin.Health` endpoint

A non-libnetwork endpoint at `/Plugin.Health` returns
`{healthy, uptime_seconds, active_endpoints, pending_hints}`.
Same socket as the libnetwork RPC, JSON body — anything that can
talk to the plugin can poll it for liveness/state.

### Phase D verification on a real LAN

Walked through the Phase D checklist on a Docker 26.1 host with a
Fritz.Box DHCP server: container gets LAN IP, two containers get
distinct leases, lease released on stop, forced renewal succeeds
without container restart, daemon-restart-with-plugin-enabled
completes in ~3 seconds with no hang and the plugin functional
immediately.

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
- The persistent client tracks the renewed IP (`LastIP` is updated on
  every `bound`/`renew` event since v0.5.0), but **does not yet
  reconfigure the in-container address** if the upstream hands out a
  different IP at renewal. The lease must be sticky enough to survive
  a renewal cycle. The renewed IP is at least surfaced to the
  next restart's tombstone, so it isn't lost.
- IPv6 lease tracking lands in tombstones (so the data flows through
  CreateEndpoint/Leave/DeleteEndpoint), but the request hint isn't
  surfaced to `udhcpc6` — busybox has no `-r` equivalent for v6.
  IPv6 endpoints get whatever the DHCPv6 server assigns; with a
  stable MAC and a server that keys reservations on
  client-id/MAC, that's typically the same address. Switching to a
  DHCPv6 client that supports preferred-address requests is a
  future enhancement.
- Concurrent `docker restart` of multiple containers on the same
  DHCP network within ~10 seconds falls back to fresh MACs (the
  tombstone mechanism requires exactly one match to avoid swapping
  identities). Sequential restarts — the typical case — are
  stable.
