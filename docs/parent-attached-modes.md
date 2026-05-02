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
| `skip_routes`       | bridge    | no       | Don't copy static routes from the bridge into the container. No-op in macvlan mode. |

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
