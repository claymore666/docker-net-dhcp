# Parent-attached modes (macvlan and ipvlan)

`docker-net-dhcp` supports three attachment modes:

| mode      | how containers reach the LAN                                              | each child's MAC                  | host changes required |
| --------- | ------------------------------------------------------------------------- | --------------------------------- | --------------------- |
| `bridge`  | a veth pair plugged into a Linux bridge you maintain                      | random per veth                   | yes — you bring the bridge |
| `macvlan` | a per-container macvlan child of one of the host's NICs                   | **distinct** (kernel-generated)   | **none** — the host NIC is untouched |
| `ipvlan`  | a per-container ipvlan child (L2 mode) of one of the host's NICs          | **shared with parent**            | **none** — the host NIC is untouched |

### Picking between macvlan and ipvlan

Both modes attach directly to a host NIC and require no Linux bridge.
The difference is at L2: each container's MAC.

- **`macvlan`** is the default choice. Each container gets its own MAC,
  which is what most LANs and DHCP servers expect. The Fritz.Box (or
  any home/SOHO router) sees each container as a fully distinct
  device.
- **`ipvlan`** (L2 mode) is the right pick when the upstream switch or
  hypervisor refuses to bridge multiple MACs from one port. Common
  triggers: managed switches with sticky-MAC port-security enabled,
  Wi-Fi access points that refuse multi-MAC bridging, hypervisor
  vSwitches with strict port policies. Children share the parent's
  MAC; the LAN distinguishes containers by IP only.

If macvlan works on your LAN, use macvlan. ipvlan is the escape hatch
for hostile L2.

## Quick start

Create a network attached to one of the host's NICs:

```bash
docker network create \
    --driver=ghcr.io/<your-namespace>/docker-net-dhcp:latest \
    --ipam-driver=null \
    -o mode=macvlan \
    -o parent=ens18 \
    lan-dhcp
```

Then attach any container the usual way — no static IP, no labels, no
sidecar, no `cap_add`:

```yaml
services:
  app:
    image: nginx
    networks: [lan-dhcp]

networks:
  lan-dhcp:
    external: true
```

```bash
docker compose up -d
docker inspect app | jq '.[0].NetworkSettings.Networks'
# IPAddress is the lease your DHCP server handed out
```

## What happens under the hood

1. `docker run` triggers libnetwork's `CreateEndpoint` against the plugin.
2. The plugin creates a macvlan child on the parent NIC (submode = bridge,
   so children on the same parent can talk to each other), still in the
   host netns.
3. `udhcpc` runs once on the new link — DHCPDISCOVER → REQUEST → ACK from
   your LAN's DHCP server. The lease (IP, mask, gateway) is captured.
4. The plugin returns the link name to libnetwork via `Join`. Docker moves
   the link into the container's netns and renames it (typically `eth0`).
5. A persistent `udhcpc -R` runs inside the container netns to renew the
   lease for the lifetime of the endpoint.
6. On `docker stop`, libnetwork calls `Leave` → the persistent `udhcpc`
   gets `SIGTERM` → it sends `DHCPRELEASE` so the upstream server's
   lease table doesn't accumulate stale entries.
7. The macvlan link is reaped automatically when the container netns is
   destroyed.

The host's NIC config (IP, routes, netplan/`systemd-networkd`,
`/etc/network/interfaces`) is **never touched**.

## Driver options

