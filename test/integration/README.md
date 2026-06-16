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
sudo make integration-test
```

The Makefile target wraps `go test -tags integration
./test/integration/...`. Plain `go test ./...` skips this directory
entirely thanks to the `//go:build integration` tag on every file
here, so the unit-test cadence stays fast.

## Prerequisites

- Linux host with `iproute2`, `dnsmasq`, and Docker installed.
- The plugin enabled at `ghcr.io/claymore666/docker-net-dhcp:golang`.
  The harness verifies this in `TestMain`; it does **not** install
  the plugin for you (deliberate — installing affects the daemon's
  global state and would conflict with smoke testing on the same
  host).
- Root (privileges: `CAP_NET_ADMIN` for veth + macvlan link work,
  ability to bind UDP `:67` for `dnsmasq`).

## What's covered

See [#56](https://github.com/claymore666/docker-net-dhcp/issues/56)
for the original umbrella scope. 22 test files, run serially (see
below). Grouped by what they prove:

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
  (bound→release), default-off absence (#109).

**Option propagation**
- `dns_propagate_test.go`, `mtu_propagate_test.go` — opt-in writes,
  default-off pairs.
- `extra_options_test.go` — captured-but-not-applied options (NTP,
  TFTP, bootfile, search list).
- `interface_name_test.go` — plugin honors the ifname endpoint
  option + invalid names rejected at attach; the engine-applied
  rename tests are capability-probe-gated (engine support pending
  upstream, #125).

**Failure injection (#128, separate step: `make integration-test-failure`)**
- `failure_test.go` — `TestFailure_*` against per-test ephemeral
  DHCP servers (`harness/ephemeral.go`): server loss during renewal
  (retention + self-recovery), lease refused on renewal via server
  renumbering (unattended re-acquisition, stale-inspect as the
  documented #104 divergence), full lease expiry (deliberate
  retention, endpoint stays reachable).

**Recovery & restart**
- `recovery_test.go` — plugin disable/enable with a live container.
- `recovery_daemon_test.go` — daemon restart (supervisor-agnostic:
  systemctl on bare metal, direct dockerd supervision in
  containerized runners, #145) with a `--restart=always` container.
- `preflight_probe_test.go` — `validate_dhcp=true` probe accept/
  reject + bridge-mode rejection.

**Error surfaces**
- `errors_test.go`, `errors_netlink_test.go` — create-time
  validation (modes, options, IPAM) and netlink-state rejections.
- `health_counters_test.go` — /Plugin.Health counter movement.
- `static_routes_bridge_test.go`, `static_routes_macvlan_test.go` —
  route copying + `skip_routes` opt-out.

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

## Architecture

```
[host netns]


  Macvlan/ipvlan path:
    dh-itest-host  <─ veth ─>  dh-itest-dhcp  (192.168.99.1/24 +
          │                          │          fd00:6470:6863::1/64)
     parent= for                dnsmasq #1 (dual-stack + RA)
     plugin children            v4 pool .10–.99, v6 ::10–::99

  Bridge path:
    dh-itest-br2  (192.168.100.1/24 + fd00:6470:6864::1/64)
          │
     bridge= for                dnsmasq #2 (dual-stack + RA)
     plugin endpoints           + ip(6)tables FORWARD ACCEPT

  Failure-injection path (per-test, created/destroyed by each
  TestFailure_*):
    dh-itest-ehost <─ veth ─>  dh-itest-edhcp (192.168.101.1/24;
          │                          │          renumbered to
     parent= for                ephemeral dnsmasq (authoritative)
     the test's network         Stop/StartAgain/RestartOnSubnet
```

A single shared `Fixture` (`test/integration/harness/fixture.go`,
`harness/bridge.go`) owns both subnets for the whole `go test`
invocation. Tests select a path by mode:
`harness.CreateNetwork(t, ctx, ..., "macvlan", nil)` uses
`dh-itest-host` as the parent; `"bridge"` uses `dh-itest-br2`.

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
