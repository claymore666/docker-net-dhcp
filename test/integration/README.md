# Integration test harness

Live, end-to-end tests for the plugin: real network namespaces, a
real parent NIC (one end of a veth pair), a real DHCP server (host
`dnsmasq` subprocess), and a real Docker daemon driving the plugin
through libnetwork. These cover the integration surface that `go
test` can't reach without privileges — `CreateEndpoint`, `Join`,
`Leave`, `recoverEndpoints`, `dhcpManager.{Start,Stop}`,
parent-attached link wiring, `dhcp.{Start,Finish,Wait,GetIP}`.

## Running locally

```sh
sudo make integration-local
```

`integration-local` is the entry point. It chains `integration-cleanup
create enable integration-test integration-test-failure`, so the plugin
under test is one built from the tree you are sitting in, and no earlier
run's leftovers are what you measure.

`sudo make integration-test` runs the main suite on its own, and
deliberately does **not** rebuild or reinstall anything — so by itself
it drives whatever plugin happens to be installed. That is not a
hypothetical: while validating #374 a stale installed build made two
tests fail for reasons unrelated to the branch *and* made the health
floor report `clean` for counters that build could not publish at all.
Use the two suite targets directly only when you have just built and
installed the plugin yourself. CI can call them directly because each
workflow job does its own build-and-install step first, and the suites
run between that and teardown. The targets deliberately carry no
rebuild dependency: one would reinstall the plugin mid-run and reset
the health floor's observation window with it. Note the primary lane
(`integration.yml`) does not run both suites in one job at all — its
matrix is `main-1/2/3` plus `failure`, each with its own build step;
the single-job shape is `integration-arm64.yml`, `integration-hosted.yml`
and `coverage.yml`.

