# docker-net-dhcp — driver reference

**This is the manual.** Every knob the plugin has, and every behaviour
you can observe, is documented here and only here — installation,
network creation in every mode, all options and settings, lease
behaviour, observability, Compose usage, and troubleshooting.

This file is versioned with the code: the copy in your installed
version's tag is the truth for that version. CI enforces that every
driver-option key the code parses, every health counter the plugin
emits, and every setting the **shipped** plugin accepts appears in this
document — and that none of them is documented a second time somewhere
else (`scripts/check-option-docs.sh`, `scripts/check-docs-drift.sh`).
The claims made *about* those counters — which five flip `healthy` —
are enforced separately, wherever this page states them
(`scripts/check-health-contract.sh`).

**What that enforcement is, and where it stops.** All three gates run in
one direction, code to document: no option, counter or setting can exist
in the plugin without being named here, and none may be documented twice.
Nothing runs the other direction, and nothing reads a sentence *about* a
name. An option documented here that the code does not parse, a default
stated wrongly, or a behaviour described as it used to be, all go green —
`scripts/check-health-contract.sh` says as much of itself, calling its
tally a receipt rather than a proof of completeness. Those are caught by
the documentation review that is a step of every release
(`docs/release-runbook.md`), not by a gate.

The one deliberate gap: the coverage-instrumented build used by CI
declares two extra settings that the shipped plugin does not have. They
are exempt from this page by name — documenting a knob you cannot set
would be worse than omitting it — and are defined in the contributor
documentation instead, which the same gate requires.

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
| `ipv6` | all | `false` (**refused** — see below) |
| `lease_timeout` | all | `34s` |
| `conflict_check` | all | `wait` |
| `ignore_conflicts` | bridge | `false` |
| `skip_routes` | all | `false` |
| `propagate_dns` | all | `false` |
| `propagate_mtu` | all | `false` |
| `client_id` | all | per-endpoint id |
| `vendor_class` | all | `docker-net-dhcp` |
| `validate_dhcp` | macvlan, ipvlan | `false` |
| `dhcp_servers` | all | _(none)_ |
| `dhcp_deny_servers` | all | _(none)_ |
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
| `METRICS_ADDR` | *(empty)* |
| `DOCKER_HOST` | *(empty)* |

