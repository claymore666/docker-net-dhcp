# Macvlan attachment mode

`docker-net-dhcp` supports two attachment modes:

| mode      | how containers reach the LAN                                  | host changes required |
| --------- | ------------------------------------------------------------- | --------------------- |
| `bridge`  | a veth pair plugged into a Linux bridge you maintain          | yes — you bring the bridge |
| `macvlan` | a per-container macvlan child of one of the host's NICs       | **none** — the host NIC is left untouched |

The `macvlan` mode is the right choice when you want containers on your LAN
without owning a Linux bridge. It's also the only mode where a container
appears on the LAN with a fully kernel-isolated MAC; switches and the
upstream DHCP server treat it as a distinct device.

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

| option              | mode      | required | meaning                                                       |
| ------------------- | --------- | -------- | ------------------------------------------------------------- |
| `mode`              | both      | no       | `bridge` (default) or `macvlan`                               |
| `parent`            | macvlan   | yes      | Host NIC to use as the macvlan parent (e.g. `ens18`, `eno1`). Must exist and be `UP`. |
| `bridge`            | bridge    | yes      | Existing Linux bridge to plug veths into.                     |
| `gateway`           | both      | no       | Override the IPv4 gateway returned by DHCP (e.g. when egress should go through a VPN router instead of the LAN's default gateway). |
| `ipv6`              | both      | no       | Also run DHCPv6 in addition to DHCPv4.                        |
| `lease_timeout`     | both      | no       | Initial-lease timeout for the up-front DHCP exchange (default `10s`). |
| `ignore_conflicts`  | bridge    | no       | Skip the bridge-already-in-use check. No-op in macvlan mode.  |
| `skip_routes`       | bridge    | no       | Don't copy static routes from the bridge into the container. No-op in macvlan mode. |

## Constraints

- The parent NIC must support macvlan children. Physical Ethernet, VLAN
  sub-interfaces, and bonds work; bridges and macvlans do not (you can't
  put a macvlan on top of a bridge or another macvlan).
- The parent NIC must be administratively `UP` before you create the
  network — the plugin won't bring it up for you (host config is
  off-limits).
- Like any macvlan setup: a container on a macvlan child cannot reach the
  parent NIC's own host IP, and vice-versa. This is a kernel-level rule,
  not a plugin restriction. For host↔container traffic you'd need either
  bridge mode or a second NIC.
- The plugin requires `--ipam-driver=null` because the LAN's DHCP server
  is the address source of truth, not Docker's IPAM.
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

**"parent interface is unsuitable for macvlan"** — you passed a bridge or
macvlan as `parent`. Use a real NIC, a VLAN sub-interface, or a bond.

**"parent interface is down"** — `ip link set <parent> up` and try again.
The plugin won't toggle host link state.

**Container gets no IP** — check that the parent NIC is on the right L2
segment, that DHCP traffic isn't being filtered (some managed switches
have DHCP snooping or storm-control turned on), and that the upstream
DHCP server has a free lease in its pool.

**`docker plugin install` reports "invalid rootfs"** — your Docker daemon
is too old. Plugin requires Docker 23+.

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
docker plugin set ghcr.io/<your-namespace>/docker-net-dhcp:v0.3.0 STATE_DIR=/some/other/path
```