| option              | mode             | required | meaning                                                       |
| ------------------- | ---------------- | -------- | ------------------------------------------------------------- |
| `mode`              | all              | no       | `bridge` (default), `macvlan`, or `ipvlan`                    |
| `parent`            | macvlan, ipvlan  | yes      | Host NIC to use as the parent (e.g. `ens18`, `eno1`). Must exist and be `UP`. |
| `bridge`            | bridge           | yes      | Existing Linux bridge to plug veths into.                     |
| `gateway`           | all              | no       | Override the IPv4 gateway returned by DHCP (e.g. when egress should go through a VPN router instead of the LAN's default gateway). |
| `ipv6`              | all              | no       | Also run DHCPv6 in addition to DHCPv4.                        |
| `lease_timeout`     | both      | no       | Initial-lease timeout for the up-front DHCP exchange (default `10s`). |
| `ignore_conflicts`  | bridge    | no       | Skip the bridge-already-in-use check. No-op in macvlan mode.  |
| `skip_routes`       | all       | no       | Don't copy non-default static routes from the parent (bridge / macvlan parent NIC / ipvlan parent NIC) into the container. v0.9.0 extended this from bridge-only to all modes for parity (#102) — set `true` to restore pre-v0.9.0 macvlan/ipvlan no-copy behaviour. |
| `propagate_dns`     | all       | no       | (v0.9.0+) Write DHCP option 6 / 23 (DNS server list) into the container's `/etc/resolv.conf` on bind/renew. Off by default; turning it on overrides Docker's embedded resolver for this network. The `search` line uses option 119 (Domain Search List) when supplied, falling back to option 15 (`Domain`) otherwise. |
| `propagate_mtu`     | all       | no       | (v0.9.0+) Apply DHCP option 26 (Interface MTU) to the container link on bind/renew. Off by default; useful for jumbo-frame networks (9000) and VPN-reduced ones (~1450). |
| `client_id`         | all       | no       | (v0.9.0+) Override DHCP option 61 (Client Identifier) for every endpoint on this network. Bytes go on the wire prefixed with type byte `0x00` (RFC 2132 opaque). Default empty keeps the per-endpoint stable id derived from the Docker endpoint ID, which is what makes per-container reservations work upstream. **Caveat:** a static `client_id` across containers means the DHCP server can't differentiate them. Typically only useful when paired with `vendor_class` to drive class-based policy. |
| `vendor_class`      | all       | no       | (v0.9.0+) Override DHCP option 60 (Vendor Class Identifier). Default `docker-net-dhcp`. Lets DHCP servers running class-based policy (Cisco / Aruba / etc.) issue different gateways or option sets to containers tagged with a known vendor string. v6 unaffected — udhcpc6 doesn't accept this option. |
| `validate_dhcp`     | macvlan, ipvlan | no | (v0.9.0+) Pre-flight DHCP probe at `docker network create` time. Creates a temporary macvlan child on the parent NIC with a random locally-administered MAC, runs one-shot udhcpc with a 5-second budget, and rejects the network with `no DHCP OFFER on <parent> within 5s` if no server answers. Catches misconfigurations (parent isolated, firewall blocking UDP/67-68, broken VLAN tag) at create time rather than the first `docker run`. Cost: one transient lease per probe in the upstream pool. Bridge mode rejects the opt with a clear error. |

## Constraints

- The parent NIC must support macvlan/ipvlan children. Physical
  Ethernet, VLAN sub-interfaces, and bonds work; bridges, macvlans,
  and ipvlans do not (you can't stack these on top of each other).
- The parent NIC must be administratively `UP` before you create the
  network — the plugin won't bring it up for you (host config is
  off-limits).
- Like any macvlan/ipvlan setup: a container on a child interface
  cannot reach the parent NIC's own host IP, and vice-versa. This is a
  kernel-level rule, not a plugin restriction. For host↔container
  traffic you'd need bridge mode or a second NIC.
- **ipvlan-specific:** custom MAC addresses are unsupported (children
  share the parent's MAC). Passing `--mac-address` on `docker run`
  with an ipvlan network will fail with `invalid MAC address`.
- **ipvlan-specific:** only L2 mode is supported. ipvlan L3 / L3S
  modes are not used because they'd break DHCP (DHCP requires L2
  broadcast).
- **ipvlan-specific:** if your DHCP server keys reservations solely
  on MAC and ignores DHCP option 61 (client identifier), ipvlan
  won't work as a stability mechanism — every ipvlan slave shares
  the parent's MAC, so the server has no way to tell them apart.
  Use `mode=macvlan` if your server is MAC-only. (See "DHCP
  identity" below for what the plugin sends.)
- **ipvlan-specific:** only one of macvlan or ipvlan can be active on
  a given parent NIC at a time. The kernel rejects mixing them with
  `EBUSY`. Use one mode per parent.
- The plugin requires `--ipam-driver=null` because the LAN's DHCP
  server is the address source of truth, not Docker's IPAM.
- One DHCP-served network per container. If a container also joins a
  bridge or other Docker network, that's its problem to coordinate.

## Verifying

After creating a container on the network:

```bash
# Container's view
docker exec <container> ip -4 addr show
docker exec <container> ip -4 route show

# Host's view of the lease
docker inspect <container> | jq '.[0].NetworkSettings.Networks'

# Upstream DHCP server's view (Fritz.Box, pfSense, etc.)
# — check the active leases page in your router's UI; the container's
# MAC and hostname should appear.
```

A container on a macvlan should be pingable from any other host on the LAN
on the IP its DHCP server handed it.

## Troubleshooting

**"parent interface is unsuitable for macvlan"** — you passed a bridge,
macvlan, or ipvlan as `parent`. Use a real NIC, a VLAN sub-interface,
or a bond.

**"ipvlan does not support a custom MAC address"** — `docker run --mac-address`
isn't compatible with `mode=ipvlan` because ipvlan children share the
parent's MAC. Drop the `--mac-address` flag, or switch the network to
`mode=macvlan` if you need distinct MACs.

**"parent interface is down"** — `ip link set <parent> up` and try again.
The plugin won't toggle host link state.

**Container gets no IP** — check that the parent NIC is on the right L2
segment, that DHCP traffic isn't being filtered (some managed switches
have DHCP snooping or storm-control turned on), and that the upstream
DHCP server has a free lease in its pool.

**`docker plugin install` reports "invalid rootfs"** — your Docker daemon
is too old. Plugin requires Docker 23+.

**Compose silently doesn't attach the container to the DHCP network.**
Compose merges base and override files at the top-level `networks:` map
*key by key*, not file by file. A common deployment shape with this
plugin is "use built-in macvlan in dev, switch to an external pre-created
DHCP network in prod" — written as:

```yaml
# docker-compose.yml (base)
networks:
  lan:
    driver: macvlan
    driver_opts:
      parent: ${LAN_INTERFACE:-eth0}
    ipam:
      config:
        - subnet: 192.168.0.0/24
          gateway: 192.168.0.1
          ip_range: 192.168.0.200/30

# docker-compose.prod.yml (override)
networks:
  lan:
    external: true
    name: lan-shared
```

Compose merges these into a hybrid: `external: true` AND `driver: macvlan`
AND `ipam.config: [...]`. At runtime Compose silently skips attaching
your service to `lan` because the merged result doesn't match either
contract (pure-external attach vs internal-create). The container comes
up attached only to the other networks listed in `services.<name>.networks`.

Diagnostic: run `docker compose -f docker-compose.yml -f docker-compose.prod.yml config`
and check the merged `networks.lan` block. If you see `external: true`
alongside `driver` or `ipam`, you've hit this trap.

Fixes (consumer-side; the plugin can't influence Compose's merge logic):

- **Best**: don't define `lan` in the base file at all. Move the dev
  definition into `docker-compose.dev.yml` and the prod override into
  `docker-compose.prod.yml`. Each file then *replaces* `lan` entirely.
- **Acceptable**: keep base, but in the override redefine `lan` with
  every key the base sets to `null` to wipe them: `driver: null`,
  `driver_opts: null`, `ipam: null`, `external: true`, `name: lan-shared`.
  Brittle — each new key in base needs a matching null in override.
- **Workaround for stuck operators**: `docker network connect lan-shared
  <container>` after `docker compose up`. One-shot fix; doesn't survive
  recreate.

## Static IP requests

Plugin networks use `--ipam-driver=null`, which means `docker run --ip=`
is rejected by the daemon before it ever reaches the plugin. To pin an
IP per container, use the per-endpoint driver option instead — the
plugin reads it as a hint and passes it to `udhcpc` as `-r ADDR` on the
initial DISCOVER:

```bash
docker create --name app alpine sleep 600
docker network connect --driver-opt ip=192.168.0.55 lan-dhcp app
docker start app
```

Compose:

```yaml
services:
  app:
    image: alpine
    command: sleep 600
    networks:
      lan-dhcp:
        driver_opts:
          ip: 192.168.0.55

networks:
  lan-dhcp:
    external: true
```

Whether the upstream DHCP server actually honors the request is up to
the server. Most enterprise DHCP servers (ISC, dnsmasq, Windows DHCP)
do; Fritz.Box does **not** unless you also configure a UI-side static
reservation for the container's MAC. With a stable MAC (which the
plugin gives you across restarts), that reservation persists.

