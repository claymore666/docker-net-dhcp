# Integration test harness

Live, end-to-end tests for the plugin: real network namespaces, a
real parent NIC (one end of a veth pair), a real DHCP server (host
`dnsmasq` subprocess), and a real Docker daemon driving the plugin
through libnetwork. These cover the integration surface that `go
test` can't reach without privileges — `CreateEndpoint`, `Join`,
`Leave`, `recoverEndpoints`, `dhcpManager.{Start,Stop}`,
parent-attached link wiring, `udhcpc.{Start,Finish,Wait,GetIP}`.

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
for the umbrella scope. Tests so far:

- `lifecycle_macvlan_test.go` — full create→run→inspect→leave→delete
  in macvlan mode.
- `lifecycle_bridge_test.go` — same, in bridge mode (uses the
  bridge fixture: separate Linux bridge + second dnsmasq on
  192.168.100/24).
- `lifecycle_ipvlan_test.go` — same, in ipvlan-L2 mode. Currently
  `t.Skip`'d because broadcast OFFER delivery to the slave doesn't
  work when the parent is a veth (real LAN parents work — covered
  by manual smoke testing). Tracked as
  [#62](https://github.com/claymore666/docker-net-dhcp/issues/62).
- `tombstone_restart_test.go` — `docker restart <ctr>` preserves
  MAC + IP via the tombstone mechanism.
- `concurrency_test.go` — N containers attached simultaneously each
  get a distinct lease.
- `errors_test.go`, `errors_netlink_test.go` — option-validation
  rejections (invalid mode, missing parent, wrong-mode options,
  IPAM not null) plus netlink-state ones (parent down, parent is
  a bridge, malformed driver-opt ip).
- `recovery_test.go` — `docker plugin disable -f` + `enable` while
  a container is attached; asserts Plugin.Health.recovered_ok ≥ 1
  and the container's IP/MAC survive the recycle.
- `recovery_daemon_test.go` — `systemctl restart docker` mid-suite
  with a `--restart=always` container attached; asserts the daemon
  comes back, the container restarts, and the IP/MAC are
  preserved. Empirically the IP is preserved via the tombstone
  path (graceful shutdown calls Leave) rather than recoverEndpoints
  — the test logs both paths' counters but tests on the
  user-visible invariant.

Tests run **serially** by design. None of the current cases call
`t.Parallel()`, even though most would be safe — the recovery and
daemon-restart tests planned for #56 will mutate global daemon
state (plugin disable/enable, `systemctl restart docker`), and
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
`.github/workflows/integration.yml`.

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
    dh-itest-host  <─ veth ─>  dh-itest-dhcp  (192.168.99.1/24)
          │                          │
     parent= for                dnsmasq #1
     plugin children            pool 192.168.99.10–99

  Bridge path:
    dh-itest-br2  (192.168.100.1/24)
          │
     bridge= for                dnsmasq #2 bound to br2
     plugin endpoints           pool 192.168.100.10–99
                                + iptables FORWARD ACCEPT
                                  (br_netfilter would otherwise
                                  drop bridged DHCP under docker's
                                  default-DROP FORWARD policy)
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

## ipvlan + veth

`TestLifecycleIPvlan_GoldenPath` is currently skipped because the
DHCP `OFFER` (broadcast — see `--dhcp-broadcast` in the fixture)
doesn't reach the ipvlan slave inside the container netns when the
parent is a veth. Real hardware NIC parents work fine — that path
is covered by manual LAN smoke testing. The harness limitation is
tracked as #62; un-skipping the test once #62 is resolved is the
acceptance criterion.