The suite targets wrap `go test`, split by name. `integration-test`
runs `go test -v -tags integration -count=1 -timeout 20m -skip
"TestFailure_" ./test/integration/...`; `integration-test-failure` runs
the same command with `-run "TestFailure_"` instead, because the
failure-injection suite is mostly deliberate waiting and does not
belong in the main feedback loop. Measured on dev (run 30753274506,
recorded in `.github/workflows/integration.yml`): failure 231s against
main-1 367s / main-2 361s. Both tee their output to a
timestamped file under `$(ITEST_LOG_DIR)` (#378). Plain `go test ./...` skips
this directory entirely thanks to the `//go:build integration` tag on
every file here, so the unit-test cadence stays fast.

## Environment knobs

Two different things get called "settings" around this suite, and
mixing them up is how a run ends up testing something other than what
you meant.

**Read by the test process** — set these in the environment of
`make integration-test`:

| variable | default | what it does |
| --- | --- | --- |
| `INTEGRATION_PLUGIN_REF` | `ghcr.io/claymore666/docker-net-dhcp:golang` | Which installed plugin the suite drives (`harness.PluginRef`). |
| `PLUGIN_BUILD_DIR` | search `plugin/`, then `plugin-cover/` | Where `harness.BuiltPluginDir` looks for the rootfs the lane built. Leave unset in the lanes — the search already knows both. |
| `ITEST_LOG_DIR` | `logs` | Where the Makefile tees the run's output (#378). |
| `SHARD` / `OF` | — | Required by `make integration-test-shard`; 1-based shard and total. |

**`INTEGRATION_PLUGIN_REF` is the one to get right.** The harness
deliberately does not install or enable anything — that is a global
daemon mutation — so with the variable unset the suite drives whatever
is currently installed under `:golang`. That may be an older build than
the tree you are sitting in. The suite passes and tells you nothing
about your change, with no warning that it did so. When verifying a
code change locally, build and install it under its own tag and point
the variable at that tag.

**Applied to the plugin, not exported to the test run** — these are
`docker plugin set` values, and setting them in your shell before
`make integration-test` does nothing:

| setting | what it does |
| --- | --- |
| `LOG_LEVEL` | Plugin log verbosity. The lanes set `trace`; `make create` does too. |
| `STATE_DIR` | The plugin's state directory, bind-mounted from the host. |
| `GOCOVERDIR` | Where the `-cover` build writes counter files. Only meaningful for the instrumented plugin — see [Coverage harvesting](#coverage-harvesting). |

`docker plugin set` requires the plugin to be disabled, so changing one
means `docker plugin disable`, `set`, `enable`.

## Prerequisites

- Linux host with `iproute2`, `dnsmasq`, `kea-dhcp4-server`, and
  Docker installed.
- The plugin enabled at `ghcr.io/claymore666/docker-net-dhcp:golang`.
  The harness verifies this in `TestMain`; it does **not** install
  the plugin for you (deliberate — installing affects the daemon's
  global state and would conflict with smoke testing on the same
  host).
- Root (privileges: `CAP_NET_ADMIN` for veth + macvlan link work,
  network-namespace creation, ability to bind UDP `:67`).

### AppArmor (Debian/Ubuntu hosts only)

If a Kea-backed test fails with `ephemeral kea did not become ready`
and an **empty** server log, this section is the answer. Since #869 the
fixture says so in that failure itself, along with the commands below —
but the error string is repeated here so a search for it lands on the
explanation.

What the fixture reports depends on what it could measure, and it
distinguishes the cases rather than blurring them. A loaded enforcing
profile *plus* a kernel denial record naming the fixture's own temp
directory is stated as the cause, with the record quoted. A loaded
enforcing profile with no such record is reported as the likely cause
only: the profile ends in `#include <local/usr.sbin.kea-dhcp4>`, so a
site override under `/etc/apparmor.d/local/` can leave it enforcing
while permitting exactly these paths. A profile that is installed but
measurably **not** loaded is not reported at all.

Debian's `kea-dhcp4-server` package ships an **enforcing** AppArmor
profile that pins Kea to its own packaged paths — right down to the
exact PID filename, which Kea derives from the config filename. The
ephemeral fixture writes its config, lease DB and lockfile into a
per-test temp directory instead, so under the shipped profile Kea
cannot start. The profile denies `dac_override`, so running as root
does not help; the symptom is `Permission denied` on paths root can
obviously write.

Put the profile in complain mode for local runs:

```sh
sudo apparmor_parser -C -r /etc/apparmor.d/usr.sbin.kea-dhcp4
```

To restore enforcement afterwards:

```sh
sudo apparmor_parser -r /etc/apparmor.d/usr.sbin.kea-dhcp4
```

A privileged container does **not** escape this. `--privileged` sets the
container's own profile to `unconfined`, and the README used to stop there —
but an unconfined process still *transitions into a matching profile on exec*,
which is ordinary AppArmor behaviour. Measured on a host with the profile
loaded: the container's shell reports `unconfined`, and `kea-dhcp4` launched
from that same shell is nonetheless denied, with the kernel logging
`apparmor="DENIED" ... profile="kea-dhcp4"`.

What actually decides it is whether **the host has the profile loaded at all**,
which in practice means whether `kea-dhcp4-server` is installed on the host —
not whether the container is privileged. CI has been unaffected because its
runner host does not have the package, not because the container is privileged.
Install kea on a runner host and CI starts failing the same way.

The package also enables and starts a system `kea-dhcp4-server`
service on install. The fixture runs its own Kea, so the packaged
service is not needed; disable it (`sudo systemctl disable --now
kea-dhcp4-server`) rather than leaving a DHCP server running on a
machine that sits on a real network.

## What's covered

See [#56](https://github.com/claymore666/docker-net-dhcp/issues/56)
for the original umbrella scope. Every suite file in
`test/integration/*_test.go` is listed below. The harness package's own
guards (`test/integration/harness/*_test.go`) run in the same targets
but test the harness rather than the plugin, and are not part of this
list. The list itself is the claim: a file count used to stand here,
went stale without anything noticing, and anyone who wants the number
can count the entries. Tests run serially (see below). Grouped by what
they prove:

**Golden paths (per mode)**
- `lifecycle_macvlan_test.go`, `lifecycle_bridge_test.go`,
  `lifecycle_ipvlan_test.go` — full create→run→inspect→leave→delete
  in each attachment mode. ipvlan active since v0.7.0 (#62: `-B`
  broadcast flag + no MAC-echo on ipvlan).
- `ipv6_test.go` — dual-stack golden paths (macvlan + bridge) with
  `ipv6=true`, DNS6 default-off, and failure-only wire diagnostics
  (tcpdump + neighbor tables). Three deeper tests (v6 renewal,
  DNS6 opt-in, DUID persistence across plugin restart) are
  `t.Skip`'d pending the IA unification (#152).
- `concurrency_test.go` — N simultaneous containers, distinct leases.

**Lease lifecycle & identity**
- `lease_renew_test.go` — the persistent client renews at T1 without
  disturbing the address (2m fixture lease floor ⇒ ~70s waits).
- `tombstone_restart_test.go` — `docker restart` preserves MAC + IP.
- `static_ip_test.go` — `--ip` / `--driver-opt ip=` request hints.
- `client_id_test.go` — option 61 stability + `client_id` override.
- `vendor_class_test.go` — option 60 round-trip via dnsmasq
  class-tagged gateway override (exact-match route parsing, #130).
- `audit_log_test.go` — `audit_log=true` ledger lifecycle
  (bound→stopped), default-off absence (#109).
- `join_no_container_test.go` — an attach that fails because no
  container ever claimed the endpoint leaves the address leased
  upstream until it expires, and sends no DHCPRELEASE (#800; the file
  covered the opposite behaviour for #566 before that). The Join is
  issued against a genuinely live sandbox, so "nobody holds this
  endpoint" is the only branch that can answer. It also asserts no
  `dh-rel-*` link is on the host — the removed reclaim's fingerprint,
  which is what a reintroduced one would leave.

  `orphan_release_test.go` covered the reclaim itself and went with it
  in #800.
- `concurrent_renew_test.go` — two containers on one network, both with
  the default `eth0`, each keep their own persistent client and each
  renew. dhcpcd keys its pidfile and control sockets by interface name
  alone, so without per-client runtime-dir isolation the second
  container's client forwarded its argv to the first one's socket and
  exited 0 (#330). No single-container renewal test can see that.
- `nonroot_test.go` — the persistent client starts in a container whose
  init process runs as a **non-root** user (#317). Every other test
  here runs its container as root, so the netns open passed on the
  uid-match arm and the missing `CAP_SYS_PTRACE` survived every
  release; the proof is a renewal DHCPACK, not a counter.

**Option propagation**
- `dns_propagate_test.go`, `mtu_propagate_test.go` — opt-in writes,
  default-off pairs.
- `extra_options_test.go` — captured-but-not-applied options (NTP,
  TFTP, bootfile, search list).
- `interface_name_test.go` — plugin honors the ifname endpoint
  option + invalid names rejected at attach; the engine-applied
  rename tests are capability-probe-gated (they need an engine that
  carries moby/moby#52866 — merged upstream, first shipping in engine
  29.8.0, #125).
- `classless_routes_test.go` — a DHCP-pushed classless static route
  (option 121, RFC 3442) reaches the container's routing table, and
  stays absent for a client that did not opt into the vendor class the
  fixture tags on (#260).
- `fqdn_test.go` — `register_dns=true` makes the client send the FQDN
  option (81) and the server register `<hostname>.<domain>` (#261).
  The fixture's `--dhcp-fqdn` registers only clients that send option
  81, so resolving the name is itself the proof that the option, and
  not a bare hostname hint, is what landed.

**Server selection**
- `dhcp_server_policy_test.go` — `dhcp_servers` preference and
  `dhcp_deny_servers` exclusion, including deny beating prefer for the
  same server, fallback to the next server, and failing closed when the
  list is exhausted (#111, #669). The only fixture in the suite that
  deliberately runs two DHCP servers on one broadcast domain, because
  against a single server a pass would prove nothing. Which server
  answered is read from the leased address (the pools are disjoint) and
  from that server's own log by MAC; the counters are a second
  statement about the same event, never the primary evidence.
  Bridge-mode only — a point-to-point veth fixture cannot host a second
  server, and the selection path itself is mode-independent.

**Address conflict detection**
- `address_conflict_test.go` — an address the server leased that
  another device on the segment already holds is **reported**, not
  silently accepted (#524), with a clean segment, bridge mode, and a
  bare parent (verdict: undetermined) as the negative cases. The
  fixture server's log is deliberately not an assertion here: from its
  point of view the lease was ordinary, and it cannot see a static host
  that never asked it for anything.
- `probe_stale_route_test.go` — a `/32` the conflict probe left on the
  parent when its process went away mid-window does not blind every
  later probe for that address; the plugin reclaims the route instead
  of failing at `RouteAdd` with EEXIST (#572). The test leaves the
  route itself rather than racing a daemon restart for it.

**Failure injection (#128, separate step: `make integration-test-failure`)**
- `failure_test.go` — `TestFailure_*` against per-test ephemeral
  DHCP servers (`harness/ephemeral.go`): server loss during renewal
  (retention + self-recovery), lease refused on renewal via server
  renumbering (unattended re-acquisition, stale-inspect as the
  documented #104 divergence), full lease expiry (deliberate
  retention, endpoint stays reachable).

  Every `TestFailure_*` case turns on an inequality between the lease
  and the timers derived from it — `T1 < outage < lease` and the like.
  Those timings are **verified, not declared** (#472): at teardown the
  ephemeral fixture reads every `DHCP4_LEASE_ALLOC` line Kea logged and
  fails the test if the lifetime the server granted differs from the
  one the fixture asked for, naming both numbers. A run with no
  allocation at all fails too, so absent evidence never reads as
  agreement. The check runs itself, once per fixture — no test opts in,
  and none can forget. Kea only; the dnsmasq backend (`WithDNS`, for
  the FQDN test) logs no granted lifetime, and that test does not
  depend on one.

**Recovery & restart**
- `recovery_test.go` — plugin disable/enable with a live container.
- `recovery_daemon_test.go` — daemon restart (supervisor-agnostic:
  systemctl on bare metal, direct dockerd supervision in
  containerized runners, #145) with a `--restart=always` container.
- `recovery_daemon_kill_test.go` — SIGKILL of the daemon: no shutdown
  sequence, no `Leave` on any endpoint, the one abrupt death nothing
  else here reaches. It does **not** assert that recovery re-adopted
  an endpoint, and that is the finding rather than a gap (#480, #679):
  measured over six runs, containerd dies with dockerd and the
  relaunched daemon removes each sandbox as stale, so no adoptable
  endpoint survives. What it asserts instead is read off the DHCP
  server: the returned container holds a lease the server actually
  granted, and the pre-death lease was **not** released — since #800
  the address is held until it expires, the same as for a machine
  powered off abruptly. The ACK is checked first and is the positive
  control; the absence after it would otherwise read as a pass against
  a log that had gone missing or stale.
- `preflight_probe_test.go` — `validate_dhcp=true` probe accept/
  reject + bridge-mode rejection.

**Parent NIC contention**
- `parent_gate_test.go` — the per-parent gate serialises two operations
  that would otherwise be on the parent NIC at the same time, so one
  cannot fail the other with `device or resource busy` (#486, #549).
  The holder is a `validate_dhcp` preflight probe, which keeps a
  `dh-probe-*` link on the parent across a DHCP round trip; the
  contender is a `CreateEndpoint` issued straight at the plugin socket
  once the probe's link is visible. Until #800 the holder was an orphan
  reclaim (`dh-rel-*`), which no longer exists — the probe is the
  remaining operation that holds a parent long enough to collide.
  The probe's hold budget is 8s against the gate's 4s wait, so the
  direction is fixed by constants rather than by timing: the contender
  either waits or times out, and `parent_link_waits + parent_link_wait_timeouts`
  is asserted non-zero. What this does **not** prove is written in the
  test header — a gate that serialised into a deadlock would satisfy
  that sum, so the endpoint's own success is asserted too.

**Host and install contracts**
- `sandbox_netns_test.go` — the sandbox netns directory is readable
  from inside the *running* plugin, so `sandboxGone` has evidence to
  work from (#567). It asserts the input, not the verdict: the unit
  tests cover the logic thoroughly against a `t.TempDir()`, and could
  never see that production passed a directory the plugin had no mount
  for, leaving the branch dead for every release up to the fix.
- `statedir_bind_test.go` — the install-time `STATE_DIR` bind-source
  contract the docs describe (#494, #499): the daemon does not create a
  missing bind source, a failed install leaves a *disabled* plugin
  rather than rolling back, a retried install answers "already exists"
  without re-attempting the mount, and mkdir + `docker plugin enable`
  is the recovery. Builds a throwaway plugin under its own name and
  temporary bind source — outside the namespaces `driverRegexp`
  matches — so the suite's own install is untouched.

**Error surfaces**
- `errors_test.go`, `errors_netlink_test.go` — create-time
  validation (modes, options, IPAM) and netlink-state rejections.
- `static_routes_bridge_test.go`, `static_routes_macvlan_test.go` —
  route copying + `skip_routes` opt-out.

**Observability**
- `health_counters_test.go` — /Plugin.Health counter movement.
- `metrics_test.go` — the plugin that was actually built and installed
  serves `/metrics` on its socket, in the text exposition format, with
  every `HealthResponse` field present (#651). The oracle is the
  harness's own struct rather than a literal list, so a counter added
  later is checked here automatically; routing, the shipped manifest
  and the real binary are all outside what a unit test can see.
- `healthfloor_test.go` — not a scenario but the suite's floor, run
  from `TestMain` after the last test: it asks /Plugin.Health whether
  anything went wrong during the run and fails the run if it did. The
  complement to the per-test deltas, not a duplicate — a delta only
  catches a fault inside some test's own bracket, the floor catches one
  that no test bracketed, including during fixture setup (#374). It
  asserts `Healthy` alongside every counter behind that flag — the
  list is `floorCounters` in the harness, four today, and the number is
  deliberately not restated here (#421). Where a test recycles the plugin the counters reset, so the
  verdict reads "no fault since the last plugin restart in this run"
  and says which of the two it is (#385).

Tests run **serially** by design. None of the current cases call
`t.Parallel()`, even though most would be safe — the recovery and
daemon-restart tests planned for #56 will mutate global daemon
state (plugin disable/enable, a full daemon restart), and
those have to run alone. Keeping the suite serial avoids designing
in a foot-gun where a future test inadvertently runs concurrently
with one that drops the docker socket.

If a future test is *clearly* read-only and pure-validation (like
the `errors_test.go` cases), parallelizing it as a `t.Run` subtest
is fine — but think before adding `t.Parallel()` to a top-level
test.

## CI

The same suite runs on a self-hosted runner for every PR, with the
**outside-collaborator approval gate** turned on so external PRs
don't get free root on the runner host. See
`.github/workflows/integration.yml`. A separate manual-only
`.github/workflows/coverage.yml` runs the same suite against a
cover-instrumented plugin — see "Coverage harvesting" below.

The workflow assumes the Go toolchain is pre-installed on the
runner — `actions/setup-go@v5` is skipped to save ~30s/run. If
you're standing up a new runner, run the operator script once:

```sh
sudo bash test/integration/install-go-runner.sh
```

This downloads the Go version pinned in `go.mod` from go.dev and
drops it under `/usr/local/go`, with `/usr/local/bin/go` symlinked
in. Re-running upgrades in place.

## Where the suite runs, and how those places differ

The suite runs in more than one environment, and they are **not**
meant to be identical. Keeping them different is what catches
portability problems — the Kea AppArmor confinement below exists on
a packaged host install and on no container, so a container-only
world would never have surfaced it.

The cost of that choice is that "green here, red there" needs a
reason, and without a written baseline the first guess is usually
wrong. This table is that baseline. **When two environments
disagree, start here rather than assuming a bug.**

| | Local (`sudo make integration-local`) | CI (`dhcp-ci` pool) | Hosted (`integration-hosted.yml`) |
|---|---|---|---|
| Machine | your dev box / the integration runner host, bare metal | privileged container from `ghcr.io/claymore666/dhcp-ci-runner` | stock GitHub-hosted VM |
| `dnsmasq`, `kea-dhcp4` | host packages, whatever the distro ships | baked into the image, see `ci/runner-image/Dockerfile` | installed per run by the workflow |
| AppArmor | **profiles apply** — Debian's enforcing `kea-dhcp4` profile blocks the fixture (see Prerequisites) | applies **if the host has the profile loaded**; today it does not, because kea is not installed on the runner host — privilege is *not* what saves it | none in practice |
| State between runs | **accumulates** — leftover containers, veths, namespaces | none: one container per job, `--rm` | none |
| Docker daemon | your host daemon, whatever version | nested daemon, Engine >= 28 | the runner's daemon |
| Gating | no | **yes**, required check | no — portability signal only |

Three consequences worth knowing before they cost you an afternoon:

- **Local is the only place state accumulates.** `integration-local`
  therefore runs `integration-cleanup` first, mirroring the CI job's
  own first step. A single container left by an earlier aborted run
  fails an unrelated test with a name conflict, days later, and reads
  exactly like a regression.
- **AppArmor confines the fixture wherever the host has the profile
  loaded** — including inside a privileged container. Today that is local
  hosts only, so a failure that reproduces locally and not in CI is as
  likely to be confinement as a bug. Do not read that as a property of
  containers: it is a property of which hosts happen to have
  `kea-dhcp4-server` installed, and it changes the day one of them does.
- **CI's image is fetched when the runner container launches, not when
  it picks up a job.** The runners are JIT and can sit idle after
  launch, so a slot can be serving an image older than the newest
  publish. That produced a ~25% per-job failure rate during #356 and
  looked like a flaky suite. The `Verify fixture dependencies` step in
  `integration.yml` exists to name that in seconds instead; if it
  fires, the runner is stale, not the code.

## Architecture

```
[host netns]


  Macvlan/ipvlan path (one L2 segment, two parents — #556):
    dh-itest-host  (.2)  <─ veth ─>  dh-itest-hostp ─┐
          │                                          │
     parent= for MACVLAN                        dh-itest-dhcp  (bridge;
     plugin children                            192.168.99.1/24 +
                                                fd00:6470:6863::1/64)
    dh-itest-ipv   (.3)  <─ veth ─>  dh-itest-ipvp  ─┘   dnsmasq #1
          │                                              (dual-stack + RA)
     parent= for IPVLAN                                  v4 pool .10–.99,
     plugin children                                     v6 ::10–::99

  Bridge path:
    dh-itest-br2  (192.168.100.1/24 + fd00:6470:6864::1/64)
          │
     bridge= for                dnsmasq #2 (dual-stack + RA)
     plugin endpoints           + ip(6)tables FORWARD ACCEPT

  Failure-injection path (per-test, created/destroyed by each
  TestFailure_*):
    dh-itest-ehost <─ veth ─>  dh-itest-edhcp (192.168.101.1/24;
          │                          │          renumbered to
     parent= for                ephemeral kea (authoritative),
     the test's network         inside netns dh-itest-eph
                                Stop/StartAgain/RestartOnSubnet
```

The ephemeral server's end of that veth pair lives in its **own
network namespace**, and that is load-bearing rather than tidy
(#356). dnsmasq binds its DHCP socket as a device-scoped wildcard
(`0.0.0.0%<iface>:67`) with `SO_REUSEADDR`; Kea binds its fallback
socket to a specific address without it, so the kernel refuses the
second bind. Since `TestMain` always has the suite-static dnsmasq
up, a shared namespace leaves Kea with **no sockets open at all** —
and Kea still logs `DHCP4_STARTED` in that state, so the fixture's
readiness probe checks for an open interface socket rather than
trusting that line.

A single shared `Fixture` (`test/integration/harness/fixture.go`,
`harness/bridge.go`) owns both subnets for the whole `go test`
invocation. Tests select a path by mode:
`harness.CreateNetwork(t, ctx, ..., "macvlan", nil)` uses
`dh-itest-host` as the parent, `"ipvlan"` uses `dh-itest-ipv`, and
`"bridge"` uses `dh-itest-br2`.

**Why macvlan and ipvlan get separate parents, and what that asks of a
new test.** A parent NIC is a macvlan port or an ipvlan port, never
both — the two kinds contend for its single receive handler, and the
second to ask is refused with `Device or resource busy`. Plugin
teardown is asynchronous relative to test boundaries: the `validate_dhcp`
preflight probe keeps its temporary `dh-probe-*` child on the parent for
a full DHCP round trip, and until v1.9.0 the orphaned-lease reclaim kept
a `dh-rel-*` child there for the same span after the test that caused it
had already returned (that mechanism is gone — see #800 — but the
asynchrony is not). With one shared parent, a macvlan test's tail could
therefore still own the parent when the next ipvlan test's head asked
for it, and the suite went red on the wrong test — an EBUSY from deep inside a netlink
call that reads as a plugin fault (#556). Dedicated parents remove the
contention; `harness.CreateNetwork` additionally asserts that the
parent it is about to use carries no child of the other kind, so a
violation is named rather than diagnosed.

The general lesson survives the fix and applies to every mode: **check
what your test leaves for its neighbours, not only what it asserts
about itself.** Shards run tests in declaration order, so a test that
returns while the plugin is still tearing down on its behalf hands that
state to whichever test is declared next. If your test can leave links
on a fixture parent, wait until they are gone before returning (see
`awaitReleaseLinksGone` in `orphan_release_test.go`) rather than
assuming teardown outruns the next test.

Distinct subnets keep the two dnsmasq instances cleanly isolated
from each other — without that, two DHCP servers on the same
broadcast domain would race and tests would bind whichever
answered first.

## Debugging a failed test

1. Run with `-v` to see the harness setup logs.
2. The failed test's `t.Cleanup` dumps the captured `dnsmasq` log
   (every DISCOVER/REQUEST/ACK/RELEASE) at the end — the wire
   conversation is usually enough to localise the problem.
3. `t.Cleanup` is best-effort. If a test panics mid-setup, run
   `sudo bash test/integration/cleanup-orphans.sh` to remove
   leftover `dh-itest-*` interfaces, networks, and the `dnsmasq`
   process if it's still running.

## Coverage harvesting

A second workflow, `.github/workflows/coverage.yml`, runs the same
suite against a `go build -cover -coverpkg=./...` instrumented
plugin (tag `:golang-cover`) and reports per-package coverage plus
an HTML report as a workflow artifact. Runs on demand
(`workflow_dispatch`) and on every release PR into `main`, where the
coverage ratchet (`scripts/coverage-ratchet.sh` against
`.github/coverage-baseline.txt`) is a required gate.

Locally:

```sh
sudo mkdir -p /var/lib/dh-cover && sudo chmod 0777 /var/lib/dh-cover
make plugin-cover create-cover enable-cover
sudo INTEGRATION_PLUGIN_REF=ghcr.io/claymore666/docker-net-dhcp:golang-cover make integration-test
make disable-cover    # flushes counter files
go tool covdata percent -i=/var/lib/dh-cover
go tool covdata textfmt -i=/var/lib/dh-cover -o coverage.out
go tool cover -html=coverage.out -o coverage.html
```

The plugin runtime emits `covmeta.*` on startup and `covcounters.*`
on graceful shutdown, so the `disable-cover` step is what actually
flushes the counters. The cover plugin is a parallel install — it
coexists with the production `:golang` tag; `make plugin-cover`
uses an isolated `plugin-cover/` rootfs dir.