`ip` must be a bare IPv4 address. IPv6 driver-opt is not yet wired
through to `udhcpc6`.

## Restart stability (MAC and IP)

The plugin keeps the container's MAC stable across `docker restart`
so the upstream DHCP server (Fritz.Box, pfSense, etc.) sees one
device, not a new one per restart. This matters because most
home-grade DHCP servers key reservations on MAC — without
stability the lease table fills up with stale (MAC, IP) pairs and
fragments the pool.

The mechanism is a short-lived tombstone written at
`DeleteEndpoint` and consumed at the next `CreateEndpoint` on the
same network within 60 seconds. It carries the previous MAC and
the most-recent leased IP. The next initial DISCOVER passes the
IP to `udhcpc` as `-r ADDR` (a hint). The TTL is generous enough
to cover both `docker restart <ctr>` (sub-second) and
`systemctl restart docker` (typically 15–30 s while the daemon
re-attaches all containers).

- **MAC stability**: works always. `docker inspect` and the LAN
  see the same MAC across restarts.
- **IP stability via the `-r` hint**: works on DHCP servers that
  honor option 50 (Requested IP). Fritz.Box specifically does
  **not** honor it without a UI-side reservation; it walks the
  pool and hands out the next free address even when the same MAC
  is presented. The fix is to configure a Fritz.Box static
  reservation on the now-stable MAC: Heimnetz → Netzwerk → pick
  the device → "Diesem Netzwerkgerät immer die gleiche IPv4-Adresse
  zuweisen". Once set, every subsequent restart gets that IP.

