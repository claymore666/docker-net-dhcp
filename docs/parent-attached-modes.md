# Parent-attached modes (macvlan and ipvlan)

`docker-net-dhcp` supports three attachment modes:

<!-- docs-drift-ok: bridge — the table below compares attachment *modes*; the `bridge` driver option is documented in reference.md -->


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

Create a network attached to one of the host's NICs (`eth0` below —
substitute yours; `ip -brief link` lists them):

```bash
docker network create \
    --driver=ghcr.io/claymore666/docker-net-dhcp:v1.5.0 \
    --ipam-driver=null \
    -o mode=macvlan \
    -o parent=eth0 \
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
3. A one-shot `dhcpcd` runs on the new link — DHCPDISCOVER → REQUEST →
   ACK from your LAN's DHCP server. The lease (IP, mask, gateway) is
   captured.
4. The plugin returns the link name to libnetwork via `Join`. Docker moves
   the link into the container's netns and renames it (typically `eth0`).
5. A persistent `dhcpcd` runs inside the container netns to renew the
   lease for the lifetime of the endpoint. It runs observe-only
   (`--noconfigure`); the plugin applies lease changes via netlink.
6. On `docker stop`, libnetwork calls `Leave` → the persistent `dhcpcd`
   gets `SIGTERM` → it sends `DHCPRELEASE` so the upstream server's
   lease table doesn't accumulate stale entries.
7. The macvlan link is reaped automatically when the container netns is
   destroyed.

The host's NIC config (IP, routes, netplan/`systemd-networkd`,
`/etc/network/interfaces`) is **never touched**.

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
- **The parent should carry an address on the leased subnet.** Normally
  it does — a macvlan/ipvlan parent is the host NIC, holding the host's
  own DHCP address — and nothing here requires you to add one. It
  matters because the plugin checks each new lease against the segment
  (v1.6.0+, #524) by resolving the address from the parent, and a host
  answers ARP only if it can route a reply back to the sender. On a
  deliberately address-less parent that check reports
  `conflict_probe_failures` and an explicit *undetermined* instead of a
  clean result — addressing still works exactly as before, you just lose
  the detection. See
  [`/Plugin.Health`](reference.md#pluginhealth).
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

If you also want to confirm the lease is not colliding with something
already on the segment, read `address_conflict_probes` before believing
`address_conflicts` is zero — with no probes the two readings are
identical, and "the detector never ran" is what the fault behind #524
looked like. Both are on
[`/Plugin.Health`](reference.md#pluginhealth).

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
DHCP server has a free lease in its pool. `-o validate_dhcp=true` catches
all three at network-create time instead of at the first `docker run`.

Everything not specific to these modes — general symptoms, the Compose
merge trap, health-endpoint problems — is in the
[driver reference](reference.md#troubleshooting).

## Where the rest of the documentation lives

This page covers **choosing and setting up** macvlan or ipvlan. The
behaviour of the plugin itself is identical in every mode and is
documented once, in the [driver reference](reference.md):

- [All driver options](reference.md#driver-options-network-level), including `parent`, `validate_dhcp`, and the rest
- [Requesting a specific address](reference.md#requesting-a-specific-address)
- [Restart stability](reference.md#restart-stability-mac-and-ip) — how MAC and IP survive `docker restart`
- [DHCP identity](reference.md#dhcp-identity) — what the plugin sends as hostname, vendor class, and client ID
- [DHCPv6](reference.md#dhcpv6-ipv6true)
- [Recovery after a plugin restart](reference.md#recovery-after-a-plugin-restart)
- [`/Plugin.Health` and the counters](reference.md#pluginhealth)
- [Plugin settings](reference.md#plugin-settings) — `LOG_LEVEL`, `AWAIT_TIMEOUT`, `STATE_DIR`, `OUTAGE_TICK`, `OUTAGE_GRACE`

For how it works under the hood, see [How it works](internals.md).