**[Health counters](#pluginhealth)** — `/Plugin.Health` on the plugin socket. Five flip `healthy` to `false`: `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, `tombstone_quarantines`, `address_conflicts`. The flag latches for the life of the plugin process — see [`healthy`](#pluginhealth).

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
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.9.0

# arm64 (v1.7.0 onward) — the architecture is in the tag, see below
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.9.0-arm64
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
docker plugin enable ghcr.io/claymore666/docker-net-dhcp:v1.9.0
```

On arm64 that second line takes the `-arm64` tag, like every other
reference on this page — the bare one names a plugin the host never
installed.

Nothing is lost or corrupted by the failed install. (Behaviour verified
against Docker 26.1.5, #494.)

Privileges requested: `network: host`, host PID namespace, the Docker
socket mount, a bind mount of `STATE_DIR` (v1.5.0+, see below), a
**read-only** bind mount of `/var/run/docker` (v1.6.0+),
`CAP_NET_ADMIN` + `CAP_SYS_ADMIN` + `CAP_SYS_PTRACE` (v1.3.3+) and
`CAP_NET_RAW` (2.0 beta+). All are inherent to what the plugin does:
creating links in arbitrary netns, driving DHCP on the host's L2
segments, and querying the daemon. **SECURITY.md carries one sentence
per grant, naming what in the tree consumes it**, and a CI gate fails
when that list and `config.json` disagree in either direction.

`CAP_SYS_PTRACE` and the host PID namespace are the pair worth reading
carefully. From the 2.0 beta the plugin ASKS FIRST for the sandbox key the
daemon publishes under `/var/run/docker/netns/`, and for an attach it is
refused: the read-only `/var/run/docker` mount is a bind taken when the
plugin starts, and it does not receive the per-sandbox namespace mounts the
daemon makes afterwards, so the key resolves to the ordinary file
underneath. The plugin checks that what it opened is a network namespace,
counts the refusal (`sandbox_key_entry_failures`), and enters through
`/proc/<pid>/ns/net` (`sandbox_pid_fallbacks`) as it always has. Recovery
after a plugin restart is the one case that goes the other way — the
sandbox is older than the plugin process, so it is inside the bind snapshot
and `sandbox_key_entries` rises instead. Neither grant could be dropped
in any case: `resolv.conf` propagation enters the container's **mount**
namespace by PID and there is no sandbox key for a mount namespace at all
— and it is those `/proc/<pid>/ns/*` opens the kernel ptrace-gates when the
container runs as a non-root user (#317).

**Upgrading onto the 2.0 beta re-prompts for privileges**, because
`CAP_NET_RAW` is a new line in the manifest. It is not new power: the
capability is in the OCI default set that Docker composes on top of the
requested one, so the process always had it. The DHCP exchange runs on
an interface with no address yet, which needs a raw socket, and there is
no configuration in which the plugin works without it. Expect the prompt
once, on the upgrade; if you get one you were not expecting, that is
worth investigating rather than approving.

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
# 2. Remove the networks (they're cheap to recreate; the addresses stay
#    leased until they expire — nothing sends a DHCPRELEASE, #800)
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

> **Upgrading onto the 2.0 beta prompts once for privileges, at step 3's
> `docker plugin install`.** The manifest requests `CAP_NET_RAW` in
> addition to the three capabilities v1.9.0 requested; nothing was
> dropped, and the effective set is unchanged (the capability was already
> in the OCI default set). An unattended run that does not pass
> `--grant-all-permissions` stops at the prompt with the host still
> without a driver, because `vOLD` was removed two lines earlier. The
> field-by-field delta is in `RELEASE_NOTES.md`; if you are prompted on an
> upgrade that is *not* this one, that is worth investigating rather than
> approving.

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
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.9.0 \
    --ipam-driver null \
    -o bridge=my-bridge \
    my-dhcp-net
```

### macvlan

No host changes — containers get per-container kernel-generated MACs
as macvlan children of a host NIC:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.9.0 \
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
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v1.9.0 \
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
| `ipv6` | all | `false` | upstream; **refused since the 2.0 beta** | **Setting this to `true` fails `docker network create`** with an error naming the beta and the milestone that restores DHCPv6. The beta's DHCP client implements DHCPv4 only. Refused at network creation rather than at the first container, because an endpoint that quietly comes up without the address its network asked for is the failure this exists to prevent — see [DHCPv6](#dhcpv6-ipv6true). The 1.x line is where DHCPv6 works today. |
| `lease_timeout` | all | `34s` | upstream; default **derived** since the 2.0 beta | Budget for the up-front DHCP exchange at container creation. It is a deadline over one acquisition, and what happens inside it is RFC 2131 §4.1's retransmission schedule, which the plugin sets explicitly. **4s, 8s, 16s, 32s, ceiling 64s are intervals, not elapsed times**: the first DISCOVER goes out immediately and arms a 4s timer, each retransmission arms the next interval as it goes out, and every interval carries ±1s of uniform jitter. The retransmissions therefore land at roughly **4s, 12s, 28s and 60s** after the first packet — the RFC's own worked example, "four times, for a total delay of 60 seconds" — and after the fourth the exchange is abandoned and restarted from DISCOVER. Permanent failures — a missing interface, a malformed option — still fail immediately rather than waiting it out.<br><br>**The default is 34s, and it is computed rather than written down.** In the default `conflict_check=wait` the acquisition is not finished at the DHCPACK: RFC 5227 §2.1's check runs before the address is used, and costs up to 7.0s (PROBE\\_WAIT 1s + two intervals of up to PROBE\\_MAX 2s + ANNOUNCE\\_WAIT 2s). One DISCOVER retransmission is 4s ±1s. One acquisition is therefore 5.0 + 7.0 = **12.0s**.<br><br>A budget of one acquisition is not enough, because the very thing the check exists to find makes a second one necessary. When the probe finds the address taken, the client sends a DHCPDECLINE, and RFC 2131 §3.1(5) requires it to wait **a minimum of ten seconds** before restarting; the address it is then offered has to clear §2.1 in its turn. So the default funds **one conflict and its recovery**: 12.0 + 10.0 + 12.0 = **34.0s**, read out of the DHCP client's own constants at startup so a change to either RFC schedule moves the default with it instead of leaving a stale literal behind. This is not theoretical — on the beta test lane a 12s default gave up 0.8s before the replacement lease was granted, on a run whose server log shows the whole exchange completing correctly. The old 10s literal funded the DISCOVER retransmission and nothing else.<br><br>**What the longer default costs.** On a segment with no DHCP server at all, `docker run` now fails after about 34s instead of about 12s. That is the price of not failing a container that hit a real address conflict, which is the case this option exists for; `-o lease_timeout=12s` buys the old behaviour back and gives up conflict recovery. In `conflict_check=off` nothing declines, so the extra budget is never spent.<br><br>**A `lease_timeout` shorter than the probe window is refused at `docker network create`** when `conflict_check=wait`, with the arithmetic in the message — under it a wait acquisition cannot succeed even against an instant DHCP server, so it is a configuration that can only time out. It is accepted in `async` and `off`, where the address is handed over without waiting for the check. Raise it on slow or relayed networks: `-o lease_timeout=60s` funds three retransmissions and sits on top of the fourth. Note the interaction with `dhcp_servers`, which subdivides this budget. |
| `conflict_check` | all | `wait` | **2.0 beta** | How RFC 5227 Address Conflict Detection is run for endpoints on this network, by the DHCP client, inside the container's own network namespace. **`wait`** (default) completes §2.1's probe before the address is configured: `docker run` blocks for the probe window (4.0–7.0s, 5.5s on average), a conflict is DECLINEd to the server and another address is requested, and the container never comes up on a contested address. **`async`** configures the address at the DHCPACK and probes behind it: `docker run` returns without the extra seconds, and a conflict found afterwards CHANGES the container's address while it is running — connections on the old one are already broken for both hosts. MEASURED end to end at about **11 seconds** from the conflict appearing to the container carrying the new address, of which ten are RFC 2131 §3.1(5)'s mandatory wait between the DHCPDECLINE and the next DISCOVER; for that whole window the container still holds the contested address, exactly as any other host in a conflict does. Detection and re-acquisition together are about a second. **`off`** sends no ARP at all, neither §2.1's probes nor §2.4's ongoing listener; nothing inside the client detects a conflict on this network, so `address_conflicts` and `acd_conflicts_detected` move only for a conflict reported to the client from outside it — which nothing in the plugin does today — and `acd_probes_sent` stays where it was. Any other value is refused at `docker network create` with the three names in the message. Networks created before this option existed read as `wait`. **`wait` applies to acquiring an address, not to keeping one:** a container joining a network it already holds a lease on runs the check in `async` even here, so a restart is not charged the probe window a second time for an address the previous run already cleared — the probes, the §2.4 listener and the DECLINE all still run. `-o validate_dhcp=true`'s preflight probe runs `off` for the same kind of reason: the address it is offered is released at once and never configured. Both `wait` and `async` keep watching after the address is in use (§2.4), which is the case the plugin's old probe could not cover at all. **This is not `ignore_conflicts`, and the two are never alternatives:** `conflict_check` is about another *device on the LAN* holding the address your DHCP server just leased, on any mode; `ignore_conflicts` is about another *Docker network on this host* already owning the bridge you named, in bridge mode, before any lease exists. |
| `ignore_conflicts` | bridge | `false` | upstream | Skip the bridge-already-in-use check against other Docker networks — a check about *this host's* Docker state, not about the segment. It has nothing to do with address conflicts on the LAN; that is `conflict_check`. No-op in macvlan/ipvlan. |
| `skip_routes` | all | `false` | upstream; all modes since v0.9.0 | Don't copy non-default static routes from the parent (bridge or NIC) into containers, **and** don't apply DHCP-supplied classless static routes (option 121, see below). v0.9.0 extended parent route-copying from bridge-only to all modes (#102); set `true` to restore the old macvlan/ipvlan no-copy behaviour. The default gateway is unaffected either way. |
| `propagate_dns` | all | `false` | v0.9.0 | Write the DHCP-supplied DNS server list (option 6 / v6 option 23) into the container's `/etc/resolv.conf` on every bind/renew. Overrides Docker's embedded resolver for this network; the `search` line uses option 119 with fallback to option 15 on v4, and DHCPv6 option 24 on v6 (v1.9.0+, #815). |
| `propagate_mtu` | all | `false` | v0.9.0 | Apply DHCP option 26 (Interface MTU) to the container link on bind/renew. For jumbo-frame (9000) and VPN-reduced (~1450) networks. Since v1.8.0 an MTU outside `[576, 65535]` is refused and the link keeps the MTU it had, counted by `mtu_refused` — nothing below this plugin holds the bottom of that range, so a server-supplied 68 used to be applied verbatim. |
| `client_id` | all | per-endpoint id | v0.9.0 | Override DHCP option 61 (Client Identifier) for every endpoint on this network; sent as RFC 2132 opaque bytes (type `0x00`). The default per-endpoint id is what makes per-container reservations work — a fixed `client_id` makes all containers look like one client to the server. Pair with `vendor_class` for class-based policy. **The derived default differs by mode** (see below). |
| `vendor_class` | all | `docker-net-dhcp` | v0.9.0 | Override DHCP option 60 (Vendor Class Identifier), for DHCP servers running class-based policy (different gateway/option sets per class). v4 only — the DHCPv6 client sends no vendor-class option. |
| `validate_dhcp` | macvlan, ipvlan | `false` | v0.9.0 | Pre-flight probe at `docker network create`: one-shot DHCP exchange on a temporary child of the parent, rejecting the network if no server answers within 8s. Catches isolated parents / blocked UDP 67-68 / broken VLAN tags at create time. Costs one transient lease per probe. Bridge mode rejects the option. **Since v1.6.0 the probe link is the same kind the network's endpoints will be** — a macvlan child for a macvlan network, an ipvlan L2 child for an ipvlan one (#486). It used to build a macvlan whatever the mode was, on the reasoning that reachability is mode-agnostic; reachability is, but the parent is not. One parent cannot carry both kinds, so a macvlan probe on an ipvlan network was refused outright whenever an ipvlan container was already running on that NIC — `validate_dhcp` failing for a reason that had nothing to do with DHCP, which is the opposite of what the flag is for. **What MAC you will see at the server:** on macvlan, a random locally-administered address. On ipvlan, the **parent's** address — an ipvlan child cannot have its own, by kernel design. The probe is otherwise identity-neutral: it sends no hostname and no client identifier, so on ipvlan there is nothing but the shared `chaddr` to tell it apart from the containers on that NIC. Don't go looking for a random MAC in an ipvlan probe's lease log. |
| `dhcp_servers` | all | _(none)_ | v1.8.0 | Ordered preference list of DHCPv4 servers, e.g. `1.1.1.1,2.2.2.2`. The initial acquisition tries each in turn, restricted to that one server, and takes the first lease offered. **The list is exhaustive**: if none of them answers, the endpoint fails rather than accepting whichever server happened to reply — naming your servers is what makes the list complete. The ladder **divides** the existing acquisition budget (`lease_timeout`) rather than extending it, so enabling this never makes `docker run` slower. Because it divides rather than extends, a long list cannot get one attempt each: an attempt costs entering the container's network namespace, opening a packet socket on the interface and a DHCP round trip, so a slice too small to hold one exchange is a guaranteed failure rather than a fast one. Once the list outgrows the budget the plugin keeps the top entries on their own attempts and asks **the tail as a single group** — with the default 10s budget that is the first two individually, then the rest together. Nothing is dropped, the total does not grow, and what degrades is only the strict ordering *within* that last group. Lists of three or fewer are unaffected. (#731) Once a lease is held it stays with the server that granted it, because renewal is unicast. **DHCPv4 only** — the beta leases no IPv6 addresses, and a v6 entry is rejected at `docker network create` rather than silently ignored. The list itself is validated the same way: an empty entry (a trailing or doubled comma), an entry that is not an IP address, and a repeated address each fail the create rather than being quietly dropped. **The 2.0 beta matches on the Server Identifier (option 54), not on the packet's source address**, so the list now works behind a DHCP relay — the 1.x limitation recorded under #111 is gone. Two consequences worth knowing: a message that carries no server identifier at all is **refused** while an allow list is set (an allow list a message can satisfy by omitting the field is not a restriction), and a server identifier is a value anyone on the link can put in a datagram, so this narrows which claimed identities the client acts on and authenticates nothing. |
| `dhcp_deny_servers` | all | _(none)_ | v1.8.0 | Unordered list of DHCPv4 servers this network must never take a lease from, e.g. `3.3.3.3` — a rogue appliance or a second router on the segment. This is a *permission*, not a preference: it composes with `dhcp_servers` rather than competing with it, and a server named in both is removed from the preference list. Denying every entry of `dhcp_servers` is refused at create time, since it would otherwise collapse to accepting any server at all. Same **DHCPv4-only** limit as `dhcp_servers`. **Deny wins** where the two lists disagree. A deny list *on its own* fails open on a message that carries no server identifier: nothing in such a message can show it came from a denied server. (The no-relay limit is gone in the 2.0 beta — see `dhcp_servers`.) (#669) |
| `register_dns` | all | `false` | v1.3.0 | Send the DHCP FQDN option (81) built from the container's hostname, asking the DHCP server to register that name in DNS (forward A/AAAA + reverse PTR). Reuses the same hostname already sent as the option-12 hint. Best-effort and advisory — many consumer routers ignore option 81, so this *requests* registration, it does not guarantee resolution. Off by default: dynamic-DNS registration is a network-policy decision. See below. |
| `audit_log` | all | `false` | v1.0.0 | Append every lease-lifecycle event (`bound` / `renew` / `stopped` / `stop_failed`) to `STATE_DIR/leases.jsonl` — one JSON object per line with timestamp, network, endpoint, container, hostname, IP, MAC. Rotated at 16 MB or 30 days (one rotated generation kept, ≤ ~32 MB total). Append failures bump `ledger_write_failures` on `/Plugin.Health`, never affecting lease handling. Off by default: per-event disk write, and container↔IP correlation on disk is privacy-relevant in some environments. |

### DHCP classless static routes (option 121)

When the DHCP server hands out classless static routes (option 121,
RFC 3442 — and the identically-formatted Microsoft option 249), the
plugin applies them inside the container alongside the routes copied
from the parent. Routes are captured from the initial v4 lease and
programmed at `Join`. A `0.0.0.0/0` entry in option 121 is treated as
the default route and **supersedes the option-3 router** per RFC 3442
(an explicit `gateway=` override still wins over both).

That is the *literal* case, and it is not the only one. Routes carrying no
default entry can still supersede it **by union**: a set that takes every
routable unicast destination between them — `0.0.0.0/1` plus `128.0.0.0/1`,
say, with this-network, loopback, link-local, multicast and reserved space
excluded — wins on longest-prefix match, while the gateway reported to Docker
and shown by `docker inspect` stays the option-3 router. Egress and the
displayed gateway then disagree, which is the shape to look for when traffic
does not go where `docker inspect` says it should. The routes are applied
either way — that is correct client behaviour and legitimate split-tunnel
setups rely on it — and the `[Join]` log names every destination and next hop
whether or not the union is complete. What marks the complete case is a
`[Join]` **warning** and the `dhcp_default_route_superseded` counter (see the
[health counters](#pluginhealth) table).

`skip_routes=true` opts out of option-121 routes as well as parent-copied
ones. v4 only — the beta leases no IPv6 addresses at all. Option 33, the
legacy static-route option, **is** honoured in the 2.0 beta: it is asked for
alongside option 121 and used when option 121 is absent or does not decode.
Option 121 supersedes it whenever both arrive.

### Dynamic-DNS registration (`register_dns`, option 81 / 39)

With `-o register_dns=true`, every endpoint on the network sends the DHCP
**FQDN option** — option 81 (RFC 4702) — built from the container's
hostname, asking the server to publish that name in DNS. This pairs with the
option-12 hostname hint the plugin already sends: the hostname says *who we
are*, the FQDN option asks the server to *publish it*. The flags byte asks the
server to perform **both** the forward (A) and the reverse (PTR) update; the
container runs no DNS updater of its own, so the server does all the work. The
v6 equivalent (option 39, RFC 4704) is not sent, because the beta runs no
DHCPv6 exchange to send it in.

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
| `com.docker.network.endpoint.ifname` | (v1.0.0+) Request a specific interface name inside the container (Compose `interface_name`, engine 28+; or this key under `driver_opts`, any engine). The plugin validates the name (≤15 bytes, kernel charset — invalid names fail the attach with a clear error) and returns it in its Join response. **Engine support:** moby's remote-driver layer discarded the returned name (`drivers/remote/driver.go` passed an empty `DstName`) until [moby/moby#52866](https://github.com/moby/moby/pull/52866), merged to moby master on 2026-08-26 and milestoned for engine **29.8.0**. No *released* engine carries it yet (latest 29.7.2 as of 2026-08-27), so on 29.7.x and older the name is still not applied for *plugin* drivers — built-in drivers only, and interfaces stay `ethN` in attach order. The plugin side is ready; the rename activates by itself on the first engine carrying the pass-through, with no change here. |

A static IPv6 request (`--ip6` / Interface.AddressIPv6) has no effect in the
2.0 beta. The beta runs no DHCPv6 exchange, so there is nothing to carry the
requested address; v6 networks are refused at `docker network create` in the
first place (see [`ipv6`](#dhcpv6-ipv6true)). 1.x honoured it as the IA_NA
preferred address (v1.2.0+); DHCPv6 returns in a later beta milestone.

Container-level knobs that interact with the plugin:

- `--mac-address` / Compose `mac_address` — fix the MAC so the DHCP
  server's MAC-keyed reservations apply (macvlan and bridge; ipvlan
  rejects custom MACs by kernel design).
- `--hostname` / Compose `hostname` — sent as DHCP option 12, so
  DHCP-DNS integration registers the container under this name.

---

## Plugin settings

Change one with `docker plugin disable`, then
`docker plugin set <plugin> NAME=value`, then `docker plugin enable`.
**The order is not a style choice:** the daemon refuses
`docker plugin set` on an enabled plugin with `cannot set on an active
plugin, disable plugin before setting`, so setting first fails on that
line and never reaches the restart.

`AWAIT_TIMEOUT`, the one duration setting, takes a Go duration string such
as `45s` or `2m`. A value that does not parse, or that is zero or negative,
**fails plugin startup** rather than falling back to the default — the same
rule `METRICS_ADDR` follows for a malformed address. An unset or empty
variable is not an error and takes the default below.

> **`OUTAGE_TICK` and `OUTAGE_GRACE` are gone in the 2.0 beta.** They tuned
> a watchdog that guessed when a lease had lapsed; the in-tree DHCP client
> holds the lease and reports a failed attempt itself, so there is no cadence
> left to tune. `config.json` no longer declares them, which means
> `docker plugin set OUTAGE_TICK=…` is refused by the daemon rather than
> quietly ignored.

| name | default | meaning |
| ---- | ------- | ------- |
| `LOG_LEVEL` | `info` | logrus level (`trace`, `debug`, `info`, `warn`, `error`). `trace` includes the DHCP client's per-event lines and full HTTP-RPC bodies. |
| `AWAIT_TIMEOUT` | `10s` | Cap on the polling helpers (sandbox readiness, link rename, netns appearance). Bump if a slow daemon-restore window starves endpoint setup. |
| `STATE_DIR` | `/var/lib/net-dhcp` | Where per-network options, the tombstone file, and the `audit_log` ledger persist. **Bind-mounted from the host at this exact path since v1.5.0**, so its contents survive `docker plugin rm` — before that they lived in the plugin rootfs and every upgrade destroyed them. Two consequences: durability begins with the version that introduced the mount (an upgrade *onto* v1.5.0 still starts from nothing, because the old state was never on the host), and **repointing this setting opts out** — a path other than the mounted one is inside the rootfs again and is wiped by the next upgrade. |
| `METRICS_ADDR` | *(empty)* | (v1.8.0+) TCP address for the Prometheus `/metrics` endpoint, e.g. `127.0.0.1:9099`. Empty means **no TCP listener**, which is the default and the recommended state unless you are scraping it. `/metrics` is always available on the plugin socket regardless of this setting. **Bind it to loopback or a management interface, never `0.0.0.0`** — see the security note under [`/metrics`](#metrics). A malformed address fails plugin startup rather than being ignored, and a wildcard bind (`:9099`, `0.0.0.0:…`, `[::]:…`) logs a warning at startup naming what the endpoint exposes — it is not refused, because it is a legitimate choice on a private segment, but it should be a choice. |
| `DOCKER_HOST` | *(empty)* | (2.0 beta+) Docker API endpoint. Empty means the socket `config.json` bind-mounts, which is what every installation before this setting used and what an operator who sets nothing keeps. Point it at a read-only Docker API proxy to reduce the one grant that makes a compromise of the plugin equivalent to root on the host — the plugin issues only `GET` and `HEAD`, refuses anything else before sending it, and counts the refusal as `docker_api_non_get_refusals`. The allowed paths and a worked example are in [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md). A TLS endpoint is not supported (nothing here reads `DOCKER_CERT_PATH`), and neither is a proxy on its own unix socket: the plugin only sees the paths `config.json` mounts, and that mount's source is not settable. Use a plain TCP endpoint on the host's loopback, which the plugin reaches through host networking. |

---

## Behaviour

What the plugin does with leases, identity, and state. All of it applies
to **every** attachment mode unless a paragraph says otherwise.

### Requesting a specific address

`--ipam-driver=null` means `docker run --ip=` is rejected by the daemon
before it ever reaches the plugin. Pin an address with the per-endpoint
driver option instead; the plugin hands it to the DHCP client as the
requested address (DHCP option 50) on the initial DISCOVER:

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

- **MAC stability holds whenever a tombstone was written** — `docker
  inspect` and the LAN then see the same MAC across the restart. A
  tombstone is written at `DeleteEndpoint` from what the plugin recorded
  when it built the endpoint, so the condition is that the plugin still
  holds that record. It does across `docker restart`. It did **not**
  across a plugin restart before v1.8.0: endpoints rebuilt by recovery
  were re-attached without re-recording, so the next `DeleteEndpoint`
  laid down nothing and the container came back on a fresh MAC (#721).
  Fixed, and stated as a condition rather than an absolute because the
  absolute is what made that bug invisible to anyone reading this page
  to decide whether they needed a reservation.
- **IP stability depends on the server** honouring option 50, exactly as
  for an explicit request above. Where it doesn't, configure a
  reservation against the now-stable MAC and every restart gets that
  address — **in `bridge` and `macvlan` only**. `ipvlan` has no stable
  per-container MAC to key a reservation on: its L2 slaves all inherit
  the parent's, so a reservation would either match nothing or match the
  parent and hand one address to every container on the network. The
  plugin writes no tombstone for `ipvlan` at all, for the same reason.
  See [DHCP identity](#dhcp-identity) for what `ipvlan` uses instead and
  why it does not survive a restart (#219).

Two things it deliberately does not do. Concurrent restarts of several
containers on one network inside the 60-second window fall back to fresh
MACs rather than risk swapping identities between containers — tombstones
carry the container hostname so restarts in flight can be told apart when
the hostname is known, and only when neither side knows it does the
network-wide "exactly one match" rule apply. Sequential restarts, the
normal case, always satisfy it.

A container whose hostname the plugin **refuses** — one carrying a
control character, which never reaches a DHCP packet (see
`unsafe_hostnames_rejected`) — gets no tombstone at all, and so does not
keep its MAC across a restart. That is deliberate and it is not the same
as having no hostname: a hostname-less container writes a tombstone that
matches network-wide, which is the v0.5.0 behaviour above and is correct
for it. Writing one for a *refused* hostname would make the value the
plugin declined to trust for a narrow match into a match against every
container on the network.

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
  container and renews the same address. It is what makes IPv4
  restart-stable without depending on a `DHCPRELEASE` being sent on the way
  out — which is not always possible (`SIGKILL`, OOM, power loss). Since
  v1.9.0 the plugin never sends one at all (#800), so this identity is the
  whole mechanism: a restarting container gets its address back by asking
  again and being recognised.

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

**Applied**, when the matching option is enabled — option 6 (DNS servers)
and option 119 (search list, falling back to option 15) into
`/etc/resolv.conf` with `propagate_dns`; option 26 into the link MTU with
`propagate_mtu`; option 121, or option 33 in its absence, as routes (see
[classless static routes](#dhcp-classless-static-routes-option-121)). The v6
equivalents (options 23 and 24) are not read, because the beta runs no DHCPv6
exchange to read them from.

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

**The 2.0 beta leases no IPv6 addresses.** `-o ipv6=true` fails
`docker network create`; the client in this branch implements DHCPv4 only.
DHCPv6 is a later beta milestone, and the 1.x line is where it works today.

The refusal is keyed on the decoded option, not on the spelling of the key,
so `-o ipv6=true`, `-o IPv6=true` and `-o Ipv6=true` are all refused. It sits
at network creation rather than at the first container because a network that
was created asking for IPv6 and then quietly runs without it is precisely the
failure the refusal exists to prevent.

Everything the 1.x page described here — the second persistent client, the
stateful lease on the interface as a `/128`, the Router Advertisement guard
(`accept_ra=2` / `autoconf=1` / `keep_addr_on_down=1` and the read-only
`/proc/sys` pin), the DUID-LL and pinned IAID, `propagate_dns` over options 23
and 24, and prefix delegation being out of scope — describes 1.x and is not
what this branch does. None of that machinery is present in this tree.

#### If you upgrade onto the beta with an IPv6 network already created

A network record written by a 1.x build carries `ipv6=true`, and nothing
rewrites it. The refusal above runs at `CreateNetwork`, which that network
does not go through again, so the stored record survives the upgrade and the
plugin behaves like this — read from the tree, not from intent:

- **`docker network create` on a *new* v6 network fails**, as above.
- **`CreateEndpoint` succeeds, IPv4-only.** The v4 acquisition runs
  normally. The v6 acquisition is refused inside the DHCP layer before a
  packet goes out, and hands back an observation with nothing seen, which the
  plugin classifies as the tolerated "no router on this segment" case: it
  logs a warning naming the interface, increments
  **`dhcpv6_no_router_advert`**, and creates the endpoint without a v6
  address. That happens once per endpoint, always — on a segment that may
  well have a router. The counter's own 1.x wording is about the segment;
  here it is about the stored record.
- **`Join` returns success and the container starts — with an address
  nothing will renew.** Two things happen, in order. Because the stored
  record says `ipv6=true`, the manager first clears the engine's
  `disable_ipv6` on the container link (the engine sets that flag on a
  sandbox interface whose endpoint carries no IPv6 address, which is exactly
  this case). If that write fails it increments
  **`ipv6_link_enable_failures`** and carries on — so that counter is *not*
  dead on the beta, and the reason is the opposite of the obvious one: it is
  precisely because no DHCPv6 client is constructed that this step runs and
  then the start fails. It then tries to start the persistent client for the
  v6 family, which is refused, and that failure takes down the v4 persistent
  client that had already started beside it. The client start runs in a
  goroutine that `Join` does not wait for — `Join` has already answered
  Docker — so the container comes up holding the IPv4 address the
  `CreateEndpoint` one-shot won, and nothing renews it. The failure lands on
  **`join_start_failures`**, which **does** flip `healthy` to false. When the
  lease runs out the container is still on the network with an address the
  server no longer considers its own.
- **At plugin start, recovery replays the same sequence for such an
  endpoint** — the same `disable_ipv6` clear, the same refusal — and bumps
  **`recovery_failed`**, which also flips `healthy` to false.

So the counters a legacy `ipv6=true` network moves, in the order they move:
`dhcpv6_no_router_advert` at `CreateEndpoint` (always, one per endpoint),
`ipv6_link_enable_failures` at `Join` (only if the link write fails), and
`join_start_failures` at `Join` (always, and `healthy`-affecting), then
`recovery_failed` at every plugin restart (also `healthy`-affecting). The
remaining two v6 counters cannot move at all: nothing emits the audit event
`dhcpv6_config_only` reads, and the refusal above always classifies as "no
router", never as the "no DHCPv6 offered" case `dhcpv6_not_offered` counts.

The remedy is to recreate the network without `ipv6=true`. There is no
migration step and the plugin does not perform one.

### Recovery after a plugin restart

`docker plugin disable && enable`, a plugin upgrade, or a plugin crash
used to leave running containers without a renewal client — the lease
would quietly expire and the container would lose its address.

The plugin now walks Docker's network list at startup, finds every
endpoint on a plugin-served network, and rebuilds a DHCP manager for
each. The first acquisition requests the address the container is already
using (option 50) so the server ACKs it rather than allocating a new one.

Recovery runs synchronously inside plugin startup, before the socket
accepts requests, whenever the daemon is answering — the normal case.
When it is not, the walk is deferred until after the socket is up and
can meet a `CreateEndpoint`; registration is a compare-and-set, so the
`Join` keeps its client and recovery stands down (see below). Results land on `/Plugin.Health` as `recovered_ok`,
`recovery_failed`, `recovery_aborted_container_gone` and
`recovery_network_gone` — the last two covering containers that had
already exited when recovery reached them, and networks removed between
the listing and the read of their detail. `recovery_fingerprints_skipped`
covers a third: an endpoint recovery adopted but could not learn the
hostname of, which keeps its renewal client but loses the tombstone that
would have carried its address across its next restart (#721). Neither is a failure and
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

A deferred retry runs while the host's containers are coming back, so it
can meet an endpoint a `Join` has already claimed. Recovery yields:
registration is a compare-and-set, and losing it leaves the `Join`'s
client in place and counts `recovery_already_managed` (v1.8.0+). A
`Join` arriving the other way round still displaces a recovery-built
manager and stops it — that direction is `displaced_stops`. Both
directions end with exactly one DHCP client on the interface, which is
the property that matters (#480).

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

#### File permissions after an upgrade

Since v1.8.0 the plugin writes everything under `STATE_DIR` with mode
`0600` (#708) — the directory is a read-write bind mount from the host, so
at `0644` the container MACs, leased IPs, hostnames and the lease audit
trail were readable by any user on the host. Nothing there is a credential
and the writer is root either way, so this is not a privilege boundary.

The mode is applied **when the plugin writes a file**, which means an
upgrade does not retroactively tighten what it finds. `tombstones.json` is
rewritten only when a tombstone is laid or consumed, so on a host with
stable containers it can keep its old `0644` indefinitely. If you upgraded
from v1.7.1 or older, tighten them once by hand:

```bash
sudo chmod 0600 /var/lib/net-dhcp/*.json /var/lib/net-dhcp/leases.jsonl
```

Having the plugin sweep `STATE_DIR` at startup is tracked in
[#804](https://github.com/claymore666/docker-net-dhcp/issues/804).

---

## Observability

### `/Plugin.Health`

JSON liveness + counters on the plugin's UNIX socket. **`sudo` is
required** — `/run/docker/plugins` is `drwx------ root root`, and
without it `curl -s` swallows the permission error and prints nothing,
which looks exactly like a dead endpoint:

```bash
PLUGIN_ID=$(docker plugin inspect -f '{{.Id}}' ghcr.io/claymore666/docker-net-dhcp:v1.9.0)
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
| `healthy` | — | `false` when `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, `tombstone_quarantines`, or `address_conflicts` is non-zero — an operator should look. Those five, and only those, are the ones marked **yes** in this column. The plugin keeps serving fresh attaches either way. **It latches:** every counter behind the flag is monotonic, so `false` means "a fault occurred at some point during this plugin process", not "something is wrong right now". Fixing the condition does not clear it — only restarting the plugin does, and that tears down the renewal client of every managed endpoint on the host. Read it together with the *instance_id* field: the same ID means the same process is still reporting a fault it recorded earlier. |
| `instance_id` | — | (v1.5.0+) Opaque identifier of the plugin **process** serving this response. Every counter below is in-memory and returns to zero when the process does, so two readings are comparable as a delta only when their `instance_id` matches. If it changed between two samples, the plugin restarted and any difference you computed is meaningless — including one that reads as zero. Prefer this over `uptime_seconds` for that check: a plugin that restarts early in a long sampling window and then runs longer than the first reading shows uptime going *up*, hiding the restart. |
| `uptime_seconds` | — | Seconds since the plugin process started. Useful as an age, but see `instance_id` before using it to decide whether a restart happened. |
| `active_endpoints` | — | DHCP managers currently registered (post-Join, pre-Leave). |
| `pending_hints` | — | Join hints awaiting consumption; steady-state ~0. |
| `recovered_ok` | — | Endpoints successfully rebuilt by plugin-restart recovery. |
| `recovery_failed` | yes | Post-restart rebuilds that failed **for a container that is still running** — it runs without lease renewal and loses its IP at expiry; restart it. Three things are deliberately *not* counted here, because none of them leaves a running container without a renewal client: a daemon that is merely still starting (`recovery_deferred`, #383), a container that had already exited when recovery reached it (`recovery_aborted_container_gone`, #376), and a network removed out from under the recovery walk (`recovery_network_gone`, #648). |
| `recovery_deferred` | no | (v1.4.0+) Recovery met a daemon that was not serving yet and was retried once the plugin socket came up (#383). Expected on a daemon restart. Only worth attention paired with `recovery_failed`, which together mean the retry ran out too. |
| `recovery_aborted_container_gone` | no | (v1.4.0+) Recoveries abandoned because the container had already exited, or been removed, by the time post-restart recovery reached it. Not a fault: nothing is left running without a renewal client, so this never flips `healthy`. The recovery-side twin of `join_aborted_container_gone`, and normal after a daemon restart that outlived some containers (#376). |
| `recovery_network_gone` | no | (v1.8.0+) Networks skipped during post-restart recovery because they had been removed between the listing that found them and the read of their detail. Not a fault: a network that is gone leaves no running container without a renewal client, so this never flips `healthy`. Counted rather than passed over in silence — a host where this climbs steadily is churning networks under a restarting daemon, which is worth knowing even though no single occurrence is a problem. Until v1.8.0 it landed in `recovery_failed`, where an ordinary `docker network rm` racing a daemon restart reported the plugin's most serious fault (#648). |
| `recovery_fingerprints_skipped` | no | (v1.8.0+) Endpoints that post-restart recovery adopted but could not describe: the `ContainerInspect` that supplies the hostname did not answer, or answered with no hostname. **No** because the endpoint keeps its renewal client — nothing is running without one, which is the line `recovery_failed` draws. What it loses is the fingerprint, so `DeleteEndpoint` lays no tombstone and that container gets a fresh MAC, and in general a different address, on its next `docker restart`. Counted because before #721 the only sign was `tombstones_consumed` staying flat, which is also what a quiet host looks like — an operator could not tell "recovery worked" from "recovery silently skipped half my endpoints". A hostname *refused* for carrying a control character is not counted here; it moves `unsafe_hostnames_rejected` instead, so a degraded daemon stays distinguishable from a hostile container. |
| `recovery_already_managed` | no | (v1.8.0+) Endpoints a recovery walk found already registered to another manager, and therefore left alone — a `Join` reached them first. Not a fault: the endpoint has a renewal client, it just is not the one this walk would have built. It is counted because it is the only outward evidence of recovery racing a `Join`, the window that made the registration a compare-and-set rather than a read followed by a write; before v1.8.0 those endpoints were reported as *recovered* in the completion log while `recovered_ok` correctly did not move (#480). |
| `join_start_failures` | yes | (v1.3.3+) Persistent-client start failures at attach time **for a container that is still running** — it got its initial lease but runs without renewal, and the lease is never released on disconnect (#317). The plugin log carries the cause; fix it and restart the container. A container that *exited* mid-attach is counted separately and is not a fault — see below (#373). |
| `join_aborted_container_gone` | no | (v1.4.0+) Attaches abandoned because the container exited before the persistent client was up. Not a fault: there is no running container missing a renewal client, so this never flips `healthy`. A sustained rise still says something real — containers dying seconds after start, e.g. a crash-loop (#373). Recognised three ways: the daemon answering "no such container", the container's netns having gone, or its sandbox key being unlinked. An attach that fails for any other reason is counted as a fault, not excused (#401). |
| `join_aborted_no_container` | no | (v1.6.0+) Attaches abandoned because no container ever claimed the endpoint on the network. Since v1.9.0 the leased address is **left to expire** rather than released (#800); before then it was released here, which is why the counter's name says nothing about either. Not a fault: nothing is running without a renewal client, because nothing is running — so this never flips `healthy`. Distinct from `join_aborted_container_gone`, which needs the daemon to say "no such container" or the sandbox netns to be visibly gone; this one covers the case where the endpoint is simply unclaimed after the attach budget, which previously fell through to `join_start_failures` and leaked the address (#566). |
| `join_attach_slow` | no | (v1.4.0+) Attaches that succeeded, but only after outlasting `AWAIT_TIMEOUT`. Not a fault — the container has its renewal client. It is reported because the wait has an external cause worth seeing: the attach asks the daemon about the container being attached, and the daemon does not answer while it is still inside that container's start. Before v1.4.0 those attaches were abandoned and counted as `join_start_failures`, leaving a running container with no renewal client (#406). A rising count means the daemon is holding containers longer, not that the plugin is degrading. |
| `join_aborted_endpoint_left` | no | (v1.4.0+) Attaches cancelled because `Leave` arrived while the attach was still running — the endpoint was being torn down. Not a fault: there is no running container missing a renewal client. Distinguished from `join_start_failures` by direct evidence rather than inference, since the plugin cancelled the attach itself and knows why (#406). |
| `tombstone_write_failures` | yes | Failed tombstone saves (disk full, EROFS) — the next restart of some container will pick a fresh MAC/IP instead of inheriting. Since v1.8.0 it also moves when the tombstone file could not be **read** for a transient reason (EIO, a read racing a writer): the plugin refuses to rewrite the file from nothing rather than destroy contents that may be perfectly good, and the consequence for that endpoint is identical to a failed write. The name is narrower than the meaning; the meaning is "an endpoint will not keep its address across a restart" (#724). |
| `tombstone_quarantines` | yes | (v1.8.0+) Times the tombstone file was found **unparseable** and moved aside as `tombstones.json.corrupt-<timestamp>` in [`STATE_DIR`](#plugin-settings) (#724). Strictly worse than `tombstone_write_failures`: that costs one container its MAC and address, this costs every one of them, because the whole live tombstone set went with the file — any container restarting for the next 60 seconds comes back with a new identity. Kept separate from the write counter on purpose, since the two call for different action. **The quarantined file is never reaped.** Read it before deleting it: it is the only record of what was lost, and its contents say whether this was a truncated write, a filesystem fault, or something else writing to that path. |
| `tombstones_consumed` | no | (v1.5.0+) Recreated containers that got their previous MAC/IP back by replaying a fresh tombstone. Not a fault — this is the address-stability mechanism working. It is the counterpart to `recovered_ok`: after a restart an address is preserved either by recovery re-adopting a still-attached endpoint (`recovered_ok`) or by a tombstone being replayed (this). Reported so the two can be told apart, which is what makes "the address survived, but via neither path" observable rather than silent (#386). |
| `lease_changed` | no | Renewals that returned a different IP than last recorded (v4+v6 aggregate). Docker's `inspect` view does **not** update on lease change (libnetwork has no in-place endpoint-IP swap), so this is the stale-inspect-window signal — alert on it for long-running containers. |
| `address_conflicts` | **yes** | (v1.6.0+; RFC 5227 since the 2.0 beta) Leased addresses found to be already in use by another device on the segment. The DHCP client runs RFC 5227 Address Conflict Detection from inside the container's network namespace: §2.1 ARP-probes the offered address **before it is used**, and §2.4 keeps listening for the whole life of the lease. Either way the address is DHCPDECLINEd to the server (RFC 2131 §3.1(5)) and another one is requested, so the counter moving means the plugin found a conflict *and acted on it* — not that a container is sitting on a contested address. **A conflict found after the address is in use changes the container's address**, which `docker inspect` does not update; watch `lease_changed` too. Under `conflict_check=off` the client neither probes nor listens, so nothing inside it can find a conflict; the counter can then move only for a conflict **reported to the client from outside it**, and no code path in the plugin does that today — so on an `off` network it does not move. The same rule governs `acd_conflicts_detected`. This is the only signal for the condition from the plugin's side — from the DHCP server's point of view the lease was issued normally — though since the 2.0 beta the server also learns about it, because the DECLINE is on the wire and in its log. The usual cause is a **statically configured** host inside the DHCP pool range: it never asks the server for anything, so the server cannot know the address is taken. Fix it at the server (reserve or exclude the address), not at the plugin. **What it does not cover:** another container on the *same host* sharing the same parent NIC. macvlan isolates a parent from its own children, so a sibling's answer never reaches the probe. Excluded by construction, not pending work (#528). |
| `acd_probes_sent` | no | (2.0 beta) RFC 5227 §2.1.1 ARP Probes sent. Read this **before** believing `address_conflicts` is 0: with no probes the two readings are identical, and "the detector never ran" is what #524 looked like for months. A healthy segment is `acd_probes_sent` climbing with `address_conflicts` at 0. Moves in `conflict_check=wait` and `=async`, never in `=off` — so a zero here on a host whose networks are all `off` is the configuration working, not a fault. |
| `acd_announcements_sent` | no | (2.0 beta) RFC 5227 §2.3 ARP Announcements sent — two per address that passed its probe, telling the segment the address is now in use. Read against `acd_probes_sent`: probes climbing with announcements flat means addresses are being checked and none is coming back clean. Moves in `wait` and `async`, never in `off`. |
| `sandbox_netns_visible` | no | (v1.6.0+) How many sandbox netns entries the plugin can currently see, or `-1` if it cannot read the directory at all. Sampled per request, not accumulated. **Read it against `active_endpoints`, never alone.** `-1` means the bind mount is missing, so the `sandboxGone` check can only ever answer "no usable evidence" — safe but useless. A `0` *with endpoints attached* is the dangerous one: the directory is readable but mounted from the wrong place, and `sandboxGone` would conclude every container had vanished. A `0` with nothing attached is neither — there is genuinely nothing to see (#567). |
| `acd_arp_send_failures` | no | (2.0 beta) ARP Probes and Announcements the socket refused. Not healthy-affecting on its own — a refused send is not a conflict — but a probe that never went out proves nothing about the address, so a rise turns "no conflict found" into "the question was not asked". Watch it for the same reason `acd_probes_sent` exists: a detector that has stopped working looks exactly like a clean segment, which is how #524 went unnoticed in the first place. |
| `acd_conflicts_detected` | no | (2.0 beta) Conflicts counted by the DHCP client's own state machine. It is the **same population** as `address_conflicts`, counted inside the client rather than from the events it emitted, and the two are expected to be equal. A difference is a defect in the plugin's event handling, not a property of your segment — report it. Its `conflict_check=off` rule is its sibling's, not a different one: with no probe and no listener the only thing that can move either counter is a conflict reported to the client from outside it, and the plugin has no such caller today. |
| `leases_obtained` | no | Client bind events: an initial bind, or a re-bind after a NAK or a lease loss. v4+v6 aggregate — but the beta leases no IPv6, so this equals its `_v4` half. |
| `leases_renewed` | no | Client renewal events, including the case where the renewal returned a different address. v4+v6 aggregate — equal to its `_v4` half in the beta. |
| `dhcp_server_tier_fallbacks` | no | (v1.8.0+) Steps **down** the `dhcp_servers` ladder: one per preferred entry that did not answer inside its slice of the acquisition budget and handed on to the next (#111). It counts transitions, not container starts — a single `docker run` against three silent preferred servers adds 2. That is the more useful number (it says how far down the list acquisition had to walk) and it is what the code has always produced; this row and two other copies said "acquisitions" until #731. Not healthy-affecting — the endpoint still got an address. This is the only outside signal that a preferred server is silently dead: a steady rise while every container still starts fine is exactly the condition that otherwise goes unnoticed until the standby fails too. |
| `dhcp_server_policy_exhausted` | no | (v1.8.0+) Initial acquisitions abandoned because **no** server listed in `dhcp_servers` answered (#111). Not healthy-affecting on its own: the acquisition failure it accompanies already fails the operation and is counted. It exists because "the servers you named are all silent" and "DHCP is broken" look identical in a timeout log and call for different action. |
| `dhcp_server_policy_timeouts` | no | (v1.8.0+) The renewal half of the same question (#731): `dhcp_timeouts` ticks raised on an endpoint whose **renewal** client is restricted to `dhcp_servers`. `dhcp_server_policy_exhausted` cannot cover this — nothing is exhausted at renewal, because the persistent client has no ladder to walk; it holds one whitelist and simply gets no answers, which looks exactly like the server being down. A **strict subset** of `dhcp_timeouts`, and that is how to read it: the two rising together says the allow-list is the cause (a named server renumbered, retired or firewalled), `dhcp_timeouts` rising alone says it is not. Not healthy-affecting, for that same reason — every tick here is already counted in `dhcp_timeouts`, and counting one outage twice would make a policy-restricted endpoint look worse than an unrestricted one failing identically. v4 only: `dhcp_servers` is not applied to DHCPv6, so this can never rise for a v6-only failure. |
| `dhcp_timeouts` | no | DHCP failures reported by the client itself: an acquisition that found no server, or a held lease that ran out without being renewed. **Changed in the 2.0 beta.** 1.x had no direct signal and inferred one from a watchdog that compared the granted lifetime against the time since the client was last served, so a bound endpoint's outage surfaced only after its whole lease had elapsed plus one watchdog tick — up to ~24 hours on a 24-hour lease. The in-tree client owns the lease and its own retransmission schedule, so it reports the failure when it happens, at the end of the retransmission budget rather than at the end of the lease. `OUTAGE_TICK` and `OUTAGE_GRACE` are gone with the watchdog. v4+v6 aggregate — equal to its `_v4` half in the beta. |
| `client_stop_failures` | no | (renamed from `lease_release_failures` in v1.9.0, #800) A renewal client did not shut down cleanly when the plugin stopped it at teardown. **What "cleanly" means changed in the 2.0 beta:** there is no client process to signal and no exit status to read — the plugin cancels the client's context and waits for its goroutine to return, so a tick here means the run loop came back with an error that was not the cancellation, or did not come back inside the finish timeout at all. It does **not** mean a lease went unreleased: since v1.9.0 nothing this plugin runs sends a DHCPRELEASE on any path, so every stopped container's address is held until it expires whatever this counter reads. A pattern points at clients wedging in their own loop, or at a finish timeout set too tight. |
| `naks_received` | no | (v1.0.0+) The server NAKed a renewal, a rebind or an INIT-REBOOT request (v4+v6 aggregate). The client recovers by re-acquiring, so each NAK is typically followed by `leases_obtained` — and, if the address moved, `lease_changed` — bumps. Climbing alongside `lease_changed` means containers are being re-addressed mid-life. |
| `displaced_stops` | no | (v1.3.5+) Attaches that found a manager already registered for the same endpoint and stopped it — a container restarting into a plugin that had already recovered it (#338). The displaced client is stopped cleanly and the new one takes over — stopped, not released: it sends no DHCPRELEASE, so the address stays leased and the incoming client renews it, so a few are normal after a plugin restart. Climbing steadily alongside `recovered_ok` means a container is in a restart loop. |
| `restart_link_up_waited` | no | (v1.5.0+) Child links that came up only after waiting out the departing link's hold on the address — i.e. how often a container restart met the #408 window and the fix carried it. Not a fault: this is the repair working, counted so the window is visible rather than inferred. A steady rise means your hosts restart containers fast enough to hit it routinely, which is expected for images that handle `SIGTERM` promptly. |
| `restart_link_up_timeouts` | no | (v1.5.0+) The same wait outlasting its budget: the restart fails and `docker restart` reports `address already in use`. A real failure, but deliberately not `healthy`-affecting — it surfaces directly to whoever ran the command, and `healthy` is for faults nothing else reports. Any non-zero value here is worth investigating; it means the departing link held the address longer than the budget allows (#422). |
| `parent_link_waits` | no | (v1.6.0+) Operations that had to queue for a shared parent interface before attaching their own link. A parent NIC can be a macvlan port or an ipvlan port but never both, so when networks of both kinds share one parent — or when a lease reclaim still has its temporary link attached — the plugin serialises them per parent rather than letting the kernel refuse one with `device or resource busy` (#486, #549). Queuing is the mechanism working; a steady rise just means that NIC is busy. |
| `parent_link_wait_timeouts` | no | (v1.6.0+) The same wait giving up after its budget, after which the operation asks the kernel anyway and may fail with `device or resource busy`. Deliberately bounded well below the reclaim's own budget so a wedged reclaim degrades to the pre-v1.6.0 behaviour instead of stalling a container start. Not `healthy`-affecting, but the actionable one of the pair: any non-zero value means something held a parent far longer than a DHCP round trip should take, and container starts on that NIC were refused as a result. |
| `unsafe_hostnames_rejected` | no | (v1.8.0+) Container hostnames dropped because they carried a control character (#692). **What the drop protects changed in the 2.0 beta.** Nothing generates a client config any more — `directives_refused`, which counted values kept out of one, is removed for exactly that reason — and the hostname now goes straight into the DHCP parameters the plugin builds and onto the wire, as option 12 and, with `register_dns`, as the option-81 FQDN. The drop is still the safe outcome and the lease proceeds — the hostname decorates the exchange and the opt-in `register_dns` registration — so this is not `healthy`-affecting. It is not purely cosmetic, though: the hostname is also the key that narrows tombstone matching to the container that wrote the tombstone, where an *empty* hostname means "match any tombstone on this network", so a refusal is deliberately kept distinguishable from an absence rather than collapsed into an empty string. Read it as an intent signal rather than a fault: Docker does not validate `--hostname`, and a legitimate one never contains a control character, so a non-zero value means something sent one on purpose. Underscores and other technically-illegal-but-common hostnames are **not** counted; the rule is about control characters, not about RFC 1123. |
| `unsafe_option_values_dropped` | no | (v1.8.0+) Server-chosen DHCP string values refused before use because they carried a control character, plus option-15 domains truncated at their first space. The filter is reflective and covers every string value in the lease, so a new one is covered the day it is added; the ones it exists for are the free-text options — 66, 67, 100, 101 and 252 — which arrive as bytes the server chose and are carried into a log line, a `resolv.conf` or the audit ledger, none of which share an escaping rule. A space in option 15 additionally turns one search domain into several, with the server's choice first in the order, so that cut is counted here too. The sibling of `unsafe_hostnames_rejected`, for the values the *server* chooses rather than the container. A legitimate server sends none of these, so any rise is deliberate. |
| `network_options_rejected` | no | (v1.8.0+) Endpoint operations that met a network's stored options and would not act on them as written: an interface name the kernel would not accept, or a `mode` this plugin does not implement. Name validation runs when a network is *created* (#705); this check runs every time the stored options are *read*, which is where the name actually reaches netlink. Not healthy-affecting: refusing is the safe outcome, the operation already fails visibly to Docker, and one network's record being wrong does not make the plugin unwell — every other network on the host keeps working. A non-zero value means one network needs recreating: either it was created before name validation existed, or its options were written directly into the state directory. `DeleteEndpoint` is deliberately exempt so a refused network can still be torn down — it counts an unknown mode and proceeds, so a rise here does not mean nothing was torn down. Only the mode: teardown reads no stored name at all (it derives the link from the endpoint ID), so there is no name refusal available to it. |
| `dns_propagation_pid_mismatches` | no | (v1.8.0+) DNS propagations refused because the container PID resolved through Docker no longer belonged to that container by the time the plugin acted on it (#688). Only reachable with `propagate_dns` opted in. Refusing is the safe outcome — the container keeps the `resolv.conf` it had, and the next renewal propagates again — so this is not `healthy`-affecting. It is reported because the plugin runs in the host PID namespace: each one is a `/etc/resolv.conf` write that would otherwise have landed in an unrelated host process that inherited the recycled PID. A sustained rise means containers are exiting inside the propagation window; an isolated one is a container that stopped at the wrong moment. |
| `netns_pid_mismatches` | no | (v1.8.0+) Sandbox network-namespace opens refused because the container PID resolved through Docker no longer belonged to that container. The sibling of `dns_propagation_pid_mismatches` above, on the path with the larger blast radius and with no opt-in: what the refusal prevents is not one file but a netlink handle carrying every address, MTU and route the plugin applies, with `CAP_NET_ADMIN`. **In the 2.0 beta the second half of that is closer to home, not further away.** Nothing is spawned into the namespace: the plugin locks one of its own OS threads, `setns`es it into the sandbox, opens the raw packet socket that carries the whole DHCP exchange there, and returns the thread. So the wrong namespace no longer means a client process pointed at the wrong container — it means the plugin's own thread and its own `CAP_NET_RAW` socket landed on an unrelated host process's network, and the client that keeps running on that socket stays there for the life of the endpoint. Refusing fails the attach, so unlike the DNS case it is not silent — but the error reads like a slow container start, and only this counter says the PID belonged to something else. Not `healthy`-affecting: the attach failure is reported to Docker, and the container simply does not come up on this network. Any non-zero value is worth reading: it means a container exited inside the attach window and the kernel handed its PID to another task. |
| `dhcp_routes_applied` | no | (v1.8.0+) DHCP option-121 classless static routes handed to Docker at Join — routes, not Joins. `skip_routes=true` opts out and then this never moves. Read it as the denominator for the row below. |
| `dhcp_default_route_superseded` | no | (v1.8.0+) Joins whose option-121 routes cover `0.0.0.0/0` **by union** rather than by a literal default entry — e.g. `0.0.0.0/1 g` plus `128.0.0.0/1 g`. Neither half is a default route, so the gateway reported to Docker (and shown by `docker inspect`) is still the one from option 3, while every packet follows the option-121 next hop instead. This is legitimate in split-tunnel setups and the routes are applied either way; the counter exists because before it, nothing in the plugin's output distinguished the two cases. The accompanying log line names each destination and next hop. |
| `mtu_refused` | no | (v1.8.0+) Option-26 MTUs outside `[576, 65535]`, refused with the container link left at the MTU it had. Only moves with `propagate_mtu=true`. Nothing below the plugin holds the bottom of that range — a server-supplied 68 is carried through verbatim and would be accepted — and the result is destroyed throughput plus black-holed path MTU discovery, re-applied on every renewal, which looks like a slow network rather than a misconfiguration. |
| `sandbox_key_entries` | no | (2.0 beta+) Container network namespaces entered through the sandbox key the daemon publishes under `/var/run/docker/netns/`. **On a stock engine no attach counts here, and each endpoint recovered after a plugin restart does**: the plugin's read-only `/var/run/docker` is a bind taken at plugin start, so a sandbox older than this plugin process is reachable through its key and one created afterwards is not (measured 2026-09-05). It is the **denominator** for the two rows below and the reason they can be read at all — zero fallbacks with zero entries means nothing was entered, not that the key route works. If it rises on attaches, the mounts do reach this plugin on your host, and the netns half of `pidhost`/`CAP_SYS_PTRACE` is not load-bearing for you. |
| `sandbox_key_entry_failures` | no | (2.0 beta+) Attempts to enter a container network namespace through the sandbox key that were refused — the key named a directory this plugin does not accept, the entry could not be opened, or (the ordinary case for an attach) it opened but is not a namespace, because the sandbox was created after the plugin's own `/var/run/docker` bind mount was taken. Each one falls straight through to the PID route without a retry, so the endpoint still comes up. **This rises once per attach on a stock engine**; it is the counterpart of `sandbox_key_entries` staying at zero, and it means the host PID namespace and `CAP_SYS_PTRACE` are load-bearing on this host. |
| `sandbox_pid_fallbacks` | no | (2.0 beta+) Endpoints whose network namespace was entered through `/proc/<pid>/ns/net` after the key route was refused. **On a stock engine this is every attach.** That route is why the manifest asks for the host PID namespace and `CAP_SYS_PTRACE`, and it carries the PID-recycling hazard `netns_pid_mismatches` counts. Not `healthy`-affecting — a fallback that succeeds is a working endpoint — but read against `sandbox_key_entries` it is the one number that says which route your host actually uses. |
| `docker_api_non_get_refusals` | no | (2.0 beta+) Requests to the Docker API refused before they were sent because their method was neither `GET` nor `HEAD`. The plugin's whole Docker surface is `NetworkList`, `NetworkInspect`, `ContainerInspect` and the client library's version ping, so this stays at zero for the life of an installation; a non-zero value means code in this process tried to **write** to the daemon, which is the grant that makes a compromise of the plugin equivalent to root on the host (#691). Not `healthy`-affecting: the refusal is the safe outcome and the caller sees the error. |
| `ledger_write_failures` | no | Failed `audit_log` ledger appends — degrades forensics, not networking. Operators using `audit_log` alert on this. |
| `dhcpv6_config_only` | no | (v1.9.0+) **Cannot move in the 2.0 beta.** In 1.x this counted DHCPv6 information replies — a stateless network answering with options and no address (#815). It increments only on a `config` audit event, and the layer that translates the library's events into audit kinds emits no such kind at all, so the increment is unreachable from any input. The field is still rendered so a scrape does not lose a series across the upgrade; **a zero here means "this build cannot report it", not "nothing went wrong"**. |
| `dhcpv6_not_offered` | no | (v1.9.0+) **Cannot move in the 2.0 beta.** In 1.x this counted endpoints on an IPv6 network whose router advertisement offered no DHCPv6 address (#868). Reaching it needs an observation that a router *was* seen and advertised no managed DHCPv6. The beta's v6 acquisition is refused before a packet goes out and hands back an observation with nothing seen, so the classifier always takes the "no router" arm and `dhcpv6_no_router_advert` below takes every legacy-network endpoint instead. A zero here means the build cannot report it. |
| `dhcpv6_no_router_advert` | no | (v1.9.0+) In 1.x: endpoints on an IPv6 network where no router advertisement arrived inside the acquisition budget (#868). **In the 2.0 beta it means something else, and it is the first counter a legacy `ipv6=true` network moves:** an endpoint was created on a network whose stored record still says `ipv6=true` — a network created by a 1.x build — and the v6 acquisition was refused inside the DHCP layer before any packet went out, which is reported as nothing observed and read as "no router". One per such endpoint, always, whatever the segment actually carries: it is not evidence about the segment. Not `healthy`-affecting by itself — the endpoint is created IPv4-only. What happens at the following `Join` is not benign; see [DHCPv6](#dhcpv6-ipv6true). |
| `ipv6_link_enable_failures` | no | (v1.9.0+) Container links the plugin could not administratively enable IPv6 on (#868). **It still moves in the 2.0 beta.** The 1.x wording said "before starting a DHCPv6 client", and that half no longer holds: no DHCPv6 client is constructed, and the enable step runs anyway. On a legacy `ipv6=true` network, `Join` clears the engine's `disable_ipv6` on the container link *before* it tries the v6 client — the engine sets that flag on a sandbox interface whose endpoint carries no IPv6 address, which is exactly this case, so the write is genuinely attempted — and this counter moves when that write fails. Degraded rather than fatal, and not `healthy`-affecting: the v4 lease is worth more than the v6 half, so the manager logs and carries on. Reachable at plugin-restart recovery too, which replays the same start. On this branch the v6 client that follows is refused whichever way the write went, so a zero here does **not** mean the network is fine. |
| `lease_changed_v6`, `leases_obtained_v6`, `leases_renewed_v6`, `dhcp_timeouts_v6`, `naks_received_v6` | no | (v1.2.0+) The IPv6-only share of the matching counter above (#212). Each counts only the v6 client's events. On a dual-stack host this isolates the v6-specific NAK/timeout signal the combined number hides. `client_stop_failures_v6` (v1.7.0+, #608) joins the split with the same rule; `ledger_write_failures` has no per-family split. |
| `lease_changed_v4`, `leases_obtained_v4`, `leases_renewed_v4`, `dhcp_timeouts_v4`, `naks_received_v4`, `client_stop_failures_v4` | no | (v1.8.0+, #730) The IPv4-only share, on the same rule. Both halves are now stored and the unsuffixed counter is their **sum** — it is not a counter in its own right and nothing increments it. Before v1.8.0 the v4 share was not stored: it was recovered as `aggregate − *_v6` at render time, which could read one lower than the previous scrape and make Prometheus treat the whole counter as reset. Use `*_v4` rather than doing that subtraction yourself. |

### `/metrics`

*(v1.8.0+)* The same counters as `/Plugin.Health`, in Prometheus text
exposition format. Both views render from one snapshot, so they cannot
disagree, and a unit test asserts by reflection that **every**
`/Plugin.Health` field is exposed here — a counter added later cannot
quietly go missing from your dashboards.

On the plugin socket, always:

```bash
PLUGIN_ID=$(docker plugin inspect -f '{{.Id}}' ghcr.io/claymore666/docker-net-dhcp:v1.9.0)
sudo curl -s --unix-socket /run/docker/plugins/$PLUGIN_ID/net-dhcp.sock \
    http://localhost/metrics
```

Prometheus cannot scrape a UNIX socket, so for an actual scrape target
set `METRICS_ADDR`:

```bash
PLUGIN=ghcr.io/claymore666/docker-net-dhcp:v1.9.0
docker plugin disable "$PLUGIN"
docker plugin set "$PLUGIN" METRICS_ADDR=127.0.0.1:9099
docker plugin enable "$PLUGIN"
```

> **This opens a port in the host's network namespace.** The plugin runs
> with `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE` and
> `"network": {"type": "host"}`, so a listener it opens is on the host
> directly, not in a container namespace. Bind loopback or a management
> interface; do not bind `0.0.0.0` on a machine reachable from anywhere
> you do not control. The listener serves `/metrics` **and nothing
> else** — the libnetwork RPCs are not routed on it, and a test asserts
> that — but an open port is still an open port. It is off by default so
> that enabling it is a decision rather than something inherited from an
> upgrade.

#### Metric names and the `family` label

Counters are `net_dhcp_<name>_total`, gauges are `net_dhcp_<name>`.

Six counters carry a `family` label — `leases_obtained`,
`leases_renewed`, `lease_changed`, `dhcp_timeouts`, `naks_received` and
`client_stop_failures`:

```
net_dhcp_leases_obtained_total{family="ipv4"} 42
net_dhcp_leases_obtained_total{family="ipv6"} 7
```

**Both family series are stored, not derived** (v1.8.0+, #730). Each of
the six has a `_v4` and a `_v6` field in `/Plugin.Health`, and the
unsuffixed counter is their **sum**. So `leases_obtained` equals
`leases_obtained_v4 + leases_obtained_v6`, and the `family="ipv4"`
series is the `_v4` field read straight out. Every other counter has no
family dimension and does not gain an invented one.

Until v1.8.0 only the aggregate and the `_v6` share were stored, and the
v4 series was computed as `total - v6` at render time. Two independently
updated counters combined by subtraction can be read in an order that
yields a value **below the previous scrape**, and Prometheus reads any
counter decrease as a reset — attributing the whole accumulated value as
an increase on the next scrape. A one-off skew of a single event
therefore showed up as a rate spike of the entire count. Adding two
monotonic counters has no such failure mode; subtracting them does.

If you have a dashboard or recording rule that reconstructed the v4
share as `leases_obtained - leases_obtained_v6`, it still gives the same
answer, but prefer `leases_obtained_v4`: the subtraction is what this
change removed, and doing it in the query reintroduces it.

#### Counter resets

`net_dhcp_build_info` carries the plugin's `instance_id` as a label:

```
net_dhcp_build_info{instance_id="..."} 1
```

The counters are process-lifetime and reset when the plugin restarts.
Because the id changes with the process, a restart appears to Prometheus
as a **new series** rather than as a counter that silently rewound —
which `rate()` already handles correctly. This is the one place the
metrics view is strictly better than reading the JSON, where an operator
has to compare `instance_id` by hand to know whether two readings are
comparable at all.

`net_dhcp_healthy` is `1`/`0`, mirroring the `healthy` field, so the one
derived judgement the plugin makes stays alertable.

#### Not exposed

Per-endpoint and per-network labels. Endpoint IDs are unbounded and turn
over with container lifecycle, so labelling by them would be a
cardinality problem in exactly the deployments where these metrics
matter most. Aggregates only.

### Verifying that renewal works

The most common question after a deployment — *is this container's lease
actually being renewed?* — has a non-obvious answer, because **a clean
renewal logs nothing**. It is emitted at `Debug` and the plugin runs at
`info`, so a silent log is correct behaviour, not evidence of a problem.

`leases_renewed` on `/Plugin.Health` is the cheap proof. It should be
non-zero once the first renewal is due, with `naks_received` and
`dhcp_timeouts` still at zero.

*When* it is due comes from the lease, not from any plugin setting: DHCP
option 58 (T1), typically half the lease time.

**In the 2.0 beta there is no lease file to read.** The client is a
goroutine inside the plugin process and keeps its lease in memory, so the
1.x recipe — find the external client process, enter its private mount
namespace and decode its on-disk lease file — has nothing to point at. Nothing on
this branch exports T1 or the granted lifetime to the operator; that is a
gap, named here rather than papered over. What is available:

- **`leases_renewed` on `/Plugin.Health` or `/metrics`**, as above. A rise
  is the renewal; its timing against the bind is the observation.
- **`audit_log=true`** for a durable record rather than a counter — it
  writes a `bound` line and a `renew` line per event, with a timestamp, so
  the interval between two of them measures T1 from the outside.
- **The DHCP server's own lease table**, which is the authority on what it
  granted and is the only place the number itself is readable.

### Plugin log

```bash
sudo cat /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log
```

Raise verbosity with a disable, a set, and an enable — in that order,
because `docker plugin set` is refused while the plugin is running:

```bash
PLUGIN=ghcr.io/claymore666/docker-net-dhcp:v1.9.0
docker plugin disable "$PLUGIN"
docker plugin set "$PLUGIN" LOG_LEVEL=trace
docker plugin enable "$PLUGIN"
```

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

One JSON object per line; kinds `bound`, `renew`, `stopped`,
`stop_failed`. `stopped` means the renewal client was asked to stop and
its goroutine returned; `stop_failed` means it returned an error other
than the cancellation, or did not return inside the finish timeout. Neither says
anything about the lease, because since v1.9.0 the plugin never
releases one — the address is held until it expires (#800). The kinds
were `release` and `release_failed` before that, and were renamed
rather than kept: the ledger must never assert something the server did
not see.

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
    driver: ghcr.io/claymore666/docker-net-dhcp:v1.9.0
    driver_opts:
      mode: macvlan
      parent: eth0
      propagate_dns: 'true'
    ipam:
      driver: 'null'
```

Multi-network containers work (one plugin network per container is
the *supported* shape; multiple attach, but interface naming order is
engine-determined on any engine without moby's remote-driver
`interface_name` pass-through, which is merged upstream but not in a
released engine yet — see the `com.docker.network.endpoint.ifname` row
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
| Container starts and gets an address, but traffic to it is intermittent or lands on the wrong machine | Another device on the segment already holds that address. Usually a **statically configured** host inside the DHCP pool range: it never asks the server for anything, so the server cannot know the address is taken and hands it out | Since the 2.0 beta the plugin detects this itself (RFC 5227) and declines the address, so the first thing to check is whether it is looking: read `acd_probes_sent` on `/Plugin.Health` **before** `address_conflicts`, because with no probes the two readings are identical. Zero probes means either every network on this host runs `-o conflict_check=off` or the check has stopped working; `acd_arp_send_failures` climbing means probes are being refused by the socket, which turns "no conflict found" into "the question was not asked". If the plugin IS finding conflicts, `address_conflicts` moves, `healthy` goes false, and your DHCP server's log shows the DHCPDECLINE with the address on it. The conflict itself is fixed at the DHCP server (reserve or exclude the address), not at the plugin |
| `docker network create -o ipv6=true` fails | The 2.0 beta is IPv4-only | Create the network without `ipv6=true`; DHCPv6 returns in a later beta milestone, and the 1.x line has it today |
| After upgrading onto the beta, `healthy` is false and containers on a network created with `ipv6=true` lose their address when the lease runs out | The stored record still asks for IPv6 and nothing rewrites it. The container **does** start — `Join` answers Docker before the client start it kicked off fails — but the v6 refusal takes the v4 renewal client down with it, so the address is never renewed. `dhcpv6_no_router_advert` moves at `CreateEndpoint`, `join_start_failures` at `Join`, and `recovery_failed` at each plugin restart | Recreate the network without `ipv6=true` — see [DHCPv6](#dhcpv6-ipv6true) |
| `--mac-address` fails on an ipvlan network | ipvlan children share the parent MAC (kernel design) | Use `mode=macvlan`, or drop the custom MAC |
| Reservations don't stick on ipvlan | DHCP server keys on MAC only, ignores option 61 | Use `mode=macvlan`, or configure the server to honor client identifiers |
| One container on two plugin networks fails to start with `cannot program address ... conflicts with existing route` | The two networks lease from **overlapping** subnets, and libnetwork refuses to program a second sandbox address in a subnet the container already routes. Overlapping, not identical: the upstream check is containment in either direction, so `10.0.0.0/8` on one network and `10.1.2.0/24` on another are different subnets and still collide. **Which modes reach this.** In `mode=macvlan` and `mode=ipvlan` nothing stands in the way — two networks on one parent NIC are one LAN with one DHCP server, which is exactly this case. In **bridge mode — the default when `mode` is unset** — two networks on the *same* bridge are refused earlier and with a different message, at `docker network create` (see the `Bridge already in use` row above), so you never get as far as starting a container. If you are seeing *this* error in bridge mode, it is one of two things: the two networks sit on *different* bridges whose subnets overlap (the create-time guard keys on the bridge **name**, not on the subnet), or you set `-o ignore_conflicts=true`, which skips that guard and is what allowed the pair to be created. Measured identical on two daemons differing only in [moby/moby#52866](https://github.com/moby/moby/pull/52866), each with and without the endpoint interface-name option — four cells, one error. That is the sample; it is not a claim about engines nobody has run, and the integration test pins it so a future engine that behaves differently shows up as a failure here rather than as a stale sentence | Not a plugin setting and not fixable here. Put the two networks on **non-overlapping** subnets — different *and* neither one containing the other (different parent NICs / VLANs, each with its own DHCP scope); "different subnets" alone is not enough, since a supernet and a range carved out of it are different and still conflict. Or attach the container to one network only. In bridge mode, if you reached this through `-o ignore_conflicts=true`, drop that option and let the create-time guard refuse the pair up front, where the message names the real problem. Note the container **takes a real lease per network before it fails** — the plugin leases in `CreateEndpoint`, before libnetwork gets as far as refusing — and **nothing releases them**: this plugin never sends a DHCPRELEASE on any path (see [How a lease gets handed back](internals.md#how-a-lease-gets-handed-back) — it does not, #800), so those addresses stay leased until the server expires them. A repeatedly retried start therefore consumes the pool at one address per network per attempt. If addresses are scarce, shorten the lease time on the server or reserve the range, and do not wait for the plugin to hand them back |
| Container can't reach the Docker host (or vice versa) | macvlan/ipvlan kernel rule: children can't talk to the parent NIC's host IP | Bridge mode, or a second NIC — not a plugin setting |
| `healthy: false` on `/Plugin.Health` | Exactly five counters flip it, and they call for different action: `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, `tombstone_quarantines`, `address_conflicts` | Read the five in the field table above to see which one moved — the flag alone does not say. `recovery_failed` / `join_start_failures`: restart the affected containers. `tombstone_write_failures`: check space and writability on the filesystem holding [`STATE_DIR`](#plugin-settings) — the host's `/var/lib/net-dhcp` since v1.5.0, the plugin rootfs before that. `tombstone_quarantines`: read the `tombstones.json.corrupt-<timestamp>` file that was left in `STATE_DIR`, then check the same filesystem — nothing reaps that file, and it is the only record of what was lost. `address_conflicts`: the lease collided with a host already using that address, so the fault is on the DHCP server or the segment, not the plugin. **Doing any of this will not clear the flag** — the counters are monotonic and `healthy` latches for the life of the plugin process. Only restarting the plugin resets it, which tears down every managed endpoint's renewal client on the host, so it is not a step to take just to silence the flag. Compare *instance_id* across reads to tell a still-latched process from a new one that has already gone bad |
| Container came back on a **different IP** after a plugin upgrade | Recreating the network minted a new child MAC; the server keys the old lease to the old MAC and declines the re-request | Expected — see the callout under [Upgrade](#install-upgrade-uninstall). Pin the endpoint MAC and reserve it server-side to make the address survive future upgrades |
| `/Plugin.Health` prints nothing and exits 0 | `curl` run without `sudo`; `/run/docker/plugins` is root-only and `-s` hides the error | Re-run with `sudo` — see [`/Plugin.Health`](#pluginhealth) |
| `leases_renewed` still 0 and the log looks empty | Probably nothing — clean renewals log at `Debug`, and T1 may not have arrived | [Verify renewal properly](#verifying-that-renewal-works): read T1 from the lease, then re-check the counter |
| Compose doesn't attach the container to the DHCP network, with no error | Base/override merge produced a hybrid network definition | [The base/override merge trap](#the-baseoverride-merge-trap) |
| `docker plugin disable` refuses | Networks still reference the plugin | `docker network rm` them first |
| Renewals failing after a server outage | — | Containers keep their address and the client keeps retrying on its retransmission schedule; `dhcp_timeouts` climbs when a retransmission budget runs out — in the 2.0 beta that is within the retry window rather than at the end of the lease — and `leases_renewed` resumes after the server returns |

Operator-side release/publishing issues (registry auth, Hub tokens)
are covered in the maintainer-facing
[`release-runbook.md`](release-runbook.md#troubleshooting).
