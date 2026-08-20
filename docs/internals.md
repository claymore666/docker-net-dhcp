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
3. A DHCP client (`dhcpcd`) is started on the container end (still in
   the host namespace) — the initial IP address is provided to Docker by
   the plugin.
4. Docker moves the container end of the `veth` pair into the
   container's network namespace and sets the IP address — at this point
   that first client is stopped.
5. `net-dhcp` starts a persistent `dhcpcd` on the container end of the
   `veth` pair in the container's **network namespace** (but still in the
   plugin's **PID namespace**, so the container can't see the DHCP
   client). It runs observe-only (`--noconfigure`): the plugin applies
   the lease to the link via netlink rather than letting the client
   reconfigure the interface.
6. `dhcpcd` keeps running, renewing the lease when required, until the
   container shuts down.

In macvlan and ipvlan mode the shape is the same, with a child interface
on a host NIC in place of the veth pair and the bridge; the client
lifecycle, the event plumbing, and everything below are identical.

## How the plugin drives `dhcpcd`

- **Events come over a FIFO, not the client's stdout.** A `dhcpcd` hook
  script reports each lease event (bind, renew, NAK) as JSON through a
  pipe the plugin opened — which is why the plugin ships a small handler
  binary rather than parsing client output. The plugin applies the
  resulting address/routes via netlink itself.
- **A lapsed lease is not one of those events.** The plugin runs
  `dhcpcd --noconfigure`, and in that mode a lease running out is
  reported as `RELEASE` — the same thing a graceful stop emits. The two
  are indistinguishable, so treating either as a failure would count
  every normal container teardown as one, and the handler drops both.
  This is why the plugin cannot learn about a dead DHCP server by
  waiting to be told.
