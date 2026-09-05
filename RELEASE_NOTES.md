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

## Acknowledged findings (not applicable to this fork)

The third-pass code review of v0.5.3 ran `govulncheck` and surfaced
two informational findings on `github.com/docker/docker`:

- **GO-2026-4887** — Moby AuthZ plugin bypass on oversized request bodies.
- **GO-2026-4883** — Moby off-by-one in plugin privilege validation.

Both are **daemon-side** vulnerabilities. This plugin uses
`github.com/docker/docker` only as a client library — the call sites
are `NetworkList`, `NetworkInspect`, and `ContainerInspect`. Neither
vulnerable code path is reachable from the plugin process, so no
action is required. Recorded here so future audits don't
re-investigate. (Original report: third-pass code review, 2026-05-04.)

The v1.1.0 Dependency Review gate (#193) surfaced two further
`github.com/docker/docker` advisories at v28.5.2:

- **GHSA-x86f-5xw2-fm2r** — `PUT /containers/{id}/archive` executes a
  container binary on the host.
- **GHSA-rg2x-37c3-w2rh** — race condition in `docker cp` allows
  bind-mount redirection to a host path.

Both are `docker cp` / archive-extraction paths in the Moby daemon and
CLI. The plugin invokes none of them — its only client calls remain the
three inspect/list APIs above. No fix is published on the frozen
`github.com/docker/docker` module path (successor `moby/moby/v2`
migration tracked in #178). They are accepted at the advisory level in
`.github/dependency-review-config.yml` with a review date, alongside the
older AuthZ finding (GHSA-x744-4wpc-v9h2 = GO-2026-4887), and will be
re-evaluated when the module migration lands.

During the v1.3.0 window (2026-06-28) the Go vulnerability database
imported these advisories, so `govulncheck` now reports them through
module-init symbol traces — the assessment above is unchanged, but
"govulncheck is green" no longer holds. All three (GO-2026-5746 =
GHSA-x86f-5xw2-fm2r, GO-2026-5617 = GHSA-rg2x-37c3-w2rh, plus the
newly published GO-2026-5668 = GHSA-vp62-88p7-qqf5, a `docker cp`
symlink-swap empty-file race, also daemon-side) are accepted with
justification in `.github/vuln-allowlist.txt` (#291, #292) until a
fixed release ships on the module path we depend on.

As of v1.3.4 (#333), GO-2026-5746 is no longer reported by
`govulncheck` and its allowlist entry has been removed — the
assessment above is retained here as the audit trail. If it becomes
reachable again the gate fails loudly rather than silently
re-accepting it.

## v2.0.0 (unreleased)

Pre-releases of this version: `v2.0.0-rc1` (2026-09-05, IPv4 only).

The plugin performs the DHCP exchange itself, through an in-tree Go library,
instead of driving an external client process. The image contains no DHCP
client and the plugin execs nothing. **This release is IPv4 only**; DHCPv6
returns in a later 2.0 milestone, and the 1.x line is where it works today.

Everything below is a change against v1.9.0.

### Upgrade notes

Required on every host before `docker plugin install`, unchanged since v1.5.0:

```bash
sudo mkdir -p /var/lib/net-dhcp
```

**The upgrade re-prompts for privileges, and you must approve it.** The
plugin manifest now requests a fourth Linux capability, `CAP_NET_RAW`,
alongside `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE`. It is
requested because the plugin now opens the packet socket that carries the
DHCP exchange, which the external client used to open for it. `docker plugin
upgrade` shows the new list and waits; an unattended upgrade that does not
pass `--grant-all-permissions` stops there.

**The capability the plugin can exercise does not change.** Docker composes a
plugin's requested capabilities *additively* over the OCI default set, and
`CAP_NET_RAW` is already in that default set, so the effective capability set
of the plugin process is the same seventeen it was before. What changed is
the manifest, and therefore the prompt: the request is now honest about a
power the process already had. See `SECURITY.md`.

**The manifest delta against v1.9.0, field by field.** `docker plugin
upgrade` prompts on the *privilege* fields only, so the two halves of this
table are read differently: the first is what the prompt shows you, the
second is not prompted at all.

Every cell below is the full set for that field, not a description of how
it changed. `scripts/check-manifest-delta-table.sh` derives both columns —
the left from `v1.9.0:config.json` in git, the right from the manifest in
the tree — and fails if either disagrees with what is written here. The
`prompted` column is not derived: which fields the daemon prompts on is a
property of Docker, not of this manifest.

<!-- manifest-delta: begin baseline=v1.9.0 -->

| field | v1.9.0 | v2.0.0-alpha.1 | prompted |
| --- | --- | --- | --- |
| `linux.capabilities` | `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE` | `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE` | **yes** |
| `network.type` | `host` | `host` | no change |
| `ipchost` | `false` | `false` | **yes**, when true |
| `pidhost` | `true` | `true` | no change |
| `mounts` | `/var/run/docker.sock:bind`, `/var/lib/net-dhcp:rbind,rw`, `/var/run/docker:rbind,ro` | `/var/run/docker.sock:bind`, `/var/lib/net-dhcp:rbind,rw`, `/var/run/docker:rbind,ro` | source only |
| `propagatedmount` | `(absent)` | `(absent)` | no change |
| `linux.devices` | `(absent)` | `(absent)` | no change |
| `linux.allowalldevices` | `false` | `false` | **yes**, when true |
| `env` | `LOG_LEVEL`, `AWAIT_TIMEOUT`, `STATE_DIR`, `OUTAGE_TICK`, `OUTAGE_GRACE`, `METRICS_ADDR` | `LOG_LEVEL`, `AWAIT_TIMEOUT`, `STATE_DIR`, `METRICS_ADDR`, `DOCKER_HOST` | no — a setting is not a privilege |

<!-- manifest-delta: end -->

Read the `env` row against **Removed: plugin settings** below: the delta
is one added and two removed, not one added. `OUTAGE_TICK` and
`OUTAGE_GRACE` are gone from the manifest, so `docker plugin set
OUTAGE_TICK=…` is refused by the daemon rather than accepted and ignored.

**Nothing was dropped, and the measurement says why.** The beta asks first
for the sandbox key the daemon publishes, so that a host where that works
can attach without the container's PID at all. For an attach it does not
work: the plugin's read-only `/var/run/docker` is a bind mount taken when
the plugin starts, so it never receives the per-sandbox namespace mounts the
daemon makes afterwards, the key is refused, and `/proc/<pid>/ns/net` carries
the attach exactly as before. Recovery after a plugin restart is the
exception — the sandbox is older than the plugin process, so the key route
carries it. `pidhost` and `CAP_SYS_PTRACE` therefore stay, and would have
stayed regardless, because `resolv.conf` propagation enters the container's
*mount* namespace by PID and a mount namespace has no sandbox key.
`sandbox_key_entries`, `sandbox_key_entry_failures` and
`sandbox_pid_fallbacks` report which route your host is using, and
`sandbox_key_absent`, `sandbox_key_not_permitted`,
`sandbox_key_not_a_namespace`, `sandbox_key_wrong_ns_type` and
`sandbox_key_unavailable` say which
refusal it was — the arms sum to `sandbox_key_entry_failures`. On a stock
engine the one that rises is `sandbox_key_not_a_namespace`, once per
attach, and that is the expected state: nothing is degraded, the log line
that accompanies it is at `debug`, and no action is indicated. A rise in
`sandbox_key_not_permitted` is the one to look at — it means the daemon
publishes sandbox keys somewhere this plugin does not accept, which a
non-default `--exec-root` does. It means a key that exists and was
refused: an endpoint no key was published for at all has its own arm,
`sandbox_key_absent`, so a rise in one is never the other.

**New: `DOCKER_HOST`.** Empty by default, which keeps the mounted socket and
the behaviour every earlier release had. Point it at a read-only Docker API
proxy — a plain TCP endpoint on the host's loopback, which the plugin reaches
through host networking — and the plugin loses nothing: it issues only `GET`
and `HEAD`, refuses anything else before sending it, and counts each refusal
as `docker_api_non_get_refusals`. The allowed paths, a worked example, and
why a proxy on its own unix socket is *not* reachable are in `SECURITY.md`.

### IPv4 only

| change | effect |
| --- | --- |
| `docker network create … -o ipv6=true` | **Refused**, in every mode, with an error naming the beta. The refusal is keyed on the decoded option, so `ipv6`, `IPv6` and `Ipv6` are all refused |
| `--ip6` / `Interface.AddressIPv6` on an endpoint | Ignored. There is no DHCPv6 exchange to carry a preferred address |
| `docker network create --ipv6` | Unchanged: Docker's own flag does not work with the null IPAM driver, and never did |

**A network created by a 1.x build with `ipv6=true` survives the upgrade, and
its containers stop working.** Nothing rewrites a stored network record and
the refusal above runs at `CreateNetwork`, which an existing network does not
go through again. What the plugin does with such a record, read from the tree
rather than from intent:

- `CreateEndpoint` **succeeds, IPv4 only**. The v6 acquisition is refused
  inside the plugin and reports no router advertisement, which is classified
  as the tolerated "no router on this segment" case: a warning is logged and
  **`dhcpv6_no_router_advert` increments**.
- `Join` **returns success and the container starts — with an address
  nothing will renew.** Two things happen, in order. Because the record says
  `ipv6=true`, the manager first clears the engine's `disable_ipv6` on the
  container link; if that write fails it increments
  **`ipv6_link_enable_failures`** and carries on. It then tries to start the
  persistent client for the v6 family, which is refused, and that failure
  takes down the v4 persistent client started beside it. The client start
  runs in the background *after* `Join` has answered Docker, so the container
  comes up holding the IPv4 address the `CreateEndpoint` one-shot won, with
  no client renewing it. The failure increments **`join_start_failures`**,
  which flips `healthy` to `false`.
- At plugin start, **recovery replays the same sequence** for such an
  endpoint — the same `disable_ipv6` clear, the same refusal — and increments
  `recovery_failed`, which also flips `healthy` to `false`.

There is no migration step. Recreate the network without `ipv6=true`.

### Removed: plugin settings

| setting | why |
| --- | --- |
| `OUTAGE_TICK` | The outage watchdog it paced is gone. The DHCP client holds the lease and reports a failed attempt itself, so there is no cadence to tune |
| `OUTAGE_GRACE` | Same. It was the settling time the watchdog added on top of the lease lifetime |

Both are removed from `config.json`, so `docker plugin set OUTAGE_TICK=…` is
refused by the daemon rather than accepted and ignored.

### Removed: health counters and `/metrics` series

Each is removed from `/Plugin.Health` and from `/metrics`. A JSON consumer
reading a removed key gets zero, not an error.

| counter | why |
| --- | --- |
| `lease_time_clamped` | Counted a lease lifetime clamped to a 24h watchdog deadline. Option 51's `0xFFFFFFFF` is now carried as an infinite lease rather than as 4294967295 seconds, so there is no overflow to clamp and no watchdog to clamp it for |
| `directives_refused` | Counted values dropped before being written into a generated client config file. No config file is generated |
| `mount_prep_failures` | Counted failed steps of a per-client private mount-namespace setup. There is no per-client state directory, so there is no mount to prepare |
| `router_advert_guard_failures` | Counted failed steps of the DHCPv6 Router Advertisement guard, which is deleted along with the rest of the DHCPv6 path |
| `address_conflict_probes` | Counted verdicts reached by the plugin's own datagram probe on the parent link. That probe is deleted; RFC 5227 runs inside the DHCP client now, and `acd_probes_sent` is what says whether the address was checked |
| `conflict_probe_failures` | Counted probes that could not reach a verdict, almost always because the parent carried no address on the leased subnet. An RFC 5227 probe does not need one, so the condition no longer exists. `acd_arp_send_failures` is the nearest thing that remains, and it means something narrower: the socket refused the send |
| `conflict_probe_stale_routes` | Counted temporary `/32` routes reclaimed from a probe cut short. The probe installed no routes at all now, so there is nothing to leak or reclaim |
| `conflict_probe_stale_addrs` | Counted borrowed link-local source addresses reclaimed from the parent NIC, for the same probe. Nothing is borrowed and nothing is left behind |

**Two of the four v6 counters can still be incremented, and both mean
something other than what their 1.x descriptions say.** On the legacy
`ipv6=true` path described above, `dhcpv6_no_router_advert` increments once
per `CreateEndpoint` — always, whatever the segment carries — and
`ipv6_link_enable_failures` increments at `Join` if clearing the engine's
`disable_ipv6` on the container link fails. That step runs even though no
DHCPv6 client is constructed, which is why the counter is not dead here.

**The other two, `dhcpv6_config_only` and `dhcpv6_not_offered`, cannot be
incremented in this build.** Nothing emits the audit event the first reads,
and the v6 refusal reports nothing observed about the segment, so it always
classifies as "no router" and never as "no DHCPv6 offered". All four are still
rendered so a scrape does not lose a series across the upgrade. **Read a zero
on those two as "this build cannot report it", not as "nothing went wrong."**

### New: RFC 5227 address conflict detection (`conflict_check`)

The plugin checks whether the address its DHCP server just leased is
already in use on the segment, and this is the release where that check
becomes real rather than best-effort. It is run by the DHCP client, as
RFC 5227 Address Conflict Detection, from inside the container's own
network namespace — not by the plugin from the parent link.

Two holes in the v1.6.0 probe close with it. The old check could only
look at the address a **new** endpoint was about to be handed, so a
conflict that started after the container was up was never seen; RFC 5227
§2.4 listens for the whole life of the lease. And the old check needed
the parent to carry an address on the leased subnet, because a host
answers an ordinary ARP request only if it can route a reply back to the
sender; a §2.1.1 Probe carries an all-zero sender address, which Linux
answers for any local target without consulting a route, so a bare parent
is no longer blind. A conflict now also produces a `DHCPDECLINE` (RFC
2131 §3.1(5)) and a fresh DISCOVER, which means **your DHCP server's log
is evidence** — it never was before.

**It costs seconds, and `-o conflict_check=<mode>` says who pays them.**

| mode | `docker run` | on a conflict |
| --- | --- | --- |
| `wait` (default) | Waits for §2.1 to clear the address: **4.0s best, 5.5s mean, 7.0s worst**, on top of the DHCP exchange | The container never comes up on the contested address. The client declines it and asks for another |
| `async` | Returns at the DHCPACK, with no added wait | The address of a **running** container changes, about **11s** after the conflict appears (MEASURED; ten of those are RFC 2131 §3.1(5)'s mandatory wait after the DHCPDECLINE, during which the container still holds the contested address). Connections on the old address are already broken for both hosts; `docker inspect` does not update (watch `lease_changed`) |
| `off` | Returns at the DHCPACK | Nothing inside the client detects it. No ARP is sent; `address_conflicts` and `acd_conflicts_detected` move only for a conflict reported to the client from outside it, which nothing in the plugin does today, and `acd_probes_sent` stays where it was |

Any other value is refused at `docker network create`, with the three
names in the message. Networks created before this option existed read as
`wait`, so **an upgrade adds up to seven seconds to `docker run` on every
existing network** — set `-o conflict_check=async` at create time if that
matters more than starting clean. The default `lease_timeout` grows with
it, from 10s to **34s**; see its entry in `docs/reference.md` for what
that buys and what it costs on a segment with no DHCP server.

`wait` is a rule about **acquiring** an address, not about keeping one.
Two paths deliberately do not wait:

- **A container joining a network it already has a lease on** (the
  resumed-lease path) runs the check in `async` even where the network
  says `wait`. Nothing is skipped — the same probes, the same §2.4
  listener and the same DHCPDECLINE — but the address the container
  already holds is not held back while they run. Holding it back would
  add the probe window to every restart of every container, to re-check
  an address the previous run already cleared.
- **`-o validate_dhcp=true`'s preflight probe at `docker network
  create`** runs `off`. It is asking whether a DHCP server answers on
  this parent; the address it is offered is released immediately and
  never configured, so there is nothing for RFC 5227 to protect and no
  reason to spend the window inside an 8s budget.

`ignore_conflicts` is untouched and is not related: it is about another
Docker network on **this host** already owning the bridge you named,
before any lease exists.

Four counters are added, on `/Plugin.Health` and `/metrics`:
`acd_probes_sent` (read it before believing `address_conflicts` is zero),
`acd_announcements_sent`, `acd_conflicts_detected` and
`acd_arp_send_failures`. `address_conflicts` keeps its name, keeps
flipping `healthy`, and is now fed by the DHCP client rather than by the
plugin's probe.

### New: the health document says what is wrong, when it started, and where

`/Plugin.Health` keeps every field it had. `healthy` is unchanged — same
latch, same counters behind it, same meaning — so a dashboard that reads it
needs no edit.

Beside it there is now a `status` of `pass`, `warn` or `fail` and a `checks`
object, in the shape of the IETF health-check draft
(`draft-inadarei-api-health-check`). **What to poll:** `status` answers "is
anything wrong", `checks` names which counter says so, and each check carries
a `time` — the moment that counter last moved — which answers "is it still
happening". That last question had no answer before: the flags latch, so a
fault an hour ago and a fault in progress produced the same document.
`fail` is exactly the set of counters that flips `healthy`, and the two are
never allowed to disagree; `warn` is the set the reference table tells you to
watch without calling it a fault, and it never touches `healthy`. Everything
else stays informational and is not a check.

The document also gains an `endpoints` array — one entry per attached
container, with its address, lease state, T1/T2/expiry as absolute times, the
server that granted the lease, the last lifecycle event and its time, the
`conflict_check` mode and where RFC 5227 has got to — and `version`, `commit`
and `library`, which say exactly which binary answered. The same three are
labels on a new `net_dhcp_build_info` series with the value 1. A build made
outside the release pipeline reports `dev` and the commit it was built from.

Two deliberate departures from the draft, both stated in `docs/reference.md`:
the endpoint answers HTTP 200 for every status, because a latched flag would
otherwise make the socket look down for the life of the process, and the media
type stays `application/json`.

### Changed on the wire

Each line names the v1.9.0 behaviour and the beta's.

| behaviour | v1.9.0 | 2.0 beta |
| --- | --- | --- |
| Parameter request list (option 55) | The 16 codes 1, 2, 3, 6, 12, 15, 26, 28, 42, 66, 67, 100, 101, 119, 121, 252, asked for by name in the external client's config; the order it put them in was that client's business | The same 16 codes minus **12** (the host name is sent, not requested back) plus **33** (static routes), and the plugin controls the order: **121 first**, which RFC 3442 requires of a client that implements it |
| Legacy static routes (option 33) | Not requested and not honoured | Requested, and used when option 121 is absent or does not decode. Option 121 supersedes it when both arrive |
| Broadcast flag in the DHCP header | Set for **ipvlan only** | Set for **every mode**. The plugin's socket is a raw packet socket on an interface with no address yet, which is the condition RFC 2131 defines the flag for; clearing it works against servers that ignore the flag and hangs against servers that honour it |
| Initial-DISCOVER delay (RFC 2131 §4.4.1) | Whatever the external client did; not set or observed by the plugin | **None**, explicitly. The 1–10 second random delay is a rule for a fleet of hosts booting together; a container start is one client asking for one address. The library defaults to applying it and the plugin disables it, on both the acquisition and the renewal client |
| Retransmission schedule | The external client's; not set or observed by the plugin | RFC 2131 §4.1's worked example, set by the plugin: intervals of 4s, 8s, 16s, 32s to a 64s ceiling, ±1s of jitter on each, armed as each packet goes out — so retransmissions land at ~4s, ~12s, ~28s and ~60s after the first DISCOVER, and the fourth is followed by a restart of the exchange. The default `lease_timeout` funds one retransmission, and since it now also has to cover RFC 5227's probe window **and one conflict found inside it** — a DHCPDECLINE, RFC 2131 §3.1(5)'s mandatory ten-second restart delay, and a second acquisition — it is **34s** rather than 10s, derived from the two schedules rather than written down |
| `dhcp_servers` / `dhcp_deny_servers` matching | Matched the **packet's source address**, so the lists did not work behind a DHCP relay (#111) | Matches the **server identifier (option 54)**, so the lists work behind a relay. Deny wins over allow; an allow list refuses a message that carries no server identifier at all; a deny list alone permits one |
| First-attempt budget floor for the `dhcp_servers` ladder | 3s per attempt, sized for a process spawn | Unchanged at 3s. The attempt no longer spawns a process, but the floor is a policy choice pinned by tests, not a measurement of the old cost |
| DHCPRELEASE | Never sent (v1.9.0, #800) | Never sent. Unchanged |

### Also

- **One binary per release.** The image built only `net-dhcp` and
  `dhcp-handler`; the handler was the hook the external client called back
  into, and it is gone. From this release the checksum list in
  `docs/verifying-releases.md` has one entry per platform, not two.
- **`/etc/resolv.conf` and the link MTU are still applied by the plugin**,
  over netlink and a `setns` into the container's mount namespace, exactly as
  before. Nothing about `propagate_dns`, `propagate_mtu`, `skip_routes`,
  `register_dns` or `audit_log` changes for an operator.

## v1.9.0

IPv6 works on stateless, SLAAC-only and managed segments. The plugin no longer
sends DHCPRELEASE on any path. Every place the plugin refuses operator input is
now counted.

### Upgrade notes

Required on every host before `docker plugin install`, unchanged since v1.5.0:

```bash
sudo mkdir -p /var/lib/net-dhcp
```

**Behaviour changes.** These take effect on upgrade.

| change | effect |
| --- | --- |
| A container's address on stop, restart or removal | **Held until the lease expires.** No DHCPRELEASE is sent on any path, and the orphaned-lease sweep is gone (#800) |
| Log line for a client signalled before it ever bound | Was `Persistent client stopped before it ever held the lease; reclaiming it`. Now ends `the one-shot's lease is left to expire on the server`. Log alerts matching the old wording stop matching (#800) |
| `ipv6=true` on a segment offering no DHCPv6 address | **Containers start.** Endpoints on a stateless segment (RA "other configuration" flag only) or a SLAAC-only segment previously failed: no DHCPv6 address exists there by definition, the acquisition always timed out, and the timeout was fatal. The endpoint is now created without a v6 lease. The discriminator is the advertised managed-address flag, not how long the plugin waited — a segment advertising managed-address whose server then goes quiet is still a fatal error (#868) |
| Router Advertisements inside the container | **Processed by the container's kernel.** `dhcpcd` set `accept_ra=0` and `autoconf=0` on the interface it manages, and re-did it on every carrier acquisition, so no router discovery ran. DHCPv6 carries no router option, so the IPv6 default route lapsed and the address stopped being refreshed while on-link traffic kept working. The DHCPv6 client now runs with `accept_ra=2`, `autoconf=1` and `keep_addr_on_down=1` (#875) |
| A segment advertising a prefix with the A flag **and** running stateful DHCPv6 | **More than one global IPv6 address per container**: the DHCPv6 lease, plus one the kernel forms from the prefix, plus privacy addresses where the image's kernel enables them. An outbound connection that does not bind explicitly selects its source per RFC 6724, which need not be the leased address `docker inspect` reports; a firewall, ACL or allowlist keyed on the leased address can see traffic from another. Mitigation: advertise the prefix with A=0, or bind explicitly (#875) |
| Stateless DHCPv6 replies | **Applied, not dropped.** A segment advertising "other configuration available" answers an information request with options and no address; those replies were discarded at two points. With `propagate_dns`, option 23 nameservers and option 24 search domain now reach the container (#815) |
| The `search` line in `resolv.conf` on a DHCPv6 lease | **Written.** The v6 branch mapped nameservers and not the search list, so a DHCPv6-only lease produced no `search` line. It now comes from option 24 (#815) |
| How a DHCP outage is first noticed | **The DHCP client reports it.** Dropping the `release` directive changed the event `dhcpcd` fires on a lapsed lease from `RELEASE` to `EXPIRE`, which the plugin counts. `dhcp_timeouts` rises at the lease deadline instead of one watchdog grace later, and the log carries a lease-failure record where it previously carried only a watchdog line. `dhcp_timeouts` is otherwise unchanged — same name, same labels. The deadline watchdog stays as the backstop; whether it still earns its place is #855 (#800) |

No setting controls release behaviour. A lease expires on the DHCP server's
schedule, and that schedule is the operator's to set there.

**Breaking for metrics scrapers and health checks.** Changes across the
`/metrics` surface, `/Plugin.Health` and the audit ledger:

| surface | before | after |
| --- | --- | --- |
| `/metrics` counter | `net_dhcp_lease_release_failures_total` (carrying `family="ipv4"` / `family="ipv6"`) | `net_dhcp_client_stop_failures_total`, same labels — same event, accurate name: a renewal client that did not shut down cleanly. It never meant a lease went unreleased, and now cannot |
| `/metrics` counter | `net_dhcp_orphaned_leases_released_total`, `net_dhcp_orphaned_lease_release_failures_total` | **Removed.** Nothing releases, so neither has a subject |
| `/Plugin.Health` keys | `lease_release_failures`, `lease_release_failures_v4`, `lease_release_failures_v6` | `client_stop_failures`, `client_stop_failures_v4`, `client_stop_failures_v6` |
| `/Plugin.Health` keys | `orphaned_leases_released`, `orphaned_lease_release_failures` | **Removed** |
| `leases.jsonl` kinds | `release`, `release_failed` | `stopped`, `stop_failed` |

Update dashboards and alerts before upgrading. A JSON consumer reading a
`/Plugin.Health` key that has been removed receives zero, not an error, so a
`jq`-over-the-socket check of the kind in `docs/reference.md` keeps reporting
healthy over a key that no longer exists.

### New

**IPv6 on the segment you actually have** (#868, #815, #875). Four changes, in
the order a container meets them:

- A DHCPv6 acquisition that produces nothing is no longer read as a failure by
  default. The discriminator is the router advertisement's managed-address
  flag, so "this segment has no DHCPv6 addresses" and "this segment has DHCPv6
  and the server went quiet" are two observations rather than two readings of
  one timeout (#868).
- The engine writes `disable_ipv6=1` on a sandbox interface whose endpoint has
  no IPv6 address — reachable only once the previous point stopped refusing
  those endpoints. Nothing IPv6 can arrive on such a link, so the plugin
  enables IPv6 on it before starting a DHCPv6 client (#868).
- Address-less DHCPv6 configuration replies become an event the plugin
  consumes instead of being dropped at two separate points (#815).
- The container's kernel is put in charge of router discovery and kept there
  for the life of the client, rather than only until `dhcpcd`'s next carrier
  event (#875).

`docs/reference.md` carries the full semantics, including the two bounds this
release does **not** close: after a carrier flap a container waits for the
router's next unsolicited advertisement before it regains a default route, and
an address the container's own kernel forms is not a stable identifier.

**Seven new health counters**, all also on `/metrics` with the `net_dhcp_`
prefix:

| counter | counts | from |
| --- | --- | --- |
| `dhcpv6_config_only` | stateless information replies received | #815 |
| `dhcpv6_not_offered` | endpoints started on a segment offering no DHCPv6 address | #868 |
| `dhcpv6_no_router_advert` | no advertisement arrived inside the budget | #868 |
| `router_advert_guard_failures` | steps of the advertisement guard that failed | #875 |
| `ipv6_link_enable_failures` | failures enabling IPv6 on the sandbox link | #868 |
| `directives_refused` | `dhcpcd` directives dropped because the value carried a control character — a `hostname`, `vendor_class` or `client_id` that never reached the server | #780 |
| `mount_prep_failures` | steps of a DHCP client's private mount-namespace preparation that failed | #780 |

None of the seven affects `healthy`; the five that do are unchanged. The
counted unit differs between them, and `docs/reference.md` states the unit per
counter.

### Fixed

IPv6 (#868, #815, #875) — listed under **New** above; the change an operator
sees is a capability rather than a repair.

Lease handling:

- The plugin sent DHCPRELEASE on stop, restart and removal, and swept for
  orphaned leases to release those too. A `docker stop` released an address
  that the `docker start` seconds later was about to ask for again, and the
  server was free to have given it away in between. Nothing releases now
  (#800).

Operator input the plugin refuses (#780) — each refusal is correct and each was
silent:

- A `dhcpcd` directive whose value carries a control character is dropped
  rather than written, because `dhcpcd.conf` has no quoting and the value would
  become a second directive. The lease is obtained without it and the plugin
  reports `healthy`.
- Steps of a DHCP client's mount-namespace preparation can fail without
  stopping the client. Two containers whose interface has the same name then
  collide on `dhcpcd`'s control socket, and the second client becomes a no-op
  that never renews.

CI gates that reported success over something they could not see:

- `staticcheck` never looked at the integration-tagged files, and reported a
  defect that was not there (#871).
- A required check named `test` ran a large set of gates that are not tests, so
  a gate failure and a test failure were one signal (#829).
- Two gates judged a narrower set than their own headers claimed (#832); the
  health-contract gate could not see a negated property (#826); the
  test-weakening gate could not see CI's own gate self-tests (#828); the
  label-taxonomy gate's queries had never executed under test (#840, #715).
- Nothing enforced action SHA-pinning, so a tag could have shipped (#831). The
  only barrier to fork code on the self-hosted runners was a web-UI toggle
  (#830).
- The install-verify set is now derived from the publish set, so a new registry
  cannot ship unverified (#833).
- The retention purge could delete an open pull request head's only evidence
  (#874, #837).
- The dispatch-pending ledger's framing left schedule-only workflows out of its
  domain (#846). A runner-image dispatch from any branch could repoint the CI
  pool's `:latest` (#812). The issue-reference gate was unsatisfiable for
  Dependabot, failing a required check on every dependency bump (#809).

Tests and harness:

- The `interface_name` acceptance test had never run and could not have passed
  (#841). The coverage baseline's cleanup arm had no observer, and its
  non-vacuity guard caught zero rather than incompleteness (#789, #791). The
  wiring gate's presence check stopped one level short of the wrapper (#790). A
  denied Kea reported only that it never became ready (#869).
- The integration gate ran over its design budget on a stale shard-balance
  table; the main suite is now five shards (#877).

Documentation:

- Two networks on one subnet cannot both attach, and nothing said so (#847).
  The starter-task promise in the README and its badge was false (#851). The
  `interface_name` upstream blocker was stale — the engine change is merged and
  waiting on a release (#822).

Base image: `libssl3` and `libcrypto3` pinned to 3.5.8-r0, clearing ten
OpenSSL advisories reported against the published image. The plugin does not
link OpenSSL, so no plugin code path was affected.

Dependencies: Go 1.27.0, `logrus` 1.10.1, and a GitHub Actions group bump.

### Deferred to v1.10.0

- The rest of IPv6. An acquisition still reports a lease timeout where no
  exchange was possible (#816). Fresh and restarted containers take different
  DHCPv6 paths on the same network (#820). The gateway, DNS, MTU and routes are
  not taken from the advertisement (#821). SLAAC is not something the plugin
  acquires (#818). Address lifetimes, withdrawal and renumbering (#819) and
  prefix delegation (#214) are later still.
- Sweeping `STATE_DIR` at startup, so an upgrade tightens the state files it
  finds rather than only the ones it writes (#804).
- The harness treats a netlink dump interruption as fatal, and two product
  sites share the hazard (#802).
- Whether the outage watchdog still earns its place now that `dhcpcd` reports a
  lapse directly (#855).
- What each `config.json` capability buys (#690), and whether a restricted
  socket proxy would serve the three read-only Docker API calls the plugin
  makes (#691). Both were listed under v1.8.0's "Deferred to v1.9.0" and
  neither was done.

## v1.8.0

A hardening release. Two external reviews of the code (#457/#699 security,
#726 lifecycle) plus a review of the CI gates (#732); all defects they found
are fixed here. New: per-network DHCP server selection, a Prometheus
`/metrics` endpoint, and durable plugin state.

### Upgrade notes

Required on every host before `docker plugin install`, unchanged since v1.5.0:

```bash
sudo mkdir -p /var/lib/net-dhcp
```

**Behaviour changes.** These take effect on upgrade:

| change | effect |
| --- | --- |
| Container hostname with a control character | dropped instead of written to `dhcpcd.conf`; the lease still proceeds |
| DHCP option 15, plus four other server-chosen string options | filtered, and truncated at the first space, before reaching `resolv.conf` |
| Plugin socket | `chmod`ed to `0600`; the plugin refuses to start if that fails |
| Attach whose container PID no longer belongs to that container | fails instead of proceeding |
| `propagate_mtu` with an option-26 MTU outside `[576, 65535]` | refused; the container link keeps its MTU |
| Option-51 lease lifetime longer than a year | 24h substituted **for the outage watchdog only**, counted as `lease_time_clamped`. Ordinary long leases are left as granted |
| Option-3 gateway that does not parse | dropped instead of applied (v1.7.1 installed `default dev ethX scope link`) |
| DHCP client left behind by a killed plugin | found and killed at startup, before recovery starts its replacement |
| State files and `leases.jsonl` under `/var/lib/net-dhcp` | created `0600`; reading them as a non-root user now needs `sudo`. **A file that already exists keeps its old mode until the plugin next writes it** — see below |

Reference digests differ from v1.7.1. Two `pkg/plugin` refactors are intended
to be behaviour-identical and are covered by the existing suite plus new unit
tests.

**Upgrading from v1.7.1 or older: tighten the existing state files by hand.**
`0600` is applied when the plugin *writes* a file, so an upgrade does not
retroactively tighten what it finds. `tombstones.json` in particular is only
rewritten when a tombstone is laid or consumed, which on a stable host can be
a long time — it was observed still at `0644` after a production upgrade to
v1.8.0. Once, per host:

```bash
sudo chmod 0600 /var/lib/net-dhcp/*.json /var/lib/net-dhcp/leases.jsonl
```

Making the plugin do this itself at startup is #804.

### New

**Per-network DHCP server selection** (#111, #669). Two network options, not
interchangeable:

- `dhcp_servers` — an *ordering*. Each server is tried in turn, restricted to
  that server; the first lease offered wins.
- `dhcp_deny_servers` — a *permission*. Never lease from these; everything
  else may answer.

Set both and the denial wins. The list is exhaustive: if none of the named
servers answers, the endpoint fails rather than accepting another server —
counted as `dhcp_server_policy_exhausted`, distinct from a DHCP timeout.
`dhcp_server_tier_fallbacks` counts a preferred server going quiet. The
preference ladder divides the existing `lease_timeout` budget, so `docker run`
does not get slower; raise `lease_timeout` to give each server more time.
Both options are **DHCPv4 only** (a v6 entry is rejected at
`docker network create`) and are **not supported behind a DHCP relay**.

**Prometheus `/metrics`** (#212, #651). Served on the plugin socket
unconditionally, rendered from the same snapshot as `/Plugin.Health`. Since
Prometheus cannot scrape a UNIX socket, an optional TCP listener:

```bash
docker plugin disable <plugin>
docker plugin set <plugin> METRICS_ADDR=127.0.0.1:9099
docker plugin enable <plugin>
```

Off by default. The plugin runs with `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` and
`CAP_SYS_PTRACE` on host networking, so bind loopback or a management
interface, never `0.0.0.0`. The listener serves `/metrics` and nothing else — any other path is a
404, and no request can stop the plugin. A `METRICS_ADDR` that cannot be
bound (bad syntax, port in use) fails plugin **startup**, so you find out
at boot instead of from an empty dashboard.

Six counters carry a `family` label. Each has `_v4` and `_v6` fields in
`/Plugin.Health` and the unsuffixed counter is their sum, so the JSON total
will not equal the `family="ipv4"` series. `net_dhcp_build_info` carries
`instance_id`, so a restart appears as a new series rather than a silent
counter rewind. Per-endpoint and per-network labels are deliberately absent
(unbounded cardinality).

**Durable state** (#708, #724). Files under `/var/lib/net-dhcp` are now
fsynced, so they survive a power cut and not just a plugin kill. The
per-network options file carries a schema version, and a corrupt
`tombstones.json` is kept rather than discarded.

**New health counters**, including the v4 halves of the six `family`-labelled
counters, which were previously derived by subtraction (#730).

### Fixed

Security review (#457, indexed by #699):

- A container hostname reached the generated `dhcpcd.conf` unvalidated;
  a newline produced additional directives, letting one container present
  another's identity (#692, #694).
- Values from the DHCP server were validated for type but not range, and
  had no counters (#689, #700–#704, #762, #353).
- A container PID resolved through Docker was used after the check without
  being pinned to the task it was checked against (#688, #695).
- The plugin socket's root-only property was inherited from umask rather
  than enforced (#687, #705–#707, #709, #710).
- State files under `/var/lib/net-dhcp` were created `0644` on a host bind
  mount (#708).

Lifecycle review (#726) — ten faults; six (#720, #721, #722, #724, #727,
#728) are verified present in v1.7.1 and predate this cycle:

- A failed start could release a live container's address (#720).
- Address stability was lost on the first plugin restart: `rememberEndpoint`
  had both call sites on the `CreateEndpoint` path, so recovery recorded
  nothing and `DeleteEndpoint` laid no tombstone (#721).
- DHCP clients outlived a SIGKILLed plugin and could not be found afterwards,
  so recovery started a second client per endpoint with the same DUID and
  IAID (#722).
- Interface names were validated on the create path but not where they are
  used, and every endpoint handler re-reads the stored options (#727).
- An unparseable option 3 became an on-link default route (#728).
- A descriptor leak on the DNS-propagation hot path (#729).
- The v4 counter share was derived as `total - v6`; subtracting two
  independently updated counters can read below the previous scrape, which
  Prometheus treats as a reset (#730).
- Two counters had no observer at all (#731).
- Plus #723, #724.

Other:

- An ordinary `docker network rm` could increment `recovery_failed`, one of
  the counters that flips `healthy` to `false` (#648).
- Two DHCP clients could end up renewing one lease when a `Join` displaced a
  manager the recovery walk had already claimed (#679).
- Unit tests asserted against request structs written by hand rather than
  what the daemon actually sends (#644, #646).

CI gate review (#732): gates reporting success over input they had never
read. The coverage ratchet accepted a missing, empty or comments-only
baseline and now refuses a verdict at zero comparisons (#734, #735); four
gates judged a subject set they never fully read (#743); three matched
something narrower than their own header, one of them a required check whose
waiver matched the gate's own failure text (#758).

Internal: the tombstone store and the six phases of lease renewal are now
separable units with their own tests (#643); the arm64 lane runs a standing
self-hosted runner that registers once and reconnects on boot (#632).

### Deferred to v1.9.0

- #690 and #691 — what each `config.json` capability buys, and whether a
  restricted socket proxy would serve the three read-only Docker API calls
  the plugin makes. Both came out of the security review; neither is a defect.
- Two structural observations recorded in #726: the release invariant is
  decided at five sites in prose rather than by one predicate (#720), and a
  growing set of invariants is held by shell gates rather than by types.

### With thanks to

- **[@Dev9269](https://github.com/Dev9269)** — replacement text for
  `docs/internals.md`, describing the `interface_name` gate as the capability
  probe it is ([#675](https://github.com/claymore666/docker-net-dhcp/pull/675),
  on [#673](https://github.com/claymore666/docker-net-dhcp/issues/673)).


## v1.7.1

A documentation release. **No plugin change** — nothing in this release
touches the plugin source, its Dockerfile, or its dependencies, so the
reference digests in [Verifying
releases](https://claymore666.github.io/docker-net-dhcp/latest/verifying-releases/)
are unchanged from v1.7.0: the same source builds the same bytes, and
the release workflow checks that claim against the binaries it just
built. Upgrade if you want the tag to match the documentation you are
reading; there is no functional reason to.

### The arm64 instructions were incomplete

v1.7.0 introduced per-architecture tags (`:vX.Y.Z-arm64`), and the pages
carrying the `docker plugin install` line said so. Three places did not:

- **The mode guides.** *Bridge mode* and *macvlan / ipvlan modes* carry
  copy-pasteable `docker network create` commands and a Compose
  `driver:` — and never mentioned arm64. Both are linked from the quick
  starts for the one-time host setup, so a reader reaches them directly
  and copies a driver reference naming a plugin their host does not
  have. A network stores that exact string, so it fails at create time
  with nothing pointing at the tag.
- **The install-failure recovery.** The recovery for the most common
  install failure — the missing state directory — named the bare tag on
  every page, including the three that were otherwise correct.
- **The explanatory comments** carried a version number that the
  release tooling could not see or update, so they would have gone
  stale on this release with nothing reporting it. They now state the
  rule without a version.

If you run arm64: the `-arm64` tag replaces the bare one in **every**
snippet that names the image, not only the install line.

### `/Plugin.Health` documented the wrong healthy contract

The `healthy` field's own row in the driver reference listed three
counters that flip it to `false`. There are four — `address_conflicts`
has been one since v1.6.0, and the table's healthy-affecting column and
the At a glance summary both said so. Only the row an operator reads
first was wrong.

If you alert on `healthy`, nothing about your alerting changes: the
plugin's behaviour was always the documented four. What changes is that
the page now agrees with itself, and a CI gate fails if the two ever
part company again.

### Also

- The roadmap reported both upstream Docker Engine pull requests as
  awaiting review. The `interface_name` pass-through that Compose
  `interface_name` support depends on has since been **approved** and is
  milestoned for engine **29.8.0**.
- A duplicated sentence in *Verifying releases*, left over from the
  arm64 digest block.

## v1.7.0

The release that ships arm64, and closes the last of the lease-leak
cases from v1.6.0.

### The v1.5.0 precondition still applies

```bash
sudo mkdir -p /var/lib/net-dhcp
```

Unchanged since v1.5.0, and still required on every host before
`docker plugin install`. Docker does not create a missing bind source,
and skipping it leaves the plugin installed but disabled with an error
that does not say why. See the v1.5.0 notes below for recovery.

### arm64 is published

Every release now ships a native `linux/arm64` build alongside amd64.
The architecture is in the **tag**, because a Docker plugin cannot be
installed from a multi-architecture manifest list:

```bash
# amd64 (unchanged)
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.7.0
# arm64
docker plugin install ghcr.io/claymore666/docker-net-dhcp:v1.7.0-arm64
```

`:latest-arm64` tracks the newest release the way `:latest` does. Both
architectures are signed, attested and carry SBOMs identically; the
arm64 artifacts are the `-arm64`-suffixed files on the release. The
suite is run natively on arm64 hardware for every release candidate —
not under emulation, which cannot run the DHCP client at all. 32-bit
ARM is not built.

### Two more lease leaks closed

v1.6.0 fixed the renewal client that was stopped before it ever bound.
Two neighbours of that case remained:

- **The client was killed by the stop signal before it bound.** The
  plugin read the exit status first, classified it as a failed release,
  skipped the reclaim, and answered the `docker` teardown with an error
  — for a shutdown in which nothing had actually failed. It is now
  judged by whether the client ever held the lease, which is the only
  thing that decides what is owed.
- **The IPv6 half of a dual-stack endpoint.** The v6 client had none of
  the v4 machinery: a never-bound v6 client was written to the audit
  ledger as *released*, and its address was leaked until the server
  expired it. Both families are now reclaimed, each with its own
  identity, and the ledger records only what the server actually saw —
  verified against the DHCP server's log, not against the plugin's
  counters. New health counter: `lease_release_failures_v6`, joining
  the per-family split of its neighbours.

### Also

- Two address-conflict probes running at the same time on a parent
  without an address of its own could unpick each other's temporary
  source address mid-probe (a kernel rule about secondary addresses,
  active in fresh network namespaces). Each probe's borrowed address is
  now standalone. A probe route that is *already* gone at cleanup is no
  longer logged as a failure to remove it — only a genuine failure is.
- Release verification: `docs/verifying-releases.md` covers the arm64
  artifacts, and the reference binary digests for both architectures
  are recorded there per release.

### CI and process (no user-visible change)

The hosted portability lane runs again on stock Ubuntu (it had been red
for two weeks on a missing DHCP server); merge bursts can no longer
leave a commit on `dev` with no integration verdict; the test harness
gives macvlan and ipvlan separate parent NICs and names the two lanes'
build directories in exactly one place; a queue-watchdog cancel that is
refused is retried and explained; and a `workflow_dispatch` against a
ref that is not this repository's is rejected before it can reach a
privileged runner.

## v1.6.0

The release that makes the plugin notice when something is wrong. Three
classes of silent failure — an address already in use, a lease nobody
hands back, and the plugin refusing its own operations — now report
themselves instead of looking healthy.

### The v1.5.0 precondition still applies

```bash
sudo mkdir -p /var/lib/net-dhcp
```

Unchanged since v1.5.0, and still required on every host before
`docker plugin install`. Docker does not create a missing bind source,
and skipping it leaves the plugin installed but disabled with an error
that does not say why. See the v1.5.0 notes below for recovery.

### An address that is already in use is now reported

The plugin accepted a lease for an address another device was already
holding, and nothing said so. The container started, `docker inspect`
showed an address, and every counter stayed at zero — because from the
DHCP server's point of view the lease was issued normally.

This was found in production, not in CI.

After each IPv4 lease the plugin now resolves the address on the parent
link and compares the answering MAC with the endpoint's. A reply from a
different MAC is a conflict: `address_conflicts` increments and
`healthy` goes false. The usual cause is a **statically configured**
host inside the DHCP pool range — it never asks the server for anything,
so the server cannot know the address is taken. Fix it at the server.

Two supporting counters exist because a detector that silently stops
running looks exactly like a clean segment:

- `address_conflict_probes` — probes that reached a verdict. Read this
  **before** believing `address_conflicts` is 0.
- `conflict_probe_failures` — probes that could not reach one. The
  common cause is a parent with no address on the leased subnet, which
  leaves the probe unable to get a reply back. Give the parent an
  on-subnet address and detection starts working.

**Known limit, by construction:** it cannot see another container on the
*same host* sharing the parent NIC. macvlan isolates a parent from its
own children, and that isolation is what lets the check tell a squatter
from your own endpoint.

### Leases are handed back instead of leaking

Two cases held an address upstream with nobody responsible for it: the
renewal client never started because the container was already gone, and
the renewal client started but was stopped before it ever bound. In
both, `dhcpcd` had nothing to release, exited cleanly, and the audit
ledger recorded a release the server never saw.

Both now hand the address back properly, and the ledger records what
actually happened. Short-lived containers (`docker run --rm`, a failed
start, a compose `up` that exits) no longer strand a lease until expiry.

### ipvlan networks work where they previously refused

- `validate_dhcp=true` on an ipvlan network built a **macvlan** probe
  link. A parent NIC cannot carry both kinds, so the probe was refused
  whenever an ipvlan container was already running on that NIC —
  `validate_dhcp` failing for a reason that had nothing to do with DHCP.
  The probe is now the same kind as the network's endpoints.
- The plugin's own operations on one parent — endpoint creation, the
  preflight probe, the lease reclaim — could refuse each other the same
  way. They now queue per parent instead, visible as `parent_link_waits`.

One mode per parent remains a kernel constraint; this is only about the
plugin no longer inflicting that error on itself.

### Also

- A conflict probe interrupted by a plugin stop left a route behind,
  and every later probe for that address failed until something removed
  it — so that address silently stopped being checked. The probe now
  reclaims the leftover (`conflict_probe_stale_routes`).
- The plugin can now see the sandbox network namespaces it was already
  trying to read, so "the container is gone" is reported as that rather
  than as a generic failure. New diagnostic: `sandbox_netns_visible`.
  Doing so adds one **read-only** bind mount of `/var/run/docker`, which
  `docker plugin install` lists among the permissions it asks you to
  grant. It is the parent of the namespace directory rather than the
  directory itself, because Docker does not create that directory until
  the first container starts — mounting it directly made the install
  fail on a host that had never run one.

### Architecture

Published images remain **linux/amd64**. Docker plugins cannot be
installed from a multi-architecture manifest list, so arm64 needs a tag
of its own; that is tracked in
[#507](https://github.com/claymore666/docker-net-dhcp/issues/507).

### With thanks to

- **[@snowyukitty](https://github.com/snowyukitty)** — taught the
  issue-state reconciler to read issue references from a pull request's
  **body**, not only its title, so a PR that names its issue only in the
  description is no longer invisible to it
  ([#553](https://github.com/claymore666/docker-net-dhcp/pull/553)).
- **[@sdjnmxd](https://github.com/sdjnmxd)** — reported that published
  manifest lists are uninstallable as Docker plugins, which is why arm64
  needs per-arch tags rather than a second platform on the same tag
  ([#507](https://github.com/claymore666/docker-net-dhcp/issues/507)).

## v1.5.0

The release that stops a plugin upgrade from erasing what the plugin
knows about your containers — and the one release you cannot install
without running a command first.

### ⚠️ Breaking — run this before you install or upgrade

```bash
sudo mkdir -p /var/lib/net-dhcp
```

v1.5.0 is the first release whose plugin manifest **bind-mounts
`STATE_DIR` from the host**. That is what makes plugin state survive an
upgrade, and it introduces a precondition earlier versions did not
have: **the directory must exist before the plugin is installed.**
Docker does not create a missing bind source. It is also a new
privilege at the interactive grant prompt — the manifest now requests
two bind mounts rather than one.

**If you skip it**, `docker plugin install` pulls the plugin, fails to
start it, and exits non-zero:

```text
Error response from daemon: failed to create task for container: ...
error mounting "/var/lib/net-dhcp" to rootfs at "/var/lib/net-dhcp":
bind mount source stat: no such file or directory
```

Nothing on disk is lost or corrupted, but you are left in a state the
error does not explain, and **the obvious next move does not work**:

| what you try | what Docker says |
|---|---|
| re-run the same `docker plugin install` | `plugin ... already exists` — no mention of the mount |
| `docker network create -d ...` | `plugin ... found but disabled` — no mention of why |

The install leaves the plugin **present but disabled**, so a second
install refuses before it ever re-attempts the mount. Recover with:

```bash
sudo mkdir -p /var/lib/net-dhcp
docker plugin enable ghcr.io/claymore666/docker-net-dhcp:v1.5.0
```

This bites hardest on the documented upgrade path, where the previous
version has already been removed by the time the new one is installed:
until you run those two lines the host has no working DHCP driver, with
networks removed and containers stopped. Every sequence above was
executed against Docker 26.1.5 rather than reasoned about (#494).

Two further consequences worth stating plainly:

- **Durability starts here.** An upgrade *onto* v1.5.0 still begins
  from nothing, because the old state was never on the host to carry
  across. The benefit applies to upgrades *after* this one.
- **Repointing `STATE_DIR` opts out.** A path other than the mounted
  one is inside the rootfs again, and is wiped by the next upgrade
  exactly as before.

### What changes for you

**You can read the plugin's log without digging into the plugin.**
Every line now also goes to the plugin's stdout, which dockerd captures
into the daemon log on the host:

```bash
sudo journalctl -u docker --since "2 hours ago" | grep net-dhcp
```

Until now the only copy lived in the plugin's own rootfs, which
`docker plugin rm` destroys — so the history disappeared at precisely
the moment an upgrade went wrong and you wanted to read it (#420).

**From your next upgrade on, the plugin stops forgetting your
containers.** Its record of which MAC and IP belonged to which endpoint
now lives on the host at `/var/lib/net-dhcp` rather than inside the
plugin. In practice: a container that comes back after an upgrade can
be handed the address it had, and `audit_log=true` produces one
continuous ledger instead of one that silently restarts at every
install. (Starting with the upgrade *after* this one — see the caveats
above.)

**Two new health counters** on `/Plugin.Health`:
`restart_link_up_waited` and `restart_link_up_timeouts`. v1.4.0 fixed a
`docker restart` that failed outright on macvlan and ipvlan by waiting
for the interface to come up, but gave you no way to see whether that
wait was happening on your host or how often it ran out of patience.
Now `curl` the plugin socket and look (#422).

**The macvlan / ipvlan quick start in the docs actually works.** The
`docker network create` line it published could not have worked as
written — anyone who copy-pasted it got an error rather than a network.
Fixed, and CI now rejects a broken driver string in the documentation
instead of publishing it (#460).

**Published artifacts name the right project.** The Go module path
still said `devplayer0`, so the binaries, the SBOM and the provenance
attestation all attributed themselves upstream. If you scan
dependencies or verify provenance, what you get back now matches the
repository you pulled from (#464).

**You can rebuild a release yourself and check it matches ours.** The
binaries reproduce byte-for-byte from the tag; the procedure is in
[Verifying releases](https://github.com/claymore666/docker-net-dhcp/blob/main/docs/verifying-releases.md#rebuilding-the-binaries-yourself),
and CI fails if reproducibility breaks. Every source file also carries
an SPDX licence and copyright header now (#456, #454).

**The documentation stopped sending you to empty paths.** Several
troubleshooting recipes read plugin state from inside the plugin
rootfs — which this release makes empty, because the data moved to the
host. A wrong path that returns nothing is indistinguishable from a
feature that does nothing, so those recipes read as broken features
(#489, #494).

### What does not change

No driver options added, removed or renamed. No change to how addresses
are requested, renewed or released, in any of the three modes. Existing
networks keep working against the tag they were created with. Apart
from the new state-directory mount, the plugin asks for exactly the
privileges it asked for in v1.4.0.

### The part you cannot see

About two thirds of this release went into the test suite and CI, with
no effect on the running plugin at all. What it buys you is indirect
but not nothing: the suite now runs a DHCP server whose lease timers it
controls, and checks the lease that **server** recorded rather than the
counter the plugin reports about itself.

That distinction is why it was worth doing. Every one of the six
defects v1.4.0 fixed was found by removing a timing crutch from a test,
and several of them — including a `docker restart` that failed on every
macvlan container — sat behind health counters reading green while the
feature did nothing. A plugin's own opinion of its health is not
evidence. Fewer defects of that shape should reach you.

## v1.4.0

A correctness release that started out as a speed release.

The work item was cutting the integration suite's wall clock. What it
found was that the suite was slow in places where the slowness was
load-bearing: every timing crutch removed turned out to be padding that
had been holding up behaviour the plugin did not actually have. Six
defects came out of it, one of them a `docker restart` that fails
outright on macvlan and is present in shipped releases including v1.3.5.

If you run macvlan or ipvlan, **upgrade for #408**. The two
address-collision defects (#408, #402) cannot occur in bridge mode — a
bridge port is not a macvlan child, so the kernel refusal behind them
has nothing to refuse. #406 is mode-independent and affects bridge
deployments equally.

No new driver options. No manifest changes. Existing networks keep
working unchanged. **One upgrade note:** the IPv4 client-id fix (#371)
changes what bridge and macvlan containers present as their DHCP
identity. On a server that keys reservations on MAC — the common case —
nothing moves. On a server that keys on option 61, each existing
container is seen as new once. Details in that entry below.

What changed:

- **`docker restart` could fail outright on macvlan** (#408). Restart
  re-applies the previous endpoint's MAC — that is what makes an IP
  survive a restart — while the previous endpoint's link can still
  exist on the parent. The kernel refuses to bring up a macvlan child
  whose MAC is already live on the parent, including the parent's own
  address, and the restart fails with `address already in use`. The
  plugin now waits for the departing link to release the address
  instead of failing on the first attempt.

  **Operator-visible consequence:** this is the fix most likely to
  matter to you. It presented as an intermittent `docker restart`
  failure with no obvious cause, more likely the faster the container
  stops — so a well-behaved container that handles `SIGTERM` promptly
  was *more* exposed than a sloppy one, which is why it survived so
  long.

- **A lease left behind by a fast restart was never released** (#370,
  #402). The reclaim path built a temporary link carrying the same MAC
  as the endpoint's live link and hit the same kernel refusal, so the
  release never ran: the address stayed leased until the server expired
  it, and the container came back on a different IP. Fixed for macvlan,
  and then again one step further in for ipvlan, where slaves share the
  parent MAC and the kernel enforces uniqueness by address instead.

- **An IPv4 address now survives a container restart on its own merits**
  (#371). IPv6 always survived one and IPv4 did not, and the asymmetry
  was the identity rather than the protocol: the v6 DUID/IAID is derived
  from the MAC, which the tombstone preserves, while the v4 option-61
  client-id was derived from Docker's endpoint ID, which Docker mints
  fresh on every restart. A returning container presented an identity
  the server had never seen, so its old address looked like somebody
  else's. It worked at all only because of the `DHCPRELEASE` sent on
  shutdown — which #370 showed is not dependable, and which can never be
  sent on `SIGKILL`, OOM, or power loss. Bridge and macvlan now use a
  MAC-derived client-id.

  ipvlan is deliberately excluded: L2 slaves inherit the parent's MAC by
  kernel design, so a MAC-derived id would be identical for every
  container on the network and they would all claim one lease. ipvlan
  keeps the endpoint-derived id and its restart fragility; #219 owns
  that case. An operator `client_id` override still wins in every mode.

  **Operator-visible consequence — read this before upgrading.** The
  client-id a container presents changes, so on bridge and macvlan a
  container may be seen as a new client once, on its first restart after
  the upgrade. Whether it actually moves depends on what your server
  keys on:

  - **Keys on MAC** (the common case, and how most home routers do
    reservations) — nothing changes. The MAC is preserved as before.
  - **Keys on option 61** — the container is unrecognised once and takes
    a new address. One-time, per container.

  ipvlan is unaffected, since its client-id is unchanged. If anything in
  your setup points at a container by address rather than by name, plan
  for it to move once on an option-61-keyed server.

- **A container that exits mid-attach is no longer counted as a plugin
  fault** (#373, #376, #383). Three separate paths counted an ordinary
  short-lived container, or a daemon that had not finished starting, as
  a failure and latched `healthy` false — enough to page an operator
  over a normal container exit. Benign cases now have their own
  counters, `join_aborted_container_gone` and
  `recovery_aborted_container_gone`, which deliberately do not affect
  `healthy`.

- **A Join no longer waits out its whole budget on a container that has
  already been removed** (#401). The attach path retried the daemon's
  "no such container" answer until the deadline, then reported a
  timeout — turning a container that had simply gone into something
  indistinguishable from a slow daemon.

- **Containers could be left with no renewal client when the daemon was
  busy** (#406). The attach asks Docker about the container it is
  attaching to — while Docker is still inside that container's start
  and does not answer. The Docker client's own 2-second timeout turned
  each request into a fast failure, five of which consumed the whole
  10-second attach budget, and the attach was abandoned. The container
  kept its address and nothing renewed the lease, so the address was
  eventually lost while the container was still running. Three to six
  containers per integration run.

  Two things were wrong and both are fixed. An attach cancelled because
  its own endpoint was leaving was counted as a fault — nothing is left
  without a renewal client when nothing is left — and now has its own
  counter, `join_aborted_endpoint_left`. And `AWAIT_TIMEOUT` no longer
  bounds the part of an attach spent waiting for the daemon: that is a
  statement about how long the plugin's own work may take, not about how
  long its caller may keep it waiting.

  **Operator-visible consequence:** new counter `join_attach_slow`
  reports attaches that needed the extra time. Not a fault — they
  succeeded — and a rising count means the daemon is holding containers
  longer, not that the plugin is degrading. `Leave` cancels an attach in
  progress, so a container going away during the wait does not delay its
  own teardown.

- **A run-wide floor on the health surface** (#377, #385). Per-test
  assertions only catch a fault that falls inside some test's own
  bracket. A run-wide floor catches one that no test bracketed,
  including faults raised during fixture setup, and says what it saw
  rather than printing an unqualified "clean". A counter the plugin
  never sent is no longer treated as a zero.

  The reason this is in a release note rather than left as an internal
  test detail: the counters reset when the plugin process does, and the
  suite restarts it, so the floor was judging the last ~12% of a run and
  reporting the other 88% as clean. Runs were passing with several
  containers left unrenewed. The verdict is now taken from the plugin's
  log, which spans the whole run, and a run with any such failure goes
  red. If you rely on `healthy` for alerting, note the same limitation
  applies on a real host: it is cumulative since the plugin started, not
  a statement about right now.

- **CI wall clock, which is where this began:** the integration gate on
  `dev` went from ~20 minutes to ~13, measured across the runs either
  side of the change (#367, #375, #390). The two
  integration suites run as concurrent jobs, the privileged
  concurrency group is scoped per ref so unrelated PRs stop queueing
  behind each other, and test containers get an init PID 1 so teardown
  stops burning `docker stop`'s full 10-second grace on every
  container.

- **Process:** a PR checklist item now asks explicitly whether an
  existing test was weakened to make the change pass (#413, #414). The
  opt-out helper removed in this cycle had been concealing #402 and
  #408 for months behind an accurate comment describing exactly what it
  did — which is the argument for a check that fails rather than prose
  that decays.

## v1.3.5

A patch release whose headline is an observability defect: a DHCP
server outage was **invisible** on a bound endpoint. `dhcp_timeouts`
never moved, so the one counter meant to expose a dead DHCP server
stayed at zero for the entire outage. Also adds persistent host-bridge
setup guidance, which the bridge-mode walkthrough had been missing
since the fork began. No new driver options, no manifest changes;
existing networks keep working unchanged.

What changed:

- **A DHCP server outage never reached `dhcp_timeouts`** (#353). The
  outage watchdog only counted while a client was in its "acquiring"
  state, and nothing ever put a *bound* client back into it. The
  premise was that `dhcpcd` fires a lease-loss hook when a lease
  lapses; under `--noconfigure`, which this plugin always uses, it
  does not — a lapsed lease is reported as `RELEASE`, which is
  indistinguishable from the one a graceful stop emits and therefore
  cannot be counted as a failure. So for every endpoint that had
  successfully bound, a server outage produced no counter movement and
  no log line, for as long as it lasted. Detection is now derived
  rather than awaited: each bind and renew records the lease lifetime
  the server granted, and the watchdog reports an outage once
  `lease + grace` has passed with nothing heard.

  **Operator-visible consequence:** detection is not immediate, and
  cannot be. A valid lease means a working address, so an outage is
  only provable once that lease would have run out — on a 24-hour
  lease, up to ~24 hours. A client that never binds at all is still
  reported within `OUTAGE_GRACE`. If you alert on `dhcp_timeouts`,
  it now moves where it previously never would; alerts calibrated
  against the old (silent) behaviour will start firing correctly.

- **New settings `OUTAGE_TICK` and `OUTAGE_GRACE`** (#278), defaulting
  to `30s` and `25s` — the watchdog's re-check period and the settling
  time added on top of the lease before an outage is called. Most
  deployments should leave them alone. `OUTAGE_GRACE` must stay above
  the time a healthy client needs to acquire its first lease; below
  that, ordinary container start-up is reported as an outage. The
  plugin logs a warning at startup whenever either is overridden.

- **Shutdown and attach races closed** (#338, #324). Paths that #330
  left as heuristics are now deterministic, and a new
  `displaced_stops` counter records attaches that found a manager
  already registered for the same endpoint and stopped it — a few are
  normal after a plugin restart; a steady climb alongside
  `recovered_ok` means a container is in a restart loop.

- **Bridge mode: the host bridge now has persistent setup guidance**
  (#352). The walkthrough gave only imperative `ip link` commands, so
  the bridge was lost on the next reboot — and the failure presents
  confusingly, because `docker network create` still succeeds and only
  container attach breaks. There are now complete stanzas for
  ifupdown, netplan, systemd-networkd and NetworkManager, a prominent
  warning that the host's own address moves to the bridge (applying it
  over SSH with the NIC still configured drops the connection and does
  not give it back), firewall-rule persistence, and why STP must stay
  off — with STP on, every container `veth` waits out two forwarding
  delays before it forwards, which breaks the container's DHCP while
  leaving the host's own lease untouched. Three of the four recipes are
  verified end-to-end through a real reboot by
  `scripts/verify-bridge-boot.sh`; the NetworkManager one is
  documented as unusable on Ubuntu, where netplan owns ethernet.

Also in this release: the failure-injection suite now proves the
failure it injects rather than passing on a technicality (#278), and
the documentation was restructured by audience with an at-a-glance
option index (#345, #344, #343).

## v1.3.4

A patch release fixing three lease-lifecycle defects, one of which
broke DHCP renewal outright whenever two containers shared an
interface name — the ordinary case for anything using the default
`eth0`. No new options, no manifest changes; existing networks keep
working unchanged.

What changed:

- **Lease events could be lost between the client and the plugin**
  (#332). The one-shot `dhcpcd` reports each lease over a FIFO. The
  goroutine that reaped the exited client closed that FIFO
  immediately, racing the goroutine still reading a `bound` event
  sitting in the kernel pipe buffer. Under CPU load — a multi-service
  `docker compose up` is the realistic trigger — the event was lost
  about 4% of the time per acquisition (0% on an idle host), and the
  container failed to start with `dhcpcd did not output a lease` even
  though the server had granted one. The FIFO now has a dedicated
  keep-alive writer: the reaper closes only the writer, and the reader
  drains to a natural EOF, so the event cannot be dropped.
- **`lease_timeout` is now a retry budget** (#332). Transient
  acquisition failures are retried within it (500 ms plus jitter
  between attempts) instead of aborting the container start on the
  first one. Permanent failures — a missing interface, a malformed
  option — still fail immediately rather than burning the whole
  budget, and the error chain is preserved, so the existing 502
  mapping, probe diagnostics, and stderr tails are unchanged. Reaching
  the timeout now means the exchange genuinely never succeeded.
- **Two containers on the same interface name broke each other's
  renewals** (#332). `dhcpcd` keys both its state directory *and* its
  runtime directory (pidfile, control socket) by interface name, with
  no runtime override for either. The plugin isolated only the state
  directory, so with two containers both on `eth0` the second client
  found the first's control socket, forwarded its arguments to that
  process and exited 0 — silently, with status 0 — without ever
  running a client of its own. Its lease was then never renewed and
  never released, while the first client reloaded the wrong
  configuration. Both directories are now shadowed by a private
  `tmpfs` in each client's own mount namespace. A new integration test
  runs two containers past their renewal deadline and asserts each
  gets its own ACKs; it fails on the pre-fix code.
- **Concurrency and shutdown audit** (#332). Also fixed in the same
  pass: two drain races where a `Start`/`Stop` error path closed the
  netns and netlink handles while an event consumer was still live;
  a failed start's late cleanup that could evict a newer healthy
  manager from the registry (removal is now identity-checked); a
  displaced manager left running when `Join` arrived over a recovered
  endpoint; `Plugin.Close` now closes the HTTP server before draining
  managers, so a `Join` arriving during shutdown cannot leak an
  unstopped client; bridge-mode `DeleteEndpoint` tolerates an
  already-vanished veth, matching macvlan; `interface not found` exits
  are treated as terminal instead of retried; and `Finish` without a
  preceding `Start` no longer panics.
- **Goroutine and file-descriptor leak per DHCP client** (#332). The
  logrus writer pipes wired to the client's stdout and stderr were
  never closed, leaking two goroutines and a pipe pair per run. The
  `SIGHUP` log-reopen path also swapped the logger's output without
  logrus's mutex — a data race that could close the writer under an
  active write; it uses `SetOutput` now.
- Documentation: `docs/internals.md` now describes both directories
  the private mount namespace shadows and why the runtime one matters,
  and the `lease_timeout` entries in the reference and macvlan/ipvlan
  docs describe the retry semantics. The contribution requirements in
  the README now state the project's authorship rule, which has been a
  required CI check since #335/#336 but was documented nowhere (#335).
- CI and dependencies: the `govulncheck` gate now rejects an
  inconclusive scan instead of passing on a verdict-less report, and
  the stale `GO-2026-5746` allowlist entry is dropped now that it is
  no longer reported (#333); dependency bumps for the Go toolchain
  image, `golang.org/x/sys`, and eight pinned GitHub Actions (#326,
  #327, #331).

Thanks to **@jridgewell** for reporting the `dhcpcd did not output a
lease` failures (#325) and for the initial retry patch — the retry
behaviour above is that idea, reworked to keep permanent failures
fast and the error chain intact.

## v1.3.3

A patch release fixing a bug as old as the fork: containers running as
a **non-root user** never got a working renewal client.

What changed:

- **Persistent DHCP client now starts for non-root containers** (#317).
  The client enters the container's network namespace via
  `/proc/<pid>/ns/net`, which the kernel guards with a ptrace check:
  the opener must share the container's uid or hold `CAP_SYS_PTRACE`.
  The plugin manifest granted neither for non-root containers, so any
  service with a Dockerfile `USER` (or compose `user:`) kept its
  initial lease but was silently never renewed and never released —
  addresses leaked server-side on every recreate, and per-endpoint
  `ip=` requests for a previous address were declined while its stale
  lease lived. The manifest now requests `CAP_SYS_PTRACE`; **the fix
  takes effect on plugin (re-)install** (a `docker plugin upgrade` or
  the remove-and-recreate flow — see the Upgrade section of the
  reference).
- **New `join_start_failures` health counter** (#317). A persistent-
  client start failure at attach time now flips `/Plugin.Health` to
  unhealthy instead of hiding in the plugin's log file. The underlying
  await errors also carry their real cause now (the EACCES above
  spent weeks disguised as `context deadline exceeded`).
- Housekeeping: the Go Report Card badge is gone — goreportcard.com is
  end-of-life; staticcheck/gofmt remain required CI gates (#318).

## v1.3.2

A CI-only patch release: the plugin's runtime code is identical to
v1.3.1 — no functional changes, no new options, no manifest changes.
Published so the version tag, docs pins, and CI behaviour stay in
lockstep.

What changed:

- **The integration suite skips runs that add no signal** (#311, #312).
  Two event shapes previously held the serialized privileged CI slot
  for ~22 minutes each without exercising anything: PRs whose diff
  touches nothing but `*.md`, and post-merge pushes whose source tree
  is byte-identical to a tree that already passed the suite (the PR's
  own pre-merge run). An in-job gate now detects both and exits in
  seconds — the required check still reports, so nothing gets stuck
  waiting on a workflow that never triggers. The gate fails open: any
  API error or ambiguity runs the full suite. Any code, script,
  workflow, or Dockerfile change is unaffected and always runs
  everything.

## v1.3.1

A patch release: one bug fix in the `validate_dhcp` pre-flight probe,
plus test-pipeline hardening. No new features, no manifest changes, no
new options.

What changed:

- **`validate_dhcp` no longer misreports live DHCP servers as
  unreachable on slow hosts** (#307). The probe's time budget assumed
  sub-second client startup; on slower or virtualized hosts (SBCs, NAS
  boxes, nested Docker), dhcpcd startup plus one lost-and-retransmitted
  DISCOVER could exceed it, failing `docker network create -o
  validate_dhcp=true` against a perfectly working server. The budget is
  now sized for that worst case (5s → 8s). Successful probes are
  unaffected (they return as soon as the lease lands, typically 1–2s);
  only the genuine-failure path reports ~3s later. The error message now
  reads `no DHCP OFFER on <parent> within 8s`.
- **CI: privileged integration and coverage jobs are serialized**
  (#296). Parallel runs on the shared runner host stretched
  DHCP-timing-sensitive tests past their budgets; a shared concurrency
  group restores one-at-a-time execution.
- **Coverage floors reconciled and ratcheted** (#303, #305). Two
  per-package coverage floors were stale carry-overs from the pre-dhcpcd
  client (one 25 points below what the suite delivers), leaving room for
  silent regressions; floors now match measured reality. New unit tests
  cover the state-persistence failure paths (temp-file creation and
  atomic-rename errors), raising and re-pinning the plugin package
  floor.
- **CI workflow audit** (#207). All workflows reconfirmed as earning
  their keep; a stale comment misdescribing the coverage workflow's
  fork-trigger security posture was corrected.

## v1.3.0

A feature release that builds new DHCP capabilities on the dhcpcd client
shipped in v1.2.0: opt-in dynamic-DNS registration, classless static
routes, and more captured DHCP options.
The plugin manifest is unchanged; all new behaviour is opt-in except the
classless-routes default (see the compatibility note below).

What changed:

- **Opt-in dynamic-DNS registration** (#261). New `-o register_dns=true`
  sends the DHCP FQDN option (81 on v4 / 39 on v6) built from the container
  hostname, asking the server to register forward (A/AAAA) + reverse (PTR)
  DNS. Best-effort and advisory — many consumer routers ignore option 81 —
  and off by default, since DDNS registration is a network-policy decision.
- **Classless static routes** (#260). DHCPv4 option 121 (RFC 3442, plus the
  legacy option 33 and MS option 249 encodings) is now honored: routes the
  server pushes are programmed into the container at `Join`. `-o
  skip_routes=true` suppresses them along with parent route-copying.
- **More captured DHCP options** (#262). The observe-only log line now also
  surfaces WPAD (option 252) and timezone (options 100/101 and legacy
  option 2), alongside the existing NTP / TFTP / boot-file fields.
- **Supply chain**: the image `:latest` tag is now a crane retag of the
  signed version digest rather than a separate unsigned rebuild (#267), so
  `:latest` and the matching `vX.Y.Z` share one signed digest. The docs
  toolchain in the Pages workflow is hash-pinned (#268).
- **Project**: the internal DHCP packages were renamed off their busybox
  origins (`pkg/udhcpc` → `pkg/dhcp`, `cmd/udhcpc-handler` →
  `cmd/dhcp-handler`, #245), and the integration suite gained a per-test
  timing summary plus several runtime cuts (#253, #254, #255, #276).

Operator-visible compatibility: classless static routes (option 121) are
honored **by default** (`skip_routes` defaults to `false`). If your DHCP
server pushes option-121 routes, containers will pick them up after the
upgrade where previously they were ignored; set `-o skip_routes=true` to
keep the old behaviour. The other new feature (`register_dns`) is off by
default — no action is required to upgrade from v1.2.x.

## v1.2.0

A DHCP-client modernization release. The plugin's DHCP client changes
from BusyBox `udhcpc`/`udhcpc6` to **`dhcpcd`**, run observe-only
(`--noconfigure`) so the plugin keeps sole ownership of interface
configuration. This unblocks DHCPv6 feature parity and adds per-family
observability. **Driver options and the plugin manifest are unchanged**;
bridge / macvlan / ipvlan UX is identical to v1.1.x.

What changed:

- **dhcpcd replaces busybox udhcpc/udhcpc6** (#152). The DHCPv6 IA is now
  unified across the one-shot acquisition and the persistent client (both
  present an identical DUID-LL + IAID derived from the endpoint's stable
  MAC), so the **Docker-visible IPv6 address is renewed** instead of being
  acquired once and left to expire.
- **Requested/preferred IPv6 address** (#213): a recorded or
  `--ip6` / `AddressIPv6`-requested address is surfaced to the DHCPv6
  client as an IA_NA preferred-address hint, and carried across restarts
  in the tombstone — the v6 counterpart of the existing v4 behaviour.
- **Per-family health counters** (#212): `lease_changed_v6`,
  `leases_obtained_v6`, `leases_renewed_v6`, `dhcp_timeouts_v6`,
  `naks_received_v6` on `/Plugin.Health`. The un-suffixed counters remain
  v4+v6 totals, so the v4 share is the aggregate minus its `*_v6`.
- **ipvlan DHCP broadcast** (#243): the broadcast flag is now actually
  emitted for ipvlan-L2 initial acquisition (where slaves share the parent
  MAC). The dhcpcd port had declared the option but dropped it — a
  regression vs v1.1.x, fixed before release.
- **Robustness**: dhcpcd's interface-setup sysctl writes now succeed on
  hosts where the plugin's `/proc/sys` is read-only (#247), and network
  IDs are validated before they reach state-file paths (#232,
  path-injection hardening).
- **Project**: a versioned documentation site (#133), governance /
  code-of-conduct / security-assurance docs (#216), a Trivy rootfs CVE
  scan and an opt-in hosted CI cross-check lane (#143, #238), and an
  automated release version-pin bump with a consistency gate (#251).

Operator-visible compatibility: the v4 client-id is still sent with the
type-0 ("opaque") prefix the busybox path used, so existing DHCP server
reservations keyed on it keep matching after the upgrade. No action is
required to upgrade from v1.1.x.

## v1.1.1

A compliance and project-hygiene release. **There are no functional
plugin changes** — bridge / macvlan / ipvlan behaviour, every driver
option, and the on-disk and `/Plugin.Health` surfaces are identical to
v1.1.0; the published image is functionally the same (a new digest, as
expected for a new tag). What changed:

- **OpenSSF Best Practices passing badge** earned and added to the
  README ([project 13229](https://www.bestpractices.dev/projects/13229)).
- **OpenSSF Scorecard Branch-Protection** is now evaluable: the Scorecard
  workflow is given a read-only, single-repo token so the check can read
  branch-protection settings (it previously scored as inconclusive). With
  this and the badge, all three previously-open Scorecard checks resolve.
- **Contributing documentation**: a `Contributing` section in the README
  covering how to report issues, the pull-request flow and required CI
  checks, the coding standard, and the tests-with-changes policy.
- **Issue and pull-request templates**: structured bug-report and
  feature-request forms (with a private security-advisory link) and a PR
  checklist.
- Maintainer-facing: the release runbook now documents post-release
  branch cleanup, and the repository auto-deletes merged PR branches.

## v1.1.0

A security, supply-chain, and toolchain-maintenance release. **There are
no functional plugin changes** — bridge / macvlan / ipvlan behaviour,
every driver option, and the on-disk and `/Plugin.Health` surfaces are
identical to v1.0.0. A network created against v1.0.0 behaves exactly the
same on v1.1.0; what changed is how the artifacts are built, signed, and
verified, and the security posture of the CI that produces them. This
release takes the project's OpenSSF Scorecard to a clean sheet.

### Supply chain & release integrity

- **Signed images** — published plugin images are now signed with cosign
  (keyless / Sigstore) by digest, on both GHCR and Docker Hub (#173).
- **Build provenance** — SLSA build-provenance attestations for the image
  and for every release artifact, verifiable with `gh attestation verify`
  (#173).
- **SBOM** — an SBOM in both SPDX and CycloneDX formats is generated over
  the plugin rootfs and attached to each release (#174).
- **Signed checksums & tags** — the release `checksums.txt` manifest is
  cosign-signed so a single signature covers every attached artifact, and
  release tags are now GPG/SSH-signed (#163, #175).
- Each GitHub Release now carries copy-pasteable verification commands
  under *Verifying the signed artifacts*; the README has a short-form
  [Verifying releases](README.md#verifying-releases) section.

### CI security hardening

- **CodeQL** advanced setup analysing Go (the primary language, previously
  unscanned) and the Actions workflows, wired in as a required check
  (#170).
- **Dependency Review** action blocks PRs that introduce high-severity
  vulnerable dependencies at review time (#171).
- **GitHub security toggles** — Dependabot security updates, secret-scanning
  push protection, and private vulnerability reporting are enabled; the
  Dependabot and CodeQL checks are required on `dev`/`main` (#172).
- **Native fuzzing** — Go fuzz targets over the untrusted DHCP-response
  parsers (the env-var event path and the handler-pipe JSON decoder) run
  time-boxed on every PR and satisfy Scorecard's Fuzzing check (#162).
- **Scorecard to 10** — all GitHub Actions and container images are
  SHA-pinned, workflow tokens are scoped to least privilege, and the
  remaining OSV advisories were triaged (unreachable client-library
  findings dismissed with rationale; migration off the frozen
  `github.com/docker/docker` module tracked separately) (#159, #160,
  #161).

### Toolchain & CI fixes

- **Go 1.26.4** across the runner image, `go.mod`, the plugin Dockerfile,
  and all workflow toolchains; actionlint bumped to v1.7.12 (#177).
- **Runner image honors `HTTP(S)_PROXY`** for the nested plugin docker
  build behind a forced-egress proxy (#181).
- **`check-apk-pins.sh` false positive fixed** — `apk policy` emits
  candidate versions with a trailing colon, which the drift check now
  strips before comparing, so current pins are no longer flagged as
  drifted; a table-driven self-test guards the regression (#169).
- **actionlint determinism** — shellcheck is pinned in the actionlint job
  so a hosted-image bump can't silently redden unrelated PRs; the SC1003
  false-positive in the cosign block was resolved (#183).
- Earlier in the cycle, the nested-dockerd `plugin disable/enable`
  failure on the ephemeral CI runner (cgroup.kill `EOPNOTSUPP`) and the
  Scorecard badge were addressed (#158, #176).

## v1.0.0

The 1.0 milestone: the fork's quality infrastructure is now mechanical
(coverage ratchet, failure-injection suite, option-docs drift gate,
dependency automation, supply-chain scanning), runtime failure
behaviour is asserted rather than assumed, DHCPv6 gets its first test
coverage — which immediately found and fixed real bugs — and the
audit/observability surface grew. No breaking changes; bridge-mode
defaults and all existing options are unchanged.

### New features

- **`audit_log=true` driver option (#109)** — append-only lease ledger
  (`STATE_DIR/leases.jsonl`, one JSON object per line: timestamp, kind
  `bound`/`renew`/`release`/`release_failed`, network, endpoint,
  container, hostname, IP, MAC). Answers "which IP did this container
  hold last Tuesday?" without DHCP-server-log archaeology. Rotated at
  16 MB / 30 days, one rotated generation kept (≤ ~32 MB). Off by
  default (privacy: container↔IP correlation on disk). Append failures
  bump the new `ledger_write_failures` health counter and never affect
  lease handling.
- **`interface_name` support, plugin side (#125)** — the
  `com.docker.network.endpoint.ifname` endpoint option (Compose
  `services.*.networks.*.interface_name`, engine 28+) is validated and
  honored: the plugin returns the requested name as `DstName` in its
  Join response; kernel-invalid names fail the attach with a clear
  error. **Engine limitation:** moby's remote-driver layer currently
  discards both the option (in Join) and the returned name, so the
  rename does not yet take effect for *plugin* drivers on any engine —
  built-in drivers only. The plugin side activates automatically once
  the upstream pass-through ships; acceptance tests are probe-gated
  and self-activating.
- **`naks_received` health counter (#128)** — a DHCPNAK was previously
  a log line only; now it's an operator-visible counter. Climbing
  alongside `lease_changed` means containers are being re-addressed
  mid-life.

### Fixes (all found by this release's new test coverage)

- **`ipv6=true` was non-functional in v0.9.0** (#103): the
  option-request flags added in v0.9.0 (`-O mtu` …) are a v4-only
  vocabulary; udhcpc6 treats unknown option names as a hard startup
  error, so every DHCPv6 exchange exited immediately. Option requests
  are now family-specific.
- **systemd-udevd MAC rewrites broke DHCP identity** (#103/#128): on
  hosts with `MACAddressPolicy=persistent` (the Debian default), udev
  replaced a freshly created macvlan child's randomly-assigned MAC
  moments after creation — so the initial DHCP exchange ran from a
  MAC the container never uses (MAC-keyed server reservations could
  not match the first lease) and the DHCPv6 one-shot poisoned the
  server's neighbor cache, blackholing the container's v6 client for
  ~45 s. The plugin now pins the child's MAC at creation
  (administratively-set MACs are exempt from the udev policy), as the
  bridge path always did for its veths.
- **A changed lease was never applied to the container link** (#128):
  `renew()` updated counters, DNS/MTU, and routes, but not the
  address itself. After a NAK re-acquisition or a server-side
  renumbering, the container kept answering on the old (possibly
  reassigned) address, and the default-route replacement failed with
  "network is unreachable", black-holing the endpoint. v4 re-binds
  now apply the new address (then routes); the v6 arm is deliberately
  deferred to the IA unification (#152).
- **Test-harness exec output demux** (#130): `docker exec` streams are
  multiplexed; raw reads embedded frame headers in line-anchored
  parsing and caused a ~11% golden-path flake.

### DHCPv6 (#103)

- First v6 coverage in the integration suite: dual-stack dnsmasq
  fixtures (ULA prefixes, RA enabled), golden paths for macvlan and
  bridge wiring, DNS6 propagation default-off, failure-only
  wire-capture diagnostics (tcpdump + neighbor tables) that
  root-caused the udev bug above.
- Audit finding (premise correction): busybox udhcpc6's DUID is a
  **DUID-LL derived from the interface MAC** — stable across
  restarts, so v6 reservations stick wherever MACs are stable (which
  the plugin guarantees via the new MAC pin + tombstones). No
  STATE_DIR DUID store needed. New docs section covers the exact
  create incantation, RA-delegated gateways, the ipvlan shared-DUID
  caveat, and the no-static-v6 boundary.
- **Known boundary:** the initial-lease and renewal clients negotiate
  separate identity associations, so the v6 address Docker reports is
  not yet the one being renewed. Documented in the DHCPv6 section;
  unification tracked in #152 with three suite tests skipped-with-
  reference until then.

### Failure-injection test suite (#128)

Three `TestFailure_*` scenarios against per-test ephemeral DHCP
servers assert *intended* degraded-mode behaviour: server loss during
renewal (address retained, Healthy, self-recovery on server return),
lease refusal on renewal via server renumbering (unattended
re-acquisition; stale-`docker inspect` as the documented #104
divergence), and full lease expiry (deliberate retention, endpoint
stays reachable). Split behind `make integration-test-failure` so
main-suite feedback speed is unchanged. The udhcpc handler now
validates v4 lease env (malformed input skips the event instead of
failing mid-renewal), and every udhcpc lifecycle event's counter
semantics are pinned in unit tests.

### CI / process / supply chain (#127, #132, #144–#148)

- actionlint + govulncheck (allowlisted findings carry justification
  + review date) + weekly cron + Dependabot (grouped, weekly) +
  CodeQL + OpenSSF Scorecard (published results, README badge) +
  SECURITY.md with private-advisory reporting.
- **Coverage ratchet**: per-package floors enforced on every release
  PR; the baseline only moves up.
- **verify-install**: every release run installs the just-published
  plugin from GHCR on a clean runner and asserts it enables.
- **Pre-release rc tags**: `vX.Y.Z-rcN` runs the full publish chain
  without touching `:latest` — every release is preceded by a
  zero-impact dry-run.
- **Driver reference manual** (`docs/reference.md`) — every option,
  setting, health field, and a troubleshooting table; CI fails if a
  parsed option key is missing from it.
- `integration` is a required PR check; the suite is
  containerized-runner-ready (supervisor-agnostic daemon restart,
  #145) with timeout budgets sized from slow-hardware measurements
  (#146) plus the failure suite.
- Release workflow `GITHUB_TOKEN` narrowed to job-scoped permissions.

### Operator-visible compatibility notes

- `/Plugin.Health` gains `naks_received` and `ledger_write_failures`
  (neither affects `healthy`).
- macvlan children now carry an administratively-set MAC (same value
  the kernel assigned; pinned against udev rewrites). If you run
  custom `.link` rules keyed on `addr_assign_type`, note the change.
- v4 re-binds that change the lease now re-address the container link
  (previously the link silently kept the stale address).
- The shipped binary is built with the current Go 1.25.x toolchain,
  curing four stdlib CVEs that affected the v0.9.0 build; two
  moby-daemon-side advisories remain allowlisted with justification
  (no fixed release exists) — see `.github/vuln-allowlist.txt`.

## v0.9.0

DHCP-helper polish: option propagation, parent-attached parity,
truthfulness counter, DHCP-wire health metrics, configurable
client-id and vendor class, NTP / search-list / TFTP capture,
pre-flight DHCP probe. Tier 1 (#100, #101, #102, #104) plus
T2-2 (#105), T2-3 (#106), T2-4 (#107) and T2-5 (#108) closed.

### New driver-opts (opt-in, default off for backwards compatibility)

- **`propagate_dns=true`** (#100) — write DHCP option 6 / 23 (DNS
  server list) into the container's `/etc/resolv.conf` on every
  bound/renew. Implemented via setns into the container's mount
  namespace; survives until Docker rewrites the file (typically on
  `docker network connect/disconnect`). Off by default because
  flipping it on changes name-resolution behaviour for every
  container on the network.
- **`propagate_mtu=true`** (#101) — apply DHCP option 26 (Interface
  MTU) to the container link via `LinkSetMTU` on bound/renew. Off
  by default because some networks advertise non-standard MTUs for
  reasons unrelated to host capability; opt-in keeps the behaviour
  change visible.
- **`client_id=<string>`** (#106) — overrides the endpoint-derived
  DHCP option 61 (Client Identifier) for every endpoint on this
  network. Bytes go on the wire prefixed with type byte `0x00`
  (RFC 2132 opaque). Default empty leaves the per-endpoint stable
  id in place, which is what makes per-container reservations
  work upstream — operators only set this when class-based DHCP
  policy demands a known client-id.
- **`vendor_class=<string>`** (#106) — overrides the default
  DHCP option 60 (Vendor Class Identifier) value of
  `docker-net-dhcp`. Lets DHCP servers using class-based policy
  (Cisco / Aruba / etc.) differentiate net-dhcp containers from
  other clients on the same LAN, e.g. to issue a different gateway
  or option set to containers tagged with a known vendor string.
  Default empty falls back to the historical `docker-net-dhcp`
  string. v6 unaffected — udhcpc6 doesn't accept the option.
- **`validate_dhcp=true`** (#108) — pre-flight DHCP probe at
  `docker network create` time. Creates a temporary macvlan child
  on the parent NIC with a random locally-administered MAC, runs
  one-shot udhcpc with a 5-second budget, and rejects the network
  with `no DHCP OFFER on <parent> within 5s` if no server answers.
  Catches misconfigurations (parent isolated, firewall blocking
  UDP/67-68, broken VLAN) at create time rather than the first
  `docker run`. macvlan / ipvlan modes only — bridge mode rejects
  the opt with a clear error. Cost: one transient lease in the
  upstream pool per probe (busybox udhcpc has no DISCOVER-only
  mode); the lease times out naturally.

### Additional captured DHCP options (#105)

`udhcpc-handler` now captures four more options on every bind /
renew event and surfaces them via the plugin log at info level
(only when at least one is non-empty, so plain LANs see no extra
noise):

- **Option 42 (NTP servers)** — captured into `Info.NTPServers`.
  Not auto-applied to the container; workloads needing NTP
  consume the value via plugin logs or future tooling.
- **Option 119 (DNS Search List)** — captured into
  `Info.SearchList`. When `propagate_dns=true`, the plugin emits
  every entry on the container's `search` line in
  `/etc/resolv.conf`. RFC 3397 precedence: option 119 supersedes
  option 15 (`Domain`) when both are present; option 15 is the
  fallback otherwise.
- **Option 66 (TFTP server name)** — captured into
  `Info.TFTPServer`. Surfaced via plugin log; not auto-applied.
- **Option 67 (Boot file name)** — captured into `Info.BootFile`.
  Surfaced via plugin log; not auto-applied.

The udhcpc command line now requests `mtu`, `search`, `tftp`,
and `bootfile` explicitly via `-O`. Busybox already requests
`ntpsrv` (option 42) by default. RFC-conformant servers only
return options the client asked for; this block is always-on but
free when the server doesn't supply them.

### Behaviour change (default flipped — see compatibility note)

- **Static-route copy in macvlan/ipvlan** (#102) — pre-v0.9.0,
  parent-attached modes never inherited host-side routes from the
  parent NIC; only bridge mode did. v0.9.0 extends the bridge-mode
  behaviour to macvlan and ipvlan for parity. The existing
  `-o skip_routes=true` opts out for users who depended on the
  no-copy behaviour.

### Observability

- **`lease_changed` counter** (#104) — new field on the
  `Plugin.Health` JSON. Bumps when a DHCP renewal returns a
  different IP than the manager last recorded. Docker's
  `NetworkSettings.IPAddress` view does NOT update on lease change
  (libnetwork has no in-place endpoint-IP swap RPC); this counter
  is the operator-facing signal that a stale-inspect window has
  opened. A deeper fix (forced container restart on lease change,
  or out-of-band docker-socket update) is deferred — see #104 for
  the design discussion.
- **DHCP-wire counters** (#107) — four new fields on
  `Plugin.Health`: `leases_obtained`, `leases_renewed`,
  `dhcp_timeouts`, `lease_release_failures`. Bumped from the
  persistent client's event loop (`bound`, `renew`, `leasefail`)
  and from `dhcpManager.Stop`'s `client.Finish` failure branch.
  Operators alerting on DHCP-side regression no longer have to
  scrape dnsmasq logs server-side or run the plugin at trace
  level. Naming intentionally drops the Prometheus `_total`
  suffix to stay consistent with the existing `Plugin.Health`
  fields (`recovered_ok`, `tombstone_write_failures`, etc.).

### Compatibility note

The macvlan/ipvlan static-route default flipped from "don't copy"
to "copy" in v0.9.0. If your existing macvlan setups had
operator-added routes on the parent NIC that you specifically did
NOT want inside containers, add `-o skip_routes=true` to the
network's create options. The bridge-mode default is unchanged
(it's always copied; v0.9.0 just extends the same behaviour to the
other modes).

### Coverage

Combined unit + integration coverage harvested by the manual
`Coverage` workflow on the dev branch:

| package | merged |
|---------|--------|
| `pkg/util` | 87.7% |
| `pkg/plugin` | 82.4% |
| `pkg/udhcpc` | 81.3% |
| `cmd/net-dhcp` | 75.0% |
| `cmd/udhcpc-handler` | 50.0% |
| **overall** | **76.5%** |

`pkg/plugin` moved from 68.9% (v0.8.0) → 82.4% — the new T1 / T2
features each shipped with their own integration tests, plus the
`BuildEvent` extraction in pkg/udhcpc and the extensive T2-3
vendor-class / client-id round-trip tests filled previously-
uncovered branches. First time the combined number is published.

## v0.8.0

Code-review fix sweep + automated release pipeline. No new features —
the change set is hardening + tooling. 23 issues closed (the 22
findings from the 2026-05-05 review pass plus the doc audit).

### Compatibility note

`IsDHCPPlugin` now matches only `devplayer0/docker-net-dhcp:*` and
`claymore666/docker-net-dhcp:*` (W-6, #74). The previous catch-all
let any image whose name ended in `docker-net-dhcp:<tag>` claim a
shared bridge — including third-party images that aren't actually
this plugin. Forks publishing under another namespace need to add
their namespace to `driverRegexp` in `pkg/plugin/plugin.go` to keep
cross-detect working on the same host. No effect for installations
using the upstream image or this fork's image.

### Robustness

- **Recovery counter** (W-2, #70) — `/Plugin.Health.recovery_failed`
  now reflects synchronous failures (`NetworkInspect`, `netOptions`,
  endpoint-prelude) in addition to the per-endpoint Start failures
  it already counted. Operators paging on `recovery_failed > 0`
  used to miss those classes.
- **Per-network recovery timeout** (W-7, #75) — recovery's per-iter
  Docker calls now use a 3s timeout each instead of sharing the
  whole 30s recoveryBudget; one stuck `NetworkInspect` no longer
  starves later networks.
- **Clean shutdown exit code** (W-3, #71) — `cmd/net-dhcp/main.go`
  filters `http.ErrServerClosed`, so plugin SIGTERM exits 0 instead
  of 1 with a spurious ERROR log line.
- **GetIP race + busy loop** (W-5, #73) — the events-collector
  goroutine in `udhcpc.GetIP` now hands the final lease back over
  a buffered channel (proper happens-before instead of a fragile
  defer-close ordering), and the loop honours channel close
  via `range` instead of busy-spinning on zero-value receives
  after the scanner exits.
- **udhcpc Finish on already-exited child** (I-3, #79) — `Finish`
  now treats `os.ErrProcessDone` on `Signal` as success and lets
  `Wait` reap, so a self-exited udhcpc doesn't leave a zombie.
- **Defensive prefix derivation** (I-2, #78) — `vethPairNames` and
  `subLinkName` no longer panic on short EndpointIDs; they match
  `shortID`'s pattern. Production EndpointIDs are 64 hex so this
  is unreachable today, but recovery on malformed daemon
  responses is now safe.
- **Close hygiene** (W-4 / I-1, #72 / #77) — `nsHandle` and netns
  Close errors land at Debug instead of being silently ignored.

### Logging and ergonomics

- **JSONErrResponse log levels** (I-12, #88) — 5xx logs at Error,
  4xx logs at Warn, others at Info. Caller-supplied bad input no
  longer drowns real failures at ERROR.
- **deconfig handler** (I-9 / I-8, #85 / #84) — the multi-line
  commented-out deconfig block in `dhcpManager.setupClient` is
  replaced by a one-paragraph rationale for why deconfig is
  intentionally not handled (would wipe Join's copied static
  routes). Git history preserves the original code.
- **Tombstone prune-on-write** (I-10, #86) — `consumeTombstone`
  skips the rewrite when the prune produced no change and no
  consume happened. Saves an fsync per CreateEndpoint on quiet
  networks.
- **Plugin.Close shutdown leak boundary** (W-8, #76) — documented.
  The WaitGroup watcher goroutine's leak on timeout is acceptable
  for process-exit only; a "do not copy this pattern into
  long-lived callers" banner now sits next to the code.

### Toolchain

- `go.mod` directive bumped 1.25.0 → 1.25.9 (W-1, #69). Drops
  `govulncheck` affecting count from 18 → 2 (the remaining two
  are upstream docker SDK vulns with no fix yet).

### Test surface

- Unit-test pin for `decodeOpts(nil)` behaviour (I-11, #87).
- Bridge integration fixture polls UDP/67 instead of sleeping 200ms
  (I-4, #80).
- `EnsureImage` decodes the pull stream and surfaces errors instead
  of silently swallowing partial pulls (I-6, #82).
- Misc style fixes — `bytes.Compare` over string-converted `net.IP`
  (I-5, #81), `DHCPClient.Start` doc comment on netns thread-locking
  (I-7, #83).

### CI

- **Coverage** (#90 / I-14) — the manual `Coverage` workflow now runs
  unit tests with binary-coverage instrumentation and merges them
  with the integration counters in the workflow summary, plus an
  HTML artefact for the merged profile. The integration-only output
  remains for back-compat.
- **APK pin drift** (#89 / I-13) — new weekly workflow (and ad-hoc
  `scripts/check-apk-pins.sh`) compares the Dockerfile's pinned
  `busybox-extras` and `iproute2` versions against the latest in
  the alpine package index. Reports drift in the workflow log so
  bumps land on the maintainer's desk without anyone having to
  remember to look.
- **Release pipeline** (#93) — new `release.yml` publishes to GHCR
  (always) and Docker Hub (gated on secrets) on `vX.Y.Z` tag push.
  Tagging stays a manual decision; the workflow only fires on a
  tag that already exists.

### Documentation

- `docs/parent-attached-modes.md` updated to current code state
  (#91): tombstone TTL is 60s (was documented as 10s, stale since
  v0.6.1); `/Plugin.Health` payload now lists all seven fields
  with semantics; new sections for DHCP identity (option 12 / 60
  / 61, the latter previously misdocumented as ipvlan-only) and
  plugin-restart recovery; env-var reference table covering
  `LOG_LEVEL`, `AWAIT_TIMEOUT`, `STATE_DIR` (the second was
  shipping since v0.4.x but never documented).
- `README.md` gains an Attachment-modes summary with a pointer to
  the parent-attached doc; the bridge-mode walkthrough is retitled
  so a reader knows it's mode-specific; debugging section mentions
  `/Plugin.Health` and the env-var knobs.

## v0.7.0

Live integration test harness running on every PR via a self-hosted
runner with the outside-collaborator approval gate enabled. The
plugin now has automated end-to-end coverage of the surface that
`go test` couldn't reach before — `CreateEndpoint`, `Join`, `Leave`,
`recoverEndpoints`, `dhcpManager.{Start,Stop}`, parent-attached link
wiring, the udhcpc client wrapper.

### What's exercised

Twelve active tests, suite-wall-clock ~3:30 on the runner:

- **Lifecycle** — full create→run→inspect→leave→delete in macvlan,
  bridge, and ipvlan-L2 modes. The ipvlan path required two
  plumbing fixes (closes #62): `udhcpc -B` so DISCOVER asks for a
  broadcast OFFER (ipvlan slaves share the parent MAC and have no
  way to demux a unicast OFFER addressed to that shared MAC), and
  not echoing the link's MAC back in `CreateEndpointResponse` (the
  kernel rejects any MAC change on ipvlan slaves with EOPNOTSUPP,
  even setting to the current value, so libnetwork must leave the
  link alone). Both behaviours are gated on `mode == ipvlan` and
  don't affect macvlan or bridge.
- **Tombstone** — `docker restart <ctr>` preserves MAC + IP via the
  v0.5.x stability mechanism.
- **Recovery: plugin recycle** — `docker plugin disable -f` +
  `enable` while a container is attached; `Plugin.Health.recovered_ok`
  ticks past zero and the container's IP/MAC survive.
- **Recovery: daemon restart** — `systemctl restart docker` with a
  `--restart=always` container; the daemon comes back, the container
  comes back, the IP/MAC are preserved (empirically via the tombstone
  path, since graceful shutdown ran `Leave`).
- **Concurrency** — four containers attached simultaneously each get
  a distinct lease; doubles as a deadlock smoke test against
  per-network-lock regressions.
- **Error paths** — option-validation rejections (invalid mode,
  missing parent, wrong-mode options, IPAM not null) plus
  netlink-state ones (parent down, parent is a bridge, malformed
  driver-opt ip).

### Architecture

A single shared `Fixture` covers both the parent-attached path
(`dh-itest-host` veth + dnsmasq on `192.168.99/24`) and the
bridge-mode path (`dh-itest-br2` Linux bridge + a second dnsmasq on
`192.168.100/24`). Distinct subnets keep the two DHCP servers from
racing. The bridge fixture handles the two known landmines that broke
earlier attempts: STP `forward_delay` defaulting to 15s (set to 0)
and Docker's default-DROP `FORWARD` policy combined with
`br_netfilter` causing bridged DHCP to be silently dropped (mitigated
with two narrow `iptables ACCEPT` rules scoped to the bridge
interface, removed in teardown).

### CI

`.github/workflows/integration.yml` runs the suite on a self-hosted
runner. Outside-collaborator approval is required for any PR from a
non-collaborator account, so external PRs can't get free root on the
runner host. The Go toolchain is pre-installed via
`test/integration/install-go-runner.sh` to skip `actions/setup-go`
and shave ~30s off every run.

### Coverage

Unit-test coverage is unchanged at the bundle level (`pkg/util` 64%,
`pkg/udhcpc` 34%, `pkg/plugin` 29%). The integration suite isn't
reflected in those numbers because the plugin runs as a separate
docker-installed process. Wiring up Go 1.20+ binary-coverage
instrumentation through the plugin image, with a host `GOCOVERDIR`
mount and an end-of-run `go tool covdata percent`, is tracked as the
first item of v0.8.0.

## v0.6.0

Bundle of code-review-driven fixes plus a unit-test coverage bump
(20.2% → 30.7%). Smoke-tested end-to-end on a live integration host:
golden path, multi-container, `docker inspect` truthfulness, LAN
reachability (gateway, peer, internet), `/Plugin.Health`, daemon
restart no-hang.

### Concurrency hazards eliminated (W-1, W-5, W-11)

- `recoverEndpoints` now runs synchronously inside `NewPlugin` before
  the listener accepts traffic, so a libnetwork RPC arriving during
  recovery can't race with the in-progress map writes (W-1).
- `dhcpManager.lastIP` / `lastIPv6` are now under a per-manager
  `ipMu`; the udhcpc-renew goroutine and Leave's reader were
  previously coordinating only via channel-drain, which the race
  detector couldn't always verify (W-5).
- New `TestTombstones_ConcurrentAddDoesNotLose` regression-tests the
  `tombstoneMu`-serialised read-modify-write path (W-11).

### Lifecycle stability (W-2, #44)

- `addTombstone` save failures now bump a
  `tombstoneWriteFailures` atomic counter, exposed on
  `/Plugin.Health.tombstone_write_failures` and folded into the
  top-level `healthy` boolean. Operators can detect a degraded
  restart-stability window (disk full, EROFS) instead of finding out
  on the next container restart (W-2).
- `DeleteNetwork` now runs `Stop()` on every DHCP manager attached to
  the disappearing network, fixing the recovery-then-network-removed
  leak where managers stayed in `persistentDHCP` forever (#44).

### Operational ergonomics (N-3, N-7, #30)

- `cmd/net-dhcp/main.go` reopens the log file on `SIGHUP` so
  `logrotate`-style external rotation works without restarting the
  plugin (N-3).
- `log.Fatal` calls replaced with a `fatalCleanup` helper that closes
  the log file before `os.Exit(1)`, preventing torn final writes
  during emergency exits (N-7).
- `Listen` does a best-effort `os.Remove(bindSock)` before
  `net.Listen` to clear stale socket files left over from unclean
  shutdowns (#30).

### HTTP error mapping widened (N-9)

`ErrToStatus` now returns:

- `502 Bad Gateway` for `ErrNoLease` (upstream DHCP server didn't
  respond — not our fault, not the caller's).
- `503 Service Unavailable` for `ErrNoContainer` / `ErrNoSandbox`
  (Docker is in a transient teardown/up state — retry later).
- `409 Conflict` for `ErrNoHint` / `ErrNotVEth` (stage state
  mismatch — request arrived in the wrong order).

Libnetwork lumps all 5xx the same so this is purely a clarity win for
direct API consumers, logs, and dashboards.

### API hygiene (N-8)

`util.ParseJSONBody` renamed to `ParseJSONOrErrorResponse`. The verbose
name makes the response-writing side-effect impossible to overlook —
a future caller writing `if err := ParseJSONBody(&req, w, r); err
!= nil { JSONErrResponse(w, err, ...); return }` would have
double-written headers; the new name doesn't read as a pure parse, so
callers reach for the right pattern.

### Dockerfile hardening (N-11, I-1, I-2, I-10)

- Alpine base pinned to `3.20.3` by digest
  (`sha256:d9e853e87e55…`), not the moving `:latest` or `:3.20` tag
  (I-1).
- `apk add` packages pinned by version: `busybox-extras=1.36.1-r31`,
  `iproute2=6.9.0-r0` (N-11).
- `CAP_SYS_PTRACE` removed from `config.json` — the plugin enters
  container netnses via `/proc/<pid>/ns/net` symlink resolution
  through `setns(2)`, which only needs `CAP_SYS_ADMIN`. The smoke
  test confirmed the plugin still works without the cap (I-2).
- `govulncheck` findings GO-2026-4887 / GO-2026-4883 documented in
  the "Acknowledged findings" preamble as not reachable from
  client-only `docker.Client` usage (I-10).

### Code-review polish

- INFO logs no longer leak MAC/IP at every endpoint event; that
  information is now emitted at DEBUG. INFO retains the
  `network`/`endpoint` (shortened) identifiers operators actually
  need to correlate (I-3).
- `dhcpClientReapTimeout` / `dhcpClientFinishTimeout` /
  `recoveryBudget` now have named constants instead of bare
  `5 * time.Second` literals scattered through the manager code
  (I-6).
- `StaticRoute.RouteType` integer literals (`0` / `1`) replaced with
  `RouteTypeNextHop` / `RouteTypeOnLink` constants from
  `pkg/plugin/endpoints.go` (I-8).
- `docs/parent-attached-modes.md` documents the Compose `external:
  true; name: <network>` merge gotcha that silently drops the network
  attachment when a base + override file declare the same network
  with the second one omitting `external: true` (#45).

### Test coverage 20.2% → 30.7%

Three rounds of unit-test additions:

- `pkg/util` 10.3% → 63.8% (`JSONResponse`, `JSONErrResponse`,
  `ParseJSONOrErrorResponse`, `AwaitCondition`, `WriteAccessLog`).
- `pkg/plugin` 20.1% → 28.7% (HTTP wrappers, `decodeOpts` edge
  cases, tombstone failure paths, `shortID` / `newChildLink`
  invariants, `updateJoinHint` read-modify-write, `dhcpManager`
  helpers, `parentAttachedEndpointOperInfo`, `newDHCPManager`
  constructor invariants).
- `pkg/udhcpc` 27.9% → 33.7% (RequestedIP carve-outs, vendor-id
  v4-only, binary selection, handler-script default/override, v6
  hostname FQDN encoding, always-on `-f`/`-i` flags).

The remaining ~70% is integration code (`CreateEndpoint`, `Join`,
`Leave`, `recoverEndpoints`, `dhcpManager.{Start,Stop}`,
parent-attached link wiring, `udhcpc.{Start,Finish,Wait,GetIP}`) that
needs a real netns + parent NIC + DHCP server. Tracked under #56 for
v0.7.0.

### Known limitation (fixed in v0.6.1)

Tombstone TTL was 10s, shorter than typical `systemctl restart
docker` window (15–30s). MAC/IP could change across daemon restart
even with `--restart=always`. v0.6.1 bumps the TTL to 60s. The
`docker restart <ctr>` contract was always covered.

## v0.5.3

Hotfix for a CPU-burning busy loop and a process-leak in the
persistent-DHCP path. Operators on v0.5.0 – v0.5.2 should upgrade.

Two of the three issues below are heritage upstream bugs that
nobody noticed because nobody is running upstream on a current
Docker. The CPU-burning one was introduced in this fork as an
incomplete fix to a different goroutine leak; v0.5.3 closes the
loop.

### Closed udhcpc event channel turned the consumer into a CPU spinner (fork-introduced)

Regression introduced in this fork's commit `d23ba50` ("Buffer
udhcpc and dhcpManager channels to prevent goroutine leaks", in
v0.5.0). Upstream's scanner goroutine in `udhcpc.Start` does not
close the events channel and uses a blocking unbuffered send, so
when udhcpc dies the consumer in `dhcpManager.setupClient` blocks
forever on `<-events` — a quiet goroutine leak (lease renewal
silently stops, no CPU burn). `d23ba50` correctly added a buffered
channel and `defer close(events)` to address that leak, but didn't
update the consumer's `case event := <-events` to handle the
close. After the close, every iteration of the consumer's `select`
took the now-always-ready `<-events` branch and got a zero-value
`Event{}`; the switch matched no case, the loop iterated, and the
goroutine pegged a CPU thread forever — silently, with no log
output. Observed in the field as ~70 % of one host core sustained
for 1 d 14 h with seven hot Go runtime threads.

The consumer now uses the comma-ok form, logs the unexpected close,
reaps the udhcpc child via `client.Wait` (see below), and posts to
`errChan` so a concurrent `Stop` doesn't deadlock waiting on a
goroutine that's already gone. Net effect: the v0.5.0 leak fix is
preserved, and the consumer no longer spins.

### Zombie udhcpc child when the process exited unexpectedly (upstream)

Pre-existing in upstream. `cmd.Wait` was only ever called from
`Finish`, which assumes `Stop` drives teardown. When udhcpc died on
its own, nobody called `Wait`, so the kernel kept the child as a
zombie until plugin shutdown. `udhcpc.Finish` is now split into a
signal phase plus a new `Wait(ctx)` method, and the consumer calls
`Wait` from the events-closed branch above to reap.

### `Await*` helper goroutines leaked on context cancel (upstream)

Pre-existing in upstream, byte-identical. `util.AwaitCondition`,
`AwaitNetNS`, `AwaitLinkByIndex`, and `AwaitContainerInspect` ran
their poll in a side goroutine that didn't observe `ctx`: when the
outer `select` returned via `<-ctx.Done()`, the poller kept calling
its expensive operation (Docker `NetworkInspect`,
`netns.GetFromPath`, `LinkByIndex`, `ContainerInspect`) every
100 ms forever, and blocked permanently on the unbuffered result
channel. Each leaked poller meant ~10 syscalls/s, accumulating
across plugin restarts and per-endpoint recovery attempts. All four
helpers are now synchronous loops with `select` on `ctx.Done()`
between iterations.

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

Phase D smoke on the integration host walked through D2 (LAN
IP), plugin disable/enable (lease persisted across the bounce),
teardown.

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
know the hostname they must agree." Verified live on the
integration host with a two-container sequential restart: each
container kept its own MAC, no swap.

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

Phase D walkthrough re-run on the integration host: D2,
restart-stability, C-5 sequential-restart, D6 distinct-leases, C-4
health counters, D9 plugin disable/enable recovery, D7
release-on-stop — all green.

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
each other), runs `dhcpcd` on it to acquire a lease from the LAN's DHCP
server, and hands the link to libnetwork. Docker moves the link into the
container's network namespace; a persistent `dhcpcd` keeps the lease
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
- capabilities: `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`

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

- The DHCP exchange uses `dhcpcd` (one process per family), run
  observe-only (`--noconfigure`) so the plugin keeps sole ownership of
  interface configuration. Features outside what dhcpcd performs in that
  mode (e.g. RFC 3315 reconfigure handling) are not surfaced.
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
  CreateEndpoint/Leave/DeleteEndpoint), and as of v1.2.0 the recorded
  address is surfaced to the DHCPv6 client as an IA_NA preferred-address
  hint (the dhcpcd migration, #152, replaced busybox `udhcpc6`, which had
  no `-r` equivalent for v6). A `--ip6` / `AddressIPv6` request is honored
  the same way. The server is still free to assign a different address;
  with a stable MAC and reservation-keyed server that is typically the
  same one.
- Concurrent `docker restart` of multiple containers on the same
  DHCP network within ~10 seconds falls back to fresh MACs (the
  tombstone mechanism requires exactly one match to avoid swapping
  identities). Sequential restarts — the typical case — are
  stable.
