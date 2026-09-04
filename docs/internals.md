# How it works

Fundamentally, `net-dhcp` uses the same mechanism as Docker's built-in
`bridge` driver to wire networking to containers: a bridge on the host
acts as a switch, and `veth` pairs connect each container's network
namespace to it. Two things differ:

- **Existing bridge, not a managed one.** Where Docker creates and
  manages its own bridges (and routes/filters traffic), `net-dhcp` uses
  an existing bridge on the host, bridged onto the desired local
  network. (In macvlan/ipvlan mode the parent is a host NIC instead —
  see [parent-attached modes](parent-attached-modes.md).)
- **External addressing.** Instead of allocating addresses from a static
  pool on the Docker host, `net-dhcp` relies on an external DHCP server
  to provide them.

## Flow (bridge mode)

1. A container-creation request is made.
2. A `veth` pair is created and the host end is connected to the bridge
   (both interfaces are still in the host namespace at this point).
3. A one-shot DHCP acquisition runs on the container end (still in the
   host namespace) — the initial IP address is provided to Docker by
   the plugin.
4. Docker moves the container end of the `veth` pair into the
   container's network namespace and sets the IP address — at this point
   that first acquisition is finished with.
5. `net-dhcp` starts a persistent DHCP client on the container end of
   the `veth` pair whose socket lives in the container's **network
   namespace**. The client is not a process: it is the in-tree DHCP
   library running inside the plugin, so there is nothing for the
   container to see and nothing to exec. It never configures the link
   either — the plugin applies the lease via netlink.
6. The client keeps running, renewing the lease when required, until the
   container shuts down.

In macvlan and ipvlan mode the shape is the same, with a child interface
on a host NIC in place of the veth pair and the bridge; the client
lifecycle, the event plumbing, and everything below are identical.

**The DHCP client is `internal/dhcp-golib`, a nested Go module** copied
in from the project's own library repository at the SHA recorded in
`internal/dhcp-golib/SOURCE` and checked byte-for-byte on every PR
(`scripts/check-dhcp-golib-copy.sh`). `pkg/dhcp` is the chassis over it:
it builds the protocol parameters (`pkg/dhcp/params.go`), owns the
namespace and the socket (`pkg/dhcp/chassis.go`), and translates library
events into the events `pkg/plugin` has always consumed. Nothing outside
`pkg/dhcp` knows which client is underneath.

## How the plugin drives the DHCP client

- **Events are values on a channel, not a pipe.** The library reports
  each lease change as a typed event; `pkg/dhcp/chassis.go`'s
  `translate` goroutine maps it to the plugin's `bound` / `renew` /
  `nak` / `leasefail` and the per-endpoint goroutine in
  `pkg/plugin/dhcp_manager.go` applies the address, routes, DNS and MTU
  via netlink. There is no argv to build, no environment to scrub, no
  JSON to parse and no second binary in the image.
- **The emit must not block, and a drop is counted.** The only reader is
  that per-endpoint goroutine, and it stops reading the moment the
  endpoint is torn down. A bare send would park the translate goroutine
  forever on the first event that arrives in that window — a `Leave`
  while a renewal is in flight is enough — and with it the counter
  snapshot the endpoint owes. The channel is buffered, the send has a
  default arm, and a discarded event increments `DroppedEvents()` and
  logs what was lost. A silent drop and a wedge look identical from
  outside the package, which is the whole reason the counter exists.
- **A lapsed lease is reported by the client, not derived from a
  deadline.** The state machine drops the lease with `ReasonExpired`
  when the expiry timer fires, and reports `ReasonNoServer` when a
  retransmission budget runs out with nothing usable heard. Both become
  `leasefail`, which is what moves `dhcp_timeouts`. The v1.x outage
  watchdog is gone, and with it `OUTAGE_TICK`, `OUTAGE_GRACE` and the
  lease-lifetime clamp that existed to keep the watchdog's deadline
  usable.

  What has not changed is the physics: a bound endpoint holds a valid
  address until its lease runs out, so an outage still cannot be
  *proven* before then. What changed is who says it and how precisely —
  the client's own schedule rather than a 30-second tick with a 25-second
  settling time on top.
- **A zero lease lifetime is an infinite lease, not a number to guess
  at.** The 1.x plugin clamped an implausibly long option 51 so its
  watchdog had a usable deadline, and counted the clamp. The library
  hands the chassis an explicit "no expiry" instead, which the caller
  tests rather than compares against a threshold.
- **No client keeps state on disk, so nothing is keyed by interface
  name.** This is where a private mount namespace per client used to be
  needed: the 1.x client kept its lease file and its control socket in
  directories named after the interface, and two containers both called
  `eth0` collided on both — the second client silently handing its
  arguments to the first one's socket and never renewing (#332). The
  library holds its lease in memory and the plugin holds the durable
  copy in its own record, keyed by endpoint. There is no per-client
  mount namespace, no `tmpfs`, and no lease file to go looking for.