Concurrent restarts of multiple containers on the same network
within the 60-second window fall back to fresh MACs to avoid
swapping identities between containers. Tombstones also carry the
container's hostname so that two restarts in flight can be
told apart when the hostname is known on both sides; only when
neither side knows the hostname does the network-wide
"exactly one match" rule apply. Sequential restarts (the typical
case) always satisfy the rule.

## DHCP identity

Every DHCP exchange the plugin runs on a container's behalf carries
the same identity fields, regardless of attachment mode:

- **Hostname (option 12)** — taken from the container's
  `hostname` (Compose `hostname:`, `docker run --hostname`). DHCP
  servers that auto-update DNS (`dnsmasq` with
  `--dhcp-hostsfile`, the typical SOHO-router behaviour) will
  publish the container under that name. Best-effort on the
  initial DISCOVER (we wait up to 2 s for libnetwork to bind the
  endpoint to a container ID); the persistent renewal client
  always sends it.
- **Vendor class (option 60)** — the literal string
  `docker-net-dhcp`. Lets DHCP servers gate behaviour (e.g. a
  separate pool for plugin-managed containers) without parsing
  hostname conventions. v4 only — DHCPv6 has no equivalent.
  Override per-network with `-o vendor_class=<string>` (v0.9.0+)
  for sites running class-based DHCP policy (Cisco / Aruba style).
