# Roadmap

Where `docker-net-dhcp` is going over roughly the next year, and — the
more useful half — where it deliberately is not going.

This is **direction, not a delivery schedule.** The project is
solo-maintained, so nothing here carries a date. Milestones remain the
per-release truth: a milestone says what is going into the next tag, this
page says what the tags are working towards and which shapes of change
will be turned down however well they are implemented.

## The bar every feature is measured against

One `docker network create` line, then plain `networks: [lan-dhcp]` in
any Compose file. No static IPs, no sidecars, no per-container plumbing,
no entrypoint script that has to know it is running on this network.

That bar decides most design arguments before they start. A workaround a
user has to script around the plugin is read here as a **bug report
against this principle**, not as a solution — see [#125], which is open
for exactly that reason.

## Where the project is today

**This branch is the 2.0 beta.** The plugin leases through the
project's own in-tree DHCP client library instead of an external client
process, and the beta implements **DHCPv4 only**: `ipv6=true` is
refused at `docker network create` and DHCPv6 lands in a later beta
milestone. Everything else in this section describes the 1.x line the
beta is replacing, feature for feature, and stays true of it.

Bridge, macvlan and ipvlan attachment; DHCPv4 and DHCPv6; addresses that
survive `docker restart`, plugin restart and daemon restart; a
`/Plugin.Health` counter surface and the same counters in Prometheus
form on `/metrics` ([#651]); per-network choice of which DHCP server to
lease from ([#111], [#669]); signed, attested, reproducible releases.

v1.9.0 is the release that makes IPv6 usable rather than merely present.
Before it, `ipv6=true` was accepted and did not work on most segments:
no container could start at all where the router advertises stateless or
SLAAC-only configuration ([#868]), a stateless server's configuration was
received and discarded ([#815]), and on a managed segment the leased
address stopped being refreshed and its default route disappeared
([#875]). The same release stops the plugin sending DHCPRELEASE on any
path — an address is held until the lease expires, like any other host's
([#800]).

v1.8.0 carried the first human review of the design and its trust
boundaries ([#457], [#699]) — pulled into that release because it is the
one that opens a TCP port in a process holding `CAP_NET_ADMIN`. Every
finding was fixed inside the same cycle rather than carried forward, and
each one ends in a test or a counter rather than a paragraph. The
[driver reference](reference.md) is the authority on what exists right
now — if this page and that one disagree, that one is right.

## Direction

Five themes, in rough order of how much they change for a user. Issue
numbers are anchors, not a queue.

### 1. Knowing when addressing is wrong, not just when it fails

The hard failures were solved first; the quiet ones are the current
work. v1.6.0 added conflict detection ([#524]) because the plugin
accepted an address a statically-configured host already held and every
counter stayed at zero — the container came up, Docker reported an
address, and nothing anywhere said otherwise.

The direction that follows from it: **every new failure mode gets a
counter, not just a log line**, and a counter that can read clean while
the feature is broken is treated as an unfinished instrument. Expect
more of the health surface, and more assertions made against what the
DHCP server saw rather than against what the plugin believes.

### 2. Lease identity across recreates

A DHCP server keys on identity, so address stability is an identity
problem. Two pieces are designed and blocked rather than unplanned —
[#218] (deterministic MAC) and [#219] (client-id for ipvlan, where every
child shares the parent's MAC). See *Blocked upstream* below.

### 3. arm64

Users have asked for it and it works — proven on real hardware: a
native arm64 CI runner executes the full integration suite, timing
behaviour included, as a release-candidate gate ([#531]). The shipping
shape is per-architecture tags (`vX.Y.Z-arm64`, `latest-arm64`), first
published with v1.7.0 ([#507]) — a Docker *plugin* cannot be installed
from a multi-architecture manifest list at all, so the architecture
lives in the tag rather than in a manifest.

### 4. A test substrate that cannot lie

This is infrastructure work with a user-visible reason: on this project,
every timing crutch removed from CI turned out to be hiding a real
defect. An opt-out helper added to make a restart test pass hid a
user-facing `docker restart` failure for months. So the rule — **a test
that only passes once you weaken something is a bug report** — is
enforced by a gate rather than by a paragraph, and the remaining
soft spots ([#682], [#403]) are on the list for the same reason
features are.

### 5. Supply chain and documentation

Releases are already cosign-signed, provenanced, SBOM'd and
reproducible. The remaining pull is the OpenSSF Best Practices silver
and gold criteria, which are tracked as ordinary issues and generally
translate into something concrete — this page exists because
`documentation_roadmap` is one of them ([#452]).

## Blocked upstream, not unplanned

Two features are implemented or designed here and cannot ship until
Docker's own engine carries a change. They stay open on purpose:

| Here | Needs | Upstream |
| --- | --- | --- |
| [#125] — Compose `interface_name` | the remote driver to honour a plugin-returned `DstName` | [moby/moby#52865] (issue, closed), [moby/moby#52866] (PR, **merged** — ships in engine 29.8.0) |
| [#218] — deterministic MAC | network drivers to receive the endpoint name at `CreateEndpoint`, as IPAM drivers already do | [moby/moby#52870] (issue), [moby/moby#52871] (PR) |

Both were filed in June 2026. The `interface_name` pass-through
([moby/moby#52866]) was **merged** to moby master on 2026-08-26 and is
milestoned for engine **29.8.0**; [moby/moby#52865] closed with it. It
unblocks [#125] on the first engine *release* carrying it — 29.8.0 is
not out yet (measured 2026-08-28: latest release 29.7.2, with
29.8.0-rc.1 published as a pre-release that day), so the blocker moved from
review to a release, not away. The endpoint-name change
([moby/moby#52871]) is still awaiting review. Neither issue here will be closed as "won't fix" while
that is the only thing in the way; the fork's own half of each is
written and waiting.

## What this project will deliberately not do

Turning these down is not a backlog. They are decided, and the reasoning
is recorded so a contributor can read it before writing the PR.

- **It will not become a DHCP server.** No lease serving, no built-in
  pool, no failover of its own. The point of the plugin is that your
  existing server is the authority; a second one would recreate the
  problem it solves. Interoperating with more than one server is a
  different question, and v1.8.0 answered it: `dhcp_servers` and
  `dhcp_deny_servers` decide which existing server a network leases
  from ([#111]). That stays on this side of the line — the plugin picks
  among authorities, it never becomes one.
- **It will not reconfigure the host's networking.** Bridge mode needs a
  bridge you maintain, and the parent-attached modes will not bring a NIC
  up, add an address, or edit netplan/`systemd-networkd`. The plugin
  reads host configuration; it does not own it.
- **It will not gain a static-IP workflow.** If an address must be
  fixed, fix it where addresses are decided — a reservation on the DHCP
  server. A per-container static-IP option would be a second, silently
  conflicting IPAM.
- **It will not ask for more privileges to buy a feature.** Adding a
  capability to `config.json` forces **every** operator to re-approve
  the plugin's privileges on upgrade, and that trade is the default
  answer, not a one-off.

    The 2.0 beta is what this rule costs when it is met head-on. The
    beta **does** add `CAP_NET_RAW` to `config.json`, and every
    operator upgrading onto it re-approves. It buys no feature: the
    DHCP exchange runs on an interface with no address, which requires
    an `AF_PACKET` socket on the ordinary path for every endpoint,
    with no configuration in which the plugin works without it. The
    *power* is unchanged — the capability is in the OCI default set and
    the process always held it — so what the line bought is an honest
    manifest and what it cost is the prompt. Read the rule as written:
    not "never add a capability", but "never add one to buy a feature",
    and pay the re-approval out loud when the plugin genuinely needs
    the grant it is already exercising. See [#725], whose title says
    the capability is *already granted* — true of the effective set the
    process runs with, and the reason the beta's addition buys no power,
    only the prompt.
- **It will not detect a conflicting container on the same host.** The
  conflict probe's vantage point is the parent link, and the isolation
  that keeps our own endpoint from answering also hides a sibling. That
  is excluded by construction rather than pending ([#528]); a vantage
  point that could see the sibling would hear our own container and no
  reply would be informative.
- **It will not support ipvlan L3 / L3S.** DHCP needs L2 broadcast.
- **It will not run its arm64 verification under qemu-user/binfmt.**
  Measured, not assumed — on the 1.x client, which opened a
  `NETLINK_GENERIC` socket qemu-user does not translate, so the
  emulated plugin could not acquire a lease at all. The 2.0 client is a
  different program and has not been re-measured under emulation; the
  conclusion is unchanged either way, because arm64 verification runs
  on real hardware and there is nothing to be gained by finding out
  which syscall the emulator drops next.
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
project does, that review is where it gets corrected — not the next time
someone asks.

[#800]: https://github.com/claymore666/docker-net-dhcp/issues/800
[#815]: https://github.com/claymore666/docker-net-dhcp/issues/815
[#868]: https://github.com/claymore666/docker-net-dhcp/issues/868
[#875]: https://github.com/claymore666/docker-net-dhcp/issues/875
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
[moby/moby#52865]: https://github.com/moby/moby/issues/52865
[moby/moby#52866]: https://github.com/moby/moby/pull/52866
[moby/moby#52870]: https://github.com/moby/moby/issues/52870
[moby/moby#52871]: https://github.com/moby/moby/pull/52871