- **The socket belongs to the endpoint's namespace because of the
  thread that created it.** `newLibClient` locks the OS thread, enters
  the endpoint's network namespace, opens the client there, and returns
  the thread. A raw `AF_PACKET` socket keeps the namespace it was
  created in, which is the property the mount-namespace machinery used
  to approximate from outside. If the thread cannot be returned it is
  retired rather than reused, since a thread left in a container's
  namespace would silently give the next caller the wrong one.

## How IPv6 is handled in the beta

**It is not.** `validateModeOptions` refuses `ipv6=true` at
`CreateNetwork` with `util.ErrIPv6Beta`, in every mode and for every
spelling the option arrives under, and the error names both the beta and
the milestone that restores DHCPv6. The refusal is at network creation
rather than at the first container because that is the only point where
an operator finds out in time to do something about it: an endpoint that
quietly comes up without the address its network asked for is precisely
the failure this exists to prevent.

`pkg/dhcp` refuses again at every entry point (`GetIP`,
`NewDHCPClient`, `buildParams`) with `ErrIPv6Unsupported`. That is not
belt-and-braces for the create path — it is the only guard on the one
route that still reaches v6 code, a network created by a 1.x build whose
stored options survived the upgrade. What happens on that route, and
what an operator sees, is in the release notes rather than here, because
it is behaviour rather than mechanism.

The v6 code that remains — `pkg/plugin/v6_absence.go`,
`pkg/plugin/v6_link.go`, the DHCPv6 counters, their `/Plugin.Health`
fields and their rows in the exposition — keeps its declarations and
loses its callers. It is left in place because the milestone that
restores DHCPv6 restores them unchanged, and deleting a documented
counter to add it back later costs two documentation changes to end
where it started. **A zero in any of them means "not reachable in this
build", not "nothing went wrong"**, and the same sentence is at the
declaration in `pkg/plugin/endpoints.go`.

`TestV6StructuralZero_TheWritersAreStillTheEnumeratedFour` is what keeps
that claim from decaying: it derives the writer population from the AST
and fails the build when a *new* writer appears, which is the shape that
would make the statement silently false on an IPv4 path. It deliberately
does not try to prove each writer sits behind a v6 branch — an AST proof
of that breaks on a harmless refactor, and the weaker property is the
one that can be held.

**The Router-Advertisement guard is deleted, not disabled.** Its sysctls
(`accept_ra=2`, `autoconf=1`, `keep_addr_on_down=1`) were applied only
from inside the external client's mount-namespace preparation, so the
path that applied them went with the process. Nothing on an IPv4 path
read them: they are `net.ipv6.conf.*` knobs by construction. The
milestone that restores DHCPv6 restores the guard and its argument
together — what it inherits is the measurement, not a mechanism that can
be switched back on.

## How a network chooses its DHCP server

