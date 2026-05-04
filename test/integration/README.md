# Integration test harness

Live, end-to-end tests for the plugin: real network namespaces, real
parent NICs (veth pairs), a real DHCP server (pure-Go via
`insomniacslk/dhcp`), and a real Docker daemon driving the plugin
through libnetwork. These cover the integration surface that `go
test` can't reach without privileges — `CreateEndpoint`, `Join`,
`Leave`, `recoverEndpoints`, `dhcpManager.{Start,Stop}`,
parent-attached link wiring, `udhcpc.{Start,Finish,Wait,GetIP}`.

## Running locally

```sh
make integration-test
```

The Makefile target wraps `sudo go test -tags integration
./test/integration/...` because the harness creates bridges, veth
pairs, and a UDP listener on `:67`. Plain `go test ./...` skips
this directory entirely thanks to the `//go:build integration` tag
on every file here, so unit-test cadence stays fast.

## Prerequisites

- Linux host with `iproute2` and Docker installed.
- The plugin enabled at `ghcr.io/claymore666/docker-net-dhcp:golang`.
  The harness verifies this in `TestMain`; it does **not** install
  the plugin for you (deliberate — installing affects the daemon's
  global state).
- `sudo` for the test binary (privileges: `CAP_NET_ADMIN`,
  `CAP_SYS_ADMIN`, ability to bind UDP `:67`).

## What's covered

See [`#56`](https://github.com/claymore666/docker-net-dhcp/issues/56)
for the umbrella scope. Tests are organised by lifecycle phase:

- `lifecycle_bridge_test.go` — full create→run→inspect→leave→delete
  in bridge mode.
- `lifecycle_macvlan_test.go` — same, in macvlan mode.
- `lifecycle_ipvlan_test.go` — same, in ipvlan L2 mode.
- `recovery_test.go` — plugin disable/enable; daemon restart with
  `--restart=always` containers.
- `tombstone_test.go` — `docker restart <ctr>` preserves MAC + IP.
- `concurrency_test.go` — N containers attached simultaneously.
- `errors_test.go` — invalid mode, parent down, parent is bridge,
  IPAM not null, conflicting `--ip`.

## CI

The same suite runs on a self-hosted runner (gpu1) for every PR, with
the **outside-collaborator approval gate** turned on so external PRs
don't get free root on the runner host. See
`.github/workflows/integration.yml`.

## Architecture

Each test owns its own bridge + veth pair + DHCP server, set up and
torn down in `t.Cleanup`. Tests do not share fixture state, so
parallel runs (`go test -parallel N`) are safe.

```
[host netns]
  dh-test-br-<unique>   (Linux bridge, no IP on host)
       |
       +-- veth-host-<unique>  (parent NIC for plugin's macvlan/ipvlan)
       +-- veth-dhcp-<unique>  (other end of pair, in netns lan)

[netns lan-<unique>]
  veth-dhcp-<unique>    (192.168.99.1/24)
       |
       (pure-Go DHCP server bound here, pool 192.168.99.10–99.99)
```

For bridge-mode tests, the plugin's network has `bridge=dh-test-br`.
For macvlan/ipvlan tests, `parent=veth-host-<unique>`.

## Debugging a failed test

1. Run with `-v` to see the harness setup logs.
2. The DHCP server logs every DISCOVER/REQUEST/RELEASE on a per-test
   channel; failed tests dump the captured packets at the end.
3. `t.Cleanup` is best-effort. If a test panics mid-setup, run
   `sudo ./test/integration/cleanup-orphans.sh` to remove leftover
   `dh-test-*` interfaces and netnses.
