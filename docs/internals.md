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

Three loops, cheapest first. Only the last two need root or a plugin.

| what | command | needs |
| --- | --- | --- |
| unit + gate scripts | `go test ./...` | nothing — seconds |
| the suite's own guards | `go test ./test/integration/harness/` | nothing |
| both integration suites | `sudo make integration-local` | root, Docker |

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

`integration-local` chains `create enable integration-test
integration-test-failure`, so a stale plugin cannot be what you measure.

The two suite targets deliberately do **not** depend on a rebuild: CI
calls them in sequence between its own build and teardown, and a
rebuild dependency there would reinstall the plugin *between* the two
suites — recycling it mid-run and resetting the health floor's
observation window with it.

### Reading the output

Both suites tee to `test/integration/logs/`. At the end of each, the
health floor prints a verdict for the whole run:

- `HEALTH FLOOR: clean — ... over the whole Ns run` — the plugin was up
  throughout and nothing healthy-affecting moved.
- `HEALTH FLOOR: clean over the last Ns of an Ms run` — the plugin
  restarted mid-suite, so the counters only cover the tail. The
  whole-run fault census covers the rest.
- `PLUGIN FAULTS: N across the whole run` — read from the log rather
  than the counters, so it survives a restart. Any non-zero value fails
  the run.

A run that cannot read the plugin log fails rather than reporting
clean: an unreadable instrument is not a clean result.

### If a local run disagrees with CI

Check, in order:

1. Did you build? `sudo make integration-local` rather than a bare
   suite target.
2. Is a previous run's state still around? `make integration-cleanup`.
3. Engine version — `interface_name` (#125) needs Docker ≥28 and skips
   below it, so a skip locally and a pass in CI can both be correct.

## See also

- [Driver reference](reference.md) — every option, counter, and behaviour
- [Bridge mode](bridge-mode.md) and [macvlan / ipvlan](parent-attached-modes.md) setup
