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

## v1.8.0

The release that got a human security review. Seventeen findings came
out of it and all seventeen are fixed here — #699 indexes them, and two
are recorded against the pull requests that fixed them rather than
against an issue of their own. Alongside that: a network can
now say which DHCP server it will lease from, the health counters are
scrapeable by Prometheus, and two lifecycle faults are gone.

If you are running v1.7.1, the reason to upgrade is the review. v1.7.1
writes a container's `--hostname` into the generated `dhcpcd.conf`
without validating it, and Docker validates it no further — which is
enough for one container to present another endpoint's DHCP identity and
be handed its reservation. The rest of the release is the
server-selection options, the new `/metrics` endpoint and the
health-counter fixes below.

**This release changes plugin behaviour**, not only its packaging.
Reading no further than this paragraph, these are the things that change
under an operator who upgrades:

- A container hostname carrying a control character is now **dropped**
  rather than written into the DHCP client's config. The lease proceeds
  without it, and the endpoint attaches with a fresh identity.
- DHCP option 15 is filtered before it reaches a container's
  `resolv.conf`, and truncated at its first space. Four more
  server-chosen string options are filtered the same way.
- The plugin socket is `chmod`ed to `0600`, and the plugin **refuses to
  start** if that fails.
- An attach whose container PID no longer belongs to that container now
  **fails** rather than proceeding; the same check refuses a DNS
  propagation silently.
- `propagate_mtu` **refuses** an option-26 MTU outside `[576, 65535]`,
  leaving the container link at the MTU it had.
- An option-51 lease lifetime longer than 24h is **clamped for the
  outage watchdog only**. The lifetime reported in logs and the ledger
  is untouched.
- State files and the lease ledger under `/var/lib/net-dhcp` are no
  longer world-readable, so reading `leases.jsonl` as a non-root user
  now needs `sudo`.

On top of those: two new network options, a Prometheus `/metrics`
endpoint, twelve new health counters and two lifecycle fixes, alongside
two refactors under `pkg/plugin`. The reference digests differ from
v1.7.1 accordingly. The refactors are intended to be
behaviour-identical — both are covered by the existing suite plus new
unit tests — but "intended" is the honest word there, not "proven".

### The v1.5.0 precondition still applies

```bash
sudo mkdir -p /var/lib/net-dhcp
```

Unchanged since v1.5.0, and still required on every host before
`docker plugin install`. Docker does not create a missing bind source,
and skipping it leaves the plugin installed but disabled with an error
that does not say why. See the v1.5.0 notes below for recovery.

### The security review

The project had substantial automated security coverage — CodeQL,
staticcheck, govulncheck, image scanning, the race detector, fuzz tests
over the DHCP parsing path — and no human review of the design or its
trust boundaries. v1.8.0 opens the first TCP listener in a process
holding `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE` on the
host network, so the release was held while that review was done.

Five surfaces were audited independently: the Docker-facing driver RPCs,
the `/metrics` endpoint, the DHCP-server input path, the
filesystem/argv/env sinks, and the CI gate scripts. Everything found is
fixed in this release; nothing was deferred. #699 is the public index
of the whole exercise — what was found, what shipped, and the three
results that belong to no single finding.

Two things are worth stating plainly:

- **No advisory was issued, and that was a decision rather than an
  omission.** A draft advisory was opened for the hostname-injection
  finding and closed unpublished, for the reason that follows.
  Setting `--hostname` requires container-creation rights, which in
  almost every deployment means Docker API access — already
  root-equivalent on this host by the plugin's documented design. No
  code execution is available on any of these paths.
- **Two of the findings came out of re-auditing the tree *after* a fix
  merged rather than before**, and they are not the same kind of thing.
  #694 was a true regression: the fix for #692 created it, it lived only
  in `dev`, and no released version carries it. #695 was the opposite —
  the fix for #688 hardened one call site and left its neighbour three
  files away doing what its own comment forbade, so what #695 describes
  is **present in v1.7.1 and every release before it**, not something
  this cycle introduced. Re-auditing after the fix is what found both;
  only one of them was ours to have caused.

