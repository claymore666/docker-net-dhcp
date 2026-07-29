# docker-net-dhcp — driver reference

**This is the manual.** Every knob the plugin has, and every behaviour
you can observe, is documented here and only here — installation,
network creation in every mode, all options and settings, lease
behaviour, observability, Compose usage, and troubleshooting.

This file is versioned with the code: the copy in your installed
version's tag is the truth for that version. CI enforces that every
driver-option key the code parses, every health counter the plugin
emits, and every plugin setting it accepts appears in this document —
and that none of them is documented a second time somewhere else
(`scripts/check-option-docs.sh`, `scripts/check-docs-drift.sh`). Neither
staleness nor a divergent copy is possible without turning CI red.

The other pages are deliberately narrow:

| page | for | what's there |
| ---- | --- | ------------ |
| [`bridge-mode.md`](bridge-mode.md) | getting started | one-time host-bridge setup, then a worked example |
| [`parent-attached-modes.md`](parent-attached-modes.md) | getting started | choosing macvlan vs ipvlan, quick start, mode-specific constraints |
| [`internals.md`](internals.md) | contributors | how the plugin is built — mechanism, not policy |

---

## At a glance

Every setting in one place. Details follow in the sections linked from
each group.

**[Network options](#driver-options-network-level)** — `docker network create -o key=value`, or `driver_opts:` in Compose:

| option | modes | default |
| ------ | ----- | ------- |
| `mode` | all | `bridge` |
| `bridge` | bridge | *(required)* |
| `parent` | macvlan, ipvlan | *(required)* |
| `gateway` | all | from DHCP |
| `ipv6` | all | `false` |
| `lease_timeout` | all | `10s` |
| `ignore_conflicts` | bridge | `false` |
| `skip_routes` | all | `false` |
| `propagate_dns` | all | `false` |
| `propagate_mtu` | all | `false` |
| `client_id` | all | per-endpoint id |
| `vendor_class` | all | `docker-net-dhcp` |
| `validate_dhcp` | macvlan, ipvlan | `false` |
| `register_dns` | all | `false` |
| `audit_log` | all | `false` |

**[Per-endpoint options](#driver-options-per-endpoint)** — `docker network connect --driver-opt`, or `driver_opts:` under a service's network attachment:

| option | default |
| ------ | ------- |
| `ip` | from DHCP |
| `com.docker.network.endpoint.ifname` | engine-assigned |

**[Container-level flags](#driver-options-per-endpoint)** that change what the plugin sends: `--mac-address`, `--hostname`, `--ip6`.

**[Plugin settings](#plugin-settings)** — `docker plugin set <plugin> NAME=value`:

| name | default |
| ---- | ------- |
| `LOG_LEVEL` | `info` |
| `AWAIT_TIMEOUT` | `10s` |
| `STATE_DIR` | `/var/lib/net-dhcp` |
| `OUTAGE_TICK` | `30s` |
| `OUTAGE_GRACE` | `25s` |

**[Health counters](#pluginhealth)** — `/Plugin.Health` on the plugin socket. Three flip `healthy` to `false`: `recovery_failed`, `join_start_failures`, `tombstone_write_failures`.

---

## Install, upgrade, uninstall

The plugin publishes to two registries; GHCR is primary:

- `ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z` (primary)
- `claymore666/net-dhcp:vX.Y.Z` (Docker Hub mirror)

**Install** (interactive privilege grant, or `--grant-all-permissions`
for unattended):

```bash
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.3.4
```

Privileges requested: `network: host`, host PID namespace, the Docker
socket mount, `CAP_NET_ADMIN` + `CAP_SYS_ADMIN` + `CAP_SYS_PTRACE`
(v1.3.3+). All are inherent to what the plugin does: creating links in
arbitrary netns, driving DHCP on the host's L2 segments, querying the
daemon — and entering a container's netns via `/proc/<pid>/ns/net`,
which the kernel ptrace-gates when the container runs as a non-root
user (#317).

**Verify the signature (v1.1.0+).** The published image is cosign-signed
(keyless) and carries SLSA build provenance; release artifacts ship a
cosign-signed `checksums.txt` and an SBOM. Per-release, copy-pasteable
verification commands live under **Verifying the signed artifacts** on
each [GitHub Release](https://github.com/claymore666/docker-net-dhcp/releases);
the [home page](index.md#verifying-releases) has the short form.

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

> **Expect the container's IP to change on `macvlan` and `ipvlan`.**
> Recreating the network builds a **new** child interface with a fresh
> kernel-generated MAC. DHCP servers key leases and reservations on MAC,
> so the previous address does not follow — and re-requesting it with
> `--driver-opt ip=<old address>` is *declined* while the server still
> holds that address against the old MAC. The container comes back on a
> different address.
>
> This is not the same as a container restart, which *does* preserve the
> address — see [Restart stability](#restart-stability-mac-and-ip). That
> mechanism is keyed by network ID, so removing the network loses it by
> construction.
>
> To keep an address across upgrades, give the endpoint a fixed MAC
> (`--mac-address` / Compose `mac_address` — an explicit MAC takes
> priority over everything else) and reserve **that MAC** on the DHCP
> server. Note that `docker network connect` has no `--mac-address`
> flag, so the MAC has to come from the container definition: an
> already-running container needs recreating once, after which the
> address is stable across every future upgrade.

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
(see [`bridge-mode.md`](bridge-mode.md) for the bridge setup itself):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.3.4 \
    --ipam-driver null \
    -o bridge=my-bridge \
    my-dhcp-net
```

### macvlan

No host changes — containers get per-container kernel-generated MACs
as macvlan children of a host NIC:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.3.4 \
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
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.3.4 \
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
| `ipv6` | all | `false` | upstream; functional again in v1.0.0 | Also run stateful DHCPv6 (a second `dhcpcd` with `-6`) alongside DHCPv4 — see [DHCPv6](#dhcpv6-ipv6true) for semantics and DUID/IAID identity. The Docker-visible v6 address is renewed as of v1.2.0 (#152). |
| `lease_timeout` | all | `10s` | upstream | Budget for the up-front DHCP exchange at container creation. Since v1.3.4 it is a *retry* budget (#332): transient failures are retried inside it (500 ms plus jitter between attempts) until it expires, while permanent ones — a missing interface, a malformed option — still fail immediately. Raise on slow/relayed networks (`-o lease_timeout=60s`). |
| `ignore_conflicts` | bridge | `false` | upstream | Skip the bridge-already-in-use check against other Docker networks. No-op in macvlan/ipvlan. |
| `skip_routes` | all | `false` | upstream; all modes since v0.9.0 | Don't copy non-default static routes from the parent (bridge or NIC) into containers, **and** don't apply DHCP-supplied classless static routes (option 121, see below). v0.9.0 extended parent route-copying from bridge-only to all modes (#102); set `true` to restore the old macvlan/ipvlan no-copy behaviour. The default gateway is unaffected either way. |
| `propagate_dns` | all | `false` | v0.9.0 | Write the DHCP-supplied DNS server list (option 6 / v6 option 23) into the container's `/etc/resolv.conf` on every bind/renew. Overrides Docker's embedded resolver for this network; the `search` line uses option 119 with fallback to option 15. |
| `propagate_mtu` | all | `false` | v0.9.0 | Apply DHCP option 26 (Interface MTU) to the container link on bind/renew. For jumbo-frame (9000) and VPN-reduced (~1450) networks. |
| `client_id` | all | per-endpoint id | v0.9.0 | Override DHCP option 61 (Client Identifier) for every endpoint on this network; sent as RFC 2132 opaque bytes (type `0x00`). The default per-endpoint id is what makes per-container reservations work — a fixed `client_id` makes all containers look like one client to the server. Pair with `vendor_class` for class-based policy. |
| `vendor_class` | all | `docker-net-dhcp` | v0.9.0 | Override DHCP option 60 (Vendor Class Identifier), for DHCP servers running class-based policy (different gateway/option sets per class). v4 only — the DHCPv6 client sends no vendor-class option. |
| `validate_dhcp` | macvlan, ipvlan | `false` | v0.9.0 | Pre-flight probe at `docker network create`: one-shot DHCP exchange on the parent with a random locally-administered MAC, rejecting the network if no server answers within 8s. Catches isolated parents / blocked UDP 67-68 / broken VLAN tags at create time. Costs one transient lease per probe. Bridge mode rejects the option. |
| `register_dns` | all | `false` | v1.3.0 | Send the DHCP FQDN option (81 on v4 / 39 on v6, via `dhcpcd fqdn both`) built from the container's hostname, asking the DHCP server to register that name in DNS (forward A/AAAA + reverse PTR). Reuses the same hostname already sent as the option-12 hint. Best-effort and advisory — many consumer routers ignore option 81, so this *requests* registration, it does not guarantee resolution. Off by default: dynamic-DNS registration is a network-policy decision. See below. |
| `audit_log` | all | `false` | v1.0.0 | Append every lease-lifecycle event (`bound` / `renew` / `release` / `release_failed`) to `STATE_DIR/leases.jsonl` — one JSON object per line with timestamp, network, endpoint, container, hostname, IP, MAC. Rotated at 16 MB or 30 days (one rotated generation kept, ≤ ~32 MB total). Append failures bump `ledger_write_failures` on `/Plugin.Health`, never affecting lease handling. Off by default: per-event disk write, and container↔IP correlation on disk is privacy-relevant in some environments. |

### DHCP classless static routes (option 121)

When the DHCP server hands out classless static routes (option 121,
RFC 3442 — and the identically-formatted Microsoft option 249), the
plugin applies them inside the container alongside the routes copied
from the parent. Routes are captured from the initial v4 lease and
programmed at `Join`. A `0.0.0.0/0` entry in option 121 is treated as
the default route and **supersedes the option-3 router** per RFC 3442
(an explicit `gateway=` override still wins over both). `skip_routes=true`
opts out of option-121 routes as well as parent-copied ones. v4 only —
IPv6 routes come from Router Advertisements. (Legacy option 33 is not
honored; modern servers send option 121.)

### Dynamic-DNS registration (`register_dns`, option 81 / 39)

With `-o register_dns=true`, every endpoint on the network sends the DHCP
**FQDN option** — option 81 on v4 (RFC 4702) and option 39 on v6
(RFC 4704) — built from the container's hostname, asking the server to
publish that name in DNS. This pairs with the option-12 hostname hint the
plugin already sends: the hostname says *who we are*, the FQDN option asks
the server to *publish it*. One `dhcpcd fqdn both` directive covers both
families, requesting forward (A/AAAA) **and** reverse (PTR) updates; the
container runs no DNS updater of its own, so the server does all the work.

The payoff is on-mission: a container becomes resolvable **by name** on
the LAN, not just reachable by its DHCP-leased IP — with no per-container
plumbing. The name source is the same one used for the hostname hint and
tombstone matching (the container's hostname; the server supplies the
domain).

It is **best-effort and advisory**, like the preferred-address hint: many
consumer routers ignore option 81 entirely, and registration depends on
the server being configured for dynamic DNS. The plugin's contract is
"send the option when asked" — not "the name will resolve." Off by
default because DDNS registration is a deliberate network-policy choice.

## Driver options (per-endpoint)

Passed per container via `docker network connect --driver-opt`, or as
`driver_opts:` under a service's network attachment in Compose:

| option | description |
| ------ | ----------- |
| `ip` | Request a specific IPv4 address (bare IP, no CIDR — the netmask comes from DHCP). Equivalent to `docker run --ip`; setting both to different values is an error. The address is *requested* from the DHCP server (DHCPREQUEST for it); the server still has final say. |
| `com.docker.network.endpoint.ifname` | (v1.0.0+) Request a specific interface name inside the container (Compose `interface_name`, engine 28+; or this key under `driver_opts`, any engine). The plugin validates the name (≤15 bytes, kernel charset — invalid names fail the attach with a clear error) and returns it in its Join response. **Current engine limitation:** moby's remote-driver layer discards the returned name (`drivers/remote/driver.go` passes an empty `DstName`), so engines do not yet apply it for *plugin* drivers — built-in drivers only. The plugin side is ready; the rename activates as soon as the upstream pass-through ships. Until then interfaces stay `ethN` in attach order. |

A static IPv6 request (`--ip6` / Interface.AddressIPv6) is honored
(v1.2.0+): it is sent to the DHCPv6 client as the IA_NA preferred
address, mirroring `--ip` for v4. The same applies to the v6 address a
container held before a restart — it is inherited from the tombstone
and re-requested — so a dual-stack container keeps both addresses
across `docker restart`. As with v4, the server has final say. (There
is no `ip6` *driver-opt* channel — use `--ip6` / `AddressIPv6`.)

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
| `LOG_LEVEL` | `info` | logrus level (`trace`, `debug`, `info`, `warn`, `error`). `trace` includes per-event `dhcpcd` lines and full HTTP-RPC bodies. |
| `AWAIT_TIMEOUT` | `10s` | Cap on the polling helpers (sandbox readiness, link rename, netns appearance). Bump if a slow daemon-restore window starves endpoint setup. |
| `STATE_DIR` | `/var/lib/net-dhcp` | Where per-network options, the tombstone file, and the `audit_log` ledger persist (inside the plugin rootfs). |
| `OUTAGE_TICK` | `30s` | How often the DHCP-outage watchdog re-checks each client, and so the resolution of `dhcp_timeouts` — the counter climbs about once per tick while a server is unreachable. Lower it for a finer-grained signal at the cost of a little more wakeup churn. |
| `OUTAGE_GRACE` | `25s` | Settling time before the watchdog will call an outage, added **on top of** the lease lifetime, so detection lands at `lease + grace`. Also the window a never-yet-bound client gets before its first failure is reported. |

> **`OUTAGE_GRACE` must stay above your clients' normal acquisition
> time.** The grace is what stops one slow DHCP exchange from
> registering as an outage; set it below how long a healthy container
> takes to get its first lease and ordinary start-up will report
> failures. The defaults suit a normal LAN — these two exist mainly so
> the integration suite can detect an outage without waiting out the
> production cadence on top of its fixture's lease, and most
> deployments should leave them alone.

---

## Behaviour

What the plugin does with leases, identity, and state. All of it applies
to **every** attachment mode unless a paragraph says otherwise.

### Requesting a specific address

`--ipam-driver=null` means `docker run --ip=` is rejected by the daemon
before it ever reaches the plugin. Pin an address with the per-endpoint
driver option instead; the plugin passes it to `dhcpcd` as a `request`
directive (DHCP option 50) on the initial DISCOVER:

```bash
docker create --name app alpine sleep 600
docker network connect --driver-opt ip=192.168.0.55 lan-dhcp app
docker start app
```

```yaml
services:
  app:
    image: alpine
    networks:
      lan-dhcp:
        driver_opts:
          ip: 192.168.0.55
networks:
  lan-dhcp:
    external: true
```

Whether the request is honoured is the **server's** decision. Most
enterprise servers (ISC, dnsmasq, Windows DHCP) respect option 50; many
consumer routers, the Fritz.Box among them, ignore it and hand out the
next free pool address unless a UI-side reservation exists for that MAC.

For IPv6 use `--ip6` / `Interface.AddressIPv6` — there is no `ip6`
driver-opt. It became a real request in v1.2.0: the address is sent as
the IA_NA preferred address, the v6 counterpart of `--ip`.

### Restart stability (MAC and IP)

Across `docker restart`, the plugin keeps the container's **MAC** stable
so the DHCP server sees one device rather than a new one each time —
without this, MAC-keyed reservations break and the server's lease table
fills with stale pairs.

The mechanism is a short-lived **tombstone**, written at `DeleteEndpoint`
and consumed by the next `CreateEndpoint` on the same network within 60
seconds. It carries the previous MAC, the last leased v4 address, and
the last v6 address. The successor endpoint reuses the MAC and re-requests
both addresses as hints. The TTL covers `docker restart` (sub-second) and
`systemctl restart docker` (15–30s while the daemon re-attaches
everything).

- **MAC stability always works** — `docker inspect` and the LAN see the
  same MAC across restarts.
- **IP stability depends on the server** honouring option 50, exactly as
  for an explicit request above. Where it doesn't, configure a
  reservation against the now-stable MAC and every restart gets that
  address.

Two things it deliberately does not do. Concurrent restarts of several
containers on one network inside the 60-second window fall back to fresh
MACs rather than risk swapping identities between containers — tombstones
carry the container hostname so restarts in flight can be told apart when
the hostname is known, and only when neither side knows it does the
network-wide "exactly one match" rule apply. Sequential restarts, the
normal case, always satisfy it.

And the tombstone is keyed by **network ID**, so it survives a container
restart but not the removal of the network itself — which is why a plugin
upgrade changes the address (see the callout under
[Upgrade](#install-upgrade-uninstall)).

### DHCP identity

Every exchange the plugin runs carries the same three identity fields,
in every mode:

- **Hostname (option 12)** — the container's hostname (Compose
  `hostname:`, `docker run --hostname`). Servers that auto-update DNS
  publish the container under that name. Best-effort on the initial
  DISCOVER (the plugin waits up to 2s for libnetwork to bind the endpoint
  to a container ID); the renewal client always sends it.
- **Vendor class (option 60)** — the literal `docker-net-dhcp`, so a
  server can gate behaviour on "this is a plugin-managed container"
  without parsing hostname conventions. v4 only; override with
  `vendor_class`.
- **Client identifier (option 61)** — eight bytes derived from the Docker
  endpoint ID, type-byte `0x00` (RFC 2132 opaque). Stable across
  container *restart* because the endpoint ID is. This is what makes
  reservations work in ipvlan, where every child shares the parent's MAC.
  It does **not** survive `docker rm` + `run`, which mints a fresh
  endpoint ID — lease stability across recreate needs a per-container
  identity the driver API doesn't currently expose (#218, #219). Override
  with `client_id`, though a fixed value makes every container look like
  one client.

#### Options captured from the server

Everything the server returns is captured. Some is applied, the rest is
logged:

**Applied**, when the matching option is enabled — option 6 / v6 option 23
(DNS servers) and option 119 (search list, falling back to option 15) into
`/etc/resolv.conf` with `propagate_dns`; option 26 into the link MTU with
`propagate_mtu`; option 121 as routes (see
[classless static routes](#dhcp-classless-static-routes-option-121)).

**Logged** at info level on every bind and renew, and only when at least
one is present, so plain LANs get no extra noise — option 42 (NTP),
66 (TFTP server), 67 (boot file), 119 (when `propagate_dns` is off),
252 (WPAD), 100/101 (RFC 4833 timezone) and 2 (legacy time offset):

```text
level=info msg="DHCP options received" ntp=[192.168.0.123]
  tftp=tftp.example.test bootfile=pxelinux.0
  search=[corp.example internal.example]
  wpad=http://wpad.example/wpad.dat posix_tz=PST8PDT
  tzdb_tz=Europe/Berlin time_offset=3600 ...
```

These are not auto-applied because the consuming application owns those
config files, and writing into them would mean another setns into the
container's mount namespace on every renewal.

### DHCPv6 (`ipv6=true`)

Runs a second persistent client (`dhcpcd -6`) alongside the v4 one —
**stateful DHCPv6**, not SLAAC. Note that Docker's own `--ipv6` flag does
not work with the null IPAM driver and is not what you want:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.3.4 \
    --ipam-driver null \
    -o mode=macvlan -o parent=eth0 -o ipv6=true \
    lan-dhcp6
```

- The leased address lands on the interface as a `/128` next to the v4
  address; `docker inspect` reports it as `GlobalIPv6Address`.
- **The default route stays RA-delegated.** DHCPv6 carries no router
  option by design, and the container's kernel honours Router
  Advertisements itself. The `gateway` option is v4-only.
- **Identity is a stable DUID-LL** (type 3) derived from the interface
  MAC via pinned config — no timestamp, so the same MAC always yields the
  same DUID. **The IAID is pinned from the MAC too** (v1.2.0, #152): the
  earlier client randomised it per process, so the one-shot and persistent
  clients disagreed and the leased address was never renewed. Server-side
  v6 reservations therefore stick exactly as far as MAC stability reaches.
  *ipvlan caveat:* children share the parent MAC, hence one DUID and one
  IAID for all of them — per-container v6 reservations aren't practical
  there.
- **The Docker-visible v6 address is renewed** as of v1.2.0. Before that
  the two clients drew different IAIDs, so the interface address could
  outlive its server-side lease on networks with short v6 lease times.
- `propagate_dns` covers v6 as well, via option 23. The two families are
  last-writer-wins on `resolv.conf`.
- Prefix delegation (DHCPv6-PD) is out of scope (#214).

### Recovery after a plugin restart

`docker plugin disable && enable`, a plugin upgrade, or a plugin crash
used to leave running containers without a renewal client — the lease
would quietly expire and the container would lose its address.

The plugin now walks Docker's network list at startup, finds every
endpoint on a plugin-served network, and rebuilds a DHCP manager for
each. The first acquisition requests the address the container is already
using (option 50) so the server ACKs it rather than allocating a new one.

Recovery runs synchronously inside plugin startup, before the socket
accepts requests, so a fresh `CreateEndpoint` arriving during enable
cannot race it. Results land on `/Plugin.Health` as `recovered_ok` and
`recovery_failed`.

The same path covers `systemctl restart docker`. In practice the address
is preserved either by recovery (when the daemon's shutdown never called
`Leave`) or by the tombstone (when it did) — the outcome is the same
either way.

### State persistence

Per-network options are written to `STATE_DIR/<network_id>.json` at
`docker network create`, so the per-endpoint handlers never have to call
back into the Docker API to learn the mode or parent. That callback is
what deadlocked the upstream plugin during `dockerd` startup, when it was
asked to restore containers using its own networks.

State survives enable/disable cycles and is reset by `docker plugin rm`
or `docker plugin upgrade`. After an upgrade, existing networks fall back
to the Docker API on first read, which back-fills the file — so by the
second endpoint operation everything is served from disk again.

---

## Observability

### `/Plugin.Health`

JSON liveness + counters on the plugin's UNIX socket. **`sudo` is
required** — `/run/docker/plugins` is `drwx------ root root`, and
without it `curl -s` swallows the permission error and prints nothing,
which looks exactly like a dead endpoint:

```bash
PLUGIN_ID=$(docker plugin inspect -f '{{.Id}}' ghcr.io/claymore666/docker-net-dhcp:v1.3.4)
sudo curl -s --unix-socket /run/docker/plugins/$PLUGIN_ID/net-dhcp.sock \
    http://localhost/Plugin.Health | jq .
```

A plain `GET` is correct; no request body or method override is needed.
Anything that can reach the socket can poll this as a liveness check.

Every counter below is a **plugin-wide total**, not a per-endpoint one.
A rise tells you *some* client on this host saw the event, never which
one — so when a counter moves, the plugin log is what attributes it:
each bump is emitted alongside a line carrying that endpoint's
`endpoint=<short id>` field. Alerting on the counters is right;
diagnosing a specific container from them alone is not.

| field | healthy-affecting | meaning |
| ----- | ----------------- | ------- |
| `healthy` | — | `false` when `recovery_failed`, `join_start_failures`, or `tombstone_write_failures` is non-zero — an operator should look. The plugin keeps serving fresh attaches either way. |
| `uptime_seconds` | — | Seconds since the plugin process started. |
| `active_endpoints` | — | DHCP managers currently registered (post-Join, pre-Leave). |
| `pending_hints` | — | Join hints awaiting consumption; steady-state ~0. |
| `recovered_ok` | — | Endpoints successfully rebuilt by plugin-restart recovery. |
| `recovery_failed` | yes | Endpoints whose post-restart rebuild failed — those containers run without lease renewal and lose their IP at expiry; restart them. |
| `join_start_failures` | yes | (v1.3.3+) Persistent-client start failures at attach time — the container got its initial lease but runs without renewal, and the lease is never released on disconnect (#317). The plugin log carries the cause; fix it and restart the container. |
| `tombstone_write_failures` | yes | Failed tombstone saves (disk full, EROFS) — the next restart of some container will pick a fresh MAC/IP instead of inheriting. |
| `lease_changed` | no | Renewals that returned a different IP than last recorded (v4+v6 aggregate). Docker's `inspect` view does **not** update on lease change (libnetwork has no in-place endpoint-IP swap), so this is the stale-inspect-window signal — alert on it for long-running containers. |
| `leases_obtained` | no | `dhcpcd` bind events (`BOUND`/`REBOOT`, and the v6 equivalents): initial bind or re-bind after NAK/lease loss. v4+v6 aggregate. |
| `leases_renewed` | no | `dhcpcd` `RENEW`/`REBIND` events. v4+v6 aggregate. |
| `dhcp_timeouts` | no | DHCP-acquisition failures (v4+v6 aggregate). Bumped by an explicit `dhcpcd` lease-loss hook when there is one, and — since v1.3.5 (#353) — by a periodic outage watchdog that compares the lease lifetime the server granted against the time since the client was last served. It keeps climbing for the duration of a server outage. **Detection is not immediate:** a bound endpoint's outage surfaces only once its lease would have run out (plus one watchdog period), because until then the client holds a valid address and `dhcpcd` reports nothing. On a 24-hour lease that is up to ~24 hours; a client that never binds at all is counted within ~30s. |
| `lease_release_failures` | no | Teardown DHCPRELEASE didn't complete cleanly — the server may hold a phantom lease until natural expiry. A pattern points at upstream reachability problems mid-teardown. |
| `naks_received` | no | (v1.0.0+) The server NAKed a renewal/rebind (v4+v6 aggregate). `dhcpcd` recovers by re-acquiring, so each NAK is typically followed by `leases_obtained` — and, if the address moved, `lease_changed` — bumps. Climbing alongside `lease_changed` means containers are being re-addressed mid-life. |
| `displaced_stops` | no | (v1.3.5+) Attaches that found a manager already registered for the same endpoint and stopped it — a container restarting into a plugin that had already recovered it (#338). The displaced client is released cleanly and the new one takes over, so a few are normal after a plugin restart. Climbing steadily alongside `recovered_ok` means a container is in a restart loop. |
| `ledger_write_failures` | no | Failed `audit_log` ledger appends — degrades forensics, not networking. Operators using `audit_log` alert on this. |
| `lease_changed_v6`, `leases_obtained_v6`, `leases_renewed_v6`, `dhcp_timeouts_v6`, `naks_received_v6` | no | (v1.2.0+) The IPv6-only share of the matching aggregate above (#212). Each counts only the v6 client's events; the v4 share is the aggregate minus its `*_v6`. On a dual-stack host this isolates the v6-specific NAK/timeout signal the aggregate hides. `lease_release_failures` and `ledger_write_failures` have no per-family split. |

### Verifying that renewal works

The most common question after a deployment — *is this container's lease
actually being renewed?* — has a non-obvious answer, because **a clean
renewal logs nothing**. It is emitted at `Debug` and the plugin runs at
`info`, so a silent log is correct behaviour, not evidence of a problem.

`leases_renewed` on `/Plugin.Health` is the cheap proof. It should be
non-zero once the first renewal is due, with `naks_received` and
`dhcp_timeouts` still at zero.

*When* it is due comes from the lease, not from any plugin setting: DHCP
option 58 (T1), typically half the lease time. Reading it means entering
the client's private mount namespace, since the state directory is
deliberately isolated (see [`internals.md`](internals.md)):

```bash
pid=$(pgrep -f '^dhcpcd: eth0 \[ip4\]' | head -1)
sudo nsenter -t "$pid" -m -- od -An -tu1 /var/lib/dhcpcd/eth0.lease
```

Options are TLV after the `99 130 83 99` magic cookie — **51** is the
lease time, **58** T1, **59** T2, each four bytes big-endian seconds. A
value of `0 1 81 128` is `0x00015180` = 86400s = a 24-hour lease, so T1
lands 12 hours after the bind.

For a durable record rather than a counter, turn on
[`audit_log`](#driver-options-network-level) — it writes a `renew` line
per event.

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
    driver: ghcr.io/claymore666/docker-net-dhcp:v1.3.4
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

### The base/override merge trap

Compose merges the top-level `networks:` map **key by key**, not file by
file, and this bites a common deployment shape: built-in macvlan in dev,
an external pre-created DHCP network in prod.

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

# docker-compose.prod.yml (override)
networks:
  lan:
    external: true
    name: lan-shared
```

The merged result is a hybrid — `external: true` **and** `driver:
macvlan` **and** `ipam.config` — matching neither the pure-external
attach contract nor the internal-create one. Compose then **silently
skips** attaching the service to `lan`; the container comes up on
whatever other networks it lists, with no error.

Diagnose with `docker compose -f docker-compose.yml -f
docker-compose.prod.yml config` and read the merged `networks.lan` block.
`external: true` sitting next to `driver` or `ipam` means you've hit it.

The plugin cannot influence Compose's merge logic, so the fixes are
consumer-side:

- **Best** — don't define `lan` in the base file at all. Put the dev
  definition in `docker-compose.dev.yml` and the prod one in
  `docker-compose.prod.yml`, so each file *replaces* the key outright.
- **Acceptable** — keep the base, but null out every key it sets in the
  override (`driver: null`, `driver_opts: null`, `ipam: null`) alongside
  `external: true`. Brittle: each new base key needs a matching null.
- **Escape hatch** — `docker network connect lan-shared <container>`
  after `compose up`. One-shot; does not survive a recreate.

---

## Troubleshooting

| symptom | likely cause | fix |
| ------- | ------------ | --- |
| `docker run` hangs then fails with a lease timeout | No DHCP reply on the parent L2 (isolated NIC, firewall on UDP 67/68, wrong VLAN). Since v1.3.4 transient failures are retried within `lease_timeout`, so reaching the timeout points at a persistent problem, not a one-off blip | Verify with `-o validate_dhcp=true` at create time; check the parent's connectivity; raise `-o lease_timeout` for slow/relayed networks |
| `invalid rootfs in image configuration` at install | Old Docker engine | Upgrade Docker |
| Network create fails `Bridge already in use` | Another Docker network owns the bridge | Use a dedicated bridge, or `-o ignore_conflicts=true` if the detection is wrong |
| Container has an IP but `docker inspect` shows a different one | Mid-life re-acquisition after NAK/lease change | Expected degraded mode; watch `lease_changed` on `/Plugin.Health`; restart the container to resync Docker's view |
| `--mac-address` fails on an ipvlan network | ipvlan children share the parent MAC (kernel design) | Use `mode=macvlan`, or drop the custom MAC |
| Reservations don't stick on ipvlan | DHCP server keys on MAC only, ignores option 61 | Use `mode=macvlan`, or configure the server to honor client identifiers |
| Container can't reach the Docker host (or vice versa) | macvlan/ipvlan kernel rule: children can't talk to the parent NIC's host IP | Bridge mode, or a second NIC — not a plugin setting |
| `healthy: false` on `/Plugin.Health` | Recovery or tombstone-write failure | See the field table above; restart affected containers; check disk space under the plugin rootfs |
| Container came back on a **different IP** after a plugin upgrade | Recreating the network minted a new child MAC; the server keys the old lease to the old MAC and declines the re-request | Expected — see the callout under [Upgrade](#install-upgrade-uninstall). Pin the endpoint MAC and reserve it server-side to make the address survive future upgrades |
| `/Plugin.Health` prints nothing and exits 0 | `curl` run without `sudo`; `/run/docker/plugins` is root-only and `-s` hides the error | Re-run with `sudo` — see [`/Plugin.Health`](#pluginhealth) |
| `leases_renewed` still 0 and the log looks empty | Probably nothing — clean renewals log at `Debug`, and T1 may not have arrived | [Verify renewal properly](#verifying-that-renewal-works): read T1 from the lease, then re-check the counter |
| Compose doesn't attach the container to the DHCP network, with no error | Base/override merge produced a hybrid network definition | [The base/override merge trap](#the-baseoverride-merge-trap) |
| `docker plugin disable` refuses | Networks still reference the plugin | `docker network rm` them first |
| Renewals failing after a server outage | — | Containers keep their address and `dhcpcd` keeps retrying; `dhcp_timeouts` climbs once the lease would have lapsed (see the counter's note on detection delay) and `leases_renewed` resumes after the server returns |

Operator-side release/publishing issues (registry auth, Hub tokens)
are covered in the maintainer-facing
[`release-runbook.md`](release-runbook.md#troubleshooting).
