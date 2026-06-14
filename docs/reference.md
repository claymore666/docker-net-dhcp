# docker-net-dhcp — driver reference

The complete operator reference for the plugin: installation, network
creation in every mode, all driver options, plugin settings,
observability, Compose usage, and troubleshooting. This file is
versioned with the code — the copy in your installed version's tag is
the truth for that version. CI enforces that every driver-option key
the code parses appears in this document
(`scripts/check-option-docs.sh`), so the options table cannot go
stale silently.

Deeper-dive companions: [`parent-attached-modes.md`](parent-attached-modes.md)
(macvlan/ipvlan concepts, DHCP identity, recovery semantics) and the
top-level [`README.md`](../README.md) (bridge-mode walkthrough,
implementation notes).

---

## Install / upgrade / uninstall

The plugin publishes to two registries; GHCR is primary:

- `ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z` (primary)
- `claymore666/net-dhcp:vX.Y.Z` (Docker Hub mirror)

**Install** (interactive privilege grant, or `--grant-all-permissions`
for unattended):

```bash
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.1.1
```

Privileges requested: `network: host`, host PID namespace, the Docker
socket mount, `CAP_NET_ADMIN` + `CAP_SYS_ADMIN`. All four are inherent
to what the plugin does (creating links in arbitrary netns, driving
DHCP on the host's L2 segments, querying the daemon).

**Verify the signature (v1.1.0+).** The published image is cosign-signed
(keyless) and carries SLSA build provenance; release artifacts ship a
cosign-signed `checksums.txt` and an SBOM. Per-release, copy-pasteable
verification commands live under **Verifying the signed artifacts** on
each [GitHub Release](https://github.com/claymore666/docker-net-dhcp/releases);
the [README](../README.md#verifying-releases) has the short form.

**Pin a version.** `:latest` exists and tracks the newest release, but
networks remember the exact driver string they were created with — a
network created against `:v1.1.1` needs that tag present to operate.
Pinning makes upgrades a deliberate step instead of a pull-side
surprise.

**Upgrade** — networks reference the plugin tag they were created
with, so the safe sequence for moving from `vOLD` to `vNEW` is:

```bash
# 1. Stop containers using plugin networks
# 2. Remove the networks (they're cheap to recreate; leases release)
docker network rm my-dhcp-net
# 3. Swap the plugin
docker plugin disable ghcr.io/claymore666/docker-net-dhcp:vOLD
docker plugin rm ghcr.io/claymore666/docker-net-dhcp:vOLD
docker plugin install ghcr.io/claymore666/docker-net-dhcp:vNEW
# 4. Recreate networks against vNEW, restart containers
```

(`docker plugin upgrade` exists but in-place upgrades while networks
exist risk a driver-reference mismatch; the remove/recreate path is
the supported one.)

**Uninstall:**

```bash
docker plugin disable ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z
docker plugin rm ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z
```

`disable` fails while networks still use the plugin — remove those
first (`docker network ls`, `docker network rm ...`).

---

## Creating networks

All modes share two invariants:

- `--ipam-driver null` is **required**. The LAN's DHCP server is the
  source of address truth; Docker's own IPAM would allocate from a
  subnet of its choosing and collide with the LAN.
- One DHCP-served network per container is the supported shape.

### bridge (default)

You bring an existing Linux bridge that is L2-connected to the LAN
(see the [README](../README.md#network-creation-bridge-mode) for the
bridge setup itself):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.1.1 \
    --ipam-driver null \
    -o bridge=my-bridge \
    my-dhcp-net
```

### macvlan

No host changes — containers get per-container kernel-generated MACs
as macvlan children of a host NIC:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.1.1 \
    --ipam-driver null \
    -o mode=macvlan -o parent=eth0 \
    lan-dhcp
```

### ipvlan (L2)

Like macvlan, but children share the parent NIC's MAC — for switches
or hypervisors that refuse multiple MACs per port (sticky-MAC port
security, hostile vSwitches, some Wi-Fi APs). The DHCP server must
key reservations on DHCP option 61 (client identifier), not MAC:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.1.1 \
    --ipam-driver null \
    -o mode=ipvlan -o parent=eth0 \
    lan-dhcp
```

Mode-specific constraints (MAC behaviour, parent-NIC rules, kernel
limitations) are catalogued in
[`parent-attached-modes.md`](parent-attached-modes.md#constraints).

---

## Driver options (network-level)

Passed as `-o key=value` on `docker network create`, or under
`driver_opts:` in Compose. Booleans take `'true'` / `'false'`
(quote them in YAML).

| option | modes | default | since | description |
| ------ | ----- | ------- | ----- | ----------- |
| `mode` | — | `bridge` | macvlan v0.2.0, ipvlan v0.4.0 | Attachment strategy: `bridge`, `macvlan`, or `ipvlan` (L2). |
| `bridge` | bridge | *(required)* | upstream | Existing Linux bridge to plug container veths into. |
| `parent` | macvlan, ipvlan | *(required)* | v0.2.0 | Host NIC to attach children to (e.g. `eth0`, `ens18`). Must exist and be administratively `UP`. |
| `gateway` | all | from DHCP | v0.3.0 | Override the IPv4 default gateway returned by the DHCP server — for split-horizon LANs where containers should egress via a different router (e.g. a VPN gateway). |
| `ipv6` | all | `false` | upstream; functional again in v1.0.0 | Also run stateful DHCPv6 (udhcpc6) alongside DHCPv4 — see the [DHCPv6 section](parent-attached-modes.md#dhcpv6-ipv6true) for semantics, DUID identity, and the current renewal boundary (#152). |
| `lease_timeout` | all | `10s` | upstream | Budget for the up-front DHCP exchange at container creation. Raise on slow/relayed networks (`-o lease_timeout=60s`). |
| `ignore_conflicts` | bridge | `false` | upstream | Skip the bridge-already-in-use check against other Docker networks. No-op in macvlan/ipvlan. |
| `skip_routes` | all | `false` | upstream; all modes since v0.9.0 | Don't copy non-default static routes from the parent (bridge or NIC) into containers. v0.9.0 extended route-copying from bridge-only to all modes (#102); set `true` to restore the old macvlan/ipvlan no-copy behaviour. |
| `propagate_dns` | all | `false` | v0.9.0 | Write the DHCP-supplied DNS server list (option 6 / v6 option 23) into the container's `/etc/resolv.conf` on every bind/renew. Overrides Docker's embedded resolver for this network; the `search` line uses option 119 with fallback to option 15. |
| `propagate_mtu` | all | `false` | v0.9.0 | Apply DHCP option 26 (Interface MTU) to the container link on bind/renew. For jumbo-frame (9000) and VPN-reduced (~1450) networks. |
| `client_id` | all | per-endpoint id | v0.9.0 | Override DHCP option 61 (Client Identifier) for every endpoint on this network; sent as RFC 2132 opaque bytes (type `0x00`). The default per-endpoint id is what makes per-container reservations work — a fixed `client_id` makes all containers look like one client to the server. Pair with `vendor_class` for class-based policy. |
| `vendor_class` | all | `docker-net-dhcp` | v0.9.0 | Override DHCP option 60 (Vendor Class Identifier), for DHCP servers running class-based policy (different gateway/option sets per class). v4 only — udhcpc6 doesn't take this option. |
| `validate_dhcp` | macvlan, ipvlan | `false` | v0.9.0 | Pre-flight probe at `docker network create`: one-shot DHCP exchange on the parent with a random locally-administered MAC, rejecting the network if no server answers within 5s. Catches isolated parents / blocked UDP 67-68 / broken VLAN tags at create time. Costs one transient lease per probe. Bridge mode rejects the option. |
| `audit_log` | all | `false` | v1.0.0 | Append every lease-lifecycle event (`bound` / `renew` / `release` / `release_failed`) to `STATE_DIR/leases.jsonl` — one JSON object per line with timestamp, network, endpoint, container, hostname, IP, MAC. Rotated at 16 MB or 30 days (one rotated generation kept, ≤ ~32 MB total). Append failures bump `ledger_write_failures` on `/Plugin.Health`, never affecting lease handling. Off by default: per-event disk write, and container↔IP correlation on disk is privacy-relevant in some environments. |

## Driver options (per-endpoint)

Passed per container via `docker network connect --driver-opt`, or as
`driver_opts:` under a service's network attachment in Compose:

| option | description |
| ------ | ----------- |
| `ip` | Request a specific IPv4 address (bare IP, no CIDR — the netmask comes from DHCP). Equivalent to `docker run --ip`; setting both to different values is an error. The address is *requested* from the DHCP server (DHCPREQUEST for it); the server still has final say. |
| `com.docker.network.endpoint.ifname` | (v1.0.0+) Request a specific interface name inside the container (Compose `interface_name`, engine 28+; or this key under `driver_opts`, any engine). The plugin validates the name (≤15 bytes, kernel charset — invalid names fail the attach with a clear error) and returns it in its Join response. **Current engine limitation:** moby's remote-driver layer discards the returned name (`drivers/remote/driver.go` passes an empty `DstName`), so engines do not yet apply it for *plugin* drivers — built-in drivers only. The plugin side is ready; the rename activates as soon as the upstream pass-through ships. Until then interfaces stay `ethN` in attach order. |

A static IPv6 request (`--ip6` / Interface.AddressIPv6) is currently
accepted but logged-and-skipped — busybox udhcpc6 has no
request-this-address flag. The v6 lease comes unhinted.

Container-level knobs that interact with the plugin:

- `--mac-address` / Compose `mac_address` — fix the MAC so the DHCP
  server's MAC-keyed reservations apply (macvlan and bridge; ipvlan
  rejects custom MACs by kernel design).
- `--hostname` / Compose `hostname` — sent as DHCP option 12, so
  DHCP-DNS integration registers the container under this name.

---

## Plugin settings

Set with `docker plugin set <plugin> NAME=value`; take effect after
`docker plugin disable && docker plugin enable`:

| name | default | meaning |
| ---- | ------- | ------- |
| `LOG_LEVEL` | `info` | logrus level (`trace`, `debug`, `info`, `warn`, `error`). `trace` includes per-event udhcpc lines and full HTTP-RPC bodies. |
| `AWAIT_TIMEOUT` | `10s` | Cap on the polling helpers (sandbox readiness, link rename, netns appearance). Bump if a slow daemon-restore window starves endpoint setup. |
| `STATE_DIR` | `/var/lib/net-dhcp` | Where per-network options, the tombstone file, and the `audit_log` ledger persist (inside the plugin rootfs). |

---

## Observability

### `/Plugin.Health`

JSON liveness + counters on the plugin's UNIX socket:

```bash
PLUGIN_ID=$(docker plugin inspect -f '{{.Id}}' ghcr.io/claymore666/docker-net-dhcp:v1.1.1)
curl -s --unix-socket /run/docker/plugins/$PLUGIN_ID/net-dhcp.sock \
    http://localhost/Plugin.Health | jq .
```

| field | healthy-affecting | meaning |
| ----- | ----------------- | ------- |
| `healthy` | — | `false` when `recovery_failed` or `tombstone_write_failures` is non-zero — an operator should look. The plugin keeps serving fresh attaches either way. |
| `uptime_seconds` | — | Seconds since the plugin process started. |
| `active_endpoints` | — | DHCP managers currently registered (post-Join, pre-Leave). |
| `pending_hints` | — | Join hints awaiting consumption; steady-state ~0. |
| `recovered_ok` | — | Endpoints successfully rebuilt by plugin-restart recovery. |
| `recovery_failed` | yes | Endpoints whose post-restart rebuild failed — those containers run without lease renewal and lose their IP at expiry; restart them. |
| `tombstone_write_failures` | yes | Failed tombstone saves (disk full, EROFS) — the next restart of some container will pick a fresh MAC/IP instead of inheriting. |
| `lease_changed` | no | Renewals that returned a different IP than last recorded. Docker's `inspect` view does **not** update on lease change (libnetwork has no in-place endpoint-IP swap), so this is the stale-inspect-window signal — alert on it for long-running containers. |
| `leases_obtained` | no | udhcpc `bound` events (initial bind or re-bind after NAK/lease loss). |
| `leases_renewed` | no | udhcpc `renew` events. |
| `dhcp_timeouts` | no | udhcpc `leasefail` events (no OFFER / no ACK within budget). |
| `lease_release_failures` | no | Teardown DHCPRELEASE didn't complete cleanly — the server may hold a phantom lease until natural expiry. A pattern points at upstream reachability problems mid-teardown. |
| `naks_received` | no | (v1.0.0+) The server NAKed a renewal/rebind. udhcpc recovers by re-acquiring, so each NAK is typically followed by `leases_obtained` — and, if the address moved, `lease_changed` — bumps. Climbing alongside `lease_changed` means containers are being re-addressed mid-life. |
| `ledger_write_failures` | no | Failed `audit_log` ledger appends — degrades forensics, not networking. Operators using `audit_log` alert on this. |

### Plugin log

```bash
sudo cat /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log
```

Raise verbosity with `docker plugin set <plugin> LOG_LEVEL=trace`
(plus a disable/enable cycle).

### Lease audit ledger (`audit_log=true`)

`STATE_DIR/leases.jsonl` inside the plugin rootfs:

```bash
sudo cat /var/lib/docker/plugins/*/rootfs/var/lib/net-dhcp/leases.jsonl | jq .
```

One JSON object per line; kinds `bound`, `renew`, `release`,
`release_failed`. `release_failed` means the DHCPRELEASE may not have
reached the server — the ledger never claims a release that might not
have happened.

---

## Compose usage

Recommended shape — network created once out-of-band, referenced as
external (shareable across projects, survives `compose down`):

```yaml
services:
  app:
    image: nginx
    hostname: my-server          # → DHCP option 12 → DHCP-DNS name
    mac_address: 02:42:ac:00:00:01  # match a server-side reservation
    networks:
      - lan
networks:
  lan:
    external: true
    name: lan-dhcp
```

Compose-managed alternative (network lifecycle tied to the project):

```yaml
networks:
  lan:
    driver: ghcr.io/claymore666/docker-net-dhcp:v1.1.1
    driver_opts:
      mode: macvlan
      parent: eth0
      propagate_dns: 'true'
    ipam:
      driver: 'null'
```

Multi-network containers work (one plugin network per container is
the *supported* shape; multiple attach, but interface naming order is
engine-determined until moby's remote-driver `interface_name`
pass-through ships — see the `com.docker.network.endpoint.ifname` row
above and issue #125).

---

## Troubleshooting

| symptom | likely cause | fix |
| ------- | ------------ | --- |
| `docker run` hangs then fails with a lease timeout | No DHCP reply on the parent L2 (isolated NIC, firewall on UDP 67/68, wrong VLAN) | Verify with `-o validate_dhcp=true` at create time; check the parent's connectivity; raise `-o lease_timeout` for slow/relayed networks |
| `invalid rootfs in image configuration` at install | Old Docker engine | Upgrade Docker |
| Network create fails `Bridge already in use` | Another Docker network owns the bridge | Use a dedicated bridge, or `-o ignore_conflicts=true` if the detection is wrong |
| Container has an IP but `docker inspect` shows a different one | Mid-life re-acquisition after NAK/lease change | Expected degraded mode; watch `lease_changed` on `/Plugin.Health`; restart the container to resync Docker's view |
| `--mac-address` fails on an ipvlan network | ipvlan children share the parent MAC (kernel design) | Use `mode=macvlan`, or drop the custom MAC |
| Reservations don't stick on ipvlan | DHCP server keys on MAC only, ignores option 61 | Use `mode=macvlan`, or configure the server to honor client identifiers |
| Container can't reach the Docker host (or vice versa) | macvlan/ipvlan kernel rule: children can't talk to the parent NIC's host IP | Bridge mode, or a second NIC — not a plugin setting |
| `healthy: false` on `/Plugin.Health` | Recovery or tombstone-write failure | See the field table above; restart affected containers; check disk space under the plugin rootfs |
| `docker plugin disable` refuses | Networks still reference the plugin | `docker network rm` them first |
| Renewals failing after a server outage | — | Containers keep their address and udhcpc keeps retrying; `dhcp_timeouts` climbs while the server is gone and `leases_renewed` resumes after it returns |

Operator-side release/publishing issues (registry auth, Hub tokens)
are covered in the maintainer-facing
[`release-runbook.md`](release-runbook.md#troubleshooting).
