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
- `lifecycle_ipvlan_test.go` — same, in ipvlan-L2 mode. Currently
  `t.Skip`'d because broadcast OFFER delivery to the slave doesn't
  work when the parent is a veth (real LAN parents work — covered
  by manual smoke testing). Tracked as
  [#62](https://github.com/claymore666/docker-net-dhcp/issues/62).
- `tombstone_restart_test.go` — `docker restart <ctr>` preserves
  MAC + IP via the tombstone mechanism.
- `concurrency_test.go` — N containers attached simultaneously each
  get a distinct lease.
- `errors_test.go` — invalid mode, missing parent, wrong-mode
  options, IPAM not null. Validation-only; never reaches DHCP.

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
  dh-itest-host  <─── veth pair ───>  dh-itest-dhcp  (192.168.99.1/24)
        │                                  │
   parent= for                         dnsmasq listens here
   plugin's macvlan                    pool: 192.168.99.10–99
   children                            lease: 30s
```

A single shared `Fixture` (`test/integration/harness/fixture.go`)
owns the veth pair and the dnsmasq subprocess for the whole `go
test` invocation. Tests that want a network call
`harness.CreateNetwork(t, ctx, ..., "macvlan", nil)` which uses
`dh-itest-host` as the parent.

Bridge mode is not yet covered here — it needs a separate fixture
(Linux bridge + a different subnet to avoid host-routing conflicts).
Tracked in #56.

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