- **Outages are therefore derived, not reported.** Each bind and renew
  records the lease lifetime the server granted, and a watchdog compares
  it against the time since that endpoint was last served: once
  `lease + grace` has passed with nothing heard, the server is treated as
  unreachable and `dhcp_timeouts` starts climbing (#353). The trade-off
  is inherent — a valid lease means a working address, so an outage
  cannot be *proven* before that lease would have run out. Cadence is
  [`OUTAGE_TICK` / `OUTAGE_GRACE`](reference.md#plugin-settings).
- **The FIFO is held open by a dedicated keep-alive writer.** The reader
  drains it to a natural EOF rather than being torn down when the client
  exits. This is not incidental: the one-shot client writes its `bound`
  event and exits immediately, and closing the FIFO on that exit races
  the reader for an event still sitting in the kernel pipe buffer. Under
  load that lost roughly 4% of acquisitions (#332). With a separate
  writer the reaper closes only the write end, so the event cannot be
  dropped — the guarantee is structural rather than retried around.
- **Each client runs in a private mount namespace.** `dhcpcd` keys *two*
  on-disk locations by interface name, with no runtime override for
  either: its **state** directory (lease files, DUID) and its **runtime**
  directory (pidfile and control socket). Two containers whose link is
  the default `eth0` would otherwise collide on both. The state collision
  corrupts lease bookkeeping; the runtime collision is worse and silent —
  the second client finds the first one's control socket, forwards its
  arguments to that process and exits 0, so it never runs a client of its
  own and its lease is never renewed or released (#332). The plugin
  shadows both directories with a private `tmpfs` in each client's own
  mount namespace, which keeps them fully independent.

  A side effect worth knowing when debugging: the lease file is only
  visible from inside that namespace, so reading it means
  `nsenter -t <dhcpcd-pid> -m` (see
  [verifying renewal](reference.md#verifying-that-renewal-works)).

## How a lease is checked against the segment

Since v1.6.0 the plugin asks, after each IPv4 lease, whether some *other*
device already holds the address it was just given (#524). The counters
and the operator-facing rules are in the
[driver reference](reference.md#pluginhealth); this is the mechanism, and
every part of it is a constraint rather than a preference.

- **The question is asked by sending, not by asking netlink.** Inserting
  the neighbour in `NUD_INCOMPLETE` and letting the kernel resolve it
  looks like the tidy implementation. The call succeeds, the kernel does
  not probe, and the entry stays `INCOMPLETE` — so the check reports
  "nobody holds it" with a squatter sitting on the address. An ordinary
  datagram to the discard port makes the kernel do the ARP instead; its
  delivery is irrelevant, the packet exists to resolve L2.
- **That also keeps the plugin inside its declared privileges.** A real
  RFC 5227 ARP probe needs `AF_PACKET` and therefore `CAP_NET_RAW`, which
  `config.json` does not grant — adding it would force every operator to
  re-approve the plugin's privileges on upgrade. Ordinary traffic gets
  the same answer with `CAP_NET_ADMIN` alone. (`dhcpcd` itself runs with
  `-A`, which turns *its* conflict detection off; this is what replaces
  it.)
- **The probe runs from the parent link, and compares MACs.** Our own
  endpoint holds the leased address too — that is the premise — so the
  vantage point has to be one our endpoint cannot answer from, which is
  what macvlan's parent/child isolation gives. Comparing the answering
  MAC against the endpoint's is what makes the same code correct in
  bridge mode, where the host *can* reach the container and a
  did-anything-reply probe would report every single endpoint as a
  conflict. The cost of that vantage point is that a squatter which is
  another container on the same parent is invisible; that is excluded by
  construction, not pending work (#528).
- **Egress is pinned, because an unrouted datagram answers the wrong
  question.** The packet is otherwise routed by the host table and can
  leave by a different interface entirely, landing the neighbour entry
  somewhere nobody is reading — measured as a squatted address reported
  clean. A temporary `/32` scope-link route on the parent fixes the exit,
  and both it and any borrowed address are removed when the probe
  returns.
- **The source address decides whether the question is answerable at
  all.** A host answers ARP only when it can route a reply back to the
  sender, so the probe prefers an address the parent already holds on the
  leased subnet. Where the parent has none it falls back to a random
  link-local source — random because two probes can run at once, and
  link-local because any address borrowed from the operator's own subnet
  might be the next one their DHCP server hands out. A gateway-less
  squatter cannot reply to that fallback, which is why a parent with no
  on-subnet address yields `conflict_probe_failures` and an explicit
  *undetermined* rather than a clean result.

The probe is asynchronous and off `CreateEndpoint`'s critical path, with
a 2-second cap that only an unclaimed address ever pays.

## How a lease gets handed back

Normally the container's own `dhcpcd` does it: a graceful stop emits a
`RELEASE` and the server frees the address at once instead of waiting
for the lease to expire.

That needs a binding to release. Two cases leave one held with nobody
responsible for it — the persistent client **never started** (the
container was already gone), or it **started but never bound** (it was
signalled before its first `ACK`, so `dhcpcd` had nothing to release and
exited cleanly). The address was won regardless, by the one-shot at
`CreateEndpoint`.

The plugin then **reclaims**: a temporary child of the network's kind,
re-acquire the same address under the endpoint's identity, release it
properly. Per family — a dual-stack endpoint hands back exactly the
addresses whose client never bound, IPv4 via the client-id, IPv6 via
the same DUID and IAID the endpoint used (v1.7.0+, #608; before that
the IPv6 half was leaked and the ledger wrote it up as released).
Counted by `orphaned_leases_released` and
`orphaned_lease_release_failures`, and written to the audit ledger.

**The scoping is load-bearing.** It fires when an endpoint *leaves*, not
on every manager shutdown — `Close` stops every manager on an upgrade or
`docker plugin disable`, with containers still running. A never-bound
manager is legitimate there (a DHCP server that is not answering, while
the container still holds the one-shot's address), and releasing would
tell the server an address is free while it is in use: the duplicate
assignment this release added detection for, manufactured by us.

A missed reclaim leaves a lease to expire. A wrong one causes an outage.

## How operations on one parent NIC are serialised

A parent NIC registers one `rx_handler`, so it is a macvlan port or an
ipvlan port and never both — whichever kind asks second gets `EBUSY`.
That is a kernel rule; one mode per parent stays the operator-facing
constraint.

What the plugin can stop is inflicting it on itself. Three of its paths
attach a child to a parent: creating an endpoint, the `validate_dhcp`
probe, and the reclaim above — which holds its link for a full DHCP
round trip, from a goroutine ordered against no Docker request. Since
v1.6.0 all three take a per-parent gate first, so they queue instead of
refusing each other (#486, #549).

`parent_link_waits` counts operations that queued — the mechanism
working. `parent_link_wait_timeouts` counts ones that gave up and
proceeded anyway; they may still succeed, but the budget has stopped
covering a reclaim's duration.

The rule is enforced by the compiler, not by review: attaching a child
link requires a guard value only the gate can produce, so a path that
skipped it does not compile. An accounting file covers the one way
around — a direct `netlink.LinkAdd`, which bridge mode needs, having no
parent to contend for.

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
  (option 50). This runs synchronously inside plugin construction,
  before the socket accepts requests, so an incoming `CreateEndpoint`
  cannot race it.

The plugin's identity is a MAC. Both stability mechanisms exist because
DHCP servers key on it, and everything above is in service of presenting
the same MAC to the server across an event the container did not choose.

## Running the tests

Four loops, cheapest first. Only the last needs root or a plugin.

| what | command | needs |
| --- | --- | --- |
| unit + gate scripts | `go test ./...` | nothing — seconds |
| the suite's own guards | `go test ./test/integration/harness/` | nothing |
| **the whole fast CI lane** | `make check` | nothing — about a minute |
| both integration suites | `sudo make integration-local` | root, Docker |

**`make check` is the one to run before pushing.** It runs the same
gates as the `test` job — build, vet, format, the race suite, the short
fuzz, every `check-*.sh`, and the gate self-tests — with no privileges
and no host mutation, so the answer you get locally is the answer CI
will give. The lane's contents live in `scripts/local-lane.sh`, and
`scripts/check-local-lane.sh` fails CI if that file lists fewer gates
than the workflow runs; a local target that hand-listed them would
quietly cover less the first time a gate was added (#636, the same
shape as #542).

Three things it does **not** do, each declared rather than absent —
`scripts/local-lane.sh --list-exempt` prints them with reasons. The two
attribution gates judge a commit range against a pull-request body, and
`govulncheck` needs the network and a pinned install, so a local answer
would be a different one.

A step whose tool is missing (`staticcheck`, `actionlint`, `shellcheck`)
is **skipped loudly** and named in the summary rather than passing
silently. `STRICT=1 make check` turns any skip into a failure — use that
anywhere a green exit is read as coverage instead of by a person who can
see the summary.

CI shards the main suite across three jobs (#381, #468); `integration-local`
deliberately does not — a local run is one machine, so sharding would
serialise the shards and only add overhead. If you want to reproduce a
single CI shard, `sudo make integration-test-shard SHARD=1 OF=3`.

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
3. Engine version — `interface_name` (#125) needs Docker ≥28 and skips
   below it, so a skip locally and a pass in CI can both be correct.

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

One command, on a host with Docker and the integration prerequisites:

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

Two open items are waiting on exactly this signal: #218 (stable MAC,
needs `netlabel.EndpointName` at `CreateEndpoint`) and #125 (Compose
`interface_name`, needs plugin-returned `DstName` honoured at `Join`).
The captures confirm both fields are absent on engine 29.7 — the day a
capture from a newer engine carries one, the test names it. The 26.1 -> 29.7
re-record is the worked example: it introduced
`com.docker.network.enable_ipv4` on `CreateNetwork`, which is carried
inside `Options` (a map) and so costs nothing, but it arrived unannounced
and the fixtures are what showed it.

## See also

- [Driver reference](reference.md) — every option, counter, and behaviour
- [Bridge mode](bridge-mode.md) and [macvlan / ipvlan](parent-attached-modes.md) setup