`dhcp_servers` ranks the servers a network may lease from and
`dhcp_deny_servers` names ones it must never lease from (#111, #669).
The operator-facing rules are in the
[driver reference](reference.md); the shape of the implementation
follows from where the filtering happens.

- **Both lists match the Server Identifier (option 54), not the
  packet's source address.** This is the one thing about these options
  that a 1.x operator has to re-learn. The external client compared the
  offer's IP source, which meant that behind a DHCP relay every offer
  looked like it came from the relay and neither list could tell servers
  apart; option 54 is what the server says it is, and it is also what a
  renewal is unicast to, so the beta filters on it and the relay
  limitation goes with the change. The two keys agree whenever a server
  answers directly. They stay DHCPv4-only because DHCPv6 is not
  implemented in the beta at all, so a v6 entry is still refused at
  `docker network create` rather than applying to nothing.

    The library's predicate is where the edge cases live, and they are
    decided rather than incidental: **deny wins** over allow for a
    server named in both; an **allow list fails closed** on a message
    that carries no server identifier at all, because "only these
    servers" that a message can satisfy by omitting the field is not a
    restriction; and a **deny list alone fails open** on that same
    message, because nothing shows it came from a denied server.
- **The plugin still never sends both lists.** The deny list is
  subtracted from the preference list at parse time, so after
  `resolveServerPolicy` there is one truth about what is allowed and one
  kind of list to hand down. That subtraction was forced in 1.x, where a
  configured whitelist switched the blacklist off inside the client and
  a network setting both would have got a denial nothing enforced; it is
  kept here because the property it buys — one truth, not two composed
  at the far end — is worth more than the redundancy the library would
  now tolerate. A preference list that denies its way to empty fails the
  network create, because the alternative is degrading into "accept any
  server at all", the opposite of what both options were set to achieve.
- **Ordering is not expressible to the client, so preference is an
  acquisition-time ladder.** The initial acquisition runs one attempt
  per preferred server, in the operator's order, each restricted to
  that server alone. The ladder **divides** the existing acquisition
  budget rather than extending it — a preference list must not make
  `docker run` slower, and the one-shot at `CreateEndpoint` already
  runs against a tight ceiling — with the remainder of the division
  dropped rather than handed to the last tier, so the attempts can only
  sum to at most the budget. The per-attempt floor (`minAttemptBudget`,
  3s) predates the library and now sits just under its first
  retransmission at 4s, so a tier that lands on the floor buys one
  DISCOVER and no retry. The same reading applies to the undivided
  budget: the library's intervals are 4s, 8s, 16s, 32s with a 64s
  ceiling and ±1s of jitter, each armed as its packet goes out, so
  retransmissions land at ~4s, ~12s, ~28s and ~60s and the default 10s
  `lease_timeout` funds one of them. That is not a regression — the same 3s used to
  have to pay for a namespace and a process spawn as well — but its
  original derivation is dead and nothing re-derives it against the
  library's schedule. `dhcp_server_tier_fallbacks` counts a
  fall-through to a lower tier, which is the only outside signal that a
  preferred server has gone quiet while every container still starts;
  `dhcp_server_policy_exhausted` counts a restricted acquisition where
  nothing answered, which is otherwise indistinguishable from an
  ordinary DHCP timeout.
- **The persistent client gets the whole allowed set, not the tier that
  won.** It has to be able to rebind after the preferred server goes
  away, and an allow list pinned to the winning tier would strand the
  endpoint with no lease instead of failing over. Preference is an
  acquisition-time concept; once a lease is held it stays with whoever
  granted it, because renewal is unicast to that server.

## How a lease is checked against the segment

The plugin asks whether some *other* device already holds the address
its DHCP server just leased (#524). Since the 2.0 beta the question is
asked by the DHCP client itself, as RFC 5227 Address Conflict Detection,
from inside the container's own network namespace and on its own link —
not by the chassis from the parent. The operator-facing rules and the
counters are in the [driver reference](reference.md#pluginhealth); this
is the mechanism.

- **It is part of the acquisition, not a step beside it.** §2.1 sends
  three ARP Probes with an all-zero sender protocol address, then waits
  ANNOUNCE_WAIT before the address may be used; §2.3 sends two
  Announcements once it is; §2.4 keeps listening for the whole life of
  the lease. A conflict at any point produces a `DHCPDECLINE` (RFC 2131
  §3.1(5)) and a fresh DISCOVER, which is what makes the DHCP server's
  own log the outside evidence for the whole thing.
- **The vantage point moved, and that is what closed the two holes the
  old check had.** The chassis used to send a datagram from the PARENT
  link to make the kernel resolve the address, and compare the answering
  MAC with the endpoint's. That could only ever check the address a new
  endpoint was about to be handed — an address that changed mid-life was
  never re-probed — and it needed the parent to carry an address on the
  leased subnet, because a host answers an ordinary ARP request only if
  it can route a reply back to the sender. A §2.1.1 Probe carries an
  all-zero sender protocol address, which Linux answers for any local
  target without consulting a route, so the bare-parent limitation is
  gone; and §2.4 covers the rest of the lease's life, so the mid-life
  hole is gone with it.
- **Our own endpoint holds the address too — that is the premise.** The
  old check answered it with macvlan's parent/child isolation plus a MAC
  comparison. RFC 5227 answers it in the client: a reply whose sender
  hardware address is the client's own is not a conflict. That is what
  keeps bridge mode correct, where the host *can* reach the container
  and a did-anything-reply check would report every single endpoint as a
  conflict. The cost is unchanged: a squatter that is another container
  on the same parent is invisible, excluded by construction and not
  pending work (#528).
- **It costs seconds, and the operator chooses who pays them.**
  `conflict_check=wait` (the default) finishes §2.1 before the address is
  configured, so `docker run` waits 4.0–7.0s; `async` configures the
  address at the DHCPACK and probes behind it, so a conflict found later
  CHANGES a running container's address; `off` sends no ARP at all. The
  `lease_timeout` default is derived from the same constants — one
  DISCOVER retransmission plus the worst probe window — rather than
  written down, so the two cannot drift apart.
- **The phase survives a plugin restart.** In `async` the address is in
  use while §2.1 is still running, so the conflict-detection phase is
  written into the durable lease record and handed back to the next
  process on resume. Without it a restart inside that window would leave
  a container holding an address nothing ever finished checking.

## How a lease gets handed back

It does not. Nothing this plugin runs ever sends a `DHCPRELEASE`, and
that is deliberate as of v1.9.0 (#800).

A lease is a lease. When a container stops, its address stays leased
until the lease expires, and if the container comes back before then it
asks for the same address and gets it — the ordinary DHCP path, and
exactly what happens when a physical host on the segment reboots or
loses power. A container is a host on this segment and costs the server
what one costs.

Neither client releases. The `CreateEndpoint` one-shot ends by
cancelling its own manager, which drops the lease locally with
`ReasonStopped` and sends nothing — the record carries the lease to the
persistent client that takes over moments later, which resumes it as
INIT-REBOOT rather than discovering afresh. The persistent client is
stopped at `Leave` and keeps the address for the container that may be
about to restart. A stop is this process's own shutdown reported back to
it, which is why nothing counts it as a lease loss: doing so would
report one for every container that started successfully.

**Why this changed.** Up to v1.8.x the plugin released aggressively —
the external client emitted a `RELEASE` on a graceful stop, and a background
*reclaim* handed back the one-shot's address whenever no persistent
client had taken ownership of it (a container that exited before the
attach completed). Both were trying to return an address promptly rather
than let it sit until expiry. Both raced the tombstone.

A `docker restart` is a `Leave` immediately followed by a `Join` for the
same MAC, and the tombstone exists to promise that `Join` the same
address. At the moment the release ran, "this endpoint is gone" and
"this endpoint is coming straight back" were indistinguishable — so the
plugin was observed telling the server an address was free in the same
second the container came back to claim it. The reclaim was measured
firing four times on ordinary restarts of live containers.

What was gained was a faster return of an address nobody wanted. What
was risked was an address handed to someone else while a container was
still using it — the duplicate assignment #524 added detection for,
manufactured by the plugin itself. Waiting for expiry has no such
failure mode, so the whole mechanism went: the release itself, the
reclaim, and the `orphaned_leases_released` and
`orphaned_lease_release_failures` counters that measured it.

The surviving teardown counter was renamed to match: what was
`lease_release_failures` is now `client_stop_failures`, because a client
that exits badly is all it can still mean.

The cost is that a short-lived container's address is unavailable for
one lease time. Size the server's pool and lease time for the churn,
the same way you would for any other population of hosts.

## How operations on one parent NIC are serialised

A parent NIC registers one `rx_handler`, so it is a macvlan port or an
ipvlan port and never both — whichever kind asks second gets `EBUSY`.
That is a kernel rule; one mode per parent stays the operator-facing
constraint.

What the plugin can stop is inflicting it on itself. Two of its paths
attach a child to a parent: creating an endpoint, and the
`validate_dhcp` probe — which holds its link for a full DHCP round
trip. Since v1.6.0 both take a per-parent gate first, so they queue
instead of refusing each other (#486, #549).

There used to be a third, the orphaned-lease reclaim, and it was the
demanding one: it ran from a goroutine ordered against no Docker request
at all. It is gone (#800, see above), which shortens the worst case the
gate has to cover but does not remove the need for it — the probe still
holds a parent across a DHCP round trip while an endpoint may ask for
the other mode.

`parent_link_waits` counts operations that queued — the mechanism
working. `parent_link_wait_timeouts` counts ones that gave up and
proceeded anyway; they may still succeed, but the budget has stopped
covering the holder's duration.

The rule is enforced by **two** mechanisms, and it is worth being exact
about where each one stops, because the guard type exists precisely to
replace a prose guarantee about a property nothing checked.

The compiler holds one half: `addChildLink` takes a guard value, so a
path that never asks for one does not compile. It does **not** hold the
other half. "Only `lockParent` makes a guard" is not something Go can
express — the struct's zero value is valid, so `addChildLink(&parentGuard{},
link)` compiles and holds nothing, and `lockParent` returns exactly that
literal on its own no-parent path, so the shape is already in the file as
a pattern to copy. The realistic route to it is not malice: a new
parent-attached call site, a compiler demanding a guard, and the zero
value sitting right there.

That half is enforced by `scripts/check-parent-gate-accounting.sh`, which
fails the build on a `parentGuard` constructed anywhere but `lockParent`.
A second accounting file, `.github/linkadd-accounting.txt`, covers the way
around the type entirely — a direct `netlink.LinkAdd`, which bridge mode
needs, having no parent to contend for.

Nor does the guard say *which* parent it is for, so one taken on one NIC
and handed to a link on another compiles. That is a deliberate
non-goal — closing it means a runtime comparison, trading a compile error
for a log line, on a mistake no current call site can make. The comment
at the top of `pkg/plugin/parent_gate.go` is the authority on all of
this; if this section and that comment ever disagree, the comment is
right.

## How state outlives a process

Three separate mechanisms keep addresses stable across three different
kinds of restart. Their *observable* behaviour is documented in the
[driver reference](reference.md#behaviour); this is how they are built.

- **Per-network options → `STATE_DIR/<network_id>.json`.** Written at
  `CreateNetwork` so the per-endpoint handlers never call back into the
  Docker API to learn the mode or parent. That callback is precisely what
  deadlocked the upstream plugin during `dockerd` startup, when the
  daemon asked it to restore containers using its own networks. On a
  cache miss the handlers fall back to the API and back-fill the file.
- **Tombstones → a single file under `STATE_DIR`.** Written at
  `DeleteEndpoint`, consumed at the next `CreateEndpoint`, 60-second TTL.
  Each carries the previous MAC, the last v4 and v6 addresses, and the
  container hostname. The lookup is keyed by **network ID** plus
  hostname — which is why an endpoint keeps its address across a
  container restart but not across removal of the network itself, since
  the replacement network has a different ID. Ambiguity is resolved
  conservatively: when neither side knows the hostname, a tombstone is
  consumed only if it is the network's single candidate, so concurrent
  restarts fall back to fresh MACs rather than risk handing one
  container's identity to another.
- **Recovery → a walk of Docker's network list at startup.** For every
  endpoint on a plugin-served network, a DHCP manager is rebuilt and its
  first acquisition requests the address the container already holds
  (option 50). It runs synchronously inside plugin construction when the
  daemon answers, which is the normal case and finishes before the
  socket accepts anything. When the daemon is **not** serving yet the
  walk cannot run there at all: Docker respawns the plugin during its
  own startup, so blocking would make us unreachable to the very daemon
  we are waiting on. Recovery is handed to `Listen` instead and runs in
  a goroutine *after* the socket is up (#383) — which puts it in the
  same window as the `Join`s a restarting host is issuing.
  `recovery_deferred` counts that postponement; it is not a fault, and
  only an exhausted retry budget lands on `recovery_failed`.

  **The deferred path is what makes the compare-and-set load-bearing.**
  Recovery builds a manager and registers it only if no manager is
  already registered for the endpoint, in one locked operation. The
  check and the registration used to be two, and a `Join` landing in
  the gap had its live manager evicted from the registry while its
  client kept running — untracked, unstoppable, and competing with
  recovery's fresh client on the same interface. A `Join` is newer
  truth than a recovery walk and may displace it; recovery is older
  truth and must yield, which is what a compare-and-set expresses and a
  stop-what-I-displaced does not. `recovery_already_managed` counts an
  endpoint left alone — not healthy-affecting, since that endpoint
  *has* a renewal client, and the only outward sign the race happened
  at all (#480, #679).

The plugin's identity is a MAC. Both stability mechanisms exist because
DHCP servers key on it, and everything above is in service of presenting
the same MAC to the server across an event the container did not choose.

## How the counters are exposed

`/Plugin.Health` and `/metrics` are two renderings of one snapshot
(#651). What each of them says is in the
[driver reference](reference.md#observability); the mechanism is that
one function builds that snapshot and both handlers render it and
nothing else.

- **One source, because two hand-kept lists rot.** A metrics handler
  that read the atomics itself would be a second list of every counter,
  and this repository has watched that shape decay more than once
  (#542, #636) — a stale list is invisible until an alert that should
  have fired does not. The exposition is a table keyed by the
  `HealthResponse` JSON tag it renders, and a unit test walks that
  struct by reflection and fails on a field nobody claimed. Adding a
  counter without exposing it is a red unit test rather than a hole in
  somebody's dashboard.
- **The snapshot is not a single atomic instant, and that is
  deliberate — but only for values read one at a time.** The counters
  are read without a lock, so two of them can be a few nanoseconds
  apart. For a counter an operator reads on its own that is harmless:
  they are monotonic counters read for rates and alerting, not an
  accounting ledger, and this is what `/Plugin.Health` has always done.
  Only the two map lengths take the mutex, because reading a map during
  a concurrent write is a data race rather than a stale number.

  It stops being harmless the moment a rendered value is **combined**
  from two of them, and #730 is what that costs. Each family pair is
  therefore loaded exactly once into a local, and the aggregate is the
  sum of those two locals — never a second `.Load()` of a half that was
  already read.
- **Both family series are stored; neither is derived.** Six counters
  carry a `family` label. `bumpFamily` increments **exactly one** of a
  pair — the v4 half or the v6 half, never both and never a third
  aggregate — so `_v4` and `_v6` are peers, and the unsuffixed counter
  an operator alerts on is their **sum**, computed at snapshot time
  (#212, #730).

  Until v1.8.0 the aggregate was the stored counter and `family="ipv4"`
  was `total - v6` at render time, clamped at zero. Two independently
  updated counters combined by **subtraction** can be read in an order
  that yields a value below the previous scrape, and Prometheus reads
  any counter decrease as a **reset**, repaying the whole accumulated
  value as an increase on the next scrape — a one-event skew surfacing
  as a rate spike of the entire count. The clamp hid the extreme case
  and did nothing about the dip. Adding two monotonic counters has no
  such failure mode, because neither operand can decrease; subtracting
  them does, in either read order.

  If a family series ever needs computing rather than reading again,
  the arithmetic belongs in `healthSnapshot` where both halves are
  loaded once, not in the renderer.
- **Two exposure paths, and only one of them opens a port.** `/metrics`
  is on the plugin socket unconditionally: it costs nothing, and it
  lets an operator with a socket-aware scrape path collect metrics
  without the plugin listening anywhere. The TCP listener is
  `METRICS_ADDR`, and it is off unless set. The plugin runs with
  `"network": {"type": "host"}` and holds `CAP_NET_ADMIN`,
  `CAP_NET_RAW`, `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE`, so any port it
  opens is on the host's own network namespace — opening one has to be a decision an
  operator made, not something they inherited by upgrading. That
  listener's mux carries `/metrics` and nothing else, so no libnetwork
  RPC becomes reachable over TCP; it binds before the call returns, so
  an unusable address fails at startup where somebody sees it rather
  than in a goroutine that logs and leaves the plugin running without
  the endpoint that was asked for; and a wildcard bind is said out loud
  rather than refused. What a wildcard leaks is not a lease inventory —
  the exposition is aggregate counters plus a per-process instance UUID,
  with no endpoint IDs, container names, addresses or MACs, which
  `SECURITY.md` promises and
  `TestMetricsExposition_NoPerEndpointIdentifiers` pins — it is this
  plugin's operational telemetry, published on every interface the host
  has, since the plugin runs in the host's network namespace (#709).
- **The socket's mode is pinned rather than inherited.** Serving
  `/metrics` on the plugin socket is unchanged ground only because that
  socket is root-only: anything able to read it can already call every
  RPC. A UNIX socket is created `0777 &^ umask`, so until #687 that
  property was whatever umask the plugin runtime happened to hand us —
  true under the usual `0022`, false under `0002`, and nothing said
  which. `Listen` now chmods the socket to `0600` and refuses to serve
  if it cannot, since an unknown mode is exactly the state being
  guarded against. Only the daemon speaks this protocol and it connects
  as root, so nothing legitimate needs group or other.

## Running the tests

Four loops, cheapest first. Only the last needs root or a plugin.

| what | command | needs |
| --- | --- | --- |
| the Go unit tests | `go test ./...` | nothing — seconds |
| the suite's own guards | `go test ./test/integration/harness/` | nothing |
| **the whole fast CI lane** | `make check` | nothing — about a minute |
| both integration suites | `sudo make integration-local` | root, Docker |

**`make check` is the one to run before pushing.** It runs the same
gates as the Test workflow's two fast jobs — `test` for build, vet,
format, the race suite and the short fuzz, `policy-gates` for every
`check-*.sh` and the gate self-tests (#829 split them, and both are
required contexts) — with no privileges
and no host mutation, so the answer you get locally is the answer CI
will give.

The fuzz step is currently a no-op, and is named here as one rather than
counted as coverage: its two targets belonged to the 1.x lease parsers
and no target of either name exists in this tree, so `go test -fuzz`
matches nothing and exits 0. The wire codec's own fuzzing lives in the
library module. Re-pointing the step is work, not a claim this page
makes.

The lane's contents live in `scripts/local-lane.sh`, and
`scripts/check-local-lane.sh` fails CI if that file lists fewer gates
than the workflow runs; a local target that hand-listed them would
quietly cover less the first time a gate was added (#636, the same
shape as #542).

Everything it does **not** do is declared rather than absent —
`scripts/local-lane.sh --list-exempt` prints the list with reasons, and
that is the place to read it rather than a count written here, which has
already gone stale once. The reasons fall into two kinds: gates that
need the pull request that does not exist locally (a commit range, a
title, a body, or the base ref the PR is opened against), and gates that
need the network. In every case a local answer would be a different
answer, not a cheaper one.

A step whose tool is missing (`staticcheck`, `actionlint`, `shellcheck`)
is **skipped loudly** and named in the summary rather than passing
silently. `STRICT=1 make check` turns any skip into a failure — use that
anywhere a green exit is read as coverage instead of by a person who can
see the summary.

CI shards the main suite across five jobs (#381, #468, #877);
`integration-local` deliberately does not — a local run is one machine, so
sharding would serialise the shards and only add overhead. If you want to
reproduce a single CI shard, `sudo make integration-test-shard SHARD=1 OF=5`.
The count lives in `.github/workflows/integration.yml`'s matrix, beside the
measurement that justifies it; `scripts/check-durations-table.sh` keeps the
weights that partition it honest.

**Use `integration-local`, not `integration-test`.**

`make integration-test` and `make integration-test-failure` only run
`go test`. Building and installing the plugin is a separate chain
(`make create enable`). CI never diverges because the workflow does the
build as its own step before calling either target — a local run has no
such guarantee, so it tests **whatever plugin happens to be installed**.

That is not a hypothetical. While validating #374 a stale installed
build made two tests fail for reasons unrelated to the branch, *and*
made the health floor report `clean` for counters that build could not
publish at all. Wrong in both directions, from one cause. Rebuilding
reproduced CI exactly.

`integration-local` chains `integration-cleanup create enable
integration-test integration-test-failure`, so neither a stale plugin
nor a previous run's leftovers can be what you measure. The cleanup
step mirrors the CI job's own first step: local runs are the only
place that state accumulates, because CI's runners are ephemeral.

The two suite targets deliberately do **not** depend on a rebuild: CI
calls them in sequence between its own build and teardown, and a
rebuild dependency there would reinstall the plugin *between* the two
suites — recycling it mid-run and resetting the health floor's
observation window with it.

### Reading the output

Both suites tee to `test/integration/logs/`. At the end of each, the
health floor prints a verdict for the whole run:

- `HEALTH FLOOR: clean — ... over the whole Ns run (plugin up Ms, ...)` —
  the plugin was up throughout and nothing healthy-affecting moved. The
  run's duration and the plugin's uptime are separate numbers and are
  always printed as such: locally the plugin often long predates the
  suite, and where the gap is large the line says by how much, because
  the counters are cumulative and carry that earlier history too.
- `HEALTH FLOOR: clean over the last Ns of an Ms run` — the plugin
  restarted mid-suite, so the counters only cover the tail. The
  whole-run fault census covers the rest.
- `HEALTH FLOOR: clean ... over the plugin's Ns uptime` — the suite's own
  duration could not be measured, so no coverage claim is made rather
  than one being invented.
- `PLUGIN FAULTS: N across the whole run` — read from the log rather
  than the counters, so it survives a restart. Any non-zero value fails
  the run.

A run that cannot read the plugin log fails rather than reporting
clean: an unreadable instrument is not a clean result.

### If a local run disagrees with CI

Check, in order:

1. Did you build? `sudo make integration-local` rather than a bare
   suite target.
2. Is a previous run's state still around? `sudo make
   integration-cleanup`. `integration-local` now does this for you;
   you only need it by hand after running a suite target directly. A
   single leftover container fails an unrelated test with a name
   conflict and reads exactly like a regression.

The `interface_name` tests (#125) are not a local-vs-CI divergence:
they probe whether the engine applies a remote driver's `DstName` and
skip when it does not. The probe (`engineAppliesIfname`, used by
`TestInterfaceName_MultiNetworkDeterministic`) runs a throwaway
container and checks the interface the engine actually created — there
is no version threshold to hit. The probe fails on every *released*
engine: the upstream fix (moby/moby#52866, stopping the remote-driver
proxy from dropping `DstName`) merged to moby master on 2026-08-26 and
is milestoned for engine 29.8.0, which is not out yet (latest release
29.7.2 as of 2026-08-27). Until a box running an engine that carries it
executes the suite, those tests skip in CI and locally alike. A skip is
expected, not a signal that the run diverged.

## Request fixtures

`pkg/plugin/testdata/requests/` holds the raw request bodies the Docker
daemon actually sent, recorded during an integration run. The unit tests
in `pkg/plugin/fixtures_test.go` replay them instead of hand-building
`CreateEndpointRequest` / `JoinRequest` values.

The difference matters more than it looks. A hand-built request asserts
the code against *our model* of what libnetwork sends. When the model and
the daemon disagree, every unit test still passes and the disagreement
surfaces on a privileged runner — or in production. That is not
hypothetical: `stable_lease` was designed against an assumed
`CreateEndpoint` payload and had to be reverted from v1.3.0 once the
endpoint identity turned out to be unresolvable in the `docker run` and
Compose flows (#298, #219). The request shape *was* the defect.

The handler decodes with `DisallowUnknownFields`, so a field the daemon
adds and we do not model is a `400` at runtime, not a warning. The
fixture tests replay through that same parser, which turns "the engine
started sending something new" into a unit-test failure instead of a
container that will not start.

### Layout

```
pkg/plugin/testdata/requests/
  macvlan-run/
    manifest.json                              engine, date, commit, flow
    0001-NetworkDriver.CreateNetwork.json      one raw body per call,
    0002-NetworkDriver.CreateEndpoint.json     numbered in the order the
    ...                                        daemon issued them
  bridge-run/
  macvlan-restart/
```

Three flows, because the flows are where the payloads differ — that is
exactly how #298 got through.

### Regenerating them

**Dispatch the `Capture fixtures` workflow.** It runs on the integration
lane, so the capture happens against the daemon the suite actually talks
to, and it opens a pull request with the re-recorded bodies:

```console
$ gh workflow run capture-fixtures.yml --ref <branch>
```

A pull request rather than a push, deliberately. A changed request body on
an engine bump is a finding — it is the signal #218 and #125 are blocked
on — so the diff wants eyes rather than an automatic commit. Before it
opens anything the job re-runs `check-fixture-engine-drift.sh` against its
own output, so a capture that recorded nothing fails there instead of on
somebody else's pull request days later. `check-capture-lane.sh` keeps the
job on the lane, which is the half the drift gate cannot check: on a
hosted runner the recorded engine and the checked engine move together,
agree with each other, and both describe a daemon the suite never speaks
to.

#### By hand

The workflow drives one command, and you can run it yourself on a host
with Docker and the integration prerequisites:

```console
$ sudo make capture-fixtures CAPTURE_COMMIT=$(git rev-parse --short HEAD)
```

**Capture against the daemon the suite runs, not the one your shell talks
to.** These are not always the same machine's engine: the integration job
runs *inside* the CI runner container, against that container's nested
`dockerd`, which can be several minor versions ahead of the host's. A
capture taken on the host is a recording of a daemon the suite never talks
to, and `check-fixture-engine-drift.sh` will reject it — which is exactly
what happened the first time these fixtures met the gate (26.1 recorded,
29.7 running). To record against the lane's engine, run the capture inside
the runner image:

```console
$ docker run -d --name dh-capture --privileged -v "$PWD":/work \
    --entrypoint bash ghcr.io/claymore666/dhcp-ci-runner:latest \
    -c 'RUNNER_JIT_CONFIG=x /entrypoint.sh || true; sleep infinity'
$ docker exec -w /work -e PATH=/usr/local/go/bin:$PATH \
    -e SUDO_UID=$(id -u) -e SUDO_GID=$(id -g) \
    dh-capture make capture-fixtures CAPTURE_COMMIT=$(git rev-parse --short HEAD)
```

The entrypoint brings up the same supervised daemon the suite uses and then
fails its runner exec, leaving that daemon running; `go` lives in
`/usr/local/go/bin`, which a bare `docker exec` does not put on `PATH`; and
`SUDO_UID`/`SUDO_GID` make the recipe hand the regenerated files back to
you instead of leaving them owned by root.

It builds the instrumented (`-cover`) plugin, sets `REQUEST_CAPTURE_DIR`
on it, runs one integration test per flow into a cleared directory, and
writes each flow's `manifest.json`. Pass `CAPTURE_COMMIT` from the
unprivileged shell as shown: the recipe runs as root against a checkout
you own, and git refuses that as dubious ownership, which would leave the
commit field empty and produce a capture nobody can attribute.

`REQUEST_CAPTURE_DIR` is declared in `config-cover.json` only — the same
place `GOCOVERDIR` lives, and for the same reason. It is test
instrumentation, so the shipped manifest never grows a setting whose only
use is regenerating this repository's fixtures. With it unset,
`captureHandler` returns the mux unchanged and the plugin carries no
extra allocation, syscall, or failure mode.

### Why they cannot quietly rot

A fixture nobody refreshes is a fossilised assumption that agrees with
itself forever — the same "asserts our model" problem, now with a green
test sitting next to it, which is worse, because it looks like evidence.
Three things stop that:

- **A missing or empty fixture fails.** `loadFixtureFlows` calls
  `t.Fatalf`, never `t.Skip`; a suite that replayed nothing would
  otherwise report green.
- **A manifest without provenance fails.** Empty `engine`, `captured`,
  `commit` or `flow` is an error, because a capture nobody can date
  cannot be reviewed for staleness.
- **`scripts/check-fixture-engine-drift.sh`** compares each manifest's
  engine against the daemon the integration suite actually runs, and
  fails on a `major.minor` difference. It runs in the self-hosted suite
  job, which is the only host that knows what that engine is; patch
  releases and distro suffixes (`26.1.5` vs `26.1.5+dfsg1`) are not
  drift. Its self-test is `scripts/test-check-fixture-engine-drift.sh`.

### What to do when the unknown-field test fails

It is not automatically a defect. A new field may be irrelevant to us. It
means the request contract moved and somebody has to decide — which is
the point, because today nothing else would say it moved at all. Model
the field, or record why it is ignored.

#218 (stable MAC) is waiting on exactly this signal: it needs
`netlabel.EndpointName` to arrive at `CreateEndpoint`, and the captures
confirm that field is absent on engine 29.7 — the day a capture from a
newer engine carries it, the test names it.

#125 is **not** covered by this signal, and that is worth stating
because the shape invites the assumption. Its blocker is on the
*response* side (the engine honouring the plugin's `DstName` at `Join`,
moby/moby#52866); the option itself has always been forwarded in the
request. No request capture will ever change when that fix ships, so
the thing that detects it is the behavioural probe in the integration
suite, not these fixtures. The 26.1 -> 29.7
re-record is the worked example: it introduced
`com.docker.network.enable_ipv4` on `CreateNetwork`, which is carried
inside `Options` (a map) and so costs nothing, but it arrived unannounced
and the fixtures are what showed it.

## See also

- [Driver reference](reference.md) — every option, counter, and behaviour
- [Bridge mode](bridge-mode.md) and [macvlan / ipvlan](parent-attached-modes.md) setup
[#725]: https://github.com/claymore666/docker-net-dhcp/issues/725