(#457, #699)

### A hostname could become another endpoint's identity

The container's hostname reached the generated `dhcpcd.conf`
unvalidated, and Docker performs no validation on it — measured against
the engine: `docker create --hostname $'a\nfoo'` succeeds and reads back
with the newline intact. A newline there does not produce a malformed
directive; it produces an additional, caller-chosen one, and dhcpcd
applies it. Measured against dhcpcd 10.3.2 from our own `:rootfs` image:
an unknown directive prints `unknown option:` and dhcpcd continues, and
a repeated directive resolves **last-wins**.

The plugin writes `duid` near the top of the generated file and the
hostname near the bottom, so an injected `duid` always won. The pinned
DUID derives solely from the endpoint MAC, and on a bridge or macvlan
network every endpoint's MAC is observable on the same segment — so a
container could present another endpoint's DHCP identity and be handed
its binding or reservation. It reaches DHCPv4 too on networks without an
explicit `client_id`, since dhcpcd derives the v4 client identifier from
the DUID by default. Not code execution: `script` is a valid directive
and an injected one parses, but the plugin always passes `-c <handler>`
and the command line wins. The `-c` flag is load-bearing security
machinery, which nothing previously said.

**This one is present in v1.7.1.** The `dhcp_deny_servers` bypass it also
enabled is not — `whitelist`/`blacklist` lines arrive with #111/#669 in
this release.

A control character in the hostname now drops it, counted as
`unsafe_hostnames_rejected`. Underscores and other
technically-illegal-but-common hostnames are **not** affected: the rule
is about the config file's structure, not about RFC 1123. (#692)

The fix for that then produced the sharpest finding of the cycle, in
`dev` only — no released version carries it. The sanitiser signalled a
refusal by returning an empty string, and `tombstoneStore.consume` reads
an empty hostname as **"match any tombstone on this network"**. That
carve-out is deliberate and honest — v0.5.0 tombstones carry no
hostname, and `CreateEndpoint` can run before the container is
registered with the daemon — and it was safe precisely because nothing
attacker-controlled could produce an empty string. After the fix,
something could: a container started with one control character in
`--hostname` matched the first live tombstone on the network, inherited
another endpoint's MAC, and asked the DHCP server for its address.
Tombstones live 60s, so the window was any container stopping on the
same network.

A refusal is now distinguishable from an absence: the sanitiser reports
whether it refused, the guard sits inside `consumeTombstone` so a third
caller cannot forget to pass it, and a refused hostname consumes no
tombstone — the endpoint attaches with a fresh identity, which is the
right answer for a value nobody should have sent. (#694)

### Values from the DHCP server are bounded, not just typed

Every finding in the second round of the review was one shape: a value
validated for its *type* and then applied with no *range* and no
*counter*. The plugin's own health surface read clean through all of
them.

- **Option 15 reached `resolv.conf` whole.** The address is `ParseCIDR`'d,
  the MTU range-checked, routes validated individually, and the DNS
  server and search lists both go through `strings.Fields` — `Domain`
  was the one field taken as-is, so a newline in it injected arbitrary
  `resolv.conf` lines, most obviously a `nameserver` redirecting the
  container's name resolution. It is now filtered (#689) — and then
  filtered properly: the first fix rejected bytes below `0x20`, and
  `0x20` itself is the *sink's own separator*, so a space still turned
  one search domain into several with the server's first in the order
  (#704). Option 15 is now truncated at its first space.
- **Four more string options carried raw newlines** into `dhcp.Info`
  with no filter at any layer — `bootfile_name`, `wpad`,
  `posix_timezone`, `tzdb_timezone`. `dhcpcd` validates only its
  `dname`-typed options; the `string`-typed ones pass `\n` and `\r`
  through verbatim. Not exploitable today only because logrus's default
  formatter quotes them, and no test pinned that. All five values are
  now filtered at the boundary, in the hook process, and refusals are
  carried across the FIFO as `unsafe_option_values_dropped`. (#703)
- **An option-121 `/1` pair superseded the default route silently.**
  `0.0.0.0/1 gw` plus `128.0.0.0/1 gw`: neither half is a default route,
  both install as ordinary static routes, and between them they take
  every destination — while the gateway reported to Docker and shown by
  `docker inspect` still names the legitimate router. Applying them is
  correct client behaviour and legitimate split-tunnel setups rely on
  it; the defect was that nothing distinguished the two cases. The log
  line now names each destination and next hop, and
  `dhcp_default_route_superseded` counts it against `dhcp_routes_applied`.
  Measured end to end. (#700)
- **An infinite lease disabled the outage watchdog permanently.**
  Option 51 = `0xFFFFFFFF` is exported verbatim and sets a 136-year
  deadline, and `leaseDeadline` is the only trigger that can detect a
  silently lapsed lease under `--noconfigure` — so one such ACK followed
  by silence left `dhcp_timeouts` at zero through a total outage for
  that endpoint. That is the exact failure #353 exists to catch,
  re-opened by a number the server chooses. The deadline is now clamped
  to 24h and `lease_time_clamped` says so; the lease lifetime the plugin
  reports is unchanged. (#701)
- **`propagate_mtu` had no lower bound.** The kernel accepts 68, dhcpcd
  exports it unchanged, and the result — destroyed throughput plus
  black-holed path MTU discovery, re-applied on every renewal — looks
  like a slow network rather than a misconfiguration. The accepted range
  is now `[576, 65535]`, counted as `mtu_refused`, with the container
  link left at the MTU it had. The ceiling is deliberately *not* the
  parent's MTU: raising a container link above its parent is the
  documented jumbo-frame use case, and the kernel enforces the real
  device maximum itself. (#702)

### A privileged handle is now pinned to the task it was checked against

The plugin runs with `pidhost: true`, so a container PID resolved
through Docker names a task in the **host's** PID namespace by the time
anything is done with it.

`propagateDNS` resolved a PID through `ContainerInspect` and handed it
straight to a function that opened `/proc/<pid>/ns/mnt`, `setns`ed into
it and wrote `/etc/resolv.conf` — with no re-check that the PID still
belonged to that container. If the container exited in that window and
the kernel recycled the PID, DHCP-server-supplied content was written
into whatever host process now held it, possibly one in the host's root
mount namespace. Reachable only with `propagate_dns` opted in, and the
race is not attacker-triggerable, which is why it is robustness rather
than an advisory — but the consequence should not rest on the window
being short. The PID's cgroup is now confirmed to name the expected
container, and the check returns a **directory fd**: procfs invalidates
a `/proc/<pid>` dentry when the task exits, so every `openat` below that
fd either reaches the same task or fails `ESRCH`. Refusals count
`dns_propagation_pid_mismatches`. (#688)

That fix reached one call site and left its neighbour three files away,
in the same release, doing exactly what its own comment forbade:
`dhcp_manager.go` built `/proc/%v/ns/net` from the same PID as a string
and resolved it **twice, independently** — once for the manager's
netlink handle, once inside `DHCPClient.Start`. Two resolutions of one
string can disagree. The sink there is not one file: the netlink handle
carries every `AddrReplace`, `AddrDel`, `LinkSetMTU` and `RouteAdd` the
manager makes, with `CAP_NET_ADMIN`, and `dhcpcd` is spawned into that
namespace as root. The window is 70s; PID wrap against
`pid_max = 4194304` was measured at roughly nine minutes of unprivileged
forking, and PIDs are host-global here.

The netns is now opened relative to the fd the identity check returned,
the wait polls *that* rather than a path — so a wait spanning a
container exit re-runs the check every attempt instead of waiting for
anything at all to turn up at that PID — and the client takes a borrowed
handle rather than a path string, which deletes the second resolution
rather than hardening it. Refusals count `netns_pid_mismatches`, and
unlike the DNS case the refusal fails the attach, so it is not silent.
The live race was not demonstrated end to end: proving it needs root and
a real container mid-attach. The primitive is confirmed; the exploit is
reasoned. **This one is present in v1.7.1** — `dhcp_manager.go` there
builds `/proc/<pid>/ns/net` as a string and resolves it twice, with no
identity check, and so does every release before it. (#695)

### The plugin's own surfaces

- **The socket's root-only property was inherited, not enforced.** A
  UNIX socket is created `0777 &^ umask`, so the access control on the
  entire RPC surface was whatever umask the plugin runtime handed us —
  `0755` today, `0775` under a umask of `0002`, `0777` under `0`.
  SECURITY.md argues that serving `/metrics` on that socket is unchanged
  ground *because* the socket is root-only, a contract stated in prose
  and established nowhere in the code. It is now `chmod`ed to `0600`,
  and a failure to do so **refuses plugin startup** rather than serving
  on a socket whose mode is unknown. (#687)
- **A NUL byte defeated the bridge-reuse guard.** The kernel truncates
  an interface name at NUL and a Go string comparison does not, so two
  networks could be brought to share one bridge. This was latent only in
  the write-up: a `POST /networks/create` carrying
  `com.docker.network.bridge.name` with an embedded NUL transports
  through the whole daemon untouched, and
  `netlink.LinkByName("docker0\x00evil")` resolves `docker0` while
  `"docker0evil"` is not found. `ValidIfaceName` already existed in
  `pkg/dhcp` and was never applied to the network options; it now runs
  in the pure-validation phase against `bridge` and `parent` alike.
  (#705)
- **`parseIfnameOption` accepted flag-shaped names**, blocked in
  practice by a leading-alphanumeric rule whose comment gave the wrong
  reason for itself — the real threat is getopt permutation, not
  re-splitting. The rule stays and now says why, and the option is now
  validated by `ValidIfaceName` as well, so a flag-shaped name fails at
  `CreateEndpoint` rather than surviving as far as the argv. (#706)
- **`unshare` was the one binary resolved through `$PATH`** rather than
  absolutely. It is pinned to `/usr/bin/unshare`. (#707)
- **Neither HTTP server had timeouts or a body cap.** Both now carry
  `ReadHeaderTimeout`, `ReadTimeout` and `IdleTimeout` plus a request
  body limit. The socket server's `WriteTimeout` is deliberately left at
  zero and a test permits a future non-zero value only if it clears the
  worst-case budget the handler constants add up to — which is not an
  upper bound on reality, since `lease_timeout` is operator-settable,
  and that is exactly why the timeout is zero: it is a deadline on the whole exchange and does not
  know a handler is still working, while `CreateEndpoint` legitimately
  holds a link wait, a DHCP acquisition and two probe budgets against an
  operator-settable `lease_timeout` with no cap. Firing mid-handler
  would hand libnetwork a truncated response for an endpoint that was
  already created. (#709)
- **A CI gate self-waived on any bind path containing a dot**, because
  it interpolated its needle into a regex. `grep -F` closes it. (#710)

### State files under `/var/lib/net-dhcp` are no longer world-readable

`tombstones.json`, the per-network options files and the `leases.jsonl`
lease ledger were created `0644`. That directory is a read-write bind
mount from the **host**, so container MAC addresses, leased IPs,
hostnames and the full lease audit trail were readable by any user on
the host. Nothing stored there is a credential and the plugin writes as
root either way, so this was never a privilege boundary — but there is
no reason for it to be open, and it costs nothing to close.

**If you read `leases.jsonl` as a non-root user, you will now need
`sudo`.** Files that already exist are tightened on the next write, so
an upgrade fixes hosts that have been running for a while rather than
only new installs. The plugin's `-logfile` stays world-readable at
`0644` — operators do read it — but drops from `0666` and is now opened
`O_NOFOLLOW`, so a symlink swapped in before a `SIGHUP` re-open cannot
decide where root appends. (#708)

### Choosing which DHCP server a network leases from

On a segment with more than one DHCP server the plugin took whichever
answered first, and there was no way to say otherwise. Two new network
options decide it instead. They answer deliberately different questions
and are not interchangeable:

- **`dhcp_servers`** is an *ordering* — "use 192.168.1.1 over
  192.168.1.2". Acquisition tries each server in turn, restricted to
  that one, and takes the first lease offered.
- **`dhcp_deny_servers`** is a *permission* — "never lease from
  192.168.1.3". Everything else may answer.

Set both and the denial wins, including for a server that also appears
in `dhcp_servers`.

Two properties are worth knowing before you enable this:

- **The list is exhaustive.** If none of the servers you named answers,
  the endpoint fails rather than accepting whichever server happened to
  reply. Naming your servers is what makes the list complete — a policy
  that widened under pressure would hand you the one outcome you
  configured it to prevent. `dhcp_server_policy_exhausted` counts this,
  so it is distinguishable from an ordinary DHCP timeout;
  `dhcp_server_tier_fallbacks` counts a preferred server going quiet and
  the next one answering, which is the only signal that a ranked server
  has gone away.
- **It never makes `docker run` slower.** The preference ladder divides
  the existing `lease_timeout` budget rather than extending it. Raise
  `lease_timeout` if you want each server given more time.

Two limits are structural, not oversights. Both options are **DHCPv4
only** — the underlying client has no v6 equivalent, so a v6 entry is
rejected at `docker network create` rather than silently ignored. And
they are **not supported behind a DHCP relay**: the filter matches the
packet's source address, not the Server Identifier, and through a relay
every offer looks identical. (#111, #669)

### A scrape target, so alerting is not a per-operator project

`/Plugin.Health` answers "is it healthy right now" for a human with
`curl`. It does not answer "has the NAK rate risen since the DHCP server
was reconfigured", which only a time series can answer — and getting the
counters into one meant writing an exporter, teaching it the
`HealthResponse` schema, reaching a UNIX socket Prometheus cannot
scrape, and handling counter resets by hand.

The plugin holds the counters, so the plugin now speaks the scrape
format. `/metrics` is served on the plugin socket unconditionally, in
Prometheus text exposition format, rendered from the same snapshot that
backs `/Plugin.Health` — the two views cannot disagree about a counter,
and a test asserts by reflection that every health field reaches the
exposition, so one added later cannot go quietly missing from your
dashboards.

Prometheus cannot scrape a UNIX socket, so there is also an optional TCP
listener:

```
docker plugin set <plugin> METRICS_ADDR=127.0.0.1:9099
```

**It is off by default, and should stay off unless you scrape it.** The
plugin runs with `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE`
with host networking, so a port it opens is on the host itself. Bind
loopback or a management interface, never `0.0.0.0`. The listener serves
`/metrics` and nothing else — the libnetwork RPCs are not routed on it,
and a test drives every registered route over TCP to prove it — and a
malformed address fails plugin startup rather than leaving you without
the endpoint you asked for.

Two things to know before building dashboards:

- **`family="ipv4"` is derived.** Six counters carry a `family` label,
  and in the JSON the unsuffixed counter is the v4+v6 *total* while the
  `_v6` one is a subset of it (#212). The metrics view computes the v4
  share as `total - v6`, which is what a `family` label ought to mean —
  so the JSON `leases_obtained` will not equal the `family="ipv4"`
  series, and that is correct rather than a bug.
- **Counter resets are visible.** `net_dhcp_build_info` carries the
  plugin's `instance_id` as a label, so a plugin restart appears as a
  new series rather than as a counter that silently rewound.

Per-endpoint and per-network labels are deliberately absent: endpoint
IDs are unbounded and turn over with container lifecycle, so labelling
by them would be a cardinality problem in exactly the deployments where
these metrics matter most. (#651)

### Twelve new health counters

None of the twelve affects `healthy`; the four counters that flip it are
unchanged. `docs/reference.md` carries the full description of each,
including what a rise means and what to do about it.

| counter | what it says |
| ------- | ------------ |
| `unsafe_hostnames_rejected` | Container hostnames dropped for carrying a control character (#692). |
| `unsafe_option_values_dropped` | Server-chosen string options refused for the same reason, plus option-15 domains truncated at their first space (#703, #704). |
| `dns_propagation_pid_mismatches` | DNS propagations refused because the container PID no longer belonged to that container (#688). |
| `netns_pid_mismatches` | Sandbox netns opens refused for the same reason — the attach fails (#695). |
| `dhcp_routes_applied` | Option-121 classless static routes handed to Docker at Join. The denominator for the row below (#700). |
| `dhcp_default_route_superseded` | Joins whose option-121 routes cover `0.0.0.0/0` by union rather than by a literal default entry (#700). |
| `lease_time_clamped` | Option-51 lifetimes cut to 24h for the outage watchdog only (#701). |
| `mtu_refused` | Option-26 MTUs outside `[576, 65535]`, with the link left as it was (#702). |
| `dhcp_server_tier_fallbacks` | Acquisitions that fell through to a lower-priority `dhcp_servers` entry — the only outside signal that a preferred server is silently dead (#111). |
| `dhcp_server_policy_exhausted` | Acquisitions abandoned because no listed server answered, as distinct from DHCP being broken (#111). |
| `recovery_network_gone` | Networks removed out from under the post-restart recovery walk (#648). |
| `recovery_already_managed` | Endpoints a recovery walk found already registered to another manager (#480). |

### An ordinary `docker network rm` could report the plugin's worst fault

`recovery_failed` means something specific and serious: after a daemon
restart, a container that is *still running* failed to get its lease
renewal client back, so it will lose its IP when the lease expires. It
is one of the four counters that flip `healthy` to `false`.

A network removed between the listing that found it and the read of its
detail was landing in that counter. Nothing was wrong — a network that
is gone leaves no running container without a renewal client — but an
operator who removed a network while the daemon happened to be
restarting saw the plugin report its most serious failure, and an alert
on `healthy` would have fired for it.

Those are now counted as `recovery_network_gone`, which never affects
`healthy`. It is counted rather than passed over in silence: a host
where it climbs steadily is churning networks under a restarting daemon,
which is worth knowing even though no single occurrence is a problem.
(#648)

### Two DHCP clients could end up renewing one lease

When the plugin starts it walks the endpoints Docker still knows about
and builds a lease-renewal client for each one it is not already
managing. If a container's `Join` arrives for an endpoint that walk has
already claimed, the `Join` wins — it is the newer truth — and the
manager it displaces is stopped, so its DHCP client does not go on
renewing the same lease on the same interface.

That held in one order and not the other. The recovery walk read the
registry, released the lock, built its manager and then registered it;
a `Join` landing inside that window had its live manager evicted and
dropped, with its DHCP client still running and now untracked — the
collision the displacement code exists to prevent, arrived at from the
other side. Recovery now registers through a compare-and-set and yields
to whoever already holds the endpoint, so older truth can no longer
overwrite newer.

Narrow, but not hypothetical: the test written to explore an unrelated
question hit this window on its first run, and hit it again in CI.
`recovery_already_managed` counts it. That counter does not affect
`healthy` — an endpoint someone else is already managing *has* a renewal
client, it simply is not the one this walk would have built — and until
now the event was invisible except as an inflated "recovered" in a
single log line. (#679)

### The tests now replay what the daemon actually sends

The unit tests used to assert against request structs we wrote
ourselves, which means they confirmed our model of libnetwork rather
than libnetwork. That is not a hypothetical failure mode here — it is
how `stable_lease` was designed against an assumed `CreateEndpoint`
payload and had to be reverted from v1.3.0 once the real one turned out
not to carry what it needed.

Three real flows are now captured from a live daemon and replayed, with
provenance recorded, and a gate that fails when the recording stops
describing the engine the suite runs against. That gate earned itself
immediately: it found that the captures had been recorded against a
different Docker engine than the integration suite actually exercises,
and the re-record showed the daemon had quietly started sending an
option the older one never did. None of that was visible before.
(#644, #646)

### Internal

- The tombstone store and the six phases of lease renewal are now
  separable units with their own tests, rather than two long functions
  reachable only through the privileged suite. (#643)
- The arm64 lane runs a standing self-hosted runner that registers once
  and reconnects on every boot, so a release candidate no longer needs
  anyone to start it by hand. Its watchdog now scales its timings to the
  hardware it finds instead of refusing to run on a device whose
  watchdog is shorter than the defaults assume — refusing to run left
  the board unprotected, which is the opposite of the intent. (#632)
- That watchdog then turned out to cover only half of what it exists
  for. Its unit declared `DefaultDependencies=no` and wrote
  `Before=shutdown.target` and `Conflicts=shutdown.target` back out
  explicitly — the documented idiom for a unit that should be *stopped*
  before shutdown proceeds — so systemd stopped it at the first instant
  of every reboot, the daemon disarmed unconditionally on `SIGTERM`, and
  the board entered the rest of the shutdown with nothing armed. A
  shutdown blocking on a dead NFS share is one of the two wedges the
  watchdog was written for, and the unit file asserted the opposite
  three lines above the code that broke it. Measured on the host as a
  14-minute hang. Both halves are fixed, because either alone leaves it
  broken: the unit adds nothing back to the shutdown ordering, and the
  daemon disarms only while `statfs` still answers — so an operator's
  `systemctl stop` on a healthy board still stops cleanly, while a stop
  taken once the share has gone silent closes the device without the
  magic byte and lets the timer end the hang. Both are gated, since the
  whole failure was a comment asserting what nothing checked. (#632,
  #684)
- A CI runner whose plugin state directory went missing did not degrade;
  it took the nested Docker daemon down with it, permanently, while
  continuing to report itself online to GitHub. The directory is now
  created before the daemon starts, and the ordering is asserted. (#660)
- That fix reached one caller and not its copies: every workflow that
  installs the plugin names the directories itself, so each copy rots
  independently the moment a manifest gains a bind source. A gate now
  holds every such step to the manifest it actually installs. The one
  that had already drifted — the coverage lane, whose manifest carries
  two sources the shipped one does not — derives them from it instead of
  naming them. (#666)
- Several gates were reporting success over work they had never
  inspected, and one fixture pinned no negative cases at all, so it
  could not have caught a rule that matched too much. (#569, #535,
  #536, #636)
- The integration harness's environment knobs are documented, rather
  than discoverable only by reading the harness. (#534)
- The arm64 lane's four boot states are named, and its docs no longer
  claim the board always recovers on its own. A bootloader whose
  download was cut mid-flight does not restart the sequence: it answers
  ARP, issues no TFTP requests, and stays there until power is removed —
  measured at 10.5 minutes of silence with the boot server fully back
  up, where a looping board would have asked about eight times. The
  server's TFTP log is the discriminator; "no ping" never was. Only one
  of the four states needs hands, and the coincidence that produces it
  is recorded as accepted rather than left implied. (#654)
- `docs/internals.md` presented the `interface_name` tests as gated on
  an engine version. They are not: they probe whether the engine applies
  a remote driver's `DstName` and skip when it does not — which today is
  every engine, pending moby/moby#52866. Describing a capability probe
  as a version check taught the weaker pattern as house style, on a page
  that exists to teach the stronger one. (#673)
- Killing the Docker daemon outright is now covered by the integration
  suite, and what it does turned out not to be what the issue asking for
  the test assumed. The plugin never dies abruptly: about a second after
  the daemon is killed the replacement daemon sends it a clean shutdown,
  and it releases every lease on the way out. Nor does the container
  survive — the daemon discards it during its own restore and a restart
  policy builds a new one — so no live endpoint is left to re-adopt. The
  test asserts what is true of that path instead, and asserts it against
  the DHCP server's log rather than the plugin's own counters: the lease
  held before the kill is released rather than left to expire, and the
  container that comes back holds a lease the server actually granted.
  (#480)
- The arm64 lane checked that the tree *would* install a working NFS
  watchdog, and never that the host had *booted* one. That host's root
  is a netbooted image, so the two drift apart without a single file in
  the tree being wrong: a root predating the timeout-scaling fix
  installs a daemon that exits at boot, and the board runs unwatched
  while every source-side check still passes. The lane now asks the
  kernel whether the watchdog is armed and which process holds it, and
  reports "cannot check" rather than a pass when that evidence has aged
  out of the ring buffer. (#677)
- `docker build` uploads the whole context to the daemon before any
  `COPY` is considered, and `.dockerignore` carried no counterpart to
  the credential block `.gitignore` has had since it was written — so a
  maintainer's local `secrets/` went with every build from the repo
  root. Nothing reached an image; the transfer happens regardless. The
  gate that exists to stop that drift could not see it, because it
  matched root-anchored directories only and that block is written
  unanchored — a blind spot its own comment named while it went green.
  It now judges every ignored directory and every credential-shaped
  path, and goes red rather than quiet if either class turns up empty.
- The arm64 lane's boot server image is now built in CI, and what it
  installs is checked. Nothing compiled that directory before, so a
  change that did not build waited for whoever next reprovisioned the
  host — usually mid-recovery from something else. Its run recipe also
  carried no restart policy, unlike the runner it serves, so a reboot of
  the serving host left the board looping on a boot server that never
  came back. That reads as a runner reporting `offline`, which is also
  its normal reading between release candidates, so nothing would have
  said so until an rc went red for want of an arm64 verdict.
- The tree names none of the project's own machines any more. Three
  release-note entries and the actionlint config carried an internal
  hostname, the last of them as a standing exception that is no longer
  one.

### With thanks to

- **[@Dev9269](https://github.com/Dev9269)** — wrote the replacement
  text for `docs/internals.md`, describing the `interface_name` gate as
  the capability probe it is and dropping it from the local-vs-CI
  checklist, where a skip on every engine explains no divergence
  ([#675](https://github.com/claymore666/docker-net-dhcp/pull/675), on
  [#673](https://github.com/claymore666/docker-net-dhcp/issues/673)).

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