- **Client identifier (option 61)** — eight bytes derived from
  the Docker endpoint ID, prefixed by the type byte 0
  (per-client opaque, RFC 2132). Stable across container
  restarts on the same network because the EndpointID is —
  meaning DHCP servers that key reservations on option 61
  (rather than MAC) keep handing the same lease back. This is
  the mechanism that makes ipvlan stability work, since every
  ipvlan slave shares the parent's MAC; for macvlan and bridge
  it's a redundant safety net. Override per-network with
  `-o client_id=<string>` (v0.9.0+); rarely useful since a static
  ID across containers means the server can't differentiate them.

### Captured DHCP options

The plugin captures every option the upstream server returns and
surfaces them in two ways:

- **Auto-applied** when the network has the corresponding opt-in:
  - Option 6 / 23 (DNS server list) → `/etc/resolv.conf` when
    `propagate_dns=true`
  - Option 26 (Interface MTU) → container link MTU when
    `propagate_mtu=true`
  - Option 119 (DNS Search List) → resolv.conf `search` line when
    `propagate_dns=true`. Falls back to option 15 (`Domain`)
    when 119 is absent.
- **Surfaced via plugin log** at info level on every bound /
  renew, gated on at least one being non-empty so plain LANs see
  no extra noise:
  - Option 42 (NTP servers, env `ntpsrv`)
  - Option 66 (TFTP server name, env `tftp`)
  - Option 67 (Boot file name, env `bootfile`)
  - Option 119 (when `propagate_dns=false`)

```text
level=info msg="DHCP options received" ntp=[192.168.0.123]
  tftp=tftp.example.test bootfile=pxelinux.0
  search=[corp.example internal.example] ...
```

Workloads needing NTP / TFTP / boot-file can grep the plugin log
or wire a sidecar to it. The plugin doesn't auto-apply these
because (a) the consuming application owns the config file, not
the plugin, and (b) writing into `/etc/ntp.conf` etc. would mean
yet more setns into the container's mount namespace per renewal.

## Plugin-restart recovery

`docker plugin disable && enable`, plugin upgrade, and plugin
crashes used to leave running containers on the plugin's networks
without a renewal client — the lease would silently expire and
the container would lose its IP.

The plugin now walks Docker's network list at startup, finds the
endpoints attached to plugin-served networks, and rebuilds an
in-memory DHCP manager for each. The first `udhcpc` call uses
`-r LAST_IP` so the upstream server ACKs the lease the container
is already using rather than handing out a fresh address.

Recovery runs synchronously inside `NewPlugin` before the plugin
socket starts accepting requests, so a fresh `CreateEndpoint`
arriving during enable can't race the recovery path. Per-endpoint
results land on `/Plugin.Health` as `recovered_ok` / `recovery_failed`.

The same recovery path also covers `systemctl restart docker` —
the daemon brings every plugin back as part of its startup. In
practice MAC/IP is preserved either via this path (when the
daemon's graceful shutdown didn't get to call `Leave`) or via the
tombstone path (when it did), and both produce the same
user-visible outcome.

## Health endpoint

`/Plugin.Health` on the plugin's Unix socket returns:

```json
{
  "healthy": true,
  "uptime_seconds": 12345.6,
  "active_endpoints": 3,
  "pending_hints": 0,
  "recovered_ok": 0,
  "recovery_failed": 0,
  "tombstone_write_failures": 0,
  "lease_changed": 0,
  "leases_obtained": 17,
  "leases_renewed": 42,
  "dhcp_timeouts": 0,
  "lease_release_failures": 0
}
```

Field meanings:

