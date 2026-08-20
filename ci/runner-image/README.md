# Ephemeral CI runner image

Self-contained GitHub Actions runner for this repository's
privileged integration workloads: one container = one job, each with
its own nested Docker daemon. Design record: issue #149; the
nested-daemon approach was validated end-to-end before this image
existed (full integration suite green inside DinD — #145 carries the
one harness fix that validation surfaced).

Published as `ghcr.io/claymore666/dhcp-ci-runner` by the
`runner-image` workflow on changes under `ci/runner-image/`.

## Orchestrator contract

```
docker run --rm --privileged \
  -e RUNNER_JIT_CONFIG=<encoded_jit_config> \
  ghcr.io/claymore666/dhcp-ci-runner:latest
```

- **`--privileged` is required**: the nested daemon plus the
  integration suite's netns/mount operations, CAP_NET_ADMIN, and
  UDP/67 binding. The trust boundary is the host this runs on, not
  the container — run it on an isolated machine.
- **`--rm` is required**: `/var/lib/docker` is an anonymous volume
  (the nested daemon needs a real filesystem to get overlay2 instead
  of vfs); `--rm` reaps it. Nothing else should be mounted in.
- **One container, one job.** JIT configs are single-use and the
  runner exits after its job; the container's exit code is the
  runner's. Relaunch with a fresh JIT config for the next job.
- JIT configs come from
  `POST /repos/<owner>/<repo>/actions/runners/generate-jitconfig`
  (GitHub App credential with repo-scoped **Administration: write**).
  Suggested fields: `name` unique per instance, `labels` matching the
  workflows' `runs-on`, `runner_group_id: 1`.
