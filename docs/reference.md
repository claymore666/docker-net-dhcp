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

**[Health counters](#pluginhealth)** — `/Plugin.Health` on the plugin socket. Four flip `healthy` to `false`: `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, `address_conflicts`.

---

## Install, upgrade, uninstall

> **⚠️ BREAKING CHANGE IN v1.5.0 — DO THIS FIRST ⚠️**
>
> ```bash
> sudo mkdir -p /var/lib/net-dhcp
> ```
>
> v1.5.0 is the first release that bind-mounts `STATE_DIR` from the
> host, and **Docker will not create a missing bind source.** Run that
> before `docker plugin install`, on every host, new install or
> upgrade. Skip it and the install fails at start-up and leaves the
> plugin **installed but disabled**, with a retry that reports only
> `plugin ... already exists`. Recovery is two lines, just below.

The plugin publishes to two registries; GHCR is primary:

- `ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z` (primary)
- `claymore666/net-dhcp:vX.Y.Z` (Docker Hub mirror)

Published builds: **`linux/amd64`** on the bare tag and
**`linux/arm64`** as `:vX.Y.Z-arm64` / `:latest-arm64` (v1.7.0 onward).
The architecture lives in the tag because a Docker plugin cannot be
installed from a multi-architecture manifest list at all, and
`docker plugin install` has no `--platform` to steer one — an index
fails with `did not find plugin config for specified reference` for
every architecture, including the one you are on. Substitute the
`-arm64` tag in **every** reference below, not only the install line:
`docker network create -d` records the tagged reference as the
network's driver, and a bare tag there names a plugin that was never
installed. The README covers the daemon-side reason in full.

**Install** (interactive privilege grant, or `--grant-all-permissions`
for unattended):

```bash
# One-time: the plugin persists lease state here, bind-mounted from
# the host so it survives upgrades (v1.5.0+). Docker will not create it
# for you, and `plugin install` fails with a mount error if it is
# missing — see "If the directory is missing" below.
sudo mkdir -p /var/lib/net-dhcp

# amd64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.7.1

# arm64 (v1.7.0 onward) — the architecture is in the tag, see below
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.7.1-arm64
```

**If the directory is missing**, the install pulls the plugin, then
fails to start it and exits non-zero with an OCI mount error naming
`/var/lib/net-dhcp`. The plugin is left **installed but disabled**, and
re-running the same install command answers `plugin ... already exists`
without re-attempting the mount, while `docker network create` against
it answers `plugin ... found but disabled`. Neither second error
mentions the cause. Recover by creating the directory and enabling the
plugin that is already there:

```bash
sudo mkdir -p /var/lib/net-dhcp
docker plugin enable ghcr.io/claymore666/docker-net-dhcp:v1.7.1
```

On arm64 that second line takes the `-arm64` tag, like every other
reference on this page — the bare one names a plugin the host never
installed.

Nothing is lost or corrupted by the failed install. (Behaviour verified
against Docker 26.1.5, #494.)

Privileges requested: `network: host`, host PID namespace, the Docker
socket mount, a bind mount of `STATE_DIR` (v1.5.0+, see below), a
**read-only** bind mount of `/var/run/docker` (v1.6.0+),
`CAP_NET_ADMIN` + `CAP_SYS_ADMIN` + `CAP_SYS_PTRACE` (v1.3.3+). All are inherent to what the plugin does: creating links in
arbitrary netns, driving DHCP on the host's L2 segments, querying the
daemon — and entering a container's netns via `/proc/<pid>/ns/net`,
which the kernel ptrace-gates when the container runs as a non-root
user (#317).

The `/var/run/docker` mount lets the plugin list the daemon's sandbox
netns entries, so "the container went away mid-attach" is reported as
that rather than as a generic failure (`sandbox_netns_visible`). It is
the parent of `/var/run/docker/netns` rather than that directory
itself, because the daemon does not create the latter until the first
container sandbox — mounting it directly made `plugin install` fail on
a host that had never run one (#588).

**Verify the signature (v1.1.0+).** The published image is cosign-signed
(keyless) and carries SLSA build provenance; release artifacts ship a
cosign-signed `checksums.txt` and an SBOM. Per-release, copy-pasteable
verification commands live in
[Verifying releases](verifying-releases.md), which every
[GitHub Release](https://github.com/claymore666/docker-net-dhcp/releases)
links to; the [home page](index.md#verifying-releases) has the short form.

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
# 3. Swap the plugin (STATE_DIR on the host is left alone, so the
#    tombstone and audit ledger carry across — v1.5.0+)
docker plugin disable ghcr.io/claymore666/docker-net-dhcp:vOLD
docker plugin rm ghcr.io/claymore666/docker-net-dhcp:vOLD
# Upgrading ONTO v1.5.0 or later from an earlier version: create the
# bind source first. v1.5.0 is the release whose manifest started
# mounting STATE_DIR from the host, and Docker will not create a missing
# bind source — the install below fails at start-up and leaves the
# plugin disabled. vOLD is already gone at this point, so the host has
# no working driver until you `docker plugin enable` the new one. See
# "If the directory is missing" above. Harmless to repeat later.
sudo mkdir -p /var/lib/net-dhcp
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
> **v1.5.0+ removes one of the two causes.** `STATE_DIR` is now bind-
> mounted from the host, so the tombstone that remembers an endpoint's
> MAC and IP survives `docker plugin rm`. Before v1.5.0 it lived inside
> the plugin rootfs and every upgrade destroyed it, which is a separate
> loss from the network-removal one described above. Removing the
> *network* still discards the tombstone — that is keyed by network ID
> by construction — so the address only survives an upgrade in which
> the network itself is left in place.
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
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.7.1 \
    --ipam-driver null \
    -o bridge=my-bridge \
    my-dhcp-net
```

### macvlan

No host changes — containers get per-container kernel-generated MACs
as macvlan children of a host NIC:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.7.1 \
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
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.7.1 \
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
| `client_id` | all | per-endpoint id | v0.9.0 | Override DHCP option 61 (Client Identifier) for every endpoint on this network; sent as RFC 2132 opaque bytes (type `0x00`). The default per-endpoint id is what makes per-container reservations work — a fixed `client_id` makes all containers look like one client to the server. Pair with `vendor_class` for class-based policy. **The derived default differs by mode** (see below). |
| `vendor_class` | all | `docker-net-dhcp` | v0.9.0 | Override DHCP option 60 (Vendor Class Identifier), for DHCP servers running class-based policy (different gateway/option sets per class). v4 only — the DHCPv6 client sends no vendor-class option. |
| `validate_dhcp` | macvlan, ipvlan | `false` | v0.9.0 | Pre-flight probe at `docker network create`: one-shot DHCP exchange on a temporary child of the parent, rejecting the network if no server answers within 8s. Catches isolated parents / blocked UDP 67-68 / broken VLAN tags at create time. Costs one transient lease per probe. Bridge mode rejects the option. **Since v1.6.0 the probe link is the same kind the network's endpoints will be** — a macvlan child for a macvlan network, an ipvlan L2 child for an ipvlan one (#486). It used to build a macvlan whatever the mode was, on the reasoning that reachability is mode-agnostic; reachability is, but the parent is not. One parent cannot carry both kinds, so a macvlan probe on an ipvlan network was refused outright whenever an ipvlan container was already running on that NIC — `validate_dhcp` failing for a reason that had nothing to do with DHCP, which is the opposite of what the flag is for. **What MAC you will see at the server:** on macvlan, a random locally-administered address. On ipvlan, the **parent's** address — an ipvlan child cannot have its own, by kernel design. The random address is still generated and still reaches `dhcpcd`, where it becomes the probe's DUID and IAID, so the probe's DHCP *identity* is its own either way; it is only the link-layer address in `chaddr` that is shared. Don't go looking for a random MAC in an ipvlan probe's lease log. |
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
| `STATE_DIR` | `/var/lib/net-dhcp` | Where per-network options, the tombstone file, and the `audit_log` ledger persist. **Bind-mounted from the host at this exact path since v1.5.0**, so its contents survive `docker plugin rm` — before that they lived in the plugin rootfs and every upgrade destroyed them. Two consequences: durability begins with the version that introduced the mount (an upgrade *onto* v1.5.0 still starts from nothing, because the old state was never on the host), and **repointing this setting opts out** — a path other than the mounted one is inside the rootfs again and is wiped by the next upgrade. |
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
- **Client identifier (option 61)** — type-byte `0x00` (RFC 2132 opaque),
  with a payload that depends on the mode:

  | mode | payload | survives `docker restart`? |
  |---|---|---|
  | `bridge`, `macvlan` | the endpoint MAC | **yes** |
  | `ipvlan` | eight bytes from the Docker endpoint ID | no |

  In `bridge` and `macvlan` the MAC is unique per endpoint and the plugin
  preserves it across a restart, so the server recognises the returning
  container and renews the same address. This is the same identity IPv6
  has always used (its DUID/IAID is MAC-derived), and it is what makes
  IPv4 restart-stable without depending on a `DHCPRELEASE` being sent on
  the way out — which is not always possible (`SIGKILL`, OOM, power loss).

  `ipvlan` is the exception: its L2 slaves all inherit the parent's MAC,
  so a MAC-derived id could not tell containers apart. Those keep the
  endpoint-derived id, which is unique but **not** stable across a
  restart — Docker mints a fresh endpoint ID each time (#219).

  No mode survives `docker rm` + `run`. A recreate builds a new sandbox
  with a new MAC and a new endpoint ID, so there is no identity to carry;
  that needs a per-container identity the driver API doesn't currently
  expose (#218).

  Override with `client_id`, though a fixed value makes every container
  look like one client.

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
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.7.1 \
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
cannot race it. Results land on `/Plugin.Health` as `recovered_ok`,
`recovery_failed`, `recovery_aborted_container_gone` and
`recovery_network_gone` — the last two covering containers that had
already exited when recovery reached them, and networks removed between
the listing and the read of their detail. Neither is a failure and
neither flips `healthy`.

One case cannot be finished there. On a daemon restart Docker respawns
the plugin while the daemon itself is still coming up, so recovery's
first API call can time out against a daemon that is not serving yet.
Waiting for it before the socket exists would stall plugin-enable
against the very daemon being waited on, so recovery is instead retried
once the socket is listening, and the wait is counted as
`recovery_deferred` rather than a failure (v1.4.0+, #383). Only a retry
that runs out of budget counts `recovery_failed`.

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

State survives enable/disable cycles, and — since v1.5.0, because
`STATE_DIR` is [bind-mounted from the host](#plugin-settings) — it
survives `docker plugin rm` and `docker plugin upgrade` too. Before
v1.5.0 it lived in the plugin rootfs and every upgrade reset it.

The fall-back path is still there and still matters, because state can
be absent for other reasons (a first install, a `STATE_DIR` repointed
off the mount, a file removed by hand): on a cache miss, existing
networks fall back to the Docker API on first read, which back-fills the
file — so by the second endpoint operation everything is served from
disk again.

---

## Observability

### `/Plugin.Health`

JSON liveness + counters on the plugin's UNIX socket. **`sudo` is
required** — `/run/docker/plugins` is `drwx------ root root`, and
without it `curl -s` swallows the permission error and prints nothing,
which looks exactly like a dead endpoint:

```bash
PLUGIN_ID=$(docker plugin inspect -f '{{.Id}}' ghcr.io/claymore666/docker-net-dhcp:v1.7.1)
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
| `healthy` | — | `false` when `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, or `address_conflicts` is non-zero — an operator should look. Those four, and only those, are the ones marked **yes** in this column. The plugin keeps serving fresh attaches either way. |
| `instance_id` | — | (v1.5.0+) Opaque identifier of the plugin **process** serving this response. Every counter below is in-memory and returns to zero when the process does, so two readings are comparable as a delta only when their `instance_id` matches. If it changed between two samples, the plugin restarted and any difference you computed is meaningless — including one that reads as zero. Prefer this over `uptime_seconds` for that check: a plugin that restarts early in a long sampling window and then runs longer than the first reading shows uptime going *up*, hiding the restart. |
| `uptime_seconds` | — | Seconds since the plugin process started. Useful as an age, but see `instance_id` before using it to decide whether a restart happened. |
| `active_endpoints` | — | DHCP managers currently registered (post-Join, pre-Leave). |
| `pending_hints` | — | Join hints awaiting consumption; steady-state ~0. |
| `recovered_ok` | — | Endpoints successfully rebuilt by plugin-restart recovery. |
| `recovery_failed` | yes | Post-restart rebuilds that failed **for a container that is still running** — it runs without lease renewal and loses its IP at expiry; restart it. Two things are deliberately *not* counted here, because neither leaves a running container without a renewal client: a daemon that is merely still starting (`recovery_deferred`, #383), a container that had already exited when recovery reached it (`recovery_aborted_container_gone`, #376), and a network removed out from under the recovery walk (`recovery_network_gone`, #648). |
| `recovery_deferred` | no | (v1.4.0+) Recovery met a daemon that was not serving yet and was retried once the plugin socket came up (#383). Expected on a daemon restart. Only worth attention paired with `recovery_failed`, which together mean the retry ran out too. |
| `recovery_aborted_container_gone` | no | (v1.4.0+) Recoveries abandoned because the container had already exited, or been removed, by the time post-restart recovery reached it. Not a fault: nothing is left running without a renewal client, so this never flips `healthy`. The recovery-side twin of `join_aborted_container_gone`, and normal after a daemon restart that outlived some containers (#376). |
| `recovery_network_gone` | no | (v1.8.0+) Networks skipped during post-restart recovery because they had been removed between the listing that found them and the read of their detail. Not a fault: a network that is gone leaves no running container without a renewal client, so this never flips `healthy`. Counted rather than passed over in silence — a host where this climbs steadily is churning networks under a restarting daemon, which is worth knowing even though no single occurrence is a problem. Until v1.8.0 it landed in `recovery_failed`, where an ordinary `docker network rm` racing a daemon restart reported the plugin's most serious fault (#648). |
| `join_start_failures` | yes | (v1.3.3+) Persistent-client start failures at attach time **for a container that is still running** — it got its initial lease but runs without renewal, and the lease is never released on disconnect (#317). The plugin log carries the cause; fix it and restart the container. A container that *exited* mid-attach is counted separately and is not a fault — see below (#373). |
| `join_aborted_container_gone` | no | (v1.4.0+) Attaches abandoned because the container exited before the persistent client was up. Not a fault: there is no running container missing a renewal client, so this never flips `healthy`. A sustained rise still says something real — containers dying seconds after start, e.g. a crash-loop (#373). Recognised three ways: the daemon answering "no such container", the container's netns having gone, or its sandbox key being unlinked. An attach that fails for any other reason is counted as a fault, not excused (#401). |
| `join_aborted_no_container` | no | (v1.6.0+) Attaches abandoned because no container ever claimed the endpoint on the network, and whose leased address was released rather than left to expire. Not a fault: nothing is running without a renewal client, because nothing is running — so this never flips `healthy`. Distinct from `join_aborted_container_gone`, which needs the daemon to say "no such container" or the sandbox netns to be visibly gone; this one covers the case where the endpoint is simply unclaimed after the attach budget, which previously fell through to `join_start_failures` and leaked the address (#566). |
| `join_attach_slow` | no | (v1.4.0+) Attaches that succeeded, but only after outlasting `AWAIT_TIMEOUT`. Not a fault — the container has its renewal client. It is reported because the wait has an external cause worth seeing: the attach asks the daemon about the container being attached, and the daemon does not answer while it is still inside that container's start. Before v1.4.0 those attaches were abandoned and counted as `join_start_failures`, leaving a running container with no renewal client (#406). A rising count means the daemon is holding containers longer, not that the plugin is degrading. |
| `join_aborted_endpoint_left` | no | (v1.4.0+) Attaches cancelled because `Leave` arrived while the attach was still running — the endpoint was being torn down. Not a fault: there is no running container missing a renewal client. Distinguished from `join_start_failures` by direct evidence rather than inference, since the plugin cancelled the attach itself and knows why (#406). |
| `tombstone_write_failures` | yes | Failed tombstone saves (disk full, EROFS) — the next restart of some container will pick a fresh MAC/IP instead of inheriting. |
| `tombstones_consumed` | no | (v1.5.0+) Recreated containers that got their previous MAC/IP back by replaying a fresh tombstone. Not a fault — this is the address-stability mechanism working. It is the counterpart to `recovered_ok`: after a restart an address is preserved either by recovery re-adopting a still-attached endpoint (`recovered_ok`) or by a tombstone being replayed (this). Reported so the two can be told apart, which is what makes "the address survived, but via neither path" observable rather than silent (#386). |
| `lease_changed` | no | Renewals that returned a different IP than last recorded (v4+v6 aggregate). Docker's `inspect` view does **not** update on lease change (libnetwork has no in-place endpoint-IP swap), so this is the stale-inspect-window signal — alert on it for long-running containers. |
| `address_conflicts` | **yes** | (v1.6.0+) Leases whose address was already held by another device on the segment (#524). After each v4 lease the plugin resolves the address on the parent link and compares the answering MAC with the endpoint's; a reply from a different MAC is a conflict. This is the only signal for the condition — the container starts, Docker reports an address, and every other counter stays at zero, because from the DHCP server's point of view the lease was issued normally. The usual cause is a **statically configured** host inside the DHCP pool range: it never asks the server for anything, so the server cannot know the address is taken. Fix it at the server (reserve or exclude the address), not at the plugin. **What it does not cover:** another container on the *same host* sharing the same parent NIC. macvlan isolates a parent from its own children, and that isolation is what lets the check tell a squatter apart from your own endpoint — which holds the address too. A vantage point that could see a sibling would also hear your own container answer, and no reply would be informative. Excluded by construction, not pending work (#528). **What it requires:** the parent interface must carry an address on the leased subnet — which is the normal case, since a macvlan/ipvlan parent is usually the host NIC and a bridge parent always has one. A host answers an ARP request only if it can route a reply back to the sender, so with no on-subnet address to send from, the probe cannot get an answer out of a device that has no default route, and reports `conflict_probe_failures` rather than a clean result. |
| `address_conflict_probes` | no | (v1.6.0+) Conflict probes that reached a verdict, clean or not. Read this **before** believing `address_conflicts` is 0: with no probes, the two readings are identical, and "the detector never ran" is what #524 looked like for months. A healthy segment is `address_conflict_probes` climbing with `address_conflicts` at 0. |
| `sandbox_netns_visible` | no | (v1.6.0+) How many sandbox netns entries the plugin can currently see, or `-1` if it cannot read the directory at all. Sampled per request, not accumulated. **Read it against `active_endpoints`, never alone.** `-1` means the bind mount is missing, so the `sandboxGone` check can only ever answer "no usable evidence" — safe but useless. A `0` *with endpoints attached* is the dangerous one: the directory is readable but mounted from the wrong place, and `sandboxGone` would conclude every container had vanished. A `0` with nothing attached is neither — there is genuinely nothing to see (#567). |
| `conflict_probe_failures` | no | (v1.6.0+) Conflict probes that could not reach a verdict — the parent link was unroutable to the address, the lease/MAC could not be parsed, or **the parent has no address on the leased subnet**, which leaves the probe unable to get an answer out of a device with no default route. The last case is the common one, and its fix is on your side: give the parent an address on the segment and conflict detection starts working. Until then the plugin reports "undetermined" here rather than counting a clean probe, because a detector that cannot see must not report all-clear. This is an unanswered question, not a known-bad address, so it does not affect `healthy`. Watch it anyway: a detector that has stopped running looks exactly like a clean segment, which is how #524 went unnoticed in the first place. |
| `conflict_probe_stale_routes` | no | (v1.6.0+) Leftover conflict-probe routes reclaimed from a probe that was cut short before it could clean up. The probe runs detached, so stopping the plugin inside its window (a daemon restart, `docker plugin disable`, an upgrade) leaves its temporary `/32` on the parent, and every later probe for that address is refused with `file exists` until something removes it — the address then silently stops being checked. The probe now replaces the leftover and carries on; a rising count means the plugin is being stopped inside probe windows, not that any address went unchecked (#572). |
| `leases_obtained` | no | `dhcpcd` bind events (`BOUND`/`REBOOT`, and the v6 equivalents): initial bind or re-bind after NAK/lease loss. v4+v6 aggregate. |
| `leases_renewed` | no | `dhcpcd` `RENEW`/`REBIND` events. v4+v6 aggregate. |
| `dhcp_timeouts` | no | DHCP-acquisition failures (v4+v6 aggregate). Bumped by an explicit `dhcpcd` lease-loss hook when there is one, and — since v1.3.5 (#353) — by a periodic outage watchdog that compares the lease lifetime the server granted against the time since the client was last served. It keeps climbing for the duration of a server outage. **Detection is not immediate:** a bound endpoint's outage surfaces only once its lease would have run out (plus one watchdog period), because until then the client holds a valid address and `dhcpcd` reports nothing. On a 24-hour lease that is up to ~24 hours; a client that never binds at all is counted within `OUTAGE_GRACE` (~25s by default). The watchdog period and settling time are [`OUTAGE_TICK` / `OUTAGE_GRACE`](#plugin-settings) — note that lowering them shortens the fixed part of the delay only, never the lease itself. |
| `lease_release_failures` | no | Teardown DHCPRELEASE didn't complete cleanly — the server may hold a phantom lease until natural expiry. A pattern points at upstream reachability problems mid-teardown. |
| `naks_received` | no | (v1.0.0+) The server NAKed a renewal/rebind (v4+v6 aggregate). `dhcpcd` recovers by re-acquiring, so each NAK is typically followed by `leases_obtained` — and, if the address moved, `lease_changed` — bumps. Climbing alongside `lease_changed` means containers are being re-addressed mid-life. |
| `displaced_stops` | no | (v1.3.5+) Attaches that found a manager already registered for the same endpoint and stopped it — a container restarting into a plugin that had already recovered it (#338). The displaced client is released cleanly and the new one takes over, so a few are normal after a plugin restart. Climbing steadily alongside `recovered_ok` means a container is in a restart loop. |
| `orphaned_leases_released` | no | (v1.3.6+) Leases reclaimed for a container that exited before its renewal client could attach (#370) — one count per address, so a dual-stack endpoint that orphaned both of its addresses counts twice (v1.7.0+, #608; before that only the IPv4 address was ever reclaimed). The address is acquired during endpoint setup and deliberately held for the handover; when the handover never happens, the plugin synthesises a release instead of letting the address sit until it expires. A steady trickle is normal wherever short-lived containers run. |
| `restart_link_up_waited` | no | (v1.5.0+) Child links that came up only after waiting out the departing link's hold on the address — i.e. how often a container restart met the #408 window and the fix carried it. Not a fault: this is the repair working, counted so the window is visible rather than inferred. A steady rise means your hosts restart containers fast enough to hit it routinely, which is expected for images that handle `SIGTERM` promptly. |
| `restart_link_up_timeouts` | no | (v1.5.0+) The same wait outlasting its budget: the restart fails and `docker restart` reports `address already in use`. A real failure, but deliberately not `healthy`-affecting — it surfaces directly to whoever ran the command, and `healthy` is for faults nothing else reports. Any non-zero value here is worth investigating; it means the departing link held the address longer than the budget allows (#422). |
| `orphaned_lease_release_failures` | no | (v1.3.6+) A reclaim above that could not be completed — the address stays held upstream until its own expiry, exactly as it did before the reclaim existed. Read as a rate against `orphaned_leases_released`: a few failures are transient upstream trouble, a ratio near 1 means the reclaim path itself is broken (no route to the segment, server refusing the synthesised client). Not `healthy`-affecting, which is worth knowing when reading it: this counter can climb on every container without turning anything red, and did (#402). |
| `parent_link_waits` | no | (v1.6.0+) Operations that had to queue for a shared parent interface before attaching their own link. A parent NIC can be a macvlan port or an ipvlan port but never both, so when networks of both kinds share one parent — or when a lease reclaim still has its temporary link attached — the plugin serialises them per parent rather than letting the kernel refuse one with `device or resource busy` (#486, #549). Queuing is the mechanism working; a steady rise just means that NIC is busy. |
| `parent_link_wait_timeouts` | no | (v1.6.0+) The same wait giving up after its budget, after which the operation asks the kernel anyway and may fail with `device or resource busy`. Deliberately bounded well below the reclaim's own budget so a wedged reclaim degrades to the pre-v1.6.0 behaviour instead of stalling a container start. Not `healthy`-affecting, but the actionable one of the pair: any non-zero value means something held a parent far longer than a DHCP round trip should take, and container starts on that NIC were refused as a result. |
| `ledger_write_failures` | no | Failed `audit_log` ledger appends — degrades forensics, not networking. Operators using `audit_log` alert on this. |
| `lease_changed_v6`, `leases_obtained_v6`, `leases_renewed_v6`, `dhcp_timeouts_v6`, `naks_received_v6` | no | (v1.2.0+) The IPv6-only share of the matching aggregate above (#212). Each counts only the v6 client's events; the v4 share is the aggregate minus its `*_v6`. On a dual-stack host this isolates the v6-specific NAK/timeout signal the aggregate hides. `lease_release_failures_v6` (v1.7.0+, #608) joins the split with the same rule; `ledger_write_failures` has no per-family split. |

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

**That file does not survive an upgrade.** It lives in the plugin
rootfs, which Docker destroys and recreates on `docker plugin rm` /
`install` — the supported upgrade path — so the previous version's
history is gone at exactly the point you are most likely to want it.

Since v1.5.0 every line also goes to the plugin's stdout, which dockerd
captures into the **daemon** log on the host filesystem. That copy
outlives the plugin:

```bash
# systemd hosts
sudo journalctl -u docker --since "2 hours ago" | grep net-dhcp
# or, where dockerd logs to a file
sudo grep net-dhcp /var/log/docker.log
```

Take the in-rootfs copy for a focused read of the running plugin, and
the daemon log when you need history across an upgrade or the plugin is
already gone.

### Lease audit ledger (`audit_log=true`)

`STATE_DIR/leases.jsonl`. Since v1.5.0 `STATE_DIR` is bind-mounted from
the host, so read it there. The old path under the plugin rootfs is not
where the ledger lands any more — the mount is applied in the plugin's
own mount namespace, so following that path from the host finds a
mount point at best and, more usually, nothing at all:

```bash
sudo cat /var/lib/net-dhcp/leases.jsonl | jq .
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
    driver: ghcr.io/claymore666/docker-net-dhcp:v1.7.1
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
| Bridge mode: containers time out getting a lease, but the host itself has one | STP is enabled on the bridge, so each new container `veth` waits out two forwarding delays (~30s) before it forwards | Check `ip -d link show <bridge> \| grep -o 'stp_state [0-9]*'`; see [Leave STP off](bridge-mode.md#leave-stp-off-unless-you-need-it) |
| Bridge mode: everything works until the host reboots, then no container can attach | The bridge was created with imperative `ip link` commands, which do not persist | Use one of the [persistent stanzas](bridge-mode.md#make-the-bridge-persistent) for your distro |
| Container has an IP but `docker inspect` shows a different one | Mid-life re-acquisition after NAK/lease change | Expected degraded mode; watch `lease_changed` on `/Plugin.Health`; restart the container to resync Docker's view |
| Container starts and gets an address, but traffic to it is intermittent or lands on the wrong machine | Another device on the segment already holds that address. Usually a **statically configured** host inside the DHCP pool range: it never asks the server for anything, so the server cannot know the address is taken and hands it out. Nothing else reports this — the container starts, Docker shows an address, the lease was issued normally | Read `address_conflicts` on `/Plugin.Health` (v1.6.0+), and read `address_conflict_probes` **first**: with no probes the two are indistinguishable and "the detector never ran" is what this looked like for months. If probes are 0 or `conflict_probe_failures` is climbing, the parent most likely has no address on the leased subnet, which leaves the probe unable to get an answer back — give the parent an on-subnet address and detection starts working. The conflict itself is fixed at the DHCP server (reserve or exclude the address), not at the plugin |
| `--mac-address` fails on an ipvlan network | ipvlan children share the parent MAC (kernel design) | Use `mode=macvlan`, or drop the custom MAC |
| Reservations don't stick on ipvlan | DHCP server keys on MAC only, ignores option 61 | Use `mode=macvlan`, or configure the server to honor client identifiers |
| Container can't reach the Docker host (or vice versa) | macvlan/ipvlan kernel rule: children can't talk to the parent NIC's host IP | Bridge mode, or a second NIC — not a plugin setting |
| `dhcp_timeouts` climbs on a healthy network, often just after containers start | `OUTAGE_GRACE` is set below the time a client needs to acquire its first lease, so ordinary start-up is being reported as an outage | Raise `OUTAGE_GRACE`, or unset both outage variables to return to the defaults. The plugin logs a warning at startup whenever either is overridden — check the log's first lines |
| `healthy: false` on `/Plugin.Health` | Recovery or tombstone-write failure | See the field table above; restart affected containers; for a tombstone-write failure check space and writability on the filesystem holding [`STATE_DIR`](#plugin-settings) — the host's `/var/lib/net-dhcp` since v1.5.0, the plugin rootfs before that |
| Container came back on a **different IP** after a plugin upgrade | Recreating the network minted a new child MAC; the server keys the old lease to the old MAC and declines the re-request | Expected — see the callout under [Upgrade](#install-upgrade-uninstall). Pin the endpoint MAC and reserve it server-side to make the address survive future upgrades |
| `/Plugin.Health` prints nothing and exits 0 | `curl` run without `sudo`; `/run/docker/plugins` is root-only and `-s` hides the error | Re-run with `sudo` — see [`/Plugin.Health`](#pluginhealth) |
| `leases_renewed` still 0 and the log looks empty | Probably nothing — clean renewals log at `Debug`, and T1 may not have arrived | [Verify renewal properly](#verifying-that-renewal-works): read T1 from the lease, then re-check the counter |
| Compose doesn't attach the container to the DHCP network, with no error | Base/override merge produced a hybrid network definition | [The base/override merge trap](#the-baseoverride-merge-trap) |
| `docker plugin disable` refuses | Networks still reference the plugin | `docker network rm` them first |
| Renewals failing after a server outage | — | Containers keep their address and `dhcpcd` keeps retrying; `dhcp_timeouts` climbs once the lease would have lapsed (see the counter's note on detection delay) and `leases_renewed` resumes after the server returns |

Operator-side release/publishing issues (registry auth, Hub tokens)
are covered in the maintainer-facing
[`release-runbook.md`](release-runbook.md#troubleshooting).
