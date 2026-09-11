# Roadmap

Where `docker-net-dhcp` is going over roughly the next year, and, the
more useful half, where it deliberately is not going.

This page is **direction**. It is not a delivery schedule. The project
is solo-maintained, so nothing here carries a date. Milestones remain
the per-release truth: a milestone says what is going into the next tag,
this page says what the tags are working towards and which shapes of
change will be turned down however well they are implemented.

## The bar every feature is measured against

One `docker network create` line, then plain `networks: [lan-dhcp]` in
any Compose file. No static IPs, no sidecars, no per-container plumbing,
no entrypoint script that has to know it is running on this network.

That bar decides most design arguments before they start. A workaround a
user has to script around the plugin is read here as a **bug report
against this principle**. [#125] is the worked example. Containers
landed on `eth0`, `eth1` and so on in attach order, which left users
scripting around the name, so the plugin now returns the requested
interface name to the engine.

## Where the project is today

This branch is the 2.0 line.
The plugin leases through the project's own in-tree DHCP client library
instead of an external client process, and it does so for both address
families: `ipv6=true` gives an endpoint a DHCPv6 lease alongside its
DHCPv4 one, at parity with the 1.x line ([#911]). Everything else in
this section describes the 1.x line 2.0 is replacing, feature for
feature, and stays true of it.

Bridge, macvlan and ipvlan attachment; DHCPv4 and DHCPv6; addresses that
survive `docker restart`, plugin restart and daemon restart; a
`/Plugin.Health` counter surface and the same counters in Prometheus
form on `/metrics` ([#651]); per-network choice of which DHCP server to
lease from ([#111], [#669]); signed, attested, reproducible releases.

v1.9.0 is the release that makes IPv6 usable. Before it the option was
present and did little. Before it, `ipv6=true` was accepted and did not
work on most segments: no container could start at all where the router
advertises stateless or SLAAC-only configuration ([#868]), a stateless
server's configuration was received and discarded ([#815]), and on a
managed segment the leased address stopped being refreshed and its
default route disappeared ([#875]). The same release stops the plugin
sending DHCPRELEASE on any path: an address is held until the lease
expires, like any other host's ([#800]).

v1.8.0 carried the first human review of the design and its trust
boundaries ([#457], [#699]), pulled into that release because it is the
one that opens a TCP port in a process holding `CAP_NET_ADMIN`. Every
finding was fixed inside the same cycle, and each one ends in a test or
a counter instead of a paragraph. The [driver reference](reference.md)
is the authority on what exists right now. If this page and that one
disagree, that one is right.

## The 2.x releases

Milestones, read from the tracker and never written here by hand. Each
release closes its milestone, so the issues below are what that tag is
working towards.

**[v2.0.0](https://github.com/claymore666/docker-net-dhcp/milestone/28)**
is parity with 1.9.0 on the project's own DHCP engine, plus the bugs the
milestone carries: [#911] tracks the IPv6 half, [#895] gives every ipvlan
endpoint its own DHCPv6 identity, and [#820] settles the path a
restarted container takes. The rest are test and CI defects:
[#881], [#802], [#682], [#879], [#839], [#827], and this page's own
sibling [#672].

**[v2.1.0](https://github.com/claymore666/docker-net-dhcp/milestone/30)**
puts the leased address into Docker's own address management. [#110]
bundles a DHCP IPAM driver, so `--ipam-driver null` stops being the only
supported shape: naming the plugin in its place makes `docker run --ip`,
`docker network connect --ip` and Compose's `ipv4_address` legal, fills
the IPAM block `docker network inspect` prints, and drops `null` from
the create line. `docker inspect` already reports the leased address in
either shape. `ipvlan` keeps `--ipam-driver null` for now ([#949]), and
so does IPv6: the IPAM shape is IPv4 only and refuses both Docker's
`--ipv6` and `-o ipv6=true` ([#960]). [#218], the deterministic MAC, is on the milestone and blocked
upstream (below).

**[v2.2.0](https://github.com/claymore666/docker-net-dhcp/milestone/29)**
is full IPv6. [#818] and [#808] acquire an address by SLAAC, [#821] takes
the gateway, DNS, MTU and routes from the advertisement, [#819] handles
lifetimes, withdrawal and renumbering, [#817] adds an `ipv6_mode` option,
[#814] parses Router Advertisements into a first-class event, [#214] is
prefix delegation, [#925] accepts a server-initiated Reconfigure, and
[#816] stops a v6 acquisition reporting a lease timeout where no
exchange was possible.

**[v2.3.0](https://github.com/claymore666/docker-net-dhcp/milestone/31)**
is the host plumbing an operator does by hand today: [#902] VLAN
sub-interfaces, [#903] a bridge the plugin creates and owns, [#904]
link-local fallback where no server answers, [#905] macvlan and ipvlan
sub-modes. [#903] sits inside the rule below that the plugin does not
change interfaces the host already has: the bridge is one the plugin
creates for its own networks and owns for as long as they exist, and no
interface the host configured is touched.

[#926] Rapid Commit and [#927] temporary addresses (IA_TA) carry no
milestone and are not scheduled. [#218], the deterministic MAC, is
backlog: it waits on upstream Docker ([moby/moby#52871], the table
below) and is scheduled only when that lands.

## Direction

Five themes, in rough order of how much they change for a user. Issue
numbers are anchors; the order is not a queue.

### 1. Quiet addressing faults

The hard failures were solved first; the quiet ones are the current
work. v1.6.0 added conflict detection ([#524]) because the plugin
accepted an address a statically-configured host already held and every
counter stayed at zero. The container came up, Docker reported an
address, and nothing anywhere said otherwise.

2.0 delivered the next step of this theme. The check moved off the
parent link and into the DHCP client, as RFC 5227 Address Conflict
Detection running inside the container's own namespace for the whole
life of the lease, with `conflict_check` choosing who pays for it
([`conflict_check`](reference.md#driver-options-network-level)). The
theme stays open: **every new failure mode gets a counter as well as a
log line**, and a counter that can read clean while the feature is
broken is treated as an unfinished instrument. Expect more of the health
surface, and more assertions made against what the DHCP server saw
instead of what the plugin believes.

### 2. Lease identity across recreates

A DHCP server keys on identity, so address stability is an identity
problem. Two pieces are designed, and neither is unplanned. [#218], the
deterministic MAC, is backlog and blocked upstream (below). [#219], a
stable client-id for ipvlan where every child shares the parent's MAC,
carries no milestone. 2.0 settled the DHCPv6 half of the same question:
an ipvlan endpoint now gets a DUID of its own ([#895]).

### 3. arm64

Users have asked for it and it works, proven on real hardware: a native
arm64 CI runner executes the full integration suite, timing behaviour
included, as a release-candidate gate ([#531]). The shipping shape is
per-architecture tags (`vX.Y.Z-arm64`, `latest-arm64`), first published
with v1.7.0 ([#507]). A Docker *plugin* cannot be installed from a
multi-architecture manifest list at all, so the architecture lives in
the tag and not in a manifest.

### 4. A test substrate that cannot lie

This is infrastructure work with a user-visible reason: on this project,
every timing crutch removed from CI turned out to be hiding a real
defect. An opt-out helper added to make a restart test pass hid a
user-facing `docker restart` failure for months. So the rule is enforced
by a gate and not by a paragraph: **a test that only passes once you
weaken something is a bug report**. The remaining soft spots ([#682],
[#403]) are on the list for the same reason features are.

### 5. Supply chain and documentation

Releases are already cosign-signed, provenanced, SBOM'd and
reproducible. The remaining pull is the OpenSSF Best Practices silver
and gold criteria, which are tracked as ordinary issues and generally
translate into something concrete. This page exists because
`documentation_roadmap` is one of them ([#452]).

## Blocked upstream

One feature is designed here and cannot ship until Docker's own engine
carries a change. It stays open on purpose:

| Here | Needs | Upstream |
| --- | --- | --- |
| [#218], the deterministic MAC | network drivers to receive the endpoint name at `CreateEndpoint`, as IPAM drivers already do | [moby/moby#52870] (issue), [moby/moby#52871] (PR, open) |

Both halves were filed in June 2026. The endpoint-name change
([moby/moby#52871]) is still awaiting review, and [#218] will not be
closed as "won't fix" while that is the only thing in the way. This
fork's own half is written and waiting.

The other one has moved. The `interface_name` pass-through
([moby/moby#52866]) was merged to moby master on 2026-08-26, milestoned
for engine **29.8.0**, and that engine was released on 2026-09-03;
[moby/moby#52865] closed with it and [#125] closed on this side. The
integration suite has not yet run against an engine carrying the change,
so the behaviour is unconfirmed and not measured. The tests probe for it
and turn themselves on, so no change here is waiting on it.

## What this project will deliberately not do

Turning these down is not a backlog. They are decided, and the reasoning
is recorded so a contributor can read it before writing the PR.

- **It will not become a DHCP server.** No lease serving, no built-in
  pool, no failover of its own. The point of the plugin is that your
  existing server is the authority; a second one would recreate the
  problem it solves. Interoperating with more than one server is a
  different question, and v1.8.0 answered it: `dhcp_servers` and
  `dhcp_deny_servers` decide which existing server a network leases from
  ([#111]). That stays on this side of the line: the plugin picks among
  authorities and never becomes one.
- **It will not change interfaces the host already has.** Bridge mode
  today needs a bridge you maintain, and the parent-attached modes will
  not bring a NIC up, add an address, or edit netplan/`systemd-networkd`.
  The plugin reads host configuration; it does not own it. A bridge the
  plugin creates for its own networks and owns for as long as they exist
  ([#903], v2.3.0) is inside this rule.
- **It will not gain a static-IP workflow.** If an address must be
  fixed, fix it where addresses are decided, in a reservation on the
  DHCP server. A per-container static-IP option would be a second,
  silently conflicting IPAM.
- **It will not ask for more privileges to buy a feature.** Adding a
  capability to
  [`config.json`](https://github.com/claymore666/docker-net-dhcp/blob/main/config.json)
  forces **every** operator to re-approve the plugin's privileges on
  upgrade. Refusing that trade is the standing answer, applied every
  time.

    2.0 is what this rule costs when it is met head-on. The 2.0 line
    **does** add `CAP_NET_RAW` to
    [`config.json`](https://github.com/claymore666/docker-net-dhcp/blob/main/config.json),
    and every operator upgrading onto it re-approves. It buys no
    feature: the DHCP exchange runs on an interface with no address,
    which requires an `AF_PACKET` socket on the ordinary path for every
    endpoint, with no configuration in which the plugin works without
    it. The *power* is unchanged, because the capability is in the OCI
    default set and the process always held it, so what the line bought
    is an honest manifest and what it cost is the prompt. Read the rule
    as written: not "never add a capability", but "never add one to buy
    a feature", and pay the re-approval out loud when the plugin
    genuinely needs the grant it is already exercising. See [#725],
    whose title says the capability is *already granted*, which is true
    of the effective set the process runs with, and the reason 2.0's
    addition buys no power, only the prompt.
- **It will not detect a conflicting container on the same host.** RFC
  5227 runs on the container's own link, and macvlan's parent/child
  isolation, the same property that keeps a sibling from disturbing us,
  also hides a sibling that has taken our address. That is excluded by
  construction and is not pending work ([#528]).
- **It will not support ipvlan L3 / L3S.** DHCP needs L2 broadcast.
- **It will not run its arm64 verification under qemu-user/binfmt.**
  This was measured. On the 1.x client, which opened a `NETLINK_GENERIC`
  socket qemu-user does not translate, the emulated plugin could not
  acquire a lease at all. The 2.0 client is a different program and has
  not been re-measured under emulation; the conclusion is unchanged
  either way, because arm64 verification runs on real hardware and there
  is nothing to be gained by finding out which syscall the emulator
  drops next.
- **It will not backport security fixes.** Only the latest release is
  supported; upgrading is one `docker plugin install`. See
  [SECURITY.md](https://github.com/claymore666/docker-net-dhcp/blob/main/SECURITY.md).
- **It will not carry AI-assistant attribution in its history.** Using
  an assistant is fine and needs no disclosure; commits and PRs are
  signed by a person who stands behind them, and a CI check enforces it.

## How this page is kept honest

The release runbook makes a top-to-bottom documentation review a release
step, and this page is part of it. If a theme above has gone a year
without motion, or a "will not do" has quietly become something the
project does, that review is where it gets corrected. It does not wait
for the next time someone asks.

[#110]: https://github.com/claymore666/docker-net-dhcp/issues/110
[#949]: https://github.com/claymore666/docker-net-dhcp/issues/949
[#960]: https://github.com/claymore666/docker-net-dhcp/issues/960
[#214]: https://github.com/claymore666/docker-net-dhcp/issues/214
[#672]: https://github.com/claymore666/docker-net-dhcp/issues/672
[#800]: https://github.com/claymore666/docker-net-dhcp/issues/800
[#802]: https://github.com/claymore666/docker-net-dhcp/issues/802
[#808]: https://github.com/claymore666/docker-net-dhcp/issues/808
[#814]: https://github.com/claymore666/docker-net-dhcp/issues/814
[#816]: https://github.com/claymore666/docker-net-dhcp/issues/816
[#817]: https://github.com/claymore666/docker-net-dhcp/issues/817
[#818]: https://github.com/claymore666/docker-net-dhcp/issues/818
[#819]: https://github.com/claymore666/docker-net-dhcp/issues/819
[#820]: https://github.com/claymore666/docker-net-dhcp/issues/820
[#821]: https://github.com/claymore666/docker-net-dhcp/issues/821
[#879]: https://github.com/claymore666/docker-net-dhcp/issues/879
[#881]: https://github.com/claymore666/docker-net-dhcp/issues/881
[#895]: https://github.com/claymore666/docker-net-dhcp/issues/895
[#902]: https://github.com/claymore666/docker-net-dhcp/issues/902
[#903]: https://github.com/claymore666/docker-net-dhcp/issues/903
[#904]: https://github.com/claymore666/docker-net-dhcp/issues/904
[#905]: https://github.com/claymore666/docker-net-dhcp/issues/905
[#925]: https://github.com/claymore666/docker-net-dhcp/issues/925
[#926]: https://github.com/claymore666/docker-net-dhcp/issues/926
[#927]: https://github.com/claymore666/docker-net-dhcp/issues/927
[#827]: https://github.com/claymore666/docker-net-dhcp/issues/827
[#839]: https://github.com/claymore666/docker-net-dhcp/issues/839
[#815]: https://github.com/claymore666/docker-net-dhcp/issues/815
[#868]: https://github.com/claymore666/docker-net-dhcp/issues/868
[#875]: https://github.com/claymore666/docker-net-dhcp/issues/875
[#911]: https://github.com/claymore666/docker-net-dhcp/issues/911
[#111]: https://github.com/claymore666/docker-net-dhcp/issues/111
[#125]: https://github.com/claymore666/docker-net-dhcp/issues/125
[#218]: https://github.com/claymore666/docker-net-dhcp/issues/218
[#219]: https://github.com/claymore666/docker-net-dhcp/issues/219
[#403]: https://github.com/claymore666/docker-net-dhcp/issues/403
[#452]: https://github.com/claymore666/docker-net-dhcp/issues/452
[#457]: https://github.com/claymore666/docker-net-dhcp/issues/457
[#507]: https://github.com/claymore666/docker-net-dhcp/issues/507
[#524]: https://github.com/claymore666/docker-net-dhcp/issues/524
[#528]: https://github.com/claymore666/docker-net-dhcp/issues/528
[#531]: https://github.com/claymore666/docker-net-dhcp/issues/531
[#651]: https://github.com/claymore666/docker-net-dhcp/issues/651
[#669]: https://github.com/claymore666/docker-net-dhcp/issues/669
[#682]: https://github.com/claymore666/docker-net-dhcp/issues/682
[#699]: https://github.com/claymore666/docker-net-dhcp/issues/699
[#725]: https://github.com/claymore666/docker-net-dhcp/issues/725
[moby/moby#52865]: https://github.com/moby/moby/issues/52865
[moby/moby#52866]: https://github.com/moby/moby/pull/52866
[moby/moby#52870]: https://github.com/moby/moby/issues/52870
[moby/moby#52871]: https://github.com/moby/moby/pull/52871