- `healthy` — `false` when at least one of `recovery_failed` or
  `tombstone_write_failures` is non-zero. The plugin keeps serving
  fresh attaches in either case, but `false` means an operator
  should look: a recovery failure means a previously-attached
  container is now running without lease renewal, and a tombstone
  write failure means the next restart of some container will pick
  a fresh MAC/IP rather than inheriting the previous one.
- `active_endpoints` — count of DHCP managers currently registered
  (post-`Join`, pre-`Leave`).
- `pending_hints` — count of `joinHint` entries waiting to be
  consumed by a `Join` (steady-state should be ~0).
- `recovered_ok` / `recovery_failed` — counters bumped by the
  plugin-restart recovery path (running endpoints rebuilt vs.
  endpoints whose rebuild failed).
- `tombstone_write_failures` — counter bumped on any
  `addTombstone` save failure (disk full, EROFS, etc.).

DHCP-wire counters (v0.9.0+):

- `lease_changed` — bumps when a renewal returns a different IP
  than the manager last recorded. Docker's `NetworkSettings.IPAddress`
  view does NOT update on lease change (libnetwork has no in-place
  endpoint-IP swap RPC); this counter is the operator-facing signal
  that a stale-inspect window has opened. Worth alerting on for
  long-running containers.
- `leases_obtained` — udhcpc `bound` event count: initial bind or
  re-bind after a NAK / lease loss from the persistent client.
- `leases_renewed` — udhcpc `renew` event count.
- `dhcp_timeouts` — udhcpc `leasefail` event count (no OFFER /
  no ACK in budget).
- `lease_release_failures` — `dhcpManager.Stop`'s SIGTERM →
  DHCPRELEASE → exit didn't complete cleanly. The upstream may now
  hold a phantom lease against the container's MAC until natural
  expiry. A pattern of these typically points at upstream
  reachability problems mid-teardown.

Sample call from a host shell:

```bash
sudo curl --unix-socket \
  /run/docker/plugins/<plugin-id>/net-dhcp.sock \
  http://./Plugin.Health
```

Where `<plugin-id>` is the long ID from `docker plugin inspect`.
Use it as a liveness check from your monitoring stack; anything
that can talk to the plugin socket can poll it.

## State persistence

Per-network options are persisted inside the plugin to
`/var/lib/net-dhcp/<network_id>.json` on `docker network create`. This
exists so the per-endpoint handlers (`CreateEndpoint`, `Join`,
`EndpointOperInfo`, `DeleteEndpoint`) don't need to call back into the
docker API to learn the mode/parent/etc — which is what made the
upstream plugin deadlock during `dockerd` startup when it was being
asked to restore containers using its own networks.

State survives plugin enable/disable cycles. It is reset on
`docker plugin rm` or `docker plugin upgrade`. After an upgrade,
existing networks transparently fall back to the docker API on first
read, which then back-fills the persisted state — so by the second
endpoint operation everything is back to disk-served.

Override the location via the `STATE_DIR` env var on the plugin:

```bash
docker plugin set ghcr.io/<your-namespace>/docker-net-dhcp:v0.9.0 STATE_DIR=/some/other/path
```

## Plugin env vars

All settable via `docker plugin set <ref> KEY=VALUE`. None require a
plugin restart — `docker plugin disable && enable` picks them up.

| name            | default            | meaning |
| --------------- | ------------------ | ------- |
| `LOG_LEVEL`     | `info`             | logrus level (`trace`, `debug`, `info`, `warn`, `error`). `trace` includes per-event udhcpc lines and full HTTP-RPC bodies. |
| `AWAIT_TIMEOUT` | `10s`              | Cap on the polling helpers (waits for sandbox readiness, container hostname lookup, link rename, netns appearance). Bump if a slow Docker restore window starves the per-endpoint Start. |
| `STATE_DIR`     | `/var/lib/net-dhcp` | Where per-network options and the tombstone file live. Override if you want them on a tmpfs or a different volume. |