- **Forced-egress proxy (optional).** On hosts that route outbound
  traffic through an HTTP proxy, pass `-e HTTP_PROXY -e HTTPS_PROXY -e
  NO_PROXY` (or just `-e HTTPS_PROXY=…`). The runner agent, job tooling
  (`go`/`git`/`curl`), and the nested daemon's registry pulls inherit
  the env directly; the entrypoint additionally writes a docker CLI
  `proxies` config so RUN steps inside `docker build` (the plugin
  builder's `go mod download`) honor it too — that one isn't automatic.
  The fixture subnets (`192.168.99.0/24`, `192.168.100.0/24`) and
  loopback are force-added to `NO_PROXY`. No proxy env → nothing is
  written and behavior is unchanged. The proxy URL is never baked into
  the image (issue #181).
- **No inbound network, no LAN dependencies.** The runner long-polls
  GitHub over outbound 443; the test DHCP traffic stays on virtual
  interfaces inside the container. Outbound allowlist (direct, or via
  the proxy above): `github.com`,
  `api.github.com`, `*.actions.githubusercontent.com`,
  `objects.githubusercontent.com`, `ghcr.io`,
  `pkg-containers.githubusercontent.com`, `registry-1.docker.io`,
  `auth.docker.io`, `production.cloudflare.docker.com`,
  `proxy.golang.org`, `sum.golang.org`, `go.dev`, `dl.google.com`.

### The plugin's bind source

The entrypoint creates the plugin state directory (`PLUGIN_BIND_SOURCE`,
default `/var/lib/net-dhcp`) before it starts the nested daemon, and the
ordering is the whole point rather than a detail.

`/var/lib/docker` is a persistent volume while this container's root
filesystem is not, so a *recreated* container starts a daemon that
restores an already-enabled `docker-net-dhcp` whose bind source is gone.
Docker does not degrade there: the mount request fails, libnetwork
registers the remote driver anyway, and the nil plugin client SIGSEGVs
the daemon — which the supervisor then relaunches into the same panic
forever, while the runner still reports **online** to GitHub. Creating
the directory after dockerd starts is indistinguishable from not creating
it at all, because the daemon has already restored and crashed by then.

`scripts/test-runner-plugin-bind-source.sh` asserts both halves — that it
is created, and that it is created first.

## Standing runner (`register` mode)

For a host with no orchestrator — the arm64 machine — the container
registers itself once and reconnects on every boot (issue #632):

```
# once, on first boot only; the token expires within the hour
TOKEN=$(gh api -X POST repos/<owner>/<repo>/actions/runners/registration-token --jq .token)

docker run -d --name arm64-runner --restart=always --privileged \
  -e RUNNER_REGISTRATION_TOKEN="$TOKEN" \
  -e RUNNER_NAME=rpi-arm64-1 \
  -e RUNNER_LABELS=dhcp-ci-arm64 \
  --mount type=volume,src=runner-state,dst=/opt/runner-state \
  --mount type=volume,dst=/var/lib/docker \
  ghcr.io/claymore666/dhcp-ci-runner:latest register
```

Every boot after that needs no token and no human: the runner's own
credentials live on the `runner-state` volume, and `--restart=always`
reconnects it.

- **What persists is the runner's own credential**, not one that can
  create runners. A PAT or App key with `Administration: write` is
  deliberately *not* used: this host's root filesystem is a network
  share, so such a credential would sit on storage with a wider trust
  boundary than the secret itself. The registration token is short-lived
  and consumed by first boot.
- **`/opt/runner-state` must be a mount.** The entrypoint refuses to
  start if it is not — a registration written into the container's own
  filesystem works exactly once, and the next boot needs hands again,
  which is the thing this mode removes. `RUNNER_REQUIRE_PERSISTENT_STATE=0`
  opts out for a throwaway run and still warns.
- **`--rm` must NOT be used**, unlike the JIT contract above. This runner
  is standing, so `/var/lib/docker` is a named volume and state carries
  across jobs. That is the accepted trade: the image cache is a large
  part of why the arm64 suite finishes in ~17 minutes, and the
  integration suite cleans orphans before it starts.
- **Never give this runner the `dhcp-ci` label.** The amd64 workflows use
  `runs-on: [self-hosted, dhcp-ci]` with no architecture label, so a
  non-x86 runner carrying it would poach their jobs and run the suite on
  the wrong architecture. `register` mode refuses to configure with that
  label on a non-x86 host; `actionlint.yaml` documents the rule.
- The runner reads **`offline` between release candidates** and whenever
  its boot server is down — that is normal for this host, not an outage.
  Pool monitoring counts `rpi-arm64-*` separately for this reason.

Behaviour is covered by `scripts/test-runner-register.sh`, which drives
first boot, reboot, partial and unpersisted state, a failing `config.sh`,
and the label rule against stub binaries — no GitHub contact.

## Self-test (no GitHub contact)

```
docker run --rm --privileged ghcr.io/claymore666/dhcp-ci-runner:latest selftest
```

Verifies: nested daemon comes up with a real overlay storage driver (overlay2 or containerd overlayfs — not the vfs fallback), seed images load,
a SIGTERM'd dockerd is relaunched by the supervisor — the property the
daemon-restart integration test depends on (`harness.RestartDockerDaemon`,
#145) — and the cgroup root is a clean `domain` with no member processes
(the cgroup-nesting precondition, #158). Run it after any change to this
directory and on any new host.

## What's baked in, and why

| Piece | Why |
|---|---|
| Docker Engine ≥ 28 (docker-ce) | nested daemon runs the plugin under test; ≥ 28 unblocks engine-gated tests (#125) |
| supervised dockerd (relaunch loop under tini) | daemon-restart recovery test must be able to bounce the daemon without killing the environment (#145) |
| cgroup v2 nesting prep (entrypoint evacuates the root cgroup into an `init` leaf, then delegates controllers) | running dockerd bare leaves every process in the cgroup-namespace root; cgroup v2's no-internal-processes rule then forces the nested daemon's plugin/container cgroups to be *threaded*, and `cgroup.kill` (runc task teardown, docker-ce ≥ 29) is unsupported on threaded cgroups → `docker plugin disable/enable` fails with EOPNOTSUPP (#158). systemd / `docker:dind` do the same evacuation |
| Go toolchain (go.mod's version) | test compilation on the runner, mirrors `install-go-runner.sh` |
| dnsmasq, iproute2, iptables | integration fixtures (test-spawned DHCP server on veth pairs) |
| kea-dhcp4-server, kea-dhcp6-server | the fixture's next DHCP server (#356). dnsmasq's minimum lease is two minutes and every failure-suite wait is built on that floor rather than on the protocol; kea takes a 20s lease. Both are present until the fixture stops referencing dnsmasq. |
| Go module + compile caches | ephemeral containers start cold; baking turns minutes of per-job downloads/compiles into cache hits |
| seed image tars (golang builder, alpine test image) | `docker load` at start beats pulling ~250 MB per job. The plugin's digest-pinned base still pulls (3.5 MB — `docker load` can't satisfy digest references) |

## Known limits

- The plugin build's `go mod download` inside its builder stage still
  fetches modules from the network per job (the baked cache helps the
  runner-side test compile, not the docker-build stage). It honors the
  proxy when one is injected (see the orchestrator contract above).
  Acceptable at current module sizes; a host-side GOPROXY cache is the
  upgrade path if it ever isn't.
- Image rebuilds don't track `go.mod` bumps automatically — the
  workflow triggers on `ci/runner-image/**` changes and manual
  dispatch. A stale cache costs seconds, not correctness.
