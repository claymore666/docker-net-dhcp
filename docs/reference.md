# docker-net-dhcp driver reference

**This is the manual.** Every knob the plugin has, and every behaviour
you can observe, is documented here and only here: installation,
network creation in every mode, all options and settings, lease
behaviour, observability, Compose usage, and troubleshooting.

This file is versioned with the code: the copy in your installed
version's tag is the truth for that version. CI enforces that every
driver-option key the code parses, every health counter the plugin
emits, and every setting the **shipped** plugin accepts appears in this
document, and that none of them is documented a second time somewhere
else
([`scripts/check-option-docs.sh`](https://github.com/claymore666/docker-net-dhcp/blob/main/scripts/check-option-docs.sh),
[`scripts/check-docs-drift.sh`](https://github.com/claymore666/docker-net-dhcp/blob/main/scripts/check-docs-drift.sh)).
The claims made *about* those counters, which five flip `healthy`, are
enforced separately, wherever this page states them
([`scripts/check-health-contract.sh`](https://github.com/claymore666/docker-net-dhcp/blob/main/scripts/check-health-contract.sh)).

**What that enforcement is, and where it stops.** All three gates run in
one direction, code to document: no option, counter or setting can exist
in the plugin without being named here, and none may be documented
twice. Nothing runs the other direction, and nothing reads a sentence
*about* a name. An option documented here that the code does not parse,
a default stated wrongly, or a behaviour described as it used to be, all
go green.
[`scripts/check-health-contract.sh`](https://github.com/claymore666/docker-net-dhcp/blob/main/scripts/check-health-contract.sh)
says as much of itself, calling its tally a receipt and not a proof of
completeness. Those are caught by the documentation review that is a
step of every release ([`docs/release-runbook.md`](release-runbook.md))
and never by a gate.

The one deliberate gap: the coverage-instrumented build used by CI
declares two extra settings that the shipped plugin does not have. They
are exempt from this page by name, because documenting a knob you cannot
set would be worse than omitting it, and are defined in the contributor
documentation instead, which the same gate requires.

The other pages are deliberately narrow:

| page | for | what's there |
| ---- | --- | ------------ |
| [`bridge-mode.md`](bridge-mode.md) | getting started | one-time host-bridge setup, then a worked example |
| [`parent-attached-modes.md`](parent-attached-modes.md) | getting started | choosing macvlan vs ipvlan, quick start, mode-specific constraints |
| [`internals.md`](internals.md) | contributors | how the plugin is built: the mechanism, never the policy |

---

## At a glance

Every setting in one place. Details follow in the sections linked from
each group.

**[Network options](#driver-options-network-level)**, set with `docker
network create -o key=value`, or `driver_opts:` in Compose:

| option | modes | default |
| ------ | ----- | ------- |
| `mode` | all | `bridge` |
| `bridge` | bridge | *(required)* |
| `parent` | macvlan, ipvlan | *(required)* |
| `macvlan_mode` | macvlan | `bridge` |
| `ipvlan_mode` | ipvlan | `l2` |
| `vlan` | macvlan, ipvlan | unset |
| `gateway` | all | from DHCP |
| `ipv6` | all | `false` |
| `ipv6_mode` | all | `off` |
| `ipv6_main_prefix` | all | (first advertised) |
| `ipv6_auto_strict` | all | `false` |
| `lease_timeout` | all | `34s` |
| `conflict_check` | all | `wait` |
| `ignore_conflicts` | bridge | `false` |
| `skip_routes` | all | `false` |
| `propagate_dns` | all | `false` |
| `propagate_mtu` | all | `false` |
| `mtu` | all | unset |
| `client_id` | all | per-endpoint id |
| `vendor_class` | all | `docker-net-dhcp` |
| `validate_dhcp` | macvlan, ipvlan | `false` |
| `dhcp_servers` | all | _(none)_ |
| `dhcp_deny_servers` | all | _(none)_ |
| `register_dns` | all | `false` |
| `audit_log` | all | `false` |
| `release_lease` | all | `never` |
| `host_ifname` | bridge | *(off)* |
| `require_mac` | bridge, macvlan | `false` |
| `link_local_fallback` | bridge, macvlan | `false` |

**[Per-endpoint options](#driver-options-per-endpoint)**, set with
`docker network connect --driver-opt`, or `driver_opts:` under a
service's network attachment:

| option | default |
| ------ | ------- |
| `ip` | from DHCP |
| `com.docker.network.endpoint.ifname` | engine-assigned |

**[Container-level flags](#driver-options-per-endpoint)** the plugin
reads: `--mac-address`, `--hostname`, `--ip6` (no effect today, #960),
and `--ip` on a network that names this plugin as its IPAM driver
([Address allocation](#address-allocation)).

**[Plugin settings](#plugin-settings)**, set with `docker plugin set
<plugin> NAME=value`:

| name | default |
| ---- | ------- |
| `LOG_LEVEL` | `info` |
| `AWAIT_TIMEOUT` | `10s` |
| `STATE_DIR` | `/var/lib/net-dhcp` |
| `METRICS_ADDR` | *(empty)* |
| `DOCKER_HOST` | *(empty)* |

**[Health counters](#pluginhealth)**, read from `/Plugin.Health` on the plugin socket. Five flip `healthy` to `false`: `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, `tombstone_quarantines`, `address_conflicts`. The flag latches for the life of the plugin process; see [`healthy`](#pluginhealth).

---

## Install, upgrade, uninstall

> **⚠️ BREAKING CHANGE IN v1.5.0: DO THIS FIRST ⚠️**
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
- `claymore666/docker-net-dhcp:vX.Y.Z` (Docker Hub mirror, the same
  image at the same digest as `claymore666/net-dhcp`. It carries every
  release from v2.0.0; v2.0.0 and v2.1.0 were copied by hand, and the
  release workflow publishes it from v2.1.1 onward)

Published builds: **`linux/amd64`** on the bare tag and
**`linux/arm64`** as `:vX.Y.Z-arm64` / `:latest-arm64` (v1.7.0 onward).
Those two are the whole set. **32-bit ARM is not built**, so there is no
`armv7` or `armhf` tag to install. The architecture lives in the tag
because a Docker plugin cannot be installed from a multi-architecture
manifest list at all, and `docker plugin install` has no `--platform` to
steer one, and an index fails with `did not find plugin config for
specified reference` for every architecture, including the one you are
on. Substitute the `-arm64` tag in **every** reference below, and not
only in the install line: `docker network create -d` records the tagged
reference as the network's driver, and a bare tag there names a plugin
that was never installed. The daemon-side reason is spelled out under
[Images and releases](index.md#images-and-releases).

**Install** (interactive privilege grant, or `--grant-all-permissions`
for unattended):

```bash
# One-time: the plugin persists lease state here, bind-mounted from
# the host so it survives upgrades (v1.5.0+). Docker will not create it
# for you, and `plugin install` fails with a mount error if it is
# missing. See "If the directory is missing" below.
sudo mkdir -p /var/lib/net-dhcp

# amd64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v2.2.3

# arm64 (v1.7.0 onward). The architecture is in the tag, see below
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v2.2.3-arm64
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
docker plugin enable ghcr.io/claymore666/docker-net-dhcp:v2.2.3
```

On arm64 that second line takes the `-arm64` tag, like every other
reference on this page. The bare one names a plugin the host never
installed.

Nothing is lost or corrupted by the failed install. (Behaviour verified
against Docker 26.1.5, #494.)

Privileges requested: `network: host`, host PID namespace, the Docker
socket mount, a bind mount of `STATE_DIR` (v1.5.0+, see below), a
**read-only** bind mount of `/var/run/docker` (v1.6.0+), `CAP_NET_ADMIN`
+ `CAP_SYS_ADMIN` + `CAP_SYS_PTRACE` (v1.3.3+) and `CAP_NET_RAW`
(v2.0.0+). All are inherent to what the plugin does: creating links in
arbitrary netns, driving DHCP on the host's L2 segments, and querying
the daemon. **SECURITY.md carries one sentence per grant, naming what in
the tree consumes it**, and a CI gate fails when that list and
[`config.json`](https://github.com/claymore666/docker-net-dhcp/blob/main/config.json)
disagree in either direction.

`CAP_SYS_PTRACE` and the host PID namespace are the pair worth reading
carefully. From 2.0 the plugin ASKS FIRST for the sandbox key the daemon
publishes under `/var/run/docker/netns/`. Whether that key resolves for
an attach is a property of your host, and the plugin reports it as
`sandbox_netns_propagation`. The read-only `/var/run/docker` mount is a
bind taken when the plugin starts. On a host where the mount the daemon
publishes on is linked to it, the per-sandbox namespace mounts made
afterwards arrive, the key resolves and `sandbox_key_entries` rises once
per attach. On a host where that mount is private they do not arrive,
the key resolves to the ordinary file underneath, and the plugin checks
what it opened, counts the refusal (`sandbox_key_entry_failures`, and
specifically `sandbox_key_not_a_namespace`, which is the arm that says
the entry was the placeholder file and not a key shape this plugin
declined), and enters through `/proc/<pid>/ns/net`
(`sandbox_pid_fallbacks`). Both readings are ordinary and neither needs
action; the accompanying log line is at `debug` for that reason, and the
counters are what an operator reads. Recovery after a plugin restart
takes the key route on either host, because the sandbox is then older
than the plugin process and inside the bind snapshot. Neither grant
could be dropped in any case: `resolv.conf` propagation enters the container's
**mount** namespace by PID and there is no sandbox key for a mount
namespace at all, and it is those `/proc/<pid>/ns/*` opens the kernel
ptrace-gates when the container runs as a non-root user (#317).

**Upgrading onto 2.0 re-prompts for privileges**, because `CAP_NET_RAW`
is a new line in the manifest. It is not new power: the capability is in
the OCI default set that Docker composes on top of the requested one, so
the process always had it. The DHCP exchange runs on an interface with
no address yet, which needs a raw socket, and there is no configuration
in which the plugin works without it. Expect the prompt once, on the
upgrade; if you get one you were not expecting, that is worth
investigating instead of approving.

The `/var/run/docker` mount lets the plugin list the daemon's sandbox
netns entries, so "the container went away mid-attach" is reported as
that instead of as a generic failure (`sandbox_netns_visible`). It is
the parent of `/var/run/docker/netns` and not that directory itself,
because the daemon does not create the latter until the first container
sandbox, and mounting it directly made `plugin install` fail on a host
that had never run one (#588).

**Verify the signature (v1.1.0+).** The published image is cosign-signed
(keyless) and carries SLSA build provenance; release artifacts ship a
cosign-signed `checksums.txt` and an SBOM. Per-release, copy-pasteable
verification commands live in
[Verifying releases](verifying-releases.md), which every
[GitHub Release](https://github.com/claymore666/docker-net-dhcp/releases)
links to; the [home page](index.md#verifying-releases) has the short form.

**Pin a version.** `:latest` exists and tracks the newest release, but
networks remember the exact driver string they were created with, so a
network created against `:v1.1.1` needs that tag present to operate.
Pinning makes upgrades a deliberate step instead of a pull-side
surprise.

**Upgrade.** Networks reference the plugin tag they were created with,
so the safe sequence for moving from `vOLD` to `vNEW` is:

```bash
# 1. Stop containers using plugin networks
# 2. Remove the networks (they're cheap to recreate; the addresses stay
#    leased until they expire unless the network set release_lease,
#    which is off by default. release_lease=on_stop hands them back at
#    step 1; release_lease=on_remove hands back whatever is still held
#    when this line runs, #800/#962/#984)
docker network rm my-dhcp-net
# 3. Swap the plugin (STATE_DIR on the host is left alone, so the
#    tombstone and audit ledger carry across; v1.5.0+)
docker plugin disable ghcr.io/claymore666/docker-net-dhcp:vOLD
docker plugin rm ghcr.io/claymore666/docker-net-dhcp:vOLD
# Upgrading ONTO v1.5.0 or later from an earlier version: create the
# bind source first. v1.5.0 is the release whose manifest started
# mounting STATE_DIR from the host, and Docker will not create a
# missing bind source, so the install below fails at start-up and leaves the
# plugin disabled. vOLD is already gone at this point, so the host has
# no working driver until you `docker plugin enable` the new one. See
# "If the directory is missing" above. Harmless to repeat later.
sudo mkdir -p /var/lib/net-dhcp
docker plugin install ghcr.io/claymore666/docker-net-dhcp:vNEW
# 4. Recreate networks against vNEW, restart containers
```

A 2.0 plugin takes an exclusive lock on the lease record in `STATE_DIR`, so a
second 2.0 tag sharing that directory cannot be enabled while the first one
is enabled. `docker plugin enable` fails, the CLI prints only a socket error,
and the daemon log carries the reason: `another tag of this plugin is enabled
and holds the lease record /var/lib/net-dhcp/lease-records.jsonl; disable it
before enabling this one`. Disable the old tag, then enable the new one.

The lock is advisory and is taken on a file beside the record, so it needs a
filesystem that implements file locking. If the host directory behind
`STATE_DIR` sits on a mount with no working locks, NFS without lockd for
example, taking the lock fails and a single plugin does not start. That case
has its own line in the daemon log, `the filesystem under
/var/lib/net-dhcp/lease-records.jsonl does not support locks; the plugin
refuses to start rather than risk two writers`. Disabling tags does not clear
it. Back `/var/lib/net-dhcp` on the host with a filesystem that implements
file locking. Do not repoint `STATE_DIR`: the bind source is fixed at that
path, so any other value puts the state back inside the plugin rootfs, where
the next upgrade wipes it. The `STATE_DIR` row under
[Plugin settings](#plugin-settings) states the same rule.

An errno the plugin does not recognise keeps the older wording, `the lease
record file is already open by another writer`, with the errno printed after
it. A plugin that cannot create the lock file at all never reaches any of the
three: the log says `lock file for` and then the path and the reason, for
example `permission denied` on a state directory the plugin may not write.
Check the owner and the mode of the host directory in that case.

(`docker plugin upgrade` exists but in-place upgrades while networks
exist risk a driver-reference mismatch; the remove/recreate path is
the supported one.)

> **Upgrading onto 2.0 prompts once for privileges, at step 3's
> `docker plugin install`.** The manifest requests `CAP_NET_RAW` in
> addition to the three capabilities v1.9.0 requested; nothing was
> dropped, and the effective set is unchanged (the capability was
> already in the OCI default set). An unattended run that does not pass
> `--grant-all-permissions` stops at the prompt with the host still
> without a driver, because `vOLD` was removed two lines earlier. The
> field-by-field delta is in [`RELEASE_NOTES.md`](https://github.com/claymore666/docker-net-dhcp/blob/main/RELEASE_NOTES.md); if you are prompted on
> an upgrade that is *not* this one, that is worth investigating instead
> of approving.

> **Expect the container's IP to change on `macvlan` and `ipvlan`.**
> Recreating the network builds a **new** child interface with a fresh
> kernel-generated MAC. DHCP servers key leases and reservations on MAC,
> so the previous address does not follow, and re-requesting it with
> `--driver-opt ip=<old address>` is *declined* while the server still
> holds that address against the old MAC. The container comes back on a
> different address.
>
> **v1.5.0+ removes one of the two causes.** `STATE_DIR` is now bind-
> mounted from the host, so the tombstone that remembers an endpoint's
> MAC and IP survives `docker plugin rm`. Before v1.5.0 it lived inside
> the plugin rootfs and every upgrade destroyed it, which is a separate
> loss from the network-removal one described above. Removing the
> *network* still discards the tombstone, which is keyed by network ID
> by construction, so the address only survives an upgrade in which the
> network itself is left in place.
>
> This is not the same as a container restart, which *does* preserve the
> address; see [Restart stability](#restart-stability-mac-and-ip). That
> mechanism is keyed by network ID, so removing the network loses it by
> construction.
>
> To keep an address across upgrades, give the endpoint a fixed MAC
> (`--mac-address` / Compose `mac_address`, since an explicit MAC takes
> priority over everything else) and reserve **that MAC** on the DHCP
> server. Note that `docker network connect` has no `--mac-address`
> flag, so the MAC has to come from the container definition: an
> already-running container needs recreating once, after which the
> address is stable across every future upgrade. A network created with
> `-o require_mac=true` refuses a container or a connect without one, so
> a missing MAC shows up as an error at start (#1036). After a refused
> connect, the container's next restart, manual or by its restart
> policy, fails the same way and leaves it stopped: then run
> `docker network disconnect` for that network and `docker start` it.

**Uninstall:**

```bash
docker plugin disable ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z
docker plugin rm ghcr.io/claymore666/docker-net-dhcp:vX.Y.Z
```

`disable` fails while networks still use the plugin. Remove those first
(`docker network ls`, `docker network rm ...`).

---

## Creating networks

All modes share two invariants:

- **Docker's built-in IPAM is never the allocator.** The LAN's DHCP
  server is the source of address truth, and Docker's default IPAM would
  allocate from a subnet of its choosing and collide with the LAN. There are two
  supported ways to say so: `--ipam-driver null`, which is what every
  example below uses, what 1.x and 2.0 shipped, and what all three modes
  take, and `--ipam-driver <this plugin>` (v2.1.0+, #110), which puts the
  leased address in Docker's own address management and covers `bridge`
  and `macvlan` for IPv4. `ipvlan` is refused in that shape (#949) and so
  is IPv6 (#960). See [Address allocation](#address-allocation).
- One DHCP-served network per container is the supported shape.

### bridge (default)

You bring an existing Linux bridge that is L2-connected to the LAN
(see [`bridge-mode.md`](bridge-mode.md) for the bridge setup itself):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.2.3 \
    --ipam-driver null \
    -o bridge=my-bridge \
    my-dhcp-net
```

### macvlan

No host changes are needed. Containers get per-container
kernel-generated MACs as macvlan children of a host NIC
(`macvlan_mode=passthru` is the exception, see
[sub-modes](#macvlan-and-ipvlan-sub-modes)):

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.2.3 \
    --ipam-driver null \
    -o mode=macvlan -o parent=eth0 \
    lan-dhcp
```

### ipvlan

Like macvlan, but children share the parent NIC's MAC, for switches or
hypervisors that refuse multiple MACs per port (sticky-MAC port
security, hostile vSwitches, some Wi-Fi APs). The DHCP server must key
reservations on DHCP option 61 (client identifier) and never on MAC:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.2.3 \
    --ipam-driver null \
    -o mode=ipvlan -o parent=eth0 \
    lan-dhcp
```

Mode-specific constraints (MAC behaviour, parent-NIC rules, kernel
limitations) are catalogued in
[`parent-attached-modes.md`](parent-attached-modes.md#constraints).

### macvlan and ipvlan sub-modes

Since v2.3.0, `-o macvlan_mode=` and `-o ipvlan_mode=` choose the kernel
mode of each container's link
([#905](https://github.com/claymore666/docker-net-dhcp/issues/905)).
Left out, a macvlan link is `bridge` and an ipvlan link is `l2`, as in
every earlier release. `ip -d link show eth0` inside the container shows
the mode, and a plugin restart rebuilds each link in the mode its network
was created with.

| option | value | DHCP | what the mode changes |
| ------ | ----- | ---- | --------------------- |
| `macvlan_mode` | `bridge` (default) | leases | containers on one parent reach each other inside the host |
| `macvlan_mode` | `vepa` | leases | every frame leaves through the parent, container to container too; two containers on one parent reach each other only through a switch port that sends frames back where they came from (hairpin, 802.1Qbg reflective relay) |
| `macvlan_mode` | `private` | leases | containers on one parent never reach each other, not even through such a switch |
| `macvlan_mode` | `passthru` | leases, one container per parent | the container takes the parent itself; see below |
| `ipvlan_mode` | `l2` (default) | leases | |
| `ipvlan_mode` | `l3`, `l3s` | refused | see below |

Every value that leases was measured leasing from dnsmasq on Linux 6.12,
and the integration suite leases in each of them. Any other value, or
`macvlan_mode` on an ipvlan network, `ipvlan_mode` on a macvlan one, or
either on a bridge network, is refused at `docker network create` with
the accepted values in the error. `validate_dhcp` probes with the
network's sub-mode.

**ipvlan `l3` and `l3s` are refused.** An ipvlan child in `l3` or `l3s`
mode sends no broadcast out of its parent, so its DHCPDISCOVER reaches no
DHCP server and no relay. Measured on Linux 6.12.107 on 2026-09-24: a
dnsmasq on the segment logged no DISCOVER from an `l3` or `l3s` child,
and with dnsmasq bound to the parent itself, a capture on the parent saw
no packet at all. No relay or server placement helps, so
`docker network create` refuses both values with that reason. The option
keeps its name, and `l2` is the one accepted value.

**The kernel keeps one ipvlan mode per parent.** A new ipvlan child in
another mode switches every child on that parent to its mode, including
the running containers of other networks (measured on the same kernel).
The plugin refuses an ipvlan network on a parent that already carries an
ipvlan network in another mode, its own or one of Docker's `ipvlan`
driver, and names that network in the error. It cannot refuse the
reverse order: a Docker `ipvlan` network in `l3` created later on a
parent this plugin uses is not the plugin's to stop. Keep `l3` ipvlan
networks off the parents this plugin uses.

**`macvlan_mode=passthru` gives the parent to one container.** Measured
on Linux 6.12:

- The container's link wears the parent's MAC, so the DHCP server sees
  the parent's MAC for that container. The plugin sets the link's MAC to
  that same value, which leaves the parent's MAC as it is and keeps
  udev from rewriting it.
- While the container runs, the host loses the parent: the host's own
  address on it stops answering from the network, and the parent is put
  in promiscuous mode. The address answers again once the container
  stops. Use a NIC the host does not need.
- A second container on the network is refused. The kernel answers
  `invalid argument`, and the plugin's error explains it:
  `failed to create macvlan link on "eth0": invalid argument. The kernel
  answers this when a macvlan_mode=passthru child holds the parent: a
  passthru network gives its parent to one container, so a second child
  is refused beside it, ...`. Stop the container that holds the parent,
  or put the network on another parent.
- A passthru network and any other macvlan network, of this plugin or of
  Docker's `macvlan` driver, cannot share a parent; the second one is
  refused at `docker network create`, naming the first.
- `--mac-address` is refused, because a MAC set on the passthru link
  changes the parent's own MAC. For the same reason `require_mac=true`
  and this plugin's IPAM mode (which gives every endpoint a MAC) are
  refused with `passthru` at `docker network create`; use
  `--ipam-driver null`.
- On `docker restart` the old link can still hold the parent for a
  moment while its namespace goes away, and the kernel refuses the new
  one `invalid argument` in that window. The plugin retries for up to
  3 seconds before it reports the error.

### VLAN sub-interfaces (`vlan`)

Since v2.3.0, `-o vlan=<id>` puts a macvlan or ipvlan network on an
802.1Q VLAN of its parent
([#902](https://github.com/claymore666/docker-net-dhcp/issues/902)).
The children attach to the sub-interface `<parent>.<id>`, the name
Docker's own `macvlan` driver uses, so their DHCP traffic and everything
after it leaves the parent tagged:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.2.3 \
    --ipam-driver null \
    -o mode=macvlan -o parent=eth0 -o vlan=100 \
    lan-vlan100
```

**Ownership.** When `eth0.100` is missing, the plugin creates it, sets
it up and marks it with the interface alias `docker-net-dhcp`;
`ip -d link show eth0.100` shows `vlan protocol 802.1Q id 100` and
`alias docker-net-dhcp`. Before it comes up, the plugin turns IPv6 off
on it for the host (`disable_ipv6=1`), so the host takes no link-local
address, no SLAAC address and no default route from the VLAN's router,
and services the host binds to `[::]` are not reachable from that VLAN.
The containers on it keep their own IPv6. Measured on Linux 6.12 with
macvlan and ipvlan children. A link of that name that already exists is used
as it is when it is an 802.1Q VLAN on that parent with that ID: never
marked, so the plugin never removes a sub-interface it did not create,
and never set up, so like any parent it must be up, and a down one is
refused as `parent interface is down: eth0.100`. Removing the mark by hand makes the link yours in the same way.

**Sharing and removal.** Networks on the same parent and ID share the
sub-interface. Deleting a network removes a marked sub-interface only
when all of these hold: no other network uses it, of this plugin or of
Docker's `macvlan` or `ipvlan` driver with `-o parent=eth0.100`; no
other link sits on it on the host; and the kernel shows it free. For
the last check the plugin adds and removes one trial macvlan and one
trial ipvlan link on it: the kernel refuses one of the two while a
macvlan, ipvlan or macvtap link, or a bridge, holds it in any network
namespace, a container's included, which no host link list shows.
Otherwise the link stays and the plugin log says why. One case is not
seen: a VLAN stacked on the sub-interface and then moved into another
namespace holds nothing the trial meets, and removing the sub-interface
deletes that link too. Measured on Linux 6.12, 2026-09-24.

**Restart and reboot.** A restarted plugin knows its sub-interfaces by
the mark and removes them with their last network as before. A
container start re-creates a missing sub-interface, after a host reboot
or a manual `ip link del`: while a network with `vlan` exists, it owns
that name.

**Refused at `docker network create`:**

- `vlan` on a bridge network: `vlan cannot be set in mode=bridge`.
- A value that is not a decimal from 1 to 4094 written without leading
  zeros: `vlan "4095" is not a VLAN ID from 1 to 4094`. 0 and 4095 are
  reserved by 802.1Q.
- A sub-interface name over 15 bytes, the kernel's limit for any
  interface name: with `-o parent=enp129s0f0np0` and `-o vlan=100`,
  `enp129s0f0np0.100` is 17 bytes and is refused as `at most 15 bytes`.
  Use a shorter parent name, for example through a `.link` file.
- An existing link of that name that is not an 802.1Q VLAN, or is one on
  another parent, with another ID, or with protocol 802.1ad. The error
  names the link and what it is.
- A VLAN with that ID already on that parent under another name, for
  example `vlan100` from netplan or systemd-networkd. The kernel takes
  one per parent and ID; the error names the link, and `-o parent=vlan100`
  without `vlan` uses it.
- `release_lease=on_stop` or `on_remove` when the plugin makes the
  sub-interface, or made it for another network: the release is sent
  from the host's address on that link, and the host has none there,
  no IPv4 and, as above, no IPv6. Create `eth0.100` yourself with a
  host address and the plugin uses it, with `release_lease` accepted.

The MTU check of the `mtu` option runs against the sub-interface, and
`validate_dhcp` probes through it. In this plugin's IPAM mode the pool
belongs to the sub-interface: a second network with the same subnet
names it as `--ipam-opt parent=eth0.100`, and
`--ipam-opt parent=eth0` on a `vlan` network is refused as naming
another interface. Docker's own `macvlan` driver deletes a
sub-interface it created itself when its network goes, even while a
network of this plugin uses it.

### Address allocation

Every example above passes `--ipam-driver null`. Since v2.1.0 the plugin
also serves an IPAM driver of its own (#110), and the line names the
plugin twice:

```bash
docker network create -d ghcr.io/claymore666/docker-net-dhcp:v2.2.3 \
    --ipam-driver ghcr.io/claymore666/docker-net-dhcp:v2.2.3 \
    -o mode=macvlan -o parent=eth0 \
    lan-dhcp
```

Nothing else changes. Compose still says `networks: [lan-dhcp]`, and a
Compose-managed network writes `ipam: driver: <plugin>` where it wrote
`driver: null`.

**What the two shapes differ in.** In both, the address comes from the
LAN's DHCP server and `docker inspect` reports it. `--ipam-driver null`
tells Docker to allocate nothing, so the daemon has no pool for the
network: `docker run --ip`, `docker network connect --ip` and Compose's
`ipv4_address` are all refused before the plugin sees them, and
`docker network inspect` shows an empty IPAM block. With this plugin as
the IPAM driver the address is Docker's to hand out, so all three work,
and the IPAM block shows the subnet and the addresses in use.

**`--subnet` is optional and changes two things.** Without it the driver
answers the pool `0.0.0.0/0`, which is what `docker network inspect`
then shows. With it, `docker network inspect` shows the subnet you
typed, and the driver refuses any lease from outside it: a DHCP server
handing out an address outside the subnet you typed would put a
container in Docker's own records outside its network's pool, and
`docker run` fails instead.

**The gateway record on engines below 28.** Docker 28 asks the plugin
whether a network needs a gateway address of its own, and the answer is
no. Older engines do not ask. They request one from the IPAM driver at
`docker network create`, and a driver that refuses fails the create with
`failed to allocate gateway ()`. The driver answers with the pool's own
network address, which is not a host address and so is not one a DHCP
server can lease, so on those engines a network created without
`--subnet` shows `Gateway 0.0.0.0` in `docker network inspect`. It is a record in Docker's store and not a
route: the gateway a container uses is the one the DHCP server names,
and it reaches the container at endpoint creation on every engine. A
`--gateway` you type yourself is kept unchanged on all of them.

Two subnets have no such address. In a `/31` both addresses are host
addresses and in a `/32` the single address is one, so on an engine
below 28 `--subnet 192.168.99.4/31` is refused at create with a message
naming the two ways through: type `--gateway <address>` yourself, which
is passed to Docker unchanged and leaves nothing to invent, or use a
shorter prefix. Docker 28 and later does not ask, so it creates those
networks with neither.

**Keep `docker plugin enable --timeout` at its 30s default.** In this
shape the address is acquired inside the daemon's IPAM call, and the
plugin sizes that work to the default budget: one reservation gets 26s
of it, which leaves room for a DHCP exchange plus the RFC 5227 probe
that runs before the address is used. The plugin is not told what value
you enabled it with and cannot follow it, so a **lower** `--timeout`
makes every address request on such a network fail, because the daemon
stops listening mid-reservation and re-sends a call whose body it has
already spent, which the plugin refuses with a message naming this flag.
A **higher** one is unused. `--ipam-driver null` networks are unaffected;
they acquire during endpoint creation instead.

**`--ipam-opt parent=<nic>` or `--ipam-opt bridge=<name>`** is needed
only for a **second** network in this shape with the same subnet on a
different parent. Two such networks otherwise derive the same pool
identity, and the second `docker network create` is refused, naming this
option. One network needs neither key. On a network with `-o vlan=<id>`
the interface is the sub-interface, `--ipam-opt parent=<parent>.<id>`
(#902).

The two keys are exclusive. A pool names one interface, so giving both
is refused at `docker network create`. The pool is requested before this
network's own `-o` options reach the driver, so the refusal cannot tell
which of the two your network is and does not pretend to: it names both,
`-o bridge=` on a bridge network, `-o parent=` on a macvlan or ipvlan
one, and you keep the one your mode owns (#1010). Before that refusal
the second key was accepted and dropped, and the pool identity was built
from the first key alone.

**Not supported.** `ipvlan` networks cannot use this plugin as their
IPAM driver: Docker generates a MAC per endpoint for an IPAM driver that
asks for one, and ipvlan children share the parent's MAC and refuse a
supplied one. The network create is refused, and `--ipam-driver null` is
unchanged and supported for ipvlan (#949).

**IPv6 is IPv4-only in this shape, and the combination is refused.** The
plugin allocates no IPv6 pool, so Docker's `--ipv6` is refused; and so is
every option that switches IPv6 on for the network, which is
`-o ipv6=true` and `-o ipv6_mode=` with any mode but `off`, because the
IPAM endpoint path runs no DHCPv6 exchange at all. A container on such a
network would get no IPv6 address from the plugin, and the identity the
v6 client falls back to at join time is derived from the endpoint MAC,
which Docker regenerates at every restart in this shape. The refusals
name #960, and the one at `docker network create` names the mode the
network was set to. On an `--ipam-driver null` network every `ipv6_mode`
is unchanged and supported.

**`link_local_fallback` is refused in this shape.** Docker takes the
address from the IPAM driver before the endpoint exists and never learns
of a later change, so a container that fell back to 169.254/16 would keep
that address in Docker's records after it moved to a lease. The refusal
comes at `docker network create`; on an `--ipam-driver null` network the
option is supported (#904).

**A network's IPAM driver is fixed when it is created.** Upgrading the
plugin never moves an existing `--ipam-driver null` network into the new
shape, and switching shapes is a `docker network rm` and a create.

**Going back to v2.0.0 or earlier strands an IPAM-mode network.** Those
builds do not offer an IPAM driver at all, so the daemon cannot find the
one the network names: `docker run` on it fails with the daemon's own
"IPAM driver not found" error, at once and for every container, and it
says nothing about this plugin. `docker network rm` still works, and
re-creating the network with `--ipam-driver null` is the way back.
Existing `--ipam-driver null` networks are unaffected by the downgrade.
Nothing on disk is damaged either way, because the plugin refuses to
read a state file newer than it understands instead of rewriting it. The
file is not what protects you here; the missing driver is, so plan a
rollback around removing the IPAM-mode networks first.

---

## Driver options (network-level)

Passed as `-o key=value` on `docker network create`, or under
`driver_opts:` in Compose. Booleans take `'true'` / `'false'`
(quote them in YAML).

**An option written with no value at all is an option you did not set.**
`-o lease_timeout=`, and `driver_opts: {lease_timeout: "${VAR}"}` in
Compose with `VAR` unset, are read exactly as leaving the option out:
the default in the table applies. Since v2.2.0 this holds for every
option, `lease_timeout` included, which before then refused an empty
value as an invalid duration.

| option | modes | default | since | description |
| ------ | ----- | ------- | ----- | ----------- |
| `mode` | n/a | `bridge` | macvlan v0.2.0, ipvlan v0.4.0 | Attachment strategy: `bridge`, `macvlan`, or `ipvlan`. `macvlan_mode` and `ipvlan_mode` choose the kernel mode of the link. |
| `bridge` | bridge | *(required)* | upstream | Existing Linux bridge to plug container veths into. |
| `parent` | macvlan, ipvlan | *(required)* | v0.2.0 | Host NIC to attach children to (e.g. `eth0`, `ens18`). Must exist and be administratively `UP`. |
| `macvlan_mode` | macvlan | `bridge` | **v2.3.0** | Kernel mode of each container's macvlan link: `bridge`, `vepa`, `private` or `passthru` (#905). Each of them leases. `passthru` gives the parent to one container, wears the parent's MAC, and is refused with `--mac-address`, `require_mac=true`, this plugin's IPAM mode, and another macvlan network on the same parent. Any other value, and the option on a network that is not macvlan, is refused at `docker network create`. See [macvlan and ipvlan sub-modes](#macvlan-and-ipvlan-sub-modes). |
| `ipvlan_mode` | ipvlan | `l2` | **v2.3.0** | Kernel mode of each container's ipvlan link (#905). `l2` is the one accepted value. `l3` and `l3s` are refused at `docker network create`: such a child sends no broadcast, so no DHCP server or relay hears its DHCPDISCOVER (measured on Linux 6.12.107, 2026-09-24). The kernel keeps one ipvlan mode per parent, so an ipvlan network is refused on a parent that another ipvlan network uses in another mode. The option on a network that is not ipvlan is refused. See [macvlan and ipvlan sub-modes](#macvlan-and-ipvlan-sub-modes). |
| `vlan` | macvlan, ipvlan | unset | **v2.3.0** | 802.1Q VLAN ID from 1 to 4094 (#902). Children attach to the sub-interface `<parent>.<id>`, which the plugin creates when it is missing and removes with the last network that uses it. Refused in bridge mode, for any other value, and when `<parent>.<id>` is over the kernel's 15 bytes. See [VLAN sub-interfaces](#vlan-sub-interfaces-vlan). |
| `gateway` | all | from DHCP | v0.3.0 | Override the IPv4 default gateway returned by the DHCP server, for split-horizon LANs where containers should egress via a different router (e.g. a VPN gateway). |
| `ipv6` | all | `false` | upstream | Lease a DHCPv6 address for every endpoint on this network, alongside its DHCPv4 one. Docker reports it as `GlobalIPv6Address`. The address is installed as a `/128` with the server's preferred and valid lifetimes, its DUID and IAID persist across restarts, and the container's link is configured to process Router Advertisements, which is what supplies the route, since DHCPv6 carries no next hop. On a segment that advertises no DHCPv6 address the endpoint still starts, without one; on a segment that advertises one and then answers nothing it fails. See [DHCPv6](#dhcpv6-ipv6true), and the ipvlan upgrade note there if you are moving from 1.x.<br><br>**Since v2.2.0 this option is the short spelling of `ipv6_mode=dhcp`** and keeps exactly that meaning. `ipv6=true` with `ipv6_mode=off` is refused at `docker network create`: the two say opposite things about the same endpoint and there is no reading of the pair that is not a guess. `ipv6=false` written out beside `ipv6_mode=dhcp`, `slaac` or `auto` is refused for the same reason. Setting `ipv6_mode` alone switches IPv6 on, so a network states it once. |
| `ipv6_mode` | all | `off` | **v2.2.0** | Where an endpoint's IPv6 address comes from. `off` (the default) is no IPv6 from this plugin. `dhcp` leases it over DHCPv6, which is what `ipv6=true` has always meant and still means. `slaac` forms it from a router advertisement's autonomous prefix (RFC 4862 §5.5.3) and sends no Solicit. `auto` reads the router advertisement and does what it says: the managed-address flag means DHCPv6 (RFC 4861 §4.2, "When set, it indicates that addresses are available via Dynamic Host Configuration Protocol"), and a clear flag means the prefix. Any other value is refused at `docker network create` with the accepted set in the message, rather than resolved to a default, because the default is `off` and a typo would silently switch IPv6 off on a network created to have it.<br><br>**`auto` on a link whose router says DHCPv6 and whose server then says nothing** falls back to the advertised prefix after half the router-discovery window, counts it in `dhcpv6_auto_fallbacks` and logs the fallback (the address itself is subject to the boundary at the end of this row). **`-o ipv6_auto_strict=true` turns that off** and fails the endpoint instead; the row below is that option.<br><br>**`slaac` and `auto` are refused in `mode=ipvlan`.** An ipvlan L2 slave inherits the parent link's MAC, an address formed from an advertisement is derived from that MAC (RFC 4291 appendix A), and RFC 4862 gives a node with a fixed interface identifier no retry after duplicate address detection fails, so every container on such a network would form one address and the second one onwards would sit in a conflict it cannot recover from. Use `ipv6_mode=dhcp` on ipvlan, which gives each endpoint its own DUID.<br><br>**In `slaac` a stored preferred address is not asked for.** `-o ipv6=...`'s per-endpoint preferred address (`preferred_ipv6`) is still validated and still refused if malformed, and the plugin logs the address it is not requesting; there is no server to ask, because the address comes from the prefix.<br><br>**In `slaac` and `auto` the plugin installs every address the advertisement forms, on the container's link.** RFC 4862 §5.5.3 forms one address per advertised autonomous prefix, so a link advertising a unique-local prefix and a global one gives the container two, and the client caps an endpoint at eight. Each address is installed with its own preferred and valid lifetimes, taken from the Prefix Information option that formed it and refreshed by every later advertisement (RFC 4861 §6.2.1). An address whose preferred lifetime runs out is left on the link and marked deprecated (`preferred_lft 0`, RFC 4862 §5.5.4: "SHOULD continue to be used as a source address in existing communications, but SHOULD NOT be used to initiate new communications"); one whose valid lifetime runs out, or whose prefix the router stops advertising, is removed. `ip -6 addr show` inside the container is where an operator reads all of this. **The lifetimes arrive a moment after the container does.** The engine installs the address `CreateEndpoint` reported while it builds the container's sandbox, and it has no lifetimes to install, so that address is on the link as `valid_lft forever preferred_lft forever` until the endpoint's own client binds and the plugin applies the advertised numbers. `ipv6_slaac_addresses` moves on that second install, which is what makes the two visible apart. Counters: `ipv6_slaac_addresses`, `ipv6_addresses_withdrawn`, `ipv6_slaac_prefixes_ignored`.<br><br>**Docker is told one address, and `ipv6_main_prefix` chooses which.** `CreateEndpoint` returns a single `AddressIPv6` and the engine has no way to change it afterwards, so `docker inspect` and the engine's own records show the first advertised prefix's address by default, whatever else the container holds. The row below is the option that names another. The addresses the container actually has are the whole set either way. [#818](https://github.com/claymore666/docker-net-dhcp/issues/818), [#819](https://github.com/claymore666/docker-net-dhcp/issues/819) and [#808](https://github.com/claymore666/docker-net-dhcp/issues/808) are the three issues this answers. |
| `ipv6_main_prefix` | all | (first advertised) | **v2.2.0** | Which of an endpoint's IPv6 addresses is the one Docker is told about. A prefix in CIDR form with no host bits set, for example `2001:db8:1::/64`. The endpoint's address inside that prefix becomes the `AddressIPv6` `docker inspect` shows and the one the engine records; the container keeps every address the advertisement formed regardless. Use it on a link that advertises both a unique-local and a global prefix, where the address other hosts reach the container on is the global one and the plugin would otherwise report whichever prefix the router lists first. If no address falls inside the named prefix, the first advertised one is reported instead, `ipv6_main_prefix_unmatched` moves once for the endpoint, and the log line names both prefixes; the endpoint is not failed, because the container's addresses are correct and only the name Docker shows is not the one asked for. **Accepted only where the addresses are formed from advertisements**, `ipv6_mode=slaac` and `ipv6_mode=auto`. On `ipv6_mode=dhcp` there is one server-granted address and the option could only ever do nothing, so it is refused at `docker network create` rather than accepted and ignored. |
| `ipv6_auto_strict` | all | `false` | **v2.2.0** | What `ipv6_mode=auto` does when the router advertises the managed-address flag and no DHCPv6 server answers. Default `false`: after half the router-discovery window the client forms an address from an autonomous prefix instead, counts it, and logs a line naming the fallback. `true`: no fallback, and the endpoint fails when the server stays silent, which is what you want on a segment where the DHCPv6 address is the one your firewall rules name. It has no effect in any other `ipv6_mode`, because no other mode can choose between the two sources. |
| `lease_timeout` | all | `34s` | upstream; default **derived** since v2.0.0 | Budget for the up-front DHCP exchange at container creation. It is a deadline over one acquisition, and what happens inside it is RFC 2131 §4.1's retransmission schedule, which the plugin sets explicitly. **4s, 8s, 16s, 32s and a 64s ceiling are intervals and never elapsed times**: the first DISCOVER goes out immediately and arms a 4s timer, each retransmission arms the next interval as it goes out, and every interval carries ±1s of uniform jitter. The retransmissions therefore land at roughly **4s, 12s, 28s and 60s** after the first packet, matching the RFC's own worked example, "four times, for a total delay of 60 seconds", and after the fourth the exchange is abandoned and restarted from DISCOVER. Permanent failures, such as a missing interface or a malformed option, still fail immediately instead of waiting it out.<br><br>**The default is 34s, and it is computed, never written down.** In the default `conflict_check=wait` the acquisition is not finished at the DHCPACK: RFC 5227 §2.1's check runs before the address is used, and costs up to 7.0s (PROBE\\_WAIT 1s + two intervals of up to PROBE\\_MAX 2s + ANNOUNCE\\_WAIT 2s). One DISCOVER retransmission is 4s ±1s. One acquisition is therefore 5.0 + 7.0 = **12.0s**.<br><br>A budget of one acquisition is not enough, because the very thing the check exists to find makes a second one necessary. When the probe finds the address taken, the client sends a DHCPDECLINE, and RFC 2131 §3.1(5) requires it to wait **a minimum of ten seconds** before restarting; the address it is then offered has to clear §2.1 in its turn. So the default funds **one conflict and its recovery**: 12.0 + 10.0 + 12.0 = **34.0s**, read out of the DHCP client's own constants at startup so a change to either RFC schedule moves the default with it instead of leaving a stale literal behind. This is not theoretical: on the 2.x test lane a 12s default gave up 0.8s before the replacement lease was granted, on a run whose server log shows the whole exchange completing correctly. The old 10s literal funded the DISCOVER retransmission and nothing else.<br><br>**What the longer default costs.** On a segment with no DHCP server at all, `docker run` now fails after about 34s instead of about 12s. That is the price of not failing a container that hit a real address conflict, which is the case this option exists for; `-o lease_timeout=12s` buys the old behaviour back and gives up conflict recovery. In `conflict_check=off` nothing declines, so the extra budget is never spent.<br><br>**A `lease_timeout` shorter than the probe window is refused at `docker network create`** when `conflict_check=wait`, with the arithmetic in the message. Under it a wait acquisition cannot succeed even against an instant DHCP server, so it is a configuration that can only time out. It is accepted in `async` and `off`, where the address is handed over without waiting for the check. Raise it on slow or relayed networks: `-o lease_timeout=60s` funds three retransmissions and sits on top of the fourth. Note the interaction with `dhcp_servers`, which subdivides this budget.<br><br>**DHCPv6 does not run on this budget.** Every term in the 34s above is DHCPv4's: a DISCOVER retransmission, RFC 5227's probe window, RFC 2131 §3.1(5)'s ten-second wait after a DHCPDECLINE, and none of them describes a DHCPv6 exchange. The v6 half of `CreateEndpoint` is bounded instead by a window derived from its own RFCs: RFC 4861 §6.3.7's router discovery (**13s** with the client's constants, because a client has no reason to speak DHCPv6 until an advertisement tells it to) plus RFC 9915 §15's Solicit schedule for four transmissions (**8.7s**), so **21.7s**, and by two other bounds, whichever of the three is shortest: `lease_timeout`, and **what is left of the Docker daemon's own deadline on the call**. The daemon waits 30s for `CreateEndpoint` to answer and then stops listening, and on a dual-stack network the DHCPv4 half runs first and has already spent part of that: about 11 seconds on the project's own bridge fixture, most of it RFC 5227's probe window. So the v6 half gets what is left of 26s, the 30 the daemon allows less 4 for writing the answer, and 21.7s only when that is the smaller number. This is what #868's fix was defeated by: the verdict it produces was correct and arrived after nobody was listening, so `docker run` failed with `Client.Timeout exceeded` instead of starting the container without a v6 address.<br><br>**On a plain SLAAC segment the v6 half ends in about a second, well under 21.7.** An advertisement carrying neither the managed nor the other-configuration flag has said there is nothing to ask DHCPv6 for, and the client sends no Solicit at all on such a link, so waiting out the rest of the budget cannot change the answer. Measured on the project's own fixture, endpoint creation on a SLAAC segment costs about a second more than the same creation on an IPv4-only network. |
| `conflict_check` | all | `wait` | **v2.0.0** | How RFC 5227 Address Conflict Detection is run for endpoints on this network, by the DHCP client, inside the container's own network namespace. **`wait`** (default) completes §2.1's probe before the address is configured: `docker run` blocks for the probe window (4.0–7.0s, 5.5s on average), a conflict is DECLINEd to the server and another address is requested, and the container never comes up on a contested address. **`async`** configures the address at the DHCPACK and probes behind it: `docker run` returns without the extra seconds, and a conflict found afterwards CHANGES the container's address while it is running, and connections on the old one are already broken for both hosts. MEASURED end to end at about **11 seconds** from the conflict appearing to the container carrying the new address, of which ten are RFC 2131 §3.1(5)'s mandatory wait between the DHCPDECLINE and the next DISCOVER; for that whole window the container still holds the contested address, exactly as any other host in a conflict does. Detection and re-acquisition together are about a second. **`off`** sends no ARP at all, neither §2.1's probes nor §2.4's ongoing listener; nothing inside the IPv4 client detects a conflict on this network, so `address_conflicts_v4` and `acd_conflicts_detected` move only for a conflict reported to the client from outside it, which nothing in the plugin does today, and `acd_probes_sent` stays where it was. **The option does not reach IPv6.** It is a DHCPv4 client parameter and the DHCPv6 client has no such parameter, so Duplicate Address Detection runs on every mode: `address_conflicts_v6`, and the `address_conflicts` total with it, still moves on an `off` network. Any other value is refused at `docker network create` with the three names in the message. Networks created before this option existed read as `wait`. **`wait` applies to acquiring an address and never to keeping one:** a container joining a network it already holds a lease on runs the check in `async` even here, so a restart is not charged the probe window a second time for an address the previous run already cleared. The probes, the §2.4 listener and the DECLINE all still run. `-o validate_dhcp=true`'s preflight probe runs `off` for the same kind of reason: the address it is offered is released at once and never configured. Both `wait` and `async` keep watching after the address is in use (§2.4), which is the case the plugin's old probe could not cover at all. **This is not `ignore_conflicts`, and the two are never alternatives:** `conflict_check` is about another *device on the LAN* holding the address your DHCP server just leased, on any mode; `ignore_conflicts` is about another *Docker network on this host* already owning the bridge you named, in bridge mode, before any lease exists. |
| `ignore_conflicts` | bridge | `false` | upstream | Skip the bridge-already-in-use check against other Docker networks. That check is about *this host's* Docker state and never about the segment. It has nothing to do with address conflicts on the LAN; that is `conflict_check`. No-op in macvlan/ipvlan. |
| `skip_routes` | all | `false` | upstream; all modes since v0.9.0 | Don't copy non-default static routes from the parent (bridge or NIC) into containers, **and** don't apply DHCP-supplied classless static routes (option 121, see below), **and** (v2.2.0+, #821) don't apply the routes an IPv6 Router Advertisement asks for. v0.9.0 extended parent route-copying from bridge-only to all modes (#102); set `true` to restore the old macvlan/ipvlan no-copy behaviour. Neither default gateway is affected either way. |
| `propagate_dns` | all | `false` | v0.9.0 | Write the DHCP-supplied DNS server list (option 6 / v6 option 23) into the container's `/etc/resolv.conf` on every bind/renew. Overrides Docker's embedded resolver for this network; the `search` line uses option 119 with fallback to option 15 on v4, and DHCPv6 option 24 on v6 (v1.9.0+, #815). Since v2.2.0 (#821) the v6 list also carries RFC 8106 RDNSS and DNSSL from the Router Advertisement, with DHCPv6's own options taking precedence (RFC 8106 §5.3.1), and it is rewritten when a later advertisement changes it. A resolver at a link-local address is written with its interface as an RFC 4007 §11 scope zone, `nameserver fe80::1%eth0`; musl does not parse that form. An empty list is never written: the container keeps the resolvers it had. |
| `propagate_mtu` | all | `false` | v0.9.0 | Apply DHCP option 26 (Interface MTU) to the container link on bind/renew. For jumbo-frame (9000) and VPN-reduced (~1450) networks. Since v1.8.0 an MTU outside `[576, 65535]` is refused and the link keeps the MTU it had, counted by `mtu_refused`. With [`mtu`](#driver-options-network-level) set, `mtu` governs the link MTU and this option cannot be combined with it. Nothing below this plugin holds the bottom of that range, so a server-supplied 68 used to be applied verbatim. **This option governs IPv4 only.** The MTU an IPv6 Router Advertisement carries (RFC 4861 §4.6.4) is applied whatever it says, because until v2.2.0 the container's kernel applied it on every IPv6 network and an option defaulting to `false` would have taken it away; it is applied to the link, so it bounds IPv4 as well, and the refusal range still holds. On a dual-stack network where both families supply an MTU, the link takes the **smaller** of the two: the larger is a promise the link cannot keep for the family that asked for the smaller one (#821). |
| `mtu` | all | unset | v2.3.0 | Set the MTU of every container link this network creates: a decimal integer from 68 to 65535. When set it is the only source of the link MTU. DHCP option 26 and the MTU an IPv6 Router Advertisement carries are not applied, one info line per endpoint names both values, and `mtu_refused` does not count it. The plugin sets the link back to this value on every bind and renew. Refused at network creation: a value outside 68..65535, a value that is not a decimal integer, a value beside `propagate_mtu=true`, and a value above the MTU of the parent (macvlan, ipvlan) or of the bridge; the error names both values. If the kernel refuses the value when a container starts, the endpoint fails with the kernel's error and leaves no link behind. In bridge mode both ends of the veth pair get the value, since ends that differ drop full-size frames without an error. **Bridge:** a bridge whose MTU was never changed follows its smallest port, so a container with a smaller mtu lowers the bridge's own host interface for as long as that container is attached, and it comes back when the container leaves; a bridge whose MTU was ever set to a value other than its current one holds it (measured 2026-09-24, kernel 6.12). The creation check reads the bridge's MTU at that moment, so while such a container is attached, a second network on that bridge with a larger mtu is refused. **ipvlan:** the child follows its parent's MTU, so a parent MTU change shows in the container until the next renew. **macvlan:** a parent lowered below the configured value clamps the child, and the re-apply is refused on every renew until the parent comes back, with one warning line each time. (#1037) |
| `client_id` | all | per-endpoint id | v0.9.0 | Override DHCP option 61 (Client Identifier) for every endpoint on this network; sent as RFC 2132 opaque bytes (type `0x00`). The default per-endpoint id is what makes per-container reservations work, and a fixed `client_id` makes all containers look like one client to the server. Pair with `vendor_class` for class-based policy. **The derived default differs by mode** (see below). **Since v2.2.2 a change to this option does not move an endpoint that already holds a lease:** the identifier an endpoint sends is the one stored with its lease record, so the new value applies to addresses taken after the change. See [Restart stability](#restart-stability-mac-and-ip). |
| `vendor_class` | all | `docker-net-dhcp` | v0.9.0 | Override DHCP option 60 (Vendor Class Identifier), for DHCP servers running class-based policy (different gateway/option sets per class). v4 only: the DHCPv6 client sends no vendor-class option. |
| `validate_dhcp` | macvlan, ipvlan | `false` | v0.9.0 | Pre-flight probe at `docker network create`: one-shot DHCP exchange on a temporary child of the parent, rejecting the network if no server answers within 8s. Catches isolated parents / blocked UDP 67-68 / broken VLAN tags at create time. Costs one transient lease per probe. Bridge mode rejects the option. **Since v1.6.0 the probe link is the same kind the network's endpoints will be**: a macvlan child for a macvlan network, an ipvlan child for an ipvlan one (#486), and since v2.3.0 in the network's `macvlan_mode` or `ipvlan_mode` (#905). It used to build a macvlan whatever the mode was, on the reasoning that reachability is mode-agnostic; reachability is, but the parent is not. One parent cannot carry both kinds, so a macvlan probe on an ipvlan network was refused outright whenever an ipvlan container was already running on that NIC, so `validate_dhcp` failed for a reason that had nothing to do with DHCP, which is the opposite of what the flag is for. **What MAC you will see at the server:** a random locally-administered address as the client hardware address (`chaddr`), in every mode, so that is the MAC in the server's log and lease table. On ipvlan and on `macvlan_mode=passthru` the frames leave with the parent's MAC, because such a child cannot have its own, by kernel design. The probe is otherwise identity-neutral: it sends no hostname and no client identifier. |
| `dhcp_servers` | all | _(none)_ | v1.8.0 | Ordered preference list of DHCPv4 servers, e.g. `1.1.1.1,2.2.2.2`. The initial acquisition tries each in turn, restricted to that one server, and takes the first lease offered. **The list is exhaustive**: if none of them answers, the endpoint fails instead of accepting whichever server happened to reply. Naming your servers is what makes the list complete. The ladder **divides** the existing acquisition budget (`lease_timeout`) instead of extending it, so enabling this never makes `docker run` slower. Because it divides instead of extending, a long list cannot get one attempt each: an attempt costs entering the container's network namespace, opening a packet socket on the interface and a DHCP round trip, so a slice too small to hold one exchange is a guaranteed failure instead of a fast one. Once the list outgrows the budget the plugin keeps the top entries on their own attempts and asks **the tail as a single group**. With the default 34s budget that is the first ten individually, then the rest together, each attempt taking 3.09s of it. Nothing is dropped, the total does not grow, and what degrades is only the strict ordering *within* that last group. Lists of eleven or fewer are unaffected at that budget. (#731) Once a lease is held it stays with the server that granted it, because renewal is unicast. **DHCPv4 only**: a v6 entry is rejected at `docker network create` instead of being silently ignored, and a DHCPv6 client on a network that sets this is given no list at all instead of a v4 one it cannot use. The list itself is validated the same way: an empty entry (a trailing or doubled comma), an entry that is not an IP address, and a repeated address each fail the create instead of being quietly dropped. **2.0 matches on the Server Identifier (option 54) and never on the packet's source address**, so the list now works behind a DHCP relay, and the 1.x limitation recorded under #111 is gone. Two consequences worth knowing: a message that carries no server identifier at all is **refused** while an allow list is set (an allow list a message can satisfy by omitting the field is not a restriction), and a server identifier is a value anyone on the link can put in a datagram, so this narrows which claimed identities the client acts on and authenticates nothing. |
| `dhcp_deny_servers` | all | _(none)_ | v1.8.0 | Unordered list of DHCPv4 servers this network must never take a lease from, e.g. `3.3.3.3`, a rogue appliance or a second router on the segment. This is a *permission* and never a preference: it composes with `dhcp_servers` instead of competing with it, and a server named in both is removed from the preference list. Denying every entry of `dhcp_servers` is refused at create time, since it would otherwise collapse to accepting any server at all. Same **DHCPv4-only** limit as `dhcp_servers`. **Deny wins** where the two lists disagree. A deny list *on its own* fails open on a message that carries no server identifier: nothing in such a message can show it came from a denied server. (The no-relay limit is gone in 2.0; see `dhcp_servers`.) (#669) |
| `register_dns` | all | `false` | v1.3.0 | Send the DHCP FQDN option (81) built from the container's hostname, asking the DHCP server to register that name in DNS (forward A/AAAA + reverse PTR). Reuses the same hostname already sent as the option-12 hint. Best-effort and advisory, because many consumer routers ignore option 81, so this *requests* registration, it does not guarantee resolution. Off by default: dynamic-DNS registration is a network-policy decision. See below. |
| `audit_log` | all | `false` | v1.0.0 | Append every lease-lifecycle event (`bound` / `renew` / `stopped` / `stop_failed`; since v1.9.0 `config` for a DHCPv6 configuration-only reply, #864; since v2.2.0 `routeradvert` for a router advertisement, #821, and `withdrawn` for an IPv6 address the lease stopped holding, #819) to `STATE_DIR/leases.jsonl`, one JSON object per line with timestamp, network, endpoint, container, hostname, IP, MAC, and since v2.2.0 `source` = `slaac` on an address formed from a router's prefix (#818). Rotated at 16 MB or 30 days (one rotated generation kept, ≤ ~32 MB total). Append failures bump `ledger_write_failures` on `/Plugin.Health`, never affecting lease handling. Off by default: per-event disk write, and container↔IP correlation on disk is privacy-relevant in some environments. |
| `release_lease` | all | `never` | **v2.1.1** (`on_remove`: **v2.2.0**) | Whether, and when, an endpoint hands its DHCP lease back. It leaves its sandbox at every `docker stop`, every `docker rm` of a running container and every `docker network disconnect`. **`never`** (default) sends nothing: the address stays leased until it expires, and a container that restarts before then asks for it again and gets it, exactly as a physical host on the segment does after a reboot (#800). **`on_stop`** sends a DHCPRELEASE (RFC 2131 section 4.4.6) for IPv4 and a Release (RFC 9915 section 18.2.7) for IPv6, one datagram per family, built from the endpoint's own lease record and sent from the host's address on the parent interface (on a `vlan` network the sub-interface, where a plugin-made one is refused, see [VLAN sub-interfaces](#vlan-sub-interfaces-vlan)). The address goes back to the server's pool at once, and the container's next start is a fresh acquisition that may land on a different address. **It does not need a running DHCP client**, which matters for the shape the option is most used for: a container that stops before the plugin's persistent client has attached still hands its address back, because the address it used came from the acquisition at endpoint creation and that acquisition wrote it into the same record. Two things follow and are not configurable: the endpoint lays **no tombstone**, so it does not keep its MAC across a restart, and the lease record of each family whose address actually went back is closed rather than kept resumable. Both are the same rule, that nothing may hand on an address the server has already taken back. **One path is not covered on `never` or `on_stop`, and is covered on `on_remove`.** In IPAM mode an address reserved for an endpoint whose `CreateEndpoint` then failed is retained: retaining it is what lets a restart policy's next attempt claim the same address back instead of burning a second lease on the server, and a reservation with no endpoint reaches no `Leave`, which is the only path `on_stop` releases from. On those two values no DHCPRELEASE goes on the wire for it and the address is left to expire, exactly as any other host on the segment leaves one. On `on_remove` the retention carries a deadline like any other, so the address goes back when the window runs out and no retry has claimed it (#984). `releases_sent` and `release_failures` report what happened, per family. `on_stop` costs `docker stop` one datagram per family, sent synchronously and not retransmitted, with no reply read and no retry. Nothing waits on the server. A release the host cannot send at all fails immediately and is counted, and the address is then left to expire exactly as under `never`. The reason is in the plugin log beside the counter: no record, no leased address on it, no server named on it, no address on the parent to send from, or the socket. **`on_remove`** (v2.2.0, #984) holds the addresses for the restart window and hands back whatever nothing has claimed when the window runs out. It is a **timed** release and not a handler on removal, because there is no removal handler to hang it on: Docker deletes an endpoint when its container **stops**, not when it is removed, so a release sent from that handler would fire on every `docker stop` (which is `on_stop`) and would never fire for `docker rm` of an already-stopped container. The window is the **tombstone TTL, 60 seconds**, the same value that decides how long a stopped container keeps its MAC, so the two can never disagree and there is no second option to set. The release goes out on the sweep that follows the deadline: the sweep runs every 15 seconds and waits 5 seconds past the deadline, so the wall clock from `docker stop` to the datagram is **65 to 80 seconds**. One attempt is made and the record is closed either way; there is no retry, and a failed attempt leaves the address to expire exactly as under `never`. A container that comes back inside the window keeps its address **and** its MAC, exactly as under `never`, and `releases_reclaimed` counts it. What decides that is the **address**: a newer record on the same network holding the same address. A container pinned to a MAC that comes back on a different address does not hold the old one, and the old one goes back. Two further cases also send nothing and do **not** move `releases_reclaimed`, because neither is a container running on the address: the same address stopped a second time, where the newer record carries its own deadline and decides the address itself, and an address acquisition in flight under the same endpoint key, where the address is left to expire so it is not taken from under an exchange that may be about to be given it. The deadline is written into the lease record, so a plugin that restarts inside the window still releases at the right moment, and `docker network rm` hands the network's still-held addresses back at once instead of leaving them for deadlines on a network that no longer exists. Everything `on_stop` does at the moment of the release, `on_remove` does at the deadline: one datagram per family, built from the endpoint's own record, no running DHCP client needed, nothing waited on, and the record closed rather than kept resumable. The one difference before the deadline is that the endpoint **does** lay a tombstone, because until the window runs out the address is still the container's. **What the log says on `on_remove`**, at `info` unless noted: at the stop, one line per endpoint, `release_lease=on_remove: keeping this endpoint's addresses for the restart window`, carrying the window and the addresses; at the deadline, one line per address, `No container claimed this address back inside the restart window, so release_lease=on_remove is handing it back`, followed by the same outcome lines `on_stop` prints; at `debug`, one line per address that is not handed back, one sentence per reason, `A container is running on this address, so it was claimed back inside the restart window and nothing is handed back` (the only one that moves `releases_reclaimed`), `A newer record holds this same address with its own deadline, so this record is closed and the newer one decides when the address goes back`, and `An address acquisition is in flight under this endpoint's key, so this address is left to expire instead of being handed back from under it`, each naming the holding record; at `debug` again, `A held address belongs to a network whose options cannot be read; leaving the record as it is`, which leaves the record alone so a later pass can still decide; at `docker network rm`, one line naming how many of the network's addresses went back, and, at `warning`, `This network's stored options could not be read while it was being removed, so its held addresses could not be handed back` when the removal cannot read what it needs, which is the one case no later pass can repair, because the options and the tombstones go with the network. **What it does not cover.** If the plugin is not running at the moment a container stops, Docker's endpoint deletion never reaches it, no window opens for that endpoint, and its address is left to expire. `on_stop` misses the same stop for the same reason. The sweep deliberately does not repair it: a record the plugin still believes a container is using is never released from the background, because a pass that released those would hand back every address on the host after a restart. Any other value is refused at `docker network create`, with the reason in the message. Networks created before v2.1.1 read as `never`. |
| `host_ifname` | bridge | *(off)* | **v2.2.0** | What the host-side interface this network creates is called, so `ip link` and `brctl show` read like the compose file (#978). Off by default, which is every release before v2.2.0: the link is `dh-` plus the endpoint ID's first 12 hex, unique and meaningless. **`container_name`** names it after the container, the name `docker ps` prints. **`hostname`** names it after the container's hostname (`docker run --hostname`), which defaults to the short container ID and is **not unique on a host**. Any other value is refused at `docker network create`. **Bridge mode only**, and refused in `macvlan` and `ipvlan` rather than ignored there: those children are moved into the container's namespace and leave nothing on the host to name. The name is derived and applied once per attach, from the same daemon answer the DHCP hostname comes from, and nothing about it is written down, so a restart re-derives it. See [Host-side interface names](#host-side-interface-names-host_ifname) for the derivation rule, what happens when the name is taken, and what an operator reads when it does not happen. |
| `require_mac` | bridge, macvlan | `false` | **v2.3.0** | Refuses a container whose MAC the user did not set, so a reservation keyed on the MAC on the DHCP server always matches (#1036). Off by default, which changes nothing. **`true`** refuses, at endpoint creation, a container started without `docker run --mac-address` or Compose `mac_address`, before anything is built: no link, no endpoint and no open lease record are left, and in the `--ipam-driver null` shape no DHCP exchange happens. The error names the option and both ways to set a MAC. The plugin tells a MAC the user set from one Docker generated by the marker Docker adds to the endpoint request only for a user-set MAC (`com.docker.network.endpoint.macaddress`, measured on engines 26 and 29). The marker must decode to the MAC the endpoint carries, so the same key passed as text through `--driver-opt` is refused too. **In IPAM mode** Docker asks for the address before it creates the endpoint, so the address was already leased when the refusal comes: the plugin forgets it at once and does not reuse it, and the server's lease expires on its own. **`docker network connect`** on the command line cannot set a MAC, so a connect from it is refused on such a network, and the error says so. Docker still lists the network in the container's `docker inspect` after the refusal, so the container's next `docker restart`, or a restart by its restart policy, is refused the same way and leaves it stopped; the policy does not retry. Once it is stopped, `docker network disconnect` of that network and then `docker start` bring it back; while it still runs, Docker refuses that disconnect as "is not connected to network" (both measured on engines 26 and 29). An API or Compose connect that sets the endpoint MAC passes on engines that forward it (measured on 29; on 26 a connect of a running container did not carry it). A container already on the network is not checked again when the plugin restarts and rebuilds its endpoint. **Refused in `ipvlan`** at `docker network create`: its children share the parent's MAC and refuse `--mac-address`, so every container would be refused. A value that is not a boolean is refused at `docker network create` too. |
| `link_local_fallback` | bridge, macvlan | `false` | **v2.3.0** | When no DHCPv4 lease arrives in time, the container starts on an IPv4 link-local address (169.254.1.0 to 169.254.254.255, RFC 3927) with no gateway, instead of failing. The plugin keeps asking for a lease and moves the container to it when one arrives (#904). IPv4 only. Off by default, which changes nothing. With it on, `lease_timeout` defaults to `16s`, and a longer value is refused. Refused in `ipvlan`, with IPv6, and in this plugin's IPAM mode. See [Link-local fallback](#link-local-fallback-link_local_fallback). |

### DHCP classless static routes (option 121)

When the DHCP server hands out classless static routes (option 121, RFC
3442, and the identically-formatted Microsoft option 249), the plugin
applies them inside the container alongside the routes copied from the
parent. Routes are captured from the initial v4 lease and programmed at
`Join`. A `0.0.0.0/0` entry in option 121 is treated as the default
route and **supersedes the option-3 router** per RFC 3442 (an explicit
`gateway=` override still wins over both).

That is the *literal* case, and it is not the only one. Routes carrying
no default entry can still supersede it **by union**: a set that takes
every routable unicast destination between them, `0.0.0.0/1` plus
`128.0.0.0/1`, say, with this-network, loopback, link-local, multicast
and reserved space excluded, wins on longest-prefix match, while the
gateway reported to Docker and shown by `docker inspect` stays the
option-3 router. Egress and the displayed gateway then disagree, which
is the shape to look for when traffic does not go where `docker inspect`
says it should. The routes are applied either way, which is correct
client behaviour that legitimate split-tunnel setups rely on, and the
`[Join]` log names every destination and next hop whether or not the
union is complete. What marks the complete case is a `[Join]`
**warning** and the `dhcp_default_route_superseded` counter (see the
[health counters](#pluginhealth) table).

`skip_routes=true` opts out of option-121 routes as well as parent-copied
ones. v4 only: DHCPv6 defines no equivalent option, and an IPv6
endpoint's route comes from the Router Advertisement instead. Option 33, the
legacy static-route option, **is** honoured in 2.0: it is asked for
alongside option 121 and used when option 121 is absent or does not decode.
Option 121 supersedes it whenever both arrive.

### Dynamic-DNS registration (`register_dns`, option 81 / 39)

With `-o register_dns=true`, every endpoint on the network sends the
DHCP **FQDN option**, option 81 (RFC 4702), built from the container's
hostname, asking the server to publish that name in DNS. This pairs with
the option-12 hostname hint the plugin already sends: the hostname says
*who we are*, the FQDN option asks the server to *publish it*. The flags
byte asks the server to perform **both** the forward (A) and the reverse
(PTR) update; the container runs no DNS updater of its own, so the
server does all the work. The v6 equivalent (option 39, RFC 4704) is not
sent. The DHCP library can send it since dhcp-golib v1.1.0
([claymore666/dhcp-golib#22](https://github.com/claymore666/dhcp-golib/pull/22)),
but the plugin sets no DHCPv6 name yet
([#1029](https://github.com/claymore666/docker-net-dhcp/issues/1029)).

The payoff is on-mission: a container becomes resolvable **by name** on
the LAN and not merely reachable by its DHCP-leased IP, with no
per-container plumbing. The name source is the same one used for the
hostname hint and tombstone matching (the container's hostname; the
server supplies the domain).

It is **best-effort and advisory**, like the preferred-address hint:
many consumer routers ignore option 81 entirely, and registration
depends on the server being configured for dynamic DNS. The plugin's
contract is "send the option when asked". It is never "the name will
resolve." Off by default because DDNS registration is a deliberate
network-policy choice.

**It costs the attach one wait that a default network no longer pays**
(v2.2.0+, #961). Option 81 is built from the name the DHCP client is
constructed with and there is no way to set it on a client that is
already running, so an endpoint on a `register_dns` network waits for
the container inspect before its client starts. Every other network
starts the client first, on a container start, and hands the name over
afterwards, which is what [`hostnames_applied_late`](#pluginhealth)
counts. A container **restart** is not that shape: Docker drives it as a
detach and a re-attach with no endpoint creation between them, so the
plugin rebuilds the endpoint first and that rebuild asks the daemon, on
every network. On a busy daemon
that difference is the length of a `docker run`: the daemon does not
answer questions about a container while it is still starting it, so a
`register_dns` endpoint has no address for that whole time and a default
one is already leasing.

### Host-side interface names (`host_ifname`)

In bridge mode every endpoint gets a veth pair: one half goes into the
container, the other stays on the host and is plugged into the bridge.
That host half is what `ip link` and `brctl show` list, and by default it
is called `dh-` plus the first 12 hex of the endpoint ID:

```
$ brctl show br-lan
bridge name     bridge id               STP     interfaces
br-lan          8000.0242ac110002       no      dh-3b1d3b0061fd
                                                dh-8c2e41aa9007
```

`-o host_ifname=container_name` names it after the container instead:

```
bridge name     bridge id               STP     interfaces
br-lan          8000.0242ac110002       no      web
                                                db
```

**The container's name is not available when the link is created.**
libnetwork does not send it to `CreateEndpoint`
([moby/moby#52871](https://github.com/moby/moby/pull/52871) is open for
that), so the link is created with the `dh-` name and **renamed** when
the plugin asks the daemon for the container, which is the same question
the DHCP hostname comes from and costs no extra call. The rename happens
after the attach has already succeeded, so nothing it does can fail a
container start.

**The old name stays on the link as an altname.** `ip link` prints it
under the new one and every lookup still resolves through it, which is
what keeps `docker network inspect`, teardown and the plugin's own
restart recovery working:

```
7: web@if6: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 master br-lan
    link/ether 4e:e1:3a:10:36:dc brd ff:ff:ff:ff:ff:ff
    altname dh-3b1d3b0061fd
```

#### The name the plugin derives

The rule is fixed, so the name is predictable from the container's:

1. every character outside `a-z A-Z 0-9 . - _` becomes `-`
2. leading characters that are not a letter or a digit are dropped,
   because a kernel-legal interface name starts with one
3. a name longer than **15 characters** (the kernel's limit, `IFNAMSIZ`
   minus the terminator) keeps its first 9 and ends in `-` plus the
   endpoint ID's first 5 hex, which are the same 5 the `dh-` name
   carried, so a truncated name still points back at `docker network
   inspect`

| container | host-side link |
| --------- | -------------- |
| `web` | `web` |
| `myproj-web-1` | `myproj-web-1` |
| `myproject-frontend-1` | `myproject-a1b2c` |
| `web@host:1` | `web-host-1` |

The endpoint's 5 hex, and not a hash of the name, because two containers
may share a `--hostname`: keyed on the name, both would derive one
interface name and the second container would lose its rename to a
collision an operator could not see in either name.

#### When the name does not happen

Interface names are unique per network namespace, and the host's is one
namespace shared with every Docker network, every physical NIC and
everything else on the box. So the derived name is a **request**:

- **the name is already taken** — the link keeps its `dh-` name and
  [`host_ifname_conflicts`](#pluginhealth) counts it. Rename the
  container, rename the other interface, or use the other `host_ifname`
  value.
- **the container's name has nothing kernel-legal in it**, the kernel
  refuses the rename for any other reason, or the old name could not be
  kept on the link as an altname (in which case the rename is undone) —
  the link keeps its `dh-` name and
  [`host_ifname_failures`](#pluginhealth) counts it. The plugin log names
  which of them it was.
- **the daemon never answered**, so the plugin never learned the
  container's name. Nothing is renamed and
  [`hostname_lookup_failures`](#pluginhealth) has already reported it;
  this option adds no second counter for one event.
- **`host_ifname=hostname` and the container's `--hostname` carries a
  control character.** The plugin refuses to send such a value as the
  DHCP hostname option ([`unsafe_hostnames_rejected`](#pluginhealth)) and
  refuses it here for the same reason, so the link keeps its `dh-` name
  and [`host_ifname_failures`](#pluginhealth) counts it. Use
  `container_name`, which Docker validates.

In every case the container has its address, its lease and its renewal
client. An interface name is cosmetic and nothing here fails an attach.

**One bound, stated rather than guarded.** The rename and the altname are
two kernel calls, and a plugin killed between them leaves a link that
carries only the derived name. Nothing then finds it by the `dh-` name,
including teardown, so it stays on the bridge until `ip link del <name>`
removes it. The window is one netlink round trip wide. Since v2.2.2 the
plugin's own lookups of that name wait for a rename in flight instead of
reading through it, so an endpoint deleted inside the window is torn down
and `docker network inspect --verbose` reports its host veth; a plugin
that dies inside the window is what the bound above is left describing.

**A restart re-derives the name.** Docker drives a restart as a detach
and a re-attach, the plugin rebuilds the host-side link with its `dh-`
name and renames it again from the daemon's current answer. Nothing about
the name is persisted, so a container renamed between two starts comes
back under the new name.

**A plugin restart leaves an already named link alone.** Recovery
rebuilds every endpoint the containers still hold, which runs the naming
step again over links that already carry both names. The plugin reads the
link's current name back from the kernel and stops there, so a recycle
moves `host_ifnames_applied` and neither failure counter. A container
renamed with `docker rename` while it was running comes back under its
new name at that point, keeping the same `dh-` altname, and is no
different: the rename is the only thing that happens and neither failure
counter moves.


## Driver options (per-endpoint)

Passed per container via `docker network connect --driver-opt`, or as
`driver_opts:` under a service's network attachment in Compose:

| option | description |
| ------ | ----------- |
| `ip` | Request a specific IPv4 address (bare IP, no CIDR; the netmask comes from DHCP). Equivalent to `docker run --ip`; setting both to different values is an error. The address is *requested* from the DHCP server (DHCPREQUEST for it); the server still has final say. |
| `com.docker.network.endpoint.ifname` | (v1.0.0+) Request a specific interface name inside the container (Compose `interface_name`, engine 28+; or this key under `driver_opts`, any engine). The plugin validates the name (≤15 bytes, kernel charset; invalid names fail the attach with a clear error) and returns it in its Join response. **Engine support:** moby's remote-driver layer discarded the returned name (`drivers/remote/driver.go` passed an empty `DstName`) until [moby/moby#52866](https://github.com/moby/moby/pull/52866), merged to moby master on 2026-08-26 and milestoned for engine **29.8.0**, which was released on 2026-09-03. Before that the name was applied for built-in drivers only, and an interface from a *plugin* driver kept the driver's prefix and an index in attach order. **Measured** (v2.1.0, #670), one engine line at a time in a nested daemon: 28.5.2 and 29.7.2 ignore the requested name, 29.8.0 applies it. Those are the lines that were measured, not every build of them: a vendor engine below 29.8.0 carrying the change applies the name, and the plugin still reports it as ignored, because the plugin compares versions and does not probe the behaviour. The plugin side is ready and the rename activates by itself on the first engine that applies the returned name, with no change on this side. Where the version says the name will not be applied, the plugin says so in its log at `CreateEndpoint`, naming the engine and the version that would apply the name, and counts [`ifname_unsupported`](#pluginhealth). |

The plugin puts the IPv6 address Docker hands it at endpoint creation
(Interface.AddressIPv6) into the Solicit as the requested IA Address, the
DHCPv6 equivalent of option 50 and, like option 50, a request the server
may decline. With the null IPAM driver the documented shapes use, Docker
hands the plugin none, so `--ip6` has no effect today (measured on engine
29.8.1, 2026-09-24). IPv6 in IPAM mode is [#960](https://github.com/claymore666/docker-net-dhcp/issues/960), where `--ip6` becomes
deliverable. The same mechanism is what makes an address survive `docker restart`: the
tombstoned v6 address goes back out as the hint.

Container-level knobs that interact with the plugin:

- `--mac-address` / Compose `mac_address` fixes the MAC so the DHCP
  server's MAC-keyed reservations apply (macvlan and bridge; ipvlan
  rejects custom MACs by kernel design).
- `--hostname` / Compose `hostname` is sent as DHCP option 12, so
  DHCP-DNS integration registers the container under this name.

---

## Plugin settings

Change one with `docker plugin disable`, then
`docker plugin set <plugin> NAME=value`, then `docker plugin enable`.
**The order is not a style choice:** the daemon refuses
`docker plugin set` on an enabled plugin with `cannot set on an active
plugin, disable plugin before setting`, so setting first fails on that
line and never reaches the restart.

`AWAIT_TIMEOUT`, the one duration setting, takes a Go duration string
such as `45s` or `2m`. A value that does not parse, or that is zero or
negative, **fails plugin startup** instead of falling back to the
default, the same rule `METRICS_ADDR` follows for a malformed address.
An unset or empty variable is not an error and takes the default below.

> **`OUTAGE_TICK` and `OUTAGE_GRACE` are gone in 2.0.** They tuned
> a watchdog that guessed when a lease had lapsed; the in-tree DHCP
> client holds the lease and reports a failed attempt itself, so there
> is no cadence left to tune. [`config.json`](https://github.com/claymore666/docker-net-dhcp/blob/main/config.json) no longer declares them,
> which means `docker plugin set OUTAGE_TICK=…` is refused by the daemon
> instead of being quietly ignored.

| name | default | meaning |
| ---- | ------- | ------- |
| `LOG_LEVEL` | `info` | logrus level (`trace`, `debug`, `info`, `warn`, `error`). `trace` includes the DHCP client's per-event lines and full HTTP-RPC bodies. |
| `AWAIT_TIMEOUT` | `10s` | Cap on the polling helpers (sandbox readiness, link rename, netns appearance). Bump if a slow daemon-restore window starves endpoint setup. |
| `STATE_DIR` | `/var/lib/net-dhcp` | Where per-network options, the tombstone file, and the `audit_log` ledger persist. **Bind-mounted from the host at this exact path since v1.5.0**, so its contents survive `docker plugin rm`. Before that they lived in the plugin rootfs and every upgrade destroyed them. Two consequences: durability begins with the version that introduced the mount (an upgrade *onto* v1.5.0 still starts from nothing, because the old state was never on the host), and **repointing this setting opts out**, because a path other than the mounted one is inside the rootfs again and is wiped by the next upgrade. |
| `METRICS_ADDR` | *(empty)* | (v1.8.0+) TCP address for the Prometheus `/metrics` endpoint, e.g. `127.0.0.1:9099`. Empty means **no TCP listener**, which is the default and the recommended state unless you are scraping it. `/metrics` is always available on the plugin socket regardless of this setting. **Bind it to loopback or a management interface, never `0.0.0.0`**; see the security note under [`/metrics`](#metrics). A malformed address fails plugin startup instead of being ignored, and a wildcard bind (`:9099`, `0.0.0.0:…`, `[::]:…`) logs a warning at startup naming what the endpoint exposes. It is not refused, because it is a legitimate choice on a private segment, but it should be a choice. |
| `DOCKER_HOST` | *(empty)* | (v2.0.0+) Docker API endpoint. Empty means the socket [`config.json`](https://github.com/claymore666/docker-net-dhcp/blob/main/config.json) bind-mounts, which is what every installation before this setting used and what an operator who sets nothing keeps. Point it at a read-only Docker API proxy to reduce the one grant that makes a compromise of the plugin equivalent to root on the host. The plugin issues only `GET` and `HEAD`, refuses anything else before sending it, and counts the refusal as `docker_api_non_get_refusals`. The allowed paths and a worked example are in [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md). A TLS endpoint is not supported (nothing here reads `DOCKER_CERT_PATH`), and neither is a proxy on its own unix socket: the plugin only sees the paths [`config.json`](https://github.com/claymore666/docker-net-dhcp/blob/main/config.json) mounts, and that mount's source is not settable. Use a plain TCP endpoint on the host's loopback, which the plugin reaches through host networking. |

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

There is no `ip6` driver-opt. The plugin puts the IPv6 address Docker
hands it at endpoint creation into the Solicit as the requested address,
the v6 counterpart of `--ip`. With the null IPAM driver the documented
shapes use, Docker hands none, so `--ip6` has no effect today (measured on
engine 29.8.1, 2026-09-24). IPv6 in IPAM mode is [#960](https://github.com/claymore666/docker-net-dhcp/issues/960), where `--ip6`
becomes deliverable.

On a network created with **this plugin as its IPAM driver** (#110),
`docker run --ip`, `docker network connect --ip` and Compose's
`ipv4_address` need `--subnet` on engine 26 (26.1.4 measured
2026-09-24). Without it the daemon refuses the container with "user
specified IP address is supported only when connecting to networks with
user configured subnets". On engine 29.8 (29.8.1 measured) they work
with or without `--subnet`: the daemon checks whether some pool on the
network contains the address, and the subnet-less pool is `0.0.0.0/0`,
which contains every address. The exact engine boundary is what the
weekly engine matrix records. The request is still the server's to
honour or ignore, as described above; what changes is that Docker knows
about it. Either way the plugin guarantees that the address you pinned
is the address you get: an ACK for a different address fails the run and
is never published in its place.

### Restart stability (MAC and IP)

Across `docker restart`, the plugin keeps the container's **MAC** stable
so the DHCP server sees one device instead of a new one each time.
Without this, MAC-keyed reservations break and the server's lease table
fills with stale pairs.

The mechanism is a short-lived **tombstone**, written at `DeleteEndpoint`
and consumed by the next `CreateEndpoint` on the same network within 60
seconds. It carries the previous MAC, the last leased v4 address, and
the last v6 address. The successor endpoint reuses the MAC and re-requests
both addresses as hints. The TTL covers `docker restart` (sub-second) and
`systemctl restart docker` (15–30s while the daemon re-attaches
everything).

- **MAC stability holds whenever a tombstone was written.** `docker
  inspect` and the LAN then see the same MAC across the restart. A
  tombstone is written at `DeleteEndpoint` from what the plugin recorded
  when it built the endpoint, so the condition is that the plugin still
  holds that record. It does across `docker restart`. It did **not**
  across a plugin restart before v1.8.0: endpoints rebuilt by recovery
  were re-attached without re-recording, so the next `DeleteEndpoint`
  laid down nothing and the container came back on a fresh MAC (#721).
  Fixed, and stated as a condition and not as an absolute, because the
  absolute is what made that bug invisible to anyone reading this page
  to decide whether they needed a reservation.
- **IP stability depends on the server** honouring option 50, exactly as
  for an explicit request above. Where it doesn't, configure a
  reservation against the now-stable MAC and every restart gets that
  address, **in `bridge` and `macvlan` only**. `ipvlan` has no stable
  per-container MAC to key a reservation on: its L2 slaves all inherit
  the parent's, so a reservation would either match nothing or match the
  parent and hand one address to every container on the network. The
  plugin writes no tombstone for `ipvlan` at all, for the same reason.
  See [DHCP identity](#dhcp-identity) for what `ipvlan` uses instead and
  why it does not survive a restart (#219).

In **IPAM mode** (#110) the address is Docker's published value rather
than something only the plugin knows, so its stability is stated
separately and it is weaker.

**The rule.** When a container on such a network stops or is removed,
the plugin keeps its DHCP identity and address for a minute. The next
container that starts on that network claims them: **whichever
container that is**, and only while it is the single one being kept. So:

- One container restarted, nothing else happening on the network: it
  claims its own, and keeps its address. This is the normal case.
- A **different** container started inside that minute claims them
  first. The new container comes up on the stopped one's address, and
  the stopped one gets a fresh address when it comes back. Nothing is
  lost and nothing collides, but neither container's address is the one
  you would predict.
- Two or more kept at once (`docker compose restart`, a daemon restart
  without `live-restore`, or one container restarting beside a
  neighbour removed seconds earlier) and none is claimed: every
  container still gets an address, and **which** address is the DHCP
  server's decision. This case is logged and counted as
  `ipam_rebind_ambiguous`, so it is visible in
  [`/Plugin.Health`](#pluginhealth). The two cases above are not
  counted, because from the plugin's side nothing ambiguous happened.

**A restart of the plugin does not spend the minute.** If the plugin, or
the daemon under it, stops between Docker asking for an address and the
container's endpoint being created, the next plugin process finds the
address held by a record no endpoint owns and hands it back, so the
container claims it again on its next start, inside the same minute. It
does this only for an address no endpoint on that network has: an
address belonging to a container Docker still lists is left alone,
running or not. The addresses handed back are counted as
`ipam_stranded_records`. Before v2.2.2 such an address stayed held until
the network was removed, and `--ip` on it, or a container pinned to the
hardware address that was leasing it, was refused for as long as it did.

The reason the rule is this blunt is that Docker's address request
carries no hostname and no endpoint id. The only thing in it that
identifies anything is a hardware address Docker generates fresh for
every endpoint. There is nothing to match a request back to a
particular previous container on. The `--ipam-driver null` shape has a
hostname at that point and narrows by it.

**It also depends on the DHCP server.** A restarted container comes back
under a new hardware address, so what recovers its lease is the client
identifier (DHCP option 61) that the plugin re-sends from the previous
endpoint. RFC 2131 §4.2 requires a server to use that identifier where
the client sends one, and dnsmasq does: it looks a lease up by client
identifier first and falls back to the hardware address only when one
side has none. Against such a server the binding matches and the
address comes back.

The plugin sends that identifier from the endpoint's own lease record,
both for the address request and for every renewal the container makes
afterwards, so the two exchanges present the one value the server has
the address filed under. Before v2.2.2 only the address request did, and
the renewals derived the identifier from the hardware address Docker had
just minted: the request was given the address back, the container's own
client was refused it seconds later and took a different one, and
`docker inspect` went on reporting the first. One consequence is worth
stating, because it is a change of behaviour: a `client_id` changed on
the network while a container is stopped does not reach the wire on that
container's next start. Its record still carries the value its address
was leased under, and giving that address back is what the record is
for. The new value applies to the next address taken fresh: within a
minute of the container being removed in IPAM mode, and on the next
container created on a `--ipam-driver null` network, where a record
lives as long as the endpoint.

A server that **ignores** option 61 and keys on the hardware address
alone sees an unknown client instead, and hands out a different
address. That is a real configuration and not a hypothetical; dnsmasq
spells it `--dhcp-ignore-clid`, and the integration suite runs a
container restart against it, so the difference is measured. Nothing
fails there and no counter moves; the property is
simply not available. If address stability across restarts matters to
you and your server is not one you can check, the `--ipam-driver null`
shape does not depend on this at all: it restores the previous MAC
itself, so a MAC-keyed server is enough.

To pin an address regardless of any of the above, use `--ip` or
`--mac-address`, or use `--ipam-driver null`.

To make sure every container on a network pins its MAC, create the
network with `-o require_mac=true` (`bridge` and `macvlan`, v2.3.0,
#1036). A container started without `--mac-address` or Compose
`mac_address` is then refused with an error naming both, so a MAC-keyed
reservation cannot silently miss. A refused start in IPAM mode has
already leased an address: the plugin forgets it at once and does not
reuse it, and the server's lease expires on its own. See
[`require_mac`](#driver-options-network-level).

Back in the `--ipam-driver null` shape, two things it deliberately does
not do. Concurrent restarts of several
containers on one network inside the 60-second window fall back to fresh
MACs instead of risking swapped identities between containers.
Tombstones carry the container hostname so restarts in flight can be
told apart when the hostname is known, and only when neither side knows
it does the network-wide "exactly one match" rule apply. Sequential
restarts, the normal case, always satisfy it.

A container whose hostname the plugin **refuses**, one carrying a
control character, which never reaches a DHCP packet (see
`unsafe_hostnames_rejected`), gets no tombstone at all, and so does not
keep its MAC across a restart. That is deliberate and it is not the same
as having no hostname: a hostname-less container writes a tombstone that
matches network-wide, which is the v0.5.0 behaviour above and is correct
for it. Writing one for a *refused* hostname would make the value the
plugin declined to trust for a narrow match into a match against every
container on the network.

An endpoint on a `release_lease=on_stop` network gets no tombstone
either, and for a different reason again: it handed its addresses back
when it stopped, so they are in the server's pool and may already
belong to somebody else. Inheriting the MAC beside them would have the
next container ask for an address that is no longer free. A network
that asks for prompt returns is asking to give up restart stability,
and that is the whole of the trade (#962).

`release_lease=on_remove` **does** get a tombstone, and that is the
difference between the two releasing values rather than a detail of one
of them. Until the window runs out the addresses are still the
container's, so the MAC that asks for them back has to survive with
them; the tombstone's own 60-second TTL is the window, so the MAC and
the addresses stop being the container's at the same moment and cannot
disagree. A container that restarts inside the window keeps both. One
that does not is released and closed, and by then the tombstone has
expired too (#984).

And the tombstone is keyed by **network ID**, so it survives a container
restart but not the removal of the network itself, which is why a plugin
upgrade changes the address (see the callout under
[Upgrade](#install-upgrade-uninstall)).

### DHCP identity

Every exchange the plugin runs carries the same three identity fields,
in every mode:

- **Hostname (option 12)** is the container's hostname (Compose
  `hostname:`, `docker run --hostname`). Servers that auto-update DNS
  publish the container under that name. Best-effort on the initial
  DISCOVER (the plugin waits up to 2s for libnetwork to bind the
  endpoint to a container ID); the renewal client always sends it. In
  **IPAM mode** the first exchange carries no hostname at all: Docker
  asks for the address before the endpoint exists, so there is no
  container to read a name from, and the name first reaches the server
  on the request the plugin sends when the container starts. A server
  that lists its clients by name shows that container unnamed for the
  few seconds in between.
- **Vendor class (option 60)** is the literal `docker-net-dhcp`, so a
  server can gate behaviour on "this is a plugin-managed container"
  without parsing hostname conventions. v4 only; override with
  `vendor_class`.
- **Client identifier (option 61)** is type-byte `0x00` (RFC 2132 opaque),
  with a payload that depends on the mode:

  | mode | payload | survives `docker restart`? |
  |---|---|---|
  | `bridge`, `macvlan` | the endpoint MAC | **yes** |
  | `ipvlan` | eight bytes from the Docker endpoint ID | no |

  In `bridge` and `macvlan` the MAC is unique per endpoint and the
  plugin preserves it across a restart, so the server recognises the
  returning container and renews the same address. It is what makes IPv4
  restart-stable without depending on a `DHCPRELEASE` being sent on the
  way out, which is not always possible (`SIGKILL`, OOM, power loss).
  Since v1.9.0 the plugin sends none by default (#800), so this identity
  is the whole mechanism: a restarting container gets its address back
  by asking again and being recognised. A network that sets
  `release_lease=on_stop` opts out of that, deliberately: it hands the
  address back and its next start is a fresh acquisition (#962).
  `release_lease=on_remove` keeps it for the restart window and gives it
  up only after that, so a container that comes straight back is
  recognised exactly as under `never` (#984).

  `ipvlan` is the exception: its L2 slaves all inherit the parent's MAC,
  so a MAC-derived id could not tell containers apart. Those keep the
  endpoint-derived id, which is unique but **not** stable across a
  restart, because Docker mints a fresh endpoint ID each time (#219).

  No mode survives `docker rm` + `run`. A recreate builds a new sandbox
  with a new MAC and a new endpoint ID, so there is no identity to carry;
  that needs a per-container identity the driver API doesn't currently
  expose (#218).

  Override with `client_id`, though a fixed value makes every container
  look like one client.

#### Options captured from the server

Everything the server returns is captured. Some is applied, the rest is
logged:

**Applied**, when the matching option is enabled: option 6 (DNS servers)
and option 119 (search list, falling back to option 15) into
`/etc/resolv.conf` with `propagate_dns`; option 26 into the link MTU
with `propagate_mtu`; option 121, or option 33 in its absence, as routes
(see [classless static
routes](#dhcp-classless-static-routes-option-121)). The v6 equivalents,
options 23 and 24, are asked for and applied on a DHCPv6 network,
including on a stateless one, where they arrive in an
Information-request reply that carries no address at all.

**Logged** at info level on every bind and renew, and only when at least
one is present, so plain LANs get no extra noise: option 42 (NTP), 66
(TFTP server), 67 (boot file), 119 (when `propagate_dns` is off), 252
(WPAD), 100/101 (RFC 4833 timezone) and 2 (legacy time offset):

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

`-o ipv6=true` gives every container on the network a **DHCPv6 lease
alongside its DHCPv4 one**. Docker reports it as `GlobalIPv6Address`,
and it behaves like the v4 address in every way the protocol allows: it
is requested back after a restart, renewed on its own timers, counted in
its own `*_v6` counters, and handed back on exactly the same rule as the
v4 address: no Release at all on a `release_lease=never` network, which
is the default, one per family at Leave on a `release_lease=on_stop`
network (#962), and one per family at the end of the restart window on a
`release_lease=on_remove` network (#984). The two families are two
records with two deadlines, so one can go back and the other fail in the
same teardown, which is what the per-family counters are for.

**Since v2.2.0 the source of the address is `ipv6_mode`**, and `-o
ipv6=true` is the short spelling of `ipv6_mode=dhcp`. Everything in this
section describes that mode. `off` is the default and means no IPv6
address from the plugin at all; `slaac` and `auto` take the address from
a router advertisement, and the [`ipv6_mode`](#driver-options-network-level)
row covers what changes under them.

**`dhcp_servers` and `dhcp_deny_servers` are DHCPv4-only and keep
applying in every `ipv6_mode`.** They are handed to the v4 client and
never to the v6 one, in every mode alike (`clientServerLists` in
`pkg/plugin/plugin.go` is where the two families split), so a network
that prefers one DHCPv4 server and takes its IPv6 address from the
router is one `docker network create` line and not a contradiction.

What the option does, concretely:

- **A second persistent client**, in the container's own network
  namespace, alongside the v4 one. The address is installed as a `/128`
  on the container link, with the preferred and valid lifetimes the
  server gave (RFC 9915 §7.1) so the kernel deprecates the address
  before it removes it.
- **Duplicate-address detection runs in the client** and the address is
  installed with the kernel's own check switched off (`nodad`). The
  client has already done it, as RFC 9915 §18.2.10.1 requires, and
  running it twice costs the container a window in which it cannot use
  the address and can take the address out of service outright on a link
  that echoes the probe back. When the check finds another node holding
  the address, the client declines it (§18.2.8) and asks again. **The
  second ask drops the preferred address**: a restarting container asks
  for the address it had (see `--ip6` below), §18.2.1 allows a server to
  honour that preference, and a server that does would hand back the
  address that has just been declined, so the endpoint would decline it
  again, once a second, until the Docker daemon stopped waiting for the
  plugin to answer. The endpoint keeps its address across a restart; it
  gives that up the moment somebody else is answering for it.<br><br>
  **The same rule holds after the container is up, and it holds for a
  different reason.** At container creation the drop is the plugin's
  own: its acquisition loop runs at most two passes and clears the
  preference between them. The long-running client is built once and
  holds the preference it was given, so the drop there is the DHCP
  client's: since v2.0.0 it does not ask again for an address it has
  declined, and it drops the resumed binding with it, so a node taking
  the address over *after* the container is up costs one DHCPDECLINE and
  one round of discovery instead of one of each per second. **The
  escape, stated beside the claim:** the plugin says nothing when that
  happens on a running container. A conflict is not a lease failure, so
  no counter moves and no event is logged, and the address the client
  converges on is not reported back to Docker. The container keeps
  working on the address it already has; the endpoint takes the new one
  on its next restart, which goes through the bounded path above.
- **A DUID and IAID that persist.** They are minted once when the
  endpoint is created and stored with it, so a plugin restart, a
  container restart and a plugin upgrade all present the same DHCPv6
  client to the server. Server-side reservations keyed on DUID stick.
- **`propagate_dns` covers DHCPv6 options 23 and 24** (DNS servers and
  the domain search list) as well as the v4 options.
- **The plugin reads the Router Advertisement, and the container's
  kernel does not** (v2.2.0+, #821). DHCPv6 carries no router, because
  RFC 9915 §21 defines no next-hop option, and RFC 5942 §4 forbids
  treating the leased address's prefix as on-link, so the advertisement
  is the only thing that supplies a route. The plugin's own DHCPv6
  client reads it and puts four things into the container: the **IPv6
  gateway** (the advertisement's link-local source address) as the Join
  answer's gateway, the **routes** it asks for (RFC 4191 Route
  Information options as next-hop routes, prefixes with the on-link flag
  as on-link routes), the **MTU** (RFC 4861 §4.6.4) on the container's
  link unless the network sets `mtu` (#1037), and the **DNS servers and search list** (RFC 8106 RDNSS and
  DNSSL) into `/etc/resolv.conf` when `propagate_dns=true`. All four are
  rewritten when a later advertisement changes them, without restarting
  the container, except that on-link prefixes are only added (v2.2.3+,
  #1088): one advertisement need not carry every prefix of the link, so
  an advertisement that leaves a prefix out keeps its route, and the
  route goes when an advertisement gives the prefix a Valid Lifetime of
  0 (RFC 4861 §6.3.4). A prefix whose lifetime runs out with no such
  advertisement keeps its route until the container restarts. Before
  v2.2.3 the on-link prefixes were set once, when the container started,
  and a lease that finished before the first advertisement got none.
  The gateway and the routes need the endpoint to have
  a global IPv6 address: see *Networks where DHCPv6 offers no address*
  below for what a segment without one gets, and why.
- **The Router Advertisement guard**: `accept_ra=0`, `autoconf=0` and
  `keep_addr_on_down=1` on the container's link, and any route the
  kernel had already installed from an advertisement removed. This is
  not optional and there is no option to turn it off. `accept_ra=0`
  because a kernel acting on the same advertisement would install a
  second default route beside the plugin's, and which of the two wins is
  decided by a metric comparison nobody chose. `autoconf=0` because the
  plugin holds the lease for the address the container uses; the kernel
  forms no SLAAC address of its own. On a segment that runs stateful
  DHCPv6 **and** advertises its prefix as autonomous, that is a change:
  such a container used to carry the lease, a kernel-formed address and
  any privacy addresses beside it, and an outbound connection selected
  among them per RFC 6724 rather than necessarily using the address
  `docker inspect` reports. It now carries the lease alone.
- **Prefix delegation is out of scope.** The client asks for an IA_NA;
  there is no IA_PD, and none is planned for 2.0.

#### The DUID differs by mode, and it matters on ipvlan

| Mode | DUID | Why |
|---|---|---|
| `bridge`, `macvlan` | DUID-LL over the endpoint's MAC (RFC 9915 §11.4) | Each endpoint has its own MAC. This is byte for byte the DUID the 1.x line sent, so **an endpoint upgraded from 1.x keeps its address** |
| `ipvlan` | DUID-UUID over the endpoint id (RFC 9915 §11.5) | An ipvlan L2 slave inherits the parent link's MAC, so every container on the network would otherwise present the same identity, claim one binding, and be handed the same address in turn |

**The upgrade note that goes with that:** an **ipvlan** endpoint moving
from 1.x to 2.0 changes DUID, because 1.x derived it from the shared
MAC. It gets a new address on its first start under 2.0 and keeps that
one afterwards. Bridge and macvlan endpoints are unaffected.

#### Networks where DHCPv6 offers no address

An IPv6 network is not always a DHCPv6 network. On a **stateless** or
**SLAAC** segment the router advertises no DHCPv6 addresses at all, and
on some segments no router advertises anything. A container must still
start on those networks, so the plugin classifies the absence rather
than failing:

| What the segment did | What the plugin does | Counter |
|---|---|---|
| Answered with configuration and no address (stateless, RFC 9915 §18.2.6) | Starts the endpoint; applies the DNS servers and search list if `propagate_dns` is on | `dhcpv6_config_only`, `dhcpv6_not_offered` |
| Advertised, with the managed flag clear (SLAAC) | Starts the endpoint without a DHCPv6 address | `dhcpv6_not_offered` |
| Advertised nothing at all, on `ipv6_mode=dhcp` or `ipv6_mode=off` | Starts the endpoint without a DHCPv6 address, and logs a warning. Refusing the container buys nothing: the address would have come from a DHCPv6 server, and a server that is there answers a Solicit whether or not a router advertises | `dhcpv6_no_router_advert` |
| Advertised nothing at all, on `ipv6_mode=slaac` or `ipv6_mode=auto` | **Fails the endpoint**, and the log line is an error naming the mode. Those modes form the address from the advertisement's Prefix Information option, so no advertisement is no address by any route. Up to v2.1.x this row started the endpoint in every mode, because the formed address was not installed yet and failing would have refused a container for the absence of something the release did not deliver ([#818](https://github.com/claymore666/docker-net-dhcp/issues/818)) | `dhcpv6_no_router_advert` |
| Advertised the managed flag, then answered nothing | **Fails the endpoint.** The segment said an address was available and did not deliver one; starting anyway would silently drop the IPv6 address an operator configured | `dhcpv6_no_server` |
| Answered, and refused this client (a Status Code option other than Success, RFC 9915 §21.13) | **Fails the endpoint**, and the log line names the code: `NoAddrsAvail` from an exhausted pool, `NotOnLink` for an address outside the range the server serves | `dhcpv6_refused` |
| Advertised, and none of its prefixes formed an address, on an `ipv6_mode` that takes the address from the advertisement | **Fails the endpoint.** The only configured source of an address produced none; RFC 4862 §5.5.3 lists the reasons a prefix forms nothing | `dhcpv6_slaac_no_prefix` |
| Advertised, and no address formed inside the acquisition budget, on an `ipv6_mode` that takes the address from the advertisement | **Fails the endpoint.** Two things produce it: another node already holds the address this endpoint's prefix and MAC form, and RFC 4862 §5.4.5 gives a node with a fixed interface identifier no second try after duplicate address detection fails; or the advertisement arrived too late in the budget for detection to finish. It is not `dhcpv6_no_server`, which needs a DHCPv6 exchange, and `ipv6_mode=slaac` sends no Solicit at all | `dhcpv6_slaac_no_address` |

**On these rows the container gets no IPv6 route and no global IPv6
address** (v2.2.0+). It has a link-local address, its IPv4, and the
stateless DHCPv6 configuration where the segment offers it. Before
v2.2.0 its own kernel read the advertisement and gave it both; the
guard's `accept_ra=0` and `autoconf=0` stop that, and on these segments
the plugin does not supply the replacement.

**Why not, exactly.** The Docker daemon disables IPv6 on a container
link that carries no global IPv6 address, and the kernel then refuses
every IPv6 route on such a link. An endpoint answer carrying the
advertised gateway or on-link prefix does not degrade: it fails the
whole sandbox with `error setting interface "<host-if>" routes to
["<prefix>"]: permission denied`, and the container does not start at
all, losing its IPv4 with it. The plugin cannot enable IPv6 on the link
first either, because that happens after the daemon has already moved
the link and applied the answer. The route is not installable until
there is a global address to install it beside.

**Where there is one, most of these rows do not apply.** On
`ipv6_mode=slaac` and `ipv6_mode=auto` the plugin forms the address
from the advertisement and installs it
([#818](https://github.com/claymore666/docker-net-dhcp/issues/818)), so
on every row above but the two carrying `dhcpv6_not_offered` such an
endpoint holds a global address or fails outright. Those two are
tolerated in **every** mode, this pair included: neither segment ever
said a DHCPv6 address was to be had, so there is none the endpoint
lost. Every other row that starts an endpoint with no global address
names `ipv6_mode=dhcp` or `ipv6_mode=off` in its own first column.

#### Server-initiated reconfiguration

A DHCPv6 server can start a renewal itself instead of waiting for the
client's T1, by sending a Reconfigure message (RFC 9915 §18.2.11). The
plugin's client accepts one, and has since v2.1.1's DHCP library pin
([#925](https://github.com/claymore666/docker-net-dhcp/issues/925)).

| What the client does | Where it comes from |
|---|---|
| Announces that it is willing to be reconfigured, in its Solicit, Request and Information-request (§21.20's Reconfigure Accept option) | Always on. §21.20: "In the absence of this option, the default behavior is that the client is unwilling to accept Reconfigure messages", so a client that does not announce is never sent one |
| Accepts a Reconfigure only when it is authenticated with the reconfigure key that server gave it (§20.4's Reconfiguration Key Authentication Protocol), and only when the replay detection value is one it has not seen from that server (§20.3) | The DHCP library |
| Discards everything else: an unauthenticated Reconfigure, a wrong key, a replayed value, one that names no server or names another client, one that arrives while an exchange is already in flight | The DHCP library |
| Answers an accepted Reconfigure with the Renew, Rebind or Information-request its Reconfigure Message option names, and applies the result | The DHCP library; the result reaches the container on the same path a renewal with new parameters uses |

**There is no counter for this yet.** A Reconfigure the client accepted
and one it discarded are both invisible on `/Plugin.Health` and on
`/metrics`: the DHCP library records them in its own journal and exports
no number, so the plugin has nothing to publish. The counters are part
of [#925](https://github.com/claymore666/docker-net-dhcp/issues/925) and
land when the library exports them.

**Two bounds worth knowing.** A reconfigure key does not survive a
plugin restart. §20.4.2 has a server choose one "during the
Request/Reply, Solicit/Reply, or Information-request/Reply message
exchange", and a restarted endpoint resumes its held lease with a
Confirm, which carries no announcement and receives no key. It
discards every Reconfigure from that server, and falls back on its own
T1, until it accepts a Reply that carries a key. Which Reply that is
belongs to the server: §20.4.2 gives it the choice of exchange, and
this client records a key from **any** Reply it accepts that carries
one, a renewal's Reply included. A server that sends the key only in
the exchanges §20.4.2 names leaves the endpoint deaf until its next
full acquisition. And an Information-request the server asks for on a
**managed** segment is counted as `dhcpv6_config_only`, the same as a
stateless answer, because it is the same message.

#### If you upgrade onto 2.0 with an IPv6 network already created

Nothing to do. A network record written by a 1.x build carries
`ipv6=true`, that option means the same thing here, and endpoints on it
get DHCPv6 leases as before. On bridge and macvlan the DUID is
unchanged, so the server hands back the same addresses; on ipvlan see
the upgrade note above.

### Link-local fallback (`link_local_fallback`)

*(v2.3.0)* `-o link_local_fallback=true` lets a container start while
the segment's DHCP server is down (#904). It covers IPv4 only; DHCPv6 is
unchanged.

- The endpoint asks for a DHCPv4 lease as usual. If none arrives in time,
  the plugin picks an address from 169.254.1.0 to 169.254.254.255 with a
  generator seeded from the container's MAC (RFC 3927 §2.1), probes the
  link for it with ARP (RFC 5227 §2.1.1), moves on from an address in use
  while a whole claim still fits the time left (see Timing), announces the
  one it keeps, and gives it to Docker as a `/16`.
- The container gets no gateway and no routes. A link-local address
  reaches its own segment and nothing else.
- The plugin keeps asking for a lease. When one arrives, the container's
  address changes to it in place and the lease's gateway and routes are
  installed. This is the same change as a renewal that returns a
  different address: `lease_changed` counts it, the log says
  `dhcp renew with changed IP`, and `docker inspect` shows the 169.254
  address until the container is recreated (#104).
- `/Plugin.Health` shows the endpoint with `lease_state` `link_local`, and
  `link_local_endpoints` counts them.

**Timing.** Docker waits 30 seconds for an endpoint. Claiming an address
takes up to 9 of them (the 7-second probe window and a second
announcement 2 seconds after the first), the plugin keeps 4 for its
answer and 1 for the DHCP attempt to stop, so the attempt gets 16. With
the option on, `lease_timeout` defaults to `16s` and a longer value is
refused at `docker network create`. In `conflict_check=wait` that funds one acquisition and not the
recovery from a conflict that the `34s` default funds. With no server on
the segment, the container starts about 22 to 25 seconds after its
endpoint was requested. With the `16s` default there is room for one
claim: if the first address is in use, a second is probed only when the
conflict shows within about a second, and otherwise the endpoint fails as
it does with the option off. Each 9 seconds taken off `lease_timeout`
leaves room for one more address.

**Refused at `docker network create`**, with the reason in the message:
`mode=ipvlan`, because an ipvlan child does not receive the ARP replies
to its own probes (measured on Linux 6.12), so a used address would look
free; IPv6 (`ipv6=true` or any `ipv6_mode` but `off`); this plugin's
[IPAM mode](#address-allocation); and a `lease_timeout` over `16s`.

**Not done.** The plugin does not defend the address after the claim
(RFC 3927 §2.5); the container's kernel answers ARP for it as for any
address. After ten addresses found in use, or earlier when no whole claim
fits the time left, the endpoint fails, instead of
RFC 3927's one attempt a minute, which no endpoint's 30 seconds could
hold. `release_lease` has nothing to hand back for an endpoint on
link-local and counts nothing, and a removed endpoint that was on
link-local leaves no address in its tombstone.

### Recovery after a plugin restart

`docker plugin disable && enable`, a plugin upgrade, or a plugin crash
used to leave running containers without a renewal client. The lease
would quietly expire and the container would lose its address.

The plugin now walks Docker's network list at startup, finds every
endpoint on a plugin-served network, and rebuilds a DHCP manager for
each. The first acquisition requests the address the container is already
using (option 50) so the server ACKs it instead of allocating a new one.

The walk runs synchronously inside plugin startup, before the socket
accepts requests, whenever the daemon is answering, which is the normal
case. When it is not, the walk is deferred until after the socket is up
and can meet a `CreateEndpoint`; registration is a compare-and-set, so
the `Join` keeps its client and recovery stands down (see below).

Restarting each endpoint's client is not part of the walk. The walk
adopts the endpoint and starts its client on its own goroutine, capped
by `AWAIT_TIMEOUT`, and the counters below move when that client has
restarted, which is after the socket has begun answering. A reading
taken the moment `/Plugin.Health` first responds is a reading taken in
the middle of the work.

Results land on `/Plugin.Health` as `recovered_ok`, `recovery_failed`,
`recovery_aborted_container_gone` and `recovery_network_gone`, with the
last two covering containers that had already exited when recovery
reached them, and networks removed between the listing and the read of
their detail. `recovery_fingerprints_skipped` covers a third: an
endpoint recovery adopted but could not learn the hostname of, which
keeps its renewal client but loses the tombstone that would have carried
its address across its next restart (#721). Neither is a failure and
neither flips `healthy`.

One case cannot be finished there. On a daemon restart Docker respawns
the plugin while the daemon itself is still coming up, so recovery's
first API call can time out against a daemon that is not serving yet.
Waiting for it before the socket exists would stall plugin-enable
against the very daemon being waited on, so recovery is instead retried
once the socket is listening, and the wait is counted as
`recovery_deferred` and not as a failure (v1.4.0+, #383). Only a retry
that runs out of budget counts `recovery_failed`.

The same path covers `systemctl restart docker`. In practice the address
is preserved either by recovery (when the daemon's shutdown never called
`Leave`) or by the tombstone (when it did), and the outcome is the same
either way.

A deferred retry runs while the host's containers are coming back, so it
can meet an endpoint a `Join` has already claimed. Recovery yields:
registration is a compare-and-set, and losing it leaves the `Join`'s
client in place and counts `recovery_already_managed` (v1.8.0+). A
`Join` arriving the other way round still displaces a recovery-built
manager and stops it, and that direction is `displaced_stops`. Both
directions end with exactly one DHCP client on the interface, which is
the property that matters (#480).

### State persistence

Per-network options are written to `STATE_DIR/<network_id>.json` at
`docker network create`, so the per-endpoint handlers never have to call
back into the Docker API to learn the mode or parent. That callback is
what deadlocked the upstream plugin during `dockerd` startup, when it was
asked to restore containers using its own networks.

State survives enable/disable cycles, and since v1.5.0, because
`STATE_DIR` is [bind-mounted from the host](#plugin-settings), it
survives `docker plugin rm` and `docker plugin upgrade` too. Before
v1.5.0 it lived in the plugin rootfs and every upgrade reset it.

The fall-back path is still there and still matters, because state can
be absent for other reasons (a first install, a `STATE_DIR` repointed
off the mount, a file removed by hand): on a cache miss, existing
networks fall back to the Docker API on first read, which back-fills the
file, so by the second endpoint operation everything is served from disk
again.

#### File permissions after an upgrade

Since v1.8.0 the plugin writes everything under `STATE_DIR` with mode
`0600` (#708). The directory is a read-write bind mount from the host,
so at `0644` the container MACs, leased IPs, hostnames and the lease
audit trail were readable by any user on the host. Nothing there is a
credential and the writer is root either way, so this is not a privilege
boundary.

Since v2.0.0 the plugin also sweeps `STATE_DIR` once at every start,
before it reads or writes any state, and clears every permission bit
outside `0600` from each file it finds there
([#804](https://github.com/claymore666/docker-net-dhcp/issues/804)), so
a `0644` file becomes `0600`. An upgrade tightens what an older plugin
left behind, and it does so at the moment the new process starts.

The limits on that sweep, because they are what an operator sees on
disk:

- It only tightens. The mode it writes is the mode it found with the
  bits outside `0600` removed, so no file gains access it did not have.
  A file you set to `0400` stays `0400`, and one you set to `0440`
  becomes `0400`.
- It does not recurse. Only files directly in `STATE_DIR` are swept;
  anything in a subdirectory keeps its mode.
- It skips anything that is not a regular file. A symlink in
  `STATE_DIR` is left alone and its target is never touched.
- A failed `chmod` never stops the plugin. It is logged at warn level
  with the path, and counted in `state_file_chmod_failures`.

Before v2.0.0 the mode was applied only **when the plugin wrote a
file**, so an upgrade did not tighten what it found. `tombstones.json`
is rewritten only when a tombstone is laid or consumed, so on a host
with stable containers it could keep its old `0644` for as long as the
host ran. On a plugin older than v2.0.0, tighten them once by hand:

```bash
sudo chmod 0600 /var/lib/net-dhcp/*.json /var/lib/net-dhcp/leases.jsonl
```

---

## Observability

### `/Plugin.Health`

JSON liveness + counters on the plugin's UNIX socket. **`sudo` is
required**, because `/run/docker/plugins` is `drwx------ root root`.
Without it `curl -s` prints nothing and exits 7, which is what an absent
socket also gives, so a permission problem looks exactly like a dead
endpoint:

```bash
PLUGIN_ID=$(docker plugin inspect -f '{{.Id}}' ghcr.io/claymore666/docker-net-dhcp:v2.2.3)
sudo curl -s --unix-socket /run/docker/plugins/$PLUGIN_ID/net-dhcp.sock \
    http://localhost/Plugin.Health | jq .
```

A plain `GET` is correct; no request body or method override is needed.
Anything that can reach the socket can poll this as a liveness check.

Every counter below is a **plugin-wide total** and never a per-endpoint
one. A rise tells you *some* client on this host saw the event, never
which one, so when a counter moves, the plugin log is what attributes
it: each bump is emitted alongside a line carrying that endpoint's
`endpoint=<short id>` field. Alerting on the counters is right;
diagnosing a specific container from them alone is not.

*(2.0-alpha.1+)* The document also carries three things that are not
counters: a `status` of `pass`/`warn`/`fail` with a named `checks` map,
an `endpoints` array describing each managed endpoint, and the build
identity this binary was compiled with. `/metrics` carries `status` as
`net_dhcp_health_status` (0 pass, 1 warn, 2 fail) and the build
identity as labels on `net_dhcp_build_info`; the `checks` and
`endpoints` structures are **not** exposed there.

The **check** column below says which status each counter contributes.
It is derived from the rows themselves, by the rule below, stated in
full here so that you can apply it to any counter row and get the column
back instead of having to trust it. **Its domain is the counter rows**,
the ones whose healthy-affecting column says **yes** or **no**. The rows
above them in the same table are the document's other fields, and a
field describes a structure or a gauge instead of a monotonic counter:
it contributes no check and carries `n/a` in both columns for that
reason, so an imperative in one of those rows (`endpoints`, `healthy`)
is about how to read that field and never about a value a check could
fire on. The rule:

1. **`fail`**, when the healthy-affecting column says **yes**. Those are
   exactly the counters behind `healthy`, so a `fail` check and
   `healthy: false` cannot disagree: both are read from one declaration
   in one pass.
2. **`warn`**, when the row tells you to act on **this counter's own
   value** (watch it, alert on it, investigate it, "the actionable
   one"), **and** zero is that counter's normal reading, so that
   non-zero is itself the abnormal state a check can fire on. Both
   clauses have to hold, and the second one is why two rows that read
   like the first are not checks.
3. **`n/a`** for everything else, which is the majority. A row that
   lands here despite carrying an imperative says so in its own words,
   with the clause that excluded it. There are exactly two such shapes,
   and they are not a list to be extended: clause 2 is a conjunction, so
   there are two ways to fail it. Either the imperative is **not about
   acting on this counter's own value**, because it sends you to another
   counter or says how to *read* a value instead of what to *do* about
   one, or the counter's **normal reading is not zero**, so a check
   firing on non-zero would fire on every healthy host. A row can carry
   both, and several do.

`warn` never touches `healthy`, and no check is invented for a counter
this table does not classify: the column and the code's declaration are
reconciled in both directions by
[`scripts/check-health-contract.sh`](https://github.com/claymore666/docker-net-dhcp/blob/main/scripts/check-health-contract.sh).

The shape is `draft-inadarei-api-health-check-06` (2021-10-16; an
expired Internet-Draft and never a standard) with two deliberate
departures, both stated because a reader who knows the draft will look
for them. **The response code stays 200 for every status.** Section 3.1
asks for 4xx/5xx on `fail`, and `healthy` latches, so one fault recorded
an hour ago would make this socket answer 5xx for the life of the
process, and everything that polls a plugin socket reads a non-2xx as
"the plugin is down". **The media type stays `application/json`** and
never becomes the draft's `application/health+json`, for the same
reason: this endpoint is also the liveness probe, and clients that
already parse it were not told to expect a new type.

| field | healthy-affecting | check | meaning |
| ----- | ----------------- | ----- | ------- |
| `status` | n/a | n/a | *(2.0-alpha.1+)* `pass`, `warn` or `fail`, the worst status of any entry in `checks`, per section 3.1 of the health-check draft. `fail` and `healthy: false` are two renderings of one fact and cannot disagree: both are derived from the same five counters, in the same read. `warn` never makes `healthy` false. Like `healthy` it **latches**, because the counters behind it do. |
| `checks` | n/a | n/a | *(2.0-alpha.1+)* One entry per counter classified `fail` or `warn` in the check column below, keyed by the counter's own name, each a single-element array (section 4). Every entry carries `status`, `observedValue` (the counter), `observedUnit`, `time`, and, only when it is not passing, `output`, the sentence saying what to do. **`time` is when that counter last moved** and never when the document was built, which is what makes a latched `fail` readable: a counter that has never moved carries the time of this reading. Not on `/metrics`: a structure of that shape has no exposition rendering, and the per-check numbers are already there as their own series. |
| `endpoints` | n/a | n/a | *(2.0-alpha.1+)* One entry per managed endpoint, and the array is exactly `active_endpoints` long, sorted by endpoint id, so two consecutive polls of an unchanged host produce the same document. Each entry: `endpoint` and `network` (short ids), `mode`, `address` (CIDR), `lease_state` (`bound`, `acquiring`, or since v2.3.0 `link_local` for an endpoint on its [link-local fallback](#link-local-fallback-link_local_fallback), whose `address` is then the 169.254 one), `renew_at` / `rebind_at` / `expires_at` (T1, T2 and the lease end as **absolute** RFC 3339 times, because a remaining-seconds figure is meaningless once the document has been cached or pasted into an issue; an absent `expires_at` on a bound endpoint is the protocol's infinite lease), `server` (option 54), `last_event` and `last_event_at` (since v2.2.3 these and the lease fields come from one record, so a `bound` entry always names the event that bound it, #1044), and the RFC 5227 pair `conflict_check` (the [`conflict_check`](#plugin-settings) mode in force) and `acd_phase` (`idle`, `probing`, `settling`, `announcing`, `defending`). Read the phase **against** the mode and never alone: in `conflict_check=off` the phase is `idle` because nothing runs. `unknown` for both means the endpoint has no client yet. **On a dual-stack endpoint every field of the entry describes the IPv4 client**, which is the one the RFC 5227 pair can describe at all: a container with an `ipv6=true` network has a DHCPv6 lease this array does not report, and `docker inspect` is where to read it. Not on `/metrics`: per-endpoint labels are the thing [`SECURITY.md`](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md) promises are absent from the exposition. |
| `healthy` | n/a | n/a | `false` when `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, `tombstone_quarantines`, or `address_conflicts` is non-zero, and an operator should look. Those five, and only those, are the ones marked **yes** in the healthy-affecting column. The plugin keeps serving fresh attaches either way. **It latches:** every counter behind the flag is monotonic, so `false` means "a fault occurred at some point during this plugin process" and never "something is wrong right now". **Until 2.0-alpha.1 that was the end of what could be learned from this document**; each named check now carries the moment its own counter last moved, so "faulted an hour ago" and "faulting now" are no longer the same reading. Fixing the condition does not clear it. Only restarting the plugin does, and that tears down the renewal client of every managed endpoint on the host. Read it together with the *instance_id* field: the same ID means the same process is still reporting a fault it recorded earlier. |
| `instance_id` | n/a | n/a | (v1.5.0+) Opaque identifier of the plugin **process** serving this response. Every counter below is in-memory and returns to zero when the process does, so two readings are comparable as a delta only when their `instance_id` matches. If it changed between two samples, the plugin restarted and any difference you computed is meaningless, including one that reads as zero. Prefer this over `uptime_seconds` for that check: a plugin that restarts early in a long sampling window and then runs longer than the first reading shows uptime going *up*, hiding the restart. |
| `version` | n/a | n/a | *(2.0-alpha.1+)* The release tag this binary was built for, or `dev` for anything built outside a release. Also a label on `net_dhcp_build_info`. **Never empty**: an empty value would read as "nothing to report" instead of "this build does not know". |
| `commit` | n/a | n/a | *(2.0-alpha.1+)* The full git revision the tree was at, or `unknown`. Full and not abbreviated, because git shortens to a length that depends on the size of the clone, and [Verifying releases](verifying-releases.md) needs the same string to reproduce the same binary. |
| `library` | n/a | n/a | *(2.0-alpha.1+)* The version of the `dhcp-golib` DHCP library this build carries, the module version pinned in [`go.mod`](https://github.com/claymore666/docker-net-dhcp/blob/main/go.mod). Read from the source tree at build time (`go list -m`), so it cannot be passed in wrong. |
| `engine_version` | n/a | n/a | *(v2.1.0+)* The Docker Engine version the daemon reported when this plugin process started. It is the value the minimum supported engine is compared against, and the plugin refuses to start below that minimum. Reads `unknown` when the daemon did not answer at startup, which happens when Docker starts the plugin during its own start-up; the plugin takes the reading again when the daemon answers, and `unknown` that persists means it never did. |
| `api_version` | n/a | n/a | *(v2.1.0+)* The Docker API version this plugin negotiated with that daemon. It is the lower of the two maximums, so it can be below what either side supports, and a socket proxy that pins an old API shows up here. Nothing is refused on it. Reads `unknown` on the same terms as `engine_version`. |
| `uptime_seconds` | n/a | n/a | Seconds since the plugin process started. Useful as an age, but see `instance_id` before using it to decide whether a restart happened. |
| `active_endpoints` | n/a | n/a | DHCP managers currently registered (post-Join, pre-Leave). |
| `link_local_endpoints` | n/a | n/a | *(v2.3.0+)* Endpoints on an IPv4 link-local address right now, because no DHCPv4 lease arrived in time on a `link_local_fallback=true` network (#904). Counted from the same snapshot as the `endpoints` array, where these entries have `lease_state` `link_local`. It falls as each endpoint moves to a lease, and that move is counted in `lease_changed`. A gauge; never accumulated. |
| `pending_hints` | n/a | n/a | Join hints awaiting consumption; steady-state ~0. |
| `recovered_ok` | n/a | n/a | Endpoints successfully rebuilt by plugin-restart recovery. |
| `recovery_failed` | yes | fail | Post-restart rebuilds that failed **for a container that is still running**. It runs without lease renewal and loses its IP at expiry; restart it. Three things are deliberately *not* counted here, because none of them leaves a running container without a renewal client: a daemon that is merely still starting (`recovery_deferred`, #383), a container that had already exited when recovery reached it (`recovery_aborted_container_gone`, #376), and a network removed out from under the recovery walk (`recovery_network_gone`, #648). |
| `recovery_deferred` | no | n/a | (v1.4.0+) Recovery met a daemon that was not serving yet and was retried once the plugin socket came up (#383). Expected on a daemon restart. Only worth attention paired with `recovery_failed`, which together mean the retry ran out too. **Not a check:** that imperative is about the pair and never about this counter's own value, which is expected to be non-zero after any daemon restart. `recovery_failed` is the `fail` check that carries the condition. |
| `recovery_aborted_container_gone` | no | n/a | (v1.4.0+) Recoveries abandoned because the container had already exited, or been removed, by the time post-restart recovery reached it. Not a fault: nothing is left running without a renewal client, so this never flips `healthy`. The recovery-side twin of `join_aborted_container_gone`, and normal after a daemon restart that outlived some containers (#376). |
| `recovery_network_gone` | no | n/a | (v1.8.0+) Networks skipped during post-restart recovery because they had been removed between the listing that found them and the read of their detail. Not a fault: a network that is gone leaves no running container without a renewal client, so this never flips `healthy`. Counted instead of passed over in silence: a host where this climbs steadily is churning networks under a restarting daemon, which is worth knowing even though no single occurrence is a problem. Until v1.8.0 it landed in `recovery_failed`, where an ordinary `docker network rm` racing a daemon restart reported the plugin's most serious fault (#648). |
| `recovery_fingerprints_skipped` | no | n/a | (v1.8.0+) Endpoints that post-restart recovery adopted but could not describe: the `ContainerInspect` that supplies the hostname did not answer, or answered with no hostname. **No** because the endpoint keeps its renewal client: nothing is running without one, which is the line `recovery_failed` draws. What it loses is the fingerprint, so `DeleteEndpoint` lays no tombstone and that container gets a fresh MAC, and in general a different address, on its next `docker restart`. Counted because before #721 the only sign was `tombstones_consumed` staying flat, which is also what a quiet host looks like, so an operator could not tell "recovery worked" from "recovery silently skipped half my endpoints". A hostname *refused* for carrying a control character is not counted here; it moves `unsafe_hostnames_rejected` instead, so a degraded daemon stays distinguishable from a hostile container. |
| `recovery_already_managed` | no | n/a | (v1.8.0+) Endpoints a recovery walk found already registered to another manager, and therefore left alone, because a `Join` reached them first. Not a fault: the endpoint has a renewal client, it just is not the one this walk would have built. It is counted because it is the only outward evidence of recovery racing a `Join`, the window that made the registration a compare-and-set instead of a read followed by a write; before v1.8.0 those endpoints were reported as *recovered* in the completion log while `recovered_ok` correctly did not move (#480). |
| `join_start_failures` | yes | fail | (v1.3.3+) Persistent-client start failures at attach time **for a container that is still running**. It got its initial lease but runs without renewal, and on a `release_lease=never` network, the default, the lease is not released on disconnect (#317). **`release_lease=on_stop` does release it**, and this counter is exactly the case that change was made for: the release is built from the endpoint's lease record and needs no running client, so an endpoint that never got one still hands its address back at `Leave` (#962). `release_lease=on_remove` reaches it too, at the end of the restart window instead of at `Leave` (#984). The plugin log carries the cause; fix it and restart the container. A container that *exited* mid-attach is counted separately and is not a fault; see below (#373). |
| `join_aborted_container_gone` | no | n/a | (v1.4.0+) Attaches abandoned because the container exited before the persistent client was up. Not a fault: there is no running container missing a renewal client, so this never flips `healthy`. A sustained rise still says something real: containers dying seconds after start, e.g. a crash-loop (#373). Recognised three ways: the daemon answering "no such container", the container's netns having gone, or its sandbox key being unlinked. An attach that fails for any other reason is counted as a fault and never excused (#401). |
| `join_aborted_no_container` | no | n/a | (v1.6.0+) Attaches abandoned because no container ever claimed the endpoint on the network. Since v1.9.0 the leased address is **left to expire** and not released (#800); before then it was released here, which is why the counter's name says nothing about either. `release_lease=on_stop` does not change this row: a release happens at Leave, and an endpoint no container ever claimed reaches no Leave (#962). **`release_lease=on_remove` does change it** (#984): it releases from the record rather than from `Leave`, and this endpoint has a record. The address is handed back once Docker deletes the endpoint, which for an unclaimed one is the removal of the container or of the network, and the restart window has then run out. Not a fault: nothing is running without a renewal client, because nothing is running, so this never flips `healthy`. Distinct from `join_aborted_container_gone`, which needs the daemon to say "no such container" or the sandbox netns to be visibly gone; this one covers the case where the endpoint is simply unclaimed after the attach budget, which previously fell through to `join_start_failures` and leaked the address (#566). |
| `join_attach_slow` | no | n/a | (v1.4.0+) Attaches that succeeded, but only after outlasting `AWAIT_TIMEOUT`. Not a fault: the container has its renewal client. It is reported because the wait has an external cause worth seeing: the attach asks the daemon about the container being attached, and the daemon does not answer while it is still inside that container's start. Before v1.4.0 those attaches were abandoned and counted as `join_start_failures`, leaving a running container with no renewal client (#406). A rising count means the daemon is holding containers longer and never that the plugin is degrading. |
| `join_attach_completed` | no | n/a | (v2.1.0+) Successful attaches. It is the population `join_attach_under_1s`, `join_attach_1s_to_budget` and `join_attach_slow` partition, and without it a bucket reading zero cannot be told from a plugin that has attached nothing (#403). |
| `join_attach_under_1s` | no | n/a | (v2.1.0+) Successful attaches that stayed inside `AWAIT_TIMEOUT` and finished in under a second. |
| `join_attach_1s_to_budget` | no | n/a | (v2.1.0+) Successful attaches that took a second or more and stayed inside `AWAIT_TIMEOUT`. Above the budget is `join_attach_slow`, so the three buckets sum to `join_attach_completed`. An attach over the budget is counted in the tail whatever its length, so with an `AWAIT_TIMEOUT` below a second this bucket is empty and `join_attach_under_1s` counts only the attaches that stayed inside the budget. |
| `join_attach_ms_max` | no | n/a | (v2.1.0+) The longest successful attach in milliseconds, saturating at 2147483647. Read it against `AWAIT_TIMEOUT`. The plugin also logs one line per successful attach with its duration and phase breakdown, but that line is at debug level and the shipped `LOG_LEVEL` is `info`, so these four readings are the per-attach timing a host carries as installed (#403). **Not a check:** the value is a duration and carries no verdict, and what counts as a long attach depends on the host and on `AWAIT_TIMEOUT`. |
| `join_aborted_endpoint_left` | no | n/a | (v1.4.0+) Attaches cancelled because `Leave` arrived while the attach was still running, since the endpoint was being torn down. Not a fault: there is no running container missing a renewal client. Distinguished from `join_start_failures` by direct evidence instead of inference, since the plugin cancelled the attach itself and knows why (#406). |
| `tombstone_write_failures` | yes | fail | Failed tombstone saves (disk full, EROFS). The next restart of some container will pick a fresh MAC/IP instead of inheriting. Since v1.8.0 it also moves when the tombstone file could not be **read** for a transient reason (EIO, a read racing a writer): the plugin refuses to rewrite the file from nothing instead of destroying contents that may be perfectly good, and the consequence for that endpoint is identical to a failed write. The name is narrower than the meaning; the meaning is "an endpoint will not keep its address across a restart" (#724). |
| `tombstone_quarantines` | yes | fail | (v1.8.0+) Times the tombstone file was found **unparseable** and moved aside as `tombstones.json.corrupt-<timestamp>` in [`STATE_DIR`](#plugin-settings) (#724). Strictly worse than `tombstone_write_failures`: that costs one container its MAC and address, this costs every one of them, because the whole live tombstone set went with the file, so any container restarting for the next 60 seconds comes back with a new identity. Kept separate from the write counter on purpose, since the two call for different action. **The quarantined file is never reaped.** Read it before deleting it: it is the only record of what was lost, and its contents say whether this was a truncated write, a filesystem fault, or something else writing to that path. |
| `tombstones_consumed` | no | n/a | (v1.5.0+) Recreated containers that got their previous MAC/IP back by replaying a fresh tombstone. Not a fault: this is the address-stability mechanism working. It is the counterpart to `recovered_ok`: after a restart an address is preserved either by recovery re-adopting a still-attached endpoint (`recovered_ok`) or by a tombstone being replayed (this). Reported so the two can be told apart, which is what makes "the address survived, but via neither path" observable instead of silent (#386). |
| `lease_changed` | no | warn | Renewals that returned a different IP than last recorded (v4+v6 aggregate). Docker's `inspect` view does **not** update on lease change (libnetwork has no in-place endpoint-IP swap), so this is the stale-inspect-window signal. Alert on it for long-running containers. **In IPAM mode the gap is wider**, because the address is also Docker's own allocation: the container moves to the new address, Docker's IPAM still holds the old one as allocated, and after a daemon restart the plugin is asked to replay an address it no longer has a record for and refuses (`ipam_replay_miss`, and a warning in the daemon log). The usual cause is a server that lost its lease file and NAKed the request, so fix it at the server; the container itself keeps working until the daemon restarts. |
| `address_conflicts` | **yes** | fail | (v1.6.0+; RFC 5227 since v2.0.0) Leased addresses found to be already in use by another device on the segment, in **both families**: it is the sum of `address_conflicts_v4` and `address_conflicts_v6` and nothing increments it directly. Only the v4 half is RFC 5227; the v6 half is Duplicate Address Detection and has its own row below. The DHCP client runs RFC 5227 Address Conflict Detection from inside the container's network namespace: §2.1 ARP-probes the offered address **before it is used**, and §2.4 keeps listening for the whole life of the lease. Either way the address is DHCPDECLINEd to the server (RFC 2131 §3.1(5)) and another one is requested, so the counter moving means the plugin found a conflict *and acted on it*, and never that a container is sitting on a contested address. **A conflict found after the address is in use changes the container's address**, which `docker inspect` does not update; watch `lease_changed` too. Under `conflict_check=off` the IPv4 client neither probes nor listens, so nothing inside it can find an IPv4 conflict; `address_conflicts_v4` can then move only for a conflict **reported to the client from outside it**, and no code path in the plugin does that today, so on an `off` network the v4 half does not move. The same rule governs `acd_conflicts_detected`. **It does not hold for this total.** `conflict_check` is a DHCPv4 client parameter with no DHCPv6 counterpart, so the v6 half keeps counting Duplicate Address Detection on an `off` network and carries the total with it. This is the only signal for the condition from the plugin's side, since from the DHCP server's point of view the lease was issued normally, though since 2.0 the server also learns about it, because the DECLINE is on the wire and in its log. The usual cause is a **statically configured** host inside the DHCP pool range: it never asks the server for anything, so the server cannot know the address is taken. Fix it at the server (reserve or exclude the address) and never at the plugin. **What it does not cover:** another container on the *same host* sharing the same parent NIC. macvlan isolates a parent from its own children, so a sibling's answer never reaches the probe. Excluded by construction and never pending work (#528). |
| `acd_probes_sent` | no | n/a | (v2.0.0) RFC 5227 §2.1.1 ARP Probes sent. Read this **before** believing `address_conflicts_v4` is 0: with no probes the two readings are identical, and "the detector never ran" is what #524 looked like for months. A healthy segment is `acd_probes_sent` climbing with `address_conflicts_v4` at 0. It says nothing about `address_conflicts_v6`, which is Duplicate Address Detection and sends no ARP frame. Moves in `conflict_check=wait` and `=async`, never in `=off`, so a zero here on a host whose networks are all `off` is the configuration working and never a fault. **Not a check:** the imperative above is to read this counter *against* `address_conflicts_v4`, and its own normal reading is non-zero and climbing, so a `warn` check here would fire on every healthy host, which is the second clause of the rule above. What is worth alerting on is this counter staying **flat**, and a check fires on a value and never on the absence of movement. |
| `acd_announcements_sent` | no | n/a | (v2.0.0) RFC 5227 §2.3 ARP Announcements sent, two per address that passed its probe, telling the segment the address is now in use. A live scrape can be one behind. The first announcement is sent at the bind and the second from a timer 2s later (§2.3 ANNOUNCE\_INTERVAL), and the plugin folds the DHCP library's count on client events, so an address that has just been bound reads 1 until the next event on that endpoint. On a quiet endpoint that is until the T1 renewal. Read against `acd_probes_sent`: probes climbing with announcements flat means addresses are being checked and none is coming back clean. Moves in `wait` and `async`, never in `off`. **Not a check:** both clauses fail. The imperative is to read this counter *against* `acd_probes_sent`, because flat announcements are a signal only while probes climb, and its own normal reading is non-zero and climbing, so a check firing on non-zero would fire on every healthy host. |
| `sandbox_netns_visible` | no | n/a | (v1.6.0+) How many sandbox netns entries the plugin can currently see, or `-1` if it cannot read the directory at all. Sampled per request and never accumulated. **Read it against `active_endpoints`, never alone.** `-1` means the bind mount is missing, so the `sandboxGone` check can only ever answer "no usable evidence", which is safe but useless. A `0` *with endpoints attached* is the dangerous one: the directory is readable but mounted from the wrong place, and `sandboxGone` would conclude every container had vanished. A `0` with nothing attached is neither, because there is genuinely nothing to see (#567). **Not a check:** it is a gauge whose normal reading is non-zero, and both of its abnormal readings (`-1`, and `0` with endpoints attached) are judged against `active_endpoints` instead of against zero, so neither clause of the `warn` rule fits, and a check that fired on "non-zero" would fire on every working host. |
| `sandbox_netns_propagation` | no | n/a | (v2.1.0+) Whether a mount the daemon makes under the sandbox netns directory after the plugin process started can reach that process: `1` linked, `0` private, `-1` no mount covers the directory or the mount table could not be read. Sampled per request. A `0` says every attach is refused on the key route with `sandbox_key_not_a_namespace` and carried by the container PID, which is what the host PID namespace and `CAP_SYS_PTRACE` grants are for. A `1` says the key route carries the attach and those arms stay flat. Both are ordinary. The reading belongs to the mount the daemon publishes sandbox keys on, not to the plugin's own bind, so no plugin setting changes it. A `shared:` tag names a peer group the plugin cannot check is the daemon's, and the shipped manifest asks for no propagation option, so on a stock install the tag is `master:` or nothing (#417). **Not a check:** both readings are correct on the hosts that produce them, and a check firing on `0` would fire on every host whose `/run` is private. |
| `sandbox_netns_init_mounts` | no | n/a | (v2.1.0+) Sandbox netns mounts in the mount table of PID 1: `-2` PID 1 is in the plugin's own mount namespace, `-1` the table could not be read, `N` otherwise. **Read it against `sandbox_netns_visible`.** Under a nested engine PID 1 is that engine's init and not the outer host's, so this reads differently on a test lane and on an ordinary host, and a route judged on one of them alone is judged against the wrong number (#417). **Not a check:** every reading is correct on the host that produces it. |
| `acd_arp_send_failures` | no | warn | (v2.0.0) ARP Probes and Announcements the socket refused. Not healthy-affecting on its own, because a refused send is not a conflict, but a probe that never went out proves nothing about the address, so a rise turns "no conflict found" into "the question was not asked". Watch it for the same reason `acd_probes_sent` exists: a detector that has stopped working looks exactly like a clean segment, which is how #524 went unnoticed in the first place. |
| `acd_resumed_unchecked` | no | warn | *(2.0-alpha.1+)* Endpoints picked up after a plugin restart from a durable record whose RFC 5227 section 2.1 check had not finished when the previous process stopped, so the address was held with no completed check behind it. **Not a fault:** the resumed client re-runs section 2.1 on its INIT-REBOOT acknowledgement whatever the record said, so the window closes on its own and the container keeps its address; this counts how often it opened. Only reachable with `conflict_check=async`, which is the mode that returns the address before the check completes. **Worth investigating** whenever it moves: zero is the normal reading, and each tick is one endpoint that held an address across a restart with no completed conflict check behind it. A segment where that window is being hit repeatedly is one where the plugin is restarting under load. The matching `endpoints` entry carries the `acd_phase` the record was resumed from. |
| `acd_conflicts_detected` | no | n/a | (v2.0.0) Conflicts counted by the DHCP client's own state machine. It is the **same population** as `address_conflicts_v4`, counted inside the client instead of from the events it emitted, and the two are expected to be equal. It is not comparable to the unsuffixed `address_conflicts`, which carries the DHCPv6 share as well, and those conflicts come from Duplicate Address Detection, which this machine never sees. A difference is a defect in the plugin's event handling and never a property of your segment. Report it. Its `conflict_check=off` rule is its sibling's and never a different one: with no probe and no listener the only thing that can move either counter is a conflict reported to the client from outside it, and the plugin has no such caller today. |
| `leases_obtained` | no | n/a | Client bind events: an initial bind, or a re-bind after a NAK or a lease loss. v4+v6 aggregate; on an IPv4-only host it equals its `_v4` half. |
| `leases_renewed` | no | n/a | Client renewal events, including the case where the renewal returned a different address. v4+v6 aggregate, equal to its `_v4` half in 2.0. Since v2.2.0 an endpoint that takes its name after its client is already leasing renews **once immediately** to carry it, so on a host where that path runs this counter reads one higher per attach than the lease timers alone would put it (#961). [`hostnames_applied_late`](#pluginhealth) counts those, so the two can be read together. |
| `renewals_unanswered` | no | n/a | *(v2.1.0+)* Renewal requests the server did not answer, one per request, counted while the client keeps running (#940). It moves at the first retransmission of a renewal request. RFC 2131 section 4.4.5 has the client "wait one-half of the remaining time until T2 (in RENEWING state) and one-half of the remaining lease time (in REBINDING state), down to a minimum of 60 seconds", so 60 seconds is a FLOOR under that wait and not a bound on it, and the wait is long on a long lease. On the 24-hour lease this was reported from, T1 falls at 12 hours and T2 at 21 hours, so the first retransmission lands about 4.5 hours after the client's first renewal request at T1, and this counter moves there. `dhcp_timeouts` first moves for a **held** lease only when that lease ends, at 24 hours, so the gain on that lease is about 7.5 hours of warning and not a day of it. Size an alert on the lease in use: the 60-second floor is reached only when T2 is about two minutes away or less. The request currently in flight is not counted, because only the retransmission that follows a request proves that request went unanswered, so a client that has sent N requests into silence reads N-1. An answer of any kind ends the renewal and leaves this counter alone, a DHCPNAK included; `naks_received` is where that case is counted. Both families: a DHCPv6 Renew or Rebind counts the same way. Not healthy-affecting; the container keeps a working address for the rest of the lease. **Not a check:** the imperative is to read this counter *against* `leases_renewed`, because renewals completing beside it is a healthy lease and renewals flat beside it is a server that has gone quiet, and a single lost datagram moves it on a segment that is working, so non-zero on its own carries no verdict. |
| `dhcp_server_tier_fallbacks` | no | n/a | (v1.8.0+) Steps **down** the `dhcp_servers` ladder: one per preferred entry that did not answer inside its slice of the acquisition budget and handed on to the next (#111). It counts transitions and never container starts: a single `docker run` against three silent preferred servers adds 2. That is the more useful number (it says how far down the list acquisition had to walk) and it is what the code has always produced; this row and two other copies said "acquisitions" until #731. Not healthy-affecting, because the endpoint still got an address. This is the only outside signal that a preferred server is silently dead: a steady rise while every container still starts fine is exactly the condition that otherwise goes unnoticed until the standby fails too. |
| `dhcp_server_policy_exhausted` | no | n/a | (v1.8.0+) Initial acquisitions abandoned because **no** server listed in `dhcp_servers` answered (#111). Not healthy-affecting on its own: the acquisition failure it accompanies already fails the operation and is counted. It exists because "the servers you named are all silent" and "DHCP is broken" look identical in a timeout log and call for different action. |
| `dhcp_server_policy_timeouts` | no | n/a | (v1.8.0+) The renewal half of the same question (#731): `dhcp_timeouts` ticks raised on an endpoint whose **renewal** client is restricted to `dhcp_servers`. `dhcp_server_policy_exhausted` cannot cover this: nothing is exhausted at renewal, because the persistent client has no ladder to walk; it holds one whitelist and simply gets no answers, which looks exactly like the server being down. A **strict subset** of `dhcp_timeouts`, and that is how to read it: the two rising together says the allow-list is the cause (a named server renumbered, retired or firewalled), `dhcp_timeouts` rising alone says it is not. Not healthy-affecting, for that same reason: every tick here is already counted in `dhcp_timeouts`, and counting one outage twice would make a policy-restricted endpoint look worse than an unrestricted one failing identically. v4 only: `dhcp_servers` is not applied to DHCPv6, so this can never rise for a v6-only failure. **Not a check:** the imperative is to read this counter *against* `dhcp_timeouts`, because the pair says which cause, and this counter alone says only that a renewal timed out, which `dhcp_timeouts` already counts and already surfaces. |
| `dhcp_timeouts` | no | n/a | DHCP failures reported by the client itself: an acquisition that found no server, or a held lease that ran out without being renewed. **Changed in 2.0.** 1.x had no direct signal and inferred one from a watchdog that compared the granted lifetime against the time since the client was last served, so a bound endpoint's outage surfaced only after its whole lease had elapsed plus one watchdog tick, up to ~24 hours on a 24-hour lease. The in-tree client owns the lease and its own retransmission schedule, so it reports the failure when the schedule runs out instead of one watchdog tick later. **Read that per case.** For an acquisition the schedule is bounded by `lease_timeout`, so the counter moves at the end of that budget. For a **held** lease the client keeps retransmitting until the lease itself expires, so on a long lease the counter still moves only at the end of it; `renewals_unanswered` is the earlier signal there (#940), and the two rows are read together. `OUTAGE_TICK` and `OUTAGE_GRACE` are gone with the watchdog. v4+v6 aggregate, equal to its `_v4` half in 2.0. |
| `client_stop_failures` | no | n/a | (renamed from `lease_release_failures` in v1.9.0, #800) A renewal client did not shut down cleanly when the plugin stopped it at teardown. **What "cleanly" means changed in 2.0:** there is no client process to signal and no exit status to read. The plugin cancels the client's context and waits for its goroutine to return, so a tick here means the run loop came back with an error that was not the cancellation, or did not come back inside the finish timeout at all. It does **not** mean a lease went unreleased: since v1.9.0 nothing this plugin runs sends a DHCPRELEASE on any path, so every stopped container's address is held until it expires whatever this counter reads. A pattern points at clients wedging in their own loop, or at a finish timeout set too tight. |
| `releases_sent` | no | n/a | **(v2.1.1)** Leases handed back to the server: a DHCPRELEASE (RFC 2131 section 4.4.6) or a DHCPv6 Release (RFC 9915 section 18.2.7) that left this host. Zero on every network that does not set `release_lease`, which is the default and every network created before v2.1.1. Counted **from the send** and never from the decision to release: it moves where the datagram left the host, so a release the host could not put on the wire moves `release_failures` instead. Split per family, because a dual-stack endpoint can hand one address back and keep the other. Expect roughly one per family per `docker stop` on an `on_stop` network, and one per family per stop that was not followed by a restart inside the window on an `on_remove` network, 65 to 80 seconds after the stop (#984). |
| `release_failures` | no | warn | **(v2.1.1)** Attempts to hand a lease back that put no message on the wire. The plugin log names which of them it was, beside the counter: this endpoint and family have no lease record to build from, the record holds no address or no server identity, the record is one the sender refuses, the parent interface carries no address to send from, the send itself failed, or a DHCPv6 address could not be taken off the container link first, which RFC 9915 section 18.2.7 requires before the exchange may begin. Not healthy-affecting, because nothing on this host is broken by it: the container is stopping either way and the address is simply left to expire on the server's clock, which is what a `release_lease=never` network does on every teardown. **Worth investigating** all the same, because a network that asked for its addresses back is not getting them back, and a pool sized for prompt returns will run short. Stays at zero on a `never` network, which has nothing to attempt. On an `on_remove` network the attempt is made once, at the end of the restart window, and is not retried: the record is closed either way and the address is left to expire (#984). |
| `releases_reclaimed` | no | n/a | **(v2.2.0, #984)** Held addresses a **running container is using again** at the end of the restart window on a `release_lease=on_remove` network, so the record was closed and nothing went on the wire. It is the option's quiet half, and it is the only outside sign that the window did what it exists for: with `releases_sent` climbing and this flat, nothing is restarting inside the window and the option is behaving as a slower `on_stop`; with this climbing, restarts are keeping their addresses. What counts as a claim is another record on the same network holding the same **address**, not the same MAC, so a container pinned to a MAC that came back on a different address does not hold the old one and the old one is released. **It is narrower than "nothing was sent".** Two other outcomes send nothing and do not move it: the same address stopped a second time, where the newer record has its own deadline and decides the address, and an address acquisition in flight under the same endpoint key, where the address is left to expire so an exchange that may be about to be given it is not undercut. Both have their own `debug` line. `releases_sent` plus `releases_reclaimed` is therefore **not** the number of windows that ended. Split per family, because a dual-stack endpoint can have one address in use again and the other released. Zero on `never` and on `on_stop`, which have no window. |
| `naks_received` | no | n/a | (v1.0.0+) The server NAKed a renewal, a rebind or an INIT-REBOOT request (v4+v6 aggregate). The client recovers by re-acquiring, so each NAK is typically followed by a `leases_obtained` bump, and by a `lease_changed` bump if the address moved. Climbing alongside `lease_changed` means containers are being re-addressed mid-life. |
| `displaced_stops` | no | n/a | (v1.3.5+) Attaches that found a manager already registered for the same endpoint and stopped it, which is a container restarting into a plugin that had already recovered it (#338). The displaced client is stopped cleanly and the new one takes over. Stopped is not released: a displacement is not an endpoint leaving its sandbox, so it sends no DHCPRELEASE on a `release_lease=on_stop` network and starts no window on a `release_lease=on_remove` one, and the address stays leased for the incoming client to renew. A few are normal after a plugin restart. Climbing steadily alongside `recovered_ok` means a container is in a restart loop. |
| `restart_link_up_waited` | no | n/a | (v1.5.0+) Child links that came up only after waiting out the departing link's hold on the address, i.e. how often a container restart met the #408 window and the fix carried it. Not a fault: this is the repair working, counted so the window is visible instead of inferred. A steady rise means your hosts restart containers fast enough to hit it routinely, which is expected for images that handle `SIGTERM` promptly. |
| `restart_link_up_timeouts` | no | warn | (v1.5.0+) The same wait outlasting its budget: the restart fails and `docker restart` reports `address already in use`. A real failure, but deliberately not `healthy`-affecting: it surfaces directly to whoever ran the command, and `healthy` is for faults nothing else reports. Any non-zero value here is worth investigating; it means the departing link held the address longer than the budget allows (#422). |
| `parent_link_waits` | no | n/a | (v1.6.0+) Operations that had to queue for a shared parent interface before attaching their own link. A parent NIC can be a macvlan port or an ipvlan port but never both, so when networks of both kinds share one parent, or when a `validate_dhcp` probe still has its temporary link attached, the plugin serialises them per parent instead of letting the kernel refuse one with `device or resource busy` (#486, #549). Queuing is the mechanism working; a steady rise just means that NIC is busy. Since v2.1.0 this also counts the operations that gave up waiting for a holder attaching the **same** kind of child: a parent takes any number of those side by side, so the wait protected nothing and the operation goes on to succeed. Two containers starting together on one IPAM-mode network land here, because an address reservation holds the parent for its whole DHCP exchange. |
| `parent_link_wait_timeouts` | no | warn | (v1.6.0+) The same wait giving up after its budget where the holder was attaching the **other** kind of child, or a holder the plugin could no longer identify. The operation asks the kernel anyway and may fail with `device or resource busy`. The budget is 4s, sized to absorb an ordinary DORA on the `validate_dhcp` probe, so a holder that wedges degrades to the pre-v1.6.0 behaviour instead of stalling a container start. Not `healthy`-affecting, but the actionable one of the pair: a non-zero value means a macvlan and an ipvlan operation contended for one parent NIC for longer than a DHCP round trip, and a container start there can fail. Same-kind contention is **not** counted here; it is in `parent_link_waits`, because the kernel permits it and the operation succeeds. |
| `unsafe_hostnames_rejected` | no | n/a | (v1.8.0+) Container hostnames dropped because they carried a control character (#692). **What the drop protects changed in 2.0.** Nothing generates a client config any more. `directives_refused`, which counted values kept out of one, is removed for exactly that reason, and the hostname now goes straight into the DHCP parameters the plugin builds and onto the wire, as option 12 and, with `register_dns`, as the option-81 FQDN. The drop is still the safe outcome and the lease proceeds, because the hostname decorates the exchange and the opt-in `register_dns` registration, so this is not `healthy`-affecting. It is not purely cosmetic, though: the hostname is also the key that narrows tombstone matching to the container that wrote the tombstone, where an *empty* hostname means "match any tombstone on this network", so a refusal is deliberately kept distinguishable from an absence instead of collapsed into an empty string. Read it as an intent signal and not as a fault: Docker does not validate `--hostname`, and a legitimate one never contains a control character, so a non-zero value means something sent one on purpose. Underscores and other technically-illegal-but-common hostnames are **not** counted; the rule is about control characters and never about RFC 1123. **Not a check:** the imperative says how to *read* a non-zero value and never what to *do* about one: the same row says the drop is the safe outcome and the lease proceeds, so there is no degraded state for a check to fire on. |
| `hostnames_applied_late` | no | n/a | *(v2.2.0+)* Container names given to a DHCP client that was already leasing (#961). Since v2.2.0 the attach starts the persistent client **before** it asks the daemon for the container's name, so a container leases at the speed of the segment instead of at the speed of a daemon that is busy starting it; the name is handed to the running client when the answer comes, and the client renews early to carry it (RFC 2131 section 4.4.5), so the server's table has it within one exchange. This counter is the mechanism working, and it **narrows** the two rows below without deciding them: zero on all three is also what a host reads when its containers were started without `--hostname`, and what it reads when every attach took the name before the client started. A non-zero value here is the only reading that says the late path ran and worked; a zero is three states and the plugin log tells them apart. It counts non-empty names only, because a container started without `--hostname` has nothing to hand over and is not an event. **v4 only**, because the plugin sets no DHCPv6 name yet ([#1029](https://github.com/claymore666/docker-net-dhcp/issues/1029)), though the DHCP library can send one since dhcp-golib v1.1.0. Two populations are deliberately **not** counted here, because on both the name was already in the client's opening parameters: a network with `register_dns`, which needs the name at construction for option 81, and a host whose sandbox key is refused, where the container inspect the PID fallback made has already answered. **Not a check:** it is the normal reading on a working host and its normal value is not zero. |
| `hostname_lookup_failures` | no | warn | *(v2.2.0+)* Attaches whose container inspect never answered, so the endpoint leases with no name in the DHCP server's table (#961). **No** because the endpoint is working: it has its address and its renewal client, which is the whole point of starting the client first, and what it lacks is its name upstream until something attaches it again. The lookup runs on the attach's own context and is abandoned with it, so this is the same `awaitTimeout` + 60s window `join_attach_slow` reports on, and the two rise together when a daemon is slow. Watch it rather than page on it: a sustained rise is a daemon that is not answering, which affects a great deal more than names. |
| `hostname_apply_failures` | no | warn | *(v2.2.0+)* Container names the running DHCP client would not take, so the endpoint leases with no name in the DHCP server's table (#961). The daemon answered here; the client refused the handover. Three causes, all from the library: a name it will not put on the wire, a request queue that was full, and no running client left to give it to. Separate from `hostname_lookup_failures` because the two describe the same endpoint and their remedies are at opposite ends of the host. Watch it: a rise with no lookup failures beside it is this side of the host and not the daemon, and the names of the containers involved are in the plugin's log at `warn`. |
| `host_ifnames_applied` | no | n/a | *(v2.2.0+)* Host-side links renamed after the container they belong to, on a network that set `host_ifname` (#978). The mechanism working, and the **denominator** for the two rows below: all three stay at zero on a host where no network asked for named links, so their zeros mean nothing without this one beside them. **Bridge mode only**, because it is the only mode that leaves a link on the host; `macvlan` and `ipvlan` children are moved into the container and the option is refused there at `docker network create`. One per attach that renamed a link, so a container restart counts again -- the name is re-derived every time and never persisted. **Not a check:** its own value is a count of work done and carries no verdict, and on a host that set the option its normal reading is non-zero and climbing. |
| `host_ifname_conflicts` | no | warn | *(v2.2.0+)* Renames refused because another interface on this host already had the name the container asked for (#978). Interface names are unique per network namespace, and the host's is one namespace shared with every Docker network, every physical NIC and every tunnel on the box, so the name a container asks for is a request and not a reservation. The endpoint keeps its lease, its renewal client and its `dh-` link name; what it loses is the readable name. **Actionable**, which is why it is separate from the row below: it is the only refusal whose remedy is to rename something. Rename the container, rename the interface that already holds the name, or switch that network's `host_ifname` to the other value. Read it against `host_ifnames_applied`. |
| `host_ifname_failures` | no | warn | *(v2.2.0+)* Renames that did not happen for any reason other than the name being taken (#978). Four causes and the plugin log names which: the container's name had no character an interface name may carry, the host-side link was not there to rename, the kernel refused the rename, or it took the rename and would not keep the old name on the link as an altname -- in which case the rename is undone, because the old name is what teardown looks the link up by. The endpoint keeps its lease and its `dh-` link name in all four. **Watch it** rather than page on it: an unreadable interface name breaks nothing, but a steady rise means the option is doing nothing on this host and the log says why. Read it against `host_ifnames_applied`, whose zero would otherwise make this one's zero meaningless. |
| `unsafe_option_values_dropped` | no | n/a | (v1.8.0+) Server-chosen DHCP string values refused before use because they carried a control character, plus option-15 domains truncated at their first space. The filter is reflective and covers every string value in the lease, so a new one is covered the day it is added; the ones it exists for are the free-text options 66, 67, 100, 101 and 252, which arrive as bytes the server chose and are carried into a log line, a `resolv.conf` or the audit ledger, none of which share an escaping rule. A space in option 15 additionally turns one search domain into several, with the server's choice first in the order, so that cut is counted here too. The sibling of `unsafe_hostnames_rejected`, for the values the *server* chooses instead of the container. A legitimate server sends none of these, so any rise is deliberate. |
| `network_options_rejected` | no | n/a | (v1.8.0+) Endpoint operations that met a network's stored options and would not act on them as written: an interface name the kernel would not accept, or a `mode` this plugin does not implement. Name validation runs when a network is *created* (#705); this check runs every time the stored options are *read*, which is where the name actually reaches netlink. Not healthy-affecting: refusing is the safe outcome, the operation already fails visibly to Docker, and one network's record being wrong does not make the plugin unwell, because every other network on the host keeps working. A non-zero value means one network needs recreating: either it was created before name validation existed, or its options were written directly into the state directory. `DeleteEndpoint` is deliberately exempt so a refused network can still be torn down. It counts an unknown mode and proceeds, so a rise here does not mean nothing was torn down. Only the mode: teardown reads no stored name at all (it derives the link from the endpoint ID), so there is no name refusal available to it. |
| `ipam_replay_hits` | no | n/a | *(v2.1.0+)* Stored endpoint addresses confirmed at a daemon restart from the plugin's own lease record. Only moves on a network created with this plugin as its IPAM driver (`--ipam-driver <plugin>`); with `--ipam-driver null` it stays zero for the life of the process, which is what makes a zero here ambiguous on a mixed host. The mechanism working: when Docker restarts it asks the IPAM driver to confirm every address it already stored, and each confirmation is one container that keeps its address. Read it as the denominator for the row below, which is the only way to tell "no misses because everything matched" from "no misses because nothing was asked". **Not a check:** its own value is a count of work done and carries no verdict, and on a host that restarts Docker its normal reading is non-zero and climbing. |
| `ipam_replay_miss` | no | warn | *(v2.1.0+)* Stored endpoint addresses the plugin would **not** confirm at a daemon restart, because no lease record in that network holds them. The refusal is the safe outcome and is not `healthy`-affecting: Docker keeps the address it stored, the refusal is logged, and the network driver's own recovery adopts the endpoint from Docker's view. It is worth investigating, because it means the lease record and Docker's store have drifted apart on a host where they are supposed to be two views of the same fact: a lost or hand-edited `lease-records.jsonl`, a state directory restored from a backup, or a network whose records were removed while its containers were not. The endpoint that missed is the one to look at; the others are unaffected. |
| `ipam_rebind_ambiguous` | no | warn | *(v2.1.0+)* Address requests that met more than one recently-removed endpoint on the same network, so nothing said which previous address the request belonged to and the DHCP server chose. **This is a documented limit, counted.** An address request carries no hostname and no endpoint id, so when several containers on one network restart together the plugin has nothing to match them on, and guessing would hand one container's address to another. Not `healthy`-affecting: every container still gets an address. Watch it anyway: it is the one signal that addresses on this host moved for a reason an operator can act on, by pinning the ones that matter with `--ip` or `--mac-address`, by restarting containers one at a time, or by using `--ipam-driver null`, where restart stability is carried by the plugin's own tombstones instead. |
| `ipam_reserve_duplicate_mac` | no | warn | *(v2.1.0+)* Address requests refused because the network was already leasing an address for that hardware address, so the container that asked second did not start. It is **not** `healthy`-affecting: refusing is the safe outcome, and the alternative is two endpoints holding one address and the loser failing later with a message about a plugin restart that did not happen. Its producer is two endpoints carrying one MAC. Docker generates a unique hardware address per endpoint and passes an operator-set one through unchanged, so `docker run --mac-address X` twice on one network, or a Compose file pinning one MAC on two services, puts two endpoints on one hardware address; a DHCP server files its lease per hardware address and would hand them the same address. Worth investigating whenever it moves, because each move is a container that did not start: give each container its own `--mac-address`, or leave it unset. It is **not** moved by Docker re-sending a request after its own plugin call timed out: that re-send carries no body (the daemon hands the same, already-drained reader to every attempt) and is refused before any handler runs, so raising `--timeout` does not change this counter. |
| `ipam_stranded_records` | no | warn | *(v2.2.2+)* Lease records a previous plugin process left behind with no endpoint on them, handed back when this process started so their addresses can be claimed again. **A move is the repair, not the fault.** The producer is a plugin process, or a daemon under it, that ended between Docker asking for an address and the container's endpoint being created. Left alone such a record keeps the address for good: it is not one of the recently-removed endpoints a restart claims from, so the container's next attempt takes a second address; it still answers address lookups, so `--ip` on that address and a container pinned to its hardware address are both refused; and no network ever hands it back, not even `release_lease=on_remove`. Not `healthy`-affecting. Watch it: a steady rise means the plugin or the daemon is restarting while containers start, and that is worth investigating on its own. |
| `ipam_release_unknown` | no | n/a | *(v2.1.0+)* Addresses Docker released that no lease record of the plugin's holds. Not a fault and not `healthy`-affecting: a release for an address whose record was already retained (the endpoint was deleted) or closed (its creation failed) is the normal ordering, and the counter exists so that the release path has an outside number at all. |
| `dns_propagation_pid_mismatches` | no | n/a | (v1.8.0+) DNS propagations refused because the container PID resolved through Docker no longer belonged to that container by the time the plugin acted on it (#688). Only reachable with `propagate_dns` opted in. Refusing is the safe outcome, because the container keeps the `resolv.conf` it had and the next renewal propagates again, so this is not `healthy`-affecting. It is reported because the plugin runs in the host PID namespace: each one is a `/etc/resolv.conf` write that would otherwise have landed in an unrelated host process that inherited the recycled PID. A sustained rise means containers are exiting inside the propagation window; an isolated one is a container that stopped at the wrong moment. |
| `netns_pid_mismatches` | no | n/a | (v1.8.0+) Sandbox network-namespace opens refused because the container PID resolved through Docker no longer belonged to that container. The sibling of `dns_propagation_pid_mismatches` above, on the path with the larger blast radius and with no opt-in: what the refusal prevents is not one file but a netlink handle carrying every address, MTU and route the plugin applies, with `CAP_NET_ADMIN`. **In 2.0 the second half of that is closer to home and never further away.** Nothing is spawned into the namespace: the plugin locks one of its own OS threads, `setns`es it into the sandbox, opens the raw packet socket that carries the whole DHCP exchange there, and returns the thread. So the wrong namespace no longer means a client process pointed at the wrong container. It means the plugin's own thread and its own `CAP_NET_RAW` socket landed on an unrelated host process's network, and the client that keeps running on that socket stays there for the life of the endpoint. Refusing fails the attach, so unlike the DNS case it is not silent, but the error reads like a slow container start, and only this counter says the PID belonged to something else. Not `healthy`-affecting: the attach failure is reported to Docker, and the container simply does not come up on this network. Any non-zero value is worth reading: it means a container exited inside the attach window and the kernel handed its PID to another task. |
| `dhcp_routes_applied` | no | n/a | (v1.8.0+) Routes handed to Docker at Join, counted as routes and never as Joins: DHCP option-121 classless static routes, and since v2.2.0 the routes an IPv6 Router Advertisement asks for (#821). `skip_routes=true` opts out and then this never moves. Read it as the denominator for the row below. **Not a check:** the imperative is to read it as the denominator for `dhcp_default_route_superseded`, while its own value is a count of work done, carrying no verdict either way, and on a network whose server sends option 121 its normal reading is non-zero and climbing. |
| `dhcp_default_route_superseded` | no | n/a | (v1.8.0+) Joins whose option-121 routes cover `0.0.0.0/0` **by union** instead of by a literal default entry, e.g. `0.0.0.0/1 g` plus `128.0.0.0/1 g`. Neither half is a default route, so the gateway reported to Docker (and shown by `docker inspect`) is still the one from option 3, while every packet follows the option-121 next hop instead. This is legitimate in split-tunnel setups and the routes are applied either way; the counter exists because before it, nothing in the plugin's output distinguished the two cases. The accompanying log line names each destination and next hop. |
| `mtu_refused` | no | n/a | (v1.8.0+) MTUs outside `[576, 65535]`, refused with the container link left at the MTU it had. Each family is counted separately, and a refused value does not vote on the link MTU. Option 26 on the IPv4 path, which moves only with `propagate_mtu=true`, and since v2.2.0 the Router Advertisement's MTU option on the IPv6 path, which is not gated on it (#821). Neither is counted on a network that sets `mtu`, which applies neither (#1037). Nothing below the plugin holds the bottom of that range: a server-supplied 68 is carried through verbatim and would be accepted, and the result is destroyed throughput plus black-holed path MTU discovery, re-applied on every renewal, which looks like a slow network instead of a misconfiguration. |
| `sandbox_key_entries` | no | n/a | (v2.0.0+) Container network namespaces entered through the sandbox key the daemon publishes under `/var/run/docker/netns/`. **Which attaches count here is a property of the host, reported as `sandbox_netns_propagation`**: the plugin's read-only `/var/run/docker` is a bind taken at plugin start, so a sandbox older than this plugin process is always reachable through its key, and one created afterwards is reachable only where the daemon's mount is linked to that bind. Every endpoint recovered after a plugin restart counts here on any host (measured 2026-09-05). It is the **denominator** for the two rows below and the reason they can be read at all: zero fallbacks with zero entries means nothing was entered, and never that the key route works. If it rises on attaches, the mounts do reach this plugin on your host, and the netns half of `pidhost`/`CAP_SYS_PTRACE` is not load-bearing for you. **Not a check:** the imperative is to read it as the denominator for `sandbox_key_entry_failures` and `sandbox_pid_fallbacks`: zero here means nothing was entered and never that nothing went wrong, and neither direction is abnormal on its own: zero is the normal reading on a host whose `/run` is private and non-zero is the normal reading on one where the key route works. |
| `sandbox_key_entry_failures` | no | n/a | (v2.0.0+) Attempts to enter a container network namespace through the sandbox key that were refused. **This rises once per attach on a host whose `sandbox_netns_propagation` is `0`, and that is the expected state there, with nothing degraded and no action indicated.** On a host answering `1` it stays flat and `sandbox_key_entries` rises instead. Each refusal falls straight through to the PID route without a retry, so the endpoint still comes up; it is the counterpart of `sandbox_key_entries` staying at zero, and it means the host PID namespace and `CAP_SYS_PTRACE` are load-bearing on this host. The five rows below say WHICH refusal it was, and they sum to this counter exactly. |
| `sandbox_key_absent` | no | n/a | *(2.0-alpha.1+)* An arm of `sandbox_key_entry_failures`: the endpoint has no sandbox key at all, so the key route was never attempted. Neither the `Join` request nor the container inspect carried one; the PID route carries the attach. **Not observed on any measured host**, since the recovery cell requires every arm to be zero on the recovered instance, and published for the same reason `sandbox_key_wrong_ns_type` is. Split out of `sandbox_key_not_permitted` in 2.0-alpha.1, where an absent key was indistinguishable from the `--exec-root` case, whose remedy is a change to this plugin. |
| `sandbox_key_not_permitted` | no | n/a | (v2.0.0+) An arm of `sandbox_key_entry_failures`: a **non-empty** key did not name an entry of `/var/run/docker/netns` or `/run/docker/netns`, so it was refused instead of opened. **This is the arm that is not expected.** A daemon started with a non-default `--exec-root` publishes sandbox keys somewhere else and produces exactly this, with the same aggregate count as the ordinary case below, which is why the arms are published separately. The remedy for a rise here is a change to this plugin and never to your host. |
| `sandbox_key_not_a_namespace` | no | n/a | (v2.0.0+) An arm of `sandbox_key_entry_failures`, and **the expected one where `sandbox_netns_propagation` is `0`: it rises once per attach there, and stays flat where it is `1`.** The entry opened and was not a namespace. It is the ordinary empty file libnetwork creates before bind-mounting the sandbox netns over it, seen through a mount namespace that bind never reached. This row is the measured evidence for the reason [`SECURITY.md`](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md) gives for keeping the host PID namespace and `CAP_SYS_PTRACE`. |
| `sandbox_key_wrong_ns_type` | no | n/a | (v2.0.0+) An arm of `sandbox_key_entry_failures`: the entry was a namespace of some other type. Not observed on any host measured so far, and published for exactly that reason: "not observed" is a claim, and a claim needs something a reader can check. |
| `sandbox_key_unavailable` | no | n/a | (v2.0.0+) The residual arm of `sandbox_key_entry_failures`: the entry never became openable inside the attach budget, or its directory could not be read. It exists so the five arms sum to the aggregate instead of nearly summing to it. A refusal reaching this row is one nothing has named yet. |
| `sandbox_pid_fallbacks` | no | n/a | (v2.0.0+) Endpoints whose network namespace was entered through `/proc/<pid>/ns/net` after the key route was refused. **Where `sandbox_netns_propagation` is `0` this is every attach, and where it is `1` it stays at zero.** That route is why the manifest asks for the host PID namespace and `CAP_SYS_PTRACE`, and it carries the PID-recycling hazard `netns_pid_mismatches` counts. Not `healthy`-affecting, because a fallback that succeeds is a working endpoint, but read against `sandbox_key_entries` it is the one number that says which route your host actually uses. **Not a check:** both clauses fail. The imperative is to read it *against* `sandbox_key_entries`, and on a host whose `/run` is private its normal reading is one per attach, so a check firing on non-zero would fire on every healthy host of that kind. |
| `docker_api_non_get_refusals` | no | n/a | (v2.0.0+) Requests to the Docker API refused before they were sent because their method was neither `GET` nor `HEAD`. The plugin's whole Docker surface is `NetworkList`, `NetworkInspect`, `ContainerInspect` and the client library's version ping, so this stays at zero for the life of an installation; a non-zero value means code in this process tried to **write** to the daemon, which is the grant that makes a compromise of the plugin equivalent to root on the host (#691). Not `healthy`-affecting: the refusal is the safe outcome and the caller sees the error. |
| `ledger_write_failures` | no | warn | Failed `audit_log` ledger appends. It degrades forensics and never networking. Operators using `audit_log` alert on this. |
| `state_file_chmod_failures` | no | warn | (v2.0.0+) Files the startup sweep could not tighten under [`STATE_DIR`](#plugin-settings), plus one for a `STATE_DIR` that could not be read at all, in which case no file was examined (#804). Not healthy-affecting: a loose mode on a state file degrades nothing the plugin does, and the writer is root either way. **Worth investigating** whenever it moves: zero is the normal reading on every host, including one that has never been upgraded, so each tick is a file still readable by any user on the host. The plugin log names the path; `chmod 0600` it. A reading of 1 with no path in the log is the directory arm, and there the whole sweep did nothing. |
| `dhcpv6_config_only` | no | n/a | (v1.9.0+) DHCPv6 replies that carried configuration and no address, such as a stateless network answering an Information-request with DNS servers and a search list (#815). On a stateless segment this rising is the feature working. On an IPv4-only one it stays at zero. On a **managed** segment it is not necessarily zero either, and has not been since v2.1.1's library pin: where a server sends a Reconfigure naming Information-request, the bound client answers with one and its reply is counted here (#925). No fixture in this repository's test suite sends such a Reconfigure, so that arm is stated from the code and has not been observed on a segment. Not `healthy`-affecting. |
| `dhcpv6_not_offered` | no | n/a | (v1.9.0+) Endpoints created on an IPv6 network that offers no DHCPv6 address (#868): the segment answered with configuration and no address, or advertised with the managed flag clear. The endpoint starts without a DHCPv6 lease. Since v2.2.0 it also gets no IPv6 route and no global IPv6 address: the guard writes `accept_ra=0` and `autoconf=0`, so its kernel no longer supplies either, and the daemon refuses an IPv6 route on a link with no IPv6 address, so the plugin cannot supply them either. This is the ending for an `ipv6_mode` that takes its address from a DHCPv6 server, which is `ipv6=true` and `ipv6_mode=dhcp`: on `slaac` and `auto` the address is formed from the advertisement, and the route and the gateway arrive with it. Not `healthy`-affecting: this is a description of the segment and never a fault. Read it against `dhcpv6_no_router_advert`: the two exist to tell an advertised absence from an absent advertisement, and their sum would not. **Not a check:** both clauses fail. Its own value carries no verdict, because the same number is correct behaviour on a stateless or SLAAC network and a misconfiguration on one meant to be managed, and on a stateless or SLAAC network its normal reading is one per endpoint, so a check on non-zero would fire on every healthy container there. |
| `dhcpv6_no_router_advert` | no | n/a | (v1.9.0+) Endpoints created on an IPv6 network where **no router advertisement arrived at all** inside the acquisition budget (#868). The endpoint starts without a DHCPv6 lease, and the plugin logs a warning: unlike the row above this usually is a fault, because a segment with no IPv6 router gives the container no route either. Not `healthy`-affecting, because the plugin cannot tell a misconfigured segment from a deliberately routerless one. If it rises on a segment that does have a router, the advertisement did not arrive inside RFC 4861's discovery window. The DHCPv6 acquisition budget covers that window by derivation, so the usual cause is something cutting the budget below it: a `lease_timeout` set under 13 seconds, or a DHCPv4 half that took so long that little of the daemon's 30-second call deadline was left for the v6 one. The plugin logs a warning naming both numbers when that happens. |
| `dhcpv6_refused` | no | n/a | (v2.2.0+) Endpoints that **failed** because a DHCPv6 server answered and refused the client: a Status Code option carrying something other than Success (RFC 9915 §21.13, #816). The plugin logs the code's name beside the endpoint. Not `healthy`-affecting: the endpoint's failure is already reported to Docker, and the segment's DHCPv6 pool is not this plugin's health. Read it against `dhcpv6_no_server`, which is the ending where nothing answered at all: a refusal means a reachable, configured server with no address for this client, so the place to look is the server's range and its bindings. `NoAddrsAvail` is an exhausted pool; `NotOnLink` is an address requested outside the range the server serves, which a stale `preferred_ipv6` can produce. **Not a check:** the imperative above is to read this counter *against* `dhcpv6_no_server`, and its own value carries no verdict — one refusal on a segment whose pool is deliberately smaller than the container count is the server working as configured, and a `warn` check here would fire on it. |
| `dhcpv6_no_server` | no | n/a | (v2.2.0+) Endpoints that **failed** because the segment's router advertisement carried the managed-address flag and no DHCPv6 server answered inside the acquisition budget (#816). This ending is not new and its message has not changed; the counter is, so that it and `dhcpv6_refused` are two rows rather than one. Not `healthy`-affecting, for the same reason as the row above. If it rises on a segment that does have a server, check the budget first: the v6 half of `CreateEndpoint` is bounded by `lease_timeout` and by what the DHCPv4 half left of the daemon's own deadline, both described under `lease_timeout`. |
| `dhcpv6_slaac_no_prefix` | no | n/a | (v2.2.0+) Endpoints that **failed** on a network whose `ipv6_mode` takes the address from a router advertisement, because a router advertised and none of its prefixes formed an address (#816, #817). RFC 4862 §5.5.3 lists the reasons a Prefix Information option forms nothing: no Autonomous flag, a zero valid lifetime, a preferred lifetime past the valid one, a prefix length that with the 64-bit interface identifier does not total 128 bits, or the link-local prefix. Not `healthy`-affecting: the prefixes are the router's. It cannot move on an `ipv6_mode=dhcp` network, which forms nothing. |
| `dhcpv6_auto_fallbacks` | no | n/a | (v2.2.0+) Endpoints on an `ipv6_mode=auto` network whose address was formed from a router's advertised prefix because the segment advertised DHCPv6 and no server answered inside the fallback window (#817). The window is half the router-discovery window unless the network sets `ipv6_auto_strict=true`, which fails those endpoints instead. It counts addresses that **formed**, never fallbacks attempted: a fallback that found no usable prefix fails the endpoint and moves `dhcpv6_slaac_no_prefix`. Not `healthy`-affecting: the endpoint has an address, and what to look at is the segment's DHCPv6 server. A steady climb on a network you believe is managed means it is not answering. |
| `dhcpv6_slaac_no_address` | no | n/a | (v2.2.0+) Endpoints that **failed** on a network whose `ipv6_mode` takes the address from a router advertisement, where a router advertised and no address formed inside the acquisition budget (#818). Two things produce it: another node already holds the address this endpoint's prefix and MAC address form, and RFC 4862 §5.4.5 gives a node with a fixed interface identifier no second try after duplicate address detection fails; or the advertisement arrived too late in the budget for detection to finish. Not `healthy`-affecting: the endpoint's failure is already reported to Docker. Read it against `dhcpv6_slaac_no_prefix`, which is the ending where the advertised prefixes themselves formed nothing, and against `dhcpv6_no_server`, which needs a DHCPv6 exchange that `ipv6_mode=slaac` never has. A steady rise on one segment is worth an `ip -6 neigh` on the node that holds the address. **Not a check:** its own value carries no verdict. Every endpoint it counts has already failed with an error Docker shows, so a `warn` here would report the same event a second time, and on a segment where another node deliberately holds the address a container's prefix and MAC form, the normal reading is one per attempt. |
| `ipv6_slaac_addresses` | no | n/a | (v2.2.0+) IPv6 addresses formed from a router's advertised prefix and **installed on a container link** (#818). It counts addresses and not endpoints: RFC 4862 §5.5.3 forms one per autonomous prefix, so a single container on a link advertising a unique-local prefix and a global one raises it by two. It moves on the netlink call and not on the client forming an address, so it cannot report an address the container does not have. Not `healthy`-affecting: it is the success path. On an `ipv6_mode=slaac` or `auto` network it staying at 0 while containers start is the thing to look at, and the counters above say which ending they took instead. |
| `ipv6_addresses_withdrawn` | no | n/a | (v2.2.0+) IPv6 addresses **removed** from a container link because the lease stopped holding them (#819): a valid lifetime that ran out, or a prefix the router stopped advertising. The other half of `ipv6_slaac_addresses`; counting only arrivals reads as a container collecting addresses forever, and a renumbering is one of each. Not `healthy`-affecting: an address leaving at the end of its advertised lifetime is the protocol working. With `audit_log=true` the ledger's `withdrawn` row names which address and when. |
| `ipv6_slaac_prefixes_ignored` | no | n/a | (v2.2.0+) Advertised Prefix Information options **no address was formed from** (#818). RFC 4862 §5.5.3 gives the reasons: no Autonomous flag, the link-local prefix, a preferred lifetime past the valid one, a prefix length that with the interface identifier does not total 128 bits, a zero valid lifetime on a prefix not already held, and an address already found in use. The client's own cap of eight addresses per endpoint is counted here too, which is what separates an endpoint holding eight on a link advertising nine from one that found nothing to form from. Counted only where the `ipv6_mode` forms addresses: on `ipv6_mode=dhcp` the same rule refuses every autonomous prefix on every advertisement, correctly, and a router readvertises every few seconds (RFC 4861 §6.2.1), so the number would climb forever and mean nothing. Not `healthy`-affecting: a link advertising one prefix for DHCPv6 and one for autoconfiguration raises it on every advertisement and is configured exactly as intended. |
| `ipv6_main_prefix_unmatched` | no | n/a | (v2.2.0+) Endpoints whose network named an [`ipv6_main_prefix`](#driver-options-network-level) that none of the endpoint's addresses fell inside, so the first advertised prefix was reported to Docker instead (#819). One per endpoint, not one per advertisement. Not `healthy`-affecting and not a failure: the container holds every address the advertisement formed, and what differs is the single address `docker inspect` shows. The log line names the prefix asked for and the prefixes the endpoint has, which is usually enough to see that the router's list changed. |
| `ipv6_link_enable_failures` | no | n/a | (v1.9.0+) Container links the plugin could not administratively enable IPv6 on (#868). `Join` clears the engine's `disable_ipv6` on the container link before the DHCPv6 client starts. The engine sets that flag on a sandbox interface whose endpoint carries no IPv6 address, which is every endpoint at that moment, and this counter moves when that write fails. Degraded and not fatal, and not `healthy`-affecting: the v4 lease is worth more than the v6 half, so the manager logs and carries on. Reachable at plugin-restart recovery too, which replays the same start. When this rises, **no DHCPv6 exchange on that link was possible at all**, so the v6 counters below it say nothing about the segment. |
| `router_advert_guard_failures` | no | n/a | (v2.0.0+, #911; widened in v2.2.0, #821) Steps of the DHCPv6 Router Advertisement guard that did not take. The guard writes three sysctls on the container's link, `accept_ra=0`, `autoconf=0` and `keep_addr_on_down=1`, reads each one back, and then removes any route the kernel had already installed from an advertisement, so it is **six steps plus the route purge per IPv6 endpoint** and this counter is bounded by seven times the number of them. Only IPv6 endpoints run it, so it cannot move on an IPv4-only host. Not `healthy`-affecting and never fatal: a kernel built without one of these knobs, or a `/proc/sys` the plugin cannot write in, is not a reason to refuse the container an address. It **is** a reason to know, because the plugin puts the IPv6 gateway into the Join answer itself and a kernel still acting on advertisements adds a second default route beside it. If this rises, check `ip -6 route` inside the container: there should be exactly one default route, via an `fe80::` address, and none with `proto ra`. |
| `ipv6_router_withdrawn` | no | n/a | (v2.2.0+, #821) Container IPv6 default routes removed because the router that advertised itself set its Router Lifetime to 0. RFC 4861 §4.2 defines that lifetime as "the lifetime associated with the default router" and §6.3.4 reads 0 as "no longer to be used as a default router". Counts **routes removed, not advertisements received**, so a router sending several on its way down moves this once per container. Not `healthy`-affecting and not a fault: the containers on that segment are correctly left with no default route rather than one pointing at a router that is gone. If this moves unexpectedly, the segment's router restarted or was reconfigured; `ip -6 route show default` inside a container on it will be empty until a router advertises itself again. |
| `router_solicits_sent` | no | n/a | *(v2.2.0+, #814)* RFC 4861 §6.3.7 Router Solicitations sent from container links by the DHCPv6 client. Only IPv6 endpoints solicit, so it cannot move on an IPv4-only host. **Read `router_adverts_seen` against it:** a zero sighting count beside a zero solicitation count is a client that never asked, which is a different thing from a link whose routers are silent, and only one of the two is a segment to go and look at. |
| `router_adverts_seen` | no | n/a | *(v2.2.0+, #814)* Router Advertisements that decoded and reached the DHCPv6 client. This is the number that says an IPv6 segment is answering at all: the gateway, the MTU, the on-link prefixes, the more-specific routes and, on a stateless segment, the resolvers all come out of these frames, so a container that came up with none of them on a host reading zero here was on a link nothing advertised on. Not `healthy`-affecting: on a segment that is deliberately IPv4-only there is nothing to advertise. **The bound:** it counts frames that reached the client. A frame the kernel or the socket filter dropped before that is in none of these six counters. Counted across every DHCPv6 client this process ran, the `CreateEndpoint` one-shots included, so it does not fall when a container stops. |
| `router_adverts_refused` | no | n/a | *(v2.2.0+, #814)* Frames whose ICMPv6 type said Router Advertisement and which would not decode: a zero-length option, an option running past the end of the frame, a non-zero ICMP Code. Separate from `router_adverts_seen` because **their difference is the diagnostic**. A link with no router and a link whose router is advertising something this client refuses read the same in a total holding both, and the operator's next step differs: one is a router to find, the other is a router to fix. Not `healthy`-affecting: the plugin cannot tell a broken router from a frame something on the path corrupted, and containers on that segment may be getting everything they need from a second router that is fine. If it rises, capture on the segment and look at what the router is sending. |
| `router_advert_options_ignored` | no | n/a | *(v2.2.0+, #814)* Options inside an advertisement refused by that option's own standard, while the rest of the advertisement was read. It counts **options and not frames**, and it rises on advertisements that are otherwise fine, so it is not part of `router_adverts_refused`. Two things put entries in it: an option the decoder cannot read by its own rule (a Route Information Option whose length and prefix length contradict each other, RFC 4191 §2.3; a malformed RDNSS or DNSSL, RFC 8106 §5.1 and §5.2), and a value the client read and may not use, which today is a link MTU outside the range RFC 4861 §6.3.4 lets a host copy. Not `healthy`-affecting: a router offering one option nobody can use still supplies everything else in the same frame. |
| `router_table_entries_dropped` | no | n/a | *(v2.2.0+, #814)* Advertised entries a full list in the client's router table would not take: a ninth router, or a seventeenth more-specific route. Non-zero means **the table's caps are in force**, which on a segment carrying one link's worth of routers means something is advertising more than a link has. A refusal holds what was heard first, so what is lost is the newest arrival: RFC 4861 §6.3.4 says how many routers a host must be able to store and nothing about what it does when it will not store another, so this list refuses rather than evicting. Not `healthy`-affecting: the caps exist so that a link cannot spend this plugin's memory, and a refusal is them working. Read it with `router_table_entries_evicted`. |
| `router_table_entries_evicted` | no | n/a | *(v2.2.0+, #814)* Held entries a full resolver or search list threw out to take an arrival, which is what RFC 8106 §6.2 (d) asks of those two lists: "delete from the DNS Server List the entry with the shortest Expiration-time". The pair of `router_table_entries_dropped`, and the other way round: an eviction holds what expires **last**, so what is lost is something the client already had, and it comes back on its own router's next advertisement. Either counter above zero means the caps are in force. Not `healthy`-affecting. |
| `ifname_unsupported` | no | warn | *(v2.1.0+, #125)* Endpoints created with a custom interface name on an engine that does not apply one. Docker accepts the request and the container comes up on a working network. Only the name differs: the interface carries the driver's prefix and an index instead of the requested name. Engines below 29.8.0 ignore a remote driver's requested name, measured one engine line at a time; 29.8.0 carries the upstream fix and applies it. Not `healthy`-affecting. Watch it wherever the interface name matters: a non-zero value means someone asked for a name and the engine did not apply it, and nothing else on the host reports that. The remedy is an engine at 29.8.0 or newer, or a configuration that does not depend on the name. It counts nothing on an engine whose version the plugin could not read, so zero on such a host means the question went unasked. The test is the engine's reported version, not what the engine does: a vendor build carrying the change below 29.8.0 is counted here anyway, and a 29.8.0 build with the change removed is not counted at all. |
| `address_conflicts_v4` | no | n/a | (v2.0.0) The RFC 5227 share of `address_conflicts`: an IPv4 address ARP found in use, §2.1 before it was used or §2.4 afterwards. **This is the half to compare against `acd_probes_sent` and `acd_conflicts_detected`**, because both count ARP and the unsuffixed total carries the v6 share too, so comparing against the total reports a plugin defect for every DHCPv6 conflict. `conflict_check` governs this half. |
| `address_conflicts_v6` | no | n/a | (v2.0.0) The DHCPv6 share: an address the container's kernel found on the link by Duplicate Address Detection (RFC 4862 §5.4), declined to the server under RFC 9915 §18.2.8. It is **not ARP**: no `acd_*` counter moves for it and `conflict_check` does not govern it, because DAD is the kernel's, runs on every IPv6 address, and cannot be turned off from a network option. The client then acquires a replacement address and the plugin applies it to the container's interface, while **Docker's record of the endpoint keeps the old address**, the same truthfulness gap `lease_changed` describes, so read `lease_changed_v6` beside this. |
| `lease_changed_v6`, `leases_obtained_v6`, `leases_renewed_v6`, `renewals_unanswered_v6`, `dhcp_timeouts_v6`, `naks_received_v6` | no | n/a | (v1.2.0+) The IPv6-only share of the matching counter above (#212). Each counts only the v6 client's events. On a dual-stack host this isolates the v6-specific NAK/timeout signal the combined number hides. `client_stop_failures_v6` (v1.7.0+, #608) joins the split with the same rule; `ledger_write_failures` has no per-family split. |
| `lease_changed_v4`, `leases_obtained_v4`, `leases_renewed_v4`, `renewals_unanswered_v4`, `dhcp_timeouts_v4`, `naks_received_v4`, `client_stop_failures_v4` | no | n/a | (v1.8.0+, #730) The IPv4-only share, on the same rule. Both halves are now stored and the unsuffixed counter is their **sum**. It is not a counter in its own right and nothing increments it. Before v1.8.0 the v4 share was not stored: it was recovered as `aggregate − *_v6` at render time, which could read one lower than the previous scrape and make Prometheus treat the whole counter as reset. Use `*_v4` instead of doing that subtraction yourself. |
| `releases_sent_v4`, `releases_sent_v6`, `release_failures_v4`, `release_failures_v6` | no | n/a (`release_failures`: warn) | **(v2.1.1, #962)** The per-family shares of the two `release_lease` counters above, on the same rule: both halves are stored and the unsuffixed counter is their sum. Read them separately, because the two families release differently. IPv4 keeps its address on the link and names the binding in `ciaddr` (RFC 2131 section 3.1(6)); IPv6 must take the address off the link before the exchange may begin (RFC 9915 section 18.2.7), so a dual-stack endpoint can hand one address back and fail to hand the other back in the same teardown. All four stay at zero on a network that does not set `release_lease`. |
| `releases_reclaimed_v4`, `releases_reclaimed_v6` | no | n/a | **(v2.2.0, #984)** The per-family shares of `releases_reclaimed`, on the same rule and with the same narrow meaning: a running container on the address, not every held address that was not handed back. Both halves are stored and the unsuffixed counter is their sum. They are separate for the reason the pair above is: the two families are two records with two deadlines, so a restarting container can claim one address back inside the window and take a fresh one of the other family. Both stay at zero on a network that does not set `release_lease=on_remove`. |

### `/metrics`

*(v1.8.0+)* The same counters as `/Plugin.Health`, in Prometheus text
exposition format. Both views render from one snapshot, so they cannot
disagree, and a unit test asserts by reflection that **every**
`/Plugin.Health` field is exposed here, so a counter added later cannot
quietly go missing from your dashboards.

On the plugin socket, always:

```bash
PLUGIN_ID=$(docker plugin inspect -f '{{.Id}}' ghcr.io/claymore666/docker-net-dhcp:v2.2.3)
sudo curl -s --unix-socket /run/docker/plugins/$PLUGIN_ID/net-dhcp.sock \
    http://localhost/metrics
```

Prometheus cannot scrape a UNIX socket, so for an actual scrape target
set `METRICS_ADDR`:

```bash
PLUGIN=ghcr.io/claymore666/docker-net-dhcp:v2.2.3
docker plugin disable "$PLUGIN"
docker plugin set "$PLUGIN" METRICS_ADDR=127.0.0.1:9099
docker plugin enable "$PLUGIN"
```

> **This opens a port in the host's network namespace.** The plugin runs
> with `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE` and
> `"network": {"type": "host"}`, so a listener it opens is on the host
> directly and never in a container namespace. Bind loopback or a
> management interface; do not bind `0.0.0.0` on a machine reachable
> from anywhere you do not control. The listener serves `/metrics` **and
> nothing else**: the libnetwork RPCs are not routed on it, and a test
> asserts that. An open port is still an open port. It is off by default
> so that enabling it is a decision instead of something inherited from
> an upgrade.

#### Metric names and the `family` label

Counters are `net_dhcp_<name>_total`, gauges are `net_dhcp_<name>`.

Eleven counters carry a `family` label: `leases_obtained`,
`leases_renewed`, `lease_changed`, `renewals_unanswered`,
`dhcp_timeouts`, `naks_received`, `client_stop_failures`,
`address_conflicts`, `releases_sent`, `release_failures` and
`releases_reclaimed`:

```
net_dhcp_leases_obtained_total{family="ipv4"} 42
net_dhcp_leases_obtained_total{family="ipv6"} 7
```

**Both family series are stored and neither is derived** (v1.8.0+, #730).
Each of the eleven has a `_v4` and a `_v6` field in
`/Plugin.Health`, and the unsuffixed counter is their **sum**. So
`leases_obtained` equals `leases_obtained_v4 + leases_obtained_v6`, and
the `family="ipv4"` series is the `_v4` field read straight out. Every
other counter has no family dimension and does not gain an invented one.

Until v1.8.0 only the aggregate and the `_v6` share were stored, and the
v4 series was computed as `total - v6` at render time. Two independently
updated counters combined by subtraction can be read in an order that
yields a value **below the previous scrape**, and Prometheus reads any
counter decrease as a reset, attributing the whole accumulated value as
an increase on the next scrape. A one-off skew of a single event
therefore showed up as a rate spike of the entire count. Adding two
monotonic counters has no such failure mode; subtracting them does.

If you have a dashboard or recording rule that reconstructed the v4
share as `leases_obtained - leases_obtained_v6`, it still gives the same
answer, but prefer `leases_obtained_v4`: the subtraction is what this
change removed, and doing it in the query reintroduces it.

#### Counter resets

`net_dhcp_build_info` carries the plugin's `instance_id` as a label,
and since 2.0-alpha.1 the build identity beside it:

```
net_dhcp_build_info{instance_id="...",version="...",commit="...",library="..."} 1
```

None of the four is ever empty. `dev` and `unknown` are what a build
that does not know says, because an empty label reads as "nothing to
report". The series is a gauge with the value 1 and no `family` label:
it exists for its labels.

The counters are process-lifetime and reset when the plugin restarts.
Because the id changes with the process, a restart appears to Prometheus
as a **new series** instead of as a counter that silently rewound, which
`rate()` already handles correctly. This is the one place the metrics
view is strictly better than reading the JSON, where an operator has to
compare `instance_id` by hand to know whether two readings are
comparable at all.

#### Engine identity

*(v2.1.0+)* `net_dhcp_engine_info` carries the daemon's identity as
labels, in the same shape as `net_dhcp_build_info`:

```
net_dhcp_engine_info{engine_version="...",api_version="..."} 1
```

It is a separate series from `net_dhcp_build_info` on purpose. The build
identity changes when the plugin is upgraded, the engine identity when
the host's Docker is, and folding both into one series would break every
`net_dhcp_build_info` time series on an engine upgrade. Both labels read
`unknown` when the daemon did not answer at start-up.

`net_dhcp_healthy` is `1`/`0`, mirroring the `healthy` field, so the one
derived judgement the plugin makes stays alertable.
`net_dhcp_health_status` (2.0-alpha.1+) is the same judgement one step
finer: `0` pass, `1` warn, `2` fail, ordered so that worse is higher. `>
0` is the alerting expression and `>= 2` is exactly the subset
`net_dhcp_healthy` already carried. It latches for the life of the
process for the same reason `healthy` does; read `net_dhcp_build_info`'s
`instance_id` to tell a fault this process recorded earlier from a new
one, and the health document's `checks` to see WHEN the counter behind
it last moved.

#### Not exposed

Per-endpoint and per-network labels. Endpoint IDs are unbounded and turn
over with container lifecycle, so labelling by them would be a
cardinality problem in exactly the deployments where these metrics
matter most. Aggregates only.

### Verifying that renewal works

The most common question after a deployment, *is this container's lease
actually being renewed?*, has a non-obvious answer, because **a clean
renewal logs nothing**. It is emitted at `Debug` and the plugin runs at
`info`, so a silent log is correct behaviour and never evidence of a
problem.

`leases_renewed` on `/Plugin.Health` is the cheap proof. It should be
non-zero once the first renewal is due, with `naks_received`,
`renewals_unanswered` and `dhcp_timeouts` still at zero.

`renewals_unanswered` is the one to read when `leases_renewed` has not
moved and the renewal is overdue. It counts renewal requests the server
did not answer, and it moves at the first retransmission, while
`dhcp_timeouts` waits for the lease to lapse. How long that is depends
on the lease: the wait before a retransmission is half the time left to
T2, floored at 60 seconds, so it is a minute on a short lease and hours
on a long one. `renewals_unanswered` rising with `leases_renewed` flat
is a server that has gone quiet. Both flat with the renewal overdue is a
client that has not asked yet, so check the lease time rather than the
server.

*When* it is due comes from the lease and never from any plugin setting:
DHCP option 58 (T1), typically half the lease time.

**In 2.0 there is no lease file to read.** The client is a goroutine
inside the plugin process and keeps its lease in memory, so the 1.x
recipe of finding the external client process, entering its private
mount namespace and decoding its on-disk lease file has nothing to point
at. Nothing on this branch exports T1 or the granted lifetime to the
operator; that is a gap, named here instead of papered over. What is
available:

- **`leases_renewed` on `/Plugin.Health` or `/metrics`**, as above. A rise
  is the renewal; its timing against the bind is the observation.
- **`audit_log=true`** for a durable record instead of a counter. It
  writes a `bound` line and a `renew` line per event, with a timestamp,
  so the interval between two of them measures T1 from the outside.
- **The DHCP server's own lease table**, which is the authority on what it
  granted and is the only place the number itself is readable.

### Plugin log

```bash
sudo cat /var/lib/docker/plugins/*/rootfs/var/log/net-dhcp.log
```

Raise verbosity with a disable, a set, and an enable, in that order,
because `docker plugin set` is refused while the plugin is running:

```bash
PLUGIN=ghcr.io/claymore666/docker-net-dhcp:v2.2.3
docker plugin disable "$PLUGIN"
docker plugin set "$PLUGIN" LOG_LEVEL=trace
docker plugin enable "$PLUGIN"
```

**That file does not survive an upgrade.** It lives in the plugin
rootfs, which Docker destroys and recreates on `docker plugin rm` /
`install`, the supported upgrade path, so the previous version's history
is gone at exactly the point you are most likely to want it.

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
where the ledger lands any more. The mount is applied in the plugin's
own mount namespace, so following that path from the host finds a mount
point at best and, more usually, nothing at all:

```bash
sudo cat /var/lib/net-dhcp/leases.jsonl | jq .
```

One JSON object per line; kinds `bound`, `renew`, `stopped`,
`stop_failed`. `stopped` means the renewal client was asked to stop and
its goroutine returned; `stop_failed` means it returned an error other
than the cancellation, or did not return inside the finish timeout.
Neither says anything about the lease. On a `release_lease=never`
network, which is the default, the plugin sends no DHCPRELEASE at all
and the address is held until it expires (#800). On a
`release_lease=on_stop` network the release is attempted at Leave,
before the client is stopped, and it is reported in `releases_sent_v4`,
`releases_sent_v6` and their failure pair, never here (#962). On a
`release_lease=on_remove` network the attempt is made later still, on
the sweep that follows the end of the restart window, long after the
client is gone, and it is reported in the same counters plus
`releases_reclaimed` for the addresses a running container is using again
when the window ends (#984). The kinds
were `release` and `release_failed` before v1.9.0, and were renamed
instead of kept: the ledger must never assert something the server did
not see.

---

## Compose usage

Recommended shape: network created once out-of-band, referenced as
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
    driver: ghcr.io/claymore666/docker-net-dhcp:v2.2.3
    driver_opts:
      mode: macvlan
      parent: eth0
      propagate_dns: 'true'
    ipam:
      driver: 'null'
```

Multi-network containers work. One plugin network per container is the
*supported* shape; several attach, but interface naming order is
engine-determined below engine 29.8.0, which is the measured boundary
for moby's remote-driver `interface_name` pass-through. See the
`com.docker.network.endpoint.ifname` row above.

### The base/override merge trap

Compose merges the top-level `networks:` map **key by key** and never
file by file, and this bites a common deployment shape: built-in macvlan
in dev, an external pre-created DHCP network in prod.

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

The merged result is a hybrid, `external: true` **and** `driver:
macvlan` **and** `ipam.config`, matching neither the pure-external
attach contract nor the internal-create one. Compose then **silently
skips** attaching the service to `lan`; the container comes up on
whatever other networks it lists, with no error.

Diagnose with `docker compose -f docker-compose.yml -f
docker-compose.prod.yml config` and read the merged `networks.lan` block.
`external: true` sitting next to `driver` or `ipam` means you've hit it.

The plugin cannot influence Compose's merge logic, so the fixes are
consumer-side:

- **Best.** Don't define `lan` in the base file at all. Put the dev
  definition in `docker-compose.dev.yml` and the prod one in
  `docker-compose.prod.yml`, so each file *replaces* the key outright.
- **Acceptable.** Keep the base, but null out every key it sets in the
  override (`driver: null`, `driver_opts: null`, `ipam: null`) alongside
  `external: true`. Brittle: each new base key needs a matching null.
- **Escape hatch.** `docker network connect lan-shared <container>`
  after `compose up`. One-shot; does not survive a recreate.

---

## Troubleshooting

| symptom | likely cause | fix |
| ------- | ------------ | --- |
| `docker run` hangs then fails with a lease timeout | No DHCP reply on the parent L2 (isolated NIC, firewall on UDP 67/68, wrong VLAN). Since v1.3.4 transient failures are retried within `lease_timeout`, so reaching the timeout points at a persistent problem and never a one-off blip | Verify with `-o validate_dhcp=true` at create time; check the parent's connectivity; raise `-o lease_timeout` for slow/relayed networks |
| `invalid rootfs in image configuration` at install | Old Docker engine | Upgrade Docker |
| Network create fails `Bridge already in use` | Another Docker network owns the bridge | Use a dedicated bridge, or `-o ignore_conflicts=true` if the detection is wrong |
| Bridge mode: containers time out getting a lease, but the host itself has one | STP is enabled on the bridge, so each new container `veth` waits out two forwarding delays (~30s) before it forwards | Check `ip -d link show <bridge> \| grep -o 'stp_state [0-9]*'`; see [Leave STP off](bridge-mode.md#leave-stp-off-unless-you-need-it) |
| Bridge mode: everything works until the host reboots, then no container can attach | The bridge was created with imperative `ip link` commands, which do not persist | Use one of the [persistent stanzas](bridge-mode.md#make-the-bridge-persistent) for your distro |
| Container has an IP but `docker inspect` shows a different one | Mid-life re-acquisition after NAK/lease change | Expected degraded mode; watch `lease_changed` on `/Plugin.Health`; restart the container to resync Docker's view |
| Container starts and gets an address, but traffic to it is intermittent or lands on the wrong machine | Another device on the segment already holds that address. Usually a **statically configured** host inside the DHCP pool range: it never asks the server for anything, so the server cannot know the address is taken and hands it out | Since 2.0 the plugin detects this itself (RFC 5227) and declines the address, so the first thing to check is whether it is looking: read `acd_probes_sent` on `/Plugin.Health` **before** `address_conflicts_v4`, because with no probes the two readings are identical. Zero probes means either every network on this host runs `-o conflict_check=off` or the check has stopped working; `acd_arp_send_failures` climbing means probes are being refused by the socket, which turns "no conflict found" into "the question was not asked". If the plugin IS finding conflicts, `address_conflicts` moves, `healthy` goes false, and your DHCP server's log shows the DHCPDECLINE with the address on it. The conflict itself is fixed at the DHCP server (reserve or exclude the address) and never at the plugin |
| Container on an `ipv6=true` network starts, but has no `GlobalIPv6Address` | The segment offers no DHCPv6 address, which is normal on a stateless or SLAAC network and is not an error. Which of the two it is: `dhcpv6_not_offered` means a router advertised and offered none, `dhcpv6_no_router_advert` means nothing advertised at all | Read the two counters on `/Plugin.Health` before changing anything. `dhcpv6_not_offered` on a segment you believe is managed means the router's advertisement has the managed flag clear. Fix it at the router. `dhcpv6_no_router_advert` on a segment that does have a router means the DHCPv6 budget was cut below RFC 4861's router-discovery window (about 13 seconds), either by a `lease_timeout` set under it, or by a slow DHCPv4 half leaving too little of the daemon's 30-second call deadline behind; the plugin logs a warning naming both numbers. In both cases the container has no IPv6 route and no global IPv6 address since v2.2.0: `accept_ra=0` and `autoconf=0` mean its kernel supplies neither, and the daemon refuses an IPv6 route on a link carrying no IPv6 address, so the plugin cannot supply one either. That is this row's `ipv6=true` network: on `ipv6_mode=slaac` and `ipv6_mode=auto` the address comes from the advertisement and the route comes with it (#821, #818) |
| Container on an `ipv6=true` network has an IPv6 address but cannot reach anything off-link | No default route. DHCPv6 carries no next hop, so the route comes from a Router Advertisement, which the plugin reads and puts into the Join answer (v2.2.0+, #821) | `ip -6 route show default` inside the container: a route `via` an `fe80::` address is what should be there. If it is empty, check `ipv6_router_withdrawn` on `/Plugin.Health` (the segment's router withdrew itself) and then `router_advert_guard_failures`. A zero for both with no route means nothing advertised |
| Container on an `ipv6=true` network has TWO IPv6 default routes | The guard did not take, so the container's kernel installed one from the advertisement beside the plugin's (v2.2.0+, #821) | `ip -6 route show default` inside the container: one of them carries `proto ra`, which is the kernel's. Check `router_advert_guard_failures` on `/Plugin.Health`; non-zero means the plugin could not write `accept_ra`, `autoconf` or `keep_addr_on_down` on the link, or could not remove a route the kernel had already installed |
| Container on an `ipv6=true` network cannot resolve names through a `fe80::` resolver | The resolver line carries an RFC 4007 §11 scope zone, `nameserver fe80::1%eth0`, which musl (Alpine) does not parse | Use an image built against glibc, or configure the segment to advertise a global-scope resolver. The plugin writes the zone because without it the address does not work under either C library |
| Container on a dual-stack network has an MTU smaller than either server asked for | Both families write the same link MTU, and since v2.2.0 the link takes the smaller of the two (#821). The larger value is a promise the link cannot keep for the family that asked for the smaller one | `ip link show` inside the container against the DHCPv4 option-26 value and the advertised MTU. Before v2.2.0 the two flipped the link back and forth, once per renewal of either family. A network that sets `mtu` applies neither value (#1037) |
| Container on an `ipv6=true` network has a smaller MTU than expected on **IPv4** too | The segment's advertisement carried an MTU option, and the plugin applies it to the container's link, which bounds both families (v2.2.0+, #821) | `ip link show` inside the container against the advertised value. This is not governed by `propagate_mtu`: until v2.2.0 the container's kernel applied the advertised MTU on every IPv6 network and the plugin keeps that behaviour |
| Container on an **ipvlan** network got a new IPv6 address after upgrading from 1.x | Expected, once. 1.x derived the DHCPv6 DUID from the MAC, and ipvlan slaves share the parent's MAC, so every container on the network presented one identity. 2.0 gives each endpoint its own | The new address is stable from here on. If you reserve v6 addresses server-side, re-key the reservation on the new DUID; see [DHCPv6](#dhcpv6-ipv6true) |
| `docker run` or `docker network connect` fails with `require_mac is set on this network` | The network was created with `require_mac=true` and the container has no MAC of its own (#1036) | Set `--mac-address` or Compose `mac_address`. The `docker network connect` command line cannot set one: recreate the container with the network in its definition. After a refused connect, the container's next restart, manual or by its restart policy, fails the same way and leaves it stopped: `docker network disconnect` that network while the container is stopped (a running one is refused as not connected), then `docker start` |
| `--mac-address` fails on an ipvlan network | ipvlan children share the parent MAC (kernel design) | Use `mode=macvlan`, or drop the custom MAC |
| Reservations don't stick on ipvlan | DHCP server keys on MAC only, ignores option 61 | Use `mode=macvlan`, or configure the server to honor client identifiers |
| One container on two plugin networks fails to start with `cannot program address ... conflicts with existing route` | The two networks lease from **overlapping** subnets, and libnetwork refuses to program a second sandbox address in a subnet the container already routes. Overlapping and never identical: the upstream check is containment in either direction, so `10.0.0.0/8` on one network and `10.1.2.0/24` on another are different subnets and still collide. **Which modes reach this.** In `mode=macvlan` and `mode=ipvlan` nothing stands in the way: two networks on one parent NIC are one LAN with one DHCP server, which is exactly this case. In **bridge mode**, the default when `mode` is unset, two networks on the *same* bridge are refused earlier and with a different message, at `docker network create` (see the `Bridge already in use` row above), so you never get as far as starting a container. If you are seeing *this* error in bridge mode, it is one of two things: the two networks sit on *different* bridges whose subnets overlap (the create-time guard keys on the bridge **name** and never on the subnet), or you set `-o ignore_conflicts=true`, which skips that guard and is what allowed the pair to be created. Measured identical on two daemons differing only in [moby/moby#52866](https://github.com/moby/moby/pull/52866), each with and without the endpoint interface-name option, so four cells and one error. That is the sample; it is not a claim about engines nobody has run, and the integration test pins it so a future engine that behaves differently shows up as a failure here instead of as a stale sentence | Not a plugin setting and not fixable here. Put the two networks on **non-overlapping** subnets, different *and* with neither one containing the other (different parent NICs / VLANs, each with its own DHCP scope); "different subnets" alone is not enough, since a supernet and a range carved out of it are different and still conflict. Or attach the container to one network only. In bridge mode, if you reached this through `-o ignore_conflicts=true`, drop that option and let the create-time guard refuse the pair up front, where the message names the real problem. Note the container **takes a real lease per network before it fails**, because the plugin leases in `CreateEndpoint`, before libnetwork gets as far as refusing, and on `never` and `on_stop` **nothing releases them**: the addresses are leased before the endpoint exists, so no `Leave` ever runs for them and those two values cannot reach them (see [How a lease gets handed back](internals.md#how-a-lease-gets-handed-back), #800, #962), so those addresses stay leased until the server expires them. On `release_lease=on_remove` they are reached (#984): the failed start still ends in a `DeleteEndpoint`, which retains each record with a deadline, and the addresses go back about a minute later. A repeatedly retried start therefore consumes the pool at one address per network per attempt. If addresses are scarce, shorten the lease time on the server or reserve the range, and do not wait for the plugin to hand them back |
| Container can't reach the Docker host (or vice versa) | macvlan/ipvlan kernel rule: children can't talk to the parent NIC's host IP | Bridge mode, or a second NIC; this is not a plugin setting |
| `healthy: false` on `/Plugin.Health` | Exactly five counters flip it, and they call for different action: `recovery_failed`, `join_start_failures`, `tombstone_write_failures`, `tombstone_quarantines`, `address_conflicts` | Read the five in the field table above to see which one moved, because the flag alone does not say. `recovery_failed` / `join_start_failures`: restart the affected containers. `tombstone_write_failures`: check space and writability on the filesystem holding [`STATE_DIR`](#plugin-settings), the host's `/var/lib/net-dhcp` since v1.5.0 and the plugin rootfs before that. `tombstone_quarantines`: read the `tombstones.json.corrupt-<timestamp>` file that was left in `STATE_DIR`, then check the same filesystem, since nothing reaps that file and it is the only record of what was lost. `address_conflicts`: the lease collided with a host already using that address, so the fault is on the DHCP server or the segment and never the plugin. **Doing any of this will not clear the flag**, because the counters are monotonic and `healthy` latches for the life of the plugin process. Only restarting the plugin resets it, which tears down every managed endpoint's renewal client on the host, so it is not a step to take just to silence the flag. Compare *instance_id* across reads to tell a still-latched process from a new one that has already gone bad |
| Container came back on a **different IP** after a plugin upgrade | Recreating the network minted a new child MAC; the server keys the old lease to the old MAC and declines the re-request | Expected; see the callout under [Upgrade](#install-upgrade-uninstall). Pin the endpoint MAC and reserve it server-side to make the address survive future upgrades |
| `/Plugin.Health` prints nothing and exits 7 | `curl` run without `sudo`; `/run/docker/plugins` is root-only, and `-s` hides `curl: (7) Failed to connect` | Re-run with `sudo`; see [`/Plugin.Health`](#pluginhealth) |
| `leases_renewed` still 0 and the log looks empty | Probably nothing; clean renewals log at `Debug`, and T1 may not have arrived | [Verify renewal properly](#verifying-that-renewal-works): read T1 from the lease, then re-check the counter |
| Compose doesn't attach the container to the DHCP network, with no error | Base/override merge produced a hybrid network definition | [The base/override merge trap](#the-baseoverride-merge-trap) |
| `docker plugin disable` refuses | Networks still reference the plugin | `docker network rm` them first |
| Renewals failing after a server outage | n/a | Containers keep their address and the client keeps retrying on its retransmission schedule; `renewals_unanswered` climbs from the first retransmission, which is where the outage becomes visible, while `dhcp_timeouts` waits for the lease to lapse, and `leases_renewed` resumes after the server returns. How far apart the two are depends on the lease: half the time left to T2, floored at 60 seconds |

Operator-side release/publishing issues (registry auth, Hub tokens)
are covered in the maintainer-facing
[`release-runbook.md`](release-runbook.md#troubleshooting).
